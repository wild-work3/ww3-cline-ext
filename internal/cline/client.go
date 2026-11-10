package cline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"wild-work3/cline-ext/internal/protocol/errkind"
)

// Client 是 api.cline.bot 的协议客户端。
//
// 与旧版 dev/internal/cline 的差异：
//   - 去掉备用上游通道改道（backuplink.Replay）与地区路由学习：本插件只做单出口直连，
//     地区限制按 ErrPassthrough 原文透传（不换号、不冷却），由 core 决定是否降级。
//   - 去掉模型级 cap 冷却表：本插件的模型面不做任何本地假设（无静态表、无实测表、
//     无模型级冷却），全部交给 core 的模型维度统计。
type Client struct {
	HTTP        *http.Client // 业务/短请求（有总超时）
	StreamHTTP  *http.Client // 流式对话（无总超时，靠 IdleReader）
	IdleTimeout time.Duration

	Base string // 上游基址覆盖（空则 defaultBaseURL）

	mu       sync.Mutex
	knownIDs map[string]string // 最近一次拉到的 free[] ID→name（冷启动为空 ⇒ 校验 fail-open）
}

// NewClient 构造客户端。
//
// 头超时 = TimeoutDur()（默认 300s）：本渠道大 prompt 的首字节可能很慢，
// 硬下限设在 180s 会误杀正常请求，故直接用配置值。
func NewClient(cfg Config) *Client {
	tr := newTransport(cfg)
	return &Client{
		HTTP:        &http.Client{Timeout: cfg.TimeoutDur(), Transport: newTransport(cfg)},
		StreamHTTP:  &http.Client{Transport: tr},
		IdleTimeout: DefaultIdleTimeout,
		Base:        strings.TrimSpace(cfg.BaseURL),
		knownIDs:    map[string]string{},
	}
}

// newTransport 构造出站 transport。
func newTransport(cfg Config) *http.Transport {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{}, // 强制 HTTP/1.1
		TLSHandshakeTimeout:   15 * time.Second,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: cfg.TimeoutDur(),
		ExpectContinueTimeout: time.Second,
	}
	if pu, err := parseProxy(cfg.Proxy); err == nil && pu != nil {
		tr.Proxy = http.ProxyURL(pu)
	}
	return tr
}

// parseProxy 解析代理地址（空 ⇒ nil,nil；非法 ⇒ BadParams）。
func parseProxy(p string) (*url.URL, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return nil, nil
	}
	u, err := url.Parse(p)
	if err != nil || u.Host == "" {
		return nil, newErr(errkind.BadParams, 0, "代理地址非法: "+p)
	}
	return u, nil
}

// base 返回上游基址（去尾斜杠）。
func (c *Client) base() string {
	if s := strings.TrimSpace(c.Base); s != "" {
		return strings.TrimRight(s, "/")
	}
	return defaultBaseURL
}

// BaseURL 返回当前生效的上游基址（供 health/ping 展示）。
func (c *Client) BaseURL() string { return c.base() }

// applyHeaders 统一出站头。
//
// ★ 实测事实 1（见 constants.go 包注释）：X-CLIENT-TYPE 必须带且必须非空，
// 缺这个头，所有 `cline-free/*` 一律 403 API_REQUEST_ERROR_CODE，
// 且症状是「免费模型全部消失」而不报任何配置错误。值本身不参与判定
// （实测 bogus-value 同样放行），故固定写死，勿当优化点删除。
func applyHeaders(req *http.Request, key string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set(HeaderClientType, HeaderClientTypeValue)
	if key != "" {
		req.Header.Set(HeaderAuth, "Bearer "+key)
	}
}

// ---------------------------------------------------------------------------
// 对话
// ---------------------------------------------------------------------------

// ChatStream 发一次流式对话请求。
//
// 返回 (上游流, 状态码, 错误响应体, error)：
//   - 成功（HTTP 200）返回 rc（已套 IdleReader），调用方必须先判 rc==nil 再 defer Close。
//   - 失败返回 rc==nil + status + raw body，由调用方 Classify。
//
// 状态码经 effectiveStatus **还原被网关压平的内层码**（实测外层恒 500），
// 让 core 看到真正的 429/401/404；响应体保持原文透传。
func (c *Client) ChatStream(ctx context.Context, key string, body []byte) (io.ReadCloser, int, []byte, error) {
	m := modelOf(body)
	if m == "" {
		return nil, http.StatusBadRequest, []byte(`{"error":{"message":"model is required"}}`), nil
	}
	// 本地校验：集合 == 暴露集 == free[]；目录未拉到过时放行（fail-open）。
	// 冷启动 knownIDs 为空 ⇒ 不拦，避免误拒上游新模型。
	if !c.knownModel(m) {
		return nil, http.StatusNotFound,
			[]byte(fmt.Sprintf(`{"error":{"message":"model not found in cline free catalog: %s","code":"model_not_found"}}`, jsonEscape(m))), nil
	}

	out := forceStream(clampMaxTokens(body))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+EpChat, bytes.NewReader(out))
	if err != nil {
		return nil, 0, nil, newErr(errkind.Client, 0, err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	applyHeaders(req, key)

	resp, err := c.StreamHTTP.Do(req)
	if err != nil {
		return nil, 0, nil, newErr(kindOfNetErr(err), 0, "连接上游失败: "+err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
		return nil, effectiveStatus(resp.StatusCode, string(raw)), raw, nil
	}
	return NewIdleReader(resp.Body, c.IdleTimeout), resp.StatusCode, nil, nil
}

// forceStream 强制 stream=true。
//
// 不强制会致上游 errEmptyStream 502，且该 502 会被当作账号错误冷却 60s
// （旧版实测教训）；非流式客户端由 core 本地聚合。
func forceStream(body []byte) []byte {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	if v, _ := obj["stream"].(bool); v {
		return body // 已是 true，零改动原样返回
	}
	obj["stream"] = true
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// clampMaxTokens 治理输出上限。
//
// 实测三段：8 → 上游 400；16 → 500 `empty response content`；64~200000 → 200；
// 1e6 → 500。**客户端未给时一律不传** —— 不传最稳（实测可自由跑 3242 token 且正常出内容），
// 强行补一个小值反而制造「空回答」。
func clampMaxTokens(body []byte) []byte {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	if v, ok := obj["max_tokens"]; ok {
		n := toInt(v)
		switch {
		case n <= 0:
			delete(obj, "max_tokens") // 0/负数/非数值：视同未给
		case n < minUpstreamMaxTokens:
			obj["max_tokens"] = minUpstreamMaxTokens
		case n > maxUpstreamMaxTokens:
			obj["max_tokens"] = maxUpstreamMaxTokens
		}
	}
	// 同步治理 OpenAI 新字段名（上游只认 max_tokens；带 max_completion_tokens 时按同规则夹）
	if v, ok := obj["max_completion_tokens"]; ok {
		n := toInt(v)
		if n > 0 {
			if n < minUpstreamMaxTokens {
				n = minUpstreamMaxTokens
			}
			if n > maxUpstreamMaxTokens {
				n = maxUpstreamMaxTokens
			}
			obj["max_completion_tokens"] = n
		} else {
			delete(obj, "max_completion_tokens")
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// modelOf 取 body 里的 model 字段。
func modelOf(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	return strings.TrimSpace(probe.Model)
}

// ---------------------------------------------------------------------------
// 凭据探针（auth.begin / accounts.refresh 用：零对话成本）
// ---------------------------------------------------------------------------

type apiKeyInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix"`
}

type apiKeysResp struct {
	Data struct {
		Keys []apiKeyInfo `json:"keys"`
	} `json:"data"`
	Success bool `json:"success"`
}

// ProbeResult 是 ProbeKey 的产出。
type ProbeResult struct {
	UID         string   // 稳定的 apk- id；取不到时退化为 key 的 sha256 前缀
	KeyPrefix   string   // 本 Key 的官方回显前缀
	SiblingKeys []string // 同一账号下的其它 Key 前缀（「同账号不扩额度」告警用）
	AccountName string   // Key 的 name 字段，仅展示
}

// ProbeKey 校验 API Key 并取回身份。
//
// 只调 GET /api/v1/api-keys —— **零 token 消耗、零副作用**，避免「添加账号即烧额度」。
// uid 取该端点里 key_prefix 与本 Key 相符的那条 id（实测 key_prefix == key 前 10 字符）。
// 同账号判定：该端点返回**本账号全部 Key**，故能列出兄弟 Key 的前缀 ——
// 由于配额键名是 `…UsageCostUSDPerUser`（按账号计），同账号多 Key **不会**扩额度。
func (c *Client) ProbeKey(key string) (*ProbeResult, error) {
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, "sk_") {
		return nil, newErr(errkind.Auth, 0, "Cline API Key 应以 sk_ 开头（在 app.cline.bot → Account / API Keys 创建）")
	}
	req, err := http.NewRequest(http.MethodGet, c.base()+EpAPIKeys, nil)
	if err != nil {
		return nil, newErr(errkind.Client, 0, err.Error())
	}
	applyHeaders(req, key)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, newErr(kindOfNetErr(err), 0, "连接上游失败: "+err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 128<<10))
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, newErr(errkind.SessionDead, resp.StatusCode,
				fmt.Sprintf("Key 无效或被拒（HTTP %d）：%s", resp.StatusCode, strings.TrimSpace(string(raw))))
		}
		return nil, newErr(Classify(resp.StatusCode, string(raw)), resp.StatusCode,
			fmt.Sprintf("校验 Key 失败 HTTP %d：%s", resp.StatusCode, truncate(strings.TrimSpace(string(raw)), 240)))
	}
	var r apiKeysResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, newErr(errkind.Client, resp.StatusCode, "api-keys 响应无法解析："+err.Error())
	}
	prefix := key
	if len(prefix) > KeyPrefixLen {
		prefix = prefix[:KeyPrefixLen]
	}
	res := &ProbeResult{KeyPrefix: prefix}
	for _, k := range r.Data.Keys {
		kp := strings.TrimSpace(k.KeyPrefix)
		if kp == "" {
			continue
		}
		if kp == prefix {
			res.UID = strings.TrimSpace(k.ID)
			res.AccountName = strings.TrimSpace(k.Name)
			continue
		}
		res.SiblingKeys = append(res.SiblingKeys, kp)
	}
	if res.UID == "" {
		// 列表里没有自己（上游形态变化）⇒ 用 hash 兜底，仍保证稳定且不含明文。
		sum := sha256.Sum256([]byte(key))
		res.UID = "k" + hex.EncodeToString(sum[:])[:16]
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// 通用 GET（推荐模型列表等）
// ---------------------------------------------------------------------------

// GetJSON 发 GET 并返回原文（带 Bearer；非 200 返回结构化错误）。
func (c *Client) GetJSON(ctx context.Context, key, path string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+path, nil)
	if err != nil {
		return nil, newErr(errkind.Client, 0, err.Error())
	}
	applyHeaders(req, key)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, newErr(kindOfNetErr(err), 0, "连接上游失败: "+err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		body := string(raw)
		return nil, newErr(Classify(resp.StatusCode, body), effectiveStatus(resp.StatusCode, body),
			truncate(strings.TrimSpace(body), 240))
	}
	return json.RawMessage(raw), nil
}
