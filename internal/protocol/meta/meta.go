// Package meta 定义插件元数据（manifest）、能力清单与模型/资源信息结构（PLUGIN_PROTOCOL §3/§5）。
package meta

import "strings"

// Manifest 插件元数据（控制面 initialize 上报，也用于清单/商店）。
type Manifest struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	Kind         string       `json:"kind"` // channel | hook
	Protocol     ProtocolRng  `json:"protocol"`
	Execution    Execution    `json:"execution"`
	Capabilities Capabilities `json:"capabilities,omitempty"`
	// DefaultChannels 声明安装后应存在的渠道实例；空表表示不自动创建。
	// initialize 返回同一份 Manifest，旧插件不带此字段仍可正常运行。
	DefaultChannels []DefaultChannel `json:"default_channels,omitempty"`
	// MultiChannels=true：本插件的渠道实例由用户在面板里自建（可任意多个），不靠 default_channels 预置。
	// 实例 ID 就是模型命名空间前缀（custom-api 这类「一个端点 = 一个前缀」的渠道；W3-44）。
	MultiChannels bool   `json:"multi_channels,omitempty"`
	Icon          string `json:"icon,omitempty"`
	Color         string `json:"color,omitempty"`
	Description   string `json:"description,omitempty"`
}

// DefaultChannel 是插件自带的渠道实例模板。Config 仅含可公开的默认值，不含凭据。
type DefaultChannel struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Config map[string]any `json:"config,omitempty"`
}

// ProtocolRng 协议版本兼容区间。
type ProtocolRng struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// Execution 进程执行模式（PLUGIN_PROTOCOL §3.1，W3-20 定案）。
//
// 两种形态，与 main/protocol/plugin 的 ModeCall / ModeStdio 对应：
//   - "call"  一次性：一个 invoke 一个进程，写完 result 即退出（渠道插件的默认形态，无常驻）
//   - "stdio" 常连：进程常驻，stdin/stdout 上反复 invoke（拦截器插件用，按需拉起后常驻）
//
// 渠道插件两种形态都实现（core 一律用 call，CLI 自测用 stdio 复现同一条链路）；
// 字段留空视同 "call"（老插件兼容）。
type Execution struct {
	Control            string `json:"control"`                        // call | stdio
	Conversation       string `json:"conversation"`                   // call | stdio
	MaxWorkers         int    `json:"max_workers,omitempty"`          // 调用方并发上限提示（0=不限）
	IdleTimeoutSeconds int    `json:"idle_timeout_seconds,omitempty"` // stdio 常连形态的空闲回收秒数
}

// Capabilities 按角色分组的能力集。
type Capabilities struct {
	Control      []string `json:"control,omitempty"`
	Conversation []string `json:"conversation,omitempty"`
}

// HasControl 报告是否声明某控制面能力。
func (c Capabilities) HasControl(m string) bool { return contains(c.Control, m) }

// HasConversation 报告是否声明某会话面能力。
func (c Capabilities) HasConversation(m string) bool { return contains(c.Conversation, m) }

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ModelInfo 模型信息（models.list，PLUGIN_PROTOCOL §5.2）。允许 null 的字段用指针/零值区分。
type ModelInfo struct {
	ID           string        `json:"id"`
	Name         string        `json:"name,omitempty"`
	Context      *int          `json:"context,omitempty"`
	InputPrice   *float64      `json:"input_price,omitempty"`
	OutputPrice  *float64      `json:"output_price,omitempty"`
	Capabilities Capabilities2 `json:"capabilities,omitempty"`
	// MaxTokens 上游声明的**单次响应输出上限**（tokens）；nil = 未声明，消费方不得编造。
	// 与 Context（输入上下文）不是一个概念：OpenRouter 称 max_completion_tokens，
	// OpenAI 官方 /models 根本不返回，故多数渠道为 nil。下游用于 /v1/models 的
	// max_output_tokens 字段（缺该字段时客户端只按自己的默认值截断）。
	MaxTokens *int `json:"max_tokens,omitempty"`

	// 思考档位（W3-32，迁移旧版 provider.ModelInfo 的 issue #96 能力）。
	//
	// Efforts 是上游目录**显式声明**的可选档位枚举（wire 值原样，如
	// ["none","low","medium","high","xhigh"]）。nil = 上游未声明 → 消费方
	// **不得自行编造**（与 Context 的「未知不声明」哲学一致）。
	// 空切片与 nil 语义相同（上游给了空表等同没声明）。
	//
	// 跨渠道唯一通用值只有 "off"（关思考）；其余枚举由渠道自定，见 CleanEfforts。
	Efforts []string `json:"efforts,omitempty"`
	// DefaultEffort 上游声明的默认档位；必须落在 Efforts 内，否则视为未声明（空）。
	DefaultEffort string `json:"default_effort,omitempty"`
	// CanDisableThinking 该模型思考能否真正关闭。三态：nil = 上游未声明；
	// false = **假关**（关掉照样思考照样计费，gemini-ext 实测 195 token）或不可关；
	// true = 确认可关。消费方据此把 "off" 解释成「请求了但可能无效」。
	CanDisableThinking *bool `json:"can_disable_thinking,omitempty"`
}

// CleanEfforts 规范化上游声明档位表：去空白、去重保序、丢空串；
// 全部无效时返回 nil（「上游没给合法列表就不编档」）。
// 与旧版 dev provider.CleanEfforts 同口径。
func CleanEfforts(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, s := range raw {
		if s = strings.TrimSpace(s); s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Capabilities2 模型能力标记（勿臆造：未探测就不置 true）。
type Capabilities2 struct {
	Tools     bool `json:"tools,omitempty"`
	Vision    bool `json:"vision,omitempty"`
	Reasoning bool `json:"reasoning,omitempty"`
}

// ResourceItem 额度/积分项（旧版 provider.ResourceItem 语义，IPC_CHAT §5 / AGENTS §7.3）。
type ResourceItem struct {
	Name     string   `json:"name"`
	Total    *float64 `json:"total,omitempty"`
	Used     *float64 `json:"used,omitempty"`
	Remain   *float64 `json:"remain,omitempty"`
	Unit     string   `json:"unit,omitempty"` // 空=credits；"token"
	Usable   bool     `json:"usable,omitempty"`
	InfoOnly bool     `json:"info_only,omitempty"`
	Key      string   `json:"key,omitempty"`       // 稳定 ID 供 stat 差分对账
	ExpireAt string   `json:"expire_at,omitempty"` // YYYY-MM-DD 墙钟日 或 RFC3339；空=未下发
}

// AccountInfo 账号视图（accounts.list）。
// Prefixes 是该账号可服务的模型命名空间前缀（workbuddy 系一插件双前缀 workbuddy/workbuddyai），
// core 据此在模型解析时把 `workbuddyai/gpt-5.5` 路由到正确的渠道/账号。
type AccountInfo struct {
	UID       string   `json:"uid"`
	Nickname  string   `json:"nickname,omitempty"`
	Status    string   `json:"status,omitempty"` // ok | cooling | disabled | session_dead
	LoginKind string   `json:"login_kind,omitempty"`
	ExpiresAt string   `json:"expires_at,omitempty"`
	ChannelID string   `json:"channel_id,omitempty"`
	Region    string   `json:"region,omitempty"`
	Credits   *float64 `json:"credits,omitempty"`
	Unit      string   `json:"unit,omitempty"`
	Prefixes  []string `json:"prefixes,omitempty"`
	// AuthFile 是凭据标识（<data>/auths/<auth_file>.json，无后缀）。
	// 它是账号在主程序与插件之间的唯一稳定标示（UID 可能跨区重名，见 PLUGIN_CLI §5.2）。
	AuthFile string `json:"auth_file,omitempty"`
}
