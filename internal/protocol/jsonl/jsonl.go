// Package jsonl 提供版本化 JSONL（一行一个 JSON）消息编码/解码，是 stdio IPC 的基础。
package jsonl

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// RawMessage 是 json.RawMessage 别名，避免各包重复 import encoding/json。
type RawMessage = json.RawMessage

// Msg 是一条基础消息外壳：v=协议版本, id=请求ID, op 见常量。
type Msg struct {
	V      int             `json:"v"`
	ID     string          `json:"id"`
	Op     string          `json:"op"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Event  string          `json:"event,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	OK     *bool           `json:"ok,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ErrObj         `json:"error,omitempty"`
}

// op 常量（与 PLUGIN_PROTOCOL.md / IPC_CHAT.md 对齐）。
const (
	OpInvoke = "invoke"
	OpEvent  = "event"
	OpResult = "result"
	// OpPush 是**调用方→被调方**的流式回灌帧（W3-26 双向流）：只在拦截器类插件的
	// 接口上出现。与 OpEvent（被调方→调用方）方向相反，分开命名是为了让
	// 任何一端都能一眼分辨帧方向，也便于日志/抓包排查。
	OpPush = "push"
)

// ErrObj 统一错误结构（kind/scope 语义见 IPC_CHAT §4）。
type ErrObj struct {
	Kind         string `json:"kind"`
	Scope        string `json:"scope,omitempty"`
	Message      string `json:"message,omitempty"`
	Retryable    *bool  `json:"retryable,omitempty"`
	RetryAfterMs int64  `json:"retry_after_ms,omitempty"`
	Cause        string `json:"cause,omitempty"`
}

// 便捷构造错误。
func Errorf(kind, scope, format string, a ...any) *ErrObj {
	return &ErrObj{Kind: kind, Scope: scope, Message: fmt.Sprintf(format, a...)}
}

// Encoder 顺序写一条消息到 w（JSONL，自动换行）。
type Encoder struct {
	w *bufio.Writer
}

func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: bufio.NewWriter(w)} }

func (e *Encoder) Flush() error { return e.w.Flush() }

func (e *Encoder) Encode(m *Msg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := e.w.Write(b); err != nil {
		return err
	}
	if err := e.w.WriteByte('\n'); err != nil {
		return err
	}
	// 必须立即 flush：IPC 对端是流式消费者（可能永远等不到下一行），
	// 不 flush 会导致事件/结果永久滞留在缓冲里。
	return e.w.Flush()
}

// Decoder 逐行读一条消息。
type Decoder struct {
	r *bufio.Scanner
}

func NewDecoder(r io.Reader) *Decoder {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024*64) // 单条 64MB 上限
	return &Decoder{r: s}
}

func (d *Decoder) Decode(m *Msg) error {
	if !d.r.Scan() {
		if err := d.r.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	line := d.r.Bytes()
	if len(line) == 0 {
		return d.Decode(m) // 跳过空行
	}
	return json.Unmarshal(line, m)
}
