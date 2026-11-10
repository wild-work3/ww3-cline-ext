package cli

import (
	"os"
	"path/filepath"
	"strings"
)

// 数据目录子目录名（契约，见 protocol/docs/PLUGIN_CLI.md §5）。
const (
	SubAuths = "auths"  // 凭据
	SubLogs  = "logs"   // 日志
	SubStat  = "stat"   // 渠道自记统计
	AuthsDir = SubAuths // 兼容别名
)

// ResolveData 决定插件数据目录（W3-21）：--data > $WW3_DATA > 可执行文件所在目录。
//
// core 永远显式传 --data（插件 exe 根目录，与旧版 wild-work.exe 同习惯）；
// 手工调试不传时退回 exe 目录，**绝不退回 cwd**——旧版曾有凭据写到 cwd 的坑。
func ResolveData(flagValue string) string {
	if v := strings.TrimSpace(flagValue); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("WW3_DATA")); v != "" {
		return v
	}
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// AuthPath 返回凭据文件路径：<data>/auths/<name>.json（name 不带后缀）。
//
// name 即「凭据标识」= 旧版的凭据文件名去后缀（如 workbuddyai-wb-uid-001），
// 它同时是 core 侧的 account_uid（见 protocol/docs/PLUGIN_CLI.md §5.2）。
func AuthPath(data, name string) string {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".json")
	if name == "" {
		return ""
	}
	return filepath.Join(data, SubAuths, name+".json")
}

// LogPath 返回插件日志路径 <data>/logs/<name>.log（data 为空返回空串=只写 stderr）。
func LogPath(data, name string) string {
	if data == "" {
		return ""
	}
	return filepath.Join(data, SubLogs, name+".log")
}
