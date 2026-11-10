// Package stat 是渠道插件自有的**统计流水**（W3-22 定案）。
//
// 为什么插件要自己记：core 的 stat 记账只记「core 看到的」——它不知道上游返回的
// 真实缓存命中、不知道积分包消耗、不知道一次控制面调用的成败细节。面板的
// 「用量与流水 / 运行统计」需要这些**渠道事实**，而事实只有插件知道（W3-09
// 「插件报告事实，core 执行策略」）。
//
// 落盘位置（W3-22 约定，插件 data 目录下）：
//
//	<data>/stat/stats.jsonl   逐条流水（append-only，一行一条 Record）
//	<data>/stat/daily.jsonl   按日聚合的派生视图（Append 时增量维护，可删可重建）
//
// 编码约定：
//   - 时间一律 RFC3339 本地时区字符串（插件机器时区即用户时区）；
//   - null ≠ 0：上游没上报的字段留 nil，不要写 0 冒充（沿用 IPC_CHAT 口径）；
//   - token 计数用 int，没上报写 -1（JSON 里仍是数字，读侧判 <0 即「未知」）。
//
// 统计**只用于展示**，不参与调度决策：调度靠 core 的实时状态，不靠磁盘流水。
package stat

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// 记录类型（Kind）。
const (
	KindChat    = "chat"    // 一次对话（含失败）
	KindCredits = "credits" // 一次额度/积分观测
	KindCheckin = "checkin" // 一次签到
	KindModels  = "models"  // 一次模型列表拉取
	KindAuth    = "auth"    // 一次登录流程终态
)

// Record 是一条统计流水。
type Record struct {
	TS       string `json:"ts"` // RFC3339（本地时区）
	Kind     string `json:"kind"`
	Channel  string `json:"channel,omitempty"`  // 插件 ID（custom-api / workbuddy / ...）
	Instance string `json:"instance,omitempty"` // 渠道实例 ID（core 侧的 channel_id，插件不一定知道）
	Account  string `json:"account,omitempty"`  // 账号（uid 或 auth 文件名）
	Region   string `json:"region,omitempty"`
	Model    string `json:"model,omitempty"`
	OK       bool   `json:"ok"`
	ErrKind  string `json:"err_kind,omitempty"`

	PromptTokens     int  `json:"prompt_tokens"`
	CompletionTokens int  `json:"completion_tokens"`
	TotalTokens      int  `json:"total_tokens"`
	CachedTokens     int  `json:"cached_tokens"`
	CachedReported   bool `json:"cached_reported,omitempty"`

	// 三维 token 是否由上游实报（W3-34）。留 false 时 Append 按「未上报」处理：
	// 值为 0 一律改写成 -1，避免把「上游没说」记成「明确是 0」。
	// 插件若确实拿到上游的显式 0，置 true 即可保留 0。
	PromptReported     bool `json:"prompt_reported,omitempty"`
	CompletionReported bool `json:"completion_reported,omitempty"`
	TotalReported      bool `json:"total_reported,omitempty"`

	CreditsDelta  *float64 `json:"credits_delta,omitempty"` // 本次观测到的积分变化（负数=消耗）
	CreditsRemain *float64 `json:"credits_remain,omitempty"`

	TimeMs       int `json:"time_ms,omitempty"`
	FirstTokenMs int `json:"first_token_ms,omitempty"`
}

// Daily 是按日/渠道/模型聚合的派生视图（daily.jsonl 的一行）。
type Daily struct {
	Day        string  `json:"day"` // 2026-10-22
	Kind       string  `json:"kind"`
	Channel    string  `json:"channel,omitempty"`
	Account    string  `json:"account,omitempty"`
	Model      string  `json:"model,omitempty"`
	Requests   int     `json:"requests"`
	OK         int     `json:"ok"`
	Errors     int     `json:"errors"`
	Prompt     int     `json:"prompt_tokens"`
	Completion int     `json:"completion_tokens"`
	Total      int     `json:"total_tokens"`
	Cached     int     `json:"cached_tokens"`
	Credits    float64 `json:"credits"`
	AvgTimeMs  int     `json:"avg_time_ms"`
}

// Store 是一个插件的统计目录（<data>/stat）。
type Store struct{ Dir string }

// Open 返回统计仓库；dataDir 为空时返回 nil（插件未配置数据目录则不记流水）。
func Open(dataDir string) *Store {
	if dataDir == "" {
		return nil
	}
	return &Store{Dir: filepath.Join(dataDir, "stat")}
}

func (s *Store) statsPath() string { return filepath.Join(s.Dir, "stats.jsonl") }
func (s *Store) dailyPath() string { return filepath.Join(s.Dir, "daily.jsonl") }

// Append 追加一条流水，并在每日文件里增量维护派生聚合。
//
// 每条都 fsync：统计是「事后取证」用的，进程被强杀不能丢最后几条。
// 写入失败静默忽略——统计坏了不该影响对话主流程（fail-open）。
func (s *Store) Append(r Record) {
	if s == nil {
		return
	}
	if r.TS == "" {
		r.TS = time.Now().Format(time.RFC3339)
	}
	r = normalize(r)
	if err := s.appendLine(s.statsPath(), r); err != nil {
		return
	}
	s.mergeDaily(r)
}

func (s *Store) appendLine(path string, v any) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// mergeDaily 把一条流水并入按日聚合（读全量 → 改一条 → 原子重写）。
//
// daily.jsonl 只是派生视图：文件损坏就整份重建（Query 会兜住），不值得为它做增量日志。
func (s *Store) mergeDaily(r Record) {
	day := r.TS
	if len(day) >= 10 {
		day = day[:10]
	}
	key := dailyKey(day, r)
	rows := s.readDaily()
	hit := -1
	for i := range rows {
		if dailyKey(rows[i].Day, Record{Kind: rows[i].Kind, Channel: rows[i].Channel, Account: rows[i].Account, Model: rows[i].Model}) == key {
			hit = i
			break
		}
	}
	if hit < 0 {
		rows = append(rows, Daily{Day: day, Kind: r.Kind, Channel: r.Channel, Account: r.Account, Model: r.Model})
		hit = len(rows) - 1
	}
	d := &rows[hit]
	d.Requests++
	if r.OK {
		d.OK++
	} else {
		d.Errors++
	}
	d.Prompt += max0(r.PromptTokens)
	d.Completion += max0(r.CompletionTokens)
	d.Total += max0(r.TotalTokens)
	d.Cached += max0(r.CachedTokens)
	if r.CreditsDelta != nil {
		d.Credits += *r.CreditsDelta
	}
	if r.TimeMs > 0 {
		d.AvgTimeMs = (d.AvgTimeMs*(d.Requests-1) + r.TimeMs) / d.Requests
	}
	// 整份重写（原子）：行数 = 天数 × 渠道 × 模型，量级极小
	tmp := s.dailyPath() + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	w := bufio.NewWriter(f)
	for _, row := range rows {
		b, err := json.Marshal(row)
		if err != nil {
			continue
		}
		w.Write(append(b, '\n'))
	}
	w.Flush()
	f.Sync()
	f.Close()
	os.Rename(tmp, s.dailyPath())
}

// normalize 统一「未上报」口径（W3-22 / W3-34）。
//
// 这条规则曾经散落在 18 份插件代码里各写一遍，实现互不一致（有的把 usage==nil 记 -1、
// 有的记 0；有的把「有 usage 但三维全 0」当未上报、有的当实报 0）。收口到这里之后，
// 所有插件的流水口径必然一致：**未标记 reported 且值为 0 的维度记 -1**（读侧判 <0 即未知）。
//
// 只对对话流水生效：积分/签到/模型列表没有 token 维度，0 就是 0。
func normalize(r Record) Record {
	if r.Kind != KindChat {
		return r
	}
	if !r.PromptReported && r.PromptTokens == 0 {
		r.PromptTokens = -1
	}
	if !r.CompletionReported && r.CompletionTokens == 0 {
		r.CompletionTokens = -1
	}
	if !r.TotalReported && r.TotalTokens == 0 {
		r.TotalTokens = -1
	}
	// 缓存：没报「命中数」时值为 0 也无意义（读侧看 CachedReported 决定是否进分母）
	if !r.CachedReported && r.CachedTokens == 0 {
		r.CachedTokens = -1
	}
	return r
}

func dailyKey(day string, r Record) string {
	return day + "|" + r.Kind + "|" + r.Channel + "|" + r.Account + "|" + r.Model
}

func max0(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// readDaily 读派生聚合文件（损坏/缺失返回空，不报错）。
func (s *Store) readDaily() []Daily {
	f, err := os.Open(s.dailyPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Daily
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var d Daily
		if json.Unmarshal(line, &d) == nil {
			out = append(out, d)
		}
	}
	return out
}

// Query 读最近 days 天的流水（days<=0 视为全部）。
func (s *Store) Query(days int) []Record {
	if s == nil {
		return nil
	}
	cut := time.Time{}
	if days > 0 {
		cut = time.Now().AddDate(0, 0, -days)
	}
	f, err := os.Open(s.statsPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if json.Unmarshal(line, &r) != nil {
			continue // 坏行跳过：强杀进程时最后一行可能只写了半截
		}
		if !cut.IsZero() {
			if t, err := time.Parse(time.RFC3339, r.TS); err == nil && t.Before(cut) {
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

// Daily 读派生聚合（可跨多个插件目录合并，见 QueryDailyDir）。
func (s *Store) DailyRows() []Daily {
	if s == nil {
		return nil
	}
	return s.readDaily()
}

// Summary 是跨渠道合并后的统计摘要（面板直接渲染）。
type Summary struct {
	Requests   int     `json:"requests"`
	OK         int     `json:"ok"`
	Errors     int     `json:"errors"`
	Prompt     int     `json:"prompt_tokens"`
	Completion int     `json:"completion_tokens"`
	Total      int     `json:"total_tokens"`
	Cached     int     `json:"cached_tokens"`
	Credits    float64 `json:"credits"`
	Days       []Day   `json:"days"` // 按日聚合（升序）
}

// Day 是摘要里的单日数据。
type Day struct {
	Day        string  `json:"day"`
	Requests   int     `json:"requests"`
	OK         int     `json:"ok"`
	Errors     int     `json:"errors"`
	Prompt     int     `json:"prompt_tokens"`
	Completion int     `json:"completion_tokens"`
	Total      int     `json:"total_tokens"`
	Cached     int     `json:"cached_tokens"`
	Credits    float64 `json:"credits"`
}

// Summarize 把多个插件的流水合并成摘要（core 侧聚合用；CLI 单插件也走这里）。
func Summarize(recs []Record) Summary {
	sum := Summary{Days: []Day{}}
	byDay := map[string]*Day{}
	for _, r := range recs {
		if r.Kind != KindChat {
			continue // 摘要口径只统计对话（积分/签到流水在各自端点看）
		}
		sum.Requests++
		if r.OK {
			sum.OK++
		} else {
			sum.Errors++
		}
		sum.Prompt += max0(r.PromptTokens)
		sum.Completion += max0(r.CompletionTokens)
		sum.Total += max0(r.TotalTokens)
		sum.Cached += max0(r.CachedTokens)
		if r.CreditsDelta != nil {
			sum.Credits += *r.CreditsDelta
		}
		day := r.TS
		if len(day) >= 10 {
			day = day[:10]
		}
		d := byDay[day]
		if d == nil {
			d = &Day{Day: day}
			byDay[day] = d
		}
		d.Requests++
		if r.OK {
			d.OK++
		} else {
			d.Errors++
		}
		d.Prompt += max0(r.PromptTokens)
		d.Completion += max0(r.CompletionTokens)
		d.Total += max0(r.TotalTokens)
		d.Cached += max0(r.CachedTokens)
		if r.CreditsDelta != nil {
			d.Credits += *r.CreditsDelta
		}
	}
	for _, d := range byDay {
		sum.Days = append(sum.Days, *d)
	}
	sort.Slice(sum.Days, func(i, j int) bool { return sum.Days[i].Day < sum.Days[j].Day })
	return sum
}
