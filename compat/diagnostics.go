package compat

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/MoeForge/moego/ojson"
	"github.com/MoeForge/moego/regexp"
)

// HostLogFunc 定义向宿主输出结构化脱敏日志的回调函数签名。
type HostLogFunc func(level, message string, fields map[string]any)

var (
	safeKeySegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	safeModelNamePattern  = regexp.MustCompile(`^[A-Za-z0-9_./:\[\]()-]{1,128}$`)
)

// DiagnosticCategory 标识未证实字段的诊断分类。
type DiagnosticCategory string

const (
	// DiagnosticUnknownProtoField 表示在固定 Proto 消息层级中发现未列入已知 Schema、但尚无 400 证据的字段。
	DiagnosticUnknownProtoField DiagnosticCategory = "unknown_proto_field"
	// DiagnosticUnverifiedCombination 表示社区曾讨论但尚无线上 400 实证的参数组合（原样放行，仅做统计诊断）。
	DiagnosticUnverifiedCombination DiagnosticCategory = "unverified_combination"
)

// DiagnosticFinding 表示单次观测到的未证实字段脱敏元数据（严禁包含任何字段值或提示词内容）。
type DiagnosticFinding struct {
	Category     DiagnosticCategory `json:"category"`
	FieldPath    string             `json:"field_path"`
	ValueType    string             `json:"value_type"`
	FromProtocol string             `json:"from_protocol"`
	ToProtocol   string             `json:"to_protocol"`
	Model        string             `json:"model"`
	Note         string             `json:"note"`
}

// DiagnosticsCounters 汇总模块运行期的脱敏计数器。
type DiagnosticsCounters struct {
	AntigravityInspected   uint64 `json:"antigravity_inspected"`
	AntigravityMutated     uint64 `json:"antigravity_mutated"`
	NonAntigravityBypassed uint64 `json:"non_antigravity_bypassed"`
	CleanedFieldsTotal     uint64 `json:"cleaned_fields_total"`
	DiagnosticFindings     uint64 `json:"diagnostic_findings_total"`
	DiagnosticDroppedKeys  uint64 `json:"diagnostic_dropped_keys"`
}

// RuleDiagnosticSummary 展示每条已证实不兼容规则的定义、证据与累计命中次数。
type RuleDiagnosticSummary struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Enabled    bool     `json:"enabled"`
	FieldPaths []string `json:"field_paths"`
	Protocols  []string `json:"protocols"`
	Evidence   string   `json:"evidence"`
	HitCount   uint64   `json:"hit_count"`
}

// ObservedFieldSummary 展示某条未证实字段路径的聚合脱敏观测记录。
type ObservedFieldSummary struct {
	Category     DiagnosticCategory `json:"category"`
	FieldPath    string             `json:"field_path"`
	ValueType    string             `json:"value_type"`
	FromProtocol string             `json:"from_protocol"`
	ToProtocol   string             `json:"to_protocol"`
	LastModel    string             `json:"last_model"`
	Action       string             `json:"action"`
	Note         string             `json:"note"`
	Count        uint64             `json:"count"`
}

// DiagnosticsSnapshot 为只读诊断接口返回的完整脱敏快照。
type DiagnosticsSnapshot struct {
	Plugin                 string                  `json:"plugin"`
	Version                string                  `json:"version"`
	BaselineVersion        string                  `json:"baseline_version"`
	BaselineCommit         string                  `json:"baseline_commit"`
	IsolationMode          string                  `json:"isolation_mode"`
	Counters               DiagnosticsCounters     `json:"counters"`
	Rules                  []RuleDiagnosticSummary `json:"rules"`
	ObservedUnprovenFields []ObservedFieldSummary  `json:"observed_unproven_fields"`
}

type observedEntry struct {
	category     DiagnosticCategory
	fieldPath    string
	valueType    string
	fromProtocol string
	toProtocol   string
	lastModel    string
	note         string
	count        uint64
}

// DiagnosticsStore 维护并发安全、容量受限的脱敏诊断状态。
type DiagnosticsStore struct {
	antigravityInspected   atomic.Uint64
	antigravityMutated     atomic.Uint64
	nonAntigravityBypassed atomic.Uint64
	cleanedFieldsTotal     atomic.Uint64
	diagnosticFindings     atomic.Uint64
	diagnosticDroppedKeys  atomic.Uint64

	mu       sync.RWMutex
	maxPaths int
	ruleHits map[string]uint64
	observed map[string]*observedEntry
}

// NewDiagnosticsStore 创建带容量上限的脱敏诊断存储。
func NewDiagnosticsStore(maxPaths int) *DiagnosticsStore {
	if maxPaths < minDiagnosticPaths {
		maxPaths = DefaultMaxDiagnosticPaths
	}
	return &DiagnosticsStore{
		maxPaths: maxPaths,
		ruleHits: make(map[string]uint64),
		observed: make(map[string]*observedEntry),
	}
}

// SetMaxPaths 动态更新未证实字段路径聚合上限；若新上限小于已观测条目数，按确定性顺序淘汰低频多余条目。
func (s *DiagnosticsStore) SetMaxPaths(maxPaths int) {
	if s == nil {
		return
	}
	if maxPaths < minDiagnosticPaths {
		maxPaths = minDiagnosticPaths
	} else if maxPaths > maxDiagnosticPathsLimit {
		maxPaths = maxDiagnosticPathsLimit
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxPaths = maxPaths
	if len(s.observed) <= maxPaths {
		return
	}

	type kv struct {
		key   string
		count uint64
	}
	items := make([]kv, 0, len(s.observed))
	for k, v := range s.observed {
		items = append(items, kv{key: k, count: v.count})
	}
	// 优先保留高频路径；相同命中次数下保留字典序靠前的路径，确保缩容行为确定可测。
	sort.Slice(items, func(i, j int) bool {
		if items[i].count != items[j].count {
			return items[i].count > items[j].count
		}
		return items[i].key < items[j].key
	})
	for idx := maxPaths; idx < len(items); idx++ {
		delete(s.observed, items[idx].key)
	}
}

// RecordBypass 记录一次因目标渠道不是 Antigravity 而原样透传的请求。
func (s *DiagnosticsStore) RecordBypass() {
	if s == nil {
		return
	}
	s.nonAntigravityBypassed.Add(1)
}

// RecordAntigravityOutcome 记录一次 Antigravity 请求的规则清理与未证实字段观测结果。
func (s *DiagnosticsStore) RecordAntigravityOutcome(cleaned []CleanedField, findings []DiagnosticFinding) {
	if s == nil {
		return
	}
	s.antigravityInspected.Add(1)
	if len(cleaned) > 0 {
		s.antigravityMutated.Add(1)
		s.cleanedFieldsTotal.Add(uint64(len(cleaned)))
	}
	if len(findings) > 0 {
		s.diagnosticFindings.Add(uint64(len(findings)))
	}
	if len(cleaned) == 0 && len(findings) == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, item := range cleaned {
		s.ruleHits[item.RuleID]++
	}
	for _, f := range findings {
		key := string(f.Category) + "|" + f.FromProtocol + "|" + f.FieldPath + "|" + f.ValueType
		if existing, ok := s.observed[key]; ok {
			existing.count++
			existing.lastModel = f.Model
			continue
		}
		if len(s.observed) >= s.maxPaths {
			s.diagnosticDroppedKeys.Add(1)
			continue
		}
		s.observed[key] = &observedEntry{
			category:     f.Category,
			fieldPath:    f.FieldPath,
			valueType:    f.ValueType,
			fromProtocol: f.FromProtocol,
			toProtocol:   f.ToProtocol,
			lastModel:    f.Model,
			note:         f.Note,
			count:        1,
		}
	}
}

// Snapshot 生成当前时刻的只读脱敏统计快照。
func (s *DiagnosticsStore) Snapshot(cfg Config, rules []Rule) DiagnosticsSnapshot {
	snap := DiagnosticsSnapshot{
		Plugin:                 PluginID,
		Version:                PluginVersion,
		BaselineVersion:        BaselineVersion,
		BaselineCommit:         BaselineCommit,
		IsolationMode:          "executor_target_format_antigravity_only",
		Rules:                  make([]RuleDiagnosticSummary, 0, len(rules)),
		ObservedUnprovenFields: make([]ObservedFieldSummary, 0),
	}
	if s == nil {
		return snap
	}

	snap.Counters = DiagnosticsCounters{
		AntigravityInspected:   s.antigravityInspected.Load(),
		AntigravityMutated:     s.antigravityMutated.Load(),
		NonAntigravityBypassed: s.nonAntigravityBypassed.Load(),
		CleanedFieldsTotal:     s.cleanedFieldsTotal.Load(),
		DiagnosticFindings:     s.diagnosticFindings.Load(),
		DiagnosticDroppedKeys:  s.diagnosticDroppedKeys.Load(),
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, r := range rules {
		snap.Rules = append(snap.Rules, RuleDiagnosticSummary{
			ID:         r.ID,
			Name:       r.Name,
			Enabled:    cfg.IsRuleEnabled(r.ID),
			FieldPaths: append([]string(nil), r.FieldPaths...),
			Protocols:  append([]string(nil), r.Protocols...),
			Evidence:   r.Evidence,
			HitCount:   s.ruleHits[r.ID],
		})
	}

	for _, entry := range s.observed {
		snap.ObservedUnprovenFields = append(snap.ObservedUnprovenFields, ObservedFieldSummary{
			Category:     entry.category,
			FieldPath:    entry.fieldPath,
			ValueType:    entry.valueType,
			FromProtocol: entry.fromProtocol,
			ToProtocol:   entry.toProtocol,
			LastModel:    entry.lastModel,
			Action:       "passthrough_diagnostic_only",
			Note:         entry.note,
			Count:        entry.count,
		})
	}
	sort.Slice(snap.ObservedUnprovenFields, func(i, j int) bool {
		a, b := snap.ObservedUnprovenFields[i], snap.ObservedUnprovenFields[j]
		if a.Category != b.Category {
			return a.Category < b.Category
		}
		if a.FieldPath != b.FieldPath {
			return a.FieldPath < b.FieldPath
		}
		if a.FromProtocol != b.FromProtocol {
			return a.FromProtocol < b.FromProtocol
		}
		return a.ValueType < b.ValueType
	})
	return snap
}

// SanitizeKeySegment 对 JSON 字段键名做脱敏规整，仅允许安全标识符字符，防止异常键名携带敏感数据。
func SanitizeKeySegment(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "<empty_key>"
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "sk-") ||
		strings.HasPrefix(lower, "aiza") ||
		strings.HasPrefix(lower, "ya29.") ||
		strings.HasPrefix(lower, "ghp_") ||
		strings.HasPrefix(lower, "bearer") {
		return "<redacted_key>"
	}
	if !safeKeySegmentPattern.MatchString(trimmed) {
		return "<redacted_key>"
	}
	return trimmed
}

// SanitizeModelName 对模型标识做脱敏校验，仅保留合法模型名格式。
func SanitizeModelName(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "<unspecified>"
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "sk-") ||
		strings.HasPrefix(lower, "aiza") ||
		strings.HasPrefix(lower, "ya29.") ||
		strings.HasPrefix(lower, "ghp_") {
		return "<redacted_model>"
	}
	if !safeModelNamePattern.MatchString(trimmed) {
		return "<redacted_model>"
	}
	return trimmed
}

// SanitizeProtocolName 规范化协议名称用于脱敏统计展示。
func SanitizeProtocolName(raw string) string {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return "unknown"
	}
	if !safeKeySegmentPattern.MatchString(trimmed) {
		return "unknown"
	}
	return trimmed
}

// ValueTypeName 返回 JSON 节点的抽象类型名，绝不读取或暴露实际值内容。
func ValueTypeName(res ojson.Result) string {
	if !res.Exists() {
		return "missing"
	}
	switch res.Type {
	case ojson.Null:
		return "null"
	case ojson.True, ojson.False:
		return "boolean"
	case ojson.Number:
		return "number"
	case ojson.String:
		return "string"
	case ojson.JSON:
		if res.IsArray() {
			return "array"
		}
		if res.IsObject() {
			return "object"
		}
		return "json"
	default:
		return "unknown"
	}
}
