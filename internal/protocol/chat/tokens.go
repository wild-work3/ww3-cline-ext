package chat

import (
	"encoding/json"
	"math"
	"reflect"
)

// UsageSource 标记 token 数量的来源。reported 表示上游实报，estimated 表示 core 粗估。
type UsageSource struct {
	PromptReported      bool `json:"prompt_reported,omitempty"`
	CompletionReported  bool `json:"completion_reported,omitempty"`
	TotalReported       bool `json:"total_reported,omitempty"`
	PromptEstimated     bool `json:"prompt_estimated,omitempty"`
	CompletionEstimated bool `json:"completion_estimated,omitempty"`
	TotalEstimated      bool `json:"total_estimated,omitempty"`
}

// MarkReported 标记三维 token 均来自上游同一 usage 对象（W3-34）。
//
// 调用约定：**仅当上游确实下发了 usage 对象时**调用；本地推导出来的 total 必须
// 随后显式清 TotalReported，否则会把「自己加出来的数」谎报成上游实报。
func (u *Usage) MarkReported() *Usage {
	if u == nil {
		return u
	}
	u.PromptReported, u.PromptEstimated = true, false
	u.CompletionReported, u.CompletionEstimated = true, false
	u.TotalReported, u.TotalEstimated = true, false
	return u
}

// CacheFromMap 从上游原始 usage 对象提取缓存计量。
//
// 取数优先级与旧版 dev 的 provider.ExtractUsageCache 逐条一致（那是 24 份真实快照
// 实测出来的，不可随意调整）：
//
//	① prompt_cache_hit_tokens        workbuddy 系主字段（hit + miss == prompt_tokens）
//	② cache_read_input_tokens        traework/traecode 的 Anthropic 命名
//	③ prompt_tokens_details.cached_tokens / input_tokens_details.cached_tokens
//	                                 必须**非空对象且确有该键**——raccoon 下发空 {} 属「不提供」
//	④ cache_creation_input_tokens    写入侧（Anthropic 族）
//	⑤ prompt_tokens_details.cache_write_tokens（OpenAI 形态写入，兜底）
//
// **刻意不认顶层裸 cached_tokens**：workbuddy 系同一帧里 prompt_cache_hit_tokens:97
// 与 cached_tokens:0 并存，后者实测恒 0（噪声）。
// 上游没报 → 返回 nil（缺失 ≠ 0，面板显示「—」）。
func CacheFromMap(u map[string]any) *CacheUsage {
	if len(u) == 0 {
		return nil
	}
	var c CacheUsage
	if v, ok := numField(u, "prompt_cache_hit_tokens"); ok {
		c.Cached, c.CachedReported = v, true
	}
	if !c.CachedReported {
		if v, ok := numField(u, "cache_read_input_tokens"); ok {
			c.Cached, c.CachedReported = v, true
		}
	}
	if !c.CachedReported {
		for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
			d, ok := u[key].(map[string]any)
			if !ok || len(d) == 0 {
				continue
			}
			if v, ok := numField(d, "cached_tokens"); ok {
				c.Cached, c.CachedReported = v, true
				break
			}
		}
	}
	if v, ok := numField(u, "cache_creation_input_tokens"); ok {
		c.Write, c.WriteReported = v, true
	}
	if !c.WriteReported {
		for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
			d, ok := u[key].(map[string]any)
			if !ok || len(d) == 0 {
				continue
			}
			if v, ok := numField(d, "cache_write_tokens"); ok {
				c.Write, c.WriteReported = v, true
				break
			}
		}
	}
	if !c.CachedReported && !c.WriteReported {
		return nil
	}
	return &c
}

// numField 取数值字段；**键存在且可转非负数值**才算命中（负数钳 0 但仍算已上报）。
func numField(m map[string]any, key string) (int, bool) {
	if m == nil {
		return 0, false
	}
	v, exists := m[key]
	if !exists || v == nil {
		return 0, false
	}
	var n int64
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return 0, false
		}
		n = int64(t)
	case int:
		n = int64(t)
	case int64:
		n = t
	case json.Number:
		x, err := t.Int64()
		if err != nil {
			return 0, false
		}
		n = x
	default:
		return 0, false
	}
	if n < 0 {
		n = 0
	}
	return int(n), true
}

// MergeUsage 合并一帧 usage，并按 JSON 字段存在性保留“明确 0”与“未上报”的区别。
func MergeUsage(dst **Usage, fields map[string]any) {
	if fields == nil {
		return
	}
	if *dst == nil {
		*dst = &Usage{}
	}
	u := *dst
	_, hasProvenance := fields["prompt_reported"]
	if !hasProvenance {
		_, hasProvenance = fields["completion_reported"]
	}
	if !hasProvenance {
		_, hasProvenance = fields["total_reported"]
	}
	pn, pok := jsonInt(fields, "prompt_tokens")
	cn, cok := jsonInt(fields, "completion_tokens")
	tn, tok := jsonInt(fields, "total_tokens")
	// 兼容旧插件：Usage 的三个 int 字段没有 omitempty，完全未知时也会发 0/0/0。
	// 只有全零且无来源标志时按“未知”处理；非零帧仍按旧语义视为上游实报。
	legacyEmpty := !hasProvenance && pok && cok && tok && pn == 0 && cn == 0 && tn == 0
	if pok && !legacyEmpty && boolOf(fields["prompt_reported"], !hasProvenance) {
		u.PromptTokens, u.PromptReported, u.PromptEstimated = pn, true, false
	}
	if cok && !legacyEmpty && boolOf(fields["completion_reported"], !hasProvenance) {
		u.CompletionTokens, u.CompletionReported, u.CompletionEstimated = cn, true, false
	}
	if tok && !legacyEmpty && boolOf(fields["total_reported"], !hasProvenance) {
		u.TotalTokens, u.TotalReported, u.TotalEstimated = tn, true, false
	}
	if b, _ := fields["prompt_estimated"].(bool); b && !u.PromptReported {
		u.PromptTokens, u.PromptEstimated = pn, pok
	}
	if b, _ := fields["completion_estimated"].(bool); b && !u.CompletionReported {
		u.CompletionTokens, u.CompletionEstimated = cn, cok
	}
	if b, _ := fields["total_estimated"].(bool); b && !u.TotalReported {
		u.TotalTokens, u.TotalEstimated = tn, tok
	}
	if n, ok := jsonInt(fields, "time_ms"); ok {
		u.TimeMs = int64(n)
	}
	if n, ok := jsonInt(fields, "first_token_ms"); ok {
		u.FirstTokenMs = int64(n)
	}
	if c, ok := fields["cache"].(map[string]any); ok {
		if u.Cache == nil {
			u.Cache = &CacheUsage{}
		}
		if n, exists := jsonInt(c, "cached"); exists {
			u.Cache.Cached = n
			u.Cache.CachedReported = boolOf(c["cached_reported"], true)
		}
		if n, exists := jsonInt(c, "write"); exists {
			u.Cache.Write = n
			u.Cache.WriteReported = boolOf(c["write_reported"], true)
		}
	}
}

// FinalizeUsage 对上游未报告的维度做稳定粗估；不会覆盖上游明确报告的 0。
func FinalizeUsage(req *GenerateRequest, usage *Usage, completionText string) *Usage {
	if usage == nil {
		usage = &Usage{}
	}
	// 旧插件副本尚未携带来源标志；非零值可安全认定为实报。明确 0 只有新版来源标志才能区分。
	if !usage.PromptReported && !usage.PromptEstimated && usage.PromptTokens > 0 {
		usage.PromptReported = true
	}
	if !usage.CompletionReported && !usage.CompletionEstimated && usage.CompletionTokens > 0 {
		usage.CompletionReported = true
	}
	if !usage.TotalReported && !usage.TotalEstimated && usage.TotalTokens > 0 {
		usage.TotalReported = true
	}
	if !usage.PromptReported {
		usage.PromptTokens, usage.PromptEstimated = EstimatePromptTokens(req), true
	}
	if !usage.CompletionReported {
		usage.CompletionTokens, usage.CompletionEstimated = EstimateTextTokens(completionText), true
	}
	if !usage.TotalReported {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		usage.TotalEstimated = true
	}
	return usage
}

// EstimatePromptTokens 估算归一化请求输入：CJK 约 1 字/token，其余约 4 字/token。
func EstimatePromptTokens(req *GenerateRequest) int {
	if req == nil {
		return 0
	}
	var cjk, other, blocks int
	add := func(v any) { countValue(v, &cjk, &other, &blocks) }
	for _, m := range req.Messages {
		add(m.Content)
		add(m.Name)
		add(m.ToolCallID)
		for _, tc := range m.ToolCalls {
			add(tc.ID)
			if tc.Function != nil {
				add(tc.Function.Name)
				add(tc.Function.Arguments)
			}
		}
		blocks++
	}
	if len(req.Tools) > 0 {
		if b, err := json.Marshal(req.Tools); err == nil {
			add(string(b))
		}
	}
	n := cjk + int(math.Ceil(float64(other)/4))
	if min := blocks * 2; n < min {
		n = min
	}
	return n
}

func EstimateTextTokens(s string) int {
	if s == "" {
		return 0
	}
	cjk, other, blocks := 0, 0, 0
	countValue(s, &cjk, &other, &blocks)
	return cjk + int(math.Ceil(float64(other)/4))
}

func countValue(v any, cjk, other, blocks *int) {
	if v == nil {
		return
	}
	switch x := v.(type) {
	case string:
		for _, r := range x {
			if r > 0x2E80 {
				*cjk = *cjk + 1
			} else {
				*other = *other + 1
			}
		}
	case json.RawMessage:
		if len(x) > 0 {
			countValue(string(x), cjk, other, blocks)
		}
	case []any:
		for _, e := range x {
			countValue(e, cjk, other, blocks)
		}
		*blocks += len(x)
	case map[string]any:
		for _, k := range []string{"text", "content", "input", "name", "description", "arguments", "thinking", "input_text", "output_text", "summary_text"} {
			countValue(x[k], cjk, other, blocks)
		}
	default:
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Map || rv.Kind() == reflect.Struct {
			if b, err := json.Marshal(v); err == nil {
				countValue(string(b), cjk, other, blocks)
			}
		}
	}
}

func jsonInt(m map[string]any, key string) (int, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	max := uint64(^uint(0) >> 1)
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n != math.Trunc(n) || n > float64(max) {
			return 0, false
		}
		return int(n), true
	case int:
		return n, n >= 0
	case int64:
		if n < 0 || uint64(n) > max {
			return 0, false
		}
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil || i < 0 || uint64(i) > max {
			return 0, false
		}
		return int(i), true
	}
	return 0, false
}
func boolOf(v any, def bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
