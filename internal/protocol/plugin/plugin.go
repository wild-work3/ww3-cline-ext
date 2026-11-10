// Package plugin 提供插件进程的固定运行形态（W3-20 定案）：
//
//	call   一次性调用：从 stdin 读一条 invoke，执行，写事件 + 一条 result，进程退出。
//	stdio  常连：stdin/stdout 上跑 JSONL RPC 循环直到 stdin EOF。
//
// 两种形态共用同一份 Registry（方法表），因此**不存在“CLI 专用捷径”**：
// 命令行自测通过即等于协议通过（AGENTS §S）。
//
// 谁用哪种形态（W3-20 定案）：
//   - 渠道插件：两者都实现，但 core 一律用 call（一次请求一个进程，结束即回收，无常驻）。
//   - 拦截器插件：每次对话都要用，由 core 按需拉起后**常驻**，走 stdio 反复 invoke。
//
// 为什么不常驻渠道插件：渠道的控制面调用是低频、离散的（查模型、刷 token、看余额），
// 常驻进程要额外维护空闲回收/健康检查/版本切换后的一致性，收益远不抵复杂度；
// 而进程启动成本本身应由**调用方的并发上限 + 间隔**消化（不能假定目标机器能 65ms 起一个进程）。
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"

	"wild-work3/cline-ext/internal/protocol/jsonl"
	"wild-work3/cline-ext/internal/protocol/meta"
	"wild-work3/cline-ext/internal/protocol/rpc"
)

// 运行形态常量（main 里分发子命令用）。
const (
	ModeCall  = "call"  // 一次性：一 invoke 一进程
	ModeStdio = "stdio" // 常连：JSONL RPC 循环
)

// Emit 推送增量事件。call 形态下事件同样会写出（保持两种形态行为一致）。
type Emit func(event string, payload any) error

// Handler 处理一个控制面方法（不产生增量事件）。
type Handler func(ctx context.Context, params jsonl.RawMessage) (jsonl.RawMessage, *jsonl.ErrObj)

// StreamHandler 处理一个会话面流式方法（可推送事件）。
type StreamHandler func(ctx context.Context, params jsonl.RawMessage, emit Emit) (jsonl.RawMessage, *jsonl.ErrObj)

// DuplexHandler 处理一个**双向流**方法（W3-26）：既能向调用方 emit 事件，
// 也能从 s.Next() 读调用方回灌的事件。拦截器插件用它把上游事件流接管过来。
type DuplexHandler func(ctx context.Context, params jsonl.RawMessage, s *rpc.Stream) (jsonl.RawMessage, *jsonl.ErrObj)

// Registry 是一个插件的全部方法表。
type Registry struct {
	plain  map[string]Handler
	stream map[string]StreamHandler
	duplex map[string]DuplexHandler
}

// New 创建空方法表。
func New() *Registry {
	return &Registry{plain: map[string]Handler{}, stream: map[string]StreamHandler{}, duplex: map[string]DuplexHandler{}}
}

// Handle 注册控制面方法。
func (r *Registry) Handle(name string, f Handler) { r.plain[name] = f }

// HandleStream 注册会话面（流式）方法。
func (r *Registry) HandleStream(name string, f StreamHandler) { r.stream[name] = f }

// HandleDuplex 注册双向流方法。
func (r *Registry) HandleDuplex(name string, f DuplexHandler) { r.duplex[name] = f }

// Methods 返回已注册方法名（排序，便于自检输出）。
func (r *Registry) Methods() []string {
	out := make([]string, 0, len(r.plain)+len(r.stream)+len(r.duplex))
	for k := range r.plain {
		out = append(out, k)
	}
	for k := range r.stream {
		out = append(out, k)
	}
	for k := range r.duplex {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Lifecycle 注册标准生命周期与元数据方法（initialize / capabilities.list / ping /
// health.check / shutdown）。所有插件都该注册：CLI 自检与 gateway 的插件商店都会用到。
func (r *Registry) Lifecycle(m meta.Manifest) {
	r.Handle("initialize", func(ctx context.Context, _ jsonl.RawMessage) (jsonl.RawMessage, *jsonl.ErrObj) {
		return mustJSON(map[string]any{
			"protocol":     1,
			"plugin_id":    m.ID,
			"manifest":     m,
			"capabilities": m.Capabilities,
		}), nil
	})
	r.Handle("capabilities.list", func(ctx context.Context, _ jsonl.RawMessage) (jsonl.RawMessage, *jsonl.ErrObj) {
		return mustJSON(map[string]any{"control": m.Capabilities.Control, "conversation": m.Capabilities.Conversation}), nil
	})
	r.Handle("ping", func(ctx context.Context, _ jsonl.RawMessage) (jsonl.RawMessage, *jsonl.ErrObj) {
		return mustJSON("pong"), nil
	})
	r.Handle("health.check", func(ctx context.Context, _ jsonl.RawMessage) (jsonl.RawMessage, *jsonl.ErrObj) {
		return mustJSON(map[string]any{"state": "ok"}), nil
	})
	r.Handle("shutdown", func(ctx context.Context, _ jsonl.RawMessage) (jsonl.RawMessage, *jsonl.ErrObj) {
		return mustJSON(map[string]any{"ok": true}), nil
	})
}

// dispatch 执行一个方法；未知方法沿用 not_found（core 靠它判断“可选能力未实现”）。
func (r *Registry) dispatch(ctx context.Context, method string, params jsonl.RawMessage, emit Emit) (jsonl.RawMessage, *jsonl.ErrObj) {
	if f, ok := r.plain[method]; ok {
		return f(ctx, params)
	}
	if f, ok := r.stream[method]; ok {
		return f(ctx, params, emit)
	}
	return nil, jsonl.Errorf("not_found", "request", "unknown method %s", method)
}

// ServeStdio 跑常连形态（stdio 命令）：直到 stdin EOF。
//
// 每个 invoke 在独立 goroutine 中执行（chat.generate 会阻塞到整段流结束），
// 在途上限由 rpc.Server 的 sem 做背压；EOF 后等在途完成，不截断已开始的流。
func (r *Registry) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	s := rpc.NewServer(in, out)
	for name, f := range r.plain {
		f := f
		s.Handle(name, func(ctx context.Context, p jsonl.RawMessage, _ *rpc.EventWriter) (jsonl.RawMessage, *jsonl.ErrObj) {
			return runGuarded(func() (jsonl.RawMessage, *jsonl.ErrObj) { return f(ctx, p) })
		})
	}
	for name, f := range r.stream {
		f := f
		s.Handle(name, func(ctx context.Context, p jsonl.RawMessage, ev *rpc.EventWriter) (jsonl.RawMessage, *jsonl.ErrObj) {
			emit := Emit(func(event string, payload any) error { return ev.Event(event, payload) })
			return runGuarded(func() (jsonl.RawMessage, *jsonl.ErrObj) { return f(ctx, p, emit) })
		})
	}
	for name, f := range r.duplex {
		f := f
		s.HandleStream(name, func(ctx context.Context, p jsonl.RawMessage, st *rpc.Stream) (jsonl.RawMessage, *jsonl.ErrObj) {
			return runGuarded(func() (jsonl.RawMessage, *jsonl.ErrObj) { return f(ctx, p, st) })
		})
	}
	return s.Serve(ctx)
}

// ServeOnce 跑一次性形态（call 命令）：读一条 invoke → 执行 → 写事件 + 一条 result。
//
// 事件也写出去，是为了让同一方法在两种形态下行为一致——若 call 形态吞掉事件，
// 流式方法就会“在 stdio 下能跑、在 call 下静默丢内容”，那才是最难查的 bug。
func (r *Registry) ServeOnce(ctx context.Context, in io.Reader, out io.Writer) error {
	dec := jsonl.NewDecoder(in)
	enc := jsonl.NewEncoder(out)
	var m jsonl.Msg
	if err := dec.Decode(&m); err != nil {
		return fmt.Errorf("读取 invoke 帧失败: %w", err)
	}
	if m.Op != jsonl.OpInvoke {
		return fmt.Errorf("一次性模式期望 op=invoke，收到 %q", m.Op)
	}
	var mu sync.Mutex
	emit := Emit(func(event string, payload any) error {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		return enc.Encode(&jsonl.Msg{V: 1, ID: m.ID, Op: jsonl.OpEvent, Event: event, Data: b})
	})
	// 双向流方法（W3-26）：同时启动一个读循环，把调用方回灌的事件送进 Stream。
	// 必须在处理前就绪：调用方可能在 handler 还没跑起来时就开始喂上游事件。
	var res jsonl.RawMessage
	var e *jsonl.ErrObj
	if _, isDuplex := r.duplex[m.Method]; isDuplex {
		st := rpc.NewStream(m.ID, enc, &mu)
		go func() {
			for {
				var in jsonl.Msg
				if err := dec.Decode(&in); err != nil {
					st.Close()
					return
				}
				if in.Op == jsonl.OpPush || in.Op == jsonl.OpEvent {
					st.Deliver(&in)
				}
			}
		}()
		defer st.Close()
		f := r.duplex[m.Method]
		res, e = runGuarded(func() (jsonl.RawMessage, *jsonl.ErrObj) { return f(ctx, m.Params, st) })
	} else {
		res, e = runGuarded(func() (jsonl.RawMessage, *jsonl.ErrObj) { return r.dispatch(ctx, m.Method, m.Params, emit) })
	}
	ok := e == nil
	msg := &jsonl.Msg{V: 1, ID: m.ID, Op: jsonl.OpResult, OK: &ok}
	if ok {
		msg.Result = res
	} else {
		msg.Error = e
	}
	mu.Lock()
	err := enc.Encode(msg)
	mu.Unlock()
	if err != nil {
		return err
	}
	return enc.Flush() // 必须立即 flush：调用方在等这一帧，缓冲区里留着就是死锁
}

// runGuarded 把 panic 转成 server 类错误（不让进程带着未发出的 result 直接崩）。
func runGuarded(f func() (jsonl.RawMessage, *jsonl.ErrObj)) (res jsonl.RawMessage, e *jsonl.ErrObj) {
	defer func() {
		if rec := recover(); rec != nil {
			res, e = nil, jsonl.Errorf("server", "request", "panic: %v", rec)
		}
	}()
	return f()
}

// Spec 描述如何启动一个插件进程（一次性调用用）。
type Spec struct {
	Exe    string    // 可执行文件绝对路径
	Args   []string  // 调用方负责带上 "call" 子命令
	Env    []string  // 追加到 os.Environ()（core 会传 WW3_DATA）
	Stderr io.Writer // nil = 继承本进程 stderr（插件日志直通）
	// KillGrace 是收到 result 后等待进程自行退出的宽限期，超时强杀。0 用默认 5s。
	KillGrace time.Duration
}

// RunOnce 一次性调用：启动插件进程 → 写一帧 invoke → 收事件/结果 → 等进程退出。
//
// 这是 core 调渠道插件的唯一路径（W3-20）：一次请求一个进程，结束即回收，无常驻。
func RunOnce(ctx context.Context, spec Spec, method string, params any, cb *rpc.Callback) (jsonl.Msg, error) {
	call, err := Start(ctx, spec, method, params, cb)
	if err != nil {
		return jsonl.Msg{}, err
	}
	return call.Wait()
}

// Call 是一个已启动的一次性插件调用（可双向：调用方可以 Send 回灌事件）。
//
// 拦截器插件需要双向：core 把上游事件用 Send 喂进去，插件用事件流回吐结果。
// 与 RunOnce 共用同一套进程回收语义（写完 result 不退则超 KillGrace 强杀）。
type Call struct {
	spec    Spec
	cmd     *exec.Cmd
	cli     *rpc.Client
	id      string
	idReady chan struct{}
	done    chan struct{} // Wait 完成后关闭（重复 Wait 返回同一结果）
	res     jsonl.Msg
	err     error
}

// Start 启动插件进程并异步发起 invoke（不等结果）。
// cb.OnID 在 invoke 帧**写出之后**被调用（给出本次调用 id）；双向流可用 call.ID() 取。
//
// 这个顺序是契约：调用方拿到 id 就会开始回灌 push 帧，若 OnID 先于 send 触发，push 可能抢在
// invoke 之前到达服务端，而服务端按 id 查在途流（rpc/server.go 的 s.live）查不到：
// stdio 形态**静默丢帧**，call 形态直接报“一次性模式期望 op=invoke，收到 push”。
// （实测：8 路并发下事件数 294/287/296/298/299，期望 300。）
func Start(ctx context.Context, spec Spec, method string, params any, cb *rpc.Callback) (*Call, error) {
	if spec.Exe == "" {
		return nil, fmt.Errorf("插件可执行文件路径为空")
	}
	cmd := exec.Command(spec.Exe, spec.Args...)
	cmd.Env = append(os.Environ(), spec.Env...)
	if spec.Stderr != nil {
		cmd.Stderr = spec.Stderr
	} else {
		cmd.Stderr = os.Stderr
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("打开插件 stdin 失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("打开插件 stdout 失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动插件失败: %w", err)
	}
	c := &Call{spec: spec, cmd: cmd, idReady: make(chan struct{}), done: make(chan struct{})}
	cli := rpc.NewClient(stdin, stdout)
	c.cli = cli
	// 保留调用方原有回调，额外挂一个 OnID 用于拿到 id（双向流回灌要用）。
	cb2 := &rpc.Callback{}
	if cb != nil {
		cb2.OnEvent = cb.OnEvent
	}
	cb2.OnID = func(id string) {
		c.id = id
		close(c.idReady)
		if cb != nil && cb.OnID != nil {
			cb.OnID(id)
		}
	}
	go func() {
		defer close(c.done)
		res, _, invokeErr := cli.Invoke(ctx, method, params, cb2)
		cli.Close()
		c.waitProc()
		c.res = res
		if invokeErr != nil {
			if ctx.Err() != nil {
				c.err = ctx.Err()
			} else {
				// 子进程可能直接崩了（没写 result），此时 rpc 层报的是 IO/EOF 错误，补上可读上下文
				c.err = fmt.Errorf("插件调用 %s 失败: %w", method, invokeErr)
			}
			return
		}
	}()
	return c, nil
}

// ID 返回本次 invoke 的 id（可用于 Send 回灌事件）。
func (c *Call) ID() string {
	<-c.idReady
	return c.id
}

// Send 向在途调用回灌一条事件（调用方 → 插件）。
func (c *Call) Send(event string, payload any) error {
	<-c.idReady
	return c.cli.Emit(c.id, event, payload)
}

// Wait 等结果并回收进程。
func (c *Call) Wait() (jsonl.Msg, error) {
	<-c.done
	return c.res, c.err
}

// Close 主动中止（取消下游），并回收进程。
func (c *Call) Close() {
	c.cli.Close()
}

// waitProc 等进程退出；写完 result 却赖着不退则超 KillGrace 强杀（W3-18 验收口径）。
func (c *Call) waitProc() {
	grace := c.spec.KillGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(grace):
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		<-done
	}
}

// mustJSON 序列化失败即 panic（runGuarded 会转成 server 错误）。
func mustJSON(v any) jsonl.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
