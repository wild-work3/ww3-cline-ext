package channel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"wild-work3/cline-ext/internal/cline"
	"wild-work3/cline-ext/internal/protocol/chat"
	"wild-work3/cline-ext/internal/protocol/errkind"
	"wild-work3/cline-ext/internal/protocol/plugin"
	"wild-work3/cline-ext/internal/protocol/stat"
)

// generate 实现 chat.generate 流式对话。
//
// 顺序（AGENTS 陷阱 21）：先判 rc==nil 再 defer rc.Close()，否则空指针 panic。
func (d *Daemon) generate(ctx context.Context, raw Raw, emit plugin.Emit) (Raw, *Err) {
	var req chat.GenerateRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, errf(errkind.BadParams, errkind.ScopeRequest, "请求体解析失败: %v", err)
	}
	cfg := mergeConfig(channelConfig(raw), req)

	a, rerr := d.pickAuth(cfg, req.AccountUID)
	if rerr != nil {
		return nil, errf(rerr.Kind, scopeOfKind(rerr.Kind), "%s", rerr.Msg)
	}
	client := d.client(cfg)

	// core 下发的是 `cline/cline-free/xxx`；上游要 `cline-free/xxx`（只剥首段）。
	// 首段就是本渠道名时 UpstreamModel 直接剥前缀并放行剩余部分（上游 free[] 里
	// 存在 `nvidia/…`、`poolside/…`、`reflection/…` 这类自带命名空间的裸 ID）。
	upModel, merr := cline.UpstreamModel(req.Model)
	if merr != nil {
		e := cline.AsError(merr, errkind.ModelBlocked)
		return nil, errf(e.Kind, scopeOfKind(e.Kind), "%s", e.Msg)
	}

	body, err := buildUpstreamBody(&req, upModel)
	if err != nil {
		return nil, errf(errkind.BadParams, errkind.ScopeRequest, "构造上游请求失败: %v", err)
	}

	start := time.Now()
	rc, status, respBody, err := client.ChatStream(ctx, a.Key(), body)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return nil, errf(errkind.Cancel, errkind.ScopeRequest, "请求已取消")
		}
		e := cline.AsError(err, errkind.Server)
		d.recordChat(cfg, a, req.Model, false, e.Kind, nil, 0, time.Since(start))
		return nil, errf(e.Kind, scopeOfKind(e.Kind), "上游请求失败: %s", e.Msg)
	}
	if rc == nil {
		kind := cline.Classify(status, string(respBody))
		e := cline.AsError(errFromStatus(status, respBody), kind)
		d.recordChat(cfg, a, req.Model, false, kind, nil, 0, time.Since(start))
		return nil, errf(kind, scopeOfKind(kind), "上游返回 %d: %s", status, e.Msg)
	}
	defer rc.Close()

	// 首字耗时：第一条增量事件的时间（reasoning 也算首字，否则思考型模型会显示 0）。
	var firstMs int64
	wrapped := func(ev string, payload any) error {
		if firstMs == 0 && (ev == "text.delta" || ev == "reasoning.delta" || ev == "tool_call.delta") {
			firstMs = time.Since(start).Milliseconds()
		}
		return emit(ev, payload)
	}

	usage, finish, perr := cline.ParseStream(rc, wrapped, req.Model)
	if perr != nil {
		if errors.Is(perr, context.Canceled) || ctx.Err() != nil {
			return nil, errf(errkind.Cancel, errkind.ScopeRequest, "请求已取消")
		}
		e := cline.AsError(perr, errkind.Server)
		d.recordChat(cfg, a, req.Model, false, e.Kind, usage, firstMs, time.Since(start))
		return nil, errf(e.Kind, scopeOfKind(e.Kind), "流式读取失败: %s", e.Msg)
	}

	if finish == "" {
		finish = "stop"
	}
	_ = emit("finish_reason", map[string]any{"reason": finish})
	if usage == nil {
		usage = &chat.Usage{}
	}
	usage.FirstTokenMs = firstMs
	usage.TimeMs = time.Since(start).Milliseconds()
	_ = emit("usage", usage)
	_ = emit("completed", map[string]any{})

	d.recordChat(cfg, a, req.Model, true, "", usage, firstMs, time.Since(start))
	return mustJSON(usage), nil
}

// buildUpstreamBody 构造上游 JSON 请求体：透传 OpenAI 字段，强制删除推理档位，
// 并按需注入 stream_options 以获得 usage。
func buildUpstreamBody(req *chat.GenerateRequest, upModel string) ([]byte, error) {
	m := map[string]any{}
	if len(req.Extra) > 0 {
		if err := json.Unmarshal(req.Extra, &m); err != nil {
			return nil, err
		}
	}
	delete(m, "channel_config")
	delete(m, "account_uid")
	delete(m, "conversation_id")
	// 上游无推理档位参数（cline 走的是模型自带思考），下发任何档位都会被拒或削弱，故删除。
	delete(m, "reasoning_effort")
	delete(m, "thinking")
	m["model"] = upModel
	m["messages"] = req.Messages
	m["stream"] = true
	if len(req.Tools) > 0 {
		m["tools"] = req.Tools
	}
	if req.ToolChoice != nil {
		m["tool_choice"] = req.ToolChoice
	}
	if req.Temperature != nil {
		m["temperature"] = *req.Temperature
	}
	// max_tokens 交给 cline.clampMaxTokens 治理（未给就不传，不传最稳）。
	if req.MaxTokens != nil {
		m["max_tokens"] = *req.MaxTokens
	}
	if _, ok := m["stream_options"]; !ok {
		m["stream_options"] = map[string]any{"include_usage": true}
	}
	return json.Marshal(m)
}

// errFromStatus 把上游非 200 响应转为可分类错误。
func errFromStatus(status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	var env struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
		Error   struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil {
		switch {
		case env.Error.Message != "":
			msg = env.Error.Message
		case env.Message != "":
			msg = env.Message
		case env.Msg != "":
			msg = env.Msg
		}
	}
	return &cline.Error{Kind: cline.Classify(status, msg), Status: status, Msg: msg}
}

// recordChat 记录一次对话统计（fail-open）。
func (d *Daemon) recordChat(cfg cline.Config, a *cline.Auth, model string, ok bool, kind errkind.Kind, usage *chat.Usage, firstMs int64, dtime time.Duration) {
	if d.stat == nil {
		return
	}
	rec := statRecord{
		Kind:         stat.KindChat,
		OK:           ok,
		Model:        model,
		Time:         dtime,
		FirstTokenMs: int(firstMs),
	}
	if a != nil {
		rec.Account = a.UID
		rec.Region = a.RegionOf()
	}
	if kind != "" {
		rec.ErrKind = kind
	}
	if usage != nil {
		rec.PromptTokens = usage.PromptTokens
		rec.CompletionTokens = usage.CompletionTokens
		rec.TotalTokens = usage.TotalTokens
		rec.PromptReported = usage.PromptReported
		rec.CompletionReported = usage.CompletionReported
		rec.TotalReported = usage.TotalReported
		if usage.Cache != nil {
			rec.CachedTokens = usage.Cache.Cached
			rec.CachedReported = usage.Cache.CachedReported
		}
	}
	d.appendStat(rec, cfg)
}
