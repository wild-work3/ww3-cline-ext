package cline

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"

	"wild-work3/cline-ext/internal/protocol/errkind"
)

// Error 是渠道层的结构化错误。
type Error struct {
	Kind   errkind.Kind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Msg
}

// newErr 构造结构化错误。
func newErr(kind errkind.Kind, status int, msg string) *Error {
	return &Error{Kind: kind, Status: status, Msg: msg}
}

// AsError 提取 *Error，否则包装为 kind。
func AsError(err error, kind errkind.Kind) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return newErr(errkind.Cancel, 0, err.Error())
	}
	return newErr(kind, 0, err.Error())
}

// ---------------------------------------------------------------------------
// 错误分类（逐字搬运 dev/internal/cline/errors.go，判据顺序敏感）
// ---------------------------------------------------------------------------

// innerStatusRE 从压平的响应体里取**内层**状态码。
// 实测形态：`… from Openrouter: request failed with status 429: {"error":{…}}`
// 外层一律是 HTTP 500，不挖这一层就只能把所有 429 当 5xx（违反不变量 15 的「429 优先」）。
var innerStatusRE = regexp.MustCompile(`(?i)\bstatus\s+(\d{3})`)

// capMarkers 标识「上限在 Cline→OpenRouter 共享出口上」的文案。
// 命中即意味着：**换 Cline 账号必然复现**（与 R38 的模型级 429 同构），
// 所以不得罚账号，也不得同请求换号。
var capMarkers = []string{
	"limit_rpd",                // OpenRouter 每日请求数上限（实测 thinkingmachines/inkling:free）
	"daily limit reached",      // 同上的人类可读形式，且明说 "Credits don't affect this cap"
	"temporarily rate-limited", // 短时上游限流，实测可在数十秒后自愈（gemma-4-26b 一轮 429、一轮 200）
	"rate limit exceeded",
}

// passMarkers 是「越界用法」：请求了订阅池或不受支持的池，属请求本身的问题。
var passMarkers = []string{"entitlement_error", "not subscribed", "this request is not supported"}

// regionBlockedMarkers 是「模型在本地区被限制」：实测 muse-spark-1.3-contributor 恒返回
// 403 `access forbidden: … is not available in your region`（地区限制，不是账号问题）。
// 与 R38（WorkBuddyAI 模型级 429）/ R47（cline 共享出口上限）同族：**模型级属性**，
// 换账号必然复现 ⇒ 必须 ErrPassthrough（原文透传、不计错、不冷却、不换号）。
// 实测教训（2026-10-12）：未识别时它落 ErrClient（R29 可轮换）⇒ 重试耗尽后把账号判为
// 「连续错误」冷却 60s，导致同账号下另外 3 个正常模型全部 503 no_healthy_account。
var regionBlockedMarkers = []string{"not available in your region", "access forbidden"}

// badParamMarkers 是本端参数/头造成的失败，不得计入账号健康度。
var badParamMarkers = []string{
	"empty response content", // max_tokens 太小，推理吃光预算 ⇒ 可见内容为空
	"api_request_error_code", // 多半是漏发 X-CLIENT-TYPE（自己的 bug，不能算到账号头上）
	"invalid 'max_output_tokens'",
	"below minimum value",
	"integer below minimum",
}

// IsCapBody 报告响应体是否命中「共享出口上限」文案（调用方据此标记模型级冷却）。
func IsCapBody(body string) bool {
	lower := strings.ToLower(body)
	for _, m := range capMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// InnerStatus 取响应体里的内层状态码，无则 0。
func InnerStatus(body string) int {
	if m := innerStatusRE.FindStringSubmatch(body); len(m) == 2 {
		var n int
		if _, err := fmt.Sscanf(m[1], "%d", &n); err == nil && n >= 400 && n <= 599 {
			return n
		}
	}
	return 0
}

// effectiveStatus 把网关压平的 500 还原成内层真实状态码。
//
// 这是对「上游 ≥400 直接透传原始响应」的**刻意偏离**：响应体仍然原文透传，
// 只把状态码修正为上游本来的意思。理由：上游把 429 报成 500 是它们网关的缺陷，
// 原样转给客户端会让客户端按「永久错误」处理而放弃重试（429 才带退避语义）。
// 无内层码时保持外层状态不变。
func effectiveStatus(status int, body string) int {
	if status >= 500 {
		if in := InnerStatus(body); in != 0 {
			return in
		}
	}
	return status
}

// Classify 把 HTTP 状态 + 响应文本映射为 errkind.Kind。
// 顺序敏感：共享上限 → 越界 → 参数 → 地区限制 → 内层码 → 外层码。
func Classify(status int, body string) errkind.Kind {
	lower := strings.ToLower(body)

	// 1) 共享出口上限：账号健康与此无关 ⇒ 透传原文、不罚号、不换号（换号必复现）。
	if IsCapBody(body) {
		return errkind.Passthrough
	}
	// 2) 订阅池/云池越界：客户端硬填了 cline-pass/* 或 cline-cloud/*。
	for _, m := range passMarkers {
		if strings.Contains(lower, m) {
			return errkind.Passthrough
		}
	}
	// 3) 本端参数/头造成的失败。
	for _, m := range badParamMarkers {
		if strings.Contains(lower, m) {
			return errkind.BadParams
		}
	}
	// 3.5) 模型地区限制：模型级属性，换账号必复现 ⇒ 透传原文、不罚号。
	for _, m := range regionBlockedMarkers {
		if strings.Contains(lower, m) {
			return errkind.Passthrough
		}
	}

	// 4) 内层码优先（外层几乎恒为 500）—— 实测事实 2 的落地点。
	if in := InnerStatus(body); in != 0 {
		status = in
	}

	// 5) 余额/权益文案（须在内层码还原**之后**判：限流 body 高频带 quota/credit 字样，
	//    先判会把限流误归成余额耗尽 → 12h 硬冷却，即不变量 15 描述的坑）。
	if strings.Contains(lower, "insufficient") || strings.Contains(lower, "credit balance") ||
		strings.Contains(lower, "exceeded your current quota") {
		return errkind.HardCredit
	}

	switch {
	case status == 401 || (status == 403 && strings.Contains(lower, "unauthorized")):
		// Key 被删/失效：多账号池里禁用该号是正确行为。
		// 注意本渠道 Key 不可刷新 ⇒ 自愈不适用，只能人工换 Key。
		return errkind.SessionDead
	case status == 429:
		// 无上限文案的 429 ⇒ 才是真·账号级（配额键名 …PerUser）⇒ 允许冷却+轮换。
		return errkind.SoftRate
	case status == 402:
		return errkind.HardCredit
	case status == 404:
		// 实测 poolside/laguna-xs：404 `please check the model you provided` + previous_errors 里
		// 有 429 ⇒ 像被驱逐/下架。模型属性而非账号属性，但按短冷却处理不会误伤整号。
		return errkind.NotFound
	case status >= 500:
		return errkind.Server
	case status >= 400:
		return errkind.Client
	default:
		return errkind.Client
	}
}

// ---------------------------------------------------------------------------
// 网络错误
// ---------------------------------------------------------------------------

// kindOfNetErr 把底层网络错误归类。
func kindOfNetErr(err error) errkind.Kind {
	if err == nil {
		return errkind.Server
	}
	if errors.Is(err, context.Canceled) {
		return errkind.Cancel
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errkind.IdleTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errkind.IdleTimeout
	}
	return errkind.Conn
}

// containsAny 判断 s 是否含任一子串。
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
