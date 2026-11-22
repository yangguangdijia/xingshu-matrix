package devcenter

// 价值流自动关联同步（source=auto，幂等 upsert）。只读 cicd_* 表，只写 devcenter_links。
//
// 数据来源（截至 v1.32 基线）：
//   - cicd_runs.branch       分支名中的 REQ-<id> / BUG-<id>（如 feature/REQ-12-login）→ run + commit 关联；
//   - cicd_webhook_deliveries.ref + commit_sha  推送到任意分支的记录（含未触发构建的特性分支推送，保留 7 天）→ commit 关联；
//   - commits JSONB 列（cicd_runs / cicd_webhook_deliveries，若存在）  提交说明中的编号 → commit + run 关联。
//     基线的 cicd 模块不保存提交说明与合并请求标题；见 /workspace/work/briefs/v132/vs-trace.cicd-commits.patch 的建议补丁。
//   - 传递：commit 关联 → 同 sha 的构建（run）；run 关联 → 由该构建发布（或镜像标签一致）的 cicd_releases（release + env）。
// 触发：GET .../trace 惰性触发（单条目，60 秒节流）；后台每 5 分钟全量扫描最近 90 天（Start 启动，随 ctx 退出）。

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
)

const (
	traceSweepEvery    = 5 * time.Minute
	traceSweepTimeout  = 90 * time.Second
	traceSweepDelay    = 30 * time.Second
	traceLazyInterval  = 60 * time.Second
	traceLookbackDays  = 90
	traceSweepRunLimit = 3000
	traceItemRunLimit  = 500
)

/* ------------------------------------------------------------------ 节流 */

type traceSyncGuard struct {
	mu   sync.Mutex
	last map[string]time.Time
}

var traceGuard = &traceSyncGuard{last: map[string]time.Time{}}

func traceGuardKey(itemType string, id int64) string {
	return itemType + ":" + strconv.FormatInt(id, 10)
}

// allow 返回该条目本次是否需要同步（距上次超过 traceLazyInterval），并记下时间。
func (g *traceSyncGuard) allow(itemType string, id int64, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := traceGuardKey(itemType, id)
	if t, ok := g.last[k]; ok && now.Sub(t) < traceLazyInterval {
		return false
	}
	if len(g.last) > 5000 { // 简单限长：过期项清理
		for kk, t := range g.last {
			if now.Sub(t) >= traceLazyInterval {
				delete(g.last, kk)
			}
		}
	}
	g.last[k] = now
	return true
}

func (g *traceSyncGuard) reset(itemType string, id int64) {
	g.mu.Lock()
	delete(g.last, traceGuardKey(itemType, id))
	g.mu.Unlock()
}

/* ------------------------------------------------------------------ 纯逻辑：从 cicd 记录生成关联 */

type traceCommit struct {
	SHA     string `json:"sha"`
	ID      string `json:"id"` // GitHub/GitLab 推送载荷里提交 sha 的字段名
	Message string `json:"message"`
	Author  string `json:"author"`
	URL     string `json:"url"`
}

func (c traceCommit) sha() string {
	if c.SHA != "" {
		return strings.ToLower(c.SHA)
	}
	return strings.ToLower(c.ID)
}

// parseTraceCommits 解析 commits JSONB；author 兼容字符串与 {name} 对象。
func parseTraceCommits(raw []byte) []traceCommit {
	if len(raw) == 0 {
		return nil
	}
	var items []map[string]any
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := make([]traceCommit, 0, len(items))
	for _, it := range items {
		var c traceCommit
		c.SHA, _ = it["sha"].(string)
		c.ID, _ = it["id"].(string)
		c.Message, _ = it["message"].(string)
		c.URL, _ = it["url"].(string)
		switch a := it["author"].(type) {
		case string:
			c.Author = a
		case map[string]any:
			if n, ok := a["name"].(string); ok {
				c.Author = n
			} else if n, ok := a["username"].(string); ok {
				c.Author = n
			}
		}
		if c.sha() != "" || c.Message != "" {
			out = append(out, c)
		}
	}
	return out
}

// traceFirstLine 取提交说明首行（最多 200 字）。
func traceFirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	return s
}

type traceRun struct {
	ID       int64
	AppID    int64
	Status   string
	Branch   string
	SHA      string
	By       string
	Pipeline string
	At       time.Time
	Commits  []traceCommit
}

type traceDelivery struct {
	AppID   int64
	Ref     string
	SHA     string
	At      time.Time
	Commits []traceCommit
}

// pendingTraceLink 是待 upsert 的自动关联。
type pendingTraceLink struct {
	Key    ItemKey
	Kind   string
	Ref    string
	AppID  int64
	Title  string
	URL    string
	Author string
	Env    string
	Status string
	At     time.Time
}

func traceRunTitle(r traceRun) string {
	if r.Pipeline != "" {
		return r.Pipeline + " #" + strconv.FormatInt(r.ID, 10)
	}
	return "构建 #" + strconv.FormatInt(r.ID, 10)
}

// linksFromRun：分支名里的编号 → 构建 + 头提交；提交说明里的编号 → 该提交 + 构建。
func linksFromRun(r traceRun) []pendingTraceLink {
	var out []pendingTraceLink
	runRef := strconv.FormatInt(r.ID, 10)
	head := strings.ToLower(r.SHA)
	headTitle := ""
	for _, c := range r.Commits {
		if c.sha() == head {
			headTitle = traceFirstLine(c.Message)
		}
	}
	addRun := func(k ItemKey) {
		out = append(out, pendingTraceLink{Key: k, Kind: "run", Ref: runRef, AppID: r.AppID, Title: traceRunTitle(r), Author: r.By, Status: r.Status, At: r.At})
	}
	for _, k := range ParseItemKeys(r.Branch) {
		addRun(k)
		if head != "" {
			out = append(out, pendingTraceLink{Key: k, Kind: "commit", Ref: head, AppID: r.AppID, Title: headTitle, Author: r.By, At: r.At})
		}
	}
	for _, c := range r.Commits {
		keys := ParseItemKeys(c.Message)
		for _, k := range keys {
			if s := c.sha(); s != "" {
				out = append(out, pendingTraceLink{Key: k, Kind: "commit", Ref: s, AppID: r.AppID, Title: traceFirstLine(c.Message), URL: c.URL, Author: c.Author, At: r.At})
			}
			addRun(k)
		}
	}
	return dedupePending(out)
}

// linksFromDelivery：推送的分支名或提交说明里的编号 → 提交关联（不要求触发了构建）。
func linksFromDelivery(d traceDelivery) []pendingTraceLink {
	var out []pendingTraceLink
	branch := strings.TrimPrefix(d.Ref, "refs/heads/")
	head := strings.ToLower(d.SHA)
	if head != "" && strings.Trim(head, "0") != "" {
		title := ""
		for _, c := range d.Commits {
			if c.sha() == head {
				title = traceFirstLine(c.Message)
			}
		}
		for _, k := range ParseItemKeys(branch) {
			out = append(out, pendingTraceLink{Key: k, Kind: "commit", Ref: head, AppID: d.AppID, Title: title, At: d.At})
		}
	}
	for _, c := range d.Commits {
		if s := c.sha(); s != "" {
			for _, k := range ParseItemKeys(c.Message) {
				out = append(out, pendingTraceLink{Key: k, Kind: "commit", Ref: s, AppID: d.AppID, Title: traceFirstLine(c.Message), URL: c.URL, Author: c.Author, At: d.At})
			}
		}
	}
	return dedupePending(out)
}

// dedupePending 按 (条目, kind, ref) 去重，后出现的非空字段补全先出现的。
func dedupePending(in []pendingTraceLink) []pendingTraceLink {
	type key struct {
		k         ItemKey
		kind, ref string
	}
	idx := map[key]int{}
	var out []pendingTraceLink
	for _, p := range in {
		kk := key{p.Key, p.Kind, p.Ref}
		if i, ok := idx[kk]; ok {
			if out[i].Title == "" {
				out[i].Title = p.Title
			}
			if out[i].URL == "" {
				out[i].URL = p.URL
			}
			if out[i].Author == "" {
				out[i].Author = p.Author
			}
			continue
		}
		idx[kk] = len(out)
		out = append(out, p)
	}
	return out
}

/* ------------------------------------------------------------------ 同步 */

// Start 启动价值流后台同步（app.Starter）。ctx 在服务关闭时取消，扫描随之退出。
func (m *Module) Start(ctx context.Context, d *app.Deps) error {
	if m.d == nil {
		m.d = d
	}
	go m.traceSweepLoop(ctx)
	return nil
}

func (m *Module) traceSweepLoop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(traceSweepDelay):
	}
	t := time.NewTicker(traceSweepEvery)
	defer t.Stop()
	for {
		sctx, cancel := context.WithTimeout(ctx, traceSweepTimeout)
		if err := m.syncTraceLinks(sctx, nil); err != nil && ctx.Err() == nil && m.d.Logger != nil {
			m.d.Logger.Warn("devcenter trace sweep failed", "err", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// syncTraceLinks 同步自动关联。scope 为 nil 时扫描最近 90 天全部数据，否则只处理该条目。
func (m *Module) syncTraceLinks(ctx context.Context, scope *ItemKey) error {
	if m.d == nil || m.d.DB == nil || !m.traceTablesReady(ctx) {
		return nil
	}
	has := m.traceHas(ctx, "cicd_runs", "cicd_releases", "cicd_applications", "cicd_pipelines", "cicd_webhook_deliveries")
	if !has["cicd_runs"] {
		return nil
	}
	pattern := ""
	limit := traceSweepRunLimit
	if scope != nil {
		pattern, limit = scope.String(), traceItemRunLimit
	}
	var pending []pendingTraceLink
	runs, err := m.loadTraceRuns(ctx, has, pattern, limit)
	if err != nil {
		return err
	}
	for _, r := range runs {
		pending = append(pending, linksFromRun(r)...)
	}
	if has["cicd_webhook_deliveries"] {
		ds, err := m.loadTraceDeliveries(ctx, pattern, limit)
		if err != nil {
			return err
		}
		for _, d := range ds {
			pending = append(pending, linksFromDelivery(d)...)
		}
	}
	pending = dedupePending(pending)
	if scope != nil {
		kept := pending[:0]
		for _, p := range pending {
			if p.Key == *scope {
				kept = append(kept, p)
			}
		}
		pending = kept
	}
	pending, err = m.filterExistingItems(ctx, pending)
	if err != nil {
		return err
	}
	if err := m.upsertPending(ctx, pending); err != nil {
		return err
	}
	return m.propagateTrace(ctx, has, scope)
}

func (m *Module) loadTraceRuns(ctx context.Context, has map[string]bool, pattern string, limit int) ([]traceRun, error) {
	commits := "'[]'::jsonb"
	if m.traceHasColumn(ctx, "cicd_runs", "commits") {
		commits = "COALESCE(r.commits,'[]'::jsonb)"
	}
	pipe, join := "''", ""
	if has["cicd_pipelines"] {
		pipe, join = "COALESCE(p.name,'')", " LEFT JOIN cicd_pipelines p ON p.id=r.pipeline_id"
	}
	q := `SELECT r.id, r.app_id, r.status, r.branch, r.commit_sha, r.triggered_by, ` + pipe + `, COALESCE(r.finished_at, r.started_at, r.created_at), ` + commits + `
		FROM cicd_runs r` + join + `
		WHERE r.created_at > now() - make_interval(days => $1)
		  AND ($2 = '' OR r.branch ILIKE '%' || $2 || '%' OR ` + commits + `::text ILIKE '%' || $2 || '%')
		ORDER BY r.id DESC LIMIT $3`
	rows, err := m.d.DB.Query(ctx, q, traceLookbackDays, pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []traceRun
	for rows.Next() {
		var r traceRun
		var raw []byte
		if err := rows.Scan(&r.ID, &r.AppID, &r.Status, &r.Branch, &r.SHA, &r.By, &r.Pipeline, &r.At, &raw); err != nil {
			return nil, err
		}
		r.Commits = parseTraceCommits(raw)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (m *Module) loadTraceDeliveries(ctx context.Context, pattern string, limit int) ([]traceDelivery, error) {
	commits := "'[]'::jsonb"
	if m.traceHasColumn(ctx, "cicd_webhook_deliveries", "commits") {
		commits = "COALESCE(commits,'[]'::jsonb)"
	}
	q := `SELECT app_id, ref, commit_sha, received_at, ` + commits + ` FROM cicd_webhook_deliveries
		WHERE commit_sha <> '' AND received_at > now() - make_interval(days => $1)
		  AND ($2 = '' OR ref ILIKE '%' || $2 || '%' OR ` + commits + `::text ILIKE '%' || $2 || '%')
		ORDER BY id DESC LIMIT $3`
	rows, err := m.d.DB.Query(ctx, q, traceLookbackDays, pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []traceDelivery
	for rows.Next() {
		var d traceDelivery
		var raw []byte
		if err := rows.Scan(&d.AppID, &d.Ref, &d.SHA, &d.At, &raw); err != nil {
			return nil, err
		}
		d.Commits = parseTraceCommits(raw)
		out = append(out, d)
	}
	return out, rows.Err()
}

// filterExistingItems 丢弃指向不存在需求 / 缺陷的编号（提交说明里可能写错）。
func (m *Module) filterExistingItems(ctx context.Context, in []pendingTraceLink) ([]pendingTraceLink, error) {
	if len(in) == 0 {
		return nil, nil
	}
	ids := map[string][]int64{}
	for _, p := range in {
		ids[p.Key.Type] = append(ids[p.Key.Type], p.Key.ID)
	}
	exists := map[ItemKey]bool{}
	for typ, list := range ids {
		rows, err := m.d.DB.Query(ctx, "SELECT id FROM "+table(ItemKey{Type: typ}.Kind())+" WHERE id=ANY($1)", list)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				exists[ItemKey{Type: typ, ID: id}] = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	out := in[:0]
	for _, p := range in {
		if exists[p.Key] {
			out = append(out, p)
		}
	}
	return out, nil
}

const traceUpsertSQL = `INSERT INTO devcenter_links(item_type,item_id,kind,ref,app_id,title,url,author,env,status,occurred_at,source)
	VALUES($1,$2,$3,$4,NULLIF($5,0),$6,$7,$8,$9,$10,$11,'auto')
	ON CONFLICT (item_type,item_id,kind,ref) DO UPDATE SET
		status      = CASE WHEN EXCLUDED.status <> '' THEN EXCLUDED.status ELSE devcenter_links.status END,
		title       = CASE WHEN devcenter_links.title = '' THEN EXCLUDED.title ELSE devcenter_links.title END,
		url         = CASE WHEN devcenter_links.url = '' THEN EXCLUDED.url ELSE devcenter_links.url END,
		author      = CASE WHEN devcenter_links.author = '' THEN EXCLUDED.author ELSE devcenter_links.author END,
		env         = CASE WHEN devcenter_links.env = '' THEN EXCLUDED.env ELSE devcenter_links.env END,
		app_id      = COALESCE(devcenter_links.app_id, EXCLUDED.app_id),
		occurred_at = CASE WHEN devcenter_links.kind IN ('run','release') THEN EXCLUDED.occurred_at
		                   ELSE COALESCE(devcenter_links.occurred_at, EXCLUDED.occurred_at) END
	WHERE (devcenter_links.status, devcenter_links.title, devcenter_links.url, devcenter_links.author, devcenter_links.env,
	       devcenter_links.app_id, devcenter_links.occurred_at) IS DISTINCT FROM
	      (CASE WHEN EXCLUDED.status <> '' THEN EXCLUDED.status ELSE devcenter_links.status END,
	       CASE WHEN devcenter_links.title = '' THEN EXCLUDED.title ELSE devcenter_links.title END,
	       CASE WHEN devcenter_links.url = '' THEN EXCLUDED.url ELSE devcenter_links.url END,
	       CASE WHEN devcenter_links.author = '' THEN EXCLUDED.author ELSE devcenter_links.author END,
	       CASE WHEN devcenter_links.env = '' THEN EXCLUDED.env ELSE devcenter_links.env END,
	       COALESCE(devcenter_links.app_id, EXCLUDED.app_id),
	       CASE WHEN devcenter_links.kind IN ('run','release') THEN EXCLUDED.occurred_at
	            ELSE COALESCE(devcenter_links.occurred_at, EXCLUDED.occurred_at) END)`

func (m *Module) upsertPending(ctx context.Context, in []pendingTraceLink) error {
	for start := 0; start < len(in); start += 200 {
		end := min(start+200, len(in))
		b := &pgx.Batch{}
		for _, p := range in[start:end] {
			b.Queue(traceUpsertSQL, p.Key.Type, p.Key.ID, p.Kind, p.Ref, p.AppID, p.Title, p.URL, p.Author, p.Env, p.Status, p.At)
		}
		if err := m.d.DB.SendBatch(ctx, b).Close(); err != nil {
			return err
		}
	}
	return nil
}

// propagateTrace 做传递关联与状态刷新（均为集合 SQL，幂等）：
//  1. run 关联的状态 / 时间跟随 cicd_runs；
//  2. commit 关联 → 同 sha（前缀匹配，至少 7 位）的构建；
//  3. run 关联 → 由该构建发布、或镜像标签一致的发布（release + env）；
//  4. release 关联的状态 / 环境 / 时间跟随 cicd_releases。
func (m *Module) propagateTrace(ctx context.Context, has map[string]bool, scope *ItemKey) error {
	typ, id := "", int64(0)
	if scope != nil {
		typ, id = scope.Type, scope.ID
	}
	// 全量扫描只处理最近 180 天建立的关联，控制开销。
	scopeSQL := ` AND ($1 = '' OR l.item_type = $1) AND ($2::bigint = 0 OR l.item_id = $2) AND ($1 <> '' OR l.created_at > now() - interval '180 days')`
	pipe, pjoin := "''", ""
	if has["cicd_pipelines"] {
		pipe, pjoin = "COALESCE(p.name,'')", " LEFT JOIN cicd_pipelines p ON p.id=r.pipeline_id"
	}
	stmts := []string{
		`UPDATE devcenter_links l SET status=r.status, app_id=COALESCE(l.app_id, r.app_id), occurred_at=COALESCE(r.finished_at, r.started_at, r.created_at)
			FROM cicd_runs r WHERE l.kind='run' AND r.id = CASE WHEN l.ref ~ '^[0-9]{1,18}$' THEN l.ref::bigint END
			AND (l.status IS DISTINCT FROM r.status OR l.occurred_at IS DISTINCT FROM COALESCE(r.finished_at, r.started_at, r.created_at))` + scopeSQL,
		`INSERT INTO devcenter_links(item_type,item_id,kind,ref,app_id,title,author,status,occurred_at,source)
			SELECT DISTINCT ON (l.item_type, l.item_id, r.id) l.item_type, l.item_id, 'run', r.id::text, r.app_id,
				CASE WHEN ` + pipe + ` <> '' THEN ` + pipe + ` || ' #' || r.id ELSE '构建 #' || r.id END,
				r.triggered_by, r.status, COALESCE(r.finished_at, r.started_at, r.created_at), 'auto'
			FROM devcenter_links l JOIN cicd_runs r ON l.kind='commit' AND length(l.ref) >= 7 AND r.commit_sha <> ''
				AND lower(left(r.commit_sha, length(l.ref))) = lower(l.ref) AND (l.app_id IS NULL OR l.app_id = r.app_id)` + pjoin + `
			WHERE true` + scopeSQL + `
			ON CONFLICT (item_type,item_id,kind,ref) DO NOTHING`,
	}
	if has["cicd_releases"] {
		aname, ajoin := "''", ""
		if has["cicd_applications"] {
			aname, ajoin = "COALESCE(a.name,'')", " LEFT JOIN cicd_applications a ON a.id=rel.app_id"
		}
		stmts = append(stmts,
			`INSERT INTO devcenter_links(item_type,item_id,kind,ref,app_id,title,author,env,status,occurred_at,source)
			SELECT DISTINCT ON (l.item_type, l.item_id, rel.id) l.item_type, l.item_id, 'release', rel.id::text, rel.app_id,
				CASE WHEN `+aname+` <> '' THEN `+aname+` || ' → ' || rel.env ELSE rel.image END,
				rel.created_by, rel.env, rel.status, COALESCE(rel.finished_at, rel.created_at), 'auto'
			FROM devcenter_links l
			JOIN cicd_runs r ON l.kind='run' AND r.id = CASE WHEN l.ref ~ '^[0-9]{1,18}$' THEN l.ref::bigint END
			JOIN cicd_releases rel ON rel.app_id=r.app_id AND (rel.run_id=r.id OR (rel.run_id IS NULL AND r.image_tag <> '' AND rel.image LIKE '%:' || r.image_tag))`+ajoin+`
			WHERE true`+scopeSQL+`
			ON CONFLICT (item_type,item_id,kind,ref) DO NOTHING`,
			`UPDATE devcenter_links l SET status=rel.status, env=rel.env, app_id=COALESCE(l.app_id, rel.app_id), occurred_at=COALESCE(rel.finished_at, rel.created_at)
			FROM cicd_releases rel WHERE l.kind='release' AND rel.id = CASE WHEN l.ref ~ '^[0-9]{1,18}$' THEN l.ref::bigint END
			AND (l.status IS DISTINCT FROM rel.status OR l.env IS DISTINCT FROM rel.env OR l.occurred_at IS DISTINCT FROM COALESCE(rel.finished_at, rel.created_at))`+scopeSQL,
		)
	}
	for _, q := range stmts {
		if _, err := m.d.DB.Exec(ctx, q, typ, id); err != nil {
			return err
		}
	}
	return nil
}
