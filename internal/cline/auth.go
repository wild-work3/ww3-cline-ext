package cline

import (
	"encoding/json"
	"strings"
	"time"
)

// Config 是 channel_config 里与本渠道相关的配置项。
type Config struct {
	Region  string `json:"region,omitempty"`
	Proxy   string `json:"proxy,omitempty"`
	Timeout int    `json:"timeout,omitempty"`  // 秒；0 表示默认
	BaseURL string `json:"base_url,omitempty"` // 覆盖上游基址（联调/自建反代）
	Auth    string `json:"auth,omitempty"`     // 指定凭据名（多账号时用）
	// APIKey 允许把 Key 直接写在渠道配置里（面板「直接填密钥」路径）。
	// 落盘凭据仍是首选：配置里的 Key 不会进 auths/，因此无法被多账号轮换使用。
	APIKey string `json:"api_key,omitempty"`
}

// ParseConfig 尽力解析渠道配置；空/非法时返回零值（不报错，交由调用方决定）。
func ParseConfig(raw json.RawMessage) Config {
	var c Config
	if len(raw) == 0 {
		return c
	}
	_ = json.Unmarshal(raw, &c)
	return c
}

// BaseURLOr 返回上游基址（去尾斜杠；空则 APIBase）。
func (c Config) BaseURLOr() string {
	if s := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/"); s != "" {
		return s
	}
	return defaultBaseURL
}

// TimeoutDur 返回短请求超时；<=0 用 DefaultTimeout。
func (c Config) TimeoutDur() time.Duration {
	if c.Timeout > 0 {
		return time.Duration(c.Timeout) * time.Second
	}
	return DefaultTimeout
}

// Auth 是落盘的凭据体（内部私有，绝不外泄 Key）。
//
// 与 raccoon/workbuddy 的凭据结构保持同形（`accessToken` + `expiresAt`），
// 这样旧版 wild-work 的 auths/ 与本插件可互换（PLUGIN_CLI §5.1）。
// 本渠道的 accessToken 就是 Cline 的 API Key（`sk_` + 64 hex）。
type Auth struct {
	AccessToken string `json:"accessToken"`
	// RefreshToken 恒空：官方 API Key **不轮换**，故本渠道没有刷新/保活/签到。
	RefreshToken string `json:"refreshToken,omitempty"`
	// ExpiresAt 恒为远期值（见 FarFutureExpiry）：Key 无过期概念，写 0 会让
	// 「已过期」恒真而触发不存在的刷新逻辑。
	ExpiresAt int64  `json:"expiresAt,omitempty"`
	UID       string `json:"uid,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	Region    string `json:"region,omitempty"`
	// KeyPrefix 是官方 api-keys 回显的 key_prefix（`sk_` + 7 位 hex），仅供展示与
	// 「同账号多 Key 不扩额度」告警比对；不是凭据本身。
	KeyPrefix string `json:"keyPrefix,omitempty"`
	// SiblingKeyPrefixes 是同账号下其它 Key 的前缀（配额按账号计 ⇒ 多 Key 不扩额度）。
	SiblingKeyPrefixes []string `json:"siblingKeyPrefixes,omitempty"`
	AuthFile           string   `json:"-"`
}

// Key 返回去空白的 API Key。
func (a *Auth) Key() string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.AccessToken)
}

// ExpiresAtText 返回 RFC3339 文本（空表示未知）。
func (a *Auth) ExpiresAtText() string {
	if a == nil || a.ExpiresAt <= 0 {
		return ""
	}
	return time.Unix(a.ExpiresAt, 0).Format(time.RFC3339)
}

// RegionOf 返回凭据所属区域（本渠道单区域，固定为渠道名）。
func (a *Auth) RegionOf() string {
	if a == nil || strings.TrimSpace(a.Region) == "" {
		return AuthPrefix
	}
	return a.Region
}

// Expired 判断凭据是否已过期（预留 60s 余量）。
//
// 本渠道 ExpiresAt 是远期值，故正常情况下恒 false；写 0 视为「未知 ⇒ 不过期」，
// 因为 Key 本身没有过期语义。
func (a *Auth) Expired() bool {
	if a == nil || a.ExpiresAt <= 0 {
		return false
	}
	return time.Now().Unix() >= a.ExpiresAt-60
}
