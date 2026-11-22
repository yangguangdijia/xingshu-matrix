package devcenter

// alert_bug.go：告警一键转缺陷（POST /devcenter/bugs/from-alert）。
//
//   - 只读读取可观测模块的 obs_alerts / obs_datasources（以及 cluster_clusters 解析集群名），不写其他模块的表；
//   - 在告警首次触发前 24 小时内，从 cicd_releases 中按应用名 / 命名空间 / 集群启发式挑出「可疑发布」；
//   - 通过 insertItem（与普通创建缺陷同一路径，含创建记录）写入缺陷，data.source 记录来源告警，
//     data.suspect_releases 记录可疑发布；
//   - 幂等：同一告警已转过缺陷时直接返回该缺陷并带 existing:true；并发请求用 PostgreSQL 会话级咨询锁串行；
//   - devcenter_links 已是价值流结构（vs-trace 的 0004 迁移）时，追加一条 kind=alert 的自动关联（尽力而为）。
//
// 扩展（契约之外、向后兼容）：
//   - dry_run:true 只返回预览（标题、可疑发布、是否已存在），不创建，供前端确认框展示；
//   - 未给 alert_id 但给了 incident_id 时，取该故障最早关联的告警（oncall_alert_links）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// registerAlertBug 注册告警转缺陷接口；module.go 在 Register 中调用一次。
func (m *Module) registerAlertBug(g *gin.RouterGroup, _, mg gin.HandlerFunc) {
	g.POST("/bugs/from-alert", mg, m.bugFromAlert)
}

const (
	alertBugWindow      = 24 * time.Hour
	alertBugMaxSuspects = 10
	alertBugScanLimit   = 500
	// alertLinkItemType 是写入 devcenter_links.item_type 的缺陷类型值（与 ParseItemKeys 的 ItemKey.Type 一致）。
	alertLinkItemType = "bug"
	// alertBugLockBase 与告警 id 异或得到咨询锁键，避免与其他模块的锁键冲突。
	alertBugLockBase int64 = 0x4442_4147 << 32 // "DBAG"
)

type alertBugInput struct {
	AlertID    int64 `json:"alert_id"`
	IncidentID int64 `json:"incident_id"`
	DryRun     bool  `json:"dry_run"`
}

// alertInfo 是从 obs_alerts 读出的告警（只读）。
type alertInfo struct {
	ID           int64
	Alertname    string
	Severity     string
	Status       string
	Fingerprint  string
	GeneratorURL string
	Labels       map[string]string
	Annotations  map[string]string
	FiredAt      time.Time
	DSEnv        string
	DSClusterID  string
	DSCluster    string
}

// alertScope 是从告警解析出的定位信息。
type alertScope struct {
	Cluster   string   // 集群名
	ClusterID string   // 集群 id（能解析时）
	Namespace string   //
	App       string   // 首选应用名（候选里的第一个）
	AppCands  []string // 应用名候选（小写，去重，按可信度排序）
	AppLabels []string // 与 AppCands 对应的来源标签
}

// SuspectRelease 是告警前 24h 内的可疑发布。
type SuspectRelease struct {
	ReleaseID     int64    `json:"release_id"`
	AppID         int64    `json:"app_id"`
	AppName       string   `json:"app_name"`
	Env           string   `json:"env"`
	Image         string   `json:"image"`
	PreviousImage string   `json:"previous_image,omitempty"`
	Kind          string   `json:"kind"`
	Status        string   `json:"status"`
	CreatedBy     string   `json:"created_by"`
	CreatedAt     string   `json:"created_at"`
	MinutesBefore int      `json:"minutes_before"`
	Score         int      `json:"score"`
	Reasons       []string `json:"reasons"`
	at            time.Time
}

// releaseRow 是候选发布及其应用环境配置。
type releaseRow struct {
	ID            int64
	AppID         int64
	AppName       string
	Env           string
	Image         string
	PreviousImage string
	Kind          string
	Status        string
	CreatedBy     string
	CreatedAt     time.Time
	Envs          []releaseEnv
}

type releaseEnv struct {
	Name       string `json:"name"`
	TargetID   int64  `json:"target_id"`
	Namespace  string `json:"namespace"`
	Deployment string `json:"deployment"`
	Helm       *struct {
		Release string `json:"release"`
	} `json:"helm"`
}

type targetCluster struct{ ID, Name string }

// bugFromAlertResp 是接口返回：缺陷对象 + existing + suspect_releases。
type bugFromAlertResp struct {
	*Item
	Existing        bool             `json:"existing"`
	SuspectReleases []SuspectRelease `json:"suspect_releases"`
}

func (m *Module) bugFromAlert(c *gin.Context) {
	var in alertBugInput
	if c.ShouldBindJSON(&in) != nil {
		httpx.Fail(c, httpx.Validation("请求内容格式错误"))
		return
	}
	if in.AlertID <= 0 && in.IncidentID > 0 {
		id, err := m.alertOfIncident(c, in.IncidentID)
		if err != nil {
			fail(c, err)
			return
		}
		in.AlertID = id
	}
	if in.AlertID <= 0 {
		httpx.Fail(c, httpx.Validation("请指定要转为缺陷的告警"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	a, err := m.loadAlertForBug(ctx, in.AlertID)
	if err != nil {
		fail(c, err)
		return
	}
	sc := m.resolveAlertScope(ctx, a)
	suspects := m.findSuspectReleases(ctx, a, sc)
	title := alertBugTitle(a, sc)

	if in.DryRun {
		existing, err := m.findAlertBug(ctx, a.ID)
		if err != nil {
			fail(c, err)
			return
		}
		audit.Skip(c)
		out := gin.H{"preview": true, "title": title, "suspect_releases": suspects, "existing": existing != nil,
			"source": alertBugSource(a, sc)}
		if existing != nil {
			out["bug"] = existing
		}
		httpx.OK(c, out)
		return
	}

	// 会话级咨询锁：同一告警的并发请求串行化，保证只建一个缺陷。
	conn, err := m.d.DB.Acquire(ctx)
	if err != nil {
		fail(c, err)
		return
	}
	defer conn.Release()
	lockKey := alertBugLockBase ^ a.ID
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		fail(c, err)
		return
	}
	defer func() {
		uctx, ucancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ucancel()
		_, _ = conn.Exec(uctx, "SELECT pg_advisory_unlock($1)", lockKey)
	}()

	existing, err := m.findAlertBug(ctx, a.ID)
	if err != nil {
		fail(c, err)
		return
	}
	if existing != nil {
		audit.SetResource(c, "devcenter_bugs", strconv.FormatInt(existing.ID, 10))
		audit.SetDetail(c, gin.H{"alert_id": a.ID, "existing": true})
		httpx.OK(c, bugFromAlertResp{Item: existing, Existing: true, SuspectReleases: storedSuspects(existing, suspects)})
		return
	}

	uid, uname := who(c)
	bugIn := buildAlertBugInput(a, sc, suspects, title)
	if err := bugIn.check("bugs", true); err != nil {
		httpx.Fail(c, err)
		return
	}
	it, err := m.insertItem(ctx, "bugs", bugIn, uid, uname)
	if err != nil {
		fail(c, err)
		return
	}
	m.linkAlertToBug(ctx, it.ID, a, sc)
	audit.SetResource(c, "devcenter_bugs", strconv.FormatInt(it.ID, 10))
	audit.SetDetail(c, gin.H{"from_alert": a.ID, "alertname": a.Alertname, "cluster": sc.Cluster, "namespace": sc.Namespace, "suspect_releases": len(suspects)})
	httpx.Created(c, bugFromAlertResp{Item: it, Existing: false, SuspectReleases: suspects})
}

// alertOfIncident 取故障最早关联的告警 id。
func (m *Module) alertOfIncident(ctx context.Context, incidentID int64) (int64, error) {
	if !m.alertBugTable(ctx, "oncall_alert_links") {
		return 0, httpx.NotFound("该故障没有关联的告警")
	}
	var id int64
	err := m.d.DB.QueryRow(ctx, "SELECT alert_id FROM oncall_alert_links WHERE incident_id=$1 ORDER BY created_at, alert_id LIMIT 1", incidentID).Scan(&id)
	if err != nil {
		return 0, httpx.NotFound("该故障没有关联的告警")
	}
	return id, nil
}

func (m *Module) alertBugTable(ctx context.Context, name string) bool {
	var ok bool
	_ = m.d.DB.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", name).Scan(&ok)
	return ok
}

func (m *Module) loadAlertForBug(ctx context.Context, id int64) (*alertInfo, error) {
	if !m.alertBugTable(ctx, "obs_alerts") {
		return nil, httpx.NotFound("告警不存在")
	}
	var a alertInfo
	var labels, annos []byte
	err := m.d.DB.QueryRow(ctx, `SELECT a.id,a.alertname,a.severity,a.status,a.fingerprint,a.generator_url,a.labels,a.annotations,
		COALESCE(a.starts_at,a.created_at),COALESCE(ds.env,''),COALESCE(ds.options->>'cluster_id',''),COALESCE(ds.options->>'cluster_name','')
		FROM obs_alerts a LEFT JOIN obs_datasources ds ON ds.id=a.datasource_id WHERE a.id=$1`, id).
		Scan(&a.ID, &a.Alertname, &a.Severity, &a.Status, &a.Fingerprint, &a.GeneratorURL, &labels, &annos, &a.FiredAt, &a.DSEnv, &a.DSClusterID, &a.DSCluster)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("告警不存在或已被清理")
		}
		return nil, err
	}
	a.Labels, a.Annotations = jsonStringMap(labels), jsonStringMap(annos)
	if a.Alertname == "" {
		a.Alertname = a.Labels["alertname"]
	}
	if a.Severity == "" {
		a.Severity = a.Labels["severity"]
	}
	return &a, nil
}

// jsonStringMap 把 JSONB 对象解码为字符串映射（非字符串值转成文本）。
func jsonStringMap(b []byte) map[string]string {
	out := map[string]string{}
	var raw map[string]any
	if json.Unmarshal(b, &raw) != nil {
		return out
	}
	for k, v := range raw {
		switch x := v.(type) {
		case string:
			out[k] = x
		case nil:
		default:
			bs, _ := json.Marshal(x)
			out[k] = string(bs)
		}
	}
	return out
}

func (m *Module) resolveAlertScope(ctx context.Context, a *alertInfo) alertScope {
	sc := deriveAlertScope(a)
	if sc.Cluster == "" && sc.ClusterID != "" && m.alertBugTable(ctx, "cluster_clusters") {
		if n, err := strconv.ParseInt(sc.ClusterID, 10, 64); err == nil {
			_ = m.d.DB.QueryRow(ctx, "SELECT name FROM cluster_clusters WHERE id=$1", n).Scan(&sc.Cluster)
		}
	}
	return sc
}

// appLabelKeys 是可能携带应用名的告警标签，按可信度排序。job 标签多为采集任务名（kube-state-metrics 等），不使用。
var appLabelKeys = []string{"app", "app_kubernetes_io_name", "app_kubernetes_io_instance", "k8s_app", "service", "deployment", "statefulset", "daemonset", "workload", "container"}

// podSuffix 匹配 Deployment 生成的 Pod 名后缀（-<rs hash>-<5 位>）或 StatefulSet 序号。
var podSuffix = regexp.MustCompile(`(-[a-z0-9]{6,10}-[a-z0-9]{5}|-[0-9]+)$`)

// deriveAlertScope 从标签与数据源配置解析集群、命名空间与应用名候选（纯函数，便于测试）。
func deriveAlertScope(a *alertInfo) alertScope {
	sc := alertScope{Cluster: strings.TrimSpace(a.Labels["cluster"]), ClusterID: strings.TrimSpace(a.Labels["cluster_id"]), Namespace: strings.TrimSpace(a.Labels["namespace"])}
	if sc.ClusterID == "" {
		sc.ClusterID = strings.TrimSpace(a.DSClusterID)
	}
	if sc.Cluster == "" {
		sc.Cluster = strings.TrimSpace(a.DSCluster)
	}
	seen := map[string]bool{}
	add := func(label, v string) {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		sc.AppCands = append(sc.AppCands, v)
		sc.AppLabels = append(sc.AppLabels, label)
	}
	for _, k := range appLabelKeys {
		add(k, a.Labels[k])
	}
	if p := a.Labels["pod"]; p != "" {
		add("pod", podSuffix.ReplaceAllString(p, ""))
	}
	if len(sc.AppCands) > 0 {
		sc.App = sc.AppCands[0]
	}
	return sc
}

func alertBugTitle(a *alertInfo, sc alertScope) string {
	name := a.Alertname
	if name == "" {
		name = "未命名告警"
	}
	cl := sc.Cluster
	if cl == "" {
		cl = "未知集群"
	}
	ns := sc.Namespace
	if ns == "" {
		ns = "-"
	}
	return truncRunes("[告警] "+name+" · "+cl+"/"+ns, 200)
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// truncBytes 按字节截断但不切断 UTF-8 字符。
func truncBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "\n…（内容过长已截断）"
}

func (m *Module) findAlertBug(ctx context.Context, alertID int64) (*Item, error) {
	it, err := scan(m.d.DB.QueryRow(ctx, "SELECT "+cols+" FROM devcenter_bugs WHERE data->'source'->>'alert_id'=$1 ORDER BY id LIMIT 1", strconv.FormatInt(alertID, 10)))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return it, nil
}

// storedSuspects 返回已存在缺陷里保存的可疑发布；没有时用本次计算结果。
func storedSuspects(it *Item, fallback []SuspectRelease) []SuspectRelease {
	var d struct {
		S []SuspectRelease `json:"suspect_releases"`
	}
	if json.Unmarshal(it.Data, &d) == nil && d.S != nil {
		return d.S
	}
	return fallback
}

// findSuspectReleases 读取告警前 24h 内的发布并按启发式打分；cicd 表不存在或查询失败时返回空列表。
func (m *Module) findSuspectReleases(ctx context.Context, a *alertInfo, sc alertScope) []SuspectRelease {
	out := []SuspectRelease{}
	if !m.alertBugTable(ctx, "cicd_releases") || !m.alertBugTable(ctx, "cicd_applications") {
		return out
	}
	rows, err := m.d.DB.Query(ctx, `SELECT r.id,r.app_id,a.name,r.env,r.image,r.previous_image,r.kind,r.status,r.created_by,r.created_at,a.envs
		FROM cicd_releases r JOIN cicd_applications a ON a.id=r.app_id
		WHERE r.created_at >= $1 AND r.created_at <= $2 ORDER BY r.created_at DESC LIMIT $3`, a.FiredAt.Add(-alertBugWindow), a.FiredAt, alertBugScanLimit)
	if err != nil {
		return out
	}
	var list []releaseRow
	for rows.Next() {
		var r releaseRow
		var envs []byte
		if rows.Scan(&r.ID, &r.AppID, &r.AppName, &r.Env, &r.Image, &r.PreviousImage, &r.Kind, &r.Status, &r.CreatedBy, &r.CreatedAt, &envs) != nil {
			continue
		}
		_ = json.Unmarshal(envs, &r.Envs)
		list = append(list, r)
	}
	rows.Close()
	if len(list) == 0 {
		return out
	}
	return matchSuspects(a.FiredAt, sc, list, m.loadTargetClusters(ctx))
}

// loadTargetClusters 读取执行目标所在集群（cicd_targets.cluster_id → cluster_clusters.name），失败时返回空映射。
func (m *Module) loadTargetClusters(ctx context.Context) map[int64]targetCluster {
	out := map[int64]targetCluster{}
	if !m.alertBugTable(ctx, "cicd_targets") {
		return out
	}
	q := "SELECT t.id, COALESCE(t.cluster_id::text,''), COALESCE(cl.name,'') FROM cicd_targets t LEFT JOIN cluster_clusters cl ON cl.id=t.cluster_id"
	if !m.alertBugTable(ctx, "cluster_clusters") {
		q = "SELECT t.id, COALESCE(t.cluster_id::text,''), '' FROM cicd_targets t"
	}
	rows, err := m.d.DB.Query(ctx, q)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var tc targetCluster
		if rows.Scan(&id, &tc.ID, &tc.Name) == nil {
			out[id] = tc
		}
	}
	return out
}

// matchSuspects 是可疑发布的打分规则（纯函数）：
//   - 应用名匹配（应用名 / 环境里的 deployment / helm release 等于告警的 app、service、deployment… 或由 pod 名推出的工作负载）+3；
//   - 发布环境的命名空间等于告警 namespace +2；
//   - 发布环境的执行目标所在集群等于告警集群 +1；
//   - 两边都能确定、但命名空间或集群不同的直接排除（同名应用的其他环境）；
//   - 至少命中应用名或命名空间之一才算可疑；按得分、时间倒序取前 10。
func matchSuspects(firedAt time.Time, sc alertScope, list []releaseRow, targets map[int64]targetCluster) []SuspectRelease {
	out := []SuspectRelease{}
	for _, r := range list {
		var env *releaseEnv
		for i := range r.Envs {
			if r.Envs[i].Name == r.Env {
				env = &r.Envs[i]
				break
			}
		}
		score, reasons := 0, []string{}
		names := []string{strings.ToLower(r.AppName)}
		var ns string
		var tc targetCluster
		if env != nil {
			if env.Deployment != "" {
				names = append(names, strings.ToLower(env.Deployment))
			}
			if env.Helm != nil && env.Helm.Release != "" {
				names = append(names, strings.ToLower(env.Helm.Release))
			}
			ns = env.Namespace
			tc = targets[env.TargetID]
		}
		appHit := false
		for i, cand := range sc.AppCands {
			for _, n := range names {
				if n != "" && n == cand {
					appHit = true
					reasons = append(reasons, "应用名匹配（"+sc.AppLabels[i]+"="+cand+"）")
					break
				}
			}
			if appHit {
				break
			}
		}
		if appHit {
			score += 3
		}
		nsHit := false
		if sc.Namespace != "" && ns != "" {
			if ns != sc.Namespace {
				continue
			}
			nsHit = true
			score += 2
			reasons = append(reasons, "命名空间相同（"+ns+"）")
		}
		if sc.Cluster != "" || sc.ClusterID != "" {
			if tc.Name != "" || tc.ID != "" {
				same := (sc.ClusterID != "" && tc.ID != "" && sc.ClusterID == tc.ID) || (sc.Cluster != "" && tc.Name != "" && strings.EqualFold(sc.Cluster, tc.Name))
				if !same {
					continue
				}
				score++
				name := tc.Name
				if name == "" {
					name = sc.Cluster
				}
				reasons = append(reasons, "同一集群（"+name+"）")
			}
		}
		if !appHit && !nsHit {
			continue
		}
		mins := int(firedAt.Sub(r.CreatedAt).Minutes())
		if mins < 0 {
			mins = 0
		}
		out = append(out, SuspectRelease{ReleaseID: r.ID, AppID: r.AppID, AppName: r.AppName, Env: r.Env, Image: r.Image, PreviousImage: r.PreviousImage,
			Kind: r.Kind, Status: r.Status, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt.Format(time.RFC3339), MinutesBefore: mins, Score: score, Reasons: reasons, at: r.CreatedAt})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].at.After(out[j].at)
	})
	if len(out) > alertBugMaxSuspects {
		out = out[:alertBugMaxSuspects]
	}
	return out
}

// alertURL 是控制台中打开告警详情的地址（index.html 把 #/observe/events/<id> 作为告警详情入口）。
func alertURL(id int64) string { return "#/observe/events/" + strconv.FormatInt(id, 10) }

func alertBugSource(a *alertInfo, sc alertScope) map[string]any {
	return map[string]any{
		"type": "alert", "alert_id": a.ID, "alertname": a.Alertname, "cluster": sc.Cluster, "cluster_id": sc.ClusterID,
		"namespace": sc.Namespace, "app": sc.App, "severity": a.Severity, "fingerprint": a.Fingerprint,
		"fired_at": a.FiredAt.Format(time.RFC3339), "alert_url": alertURL(a.ID),
	}
}

// bugSeverity / bugPriority / bugEnv 把告警级别、数据源环境映射到缺陷字段。
func bugSeverity(s string) string {
	switch strings.ToLower(s) {
	case "critical", "p0", "p1", "fatal", "emergency":
		return "critical"
	case "warning", "error", "p2", "high", "major":
		return "major"
	case "info", "p3", "low", "minor", "none":
		return "minor"
	}
	return "major"
}

func bugPriority(s string) string {
	switch bugSeverity(s) {
	case "critical":
		return "P1"
	case "minor":
		return "P3"
	}
	return "P2"
}

func bugEnv(e string) string {
	switch strings.ToLower(strings.TrimSpace(e)) {
	case "prod", "production", "prd", "生产":
		return "prod"
	case "pre", "staging", "stage", "uat", "预发":
		return "pre"
	case "test", "testing", "qa", "测试":
		return "test"
	case "dev", "development", "开发":
		return "dev"
	}
	return ""
}

func buildAlertBugInput(a *alertInfo, sc alertScope, suspects []SuspectRelease, title string) input {
	desc := buildAlertBugDescription(a, sc, suspects)
	data := map[string]any{"source": alertBugSource(a, sc), "suspect_releases": suspects, "severity": bugSeverity(a.Severity)}
	if e := bugEnv(firstNonEmpty(a.Labels["env"], a.DSEnv)); e != "" {
		data["env"] = e
	}
	raw, _ := json.Marshal(data)
	for len(raw) > 16000 && len(suspects) > 0 { // 扩展字段上限 16 KB：超出时减少附带的可疑发布
		suspects = suspects[:len(suspects)-1]
		data["suspect_releases"] = suspects
		raw, _ = json.Marshal(data)
	}
	pr := bugPriority(a.Severity)
	return input{Title: &title, Description: &desc, Priority: &pr, Data: raw}
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// buildAlertBugDescription 生成缺陷描述（Markdown 文本）：告警摘要、定位、标签、告警链接、可疑发布。
func buildAlertBugDescription(a *alertInfo, sc alertScope, suspects []SuspectRelease) string {
	var b strings.Builder
	b.WriteString("## 告警摘要\n")
	summary := firstNonEmpty(a.Annotations["summary"], a.Annotations["description"], a.Annotations["message"], a.Alertname)
	b.WriteString(summary + "\n")
	if d := a.Annotations["description"]; d != "" && d != summary {
		b.WriteString("\n" + d + "\n")
	}
	b.WriteString("\n## 告警信息\n")
	status := map[string]string{"firing": "告警中", "resolved": "已恢复"}[a.Status]
	if status == "" {
		status = a.Status
	}
	rows := [][2]string{{"告警名称", a.Alertname}, {"级别", a.Severity}, {"状态", status}, {"首次触发", a.FiredAt.Format("2006-01-02 15:04:05 MST")},
		{"集群", firstNonEmpty(sc.Cluster, "未知")}, {"命名空间", firstNonEmpty(sc.Namespace, "—")}, {"应用", firstNonEmpty(sc.App, "未识别")}, {"指纹", a.Fingerprint}}
	for _, r := range rows {
		if r[1] != "" {
			b.WriteString("- " + r[0] + "：" + r[1] + "\n")
		}
	}
	b.WriteString("- 告警详情：" + alertURL(a.ID) + "\n")
	if u := a.Annotations["runbook_url"]; strings.HasPrefix(u, "https://") {
		b.WriteString("- 处理手册：" + u + "\n")
	}
	if len(a.Labels) > 0 {
		b.WriteString("\n## 标签\n")
		keys := make([]string, 0, len(a.Labels))
		for k := range a.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString("- " + k + "=" + a.Labels[k] + "\n")
		}
	}
	b.WriteString("\n## 可疑发布（告警前 24 小时内）\n")
	if len(suspects) == 0 {
		b.WriteString("未找到同应用 / 命名空间的发布记录。\n")
	}
	for _, s := range suspects {
		fmt.Fprintf(&b, "- 发布 #%d %s · %s · %s（%s，告警前 %s）%s\n", s.ReleaseID, s.AppName, s.Env, s.Image, s.Status, humanMinutes(s.MinutesBefore), reasonText(s.Reasons))
	}
	b.WriteString("\n> 由告警自动转为缺陷；可疑发布按应用名、命名空间、集群启发式匹配，仅供排查参考。\n")
	return truncBytes(b.String(), 19000)
}

func reasonText(r []string) string {
	if len(r) == 0 {
		return ""
	}
	return "；依据：" + strings.Join(r, "、")
}

func humanMinutes(m int) string {
	if m < 60 {
		return strconv.Itoa(m) + " 分钟"
	}
	if m%60 == 0 {
		return strconv.Itoa(m/60) + " 小时"
	}
	return strconv.Itoa(m/60) + " 小时 " + strconv.Itoa(m%60) + " 分钟"
}

// linkAlertToBug 在价值流关联表就绪时写入 kind=alert 的自动关联；失败不影响缺陷创建。
func (m *Module) linkAlertToBug(ctx context.Context, bugID int64, a *alertInfo, sc alertScope) {
	var n int
	if m.d.DB.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='devcenter_links'
		AND column_name IN ('item_type','item_id','kind','ref','title','url','env','status','occurred_at','source')`).Scan(&n) != nil || n < 10 {
		return
	}
	title := truncRunes(firstNonEmpty(a.Annotations["summary"], a.Alertname), 200)
	_, err := m.d.DB.Exec(ctx, `INSERT INTO devcenter_links(item_type,item_id,kind,ref,title,url,env,status,occurred_at,source)
		VALUES($1,$2,'alert',$3,$4,$5,$6,$7,$8,'auto') ON CONFLICT DO NOTHING`,
		alertLinkItemType, bugID, strconv.FormatInt(a.ID, 10), title, alertURL(a.ID), firstNonEmpty(a.Labels["env"], a.DSEnv, sc.Cluster), a.Status, a.FiredAt)
	if err != nil && m.d.Logger != nil {
		m.d.Logger.Warn("devcenter: alert link insert failed", "bug_id", bugID, "alert_id", a.ID, "err", err)
	}
}
