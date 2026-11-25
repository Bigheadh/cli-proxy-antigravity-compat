# AI 编程客户端与中国大陆模型生态特征关键字收录清单

本文档梳理各主流 AI 编程 CLI / IDE / Agent 客户端及中国大陆大模型生态在通过 `CLIProxyAPI v8.1.2` 访问 Antigravity 模型时的适配分层与系统指令（`systemInstruction`）特征关键字清单，配套配置见 [`../configs/cli-clients.example.yaml`](../configs/cli-clients.example.yaml)。

> [!IMPORTANT]
> - **职责红线**：本仓库 Go 插件（`antigravity-compat`）**绝不扫描、不修改任何提示词关键词或客户端身份**，仅负责清理触发 Google Cloud Code PA `HTTP 400 INVALID_ARGUMENT` 的跨协议残留字段（自动覆盖 `/v1/chat/completions`、`/v1/responses`、`/v1/messages`、`/v1beta/models/*` 四类入口）。
> - **系统提示词替换归属**：下方收集的关键字仅供按需填入 `CLIProxyAPI v8.1.2` 原生内置的 `oauth.providers.antigravity.sensitive-patterns-replace`（旧版无 `config-version` 布局下为顶层 `antigravity.sensitive-patterns-replace`），仅对 `AntigravityExecutor` 生效，不影响任何其他渠道（含 Codex、Claude API、Gemini API 及各国产大模型直连渠道）。
> - **验证状态声明**：除 `Codex CLI` 与 `Claude Code CLI` 外，**OpenCode、Qoder、ZCode 及中国大陆编程工具与模型条目当前均为「仅收集关键字、未做实机验证」状态**，建议按实际使用的客户端分段按需裁剪启用。

---

## 一、已实测验证客户端

| 生态 / 厂商 | 客户端 / 工具 | 常见入口协议 | 已收录特征关键字（身份句 / 包名 / 域名 / 标识） | 状态 |
| :--- | :--- | :--- | :--- | :--- |
| **OpenAI** | Codex CLI | `/v1/responses`<br>`/v1/chat/completions` | `You are Codex`、`Codex CLI`、`openai.com`、`OpenAI` | 已实测验证（`0.172.0`） |
| **Anthropic** | Claude Code CLI | `/v1/messages` | `You are a Claude agent`、`Claude Code`、`Claude Agent SDK`、`anthropics/claude-code`、`claude.ai/code` | 已实测验证（`2.1.332`） |

---

## 二、OpenCode 及中国大陆主流 AI 编程工具与模型关键字清单（仅收集关键字，未做验证）

| 生态 / 厂商 | 编程客户端 / Agent | 关联国产模型 / 平台标识 | 已收录特征关键字（身份句 / 目录包名 / 域名 / 品牌词） | 状态 |
| :--- | :--- | :--- | :--- | :--- |
| **OpenCode 开源生态** | OpenCode CLI / Desktop / IDE | 多模型通用底座 | `You are OpenCode`、`OpenCode`、`OpenCode CLI`、`OpenCode Go`、`OpenCode Zen`、`anomalyco/opencode`、`sst/opencode`、`opencode-ai/opencode`、`@opencode-ai/*`、`opencode.ai`、`.opencode/` | 仅收集关键字（未验证） |
| **阿里巴巴 / 阿里云** | Qoder、Qoder CN、`qodercli` / `qoderclicn`、QoderWork、Qwen Code (`qwen-code`)、QwenPaw、通义灵码 (Lingma) | 通义千问 (`Qwen` / `Qwen-Coder` / `Qwen-Max` / `Qwen-Plus`)、百炼 (`DashScope`) | `You are Qoder`、`You are Qwen Code`、`Qoder`、`Qoder CN`、`qodercli`、`qoderclicn`、`QoderWork`、`@qoder-ai/*`、`qoder.com`、`.qoder/`、`Qwen Code`、`qwen-code`、`QwenLM/qwen-code`、`QwenPaw`、`qwen.ai`、`.qwen/`、`Tongyi Lingma`、`Lingma`、`通义灵码`、`Tongyi Qianwen`、`通义千问`、`Qwen*`、`DashScope`、`百炼`、`Alibaba Group`、`Alibaba Cloud`、`Aliyun`、`阿里云` | 仅收集关键字（未验证） |
| **智谱 AI (Z.ai)** | ZCode (`Z Code` / `zcode-cli` / ZCode Desktop)、Z.ai Code、CodeGeeX、AutoGLM | `GLM` 系列 (`GLM-4*` / `GLM-5*` / `ChatGLM`)、BigModel (`bigmodel.cn`) | `You are ZCode`、`You are an interactive ZCode agent`、`You are Z.ai Code`、`ZCode`、`Z Code`、`zcode-cli`、`ZCode Desktop`、`zai-org/zcode`、`window.zcode`、`.zcode/`、`z.ai`、`CodeGeeX`、`codegeex.cn`、`AutoGLM`、`BigModel`、`bigmodel.cn`、`ChatGLM*`、`GLM-*`、`GLM Coding Plan`、`Zhipu AI`、`智谱清言`、`智谱AI`、`智谱` | 仅收集关键字（未验证） |
| **月之暗面 (Moonshot AI)** | Kimi Code (`kimi-code`)、Kimi CLI (`kimi-cli`)、Kimi Work | `Kimi` 系列 (`Kimi K2*` / `Kimi K3*`)、Moonshot 开放平台 | `You are Kimi`、`You are Kimi K3`、`Kimi Code`、`Kimi CLI`、`kimi-cli`、`Kimi Work`、`@moonshot-ai/kimi-code`、`MoonshotAI/kimi-code`、`.kimi-code/`、`.kimi/`、`kimi.ai`、`kimi.com`、`moonshot.cn`、`moonshot.ai`、`Moonshot AI`、`月之暗面` | 仅收集关键字（未验证） |
| **深度求索 (DeepSeek)** | DeepSeek Harness (`dsh` CLI / Desktop / Web) | `DeepSeek` 系列 (`DeepSeek-V3*` / `DeepSeek-V4*` / `DeepSeek-R1*` / `DeepSeek Coder`) | `DeepSeek Harness`、`Current DSH file policy`、`dsh cli` / `dsh agent` / `dsh harness`、`.dsh/`、`deepseek-ai/*`、`deepseek.com`、`DeepSeek Coder`、`DeepSeek-*`、`DeepSeek AI`、`深度求索` | 仅收集关键字（未验证） |
| **腾讯 / 腾讯云** | CodeBuddy (`@tencent-ai/codebuddy-code`)、WorkBuddy、WorkBuddy AI、腾讯云代码助手 | 腾讯混元 (`Hunyuan`) | `You are CodeBuddy`、`You are WorkBuddy`、`CodeBuddy`、`CodeBuddy CLI`、`AskCodebuddy`、`@tencent-ai/codebuddy-code`、`codebuddy.cn`、`codebuddy.ai`、`WorkBuddy`、`WorkBuddy AI`、`workbuddy.cn`、`workbuddy.ai`、`腾讯云代码助手`、`Tencent Cloud`、`Hunyuan*`、`腾讯混元` | 仅收集关键字（未验证） |
| **字节跳动 / 火山引擎** | Trae (`Trae AI` / `Trae CN` / `Trae CLI` / `Trae IDE`)、豆包 MarsCode | 豆包大模型 (`Doubao-*` / `Seed-Coder`)、火山方舟 (`Volcengine`) | `You operate exclusively in Trae AI`、`Trae`、`Trae AI`、`Trae CN`、`Trae CLI`、`Trae IDE`、`trae.ai`、`trae.com.cn`、`MarsCode`、`marscode.com`、`marscode.cn`、`Doubao*`、`豆包`、`Seed-Coder`、`Volcengine`、`火山引擎`、`火山方舟`、`ByteDance`、`字节跳动` | 仅收集关键字（未验证） |
| **百度 / 百度智能云** | 文心快码 (`Baidu Comate` / `ComateCLI` / `BaiduComate`) | 文心一言 (`ERNIE-*` / `Wenxin`)、百度千帆 (`Qianfan`) | `Baidu Comate`、`BaiduComate`、`ComateCLI`、`Comate`、`comate.baidu.com`、`文心快码`、`ERNIE*`、`Wenxin`、`文心一言`、`Qianfan`、`百度千帆`、`Baidu Cloud`、`百度智能云` | 仅收集关键字（未验证） |
| **小米 (Xiaomi)** | MiMoCode (`MiMo Code` / `MiMo-Code`) | `Xiaomi MiMo` 系列 (`MiMo-V2*`) | `MiMoCode`、`MiMo Code`、`MiMo-Code`、`XiaomiMiMo/MiMo-Code`、`XiaomiMiMo`、`MiMo-V2*`、`Xiaomi MiMo`、`小米MiMo` | 仅收集关键字（未验证） |
| **稀宇科技 (MiniMax)** | MiniMax Code (`minimax-code`)、MiniMax Agent | `MiniMax` 系列 (`MiniMax-M1` / `M2` / `M3`)、海螺 AI (`Hailuo`) | `MiniMax Code`、`minimax-code`、`MiniMax-AI/*`、`conversational assistant for MiniMax Agent`、`MiniMax Agent`、`minimax.io`、`minimaxi.com`、`MiniMax-M*`、`Hailuo AI`、`海螺AI`、`稀宇科技` | 仅收集关键字（未验证） |
| **阶跃星辰 (StepFun)** | StepCode (`Step-Code`) | 阶跃 `Step` 系列、跃问 | `StepCode`、`Step-Code`、`StepFun`、`stepfun.com`、`阶跃星辰`、`跃问` | 仅收集关键字（未验证） |
| **华为 / 京东 / 讯飞 / 美团 / 其他国产研发工具与模型** | 华为云码道 (`CodeArts Snap` / `CodeArts Agent`)、`DevEco Code`、京东 `JoyCode`、讯飞 `iFlyCode`、美团 `CatPaw`、非十 `Fitten Code`、商汤 `Raccoon`（代码小浣熊） | 华为盘古大模型、讯飞星火 (`SparkDesk`)、美团 `LongCat`、商汤日日新 (`SenseNova`)、百川 (`Baichuan`)、零一万物 (`01.AI` / `Yi-Coder` / `Yi-Lightning`)、面壁 (`MiniCPM`)、硅基流动 (`SiliconFlow`)、魔搭 (`ModelScope`) | `CodeArts`、`CodeArts Snap`、`CodeArts Agent`、`codearts.huaweicloud.com`、`华为云码道`、`DevEco Code`、`盘古大模型`、`JoyCode`、`JoyCoder`、`joycode.jd.com`、`iFlyCode`、`星火代码`、`iFlytek`、`SparkDesk`、`讯飞星火`、`CatPaw`、`LongCat`、`Fitten Code`、`Raccoon Code`、`代码小浣熊`、`SenseNova`、`商汤日日新`、`Baichuan*`、`百川智能`、`01.AI`、`Lingyiwanwu`、`Yi-Coder*`、`Yi-Lightning*`、`零一万物`、`MiniCPM*`、`ModelBest`、`面壁智能`、`SiliconFlow`、`硅基流动`、`ModelScope`、`魔搭社区` | 仅收集关键字（未验证） |
| **国内高频搭配开源 IDE 扩展** | Cline、Roo Code、Kilo Code | 常对接国内聚合网关与 `CLIProxyAPI` | `You are Cline`、`You are Roo`、`You are Kilo Code`、`Cline`、`Roo Code`、`Kilo Code` | 仅收集关键字（未验证） |

---

## 三、编写正则规则时的防误伤约定

1. **英文短词强制使用单词边界 `\b`**：避免把常见英文词内部子串误替换（例如不裸写 `yi` 或 `dsh`，防止误伤 `copying`、`modifying`、`handshake` 等正常英文单词；`Yi` 模型仅匹配明确的 `yi-coder` / `yi-lightning` / `yi-large` / `yi-*b` 前缀，`dsh` 仅匹配 `.dsh/`、`Current DSH file policy`、`dsh cli/agent/harness`）。
2. **中文专有名词不使用 `\b`**：Go RE2 中 `\b` 仅针对 ASCII 单词字符（`[0-9A-Za-z_]`）生效，中文多字品牌词（如 `通义灵码`、`文心快码`、`腾讯云代码助手`、`深度求索`）直接按字面量匹配即可。
3. **先长后短、先域名路径后品牌词**：`CLIProxyAPI` 按配置顺序依次执行正则替换，因此先替换包名、URL 域名与路径（如 `opencode.ai` -> `workspace.local`），再替换工具与品牌名（如 `\bopencode\b` -> `Workspace CLI`），避免产生 `Workspace CLI.ai` 等破坏格式的结果。
