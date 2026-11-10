// Package errkind 定义统一错误分类（沿用旧版 provider.ErrKind 全集），供 core 决策冷却/换号/降级。
package errkind

// Scope 错误作用域。
type Scope = string

const (
	ScopeRequest Scope = "request"
	ScopeModel   Scope = "model"
	ScopeAccount Scope = "account"
	ScopeChannel Scope = "channel"
)

// Kind 稳定错误码。
type Kind = string

const (
	HardCredit     Kind = "hard_credit"     // 余额/权益不足 → 长冷却
	SoftRate       Kind = "soft_rate"       // 429 软限流 → 短冷却
	SessionDead    Kind = "session_dead"    // 登录态失效 → 禁用
	NotFound       Kind = "not_found"       // 上游偶发 404
	Server         Kind = "server"          // 5xx 上游故障（不换号）
	Client         Kind = "client"          // 其他 4xx/业务错误
	ContentBlocked Kind = "content_blocked" // 内容策略拦截
	PromptTooLong  Kind = "prompt_too_long" // 上下文超限
	ImageInvalid   Kind = "image_invalid"   // 图片无效
	BadParams      Kind = "bad_params"      // 出站 body/参数不被接受
	WafBlock       Kind = "waf_block"       // 403+非业务信封（WAF 页）
	AccountFault   Kind = "account_fault"   // 账号级授权/配额故障
	ModelBlocked   Kind = "model_blocked"   // 后端无此模型
	Passthrough    Kind = "passthrough"     // 请求级拒绝，原文透传
	Queued         Kind = "queued"          // 模型排队

	// 扩展分类（必须显式判定 Rotatable，避免 core 误判）
	Conn        Kind = "conn"         // 连接失败
	Auth        Kind = "auth"         // 认证失败
	IdleTimeout Kind = "idle_timeout" // 空闲看门狗超时
	Cancel      Kind = "cancel"       // 客户端取消
	Truncated   Kind = "truncated"    // 流被截断
	ServerErr   Kind = "server_error" // core/网关内部错误
	NotFoundOp  Kind = "not_found_op" // 未知方法
	// ReasoningLoop 思考链死循环（发癫）拦截器命中：主动终止 SSE 并向下游报错（W3-26）。
	// 刻意**不**可换号（见 Rotatable）：死循环是模型/请求级问题，换账号必然复现。
	ReasoningLoop Kind = "reasoning_loop"
	// NotSupported 渠道不支持该可选能力（W3-30：systemone.decide / images.generate 的统一兜底）。
	// scope=channel，不可换号不冷却（Rotatable=false）——换号只会撞到同一个不支持。
	NotSupported Kind = "not_supported"
)

// Rotatable 报告该错误是否应该换号重试（旧版 Rotatable() 语义）。
// ErrSoftRate/ErrHardCredit/ErrSessionDead/ErrNotFound/ErrAccountFault/ErrClient/ErrQueued ⇒ 换号；ErrServer 除外。
//
// Conn（连接失败）**不在列**：传输层故障与账号健康无关，换号必撞同一面墙，
// 只会把全池账号逐个白打一遍（旧版 2026-10-20 已把传输层/5xx 定为 upstream-transparent，
// 见 dev/docs/模型降级链定义.md §5）。
func Rotatable(k Kind) bool {
	switch k {
	case SoftRate, HardCredit, SessionDead, NotFound, AccountFault, Client, Queued, Auth:
		return true
	}
	return false
}

// Terminal 报告错误是否必定终结该请求（全部 kind 都是终结）。
func Terminal(k Kind) bool { return k != "" }
