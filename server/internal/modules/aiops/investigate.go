package aiops

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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/ai"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// Investigation limits. Every single read is bounded by invItemTimeout and the
// whole gathering phase (plus the model call) by invTotalTimeout.
const (
	invItemTimeout  = 10 * time.Second
	invTotalTimeout = 60 * time.Second
	invListLimit    = 500
	invMaxNodes     = 20
	invMaxPlans     = 3
	invSummaryLimit = 600
)

// Engines reported by investigate.
const (
	invEngineLLM   = "llm"
	invEngineRules = "rules"
)

// ---------------------------------------------------------------------------
// Secret masking (package-local on purpose; no cross-package import).

var invSecretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), "-----PRIVATE KEY ******-----"},
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "${1} ******"},
	{regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://[^\s:/@]+):[^\s@/]+@`), "${1}:******@"},
	{regexp.MustCompile(`(?i)("?[a-z0-9_.-]*(?:password|passwd|pwd|token|secret|api[_-]?key|access[_-]?key|private[_-]?key|credential|authorization)[a-z0-9_.-]*"?\s*[:=]\s*"?)([^\s",;}&]+)`), "${1}******"},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), "AKIA******"},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), "******(jwt)"},
}

// invSecretText masks common credential shapes in free text.
func invSecretText(s string) string {
	for _, p := range invSecretPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// invSafeEvidence masks secrets and truncates to limit bytes (rune-safe).
func invSafeEvidence(s string, limit int) string {
	s = invSecretText(s)
	if limit > 0 && len(s) > limit {
		cut := limit
		for cut > 0 && !utf8RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…(已截断)"
	}
	return s
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// ---------------------------------------------------------------------------
// Scope resolution.

type invScope struct {
	ClusterID int64
	Env       string
	Namespace string
	Kind      string // pod | deployment | statefulset | node | ...
	Name      string
	Node      string
	AlertAt   time.Time
	AlertName string
}

type invTarget struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Node      string `json:"node"`
	AssetID   *int64 `json:"asset_id"`
	ClusterID *int64 `json:"cluster_id"`
}

func (m *Module) invResolveScope(ctx context.Context, c *Case, ev *[]EvidenceItem) invScope {
	s := invScope{Env: c.Env}
	if c.ClusterID != nil {
		s.ClusterID = *c.ClusterID
	}
	var t invTarget
	if len(c.Target) > 0 && json.Unmarshal(c.Target, &t) == nil {
		s.Kind, s.Namespace, s.Name, s.Node = t.Kind, t.Namespace, t.Name, t.Node
		if s.ClusterID == 0 && t.ClusterID != nil {
			s.ClusterID = *t.ClusterID
		}
	}
	if c.Kind == KindAlert && c.AlertID != nil {
		m.invAlertScope(ctx, *c.AlertID, &s, ev)
	}
	return s
}

// invAlertScope reads the alert row and fills scope fields from its labels.
func (m *Module) invAlertScope(ctx context.Context, alertID int64, s *invScope, ev *[]EvidenceItem) {
	item := EvidenceItem{Key: "alert", Title: "告警详情", At: time.Now()}
	defer func() { *ev = append(*ev, item) }()
	if m.d == nil || m.d.DB == nil {
		item.Status, item.Summary = "unavailable", "数据库不可用，无法读取告警"
		return
	}
	qctx, cancel := context.WithTimeout(ctx, invItemTimeout)
	defer cancel()
	var (
		name, severity, status, labels, annotations, env string
	)
	err := m.d.DB.QueryRow(qctx, `SELECT a.alertname, a.severity, a.status, COALESCE(a.labels::text,'{}'),
		COALESCE(a.annotations::text,'{}'), COALESCE(d.env,'')
		FROM obs_alerts a JOIN obs_datasources d ON d.id=a.datasource_id WHERE a.id=$1`, alertID).
		Scan(&name, &severity, &status, &labels, &annotations, &env)
	if err != nil {
		item.Status, item.Summary = "unavailable", "读取告警失败："+invSafeEvidence(err.Error(), 200)
		return
	}
	lb := map[string]string{}
	_ = json.Unmarshal([]byte(labels), &lb)
	an := map[string]string{}
	_ = json.Unmarshal([]byte(annotations), &an)
	s.AlertName = name
	s.AlertAt = time.Now()
	if s.Env == "" {
		s.Env = env
	}
	if s.ClusterID == 0 {
		if v, err := strconv.ParseInt(firstNonEmpty(lb["cluster_id"]), 10, 64); err == nil && v > 0 {
			s.ClusterID = v
		}
	}
	if s.Namespace == "" {
		s.Namespace = lb["namespace"]
	}
	if s.Node == "" {
		s.Node = lb["node"]
	}
	if s.Name == "" {
		switch {
		case lb["pod"] != "":
			s.Kind, s.Name = "pod", lb["pod"]
		case lb["deployment"] != "":
			s.Kind, s.Name = "deployment", lb["deployment"]
		case lb["statefulset"] != "":
			s.Kind, s.Name = "statefulset", lb["statefulset"]
		case lb["node"] != "":
			s.Kind, s.Name = "node", lb["node"]
		}
	}
	keep := map[string]string{}
	for _, k := range []string{"cluster_id", "cluster", "namespace", "pod", "deployment", "statefulset", "node", "reason"} {
		if lb[k] != "" {
			keep[k] = invSafeEvidence(lb[k], 200)
		}
	}
	item.Status = "ok"
	if strings.EqualFold(severity, "critical") {
		item.Status = "error"
	} else if strings.EqualFold(severity, "warning") {
		item.Status = "warn"
	}
	desc := firstNonEmpty(an["summary"], an["description"], an["message"])
	item.Summary = invSafeEvidence(fmt.Sprintf("%s（%s，%s）%s", name, severity, status, desc), invSummaryLimit)
	item.Data = map[string]any{"alertname": name, "severity": severity, "status": status, "env": env, "labels": keep}
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Kubernetes gathering.

// invStep runs one bounded read and records an EvidenceItem; a failure becomes
// status "unavailable" and never aborts the investigation.
func invStep(ctx context.Context, ev *[]EvidenceItem, key, title string, fn func(ctx context.Context) (status, summary string, data any, err error)) bool {
	ictx, cancel := context.WithTimeout(ctx, invItemTimeout)
	defer cancel()
	st, sum, data, err := fn(ictx)
	item := EvidenceItem{Key: key, Title: title, At: time.Now()}
	if err != nil {
		item.Status = "unavailable"
		item.Summary = invSafeEvidence("读取失败："+err.Error(), 300)
		*ev = append(*ev, item)
		return false
	}
	item.Status, item.Summary, item.Data = st, invSafeEvidence(sum, invSummaryLimit), data
	*ev = append(*ev, item)
	return true
}

func (m *Module) invGather(ctx context.Context, s invScope, ev *[]EvidenceItem) (AnalyzerInput, bool) {
	in := AnalyzerInput{ClusterID: s.ClusterID, Namespace: s.Namespace, AlertAt: s.AlertAt}
	if s.ClusterID == 0 {
		*ev = append(*ev, EvidenceItem{Key: "k8s", Title: "Kubernetes 快照", Status: "unavailable",
			Summary: "该排查单未关联集群，跳过 Kubernetes 只读检查", At: time.Now()})
		return in, false
	}
	if m.d == nil || m.d.Clusters == nil {
		*ev = append(*ev, EvidenceItem{Key: "k8s", Title: "Kubernetes 快照", Status: "unavailable",
			Summary: "集群访问未就绪", At: time.Now()})
		return in, false
	}
	var cs kubernetes.Interface
	ok := invStep(ctx, ev, "cluster", "集群连接", func(ctx context.Context) (string, string, any, error) {
		ref, err := m.d.Clusters.Cluster(ctx, s.ClusterID)
		if err != nil {
			return "", "", nil, err
		}
		cfg, err := m.d.Clusters.RESTConfig(ctx, s.ClusterID)
		if err != nil {
			return "", "", nil, err
		}
		cfg.Timeout = invItemTimeout
		c, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return "", "", nil, err
		}
		cs = c
		return "ok", fmt.Sprintf("集群 %s（%s）连接正常", ref.Name, ref.Env), map[string]any{"cluster_id": ref.ID, "name": ref.Name, "env": ref.Env}, nil
	})
	if !ok {
		return in, false
	}
	return invCollectK8s(ctx, cs, s, in, ev), true
}

// invCollectK8s gathers the namespaced snapshot and related nodes. Exposed as
// a free function over kubernetes.Interface so tests can use a fake clientset.
func invCollectK8s(ctx context.Context, cs kubernetes.Interface, s invScope, in AnalyzerInput, ev *[]EvidenceItem) AnalyzerInput {
	lo := metav1.ListOptions{Limit: invListLimit}
	ns := s.Namespace
	if ns != "" {
		invStep(ctx, ev, "pod_status", "Pod 状态", func(ctx context.Context) (string, string, any, error) {
			l, err := cs.CoreV1().Pods(ns).List(ctx, lo)
			if err != nil {
				return "", "", nil, err
			}
			in.Pods = l.Items
			return invPodSummary(l.Items)
		})
		invStep(ctx, ev, "events", "事件", func(ctx context.Context) (string, string, any, error) {
			l, err := cs.CoreV1().Events(ns).List(ctx, lo)
			if err != nil {
				return "", "", nil, err
			}
			in.Events = l.Items
			return invEventSummary(l.Items)
		})
		invStep(ctx, ev, "deployments", "Deployment / ReplicaSet", func(ctx context.Context) (string, string, any, error) {
			l, err := cs.AppsV1().Deployments(ns).List(ctx, lo)
			if err != nil {
				return "", "", nil, err
			}
			in.Deployments = l.Items
			rs, err := cs.AppsV1().ReplicaSets(ns).List(ctx, lo)
			if err == nil {
				in.ReplicaSets = rs.Items
			}
			var rows []map[string]any
			bad := 0
			for _, d := range l.Items {
				want := int32(1)
				if d.Spec.Replicas != nil {
					want = *d.Spec.Replicas
				}
				if d.Status.AvailableReplicas < want {
					bad++
				}
				if len(rows) < 50 {
					rows = append(rows, map[string]any{"name": d.Name, "replicas": want, "ready": d.Status.ReadyReplicas, "available": d.Status.AvailableReplicas, "updated": d.Status.UpdatedReplicas})
				}
			}
			st := "ok"
			if bad > 0 {
				st = "warn"
			}
			sum := fmt.Sprintf("%d 个 Deployment，%d 个可用副本不足；%d 个 ReplicaSet", len(l.Items), bad, len(in.ReplicaSets))
			if err != nil {
				sum += "（ReplicaSet 读取失败）"
			}
			return st, sum, rows, nil
		})
		invStep(ctx, ev, "pvcs", "存储卷声明", func(ctx context.Context) (string, string, any, error) {
			l, err := cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, lo)
			if err != nil {
				return "", "", nil, err
			}
			in.PVCs = l.Items
			pending := 0
			for _, p := range l.Items {
				if p.Status.Phase != corev1.ClaimBound {
					pending++
				}
			}
			st := "ok"
			if pending > 0 {
				st = "warn"
			}
			return st, fmt.Sprintf("%d 个 PVC，%d 个未绑定", len(l.Items), pending), nil, nil
		})
		invStep(ctx, ev, "config_refs", "ConfigMap / Secret 名称", func(ctx context.Context) (string, string, any, error) {
			cms, err := cs.CoreV1().ConfigMaps(ns).List(ctx, lo)
			if err != nil {
				return "", "", nil, err
			}
			for _, c := range cms.Items {
				in.ConfigMaps = append(in.ConfigMaps, c.Name)
			}
			// Only names are kept; secret data is never read into evidence.
			secs, err := cs.CoreV1().Secrets(ns).List(ctx, lo)
			if err != nil {
				return "", "", nil, err
			}
			for _, sc := range secs.Items {
				in.Secrets = append(in.Secrets, sc.Name)
			}
			return "ok", fmt.Sprintf("%d 个 ConfigMap，%d 个 Secret（仅名称）", len(in.ConfigMaps), len(in.Secrets)), nil, nil
		})
	} else {
		*ev = append(*ev, EvidenceItem{Key: "namespace", Title: "命名空间", Status: "unavailable",
			Summary: "未能确定命名空间，跳过命名空间内资源检查", At: time.Now()})
	}

	// Related nodes: the alert/target node plus nodes hosting the pods.
	names := map[string]bool{}
	if s.Node != "" {
		names[s.Node] = true
	}
	if s.Kind == "node" && s.Name != "" {
		names[s.Name] = true
	}
	for _, p := range in.Pods {
		if p.Spec.NodeName != "" && len(names) < invMaxNodes {
			names[p.Spec.NodeName] = true
		}
	}
	if len(names) > 0 {
		list := make([]string, 0, len(names))
		for n := range names {
			list = append(list, n)
		}
		sort.Strings(list)
		invStep(ctx, ev, "node_status", "相关节点", func(ctx context.Context) (string, string, any, error) {
			var rows []map[string]any
			notReady, failed := 0, 0
			var lastErr error
			for _, n := range list {
				node, err := cs.CoreV1().Nodes().Get(ctx, n, metav1.GetOptions{})
				if err != nil {
					failed++
					lastErr = err
					continue
				}
				in.Nodes = append(in.Nodes, *node)
				ready, pressure := "Unknown", []string{}
				for _, c := range node.Status.Conditions {
					if c.Type == corev1.NodeReady {
						ready = string(c.Status)
					} else if c.Status == corev1.ConditionTrue {
						pressure = append(pressure, string(c.Type))
					}
				}
				if ready != "True" {
					notReady++
				}
				rows = append(rows, map[string]any{"name": node.Name, "ready": ready, "conditions": pressure, "unschedulable": node.Spec.Unschedulable})
			}
			if failed == len(list) && lastErr != nil {
				return "", "", nil, lastErr
			}
			st := "ok"
			if notReady > 0 {
				st = "error"
			}
			return st, fmt.Sprintf("检查 %d 个节点，%d 个未就绪，%d 个读取失败", len(list), notReady, failed), rows, nil
		})
	}
	return in
}

func invPodSummary(pods []corev1.Pod) (string, string, any, error) {
	var rows []map[string]any
	unhealthy := 0
	for _, p := range pods {
		restarts := int32(0)
		reasons := []string{}
		ready := true
		for _, c := range p.Status.ContainerStatuses {
			restarts += c.RestartCount
			if !c.Ready {
				ready = false
			}
			if c.State.Waiting != nil && c.State.Waiting.Reason != "" {
				reasons = append(reasons, c.State.Waiting.Reason)
			}
			if c.LastTerminationState.Terminated != nil && c.LastTerminationState.Terminated.Reason != "" {
				reasons = append(reasons, "last:"+c.LastTerminationState.Terminated.Reason)
			}
		}
		bad := p.Status.Phase != corev1.PodRunning && p.Status.Phase != corev1.PodSucceeded || !ready && p.Status.Phase != corev1.PodSucceeded
		if bad {
			unhealthy++
		}
		if bad || restarts > 0 {
			if len(rows) < 50 {
				rows = append(rows, map[string]any{"name": p.Name, "phase": string(p.Status.Phase), "node": p.Spec.NodeName, "restarts": restarts, "reasons": reasons})
			}
		}
	}
	st := "ok"
	if unhealthy > 0 {
		st = "warn"
	}
	return st, fmt.Sprintf("%d 个 Pod，%d 个异常", len(pods), unhealthy), rows, nil
}

func invEventSummary(evs []corev1.Event) (string, string, any, error) {
	var warn []corev1.Event
	for _, e := range evs {
		if e.Type == corev1.EventTypeWarning {
			warn = append(warn, e)
		}
	}
	sort.Slice(warn, func(i, j int) bool { return invEventTime(warn[i]).After(invEventTime(warn[j])) })
	var lines []string
	for _, e := range warn {
		if len(lines) >= 30 {
			break
		}
		lines = append(lines, invSafeEvidence(fmt.Sprintf("%s %s/%s %s: %s (x%d)", invEventTime(e).Format(time.RFC3339),
			e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Reason, e.Message, e.Count), 300))
	}
	st := "ok"
	if len(warn) > 0 {
		st = "warn"
	}
	return st, fmt.Sprintf("%d 条事件，其中 %d 条 Warning", len(evs), len(warn)), lines, nil
}

func invEventTime(e corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	default:
		return e.CreationTimestamp.Time
	}
}

// ---------------------------------------------------------------------------
// investigate.

// investigate gathers read-only evidence for c and returns the conclusion and
// plan drafts. engine is "llm" (model output validated) or "rules". It does not
// change state and does not persist anything.
func (m *Module) investigate(ctx context.Context, c *Case) (Conclusion, []PlanDraft, []EvidenceItem, string, error) {
	if c == nil {
		return Conclusion{}, nil, nil, invEngineRules, errors.New("nil case")
	}
	ctx, cancel := context.WithTimeout(ctx, invTotalTimeout)
	defer cancel()

	ev := []EvidenceItem{{Key: "request", Title: "排查请求", Status: "ok",
		Summary: invSafeEvidence(firstNonEmpty(c.Request, c.Title), invSummaryLimit), At: time.Now(),
		Data: map[string]any{"kind": c.Kind, "source": c.Source, "env": c.Env}}}

	s := m.invResolveScope(ctx, c, &ev)
	if c.Kind == KindDeploy && s.ClusterID == 0 {
		ev = append(ev, EvidenceItem{Key: "deploy_target", Title: "部署目标", Status: "ok", At: time.Now(),
			Summary: invSafeEvidence("目标："+string(c.Target), 300)})
	}
	in, _ := m.invGather(ctx, s, &ev)

	findings := runAnalyzers(in)
	concl, plans := aggregateFindings(findings)
	concl.Findings = findings
	plans = invFilterPlans(plans)

	modelID := m.invModelID(ctx)
	if modelID <= 0 || m.d == nil || m.d.AI == nil {
		return concl, plans, ev, invEngineRules, nil
	}
	text, err := m.invAskModel(ctx, c, modelID, ev, concl, plans)
	if err != nil {
		ev = append(ev, EvidenceItem{Key: "model", Title: "模型分析", Status: "unavailable",
			Summary: invSafeEvidence("模型调用失败，已使用规则引擎结论："+err.Error(), 300), At: time.Now()})
		return concl, plans, ev, invEngineRules, nil
	}
	fc, fp, engine := invApplyModel(text, concl, plans)
	if engine == invEngineRules {
		ev = append(ev, EvidenceItem{Key: "model", Title: "模型分析", Status: "unavailable",
			Summary: "模型输出不是合法 JSON，已使用规则引擎结论", At: time.Now()})
	}
	return fc, fp, ev, engine, nil
}

// invModelID reads the configured model from the policy (policy.go).
func (m *Module) invModelID(ctx context.Context) int64 {
	pol, err := m.loadPolicy(ctx)
	if err != nil {
		return 0
	}
	// Policy.ModelID is a string; ai.ChatRequest wants the numeric model id.
	id, err := strconv.ParseInt(strings.TrimSpace(pol.ModelID), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// invApplyModel validates model output. On parse failure it returns the rule
// result with engine "rules". Unregistered actions are dropped step by step.
func invApplyModel(text string, ruleConcl Conclusion, rulePlans []PlanDraft) (Conclusion, []PlanDraft, string) {
	out, err := invParseModelOutput(text)
	if err != nil || strings.TrimSpace(out.Conclusion.Summary) == "" {
		return ruleConcl, rulePlans, invEngineRules
	}
	concl := out.Conclusion
	concl.Findings = ruleConcl.Findings
	if concl.Confidence == "" {
		concl.Confidence = "low"
	}
	plans := invFilterPlans(out.Plans)
	if len(plans) == 0 {
		plans = rulePlans
	}
	return concl, plans, invEngineLLM
}

// invFilterPlans drops steps whose action is not registered, then drops plans
// left with no main steps, and caps the count.
func invFilterPlans(in []PlanDraft) []PlanDraft {
	var out []PlanDraft
	for _, p := range in {
		p.Steps = invKnownSteps(p.Steps)
		p.Verify = invKnownSteps(p.Verify)
		if len(p.Steps) == 0 {
			continue
		}
		out = append(out, p)
		if len(out) >= invMaxPlans {
			break
		}
	}
	return out
}

func invKnownSteps(steps []StepDraft) []StepDraft {
	var out []StepDraft
	for _, s := range steps {
		if _, ok := ActionByName(strings.TrimSpace(s.Action)); !ok {
			continue
		}
		s.Action = strings.TrimSpace(s.Action)
		if len(s.Params) == 0 || !json.Valid(s.Params) {
			s.Params = json.RawMessage("{}")
		}
		out = append(out, s)
	}
	return out
}

// invAskModel sends masked evidence plus the rule verdict and collects the text.
func (m *Module) invAskModel(ctx context.Context, c *Case, modelID int64, ev []EvidenceItem, concl Conclusion, plans []PlanDraft) (string, error) {
	rejections := m.invRejections(ctx, c.ID)
	user, err := invUserPrompt(c, ev, concl, plans)
	if err != nil {
		return "", err
	}
	em := &invEmitter{}
	p := &rbac.Principal{Username: "aiops-investigate", Permissions: []string{"ai:chat"}, EnvScope: []string{c.Env}}
	err = m.d.AI.Chat(ctx, p, ai.ChatRequest{Scene: "aiops.investigate", ModelID: modelID, MaxTokens: 3000,
		Tools: json.RawMessage("false"), Messages: []ai.Message{
			ai.TextMessage("system", invSystemPrompt(ActionSpecs(), rejections)),
			ai.TextMessage("user", user),
		}}, em)
	if err != nil {
		return "", err
	}
	return em.text.String(), nil
}

type invEmitter struct{ text strings.Builder }

func (e *invEmitter) Event(string, any) error { return nil }
func (e *invEmitter) Chunk(b []byte) error {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(b, &chunk) == nil {
		for _, c := range chunk.Choices {
			if e.text.Len()+len(c.Delta.Content) > 40000 {
				return errors.New("investigation output too large")
			}
			e.text.WriteString(c.Delta.Content)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Assistant tools.

func (m *Module) registerAITools() {
	if m.d == nil || m.d.AITools == nil {
		return
	}
	reg := func(t ai.Tool) {
		t.Module = "aiops"
		if err := m.d.AITools.Register(t); err != nil && m.d.Logger != nil {
			m.d.Logger.Warn("register ai tool", "tool", t.Name, "err", err)
		}
	}
	reg(ai.Tool{
		Name:        "aiops_start_case",
		Description: "发起一个 AI 排查单（只读调查，平台生成结论与候选方案，需人工审阅后才会执行）。kind: alert（告警）/deploy（部署）/request（其他运维请求）。",
		Permission:  PermHandle,
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"kind":{"type":"string","enum":["alert","deploy","request"]},
			"title":{"type":"string"},
			"request":{"type":"string","description":"操作员的原话或告警摘要，不要包含密码等凭据"},
			"env":{"type":"string"},
			"cluster_id":{"type":"integer"},
			"alert_id":{"type":"integer"},
			"asset_id":{"type":"integer"},
			"target":{"type":"object","description":"{\"kind\":\"deployment\",\"namespace\":\"\",\"name\":\"\"} 或 {\"asset_id\":1}"}},
			"required":["kind","request"]}`),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p := rbac.FromContext(ctx)
			if p == nil || !p.Can(PermHandle) {
				return nil, errors.New("permission denied: " + PermHandle)
			}
			in, err := invParseStartArgs(raw)
			if err != nil {
				return nil, err
			}
			if in.Env != "" && !p.CanEnv(in.Env) {
				return nil, errors.New("permission denied for env " + in.Env)
			}
			res, err := m.createCase(ctx, p, in)
			if err != nil {
				return nil, err
			}
			return map[string]any{"case": res, "note": "排查已开始，可用 aiops_case_status 查询进度"}, nil
		},
	})
	reg(ai.Tool{
		Name:        "aiops_case_status",
		Description: "查询 AI 排查单的状态、结论摘要与候选方案。",
		Permission:  PermRead,
		Parameters:  json.RawMessage(`{"type":"object","properties":{"case_id":{"type":"integer"}},"required":["case_id"]}`),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p := rbac.FromContext(ctx)
			if p == nil || !p.Can(PermRead) {
				return nil, errors.New("permission denied: " + PermRead)
			}
			var a struct {
				CaseID int64 `json:"case_id"`
			}
			if err := json.Unmarshal(raw, &a); err != nil || a.CaseID <= 0 {
				return nil, errors.New("case_id is required")
			}
			c, err := m.getCase(ctx, a.CaseID)
			if err != nil {
				return nil, err
			}
			if c.Env != "" && !p.CanEnv(c.Env) {
				return nil, errors.New("permission denied for env " + c.Env)
			}
			var concl Conclusion
			_ = json.Unmarshal(c.Conclusion, &concl)
			out := map[string]any{"id": c.ID, "title": c.Title, "status": c.Status, "engine": c.Engine,
				"summary": concl.Summary, "root_cause": concl.RootCause, "confidence": concl.Confidence, "error": c.Error}
			if plans, err := m.listPlans(ctx, c.ID); err == nil {
				var ps []map[string]any
				for _, pl := range plans {
					ps = append(ps, map[string]any{"id": pl.ID, "title": pl.Title, "risk": pl.Risk, "status": pl.Status, "steps": len(pl.Steps)})
				}
				out["plans"] = ps
			}
			return out, nil
		},
	})
}

func invParseStartArgs(raw json.RawMessage) (CreateCaseInput, error) {
	var a struct {
		Kind      string          `json:"kind"`
		Title     string          `json:"title"`
		Request   string          `json:"request"`
		Env       string          `json:"env"`
		ClusterID *int64          `json:"cluster_id"`
		AlertID   *int64          `json:"alert_id"`
		AssetID   *int64          `json:"asset_id"`
		Target    json.RawMessage `json:"target"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return CreateCaseInput{}, err
	}
	switch a.Kind {
	case KindAlert, KindDeploy, KindRequest:
	case "":
		a.Kind = KindRequest
	default:
		return CreateCaseInput{}, fmt.Errorf("unknown kind %q", a.Kind)
	}
	if a.Kind == KindAlert && a.AlertID == nil {
		return CreateCaseInput{}, errors.New("alert_id is required for kind=alert")
	}
	a.Request = strings.TrimSpace(invSecretText(a.Request))
	if a.Request == "" && a.Title == "" {
		return CreateCaseInput{}, errors.New("request is required")
	}
	title := strings.TrimSpace(a.Title)
	if title == "" {
		title = invSafeEvidence(a.Request, 80)
	}
	var target []byte
	if len(a.Target) > 0 && string(a.Target) != "null" {
		target = a.Target
	}
	return CreateCaseInput{Kind: a.Kind, Source: SourceChat, Title: title, Request: a.Request, Env: a.Env,
		ClusterID: a.ClusterID, AlertID: a.AlertID, AssetID: a.AssetID, Target: target}, nil
}
