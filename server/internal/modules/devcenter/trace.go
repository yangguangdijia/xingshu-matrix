package devcenter

// 交付价值流（v1.32 vs-trace）：需求 / 缺陷 ↔ 提交、合并请求、构建、发布、告警。
//
//   GET    /devcenter/{requirements|bugs}/:id/trace     价值流追踪（devcenter:read）
//   POST   /devcenter/{requirements|bugs}/:id/links     手工关联（devcenter:manage，source=manual）
//   DELETE /devcenter/links/:id                          解除关联（devcenter:manage，仅 manual）
//   GET    /devcenter/releases/:release_id/items         发布包含的需求 / 缺陷 / 提交与发布说明（devcenter:read）
//
// 兼容：POST .../links 请求体带 ref_type（app|pipeline）时仍按 v1.28 的“代码仓库 / 流水线关联”处理
// （表 devcenter_repo_links）；DELETE /devcenter/links/:id 先找价值流关联，找不到再找旧关联。两表共用 id 序列，不会混淆。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// registerTrace 注册价值流接口。module.go 中原有的 POST /:k/:id/links 与 DELETE /links/:id 需移除（见报告补丁），
// 由这里的兼容处理器接管。
func (m *Module) registerTrace(g *gin.RouterGroup, rd, mg gin.HandlerFunc) {
	for _, k := range []string{"requirements", "bugs"} {
		g.GET("/"+k+"/:id/trace", rd, m.traceItem(k))
		g.POST("/"+k+"/:id/links", mg, m.addTraceLink(k))
	}
	g.DELETE("/links/:id", mg, m.delTraceLink)
	g.GET("/releases/:release_id/items", rd, m.releaseItems)
}

/* ------------------------------------------------------------------ 类型 */

// TraceLinkKinds 是价值流关联种类。
var TraceLinkKinds = []string{"commit", "mr", "run", "release", "alert"}

// TraceStageKeys 是追踪视图的阶段顺序。
var TraceStageKeys = []string{"code", "build", "artifact", "release", "env"}

// TraceLink 是 devcenter_links（价值流结构）的一行。
type TraceLink struct {
	ID         int64      `json:"id"`
	ItemType   string     `json:"item_type"`
	ItemID     int64      `json:"item_id"`
	Kind       string     `json:"kind"`
	Ref        string     `json:"ref"`
	AppID      *int64     `json:"app_id"`
	Title      string     `json:"title"`
	URL        string     `json:"url"`
	Author     string     `json:"author"`
	Env        string     `json:"env"`
	Status     string     `json:"status"`
	OccurredAt *time.Time `json:"occurred_at"`
	Source     string     `json:"source"`
	CreatedAt  time.Time  `json:"created_at"`
}

// TraceStageItem 是阶段内的一条记录。id/source 仅关联表来源的记录有（artifact/env 为推导记录，id=0）。
type TraceStageItem struct {
	ID         int64      `json:"id,omitempty"`
	Source     string     `json:"source,omitempty"`
	Kind       string     `json:"kind"`
	Ref        string     `json:"ref"`
	Title      string     `json:"title"`
	URL        string     `json:"url"`
	Author     string     `json:"author"`
	Env        string     `json:"env"`
	Status     string     `json:"status"`
	OccurredAt *time.Time `json:"occurred_at"`
	AppID      *int64     `json:"app_id"`
}

// TraceStage 是追踪视图的一个阶段。
type TraceStage struct {
	Key   string           `json:"key"`
	State string           `json:"state"` // done | running | failed | none
	At    *time.Time       `json:"at"`
	Items []TraceStageItem `json:"items"`
}

// TraceEnv 是某应用某环境当前在线、且包含本条目改动的发布。
type TraceEnv struct {
	Env       string     `json:"env"`
	AppID     int64      `json:"app_id"`
	AppName   string     `json:"app_name"`
	ReleaseID int64      `json:"release_id"`
	Image     string     `json:"image"`
	At        *time.Time `json:"at"`
}

// traceArtifact 是成功构建产出的镜像。
type traceArtifact struct {
	RunID  int64
	AppID  int64
	Image  string
	Status string
	At     *time.Time
}

/* ------------------------------------------------------------------ 纯逻辑 */

// traceState 把各来源的状态归一为 done | running | failed。
func traceState(kind, status string) string {
	s := strings.ToLower(strings.TrimSpace(status))
	switch s {
	case "queued", "pending", "running", "cancelling", "awaiting_approval", "approved", "starting", "progressing", "paused", "promoting", "processing", "opened", "open":
		if kind == "alert" {
			return "failed"
		}
		return "running"
	case "failed", "cancelled", "canceled", "error", "rejected", "rolled_back", "rolling_back", "blocked", "firing", "active":
		return "failed"
	}
	return "done" // succeeded / merged / resolved / 提交（无状态）等
}

func traceItemOf(l TraceLink) TraceStageItem {
	return TraceStageItem{ID: l.ID, Source: l.Source, Kind: l.Kind, Ref: l.Ref, Title: l.Title, URL: l.URL, Author: l.Author, Env: l.Env, Status: l.Status, OccurredAt: l.OccurredAt, AppID: l.AppID}
}

func traceLater(a, b *time.Time) bool {
	if a == nil {
		return false
	}
	return b == nil || a.After(*b)
}

// traceStageOf 按最新一条记录的状态决定阶段状态；records 已按时间倒序。
func traceStageOf(key string, items []TraceStageItem) TraceStage {
	st := TraceStage{Key: key, State: "none", Items: items}
	if st.Items == nil {
		st.Items = []TraceStageItem{}
	}
	if len(items) == 0 {
		return st
	}
	st.At = items[0].OccurredAt
	st.State = traceState(items[0].Kind, items[0].Status)
	if key == "code" {
		st.State = "done"
	}
	return st
}

func traceSortItems(items []TraceStageItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if traceLater(items[i].OccurredAt, items[j].OccurredAt) {
			return true
		}
		if traceLater(items[j].OccurredAt, items[i].OccurredAt) {
			return false
		}
		return items[i].ID > items[j].ID
	})
}

// buildTraceStages 由关联、成功构建的镜像和在线环境组装五个阶段。
// env 阶段：有在线环境为 done；关联的告警仍在触发时为 failed（告警作为记录附在 env 阶段）。
func buildTraceStages(links []TraceLink, arts []traceArtifact, envs []TraceEnv) []TraceStage {
	by := map[string][]TraceStageItem{}
	for _, l := range links {
		switch l.Kind {
		case "commit", "mr":
			by["code"] = append(by["code"], traceItemOf(l))
		case "run":
			by["build"] = append(by["build"], traceItemOf(l))
		case "release":
			by["release"] = append(by["release"], traceItemOf(l))
		case "alert":
			by["alert"] = append(by["alert"], traceItemOf(l))
		}
	}
	for _, a := range arts {
		app := a.AppID
		by["artifact"] = append(by["artifact"], TraceStageItem{Kind: "artifact", Ref: a.Image, Title: a.Image, Status: a.Status, OccurredAt: a.At, AppID: &app})
	}
	var envItems []TraceStageItem
	for _, e := range envs {
		app := e.AppID
		envItems = append(envItems, TraceStageItem{Kind: "env", Ref: strconv.FormatInt(e.ReleaseID, 10), Title: strings.TrimSpace(e.AppName + " " + e.Env), Env: e.Env, Status: "succeeded", OccurredAt: e.At, AppID: &app})
	}
	out := make([]TraceStage, 0, len(TraceStageKeys))
	for _, k := range TraceStageKeys {
		items := by[k]
		if k == "env" {
			traceSortItems(envItems)
			alerts := by["alert"]
			traceSortItems(alerts)
			st := traceStageOf(k, append(append([]TraceStageItem{}, envItems...), alerts...))
			st.State, st.At = "none", nil
			if len(envItems) > 0 {
				st.State, st.At = "done", envItems[0].OccurredAt
			}
			for _, a := range alerts {
				if traceState("alert", a.Status) == "failed" {
					st.State = "failed"
					if traceLater(a.OccurredAt, st.At) {
						st.At = a.OccurredAt
					}
					break
				}
			}
			out = append(out, st)
			continue
		}
		traceSortItems(items)
		out = append(out, traceStageOf(k, items))
	}
	return out
}

// traceLinkInput 是手工关联的请求体。
type traceLinkInput struct {
	Kind  string `json:"kind"`
	Ref   string `json:"ref"`
	AppID *int64 `json:"app_id"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

var (
	traceShaRe    = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
	traceNumRe    = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	traceMRRefRe  = regexp.MustCompile(`^[!#]?[1-9][0-9]{0,18}$`)
	traceAnyRefRe = regexp.MustCompile(`^[^\s]{1,200}$`)
)

func (in *traceLinkInput) check() error {
	in.Kind = strings.ToLower(strings.TrimSpace(in.Kind))
	in.Ref = strings.TrimSpace(in.Ref)
	in.Title = strings.TrimSpace(in.Title)
	in.URL = strings.TrimSpace(in.URL)
	switch in.Kind {
	case "commit":
		if !traceShaRe.MatchString(in.Ref) {
			return httpx.Validation("提交编号应为 7–64 位十六进制字符")
		}
		in.Ref = strings.ToLower(in.Ref)
	case "mr":
		if !traceMRRefRe.MatchString(in.Ref) {
			return httpx.Validation("合并请求编号应为数字，如 !12 或 12")
		}
		in.Ref = strings.TrimLeft(in.Ref, "!#")
	case "run", "release":
		if !traceNumRe.MatchString(in.Ref) {
			return httpx.Validation("请填写有效的构建或发布编号")
		}
	case "alert":
		if !traceAnyRefRe.MatchString(in.Ref) {
			return httpx.Validation("请填写有效的告警编号")
		}
	default:
		return httpx.Validation("关联类型只能是提交、合并请求、构建、发布或告警")
	}
	if in.AppID != nil && *in.AppID <= 0 {
		in.AppID = nil
	}
	if len([]rune(in.Title)) > 200 {
		return httpx.Validation("标题不能超过 200 个字")
	}
	if in.URL != "" {
		if len(in.URL) > 1000 || !(strings.HasPrefix(in.URL, "https://") || strings.HasPrefix(in.URL, "http://")) {
			return httpx.Validation("链接需以 http:// 或 https:// 开头，且不超过 1000 个字符")
		}
	}
	return nil
}

/* ------------------------------------------------------------------ 存储辅助 */

// traceTablesReady 判断价值流关联表是否就绪（0004 迁移已执行）。
func (m *Module) traceTablesReady(ctx context.Context) bool {
	var ok bool
	_ = m.d.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='devcenter_links' AND column_name='item_type')`).Scan(&ok)
	return ok
}

// traceHas 返回给定表是否存在（只读跨模块查询前判断）。
func (m *Module) traceHas(ctx context.Context, tables ...string) map[string]bool {
	out := map[string]bool{}
	rows, err := m.d.DB.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema=current_schema() AND table_name=ANY($1)`, tables)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			out[n] = true
		}
	}
	return out
}

func (m *Module) traceHasColumn(ctx context.Context, table, col string) bool {
	var ok bool
	_ = m.d.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 AND column_name=$2)`, table, col).Scan(&ok)
	return ok
}

const traceLinkCols = "id,item_type,item_id,kind,ref,app_id,title,url,author,env,status,occurred_at,source,created_at"

func scanTraceLink(row pgx.Row) (TraceLink, error) {
	var l TraceLink
	err := row.Scan(&l.ID, &l.ItemType, &l.ItemID, &l.Kind, &l.Ref, &l.AppID, &l.Title, &l.URL, &l.Author, &l.Env, &l.Status, &l.OccurredAt, &l.Source, &l.CreatedAt)
	return l, err
}

func (m *Module) loadTraceLinks(ctx context.Context, itemType string, id int64) ([]TraceLink, error) {
	rows, err := m.d.DB.Query(ctx, "SELECT "+traceLinkCols+" FROM devcenter_links WHERE item_type=$1 AND item_id=$2 ORDER BY COALESCE(occurred_at,created_at) DESC, id DESC LIMIT 500", itemType, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TraceLink{}
	for rows.Next() {
		l, err := scanTraceLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// loadTraceArtifacts 读取关联构建中已成功且产出镜像的记录。
func (m *Module) loadTraceArtifacts(ctx context.Context, links []TraceLink, has map[string]bool) []traceArtifact {
	if !has["cicd_runs"] || !has["cicd_applications"] {
		return nil
	}
	var ids []int64
	for _, l := range links {
		if l.Kind == "run" {
			if n, err := strconv.ParseInt(l.Ref, 10, 64); err == nil {
				ids = append(ids, n)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := m.d.DB.Query(ctx, `SELECT r.id, r.app_id, COALESCE(a.image_repo,''), r.image_tag, r.status, COALESCE(r.finished_at, r.created_at)
		FROM cicd_runs r LEFT JOIN cicd_applications a ON a.id=r.app_id
		WHERE r.id=ANY($1) AND r.image_tag<>'' AND r.status='succeeded' ORDER BY r.id DESC LIMIT 50`, ids)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []traceArtifact
	seen := map[string]bool{}
	for rows.Next() {
		var a traceArtifact
		var repo, tag string
		var at time.Time
		if rows.Scan(&a.RunID, &a.AppID, &repo, &tag, &a.Status, &at) != nil {
			continue
		}
		a.Image = tag
		if repo != "" {
			a.Image = repo + ":" + tag
		}
		a.At = &at
		if !seen[a.Image] {
			seen[a.Image] = true
			out = append(out, a)
		}
	}
	return out
}

// loadTraceEnvs：对关联发布涉及的每个 应用+环境，取当前在线的发布（最新一次成功的发布）。
// 当前在线发布就是关联发布本身，或是在最早一次关联的成功发布之后的常规部署（非回滚）时，认为改动已在该环境上线。
// 这是按时间线性推进的近似判断（不比对 git 祖先关系）。
func (m *Module) loadTraceEnvs(ctx context.Context, links []TraceLink, has map[string]bool) []TraceEnv {
	if !has["cicd_releases"] {
		return nil
	}
	type pair struct {
		app int64
		env string
	}
	linked := map[int64]bool{}
	first := map[pair]int64{}
	var order []pair
	for _, l := range links {
		if l.Kind != "release" || l.AppID == nil || l.Env == "" || traceState("release", l.Status) != "done" {
			continue
		}
		id, err := strconv.ParseInt(l.Ref, 10, 64)
		if err != nil {
			continue
		}
		linked[id] = true
		p := pair{*l.AppID, l.Env}
		if cur, ok := first[p]; !ok || id < cur {
			if !ok {
				order = append(order, p)
			}
			first[p] = id
		}
	}
	appName := "''"
	join := ""
	if has["cicd_applications"] {
		appName, join = "COALESCE(a.name,'')", " LEFT JOIN cicd_applications a ON a.id=r.app_id"
	}
	var out []TraceEnv
	for i, p := range order {
		if i >= 20 {
			break
		}
		var e TraceEnv
		var kind string
		var at time.Time
		err := m.d.DB.QueryRow(ctx, `SELECT r.id, r.app_id, `+appName+`, r.env, r.image, r.kind, COALESCE(r.finished_at, r.created_at)
			FROM cicd_releases r`+join+` WHERE r.app_id=$1 AND r.env=$2 AND r.status='succeeded' ORDER BY r.id DESC LIMIT 1`, p.app, p.env).
			Scan(&e.ReleaseID, &e.AppID, &e.AppName, &e.Env, &e.Image, &kind, &at)
		if err != nil {
			continue
		}
		if linked[e.ReleaseID] || (kind != "rollback" && e.ReleaseID > first[p]) {
			e.At = &at
			out = append(out, e)
		}
	}
	return out
}

/* ------------------------------------------------------------------ 处理器 */

func (m *Module) traceItem(k string) gin.HandlerFunc {
	itemType := itemTypeOfKind(k)
	return func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		var title, status string
		if err := m.d.DB.QueryRow(c, "SELECT title,status FROM "+table(k)+" WHERE id=$1", id).Scan(&title, &status); err != nil {
			fail(c, err)
			return
		}
		item := gin.H{"type": itemType, "id": id, "title": title, "status": status, "key": ItemKey{Type: itemType, ID: id}.String()}
		if !m.traceTablesReady(c) {
			httpx.OK(c, gin.H{"item": item, "stages": buildTraceStages(nil, nil, nil), "envs": []TraceEnv{}})
			return
		}
		// 惰性同步：同一条目 60 秒内最多一次，最长 3 秒，失败不影响查询。
		if traceGuard.allow(itemType, id, time.Now()) {
			sctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
			key := ItemKey{Type: itemType, ID: id}
			if err := m.syncTraceLinks(sctx, &key); err != nil && m.d.Logger != nil {
				m.d.Logger.Warn("devcenter trace lazy sync failed", "item", key.String(), "err", err)
			}
			cancel()
		}
		links, err := m.loadTraceLinks(c, itemType, id)
		if err != nil {
			fail(c, err)
			return
		}
		has := m.traceHas(c, "cicd_runs", "cicd_releases", "cicd_applications")
		arts := m.loadTraceArtifacts(c, links, has)
		envs := m.loadTraceEnvs(c, links, has)
		if envs == nil {
			envs = []TraceEnv{}
		}
		httpx.OK(c, gin.H{"item": item, "stages": buildTraceStages(links, arts, envs), "envs": envs})
	}
}

// addTraceLink 手工关联。请求体含 ref_type 时转交 v1.28 的代码仓库 / 流水线关联（兼容旧前端）。
func (m *Module) addTraceLink(k string) gin.HandlerFunc {
	legacy := m.addLink(k)
	itemType := itemTypeOfKind(k)
	return func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 64<<10))
		if err != nil {
			httpx.Fail(c, httpx.Validation("请求内容格式错误"))
			return
		}
		var probe map[string]json.RawMessage
		if json.Unmarshal(raw, &probe) != nil {
			httpx.Fail(c, httpx.Validation("请求内容格式错误"))
			return
		}
		if _, isLegacy := probe["ref_type"]; isLegacy {
			c.Request.Body = io.NopCloser(bytes.NewReader(raw))
			legacy(c)
			return
		}
		var in traceLinkInput
		if json.Unmarshal(raw, &in) != nil {
			httpx.Fail(c, httpx.Validation("请求字段类型错误"))
			return
		}
		if err := in.check(); err != nil {
			httpx.Fail(c, err)
			return
		}
		if !m.traceTablesReady(c) {
			httpx.Fail(c, httpx.Conflict("价值流关联尚未启用，请先完成数据库升级"))
			return
		}
		if err := m.itemExists(c, k, id); err != nil {
			fail(c, err)
			return
		}
		_, me := who(c)
		l := TraceLink{ItemType: itemType, ItemID: id, Kind: in.Kind, Ref: in.Ref, AppID: in.AppID, Title: in.Title, URL: in.URL, Author: me, Source: "manual"}
		m.fillManualLink(c, &l)
		row := m.d.DB.QueryRow(c, `INSERT INTO devcenter_links(item_type,item_id,kind,ref,app_id,title,url,author,env,status,occurred_at,source)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'manual') RETURNING `+traceLinkCols,
			l.ItemType, l.ItemID, l.Kind, l.Ref, l.AppID, l.Title, l.URL, l.Author, l.Env, l.Status, l.OccurredAt)
		saved, err := scanTraceLink(row)
		if err != nil {
			fail(c, err) // 23505 → 409 已存在相同的关联
			return
		}
		traceGuard.reset(itemType, id) // 下次查看追踪时立即按新关联补全构建 / 发布
		audit.SetResource(c, "devcenter_link", strconv.FormatInt(saved.ID, 10))
		audit.SetDetail(c, gin.H{"item_type": itemType, "item_id": id, "kind": l.Kind, "ref": l.Ref, "source": "manual"})
		httpx.Created(c, saved)
	}
}

// fillManualLink 尽力用 cicd 数据补全手工关联的标题、应用、环境与状态（只读）。
func (m *Module) fillManualLink(ctx context.Context, l *TraceLink) {
	refID, _ := strconv.ParseInt(l.Ref, 10, 64)
	switch l.Kind {
	case "run":
		if !m.traceHas(ctx, "cicd_runs")["cicd_runs"] {
			return
		}
		var app int64
		var st, by string
		var at time.Time
		if m.d.DB.QueryRow(ctx, `SELECT app_id,status,triggered_by,COALESCE(finished_at,started_at,created_at) FROM cicd_runs WHERE id=$1`, refID).Scan(&app, &st, &by, &at) == nil {
			if l.AppID == nil {
				l.AppID = &app
			}
			l.Status, l.OccurredAt = st, &at
			if l.Title == "" {
				l.Title = "构建 #" + l.Ref
			}
		}
	case "release":
		if !m.traceHas(ctx, "cicd_releases")["cicd_releases"] {
			return
		}
		var app int64
		var env, st, img string
		var at time.Time
		if m.d.DB.QueryRow(ctx, `SELECT app_id,env,status,image,COALESCE(finished_at,created_at) FROM cicd_releases WHERE id=$1`, refID).Scan(&app, &env, &st, &img, &at) == nil {
			if l.AppID == nil {
				l.AppID = &app
			}
			l.Env, l.Status, l.OccurredAt = env, st, &at
			if l.Title == "" {
				l.Title = img
			}
		}
	}
	if l.OccurredAt == nil {
		now := time.Now()
		l.OccurredAt = &now
	}
}

// delTraceLink 解除关联：价值流关联只允许解除手工关联（自动关联会被同步重新建立）；
// id 不在价值流表时按 v1.28 代码仓库 / 流水线关联处理。
func (m *Module) delTraceLink(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	if !m.traceTablesReady(c) {
		m.delLink(c)
		return
	}
	var itemType, source, kind, ref string
	var itemID int64
	err := m.d.DB.QueryRow(c, "SELECT item_type,item_id,kind,ref,source FROM devcenter_links WHERE id=$1", id).Scan(&itemType, &itemID, &kind, &ref, &source)
	if errors.Is(err, pgx.ErrNoRows) {
		m.delLink(c) // 旧关联（devcenter_repo_links）；不存在时返回 404
		return
	}
	if err != nil {
		fail(c, err)
		return
	}
	if source != "manual" {
		httpx.Fail(c, httpx.Conflict("自动识别的关联不能手工解除；请修改提交说明或分支名中的编号"))
		return
	}
	tag, err := m.d.DB.Exec(c, "DELETE FROM devcenter_links WHERE id=$1 AND source='manual'", id)
	if err != nil {
		fail(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Fail(c, httpx.NotFound("关联不存在或已解除"))
		return
	}
	audit.SetResource(c, "devcenter_link", strconv.FormatInt(id, 10))
	audit.SetDetail(c, gin.H{"item_type": itemType, "item_id": itemID, "kind": kind, "ref": ref, "source": "manual"})
	httpx.OK(c, gin.H{"id": id})
}
