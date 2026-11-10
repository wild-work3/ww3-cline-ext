package cline

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"wild-work3/cline-ext/internal/protocol/chat"
	"wild-work3/cline-ext/internal/protocol/errkind"
)

// Emitter 是流式事件出口（与 plugin.Emit 同形）。
type Emitter func(event string, payload any) error

// ParseStream 解析上游 OpenAI SSE，逐帧 emit 增量事件，并把**流内 error 帧**转成 error。
//
// 上游就是标准 OpenAI SSE（无签名/加密），这里负责：
//   - delta.reasoning_content / delta.reasoning → reasoning.delta
//   - delta.content → text.delta
//   - delta.tool_calls → tool_call.delta（index 用 float64，匹配 chat.Accumulator）
//   - 末帧 usage（含 prompt_tokens_details.cached_tokens）与 finish_reason 由调用方收尾
//
// ★ 实测事实 3（见 constants.go 包注释）：失败可能以
// **HTTP 200 + 一帧 error + 上游自己发 [DONE]** 呈现
// （`stream_initialization_failed` / `{"code":"502"}`）。因此 error 帧必须在这里
// 立刻转成 error 返回调用方，否则它会被伪装成「模型回了个空回答」，账号侧拿不到任何
// 惩罚信号、粘性路由也不会清（旧版 R40 的同款教训）。
func ParseStream(r io.Reader, emit Emitter, model string) (*chat.Usage, string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)

	var usage *chat.Usage
	var finish string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}
		var ch sseChunk
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			// 非 JSON 帧（心跳/噪声）跳过。
			continue
		}
		if ch.Error != nil && ch.Error.Message != "" {
			status := ch.Error.StatusCode()
			msg := ch.Error.Message
			// 实测事实 3：HTTP 200 + 流内 error 帧（stream_initialization_failed）必须转成
			// error 返回调用方，否则会被伪装成「模型回了个空回答」（R40）。
			// 口径（对齐旧版 StreamError.Kind）：**一律 Passthrough** —— 流内失败几乎都源于
			// 上游模型/池状态（含「共享出口上限」），与账号健康无关；若归成 SoftRate 等
			// Rotatable 类型，core 会换号重打，而换号必然复现（配额按账号计但日限落在
			// Cline 自己的 OpenRouter key 上），等于白白烧掉整池账号。
			return nil, "", newErr(errkind.Passthrough, status, "上游流内错误: "+msg)
		}
		if len(ch.Usage) > 0 {
			var raw map[string]any
			if json.Unmarshal(ch.Usage, &raw) == nil && len(raw) > 0 {
				usage = usageOf(raw)
			}
		}
		for _, c := range ch.Choices {
			if text := reasoningTextOf(c.Delta.ReasoningContent, c.Delta.Reasoning, c.Delta.ReasoningDetails); text != "" {
				if err := emit("reasoning.delta", map[string]any{"content": text}); err != nil {
					return nil, "", err
				}
			}
			if c.Delta.Content != "" {
				if err := emit("text.delta", map[string]any{"content": c.Delta.Content}); err != nil {
					return nil, "", err
				}
			}
			for _, tc := range c.Delta.ToolCalls {
				fields := map[string]any{"index": float64(tc.Index)}
				if tc.ID != "" {
					fields["id"] = tc.ID
				}
				if tc.Type != "" {
					fields["type"] = tc.Type
				}
				if tc.Function.Name != "" {
					fields["name"] = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					fields["arguments_fragment"] = tc.Function.Arguments
				}
				if err := emit("tool_call.delta", fields); err != nil {
					return nil, "", err
				}
			}
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, ErrIdleTimeout) {
			return nil, "", newErr(errkind.IdleTimeout, 0, "上游流空闲超时（无数据）")
		}
		return nil, "", newErr(errkind.Truncated, 0, "流读取中断: "+err.Error())
	}
	return usage, finish, nil
}

type sseChunk struct {
	ID      string      `json:"id"`
	Error   *sseError   `json:"error"`
	Choices []sseChoice `json:"choices"`
	// Usage 保留原始对象（不解析成结构体）：缓存计量藏在 prompt_tokens_details /
	// cache_read_input_tokens / cache_creation_input_tokens 等**渠道族各异**的键里，
	// 用结构体接收未知键会被静默丢弃（面板缓存利用率恒为「—」）。
	Usage json.RawMessage `json:"usage"`
}

// sseError 是流内 error 帧的错误体。
//
// code 既可能是字符串（实测 `stream_initialization_failed` / `"502"`），
// 也可能是数字，故用 any 承接，再在 StatusCode 里两种都试。
type sseError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
	Status  int    `json:"status"`
}

// StatusCode 尽力从错误体推断 HTTP 状态（推断不出返回 0，交给 Classify 走文案判据）。
func (e *sseError) StatusCode() int {
	if e == nil {
		return 0
	}
	if e.Status > 0 {
		return e.Status
	}
	switch v := e.Code.(type) {
	case float64:
		if v > 0 {
			return int(v)
		}
	case string:
		s := strings.TrimSpace(v)
		n := 0
		for _, r := range s {
			if r < '0' || r > '9' {
				n = 0
				break
			}
			n = n*10 + int(r-'0')
		}
		if n >= 100 && n <= 599 {
			return n
		}
	}
	// 兜底：message 里可能压着内层状态码（上游把 429 压平成 500 的同款形态）。
	return InnerStatus(e.Message)
}

type sseChoice struct {
	Delta struct {
		Content          string          `json:"content"`
		ReasoningContent string          `json:"reasoning_content"`
		Reasoning        string          `json:"reasoning"`
		ReasoningDetails []reasoningItem `json:"reasoning_details"` // OpenRouter 式结构化思考块
		ToolCalls        []sseTool       `json:"tool_calls"`
	} `json:"delta"`
	FinishReason string `json:"finish_reason"`
}

// reasoningItem 是 reasoning_details 数组条目：type=reasoning.text 带全文，type=reasoning.summary 带摘要。
type reasoningItem struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Summary string `json:"summary"`
}

// reasoningTextOf：reasoning_content > reasoning > reasoning_details（text 全文优先于 summary 摘要，旧版 D-口径）。
func reasoningTextOf(rc, r string, details []reasoningItem) string {
	if rc != "" {
		return rc
	}
	if r != "" {
		return r
	}
	var full, summary strings.Builder
	for _, it := range details {
		switch it.Type {
		case "reasoning.text":
			full.WriteString(it.Text)
		case "reasoning.summary":
			summary.WriteString(it.Summary)
		}
	}
	if full.Len() > 0 {
		return full.String()
	}
	return summary.String()
}

type sseTool struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}


// toUsage 转成协议用量；缓存命中只在 prompt_tokens_details 真下发时置 Reported。

// usageOf 把上游原始 usage 对象转成协议用量（W3-34 统一口径）。
//
// 上游是 OpenAI 兼容形态，但**缓存字段渠道族各异**（prompt_tokens_details.cached_tokens /
// cache_read_input_tokens / cache_creation_input_tokens…），取数统一走 chat.CacheFromMap，
// 与 main/protocol 的优先级逐条一致。三维 token 带 reported 标记：值来自上游同一对象，
// 明确 0 也是有效实报（区分「上游说 0」与「上游没说」）。
func usageOf(raw map[string]any) *chat.Usage {
	u := &chat.Usage{
		PromptTokens:     usageInt(raw["prompt_tokens"]),
		CompletionTokens: usageInt(raw["completion_tokens"]),
		TotalTokens:      usageInt(raw["total_tokens"]),
	}
	// 上游没给 total 时本地相加——那是推导值，不能标成实报（W3-34）。
	derived := u.TotalTokens == 0 && (u.PromptTokens > 0 || u.CompletionTokens > 0)
	if derived {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	u.Cache = chat.CacheFromMap(raw)
	u.MarkReported()
	if derived {
		u.TotalReported = false
	}
	return u
}

// usageInt 取数值字段（JSON 数字统一是 float64）；缺失/非数值按 0。
func usageInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n)
		}
	}
	return 0
}
