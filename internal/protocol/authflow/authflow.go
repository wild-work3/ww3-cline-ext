// Package authflow 定义渠道插件登录授权的统一 IPC 契约（PLUGIN_PROTOCOL §5.3 / W3-19）。
//
// 设计目标：core 只认**范式**（Type），不认渠道。所有渠道的登录差异被压成 5 种范式，
// core 据范式决定「要不要起轮询」「要不要把 URL 交给浏览器」「要不要暴露粘贴入口」，
// 从而不需要为每个新渠道改 core（旧版 app.go 用 4337 行硬编码 13 个渠道的登录编排）。
package authflow

// Type 登录范式。用字符串而非枚举整数，便于跨语言与向前兼容。
type Type string

const (
	// TypeDevicePoll 服务端轮询：插件向自己的上游申请 state/device_code 并自行轮询，
	// 浏览器只需打开插件给的 auth_url。插件**不监听本地端口**，天然支持跨机。
	// 覆盖旧版 workbuddy(auth/token)、qoder(deviceToken/poll)、zcode(oauth/cli/poll)、
	// minimax(oauth2/device/code)。
	TypeDevicePoll Type = "device_poll"
	// TypeLoopback 回环回调：插件监听 127.0.0.1:<port> 收 code 再换 token。
	// 浏览器必须与插件同机才有自动回调；跨机场景靠用户粘贴回调 URL 兜底。
	// 覆盖旧版 traework/codearts/lobsterai/qwenwork/gemini。
	TypeLoopback Type = "loopback_callback"
	// TypeCDP 插件拉起独立 profile 浏览器并读 Cookie（调试端口限 127.0.0.1）。
	// 覆盖旧版 glm / raccoon。
	TypeCDP Type = "cdp_capture"
	// TypeForm 用户表单输入（API Key / Cookie 等），插件校验后落盘。
	// 覆盖旧版 custom / cline / glm 手工粘贴。
	TypeForm Type = "form"
	// TypeCredentialImport 从本机已登录的官方客户端导入凭据。
	// 覆盖旧版 monkeycode。
	TypeCredentialImport Type = "credential_import"
	// TypeNone 无需登录（匿名通道 / 免费池）。
	TypeNone Type = "none"
)

// LegacyTypeMap 旧版 @PLUGIN_PROTOCOL v1 §5.3 的五类旧名 → 新范式。
// 保留映射是为了不破坏已发布插件（W3-12 版本兼容协商）。
var LegacyTypeMap = map[string]Type{
	"form":              TypeForm,
	"browser_callback":  TypeLoopback,
	"cdp_browser":       TypeCDP,
	"credential_import": TypeCredentialImport,
	"device_code":       TypeDevicePoll,
	"state_poll":        TypeDevicePoll, // 旧文档内文称呼
	"none":              TypeNone,
}

// Normalize 把旧名/新名统一成新范式；未知值原样返回（由调用方校验）。
func Normalize(t string) Type {
	if v, ok := LegacyTypeMap[t]; ok {
		return v
	}
	return Type(t)
}

// NeedsPoll 该范式是否需要调用方起轮询（设备流 / 回环回调 / CDP 都需要，form 与 none 不需要）。
// 这是 core 侧「要不要起 status 循环」的唯一判据——不要在前端/网关再判一次。
func (t Type) NeedsPoll() bool {
	switch t {
	case TypeDevicePoll, TypeLoopback, TypeCDP:
		return true
	}
	return false
}

// NeedsBrowser 该范式是否需要把 auth_url 交给用户浏览器。
func (t Type) NeedsBrowser() bool {
	switch t {
	case TypeDevicePoll, TypeLoopback, TypeCDP:
		return true
	}
	return false
}

// CanPasteCallback 只有回环回调需要人工粘贴兜底（跨机场景的唯一通用解，R101）。
func (t Type) CanPasteCallback() bool { return t == TypeLoopback }

// Method 插件声明的一种登录方式（auth.methods 的数组元素）。
//
// 同一个插件可声明多种（如 workbuddy 支持 device_poll 与 credential_import），
// core 全部原样透传给前端，由用户选择；选择结果回传给 auth.begin。
type Method struct {
	ID          string `json:"id"`    // 方式标识（插件内唯一），如 "oauth" / "apikey"
	Type        Type   `json:"type"`  // 范式
	Label       string `json:"label"` // 展示名
	Description string `json:"description,omitempty"`
	// Fields 仅 TypeForm / TypeCredentialImport 使用：字段定义沿用 JSON Schema 子集，
	// 前端复用 schemaForm() 渲染（与 config.schema 同一套代码路径）。
	Fields map[string]any `json:"fields,omitempty"`
	// Region 该方式适用的区域（如 workbuddy 的 cn / global）；空表示不限。
	Region string `json:"region,omitempty"`
	// Default 建议默认选中的方式（多方式时前端高亮）。
	Default bool `json:"default,omitempty"`
}

// State 一次登录流程的状态（auth.status 的 state 字段）。
type State string

const (
	StatePending         State = "pending"          // 已发起，等用户操作
	StateWaitingUser     State = "waiting_user"     // 等用户在浏览器完成授权
	StateWaitingCallback State = "waiting_callback" // 等浏览器回调到达本地端口
	StateWaitingInput    State = "waiting_input"    // 等用户粘贴回调 URL / 表单提交
	StateSuccess         State = "success"          // 成功（携带 account）
	StateFailed          State = "failed"           // 失败（携带 error）
	StateExpired         State = "expired"          // 超时
	StateCancelled       State = "cancelled"        // 用户取消
)

// Terminal 是否终态（core 收到终态即停止轮询并回收会话）。
func (s State) Terminal() bool {
	switch s {
	case StateSuccess, StateFailed, StateExpired, StateCancelled:
		return true
	}
	return false
}

// BeginRequest auth.begin 的请求体。
type BeginRequest struct {
	Method string `json:"method,omitempty"` // 空 = 用插件声明的默认方式
	// Region 区域（cn / global）；插件可据此选端点，如 workbuddy 双区共用一份实现。
	Region string `json:"region,omitempty"`
	// Values 表单形态的输入（API Key / Cookie 等），Form 与 CredentialImport 用。
	Values map[string]string `json:"values,omitempty"`
	// AccountUID 追加账号时的提示（部分渠道的凭据按账号存储）。
	AccountUID string `json:"account_uid,omitempty"`
}

// BeginResult auth.begin 的返回。空字段省略，不臆造。
type BeginResult struct {
	FlowID string `json:"flow_id"` // core 用它关联后续 status/finish/cancel
	Type   Type   `json:"type"`
	Method string `json:"method"`
	// AuthURL 需要用户打开的授权页（device_poll / loopback / cdp 均有）。
	AuthURL string `json:"auth_url,omitempty"`
	// UserCode 设备码（用户需在授权页手输时展示）。
	UserCode string `json:"user_code,omitempty"`
	// VerificationURI 设备码配套的输入页（与 UserCode 同时出现）。
	VerificationURI string `json:"verification_uri,omitempty"`
	// CallbackURL 插件已监听的本地回调地址（loopback 范式），供用户核对/端口转发。
	CallbackURL string `json:"callback_url,omitempty"`
	// DebugPort CDP 调试端口（限 127.0.0.1）。
	DebugPort int    `json:"debug_port,omitempty"`
	State     string `json:"state,omitempty"`
	// Prompt 给用户看的一步提示（core 直接展示，不再自行拼文案）。
	Prompt string `json:"prompt,omitempty"`
	// ExpiresAt 流程有效期（RFC3339）；空 = 由 core 按默认 10 分钟。
	ExpiresAt string `json:"expires_at,omitempty"`
	// CanPasteCallback 是否支持「粘贴回调 URL」（core 据此决定是否暴露输入框）。
	CanPasteCallback bool `json:"can_paste_callback,omitempty"`
	// NextPollMS 建议轮询间隔（毫秒）；0 = 由 core 用默认 2000ms。
	NextPollMS int `json:"next_poll_ms,omitempty"`
}

// StatusRequest auth.status 的请求体。
type StatusRequest struct {
	FlowID string `json:"flow_id"`
}

// Event 流程进度事件（CDP 探测到浏览器、回调已到达、正在换 token…）。
type Event struct {
	TS      string `json:"ts"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// StatusResult auth.status 的返回。
type StatusResult struct {
	State State  `json:"state"`
	Hint  string `json:"hint,omitempty"`
	// NextPollMS 覆盖本次建议间隔（如上游给了 retry_after）。
	NextPollMS int `json:"next_poll_ms,omitempty"`
	// Account 仅 StateSuccess 时出现：插件已完成凭据落盘，这里只报事实。
	Account *Account `json:"account,omitempty"`
	// Error 仅 StateFailed 时出现。
	Error *Error `json:"error,omitempty"`
	// Events 本次新增的进度事件（增量，core 只需追加）。
	Events []Event `json:"events,omitempty"`
}

// Account 登录成功后上报的账号事实（凭据本身由插件私有目录持久化，绝不过 IPC，W3-07）。
type Account struct {
	UID       string   `json:"uid"`
	Nickname  string   `json:"nickname,omitempty"`
	ChannelID string   `json:"channel_id,omitempty"`
	Region    string   `json:"region,omitempty"`
	ExpiresAt string   `json:"expires_at,omitempty"`
	Credits   *float64 `json:"credits,omitempty"`
	Unit      string   `json:"unit,omitempty"`
	// LoginKind 是登录方式的事实（插件上报，core 落库不猜渠道；可选）。
	LoginKind string `json:"login_kind,omitempty"`
	// AuthFile 是凭据标识＝<data>/auths/<auth_file>.json 的文件名（无 .json 后缀）。
	//
	// 与 UID 的区别：UID 是上游账号事实（同一自然人可能两区同名），AuthFile 是
	// **本插件磁盘上的唯一定位**（含区域前缀）；core 把它当账号主键与 `account_uid`
	// 回传给插件（W3-21/22，PLUGIN_CLI §5.2）。缺省时 core 退回 UID。
	AuthFile string `json:"auth_file,omitempty"`
}

// Error 登录失败分类。Kind 沿用 errkind 全集，core 据此决定是否可重试。
type Error struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// FinishRequest auth.finish 的请求体：手工粘贴回调 URL / 表单补充输入。
// 这是跨机登录的唯一通用兜底（R101），与自动回调走**同一条**兑换路径。
type FinishRequest struct {
	FlowID      string            `json:"flow_id"`
	CallbackURL string            `json:"callback_url,omitempty"`
	Values      map[string]string `json:"values,omitempty"`
}

// CancelRequest auth.cancel 的请求体。
type CancelRequest struct {
	FlowID string `json:"flow_id"`
}

// MethodsResult auth.methods 的返回。
type MethodsResult struct {
	// Region 该账号/渠道的当前区域（workbuddy 系用于前端展示与端点选择）。
	Region  string   `json:"region,omitempty"`
	Methods []Method `json:"methods"`
	// Note 整体说明（如「匿名通道无需登录」），属于 TypeNone 的可读理由。
	Note string `json:"note,omitempty"`
}
