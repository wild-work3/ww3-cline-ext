package channel

import (
	"encoding/json"
	"os"
	"path/filepath"

	"wild-work3/cline-ext/internal/protocol/errkind"
	"wild-work3/cline-ext/internal/protocol/jsonl"
)

// Raw 是协议原始 JSON。
type Raw = jsonl.RawMessage

// Err 是协议错误对象。
type Err = jsonl.ErrObj

// errf 构造协议错误。
func errf(kind errkind.Kind, scope errkind.Scope, format string, a ...any) *Err {
	return jsonl.Errorf(kind, scope, format, a...)
}

// mustJSON 序列化协议响应（不可失败）。
func mustJSON(v any) Raw {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// scopeOfKind 把错误种类映射到协议作用域。
//
// 与 raccoon 的差别：本渠道没有会话态（Key 不可刷新），SessionDead 意味着 Key 被删，
// 属账号级；Passthrough 一律请求级（原文透传，不罚号）。
func scopeOfKind(k errkind.Kind) errkind.Scope {
	switch k {
	case errkind.HardCredit, errkind.SoftRate, errkind.SessionDead, errkind.Auth, errkind.AccountFault:
		return errkind.ScopeAccount
	case errkind.NotFound, errkind.ModelBlocked, errkind.PromptTooLong:
		return errkind.ScopeModel
	case errkind.Server, errkind.ServerErr, errkind.WafBlock, errkind.Conn:
		return errkind.ScopeChannel
	default:
		return errkind.ScopeRequest
	}
}

// writeJSON 原子写入 JSON（目录 0700，文件 0600）。
func writeJSON(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readJSON 读取 JSON 文件。
func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
