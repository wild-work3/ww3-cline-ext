// Package cli 的 flags.go 提供渠道插件统一的命令行外壳（W3-21 定案）。
//
// 形态：
//
//	custom-api-ext.exe models --base-url https://... --api-key sk-...
//	custom-api-ext.exe call --data D:/plugins/custom-api
//
// 两条硬约定（见 protocol/docs/PLUGIN_CLI.md）：
//  1. 第一个位置参数是**裸命令名**（不带减号），不是 `-mode xxx`；
//  2. 开关一律写双短横线 `--data`，不提供单短横线简写——这些参数只由 core 与
//     脚本传，不面向人类手打。
//
// 为什么自己包一层而不是直接用 flag：需要在命令之间隔离开关（`--flow` 只属于
// `status` 命令），并且要让“传了本命令不认的开关”直接报错，而不是被静默忽略。
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// ErrHelp 表示用户请求帮助（调用方应直接 exit 0，不要当失败）。
var ErrHelp = errors.New("help")

// Command 是一个子命令。
type Command struct {
	Name  string // 裸命令名，等
	Short string // 一行说明（出现在命令列表里）
	FS    *flag.FlagSet
	app   *App
}

// App 是一个可执行文件的命令集。
type App struct {
	Name  string // 可执行文件名（帮助里显示）
	Short string // 一行用途
	Def   string // 缺省命令：argv 未给命令名时使用
	Out   io.Writer
	cmds  []*Command
	ver   func() string // --version 输出行（未注册则不认这个开关）
}

// SetVersion 注册 `--version`：任意位置出现即把版本行打到 **stdout** 并结束（返回 ErrHelp，
// 调用方 exit 0）。只给 gateway/core 这类要被人手查版本的二进制用；插件不必。
//
// 为什么做在外壳里：各 main 的开关都是按命令隔离的，`--version` 属于「不给命令名也要能查」
// 的全局信息，写进 serve 的 FlagSet 就变成 `serve --version` 这种别扭形态。
func (a *App) SetVersion(fn func() string) { a.ver = fn }

// NewApp 创建命令集；def 为缺省命令名（通常为协议本体 "call"）。
func NewApp(name, short, def string) *App {
	return &App{Name: name, Short: short, Def: def, Out: os.Stderr}
}

// Cmd 声明一个子命令并返回它（后续用 c.Str/c.Int 注册开关）。
func (a *App) Cmd(name, short string) *Command {
	c := &Command{Name: name, Short: short, app: a}
	c.FS = flag.NewFlagSet(name, flag.ContinueOnError)
	c.FS.SetOutput(a.Out)
	// 开关一律双短横线（W3-21）：flag 包默认用单短横线渲染 help，这里改掉，
	// 否则文档说 --data、帮助里写 -data，内部调用方照帮助写就会两端不一致。
	c.FS.Usage = func() {
		fmt.Fprintf(a.Out, "用法：%s %s [选项]\n\n选项：\n", a.Name, name)
		c.FS.VisitAll(func(f *flag.Flag) {
			if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
				fmt.Fprintf(a.Out, "  --%s\n\t%s（默认 %q）\n", f.Name, f.Usage, f.DefValue)
				return
			}
			fmt.Fprintf(a.Out, "  --%s\n\t%s\n", f.Name, f.Usage)
		})
	}
	a.cmds = append(a.cmds, c)
	return c
}

// Str 注册一个字符串开关（调用时必须写 --name value）。
func (c *Command) Str(name, def, usage string) *string {
	return c.FS.String(name, def, usage)
}

// Int 注册一个整数开关。
func (c *Command) Int(name string, def int, usage string) *int {
	return c.FS.Int(name, def, usage)
}

// Bool 注册一个布尔开关（`--name` 即 true）。
func (c *Command) Bool(name string, def bool, usage string) *bool {
	return c.FS.Bool(name, def, usage)
}

// listValue 让同一个开关可以**重复出现**，按给出顺序累积成切片（W3-30 的 `--ref`）。
// 刻意不做逗号切分：data URI 与部分预签名 URL 本身合法含逗号（`base64,iVBOR…`），切分会拆坏值。
type listValue struct{ dst *[]string }

func (l listValue) String() string     { return strings.Join(*l.dst, ", ") }
func (l listValue) Set(v string) error { *l.dst = append(*l.dst, v); return nil }

// StrList 注册一个可重复的字符串开关：`--ref a.png --ref b.jpg` → ["a.png","b.jpg"]。
// 返回切片指针供调用方读取；一次都没给时为空切片（非 nil）。
func (c *Command) StrList(name, usage string) *[]string {
	dst := new([]string)
	c.FS.Var(listValue{dst}, name, usage)
	return dst
}

// Usage 打印整个命令集帮助。
func (a *App) Usage() {
	w := a.Out
	fmt.Fprintf(w, "%s —— %s\n\n用法：%s <command> [--switch value ...]\n\n命令：\n", a.Name, a.Short, a.Name)
	for _, c := range a.cmds {
		mark := " "
		if c.Name == a.Def {
			mark = "*"
		}
		fmt.Fprintf(w, " %s %-10s %s\n", mark, c.Name, c.Short)
	}
	fmt.Fprintf(w, "\n（* 为缺省命令：不给命令名时使用）\n开关一律用双短横线，例如 `%s %s --help`。\n", a.Name, a.Def)
}

// Parse 解析 argv（不含程序名）。
//
// 无可识别的命令时用 Def；命令名后的一切交给该命令自己的 FlagSet。
// `--help` / `-h` / 无参数且 Def 为空 → 返回 ErrHelp。
func (a *App) Parse(args []string) (*Command, error) {
	if len(args) == 0 {
		if a.Def == "" {
			a.Usage()
			return nil, ErrHelp
		}
		return a.find(a.Def)
	}
	switch args[0] {
	case "--help", "-h", "help":
		a.Usage()
		return nil, ErrHelp
	}
	// --version 不看命令：`wild-work3 --version` 与 `wild-work3 serve --version` 都要能查
	if a.ver != nil {
		for _, s := range args {
			if s == "--version" {
				fmt.Fprintln(os.Stdout, a.ver())
				return nil, ErrHelp
			}
		}
	}
	// 第一个位置参数是命令名；以 "-" 开头说明没给命令名（用缺省命令）。
	name := args[0]
	rest := args[1:]
	if strings.HasPrefix(name, "-") {
		if a.Def == "" {
			a.Usage()
			return nil, fmt.Errorf("缺少命令名")
		}
		name, rest = a.Def, args
	}
	c, err := a.find(name)
	if err != nil {
		return nil, err
	}
	if err := c.FS.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, ErrHelp
		}
		return nil, err
	}
	if c.FS.NArg() > 0 {
		return nil, fmt.Errorf("命令 %s 不接受位置参数，多余: %v", name, c.FS.Args())
	}
	return c, nil
}

func (a *App) find(name string) (*Command, error) {
	for _, c := range a.cmds {
		if c.Name == name {
			return c, nil
		}
	}
	fmt.Fprintf(a.Out, "未知命令: %s\n\n", name)
	a.Usage()
	return nil, fmt.Errorf("未知命令: %s", name)
}

// Name 返回所选命令名（便于 switch）。
func (c *Command) CmdName() string { return c.Name }

// ConfigArg 解析 --config 的取值：以 "@" 开头则读该文件内容，否则原样返回。
//
// 目的：渠道配置里常有长 JSON（headers、schema），命令行里内联很难看；
// `--config @cfg.json` 让脚本可读。
func ConfigArg(v string) string {
	if !strings.HasPrefix(v, "@") {
		return v
	}
	b, err := os.ReadFile(strings.TrimPrefix(v, "@"))
	if err != nil {
		return v // 读不到就按原样返回，让上层 JSON 解析报出可读错误
	}
	return string(b)
}
