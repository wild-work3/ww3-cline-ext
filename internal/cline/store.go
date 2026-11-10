package cline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Store 管理 <data>/auths 下的凭据文件（每账号一个 JSON，0600）。
//
// 落盘形与旧版 wild-work 的 auths/ 兼容：固定嵌套形 {"auth":{...},"account":{...}}，
// 读取同时兼容扁平形（旧 CPA 面板格式）。见 PLUGIN_CLI §5.1。
type Store struct {
	Dir string
}

// NewStore 在 dataDir 下创建凭据目录视图。
func NewStore(dataDir string) *Store {
	return &Store{Dir: filepath.Join(dataDir, "auths")}
}

// authFile 是固定嵌套落盘形：{"auth":{...},"account":{...}}。
type authFile struct {
	Auth    json.RawMessage `json:"auth"`
	Account authAccount     `json:"account"`
}

type authAccount struct {
	UID      string `json:"uid,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	Region   string `json:"region,omitempty"`
}

// sanitizeName 仅保留 [A-Za-z0-9._-]，其余替换为 '_'。
func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// AuthFileName 返回凭据文件名（不含 .json）：cline-<uid>。
func AuthFileName(a *Auth) string {
	if a == nil {
		return ""
	}
	if a.AuthFile != "" {
		return a.AuthFile
	}
	uid := sanitizeName(a.UID)
	if uid == "" {
		return ""
	}
	region := a.RegionOf()
	if region == "" {
		region = AuthPrefix
	}
	return region + "-" + uid
}

// Path 返回 name 对应的绝对路径（自动补 .json）。
func (s *Store) Path(name string) string {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".json")
	if name == "" {
		return ""
	}
	return filepath.Join(s.Dir, name+".json")
}

// Save 原子写入凭据（目录 0700、文件 0600，固定嵌套形）。
func (s *Store) Save(a *Auth) (string, error) {
	if a == nil {
		return "", fmt.Errorf("凭据为空")
	}
	name := AuthFileName(a)
	if name == "" {
		return "", fmt.Errorf("凭据缺少 uid，无法确定文件名")
	}
	a.AuthFile = name
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return "", err
	}
	authRaw, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	payload, err := json.MarshalIndent(authFile{
		Auth: authRaw,
		Account: authAccount{
			UID:      a.UID,
			Nickname: a.Nickname,
			Region:   a.Region,
		},
	}, "", "  ")
	if err != nil {
		return "", err
	}
	path := s.Path(name)
	tmp, err := os.CreateTemp(s.Dir, "."+name+".*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", err
	}
	_ = os.Chmod(path, 0o600)
	return name, nil
}

// Load 读取凭据，兼容嵌套形与扁平形。
func (s *Store) Load(name string) (*Auth, error) {
	path := s.Path(name)
	if path == "" {
		return nil, fmt.Errorf("凭据名为空")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseAuth(raw)
}

func parseAuth(raw []byte) (*Auth, error) {
	// 先试嵌套形。
	var nested authFile
	if err := json.Unmarshal(raw, &nested); err == nil && len(nested.Auth) > 0 {
		a, err := decodeAuth(nested.Auth)
		if err != nil {
			return nil, err
		}
		if a.UID == "" {
			a.UID = nested.Account.UID
		}
		if a.Nickname == "" {
			a.Nickname = nested.Account.Nickname
		}
		if a.Region == "" {
			a.Region = nested.Account.Region
		}
		return a, nil
	}
	// 回退扁平形。
	return decodeAuth(raw)
}

func decodeAuth(raw json.RawMessage) (*Auth, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("凭据文件损坏: %v", err)
	}
	var a Auth
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("凭据文件损坏: %v", err)
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("凭据缺少 accessToken")
	}
	a.ExpiresAt = parseExpiresAt(probe["expiresAt"])
	return &a, nil
}

// parseExpiresAt 接受 Unix 秒（数字或字符串）与 RFC3339。
func parseExpiresAt(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return normalizeSec(n)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return 0
		}
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			return normalizeSec(v)
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// normalizeSec 把毫秒时间戳归一到秒。
func normalizeSec(v int64) int64 {
	if v > 1_000_000_000_000 {
		return v / 1000
	}
	return v
}

// List 返回全部凭据，按文件名排序。
func (s *Store) List() ([]*Auth, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Auth
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		a, err := s.Load(name)
		if err != nil {
			continue
		}
		a.AuthFile = name
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AuthFile < out[j].AuthFile })
	return out, nil
}

// Delete 删除凭据文件。
func (s *Store) Delete(name string) error {
	path := s.Path(name)
	if path == "" {
		return fmt.Errorf("凭据名为空")
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// FindByUID 按 uid 查找凭据。
func (s *Store) FindByUID(uid string) (*Auth, error) {
	list, err := s.List()
	if err != nil {
		return nil, err
	}
	for _, a := range list {
		if a.UID == uid {
			return a, nil
		}
	}
	return nil, os.ErrNotExist
}
