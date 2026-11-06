package devcenter

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// 统计报表：全部基于需求 / 缺陷表与变更记录实时计算，不做预聚合。
// 吞吐 = 区间内进入完成状态的条目数（按周）；周期时长 = 首次进入“开发中/处理中”到完成；交付时长 = 创建到完成。

type flowItem struct {
	Kind      string
	ID        int64
	Status    string
	Assignee  string
	Severity  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type flowEvent struct {
	Kind   string
	ItemID int64
	To     string
	At     time.Time
}

// itemTimes 从状态事件中推算开始时间（首次进入 doing）与完成时间（最后一次进入完成状态，且当前仍为完成）。
func itemTimes(it flowItem, evs []flowEvent) (start, done time.Time) {
	for _, e := range evs {
		if e.To == "doing" && start.IsZero() {
			start = e.At
		}
		if isDone(it.Kind, e.To) {
			done = e.At
		} else {
			done = time.Time{}
		}
	}
	if isDone(it.Kind, it.Status) && done.IsZero() {
		done = it.UpdatedAt // 没有变更记录的历史数据，退化为最后更新时间
	}
	if !isDone(it.Kind, it.Status) {
		done = time.Time{}
	}
	return start, done
}

// weekStart 返回 t 所在周（周一开始）在 loc 时区的零点。
func weekStart(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	wd := (int(t.Weekday()) + 6) % 7
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	return d.AddDate(0, 0, -wd)
}

// Durations 是一组时长（小时）的摘要。
type Durations struct {
	Count int     `json:"count"`
	Avg   float64 `json:"avg_hours"`
	P50   float64 `json:"p50_hours"`
	P85   float64 `json:"p85_hours"`
}

func summarize(hours []float64) Durations {
	if len(hours) == 0 {
		return Durations{}
	}
	sort.Float64s(hours)
	sum := 0.0
	for _, h := range hours {
		sum += h
	}
	pct := func(p float64) float64 {
		i := int(math.Ceil(p*float64(len(hours)))) - 1
		if i < 0 {
			i = 0
		}
		return round1(hours[i])
	}
	return Durations{Count: len(hours), Avg: round1(sum / float64(len(hours))), P50: pct(0.5), P85: pct(0.85)}
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

// WeekPoint 是一周的吞吐与新增。
type WeekPoint struct {
	Week            string `json:"week"`
	RequirementDone int    `json:"requirements_done"`
	BugClosed       int    `json:"bugs_closed"`
	RequirementNew  int    `json:"requirements_created"`
	BugNew          int    `json:"bugs_created"`
}

// AssigneeLoad 是每个处理人的在手与区间内完成数量。
type AssigneeLoad struct {
	Assignee string `json:"assignee"`
	OpenReq  int    `json:"open_requirements"`
	OpenBug  int    `json:"open_bugs"`
	Done     int    `json:"done_in_range"`
}

// Report 是统计结果。
type Report struct {
	From           time.Time            `json:"from"`
	To             time.Time            `json:"to"`
	Weeks          []WeekPoint          `json:"weeks"`
	Throughput     map[string]int       `json:"throughput"`
	Created        map[string]int       `json:"created"`
	CycleTime      map[string]Durations `json:"cycle_time"`
	LeadTime       map[string]Durations `json:"lead_time"`
	ReqByStatus    map[string]int       `json:"requirements_by_status"`
	BugByStatus    map[string]int       `json:"bugs_by_status"`
	OpenBugsBySev  map[string]int       `json:"open_bugs_by_severity"`
	Assignees      []AssigneeLoad       `json:"assignees"`
	OpenTotal      int                  `json:"open_total"`
	CompletionRate float64              `json:"completion_rate"`
}

// computeReport 是纯函数，便于单测。events 需按时间升序。
func computeReport(items []flowItem, events []flowEvent, from, to time.Time, loc *time.Location) Report {
	r := Report{From: from, To: to, Throughput: map[string]int{"requirements": 0, "bugs": 0}, Created: map[string]int{"requirements": 0, "bugs": 0},
		CycleTime: map[string]Durations{}, LeadTime: map[string]Durations{}, ReqByStatus: map[string]int{}, BugByStatus: map[string]int{}, OpenBugsBySev: map[string]int{}}
	byItem := map[string][]flowEvent{}
	key := func(k string, id int64) string { return k + "#" + strconv.FormatInt(id, 10) }
	for _, e := range events {
		byItem[key(e.Kind, e.ItemID)] = append(byItem[key(e.Kind, e.ItemID)], e)
	}
	weeks := map[time.Time]*WeekPoint{}
	var order []time.Time
	for w := weekStart(from, loc); !w.After(to); w = w.AddDate(0, 0, 7) {
		wp := &WeekPoint{Week: w.Format("2006-01-02")}
		weeks[w] = wp
		order = append(order, w)
	}
	in := func(t time.Time) bool { return !t.IsZero() && !t.Before(from) && !t.After(to) }
	cycle, lead := map[string][]float64{}, map[string][]float64{}
	loads := map[string]*AssigneeLoad{}
	load := func(a string) *AssigneeLoad {
		if a == "" {
			a = "（未指派）"
		}
		if loads[a] == nil {
			loads[a] = &AssigneeLoad{Assignee: a}
		}
		return loads[a]
	}
	createdInRange := 0
	for _, it := range items {
		start, done := itemTimes(it, byItem[key(it.Kind, it.ID)])
		if it.Kind == "requirements" {
			r.ReqByStatus[it.Status]++
		} else {
			r.BugByStatus[it.Status]++
		}
		open := !isDone(it.Kind, it.Status)
		if open {
			r.OpenTotal++
			l := load(it.Assignee)
			if it.Kind == "requirements" {
				l.OpenReq++
			} else {
				l.OpenBug++
				sev := it.Severity
				if sev == "" {
					sev = "unknown"
				}
				r.OpenBugsBySev[sev]++
			}
		}
		if in(it.CreatedAt) {
			createdInRange++
			r.Created[it.Kind]++
			if wp := weeks[weekStart(it.CreatedAt, loc)]; wp != nil {
				if it.Kind == "requirements" {
					wp.RequirementNew++
				} else {
					wp.BugNew++
				}
			}
		}
		// “已关闭/驳回”的需求不计入吞吐和时长
		if !in(done) || (it.Kind == "requirements" && it.Status == "rejected") {
			continue
		}
		r.Throughput[it.Kind]++
		load(it.Assignee).Done++
		if wp := weeks[weekStart(done, loc)]; wp != nil {
			if it.Kind == "requirements" {
				wp.RequirementDone++
			} else {
				wp.BugClosed++
			}
		}
		lead[it.Kind] = append(lead[it.Kind], done.Sub(it.CreatedAt).Hours())
		if !start.IsZero() && !start.After(done) {
			cycle[it.Kind] = append(cycle[it.Kind], done.Sub(start).Hours())
		}
	}
	for _, k := range []string{"requirements", "bugs"} {
		r.CycleTime[k] = summarize(cycle[k])
		r.LeadTime[k] = summarize(lead[k])
	}
	for _, w := range order {
		r.Weeks = append(r.Weeks, *weeks[w])
	}
	for _, l := range loads {
		r.Assignees = append(r.Assignees, *l)
	}
	sort.Slice(r.Assignees, func(i, j int) bool {
		a, b := r.Assignees[i], r.Assignees[j]
		if a.OpenReq+a.OpenBug != b.OpenReq+b.OpenBug {
			return a.OpenReq+a.OpenBug > b.OpenReq+b.OpenBug
		}
		return a.Assignee < b.Assignee
	})
	if createdInRange > 0 {
		r.CompletionRate = round1(float64(r.Throughput["requirements"]+r.Throughput["bugs"]) * 100 / float64(createdInRange))
	}
	return r
}

// zone 由前端传入的 tz_offset（分钟，等于 JS getTimezoneOffset 的相反数）构造时区，缺省东八区。
func zone(c *gin.Context) *time.Location {
	if v := c.Query("tz_offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= -14*60 && n <= 14*60 {
			return time.FixedZone("local", n*60)
		}
	}
	return time.FixedZone("CST", 8*3600)
}

func (m *Module) loadFlow(c *gin.Context, iteration *int64) ([]flowItem, []flowEvent, error) {
	var items []flowItem
	ids := map[string]bool{}
	for _, k := range []string{"requirements", "bugs"} {
		rows, err := m.d.DB.Query(c, "SELECT id,status,assignee,COALESCE(data->>'severity',''),created_at,updated_at FROM "+table(k)+" WHERE ($1::bigint IS NULL OR iteration_id=$1) LIMIT 50000", iteration)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			it := flowItem{Kind: k}
			if err := rows.Scan(&it.ID, &it.Status, &it.Assignee, &it.Severity, &it.CreatedAt, &it.UpdatedAt); err != nil {
				rows.Close()
				return nil, nil, err
			}
			items = append(items, it)
			ids[k+"#"+strconv.FormatInt(it.ID, 10)] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
	}
	rows, err := m.d.DB.Query(c, "SELECT kind,item_id,to_value,created_at FROM devcenter_activities WHERE kind IN ('requirements','bugs') AND action IN ('create','status') ORDER BY created_at,id LIMIT 300000")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var evs []flowEvent
	for rows.Next() {
		var e flowEvent
		if err := rows.Scan(&e.Kind, &e.ItemID, &e.To, &e.At); err != nil {
			return nil, nil, err
		}
		if ids[e.Kind+"#"+strconv.FormatInt(e.ItemID, 10)] {
			evs = append(evs, e)
		}
	}
	return items, evs, rows.Err()
}

// stats：GET /devcenter/stats?days=30&iteration_id=&tz_offset=480
func (m *Module) stats(c *gin.Context) {
	days, _ := strconv.Atoi(c.DefaultQuery("days", "30"))
	if days < 7 || days > 365 {
		httpx.Fail(c, httpx.Validation("统计区间只能在 7 到 365 天之间"))
		return
	}
	iter, ok := optID(c, "iteration_id")
	if !ok {
		return
	}
	items, evs, err := m.loadFlow(c, iter)
	if err != nil {
		fail(c, err)
		return
	}
	loc := zone(c)
	to := time.Now().In(loc)
	from := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(days - 1))
	rep := computeReport(items, evs, from, to, loc)
	// 测试用例执行情况（当前状态）
	tests := map[string]int{}
	rows, err := m.d.DB.Query(c, "SELECT status,count(*) FROM devcenter_tests WHERE ($1::bigint IS NULL OR iteration_id=$1) GROUP BY status", iter)
	if err != nil {
		fail(c, err)
		return
	}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			rows.Close()
			fail(c, err)
			return
		}
		tests[s] = n
	}
	rows.Close()
	httpx.OK(c, gin.H{"days": days, "report": rep, "tests_by_status": tests})
}

// BurnPoint 是燃尽图上的一天。
type BurnPoint struct {
	Date      string   `json:"date"`
	Scope     int      `json:"scope"`
	Done      int      `json:"done"`
	Remaining int      `json:"remaining"`
	Ideal     *float64 `json:"ideal"`
}

// computeBurndown 计算迭代内需求每天的范围、完成与剩余（截至当天 24 点），today 之后只给理想线。
func computeBurndown(items []flowItem, events []flowEvent, start, end, today time.Time, loc *time.Location) []BurnPoint {
	byItem := map[int64][]flowEvent{}
	for _, e := range events {
		byItem[e.ItemID] = append(byItem[e.ItemID], e)
	}
	day := func(t time.Time) time.Time {
		t = t.In(loc)
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	}
	s, e, td := day(start), day(end), day(today)
	if e.Before(s) {
		e = s
	}
	if e.Sub(s) > 120*24*time.Hour {
		e = s.AddDate(0, 0, 120)
	}
	total := int(e.Sub(s).Hours()/24 + 0.5)
	var out []BurnPoint
	var initScope float64
	for i, d := 0, s; !d.After(e); i, d = i+1, d.AddDate(0, 0, 1) {
		p := BurnPoint{Date: d.Format("2006-01-02")}
		if !d.After(td) {
			cut := d.AddDate(0, 0, 1)
			for _, it := range items {
				if it.Status == "rejected" || !it.CreatedAt.Before(cut) {
					continue
				}
				p.Scope++
				doneAt := time.Time{}
				for _, ev := range byItem[it.ID] {
					if !ev.At.Before(cut) {
						break
					}
					if isDone("requirements", ev.To) {
						doneAt = ev.At
					} else {
						doneAt = time.Time{}
					}
				}
				if doneAt.IsZero() && isDone("requirements", it.Status) && len(byItem[it.ID]) == 0 && it.UpdatedAt.Before(cut) {
					doneAt = it.UpdatedAt
				}
				if !doneAt.IsZero() {
					p.Done++
				}
			}
			p.Remaining = p.Scope - p.Done
			if i == 0 || initScope == 0 {
				// 理想线从迭代开始时的范围出发；开始时还没有需求（先建迭代后补需求）时取第一个非零范围
				initScope = float64(p.Scope)
			}
		}
		out = append(out, p)
	}
	if total > 0 {
		for i := range out {
			v := round1(initScope * float64(total-i) / float64(total))
			out[i].Ideal = &v
		}
	}
	return out
}

func parseDay(s string, loc *time.Location) (time.Time, bool) {
	t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(s), loc)
	return t, err == nil
}

// burndown：GET /devcenter/iterations/:id/burndown?tz_offset=480。迭代起止取 data.start / data.end（YYYY-MM-DD），
// 缺省开始为创建日期、结束为开始后 14 天。
func (m *Module) burndown(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	loc := zone(c)
	var startS, endS string
	var created time.Time
	if err := m.d.DB.QueryRow(c, "SELECT COALESCE(data->>'start',''),COALESCE(data->>'end',''),created_at FROM devcenter_iterations WHERE id=$1", id).Scan(&startS, &endS, &created); err != nil {
		fail(c, err)
		return
	}
	start, okS := parseDay(startS, loc)
	if !okS {
		start = created
	}
	end, okE := parseDay(endS, loc)
	if !okE {
		end = start.AddDate(0, 0, 14)
	}
	items, evs, err := m.loadFlow(c, &id)
	if err != nil {
		fail(c, err)
		return
	}
	var reqs []flowItem
	var reqEvs []flowEvent
	for _, it := range items {
		if it.Kind == "requirements" {
			reqs = append(reqs, it)
		}
	}
	for _, e := range evs {
		if e.Kind == "requirements" {
			reqEvs = append(reqEvs, e)
		}
	}
	httpx.OK(c, gin.H{"start": start.Format("2006-01-02"), "end": end.Format("2006-01-02"), "dates_from_iteration": okS && okE,
		"points": computeBurndown(reqs, reqEvs, start, end, time.Now(), loc)})
}
