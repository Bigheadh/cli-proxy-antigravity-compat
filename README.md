# cli-proxy-antigravity-compat

基于开源版本 **CLIProxyAPI `v8.1.2`（提交 `85225381`）** 的独立 **Antigravity 请求兼容模块**（原生 C ABI 动态插件适配）。

核心目标：**严格只处理即将实际发往 Antigravity 上游执行器的请求，最小化清理已证实触发上游 `HTTP 400 INVALID_ARGUMENT` 的协议残留字段；确保 `gpt-6.2-sol`、`gpt-6.2-astra` 等走 Codex、Gemini API、Claude API 及其他渠道的请求（含从 Antigravity 回退的场景）100% 字节级原样透传、零污染。**

---

## 一、设计原则与安全红线

1. **按实际执行渠道严格隔离**：
   - 绝不按模型名（如 `claude-*`、`gemini-*`）、模型别名或客户端入口格式（OpenAI Chat Completions / Responses / Claude Messages）猜测目标渠道。
   - 仅当请求已被调度器选中 `AntigravityExecutor`、进入面向 `ToFormat == "antigravity"` 的请求规整阶段时才允许执行；Codex、Gemini API、Claude API、OpenAI 兼容等其他所有执行器直接零拷贝原样返回原始字节切片。
   - **跨渠道回退零污染**：当高优先级 Antigravity 凭证因 `429` / `5xx` 触发回退至 Codex、Claude API 或 Gemini API 时，回退执行器基于未修改的原始入口载荷重新转换，绝不会收到被 Antigravity 规则改写过的请求体。
2. **优先核对现有配置，禁用全局前置拦截器**：
   - 先核查内置 `requests.payload` 规则边界（详见下文第二节及 [`configs/builtin-payload-audit.example.yaml`](configs/builtin-payload-audit.example.yaml)）。
   - **严禁注册 `InterceptRequestBeforeAuth` 等全局前置拦截器**（全局前置拦截器在选择上游凭证之前运行且直接覆写共享 `req.Payload` / `opts.OriginalRequest`，一旦发生跨渠道回退必然污染下游其他渠道）。
3. **只清理已证实不兼容的字段**：
   - 每条规则均明确绑定 **字段路径 + 适用协议 + 上游 `400 INVALID_ARGUMENT` 不兼容证据**。
   - 对尚无明确上游 `400` 证据的未知字段或社区争议参数组合（例如 Issue `#7878` 中 Claude 启用 `thinkingConfig` 时同时携带 `temperature`/`topP`/`topK`），**只输出脱敏诊断，绝不修改请求体**。
4. **四不红线**：
   - 不扫描提示词关键词；
   - 不改写用户消息与系统提示词；
   - 不改写客户端身份（`User-Agent` 及请求头）；
   - 不介入或改变 `HTTP 429` 的退避与凭证冷却行为。

---

## 二、内置配置（`requests.payload`）核查结论

在编写插件前，已对 CLIProxyAPI `v8.1.2 (85225381)` 内置请求规则引擎（`internal/runtime/executor/helps/payload_helpers.go`）及全部内置执行器调用点进行逐一核查：

| 核查维度 | 内置 `requests.payload` 表现 | 是否满足全部隔离与最小改动要求 |
| :--- | :--- | :--- |
| **渠道限定能力** | 内置执行器中仅 `AntigravityExecutor` 向 `PayloadFinalizer` 传入 `protocol = "antigravity"`（`root = "request"`）。显式填写 `protocol: "antigravity"` 时不会命中内置 Codex / Claude / Gemini API。 | **部分满足但存在隐患**：规则选择器无显式 `executor` 字段；一旦漏写 `protocol` 会退化为全渠道通配污染 Codex；且任何声明 `antigravity` 输入格式的第三方插件执行器也会被误匹配。 |
| **静态残留字段清理** | 可静态删除 `request.stream`、`request.generationConfig.service_tier` 等无条件固定路径。 | **满足静态子集**（已整理于 [`configs/builtin-payload-audit.example.yaml`](configs/builtin-payload-audit.example.yaml)）。 |
| **跨字段条件清理** | 仅支持静态 GJSON/OJSON 路径匹配，不支持跨字段与类型谓词判断。 | **不满足**：例如 Claude 5.5 仅当携带对象型 `responseSchema` 时才需移除冲突的 `request.tools`；`Part.thoughtSignature` 仅在 `functionResponse` Part 上非法而在 `functionCall` Part 上合法；`maxOutputTokens` 仅在 `<= 0` 或非整数时非法。用静态规则会误删正常请求参数。 |
| **顶层信封作用范围** | `AntigravityExecutor` 将 `PayloadFinalizer` 的 `root` 锁定在 `"request"` 子对象。 | **不满足**：无法清理顶层信封残留字段（如顶层 `toolConfig`）。 |
| **无证据字段脱敏诊断** | 仅支持 `default` / `override` / `filter` / `replace` 修改操作，无只读观测能力。 | **不满足**：无法实现“没有证据时只做脱敏诊断、不改请求”。 |

**结论**：对于纯静态字段应急场景可使用 [`configs/builtin-payload-audit.example.yaml`](configs/builtin-payload-audit.example.yaml)；为完整实现执行器级精确隔离、跨字段最小化条件清理与无证据字段脱敏诊断，本仓库提供零侵入原版源码的独立 `RequestNormalizer` 插件。

---

## 三、架构与渠道隔离机制

```
客户端请求 (/v1/chat/completions | /v1/responses | /v1/messages | /v1beta/models/*)
   │
   ▼  （插件不注册任何前置拦截器，原始 req.Payload 保持 100% 不可变）
Conductor 凭证调度与回退循环 (conductor_execution.go)
   ├─► 选中 CodexExecutor (gpt-6.2-sol / gpt-6.2-astra)
   │      └─► TranslateRequestEnvelope(ToFormat="codex")
   │             └─► NormalizeRequest: ToFormat != "antigravity" ──► 零拷贝原样返回 (100% 透传)
   │
   ├─► 选中 ClaudeExecutor / GeminiExecutor / OpenAICompatExecutor
   │      └─► TranslateRequestEnvelope(ToFormat="claude"/"gemini"/"openai")
   │             └─► NormalizeRequest: ToFormat != "antigravity" ──► 零拷贝原样返回 (100% 透传)
   │
   └─► 选中 AntigravityExecutor
          └─► prepareAntigravityRequest -> TranslateRequestEnvelope(ToFormat="antigravity")
                 └─► NormalizeRequest: ToFormat == "antigravity"
                        ├─► 1. 仅按 AG-COMPAT-001 ~ 006 最小化删除已证实 400 字段（局部切片）
                        └─► 2. 扫描未知字段与未证实组合 -> 输出脱敏诊断（不改请求）
          │
          ▼ 若 Antigravity 返回 429 / 5xx 触发跨渠道回退：
          局部转换切片随本次尝试丢弃；下一候选执行器（如 Codex / Claude API）
          从原始未改动的 req.Payload 重新转换 ──► 回退渠道 100% 零污染，429 冷却逻辑完全不变
```

---

## 四、已证实不兼容字段清理规则明细（`AG-COMPAT-001` ~ `AG-COMPAT-006`）

所有清理规则定义在 [`compat/rules.go`](compat/rules.go)，每条规则均附带明确的字段路径、适用协议与上游 `400` 证据：

| 规则 ID | 清理字段路径 | 适用入口协议（目标恒为 `antigravity`） | 上游不兼容证据与 `v8.1.2 (85225381)` 漏网原因 |
| :--- | :--- | :--- | :--- |
| **`AG-COMPAT-001`** | `request.stream` | `gemini`, `antigravity`, `openai`, `openai-response`, `claude` | Google Cloud Code PA (`v1internal`) 对 `GenerateContentRequest` 执行严格 Proto3 JSON 反序列化，内层 `stream` 不在 Proto 定义中，直接返回 `HTTP 400 INVALID_ARGUMENT (Unknown name "stream" at 'request': Cannot find field)`。`v8.1.2` 仅在 `ExecuteStream` 与 `CountTokens` 中删除了 `request.stream`，非流式 `Execute` / `executeClaudeNonStream` 及 `ConvertGeminiRequestToAntigravity` 遗漏（对应上游修复提交 `fb989e5f`）。 |
| **`AG-COMPAT-002`** | `request.tools`<br>`request.toolConfig`<br>`request.tool_config`<br>`toolConfig`<br>*(仅当目标模型匹配 `claude-(opus\|sonnet)-5-5*` 且 `generationConfig` 存在对象型 `responseSchema`/`responseJsonSchema` 时触发；保留 `responseSchema` 供后续结构化指令注入消费)* | `openai`, `openai-response`, `claude`, `gemini`, `antigravity` | Antigravity Claude 5.5 上游不支持结构化输出与工具调用并存，并存时返回 `HTTP 400 INVALID_ARGUMENT (tool_choice: type "tool" and "any" are not supported for this model)`。`v8.1.2` 的 `PrepareAntigravityClaudeStructuredOutputDoc` 将 `responseSchema` 转为系统指令后未清理冲突的 `request.tools` 与 `toolConfig`（对应上游 Issue `#8004` / `#7000`）。 |
| **`AG-COMPAT-003`** | `request.contents.*.parts.*.functionCall.{thoughtSignature,thought_signature}`<br>`request.contents.*.parts.*.function_call.{thoughtSignature,thought_signature}`<br>`request.contents.*.parts.*.functionResponse.{thoughtSignature,thought_signature}`<br>`request.contents.*.parts.*.function_response.{thoughtSignature,thought_signature}`<br>`request.contents.*.parts.{thoughtSignature,thought_signature}` *(仅当该 Part 为 `functionResponse`/`function_response` 时)*<br>`request.contents.*.parts.*.extra_content` | `gemini`, `antigravity`, `openai`, `openai-response`, `claude` | `v1internal` 的 `FunctionCall` 与 `FunctionResponse` 子消息无 `thoughtSignature` 字段，`Part` 无 OpenAI 桥接字段 `extra_content`，保留均触发 `HTTP 400 Cannot find field`；且 `functionResponse` Part 同级携带 `thoughtSignature` 会被上游签名校验器以 `HTTP 400` 拒绝（注意：`functionCall` Part 同级合法签名 100% 保留）。`v8.1.2` 的 `gemini_sanitize.go` 漏判了 snake_case `function_call`/`function_response` 及残留 `extra_content`（对应上游提交 `7be2a678`）。 |
| **`AG-COMPAT-004`** | `request.tools.*.functionDeclarations.*.{strict,defer_loading,cache_control,eager_input_streaming,input_examples,annotations,custom,type,additionalProperties}`<br>`request.tools.*.function_declarations.*.<同上字段>`<br>*(绝不触碰 `parameters` JSON Schema 内部字段)* | `openai`, `gemini`, `antigravity`, `claude`, `openai-response` | `v1internal` `FunctionDeclaration` Proto 仅允许 `name`、`description`、`parameters`/`parametersJsonSchema`、`response`/`responseJsonSchema`、`behavior`。`v8.1.2` 的 `antigravity_openai_request.go:470-503` 仅删除了 `strict` 而将其余键原样拼入 `functionDeclarations`，携带 MCP / Anthropic / OpenAI 工具扩展键时触发 `HTTP 400 Cannot find field`。 |
| **`AG-COMPAT-005`** | `request.generationConfig.{service_tier,store,parallel_tool_calls,stream_options,logit_bias,user,metadata,prompt_cache_key,prompt_cache_retention,verbosity,prediction,web_search_options,reasoning_effort,max_tokens,max_completion_tokens}`<br>`request.generationConfig.{maxOutputTokens,max_output_tokens}` *(仅当 `<= 0`、`null` 或非整数浮点数时)*<br>`request.generation_config.<同上>` | `openai`, `openai-response`, `claude`, `gemini`, `antigravity` | `v8.1.2` 的 `antigravity_openai_request.go:169-173` 将客户端 `generationConfig` 原始 JSON 整块透传，混入 OpenAI 字段触发 `HTTP 400 Cannot find field`；同时 `gemini_openai-responses_request.go:487-490` 与 `antigravity_claude_request.go:1013-1015` 未过滤 `<= 0` 或非整数输出预算，触发 `TYPE_INT32` / `max_tokens >= 1` `HTTP 400` 错误（对应上游提交 `ac415c08`）。 |
| **`AG-COMPAT-006`** | `request.{messages,input,instructions,service_tier,store,parallel_tool_calls,stream_options,max_tokens,max_completion_tokens,max_output_tokens,reasoning,reasoning_effort,response_format,prompt_cache_key,prompt_cache_retention,verbosity,user,metadata,logit_bias,top_logprobs}` | `gemini`, `antigravity` | `v8.1.2` 的 `ConvertGeminiRequestToAntigravity` 仅删除顶层 `model` 后将整包 JSON 包裹进 `request` 字段；当下游聚合网关携带跨协议顶层字段时触发 `HTTP 400 Unknown name at 'request': Cannot find field`。 |

### 未证实字段反例（只诊断、不改请求）

- **Issue `#7878`（Claude 启用 `thinkingConfig` 时携带 `temperature` / `topP` / `topK`）**：经核查上游仓库与 Cloud Code PA 线上行为，该组合不会触发上游 `HTTP 400`（官方关闭为 `not planned`）。因此本模块将其归类为 `unverified_combination`，**100% 原样保留参数值**，仅在脱敏诊断中记录路径与类型。
- **固定 Proto 消息层级的其他未知扩展字段**：在未确认上游报错前，一律归类为 `unknown_proto_field` 原样透传；若整次请求未命中任何已证实规则，直接零拷贝返回原始字节切片（连空白符与键顺序都不变）。

---

## 五、脱敏诊断机制

模块在扫描与日志输出时严格遵守隐私与安全边界：

1. **绝不读取或输出任何字段值**：无论是系统提示词、用户消息、工具参数 `args`/`response`、JSON Schema 内容还是数值参数，均调用 [`ValueTypeName`](compat/diagnostics.go) 仅提取抽象 JSON 类型（`string`/`number`/`boolean`/`object`/`array`/`null`）。
2. **异常键名自动掩码**：若未知 JSON 键名疑似误填凭证（如 `sk-*`、`AIza*`、`ya29.*`、`Bearer*`）或含非安全标识符字符，[`SanitizeKeySegment`](compat/diagnostics.go) 自动将其掩码为 `<redacted_key>`。
3. **不深入业务字典**：只读扫描严格停留在固定 Proto 消息骨架层，绝不递归进入 `functionCall.args`、`functionResponse.response`、`parameters`、`responseSchema`、`labels` 等用户数据字典。
4. **受管理密钥保护的只读诊断接口**：
   - 路由：`GET /v0/management/plugins/antigravity-compat/diagnostics`（需携带 CLIProxyAPI `remote-management.secret-key` 认证头）。
   - 返回示例：
     ```json
     {
       "plugin": "antigravity-compat",
       "version": "0.1.0",
       "baseline_version": "v8.1.2",
       "baseline_commit": "85225381",
       "isolation_mode": "executor_target_format_antigravity_only",
       "counters": {
         "antigravity_inspected": 4,
         "antigravity_mutated": 4,
         "non_antigravity_bypassed": 17,
         "cleaned_fields_total": 12,
         "diagnostic_findings_total": 2,
         "diagnostic_dropped_keys": 0
       },
       "observed_unproven_fields": [
         {
           "category": "unknown_proto_field",
           "field_path": "request.generationConfig.customUnprovenField",
           "value_type": "string",
           "from_protocol": "openai",
           "to_protocol": "antigravity",
           "last_model": "claude-opus-5-5-high",
           "action": "passthrough_diagnostic_only",
           "count": 1
         }
       ]
     }
     ```

---

## 六、构建与部署使用

### 1. 环境要求
- Go `>= 1.26.10`（开启 `CGO_ENABLED=1`）与 C 编译器（Linux `gcc` / macOS `clang` / Windows `mingw-w64`）
- 宿主程序：`CLIProxyAPI v8.1.2`（提交 `85225381`）

### 2. 编译插件动态库
```bash
make build
# 输出产物：dist/antigravity-compat.so（macOS 为 .dylib，Windows 为 .dll）

# 如需编译内含 Go Race Detector 的动态库用于并发竞态测试：
make build-race
```

如需打包符合 CLIProxyAPI 插件商店规范的发行压缩包与 SHA-256 校验文件：
```bash
make package
```

### 3. 配置启用
将 `dist/antigravity-compat.so` 复制到 CLIProxyAPI 的 `plugins/` 目录，并在 `config.yaml` 中加入（完整示例见 [`configs/plugin.example.yaml`](configs/plugin.example.yaml)）：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    antigravity-compat:
      enabled: true
      diagnostics_endpoint_enabled: true
      diagnostics_log_level: "debug"
```

若使用编程客户端通过 `CLIProxyAPI` 访问 Antigravity 模型且需按职责分层配置原生 `sensitive-patterns-replace` 系统指令特征替换，可参考：
- **配置模板**：[`configs/cli-clients.example.yaml`](configs/cli-clients.example.yaml)
- **关键字收录清单**：[`docs/cli-and-cn-models-keywords.md`](docs/cli-and-cn-models-keywords.md)（其中 `Codex CLI`、`Claude Code CLI` 已实测验证；`OpenCode`、`Qoder`、`ZCode`、`Kimi Code`、`DeepSeek Harness`、`CodeBuddy`/`WorkBuddy`、`Trae`/`MarsCode`、`文心快码`、`MiMoCode`、`MiniMax Code`、`StepCode` 等中国大陆编程工具与大模型条目已收集整理关键字，标注为未做实机验证）。

---

## 七、离线模拟上游验收

仓库内置基于原版零修改 `CLIProxyAPI v8.1.2 (85225381)` 真实服务链路的离线验收套件（[`acceptance_test/acceptance_test.go`](acceptance_test/acceptance_test.go)）。测试同时启动两个物理隔离的真实 `CLIProxyAPI` 进程（未启用模块对照组 vs C ABI `dlopen` 加载 `antigravity-compat.so` 实验组）并对接本地模拟上游集群，离线验证以下全部场景：

1. **Codex 渠道 `gpt-6.2-sol` 与 `gpt-6.2-astra` 一致性证明**：覆盖 `/v1/chat/completions`、`/v1/responses`、`/v1/messages`（流式与非流式），断言启用插件前后模拟 Codex 收到的请求体与请求头 **100% 逐字节相等**。
2. **其他非 Antigravity 渠道（Claude API、Gemini API）一致性证明**：断言启用插件前后模拟上游收到的请求体 **100% 逐字节相等**。
3. **Antigravity 精确字段清理与四不原则证明**：覆盖 Chat Completions、Responses、Claude Messages、Gemini Native 四类入口，证明严格 Proto3 模拟上游在未启用模块时返回 `HTTP 400 INVALID_ARGUMENT`、启用模块后返回 `HTTP 200 OK`；且除规则指定路径外，未证实字段、含敏感词的系统提示词、用户消息及上游 `User-Agent` 与对照组完全一致。
4. **从 Antigravity 回退到其他渠道零污染 + `429` 退避行为不变证明**：高优先级 Antigravity 返回 `HTTP 429 (Retry-After: 8)` 自动回退至低优先级 Codex / Claude API 时，回退渠道收到的请求体与对照组 **100% 逐字节相等**（`service_tier: priority`、`defer_loading: true` 等原渠道字段完好保留），且后续请求跳过 `429` 冷却中 Antigravity 凭证的退避行为与对照组完全一致。
5. **只读脱敏诊断接口与无泄露验证**：验证未授权访问被拦截、授权访问返回准确计数，且快照与日志中零敏感请求内容泄露。

执行全量单元测试与离线全链路验收：
```bash
# 运行核心规则与脱敏单元测试
make unit-test

# 编译 .so 并运行真实 CLIProxyAPI v8.1.2 离线模拟上游全链路验收
make acceptance

# 一键执行全部构建与测试
make test
```
