// Package compat 实现面向 CLIProxyAPI v8.1.2（提交 85225381）的独立 Antigravity 请求兼容核心逻辑。
// 核心原则：
//  1. 严格按实际选中的上游目标格式（ToFormat == "antigravity"）隔离，绝不按模型名、别名或入口格式猜测；
//  2. 仅清理有明确上游 400 INVALID_ARGUMENT / Proto3 严格校验不兼容证据的字段；
//  3. 无明确不兼容证据的可疑或未知字段仅输出脱敏结构化诊断，绝不修改请求体；
//  4. 不扫描提示词关键词，不改写用户消息、系统提示词或客户端身份，不改变 429 退避行为。
package compat

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	// PluginID 为插件默认标识符（与编译产物文件名保持一致）。
	PluginID = "antigravity-compat"
	// PluginVersion 为当前插件语义化版本号。
	PluginVersion = "0.1.0"
	// PluginAuthor 为插件作者标识。
	PluginAuthor = "Bigheadh"
	// RepositoryURL 为插件源码仓库地址。
	RepositoryURL = "https://github.com/Bigheadh/cli-proxy-antigravity-compat"
	// BaselineVersion 为适配验证锁定的 CLIProxyAPI 版本。
	BaselineVersion = "v8.1.2"
	// BaselineCommit 为适配验证锁定的 CLIProxyAPI Git 提交短哈希。
	BaselineCommit = "85225381"

	// DefaultMaxDiagnosticPaths 为脱敏诊断观测路径表的默认容量上限。
	DefaultMaxDiagnosticPaths = 256
	minDiagnosticPaths        = 16
	maxDiagnosticPathsLimit   = 4096
)

// Config 定义插件在 config.yaml 中 plugins.configs.antigravity-compat 下的可配置项。
type Config struct {
	// DiagnosticsEndpointEnabled 控制是否注册受管理密钥保护的只读诊断统计接口。
	DiagnosticsEndpointEnabled bool `json:"diagnostics_endpoint_enabled"`
	// DiagnosticsLogLevel 控制脱敏诊断日志输出级别：debug（默认）、info、warn、off。
	DiagnosticsLogLevel string `json:"diagnostics_log_level"`
	// DisabledRules 允许按规则 ID（如 AG-COMPAT-001）单独停用指定清理规则以便排障对比。
	DisabledRules []string `json:"disabled_rules,omitempty"`
	// MaxDiagnosticPaths 限制内存中记录的未证实字段路径种类上限，防止高基数键导致内存增长。
	MaxDiagnosticPaths int `json:"max_diagnostic_paths"`

	disabledSet map[string]struct{}
}

// DefaultConfig 返回插件默认配置。
func DefaultConfig() Config {
	return Config{
		DiagnosticsEndpointEnabled: true,
		DiagnosticsLogLevel:        "debug",
		MaxDiagnosticPaths:         DefaultMaxDiagnosticPaths,
		disabledSet:                make(map[string]struct{}),
	}
}

type rawYAMLConfig struct {
	DiagnosticsEndpointEnabled     *bool    `yaml:"diagnostics_endpoint_enabled"`
	DiagnosticsEndpointEnabledDash *bool    `yaml:"diagnostics-endpoint-enabled"`
	DiagnosticsLogLevel            *string  `yaml:"diagnostics_log_level"`
	DiagnosticsLogLevelDash        *string  `yaml:"diagnostics-log-level"`
	DisabledRules                  []string `yaml:"disabled_rules"`
	DisabledRulesDash              []string `yaml:"disabled-rules"`
	MaxDiagnosticPaths             *int     `yaml:"max_diagnostic_paths"`
	MaxDiagnosticPathsDash         *int     `yaml:"max-diagnostic-paths"`
}

// ParseConfig 解析 YAML 配置字节流并校验字段合法性，未显式设置的字段保持默认值。
func ParseConfig(configYAML []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(strings.TrimSpace(string(configYAML))) == 0 {
		return cfg, nil
	}

	var raw rawYAMLConfig
	if errUnmarshal := yaml.Unmarshal(configYAML, &raw); errUnmarshal != nil {
		return Config{}, fmt.Errorf("解析插件配置失败: %w", errUnmarshal)
	}

	if raw.DiagnosticsEndpointEnabled != nil {
		cfg.DiagnosticsEndpointEnabled = *raw.DiagnosticsEndpointEnabled
	} else if raw.DiagnosticsEndpointEnabledDash != nil {
		cfg.DiagnosticsEndpointEnabled = *raw.DiagnosticsEndpointEnabledDash
	}

	levelRaw := ""
	if raw.DiagnosticsLogLevel != nil {
		levelRaw = *raw.DiagnosticsLogLevel
	} else if raw.DiagnosticsLogLevelDash != nil {
		levelRaw = *raw.DiagnosticsLogLevelDash
	}
	if strings.TrimSpace(levelRaw) != "" {
		normalizedLevel := strings.ToLower(strings.TrimSpace(levelRaw))
		switch normalizedLevel {
		case "debug", "info", "warn", "warning", "off", "none", "disabled":
			if normalizedLevel == "warning" {
				normalizedLevel = "warn"
			}
			if normalizedLevel == "none" || normalizedLevel == "disabled" {
				normalizedLevel = "off"
			}
			cfg.DiagnosticsLogLevel = normalizedLevel
		default:
			return Config{}, fmt.Errorf("不支持的 diagnostics_log_level 值 %q（可选：debug、info、warn、off）", levelRaw)
		}
	}

	var disabled []string
	if len(raw.DisabledRules) > 0 {
		disabled = append(disabled, raw.DisabledRules...)
	}
	if len(raw.DisabledRulesDash) > 0 {
		disabled = append(disabled, raw.DisabledRulesDash...)
	}
	if len(disabled) > 0 {
		seen := make(map[string]struct{}, len(disabled))
		normalized := make([]string, 0, len(disabled))
		for _, id := range disabled {
			cleanID := strings.ToUpper(strings.TrimSpace(id))
			if cleanID == "" {
				continue
			}
			if _, exists := seen[cleanID]; exists {
				continue
			}
			seen[cleanID] = struct{}{}
			normalized = append(normalized, cleanID)
		}
		cfg.DisabledRules = normalized
		cfg.disabledSet = seen
	}

	maxPaths := cfg.MaxDiagnosticPaths
	if raw.MaxDiagnosticPaths != nil {
		maxPaths = *raw.MaxDiagnosticPaths
	} else if raw.MaxDiagnosticPathsDash != nil {
		maxPaths = *raw.MaxDiagnosticPathsDash
	}
	if maxPaths < minDiagnosticPaths {
		maxPaths = minDiagnosticPaths
	} else if maxPaths > maxDiagnosticPathsLimit {
		maxPaths = maxDiagnosticPathsLimit
	}
	cfg.MaxDiagnosticPaths = maxPaths

	return cfg, nil
}

// Clone 返回当前配置的深拷贝副本，确保切片与内部集合不与外部共享底层内存。
func (c Config) Clone() Config {
	cloned := Config{
		DiagnosticsEndpointEnabled: c.DiagnosticsEndpointEnabled,
		DiagnosticsLogLevel:        c.DiagnosticsLogLevel,
		MaxDiagnosticPaths:         c.MaxDiagnosticPaths,
	}
	if len(c.DisabledRules) > 0 {
		cloned.DisabledRules = append([]string(nil), c.DisabledRules...)
	}
	if len(c.disabledSet) > 0 {
		cloned.disabledSet = make(map[string]struct{}, len(c.disabledSet))
		for k, v := range c.disabledSet {
			cloned.disabledSet[k] = v
		}
	} else {
		cloned.disabledSet = make(map[string]struct{})
	}
	return cloned
}

// IsRuleEnabled 判断指定规则 ID 当前是否启用。
func (c Config) IsRuleEnabled(ruleID string) bool {
	if len(c.disabledSet) == 0 {
		return true
	}
	_, disabled := c.disabledSet[strings.ToUpper(strings.TrimSpace(ruleID))]
	return !disabled
}

// ConfigFields 返回供 CLIProxyAPI 管理面板渲染的插件配置元数据声明。
func ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{
			Name:        "diagnostics_endpoint_enabled",
			Type:        pluginapi.ConfigFieldTypeBoolean,
			Default:     true,
			Description: "启用受管理密钥保护的只读脱敏诊断接口 GET /v0/management/plugins/antigravity-compat/diagnostics。",
		},
		{
			Name:        "diagnostics_log_level",
			Type:        pluginapi.ConfigFieldTypeEnum,
			Default:     "debug",
			EnumValues:  []string{"debug", "info", "warn", "off"},
			Description: "未证实字段脱敏诊断与规则命中日志的输出级别（仅记录字段路径、JSON 类型、协议与模型，绝不记录请求内容）。",
		},
		{
			Name:        "disabled_rules",
			Type:        pluginapi.ConfigFieldTypeArray,
			Default:     []string{},
			Description: "需临时停用的规则 ID 列表（如 [\"AG-COMPAT-001\"]）；默认为空表示启用全部已证实不兼容字段规则。",
		},
		{
			Name:        "max_diagnostic_paths",
			Type:        pluginapi.ConfigFieldTypeInteger,
			Default:     DefaultMaxDiagnosticPaths,
			Description: "内存中聚合保留的未证实字段路径上限（范围 16-4096）。",
		},
	}
}
