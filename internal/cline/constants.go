// Package cline 是 Cline（api.cline.bot）渠道的**免费模型池**上游实现。
//
// 凭据：官方文档化的 **API Key**（`sk_` + 64 hex），在 https://app.cline.bot/ →
// 注册账号 → Account / API Keys → Create API Key 创建（创建时只显示一次）。**不走** WorkOS device-code OAuth ——
// 后者需要弹浏览器、refresh token 会轮换、同凭据多处使用会互踢（对照 R26/R44），
// 而 Key 不轮换 ⇒ 本渠道没有 RefreshToken，也没有任何保活/签到需求。
//
// 渠道定位（决议 R46）：多账号 Key 池 + 免费额度调度器 + 反代。
// 模型面**不做任何本地假设**：绑死上游 recommended-models 的 free 数组原样透传
// （ID 不改写、不过滤、无静态表、无本地配置）。
//
// 四个必须知道的实测事实（2026-10-10，逐项对照实验，详见 dev 仓 docs/cline渠道接入计划.md）：
//  1. **`X-CLIENT-TYPE` 头必须非空**，否则所有 `cline-free/*` 别名一律 403
//     `API_REQUEST_ERROR_CODE`，而**值内容无关**（`bogus-value` 也放行）⇒ 是个「存在性」闸门。
//  2. **上游把真实状态压平成 HTTP 500**：限流是 `500 {…from Openrouter: request failed
//     with status 429…}`。故 Classify 必须从 body 里正则取**内层**状态码，否则 429 会被
//     归成 5xx（违反不变量 15 的「429 优先」）。
//  3. **失败可能以「HTTP 200 + 流内 error 帧 + 上游自己发 [DONE]」呈现**
//     （`stream_initialization_failed`）⇒ 必须转成 error 返回调用方，否则被伪装成空回答（R40）。
//  4. **路由可从 `response.id` 前缀判别**：`gen-<ts>-<b62>` = OpenRouter；
//     `gen_<ULID>` = Cline 自有网关（带 provider_metadata/marketCost）。配额键名
//     `…UsageCostUSDPerUser` 表明上限按**账号**计 ⇒ 只有走 Cline 网关的模型，
//     多账号轮换才有意义；走 OpenRouter 的撞的是 Cline 共享 key 的 limit_rpd 日限，换号必复现。
package cline

import (
	"fmt"
	"strings"
	"time"

	"wild-work3/cline-ext/internal/protocol/errkind"
)

// ChannelName 是渠道名（＝插件 ID＝模型命名空间前缀）。
const ChannelName = "cline"

// 上游端点。公开目录 /models 与 recommended-models 都**无需鉴权**，
// 但对话与 api-keys 需要 Bearer sk_。
const (
	APIBase        = "https://api.cline.bot/api/v1"
	EpChat         = "/chat/completions"
	EpRecommends   = "/ai/cline/recommended-models" // 返回 recommended/free/clinePass/clineCloud 四组；free[] 是模型面唯一来源
	EpAPIKeys      = "/api-keys"                    // 返回本账号全部 Key（uid 与同账号判定的来源）
	defaultBaseURL = APIBase
)

// 请求头常量。HeaderClientTypeValue 取何值都放行，**但不得为空**；
// 一旦这个头被删，症状是「免费模型全部 403 且面板无异常」——同 R34 的静默失败家族。
const (
	HeaderClientType      = "X-CLIENT-TYPE"
	HeaderClientTypeValue = "cline-sdk"
	HeaderAuth            = "Authorization"
)

// max_tokens 三段实测：8 → 上游 400（低于提供方最小值）；16 → 500 `empty response content`
// （过了校验但推理吃光预算，可见内容为 0）；64~200000 → 全部 200；1e6 → 500。
// 客户端不传时**一律不传**（实测不传最稳，可自由跑到 3242 token 并正常出内容）。
const (
	minUpstreamMaxTokens = 64
	maxUpstreamMaxTokens = 200000
)

// KeyPrefixLen 是官方 api-keys 回显的 key_prefix 长度（`sk_` + 7 位 hex）。
// 用它把「我手上的 Key」匹配回列表中的那一条，从而拿到稳定的 apk- id 作 uid；
// 面板的「同账号多 Key 不扩额度」告警也用它比对（旧版 app.SubmitClineKey）。
const KeyPrefixLen = 10

// DefaultIdleTimeout 无 config 注入时的流式空闲上限。
// ⚠️ 生产恒取本值：配置项不存在（见 Client.IdleTimeout 注释）。
const DefaultIdleTimeout = 90 * time.Second

// DefaultTimeout 是短请求（api-keys / 模型目录）的总超时。
const DefaultTimeout = 300 * time.Second

// FarFutureExpiry Key 无过期概念。ExpiresAt 必须给远期值而**不得为 0**：
// 0 会让「是否过期」恒真 ⇒ 每次请求都尝试刷新（旧版不变量 21 的同款陷阱）。
func FarFutureExpiry() int64 { return time.Now().AddDate(80, 0, 0).Unix() }

// freePrefix 是 Cline 自有免费命名空间；passPrefix/cloudPrefix 属订阅/云池，本渠道**不使用**
// （cline-pass 需要订阅，实测加头也无效：ENTITLEMENT_ERROR）。
const (
	freePrefix  = "cline-free/"
	passPrefix  = "cline-pass/"
	cloudPrefix = "cline-cloud/"
)

// routePrefix 判据：response.id 前缀 ⇒ 该请求实际走的是哪条链。
const (
	routeOpenRouter = "gen-" // 经 OpenRouter 转发（无 provider_metadata）
	routeClineGW    = "gen_" // Cline 自有网关（有 marketCost/routing/fallbacks）
)

// AuthPrefix 是凭据文件名的渠道前缀（<data>/auths/cline-<uid>.json）。
const AuthPrefix = ChannelName

// DefaultRegion 本渠道单区域（固定为渠道名）。
const DefaultRegion = ChannelName

// ModelPrefixOf 取模型名的首段（core 下发的完整模型名形如 `cline/cline-free/xxx`）。
func ModelPrefixOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "/"); i > 0 {
		return s[:i]
	}
	return ""
}

// TrimModel 去掉渠道前缀（core 下发的完整模型名形如 `cline/cline-free/xxx`）。
//
// ⚠️ 与 raccoon 的 TrimModel（取最后一个 "/" 之后）**不同**：本渠道的上游模型 ID
// 自身就含 "/"（`cline-free/mimo-v2.6-flash`），取最后一段会把 `mimo-v2.6-flash`
// 之外的命名空间剥掉、造成模型名错配。故这里**只剥首段**，且仅当首段就是本渠道名。
func TrimModel(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "/"); i > 0 && s[:i] == ChannelName {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

// PrefixModel 给模型 ID 加渠道前缀（models.list 展示用）。
func PrefixModel(id string) string {
	if id == "" {
		return id
	}
	if strings.HasPrefix(id, ChannelName+"/") {
		return id
	}
	return ChannelName + "/" + id
}

// upstreamNamespaces 是上游自己的模型命名空间：它们**不是**渠道前缀，
// 出现时说明 core 下发的是未加渠道前缀的裸上游 ID，应原样放行。
var upstreamNamespaces = map[string]bool{
	strings.TrimSuffix(freePrefix, "/"):  true,
	strings.TrimSuffix(passPrefix, "/"):  true,
	strings.TrimSuffix(cloudPrefix, "/"): true,
}

// UpstreamModel 把 core 下发的完整模型名转成上游模型名，并校验命名空间归属。
//
// 规则（W3-24）：首段 == 本渠道名 ⇒ 剥掉；首段是上游自己的命名空间
// （cline-free/cline-pass/cline-cloud）⇒ 原样透传；其它首段 ⇒ 说明这条模型属于
// 别的渠道（模型路由配错），返回 ModelBlocked，绝不静默拿本渠道凭据去跑别人的模型。
func UpstreamModel(full string) (string, error) {
	full = strings.TrimSpace(full)
	if full == "" {
		return "", &Error{Kind: errkind.ModelBlocked, Msg: "模型名为空"}
	}
	pfx := ModelPrefixOf(full)
	switch {
	case pfx == "":
		return full, nil
	case pfx == ChannelName:
		return TrimModel(full), nil
	case upstreamNamespaces[pfx]:
		return full, nil
	default:
		return "", &Error{Kind: errkind.ModelBlocked,
			Msg: fmt.Sprintf("模型 %s 属于 %s 命名空间，不属于本渠道（%s），请检查模型路由", full, pfx, ChannelName)}
	}
}
