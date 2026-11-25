package compat

import (
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TargetFormatAntigravity 为 CLIProxyAPI 中唯一代表实际发往 Antigravity 执行器的目标协议标识。
const TargetFormatAntigravity = "antigravity"

// 下列集合列出 Google Cloud Code PA (v1internal) 各固定 Proto 消息层级的已知合法字段名（含 camelCase 与 snake_case）。
// 未在此列表中且不在已证实 400 清理规则中的未知字段，仅记录脱敏诊断（路径+类型），绝不删除或修改。
var (
	knownEnvelopeKeys = toKeySet(
		"project",
		"requestId", "request_id",
		"request",
		"model",
		"userAgent", "user_agent",
		"requestType", "request_type",
		"toolConfig", "tool_config",
		"enabledCreditTypes", "enabled_credit_types",
	)

	knownRequestKeys = toKeySet(
		"contents",
		"systemInstruction", "system_instruction",
		"tools",
		"toolConfig", "tool_config",
		"labels",
		"generationConfig", "generation_config",
		"sessionId", "session_id",
		"safetySettings", "safety_settings",
		"cachedContent", "cached_content",
		"model",
	)

	knownGenerationConfigKeys = toKeySet(
		"temperature",
		"topP", "top_p",
		"topK", "top_k",
		"candidateCount", "candidate_count",
		"maxOutputTokens", "max_output_tokens",
		"stopSequences", "stop_sequences",
		"responseLogprobs", "response_logprobs",
		"logprobs",
		"presencePenalty", "presence_penalty",
		"frequencyPenalty", "frequency_penalty",
		"seed",
		"responseMimeType", "response_mime_type",
		"responseSchema", "response_schema",
		"responseJsonSchema", "response_json_schema",
		"routingConfig", "routing_config",
		"audioTimestamp", "audio_timestamp",
		"responseModalities", "response_modalities",
		"mediaResolution", "media_resolution",
		"speechConfig", "speech_config",
		"thinkingConfig", "thinking_config",
		"imageConfig", "image_config",
		"enableAffectiveDialog", "enable_affective_dialog",
	)

	knownThinkingConfigKeys = toKeySet(
		"includeThoughts", "include_thoughts",
		"thinkingBudget", "thinking_budget",
		"thinkingLevel", "thinking_level",
	)

	knownToolConfigKeys = toKeySet(
		"functionCallingConfig", "function_calling_config",
		"retrievalConfig", "retrieval_config",
	)

	knownToolItemKeys = toKeySet(
		"functionDeclarations", "function_declarations",
		"googleSearch", "google_search",
		"googleSearchRetrieval", "google_search_retrieval",
		"codeExecution", "code_execution",
		"urlContext", "url_context",
		"enterpriseWebSearch", "enterprise_web_search",
		"retrieval",
		"computerUse", "computer_use",
		"googleMaps", "google_maps",
	)

	knownFunctionDeclarationKeys = toKeySet(
		"name",
		"description",
		"parameters",
		"parametersJsonSchema", "parameters_json_schema",
		"response",
		"responseJsonSchema", "response_json_schema",
		"behavior",
	)

	knownContentKeys = toKeySet(
		"role",
		"parts",
	)

	knownPartKeys = toKeySet(
		"text",
		"inlineData", "inline_data",
		"fileData", "file_data",
		"functionCall", "function_call",
		"functionResponse", "function_response",
		"executableCode", "executable_code",
		"codeExecutionResult", "code_execution_result",
		"videoMetadata", "video_metadata",
		"thought",
		"thoughtSignature", "thought_signature",
		"mediaResolution", "media_resolution",
	)
)

func toKeySet(keys ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		set[k] = struct{}{}
	}
	return set
}

// Engine 封装 Antigravity 请求兼容模块的核心处理与脱敏诊断引擎。
type Engine struct {
	cfgValue    atomic.Value // 存储 Config
	rules       []Rule
	diagnostics *DiagnosticsStore
	logger      HostLogFunc
}

// NewEngine 使用指定配置与可选宿主日志回调构造兼容引擎。
func NewEngine(cfg Config, logger HostLogFunc) *Engine {
	cloned := cfg.Clone()
	e := &Engine{
		rules:       DefaultRules(),
		diagnostics: NewDiagnosticsStore(cloned.MaxDiagnosticPaths),
		logger:      logger,
	}
	e.cfgValue.Store(cloned)
	return e
}

// Reconfigure 原子热更新引擎配置，保留已累积的诊断统计指标。
func (e *Engine) Reconfigure(cfg Config) {
	if e == nil {
		return
	}
	cloned := cfg.Clone()
	e.cfgValue.Store(cloned)
	e.diagnostics.SetMaxPaths(cloned.MaxDiagnosticPaths)
}

// CurrentConfig 返回当前生效的只读配置深拷贝副本。
func (e *Engine) CurrentConfig() Config {
	if e == nil {
		return DefaultConfig()
	}
	if v := e.cfgValue.Load(); v != nil {
		if cfg, ok := v.(Config); ok {
			return cfg.Clone()
		}
	}
	return DefaultConfig()
}

// Rules 返回引擎内置的只读规则列表副本。
func (e *Engine) Rules() []Rule {
	if e == nil {
		return nil
	}
	out := make([]Rule, len(e.rules))
	copy(out, e.rules)
	return out
}

// Snapshot 返回当前运行期脱敏诊断快照。
func (e *Engine) Snapshot() DiagnosticsSnapshot {
	if e == nil {
		return DiagnosticsSnapshot{}
	}
	return e.diagnostics.Snapshot(e.CurrentConfig(), e.rules)
}

// NormalizeRequest 实现 CLIProxyAPI RequestNormalizer 核心处理逻辑。
// 严格仅当 req.ToFormat == "antigravity"（即当前实际选中的执行器为 AntigravityExecutor）时运行；
// Codex、Gemini API、Claude、OpenAI 兼容等其他所有渠道 100% 原样零拷贝返回。
func (e *Engine) NormalizeRequest(req pluginapi.RequestTransformRequest) pluginapi.PayloadResponse {
	if e == nil {
		return pluginapi.PayloadResponse{Body: req.Body}
	}

	// 严格渠道隔离门禁：只认实际目标格式 ToFormat == "antigravity"，绝不按模型名、别名或入口格式猜测。
	if !strings.EqualFold(strings.TrimSpace(req.ToFormat), TargetFormatAntigravity) {
		e.diagnostics.RecordBypass()
		return pluginapi.PayloadResponse{Body: req.Body}
	}

	if len(req.Body) == 0 || !ojson.ValidBytes(req.Body) || !ojson.ParseBytes(req.Body).IsObject() {
		e.diagnostics.RecordAntigravityOutcome(nil, nil)
		return pluginapi.PayloadResponse{Body: req.Body}
	}

	cfg := e.CurrentConfig()
	rawModel := strings.TrimSpace(req.Model)
	ruleCtx := RuleContext{
		FromProtocol: SanitizeProtocolName(req.FromFormat),
		ToProtocol:   TargetFormatAntigravity,
		Model:        rawModel,
		Stream:       req.Stream,
	}

	doc := ojson.NewDoc(req.Body)
	var cleaned []CleanedField
	for i := range e.rules {
		rule := &e.rules[i]
		if !cfg.IsRuleEnabled(rule.ID) {
			continue
		}
		if items := rule.Apply(doc, ruleCtx); len(items) > 0 {
			cleaned = append(cleaned, items...)
		}
	}

	sanitizedModel := SanitizeModelName(rawModel)
	findings := inspectUnprovenFields(doc, ruleCtx.FromProtocol, sanitizedModel)
	e.diagnostics.RecordAntigravityOutcome(cleaned, findings)
	e.emitDiagnosticsLogs(cfg, ruleCtx.FromProtocol, sanitizedModel, cleaned, findings)

	// 若未命中任何已证实不兼容字段，严格原样返回原始字节切片（即便存在未证实字段诊断也绝不重序列化修改请求）。
	if len(cleaned) == 0 {
		return pluginapi.PayloadResponse{Body: req.Body}
	}

	return pluginapi.PayloadResponse{Body: doc.Bytes()}
}

// RegisterManagement 返回受管理密钥认证保护的只读诊断统计路由声明。
func (e *Engine) RegisterManagement(req pluginapi.ManagementRegistrationRequest) pluginapi.ManagementRegistrationResponse {
	if e == nil || !e.CurrentConfig().DiagnosticsEndpointEnabled {
		return pluginapi.ManagementRegistrationResponse{}
	}
	pluginID := extractPluginIDFromResourceBase(req.ResourceBasePath)
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{
				Method: http.MethodGet,
				Path:   "/plugins/" + pluginID + "/diagnostics",
			},
		},
	}
}

// HandleManagement 处理只读脱敏诊断查询请求。
func (e *Engine) HandleManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	headers := http.Header{
		"Content-Type": []string{"application/json; charset=utf-8"},
	}
	if !strings.EqualFold(strings.TrimSpace(req.Method), http.MethodGet) {
		body, _ := json.Marshal(map[string]string{
			"error": "仅支持 GET 只读查询脱敏诊断统计",
		})
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusMethodNotAllowed,
			Headers:    headers,
			Body:       body,
		}
	}
	snap := e.Snapshot()
	body, errMarshal := json.MarshalIndent(snap, "", "  ")
	if errMarshal != nil {
		fallback, _ := json.Marshal(map[string]string{
			"error": "序列化诊断快照失败",
		})
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusInternalServerError,
			Headers:    headers,
			Body:       fallback,
		}
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    headers,
		Body:       body,
	}
}

func extractPluginIDFromResourceBase(resourceBase string) string {
	trimmed := strings.Trim(strings.TrimSpace(resourceBase), "/")
	if trimmed == "" {
		return PluginID
	}
	if slashIdx := strings.LastIndex(trimmed, "/"); slashIdx >= 0 && slashIdx+1 < len(trimmed) {
		candidate := strings.TrimSpace(trimmed[slashIdx+1:])
		if safeKeySegmentPattern.MatchString(candidate) {
			return candidate
		}
	}
	return PluginID
}

func (e *Engine) emitDiagnosticsLogs(cfg Config, fromProtocol, sanitizedModel string, cleaned []CleanedField, findings []DiagnosticFinding) {
	if e == nil || e.logger == nil || cfg.DiagnosticsLogLevel == "off" {
		return
	}
	if len(cleaned) > 0 {
		safeCleaned := make([]map[string]string, 0, len(cleaned))
		for _, item := range cleaned {
			safeCleaned = append(safeCleaned, map[string]string{
				"rule_id":    item.RuleID,
				"field_path": sanitizePathForDisplay(item.Path),
				"value_type": item.ValueType,
			})
		}
		e.logger(cfg.DiagnosticsLogLevel, "antigravity-compat: 已清理已证实不兼容字段", map[string]any{
			"component":      PluginID,
			"action":         "cleaned_proven_incompatible",
			"from_protocol":  fromProtocol,
			"to_protocol":    TargetFormatAntigravity,
			"model":          sanitizedModel,
			"cleaned_count":  len(cleaned),
			"cleaned_fields": safeCleaned,
		})
	}
	for _, f := range findings {
		e.logger(cfg.DiagnosticsLogLevel, "antigravity-compat: 发现未证实字段或参数组合（仅脱敏诊断，未改请求）", map[string]any{
			"component":     PluginID,
			"action":        "passthrough_diagnostic_only",
			"category":      string(f.Category),
			"field_path":    f.FieldPath,
			"value_type":    f.ValueType,
			"from_protocol": f.FromProtocol,
			"to_protocol":   f.ToProtocol,
			"model":         f.Model,
			"note":          f.Note,
		})
	}
}

func sanitizePathForDisplay(path string) string {
	parts := strings.Split(path, ".")
	for idx, seg := range parts {
		if seg == "*" || seg == "[*]" {
			continue
		}
		parts[idx] = SanitizeKeySegment(seg)
	}
	return strings.Join(parts, ".")
}

// inspectUnprovenFields 只读扫描固定 Proto 消息层级的未知字段与未证实参数组合。
// 严格禁止深入 args、response、labels、parameters、responseSchema、text、inlineData 等业务数据字典，
// 且单次请求内对相同 (Category, FieldPath, ValueType) 自动去重。
func inspectUnprovenFields(doc *ojson.Doc, fromProtocol, sanitizedModel string) []DiagnosticFinding {
	if doc == nil {
		return nil
	}
	root := ojson.ParseBytes(doc.Bytes())
	if !root.IsObject() {
		return nil
	}

	var findings []DiagnosticFinding
	seen := make(map[string]struct{})
	addFinding := func(category DiagnosticCategory, fieldPath, valueType, note string) {
		dedupKey := string(category) + "|" + fieldPath + "|" + valueType
		if _, exists := seen[dedupKey]; exists {
			return
		}
		seen[dedupKey] = struct{}{}
		if len(note) > 256 {
			note = note[:256]
		}
		findings = append(findings, DiagnosticFinding{
			Category:     category,
			FieldPath:    fieldPath,
			ValueType:    valueType,
			FromProtocol: fromProtocol,
			ToProtocol:   TargetFormatAntigravity,
			Model:        sanitizedModel,
			Note:         note,
		})
	}

	scanObjectUnknownKeys(root, "", knownEnvelopeKeys, addFinding, "顶层信封存在未列入已知 Antigravity 协议的字段，无 400 实证故原样保留")

	reqObj := root.Get("request")
	if !reqObj.IsObject() {
		return findings
	}
	scanObjectUnknownKeys(reqObj, "request", knownRequestKeys, addFinding, "request 层级存在未列入已知 GenerateContentRequest Proto 的字段，无 400 实证故原样保留")

	isClaudeTarget := strings.Contains(strings.ToLower(sanitizedModel), "claude") ||
		strings.Contains(strings.ToLower(root.Get("model").String()), "claude")

	for _, genKey := range []string{"generationConfig", "generation_config"} {
		genObj := reqObj.Get(genKey)
		if !genObj.IsObject() {
			continue
		}
		prefix := "request." + genKey
		scanObjectUnknownKeys(genObj, prefix, knownGenerationConfigKeys, addFinding, "generationConfig 存在未知字段，无 400 实证故原样保留")

		hasThinking := false
		for _, thinkKey := range []string{"thinkingConfig", "thinking_config"} {
			thinkObj := genObj.Get(thinkKey)
			if thinkObj.IsObject() {
				hasThinking = true
				scanObjectUnknownKeys(thinkObj, prefix+"."+thinkKey, knownThinkingConfigKeys, addFinding, "thinkingConfig 存在未知字段，无 400 实证故原样保留")
			}
		}

		// Issue #7878：Antigravity Claude 启用 thinking 时携带 temperature/topP/topK 尚无 Cloud Code PA 线上 400 证据，只做脱敏诊断不改请求。
		if isClaudeTarget && hasThinking {
			for _, samplingKey := range []string{"temperature", "topP", "top_p", "topK", "top_k"} {
				samplingVal := genObj.Get(samplingKey)
				if samplingVal.Exists() {
					addFinding(
						DiagnosticUnverifiedCombination,
						prefix+"."+samplingKey,
						ValueTypeName(samplingVal),
						"Claude 模型启用 thinkingConfig 时同时携带采样参数，尚无 Antigravity 上游 400 实证（参见 Issue #7878），原样保留",
					)
				}
			}
		}
	}

	for _, tcKey := range []string{"toolConfig", "tool_config"} {
		tcObj := reqObj.Get(tcKey)
		if tcObj.IsObject() {
			scanObjectUnknownKeys(tcObj, "request."+tcKey, knownToolConfigKeys, addFinding, "toolConfig 存在未知字段，无 400 实证故原样保留")
		}
	}

	toolsArr := reqObj.Get("tools")
	if toolsArr.IsArray() {
		toolsArr.ForEach(func(_, tool ojson.Result) bool {
			if !tool.IsObject() {
				return true
			}
			scanObjectUnknownKeys(tool, "request.tools[*]", knownToolItemKeys, addFinding, "tools 元素存在未知字段，无 400 实证故原样保留")
			for _, declContainer := range []string{"functionDeclarations", "function_declarations"} {
				decls := tool.Get(declContainer)
				if !decls.IsArray() {
					continue
				}
				decls.ForEach(func(_, decl ojson.Result) bool {
					if decl.IsObject() {
						scanObjectUnknownKeys(decl, "request.tools[*]."+declContainer+"[*]", knownFunctionDeclarationKeys, addFinding, "FunctionDeclaration 存在未知字段，无 400 实证故原样保留")
					}
					return true
				})
			}
			return true
		})
	}

	contentsArr := reqObj.Get("contents")
	if contentsArr.IsArray() {
		contentsArr.ForEach(func(_, content ojson.Result) bool {
			if !content.IsObject() {
				return true
			}
			scanObjectUnknownKeys(content, "request.contents[*]", knownContentKeys, addFinding, "Content 存在未知字段，无 400 实证故原样保留")
			partsArr := content.Get("parts")
			if partsArr.IsArray() {
				partsArr.ForEach(func(_, part ojson.Result) bool {
					if part.IsObject() {
						scanObjectUnknownKeys(part, "request.contents[*].parts[*]", knownPartKeys, addFinding, "Part 存在未知字段，无 400 实证故原样保留")
					}
					return true
				})
			}
			return true
		})
	}

	return findings
}

func scanObjectUnknownKeys(obj ojson.Result, prefix string, known map[string]struct{}, addFinding func(DiagnosticCategory, string, string, string), note string) {
	if !obj.IsObject() {
		return
	}
	obj.ForEach(func(key, val ojson.Result) bool {
		rawKey := key.String()
		if _, ok := known[rawKey]; ok {
			return true
		}
		safeKey := SanitizeKeySegment(rawKey)
		fullPath := safeKey
		if prefix != "" {
			fullPath = prefix + "." + safeKey
		}
		addFinding(DiagnosticUnknownProtoField, fullPath, ValueTypeName(val), note)
		return true
	})
}
