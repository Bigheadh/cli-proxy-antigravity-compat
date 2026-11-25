package compat

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type capturedLog struct {
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields"`
}

func newTestLogRecorder() (HostLogFunc, func() []capturedLog) {
	var mu sync.Mutex
	var logs []capturedLog
	record := func(level, message string, fields map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, capturedLog{
			Level:   level,
			Message: message,
			Fields:  fields,
		})
	}
	snapshot := func() []capturedLog {
		mu.Lock()
		defer mu.Unlock()
		out := make([]capturedLog, len(logs))
		copy(out, logs)
		return out
	}
	return record, snapshot
}

func TestNonAntigravityChannelsZeroMutationAndZeroCopy(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)

	rawPayload := []byte(`{
  "model": "gpt-6.2-sol",
  "stream": true,
  "service_tier": "priority",
  "store": false,
  "request": {
    "stream": true,
    "service_tier": "priority"
  },
  "input": [{"role": "user", "content": "You are Codex. Keep exact bytes!"}]
}`)

	channels := []struct {
		toFormat string
		model    string
	}{
		{toFormat: "codex", model: "gpt-6.2-sol"},
		{toFormat: "codex", model: "gpt-6.2-astra"},
		{toFormat: "claude", model: "claude-opus-5-5-high"},
		{toFormat: "gemini", model: "gemini-3.8-pro-high"},
		{toFormat: "openai", model: "gpt-6.2-sol"},
		{toFormat: "openai-response", model: "gpt-6.2-astra"},
		{toFormat: "", model: "claude-opus-5-5"},
	}

	for _, tc := range channels {
		resp := eng.NormalizeRequest(pluginapi.RequestTransformRequest{
			FromFormat: "openai",
			ToFormat:   tc.toFormat,
			Model:      tc.model,
			Stream:     true,
			Body:       rawPayload,
		})
		if !bytes.Equal(resp.Body, rawPayload) {
			t.Fatalf("非 Antigravity 渠道 %q (model=%s) 请求体被意外修改", tc.toFormat, tc.model)
		}
		if len(resp.Body) > 0 && &resp.Body[0] != &rawPayload[0] {
			t.Fatalf("非 Antigravity 渠道 %q 应当零拷贝直接返回原切片", tc.toFormat)
		}
	}

	snap := eng.Snapshot()
	if snap.Counters.NonAntigravityBypassed != uint64(len(channels)) {
		t.Fatalf("期望 NonAntigravityBypassed=%d，实际=%d", len(channels), snap.Counters.NonAntigravityBypassed)
	}
	if snap.Counters.AntigravityInspected != 0 || snap.Counters.AntigravityMutated != 0 {
		t.Fatalf("非 Antigravity 渠道不应触发 Antigravity 检查或修改计数: %+v", snap.Counters)
	}
}

func TestRule001_RequestStreamResidual(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)
	body := []byte(`{
		"project": "demo-proj",
		"model": "gemini-3-flash",
		"request": {
			"stream": false,
			"contents": [{"role": "user", "parts": [{"text": "hello"}]}]
		}
	}`)

	resp := eng.NormalizeRequest(pluginapi.RequestTransformRequest{
		FromFormat: "gemini",
		ToFormat:   "antigravity",
		Model:      "gemini-3-flash",
		Stream:     false,
		Body:       body,
	})

	parsed := ojson.ParseBytes(resp.Body)
	if parsed.Get("request.stream").Exists() {
		t.Fatalf("request.stream 应当被 AG-COMPAT-001 清理: %s", string(resp.Body))
	}
	if parsed.Get("request.contents.0.parts.0.text").String() != "hello" {
		t.Fatalf("合法 contents 内容不应受影响: %s", string(resp.Body))
	}
}

func TestRule002_Claude55StructuredOutputToolsConflict(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)

	// 场景 1：Claude 5.5 同时携带 responseSchema 与 tools/toolConfig，应清理冲突工具声明并保留 responseSchema
	conflictBody := []byte(`{
		"model": "claude-opus-5-5-high",
		"toolConfig": {"functionCallingConfig": {"mode": "VALIDATED"}},
		"request": {
			"contents": [{"role": "user", "parts": [{"text": "输出 JSON"}]}],
			"tools": [{"functionDeclarations": [{"name": "get_weather", "description": "查天气"}]}],
			"toolConfig": {"functionCallingConfig": {"mode": "ANY"}},
			"generationConfig": {
				"responseMimeType": "application/json",
				"responseSchema": {"type": "object", "properties": {"answer": {"type": "string"}}}
			}
		}
	}`)

	resp := eng.NormalizeRequest(pluginapi.RequestTransformRequest{
		FromFormat: "openai",
		ToFormat:   "antigravity",
		Model:      "claude-opus-5-5(high)",
		Body:       conflictBody,
	})
	parsed := ojson.ParseBytes(resp.Body)
	if parsed.Get("request.tools").Exists() || parsed.Get("request.toolConfig").Exists() || parsed.Get("toolConfig").Exists() {
		t.Fatalf("Claude 5.5 结构化输出冲突的 tools/toolConfig 应被清理: %s", string(resp.Body))
	}
	if !parsed.Get("request.generationConfig.responseSchema").IsObject() {
		t.Fatalf("responseSchema 必须保留供后续执行器结构化输出注入阶段消费: %s", string(resp.Body))
	}

	// 场景 2：非 Claude 5.5 模型（如 claude-sonnet-4-6）携带 responseSchema 与 tools 时不应清理 tools
	nonClaude55Body := bytes.ReplaceAll(conflictBody, []byte("claude-opus-5-5-high"), []byte("claude-sonnet-4-6"))
	respOther := eng.NormalizeRequest(pluginapi.RequestTransformRequest{
		FromFormat: "openai",
		ToFormat:   "antigravity",
		Model:      "claude-sonnet-4-6",
		Body:       nonClaude55Body,
	})
	if !ojson.ParseBytes(respOther.Body).Get("request.tools").Exists() {
		t.Fatalf("非 Claude 5.5 模型不应被 AG-COMPAT-002 删除 request.tools")
	}

	// 场景 3：Claude 5.5 未启用结构化输出时，常规工具调用必须保留
	normalToolBody := []byte(`{
		"model": "claude-opus-5-5-high",
		"request": {
			"contents": [{"role": "user", "parts": [{"text": "查天气"}]}],
			"tools": [{"functionDeclarations": [{"name": "get_weather", "description": "查天气"}]}]
		}
	}`)
	respNormal := eng.NormalizeRequest(pluginapi.RequestTransformRequest{
		FromFormat: "openai",
		ToFormat:   "antigravity",
		Model:      "claude-opus-5-5-high",
		Body:       normalToolBody,
	})
	if !bytes.Equal(respNormal.Body, normalToolBody) {
		t.Fatalf("Claude 5.5 常规工具调用不应发生任何改动")
	}
}

func TestRule003_PartToolSignaturesAndExtraContent(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)
	body := []byte(`{
		"model": "gemini-3-pro-high",
		"request": {
			"contents": [
				{
					"role": "model",
					"parts": [
						{
							"thoughtSignature": "valid-part-level-sig",
							"extra_content": {"google": {"thought_signature": "bridge-sig"}},
							"functionCall": {
								"name": "run_tool",
								"thoughtSignature": "bad-nested-sig-camel",
								"args": {
									"stream": true,
									"strict": true
								}
							}
						},
						{
							"function_call": {
								"name": "run_tool_snake",
								"thought_signature": "bad-nested-sig-snake",
								"args": {"ok": 1}
							}
						}
					]
				},
				{
					"role": "user",
					"parts": [
						{
							"thoughtSignature": "bad-func-resp-sibling-sig",
							"thought_signature": "bad-func-resp-sibling-snake",
							"functionResponse": {
								"name": "run_tool",
								"thoughtSignature": "bad-nested-resp-sig",
								"response": {"result": "ok", "extra_content": "keep-inside-response"}
							}
						}
					]
				}
			]
		}
	}`)

	resp := eng.NormalizeRequest(pluginapi.RequestTransformRequest{
		FromFormat: "openai",
		ToFormat:   "antigravity",
		Model:      "gemini-3-pro-high",
		Body:       body,
	})
	parsed := ojson.ParseBytes(resp.Body)

	// 验证合法的 functionCall 同级 Part.thoughtSignature 被保留
	if parsed.Get("request.contents.0.parts.0.thoughtSignature").String() != "valid-part-level-sig" {
		t.Fatalf("functionCall Part 同级合法 thoughtSignature 必须保留: %s", string(resp.Body))
	}
	// 验证业务参数 args 与 response 内部的同名键绝不被误删
	if !parsed.Get("request.contents.0.parts.0.functionCall.args.stream").Bool() {
		t.Fatalf("functionCall.args 内部的用户参数绝不能被修改")
	}
	if parsed.Get("request.contents.1.parts.0.functionResponse.response.extra_content").String() != "keep-inside-response" {
		t.Fatalf("functionResponse.response 内部的业务字段绝不能被修改")
	}
	// 验证不兼容字段全部移除
	for _, removedPath := range []string{
		"request.contents.0.parts.0.extra_content",
		"request.contents.0.parts.0.functionCall.thoughtSignature",
		"request.contents.0.parts.1.function_call.thought_signature",
		"request.contents.1.parts.0.thoughtSignature",
		"request.contents.1.parts.0.thought_signature",
		"request.contents.1.parts.0.functionResponse.thoughtSignature",
	} {
		if parsed.Get(removedPath).Exists() {
			t.Fatalf("不兼容路径 %s 未被清理: %s", removedPath, string(resp.Body))
		}
	}
}

func TestRule004_FunctionDeclarationUnsupportedFields(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"request": {
			"contents": [{"role": "user", "parts": [{"text": "hi"}]}],
			"tools": [
				{
					"functionDeclarations": [
						{
							"name": "search_docs",
							"description": "检索文档",
							"strict": true,
							"defer_loading": true,
							"cache_control": {"type": "ephemeral"},
							"eager_input_streaming": true,
							"input_examples": [{"q": "demo"}],
							"annotations": {"readOnlyHint": true},
							"custom": {"x": 1},
							"type": "function",
							"additionalProperties": false,
							"parameters": {
								"type": "object",
								"additionalProperties": false,
								"properties": {"q": {"type": "string"}}
							}
						}
					]
				}
			]
		}
	}`)

	resp := eng.NormalizeRequest(pluginapi.RequestTransformRequest{
		FromFormat: "openai",
		ToFormat:   "antigravity",
		Model:      "claude-sonnet-4-6",
		Body:       body,
	})
	parsed := ojson.ParseBytes(resp.Body)
	decl := parsed.Get("request.tools.0.functionDeclarations.0")

	for _, bad := range unsupportedFunctionDeclarationKeys {
		if decl.Get(bad).Exists() {
			t.Fatalf("FunctionDeclaration.%s 应当被清理: %s", bad, string(resp.Body))
		}
	}
	// 验证 parameters JSON Schema 内部的 type 与 additionalProperties 原样保留
	if decl.Get("parameters.type").String() != "object" || decl.Get("parameters.additionalProperties").Type != ojson.False {
		t.Fatalf("FunctionDeclaration.parameters 内部 Schema 字段必须完整保留: %s", string(resp.Body))
	}
}

func TestRule005_And_Rule006_GenerationConfigAndEnvelopeFields(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)
	body := []byte(`{
		"model": "gemini-3-flash",
		"request": {
			"messages": [{"role": "user", "content": "legacy"}],
			"service_tier": "priority",
			"store": true,
			"contents": [{"role": "user", "parts": [{"text": "hello"}]}],
			"generationConfig": {
				"temperature": 0.6,
				"service_tier": "priority",
				"parallel_tool_calls": true,
				"reasoning_effort": "high",
				"max_completion_tokens": 2048,
				"maxOutputTokens": 0
			}
		}
	}`)

	resp := eng.NormalizeRequest(pluginapi.RequestTransformRequest{
		FromFormat: "gemini",
		ToFormat:   "antigravity",
		Model:      "gemini-3-flash",
		Body:       body,
	})
	parsed := ojson.ParseBytes(resp.Body)

	for _, badPath := range []string{
		"request.messages",
		"request.service_tier",
		"request.store",
		"request.generationConfig.service_tier",
		"request.generationConfig.parallel_tool_calls",
		"request.generationConfig.reasoning_effort",
		"request.generationConfig.max_completion_tokens",
		"request.generationConfig.maxOutputTokens",
	} {
		if parsed.Get(badPath).Exists() {
			t.Fatalf("路径 %s 应当被清理: %s", badPath, string(resp.Body))
		}
	}
	if parsed.Get("request.generationConfig.temperature").Float() != 0.6 {
		t.Fatalf("合法的 temperature 必须保留")
	}

	// 验证正整数 maxOutputTokens 不会被误删
	validMaxBody := []byte(`{
		"model": "gemini-3-flash",
		"request": {
			"contents": [{"role": "user", "parts": [{"text": "hello"}]}],
			"generationConfig": {"maxOutputTokens": 4096}
		}
	}`)
	respValid := eng.NormalizeRequest(pluginapi.RequestTransformRequest{
		FromFormat: "openai",
		ToFormat:   "antigravity",
		Model:      "gemini-3-flash",
		Body:       validMaxBody,
	})
	if !bytes.Equal(respValid.Body, validMaxBody) {
		t.Fatalf("合法正整数 maxOutputTokens 请求不应发生任何修改")
	}
}

func TestUnprovenFieldsDiagnosticsOnlyZeroMutationAndRedaction(t *testing.T) {
	logger, getLogs := newTestLogRecorder()
	eng := NewEngine(DefaultConfig(), logger)

	// 构造不含任何已证实 400 字段，但包含未知扩展字段、敏感键名、#7878 未证实采样组合及敏感提示词的请求
	rawBody := []byte(`{
  "project": "proj-1",
  "model": "claude-sonnet-4-6-thinking",
  "request": {
    "systemInstruction": {
      "role": "user",
      "parts": [{"text": "You are Codex and Claude Code behind CLIProxyAPI. TOP_SECRET_SYSTEM_PROMPT_9001"}]
    },
    "contents": [
      {"role": "user", "parts": [{"text": "USER_SECRET_MESSAGE_778899"}]}
    ],
    "futureExperimentalProtoField": "SECRET_PROTO_VALUE_DO_NOT_LEAK",
    "generationConfig": {
      "temperature": 0.73,
      "topP": 0.92,
      "thinkingConfig": {
        "includeThoughts": true,
        "thinkingBudget": 8192
      },
      "sk-live-sensitive-key-abcdef": "SUPER_SECRET_CREDENTIAL_VALUE"
    }
  }
}`)

	resp := eng.NormalizeRequest(pluginapi.RequestTransformRequest{
		FromFormat: "claude",
		ToFormat:   "antigravity",
		Model:      "claude-sonnet-4-6-thinking",
		Stream:     true,
		Body:       rawBody,
	})

	// 1. 严格断言未发生任何字节级修改（原样返回原切片）
	if !bytes.Equal(resp.Body, rawBody) {
		t.Fatalf("未证实字段只允许脱敏诊断，绝不允许修改请求体！\n原始:\n%s\n返回:\n%s", string(rawBody), string(resp.Body))
	}
	if &resp.Body[0] != &rawBody[0] {
		t.Fatalf("未命中任何已证实清理规则时应直接返回原字节切片")
	}

	// 2. 验证诊断快照准确记录路径与分类，且敏感键名已被掩码为 <redacted_key>
	snap := eng.Snapshot()
	if snap.Counters.AntigravityInspected != 1 || snap.Counters.AntigravityMutated != 0 {
		t.Fatalf("期望 Inspected=1 且 Mutated=0，实际: %+v", snap.Counters)
	}
	if len(snap.ObservedUnprovenFields) < 4 {
		t.Fatalf("期望至少记录 4 条未证实字段观测（futureExperimentalProtoField、脱敏键名、temperature、topP），实际: %+v", snap.ObservedUnprovenFields)
	}

	foundPaths := make(map[string]ObservedFieldSummary)
	for _, item := range snap.ObservedUnprovenFields {
		foundPaths[item.FieldPath] = item
	}
	if f, ok := foundPaths["request.futureExperimentalProtoField"]; !ok || f.ValueType != "string" || f.Category != DiagnosticUnknownProtoField {
		t.Fatalf("缺少对 request.futureExperimentalProtoField 的准确脱敏诊断: %+v", foundPaths)
	}
	if f, ok := foundPaths["request.generationConfig.<redacted_key>"]; !ok || f.ValueType != "string" {
		t.Fatalf("敏感键名必须脱敏为 request.generationConfig.<redacted_key>: %+v", foundPaths)
	}
	if f, ok := foundPaths["request.generationConfig.temperature"]; !ok || f.Category != DiagnosticUnverifiedCombination {
		t.Fatalf("Claude thinking + temperature 应归类为 unverified_combination 脱敏诊断: %+v", foundPaths)
	}

	// 3. 验证快照 JSON 与宿主日志回调中绝不泄露任何字段值、提示词文本或敏感键名原文
	snapBytes, errMarshal := json.Marshal(snap)
	if errMarshal != nil {
		t.Fatalf("序列化诊断快照失败: %v", errMarshal)
	}
	logsBytes, errMarshalLogs := json.Marshal(getLogs())
	if errMarshalLogs != nil {
		t.Fatalf("序列化捕获日志失败: %v", errMarshalLogs)
	}
	combined := string(snapBytes) + "\n" + string(logsBytes)
	forbiddenSubstrings := []string{
		"TOP_SECRET_SYSTEM_PROMPT_9001",
		"USER_SECRET_MESSAGE_778899",
		"SECRET_PROTO_VALUE_DO_NOT_LEAK",
		"sk-live-sensitive-key-abcdef",
		"SUPER_SECRET_CREDENTIAL_VALUE",
		"0.73",
		"0.92",
		"8192",
	}
	for _, secret := range forbiddenSubstrings {
		if strings.Contains(combined, secret) {
			t.Fatalf("脱敏诊断或日志中泄露了禁止出现的原始数据 %q:\n%s", secret, combined)
		}
	}
}

func TestConfigDisabledRulesAndManagementAPI(t *testing.T) {
	cfg, errCfg := ParseConfig([]byte(`
diagnostics_endpoint_enabled: true
diagnostics_log_level: info
disabled_rules:
  - ag-compat-001
max_diagnostic_paths: 64
`))
	if errCfg != nil {
		t.Fatalf("解析合法 YAML 配置失败: %v", errCfg)
	}
	if cfg.IsRuleEnabled("AG-COMPAT-001") {
		t.Fatalf("AG-COMPAT-001 应当处于已停用状态")
	}
	if !cfg.IsRuleEnabled("AG-COMPAT-002") {
		t.Fatalf("未列入 disabled_rules 的规则应保持启用")
	}

	eng := NewEngine(cfg, nil)
	body := []byte(`{"model":"gemini-3-flash","request":{"stream":true,"contents":[]}}`)
	resp := eng.NormalizeRequest(pluginapi.RequestTransformRequest{
		FromFormat: "gemini",
		ToFormat:   "antigravity",
		Model:      "gemini-3-flash",
		Body:       body,
	})
	if !bytes.Equal(resp.Body, body) {
		t.Fatalf("停用 AG-COMPAT-001 后不应清理 request.stream")
	}

	reg := eng.RegisterManagement(pluginapi.ManagementRegistrationRequest{
		BasePath:         "/v0/management",
		ResourceBasePath: "/v0/resource/plugins/antigravity-compat",
	})
	if len(reg.Routes) != 1 || reg.Routes[0].Path != "/plugins/antigravity-compat/diagnostics" {
		t.Fatalf("管理路由注册不符合预期: %+v", reg.Routes)
	}

	getResp := eng.HandleManagement(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/antigravity-compat/diagnostics",
	})
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET 诊断接口期望返回 200，实际=%d", getResp.StatusCode)
	}

	postResp := eng.HandleManagement(pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/antigravity-compat/diagnostics",
	})
	if postResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST 诊断接口期望返回 405，实际=%d", postResp.StatusCode)
	}
}

func TestRule002_ModelVariantsExactAlignmentWithCPA(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)

	cases := []struct {
		model     string
		wantStrip bool
	}{
		{model: "claude-opus-5-5", wantStrip: true},
		{model: "claude-opus-5-5-low-20261001[1m](high)", wantStrip: true},
		{model: "claude-sonnet-5-5-max-20261001", wantStrip: true},
		{model: "claude-opus-5-5-reasoning(8192)", wantStrip: true},
		// 对齐 CLIProxyAPI antigravity_structured_output_test.go：以下模型不触发 Claude 5.5 结构化输出转换，绝不能误删 tools
		{model: "other/claude-opus-5-5-max", wantStrip: false},
		{model: "claude-opus-5-5[a[b]]", wantStrip: false},
		{model: "claude-opus-5-5-custom", wantStrip: false},
		{model: "claude-sonnet-4-6", wantStrip: false},
		{model: "claude-opus-4-6-thinking", wantStrip: false},
	}

	for _, tc := range cases {
		body := []byte(`{
			"model": "` + tc.model + `",
			"request": {
				"contents": [{"role": "user", "parts": [{"text": "hi"}]}],
				"tools": [{"functionDeclarations": [{"name": "lookup", "description": "d"}]}],
				"toolConfig": {"functionCallingConfig": {"mode": "VALIDATED"}},
				"generationConfig": {
					"responseMimeType": "application/json",
					"responseSchema": {"type": "object", "properties": {"status": {"type": "string"}}}
				}
			}
		}`)
		resp := eng.NormalizeRequest(pluginapi.RequestTransformRequest{
			FromFormat: "openai-response",
			ToFormat:   "antigravity",
			Model:      tc.model,
			Body:       body,
		})
		hasTools := ojson.ParseBytes(resp.Body).Get("request.tools").Exists()
		if tc.wantStrip && hasTools {
			t.Fatalf("模型 %q 应由 AG-COMPAT-002 清理冲突的 request.tools", tc.model)
		}
		if !tc.wantStrip && !hasTools {
			t.Fatalf("模型 %q 不应触发 AG-COMPAT-002 误删 request.tools", tc.model)
		}
	}
}

func TestRule003_SnakeCaseVariants(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)
	body := []byte(`{
		"model": "gemini-3-flash",
		"request": {
			"contents": [
				{
					"role": "model",
					"parts": [
						{
							"thought_signature": "valid-snake-part-sig",
							"function_call": {
								"name": "exec_cmd",
								"thoughtSignature": "bad-camel-in-snake-fc",
								"thought_signature": "bad-snake-in-snake-fc",
								"args": {"cmd": "ls"}
							}
						}
					]
				},
				{
					"role": "user",
					"parts": [
						{
							"thoughtSignature": "bad-sibling-camel-on-snake-fr",
							"thought_signature": "bad-sibling-snake-on-snake-fr",
							"function_response": {
								"name": "exec_cmd",
								"thoughtSignature": "bad-nested-camel-on-snake-fr",
								"thought_signature": "bad-nested-snake-on-snake-fr",
								"response": {"output": "ok"}
							}
						}
					]
				}
			]
		}
	}`)

	resp := eng.NormalizeRequest(pluginapi.RequestTransformRequest{
		FromFormat: "gemini",
		ToFormat:   "antigravity",
		Model:      "gemini-3-flash",
		Body:       body,
	})
	parsed := ojson.ParseBytes(resp.Body)

	if parsed.Get("request.contents.0.parts.0.thought_signature").String() != "valid-snake-part-sig" {
		t.Fatalf("function_call Part 同级合法 thought_signature 必须保留: %s", string(resp.Body))
	}
	for _, removed := range []string{
		"request.contents.0.parts.0.function_call.thoughtSignature",
		"request.contents.0.parts.0.function_call.thought_signature",
		"request.contents.1.parts.0.thoughtSignature",
		"request.contents.1.parts.0.thought_signature",
		"request.contents.1.parts.0.function_response.thoughtSignature",
		"request.contents.1.parts.0.function_response.thought_signature",
	} {
		if parsed.Get(removed).Exists() {
			t.Fatalf("snake_case 变体路径 %s 未被清理: %s", removed, string(resp.Body))
		}
	}
}

func TestRule005_MaxOutputTokensBoundary(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)

	cases := []struct {
		name     string
		rawToken string
		wantKeep bool
	}{
		{name: "zero", rawToken: `0`, wantKeep: false},
		{name: "negative", rawToken: `-1`, wantKeep: false},
		{name: "float_fraction", rawToken: `128.5`, wantKeep: false},
		{name: "null", rawToken: `null`, wantKeep: false},
		{name: "boolean", rawToken: `true`, wantKeep: false},
		{name: "array", rawToken: `[]`, wantKeep: false},
		{name: "overflow_int32", rawToken: `2147483648`, wantKeep: false},
		{name: "string_zero", rawToken: `"0"`, wantKeep: false},
		{name: "string_decimal_fraction", rawToken: `"1.5"`, wantKeep: false},
		{name: "positive_one", rawToken: `1`, wantKeep: true},
		{name: "max_int32", rawToken: `2147483647`, wantKeep: true},
		{name: "scientific_integer_number", rawToken: `1e4`, wantKeep: true},
		{name: "string_positive_int", rawToken: `"8192"`, wantKeep: true},
		{name: "string_scientific_int", rawToken: `"1e4"`, wantKeep: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"gemini-3-flash","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":` + tc.rawToken + `}}}`)
			resp := eng.NormalizeRequest(pluginapi.RequestTransformRequest{
				FromFormat: "openai",
				ToFormat:   "antigravity",
				Model:      "gemini-3-flash",
				Body:       body,
			})
			exists := ojson.ParseBytes(resp.Body).Get("request.generationConfig.maxOutputTokens").Exists()
			if exists != tc.wantKeep {
				t.Fatalf("maxOutputTokens=%s: exists=%v, 期望=%v, body=%s", tc.rawToken, exists, tc.wantKeep, string(resp.Body))
			}
		})
	}
}

func TestOJSONKeyEscaperSpecialModifiers(t *testing.T) {
	escaped := escapeOJSONKey(`a.b*c?d#e@f|g\h`)
	want := `a\.b\*c\?d\#e\@f\|g\\h`
	if escaped != want {
		t.Fatalf("escapeOJSONKey 结果不符合预期: got=%q want=%q", escaped, want)
	}

	doc := ojson.NewDoc([]byte(`{"request":{"@type":"test","#count":3,"keep":true}}`))
	if _, ok := deleteDocPathIfExists(doc, "TEST", "request."+escapeOJSONKey("@type"), "request.@type"); !ok {
		t.Fatalf("含 @ 修饰符的键应当被精确删除")
	}
	if _, ok := deleteDocPathIfExists(doc, "TEST", "request."+escapeOJSONKey("#count"), "request.#count"); !ok {
		t.Fatalf("含 # 修饰符的键应当被精确删除")
	}
	parsed := ojson.ParseBytes(doc.Bytes())
	if parsed.Get(`request.\@type`).Exists() || parsed.Get(`request.\#count`).Exists() {
		t.Fatalf("特殊修饰符键未被删除: %s", string(doc.Bytes()))
	}
	if !parsed.Get("request.keep").Bool() {
		t.Fatalf("同级合法键 keep 不应受影响: %s", string(doc.Bytes()))
	}
}

func TestDiagnosticsShrinkEvictionAndEdgeCases(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxDiagnosticPaths = 32
	eng := NewEngine(cfg, nil)

	// 1. 验证 CurrentConfig 返回深拷贝，外部篡改切片不影响引擎内部状态
	cfgCopy := eng.CurrentConfig()
	cfgCopy.DisabledRules = append(cfgCopy.DisabledRules, "AG-COMPAT-001")
	if !eng.CurrentConfig().IsRuleEnabled("AG-COMPAT-001") {
		t.Fatalf("外部修改 CurrentConfig 切片不应污染引擎内部配置")
	}

	// 2. 验证非 JSON 对象边缘 Antigravity 请求计入 antigravity_inspected 且原样返回
	edgeBody := []byte(`["not-an-object"]`)
	edgeResp := eng.NormalizeRequest(pluginapi.RequestTransformRequest{
		FromFormat: "gemini",
		ToFormat:   "antigravity",
		Model:      "gemini-3-flash",
		Body:       edgeBody,
	})
	if !bytes.Equal(edgeResp.Body, edgeBody) {
		t.Fatalf("非对象边缘请求体不应被修改")
	}
	if eng.Snapshot().Counters.AntigravityInspected != 1 {
		t.Fatalf("非对象边缘 Antigravity 请求应计入 AntigravityInspected")
	}

	// 3. 写入 24 条不同未知字段路径（其中 hotField 命中 3 次），随后热更新缩容至 16 条，验证保留高频条目并立即淘汰超额条目
	for idx := 0; idx < 24; idx++ {
		keyName := "unknownField_" + string(rune('A'+idx))
		payload := []byte(`{"model":"gemini-3-flash","request":{"` + keyName + `":1,"contents":[]}}`)
		eng.NormalizeRequest(pluginapi.RequestTransformRequest{
			FromFormat: "gemini",
			ToFormat:   "antigravity",
			Model:      "gemini-3-flash",
			Body:       payload,
		})
	}
	for repeat := 0; repeat < 3; repeat++ {
		hotPayload := []byte(`{"model":"gemini-3-flash","request":{"unknownField_Z":1,"contents":[]}}`)
		eng.NormalizeRequest(pluginapi.RequestTransformRequest{
			FromFormat: "gemini",
			ToFormat:   "antigravity",
			Model:      "gemini-3-flash",
			Body:       hotPayload,
		})
	}
	if got := len(eng.Snapshot().ObservedUnprovenFields); got != 25 {
		t.Fatalf("缩容前期望 25 条观测记录，实际=%d", got)
	}

	smallerCfg := DefaultConfig()
	smallerCfg.MaxDiagnosticPaths = 16
	eng.Reconfigure(smallerCfg)

	afterSnap := eng.Snapshot()
	if got := len(afterSnap.ObservedUnprovenFields); got != 16 {
		t.Fatalf("Reconfigure 缩容后应立即淘汰超额条目至 16 条，实际=%d", got)
	}
	foundHot := false
	for _, item := range afterSnap.ObservedUnprovenFields {
		if item.FieldPath == "request.unknownField_Z" && item.Count == 3 {
			foundHot = true
			break
		}
	}
	if !foundHot {
		t.Fatalf("缩容淘汰时应优先保留高频观测路径 request.unknownField_Z")
	}
}
