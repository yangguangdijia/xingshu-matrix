package cluster

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// ---------------------------------------------------------------- nodes

// NodeRow is a node summary.
type NodeRow struct {
	Name             string                 `json:"name"`
	Ready            bool                   `json:"ready"`
	Status           string                 `json:"status"`
	Roles            []string               `json:"roles"`
	Unschedulable    bool                   `json:"unschedulable"`
	InternalIP       string                 `json:"internal_ip"`
	KubeletVersion   string                 `json:"kubelet_version"`
	OSImage          string                 `json:"os_image"`
	KernelVersion    string                 `json:"kernel_version"`
	ContainerRuntime string                 `json:"container_runtime"`
	Architecture     string                 `json:"architecture"`
	Capacity         map[string]string      `json:"capacity"`
	Allocatable      map[string]string      `json:"allocatable"`
	Conditions       []corev1.NodeCondition `json:"conditions"`
	Labels           map[string]string      `json:"labels"`
	Taints           []corev1.Taint         `json:"taints"`
	Usage            *NodeUsage             `json:"usage"` // nil when metrics.k8s.io is unavailable
	CreatedAt        time.Time              `json:"created_at"`
}

// NodeUsage comes from metrics.k8s.io (metrics-server).
type NodeUsage struct {
	CPUMilli    int64   `json:"cpu_millicores"`
	MemoryBytes int64   `json:"memory_bytes"`
	CPUPercent  float64 `json:"cpu_percent"`    // of allocatable
	MemPercent  float64 `json:"memory_percent"` // of allocatable
	Timestamp   string  `json:"timestamp"`
}

func resList(rl corev1.ResourceList) map[string]string {
	out := map[string]string{}
	for k, v := range rl {
		out[string(k)] = v.String()
	}
	return out
}

func nodeRow(n corev1.Node) NodeRow {
	r := NodeRow{Name: n.Name, Unschedulable: n.Spec.Unschedulable, Labels: n.Labels, Taints: n.Spec.Taints,
		KubeletVersion: n.Status.NodeInfo.KubeletVersion, OSImage: n.Status.NodeInfo.OSImage,
		KernelVersion: n.Status.NodeInfo.KernelVersion, ContainerRuntime: n.Status.NodeInfo.ContainerRuntimeVersion,
		Architecture: n.Status.NodeInfo.Architecture, Capacity: resList(n.Status.Capacity),
		Allocatable: resList(n.Status.Allocatable), Conditions: n.Status.Conditions, CreatedAt: n.CreationTimestamp.Time,
		Roles: []string{}}
	for k := range n.Labels {
		if role, ok := strings.CutPrefix(k, "node-role.kubernetes.io/"); ok && role != "" {
			r.Roles = append(r.Roles, role)
		}
	}
	sort.Strings(r.Roles)
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			r.InternalIP = a.Address
		}
	}
	r.Status = "NotReady"
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			r.Ready, r.Status = true, "Ready"
		}
	}
	if r.Unschedulable {
		r.Status += ",SchedulingDisabled"
	}
	return r
}

// nodeRows lists nodes and merges metrics when metrics-server is installed.
func nodeRows(ctx context.Context, kc *kubeClients, name string) ([]NodeRow, bool, error) {
	var nodes []corev1.Node
	if name != "" {
		n, err := kc.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, false, err
		}
		nodes = []corev1.Node{*n}
	} else {
		l, err := kc.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, false, err
		}
		nodes = l.Items
	}
	rows := make([]NodeRow, 0, len(nodes))
	byName := map[string]int{}
	for i, n := range nodes {
		rows = append(rows, nodeRow(n))
		byName[n.Name] = i
	}
	metricsOK := false
	if kc.metrics != nil {
		mctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if ml, err := kc.metrics.MetricsV1beta1().NodeMetricses().List(mctx, metav1.ListOptions{}); err == nil {
			metricsOK = true
			for _, nm := range ml.Items {
				i, ok := byName[nm.Name]
				if !ok {
					continue
				}
				u := &NodeUsage{CPUMilli: nm.Usage.Cpu().MilliValue(), MemoryBytes: nm.Usage.Memory().Value(),
					Timestamp: nm.Timestamp.UTC().Format(time.RFC3339)}
				alloc := nodes[i].Status.Allocatable
				if c := alloc.Cpu().MilliValue(); c > 0 {
					u.CPUPercent = round1(float64(u.CPUMilli) * 100 / float64(c))
				}
				if mem := alloc.Memory().Value(); mem > 0 {
					u.MemPercent = round1(float64(u.MemoryBytes) * 100 / float64(mem))
				}
				rows[i].Usage = u
			}
		}
	}
	return rows, metricsOK, nil
}

func round1(f float64) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', 1, 64), 64)
	return v
}

func (m *Module) withKube(c *gin.Context) (*Cluster, *kubeClients, bool) {
	cl, ok := m.clusterFor(c)
	if !ok {
		return nil, nil, false
	}
	kc, err := m.kube(c.Request.Context(), cl)
	if err != nil {
		httpx.Fail(c, httpx.BadRequest("cluster client: "+err.Error()))
		return nil, nil, false
	}
	return cl, kc, true
}

func (m *Module) listNodes(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	rows, mOK, err := nodeRows(c.Request.Context(), kc, "")
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	httpx.OK(c, gin.H{"items": rows, "total": len(rows), "metrics_available": mOK})
}

func (m *Module) getNode(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	rows, mOK, err := nodeRows(ctx, kc, c.Param("node"))
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	pods, err := kc.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("spec.nodeName", c.Param("node")).String()})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	pr := make([]PodRow, 0, len(pods.Items))
	for _, p := range pods.Items {
		pr = append(pr, podRow(p))
	}
	httpx.OK(c, gin.H{"node": rows[0], "metrics_available": mOK, "pods": pr})
}

// ---------------------------------------------------------------- namespaces

func namespaceRow(ns corev1.Namespace) map[string]any {
	return map[string]any{"name": ns.Name, "status": string(ns.Status.Phase), "labels": ns.Labels, "created_at": ns.CreationTimestamp.Time}
}

func (m *Module) listNamespaces(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	l, err := kc.cs.CoreV1().Namespaces().List(c.Request.Context(), metav1.ListOptions{LabelSelector: c.Query("label_selector")})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	items := make([]map[string]any, 0, len(l.Items))
	for _, ns := range l.Items {
		items = append(items, namespaceRow(ns))
	}
	httpx.OK(c, gin.H{"items": items, "total": len(items)})
}

// ---------------------------------------------------------------- events

// EventRow is a core/v1 Event summary.
type EventRow struct {
	Namespace string    `json:"namespace"`
	Type      string    `json:"type"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
	Kind      string    `json:"object_kind"`
	Name      string    `json:"object_name"`
	Source    string    `json:"source"`
	Count     int32     `json:"count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

type eventQuery struct {
	Namespace, Kind, Name, Type string
	Limit                       int
}

func listEventRows(ctx context.Context, cs kubernetes.Interface, q eventQuery) ([]EventRow, error) {
	sel := []fields.Selector{}
	if q.Kind != "" {
		sel = append(sel, fields.OneTermEqualSelector("involvedObject.kind", q.Kind))
	}
	if q.Name != "" {
		sel = append(sel, fields.OneTermEqualSelector("involvedObject.name", q.Name))
	}
	if q.Type != "" {
		sel = append(sel, fields.OneTermEqualSelector("type", q.Type))
	}
	l, err := cs.CoreV1().Events(q.Namespace).List(ctx, metav1.ListOptions{FieldSelector: fields.AndSelectors(sel...).String()})
	if err != nil {
		return nil, err
	}
	rows := make([]EventRow, 0, len(l.Items))
	for _, e := range l.Items {
		last := e.LastTimestamp.Time
		if last.IsZero() {
			last = e.EventTime.Time
		}
		if last.IsZero() {
			last = e.CreationTimestamp.Time
		}
		src := e.Source.Component
		if src == "" {
			src = e.ReportingController
		}
		cnt := e.Count
		if cnt == 0 && e.Series != nil {
			cnt = e.Series.Count
		}
		rows = append(rows, EventRow{Namespace: e.Namespace, Type: e.Type, Reason: e.Reason, Message: e.Message,
			Kind: e.InvolvedObject.Kind, Name: e.InvolvedObject.Name, Source: src, Count: cnt,
			FirstSeen: e.FirstTimestamp.Time, LastSeen: last})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].LastSeen.After(rows[j].LastSeen) })
	if q.Limit <= 0 || q.Limit > 1000 {
		q.Limit = 200
	}
	if len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	return rows, nil
}

func (m *Module) listEvents(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	rows, err := listEventRows(c.Request.Context(), kc.cs, eventQuery{Namespace: c.Query("namespace"), Kind: c.Query("kind"),
		Name: c.Query("name"), Type: c.Query("type"), Limit: limit})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	httpx.OK(c, gin.H{"items": rows, "total": len(rows)})
}

// ---------------------------------------------------------------- workloads

// WorkloadRow is a uniform summary for deployments/statefulsets/daemonsets/jobs/cronjobs.
type WorkloadRow struct {
	Kind      string            `json:"kind"`
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Ready     string            `json:"ready"`
	Replicas  *int32            `json:"replicas,omitempty"`
	Status    string            `json:"status"`
	Images    []string          `json:"images"`
	Labels    map[string]string `json:"labels"`
	Extra     map[string]any    `json:"extra,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// PodRow is a pod summary.
type PodRow struct {
	Namespace  string            `json:"namespace"`
	Name       string            `json:"name"`
	Phase      string            `json:"phase"`
	Status     string            `json:"status"`
	Ready      string            `json:"ready"`
	Restarts   int32             `json:"restarts"`
	Node       string            `json:"node"`
	PodIP      string            `json:"pod_ip"`
	Containers []string          `json:"containers"`
	Images     []string          `json:"images"`
	Owner      string            `json:"owner"`
	Labels     map[string]string `json:"labels"`
	CreatedAt  time.Time         `json:"created_at"`
}

func images(spec corev1.PodSpec) []string {
	out := []string{}
	for _, c := range spec.Containers {
		out = append(out, c.Image)
	}
	return out
}

func podRow(p corev1.Pod) PodRow {
	r := PodRow{Namespace: p.Namespace, Name: p.Name, Phase: string(p.Status.Phase), Status: string(p.Status.Phase),
		Node: p.Spec.NodeName, PodIP: p.Status.PodIP, Images: images(p.Spec), Labels: p.Labels, CreatedAt: p.CreationTimestamp.Time}
	if p.Status.Reason != "" {
		r.Status = p.Status.Reason
	}
	ready := 0
	for _, c := range p.Spec.Containers {
		r.Containers = append(r.Containers, c.Name)
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
		r.Restarts += cs.RestartCount
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			r.Status = cs.State.Waiting.Reason
		} else if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" && p.Status.Phase != corev1.PodSucceeded {
			r.Status = cs.State.Terminated.Reason
		}
	}
	if p.DeletionTimestamp != nil {
		r.Status = "Terminating"
	}
	r.Ready = fmt.Sprintf("%d/%d", ready, len(p.Spec.Containers))
	if o := metav1.GetControllerOf(&p); o != nil {
		r.Owner = o.Kind + "/" + o.Name
	}
	return r
}

func listWorkloadRows(ctx context.Context, cs kubernetes.Interface, kind, ns string, opts metav1.ListOptions) (any, int, error) {
	switch kind {
	case "pods":
		l, err := cs.CoreV1().Pods(ns).List(ctx, opts)
		if err != nil {
			return nil, 0, err
		}
		rows := make([]PodRow, 0, len(l.Items))
		for _, p := range l.Items {
			rows = append(rows, podRow(p))
		}
		return rows, len(rows), nil
	}
	rows := []WorkloadRow{}
	switch kind {
	case "deployments":
		l, err := cs.AppsV1().Deployments(ns).List(ctx, opts)
		if err != nil {
			return nil, 0, err
		}
		for _, d := range l.Items {
			st := "Available"
			if d.Status.ReadyReplicas < d.Status.Replicas || d.Status.UpdatedReplicas < d.Status.Replicas {
				st = "Progressing"
			}
			rows = append(rows, WorkloadRow{Kind: "Deployment", Namespace: d.Namespace, Name: d.Name, Replicas: d.Spec.Replicas,
				Ready: fmt.Sprintf("%d/%d", d.Status.ReadyReplicas, ptrVal(d.Spec.Replicas)), Status: st, Images: images(d.Spec.Template.Spec),
				Labels: d.Labels, CreatedAt: d.CreationTimestamp.Time,
				Extra: map[string]any{"updated": d.Status.UpdatedReplicas, "available": d.Status.AvailableReplicas}})
		}
	case "statefulsets":
		l, err := cs.AppsV1().StatefulSets(ns).List(ctx, opts)
		if err != nil {
			return nil, 0, err
		}
		for _, s := range l.Items {
			st := "Ready"
			if s.Status.ReadyReplicas < ptrVal(s.Spec.Replicas) {
				st = "Progressing"
			}
			rows = append(rows, WorkloadRow{Kind: "StatefulSet", Namespace: s.Namespace, Name: s.Name, Replicas: s.Spec.Replicas,
				Ready: fmt.Sprintf("%d/%d", s.Status.ReadyReplicas, ptrVal(s.Spec.Replicas)), Status: st, Images: images(s.Spec.Template.Spec),
				Labels: s.Labels, CreatedAt: s.CreationTimestamp.Time})
		}
	case "daemonsets":
		l, err := cs.AppsV1().DaemonSets(ns).List(ctx, opts)
		if err != nil {
			return nil, 0, err
		}
		for _, d := range l.Items {
			st := "Ready"
			if d.Status.NumberReady < d.Status.DesiredNumberScheduled {
				st = "Progressing"
			}
			rows = append(rows, WorkloadRow{Kind: "DaemonSet", Namespace: d.Namespace, Name: d.Name,
				Ready: fmt.Sprintf("%d/%d", d.Status.NumberReady, d.Status.DesiredNumberScheduled), Status: st, Images: images(d.Spec.Template.Spec),
				Labels: d.Labels, CreatedAt: d.CreationTimestamp.Time})
		}
	case "jobs":
		l, err := cs.BatchV1().Jobs(ns).List(ctx, opts)
		if err != nil {
			return nil, 0, err
		}
		for _, j := range l.Items {
			st := "Running"
			for _, c := range j.Status.Conditions {
				if c.Status == corev1.ConditionTrue && (c.Type == "Complete" || c.Type == "Failed") {
					st = string(c.Type)
				}
			}
			comp := int32(1)
			if j.Spec.Completions != nil {
				comp = *j.Spec.Completions
			}
			rows = append(rows, WorkloadRow{Kind: "Job", Namespace: j.Namespace, Name: j.Name,
				Ready: fmt.Sprintf("%d/%d", j.Status.Succeeded, comp), Status: st, Images: images(j.Spec.Template.Spec),
				Labels: j.Labels, CreatedAt: j.CreationTimestamp.Time,
				Extra: map[string]any{"active": j.Status.Active, "failed": j.Status.Failed}})
		}
	case "cronjobs":
		l, err := cs.BatchV1().CronJobs(ns).List(ctx, opts)
		if err != nil {
			return nil, 0, err
		}
		for _, cj := range l.Items {
			st := "Active"
			if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
				st = "Suspended"
			}
			var last any
			if cj.Status.LastScheduleTime != nil {
				last = cj.Status.LastScheduleTime.Time
			}
			rows = append(rows, WorkloadRow{Kind: "CronJob", Namespace: cj.Namespace, Name: cj.Name,
				Ready: fmt.Sprintf("%d active", len(cj.Status.Active)), Status: st, Images: images(cj.Spec.JobTemplate.Spec.Template.Spec),
				Labels: cj.Labels, CreatedAt: cj.CreationTimestamp.Time,
				Extra: map[string]any{"schedule": cj.Spec.Schedule, "last_schedule": last}})
		}
	}
	return rows, len(rows), nil
}

func ptrVal(p *int32) int32 {
	if p == nil {
		return 1
	}
	return *p
}

func (m *Module) listWorkloads(c *gin.Context) {
	kind, err := normalizeKind(c.Param("kind"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	rows, n, err := listWorkloadRows(c.Request.Context(), kc.cs, kind, c.Query("namespace"),
		metav1.ListOptions{LabelSelector: c.Query("label_selector"), FieldSelector: c.Query("field_selector")})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	httpx.OK(c, gin.H{"items": rows, "total": n})
}

func (m *Module) getWorkloadYAML(c *gin.Context) {
	kind, err := normalizeKind(c.Param("kind"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	obj, err := kc.dyn.Resource(workloadGVR[kind]).Namespace(c.Param("ns")).Get(c.Request.Context(), c.Param("name"), metav1.GetOptions{})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	obj.SetManagedFields(nil)
	if c.Query("format") == "json" {
		httpx.OK(c, gin.H{"object": obj.Object})
		return
	}
	y, err := yaml.Marshal(obj.Object)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	httpx.OK(c, gin.H{"yaml": string(y)})
}

// ---------------------------------------------------------------- logs

const maxLogBytes = 5 << 20

func (m *Module) podLogs(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	opts := &corev1.PodLogOptions{Container: c.Query("container"), Previous: c.Query("previous") == "true",
		Timestamps: c.Query("timestamps") == "true", Follow: c.Query("follow") == "true"}
	tail := int64(500)
	if v, err := strconv.ParseInt(c.Query("tail_lines"), 10, 64); err == nil && v > 0 {
		tail = v
	}
	opts.TailLines = &tail
	if v, err := strconv.ParseInt(c.Query("since_seconds"), 10, 64); err == nil && v > 0 {
		opts.SinceSeconds = &v
	}
	if !opts.Follow {
		lb := int64(maxLogBytes)
		opts.LimitBytes = &lb
		b, err := kc.cs.CoreV1().Pods(c.Param("ns")).GetLogs(c.Param("name"), opts).Do(c.Request.Context()).Raw()
		if err != nil {
			httpx.Fail(c, kubeErr(err))
			return
		}
		httpx.OK(c, gin.H{"logs": string(b)})
		return
	}
	cs := kc.cs
	if kc.rest != nil {
		if sc, err := kubernetes.NewForConfig(streamConfig(kc.rest)); err == nil {
			cs = sc
		}
	}
	ctx := c.Request.Context()
	rc, err := cs.CoreV1().Pods(c.Param("ns")).GetLogs(c.Param("name"), opts).Stream(ctx)
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	defer rc.Close()
	streamSSE(ctx, c, rc)
}

// streamSSE relays lines from r as SSE "log" events with 15s heartbeats.
func streamSSE(ctx context.Context, c *gin.Context, r io.Reader) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	hb := time.NewTicker(15 * time.Second)
	defer hb.Stop()
	c.Status(200)
	c.Writer.Flush()
	for {
		select {
		case <-ctx.Done():
			return
		case l, ok := <-lines:
			if !ok {
				c.SSEvent("end", "EOF")
				c.Writer.Flush()
				return
			}
			c.SSEvent("log", l)
			c.Writer.Flush()
		case <-hb.C:
			_, _ = c.Writer.WriteString(": ping\n\n")
			c.Writer.Flush()
		}
	}
}
