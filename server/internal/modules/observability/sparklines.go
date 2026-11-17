package observability

// 列表行内迷你趋势图（近 30 分钟 CPU / 内存）。
//
// GET /observability/metrics/sparklines?cluster_id=&kind=pod|node|workload
//     &workload_kind=deployments|statefulsets|daemonsets|jobs|cronjobs
//     &namespace=&names=a,b[&ips=10.0.0.1,10.0.0.2]
//
// 一次请求最多 50 个对象，固定 30 分钟、步长 1 分钟。数据源按集群解析
// （selectSource，与总览 24 小时趋势同一套归属规则）；集群未绑定 Prometheus
// 时返回 200 + source="none" + 空数组，前端静默降级为「—」。
//
// PromQL 刻意不使用 by()/on() 聚合：中央数据源的 scopedPromQL 校验器会拒绝
// 分组标签列表。因此按原始序列取回，在服务端按 Pod / 节点 / 工作负载求和或求均值，
// 两种数据源（独立 / 中央）走同一条代码路径。

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const (
	sparkMaxNames = 50
	sparkWindow   = 30 * time.Minute
	sparkStep     = time.Minute
	sparkTimeout  = 10 * time.Second
	// rate() 窗口：与总览趋势一致，兼容 1 分钟抓取间隔。
	sparkRateWindow = "5m"
)

// PermWorkloadRead 与 cluster 模块同名权限码（迷你趋势图只展示列表里已可见对象的用量）。
const permSparkWorkloadRead = "workload:read"
const permSparkNodeRead = "node:read"

var (
	sparkNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	// 节点名允许大写（部分云厂商主机名），仍限制为 DNS 字符。
	sparkNodeNameRe = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9._]*[A-Za-z0-9])?$`)
)

// workloadPodSuffix 是各控制器生成的 Pod 名后缀（RE2，Prometheus 与 Go 通用）。
// Deployment: <name>-<pod-template-hash>-<5>；StatefulSet: <name>-<序号>；
// DaemonSet / Job: <name>-<5>；CronJob: <name>-<调度时间(分钟)>-<5>。
//
// 注意：不能使用 {n} 量词——中央数据源校验器按第一个 '}' 截取选择器，
// 引号内的花括号会被误判，因此 5 位随机串展开写，哈希段用 +。
const rand5 = `[a-z0-9][a-z0-9][a-z0-9][a-z0-9][a-z0-9]`

var workloadPodSuffix = map[string]string{
	"deployments":  `-[a-z0-9]+-` + rand5,
	"statefulsets": `-[0-9]+`,
	"daemonsets":   `-` + rand5,
	"jobs":         `-` + rand5,
	"cronjobs":     `-[0-9]+-` + rand5,
}

type sparkTarget struct {
	Key       string // 原样回显给前端的名称（name 或 ns/name）
	Namespace string
	Name      string
	IP        string // 仅节点
}

type sparkRequest struct {
	ClusterID    int64
	Kind         string // pod | node | workload
	WorkloadKind string
	Targets      []sparkTarget
}

// SparkSeries 是单个对象的两条曲线，点为 [unix 秒, 值]。
type SparkSeries struct {
	Name string       `json:"name"`
	CPU  [][2]float64 `json:"cpu"`
	Mem  [][2]float64 `json:"mem"`
}

type sparkUnit struct {
	CPU string `json:"cpu"`
	Mem string `json:"mem"`
}

// SparkResponse 是接口返回体。
type SparkResponse struct {
	Source       string        `json:"source"` // prometheus | none
	Kind         string        `json:"kind"`
	WorkloadKind string        `json:"workload_kind,omitempty"`
	Start        int64         `json:"start"`
	End          int64         `json:"end"`
	Step         int           `json:"step"`
	Unit         sparkUnit     `json:"unit"`
	Items        []SparkSeries `json:"items"`
	Note         string        `json:"note,omitempty"`
}

func splitCSV(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// parseSparkRequest 只做纯参数校验，不访问数据库，便于单测。
func parseSparkRequest(q map[string]string) (*sparkRequest, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(q["cluster_id"]), 10, 64)
	if err != nil || id <= 0 {
		return nil, httpx.Validation("集群 ID 无效")
	}
	req := &sparkRequest{ClusterID: id, Kind: strings.TrimSpace(q["kind"])}
	switch req.Kind {
	case "pod", "node":
	case "workload":
		req.WorkloadKind = strings.TrimSpace(q["workload_kind"])
		if req.WorkloadKind == "" {
			req.WorkloadKind = "deployments"
		}
		if _, ok := workloadPodSuffix[req.WorkloadKind]; !ok {
			return nil, httpx.Validation("工作负载类型仅支持 deployments、statefulsets、daemonsets、jobs、cronjobs")
		}
	default:
		return nil, httpx.Validation("对象类型仅支持 pod、node、workload")
	}
	names := splitCSV(q["names"])
	if len(names) == 0 {
		return nil, httpx.Validation("请至少指定一个对象名称")
	}
	if len(names) > sparkMaxNames {
		return nil, httpx.Validation("一次最多查询 50 个对象")
	}
	ns := strings.TrimSpace(q["namespace"])
	if ns != "" && !rbac.ValidNamespace(ns) {
		return nil, httpx.Validation("命名空间名称无效")
	}
	var ips []string
	if req.Kind == "node" {
		ips = strings.Split(q["ips"], ",")
		if strings.TrimSpace(q["ips"]) == "" {
			ips = nil
		}
		if ips != nil && len(ips) != len(names) {
			return nil, httpx.Validation("节点 IP 数量必须与名称数量一致")
		}
	}
	seen := map[string]bool{}
	for i, raw := range names {
		t := sparkTarget{Key: raw}
		if req.Kind == "node" {
			if len(raw) > 253 || !sparkNodeNameRe.MatchString(raw) {
				return nil, httpx.Validation("节点名称无效：" + raw)
			}
			t.Name = raw
			if ips != nil {
				ip := strings.TrimSpace(ips[i])
				if ip != "" && net.ParseIP(ip) == nil {
					return nil, httpx.Validation("节点 IP 无效：" + ip)
				}
				t.IP = ip
			}
		} else {
			t.Namespace, t.Name = ns, raw
			if left, right, ok := strings.Cut(raw, "/"); ok {
				t.Namespace, t.Name = left, right
			}
			if t.Namespace == "" {
				return nil, httpx.Validation("请指定命名空间，或使用「命名空间/名称」格式")
			}
			if !rbac.ValidNamespace(t.Namespace) {
				return nil, httpx.Validation("命名空间名称无效：" + t.Namespace)
			}
			if len(t.Name) > 253 || !sparkNameRe.MatchString(t.Name) {
				return nil, httpx.Validation("对象名称无效：" + t.Name)
			}
		}
		if seen[t.Key] {
			continue
		}
		seen[t.Key] = true
		req.Targets = append(req.Targets, t)
	}
	return req, nil
}

func promStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// scopeMatcher 为中央数据源追加精确集群匹配；独立数据源返回空串。
func scopeMatcher(scope sourceScope) string {
	if !scope.Central {
		return ""
	}
	return scope.Label + "=" + promStr(scope.Value)
}

func joinMatchers(parts ...string) string {
	out := []string{}
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return "{" + strings.Join(out, ",") + "}"
}

func sortedUnique(in []string) []string {
	set := map[string]bool{}
	for _, s := range in {
		set[s] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func quoteAlternation(values []string) string {
	q := make([]string, len(values))
	for i, v := range values {
		q[i] = regexp.QuoteMeta(v)
	}
	return strings.Join(q, "|")
}

type sparkQueries struct{ CPU, Mem string }

// buildSparkQueries 生成两条 PromQL。pod/workload：CPU 为核数、内存为字节；
// node：CPU 返回各核空闲率（服务端求均值后换算使用率），内存直接为使用率 %。
func buildSparkQueries(req *sparkRequest, scope sourceScope) sparkQueries {
	sm := scopeMatcher(scope)
	if req.Kind == "node" {
		sel := joinMatchers(sm)
		return sparkQueries{
			CPU: `rate(node_cpu_seconds_total` + joinMatchers(`mode="idle"`, sm) + `[` + sparkRateWindow + `])`,
			Mem: `100 * (1 - node_memory_MemAvailable_bytes` + sel + ` / node_memory_MemTotal_bytes` + sel + `)`,
		}
	}
	namespaces := make([]string, 0, len(req.Targets))
	names := make([]string, 0, len(req.Targets))
	for _, t := range req.Targets {
		namespaces = append(namespaces, t.Namespace)
		names = append(names, t.Name)
	}
	namespaces, names = sortedUnique(namespaces), sortedUnique(names)
	podPattern := quoteAlternation(names)
	if req.Kind == "workload" {
		podPattern = "(?:" + podPattern + ")" + workloadPodSuffix[req.WorkloadKind]
	}
	nsMatcher := `namespace=` + promStr(namespaces[0])
	if len(namespaces) > 1 {
		nsMatcher = `namespace=~` + promStr(quoteAlternation(namespaces))
	}
	base := []string{nsMatcher, `pod=~` + promStr(podPattern), `container!=""`, `container!="POD"`, sm}
	return sparkQueries{
		CPU: `rate(container_cpu_usage_seconds_total` + joinMatchers(base...) + `[` + sparkRateWindow + `])`,
		Mem: `container_memory_working_set_bytes` + joinMatchers(base...),
	}
}

type promMatrixSeries struct {
	Metric map[string]string `json:"metric"`
	Values [][2]any          `json:"values"`
}

func decodeMatrix(env *promEnvelope) ([]promMatrixSeries, error) {
	if env == nil || len(env.Data) == 0 {
		return nil, nil
	}
	var data struct {
		ResultType string             `json:"resultType"`
		Result     []promMatrixSeries `json:"result"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, upstreamErr("prometheus: invalid matrix: %v", err)
	}
	return data.Result, nil
}

func pointValue(p [2]any) (int64, float64, bool) {
	ts, ok := p[0].(float64)
	if !ok {
		return 0, 0, false
	}
	s, ok := p[1].(string)
	if !ok {
		return 0, 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, 0, false
	}
	return int64(math.Round(ts)), v, true
}

// accumulator 按 (目标, 时间戳) 聚合多条原始序列。
type accumulator struct {
	sum   map[string]map[int64]float64
	count map[string]map[int64]int
}

func newAccumulator() *accumulator {
	return &accumulator{sum: map[string]map[int64]float64{}, count: map[string]map[int64]int{}}
}

func (a *accumulator) add(key string, s promMatrixSeries) {
	if a.sum[key] == nil {
		a.sum[key], a.count[key] = map[int64]float64{}, map[int64]int{}
	}
	for _, p := range s.Values {
		if ts, v, ok := pointValue(p); ok {
			a.sum[key][ts] += v
			a.count[key][ts]++
		}
	}
}

func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

// points 输出按时间升序的点；mean=true 取均值，transform 可换算（如 空闲率→使用率%）。
func (a *accumulator) points(key string, mean bool, transform func(float64) float64) [][2]float64 {
	sums := a.sum[key]
	out := make([][2]float64, 0, len(sums))
	ts := make([]int64, 0, len(sums))
	for t := range sums {
		ts = append(ts, t)
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
	for _, t := range ts {
		v := sums[t]
		if mean && a.count[key][t] > 0 {
			v /= float64(a.count[key][t])
		}
		if transform != nil {
			v = transform(v)
		}
		out = append(out, [2]float64{float64(t), round4(v)})
	}
	return out
}

// podResolver 把 Prometheus 序列的 (namespace, pod) 映射回请求里的目标 Key。
type podResolver struct {
	byPod    map[string]string           // pod 模式：ns/pod → key
	patterns map[string][]workloadMatchT // workload 模式：ns → 候选
}

type workloadMatchT struct {
	key  string
	name string
	re   *regexp.Regexp
}

func newPodResolver(req *sparkRequest) *podResolver {
	r := &podResolver{byPod: map[string]string{}, patterns: map[string][]workloadMatchT{}}
	for _, t := range req.Targets {
		if req.Kind == "pod" {
			r.byPod[t.Namespace+"/"+t.Name] = t.Key
			continue
		}
		re := regexp.MustCompile(`^` + regexp.QuoteMeta(t.Name) + workloadPodSuffix[req.WorkloadKind] + `$`)
		r.patterns[t.Namespace] = append(r.patterns[t.Namespace], workloadMatchT{key: t.Key, name: t.Name, re: re})
	}
	return r
}

func (r *podResolver) resolve(labels map[string]string) (string, bool) {
	ns, pod := labels["namespace"], labels["pod"]
	if ns == "" || pod == "" {
		return "", false
	}
	if key, ok := r.byPod[ns+"/"+pod]; ok {
		return key, true
	}
	best, bestLen := "", -1
	for _, cand := range r.patterns[ns] {
		// 名称互为前缀时取最长匹配（如 api 与 api-v2）。
		if cand.re.MatchString(pod) && len(cand.name) > bestLen {
			best, bestLen = cand.key, len(cand.name)
		}
	}
	return best, bestLen >= 0
}

// nodeResolver 把 node_exporter 序列映射回节点名：优先按 IP（instance 主机部分），
// 其次 node / nodename / kubernetes_node 标签，最后 node_uname_info 的 nodename。
type nodeResolver struct {
	byIP       map[string]string
	byName     map[string]string
	byInstance map[string]string
}

func newNodeResolver(req *sparkRequest) *nodeResolver {
	r := &nodeResolver{byIP: map[string]string{}, byName: map[string]string{}, byInstance: map[string]string{}}
	for _, t := range req.Targets {
		if t.IP != "" {
			r.byIP[t.IP] = t.Key
		}
		r.byName[t.Name] = t.Key
		r.byName[strings.ToLower(t.Name)] = t.Key
	}
	return r
}

func instanceHost(instance string) string {
	if host, _, err := net.SplitHostPort(instance); err == nil {
		return host
	}
	return strings.Trim(instance, "[]")
}

func (r *nodeResolver) learnUname(series []promMatrixSeries) {
	for _, s := range series {
		name := s.Metric["nodename"]
		if key, ok := r.byName[name]; ok && s.Metric["instance"] != "" {
			r.byInstance[s.Metric["instance"]] = key
		} else if key, ok := r.byName[strings.ToLower(name)]; ok && s.Metric["instance"] != "" {
			r.byInstance[s.Metric["instance"]] = key
		}
	}
}

func (r *nodeResolver) resolve(labels map[string]string) (string, bool) {
	inst := labels["instance"]
	if key, ok := r.byIP[instanceHost(inst)]; ok && inst != "" {
		return key, true
	}
	for _, l := range []string{"node", "nodename", "kubernetes_node"} {
		if v := labels[l]; v != "" {
			if key, ok := r.byName[v]; ok {
				return key, true
			}
			if key, ok := r.byName[strings.ToLower(v)]; ok {
				return key, true
			}
		}
	}
	if key, ok := r.byInstance[inst]; ok {
		return key, true
	}
	return "", false
}

// assembleSparklines 把原始序列聚合为每个目标的两条曲线。nodeUname 可为空。
func assembleSparklines(req *sparkRequest, cpu, mem, nodeUname []promMatrixSeries) []SparkSeries {
	cpuAcc, memAcc := newAccumulator(), newAccumulator()
	var resolve func(map[string]string) (string, bool)
	if req.Kind == "node" {
		nr := newNodeResolver(req)
		nr.learnUname(nodeUname)
		resolve = nr.resolve
	} else {
		resolve = newPodResolver(req).resolve
	}
	for _, s := range cpu {
		if key, ok := resolve(s.Metric); ok {
			cpuAcc.add(key, s)
		}
	}
	for _, s := range mem {
		if key, ok := resolve(s.Metric); ok {
			memAcc.add(key, s)
		}
	}
	out := make([]SparkSeries, 0, len(req.Targets))
	for _, t := range req.Targets {
		item := SparkSeries{Name: t.Key}
		if req.Kind == "node" {
			item.CPU = cpuAcc.points(t.Key, true, func(idle float64) float64 {
				return math.Max(0, math.Min(100, 100*(1-idle)))
			})
			// 同一节点被多个抓取任务重复采集时取均值，避免内存使用率翻倍。
			item.Mem = memAcc.points(t.Key, true, nil)
		} else {
			item.CPU = cpuAcc.points(t.Key, false, nil)
			item.Mem = memAcc.points(t.Key, false, nil)
		}
		out = append(out, item)
	}
	return out
}

func emptySparkItems(req *sparkRequest) []SparkSeries {
	out := make([]SparkSeries, 0, len(req.Targets))
	for _, t := range req.Targets {
		out = append(out, SparkSeries{Name: t.Key, CPU: [][2]float64{}, Mem: [][2]float64{}})
	}
	return out
}

func sparkUnits(kind string) sparkUnit {
	if kind == "node" {
		return sparkUnit{CPU: "percent", Mem: "percent"}
	}
	return sparkUnit{CPU: "cores", Mem: "bytes"}
}

// sparkWindowAt 返回按分钟对齐的 [start, end]，共 31 个采样点。
func sparkWindowAt(now time.Time) (time.Time, time.Time) {
	end := now.Truncate(sparkStep)
	return end.Add(-sparkWindow), end
}

// authorizeSpark 校验对象级访问：节点需 node:read；Pod/工作负载需在每个命名空间具备 workload:read。
func authorizeSpark(p *rbac.Principal, req *sparkRequest) error {
	if p == nil {
		return httpx.Unauthorized("authentication required")
	}
	if !p.CanCluster(req.ClusterID) {
		return httpx.Forbidden("无权访问该集群")
	}
	if req.Kind == "node" {
		if !p.Can(permSparkNodeRead) || !p.HasClusterWide(req.ClusterID) {
			return httpx.Forbidden("查看节点趋势需要节点查看权限")
		}
		return nil
	}
	for _, t := range req.Targets {
		if !p.CanIn(permSparkWorkloadRead, req.ClusterID, t.Namespace) {
			return httpx.Forbidden("无权查看命名空间 " + t.Namespace + " 的工作负载")
		}
	}
	return nil
}

// fetchSparklines 并发执行范围查询（共享一个超时上下文）。
func fetchSparklines(ctx context.Context, cl *client, req *sparkRequest, start, end time.Time) ([]SparkSeries, error) {
	ctx, cancel := context.WithTimeout(ctx, sparkTimeout)
	defer cancel()
	qs := buildSparkQueries(req, cl.scope)
	type result struct {
		series []promMatrixSeries
		err    error
	}
	run := func(query string) result {
		env, err := cl.PromRange(ctx, query, fmtUnix(start), fmtUnix(end), strconv.Itoa(int(sparkStep.Seconds())))
		if err != nil {
			return result{err: err}
		}
		s, err := decodeMatrix(env)
		return result{series: s, err: err}
	}
	queries := []string{qs.CPU, qs.Mem}
	if req.Kind == "node" {
		queries = append(queries, `node_uname_info`+joinMatchers(scopeMatcher(cl.scope)))
	}
	results := make([]result, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q string) {
			defer wg.Done()
			results[i] = run(q)
		}(i, q)
	}
	wg.Wait()
	if results[0].err != nil && results[1].err != nil {
		return nil, results[0].err
	}
	var uname []promMatrixSeries
	if len(results) > 2 && results[2].err == nil {
		uname = results[2].series
	}
	return assembleSparklines(req, results[0].series, results[1].series, uname), nil
}

func isNotFound(err error) bool {
	var he *httpx.Error
	return errors.As(err, &he) && he.Status == http.StatusNotFound
}

func (m *Module) sparklines(c *gin.Context) {
	q := map[string]string{}
	for _, k := range []string{"cluster_id", "kind", "workload_kind", "namespace", "names", "ips"} {
		q[k] = c.Query(k)
	}
	req, err := parseSparkRequest(q)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	p := rbac.PrincipalFrom(c)
	if err := authorizeSpark(p, req); err != nil {
		httpx.Fail(c, err)
		return
	}
	if _, err := m.clusterByID(c, p, req.ClusterID); err != nil {
		httpx.Fail(c, err)
		return
	}
	start, end := sparkWindowAt(time.Now())
	resp := SparkResponse{Source: "none", Kind: req.Kind, WorkloadKind: req.WorkloadKind,
		Start: start.Unix(), End: end.Unix(), Step: int(sparkStep.Seconds()), Unit: sparkUnits(req.Kind)}
	ds, scope, err := m.selectSource(c, p, "prometheus", strconv.FormatInt(req.ClusterID, 10))
	if err != nil {
		if isNotFound(err) {
			resp.Items = emptySparkItems(req)
			resp.Note = "集群未接入监控数据源"
			httpx.OK(c, resp)
			return
		}
		httpx.Fail(c, err)
		return
	}
	cl := newClient(ds)
	cl.scope = scope
	items, err := fetchSparklines(c, cl, req, start, end)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	resp.Source, resp.Items = "prometheus", items
	httpx.OK(c, resp)
}
