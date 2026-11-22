package devcenter

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// 交付效能（DORA 四项指标）：GET /devcenter/dora?days=30&app_id=&env=&tz_offset=480
// 只读 SQL 读取 cicd_releases / cicd_runs / cicd_canaries / oncall_incidents / devcenter_links（新结构），
// 读取前判断表（列）是否存在；计算部分（computeDora 及以下）与 SQL 分离、纯函数可单测。
// 分级阈值采用 DORA 2023（State of DevOps 2023）常用的逐项阈值，见 doraLevel* 函数。

// doraRelease 是一次发布（cicd_releases 行）计算所需的字段。
type doraRelease struct {
	ID         int64
	AppID      int64
	Env        string
	Kind       string // deploy | rollback
	RunID      *int64
	Status     string // running | succeeded | failed
	CreatedAt  time.Time
	FinishedAt *time.Time
	// LeadStart 是该发布的变更起点：优先取关联的最早提交 / 构建时间，否则取构建（run）创建时间；零值表示无法确定。
	LeadStart time.Time
	// LeadFromLinks 为 true 表示 LeadStart 来自关联表（devcenter_links）。
	LeadFromLinks bool
	// CanaryAborted 为 true 表示该发布的灰度被中止（回滚或失败）。
	CanaryAborted bool
}

// doraIncident 是一条值班故障（oncall_incidents 行）。
type doraIncident struct {
	DeclaredAt time.Time
	ResolvedAt *time.Time
}

// DoraDeployFreq 部署频率。
type DoraDeployFreq struct {
	PerDay float64 `json:"per_day"`
	Total  int     `json:"total"`
	Series [][]any `json:"series"` // [[YYYY-MM-DD, count], ...] 按天，含 0
}

// DoraLeadTime 变更前置时间（小时）。
type DoraLeadTime struct {
	MedianHours *float64 `json:"median_hours"`
	P90Hours    *float64 `json:"p90_hours"`
	Samples     int      `json:"samples"`
	// Basis 说明起点来源：links（关联的提交/构建时间）| run（构建创建时间）| mixed（两者都有）| none（无样本）。
	Basis string `json:"basis"`
}

// DoraCFR 变更失败率。
type DoraCFR struct {
	Rate   *float64 `json:"rate"` // 0~1，无部署时为 null
	Failed int      `json:"failed"`
	Total  int      `json:"total"`
}

// DoraMTTR 故障恢复时间（小时）。
type DoraMTTR struct {
	MedianHours *float64 `json:"median_hours"`
	Samples     int      `json:"samples"`
	Available   bool     `json:"available"` // 值班故障表不存在时为 false
}

// DoraLevels 四项指标的分级。
type DoraLevels struct {
	DeployFrequency   string `json:"deploy_frequency"`
	LeadTime          string `json:"lead_time"`
	ChangeFailureRate string `json:"change_failure_rate"`
	MTTR              string `json:"mttr"`
}

// DoraReport 是接口返回体。
type DoraReport struct {
	WindowDays        int            `json:"window_days"`
	From              string         `json:"from"`
	To                string         `json:"to"`
	DeployFrequency   DoraDeployFreq `json:"deploy_frequency"`
	LeadTime          DoraLeadTime   `json:"lead_time"`
	ChangeFailureRate DoraCFR        `json:"change_failure_rate"`
	MTTR              DoraMTTR       `json:"mttr"`
	Level             DoraLevels     `json:"level"`
}

const (
	doraElite   = "elite"
	doraHigh    = "high"
	doraMedium  = "medium"
	doraLow     = "low"
	doraUnknown = "unknown"
)

// doraRollbackWindow 是“发布后多久内同应用同环境回滚即视为失败变更”。
const doraRollbackWindow = 24 * time.Hour

// doraPercentile 用线性插值求分位数（p 取 0~100），输入不要求有序。
func doraPercentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	if len(s) == 1 {
		return s[0]
	}
	pos := p / 100 * float64(len(s)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return s[lo]
	}
	return s[lo] + (s[hi]-s[lo])*(pos-float64(lo))
}

func doraRound(f float64, n int) float64 {
	m := math.Pow(10, float64(n))
	return math.Round(f*m) / m
}

func doraPtr(f float64) *float64 { return &f }

// doraLevelDeployFreq：精英 每天至少 1 次（按需多次）；高 每周至少 1 次；中 每月至少 1 次；低 少于每月 1 次。
func doraLevelDeployFreq(perDay float64, hasData bool) string {
	switch {
	case !hasData:
		return doraUnknown
	case perDay >= 1:
		return doraElite
	case perDay >= 1.0/7:
		return doraHigh
	case perDay >= 1.0/30:
		return doraMedium
	default:
		return doraLow
	}
}

// doraLevelLeadTime：精英 < 1 天；高 < 1 周；中 < 1 个月（30 天）；低 ≥ 1 个月。按中位数判断。
func doraLevelLeadTime(median *float64) string {
	if median == nil {
		return doraUnknown
	}
	switch h := *median; {
	case h < 24:
		return doraElite
	case h < 24*7:
		return doraHigh
	case h < 24*30:
		return doraMedium
	default:
		return doraLow
	}
}

// doraLevelCFR：精英 ≤ 5%；高 ≤ 10%；中 ≤ 15%；低 > 15%。
func doraLevelCFR(rate *float64) string {
	if rate == nil {
		return doraUnknown
	}
	switch r := *rate; {
	case r <= 0.05:
		return doraElite
	case r <= 0.10:
		return doraHigh
	case r <= 0.15:
		return doraMedium
	default:
		return doraLow
	}
}

// doraLevelMTTR：精英 < 1 小时；高 < 1 天；中 < 1 周；低 ≥ 1 周。按中位数判断。
func doraLevelMTTR(median *float64) string {
	if median == nil {
		return doraUnknown
	}
	switch h := *median; {
	case h < 1:
		return doraElite
	case h < 24:
		return doraHigh
	case h < 24*7:
		return doraMedium
	default:
		return doraLow
	}
}

// releaseEnd 是发布的结束时间（未结束则为零值）。
func releaseEnd(r doraRelease) time.Time {
	if r.FinishedAt != nil {
		return *r.FinishedAt
	}
	return time.Time{}
}

// doraFailedDeploys 返回被判定为失败变更的部署发布 ID 集合：
// 发布失败；或灰度被中止；或其后 24 小时内同应用同环境出现回滚（回滚只归到它之前最近一次成功部署）。
func doraFailedDeploys(rels []doraRelease) map[int64]bool {
	failed := map[int64]bool{}
	type key struct {
		app int64
		env string
	}
	deploys := map[key][]doraRelease{}
	for _, r := range rels {
		if r.Kind == "rollback" {
			continue
		}
		if r.Status == "failed" || r.CanaryAborted {
			failed[r.ID] = true
		}
		if r.Status == "succeeded" && r.FinishedAt != nil {
			k := key{r.AppID, r.Env}
			deploys[k] = append(deploys[k], r)
		}
	}
	for k := range deploys {
		d := deploys[k]
		sort.Slice(d, func(i, j int) bool { return releaseEnd(d[i]).Before(releaseEnd(d[j])) })
	}
	for _, r := range rels {
		if r.Kind != "rollback" {
			continue
		}
		var prev *doraRelease
		for i, d := range deploys[key{r.AppID, r.Env}] {
			if releaseEnd(d).After(r.CreatedAt) {
				break
			}
			prev = &deploys[key{r.AppID, r.Env}][i]
		}
		if prev != nil && r.CreatedAt.Sub(releaseEnd(*prev)) <= doraRollbackWindow {
			failed[prev.ID] = true
		}
	}
	return failed
}

// computeDora 根据发布与故障计算 DORA 报告。rels 应为窗口内（按 created_at）的发布，含回滚；
// incidents 为窗口内宣告的故障，incidentsAvailable=false 表示故障数据源不存在。
func computeDora(rels []doraRelease, incidents []doraIncident, incidentsAvailable bool, from, to time.Time, days int, loc *time.Location) DoraReport {
	rep := DoraReport{WindowDays: days, From: from.In(loc).Format("2006-01-02"), To: to.In(loc).Format("2006-01-02")}

	// 部署频率：窗口内成功的部署（kind=deploy），按完成时间（缺省创建时间）落到天。
	counts := map[string]int{}
	hasAny := false
	var leadHours []float64
	fromLinks, fromRun := 0, 0
	for _, r := range rels {
		if r.Kind == "rollback" {
			continue
		}
		hasAny = true
		if r.Status != "succeeded" {
			continue
		}
		at := r.CreatedAt
		if r.FinishedAt != nil {
			at = *r.FinishedAt
		}
		if at.Before(from) || at.After(to) {
			continue
		}
		rep.DeployFrequency.Total++
		counts[at.In(loc).Format("2006-01-02")]++
		// 前置时间：起点 → 发布完成。
		if r.FinishedAt != nil && !r.LeadStart.IsZero() && !r.FinishedAt.Before(r.LeadStart) {
			leadHours = append(leadHours, r.FinishedAt.Sub(r.LeadStart).Hours())
			if r.LeadFromLinks {
				fromLinks++
			} else {
				fromRun++
			}
		}
	}
	rep.DeployFrequency.Series = [][]any{}
	d0 := time.Date(from.In(loc).Year(), from.In(loc).Month(), from.In(loc).Day(), 0, 0, 0, 0, loc)
	for i := 0; i < days; i++ {
		ds := d0.AddDate(0, 0, i).Format("2006-01-02")
		rep.DeployFrequency.Series = append(rep.DeployFrequency.Series, []any{ds, counts[ds]})
	}
	if days > 0 {
		rep.DeployFrequency.PerDay = doraRound(float64(rep.DeployFrequency.Total)/float64(days), 3)
	}

	rep.LeadTime.Samples = len(leadHours)
	switch {
	case fromLinks > 0 && fromRun > 0:
		rep.LeadTime.Basis = "mixed"
	case fromLinks > 0:
		rep.LeadTime.Basis = "links"
	case fromRun > 0:
		rep.LeadTime.Basis = "run"
	default:
		rep.LeadTime.Basis = "none"
	}
	if len(leadHours) > 0 {
		rep.LeadTime.MedianHours = doraPtr(doraRound(doraPercentile(leadHours, 50), 2))
		rep.LeadTime.P90Hours = doraPtr(doraRound(doraPercentile(leadHours, 90), 2))
	}

	// 变更失败率：窗口内已结束的部署（成功或失败）为分母。
	failed := doraFailedDeploys(rels)
	for _, r := range rels {
		if r.Kind == "rollback" || (r.Status != "succeeded" && r.Status != "failed") {
			continue
		}
		rep.ChangeFailureRate.Total++
		if failed[r.ID] {
			rep.ChangeFailureRate.Failed++
		}
	}
	if rep.ChangeFailureRate.Total > 0 {
		rep.ChangeFailureRate.Rate = doraPtr(doraRound(float64(rep.ChangeFailureRate.Failed)/float64(rep.ChangeFailureRate.Total), 4))
	}

	// 故障恢复时间：窗口内宣告且已解决的故障，宣告 → 解决 的中位数。
	rep.MTTR.Available = incidentsAvailable
	var rec []float64
	for _, in := range incidents {
		if in.ResolvedAt == nil || in.ResolvedAt.Before(in.DeclaredAt) {
			continue
		}
		rec = append(rec, in.ResolvedAt.Sub(in.DeclaredAt).Hours())
	}
	rep.MTTR.Samples = len(rec)
	if len(rec) > 0 {
		rep.MTTR.MedianHours = doraPtr(doraRound(doraPercentile(rec, 50), 2))
	}

	rep.Level = DoraLevels{
		DeployFrequency:   doraLevelDeployFreq(rep.DeployFrequency.PerDay, hasAny),
		LeadTime:          doraLevelLeadTime(rep.LeadTime.MedianHours),
		ChangeFailureRate: doraLevelCFR(rep.ChangeFailureRate.Rate),
		MTTR:              doraLevelMTTR(rep.MTTR.MedianHours),
	}
	return rep
}

// ---------------- SQL 部分 ----------------

func (m *Module) doraTableExists(ctx context.Context, name string) bool {
	var ok bool
	if err := m.d.DB.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok); err != nil {
		return false
	}
	return ok
}

// doraValueStreamLinks 判断 devcenter_links 是否已是价值流结构（vs-trace 的 0004 迁移：item_type/kind/ref/occurred_at）。
func (m *Module) doraValueStreamLinks(ctx context.Context) bool {
	var n int
	err := m.d.DB.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'devcenter_links'
		AND column_name IN ('item_type','item_id','kind','ref','occurred_at')`).Scan(&n)
	return err == nil && n == 5
}

func (m *Module) loadDoraReleases(ctx context.Context, from time.Time, appID *int64, env string) ([]doraRelease, error) {
	rows, err := m.d.DB.Query(ctx, `SELECT r.id, r.app_id, r.env, r.kind, r.status, r.created_at, r.finished_at, r.run_id, `+
		`COALESCE(run.created_at, 'epoch'::timestamptz) FROM cicd_releases r LEFT JOIN cicd_runs run ON run.id = r.run_id `+
		`WHERE r.created_at >= $1 AND ($2::bigint IS NULL OR r.app_id = $2) AND ($3 = '' OR r.env = $3) ORDER BY r.created_at LIMIT 50000`, from, appID, env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []doraRelease
	for rows.Next() {
		var r doraRelease
		var runAt time.Time
		if err := rows.Scan(&r.ID, &r.AppID, &r.Env, &r.Kind, &r.Status, &r.CreatedAt, &r.FinishedAt, &r.RunID, &runAt); err != nil {
			return nil, err
		}
		if runAt.Unix() > 0 {
			r.LeadStart = runAt
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// applyDoraLinks 用关联表把发布的起点替换为“关联到该发布（kind=release）或其构建（kind=run）的需求/缺陷上
// 最早的提交 / MR / 构建时间”。
func (m *Module) applyDoraLinks(ctx context.Context, rels []doraRelease) error {
	if len(rels) == 0 {
		return nil
	}
	refs, runs := make([]string, 0, len(rels)), make([]string, 0, len(rels))
	for _, r := range rels {
		refs = append(refs, strconv.FormatInt(r.ID, 10))
		if r.RunID != nil {
			runs = append(runs, strconv.FormatInt(*r.RunID, 10))
		}
	}
	rows, err := m.d.DB.Query(ctx, `SELECT rl.kind || ':' || rl.ref, min(x.occurred_at) FROM devcenter_links rl
		JOIN devcenter_links x ON x.item_type = rl.item_type AND x.item_id = rl.item_id
			AND x.kind IN ('commit','mr','run') AND x.occurred_at IS NOT NULL
		WHERE (rl.kind = 'release' AND rl.ref = ANY($1)) OR (rl.kind = 'run' AND rl.ref = ANY($2))
		GROUP BY rl.kind, rl.ref`, refs, runs)
	if err != nil {
		return err
	}
	defer rows.Close()
	starts := map[string]time.Time{}
	for rows.Next() {
		var ref string
		var at time.Time
		if err := rows.Scan(&ref, &at); err != nil {
			return err
		}
		starts[ref] = at
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range rels {
		at, ok := starts["release:"+strconv.FormatInt(rels[i].ID, 10)]
		if rels[i].RunID != nil {
			if rt, rok := starts["run:"+strconv.FormatInt(*rels[i].RunID, 10)]; rok && (!ok || rt.Before(at)) {
				at, ok = rt, true
			}
		}
		if ok {
			rels[i].LeadStart, rels[i].LeadFromLinks = at, true
		}
	}
	return nil
}

// applyDoraCanaries 标记灰度被中止（rolled_back / failed）的发布。
func (m *Module) applyDoraCanaries(ctx context.Context, rels []doraRelease) error {
	if len(rels) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(rels))
	for _, r := range rels {
		ids = append(ids, r.ID)
	}
	rows, err := m.d.DB.Query(ctx, `SELECT DISTINCT release_id FROM cicd_canaries WHERE release_id = ANY($1) AND status IN ('rolled_back','failed')`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	aborted := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		aborted[id] = true
	}
	for i := range rels {
		rels[i].CanaryAborted = aborted[rels[i].ID]
	}
	return rows.Err()
}

func (m *Module) loadDoraIncidents(ctx context.Context, from time.Time, appID *int64, env string) ([]doraIncident, error) {
	service := ""
	if appID != nil && m.doraTableExists(ctx, "cicd_applications") {
		_ = m.d.DB.QueryRow(ctx, `SELECT name FROM cicd_applications WHERE id = $1`, *appID).Scan(&service)
		if service == "" {
			return nil, nil // 应用不存在：没有可归属的故障
		}
	}
	rows, err := m.d.DB.Query(ctx, `SELECT declared_at, resolved_at FROM oncall_incidents
		WHERE declared_at >= $1 AND ($2 = '' OR service = $2) AND ($3 = '' OR env = $3) LIMIT 50000`, from, service, env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []doraIncident
	for rows.Next() {
		var in doraIncident
		if err := rows.Scan(&in.DeclaredAt, &in.ResolvedAt); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// registerDora 注册交付效能接口（在 module.go Register 中调用）。
func (m *Module) registerDora(g *gin.RouterGroup, rd, _ gin.HandlerFunc) {
	g.GET("/dora", rd, m.dora)
}

func (m *Module) dora(c *gin.Context) {
	days, err := strconv.Atoi(c.DefaultQuery("days", "30"))
	if err != nil || days < 1 || days > 365 {
		httpx.Fail(c, httpx.Validation("统计时间窗只能在 1 到 365 天之间"))
		return
	}
	appID, ok := optID(c, "app_id")
	if !ok {
		return
	}
	env := strings.TrimSpace(c.Query("env"))
	if len(env) > 63 {
		httpx.Fail(c, httpx.Validation("环境名称过长"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	loc := zone(c)
	to := time.Now().In(loc)
	from := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(days - 1))

	var rels []doraRelease
	if m.doraTableExists(ctx, "cicd_releases") {
		if rels, err = m.loadDoraReleases(ctx, from, appID, env); err != nil {
			fail(c, err)
			return
		}
		if m.doraTableExists(ctx, "cicd_canaries") {
			if err := m.applyDoraCanaries(ctx, rels); err != nil {
				fail(c, err)
				return
			}
		}
		if m.doraValueStreamLinks(ctx) {
			if err := m.applyDoraLinks(ctx, rels); err != nil {
				fail(c, err)
				return
			}
		}
	}
	var incidents []doraIncident
	incOK := m.doraTableExists(ctx, "oncall_incidents")
	if incOK {
		if incidents, err = m.loadDoraIncidents(ctx, from, appID, env); err != nil {
			fail(c, err)
			return
		}
	}
	httpx.OK(c, computeDora(rels, incidents, incOK, from, to, days, loc))
}
