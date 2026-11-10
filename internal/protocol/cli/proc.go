package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"wild-work3/cline-ext/internal/protocol/jsonl"
	"wild-work3/cline-ext/internal/protocol/rpc"
)

// Proc 是一个被我们自驱动的子进程（插件 daemon/worker、core、gateway）。
// CLI 联调模式统一走这里：父进程扮演 core/gateway 的角色，子进程是被调方。
type Proc struct {
	Cmd    *exec.Cmd
	Client *rpc.Client
	Log    *Logger
	Name   string
	start  time.Time
	// Out 非空时，流式正文（text.delta）会同步写到这个 writer（命令行模式设为 os.Stdout）。
	Out io.Writer
}

// Spawn 启动子进程并把 stdin/stdout 接成 JSONL RPC 管道；stderr 直通本进程 stderr。
func Spawn(name, exe string, args []string, log *Logger) (*Proc, error) {
	cmd := exec.Command(exe, args...)
	cmd.Stderr = os.Stderr
	if env := os.Environ(); len(env) > 0 {
		cmd.Env = env
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("打开 %s stdin 失败: %w", name, err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("打开 %s stdout 失败: %w", name, err)
	}
	p := &Proc{Cmd: cmd, Log: log, Name: name, start: time.Now()}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 %s 失败: %w", name, err)
	}
	p.Client = rpc.NewClient(in, out)
	log.Infof("已启动 %s: exe=%s args=%v pid=%d", name, exe, args, cmd.Process.Pid)
	return p, nil
}

// Wait 等待子进程退出（含 stderr 输出完毕）。
func (p *Proc) Wait() error {
	err := p.Cmd.Wait()
	if err != nil {
		p.Log.Errorf("%s 退出: %v（运行 %s）", p.Name, err, time.Since(p.start).Round(time.Millisecond))
	} else {
		p.Log.Infof("%s 正常退出（运行 %s）", p.Name, time.Since(p.start).Round(time.Millisecond))
	}
	return err
}

// Kill 强杀子进程。
func (p *Proc) Kill() {
	if p.Cmd.Process != nil {
		_ = p.Cmd.Process.Kill()
	}
}

// Close 关闭 RPC 管道。
func (p *Proc) Close() {
	if p.Client != nil {
		p.Client.Close()
	}
}

// Step 是一次 RPC 调用（可带事件流）。
type Step struct {
	Method string
	Params any
	// Stream 为 true 时逐事件打印（对话类）；false 时只打印 result。
	Stream bool
}

// Result 汇总一次 Step 的结果。
type Result struct {
	Method string
	OK     bool
	Result map[string]any
	Err    *jsonl.ErrObj
	Events []map[string]any
	Text   string // 拼接后的正文（text.delta）
	Usage  map[string]any
	Finish string
	Took   time.Duration
	// Raw 是 result 的原始 JSON（调用方自己解析时用）
	Raw string
}

// UsageInt 读 usage 事件里的整数字段（JSON 数字解出来是 float64）。
func (r *Result) UsageInt(key string) int64 {
	if r.Usage == nil {
		return 0
	}
	if f, ok := r.Usage[key].(float64); ok {
		return int64(f)
	}
	return 0
}

// Invoke 执行一个 Step，日志记录完整请求/事件/响应。
func (p *Proc) Invoke(ctx context.Context, st Step) (*Result, error) {
	start := time.Now()
	// 参数落日志（零 token 落盘：日志前先脱敏，AGENTS §9）
	if raw, err := json.Marshal(st.Params); err == nil {
		p.Log.Infof("→ %s.%s params=%s", p.Name, st.Method, RedactJSON(string(raw)))
	}
	res := &Result{Method: st.Method}
	var text, reasoning strings.Builder
	cb := &rpc.Callback{OnEvent: func(m *jsonl.Msg) error {
		var fields map[string]any
		if len(m.Data) > 0 {
			_ = json.Unmarshal(m.Data, &fields)
		}
		if fields == nil {
			fields = map[string]any{}
		}
		fields["_event"] = m.Event
		res.Events = append(res.Events, fields)
		switch m.Event {
		case "text.delta":
			s, _ := fields["content"].(string)
			text.WriteString(s)
			if !st.Stream {
				p.Log.Infof("  ← text.delta %q", s)
			} else if p.Out != nil {
				p.Log.RawFile(s)                // 落盘留证（不写 stderr，避免终端两份）
				_, _ = io.WriteString(p.Out, s) // 命令行模式把正文实时打到 stdout
			} else {
				p.Log.Raw(s)
			}
		case "reasoning.delta":
			s, _ := fields["content"].(string)
			reasoning.WriteString(s)
			if !st.Stream {
				p.Log.Infof("  ← reasoning.delta %q", truncate(s, 120))
			}
		case "tool_call.delta":
			b, _ := json.Marshal(fields)
			p.Log.Infof("  ← tool_call.delta %s", string(b))
		case "finish_reason":
			res.Finish, _ = fields["reason"].(string)
			p.Log.Infof("  ← finish_reason %v", fields["reason"])
		case "usage":
			res.Usage = fields
			b, _ := json.Marshal(fields)
			p.Log.Infof("  ← usage %s", string(b))
		case "error":
			// error 事件体是扁平结构 {kind,scope,message,retryable}，不能只取 "error" 字段
			delete(fields, "_event")
			b, _ := json.Marshal(fields)
			p.Log.Errorf("  ← error %s", string(b))
		default:
			b, _ := json.Marshal(fields)
			p.Log.Infof("  ← %s %s", m.Event, truncate(string(b), 400))
		}
		return nil
	}}
	var evResult jsonl.Msg
	evResult, _, err := p.Client.Invoke(ctx, st.Method, st.Params, cb)
	res.Took = time.Since(start)
	res.Text, _ = text.String(), reasoning.String()
	if strings.HasSuffix(res.Text, "\n") == false && st.Stream {
		p.Log.Raw("\n")
	}
	if err != nil {
		res.OK = false
		var re *rpc.RPCError
		if e, ok := err.(*rpc.RPCError); ok {
			re = e
		}
		if re != nil {
			res.Err = &jsonl.ErrObj{Kind: re.Kind, Scope: re.Scope, Message: re.Message}
			p.Log.Errorf("← %s.%s 失败 kind=%s scope=%s msg=%s（%s）", p.Name, st.Method, re.Kind, re.Scope, re.Message, res.Took.Round(time.Millisecond))
		} else {
			p.Log.Errorf("← %s.%s 失败 %v（%s）", p.Name, st.Method, err, res.Took.Round(time.Millisecond))
		}
		return res, err
	}
	res.OK = true
	if len(evResult.Result) > 0 {
		res.Raw = string(evResult.Result)
		_ = json.Unmarshal(evResult.Result, &res.Result)
		p.Log.Infof("← %s.%s result=%s（%s，%d 事件）", p.Name, st.Method, RedactJSON(compact(string(evResult.Result))), res.Took.Round(time.Millisecond), len(res.Events))
	} else {
		p.Log.Infof("← %s.%s ok（%s，%d 事件）", p.Name, st.Method, res.Took.Round(time.Millisecond), len(res.Events))
	}
	return res, nil
}

// InvokeSimple 发起一次不关心事件流的调用（控制面方法）。
func (p *Proc) InvokeSimple(ctx context.Context, method string, params any) (*Result, error) {
	return p.Invoke(ctx, Step{Method: method, Params: params})
}

// Handshake 执行 initialize → capabilities.list → health.check（PLUGIN_PROTOCOL 生命周期）。
// 注意：会话面 worker 可能只注册 chat.generate/ping，initialize 缺失仅警告不中断。
func (p *Proc) Handshake(ctx context.Context) error {
	for _, m := range []string{"initialize", "capabilities.list", "health.check"} {
		if _, err := p.InvokeSimple(ctx, m, nil); err != nil {
			// 会话面 worker 只注册 chat.generate/ping：not_found 视为可选方法缺失，不中断。
			if rpc.KindOf(err) == "not_found" {
				p.Log.Infof("%s: 未注册 %s（跳过）", p.Name, m)
				continue
			}
			if m == "initialize" {
				return fmt.Errorf("initialize 失败: %w", err)
			}
			p.Log.Infof("%s: %s 不可用（跳过）", p.Name, m)
		}
	}
	return nil
}

// compact 压掉 JSON 里的空白，便于单行日志阅读。
func compact(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return s
	}
	return string(b)
}

// RedactJSON 日志脱敏：凡键名含 key/token/secret/password/authorization 的值一律遮掉（零 token 落盘，AGENTS §9）。
// 输入可为任意 JSON（对象/数组/标量），解析失败则退回字符串脱敏。
func RedactJSON(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return maskInline(s)
	}
	return compact2(redact(v))
}

func redact(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if isSecretKey(k) {
				out[k] = maskValue(val)
				continue
			}
			out[k] = redact(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = redact(e)
		}
		return out
	default:
		return v
	}
}

func maskValue(v any) any {
	s, ok := v.(string)
	if !ok {
		return "***"
	}
	if len(s) <= 4 {
		return "***"
	}
	return "***" + s[len(s)-4:]
}

func isSecretKey(k string) bool {
	l := strings.ToLower(k)
	return strings.Contains(l, "key") || strings.Contains(l, "token") || strings.Contains(l, "secret") ||
		strings.Contains(l, "password") || strings.Contains(l, "authorization") || strings.Contains(l, "cookie")
}

// maskInline 对非 JSON 文本做保守处理：把 Bearer xxx / bce-v3/... 这类长串遮掉。
func maskInline(s string) string {
	fields := strings.Fields(s)
	for i, f := range fields {
		if len(f) > 24 && (strings.Contains(f, "/") || strings.HasPrefix(f, "sk-") || strings.HasPrefix(f, "ALTAK")) {
			fields[i] = "***" + f[len(f)-4:]
		}
	}
	return strings.Join(fields, " ")
}

func compact2(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// truncate 截断长字符串（日志用）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
