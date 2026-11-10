package cline

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"wild-work3/cline-ext/internal/protocol/meta"
)

// ---------------------------------------------------------------------------
// 动态目录（唯一来源）
// ---------------------------------------------------------------------------

// remoteModel 是 recommended-models 数组元素的形状。
type remoteModel struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}

// recommendResp 上游 recommended-models 信封：模型面只消费 free 数组（其余分组不落结构）。
type recommendResp struct {
	Free []remoteModel `json:"free"`
}

// StaticModels 恒 nil：本渠道**不做任何本地假设**（无静态表、无映射、无过滤）。
// 模型面完全由上游 recommended-models 的 free 数组决定，原样透传。
// 兜底语义：缓存优先于静态表，空表不会让渠道分组消失。
func StaticModels() []meta.ModelInfo { return nil }

// FetchModels 返回暴露集 = recommended-models 的 free 数组**原样透传**：
//  1. 不碰 /models（458 条 OpenRouter 镜像，对「免费池」定位无信息量）；
//  2. ID 不改名不改前缀（free[] 本身就是 `cline-free/*` 别名空间）；
//  3. 不按 tag/后缀过滤、不做基名去重 —— 上游给的什么就暴露什么；
//  4. 仅做两条机械兜底：精确 ID 去重、name 缺失时 prettifyID 兜底；
//  5. free[] 条目顺带记入 knownIDs（本地校验集 = 暴露集，构造即一致）。
//
// 端点无需鉴权（无 Key 也能拉），故 key 可为空。
func (c *Client) FetchModels(ctx context.Context, key string) ([]meta.ModelInfo, error) {
	var rec recommendResp
	raw, err := c.GetJSON(ctx, key, EpRecommends)
	if err == nil {
		err = json.Unmarshal(raw, &rec)
	}
	if err != nil {
		// 兜底：free[] 拉不到但本进程尚有缓存 ⇒ 返回缓存（上游目录瞬时抖动不掏空面板）。
		if cached := c.cachedModels(); len(cached) > 0 {
			return cached, nil
		}
		return nil, fmt.Errorf("cline: 免费模型目录拉取失败: %w", err)
	}

	seen := map[string]bool{}
	out := make([]meta.ModelInfo, 0, len(rec.Free))
	names := make(map[string]string, len(rec.Free))
	for _, m := range rec.Free {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := strings.TrimSpace(m.Name)
		if name == "" || name == id {
			name = prettifyID(id)
		}
		names[id] = name
		out = append(out, meta.ModelInfo{ID: id, Name: name})
	}
	c.rememberLiveModels(names)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// RouteOf 按上游响应 id 的形态判断该请求实际走了哪条链。
//
// 实测判据（2026-10-10，9 个 ID 零反例）：
//
//	gen-<unix秒>-<20位Base62> → OpenRouter（Cline 转发给聚合器，响应无 provider_metadata）
//	gen_<ULID 26 位>          → Cline 自有网关（带 marketCost/routing/fallbacksAvailable）
//
// 为什么需要它：配额键名是 `…UsageCostUSDPerUser`（按 Cline 账号计），
// 而 `limit_rpd` 日限落在 **Cline 自己的 OpenRouter key** 上 ⇒ 换 Cline 账号必然复现。
// 二者必须区分处置（见 Classify 的 ErrPassthrough vs ErrSoftRate），而成功响应里
// **没有任何字段直接写着聚合器名字**（is_byok / provider 实测都不等价，不可用作判据）。
// 结论：只有走 Cline 网关的模型，多账号轮换才有意义。
func RouteOf(responseID string) string {
	switch {
	case strings.HasPrefix(responseID, routeClineGW):
		return "cline_gateway"
	case strings.HasPrefix(responseID, routeOpenRouter):
		return "openrouter"
	default:
		return "unknown"
	}
}

// prettifyID 把 `cline-free/step-5-preview` 这类裸 ID 变成可读展示名。
func prettifyID(id string) string {
	s := strings.TrimPrefix(strings.TrimSpace(id), freePrefix)
	s = strings.ReplaceAll(s, "-", " ")
	if r := []rune(s); len(r) > 0 {
		return strings.ToUpper(string(r[0])) + string(r[1:])
	}
	return s
}

// ---------------------------------------------------------------------------
// 校验集
// ---------------------------------------------------------------------------

// knownModel 本地校验。**冷启动放行**：目录尚未成功拉取过（knownIDs 为空）时不拦截 ——
// 若拦，进程重启后第一批请求会全部被本地 404（等于「渠道周期性瘫痪」），
// 而放行的最坏结果只是让上游给出权威错误。目录拉取成功后校验集 == 暴露集 == free[]。
func (c *Client) knownModel(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.knownIDs) == 0 {
		return true
	}
	_, ok := c.knownIDs[id]
	return ok
}

// rememberLiveModels 用本次 free[] 结果**替换**已知集（name 跟随上游目录同步）。
func (c *Client) rememberLiveModels(names map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.knownIDs = names
}

// cachedModels 返回已知集快照（按 ID 排序）。
func (c *Client) cachedModels() []meta.ModelInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.knownIDs) == 0 {
		return nil
	}
	out := make([]meta.ModelInfo, 0, len(c.knownIDs))
	for id, name := range c.knownIDs {
		out = append(out, meta.ModelInfo{ID: id, Name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
