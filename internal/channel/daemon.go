// Package channel 是 cline-ext（Cline 免费模型池）渠道插件的实现：控制面 + 会话面同一份方法表。
//
// 运行形态（与 raccoon-ext 一致）：`call` 一次性（stdin 一条 invoke → 事件 + result → 退出）
// 与 `stdio` 常连；core 一律用 call，无常驻进程。
//
// 与旧版 wild-work 的差异（移植取舍，均为本仓库契约要求）：
//   - 认证是 **API Key 表单**（`sk_` + 64 hex），不走 WorkOS device-code OAuth；
//     Key 不轮换 ⇒ 没有 refreshToken、没有保活/签到（故**没有** accounts.checkin）。
//   - auth.begin/finish 直接校验并落盘凭据，auth.status 立即返回成功（表单一次性流程）。
//   - 不做地区限制改道（旧版 backuplink.Replay）：只单出口直连，地区拒绝按 Passthrough 透传。
//   - 不做模型级 cap 冷却表：模型面零本地假设，模型维度统计交给 core。
package channel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wild-work3/cline-ext/internal/cline"
	"wild-work3/cline-ext/internal/protocol/authflow"
	"wild-work3/cline-ext/internal/protocol/chat"
	"wild-work3/cline-ext/internal/protocol/cli"
	"wild-work3/cline-ext/internal/protocol/errkind"
	"wild-work3/cline-ext/internal/protocol/meta"
	"wild-work3/cline-ext/internal/protocol/plugin"
	"wild-work3/cline-ext/internal/protocol/stat"
)

// PluginID 是插件 ID（模型命名空间前缀同为 cline）。
const PluginID = cline.ChannelName

// Log 是插件进程级日志器（main 里初始化；nil 时静默）。
var Log *cli.Logger

// Manifest 返回插件清单。
func Manifest() meta.Manifest {
	return meta.Manifest{
		ID:              PluginID,
		Name:            "Cline 免费池（cline）",
		DefaultChannels: []meta.DefaultChannel{{ID: PluginID, Name: "Cline", Config: map[string]any{}}},
		Version:         "1.0.0",
		Kind:            "channel",
		Protocol:        meta.ProtocolRng{Min: 1, Max: 1},
		Execution: meta.Execution{
			Control:      plugin.ModeCall,
			Conversation: plugin.ModeCall,
			MaxWorkers:   4,
		},
		Capabilities: meta.Capabilities{
			Control: []string{
				"models.list", "config.schema", "config.get", "config.set",
				"auth.methods", "auth.begin", "auth.status", "auth.finish", "auth.cancel",
				"accounts.list", "accounts.resource_detail", "accounts.refresh",
				"credits.get", "stats.get", "health.check", "ping",
			},
			Conversation: []string{"chat.generate"},
		},
		Icon:  "cline",
		Color: "#00b0ff",
		Description: "Cline 免费模型池渠道：API Key（sk_ 前缀）鉴权，模型面绑死上游 " +
			"recommended-models 的 free[] 原样透传，OpenAI 兼容 SSE，模型前缀 cline。",
	}
}

// Daemon 是控制面状态（凭据仓库 + 登录流程记录 + 统计）。
type Daemon struct {
	dataDir string
	store   *cline.Store
	stat    *stat.Store
}

// NewDaemon 创建控制面（dataDir 由 core 通过 --data 注入）。
func NewDaemon(dataDir string) *Daemon {
	return &Daemon{
		dataDir: dataDir,
		store:   cline.NewStore(dataDir),
		stat:    stat.Open(dataDir),
	}
}

// Serve 运行插件（call 或 stdio 两种形态，共用同一份方法表）。
func Serve(ctx context.Context, mode, dataDir string, in io.Reader, out io.Writer) error {
	r := registry(NewDaemon(dataDir))
	if mode == plugin.ModeCall {
		return r.ServeOnce(ctx, in, out)
	}
	return r.ServeStdio(ctx, in, out)
}

// registry 组装完整方法表（控制面 + 会话面）。
func registry(d *Daemon) *plugin.Registry {
	r := plugin.New()
	r.Lifecycle(Manifest())

	r.Handle("config.schema", func(_ context.Context, _ Raw) (Raw, *Err) {
		return mustJSON(configSchema()), nil
	})
	r.Handle("config.get", func(_ context.Context, p Raw) (Raw, *Err) {
		cfg := d.loadConfig()
		if in := parseConfig(p); !cfgEmpty(in) {
			cfg = mergeOne(cfg, in)
		}
		return mustJSON(map[string]any{"config": cfg}), nil
	})
	r.Handle("config.set", func(_ context.Context, p Raw) (Raw, *Err) {
		in := parseConfig(p)
		cfg := mergeOne(d.loadConfig(), in)
		if err := d.saveConfig(cfg); err != nil {
			return nil, errf(errkind.BadParams, errkind.ScopeRequest, "保存配置失败: %v", err)
		}
		return mustJSON(map[string]any{"ok": true}), nil
	})

	r.Handle("auth.methods", func(_ context.Context, p Raw) (Raw, *Err) {
		return mustJSON(d.authMethods(p)), nil
	})
	r.Handle("auth.begin", func(ctx context.Context, p Raw) (Raw, *Err) {
		return d.authBegin(ctx, p)
	})
	r.Handle("auth.status", func(_ context.Context, p Raw) (Raw, *Err) {
		return d.authStatus(p)
	})
	r.Handle("auth.finish", func(ctx context.Context, p Raw) (Raw, *Err) {
		return d.authFinish(ctx, p)
	})
	r.Handle("auth.cancel", func(_ context.Context, p Raw) (Raw, *Err) {
		var req authflow.CancelRequest
		_ = json.Unmarshal(p, &req)
		if req.FlowID == "" {
			return nil, errf(errkind.BadParams, errkind.ScopeRequest, "缺少 flow_id")
		}
		d.dropFlow(req.FlowID)
		return mustJSON(map[string]any{"ok": true}), nil
	})

	r.Handle("accounts.list", func(_ context.Context, _ Raw) (Raw, *Err) {
		return d.accountsList()
	})
	r.Handle("accounts.resource_detail", func(ctx context.Context, p Raw) (Raw, *Err) {
		return d.resourceDetail(ctx, p)
	})
	r.Handle("accounts.refresh", func(ctx context.Context, p Raw) (Raw, *Err) {
		return d.accountRefresh(ctx, p)
	})
	r.Handle("models.list", func(ctx context.Context, p Raw) (Raw, *Err) {
		return d.modelsList(ctx, p)
	})
	r.Handle("credits.get", func(ctx context.Context, p Raw) (Raw, *Err) {
		return d.creditsGet(ctx, p)
	})
	r.Handle("stats.get", func(_ context.Context, p Raw) (Raw, *Err) {
		return d.statsGet(p)
	})
	r.Handle("health.check", func(ctx context.Context, p Raw) (Raw, *Err) {
		return d.healthCheck(ctx, p)
	})
	r.HandleStream("chat.generate", d.generate)
	return r
}

// ---------- 配置 ----------

func (d *Daemon) configPath() string {
	if d.dataDir == "" {
		return ""
	}
	return filepath.Join(d.dataDir, "config.json")
}

func (d *Daemon) loadConfig() cline.Config {
	p := d.configPath()
	if p == "" {
		return cline.Config{}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return cline.Config{}
	}
	var cfg cline.Config
	_ = json.Unmarshal(b, &cfg)
	return cfg
}

func (d *Daemon) saveConfig(cfg cline.Config) error {
	p := d.configPath()
	if p == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// empty 报告是否所有字段都为空（用于 config.get 的「无覆盖」判定）。
func cfgEmpty(c cline.Config) bool {
	return c.Region == "" && c.Proxy == "" && c.Timeout == 0 &&
		c.BaseURL == "" && c.Auth == "" && c.APIKey == ""
}

// mergeOne 用非空字段覆盖 base。
func mergeOne(base, in cline.Config) cline.Config {
	if in.Region != "" {
		base.Region = in.Region
	}
	if in.Proxy != "" {
		base.Proxy = in.Proxy
	}
	if in.Timeout != 0 {
		base.Timeout = in.Timeout
	}
	if in.BaseURL != "" {
		base.BaseURL = in.BaseURL
	}
	if in.Auth != "" {
		base.Auth = in.Auth
	}
	if in.APIKey != "" {
		base.APIKey = in.APIKey
	}
	return base
}

// parseConfig 解析 core 下发的渠道配置（先 {config:{...}} 再直接形态）。
func parseConfig(p Raw) cline.Config {
	var w struct {
		Config cline.Config `json:"config"`
	}
	if err := json.Unmarshal(p, &w); err == nil && !cfgEmpty(w.Config) {
		return w.Config
	}
	var direct cline.Config
	_ = json.Unmarshal(p, &direct)
	return direct
}

// channelConfig 解析 chat.generate 参数里的 channel_config 字段。
//
// ★ AGENTS 陷阱 20：渠道配置在 channel_config，不在 params.config。
func channelConfig(raw Raw) cline.Config {
	var w struct {
		ChannelConfig json.RawMessage `json:"channel_config"`
	}
	_ = json.Unmarshal(raw, &w)
	if len(w.ChannelConfig) == 0 {
		return cline.Config{}
	}
	return cline.ParseConfig(w.ChannelConfig)
}

// mergeConfig 用请求内嵌的渠道配置覆盖外层配置（非空字段优先）。
func mergeConfig(cfg cline.Config, req chat.GenerateRequest) cline.Config {
	return mergeOne(cfg, cline.ParseConfig(req.ChannelConfig))
}

// uidOf 解析 params 里的账号选择。
func uidOf(p Raw) string {
	var w struct {
		UID string `json:"uid"`
	}
	_ = json.Unmarshal(p, &w)
	return strings.TrimSpace(w.UID)
}

func configSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"proxy": map[string]any{
				"type":        "string",
				"title":       "代理",
				"description": "HTTP(S) 代理，例如 http://127.0.0.1:7890（留空走系统环境变量）",
			},
			"timeout": map[string]any{
				"type":        "integer",
				"title":       "超时（秒）",
				"default":     300,
				"description": "上游短请求与响应头超时（秒）；流式读取另由 90s 空闲看门狗兜底",
			},
			"base_url": map[string]any{
				"type":        "string",
				"title":       "上游基址",
				"default":     cline.APIBase,
				"description": "高级：覆盖上游基址（联调/自建反代用）",
			},
			"api_key": map[string]any{
				"type":        "string",
				"title":       "API Key",
				"format":      "password",
				"description": "Cline API Key（sk_ 开头）。可直接填在这里，或在「添加账号」里保存到凭据文件",
			},
			"region": map[string]any{
				"type":        "string",
				"title":       "区域",
				"description": "渠道区域（当前仅 cline）",
				"default":     cline.DefaultRegion,
			},
			"auth": map[string]any{
				"type":        "string",
				"title":       "指定凭据",
				"description": "指定使用某个凭据文件名（多账号时用）",
			},
		},
	}
}

// ---------- 登录（表单一次性：begin 即校验落盘，status 立即成功） ----------

func (d *Daemon) authMethods(p Raw) authflow.MethodsResult {
	cfg := parseConfig(p)
	region := cfg.Region
	if region == "" {
		region = cline.DefaultRegion
	}
	return authflow.MethodsResult{
		Region: region,
		Methods: []authflow.Method{
			{
				ID:          "apikey",
				Type:        authflow.TypeForm,
				Label:       "API Key",
				Description: "在 app.cline.bot → Account / API Keys 创建 Key（sk_ 开头，只显示一次）。",
				Fields: map[string]any{
					"api_key": map[string]any{
						"type":        "string",
						"title":       "API Key",
						"format":      "password",
						"required":    true,
						"description": "sk_ 开头的 Cline API Key",
					},
					"nickname": map[string]any{
						"type":        "string",
						"title":       "备注名（可选）",
						"required":    false,
						"description": "留空则用上游 Key 的 name 字段",
					},
				},
				Region:  region,
				Default: true,
			},
		},
		Note: "Key 不轮换 ⇒ 无需保活/签到；配额按**账号**计（…UsageCostUSDPerUser），" +
			"同账号多个 Key 不会扩大额度。",
	}
}

// flowRecord 是落盘的登录流程记录。
//
// 为什么要落盘：auth.begin 与 auth.status 是**两个独立进程**（call 形态），
// 流程状态必须跨进程可见（与 raccoon 的 flows 落盘同理）。
type flowRecord struct {
	FlowID  string            `json:"flow_id"`
	State   string            `json:"state"`
	Hint    string            `json:"hint,omitempty"`
	Account *authflow.Account `json:"account,omitempty"`
	Error   *authflow.Error   `json:"error,omitempty"`
	Created int64             `json:"created"`
}

func (d *Daemon) flowPath(id string) string {
	if d.dataDir == "" || id == "" {
		return ""
	}
	return filepath.Join(d.dataDir, "flows", id+".json")
}

func (d *Daemon) saveFlow(f flowRecord) {
	p := d.flowPath(f.FlowID)
	if p == "" {
		return
	}
	if err := writeJSON(p, f); err != nil && Log != nil {
		Log.Errorf("写登录流程失败: %v", err)
	}
}

func (d *Daemon) loadFlow(id string) (flowRecord, bool) {
	p := d.flowPath(id)
	if p == "" {
		return flowRecord{}, false
	}
	var f flowRecord
	if err := readJSON(p, &f); err != nil {
		return flowRecord{}, false
	}
	return f, true
}

func (d *Daemon) dropFlow(id string) {
	if p := d.flowPath(id); p != "" {
		_ = os.Remove(p)
	}
}

func newFlowID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("f%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func (d *Daemon) authBegin(ctx context.Context, p Raw) (Raw, *Err) {
	var req authflow.BeginRequest
	_ = json.Unmarshal(p, &req)
	cfg := parseConfig(p)
	key := strings.TrimSpace(req.Values["api_key"])
	if key == "" {
		key = strings.TrimSpace(cfg.APIKey)
	}
	if key == "" {
		return nil, errf(errkind.BadParams, errkind.ScopeRequest, "请填写 API Key（sk_ 开头）")
	}
	a, rerr := d.login(ctx, cfg, key, strings.TrimSpace(req.Values["nickname"]))
	if rerr != nil {
		return nil, errf(rerr.Kind, scopeOfKind(rerr.Kind), "%s", rerr.Msg)
	}
	flowID := newFlowID()
	d.saveFlow(flowRecord{
		FlowID:  flowID,
		State:   string(authflow.StateSuccess),
		Hint:    "API Key 校验通过，凭据已保存",
		Account: accountOf(a),
		Created: time.Now().Unix(),
	})
	return mustJSON(authflow.BeginResult{
		FlowID:     flowID,
		Type:       authflow.TypeForm,
		Method:     orDefault(req.Method, "apikey"),
		State:      string(authflow.StateSuccess),
		Prompt:     "API Key 校验通过，凭据已保存。",
		ExpiresAt:  time.Now().Add(10 * time.Minute).Format(time.RFC3339),
		NextPollMS: 0, // 表单流程同步完成，不需要轮询
	}), nil
}

// authFinish 与 auth.begin 等价（表单可能由 core 走 finish 通道提交 values）。
func (d *Daemon) authFinish(ctx context.Context, p Raw) (Raw, *Err) {
	var req authflow.FinishRequest
	if err := json.Unmarshal(p, &req); err != nil {
		return nil, errf(errkind.BadParams, errkind.ScopeRequest, "请求体解析失败: %v", err)
	}
	if req.FlowID != "" {
		if f, ok := d.loadFlow(req.FlowID); ok && f.State == string(authflow.StateSuccess) {
			return mustJSON(authflow.StatusResult{
				State:   authflow.StateSuccess,
				Hint:    f.Hint,
				Account: f.Account,
			}), nil
		}
	}
	cfg := parseConfig(p)
	key := strings.TrimSpace(req.Values["api_key"])
	if key == "" {
		key = strings.TrimSpace(cfg.APIKey)
	}
	if key == "" {
		return nil, errf(errkind.BadParams, errkind.ScopeRequest, "请填写 API Key（sk_ 开头）")
	}
	a, rerr := d.login(ctx, cfg, key, strings.TrimSpace(req.Values["nickname"]))
	if rerr != nil {
		return nil, errf(rerr.Kind, scopeOfKind(rerr.Kind), "%s", rerr.Msg)
	}
	flowID := req.FlowID
	if flowID == "" {
		flowID = newFlowID()
	}
	d.saveFlow(flowRecord{
		FlowID:  flowID,
		State:   string(authflow.StateSuccess),
		Hint:    "API Key 校验通过，凭据已保存",
		Account: accountOf(a),
		Created: time.Now().Unix(),
	})
	return mustJSON(authflow.StatusResult{
		State:   authflow.StateSuccess,
		Hint:    "API Key 校验通过，凭据已保存",
		Account: accountOf(a),
	}), nil
}

// authStatus 立即返回流程结果（表单流程在 begin 时已完成全部工作）。
func (d *Daemon) authStatus(p Raw) (Raw, *Err) {
	var req authflow.StatusRequest
	_ = json.Unmarshal(p, &req)
	if req.FlowID == "" {
		return mustJSON(authflow.StatusResult{
			State: authflow.StateFailed,
			Hint:  "缺少 flow_id",
			Error: &authflow.Error{Kind: string(errkind.BadParams), Message: "缺少 flow_id"},
		}), nil
	}
	if f, ok := d.loadFlow(req.FlowID); ok {
		return mustJSON(authflow.StatusResult{
			State:   authflow.State(f.State),
			Hint:    f.Hint,
			Account: f.Account,
			Error:   f.Error,
		}), nil
	}
	// 流程记录已丢（数据目录迁移/清理）时不能谎报成功：给 expired 让前端重来。
	return mustJSON(authflow.StatusResult{
		State: authflow.StateExpired,
		Hint:  "登录流程已失效，请重新添加",
	}), nil
}

// login 校验 Key、探测身份并落盘凭据。
//
// 只调 GET /api/v1/api-keys（零 token 消耗、零副作用），避免「添加账号即烧额度」。
func (d *Daemon) login(ctx context.Context, cfg cline.Config, key, nickname string) (*cline.Auth, *cline.Error) {
	res, err := d.client(cfg).ProbeKey(key)
	if err != nil {
		return nil, cline.AsError(err, errkind.Auth)
	}
	a := &cline.Auth{
		AccessToken:        key,
		ExpiresAt:          cline.FarFutureExpiry(),
		UID:                res.UID,
		Nickname:           res.AccountName,
		Region:             cline.DefaultRegion,
		KeyPrefix:          res.KeyPrefix,
		SiblingKeyPrefixes: res.SiblingKeys,
	}
	if nickname != "" {
		a.Nickname = nickname
	}
	if a.Nickname == "" {
		a.Nickname = a.KeyPrefix
	}
	if _, serr := d.store.Save(a); serr != nil {
		return nil, &cline.Error{Kind: errkind.AccountFault, Msg: "保存凭据失败: " + serr.Error()}
	}
	if Log != nil {
		Log.Infof("已保存 cline 凭据 uid=%s prefix=%s 同账号其它 Key=%d",
			a.UID, a.KeyPrefix, len(a.SiblingKeyPrefixes))
	}
	return a, nil
}

// accountOf 把凭据转成 authflow.Account（登录成功后 core 据此落账号）。
func accountOf(a *cline.Auth) *authflow.Account {
	if a == nil {
		return nil
	}
	return &authflow.Account{
		UID:       a.UID,
		Nickname:  a.Nickname,
		ChannelID: PluginID,
		Region:    a.RegionOf(),
		ExpiresAt: a.ExpiresAtText(),
		LoginKind: string(authflow.TypeForm),
		AuthFile:  a.AuthFile,
	}
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// ---------- 账号 ----------

func (d *Daemon) accountsList() (Raw, *Err) {
	list, _ := d.store.List()
	out := make([]meta.AccountInfo, 0, len(list))
	for _, a := range list {
		uid := a.UID
		if uid == "" {
			uid = a.AuthFile
		}
		status := "ok"
		if a.Expired() {
			status = "expired"
		}
		out = append(out, meta.AccountInfo{
			UID:       uid,
			Nickname:  a.Nickname,
			Status:    status,
			LoginKind: string(authflow.TypeForm),
			ExpiresAt: a.ExpiresAtText(),
			ChannelID: PluginID,
			Region:    a.RegionOf(),
			Prefixes:  []string{cline.AuthPrefix},
			AuthFile:  a.AuthFile,
		})
	}
	return mustJSON(map[string]any{"accounts": out}), nil
}

// resourceDetail 返回额度明细。
//
// 上游**确无额度/余额端点**（旧版 12 个候选端点全 404），故只能返回空数组；
// 契约（docs/WW3_FRONTEND_API.md §3.4）要求**裸数组**，前端直接 map 渲染。
func (d *Daemon) resourceDetail(_ context.Context, p Raw) (Raw, *Err) {
	if uid := uidOf(p); uid != "" {
		if _, rerr := d.pickAuth(parseConfig(p), uid); rerr != nil {
			return nil, errf(rerr.Kind, scopeOfKind(rerr.Kind), "%s", rerr.Msg)
		}
	}
	return mustJSON([]meta.ResourceItem{}), nil
}

// accountRefresh 重新探测 Key 身份（昵称/前缀/兄弟 Key），Key 本身不轮换。
func (d *Daemon) accountRefresh(ctx context.Context, p Raw) (Raw, *Err) {
	cfg := parseConfig(p)
	a, aerr := d.pickAuth(cfg, uidOf(p))
	if aerr != nil {
		return nil, errf(aerr.Kind, scopeOfKind(aerr.Kind), "%s", aerr.Msg)
	}
	if a.AuthFile == "" {
		return mustJSON(map[string]any{"ok": true, "message": "凭据来自渠道配置（api_key），无需刷新"}), nil
	}
	res, err := d.client(cfg).ProbeKey(a.Key())
	if err != nil {
		e := cline.AsError(err, errkind.SessionDead)
		return nil, errf(e.Kind, scopeOfKind(e.Kind), "刷新凭据失败: %s", e.Msg)
	}
	a.UID = res.UID
	a.KeyPrefix = res.KeyPrefix
	a.SiblingKeyPrefixes = res.SiblingKeys
	if a.Nickname == "" || a.Nickname == a.KeyPrefix {
		a.Nickname = res.AccountName
	}
	if a.ExpiresAt <= 0 {
		a.ExpiresAt = cline.FarFutureExpiry()
	}
	if _, serr := d.store.Save(a); serr != nil {
		return nil, errf(errkind.AccountFault, errkind.ScopeAccount, "写回凭据失败: %v", serr)
	}
	msg := "已刷新（Key 无有效期，无需轮换）"
	if n := len(a.SiblingKeyPrefixes); n > 0 {
		msg += fmt.Sprintf("；同账号另有 %d 个 Key，额度按账号计不叠加", n)
	}
	return mustJSON(map[string]any{"ok": true, "message": msg}), nil
}

// ---------- 模型 / 积分 / 统计 / 健康 ----------

func (d *Daemon) modelsList(ctx context.Context, p Raw) (Raw, *Err) {
	cfg := parseConfig(p)
	// 目录端点无需鉴权：有凭据就带上（便于上游按账号定制），没有也照常拉。
	key := ""
	if a, err := d.pickAuth(cfg, ""); err == nil {
		key = a.Key()
	}
	ms, err := d.client(cfg).FetchModels(ctx, key)
	if err != nil {
		if Log != nil {
			Log.Errorf("拉取模型目录失败: %v", err)
		}
		ms = cline.StaticModels()
	}
	return mustJSON(map[string]any{"models": prefixModels(ms)}), nil
}

func prefixModels(ms []meta.ModelInfo) []meta.ModelInfo {
	out := make([]meta.ModelInfo, 0, len(ms))
	for _, m := range ms {
		m.ID = cline.PrefixModel(m.ID)
		out = append(out, m)
	}
	return out
}

func (d *Daemon) creditsGet(_ context.Context, p Raw) (Raw, *Err) {
	// 上游无额度端点 ⇒ 恒返回空明细（不报错，避免面板刷新链报红）。
	_ = p
	return mustJSON(map[string]any{
		"balance": nil,
		"items":   []meta.ResourceItem{},
		"unit":    "credits",
		"note":    "Cline 未提供余额查询接口（配额按账号计，仅在上游后台可见）",
	}), nil
}

func (d *Daemon) statsGet(p Raw) (Raw, *Err) {
	var q struct {
		Days int `json:"days"`
	}
	_ = json.Unmarshal(p, &q)
	days := q.Days
	if days <= 0 {
		days = 7
	}
	if d.stat == nil {
		return mustJSON(stat.Summary{Days: []stat.Day{}}), nil
	}
	return mustJSON(stat.Summarize(d.stat.Query(days))), nil
}

// healthCheck 报告本地状态（不触网：core 的健康检查会被定时调用，不得烧配额）。
func (d *Daemon) healthCheck(_ context.Context, p Raw) (Raw, *Err) {
	cfg := parseConfig(p)
	list, _ := d.store.List()
	ready := len(list) > 0 || strings.TrimSpace(cfg.APIKey) != ""
	state := "ok"
	hint := ""
	if !ready {
		state = "unconfigured"
		hint = "尚未添加 API Key"
	}
	return mustJSON(map[string]any{
		"state":    state,
		"hint":     hint,
		"base_url": d.client(cfg).BaseURL(),
		"accounts": len(list),
		"ready":    ready,
	}), nil
}

// ---------- 账号解析与统计写入 ----------

// pickAuth 按 uid / cfg.Auth / 列表顺序选取凭据；都没有时回落到渠道配置里的 api_key。
func (d *Daemon) pickAuth(cfg cline.Config, uid string) (*cline.Auth, *cline.Error) {
	if uid == "" {
		uid = cfg.Auth
	}
	if uid != "" {
		if a, err := d.store.Load(uid); err == nil {
			return a, nil
		}
		if a, err := d.store.FindByUID(uid); err == nil {
			return a, nil
		}
		return nil, &cline.Error{Kind: errkind.Auth, Msg: fmt.Sprintf("未找到账号 %s，请先添加 API Key", uid)}
	}
	list, _ := d.store.List()
	var pick *cline.Auth
	for _, a := range list {
		if !a.Expired() {
			pick = a
			break
		}
	}
	if pick == nil && len(list) > 0 {
		pick = list[0]
	}
	if pick != nil {
		return pick, nil
	}
	// 无落盘凭据 ⇒ 用渠道配置里的 api_key（面板「直接填密钥」路径）。
	if key := strings.TrimSpace(cfg.APIKey); key != "" {
		a := &cline.Auth{
			AccessToken: key,
			ExpiresAt:   cline.FarFutureExpiry(),
			UID:         "cfg-" + shortHash(key),
			Nickname:    "渠道配置 api_key",
			Region:      cline.DefaultRegion,
			KeyPrefix:   prefixOf(key),
		}
		return a, nil
	}
	return nil, &cline.Error{Kind: errkind.Auth, Msg: "该渠道尚未添加 API Key，请先在面板完成登录"}
}

// shortHash 取 key 的 sha256 前 12 位（稳定 uid，不含明文）。
func shortHash(key string) string { return cline.ShortHash(key) }

// prefixOf 取 Key 的官方回显前缀形态（sk_ + 7 hex，共 10 字符）。
func prefixOf(key string) string { return cline.KeyPrefixOf(key) }

// client 构造按配置定制的上游客户端。
func (d *Daemon) client(cfg cline.Config) *cline.Client {
	return cline.NewClient(cfg)
}

// statRecord 是待写入统计的一条记录（会话面内部结构）。
type statRecord struct {
	Kind               string
	OK                 bool
	Model              string
	Account            string
	Region             string
	ErrKind            string
	PromptTokens       int
	CompletionTokens   int
	TotalTokens        int
	CachedTokens       int
	CachedReported     bool
	PromptReported     bool
	CompletionReported bool
	TotalReported      bool
	FirstTokenMs       int
	Time               time.Duration
}

// appendStat 把内部记录写入 stat.Store（fail-open；未上报 token 记 -1）。
func (d *Daemon) appendStat(rec statRecord, _ cline.Config) {
	if d.stat == nil {
		return
	}
	p, cp, t := rec.PromptTokens, rec.CompletionTokens, rec.TotalTokens
	if p == 0 && cp == 0 && t == 0 {
		p, cp, t = -1, -1, -1
	}
	d.stat.Append(stat.Record{
		Kind:               rec.Kind,
		Channel:            PluginID,
		Account:            rec.Account,
		Region:             rec.Region,
		Model:              rec.Model,
		ErrKind:            rec.ErrKind,
		OK:                 rec.OK,
		PromptTokens:       p,
		CompletionTokens:   cp,
		TotalTokens:        t,
		CachedTokens:       rec.CachedTokens,
		CachedReported:     rec.CachedReported,
		PromptReported:     rec.PromptReported,
		CompletionReported: rec.CompletionReported,
		TotalReported:      rec.TotalReported,
		TimeMs:             int(rec.Time.Milliseconds()),
		FirstTokenMs:       rec.FirstTokenMs,
	})
}
