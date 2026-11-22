package devcenter

// mywork.go 提供「我的交付」：当前用户负责或创建的需求 / 缺陷，各自走到了交付价值流的哪一段
// （代码 → 构建 → 制品 → 发布 → 环境），哪些受阻、最近上线了什么。
// 阶段优先取 devcenter_links（价值流关联表，由 0004_value_stream 迁移创建）；表不存在或条目没有关联时按状态推断。

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// StageKeys 是价值流阶段顺序。
var StageKeys = []string{"code", "build", "artifact", "release", "env"}

func stageIndex(k string) int {
	for i, s := range StageKeys {
		if s == k {
			return i
		}
	}
	return -1
}

// registerMyWork 注册「我的交付」接口；module.go 在 Register 中调用一次。
func (m *Module) registerMyWork(g *gin.RouterGroup, rd, _ gin.HandlerFunc) {
	g.GET("/my/delivery", rd, m.myDelivery)
}

// vsLink 是计算阶段所需的一条关联（只取用到的字段）。
type vsLink struct {
	Kind   string     // commit | mr | run | release | alert
	Ref    string     //
	Env    string     //
	Status string     //
	At     *time.Time // occurred_at，缺省为 created_at
	Image  string     // 运行产出的镜像标签（来自 cicd_runs，可空）
}

// MyItem 是「我的交付」列表中的一条。
type MyItem struct {
	ID            int64             `json:"id"`
	Title         string            `json:"title"`
	Status        string            `json:"status"`
	Priority      string            `json:"priority"`
	Assignee      string            `json:"assignee"`
	Stage         string            `json:"stage"`       // 走到的最远阶段；none 表示尚未进入价值流
	StageState    string            `json:"stage_state"` // done | running | failed | none
	EnvLatest     string            `json:"env_latest"`  // 最近一次成功发布的环境
	Stages        map[string]string `json:"stages"`      // 每个阶段的状态，供前端画阶段条
	Blocked       bool              `json:"blocked"`
	BlockedReason string            `json:"blocked_reason"`
	Bucket        string            `json:"bucket"` // dev | build | released | blocked | closed
	ReleasedAt    *time.Time        `json:"released_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	Source        string            `json:"stage_source"` // links | status
}

func normState(kind, s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch kind {
	case "commit":
		return "done"
	case "mr":
		switch s {
		case "merged", "done", "succeeded":
			return "done"
		case "closed":
			return "none"
		default:
			return "running"
		}
	}
	switch s {
	case "succeeded", "success", "done", "deployed", "merged":
		return "done"
	case "failed", "failure", "error", "cancelled", "canceled", "rejected":
		return "failed"
	case "":
		return "none"
	default:
		return "running"
	}
}

func later(a, b *time.Time) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	return a.After(*b)
}

// stageResult 是阶段计算结果。
type stageResult struct {
	Stages     map[string]string
	Stage      string
	State      string
	EnvLatest  string
	ReleasedAt *time.Time
	FailReason string
}

func emptyStages() map[string]string {
	m := map[string]string{}
	for _, k := range StageKeys {
		m[k] = "none"
	}
	return m
}

func (r *stageResult) finish() {
	r.Stage, r.State = "none", "none"
	for i := len(StageKeys) - 1; i >= 0; i-- {
		if st := r.Stages[StageKeys[i]]; st != "none" {
			r.Stage, r.State = StageKeys[i], st
			return
		}
	}
}

// stagesFromLinks 按关联推算各阶段状态：提交 / 合并请求 → 代码；最近一次运行 → 构建；
// 有成功运行 → 制品；最近一次发布 → 发布；有成功发布 → 环境（取最近成功发布的环境）。
// 最近一次运行或发布失败视为受阻。
func stagesFromLinks(links []vsLink) stageResult {
	r := stageResult{Stages: emptyStages()}
	var lastRun, lastRel, lastOK *vsLink
	for i := range links {
		l := &links[i]
		st := normState(l.Kind, l.Status)
		switch l.Kind {
		case "commit", "mr":
			if st == "done" || (st == "running" && r.Stages["code"] == "none") {
				r.Stages["code"] = st
			}
		case "run":
			if lastRun == nil || later(l.At, lastRun.At) {
				lastRun = l
			}
			if st == "done" {
				r.Stages["artifact"] = "done"
			}
		case "release":
			if lastRel == nil || later(l.At, lastRel.At) {
				lastRel = l
			}
			if st == "done" && (lastOK == nil || later(l.At, lastOK.At)) {
				lastOK = l
			}
		}
	}
	if lastRun != nil {
		r.Stages["build"] = normState("run", lastRun.Status)
		if r.Stages["build"] == "failed" {
			r.FailReason = "最近一次构建失败"
		}
	}
	if lastRel != nil {
		r.Stages["release"] = normState("release", lastRel.Status)
		if r.Stages["release"] == "failed" {
			r.FailReason = "最近一次发布失败"
			if lastRel.Env != "" {
				r.FailReason += "（" + lastRel.Env + "）"
			}
		}
	}
	if lastOK != nil {
		r.Stages["env"] = "done"
		r.EnvLatest = lastOK.Env
		r.ReleasedAt = lastOK.At
	}
	// 后面的阶段已经走到时，之前没有记录的阶段视为已完成（例如只关联了发布）。
	reached := false
	for i := len(StageKeys) - 1; i >= 0; i-- {
		k := StageKeys[i]
		if reached && r.Stages[k] == "none" {
			r.Stages[k] = "done"
		}
		if r.Stages[k] != "none" {
			reached = true
		}
	}
	r.finish()
	return r
}

// stagesFromStatus 在没有关联数据时按条目状态推断阶段（近似值）。
func stagesFromStatus(kind, status string, updated time.Time) stageResult {
	r := stageResult{Stages: emptyStages()}
	upto, cur := -1, "done"
	switch kind + ":" + status {
	case "requirements:doing", "bugs:doing", "bugs:reopened":
		upto, cur = 0, "running"
	case "bugs:fixed":
		upto, cur = 0, "done"
	case "requirements:testing", "requirements:accepting":
		upto, cur = 3, "running"
	case "requirements:done", "bugs:verified", "bugs:closed":
		upto, cur = 4, "done"
	}
	for i := 0; i <= upto; i++ {
		r.Stages[StageKeys[i]] = "done"
	}
	if upto >= 0 {
		r.Stages[StageKeys[upto]] = cur
	}
	if upto == 4 {
		t := updated
		r.ReleasedAt = &t
	}
	r.finish()
	return r
}

// blockedFromData 读取条目扩展字段里的人工受阻标记。
func blockedFromData(raw []byte) (bool, string) {
	var d map[string]any
	if json.Unmarshal(raw, &d) != nil {
		return false, ""
	}
	b, _ := d["blocked"].(bool)
	reason, _ := d["blocked_reason"].(string)
	return b, strings.TrimSpace(reason)
}

// classify 汇总条目的阶段、受阻与分组。
func classify(kind string, it *MyItem, data []byte, links []vsLink) {
	var r stageResult
	if len(links) > 0 {
		r, it.Source = stagesFromLinks(links), "links"
	} else {
		r, it.Source = stagesFromStatus(kind, it.Status, it.UpdatedAt), "status"
	}
	it.Stages, it.Stage, it.StageState, it.EnvLatest, it.ReleasedAt = r.Stages, r.Stage, r.State, r.EnvLatest, r.ReleasedAt
	manual, reason := blockedFromData(data)
	switch {
	case it.Status == "blocked" || manual:
		it.Blocked, it.BlockedReason = true, reason
		if reason == "" {
			it.BlockedReason = "已标记为受阻"
		}
	case r.FailReason != "" && !isDone(kind, it.Status):
		it.Blocked, it.BlockedReason = true, r.FailReason
	}
	switch {
	case it.Blocked:
		it.Bucket = "blocked"
	case it.Status == "rejected":
		it.Bucket = "closed"
	case (it.Stage == "env" && it.StageState == "done") || isDone(kind, it.Status):
		it.Bucket = "released"
	case it.Stage == "build" || it.Stage == "artifact" || it.Stage == "release":
		it.Bucket = "build"
	default:
		it.Bucket = "dev"
	}
}

// MyCounts 是顶部四个计数。
type MyCounts struct {
	InDev    int `json:"in_dev"`
	InBuild  int `json:"in_build"`
	Released int `json:"released"`
	Blocked  int `json:"blocked"`
}

func (c *MyCounts) add(items []*MyItem) {
	for _, it := range items {
		switch it.Bucket {
		case "dev":
			c.InDev++
		case "build":
			c.InBuild++
		case "released":
			c.Released++
		case "blocked":
			c.Blocked++
		}
	}
}

// sortMine 受阻置顶，其余按更新时间倒序。
func sortMine(items []*MyItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Blocked != items[j].Blocked {
			return items[i].Blocked
		}
		return items[i].UpdatedAt.After(items[j].UpdatedAt)
	})
}

func itemType(k string) string {
	if k == "bugs" {
		return "bug"
	}
	return "requirement"
}

func (m *Module) myDelivery(c *gin.Context) {
	uid, me := who(c)
	out := gin.H{"requirements": []*MyItem{}, "bugs": []*MyItem{}, "reviews": []any{}, "counts": MyCounts{}}
	if uid == 0 && me == "" {
		httpx.OK(c, out)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	hasLinks := m.valueStreamLinks(ctx)
	var counts MyCounts
	for _, k := range []string{"requirements", "bugs"} {
		items, datas, err := m.loadMine(ctx, k, uid, me)
		if err != nil {
			fail(c, err)
			return
		}
		var links map[int64][]vsLink
		if hasLinks && len(items) > 0 {
			if links, err = m.loadLinks(ctx, k, items); err != nil {
				fail(c, err)
				return
			}
		}
		for _, it := range items {
			classify(k, it, datas[it.ID], links[it.ID])
		}
		sortMine(items)
		counts.add(items)
		out[k] = items
	}
	out["counts"] = counts
	httpx.OK(c, out)
}

// valueStreamLinks 判断价值流关联表是否就绪（旧版 devcenter_links 没有 item_type 列）。
func (m *Module) valueStreamLinks(ctx context.Context) bool {
	var ok bool
	_ = m.d.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='devcenter_links' AND column_name='item_type')").Scan(&ok)
	return ok
}

func (m *Module) tableExists(ctx context.Context, name string) bool {
	var ok bool
	_ = m.d.DB.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", name).Scan(&ok)
	return ok
}

// loadMine 读取当前用户负责或创建的条目：未结束的全部，已结束的取近 30 天内更新的。
func (m *Module) loadMine(ctx context.Context, k string, uid int64, me string) ([]*MyItem, map[int64][]byte, error) {
	rows, err := m.d.DB.Query(ctx, "SELECT id,title,status,priority,assignee,data,updated_at FROM "+table(k)+
		" WHERE ((assignee<>'' AND assignee=$1) OR (created_by<>0 AND created_by=$2)) AND (status<>ALL($3) OR updated_at>now()-interval '30 days')"+
		" ORDER BY updated_at DESC, id DESC LIMIT 200", me, uid, DoneStatuses[k])
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items, datas := []*MyItem{}, map[int64][]byte{}
	for rows.Next() {
		it := &MyItem{}
		var data []byte
		if err := rows.Scan(&it.ID, &it.Title, &it.Status, &it.Priority, &it.Assignee, &data, &it.UpdatedAt); err != nil {
			return nil, nil, err
		}
		items, datas[it.ID] = append(items, it), data
	}
	return items, datas, rows.Err()
}

// loadLinks 读取条目的价值流关联；运行 / 发布的状态以 cicd 表的最新值为准（表存在时）。
func (m *Module) loadLinks(ctx context.Context, k string, items []*MyItem) (map[int64][]vsLink, error) {
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	rows, err := m.d.DB.Query(ctx, "SELECT item_id,kind,ref,COALESCE(env,''),COALESCE(status,''),COALESCE(occurred_at,created_at) FROM devcenter_links WHERE lower(item_type) IN ($1,$2) AND item_id=ANY($3) AND kind IN ('commit','mr','run','release')",
		itemType(k), itemType(k)+"s", ids)
	if err != nil {
		return nil, err
	}
	out := map[int64][]vsLink{}
	runIDs, relIDs := []int64{}, []int64{}
	for rows.Next() {
		var id int64
		var l vsLink
		var at time.Time
		if err := rows.Scan(&id, &l.Kind, &l.Ref, &l.Env, &l.Status, &at); err != nil {
			rows.Close()
			return nil, err
		}
		l.At = &at
		out[id] = append(out[id], l)
		if n, err := strconv.ParseInt(strings.TrimPrefix(l.Ref, "#"), 10, 64); err == nil {
			if l.Kind == "run" {
				runIDs = append(runIDs, n)
			} else if l.Kind == "release" {
				relIDs = append(relIDs, n)
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	runs, rels := m.cicdStatus(ctx, "cicd_runs", runIDs), m.cicdStatus(ctx, "cicd_releases", relIDs)
	for id, ls := range out {
		for i := range ls {
			n, err := strconv.ParseInt(strings.TrimPrefix(ls[i].Ref, "#"), 10, 64)
			if err != nil {
				continue
			}
			src := runs
			if ls[i].Kind == "release" {
				src = rels
			} else if ls[i].Kind != "run" {
				continue
			}
			if s, ok := src[n]; ok {
				ls[i].Status = s.status
				if s.env != "" {
					ls[i].Env = s.env
				}
				ls[i].Image = s.image
			}
		}
		out[id] = ls
	}
	return out, nil
}

type cicdState struct{ status, env, image string }

// cicdStatus 只读查询运行 / 发布的当前状态；表不存在或查询失败时返回空（退回关联里记录的状态）。
func (m *Module) cicdStatus(ctx context.Context, tbl string, ids []int64) map[int64]cicdState {
	out := map[int64]cicdState{}
	if len(ids) == 0 || !m.tableExists(ctx, tbl) {
		return out
	}
	q := "SELECT id,status,'',image_tag FROM cicd_runs WHERE id=ANY($1)"
	if tbl == "cicd_releases" {
		q = "SELECT id,status,env,image FROM cicd_releases WHERE id=ANY($1)"
	}
	rows, err := m.d.DB.Query(ctx, q, ids)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var s cicdState
		if rows.Scan(&id, &s.status, &s.env, &s.image) == nil {
			out[id] = s
		}
	}
	return out
}
