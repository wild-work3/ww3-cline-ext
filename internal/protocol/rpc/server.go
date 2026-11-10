package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"wild-work3/cline-ext/internal/protocol/jsonl"
)

// MethodFunc 处理一个 invoke。ctx 用于取消；params 是原始 JSON。
// 返回 result payload（ok=true）或 error（将转为 ok=false result err 结构）。
// 流式调用通过回调 evWriter 发送 event。
type MethodFunc func(ctx context.Context, params json.RawMessage, ev *EventWriter) (jsonl.RawMessage, *jsonl.ErrObj)

// DefaultMaxConcurrent 单进程内同时在途 invoke 的默认上限。
// W3-03：插件一请求一 worker，会话面同进程内并发很少；限制主要防失控（背压）。
const DefaultMaxConcurrent = 64

// EventWriter 允许方法在返回前推送增量事件。
// mu 与 Server 共用：并发 invoke 共用同一个编码器，事件与结果必须串行写。
type EventWriter struct {
	mu  *sync.Mutex
	enc *jsonl.Encoder
	id  string
}

func (w *EventWriter) Event(name string, payload any) error {
	var p jsonl.RawMessage
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	p = b
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enc.Encode(&jsonl.Msg{V: 1, ID: w.id, Op: jsonl.OpEvent, Event: name, Data: p})
}

// Server 处理 stdio 上的 invoke 循环。
//
// 每个 invoke 在独立 goroutine 中执行：调用方常把「长事务」放在一个 invoke 里
// （如 chat.generate 阻塞到整段流结束），同步执行会把同进程内其它 invoke 全部排队，
// 表现为上层并发请求串行化。事件/结果的 id 已足以关联，故可并发。
type Server struct {
	enc     *jsonl.Encoder
	dec     *jsonl.Decoder
	out     io.Writer
	mu      sync.Mutex // 保护 enc（事件与结果共用同一写入端）
	meth    map[string]MethodFunc
	streams map[string]StreamHandler // 双向流方法（W3-26）
	live    map[string]*Stream       // 在途双向流：id → Stream（接收调用方回灌的事件）
	sem     chan struct{}            // 在途上限，天然背压
	wg      sync.WaitGroup
}

// NewServer 建立服务端。
func NewServer(r io.Reader, w io.Writer) *Server {
	return &Server{
		enc:     jsonl.NewEncoder(w),
		dec:     jsonl.NewDecoder(r),
		out:     w,
		meth:    make(map[string]MethodFunc),
		streams: make(map[string]StreamHandler),
		live:    make(map[string]*Stream),
		sem:     make(chan struct{}, DefaultMaxConcurrent),
	}
}

// SetMaxConcurrent 调整在途上限（<=0 视为不限制，慎用）。须在 Serve 前调用。
func (s *Server) SetMaxConcurrent(n int) {
	if n <= 0 {
		s.sem = make(chan struct{}, 1<<20)
		return
	}
	s.sem = make(chan struct{}, n)
}

// Handle 注册方法。
func (s *Server) Handle(method string, f MethodFunc) { s.meth[method] = f }

// HandleStream 注册双向流方法（W3-26）：handler 拿到 *Stream，既能下发事件，
// 也能 Next 读取调用方回灌的事件。
func (s *Server) HandleStream(method string, f StreamHandler) { s.streams[method] = f }

// Event 主动推送通知（无 id 关联）。
func (s *Server) Event(name string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enc.Encode(&jsonl.Msg{V: 1, Op: jsonl.OpEvent, Event: name, Data: b})
}

// Serve 运行循环直到输入 EOF；EOF 时等待在途 invoke 结束（优雅退出，不截断已开始的流）。
func (s *Server) Serve(ctx context.Context) error {
	defer s.wg.Wait()
	for {
		var m jsonl.Msg
		if err := s.dec.Decode(&m); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if m.Op == jsonl.OpPush || m.Op == jsonl.OpEvent {
			// 调用方回灌给在途双向流的事件（W3-26）：新协议用 push，event 为向后兼容。
			s.mu.Lock()
			st := s.live[m.ID]
			s.mu.Unlock()
			if st != nil {
				msg := m
				st.deliver(&msg)
			}
			continue
		}
		if m.Op != jsonl.OpInvoke {
			continue // 只处理 invoke
		}
		// 每个 invoke 独立 goroutine：长事务（chat.generate）不再阻塞同进程其它调用。
		// 参数按值拷出：m 在下轮循环会被复用。
		msg := m
		s.wg.Add(1)
		if _, isStream := s.streams[msg.Method]; isStream {
			// 双向流：**必须在循环里同步注册** live[id]，否则调用方紧接着回灌的事件
			// 会在 handler goroutine 注册前到达，被当成未知 id 丢掉（时序 race）。
			st := &Stream{id: msg.ID, mu: &s.mu, enc: s.enc, in: make(chan *jsonl.Msg, 64), done: make(chan struct{})}
			s.mu.Lock()
			s.live[msg.ID] = st
			s.mu.Unlock()
			go s.dispatchStream(ctx, &msg, st)
			continue
		}
		go s.dispatch(ctx, &msg)
	}
}

// dispatch 执行一个 invoke 并回 result；并发上限由 sem 控制（背压）。
func (s *Server) dispatch(ctx context.Context, m *jsonl.Msg) {
	defer s.wg.Done()
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return
	}
	var result jsonl.RawMessage
	var errObj *jsonl.ErrObj
	f, ok := s.meth[m.Method]
	ev := &EventWriter{mu: &s.mu, enc: s.enc, id: m.ID}
	if !ok {
		errObj = jsonl.Errorf("not_found", "request", "unknown method %s", m.Method)
	} else {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					errObj = jsonl.Errorf("server", "request", "panic: %v", rec)
				}
			}()
			result, errObj = f(ctx, m.Params, ev)
		}()
	}
	s.writeResult(m.ID, result, errObj)
}

// dispatchStream 执行一个双向流方法（Stream 已由 Serve 循环注册）。
func (s *Server) dispatchStream(ctx context.Context, m *jsonl.Msg, st *Stream) {
	defer s.wg.Done()
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		st.close()
		s.mu.Lock()
		delete(s.live, m.ID)
		s.mu.Unlock()
		return
	}
	f := s.streams[m.Method]
	var result jsonl.RawMessage
	var errObj *jsonl.ErrObj
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				errObj = jsonl.Errorf("server", "request", "panic: %v", rec)
			}
		}()
		result, errObj = f(ctx, m.Params, st)
	}()
	st.close()
	s.mu.Lock()
	delete(s.live, m.ID)
	s.mu.Unlock()
	s.writeResult(m.ID, result, errObj)
}

// writeResult 回写一帧 result（ok=true 带 result，否则带 error）。
func (s *Server) writeResult(id string, result jsonl.RawMessage, errObj *jsonl.ErrObj) {
	// 发送 result
	okV := errObj == nil
	resp := &jsonl.Msg{V: 1, ID: id, Op: jsonl.OpResult, OK: &okV}
	if errObj == nil {
		resp.Result = result
	} else {
		resp.Error = errObj
	}
	s.mu.Lock()
	err := s.enc.Encode(resp)
	s.mu.Unlock()
	if err != nil {
		return
	}
}

// ServeWithMethods 便捷：注册 map 后 Serve。
func Serve(ctx context.Context, r io.Reader, w io.Writer, methods map[string]MethodFunc) error {
	s := NewServer(r, w)
	for k, v := range methods {
		s.Handle(k, v)
	}
	fmt.Fprintf(w, "") // noop
	return s.Serve(ctx)
}
