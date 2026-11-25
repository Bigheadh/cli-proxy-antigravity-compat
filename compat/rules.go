package compat

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/MoeForge/moego/regexp"
)

var (
	ojsonKeyEscaper = strings.NewReplacer(
		"\\", "\\\\",
		".", "\\.",
		"*", "\\*",
		"?", "\\?",
		"#", "\\#",
		"@", "\\@",
		"|", "\\|",
	)

	// 1:1 严格对齐 CLIProxyAPI v8.1.2 internal/runtime/executor/helps/antigravity_structured_output.go:13 的 Claude 5.5 匹配规则。
	antigravityClaude55ModelPattern = regexp.MustCompile(`^claude-(opus|sonnet)-5-5(?:-(low|medium|high|max|reasoning))?(?:-[0-9]{8})?(?:\[[^\[\]]+\])?$`)
)

// RuleContext 携带单次 Antigravity 请求规整时的只读元数据上下文。
type RuleContext struct {
	FromProtocol string
	ToProtocol   string
	Model        string
	Stream       bool
}

// CleanedField 记录一次已证实不兼容字段的精确清理动作（不含原始字段值）。
type CleanedField struct {
	RuleID    string `json:"rule_id"`
	Path      string `json:"path"`
	ValueType string `json:"value_type"`
}

// Rule 定义一条具备明确字段路径、适用协议与上游不兼容证据的清理规则。
type Rule struct {
	// ID 为稳定唯一的规则编号。
	ID string
	// Name 为简短可读的规则名称。
	Name string
	// FieldPaths 明确列出本规则可能清理的精确字段路径模式。
	FieldPaths []string
	// Protocols 明确列出本规则适用的入口协议范围（目标协议恒为 antigravity）。
	Protocols []string
	// Evidence 记录上游 HTTP 400 INVALID_ARGUMENT / Proto3 严格校验失败证据与 v8.1.2 代码漏网依据。
	Evidence string
	// Apply 在 *ojson.Doc 上原位做最小化字段删除，返回实际清理的字段记录。
	Apply func(doc *ojson.Doc, ctx RuleContext) []CleanedField
}

// DefaultRules 返回全部有明确上游不兼容证据支持的 Antigravity 请求字段清理规则集。
func DefaultRules() []Rule {
	return []Rule{
		{
			ID:         "AG-COMPAT-001",
			Name:       "清理内层 request.stream 残留字段",
			FieldPaths: []string{"request.stream"},
			Protocols:  []string{"gemini", "antigravity", "openai", "openai-response", "claude"},
			Evidence: "Google Cloud Code PA (v1internal:generateContent / streamGenerateContent) 对 GenerateContentRequest 执行严格 Proto3 JSON 反序列化，" +
				"内层 request.stream 不在 Proto 定义中，触发 HTTP 400 INVALID_ARGUMENT (Unknown name \"stream\" at 'request': Cannot find field)。" +
				"CLIProxyAPI v8.1.2 (85225381) 仅在 ExecuteStream 与 CountTokens 中删除 request.stream，非流式 Execute / executeClaudeNonStream 及 Gemini 转换器遗漏（参见提交 fb989e5f）。",
			Apply: applyRuleRequestStreamResidual,
		},
		{
			ID:   "AG-COMPAT-002",
			Name: "清理 Claude 5.5 结构化输出模式下的冲突工具声明",
			FieldPaths: []string{
				"request.tools",
				"request.toolConfig",
				"request.tool_config",
				"toolConfig",
			},
			Protocols: []string{"openai", "openai-response", "claude", "gemini", "antigravity"},
			Evidence: "Antigravity Claude 5.5 (claude-opus-5-5* / claude-sonnet-5-5*) 上游不支持结构化输出与工具调用并存，" +
				"并存时返回 HTTP 400 INVALID_ARGUMENT (tool_choice: type \"tool\" and \"any\" are not supported for this model)。" +
				"CLIProxyAPI v8.1.2 (85225381) 的 PrepareAntigravityClaudeStructuredOutputDoc 将 responseSchema 转为指令后未清理 request.tools 与 request.toolConfig（Issue #8004 / #7000）。",
			Apply: applyRuleClaude55StructuredOutputToolsConflict,
		},
		{
			ID:   "AG-COMPAT-003",
			Name: "清理 Part 工具调用子消息内嵌签名与工具响应残留签名",
			FieldPaths: []string{
				"request.contents.*.parts.*.functionCall.thoughtSignature",
				"request.contents.*.parts.*.functionCall.thought_signature",
				"request.contents.*.parts.*.function_call.thoughtSignature",
				"request.contents.*.parts.*.function_call.thought_signature",
				"request.contents.*.parts.*.functionResponse.thoughtSignature",
				"request.contents.*.parts.*.functionResponse.thought_signature",
				"request.contents.*.parts.*.function_response.thoughtSignature",
				"request.contents.*.parts.*.function_response.thought_signature",
				"request.contents.*.parts.*.thoughtSignature (仅 functionResponse / function_response Part)",
				"request.contents.*.parts.*.thought_signature (仅 functionResponse / function_response Part)",
				"request.contents.*.parts.*.extra_content",
			},
			Protocols: []string{"gemini", "antigravity", "openai", "openai-response", "claude"},
			Evidence: "Google v1internal FunctionCall 与 FunctionResponse Proto 子消息中均无 thoughtSignature/thought_signature 字段，且 Part Proto 中无 OpenAI 桥接字段 extra_content，" +
				"保留将触发 HTTP 400 INVALID_ARGUMENT (Cannot find field)；同时 functionResponse/function_response Part 携带同级 thoughtSignature 会被上游签名校验器以 HTTP 400 拒绝。" +
				"CLIProxyAPI v8.1.2 (85225381) 的 internal/signature/gemini_sanitize.go 漏判了 snake_case 的 function_call / function_response 及残留空 extra_content 对象（参见提交 7be2a678）。",
			Apply: applyRulePartToolSignatures,
		},
		{
			ID:   "AG-COMPAT-004",
			Name: "清理 FunctionDeclaration 中的跨协议非 Proto 扩展字段",
			FieldPaths: []string{
				"request.tools.*.functionDeclarations.*.strict",
				"request.tools.*.functionDeclarations.*.defer_loading",
				"request.tools.*.functionDeclarations.*.cache_control",
				"request.tools.*.functionDeclarations.*.eager_input_streaming",
				"request.tools.*.functionDeclarations.*.input_examples",
				"request.tools.*.functionDeclarations.*.annotations",
				"request.tools.*.functionDeclarations.*.custom",
				"request.tools.*.functionDeclarations.*.type",
				"request.tools.*.functionDeclarations.*.additionalProperties",
				"request.tools.*.function_declarations.*.<同上字段>",
			},
			Protocols: []string{"openai", "gemini", "antigravity", "claude", "openai-response"},
			Evidence: "Google v1internal FunctionDeclaration Proto 仅接受 name、description、parameters/parametersJsonSchema、response/responseJsonSchema、behavior；" +
				"CLIProxyAPI v8.1.2 (85225381) 的 antigravity_openai_request.go:470-503 仅删除了 strict 而将 tools[].function 其余键原样拼入 functionDeclarations，" +
				"antigravity_gemini_request.go:102-142 亦保留未知键，导致携带 defer_loading、cache_control、eager_input_streaming、input_examples、annotations 等字段时触发 HTTP 400 INVALID_ARGUMENT (Cannot find field)。",
			Apply: applyRuleFunctionDeclarationUnsupportedFields,
		},
		{
			ID:   "AG-COMPAT-005",
			Name: "清理 generationConfig 透传跨协议字段及非正整数 maxOutputTokens",
			FieldPaths: []string{
				"request.generationConfig.{service_tier,store,parallel_tool_calls,stream_options,logit_bias,user,metadata,prompt_cache_key,prompt_cache_retention,verbosity,prediction,web_search_options,reasoning_effort,max_tokens,max_completion_tokens}",
				"request.generation_config.<同上字段>",
				"request.generationConfig.maxOutputTokens (仅当 <= 0、null 或非整数浮点数时)",
				"request.generationConfig.max_output_tokens (仅当 <= 0、null 或非整数浮点数时)",
				"request.generation_config.{maxOutputTokens,max_output_tokens} (仅当 <= 0、null 或非整数浮点数时)",
			},
			Protocols: []string{"openai", "openai-response", "claude", "gemini", "antigravity"},
			Evidence: "CLIProxyAPI v8.1.2 (85225381) 的 antigravity_openai_request.go:169-173 将客户端 generationConfig 原始 JSON 整块透传至 request.generationConfig，" +
				"混入 OpenAI 专有参数时触发 Google v1internal GenerationConfig Proto3 HTTP 400 INVALID_ARGUMENT (Cannot find field)；" +
				"同时 gemini_openai-responses_request.go:487-490 与 antigravity_claude_request.go:1013-1015 在 v8.1.2 中未过滤 <= 0 或小数的 max_output_tokens/max_tokens，" +
				"触发 TYPE_INT32 反序列化失败或 max_tokens >= 1 校验 HTTP 400 错误（参见提交 ac415c08）。",
			Apply: applyRuleGenerationConfigIncompatibleFields,
		},
		{
			ID:   "AG-COMPAT-006",
			Name: "清理 request 层级因整包包裹残留的 OpenAI/Anthropic 顶层协议字段",
			FieldPaths: []string{
				"request.{messages,input,instructions,service_tier,store,parallel_tool_calls,stream_options,max_tokens,max_completion_tokens,max_output_tokens,reasoning,reasoning_effort,response_format,prompt_cache_key,prompt_cache_retention,verbosity,user,metadata,logit_bias,top_logprobs}",
			},
			Protocols: []string{"gemini", "antigravity"},
			Evidence: "CLIProxyAPI v8.1.2 (85225381) 的 ConvertGeminiRequestToAntigravity (antigravity_gemini_request.go:42-46, 175) 仅删除顶层 model 后将输入 JSON 直接包裹进 request 字段；" +
				"当网关或混合客户端携带上述 OpenAI/Anthropic 顶层参数时，Google v1internal GenerateContentRequest Proto3 严格校验返回 HTTP 400 INVALID_ARGUMENT (Unknown name at 'request': Cannot find field)。",
			Apply: applyRuleRequestEnvelopeCrossProtocolFields,
		},
	}
}

func escapeOJSONKey(key string) string {
	return ojsonKeyEscaper.Replace(key)
}

func deleteDocPathIfExists(doc *ojson.Doc, ruleID, path, displayPath string) (CleanedField, bool) {
	if doc == nil {
		return CleanedField{}, false
	}
	node := doc.Get(path)
	if !node.Exists() {
		return CleanedField{}, false
	}
	vType := ValueTypeName(node)
	if errDel := doc.Delete(path); errDel != nil {
		return CleanedField{}, false
	}
	if displayPath == "" {
		displayPath = path
	}
	return CleanedField{
		RuleID:    ruleID,
		Path:      displayPath,
		ValueType: vType,
	}, true
}

func applyRuleRequestStreamResidual(doc *ojson.Doc, _ RuleContext) []CleanedField {
	if item, ok := deleteDocPathIfExists(doc, "AG-COMPAT-001", "request.stream", "request.stream"); ok {
		return []CleanedField{item}
	}
	return nil
}

// normalizeAntigravityBaseModelName 严格对齐 CLIProxyAPI v8.1.2 thinking.ParseSuffix：
// 仅剥离单次末尾 "(...)" 思维预算/等级后缀并转小写，保留可选 "[...]" 交由 antigravityClaude55ModelPattern 精确匹配，
// 且不截断 "/" 前缀（确保与 helps.PrepareAntigravityClaudeStructuredOutputDoc 的触发条件 100% 等价）。
func normalizeAntigravityBaseModelName(rawModel string) string {
	m := strings.TrimSpace(rawModel)
	if m == "" {
		return ""
	}
	if strings.HasSuffix(m, ")") {
		if lastOpen := strings.LastIndex(m, "("); lastOpen >= 0 {
			m = m[:lastOpen]
		}
	}
	return strings.ToLower(m)
}

func hasStructuredOutputSchemaObject(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	containers := []string{"request.generationConfig", "request.generation_config"}
	schemaKeys := []string{"responseSchema", "responseJsonSchema", "response_schema", "response_json_schema"}
	for _, container := range containers {
		cfgNode := doc.Get(container)
		if !cfgNode.IsObject() {
			continue
		}
		for _, key := range schemaKeys {
			if cfgNode.Get(key).IsObject() {
				return true
			}
		}
	}
	return false
}

func applyRuleClaude55StructuredOutputToolsConflict(doc *ojson.Doc, ctx RuleContext) []CleanedField {
	if doc == nil {
		return nil
	}
	candidateModel := normalizeAntigravityBaseModelName(ctx.Model)
	if candidateModel == "" {
		candidateModel = normalizeAntigravityBaseModelName(doc.Get("model").String())
	}
	if !antigravityClaude55ModelPattern.MatchString(candidateModel) {
		return nil
	}
	if !hasStructuredOutputSchemaObject(doc) {
		return nil
	}

	var cleaned []CleanedField
	for _, path := range []string{
		"request.tools",
		"request.toolConfig",
		"request.tool_config",
		"toolConfig",
	} {
		if item, ok := deleteDocPathIfExists(doc, "AG-COMPAT-002", path, path); ok {
			cleaned = append(cleaned, item)
		}
	}
	return cleaned
}

func applyRulePartToolSignatures(doc *ojson.Doc, _ RuleContext) []CleanedField {
	if doc == nil {
		return nil
	}
	contents := doc.Get("request.contents")
	if !contents.IsArray() {
		return nil
	}

	var cleaned []CleanedField
	for i, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		for j, part := range parts.Array() {
			if !part.IsObject() {
				continue
			}
			partBase := fmt.Sprintf("request.contents.%d.parts.%d", i, j)
			hasFuncResp := part.Get("functionResponse").Exists() || part.Get("function_response").Exists()
			hasFuncCall := part.Get("functionCall").Exists() || part.Get("function_call").Exists()

			if hasFuncResp {
				for _, rel := range []string{
					"thoughtSignature",
					"thought_signature",
					"functionResponse.thoughtSignature",
					"functionResponse.thought_signature",
					"function_response.thoughtSignature",
					"function_response.thought_signature",
				} {
					full := partBase + "." + rel
					if item, ok := deleteDocPathIfExists(doc, "AG-COMPAT-003", full, full); ok {
						cleaned = append(cleaned, item)
					}
				}
			}

			if hasFuncCall {
				for _, rel := range []string{
					"functionCall.thoughtSignature",
					"functionCall.thought_signature",
					"function_call.thoughtSignature",
					"function_call.thought_signature",
				} {
					full := partBase + "." + rel
					if item, ok := deleteDocPathIfExists(doc, "AG-COMPAT-003", full, full); ok {
						cleaned = append(cleaned, item)
					}
				}
			}

			// 清理 Part 层级的非 Proto 桥接包装字段 extra_content（OpenAI 兼容格式残留）。
			if part.Get("extra_content").Exists() {
				full := partBase + ".extra_content"
				if item, ok := deleteDocPathIfExists(doc, "AG-COMPAT-003", full, full); ok {
					cleaned = append(cleaned, item)
				}
			}
		}
	}
	return cleaned
}

var unsupportedFunctionDeclarationKeys = []string{
	"strict",
	"defer_loading",
	"cache_control",
	"eager_input_streaming",
	"input_examples",
	"annotations",
	"custom",
	"type",
	"additionalProperties",
}

func applyRuleFunctionDeclarationUnsupportedFields(doc *ojson.Doc, _ RuleContext) []CleanedField {
	if doc == nil {
		return nil
	}
	tools := doc.Get("request.tools")
	if !tools.IsArray() {
		return nil
	}

	var cleaned []CleanedField
	for i, tool := range tools.Array() {
		if !tool.IsObject() {
			continue
		}
		for _, containerKey := range []string{"functionDeclarations", "function_declarations"} {
			decls := tool.Get(containerKey)
			if !decls.IsArray() {
				continue
			}
			for j, decl := range decls.Array() {
				if !decl.IsObject() {
					continue
				}
				declBase := fmt.Sprintf("request.tools.%d.%s.%d", i, containerKey, j)
				for _, badKey := range unsupportedFunctionDeclarationKeys {
					escaped := escapeOJSONKey(badKey)
					if item, ok := deleteDocPathIfExists(doc, "AG-COMPAT-004", declBase+"."+escaped, declBase+"."+badKey); ok {
						cleaned = append(cleaned, item)
					}
				}
			}
		}
	}
	return cleaned
}

var unsupportedGenerationConfigKeys = []string{
	"service_tier",
	"store",
	"parallel_tool_calls",
	"stream_options",
	"logit_bias",
	"user",
	"metadata",
	"prompt_cache_key",
	"prompt_cache_retention",
	"verbosity",
	"prediction",
	"web_search_options",
	"reasoning_effort",
	"max_tokens",
	"max_completion_tokens",
}

func isValidPositiveProtoInt32(val ojson.Result) bool {
	if !val.Exists() {
		return true
	}
	switch val.Type {
	case ojson.Number:
		f := val.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) || f < 1 || f > math.MaxInt32 {
			return false
		}
		return math.Trunc(f) == f
	case ojson.String:
		trimmed := strings.TrimSpace(val.String())
		if trimmed == "" {
			return false
		}
		if parsed, errParse := strconv.ParseInt(trimmed, 10, 32); errParse == nil {
			return parsed >= 1
		}
		// 兼容科学计数法形式的正整数字符串（如 "1e4"），但拒绝带小数部分的如 "1.0" / "1.5"（避免 Proto3 TYPE_INT32 解析歧义）。
		if strings.ContainsAny(trimmed, "eE") {
			f, errFloat := strconv.ParseFloat(trimmed, 64)
			if errFloat != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 1 || f > math.MaxInt32 {
				return false
			}
			return math.Trunc(f) == f
		}
		return false
	default:
		return false
	}
}

func applyRuleGenerationConfigIncompatibleFields(doc *ojson.Doc, _ RuleContext) []CleanedField {
	if doc == nil {
		return nil
	}
	var cleaned []CleanedField
	for _, container := range []string{"request.generationConfig", "request.generation_config"} {
		cfgNode := doc.Get(container)
		if !cfgNode.IsObject() {
			continue
		}
		for _, badKey := range unsupportedGenerationConfigKeys {
			escaped := escapeOJSONKey(badKey)
			if item, ok := deleteDocPathIfExists(doc, "AG-COMPAT-005", container+"."+escaped, container+"."+badKey); ok {
				cleaned = append(cleaned, item)
			}
		}
		for _, maxKey := range []string{"maxOutputTokens", "max_output_tokens"} {
			maxNode := doc.Get(container + "." + maxKey)
			if maxNode.Exists() && !isValidPositiveProtoInt32(maxNode) {
				if item, ok := deleteDocPathIfExists(doc, "AG-COMPAT-005", container+"."+maxKey, container+"."+maxKey); ok {
					cleaned = append(cleaned, item)
				}
			}
		}
	}
	return cleaned
}

var unsupportedRequestEnvelopeKeys = []string{
	"messages",
	"input",
	"instructions",
	"service_tier",
	"store",
	"parallel_tool_calls",
	"stream_options",
	"max_tokens",
	"max_completion_tokens",
	"max_output_tokens",
	"reasoning",
	"reasoning_effort",
	"response_format",
	"prompt_cache_key",
	"prompt_cache_retention",
	"verbosity",
	"user",
	"metadata",
	"logit_bias",
	"top_logprobs",
}

func applyRuleRequestEnvelopeCrossProtocolFields(doc *ojson.Doc, _ RuleContext) []CleanedField {
	if doc == nil {
		return nil
	}
	reqNode := doc.Get("request")
	if !reqNode.IsObject() {
		return nil
	}
	var cleaned []CleanedField
	for _, badKey := range unsupportedRequestEnvelopeKeys {
		escaped := escapeOJSONKey(badKey)
		if item, ok := deleteDocPathIfExists(doc, "AG-COMPAT-006", "request."+escaped, "request."+badKey); ok {
			cleaned = append(cleaned, item)
		}
	}
	return cleaned
}
