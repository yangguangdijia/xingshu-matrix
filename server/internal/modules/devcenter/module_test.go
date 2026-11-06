package devcenter

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

func TestInputCheck(t *testing.T) {
	s := func(v string) *string { return &v }
	in := input{Title: s("  登录支持企业 SSO  "), Status: s("accepting"), Priority: s("P1"), Assignee: s(" alice ")}
	if err := in.check("requirements", true); err != nil || *in.Title != "登录支持企业 SSO" || *in.Assignee != "alice" {
		t.Fatalf("valid input rejected: %v", err)
	}
	if (&input{Title: s("x"), Status: s("accepting")}).check("bugs", true) == nil {
		t.Fatal("requirement status must be invalid for bugs")
	}
	if (&input{Title: s("x"), Priority: s("P9")}).check("bugs", true) == nil {
		t.Fatal("bad priority must be rejected")
	}
	if (&input{}).check("tests", true) == nil {
		t.Fatal("title required on create")
	}
	if (&input{Status: s("passed")}).check("tests", false) != nil {
		t.Fatal("patch without title must be allowed")
	}
	if (&input{Title: s("   ")}).check("tests", false) == nil {
		t.Fatal("patch must not blank the title")
	}
	if (&input{Title: s("x"), Data: []byte(`[1]`)}).check("tests", true) == nil {
		t.Fatal("data must be an object")
	}
	if (&input{Title: s("x"), Data: []byte(`{"type":"epic"}`)}).check("requirements", true) == nil {
		t.Fatal("unknown item type must be rejected")
	}
	if (&input{Title: s("x"), Data: []byte(`{"type":"task"}`)}).check("requirements", true) != nil {
		t.Fatal("task type must be accepted")
	}
}

func TestBuildWhere(t *testing.T) {
	if w, a := buildWhere("requirements", listQuery{}); w != "" || len(a) != 0 {
		t.Fatalf("empty query must have no where: %q %v", w, a)
	}
	it, pid := int64(3), int64(9)
	w, a := buildWhere("requirements", listQuery{Status: "todo,doing", Assignee: "alice", Iteration: &it, Q: "sso", Type: "task", Parent: &pid, Involved: "bob"})
	for _, frag := range []string{"t.status=ANY", "t.assignee=$2", "t.iteration_id=$3", "ILIKE", "data->>'type'", "data->>'parent_id'=$6", "devcenter_activities", "devcenter_comments"} {
		if !strings.Contains(w, frag) {
			t.Fatalf("where %q misses %q", w, frag)
		}
	}
	if len(a) != 8 || a[5] != "9" || a[7] != "requirements" {
		t.Fatalf("unexpected args %v", a)
	}
	// 用户输入只出现在参数里
	w2, _ := buildWhere("bugs", listQuery{Q: "'; DROP TABLE x; --", Type: "task"})
	if strings.Contains(w2, "DROP") || strings.Contains(w2, "data->>'type'") {
		t.Fatalf("unsafe or misplaced where: %q", w2)
	}
}

func TestDiffActivities(t *testing.T) {
	i1, i2 := int64(1), int64(2)
	cur := &Item{Status: "todo", Assignee: "alice"}
	if a := diffActivities(nil, cur); len(a) != 2 || a[0].Action != "create" || a[1].Action != "assign" {
		t.Fatalf("create activities: %+v", a)
	}
	old := &Item{Status: "todo", Assignee: "alice", IterationID: &i1}
	nw := &Item{Status: "doing", Assignee: "bob", IterationID: &i2}
	a := diffActivities(old, nw)
	if len(a) != 3 || a[0].From != "todo" || a[0].To != "doing" || a[1].To != "bob" || a[2].From != "1" || a[2].To != "2" {
		t.Fatalf("diff: %+v", a)
	}
	if len(diffActivities(old, old)) != 0 {
		t.Fatal("no change, no activity")
	}
	if a := diffActivities(old, &Item{Status: "todo", Assignee: "alice"}); len(a) != 1 || a[0].Action != "iteration" || a[0].To != "" {
		t.Fatalf("leaving iteration: %+v", a)
	}
}

func TestComputeReport(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	day := func(d, h int) time.Time { return time.Date(2026, 11, d, h, 0, 0, 0, loc) }
	items := []flowItem{
		{Kind: "requirements", ID: 1, Status: "done", Assignee: "alice", CreatedAt: day(2, 9), UpdatedAt: day(5, 9)},
		{Kind: "requirements", ID: 2, Status: "doing", Assignee: "alice", CreatedAt: day(3, 9), UpdatedAt: day(4, 9)},
		{Kind: "requirements", ID: 3, Status: "rejected", CreatedAt: day(3, 9), UpdatedAt: day(4, 9)},
		{Kind: "bugs", ID: 1, Status: "closed", Assignee: "bob", CreatedAt: day(4, 9), UpdatedAt: day(4, 21)},
		{Kind: "bugs", ID: 2, Status: "todo", Severity: "critical", CreatedAt: day(4, 10), UpdatedAt: day(4, 10)},
		{Kind: "requirements", ID: 4, Status: "done", Assignee: "carol", CreatedAt: day(1, 9), UpdatedAt: day(2, 9)}, // 无变更记录
	}
	evs := []flowEvent{
		{"requirements", 1, "todo", day(2, 9)}, {"requirements", 1, "doing", day(3, 9)}, {"requirements", 1, "accepting", day(4, 9)}, {"requirements", 1, "done", day(5, 9)},
		{"requirements", 2, "todo", day(3, 9)}, {"requirements", 2, "doing", day(4, 9)},
		{"bugs", 1, "todo", day(4, 9)}, {"bugs", 1, "doing", day(4, 12)}, {"bugs", 1, "closed", day(4, 21)},
	}
	r := computeReport(items, evs, day(1, 0), day(7, 23), loc)
	if r.Throughput["requirements"] != 2 || r.Throughput["bugs"] != 1 {
		t.Fatalf("throughput %+v", r.Throughput)
	}
	if ct := r.CycleTime["requirements"]; ct.Count != 1 || ct.Avg != 48 {
		t.Fatalf("req cycle %+v", ct)
	}
	if ct := r.CycleTime["bugs"]; ct.Count != 1 || ct.Avg != 9 {
		t.Fatalf("bug cycle %+v", ct)
	}
	if lt := r.LeadTime["requirements"]; lt.Count != 2 || lt.P50 != 24 || lt.P85 != 72 {
		t.Fatalf("lead %+v", lt)
	}
	if r.OpenTotal != 2 || r.OpenBugsBySev["critical"] != 1 || r.ReqByStatus["doing"] != 1 {
		t.Fatalf("open %+v %+v", r.OpenTotal, r.OpenBugsBySev)
	}
	if len(r.Weeks) != 2 || r.Weeks[0].Week != "2026-10-26" || r.Weeks[1].RequirementDone != 2 || r.Weeks[0].RequirementDone != 0 {
		t.Fatalf("weeks %+v", r.Weeks)
	}
	if r.Assignees[0].Assignee != "alice" || r.Assignees[0].OpenReq != 1 || r.Assignees[0].Done != 1 {
		t.Fatalf("assignees %+v", r.Assignees)
	}
	// 重新打开后不再计为完成
	evs2 := append(evs, flowEvent{"bugs", 1, "reopened", day(6, 9)})
	items[3].Status = "reopened"
	if r2 := computeReport(items, evs2, day(1, 0), day(7, 23), loc); r2.Throughput["bugs"] != 0 {
		t.Fatalf("reopened bug must not count: %+v", r2.Throughput)
	}
}

func TestSummarizeAndWeek(t *testing.T) {
	if d := summarize(nil); d.Count != 0 {
		t.Fatal("empty summary")
	}
	d := summarize([]float64{10, 1, 5, 3})
	if d.Count != 4 || d.Avg != 4.8 || d.P50 != 3 || d.P85 != 10 {
		t.Fatalf("summary %+v", d)
	}
	loc := time.FixedZone("CST", 8*3600)
	sun := time.Date(2026, 11, 8, 23, 0, 0, 0, loc)
	if w := weekStart(sun, loc); w.Format("2006-01-02") != "2026-11-02" {
		t.Fatalf("week of sunday = %s", w)
	}
}

func TestComputeBurndown(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	day := func(d, h int) time.Time { return time.Date(2026, 11, d, h, 0, 0, 0, loc) }
	items := []flowItem{
		{Kind: "requirements", ID: 1, Status: "done", CreatedAt: day(1, 9)},
		{Kind: "requirements", ID: 2, Status: "doing", CreatedAt: day(1, 9)},
		{Kind: "requirements", ID: 3, Status: "todo", CreatedAt: day(3, 9)},
	}
	evs := []flowEvent{{"requirements", 1, "todo", day(1, 9)}, {"requirements", 1, "done", day(2, 15)}, {"requirements", 2, "todo", day(1, 9)}}
	pts := computeBurndown(items, evs, day(1, 0), day(5, 0), day(3, 12), loc)
	if len(pts) != 5 {
		t.Fatalf("points %d", len(pts))
	}
	if pts[0].Scope != 2 || pts[0].Remaining != 2 || pts[1].Done != 1 || pts[2].Scope != 3 || pts[2].Remaining != 2 {
		t.Fatalf("burndown %+v", pts)
	}
	if pts[4].Scope != 0 || pts[4].Ideal == nil || *pts[4].Ideal != 0 || *pts[0].Ideal != 2 {
		t.Fatalf("future/ideal %+v", pts[4])
	}
	// 先建迭代、后补需求：理想线取第一个非零范围
	late := computeBurndown(items, evs, time.Date(2026, 10, 30, 0, 0, 0, 0, loc), day(5, 0), day(3, 12), loc)
	if late[0].Scope != 0 || late[0].Ideal == nil || *late[0].Ideal != 2 {
		t.Fatalf("late scope ideal %+v", late[0])
	}
}

func TestParseAIPlan(t *testing.T) {
	raw := "```json\n{\"summary\":\"接入 SSO\",\"steps\":[\"a\"],\"subtasks\":[{\"title\":\"  后端回调 \",\"priority\":\"P9\"},{\"title\":\"\"}],\"risks\":[],\"test_points\":[\"t\"]}\n```"
	p, err := parseAIPlan(raw)
	if err != nil || p.Summary != "接入 SSO" || len(p.Subtasks) != 1 || p.Subtasks[0].Title != "后端回调" || p.Subtasks[0].Priority != "P2" {
		t.Fatalf("plan %+v %v", p, err)
	}
	if _, err := parseAIPlan("抱歉我不能"); err == nil {
		t.Fatal("non-json must fail")
	}
	if _, err := parseAIPlan("{}"); err == nil {
		t.Fatal("empty plan must fail")
	}
}

func TestBuildAIPrompt(t *testing.T) {
	it := &Item{ID: 7, Title: "SSO", Priority: "P1", Description: "背景", Data: json.RawMessage(`{"acceptance":"能登录"}`)}
	s := buildAIPrompt(it, []string{"代码仓库 web"}, "用 OIDC")
	for _, frag := range []string{"#7", "SSO", "背景", "能登录", "代码仓库 web", "用 OIDC"} {
		if !strings.Contains(s, frag) {
			t.Fatalf("prompt misses %q: %s", frag, s)
		}
	}
	if len([]rune(truncate(strings.Repeat("字", 10), 3))) != 4 {
		t.Fatal("truncate adds ellipsis")
	}
}

func TestLinkAndCommentChecks(t *testing.T) {
	ok := linkInput{RefType: "app", RefID: 3, RefName: " web "}
	if ok.check() != nil || ok.RefName != "web" {
		t.Fatal("valid link rejected")
	}
	for _, bad := range []linkInput{{RefType: "repo", RefID: 1, RefName: "x"}, {RefType: "app", RefName: "x"}, {RefType: "pipeline", RefID: 1}} {
		if bad.check() == nil {
			t.Fatalf("bad link accepted: %+v", bad)
		}
	}
	if _, err := checkCommentBody("   "); err == nil {
		t.Fatal("empty comment")
	}
	if _, err := checkCommentBody(strings.Repeat("a", 4001)); err == nil {
		t.Fatal("long comment")
	}
	if !canDeleteComment(&rbac.Principal{Username: "a"}, "a") || canDeleteComment(&rbac.Principal{Username: "b", Permissions: []string{"devcenter:manage"}}, "a") ||
		!canDeleteComment(&rbac.Principal{Username: "b", Permissions: []string{"*"}}, "a") || canDeleteComment(nil, "a") {
		t.Fatal("comment delete permission")
	}
}
