package rpc

import (
	"context"
	"encoding/json"
	"io"
	"sync"

	"wild-work3/cline-ext/internal/protocol/jsonl"
)

// 双向流式调用（W3-26）：invoke 发出后，调用方与被调方都可以在同一 id 上继续写
// `op=event` 帧——被调方用 Stream.Event 向下游吐事件，调用方用 Client.Emit 向上游
// 回灌事件。拦截器插件正是这种形态：core 把上游渠道的事件喂进去，插件处理后
// 产出自己的事件流（或直接报错中止整段生成）。
//
// 为什么必须双向而不是「每个事件一次 invoke」：逐事件做一次 IPC 往返，2000 token
// 的回答就是 2000 次调度；更关键的是插件需要**跨事件的连续状态**（滑动窗口、
// 句子缓冲），无状态的逐事件函数做不到。

// StreamHandler 处理一个双向流式 invoke。
type StreamHandler func(ctx context.Context, params json.RawMessage, s *Stream) (jsonl.RawMessage, *jsonl.ErrObj)

// Stream 是被调方侧的双向流句柄。
type Stream struct {
	id   string
	mu   *sync.Mutex     // 与 Server 共用：事件与结果串行写同一个编码器
	enc  *jsonl.Encoder  //
	in   chan *jsonl.Msg // 调用方回灌的事件帧
	done chan struct{}   // 已结束（handler 返回后关闭）
	once sync.Once       //
}

// ID 返回本次 invoke 的 id。
func (s *Stream) ID() string { return s.id }

// Event 向下游（调用方）推送一条事件。
func (s *Stream) Event(name string, payload any) error {
	var p jsonl.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		p = b
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enc.Encode(&jsonl.Msg{V: 1, ID: s.id, Op: jsonl.OpEvent, Event: name, Data: p})
}

// Next 读取下一条上游事件；上游发完（Stream 关闭）返回 io.EOF，ctx 取消返回 ctx.Err()。
func (s *Stream) Next(ctx context.Context) (*jsonl.Msg, error) {
	select {
	case m := <-s.in:
		return m, nil
	case <-s.done:
		// done 与 in 同时就绪时优先取残留帧，避免尾部事件丢失。
		select {
		case m := <-s.in:
			return m, nil
		default:
			return nil, io.EOF
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// close 由 Server 在 handler 返回后调用。
func (s *Stream) close() {
	s.once.Do(func() { close(s.done) })
}

// deliver 把调用方回灌的事件放进 in 队列；已在途上限内阻塞，用于天然背压。
func (s *Stream) deliver(m *jsonl.Msg) {
	select {
	case s.in <- m:
	case <-s.done:
	}
}

// NewStream 在插件侧手工构造一个双向流句柄（供 plugin.ServeOnce 的 duplex 路径使用：
// 那里自己读 stdin，而不是走 Server.Serve 循环）。
func NewStream(id string, enc *jsonl.Encoder, mu *sync.Mutex) *Stream {
	return &Stream{id: id, mu: mu, enc: enc, in: make(chan *jsonl.Msg, 64), done: make(chan struct{})}
}

// Deliver 把一个调用方回灌的事件帧放进队列（已结束时丢弃）。
func (s *Stream) Deliver(m *jsonl.Msg) { s.deliver(m) }

// Close 标记流结束：Next 在队列取空后返回 io.EOF。
func (s *Stream) Close() { s.close() }
