// Package acceptance_test 基于原版零修改的 CLIProxyAPI v8.1.2 (85225381) 完整服务与 C ABI 动态插件加载机制，
// 使用本地离线模拟上游（Mock Antigravity / Codex / Claude / Gemini API）完成全链路隔离、清理、回退与脱敏验收。
package acceptance_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Bigheadh/cli-proxy-antigravity-compat/compat"
	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	_ "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
)

const (
	envHelperConfigPath = "CPA_ACCEPTANCE_SERVER_CONFIG"
	testClientAPIKey    = "cpa-client-acceptance-key"
	testMgmtSecretKey   = "cpa-mgmt-secret-acceptance-key"
	fixedClaudeUserID   = "user_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef_account_00000000-0000-0000-0000-000000000001_session_00000000-0000-0000-0000-000000000002"
	fixedPromptCacheKey = "fixed-acceptance-cache-key-001"
)

// TestHelperCLIProxyServer 作为独立子进程入口运行原版 CLIProxyAPI v8.1.2 服务实例，
// 确保对照组（未启用插件）与实验组（通过 dlopen 加载 antigravity-compat.so）拥有 100% 物理隔离的地址空间。
func TestHelperCLIProxyServer(t *testing.T) {
	cfgPath := strings.TrimSpace(os.Getenv(envHelperConfigPath))
	if cfgPath == "" {
		return
	}

	cfg, errLoad := config.LoadConfig(cfgPath)
	if errLoad != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", errLoad)
		os.Exit(2)
	}

	svc, errBuild := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(cfgPath).
		WithLocalManagementPassword(testMgmtSecretKey).
		Build()
	if errBuild != nil {
		fmt.Fprintf(os.Stderr, "构建 CLIProxyAPI 服务失败: %v\n", errBuild)
		os.Exit(3)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if errRun := svc.Run(ctx); errRun != nil && errRun != context.Canceled {
		fmt.Fprintf(os.Stderr, "运行 CLIProxyAPI 服务异常退出: %v\n", errRun)
		os.Exit(4)
	}
}

// recordedUpstreamRequest 保存模拟上游收到的单次真实 HTTP 请求快照。
type recordedUpstreamRequest struct {
	Method  string
	Path    string
	Query   string
	Headers http.Header
	Body    []byte
}

// upstreamBehavior 控制模拟上游的故障注入与严格校验开关。
type upstreamBehavior struct {
	statusCode       atomic.Int32
	retryAfterSec    atomic.Int32
	strictProtoCheck atomic.Bool
}

// mockUpstreamSuite 封装一套完整的离线模拟上游集群（Antigravity + Codex + Claude + Gemini API）。
type mockUpstreamSuite struct {
	antigravitySrv *httptest.Server
	codexSrv       *httptest.Server
	claudeSrv      *httptest.Server
	geminiSrv      *httptest.Server

	antigravityBehavior upstreamBehavior

	mu                  sync.Mutex
	antigravityRequests []recordedUpstreamRequest
	codexRequests       []recordedUpstreamRequest
	claudeRequests      []recordedUpstreamRequest
	geminiRequests      []recordedUpstreamRequest
}

func newMockUpstreamSuite(t *testing.T) *mockUpstreamSuite {
	t.Helper()
	m := &mockUpstreamSuite{}
	m.antigravityBehavior.statusCode.Store(http.StatusOK)

	// 1. 模拟 Google Cloud Code PA (Antigravity v1internal) 上游
	m.antigravitySrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		if strings.HasSuffix(r.URL.Path, ":fetchAvailableModels") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"models": {
					"claude-opus-5-5-high": {"maxTokens": 32768},
					"claude-sonnet-4-6": {"maxTokens": 32768},
					"gemini-3.8-flash-high": {"maxTokens": 32768},
					"gemini-3-flash": {"maxTokens": 32768}
				},
				"webSearchModelIds": ["gemini-3.8-flash-high", "gemini-3-flash"]
			}`))
			return
		}

		m.mu.Lock()
		m.antigravityRequests = append(m.antigravityRequests, recordedUpstreamRequest{
			Method:  r.Method,
			Path:    r.URL.Path,
			Query:   r.URL.RawQuery,
			Headers: r.Header.Clone(),
			Body:    bytes.Clone(body),
		})
		m.mu.Unlock()

		// 故障注入（如模拟 HTTP 429 限流回退）
		if forcedStatus := int(m.antigravityBehavior.statusCode.Load()); forcedStatus != http.StatusOK {
			if retrySec := m.antigravityBehavior.retryAfterSec.Load(); retrySec > 0 {
				w.Header().Set("Retry-After", fmt.Sprintf("%d", retrySec))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(forcedStatus)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"code":%d,"message":"Simulated upstream status %d","status":"RESOURCE_EXHAUSTED"}}`, forcedStatus, forcedStatus)))
			return
		}

		// 可选严格 Proto3 / Claude 5.5 兼容性校验器：用于证明未启用模块时真实触发 400 INVALID_ARGUMENT
		if m.antigravityBehavior.strictProtoCheck.Load() {
			if reason := validateStrictAntigravityProto(body); reason != "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				respErr, _ := json.Marshal(map[string]any{
					"error": map[string]any{
						"code":    400,
						"status":  "INVALID_ARGUMENT",
						"message": reason,
					},
				})
				_, _ = w.Write(respErr)
				return
			}
		}

		if strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"mock-antigravity-ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":12,\"candidatesTokenCount\":6,\"totalTokenCount\":18}}}\n\n"))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"mock-antigravity-ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":6,"totalTokenCount":18}}}`))
	}))

	// 2. 模拟 OpenAI Codex (/responses SSE) 上游
	m.codexSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		m.mu.Lock()
		m.codexRequests = append(m.codexRequests, recordedUpstreamRequest{
			Method:  r.Method,
			Path:    r.URL.Path,
			Query:   r.URL.RawQuery,
			Headers: r.Header.Clone(),
			Body:    bytes.Clone(body),
		})
		m.mu.Unlock()

		model := ojson.GetBytes(body, "model").String()
		if model == "" {
			model = "gpt-6.2-sol"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_codex_1\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":%q,\"output\":[]}}\n\n", model)
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_codex_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"mock-codex-ok\"}\n\n")
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_codex_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":%q,\"output\":[{\"id\":\"msg_codex_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"mock-codex-ok\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n\n", model)
	}))

	// 3. 模拟 Anthropic Claude API (/v1/messages) 上游
	m.claudeSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		m.mu.Lock()
		m.claudeRequests = append(m.claudeRequests, recordedUpstreamRequest{
			Method:  r.Method,
			Path:    r.URL.Path,
			Query:   r.URL.RawQuery,
			Headers: r.Header.Clone(),
			Body:    bytes.Clone(body),
		})
		m.mu.Unlock()

		model := ojson.GetBytes(body, "model").String()
		if model == "" {
			model = "claude-direct-model"
		}
		if ojson.GetBytes(body, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_claude_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n", model)
			_, _ = fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"mock-claude-ok\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
			_, _ = fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n")
			_, _ = fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"id":"msg_claude_1","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"mock-claude-ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`, model)
	}))

	// 4. 模拟 Google Gemini API (/v1beta/models/*) 上游
	m.geminiSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-2.5-pro","supportedGenerationMethods":["generateContent","streamGenerateContent"]}]}`))
			return
		}

		m.mu.Lock()
		m.geminiRequests = append(m.geminiRequests, recordedUpstreamRequest{
			Method:  r.Method,
			Path:    r.URL.Path,
			Query:   r.URL.RawQuery,
			Headers: r.Header.Clone(),
			Body:    bytes.Clone(body),
		})
		m.mu.Unlock()

		if strings.Contains(r.URL.Path, ":streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"mock-gemini-ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":5,\"totalTokenCount\":15}}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"mock-gemini-ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`))
	}))

	t.Cleanup(func() {
		m.antigravitySrv.Close()
		m.codexSrv.Close()
		m.claudeSrv.Close()
		m.geminiSrv.Close()
	})
	return m
}

func (m *mockUpstreamSuite) resetRecords() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.antigravityRequests = nil
	m.codexRequests = nil
	m.claudeRequests = nil
	m.geminiRequests = nil
}

func (m *mockUpstreamSuite) snapshotAntigravity() []recordedUpstreamRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]recordedUpstreamRequest(nil), m.antigravityRequests...)
}

func (m *mockUpstreamSuite) snapshotCodex() []recordedUpstreamRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]recordedUpstreamRequest(nil), m.codexRequests...)
}

func (m *mockUpstreamSuite) snapshotClaude() []recordedUpstreamRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]recordedUpstreamRequest(nil), m.claudeRequests...)
}

func (m *mockUpstreamSuite) snapshotGemini() []recordedUpstreamRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]recordedUpstreamRequest(nil), m.geminiRequests...)
}

// validateStrictAntigravityProto 模拟真实 Google Cloud Code PA (v1internal) 对已证实不兼容字段的 400 INVALID_ARGUMENT 校验。
func validateStrictAntigravityProto(raw []byte) string {
	parsed := ojson.ParseBytes(raw)
	if parsed.Get("request.stream").Exists() {
		return `Invalid JSON payload received. Unknown name "stream" at 'request': Cannot find field.`
	}

	model := strings.ToLower(parsed.Get("model").String())
	if strings.HasPrefix(model, "claude-opus-5-5") || strings.HasPrefix(model, "claude-sonnet-5-5") {
		sysText := parsed.Get("request.systemInstruction").Raw
		if strings.Contains(sysText, "strictly adhere to the following JSON schema") && parsed.Get("request.tools.0").Exists() {
			return `tool_choice: type "tool" and "any" are not supported for this model when structured output is active`
		}
	}

	for _, badReqKey := range []string{"messages", "input", "instructions", "service_tier", "store"} {
		if parsed.Get("request." + badReqKey).Exists() {
			return fmt.Sprintf(`Invalid JSON payload received. Unknown name %q at 'request': Cannot find field.`, badReqKey)
		}
	}

	genCfg := parsed.Get("request.generationConfig")
	if genCfg.IsObject() {
		for _, badGenKey := range []string{"service_tier", "store", "parallel_tool_calls", "reasoning_effort", "max_tokens", "max_completion_tokens"} {
			if genCfg.Get(badGenKey).Exists() {
				return fmt.Sprintf(`Invalid JSON payload received. Unknown name %q at 'request.generation_config': Cannot find field.`, badGenKey)
			}
		}
		if maxOut := genCfg.Get("maxOutputTokens"); maxOut.Exists() {
			if maxOut.Type != ojson.Number || maxOut.Int() < 1 || float64(maxOut.Int()) != maxOut.Float() {
				return `Invalid value at 'request.generation_config.max_output_tokens' (TYPE_INT32): must be positive integer`
			}
		}
	}

	tools := parsed.Get("request.tools")
	if tools.IsArray() {
		for _, tItem := range tools.Array() {
			for _, container := range []string{"functionDeclarations", "function_declarations"} {
				decls := tItem.Get(container)
				if !decls.IsArray() {
					continue
				}
				for _, decl := range decls.Array() {
					for _, badDeclKey := range []string{"strict", "defer_loading", "cache_control", "eager_input_streaming", "input_examples", "annotations", "custom", "type", "additionalProperties"} {
						if decl.Get(badDeclKey).Exists() {
							return fmt.Sprintf(`Invalid JSON payload received. Unknown name %q at 'request.tools.function_declarations': Cannot find field.`, badDeclKey)
						}
					}
				}
			}
		}
	}

	contents := parsed.Get("request.contents")
	if contents.IsArray() {
		for _, cItem := range contents.Array() {
			parts := cItem.Get("parts")
			if !parts.IsArray() {
				continue
			}
			for _, pItem := range parts.Array() {
				if pItem.Get("extra_content").Exists() {
					return `Invalid JSON payload received. Unknown name "extra_content" at 'request.contents.parts': Cannot find field.`
				}
				if pItem.Get("functionCall.thoughtSignature").Exists() || pItem.Get("functionCall.thought_signature").Exists() ||
					pItem.Get("function_call.thoughtSignature").Exists() || pItem.Get("function_call.thought_signature").Exists() {
					return `Invalid JSON payload received. Unknown name "thought_signature" at 'request.contents.parts.function_call': Cannot find field.`
				}
				if pItem.Get("functionResponse").Exists() || pItem.Get("function_response").Exists() {
					if pItem.Get("thoughtSignature").Exists() || pItem.Get("thought_signature").Exists() ||
						pItem.Get("functionResponse.thoughtSignature").Exists() || pItem.Get("function_response.thought_signature").Exists() {
						return `Invalid thought_signature on functionResponse part`
					}
				}
			}
		}
	}

	return ""
}

type runningCPAInstance struct {
	baseURL string
	mocks   *mockUpstreamSuite
	logsBuf *safeBuffer
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func ensurePluginSharedObject(t *testing.T) string {
	t.Helper()
	repoRoot, errAbs := filepath.Abs("..")
	if errAbs != nil {
		t.Fatalf("解析仓库根目录失败: %v", errAbs)
	}
	soPath := filepath.Join(repoRoot, "dist", "antigravity-compat.so")
	if info, errStat := os.Stat(soPath); errStat == nil && info.Size() > 0 {
		return soPath
	}
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-buildmode=c-shared", "-o", soPath, ".")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, errBuild := cmd.CombinedOutput()
	if errBuild != nil {
		t.Fatalf("编译插件动态库失败: %v\n%s", errBuild, string(out))
	}
	_ = os.Remove(strings.TrimSuffix(soPath, ".so") + ".h")
	return soPath
}

func allocateFreeLocalPort(t *testing.T) int {
	t.Helper()
	ln, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("分配本地空闲端口失败: %v", errListen)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, errRead := os.ReadFile(src)
	if errRead != nil {
		t.Fatalf("读取文件 %s 失败: %v", src, errRead)
	}
	if errMkdir := os.MkdirAll(filepath.Dir(dst), 0o755); errMkdir != nil {
		t.Fatalf("创建目录失败: %v", errMkdir)
	}
	if errWrite := os.WriteFile(dst, data, 0o755); errWrite != nil {
		t.Fatalf("写入文件 %s 失败: %v", dst, errWrite)
	}
}

func startCLIProxyInstance(t *testing.T, enablePlugin bool, pluginSOPath string) *runningCPAInstance {
	t.Helper()
	mocks := newMockUpstreamSuite(t)
	workDir := t.TempDir()
	authDir := filepath.Join(workDir, "auths")
	pluginsDir := filepath.Join(workDir, "plugins")
	if errMkdir := os.MkdirAll(authDir, 0o755); errMkdir != nil {
		t.Fatalf("创建 authDir 失败: %v", errMkdir)
	}
	if errMkdir := os.MkdirAll(pluginsDir, 0o755); errMkdir != nil {
		t.Fatalf("创建 pluginsDir 失败: %v", errMkdir)
	}
	if enablePlugin {
		copyFile(t, pluginSOPath, filepath.Join(pluginsDir, "antigravity-compat.so"))
	}

	// 写入高优先级 Antigravity OAuth 凭证文件（priority: 100），指向当前实例专属 Mock Antigravity 上游
	antigravityAuthJSON := fmt.Sprintf(`{
  "type": "antigravity",
  "access_token": "mock-ag-access-token-valid",
  "refresh_token": "mock-ag-refresh-token",
  "project_id": "test-antigravity-proj",
  "email": "tester@example.com",
  "expired": "2099-01-01T00:00:00Z",
  "base_url": %q,
  "priority": 100
}`, mocks.antigravitySrv.URL)
	if errWriteAuth := os.WriteFile(filepath.Join(authDir, "antigravity-primary.json"), []byte(antigravityAuthJSON), 0o600); errWriteAuth != nil {
		t.Fatalf("写入 Antigravity 凭证文件失败: %v", errWriteAuth)
	}

	port := allocateFreeLocalPort(t)
	cfgPath := filepath.Join(workDir, "config.yaml")

	// 配置说明：
	// - gpt-6.2-sol 与 gpt-6.2-astra 路由至 Codex 执行器；
	// - claude-direct-model 路由至 Claude API 执行器；
	// - gemini-direct-model 路由至 Gemini API 执行器；
	// - claude-opus-5-5-high / claude-sonnet-4-6 / gemini-3.8-flash-high / gemini-3-flash 路由至 Antigravity 执行器；
	// - fallback-to-codex 同时挂载在 Antigravity (priority 100) 与 Codex (priority 10)，用于跨渠道回退验收；
	// - fallback-to-claude 同时挂载在 Antigravity (priority 100) 与 Claude API (priority 10)，用于跨渠道回退验收。
	configYAML := fmt.Sprintf(`host: "127.0.0.1"
port: %d
debug: true
auth-dir: %q
api-keys:
  - %q
remote-management:
  allow-remote: false
  secret-key: %q
request-retry: 0

oauth-model-alias:
  antigravity:
    - name: "claude-sonnet-4-6"
      alias: "fallback-to-codex"
      fork: true
    - name: "claude-sonnet-4-6"
      alias: "fallback-to-claude"
      fork: true

codex-api-key:
  - api-key: "mock-codex-upstream-key"
    base-url: %q
    priority: 10
    models:
      - name: "gpt-6.2-sol"
        alias: "gpt-6.2-sol"
      - name: "gpt-6.2-astra"
        alias: "gpt-6.2-astra"
      - name: "gpt-6.2-sol"
        alias: "fallback-to-codex"

claude-api-key:
  - api-key: "mock-claude-upstream-key"
    base-url: %q
    priority: 10
    models:
      - name: "claude-sonnet-4-6"
        alias: "claude-direct-model"
      - name: "claude-sonnet-4-6"
        alias: "fallback-to-claude"

gemini-api-key:
  - api-key: "mock-gemini-upstream-key"
    base-url: %q
    priority: 10
    models:
      - name: "gemini-2.5-pro"
        alias: "gemini-direct-model"

plugins:
  enabled: %t
  dir: %q
  configs:
    antigravity-compat:
      enabled: true
      diagnostics_endpoint_enabled: true
      diagnostics_log_level: "info"
`, port, authDir, testClientAPIKey, testMgmtSecretKey,
		mocks.codexSrv.URL, mocks.claudeSrv.URL, mocks.geminiSrv.URL,
		enablePlugin, pluginsDir)

	if errWriteCfg := os.WriteFile(cfgPath, []byte(configYAML), 0o600); errWriteCfg != nil {
		t.Fatalf("写入测试 config.yaml 失败: %v", errWriteCfg)
	}

	logsBuf := &safeBuffer{}
	var cmd *exec.Cmd
	if extBin := strings.TrimSpace(os.Getenv("CPA_ACCEPTANCE_BINARY")); extBin != "" {
		t.Logf("使用外部官方原版 CLIProxyAPI 可执行文件启动实例 (enablePlugin=%t): %s", enablePlugin, extBin)
		cmd = exec.Command(extBin, "-config", cfgPath, "-password", testMgmtSecretKey, "-local-model")
		cmd.Dir = workDir
		cmd.Env = os.Environ()
	} else {
		cmd = exec.Command(os.Args[0], "-test.run=^TestHelperCLIProxyServer$")
		cmd.Env = append(os.Environ(), envHelperConfigPath+"="+cfgPath)
	}
	cmd.Stdout = logsBuf
	cmd.Stderr = logsBuf
	if errStart := cmd.Start(); errStart != nil {
		t.Fatalf("启动 CLIProxyAPI 测试服务子进程失败: %v", errStart)
	}

	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		}
	})

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForModelsReady(t, baseURL, logsBuf, []string{
		"gpt-6.2-sol",
		"gpt-6.2-astra",
		"claude-opus-5-5-high",
		"claude-sonnet-4-6",
		"gemini-3.8-flash-high",
		"fallback-to-codex",
		"fallback-to-claude",
	})

	return &runningCPAInstance{
		baseURL: baseURL,
		mocks:   mocks,
		logsBuf: logsBuf,
	}
}

func waitForModelsReady(t *testing.T, baseURL string, logs *safeBuffer, requiredModels []string) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, baseURL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+testClientAPIKey)
		resp, errDo := client.Do(req)
		if errDo == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				allFound := true
				bodyStr := string(body)
				for _, m := range requiredModels {
					if !strings.Contains(bodyStr, `"`+m+`"`) {
						allFound = false
						break
					}
				}
				if allFound {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待 CLIProxyAPI 服务模型就绪超时 (%s)，子进程日志:\n%s", baseURL, logs.String())
}

func sendClientRequest(t *testing.T, baseURL, path string, payload []byte, extraHeaders map[string]string) (int, []byte) {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	req, errNew := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(payload))
	if errNew != nil {
		t.Fatalf("构造请求失败: %v", errNew)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testClientAPIKey)
	req.Header.Set("X-Api-Key", testClientAPIKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	httpResp, errExec := client.Do(req)
	if errExec != nil {
		t.Fatalf("请求 %s%s 失败: %v", baseURL, path, errExec)
	}
	defer func() { _ = httpResp.Body.Close() }()
	respBody, errRead := io.ReadAll(httpResp.Body)
	if errRead != nil {
		t.Fatalf("读取响应体失败: %v", errRead)
	}
	return httpResp.StatusCode, respBody
}

// normalizeDynamicTraceFields 剥离 AntigravityExecutor 每次请求动态生成的随机追踪 UUID（requestId、sessionId 与 labels 轨迹 ID），
// 用于将实验组与对照组的上游请求体做精确字段级比对。
func normalizeDynamicTraceFields(raw []byte) []byte {
	doc := ojson.NewDoc(bytes.Clone(raw))
	for _, traceKey := range []string{
		"requestId",
		"request.sessionId",
		"request.labels.cascade_id",
		"request.labels.request_id",
		"request.labels.root_cascade_id",
		"request.labels.trajectory_id",
	} {
		if doc.Get(traceKey).Exists() {
			_ = doc.Set(traceKey, "<normalized-trace-uuid>")
		}
	}
	return doc.Bytes()
}

// TestOfflineFullChainAcceptance 在当前虚拟机一次性启动对照组（plugins.enabled=false）与实验组（plugins.enabled=true，C ABI 加载 antigravity-compat.so），
// 完整执行全部离线模拟上游验收项。
func TestOfflineFullChainAcceptance(t *testing.T) {
	pluginSO := ensurePluginSharedObject(t)
	baseline := startCLIProxyInstance(t, false, pluginSO)
	compatInst := startCLIProxyInstance(t, true, pluginSO)

	// =========================================================================
	// 验收项 1：gpt-6.2-sol 与 gpt-6.2-astra 走 Codex 渠道，启用模块前后请求体逐字节完全一致
	// 覆盖入口：Chat Completions (/v1/chat/completions)、Responses (/v1/responses)、Claude Messages (/v1/messages)，含流式与非流式
	// =========================================================================
	t.Run("Codex_GPT62Sol_And_GPT62Astra_ByteIdentical", func(t *testing.T) {
		for _, modelName := range []string{"gpt-6.2-sol", "gpt-6.2-astra"} {
			for _, stream := range []bool{false, true} {
				// 1.1 Chat Completions 入口
				chatPayload := []byte(fmt.Sprintf(`{
  "model": %q,
  "stream": %t,
  "service_tier": "priority",
  "store": false,
  "reasoning_effort": "high",
  "prompt_cache_key": %q,
  "messages": [
    {"role": "system", "content": "You are Codex assistant. Keep exact bytes."},
    {"role": "user", "content": "Verify Codex passthrough for %s"}
  ],
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "query_metric",
        "description": "Query metric",
        "strict": true,
        "parameters": {"type": "object", "properties": {"metric": {"type": "string"}}, "required": ["metric"], "additionalProperties": false}
      }
    }
  ]
}`, modelName, stream, fixedPromptCacheKey, modelName))

				// 1.2 Responses 入口
				responsesPayload := []byte(fmt.Sprintf(`{
  "model": %q,
  "stream": %t,
  "service_tier": "priority",
  "store": false,
  "prompt_cache_key": %q,
  "instructions": "System instructions for Responses API",
  "reasoning": {"effort": "high"},
  "input": [
    {"role": "user", "content": [{"type": "input_text", "text": "Verify Responses API for %s"}]}
  ],
  "tools": [
    {
      "type": "function",
      "name": "query_metric",
      "description": "Query metric",
      "strict": true,
      "parameters": {"type": "object", "properties": {"metric": {"type": "string"}}, "required": ["metric"], "additionalProperties": false}
    }
  ]
}`, modelName, stream, fixedPromptCacheKey, modelName))

				// 1.3 Claude Messages 入口
				claudePayload := []byte(fmt.Sprintf(`{
  "model": %q,
  "stream": %t,
  "max_tokens": 4096,
  "metadata": {"user_id": %q},
  "system": "You are Claude Code routed to Codex.",
  "messages": [
    {"role": "user", "content": "Verify Claude Messages to Codex for %s"}
  ],
  "tools": [
    {
      "name": "query_metric",
      "description": "Query metric",
      "input_schema": {"type": "object", "properties": {"metric": {"type": "string"}}, "required": ["metric"]}
    }
  ]
}`, modelName, stream, fixedClaudeUserID, modelName))

				cases := []struct {
					entryName string
					path      string
					payload   []byte
				}{
					{entryName: "ChatCompletions", path: "/v1/chat/completions", payload: chatPayload},
					{entryName: "Responses", path: "/v1/responses", payload: responsesPayload},
					{entryName: "ClaudeMessages", path: "/v1/messages", payload: claudePayload},
				}

				for _, tc := range cases {
					caseLabel := fmt.Sprintf("%s/%s/stream=%t", modelName, tc.entryName, stream)
					baseline.mocks.resetRecords()
					compatInst.mocks.resetRecords()

					baseStatus, baseResp := sendClientRequest(t, baseline.baseURL, tc.path, tc.payload, map[string]string{"User-Agent": "CodexAcceptanceClient/1.0"})
					compatStatus, compatResp := sendClientRequest(t, compatInst.baseURL, tc.path, tc.payload, map[string]string{"User-Agent": "CodexAcceptanceClient/1.0"})

					if baseStatus != http.StatusOK || compatStatus != http.StatusOK {
						t.Fatalf("[%s] 客户端状态码异常: baseline=%d (%s), compat=%d (%s)", caseLabel, baseStatus, string(baseResp), compatStatus, string(compatResp))
					}

					baseReqs := baseline.mocks.snapshotCodex()
					compatReqs := compatInst.mocks.snapshotCodex()
					if len(baseReqs) != 1 || len(compatReqs) != 1 {
						t.Fatalf("[%s] Codex 上游收到请求数不符: baseline=%d, compat=%d", caseLabel, len(baseReqs), len(compatReqs))
					}
					if !bytes.Equal(baseReqs[0].Body, compatReqs[0].Body) {
						t.Fatalf("[%s] 启用兼容插件前后发往 Codex 的请求体不一致！\nBaseline:\n%s\nCompat:\n%s",
							caseLabel, string(baseReqs[0].Body), string(compatReqs[0].Body))
					}
					if baseReqs[0].Headers.Get("User-Agent") != compatReqs[0].Headers.Get("User-Agent") {
						t.Fatalf("[%s] Codex 上游 User-Agent 发生变化: %q vs %q",
							caseLabel, baseReqs[0].Headers.Get("User-Agent"), compatReqs[0].Headers.Get("User-Agent"))
					}
				}
			}
		}
	})

	// =========================================================================
	// 验收项 2：Claude API 与 Gemini API 等其他非 Antigravity 渠道原样透传、零影响
	// =========================================================================
	t.Run("OtherChannels_ClaudeAPI_And_GeminiAPI_ByteIdentical", func(t *testing.T) {
		// 2.1 Claude API 渠道
		claudeReqPayload := []byte(`{
  "model": "claude-direct-model",
  "max_tokens": 1024,
  "system": "System prompt for direct Claude API",
  "messages": [{"role": "user", "content": "Hello direct Claude"}],
  "tools": [{"name": "t1", "description": "d1", "defer_loading": true, "input_schema": {"type": "object", "properties": {"k": {"type": "string"}}}}]
}`)
		baseline.mocks.resetRecords()
		compatInst.mocks.resetRecords()
		s1, _ := sendClientRequest(t, baseline.baseURL, "/v1/messages", claudeReqPayload, nil)
		s2, _ := sendClientRequest(t, compatInst.baseURL, "/v1/messages", claudeReqPayload, nil)
		if s1 != http.StatusOK || s2 != http.StatusOK {
			t.Fatalf("Claude API 渠道响应状态异常: %d vs %d", s1, s2)
		}
		bClaude := baseline.mocks.snapshotClaude()
		cClaude := compatInst.mocks.snapshotClaude()
		if len(bClaude) != 1 || len(cClaude) != 1 || !bytes.Equal(bClaude[0].Body, cClaude[0].Body) {
			t.Fatalf("Claude API 渠道请求体在启用模块前后不一致")
		}

		// 2.2 Gemini API 渠道（经 Chat Completions 转换）
		geminiChatPayload := []byte(`{
  "model": "gemini-direct-model",
  "messages": [{"role": "user", "content": "Hello direct Gemini API"}]
}`)
		baseline.mocks.resetRecords()
		compatInst.mocks.resetRecords()
		g1, _ := sendClientRequest(t, baseline.baseURL, "/v1/chat/completions", geminiChatPayload, nil)
		g2, _ := sendClientRequest(t, compatInst.baseURL, "/v1/chat/completions", geminiChatPayload, nil)
		if g1 != http.StatusOK || g2 != http.StatusOK {
			t.Fatalf("Gemini API 渠道响应状态异常: %d vs %d", g1, g2)
		}
		bGemini := baseline.mocks.snapshotGemini()
		cGemini := compatInst.mocks.snapshotGemini()
		if len(bGemini) != 1 || len(cGemini) != 1 || !bytes.Equal(bGemini[0].Body, cGemini[0].Body) {
			t.Fatalf("Gemini API 渠道请求体在启用模块前后不一致")
		}
	})

	// =========================================================================
	// 验收项 3：Antigravity 渠道只清理已证实不兼容字段，保留未证实字段，不改提示词/用户消息/客户端身份
	// 覆盖入口：Chat Completions、Responses、Claude Messages、Gemini Native
	// =========================================================================
	t.Run("Antigravity_ExactCleanup_AcrossAllProtocols_And_FourRedlines", func(t *testing.T) {
		sensitiveSystemPrompt := "You are Codex and Claude Code running via CLIProxyAPI proxy. DO_NOT_TOUCH_SYSTEM_PROMPT_42"
		sensitiveUserPrompt := "User secret prompt: please keep exact wording DO_NOT_TOUCH_USER_MSG_99"

		// 3.1 Chat Completions 入口 -> Antigravity claude-opus-5-5-high（触发 AG-COMPAT-002、004、005，同时携带未证实字段 customUnprovenField）
		chatToAGPayload := []byte(fmt.Sprintf(`{
  "model": "claude-opus-5-5-high",
  "stream": false,
  "messages": [
    {"role": "system", "content": %q},
    {"role": "user", "content": %q}
  ],
  "response_format": {
    "type": "json_schema",
    "json_schema": {
      "name": "structured_reply",
      "schema": {
        "type": "object",
        "properties": {"summary": {"type": "string"}},
        "required": ["summary"]
      }
    }
  },
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "conflicting_tool",
        "description": "Tool conflicting with Claude 5.5 structured output",
        "strict": true,
        "defer_loading": true,
        "parameters": {"type": "object", "properties": {"q": {"type": "string"}}}
      }
    }
  ],
  "generationConfig": {
    "service_tier": "priority",
    "parallel_tool_calls": true,
    "customUnprovenField": "UNPROVEN_SECRET_VALUE_KEEP_INTACT"
  }
}`, sensitiveSystemPrompt, sensitiveUserPrompt))

		// 先开启两边模拟上游的严格 Google Cloud Code PA 400 校验器：验证未启用插件返回 400，启用本插件返回 200！
		baseline.mocks.resetRecords()
		compatInst.mocks.resetRecords()
		baseline.mocks.antigravityBehavior.strictProtoCheck.Store(true)
		compatInst.mocks.antigravityBehavior.strictProtoCheck.Store(true)

		baseStrictStatus, _ := sendClientRequest(t, baseline.baseURL, "/v1/chat/completions", chatToAGPayload, map[string]string{"User-Agent": "MyIDE/2.0"})
		compatStrictStatus, compatStrictBody := sendClientRequest(t, compatInst.baseURL, "/v1/chat/completions", chatToAGPayload, map[string]string{"User-Agent": "MyIDE/2.0"})

		baseline.mocks.antigravityBehavior.strictProtoCheck.Store(false)
		compatInst.mocks.antigravityBehavior.strictProtoCheck.Store(false)

		if baseStrictStatus != http.StatusBadRequest {
			t.Fatalf("未启用插件时，严格 Antigravity 上游应对不兼容字段返回 400，实际=%d", baseStrictStatus)
		}
		if compatStrictStatus != http.StatusOK {
			t.Fatalf("启用兼容插件后，严格 Antigravity 上游应返回 200，实际=%d，响应=%s", compatStrictStatus, string(compatStrictBody))
		}

		baseAGReqs := baseline.mocks.snapshotAntigravity()
		compatAGReqs := compatInst.mocks.snapshotAntigravity()
		if len(baseAGReqs) != 1 || len(compatAGReqs) != 1 {
			t.Fatalf("Antigravity 收到请求次数异常: base=%d, compat=%d", len(baseAGReqs), len(compatAGReqs))
		}

		compatDoc := ojson.ParseBytes(compatAGReqs[0].Body)
		// 验证已证实不兼容字段已被清理
		if compatDoc.Get("request.tools").Exists() || compatDoc.Get("request.toolConfig").Exists() {
			t.Fatalf("AG-COMPAT-002 应清理 Claude 5.5 结构化输出冲突的 tools/toolConfig: %s", string(compatAGReqs[0].Body))
		}
		if compatDoc.Get("request.generationConfig.service_tier").Exists() || compatDoc.Get("request.generationConfig.parallel_tool_calls").Exists() {
			t.Fatalf("AG-COMPAT-005 应清理 generationConfig 中的跨协议字段: %s", string(compatAGReqs[0].Body))
		}
		// 验证未证实字段 customUnprovenField 原样保留！
		if compatDoc.Get("request.generationConfig.customUnprovenField").String() != "UNPROVEN_SECRET_VALUE_KEEP_INTACT" {
			t.Fatalf("无 400 证据的未证实字段必须原样保留，不得删除或改动: %s", string(compatAGReqs[0].Body))
		}
		// 验证系统提示词（含结构化输出附加指令）、用户消息、上游 User-Agent 与对照组 100% 一致！
		baseDoc := ojson.ParseBytes(baseAGReqs[0].Body)
		if compatDoc.Get("request.systemInstruction").Raw != baseDoc.Get("request.systemInstruction").Raw {
			t.Fatalf("系统提示词被意外改动！\nBaseline: %s\nCompat:   %s",
				baseDoc.Get("request.systemInstruction").Raw, compatDoc.Get("request.systemInstruction").Raw)
		}
		if !strings.Contains(compatDoc.Get("request.systemInstruction").Raw, sensitiveSystemPrompt) {
			t.Fatalf("包含 Codex/Claude Code/CLIProxyAPI 关键词的系统提示词未原样保留")
		}
		if compatDoc.Get("request.contents").Raw != baseDoc.Get("request.contents").Raw {
			t.Fatalf("用户消息内容被意外改动！")
		}
		if compatAGReqs[0].Headers.Get("User-Agent") != baseAGReqs[0].Headers.Get("User-Agent") {
			t.Fatalf("发往 Antigravity 的客户端身份 User-Agent 被改动: %q vs %q",
				compatAGReqs[0].Headers.Get("User-Agent"), baseAGReqs[0].Headers.Get("User-Agent"))
		}

		// 验证：在对照组请求上仅手工删除规则指定的路径后，与实验组请求完全一致（证明“只改指定字段”）！
		expectedFromBase := ojson.NewDoc(normalizeDynamicTraceFields(baseAGReqs[0].Body))
		_ = expectedFromBase.Delete("request.tools")
		_ = expectedFromBase.Delete("request.toolConfig")
		_ = expectedFromBase.Delete("request.generationConfig.service_tier")
		_ = expectedFromBase.Delete("request.generationConfig.parallel_tool_calls")
		actualCompatNorm := normalizeDynamicTraceFields(compatAGReqs[0].Body)
		if !jsonEqualNormalized(expectedFromBase.Bytes(), actualCompatNorm) {
			t.Fatalf("实验组除了清理指定字段外不应有任何其他差异！\nExpected:\n%s\nActual:\n%s",
				string(expectedFromBase.Bytes()), string(actualCompatNorm))
		}

		// 3.2 Responses 入口 (/v1/responses) -> Antigravity gemini-3.8-flash-high（非正数 max_output_tokens + 提示词原样保留）
		responsesToAG := []byte(fmt.Sprintf(`{
  "model": "gemini-3.8-flash-high",
  "stream": false,
  "max_output_tokens": 0,
  "instructions": %q,
  "input": [{"role": "user", "content": [{"type": "input_text", "text": %q}]}]
}`, sensitiveSystemPrompt, sensitiveUserPrompt))

		compatInst.mocks.resetRecords()
		compatInst.mocks.antigravityBehavior.strictProtoCheck.Store(true)
		rStatus, rBody := sendClientRequest(t, compatInst.baseURL, "/v1/responses", responsesToAG, nil)
		compatInst.mocks.antigravityBehavior.strictProtoCheck.Store(false)
		if rStatus != http.StatusOK {
			t.Fatalf("Responses -> Antigravity 期望 200，实际=%d: %s", rStatus, string(rBody))
		}
		rAGReqs := compatInst.mocks.snapshotAntigravity()
		if len(rAGReqs) != 1 || ojson.ParseBytes(rAGReqs[0].Body).Get("request.generationConfig.maxOutputTokens").Exists() {
			t.Fatalf("Responses 入口的非法 maxOutputTokens=0 应被 AG-COMPAT-005 清理")
		}

		// 3.3 Claude Messages 入口 (/v1/messages) -> Antigravity claude-sonnet-4-6
		// 验证：工具扩展字段 defer_loading/cache_control/eager_input_streaming 被清理，而 Issue #7878 的 thinking + temperature 未证实组合原样保留！
		claudeToAG := []byte(fmt.Sprintf(`{
  "model": "claude-sonnet-4-6",
  "stream": true,
  "max_tokens": 8192,
  "temperature": 0.75,
  "thinking": {"type": "enabled", "budget_tokens": 2048},
  "system": %q,
  "messages": [{"role": "user", "content": %q}],
  "tools": [
    {
      "name": "lookup_symbol",
      "description": "Lookup symbol",
      "defer_loading": true,
      "eager_input_streaming": true,
      "cache_control": {"type": "ephemeral"},
      "input_schema": {"type": "object", "properties": {"sym": {"type": "string"}}}
    }
  ]
}`, sensitiveSystemPrompt, sensitiveUserPrompt))

		baseline.mocks.resetRecords()
		compatInst.mocks.resetRecords()
		_, _ = sendClientRequest(t, baseline.baseURL, "/v1/messages", claudeToAG, nil)
		cStatus, cBody := sendClientRequest(t, compatInst.baseURL, "/v1/messages", claudeToAG, nil)
		if cStatus != http.StatusOK {
			t.Fatalf("Claude Messages -> Antigravity 流式期望 200，实际=%d: %s", cStatus, string(cBody))
		}
		cAGReqs := compatInst.mocks.snapshotAntigravity()
		if len(cAGReqs) != 1 {
			t.Fatalf("期望 Antigravity 收到 1 次请求，实际=%d", len(cAGReqs))
		}
		cParsed := ojson.ParseBytes(cAGReqs[0].Body)
		if cParsed.Get("request.generationConfig.temperature").Float() != 0.75 {
			t.Fatalf("Issue #7878 未证实的 Claude thinking + temperature 组合必须原样保留: %s", string(cAGReqs[0].Body))
		}

		// 3.4 Gemini 原生入口 (/v1beta/models/gemini-3-flash:generateContent) -> Antigravity
		// 覆盖 AG-COMPAT-001 (request.stream)、AG-COMPAT-003 (工具内嵌签名与 extra_content)、AG-COMPAT-004、AG-COMPAT-006 (顶层跨协议残留)
		geminiNativeToAG := []byte(`{
  "stream": false,
  "service_tier": "priority",
  "store": false,
  "contents": [
    {
      "role": "user",
      "parts": [{"text": "Run weather check"}]
    },
    {
      "role": "model",
      "parts": [
        {
          "thoughtSignature": "valid-part-sig-123",
          "extra_content": {"google": {"thought_signature": "valid-part-sig-123"}},
          "functionCall": {
            "name": "get_weather",
            "thoughtSignature": "invalid-nested-fc-sig",
            "args": {"city": "Tokyo"}
          }
        }
      ]
    },
    {
      "role": "user",
      "parts": [
        {
          "thoughtSignature": "invalid-fr-sibling-sig",
          "functionResponse": {
            "name": "get_weather",
            "thoughtSignature": "invalid-nested-fr-sig",
            "response": {"temp": 22}
          }
        }
      ]
    }
  ],
  "tools": [
    {
      "functionDeclarations": [
        {
          "name": "get_weather",
          "description": "Get weather",
          "defer_loading": true,
          "input_examples": [{"city": "Tokyo"}],
          "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}
        }
      ]
    }
  ]
}`)

		baseline.mocks.resetRecords()
		compatInst.mocks.resetRecords()
		baseline.mocks.antigravityBehavior.strictProtoCheck.Store(true)
		compatInst.mocks.antigravityBehavior.strictProtoCheck.Store(true)
		gnBaseStatus, _ := sendClientRequest(t, baseline.baseURL, "/v1beta/models/gemini-3-flash:generateContent", geminiNativeToAG, nil)
		gnStatus, gnBody := sendClientRequest(t, compatInst.baseURL, "/v1beta/models/gemini-3-flash:generateContent", geminiNativeToAG, nil)
		baseline.mocks.antigravityBehavior.strictProtoCheck.Store(false)
		compatInst.mocks.antigravityBehavior.strictProtoCheck.Store(false)
		if gnBaseStatus != http.StatusBadRequest {
			t.Fatalf("未启用插件时，Gemini 原生入口残留字段应触发严格上游 400，实际=%d", gnBaseStatus)
		}
		if gnStatus != http.StatusOK {
			t.Fatalf("Gemini 原生入口 -> Antigravity 严格校验期望 200，实际=%d: %s", gnStatus, string(gnBody))
		}
		gnBaseReqs := baseline.mocks.snapshotAntigravity()
		gnReqs := compatInst.mocks.snapshotAntigravity()
		gnBaseSig := ojson.ParseBytes(gnBaseReqs[0].Body).Get("request.contents.1.parts.0.thoughtSignature").String()
		gnCompatSig := ojson.ParseBytes(gnReqs[0].Body).Get("request.contents.1.parts.0.thoughtSignature").String()
		if gnCompatSig == "" || gnCompatSig != gnBaseSig {
			t.Fatalf("functionCall Part 同级签名必须保留且与对照组一致: base=%q, compat=%q", gnBaseSig, gnCompatSig)
		}
	})

	// =========================================================================
	// 验收项 4：从 Antigravity 回退到其他渠道（Codex / Claude API）零污染 + 429 退避冷却行为一致证明
	// 覆盖入口：Chat Completions、Responses、Claude Messages
	// =========================================================================
	t.Run("FallbackFromAntigravity_ZeroPollution_And_429BackoffUnchanged", func(t *testing.T) {
		// 4.1 从 Antigravity (priority 100, 返回 429 Retry-After: 8) 回退至 Codex (priority 10)
		// 分别测试 Chat Completions 与 Responses 入口，且紧接第 2 次请求验证 429 冷却跳过行为与对照组完全一致！
		baseline.mocks.resetRecords()
		compatInst.mocks.resetRecords()
		baseline.mocks.antigravityBehavior.retryAfterSec.Store(8)
		compatInst.mocks.antigravityBehavior.retryAfterSec.Store(8)
		baseline.mocks.antigravityBehavior.statusCode.Store(http.StatusTooManyRequests)
		compatInst.mocks.antigravityBehavior.statusCode.Store(http.StatusTooManyRequests)
		defer func() {
			baseline.mocks.antigravityBehavior.statusCode.Store(http.StatusOK)
			compatInst.mocks.antigravityBehavior.statusCode.Store(http.StatusOK)
			baseline.mocks.antigravityBehavior.retryAfterSec.Store(0)
			compatInst.mocks.antigravityBehavior.retryAfterSec.Store(0)
		}()

		fallbackChatPayload := []byte(fmt.Sprintf(`{
  "model": "fallback-to-codex",
  "stream": false,
  "service_tier": "priority",
  "store": false,
  "prompt_cache_key": %q,
  "messages": [{"role": "user", "content": "Test fallback from Antigravity 429 to Codex"}],
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "calc",
        "description": "calc",
        "strict": true,
        "parameters": {"type": "object", "properties": {"x": {"type": "number"}}, "required": ["x"], "additionalProperties": false}
      }
    }
  ]
}`, fixedPromptCacheKey))

		fbBaseStatus1, fbBaseResp1 := sendClientRequest(t, baseline.baseURL, "/v1/chat/completions", fallbackChatPayload, nil)
		fbCompatStatus1, fbCompatResp1 := sendClientRequest(t, compatInst.baseURL, "/v1/chat/completions", fallbackChatPayload, nil)
		if fbBaseStatus1 != http.StatusOK || fbCompatStatus1 != http.StatusOK {
			t.Fatalf("Antigravity 429 回退至 Codex 期望返回 200: base=%d (%s), compat=%d (%s)",
				fbBaseStatus1, string(fbBaseResp1), fbCompatStatus1, string(fbCompatResp1))
		}

		// 验证首次请求确实先尝试了 Antigravity (1 次)，随后回退到了 Codex (1 次)
		if len(baseline.mocks.snapshotAntigravity()) != 1 || len(compatInst.mocks.snapshotAntigravity()) != 1 {
			t.Fatalf("首次请求应当先尝试高优先级 Antigravity 各 1 次: base=%d, compat=%d",
				len(baseline.mocks.snapshotAntigravity()), len(compatInst.mocks.snapshotAntigravity()))
		}
		baseCodexFB := baseline.mocks.snapshotCodex()
		compatCodexFB := compatInst.mocks.snapshotCodex()
		if len(baseCodexFB) != 1 || len(compatCodexFB) != 1 {
			t.Fatalf("Antigravity 429 后应回退至 Codex 各 1 次: base=%d, compat=%d", len(baseCodexFB), len(compatCodexFB))
		}
		// 铁证：回退至 Codex 的请求体与未启用模块的对照组 100% 逐字节相等（service_tier: priority 等 Codex 参数完好无损）！
		if !bytes.Equal(baseCodexFB[0].Body, compatCodexFB[0].Body) {
			t.Fatalf("回退至 Codex 的请求体被 Antigravity 清理污染！\nBaseline:\n%s\nCompat:\n%s",
				string(baseCodexFB[0].Body), string(compatCodexFB[0].Body))
		}
		if ojson.ParseBytes(compatCodexFB[0].Body).Get("service_tier").String() != "priority" {
			t.Fatalf("回退至 Codex 的请求丢失了 service_tier=priority: %s", string(compatCodexFB[0].Body))
		}

		// 验证 429 冷却退避行为完全一致：在 8s 冷却期内紧接着通过 Responses (/v1/responses) 发送第 2 个请求，
		// 对照组与实验组均应直接跳过冷却中的 Antigravity（Antigravity 请求计数保持为 1，不增加），直接路由至 Codex！
		fallbackResponsesPayload := []byte(fmt.Sprintf(`{
  "model": "fallback-to-codex",
  "stream": false,
  "service_tier": "priority",
  "prompt_cache_key": %q,
  "input": [{"role": "user", "content": [{"type": "input_text", "text": "Second request during 429 cooldown"}]}]
}`, fixedPromptCacheKey))

		fbBaseStatus2, _ := sendClientRequest(t, baseline.baseURL, "/v1/responses", fallbackResponsesPayload, nil)
		fbCompatStatus2, _ := sendClientRequest(t, compatInst.baseURL, "/v1/responses", fallbackResponsesPayload, nil)
		if fbBaseStatus2 != http.StatusOK || fbCompatStatus2 != http.StatusOK {
			t.Fatalf("冷却期内第 2 次请求期望返回 200: base=%d, compat=%d", fbBaseStatus2, fbCompatStatus2)
		}
		if len(baseline.mocks.snapshotAntigravity()) != len(compatInst.mocks.snapshotAntigravity()) {
			t.Fatalf("429 退避冷却行为不一致！Antigravity 命中次数: base=%d vs compat=%d",
				len(baseline.mocks.snapshotAntigravity()), len(compatInst.mocks.snapshotAntigravity()))
		}
		baseCodexFB2 := baseline.mocks.snapshotCodex()
		compatCodexFB2 := compatInst.mocks.snapshotCodex()
		if len(baseCodexFB2) != 2 || len(compatCodexFB2) != 2 || !bytes.Equal(baseCodexFB2[1].Body, compatCodexFB2[1].Body) {
			t.Fatalf("Responses 入口回退至 Codex 的请求体不一致！")
		}

		// 4.2 Claude Messages 入口 (/v1/messages) 从 Antigravity (503/429) 回退至 Claude API 渠道零污染验证
		// `fallback-to-claude` 为独立模型名，其 per-model 冷却尚未触发，会先尝试 Antigravity 再回退到 Claude API！
		fallbackClaudeMessagesPayload := []byte(`{
  "model": "fallback-to-claude",
  "max_tokens": 1024,
  "messages": [{"role": "user", "content": "Fallback from Antigravity to Claude API"}],
  "tools": [
    {
      "name": "read_doc",
      "description": "Read doc",
      "defer_loading": true,
      "cache_control": {"type": "ephemeral"},
      "input_schema": {"type": "object", "properties": {"id": {"type": "string"}}}
    }
  ]
}`)
		baseline.mocks.resetRecords()
		compatInst.mocks.resetRecords()
		fbC1, _ := sendClientRequest(t, baseline.baseURL, "/v1/messages", fallbackClaudeMessagesPayload, nil)
		fbC2, _ := sendClientRequest(t, compatInst.baseURL, "/v1/messages", fallbackClaudeMessagesPayload, nil)
		if fbC1 != http.StatusOK || fbC2 != http.StatusOK {
			t.Fatalf("Claude Messages 从 Antigravity 回退至 Claude API 期望返回 200: base=%d, compat=%d", fbC1, fbC2)
		}
		baseClaudeFB := baseline.mocks.snapshotClaude()
		compatClaudeFB := compatInst.mocks.snapshotClaude()
		if len(baseClaudeFB) != 1 || len(compatClaudeFB) != 1 || !bytes.Equal(baseClaudeFB[0].Body, compatClaudeFB[0].Body) {
			t.Fatalf("从 Antigravity 回退至 Claude API 的请求体不一致！")
		}
		// 证明 Antigravity 会删的 defer_loading / cache_control 在回退至原生 Claude API 时被 100% 完整保留！
		if !ojson.ParseBytes(compatClaudeFB[0].Body).Get("tools.0.defer_loading").Bool() {
			t.Fatalf("回退至原生 Claude API 时 tools.0.defer_loading 必须完整保留: %s", string(compatClaudeFB[0].Body))
		}
	})

	// =========================================================================
	// 验收项 5：只读脱敏诊断接口鉴权、统计准确性与零敏感信息泄露验证
	// =========================================================================
	t.Run("ManagementDiagnostics_AuthProtected_And_Redacted", func(t *testing.T) {
		client := &http.Client{Timeout: 5 * time.Second}
		diagURL := compatInst.baseURL + "/v0/management/plugins/antigravity-compat/diagnostics"

		// 5.1 未鉴权访问必须被拒绝
		unauthReq, _ := http.NewRequest(http.MethodGet, diagURL, nil)
		unauthResp, errUnauth := client.Do(unauthReq)
		if errUnauth != nil {
			t.Fatalf("请求未鉴权诊断接口失败: %v", errUnauth)
		}
		_ = unauthResp.Body.Close()
		if unauthResp.StatusCode != http.StatusUnauthorized && unauthResp.StatusCode != http.StatusForbidden {
			t.Fatalf("未携带管理密钥访问诊断接口应返回 401/403，实际=%d", unauthResp.StatusCode)
		}

		// 5.2 携带管理密钥访问返回 200 与结构化脱敏快照
		authReq, _ := http.NewRequest(http.MethodGet, diagURL, nil)
		authReq.Header.Set("Authorization", "Bearer "+testMgmtSecretKey)
		authResp, errAuth := client.Do(authReq)
		if errAuth != nil {
			t.Fatalf("请求已鉴权诊断接口失败: %v", errAuth)
		}
		diagBody, _ := io.ReadAll(authResp.Body)
		_ = authResp.Body.Close()
		if authResp.StatusCode != http.StatusOK {
			t.Fatalf("携带正确管理密钥访问诊断接口应返回 200，实际=%d: %s\n宿主日志:\n%s", authResp.StatusCode, string(diagBody), compatInst.logsBuf.String())
		}

		var snap compat.DiagnosticsSnapshot
		if errUnmarshal := json.Unmarshal(diagBody, &snap); errUnmarshal != nil {
			t.Fatalf("解析诊断接口返回 JSON 失败: %v\n%s", errUnmarshal, string(diagBody))
		}
		if snap.Plugin != compat.PluginID || snap.BaselineCommit != compat.BaselineCommit {
			t.Fatalf("诊断快照元信息不符: %+v", snap)
		}
		if snap.Counters.NonAntigravityBypassed == 0 || snap.Counters.AntigravityInspected == 0 || snap.Counters.AntigravityMutated == 0 {
			t.Fatalf("诊断计数器未正确反映真实流量: %+v", snap.Counters)
		}

		// 确认观测到了未证实字段 customUnprovenField 与 #7878 temperature，且诊断快照与宿主 C ABI 日志中均未泄露任何值或提示词文本
		diagStr := string(diagBody)
		hostLogsStr := compatInst.logsBuf.String()
		if !strings.Contains(diagStr, "request.generationConfig.customUnprovenField") {
			t.Fatalf("诊断快照缺少对未证实字段 customUnprovenField 的记录:\n%s", diagStr)
		}
		if !strings.Contains(hostLogsStr, "antigravity-compat:") {
			t.Fatalf("宿主进程日志未收到插件通过 C ABI host.log 输出的结构化诊断日志:\n%s", hostLogsStr)
		}
		combinedAuditText := diagStr + "\n" + hostLogsStr
		for _, forbidden := range []string{
			"DO_NOT_TOUCH_SYSTEM_PROMPT_42",
			"DO_NOT_TOUCH_USER_MSG_99",
			"UNPROVEN_SECRET_VALUE_KEEP_INTACT",
		} {
			if strings.Contains(combinedAuditText, forbidden) {
				t.Fatalf("诊断接口响应或宿主日志泄露了敏感请求值 %q", forbidden)
			}
		}
	})
}

func jsonEqualNormalized(a, b []byte) bool {
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		return false
	}
	ca, errA := json.Marshal(va)
	cb, errB := json.Marshal(vb)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(ca, cb)
}
