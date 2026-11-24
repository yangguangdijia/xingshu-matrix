package aiops

// actions_k8s.go (owner: ai-k8s) — Kubernetes actions for AI operations
// plans. Every action talks to the cluster through the platform's cluster
// channel (Deps.Clusters.RESTConfig) and never shells out to kubectl.
//
// Read actions:   k8s.get_pods, k8s.describe, k8s.logs, k8s.events, k8s.wait_ready
// Change actions: k8s.restart, k8s.scale, k8s.set_resources, k8s.set_image,
//                 k8s.rollback_deployment
// High actions:   k8s.cordon, k8s.uncordon, k8s.drain, k8s.delete_pod, k8s.apply_yaml
//
// Mutating actions append a state line ("#state:{json}") to their Execute
// output; Rollback reads it back to restore the original value.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"
)

const (
	k8sLogTailLines      = 200
	k8sLogLimitBytes     = 4096
	k8sOutputLimit       = 60 << 10 // leaves room for the state line within 64 KiB
	k8sFieldManager      = "xingshu-aiops"
	k8sStateMarker       = "\n#state:"
	k8sRestartAnnotation = "kubectl.kubernetes.io/restartedAt"
	k8sRevisionAnno      = "deployment.kubernetes.io/revision"
	k8sMirrorAnno        = "kubernetes.io/config.mirror"
	k8sMaxApplyObjects   = 20
	k8sMaxYAMLBytes      = 256 << 10
	k8sMaxReplicas       = 1000
)

// Test seams.
var (
	k8sClientFor = func(ctx context.Context, ec *ExecContext) (kubernetes.Interface, error) {
		cfg, err := k8sRESTConfig(ctx, ec)
		if err != nil {
			return nil, err
		}
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("创建 Kubernetes 客户端失败：%w", err)
		}
		return cs, nil
	}
	k8sDynamicFor = func(ctx context.Context, ec *ExecContext) (dynamic.Interface, meta.RESTMapper, error) {
		cfg, err := k8sRESTConfig(ctx, ec)
		if err != nil {
			return nil, nil, err
		}
		dyn, err := dynamic.NewForConfig(cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("创建 Kubernetes 动态客户端失败：%w", err)
		}
		dc, err := discovery.NewDiscoveryClientForConfig(cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("创建 Kubernetes 发现客户端失败：%w", err)
		}
		return dyn, restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc)), nil
	}
	k8sPollInterval = 2 * time.Second
	k8sNow          = time.Now
)

func k8sRESTConfig(ctx context.Context, ec *ExecContext) (*rest.Config, error) {
	if ec == nil || ec.Deps == nil || ec.Deps.Clusters == nil {
		return nil, errors.New("集群服务不可用")
	}
	if ec.ClusterID == 0 {
		return nil, errors.New("该排查单未关联集群，无法执行 Kubernetes 操作")
	}
	cfg, err := ec.Deps.Clusters.RESTConfig(ctx, ec.ClusterID)
	if err != nil {
		return nil, fmt.Errorf("获取集群 %d 的连接配置失败：%w", ec.ClusterID, err)
	}
	return cfg, nil
}

// ---------------------------------------------------------------- params

type k8sParams struct {
	Kind               string            `json:"kind,omitempty"`
	Namespace          string            `json:"namespace,omitempty"`
	Name               string            `json:"name,omitempty"`
	Container          string            `json:"container,omitempty"`
	Selector           string            `json:"selector,omitempty"`
	Type               string            `json:"type,omitempty"`
	Previous           bool              `json:"previous,omitempty"`
	TailLines          int64             `json:"tail_lines,omitempty"`
	Replicas           *int32            `json:"replicas,omitempty"`
	Image              string            `json:"image,omitempty"`
	Requests           map[string]string `json:"requests,omitempty"`
	Limits             map[string]string `json:"limits,omitempty"`
	Revision           int64             `json:"revision,omitempty"`
	TimeoutSec         int               `json:"timeout_sec,omitempty"`
	GracePeriodSec     *int64            `json:"grace_period_sec,omitempty"`
	Force              bool              `json:"force,omitempty"`
	DeleteEmptyDirData bool              `json:"delete_emptydir_data,omitempty"`
	YAML               string            `json:"yaml,omitempty"`
}

var k8sKindAlias = map[string]string{
	"pod": "pod", "pods": "pod", "po": "pod",
	"deployment": "deployment", "deployments": "deployment", "deploy": "deployment",
	"statefulset": "statefulset", "statefulsets": "statefulset", "sts": "statefulset",
	"daemonset": "daemonset", "daemonsets": "daemonset", "ds": "daemonset",
	"node": "node", "nodes": "node", "no": "node",
	"service": "service", "services": "service", "svc": "service",
	"pvc": "pvc", "persistentvolumeclaim": "pvc", "persistentvolumeclaims": "pvc",
}

var k8sKindTitle = map[string]string{
	"pod": "Pod", "deployment": "Deployment", "statefulset": "StatefulSet", "daemonset": "DaemonSet",
	"node": "Node", "service": "Service", "pvc": "PersistentVolumeClaim",
}

var k8sKindCN = map[string]string{
	"pod": "Pod", "deployment": "无状态工作负载 Deployment", "statefulset": "有状态工作负载 StatefulSet",
	"daemonset": "守护进程集 DaemonSet", "node": "节点", "service": "服务 Service", "pvc": "存储卷声明 PVC",
}

func decodeK8sParams(raw json.RawMessage) (*k8sParams, error) {
	p := &k8sParams{}
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return p, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(p); err != nil {
		return nil, fmt.Errorf("参数格式错误：%v", err)
	}
	p.Kind = strings.ToLower(strings.TrimSpace(p.Kind))
	if p.Kind != "" {
		k, ok := k8sKindAlias[p.Kind]
		if !ok {
			return nil, fmt.Errorf("不支持的资源类型「%s」", p.Kind)
		}
		p.Kind = k
	}
	p.Namespace = strings.TrimSpace(p.Namespace)
	p.Name = strings.TrimSpace(p.Name)
	p.Container = strings.TrimSpace(p.Container)
	p.Image = strings.TrimSpace(p.Image)
	p.Selector = strings.TrimSpace(p.Selector)
	return p, nil
}

func k8sRequireName(p *k8sParams, what string) error {
	if p.Name == "" {
		return fmt.Errorf("缺少参数 name（%s名称）", what)
	}
	if errs := validation.IsDNS1123Subdomain(p.Name); len(errs) > 0 {
		return fmt.Errorf("%s名称「%s」不合法：%s", what, p.Name, strings.Join(errs, "；"))
	}
	return nil
}

func k8sRequireNamespace(p *k8sParams) error {
	if p.Namespace == "" {
		p.Namespace = "default"
	}
	if errs := validation.IsDNS1123Label(p.Namespace); len(errs) > 0 {
		return fmt.Errorf("命名空间「%s」不合法：%s", p.Namespace, strings.Join(errs, "；"))
	}
	return nil
}

func k8sRequireKind(p *k8sParams, allowed ...string) error {
	if p.Kind == "" {
		if len(allowed) == 1 {
			p.Kind = allowed[0]
			return nil
		}
		return fmt.Errorf("缺少参数 kind，可选：%s", strings.Join(allowed, "、"))
	}
	for _, k := range allowed {
		if p.Kind == k {
			return nil
		}
	}
	return fmt.Errorf("该操作不支持资源类型「%s」，可选：%s", p.Kind, strings.Join(allowed, "、"))
}

func k8sRequireTimeout(p *k8sParams, def, min, max int) error {
	if p.TimeoutSec == 0 {
		p.TimeoutSec = def
	}
	if p.TimeoutSec < min || p.TimeoutSec > max {
		return fmt.Errorf("超时时间 timeout_sec 须在 %d–%d 秒之间", min, max)
	}
	return nil
}

func k8sIsProdEnv(ec *ExecContext) bool {
	if ec == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(ec.Env)) {
	case "prod", "production", "prd", "生产":
		return true
	}
	return false
}

// ---------------------------------------------------------------- generic action

type k8sFn func(ctx context.Context, ec *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, error)

type k8sAction struct {
	spec     ActionSpec
	check    func(p *k8sParams) error
	escalate func(ec *ExecContext, p *k8sParams) string // optional extra risk
	title    func(p *k8sParams) string
	manual   func(p *k8sParams) string
	untyped  bool  // uses the dynamic client itself (apply_yaml); cs is nil
	preview  k8sFn // mutating actions: human-readable Chinese diff
	run      func(ctx context.Context, ec *ExecContext, cs kubernetes.Interface, p *k8sParams) (out string, state any, err error)
	undo     func(ctx context.Context, ec *ExecContext, cs kubernetes.Interface, p *k8sParams, state json.RawMessage) (string, error)
}

func (a *k8sAction) Name() string     { return a.spec.Name }
func (a *k8sAction) Spec() ActionSpec { return a.spec }

func (a *k8sAction) parse(raw json.RawMessage) (*k8sParams, error) {
	p, err := decodeK8sParams(raw)
	if err != nil {
		return nil, err
	}
	if err := a.check(p); err != nil {
		return nil, err
	}
	return p, nil
}

func (a *k8sAction) Validate(_ context.Context, ec *ExecContext, raw json.RawMessage) (json.RawMessage, string, error) {
	p, err := a.parse(raw)
	if err != nil {
		return nil, "", err
	}
	risk := a.spec.Risk
	if a.escalate != nil {
		risk = MaxRisk(risk, a.escalate(ec, p))
	}
	if risk == RiskChange && k8sIsProdEnv(ec) {
		risk = RiskHigh
	}
	out, err := json.Marshal(p)
	if err != nil {
		return nil, "", err
	}
	return out, risk, nil
}

func (a *k8sAction) client(ctx context.Context, ec *ExecContext) (kubernetes.Interface, error) {
	if a.untyped {
		return nil, nil
	}
	return k8sClientFor(ctx, ec)
}

func (a *k8sAction) Describe(ctx context.Context, ec *ExecContext, raw json.RawMessage) (string, string, string, error) {
	p, err := a.parse(raw)
	if err != nil {
		return "", "", "", err
	}
	title, manual := a.title(p), a.manual(p)
	if a.spec.ReadOnly || a.preview == nil {
		return title, "只读操作，不会修改集群。", manual, nil
	}
	cs, err := a.client(ctx, ec)
	if err != nil {
		return title, "", manual, err
	}
	preview, err := a.preview(ctx, ec, cs, p)
	if err != nil {
		return title, "", manual, err
	}
	return title, preview, manual, nil
}

func (a *k8sAction) Execute(ctx context.Context, ec *ExecContext, raw json.RawMessage) (string, error) {
	p, err := a.parse(raw)
	if err != nil {
		return "", err
	}
	cs, err := a.client(ctx, ec)
	if err != nil {
		return "", err
	}
	out, state, err := a.run(ctx, ec, cs, p)
	out = k8sTruncateOutput(k8sMaskSecrets(out), k8sOutputLimit)
	if state != nil {
		b, merr := json.Marshal(state)
		if merr == nil {
			out += k8sStateMarker + string(b)
		}
	}
	return out, err
}

func (a *k8sAction) Rollback(ctx context.Context, ec *ExecContext, raw json.RawMessage, executeOutput string) (string, error) {
	if a.undo == nil {
		return "", ErrNoRollback
	}
	p, err := a.parse(raw)
	if err != nil {
		return "", err
	}
	state, ok := k8sExtractState(executeOutput)
	if !ok {
		return "", errors.New("执行结果中缺少原始状态记录，无法自动回滚")
	}
	cs, err := a.client(ctx, ec)
	if err != nil {
		return "", err
	}
	return a.undo(ctx, ec, cs, p, state)
}

func k8sExtractState(out string) (json.RawMessage, bool) {
	i := strings.LastIndex(out, k8sStateMarker)
	if i < 0 {
		return nil, false
	}
	s := strings.TrimSpace(out[i+len(k8sStateMarker):])
	if s == "" || !json.Valid([]byte(s)) {
		return nil, false
	}
	return json.RawMessage(s), true
}

// StripK8sState removes the internal state line from an Execute output (for display).
func StripK8sState(out string) string {
	if i := strings.LastIndex(out, k8sStateMarker); i >= 0 {
		return out[:i]
	}
	return out
}

func k8sTruncateOutput(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && cut < len(s) && (s[cut]&0xC0) == 0x80 { // don't split a UTF-8 rune
		cut--
	}
	return s[:cut] + "\n…（输出已截断）"
}

var (
	k8sSecretKVRe = regexp.MustCompile(`(?i)((?:password|passwd|pwd|token|secret|api[_-]?key|access[_-]?key)["']?\s*[:=]\s*["']?)([^\s"',;&]+)`)
	k8sBearerRe   = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`)
	k8sSecretName = regexp.MustCompile(`(?i)(password|passwd|pwd|token|secret|api[_-]?key|access[_-]?key|credential)`)
)

func k8sMaskSecrets(s string) string {
	s = k8sSecretKVRe.ReplaceAllString(s, "${1}***")
	return k8sBearerRe.ReplaceAllString(s, "${1}***")
}

func k8sErr(err error, what string) error {
	if err == nil {
		return nil
	}
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("未找到%s", what)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("无权操作%s：%v", what, err)
	case apierrors.IsConflict(err):
		return fmt.Errorf("%s已被其他人修改，请重试：%v", what, err)
	}
	return fmt.Errorf("%s操作失败：%v", what, err)
}

func k8sObjName(p *k8sParams) string {
	if p.Kind == "node" {
		return fmt.Sprintf("%s %s", k8sKindCN[p.Kind], p.Name)
	}
	return fmt.Sprintf("%s %s/%s", k8sKindCN[p.Kind], p.Namespace, p.Name)
}

func k8sKubectlKind(k string) string {
	switch k {
	case "pvc":
		return "pvc"
	}
	return k
}

// ---------------------------------------------------------------- workloads

func k8sGetWorkload(ctx context.Context, cs kubernetes.Interface, p *k8sParams) (runtime.Object, *corev1.PodTemplateSpec, error) {
	switch p.Kind {
	case "deployment":
		d, err := cs.AppsV1().Deployments(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
		if err != nil {
			return nil, nil, k8sErr(err, k8sObjName(p))
		}
		return d, &d.Spec.Template, nil
	case "statefulset":
		s, err := cs.AppsV1().StatefulSets(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
		if err != nil {
			return nil, nil, k8sErr(err, k8sObjName(p))
		}
		return s, &s.Spec.Template, nil
	case "daemonset":
		d, err := cs.AppsV1().DaemonSets(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
		if err != nil {
			return nil, nil, k8sErr(err, k8sObjName(p))
		}
		return d, &d.Spec.Template, nil
	}
	return nil, nil, fmt.Errorf("不支持的工作负载类型「%s」", p.Kind)
}

func k8sUpdateWorkload(ctx context.Context, cs kubernetes.Interface, obj runtime.Object) error {
	var err error
	switch o := obj.(type) {
	case *appsv1.Deployment:
		_, err = cs.AppsV1().Deployments(o.Namespace).Update(ctx, o, metav1.UpdateOptions{FieldManager: k8sFieldManager})
	case *appsv1.StatefulSet:
		_, err = cs.AppsV1().StatefulSets(o.Namespace).Update(ctx, o, metav1.UpdateOptions{FieldManager: k8sFieldManager})
	case *appsv1.DaemonSet:
		_, err = cs.AppsV1().DaemonSets(o.Namespace).Update(ctx, o, metav1.UpdateOptions{FieldManager: k8sFieldManager})
	default:
		err = fmt.Errorf("unsupported workload %T", obj)
	}
	return err
}

// k8sMutateWorkload re-reads and retries on conflict; fn may run more than once.
func k8sMutateWorkload(ctx context.Context, cs kubernetes.Interface, p *k8sParams, fn func(obj runtime.Object, t *corev1.PodTemplateSpec) error) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj, t, err := k8sGetWorkload(ctx, cs, p)
		if err != nil {
			return err
		}
		if err := fn(obj, t); err != nil {
			return err
		}
		return k8sUpdateWorkload(ctx, cs, obj)
	})
	var st apierrors.APIStatus
	if err != nil && errors.As(err, &st) {
		return k8sErr(err, k8sObjName(p))
	}
	return err
}

func k8sGetReplicas(obj runtime.Object) int32 {
	var r *int32
	switch o := obj.(type) {
	case *appsv1.Deployment:
		r = o.Spec.Replicas
	case *appsv1.StatefulSet:
		r = o.Spec.Replicas
	}
	if r == nil {
		return 1
	}
	return *r
}

func k8sSetReplicas(obj runtime.Object, n int32) {
	switch o := obj.(type) {
	case *appsv1.Deployment:
		o.Spec.Replicas = &n
	case *appsv1.StatefulSet:
		o.Spec.Replicas = &n
	}
}

func k8sWorkloadSelector(obj runtime.Object) (labels.Selector, error) {
	var ls *metav1.LabelSelector
	switch o := obj.(type) {
	case *appsv1.Deployment:
		ls = o.Spec.Selector
	case *appsv1.StatefulSet:
		ls = o.Spec.Selector
	case *appsv1.DaemonSet:
		ls = o.Spec.Selector
	}
	if ls == nil {
		return nil, errors.New("工作负载没有标签选择器")
	}
	return metav1.LabelSelectorAsSelector(ls)
}

func k8sFindContainer(t *corev1.PodTemplateSpec, name string) (*corev1.Container, error) {
	cs := t.Spec.Containers
	if name == "" {
		if len(cs) == 1 {
			return &cs[0], nil
		}
		names := make([]string, 0, len(cs))
		for _, c := range cs {
			names = append(names, c.Name)
		}
		return nil, fmt.Errorf("工作负载有多个容器（%s），请指定 container", strings.Join(names, "、"))
	}
	for i := range cs {
		if cs[i].Name == name {
			return &cs[i], nil
		}
	}
	return nil, fmt.Errorf("未找到容器「%s」", name)
}

// k8sWorkloadReady reports readiness and a Chinese progress line.
func k8sWorkloadReady(obj runtime.Object) (bool, string) {
	switch o := obj.(type) {
	case *appsv1.Deployment:
		want := int32(1)
		if o.Spec.Replicas != nil {
			want = *o.Spec.Replicas
		}
		s := o.Status
		ok := s.ObservedGeneration >= o.Generation && s.UpdatedReplicas == want && s.ReadyReplicas == want &&
			s.AvailableReplicas == want && s.Replicas == want
		return ok, fmt.Sprintf("期望 %d，已更新 %d，就绪 %d，可用 %d，总数 %d", want, s.UpdatedReplicas, s.ReadyReplicas, s.AvailableReplicas, s.Replicas)
	case *appsv1.StatefulSet:
		want := int32(1)
		if o.Spec.Replicas != nil {
			want = *o.Spec.Replicas
		}
		s := o.Status
		ok := s.ObservedGeneration >= o.Generation && s.ReadyReplicas == want && s.UpdatedReplicas == want &&
			(s.UpdateRevision == "" || s.CurrentRevision == s.UpdateRevision)
		return ok, fmt.Sprintf("期望 %d，已更新 %d，就绪 %d", want, s.UpdatedReplicas, s.ReadyReplicas)
	case *appsv1.DaemonSet:
		s := o.Status
		ok := s.ObservedGeneration >= o.Generation && s.NumberReady == s.DesiredNumberScheduled && s.UpdatedNumberScheduled == s.DesiredNumberScheduled
		return ok, fmt.Sprintf("期望 %d，已更新 %d，就绪 %d", s.DesiredNumberScheduled, s.UpdatedNumberScheduled, s.NumberReady)
	}
	return false, "未知类型"
}

// ---------------------------------------------------------------- pods helpers

func k8sPodStatusText(p *corev1.Pod) (ready string, status string, restarts int32) {
	total, ok := len(p.Spec.Containers), 0
	status = string(p.Status.Phase)
	if p.Status.Reason != "" {
		status = p.Status.Reason
	}
	for _, c := range p.Status.ContainerStatuses {
		if c.Ready {
			ok++
		}
		restarts += c.RestartCount
		if c.State.Waiting != nil && c.State.Waiting.Reason != "" {
			status = c.State.Waiting.Reason
		} else if c.State.Terminated != nil && c.State.Terminated.Reason != "" && p.Status.Phase != corev1.PodSucceeded {
			status = c.State.Terminated.Reason
		}
	}
	if p.DeletionTimestamp != nil {
		status = "Terminating"
	}
	return fmt.Sprintf("%d/%d", ok, total), status, restarts
}

func k8sHumanAge(t metav1.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := k8sNow().Sub(t.Time)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// ---------------------------------------------------------------- read actions

func k8sRunGetPods(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, any, error) {
	sel := p.Selector
	if p.Kind != "" && p.Kind != "pod" {
		obj, _, err := k8sGetWorkload(ctx, cs, p)
		if err != nil {
			return "", nil, err
		}
		s, err := k8sWorkloadSelector(obj)
		if err != nil {
			return "", nil, err
		}
		sel = s.String()
	}
	list, err := cs.CoreV1().Pods(p.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return "", nil, k8sErr(err, "命名空间 "+p.Namespace+" 的 Pod 列表")
	}
	pods := list.Items
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	var b strings.Builder
	notReady := 0
	fmt.Fprintf(&b, "%-48s %-6s %-22s %-8s %-6s %s\n", "NAME", "READY", "STATUS", "RESTARTS", "AGE", "NODE")
	for i := range pods {
		pod := &pods[i]
		ready, status, restarts := k8sPodStatusText(pod)
		if !strings.HasPrefix(ready, strconv.Itoa(len(pod.Spec.Containers))+"/") || status != string(corev1.PodRunning) {
			if pod.Status.Phase != corev1.PodSucceeded {
				notReady++
			}
		}
		if i < 200 {
			fmt.Fprintf(&b, "%-48s %-6s %-22s %-8d %-6s %s\n", pod.Name, ready, status, restarts, k8sHumanAge(pod.CreationTimestamp), pod.Spec.NodeName)
		}
	}
	if len(pods) > 200 {
		fmt.Fprintf(&b, "…另有 %d 个 Pod 未列出\n", len(pods)-200)
	}
	fmt.Fprintf(&b, "共 %d 个 Pod，未就绪 %d 个", len(pods), notReady)
	return b.String(), nil, nil
}

func k8sCleanForDisplay(obj any) string {
	b, err := json.Marshal(obj)
	if err != nil {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	if md, ok := m["metadata"].(map[string]any); ok {
		delete(md, "managedFields")
		if an, ok := md["annotations"].(map[string]any); ok {
			delete(an, "kubectl.kubernetes.io/last-applied-configuration")
		}
	}
	k8sMaskEnvValues(m)
	y, err := yaml.Marshal(m)
	if err != nil {
		return ""
	}
	return string(y)
}

// k8sMaskEnvValues hides env values whose names look like secrets.
func k8sMaskEnvValues(v any) {
	switch t := v.(type) {
	case map[string]any:
		if env, ok := t["env"].([]any); ok {
			for _, e := range env {
				if em, ok := e.(map[string]any); ok {
					if n, _ := em["name"].(string); k8sSecretName.MatchString(n) {
						if _, has := em["value"]; has {
							em["value"] = "***"
						}
					}
				}
			}
		}
		for _, c := range t {
			k8sMaskEnvValues(c)
		}
	case []any:
		for _, c := range t {
			k8sMaskEnvValues(c)
		}
	}
}

func k8sRunDescribe(ctx context.Context, ec *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, any, error) {
	var obj any
	var err error
	switch p.Kind {
	case "pod":
		obj, err = cs.CoreV1().Pods(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
	case "deployment", "statefulset", "daemonset":
		obj, _, err = k8sGetWorkload(ctx, cs, p)
	case "node":
		obj, err = cs.CoreV1().Nodes().Get(ctx, p.Name, metav1.GetOptions{})
	case "service":
		obj, err = cs.CoreV1().Services(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
	case "pvc":
		obj, err = cs.CoreV1().PersistentVolumeClaims(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
	}
	if err != nil {
		return "", nil, k8sErr(err, k8sObjName(p))
	}
	body := k8sTruncateOutput(k8sCleanForDisplay(obj), 24<<10)
	evP := *p
	ev, _, evErr := k8sRunEvents(ctx, ec, cs, &evP)
	if evErr != nil {
		ev = "事件读取失败：" + evErr.Error()
	}
	return fmt.Sprintf("# %s\n%s\n# 相关事件\n%s", k8sObjName(p), body, ev), nil, nil
}

func k8sRunLogs(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, any, error) {
	tail, limit := p.TailLines, int64(k8sLogLimitBytes)
	opts := &corev1.PodLogOptions{Container: p.Container, Previous: p.Previous, TailLines: &tail, LimitBytes: &limit}
	rc, err := cs.CoreV1().Pods(p.Namespace).GetLogs(p.Name, opts).Stream(ctx)
	if err != nil {
		return "", nil, k8sErr(err, k8sObjName(p)+" 的日志")
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, k8sLogLimitBytes+1))
	if err != nil {
		return "", nil, fmt.Errorf("读取日志失败：%v", err)
	}
	s := string(b)
	if len(s) > k8sLogLimitBytes {
		s = k8sTruncateOutput(s, k8sLogLimitBytes)
	}
	if strings.TrimSpace(s) == "" {
		s = "（无日志输出）"
	}
	return k8sMaskSecrets(s), nil, nil
}

func eventTime(e *corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	case !e.FirstTimestamp.IsZero():
		return e.FirstTimestamp.Time
	}
	return e.CreationTimestamp.Time
}

func k8sRunEvents(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, any, error) {
	ns := p.Namespace
	if p.Kind == "node" {
		ns = ""
	}
	opts := metav1.ListOptions{}
	if p.Name != "" {
		opts.FieldSelector = "involvedObject.name=" + p.Name
	}
	list, err := cs.CoreV1().Events(ns).List(ctx, opts)
	if err != nil {
		return "", nil, k8sErr(err, "事件")
	}
	var evs []corev1.Event
	for _, e := range list.Items {
		if p.Name != "" && e.InvolvedObject.Name != p.Name {
			continue
		}
		if p.Kind != "" && !strings.EqualFold(e.InvolvedObject.Kind, k8sKindTitle[p.Kind]) {
			continue
		}
		if p.Type != "" && !strings.EqualFold(e.Type, p.Type) {
			continue
		}
		evs = append(evs, e)
	}
	sort.Slice(evs, func(i, j int) bool { return eventTime(&evs[i]).Before(eventTime(&evs[j])) })
	if len(evs) > 50 {
		evs = evs[len(evs)-50:]
	}
	if len(evs) == 0 {
		return "（无相关事件）", nil, nil
	}
	var b strings.Builder
	for i := range evs {
		e := &evs[i]
		fmt.Fprintf(&b, "%s  %-7s %-24s %s/%s  %s",
			eventTime(e).Format("01-02 15:04:05"), e.Type, e.Reason, e.InvolvedObject.Kind, e.InvolvedObject.Name, strings.TrimSpace(e.Message))
		if e.Count > 1 {
			fmt.Fprintf(&b, "（×%d）", e.Count)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n"), nil, nil
}

func k8sRunWaitReady(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, any, error) {
	deadline := k8sNow().Add(time.Duration(p.TimeoutSec) * time.Second)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	last := ""
	for {
		obj, _, err := k8sGetWorkload(ctx, cs, p)
		if err != nil && ctx.Err() == nil {
			return "", nil, err
		}
		if err == nil {
			ok, progress := k8sWorkloadReady(obj)
			last = progress
			if ok {
				return fmt.Sprintf("%s 已就绪：%s", k8sObjName(p), progress), nil, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", nil, fmt.Errorf("等待 %s 就绪超时（%d 秒）：%s", k8sObjName(p), p.TimeoutSec, last)
		case <-time.After(k8sPollInterval):
		}
	}
}

// ---------------------------------------------------------------- change actions

func k8sPreviewRestart(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, error) {
	obj, _, err := k8sGetWorkload(ctx, cs, p)
	if err != nil {
		return "", err
	}
	extra := ""
	if p.Kind != "daemonset" {
		extra = fmt.Sprintf("，当前 %d 个副本", k8sGetReplicas(obj))
	}
	return fmt.Sprintf("滚动重启 %s%s。\n变更：Pod 模板注解 %s 设为当前时间，控制器将按滚动策略逐批替换 Pod。\n回滚：重启无法撤销（不会改变镜像或配置）。", k8sObjName(p), extra, k8sRestartAnnotation), nil
}

func k8sRunRestart(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, any, error) {
	now := k8sNow().UTC().Format(time.RFC3339)
	err := k8sMutateWorkload(ctx, cs, p, func(_ runtime.Object, t *corev1.PodTemplateSpec) error {
		if t.Annotations == nil {
			t.Annotations = map[string]string{}
		}
		t.Annotations[k8sRestartAnnotation] = now
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("已触发 %s 滚动重启（restartedAt=%s）", k8sObjName(p), now), nil, nil
}

type k8sScaleState struct {
	Replicas int32 `json:"replicas"`
}

func k8sPreviewScale(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, error) {
	obj, _, err := k8sGetWorkload(ctx, cs, p)
	if err != nil {
		return "", err
	}
	cur := k8sGetReplicas(obj)
	s := fmt.Sprintf("%s 副本数：%d → %d", k8sObjName(p), cur, *p.Replicas)
	if *p.Replicas == 0 {
		s += "\n注意：缩容到 0 会停止全部服务实例。"
	}
	if cur == *p.Replicas {
		s += "\n（与当前值相同，执行后无变化）"
	}
	return s + fmt.Sprintf("\n回滚：恢复为 %d 个副本。", cur), nil
}

func k8sRunScale(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, any, error) {
	var orig int32
	err := k8sMutateWorkload(ctx, cs, p, func(obj runtime.Object, _ *corev1.PodTemplateSpec) error {
		orig = k8sGetReplicas(obj)
		k8sSetReplicas(obj, *p.Replicas)
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("%s 副本数已从 %d 调整为 %d", k8sObjName(p), orig, *p.Replicas), k8sScaleState{Replicas: orig}, nil
}

func k8sUndoScale(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams, raw json.RawMessage) (string, error) {
	var st k8sScaleState
	if err := json.Unmarshal(raw, &st); err != nil {
		return "", fmt.Errorf("原始状态记录损坏：%v", err)
	}
	err := k8sMutateWorkload(ctx, cs, p, func(obj runtime.Object, _ *corev1.PodTemplateSpec) error {
		k8sSetReplicas(obj, st.Replicas)
		return nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("已将 %s 副本数恢复为 %d", k8sObjName(p), st.Replicas), nil
}

type k8sContainerState struct {
	Container string                       `json:"container"`
	Image     string                       `json:"image,omitempty"`
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

func k8sCheckResourceMap(m map[string]string, field string) (corev1.ResourceList, error) {
	out := corev1.ResourceList{}
	for k, v := range m {
		switch k {
		case "cpu", "memory", "ephemeral-storage":
		default:
			return nil, fmt.Errorf("%s 只支持 cpu、memory、ephemeral-storage，收到「%s」", field, k)
		}
		q, err := resource.ParseQuantity(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("%s.%s 的值「%s」不是合法的资源数量（例如 500m、1Gi）", field, k, v)
		}
		if q.Sign() <= 0 {
			return nil, fmt.Errorf("%s.%s 必须大于 0", field, k)
		}
		out[corev1.ResourceName(k)] = q
	}
	return out, nil
}

func k8sQtyText(l corev1.ResourceList, k string) string {
	if q, ok := l[corev1.ResourceName(k)]; ok {
		return q.String()
	}
	return "未设置"
}

func k8sResourceDiff(c *corev1.Container, req, lim corev1.ResourceList) []string {
	var lines []string
	add := func(field string, cur, next corev1.ResourceList) {
		keys := make([]string, 0, len(next))
		for k := range next {
			keys = append(keys, string(k))
		}
		sort.Strings(keys)
		for _, k := range keys {
			lines = append(lines, fmt.Sprintf("容器 %s：%s.%s %s → %s", c.Name, field, k, k8sQtyText(cur, k), k8sQtyText(next, k)))
		}
	}
	add("requests", c.Resources.Requests, req)
	add("limits", c.Resources.Limits, lim)
	return lines
}

func k8sApplyResources(c *corev1.Container, req, lim corev1.ResourceList) {
	if len(req) > 0 && c.Resources.Requests == nil {
		c.Resources.Requests = corev1.ResourceList{}
	}
	if len(lim) > 0 && c.Resources.Limits == nil {
		c.Resources.Limits = corev1.ResourceList{}
	}
	for k, v := range req {
		c.Resources.Requests[k] = v
	}
	for k, v := range lim {
		c.Resources.Limits[k] = v
	}
}

func k8sResourcesFromParams(p *k8sParams) (corev1.ResourceList, corev1.ResourceList) {
	req, _ := k8sCheckResourceMap(p.Requests, "requests")
	lim, _ := k8sCheckResourceMap(p.Limits, "limits")
	return req, lim
}

func k8sPreviewSetResources(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, error) {
	_, t, err := k8sGetWorkload(ctx, cs, p)
	if err != nil {
		return "", err
	}
	c, err := k8sFindContainer(t, p.Container)
	if err != nil {
		return "", err
	}
	req, lim := k8sResourcesFromParams(p)
	after := c.DeepCopy()
	k8sApplyResources(after, req, lim)
	if err := k8sCheckRequestsWithinLimits(after); err != nil {
		return "", err
	}
	lines := k8sResourceDiff(c, req, lim)
	return fmt.Sprintf("%s：\n%s\n修改资源会触发滚动更新。\n回滚：恢复容器 %s 原有的 requests/limits。", k8sObjName(p), strings.Join(lines, "\n"), c.Name), nil
}

func k8sCheckRequestsWithinLimits(c *corev1.Container) error {
	for k, r := range c.Resources.Requests {
		if l, ok := c.Resources.Limits[k]; ok && r.Cmp(l) > 0 {
			return fmt.Errorf("容器 %s 的 requests.%s（%s）不能大于 limits.%s（%s）", c.Name, k, r.String(), k, l.String())
		}
	}
	return nil
}

func k8sRunSetResources(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, any, error) {
	req, lim := k8sResourcesFromParams(p)
	var st k8sContainerState
	var lines []string
	err := k8sMutateWorkload(ctx, cs, p, func(_ runtime.Object, t *corev1.PodTemplateSpec) error {
		c, err := k8sFindContainer(t, p.Container)
		if err != nil {
			return err
		}
		orig := c.Resources.DeepCopy()
		st = k8sContainerState{Container: c.Name, Resources: orig}
		lines = k8sResourceDiff(c, req, lim)
		k8sApplyResources(c, req, lim)
		return k8sCheckRequestsWithinLimits(c)
	})
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("已更新 %s 资源配置：\n%s", k8sObjName(p), strings.Join(lines, "\n")), st, nil
}

func k8sUndoContainer(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams, raw json.RawMessage) (string, error) {
	var st k8sContainerState
	if err := json.Unmarshal(raw, &st); err != nil || st.Container == "" {
		return "", errors.New("原始状态记录损坏，无法回滚")
	}
	err := k8sMutateWorkload(ctx, cs, p, func(_ runtime.Object, t *corev1.PodTemplateSpec) error {
		c, err := k8sFindContainer(t, st.Container)
		if err != nil {
			return err
		}
		if st.Image != "" {
			c.Image = st.Image
		}
		if st.Resources != nil {
			c.Resources = *st.Resources
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if st.Image != "" {
		return fmt.Sprintf("已将 %s 容器 %s 的镜像恢复为 %s", k8sObjName(p), st.Container, st.Image), nil
	}
	return fmt.Sprintf("已将 %s 容器 %s 的资源配置恢复为原值", k8sObjName(p), st.Container), nil
}

var k8sImageRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._\-/:@]*$`)

func k8sPreviewSetImage(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, error) {
	_, t, err := k8sGetWorkload(ctx, cs, p)
	if err != nil {
		return "", err
	}
	c, err := k8sFindContainer(t, p.Container)
	if err != nil {
		return "", err
	}
	s := fmt.Sprintf("%s 容器 %s 镜像：\n- %s\n+ %s\n将触发滚动更新。\n回滚：恢复为原镜像 %s。", k8sObjName(p), c.Name, c.Image, p.Image, c.Image)
	if c.Image == p.Image {
		s += "\n（与当前镜像相同，执行后无变化）"
	}
	return s, nil
}

func k8sRunSetImage(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, any, error) {
	var st k8sContainerState
	err := k8sMutateWorkload(ctx, cs, p, func(_ runtime.Object, t *corev1.PodTemplateSpec) error {
		c, err := k8sFindContainer(t, p.Container)
		if err != nil {
			return err
		}
		st = k8sContainerState{Container: c.Name, Image: c.Image}
		c.Image = p.Image
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("%s 容器 %s 镜像已从 %s 更新为 %s", k8sObjName(p), st.Container, st.Image, p.Image), st, nil
}

// ---- rollback_deployment

type k8sRsRev struct {
	rs  *appsv1.ReplicaSet
	rev int64
}

func k8sDeploymentHistory(ctx context.Context, cs kubernetes.Interface, d *appsv1.Deployment) ([]k8sRsRev, error) {
	sel, err := k8sWorkloadSelector(d)
	if err != nil {
		return nil, err
	}
	list, err := cs.AppsV1().ReplicaSets(d.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, k8sErr(err, "ReplicaSet 列表")
	}
	var out []k8sRsRev
	for i := range list.Items {
		rs := &list.Items[i]
		if ref := metav1.GetControllerOf(rs); ref == nil || ref.UID != d.UID {
			continue
		}
		rev, err := strconv.ParseInt(rs.Annotations[k8sRevisionAnno], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, k8sRsRev{rs: rs, rev: rev})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rev > out[j].rev })
	return out, nil
}

func k8sPickRollbackTarget(d *appsv1.Deployment, hist []k8sRsRev, want int64) (cur int64, target *k8sRsRev, err error) {
	if len(hist) == 0 {
		return 0, nil, errors.New("没有找到该 Deployment 的历史版本（ReplicaSet）")
	}
	cur, _ = strconv.ParseInt(d.Annotations[k8sRevisionAnno], 10, 64)
	if cur == 0 {
		cur = hist[0].rev
	}
	for i := range hist {
		h := &hist[i]
		if want > 0 {
			if h.rev == want {
				if want == cur {
					return cur, nil, fmt.Errorf("第 %d 版就是当前版本，无需回滚", want)
				}
				return cur, h, nil
			}
			continue
		}
		if h.rev < cur {
			return cur, h, nil
		}
	}
	if want > 0 {
		return cur, nil, fmt.Errorf("未找到第 %d 版（可能已被历史保留数清理）", want)
	}
	return cur, nil, errors.New("没有可回滚的上一版本")
}

func k8sTemplateImages(t *corev1.PodTemplateSpec) map[string]string {
	m := map[string]string{}
	for _, c := range t.Spec.Containers {
		m[c.Name] = c.Image
	}
	return m
}

func k8sPreviewRollbackDeployment(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, error) {
	d, err := cs.AppsV1().Deployments(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
	if err != nil {
		return "", k8sErr(err, k8sObjName(p))
	}
	hist, err := k8sDeploymentHistory(ctx, cs, d)
	if err != nil {
		return "", err
	}
	cur, target, err := k8sPickRollbackTarget(d, hist, p.Revision)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s 版本：第 %d 版 → 第 %d 版（ReplicaSet %s）\n", k8sObjName(p), cur, target.rev, target.rs.Name)
	now, then := k8sTemplateImages(&d.Spec.Template), k8sTemplateImages(&target.rs.Spec.Template)
	names := make([]string, 0, len(now))
	for n := range now {
		names = append(names, n)
	}
	sort.Strings(names)
	changed := false
	for _, n := range names {
		if then[n] != now[n] {
			changed = true
			fmt.Fprintf(&b, "容器 %s 镜像：%s → %s\n", n, now[n], k8sValueOr(then[n], "（该版本无此容器）"))
		}
	}
	if !changed {
		b.WriteString("镜像无变化，仅恢复 Pod 模板中的其他配置（环境变量、资源等）。\n")
	}
	b.WriteString("将触发滚动更新。\n回滚：恢复为执行前的 Pod 模板。")
	return b.String(), nil
}

func k8sValueOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

type k8sTemplateState struct {
	Template corev1.PodTemplateSpec `json:"template"`
}

func k8sRunRollbackDeployment(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, any, error) {
	var st k8sTemplateState
	var cur int64
	var target *k8sRsRev
	err := k8sMutateWorkload(ctx, cs, p, func(obj runtime.Object, t *corev1.PodTemplateSpec) error {
		d := obj.(*appsv1.Deployment)
		hist, err := k8sDeploymentHistory(ctx, cs, d)
		if err != nil {
			return err
		}
		cur, target, err = k8sPickRollbackTarget(d, hist, p.Revision)
		if err != nil {
			return err
		}
		st = k8sTemplateState{Template: *t.DeepCopy()}
		nt := target.rs.Spec.Template.DeepCopy()
		delete(nt.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
		d.Spec.Template = *nt
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("%s 已从第 %d 版回滚到第 %d 版", k8sObjName(p), cur, target.rev), st, nil
}

func k8sUndoRollbackDeployment(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams, raw json.RawMessage) (string, error) {
	var st k8sTemplateState
	if err := json.Unmarshal(raw, &st); err != nil {
		return "", errors.New("原始状态记录损坏，无法回滚")
	}
	err := k8sMutateWorkload(ctx, cs, p, func(obj runtime.Object, _ *corev1.PodTemplateSpec) error {
		obj.(*appsv1.Deployment).Spec.Template = st.Template
		return nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("已将 %s 的 Pod 模板恢复为回滚前的版本", k8sObjName(p)), nil
}

// ---------------------------------------------------------------- high actions

type k8sCordonState struct {
	Unschedulable bool `json:"unschedulable"`
}

func k8sSetNodeUnschedulable(ctx context.Context, cs kubernetes.Interface, name string, v bool) (bool, error) {
	var orig bool
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		n, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		orig = n.Spec.Unschedulable
		if orig == v {
			return nil
		}
		n.Spec.Unschedulable = v
		_, err = cs.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{FieldManager: k8sFieldManager})
		return err
	})
	return orig, k8sErr(err, "节点 "+name)
}

func k8sCordonWord(v bool) string {
	if v {
		return "不可调度（已封锁）"
	}
	return "可调度"
}

func k8sPreviewCordon(target bool) k8sFn {
	return func(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, error) {
		n, err := cs.CoreV1().Nodes().Get(ctx, p.Name, metav1.GetOptions{})
		if err != nil {
			return "", k8sErr(err, "节点 "+p.Name)
		}
		pods, _ := k8sPodsOnNode(ctx, cs, p.Name)
		s := fmt.Sprintf("节点 %s：%s → %s\n节点上现有 %d 个 Pod，不会被驱逐。", p.Name, k8sCordonWord(n.Spec.Unschedulable), k8sCordonWord(target), len(pods))
		if n.Spec.Unschedulable == target {
			s += "\n（与当前状态相同，执行后无变化）"
		}
		return s + fmt.Sprintf("\n回滚：恢复为 %s。", k8sCordonWord(n.Spec.Unschedulable)), nil
	}
}

func k8sRunCordon(target bool) func(ctx context.Context, ec *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, any, error) {
	return func(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, any, error) {
		orig, err := k8sSetNodeUnschedulable(ctx, cs, p.Name, target)
		if err != nil {
			return "", nil, err
		}
		if orig == target {
			return fmt.Sprintf("节点 %s 已是%s，无需变更", p.Name, k8sCordonWord(target)), k8sCordonState{Unschedulable: orig}, nil
		}
		return fmt.Sprintf("节点 %s 已设为%s", p.Name, k8sCordonWord(target)), k8sCordonState{Unschedulable: orig}, nil
	}
}

func k8sUndoCordon(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams, raw json.RawMessage) (string, error) {
	var st k8sCordonState
	if err := json.Unmarshal(raw, &st); err != nil {
		return "", errors.New("原始状态记录损坏，无法回滚")
	}
	if _, err := k8sSetNodeUnschedulable(ctx, cs, p.Name, st.Unschedulable); err != nil {
		return "", err
	}
	return fmt.Sprintf("节点 %s 已恢复为%s", p.Name, k8sCordonWord(st.Unschedulable)), nil
}

func k8sPodsOnNode(ctx context.Context, cs kubernetes.Interface, node string) ([]corev1.Pod, error) {
	list, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node})
	if err != nil {
		return nil, k8sErr(err, "节点 "+node+" 上的 Pod 列表")
	}
	out := list.Items[:0]
	for _, p := range list.Items {
		if p.Spec.NodeName == node {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Namespace+"/"+out[i].Name < out[j].Namespace+"/"+out[j].Name
	})
	return out, nil
}

type k8sDrainPlan struct {
	evict   []corev1.Pod
	skipped []string // DaemonSet / mirror / finished
	blocked []string
}

func k8sPlanDrain(ctx context.Context, cs kubernetes.Interface, p *k8sParams) (*k8sDrainPlan, error) {
	pods, err := k8sPodsOnNode(ctx, cs, p.Name)
	if err != nil {
		return nil, err
	}
	dp := &k8sDrainPlan{}
	for _, pod := range pods {
		id := pod.Namespace + "/" + pod.Name
		ref := metav1.GetControllerOf(&pod)
		switch {
		case pod.Annotations[k8sMirrorAnno] != "":
			dp.skipped = append(dp.skipped, id+"（静态 Pod）")
			continue
		case ref != nil && ref.Kind == "DaemonSet":
			dp.skipped = append(dp.skipped, id+"（DaemonSet）")
			continue
		case pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed:
			dp.skipped = append(dp.skipped, id+"（已结束）")
			continue
		}
		if ref == nil && !p.Force {
			dp.blocked = append(dp.blocked, id+"：没有控制器管理，驱逐后不会重建（如确认可删除，设置 force=true）")
			continue
		}
		if !p.DeleteEmptyDirData {
			for _, v := range pod.Spec.Volumes {
				if v.EmptyDir != nil {
					dp.blocked = append(dp.blocked, id+"：使用 emptyDir 本地数据，驱逐会丢失（如确认可丢弃，设置 delete_emptydir_data=true）")
					break
				}
			}
			if len(dp.blocked) > 0 && strings.HasPrefix(dp.blocked[len(dp.blocked)-1], id+"：") {
				continue
			}
		}
		dp.evict = append(dp.evict, pod)
	}
	return dp, nil
}

func (dp *k8sDrainPlan) text(node string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "排空节点 %s：先封锁（不可调度），再通过 Eviction API 驱逐 Pod（遵守 PodDisruptionBudget，被 PDB 拒绝时会等待重试）。\n", node)
	fmt.Fprintf(&b, "将驱逐 %d 个 Pod：\n", len(dp.evict))
	for i, pod := range dp.evict {
		if i == 50 {
			fmt.Fprintf(&b, "  …另有 %d 个\n", len(dp.evict)-50)
			break
		}
		fmt.Fprintf(&b, "  - %s/%s\n", pod.Namespace, pod.Name)
	}
	if len(dp.skipped) > 0 {
		fmt.Fprintf(&b, "跳过 %d 个：%s\n", len(dp.skipped), strings.Join(dp.skipped, "、"))
	}
	if len(dp.blocked) > 0 {
		fmt.Fprintf(&b, "无法执行，以下 Pod 阻止排空：\n  - %s\n", strings.Join(dp.blocked, "\n  - "))
	}
	b.WriteString("回滚：仅能取消节点封锁；已驱逐的 Pod 由各自控制器在其他节点重建，不会迁回。")
	return b.String()
}

func k8sPreviewDrain(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, error) {
	if _, err := cs.CoreV1().Nodes().Get(ctx, p.Name, metav1.GetOptions{}); err != nil {
		return "", k8sErr(err, "节点 "+p.Name)
	}
	dp, err := k8sPlanDrain(ctx, cs, p)
	if err != nil {
		return "", err
	}
	return dp.text(p.Name), nil
}

func k8sRunDrain(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, any, error) {
	dp, err := k8sPlanDrain(ctx, cs, p)
	if err != nil {
		return "", nil, err
	}
	if len(dp.blocked) > 0 {
		return "", nil, fmt.Errorf("有 %d 个 Pod 阻止排空，未做任何变更：%s", len(dp.blocked), strings.Join(dp.blocked, "；"))
	}
	orig, err := k8sSetNodeUnschedulable(ctx, cs, p.Name, true)
	if err != nil {
		return "", nil, err
	}
	st := k8sCordonState{Unschedulable: orig}
	var b strings.Builder
	fmt.Fprintf(&b, "节点 %s 已封锁\n", p.Name)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSec)*time.Second)
	defer cancel()
	pending := map[string]types.UID{}
	for _, pod := range dp.evict {
		id := pod.Namespace + "/" + pod.Name
		if err := k8sEvictWithRetry(ctx, cs, &pod, p.GracePeriodSec); err != nil {
			fmt.Fprintf(&b, "驱逐 %s 失败：%v\n", id, err)
			return b.String(), st, fmt.Errorf("排空节点 %s 未完成：驱逐 %s 失败：%v（节点保持封锁，可回滚取消封锁）", p.Name, id, err)
		}
		pending[id] = pod.UID
		fmt.Fprintf(&b, "已驱逐 %s\n", id)
	}
	for len(pending) > 0 {
		for id, uid := range pending {
			ns, name, _ := strings.Cut(id, "/")
			cur, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) || (err == nil && cur.UID != uid) {
				delete(pending, id)
			}
		}
		if len(pending) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			left := make([]string, 0, len(pending))
			for id := range pending {
				left = append(left, id)
			}
			sort.Strings(left)
			return b.String(), st, fmt.Errorf("等待 Pod 退出超时（%d 秒），仍在运行：%s", p.TimeoutSec, strings.Join(left, "、"))
		case <-time.After(k8sPollInterval):
		}
	}
	fmt.Fprintf(&b, "排空完成：驱逐 %d 个，跳过 %d 个", len(dp.evict), len(dp.skipped))
	return b.String(), st, nil
}

func k8sEvictWithRetry(ctx context.Context, cs kubernetes.Interface, pod *corev1.Pod, grace *int64) error {
	ev := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace}}
	if grace != nil {
		ev.DeleteOptions = &metav1.DeleteOptions{GracePeriodSeconds: grace}
	}
	for {
		err := cs.PolicyV1().Evictions(pod.Namespace).Evict(ctx, ev)
		switch {
		case err == nil, apierrors.IsNotFound(err):
			return nil
		case apierrors.IsTooManyRequests(err):
			// PodDisruptionBudget currently forbids it; wait and retry.
		default:
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("受 PodDisruptionBudget 限制，超时前未能驱逐：%v", err)
		case <-time.After(k8sPollInterval):
		}
	}
}

func k8sUndoDrain(ctx context.Context, ec *ExecContext, cs kubernetes.Interface, p *k8sParams, raw json.RawMessage) (string, error) {
	out, err := k8sUndoCordon(ctx, ec, cs, p, raw)
	if err != nil {
		return "", err
	}
	return out + "；已驱逐的 Pod 不会迁回该节点。", nil
}

func k8sPreviewDeletePod(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, error) {
	pod, err := cs.CoreV1().Pods(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
	if err != nil {
		return "", k8sErr(err, k8sObjName(p))
	}
	_, status, restarts := k8sPodStatusText(pod)
	s := fmt.Sprintf("删除 %s（状态 %s，重启 %d 次，节点 %s）。\n", k8sObjName(p), status, restarts, k8sValueOr(pod.Spec.NodeName, "-"))
	if ref := metav1.GetControllerOf(pod); ref != nil {
		s += fmt.Sprintf("该 Pod 由 %s %s 管理，删除后会自动重建。\n", ref.Kind, ref.Name)
	} else {
		s += "警告：该 Pod 没有控制器管理，删除后不会重建。\n"
	}
	if p.GracePeriodSec != nil {
		s += fmt.Sprintf("优雅终止时间：%d 秒。\n", *p.GracePeriodSec)
	}
	return s + "回滚：删除无法撤销。", nil
}

func k8sRunDeletePod(ctx context.Context, _ *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, any, error) {
	opts := metav1.DeleteOptions{GracePeriodSeconds: p.GracePeriodSec}
	if err := cs.CoreV1().Pods(p.Namespace).Delete(ctx, p.Name, opts); err != nil {
		return "", nil, k8sErr(err, k8sObjName(p))
	}
	return fmt.Sprintf("已删除 %s", k8sObjName(p)), nil, nil
}

// ---- apply_yaml

func k8sParseManifests(src string, defNS string) ([]*unstructured.Unstructured, error) {
	if strings.TrimSpace(src) == "" {
		return nil, errors.New("缺少参数 yaml（要应用的资源清单）")
	}
	if len(src) > k8sMaxYAMLBytes {
		return nil, fmt.Errorf("YAML 过大（超过 %d KiB）", k8sMaxYAMLBytes>>10)
	}
	dec := k8syaml.NewYAMLOrJSONDecoder(strings.NewReader(src), 4096)
	var out []*unstructured.Unstructured
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("YAML 解析失败：%v", err)
		}
		if len(m) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: m}
		idx := len(out) + 1
		if u.GetAPIVersion() == "" || u.GetKind() == "" {
			return nil, fmt.Errorf("第 %d 个对象缺少 apiVersion 或 kind", idx)
		}
		if u.IsList() {
			return nil, fmt.Errorf("第 %d 个对象是 List，请展开为多个文档", idx)
		}
		if u.GetName() == "" {
			return nil, fmt.Errorf("第 %d 个对象（%s）缺少 metadata.name", idx, u.GetKind())
		}
		if strings.EqualFold(u.GetKind(), "Secret") {
			return nil, fmt.Errorf("不允许通过 AI 运维应用 Secret（%s），凭据请在密钥管理中维护", u.GetName())
		}
		if u.GetNamespace() == "" && defNS != "" {
			u.SetNamespace(defNS)
		}
		out = append(out, u)
		if len(out) > k8sMaxApplyObjects {
			return nil, fmt.Errorf("一次最多应用 %d 个对象", k8sMaxApplyObjects)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("YAML 中没有任何资源对象")
	}
	return out, nil
}

type k8sApplyTarget struct {
	obj        *unstructured.Unstructured
	gvr        schema.GroupVersionResource
	namespaced bool
}

func (t *k8sApplyTarget) client(dyn dynamic.Interface) dynamic.ResourceInterface {
	if t.namespaced {
		return dyn.Resource(t.gvr).Namespace(t.obj.GetNamespace())
	}
	return dyn.Resource(t.gvr)
}

func (t *k8sApplyTarget) label() string {
	if t.namespaced {
		return fmt.Sprintf("%s %s/%s", t.obj.GetKind(), t.obj.GetNamespace(), t.obj.GetName())
	}
	return fmt.Sprintf("%s %s", t.obj.GetKind(), t.obj.GetName())
}

func k8sResolveTargets(mapper meta.RESTMapper, objs []*unstructured.Unstructured) ([]k8sApplyTarget, error) {
	out := make([]k8sApplyTarget, 0, len(objs))
	for _, o := range objs {
		gvk := o.GroupVersionKind()
		m, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return nil, fmt.Errorf("集群不认识资源类型 %s：%v", gvk.String(), err)
		}
		t := k8sApplyTarget{obj: o, gvr: m.Resource, namespaced: m.Scope.Name() == meta.RESTScopeNameNamespace}
		if !t.namespaced {
			o.SetNamespace("")
		}
		out = append(out, t)
	}
	return out, nil
}

func k8sCleanUnstructured(u *unstructured.Unstructured) string {
	if u == nil {
		return ""
	}
	c := u.DeepCopy()
	unstructured.RemoveNestedField(c.Object, "metadata", "managedFields")
	unstructured.RemoveNestedField(c.Object, "metadata", "resourceVersion")
	unstructured.RemoveNestedField(c.Object, "metadata", "uid")
	unstructured.RemoveNestedField(c.Object, "metadata", "generation")
	unstructured.RemoveNestedField(c.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(c.Object, "metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration")
	unstructured.RemoveNestedField(c.Object, "status")
	k8sMaskEnvValues(c.Object)
	y, err := yaml.Marshal(c.Object)
	if err != nil {
		return ""
	}
	return string(y)
}

// k8sLineDiff is a small LCS line diff; only changed lines are returned.
func k8sLineDiff(a, b string) string {
	al, bl := k8sSplitLines(a), k8sSplitLines(b)
	if len(al)*len(bl) > 4_000_000 {
		return fmt.Sprintf("（对象过大，跳过逐行对比：%d 行 → %d 行）", len(al), len(bl))
	}
	n, m := len(al), len(bl)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var out strings.Builder
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && al[i] == bl[j]:
			i++
			j++
		case j < m && (i == n || dp[i][j+1] >= dp[i+1][j]):
			out.WriteString("+ " + bl[j] + "\n")
			j++
		default:
			out.WriteString("- " + al[i] + "\n")
			i++
		}
	}
	return out.String()
}

func k8sSplitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func k8sApplyAll(ctx context.Context, ec *ExecContext, p *k8sParams, dryRun bool) (dynamic.Interface, []k8sApplyTarget, []*unstructured.Unstructured, []*unstructured.Unstructured, error) {
	objs, err := k8sParseManifests(p.YAML, p.Namespace)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	dyn, mapper, err := k8sDynamicFor(ctx, ec)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	targets, err := k8sResolveTargets(mapper, objs)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	before := make([]*unstructured.Unstructured, len(targets))
	after := make([]*unstructured.Unstructured, len(targets))
	for i := range targets {
		t := &targets[i]
		cur, err := t.client(dyn).Get(ctx, t.obj.GetName(), metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, nil, nil, nil, k8sErr(err, t.label())
		}
		if err == nil {
			before[i] = cur
		}
		opts := metav1.ApplyOptions{FieldManager: k8sFieldManager, Force: true}
		if dryRun {
			opts.DryRun = []string{metav1.DryRunAll}
		}
		res, err := t.client(dyn).Apply(ctx, t.obj.GetName(), t.obj, opts)
		if err != nil {
			what := "应用"
			if dryRun {
				what = "服务端预演（dry-run）"
			}
			return dyn, targets[:i], before[:i], after[:i], fmt.Errorf("%s %s 失败：%v", what, t.label(), err)
		}
		after[i] = res
	}
	return dyn, targets, before, after, nil
}

func k8sPreviewApplyYAML(ctx context.Context, ec *ExecContext, _ kubernetes.Interface, p *k8sParams) (string, error) {
	_, targets, before, after, err := k8sApplyAll(ctx, ec, p, true)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("以下差异来自 Kubernetes 服务端预演（server-side dry-run），尚未修改集群：\n")
	for i := range targets {
		if before[i] == nil {
			fmt.Fprintf(&b, "\n【新建】%s\n", targets[i].label())
		} else {
			fmt.Fprintf(&b, "\n【修改】%s\n", targets[i].label())
		}
		d := k8sLineDiff(k8sCleanUnstructured(before[i]), k8sCleanUnstructured(after[i]))
		if strings.TrimSpace(d) == "" {
			d = "（无变化）\n"
		}
		b.WriteString(d)
	}
	b.WriteString("\n回滚：新建的对象将被删除，修改的对象恢复为执行前的内容。")
	return k8sTruncateOutput(b.String(), 48<<10), nil
}

type k8sApplyStateItem struct {
	Group      string          `json:"group"`
	Version    string          `json:"version"`
	Resource   string          `json:"resource"`
	Namespaced bool            `json:"namespaced"`
	Namespace  string          `json:"namespace,omitempty"`
	Name       string          `json:"name"`
	Original   json.RawMessage `json:"original,omitempty"` // absent = created by this action
}

func k8sRunApplyYAML(ctx context.Context, ec *ExecContext, _ kubernetes.Interface, p *k8sParams) (string, any, error) {
	_, targets, before, _, err := k8sApplyAll(ctx, ec, p, false)
	st := make([]k8sApplyStateItem, 0, len(targets))
	var b strings.Builder
	for i := range targets {
		t := &targets[i]
		it := k8sApplyStateItem{Group: t.gvr.Group, Version: t.gvr.Version, Resource: t.gvr.Resource,
			Namespaced: t.namespaced, Namespace: t.obj.GetNamespace(), Name: t.obj.GetName()}
		if before[i] != nil {
			orig := before[i].DeepCopy()
			unstructured.RemoveNestedField(orig.Object, "metadata", "managedFields")
			unstructured.RemoveNestedField(orig.Object, "status")
			it.Original, _ = json.Marshal(orig.Object)
			fmt.Fprintf(&b, "已更新 %s\n", t.label())
		} else {
			fmt.Fprintf(&b, "已创建 %s\n", t.label())
		}
		st = append(st, it)
	}
	if err != nil {
		return b.String(), st, err
	}
	return strings.TrimRight(b.String(), "\n"), st, nil
}

func k8sUndoApplyYAML(ctx context.Context, ec *ExecContext, _ kubernetes.Interface, _ *k8sParams, raw json.RawMessage) (string, error) {
	var st []k8sApplyStateItem
	if err := json.Unmarshal(raw, &st); err != nil {
		return "", errors.New("原始状态记录损坏，无法回滚")
	}
	dyn, _, err := k8sDynamicFor(ctx, ec)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	var errs []string
	for i := len(st) - 1; i >= 0; i-- {
		it := st[i]
		var rc dynamic.ResourceInterface = dyn.Resource(schema.GroupVersionResource{Group: it.Group, Version: it.Version, Resource: it.Resource})
		label := it.Resource + " " + it.Name
		if it.Namespaced {
			rc = dyn.Resource(schema.GroupVersionResource{Group: it.Group, Version: it.Version, Resource: it.Resource}).Namespace(it.Namespace)
			label = it.Resource + " " + it.Namespace + "/" + it.Name
		}
		if len(it.Original) == 0 {
			if err := rc.Delete(ctx, it.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Sprintf("删除 %s 失败：%v", label, err))
				continue
			}
			fmt.Fprintf(&b, "已删除新建的 %s\n", label)
			continue
		}
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur, err := rc.Get(ctx, it.Name, metav1.GetOptions{})
			orig := &unstructured.Unstructured{}
			if jerr := json.Unmarshal(it.Original, &orig.Object); jerr != nil {
				return jerr
			}
			if apierrors.IsNotFound(err) {
				orig.SetResourceVersion("")
				orig.SetUID("")
				_, err = rc.Create(ctx, orig, metav1.CreateOptions{FieldManager: k8sFieldManager})
				return err
			}
			if err != nil {
				return err
			}
			orig.SetResourceVersion(cur.GetResourceVersion())
			_, err = rc.Update(ctx, orig, metav1.UpdateOptions{FieldManager: k8sFieldManager})
			return err
		})
		if err != nil {
			errs = append(errs, fmt.Sprintf("恢复 %s 失败：%v", label, err))
			continue
		}
		fmt.Fprintf(&b, "已恢复 %s\n", label)
	}
	if len(errs) > 0 {
		return b.String(), errors.New(strings.Join(errs, "；"))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// ---------------------------------------------------------------- registry

const (
	k8sPermRead  = "workload:read"
	k8sPermWrite = "workload:write"
	k8sPermNode  = "node:write"
)

var k8sPropDefs = map[string]string{
	"kind":                 `{"type":"string","description":"资源类型"}`,
	"namespace":            `{"type":"string","description":"命名空间，默认 default"}`,
	"name":                 `{"type":"string","description":"资源名称"}`,
	"container":            `{"type":"string","description":"容器名，仅一个容器时可省略"}`,
	"selector":             `{"type":"string","description":"标签选择器，如 app=web"}`,
	"type":                 `{"type":"string","enum":["Normal","Warning"],"description":"事件类型"}`,
	"previous":             `{"type":"boolean","description":"读取上一次（崩溃前）容器的日志"}`,
	"tail_lines":           `{"type":"integer","minimum":1,"maximum":200,"description":"尾部行数，默认且最多 200"}`,
	"replicas":             `{"type":"integer","minimum":0,"maximum":1000}`,
	"image":                `{"type":"string","description":"完整镜像地址，如 nginx:1.27"}`,
	"requests":             `{"type":"object","additionalProperties":{"type":"string"},"description":"如 {\"cpu\":\"200m\",\"memory\":\"256Mi\"}"}`,
	"limits":               `{"type":"object","additionalProperties":{"type":"string"},"description":"如 {\"cpu\":\"1\",\"memory\":\"512Mi\"}"}`,
	"revision":             `{"type":"integer","minimum":1,"description":"目标版本号，省略则回滚到上一版"}`,
	"timeout_sec":          `{"type":"integer","description":"超时秒数"}`,
	"grace_period_sec":     `{"type":"integer","minimum":0,"description":"优雅终止秒数"}`,
	"force":                `{"type":"boolean","description":"允许驱逐没有控制器管理的 Pod"}`,
	"delete_emptydir_data": `{"type":"boolean","description":"允许驱逐使用 emptyDir 的 Pod（本地数据会丢失）"}`,
	"yaml":                 `{"type":"string","description":"资源清单（可多文档，最多 20 个对象，不允许 Secret）"}`,
}

func k8sSchema(required []string, kinds []string, props ...string) json.RawMessage {
	var b strings.Builder
	b.WriteString(`{"type":"object","additionalProperties":false,"properties":{`)
	for i, p := range props {
		if i > 0 {
			b.WriteByte(',')
		}
		def := k8sPropDefs[p]
		if p == "kind" && len(kinds) > 0 {
			ks, _ := json.Marshal(kinds)
			def = `{"type":"string","enum":` + string(ks) + `,"description":"资源类型"}`
		}
		fmt.Fprintf(&b, "%q:%s", p, def)
	}
	b.WriteString("}")
	if len(required) > 0 {
		rs, _ := json.Marshal(required)
		b.WriteString(`,"required":` + string(rs))
	}
	b.WriteString("}")
	return json.RawMessage(b.String())
}

func k8sSpec(name, title, desc, risk, perm string, schemaJSON json.RawMessage) ActionSpec {
	return ActionSpec{Name: name, Title: title, Description: desc, Risk: risk, Channel: "k8s",
		Params: schemaJSON, Permission: perm, ReadOnly: risk == RiskRead}
}

func k8sReadRun(fn func(ctx context.Context, ec *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, any, error)) func(ctx context.Context, ec *ExecContext, cs kubernetes.Interface, p *k8sParams) (string, any, error) {
	return fn
}

var k8sWorkloadKinds = []string{"deployment", "statefulset", "daemonset"}

func k8sCheckWorkload(kinds ...string) func(p *k8sParams) error {
	return func(p *k8sParams) error {
		if err := k8sRequireKind(p, kinds...); err != nil {
			return err
		}
		if err := k8sRequireNamespace(p); err != nil {
			return err
		}
		return k8sRequireName(p, k8sKindCN[p.Kind])
	}
}

func k8sActions() []*k8sAction {
	return []*k8sAction{
		{
			spec: k8sSpec("k8s.get_pods", "查看 Pod 列表", "列出命名空间内的 Pod（可按标签或工作负载过滤），含就绪数、状态、重启次数、节点。",
				RiskRead, k8sPermRead, k8sSchema(nil, k8sWorkloadKinds, "namespace", "selector", "kind", "name")),
			check: func(p *k8sParams) error {
				if err := k8sRequireNamespace(p); err != nil {
					return err
				}
				if p.Selector != "" {
					if _, err := labels.Parse(p.Selector); err != nil {
						return fmt.Errorf("标签选择器「%s」不合法：%v", p.Selector, err)
					}
				}
				if p.Kind != "" || p.Name != "" {
					if err := k8sRequireKind(p, k8sWorkloadKinds...); err != nil {
						return err
					}
					return k8sRequireName(p, k8sKindCN[p.Kind])
				}
				return nil
			},
			title: func(p *k8sParams) string {
				if p.Name != "" {
					return "查看 " + k8sObjName(p) + " 的 Pod"
				}
				return "查看命名空间 " + p.Namespace + " 的 Pod"
			},
			manual: func(p *k8sParams) string {
				s := "kubectl get pods -o wide -n " + p.Namespace
				if p.Selector != "" {
					s += " -l '" + p.Selector + "'"
				}
				if p.Name != "" {
					s = fmt.Sprintf("kubectl get %s %s -n %s -o jsonpath='{.spec.selector.matchLabels}'  # 再按标签 kubectl get pods -l ...", p.Kind, p.Name, p.Namespace)
				}
				return s
			},
			run: k8sReadRun(k8sRunGetPods),
		},
		{
			spec: k8sSpec("k8s.describe", "查看资源详情", "读取 Pod/工作负载/节点/Service/PVC 的规格与状态及相关事件（敏感环境变量已遮盖）。",
				RiskRead, k8sPermRead, k8sSchema([]string{"kind", "name"}, []string{"pod", "deployment", "statefulset", "daemonset", "node", "service", "pvc"}, "kind", "namespace", "name")),
			check: func(p *k8sParams) error {
				if err := k8sRequireKind(p, "pod", "deployment", "statefulset", "daemonset", "node", "service", "pvc"); err != nil {
					return err
				}
				if p.Kind == "node" {
					p.Namespace = ""
				} else if err := k8sRequireNamespace(p); err != nil {
					return err
				}
				return k8sRequireName(p, k8sKindCN[p.Kind])
			},
			title: func(p *k8sParams) string { return "查看 " + k8sObjName(p) + " 详情" },
			manual: func(p *k8sParams) string {
				if p.Kind == "node" {
					return "kubectl describe node " + p.Name
				}
				return fmt.Sprintf("kubectl describe %s %s -n %s", k8sKubectlKind(p.Kind), p.Name, p.Namespace)
			},
			run: k8sReadRun(k8sRunDescribe),
		},
		{
			spec: k8sSpec("k8s.logs", "查看容器日志", "读取 Pod 容器日志尾部（最多 200 行、4 KiB，敏感字段已遮盖）。",
				RiskRead, k8sPermRead, k8sSchema([]string{"name"}, nil, "namespace", "name", "container", "previous", "tail_lines")),
			check: func(p *k8sParams) error {
				p.Kind = "pod"
				if err := k8sRequireNamespace(p); err != nil {
					return err
				}
				if err := k8sRequireName(p, "Pod "); err != nil {
					return err
				}
				if p.TailLines == 0 {
					p.TailLines = k8sLogTailLines
				}
				if p.TailLines < 1 || p.TailLines > k8sLogTailLines {
					return fmt.Errorf("tail_lines 须在 1–%d 之间", k8sLogTailLines)
				}
				return nil
			},
			title: func(p *k8sParams) string { return "查看 " + k8sObjName(p) + " 日志" },
			manual: func(p *k8sParams) string {
				s := fmt.Sprintf("kubectl logs %s -n %s --tail=%d --limit-bytes=%d", p.Name, p.Namespace, p.TailLines, k8sLogLimitBytes)
				if p.Container != "" {
					s += " -c " + p.Container
				}
				if p.Previous {
					s += " --previous"
				}
				return s
			},
			run: k8sReadRun(k8sRunLogs),
		},
		{
			spec: k8sSpec("k8s.events", "查看事件", "列出命名空间或某个对象的最近 50 条事件，可只看 Warning。",
				RiskRead, k8sPermRead, k8sSchema(nil, []string{"pod", "deployment", "statefulset", "daemonset", "node", "service", "pvc"}, "namespace", "kind", "name", "type")),
			check: func(p *k8sParams) error {
				if p.Kind == "node" {
					p.Namespace = ""
				} else if err := k8sRequireNamespace(p); err != nil {
					return err
				}
				if p.Name != "" {
					if err := k8sRequireName(p, "对象"); err != nil {
						return err
					}
				}
				if p.Type != "" && !strings.EqualFold(p.Type, "Normal") && !strings.EqualFold(p.Type, "Warning") {
					return errors.New("type 只能是 Normal 或 Warning")
				}
				return nil
			},
			title: func(p *k8sParams) string {
				if p.Name != "" && p.Kind != "" {
					return "查看 " + k8sObjName(p) + " 的事件"
				}
				if p.Name != "" {
					return "查看对象 " + p.Name + " 的事件"
				}
				return "查看命名空间 " + p.Namespace + " 的事件"
			},
			manual: func(p *k8sParams) string {
				s := "kubectl get events --sort-by=.lastTimestamp"
				if p.Namespace != "" {
					s += " -n " + p.Namespace
				} else {
					s += " -A"
				}
				if p.Name != "" {
					s += " --field-selector involvedObject.name=" + p.Name
				}
				if p.Type != "" {
					s += ",type=" + p.Type
				}
				return s
			},
			run: k8sReadRun(k8sRunEvents),
		},
		{
			spec: k8sSpec("k8s.wait_ready", "等待工作负载就绪", "验证步骤：轮询直到 Deployment/StatefulSet/DaemonSet 全部副本更新并就绪，超时即失败。",
				RiskRead, k8sPermRead, k8sSchema([]string{"kind", "name"}, k8sWorkloadKinds, "kind", "namespace", "name", "timeout_sec")),
			check: func(p *k8sParams) error {
				if err := k8sCheckWorkload(k8sWorkloadKinds...)(p); err != nil {
					return err
				}
				return k8sRequireTimeout(p, 180, 5, 1800)
			},
			title: func(p *k8sParams) string {
				return fmt.Sprintf("等待 %s 就绪（最多 %d 秒）", k8sObjName(p), p.TimeoutSec)
			},
			manual: func(p *k8sParams) string {
				return fmt.Sprintf("kubectl rollout status %s/%s -n %s --timeout=%ds", p.Kind, p.Name, p.Namespace, p.TimeoutSec)
			},
			run: k8sReadRun(k8sRunWaitReady),
		},
		// ---- change
		{
			spec: k8sSpec("k8s.restart", "滚动重启", "通过 Pod 模板注解触发滚动重启（等同 kubectl rollout restart）。",
				RiskChange, k8sPermWrite, k8sSchema([]string{"kind", "name"}, k8sWorkloadKinds, "kind", "namespace", "name")),
			check: k8sCheckWorkload(k8sWorkloadKinds...),
			title: func(p *k8sParams) string { return "滚动重启 " + k8sObjName(p) },
			manual: func(p *k8sParams) string {
				return fmt.Sprintf("kubectl rollout restart %s/%s -n %s", p.Kind, p.Name, p.Namespace)
			},
			preview: k8sPreviewRestart,
			run:     k8sRunRestart,
		},
		{
			spec: k8sSpec("k8s.scale", "调整副本数", "修改 Deployment/StatefulSet 副本数；缩到 0 视为高风险。回滚恢复原副本数。",
				RiskChange, k8sPermWrite, k8sSchema([]string{"kind", "name", "replicas"}, []string{"deployment", "statefulset"}, "kind", "namespace", "name", "replicas")),
			check: func(p *k8sParams) error {
				if err := k8sCheckWorkload("deployment", "statefulset")(p); err != nil {
					return err
				}
				if p.Replicas == nil {
					return errors.New("缺少参数 replicas（目标副本数）")
				}
				if *p.Replicas < 0 || *p.Replicas > k8sMaxReplicas {
					return fmt.Errorf("副本数须在 0–%d 之间", k8sMaxReplicas)
				}
				return nil
			},
			escalate: func(_ *ExecContext, p *k8sParams) string {
				if p.Replicas != nil && *p.Replicas == 0 {
					return RiskHigh
				}
				return RiskChange
			},
			title: func(p *k8sParams) string {
				return fmt.Sprintf("将 %s 调整为 %d 个副本", k8sObjName(p), *p.Replicas)
			},
			manual: func(p *k8sParams) string {
				return fmt.Sprintf("kubectl scale %s/%s -n %s --replicas=%d", p.Kind, p.Name, p.Namespace, *p.Replicas)
			},
			preview: k8sPreviewScale,
			run:     k8sRunScale,
			undo:    k8sUndoScale,
		},
		{
			spec: k8sSpec("k8s.set_resources", "调整容器资源", "修改容器 requests/limits（cpu、memory、ephemeral-storage）。回滚恢复原值。",
				RiskChange, k8sPermWrite, k8sSchema([]string{"kind", "name"}, k8sWorkloadKinds, "kind", "namespace", "name", "container", "requests", "limits")),
			check: func(p *k8sParams) error {
				if err := k8sCheckWorkload(k8sWorkloadKinds...)(p); err != nil {
					return err
				}
				if len(p.Requests) == 0 && len(p.Limits) == 0 {
					return errors.New("requests 和 limits 至少填写一项")
				}
				req, err := k8sCheckResourceMap(p.Requests, "requests")
				if err != nil {
					return err
				}
				lim, err := k8sCheckResourceMap(p.Limits, "limits")
				if err != nil {
					return err
				}
				for k, r := range req {
					if l, ok := lim[k]; ok && r.Cmp(l) > 0 {
						return fmt.Errorf("requests.%s（%s）不能大于 limits.%s（%s）", k, r.String(), k, l.String())
					}
				}
				return nil
			},
			title: func(p *k8sParams) string { return "调整 " + k8sObjName(p) + " 的容器资源" },
			manual: func(p *k8sParams) string {
				var parts []string
				for _, kv := range []struct {
					f string
					m map[string]string
				}{{"--requests", p.Requests}, {"--limits", p.Limits}} {
					if len(kv.m) == 0 {
						continue
					}
					keys := make([]string, 0, len(kv.m))
					for k := range kv.m {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					vals := make([]string, 0, len(keys))
					for _, k := range keys {
						vals = append(vals, k+"="+kv.m[k])
					}
					parts = append(parts, kv.f+"="+strings.Join(vals, ","))
				}
				s := fmt.Sprintf("kubectl set resources %s/%s -n %s %s", p.Kind, p.Name, p.Namespace, strings.Join(parts, " "))
				if p.Container != "" {
					s += " -c " + p.Container
				}
				return s
			},
			preview: k8sPreviewSetResources,
			run:     k8sRunSetResources,
			undo:    k8sUndoContainer,
		},
		{
			spec: k8sSpec("k8s.set_image", "更新镜像", "修改容器镜像并触发滚动更新。回滚恢复原镜像。",
				RiskChange, k8sPermWrite, k8sSchema([]string{"kind", "name", "image"}, k8sWorkloadKinds, "kind", "namespace", "name", "container", "image")),
			check: func(p *k8sParams) error {
				if err := k8sCheckWorkload(k8sWorkloadKinds...)(p); err != nil {
					return err
				}
				if p.Image == "" {
					return errors.New("缺少参数 image（新镜像地址）")
				}
				if len(p.Image) > 512 || !k8sImageRe.MatchString(p.Image) {
					return fmt.Errorf("镜像地址「%s」不合法", p.Image)
				}
				return nil
			},
			title: func(p *k8sParams) string { return "将 " + k8sObjName(p) + " 镜像更新为 " + p.Image },
			manual: func(p *k8sParams) string {
				c := p.Container
				if c == "" {
					c = "<容器名>"
				}
				return fmt.Sprintf("kubectl set image %s/%s -n %s %s=%s", p.Kind, p.Name, p.Namespace, c, p.Image)
			},
			preview: k8sPreviewSetImage,
			run:     k8sRunSetImage,
			undo:    k8sUndoContainer,
		},
		{
			spec: k8sSpec("k8s.rollback_deployment", "回滚 Deployment 版本", "将 Deployment 的 Pod 模板恢复为上一个（或指定）ReplicaSet 版本，等同 kubectl rollout undo。",
				RiskChange, k8sPermWrite, k8sSchema([]string{"name"}, nil, "namespace", "name", "revision")),
			check: func(p *k8sParams) error {
				if err := k8sCheckWorkload("deployment")(p); err != nil {
					return err
				}
				if p.Revision < 0 {
					return errors.New("revision 不能为负数")
				}
				return nil
			},
			title: func(p *k8sParams) string {
				if p.Revision > 0 {
					return fmt.Sprintf("将 %s 回滚到第 %d 版", k8sObjName(p), p.Revision)
				}
				return "将 " + k8sObjName(p) + " 回滚到上一版本"
			},
			manual: func(p *k8sParams) string {
				s := fmt.Sprintf("kubectl rollout undo deployment/%s -n %s", p.Name, p.Namespace)
				if p.Revision > 0 {
					s += fmt.Sprintf(" --to-revision=%d", p.Revision)
				}
				return s
			},
			preview: k8sPreviewRollbackDeployment,
			run:     k8sRunRollbackDeployment,
			undo:    k8sUndoRollbackDeployment,
		},
		// ---- high
		{
			spec: k8sSpec("k8s.cordon", "封锁节点", "将节点设为不可调度，新 Pod 不再调度到该节点，现有 Pod 不受影响。回滚恢复原状态。",
				RiskHigh, k8sPermNode, k8sSchema([]string{"name"}, nil, "name")),
			check:   k8sCheckNode,
			title:   func(p *k8sParams) string { return "封锁节点 " + p.Name },
			manual:  func(p *k8sParams) string { return "kubectl cordon " + p.Name },
			preview: k8sPreviewCordon(true),
			run:     k8sRunCordon(true),
			undo:    k8sUndoCordon,
		},
		{
			spec: k8sSpec("k8s.uncordon", "解除节点封锁", "将节点恢复为可调度。回滚恢复原状态。",
				RiskHigh, k8sPermNode, k8sSchema([]string{"name"}, nil, "name")),
			check:   k8sCheckNode,
			title:   func(p *k8sParams) string { return "解除节点 " + p.Name + " 的封锁" },
			manual:  func(p *k8sParams) string { return "kubectl uncordon " + p.Name },
			preview: k8sPreviewCordon(false),
			run:     k8sRunCordon(false),
			undo:    k8sUndoCordon,
		},
		{
			spec: k8sSpec("k8s.drain", "排空节点", "封锁节点并通过 Eviction API 驱逐其上 Pod：遵守 PodDisruptionBudget，跳过 DaemonSet 与静态 Pod；无控制器或使用 emptyDir 的 Pod 默认阻止执行。回滚仅取消封锁。",
				RiskHigh, k8sPermNode, k8sSchema([]string{"name"}, nil, "name", "timeout_sec", "grace_period_sec", "force", "delete_emptydir_data")),
			check: func(p *k8sParams) error {
				if err := k8sCheckNode(p); err != nil {
					return err
				}
				if p.GracePeriodSec != nil && *p.GracePeriodSec < 0 {
					return errors.New("grace_period_sec 不能为负数")
				}
				return k8sRequireTimeout(p, 600, 30, 3600)
			},
			title: func(p *k8sParams) string { return "排空节点 " + p.Name },
			manual: func(p *k8sParams) string {
				s := fmt.Sprintf("kubectl drain %s --ignore-daemonsets --timeout=%ds", p.Name, p.TimeoutSec)
				if p.DeleteEmptyDirData {
					s += " --delete-emptydir-data"
				}
				if p.Force {
					s += " --force"
				}
				if p.GracePeriodSec != nil {
					s += fmt.Sprintf(" --grace-period=%d", *p.GracePeriodSec)
				}
				return s
			},
			preview: k8sPreviewDrain,
			run:     k8sRunDrain,
			undo:    k8sUndoDrain,
		},
		{
			spec: k8sSpec("k8s.delete_pod", "删除 Pod", "删除指定 Pod；由控制器管理的会自动重建。无法回滚。",
				RiskHigh, k8sPermWrite, k8sSchema([]string{"name"}, nil, "namespace", "name", "grace_period_sec")),
			check: func(p *k8sParams) error {
				p.Kind = "pod"
				if err := k8sRequireNamespace(p); err != nil {
					return err
				}
				if p.GracePeriodSec != nil && *p.GracePeriodSec < 0 {
					return errors.New("grace_period_sec 不能为负数")
				}
				return k8sRequireName(p, "Pod ")
			},
			title: func(p *k8sParams) string { return "删除 " + k8sObjName(p) },
			manual: func(p *k8sParams) string {
				s := fmt.Sprintf("kubectl delete pod %s -n %s", p.Name, p.Namespace)
				if p.GracePeriodSec != nil {
					s += fmt.Sprintf(" --grace-period=%d", *p.GracePeriodSec)
				}
				return s
			},
			preview: k8sPreviewDeletePod,
			run:     k8sRunDeletePod,
		},
		{
			spec: k8sSpec("k8s.apply_yaml", "应用资源清单", "以 server-side apply 应用 YAML（最多 20 个对象，禁止 Secret）；预览为服务端 dry-run 差异。回滚删除新建对象、恢复被修改对象。",
				RiskHigh, k8sPermWrite, k8sSchema([]string{"yaml"}, nil, "namespace", "yaml")),
			check: func(p *k8sParams) error {
				if err := k8sRequireNamespace(p); err != nil {
					return err
				}
				_, err := k8sParseManifests(p.YAML, p.Namespace)
				return err
			},
			title: func(p *k8sParams) string {
				objs, _ := k8sParseManifests(p.YAML, p.Namespace)
				names := make([]string, 0, len(objs))
				for _, o := range objs {
					names = append(names, o.GetKind()+"/"+o.GetName())
				}
				return "应用资源清单：" + strings.Join(names, "、")
			},
			manual: func(p *k8sParams) string {
				return fmt.Sprintf("cat <<'EOF' > aiops.yaml\n%s\nEOF\nkubectl diff --server-side -n %s -f aiops.yaml\nkubectl apply --server-side --field-manager=%s -n %s -f aiops.yaml",
					strings.TrimRight(p.YAML, "\n"), p.Namespace, k8sFieldManager, p.Namespace)
			},
			untyped: true,
			preview: k8sPreviewApplyYAML,
			run:     k8sRunApplyYAML,
			undo:    k8sUndoApplyYAML,
		},
	}
}

func k8sCheckNode(p *k8sParams) error {
	p.Kind = "node"
	p.Namespace = ""
	return k8sRequireName(p, "节点 ")
}

func init() {
	for _, a := range k8sActions() {
		registerAction(a)
	}
}
