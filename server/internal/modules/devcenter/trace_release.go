package devcenter

// GET /devcenter/releases/:release_id/items：某次发布包含了哪些需求 / 缺陷 / 提交，并生成中文发布说明。
// 范围：同应用同环境上一次成功发布（不含）到本次发布（含）之间的构建；构建关联的、以及这些构建的提交关联的需求与缺陷，
// 再加上直接关联到本次发布的条目，以及提交说明中直接写了编号（且条目存在）的条目。

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// traceRelease 是发布的基本信息（cicd_releases + 应用名）。
type traceRelease struct {
	ID        int64     `json:"id"`
	AppID     int64     `json:"app_id"`
	AppName   string    `json:"app_name"`
	Env       string    `json:"env"`
	Image     string    `json:"image"`
	Status    string    `json:"status"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
	// PreviousReleaseID 是对比基线（同应用同环境上一次成功发布），首次发布为 nil。
	PreviousReleaseID *int64 `json:"previous_release_id"`
	runID             *int64
}

type traceRelItem struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

type traceRelCommit struct {
	SHA    string `json:"sha"`
	Title  string `json:"title"`
	Author string `json:"author"`
}

var traceStatusLabel = map[string]map[string]string{
	"requirement": {"todo": "待处理", "ready": "就绪", "doing": "开发中", "testing": "测试中", "accepting": "验收中", "done": "已完成", "rejected": "已拒绝"},
	"bug":         {"todo": "待处理", "doing": "处理中", "fixed": "已修复", "verified": "已验证", "closed": "已关闭", "reopened": "重新打开"},
}

func traceOneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

func traceShortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

var traceNotesZone = func() *time.Location {
	if z, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return z
	}
	return time.FixedZone("CST", 8*3600)
}()

// buildReleaseNotes 生成中文发布说明（Markdown），分组：新功能 / 缺陷修复 / 其他提交；空分组省略。
// others 为未关联任何需求 / 缺陷的提交。
func buildReleaseNotes(rel traceRelease, reqs, bugs []traceRelItem, others []traceRelCommit) string {
	var b strings.Builder
	head := strings.TrimSpace(rel.AppName + " " + rel.Env)
	if head == "" {
		head = "发布 #" + strconv.FormatInt(rel.ID, 10)
	}
	b.WriteString("## " + head + " 发布说明\n\n")
	b.WriteString("- 发布编号：#" + strconv.FormatInt(rel.ID, 10) + "\n")
	if !rel.CreatedAt.IsZero() {
		b.WriteString("- 发布时间：" + rel.CreatedAt.In(traceNotesZone).Format("2006-01-02 15:04") + "\n")
	}
	if rel.Image != "" {
		b.WriteString("- 镜像：`" + rel.Image + "`\n")
	}
	if rel.Kind == "rollback" {
		b.WriteString("- 类型：回滚\n")
	}
	if rel.PreviousReleaseID != nil {
		b.WriteString("- 对比基线：上一次成功发布 #" + strconv.FormatInt(*rel.PreviousReleaseID, 10) + "\n")
	} else {
		b.WriteString("- 对比基线：该环境首次成功发布\n")
	}
	section := func(title, typ string, items []traceRelItem) {
		if len(items) == 0 {
			return
		}
		b.WriteString("\n### " + title + "\n\n")
		for _, it := range items {
			line := "- " + ItemKey{Type: typ, ID: it.ID}.String() + " " + traceOneLine(it.Title)
			if l := traceStatusLabel[typ][it.Status]; l != "" {
				line += "（" + l + "）"
			}
			b.WriteString(line + "\n")
		}
	}
	section("新功能", "requirement", reqs)
	section("缺陷修复", "bug", bugs)
	if len(others) > 0 {
		b.WriteString("\n### 其他提交\n\n")
		for _, c := range others {
			line := "- `" + traceShortSHA(c.SHA) + "`"
			if t := traceOneLine(c.Title); t != "" {
				line += " " + t
			} else {
				line += " （未记录提交说明）"
			}
			if c.Author != "" {
				line += " — " + traceOneLine(c.Author)
			}
			b.WriteString(line + "\n")
		}
	}
	if len(reqs) == 0 && len(bugs) == 0 && len(others) == 0 {
		b.WriteString("\n本次发布没有识别到关联的需求、缺陷或提交记录。\n")
	}
	return b.String()
}

/* ------------------------------------------------------------------ 查询 */

func (m *Module) loadTraceRelease(ctx context.Context, id int64, has map[string]bool) (*traceRelease, error) {
	aname, ajoin := "''", ""
	if has["cicd_applications"] {
		aname, ajoin = "COALESCE(a.name,'')", " LEFT JOIN cicd_applications a ON a.id=r.app_id"
	}
	var r traceRelease
	err := m.d.DB.QueryRow(ctx, `SELECT r.id, r.app_id, `+aname+`, r.env, r.image, r.status, r.kind, r.created_at, r.run_id FROM cicd_releases r`+ajoin+` WHERE r.id=$1`, id).
		Scan(&r.ID, &r.AppID, &r.AppName, &r.Env, &r.Image, &r.Status, &r.Kind, &r.CreatedAt, &r.runID)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// releaseRunID 返回发布对应的构建：优先 run_id，否则按镜像标签匹配同应用最近一次构建（回滚不匹配）。
func (m *Module) releaseRunID(ctx context.Context, appID int64, runID *int64, image, kind string) *int64 {
	if runID != nil {
		return runID
	}
	if kind == "rollback" || image == "" {
		return nil
	}
	var id int64
	if m.d.DB.QueryRow(ctx, `SELECT id FROM cicd_runs WHERE app_id=$1 AND image_tag<>'' AND $2 LIKE '%:' || image_tag ORDER BY id DESC LIMIT 1`, appID, image).Scan(&id) != nil {
		return nil
	}
	return &id
}

func (m *Module) releaseItems(c *gin.Context) {
	id, ok := httpx.ParamID(c, "release_id")
	if !ok {
		return
	}
	has := m.traceHas(c, "cicd_releases", "cicd_runs", "cicd_applications")
	if !has["cicd_releases"] {
		httpx.Fail(c, httpx.NotFound("发布记录不存在"))
		return
	}
	rel, err := m.loadTraceRelease(c, id, has)
	if err != nil {
		fail(c, err)
		return
	}
	// 对比基线：同应用同环境、本次之前最近一次成功发布。
	var prevID int64
	var prevRun *int64
	var prevImage, prevKind string
	var prevAt time.Time
	err = m.d.DB.QueryRow(c, `SELECT id, run_id, image, kind, created_at FROM cicd_releases WHERE app_id=$1 AND env=$2 AND id<$3 AND status='succeeded' ORDER BY id DESC LIMIT 1`, rel.AppID, rel.Env, rel.ID).
		Scan(&prevID, &prevRun, &prevImage, &prevKind, &prevAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		fail(c, err)
		return
	}
	hasPrev := err == nil
	if hasPrev {
		rel.PreviousReleaseID = &prevID
	}

	var runIDs []int64
	var commits []traceRelCommit
	if has["cicd_runs"] {
		curRun := m.releaseRunID(c, rel.AppID, rel.runID, rel.Image, rel.Kind)
		if curRun != nil {
			var lower int64
			var after *time.Time
			if hasPrev {
				if pr := m.releaseRunID(c, rel.AppID, prevRun, prevImage, prevKind); pr != nil {
					lower = *pr
				} else {
					after = &prevAt
				}
			}
			runIDs, commits, err = m.releaseRuns(c, rel.AppID, *curRun, lower, after)
			if err != nil {
				fail(c, err)
				return
			}
		}
	}

	// 条目：关联表 + 提交说明里直接写的编号。
	keys := map[ItemKey]bool{}
	linkedSHA := map[string]bool{}
	if m.traceTablesReady(c) {
		runRefs := make([]string, 0, len(runIDs))
		for _, r := range runIDs {
			runRefs = append(runRefs, strconv.FormatInt(r, 10))
		}
		shas := make([]string, 0, len(commits))
		for _, cm := range commits {
			shas = append(shas, cm.SHA)
		}
		rows, err := m.d.DB.Query(c, `SELECT l.item_type, l.item_id, l.kind, l.ref, l.title FROM devcenter_links l
			WHERE (l.kind='run' AND l.ref=ANY($1))
			   OR (l.kind='release' AND l.ref=$2)
			   OR (l.kind='commit' AND length(l.ref) >= 7 AND EXISTS (SELECT 1 FROM unnest($3::text[]) s WHERE lower(left(s, length(l.ref)))=lower(l.ref)))
			LIMIT 2000`, runRefs, strconv.FormatInt(rel.ID, 10), shas)
		if err != nil {
			fail(c, err)
			return
		}
		titles := map[string]string{}
		for rows.Next() {
			var typ, kind, ref, title string
			var iid int64
			if rows.Scan(&typ, &iid, &kind, &ref, &title) != nil {
				continue
			}
			keys[ItemKey{Type: typ, ID: iid}] = true
			if kind == "commit" {
				ref = strings.ToLower(ref)
				for _, s := range shas {
					if strings.HasPrefix(strings.ToLower(s), ref) {
						linkedSHA[s] = true
						if title != "" {
							titles[s] = title
						}
					}
				}
			}
		}
		rows.Close()
		for i := range commits {
			if commits[i].Title == "" {
				commits[i].Title = titles[commits[i].SHA]
			}
		}
	}
	for _, cm := range commits {
		for _, k := range ParseItemKeys(cm.Title) {
			keys[k] = true
			linkedSHA[cm.SHA] = true
		}
	}
	reqs, bugs, err := m.loadRelItems(c, keys)
	if err != nil {
		fail(c, err)
		return
	}
	var others []traceRelCommit
	for _, cm := range commits {
		if !linkedSHA[cm.SHA] {
			others = append(others, cm)
		}
	}
	if commits == nil {
		commits = []traceRelCommit{}
	}
	httpx.OK(c, gin.H{
		"release":      rel,
		"requirements": reqs,
		"bugs":         bugs,
		"commits":      commits,
		"notes_md":     buildReleaseNotes(*rel, reqs, bugs, others),
	})
}

// releaseRuns 返回范围内（lower < id <= cur，或 created_at > after）的构建编号与去重后的提交，最多 200 个构建。
func (m *Module) releaseRuns(ctx context.Context, appID, cur, lower int64, after *time.Time) ([]int64, []traceRelCommit, error) {
	commitsCol := "'[]'::jsonb"
	if m.traceHasColumn(ctx, "cicd_runs", "commits") {
		commitsCol = "COALESCE(commits,'[]'::jsonb)"
	}
	rows, err := m.d.DB.Query(ctx, `SELECT id, commit_sha, triggered_by, `+commitsCol+` FROM cicd_runs
		WHERE app_id=$1 AND id<=$2 AND id>$3 AND ($4::timestamptz IS NULL OR created_at>$4 OR id=$2)
		ORDER BY id DESC LIMIT 200`, appID, cur, lower, after)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var ids []int64
	var out []traceRelCommit
	seen := map[string]int{}
	add := func(c traceRelCommit) {
		if c.SHA == "" {
			return
		}
		if i, ok := seen[c.SHA]; ok {
			if out[i].Title == "" {
				out[i].Title = c.Title
			}
			if out[i].Author == "" {
				out[i].Author = c.Author
			}
			return
		}
		seen[c.SHA] = len(out)
		out = append(out, c)
	}
	for rows.Next() {
		var id int64
		var sha, by string
		var raw []byte
		if err := rows.Scan(&id, &sha, &by, &raw); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		cs := parseTraceCommits(raw)
		for _, c := range cs {
			add(traceRelCommit{SHA: c.sha(), Title: traceFirstLine(c.Message), Author: c.Author})
		}
		add(traceRelCommit{SHA: strings.ToLower(sha), Author: by})
		if len(out) >= 500 {
			break
		}
	}
	return ids, out, rows.Err()
}

func (m *Module) loadRelItems(ctx context.Context, keys map[ItemKey]bool) ([]traceRelItem, []traceRelItem, error) {
	byType := map[string][]int64{}
	for k := range keys {
		byType[k.Type] = append(byType[k.Type], k.ID)
	}
	load := func(typ string) ([]traceRelItem, error) {
		out := []traceRelItem{}
		ids := byType[typ]
		if len(ids) == 0 {
			return out, nil
		}
		rows, err := m.d.DB.Query(ctx, "SELECT id,title,status FROM "+table(ItemKey{Type: typ}.Kind())+" WHERE id=ANY($1) ORDER BY id", ids)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var it traceRelItem
			if err := rows.Scan(&it.ID, &it.Title, &it.Status); err != nil {
				return nil, err
			}
			out = append(out, it)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out, rows.Err()
	}
	reqs, err := load("requirement")
	if err != nil {
		return nil, nil, err
	}
	bugs, err := load("bug")
	return reqs, bugs, err
}
