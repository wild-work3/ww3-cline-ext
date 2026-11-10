// Package rpc 提供 stdio/pipe 上的 JSONL RPC 客户端与服务端框架。
// invoke → （可选 event 序列）→ result(ok:true/false)。流式对话用事件回调。
package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"wild-work3/cline-ext/internal/protocol/jsonl"
)

// ErrClosed 表示 RPC 连接已关闭。
var ErrClosed = errors.New("rpc: closed")

// EventHandler 处理增量事件；返回 error 会中止读取并传播到 Invoke 的调用方。
type EventHandler func(ev *jsonl.Msg) error

// Callback 是事件分发的 sentinel：当 Invoke 收到 event 时交由回调，收到 result 时结束。
// Cancel 通知被调方取消当前请求。
//
// OnID 在 invoke 帧**写出之后**被调用，给出本次调用的 id——双向流（W3-26）里调用方
// 需要用它在同一 id 上回灌事件（Client.Emit）。顺序不可反：先回灌会让 push 抢在 invoke
// 之前到达服务端，而服务端按 id 查在途流查不到 → stdio 形态静默丢帧、call 形态直接报错。
type Callback struct {
	OnEvent EventHandler
	OnID    func(id string)
}

// Client 是 JSONL RPC 客户端，向一个 stdin/stdout（或任意双端）进程发起 invoke。
type Client struct {
	encMu    sync.Mutex
	enc      *jsonl.Encoder
	dec      *jsonl.Decoder
	write    io.WriteCloser // 被调方 stdin
	read     io.ReadCloser  // 被调方 stdout
	done     chan struct{}
	closed   atomic.Bool
	nextID   atomic.Int64
	inFlight map[string]chan *resultMsg
	mu       sync.Mutex
}

type resultMsg struct {
	event *jsonl.Msg
	err   error
}

// NewClient 建立 RPC 客户端。in/out 分别是通向被调方进程 stdin/stdout 的管道。
func NewClient(stdin io.WriteCloser, stdout io.ReadCloser) *Client {
	c := &Client{
		enc:      jsonl.NewEncoder(stdin),
		dec:      jsonl.NewDecoder(stdout),
		write:    stdin,
		read:     stdout,
		done:     make(chan struct{}),
		inFlight: make(map[string]chan *resultMsg),
	}
	go c.readLoop()
	return c
}

// NextID 分配一个自增 id。
func (c *Client) NextID() string { return fmt.Sprintf("r%d", c.nextID.Add(1)) }

func (c *Client) readLoop() {
	defer close(c.done)
	for {
		var m jsonl.Msg
		if err := c.dec.Decode(&m); err != nil {
			// 连接结束：失败所有在途
			c.mu.Lock()
			for id, ch := range c.inFlight {
				ch <- &resultMsg{err: err}
				delete(c.inFlight, id)
			}
			c.mu.Unlock()
			return
		}
		// 未关联 id 的通知类帧（如 health 推送）忽略或 dispatch
		if m.ID == "" {
			continue
		}
		c.mu.Lock()
		ch, ok := c.inFlight[m.ID]
		c.mu.Unlock()
		if !ok {
			continue
		}
		switch m.Op {
		case jsonl.OpEvent:
			ch <- &resultMsg{event: &m}
		case jsonl.OpResult:
			ch <- &resultMsg{event: &m} // result 当作终结 event 由调用方解析
		}
	}
}

func (c *Client) send(m *jsonl.Msg) error {
	c.encMu.Lock()
	defer c.encMu.Unlock()
	return c.enc.Encode(m)
}

// Emit 向一个在途的**双向流**调用回灌一条事件（W3-26）。
//
// 方向：调用方 → 被调方。被调方（rpc.Server）按 id 把事件送到对应 Stream.in，
// handler 用 Stream.Next 读取。用于把上游事件实时喂给拦截器插件。
func (c *Client) Emit(id, event string, payload any) error {
	if c.IsClosed() {
		return ErrClosed
	}
	var p jsonl.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		p = b
	}
	return c.send(&jsonl.Msg{V: 1, ID: id, Op: jsonl.OpPush, Event: event, Data: p})
}

// Invoke 发起一次调用，默认收集所有 event 并按需交回调；ctx 取消可中断等待。
// 若 callback 提供 OnEvent，则每个 event 会被调用；否则缓存到返回的 events 列表。
// 返回 ok=true 时填充 result；ok=false 时返回 error。
func (c *Client) Invoke(ctx context.Context, method string, params any, cb *Callback) (result jsonl.Msg, events []*jsonl.Msg, err error) {
	if c.IsClosed() {
		return jsonl.Msg{}, nil, ErrClosed
	}
	id := c.NextID()
	var p jsonl.RawMessage
	if params != nil {
		b, e := json.Marshal(params)
		if e != nil {
			return jsonl.Msg{}, nil, e
		}
		p = b
	}
	req := &jsonl.Msg{V: 1, ID: id, Op: jsonl.OpInvoke, Method: method, Params: p}
	ch := make(chan *resultMsg, 16)
	c.mu.Lock()
	c.inFlight[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.inFlight, id)
		c.mu.Unlock()
	}()
	if err := c.send(req); err != nil {
		return jsonl.Msg{}, nil, err
	}
	// OnID 必须在 send 之后（见 Callback 注释）：调用方一拿到 id 就会开始 push。
	if cb != nil && cb.OnID != nil {
		cb.OnID(id)
	}
	for {
		select {
		case <-ctx.Done():
			return jsonl.Msg{}, events, ctx.Err()
		case rm := <-ch:
			if rm.err != nil {
				return jsonl.Msg{}, events, rm.err
			}
			ev := rm.event
			switch ev.Op {
			case jsonl.OpEvent:
				if cb != nil && cb.OnEvent != nil {
					if err := cb.OnEvent(ev); err != nil {
						return jsonl.Msg{}, events, err
					}
				} else {
					events = append(events, ev)
				}
			case jsonl.OpResult:
				if ev.OK != nil && *ev.OK {
					return *ev, events, nil
				}
				// 失败
				if ev.Error != nil {
					err = &RPCError{ID: id, Kind: ev.Error.Kind, Scope: ev.Error.Scope, Message: ev.Error.Message, RetryAfterMs: ev.Error.RetryAfterMs}
				} else {
					err = fmt.Errorf("rpc: invoke %s failed (no error detail)", method)
				}
				return *ev, events, err
			}
		}
	}
}

// RPCError 携带 kind/scope 供上层判断重试/路由。
type RPCError struct {
	ID, Kind, Scope, Message string
	// RetryAfterMs 上游明示的重试等待毫秒数（排队/限流帧），0=未给。
	// core 拿它当冷却时长（旧版 Queued.Cooldown() 取 retryAfter 的等价物）。
	RetryAfterMs int64
}

func (e *RPCError) Error() string { return fmt.Sprintf("[%s]%s: %s", e.Kind, e.Scope, e.Message) }

// RetryAfterOf 提取错误携带的重试等待时长；非 RPCError 或未给时返回 0。
func RetryAfterOf(err error) time.Duration {
	var re *RPCError
	if errors.As(err, &re) && re.RetryAfterMs > 0 {
		return time.Duration(re.RetryAfterMs) * time.Millisecond
	}
	return 0
}

// Close 关闭连接（向进程发送 shutdown 由调用方决定，这里只管 IO）。
func (c *Client) Close() {
	if c.closed.CompareAndSwap(false, true) {
		c.write.Close()
		c.read.Close()
	}
}

// IsClosed ...
func (c *Client) IsClosed() bool { return c.closed.Load() }

// KindOf 从错误链中提取统一错误分类（kind）。优先 RPCError，其次实现了 Kind() 的错误。
// 供 gateway/core 统一判定 retryable/换号/降级，避免各处重复做类型断言。
func KindOf(err error) string {
	if err == nil {
		return ""
	}
	var re *RPCError
	if errors.As(err, &re) {
		return re.Kind
	}
	var ke interface{ Kind() string }
	if errors.As(err, &ke) {
		return ke.Kind()
	}
	return ""
}

// MessageOf 取错误的原始消息（RPCError 的 Error() 带 "[kind]scope: " 前缀，转发时会造成二次拼接）。
func MessageOf(err error) string {
	if err == nil {
		return ""
	}
	var re *RPCError
	if errors.As(err, &re) {
		return re.Message
	}
	return err.Error()
}
