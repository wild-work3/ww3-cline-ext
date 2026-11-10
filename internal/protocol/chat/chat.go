// Package chat 实现归一化对话流式契约（IPC_CHAT.md）的 Go 类型与语义辅助。
package chat

import (
	"encoding/json"
	"strings"
)

// ChatMessage 是一条 OpenAI 风格消息。
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"` // string 或若干 content part
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function *Fn    `json:"function,omitempty"`
}

type Fn struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// Tool 工具定义（OpenAI 形态）。Parameters 是 JSON Schema 原始对象，不做结构约束（向前兼容）。
type Tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// GenerateRequest 是 chat.generate 的请求体。
type GenerateRequest struct {
	Model           string          `json:"model"`
	Messages        []ChatMessage   `json:"messages"`
	Tools           []Tool          `json:"tools,omitempty"`
	ToolChoice      any             `json:"tool_choice,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	MaxTokens       *int            `json:"max_tokens,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Thinking        bool            `json:"thinking,omitempty"`
	ConversationID  string          `json:"conversation_id,omitempty"`
	ChannelConfig   json.RawMessage `json:"channel_config,omitempty"`
	// AccountUID 是 core 选定服务的逻辑账号（多账号渠道必须下发，插件据此取凭据）。
	// 单账号渠道（API Key）可忽略；插件不得自行换号（§9 红线）。
	AccountUID string          `json:"account_uid,omitempty"`
	Extra      json.RawMessage `json:"extra,omitempty"`
}

// Usage 统一用量（IPC_CHAT §5）。空/未上报用指针或整体 nil。
type Usage struct {
	PromptTokens     int         `json:"prompt_tokens"`
	CompletionTokens int         `json:"completion_tokens"`
	TotalTokens      int         `json:"total_tokens"`
	Cache            *CacheUsage `json:"cache,omitempty"`
	TimeMs           int64       `json:"time_ms,omitempty"`
	FirstTokenMs     int64       `json:"first_token_ms,omitempty"`
	// UsageSource 标记 token 数量的来源（W3-34）：reported=上游实报，estimated=core 粗估。
	// 插件上报上游 usage 时必须带 reported，否则 core 无法区分「明确 0」与「未上报」。
	UsageSource
}

// CacheUsage 缓存计量：cached=命中, write=写入, *Reported 标记上游是否真报。
type CacheUsage struct {
	Cached         int  `json:"cached"`
	CachedReported bool `json:"cached_reported,omitempty"`
	Write          int  `json:"write,omitempty"`
	WriteReported  bool `json:"write_reported,omitempty"`
}

// Accumulator 累积 chat.generate 增量事件，安全合并到最终结果。
// 语义来自 IPC_CHAT §3.1（正文 latch）/ §3.2（工具调用按 index 合并）。
type Accumulator struct {
	Text         strings.Builder
	Reasoning    strings.Builder
	FinishReason string
	ToolCalls    []*PartialTool
	HadContent   bool // latch：是否已取到正文
	Err          error
	Done         bool // 收到终结
	Usage        *Usage
}

type PartialTool struct {
	ID, Name, Type   string
	ArgumentsBuilder strings.Builder
}

// Add 处理一条增量事件（ev.Event 是 op=event 里的 Event 名字，确认与 SDK 对齐后用字符串）。
// 为保持解耦，这里处理一串 key-value；调用方把 JSON 解码后的事件字段传入。
func (a *Accumulator) Add(event string, fields map[string]any) {
	switch event {
	case "text.delta":
		if c, ok := fields["content"].(string); ok {
			if c != "" {
				a.HadContent = true
			}
			if a.HadContent {
				a.Text.WriteString(c)
			}
		}
	case "reasoning.delta":
		if c, ok := fields["content"].(string); ok {
			a.Reasoning.WriteString(c)
		}
	case "tool_call.delta":
		a.mergeTool(fields)
	case "finish_reason":
		if r, ok := fields["reason"].(string); ok {
			a.FinishReason = r
		}
	case "usage":
		MergeUsage(&a.Usage, fields)
	case "completed", "incomplete":
		a.Done = true
	case "error":
		a.Done = true
		if msg, ok := fields["message"].(string); ok {
			a.Err = &StreamError{Kind: strOf(fields["kind"]), Message: msg}
		}
	}
}

func strOf(v any) string { s, _ := v.(string); return s }

func (a *Accumulator) mergeTool(fields map[string]any) {
	idx, hasIdx := fields["index"].(float64)
	i := int(idx)
	for len(a.ToolCalls) <= i {
		a.ToolCalls = append(a.ToolCalls, &PartialTool{})
	}
	t := a.ToolCalls[i]
	// 首片带 id/name/type；后续只带 arguments 片段
	if id, ok := fields["id"].(string); ok && id != "" {
		t.ID = id
	}
	if name, ok := fields["name"].(string); ok && name != "" {
		t.Name = name
	}
	if ty, ok := fields["type"].(string); ok && ty != "" {
		t.Type = ty
	}
	if frag, ok := fields["arguments_fragment"].(string); ok {
		if frag != "" || !hasIdx { // 空串也追加以保留 json 分片完整性（除非是首帧建条目）
			t.ArgumentsBuilder.WriteString(frag)
		}
	}
}

// StreamError 携带 kind 的流错误。
type StreamError struct {
	Kind, Message string
}

func (e *StreamError) Error() string { return e.Message }

// Fn 暴露的名字避免 unused。
var _ = strings.Builder{}
