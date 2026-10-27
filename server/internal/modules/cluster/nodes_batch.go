package cluster

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/types"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// NodePool is not a core Kubernetes resource. This module therefore exposes
// label-based node groups for imported/self-managed clusters and never offers
// scale/upgrade buttons for them. Provider-managed pools would need a separate
// real provider integration; until one exists the API reports that explicitly.

type nodeGroup struct {
	Name          string    `json:"name"`
	LabelValue    string    `json:"label_value"`
	Nodes         []string  `json:"nodes"`
	Ready         int       `json:"ready"`
	Unschedulable int       `json:"unschedulable"`
	Roles         []string  `json:"roles"`
	Taints        []string  `json:"taints"`
}

type nodeBatchLabelItem struct {
	Name   string             `json:"name"`
	Labels map[string]*string `json:"labels"`
}

type nodeBatchLabelsParams struct {
	Nodes  []string             `json:"nodes"`
	Labels map[string]*string   `json:"labels"`
	Items  []nodeBatchLabelItem `json:"items"`
}

type nodeBatchTaintItem struct {
	Name   string         `json:"name"`
	Taints []corev1.Taint `json:"taints,omitempty"`
	Add    []corev1.Taint `json:"add,omitempty"`
	Remove []corev1.Taint `json:"remove,omitempty"`
}

type nodeBatchTaintsParams struct {
	Nodes  []string            `json:"nodes"`
	Taints []corev1.Taint      `json:"taints"`
	Items  []nodeBatchTaintItem `json:"items"`
}

func (m *Module) nodeGroups(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	label := strings.TrimSpace(c.DefaultQuery("label", "node.xingshu.com/pool"))
	if errs := validation.IsQualifiedName(label); len(errs) > 0 {
		httpx.Fail(c, httpx.BadRequest("label must be a qualified Kubernetes label key"))
		return
	}
	list, err := kc.cs.CoreV1().Nodes().List(c.Request.Context(), metav1.ListOptions{})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	groups := map[string]*nodeGroup{}
	for _, n := range list.Items {
		value, ok := n.Labels[label]
		if !ok {
			value = ""
		}
		name := value
		if name == "" {
			name = "未设置"
		}
		g := groups[name]
		if g == nil {
			g = &nodeGroup{Name: name, LabelValue: value, Nodes: []string{}, Roles: []string{}, Taints: []string{}}
			groups[name] = g
		}
		g.Nodes = append(g.Nodes, n.Name)
		row := nodeRow(n)
		if row.Ready {
			g.Ready++
		}
		if row.Unschedulable {
			g.Unschedulable++
		}
		for _, role := range row.Roles {
			if !containsString(g.Roles, role) {
				g.Roles = append(g.Roles, role)
			}
		}
		for _, taint := range row.Taints {
			text := taint.Key
			if taint.Value != "" {
				text += "=" + taint.Value
			}
			text += ":" + string(taint.Effect)
			if !containsString(g.Taints, text) {
				g.Taints = append(g.Taints, text)
			}
		}
	}
	out := make([]nodeGroup, 0, len(groups))
	for _, g := range groups {
		sort.Strings(g.Nodes)
		sort.Strings(g.Roles)
		sort.Strings(g.Taints)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	httpx.OK(c, gin.H{"label": label, "items": out, "total": len(out), "provider_integration": false,
		"note": "NodePool 是云厂商/集群供应商资源；本平台当前仅按所选标签做节点分组，不提供扩缩容、自动伸缩或滚动升级。"})
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// nodePreflight is read-only: it checks identity, readiness and current
// maintenance state before a batch request is submitted for approval.
func (m *Module) nodePreflight(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	var in struct {
		Nodes  []string `json:"nodes"`
		Action string   `json:"action"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	if len(in.Nodes) == 0 || len(in.Nodes) > 200 {
		httpx.Fail(c, httpx.BadRequest("nodes must contain 1..200 names"))
		return
	}
	switch in.Action {
	case "", "labels", "taints", "drain":
	default:
		httpx.Fail(c, httpx.BadRequest("action must be labels, taints or drain"))
		return
	}
	results := make([]gin.H, 0, len(in.Nodes))
	warnings := 0
	for _, name := range in.Nodes {
		row := gin.H{"name": name, "exists": false}
		n, err := kc.cs.CoreV1().Nodes().Get(c.Request.Context(), name, metav1.GetOptions{})
		if err != nil {
			row["error"] = "node not found or not readable"
			row["blocked"] = true
			warnings++
			results = append(results, row)
			continue
		}
		vr := nodeRow(*n)
		row["exists"] = true
		row["ready"] = vr.Ready
		row["unschedulable"] = vr.Unschedulable
		row["roles"] = vr.Roles
		row["status"] = vr.Status
		var notes []string
		if !vr.Ready {
			notes = append(notes, "节点 NotReady")
		}
		if in.Action == "drain" && vr.Unschedulable {
			notes = append(notes, "节点已封锁")
		}
		if in.Action == "drain" {
			pods, err := kc.cs.CoreV1().Pods("").List(c.Request.Context(), metav1.ListOptions{
				FieldSelector: "spec.nodeName=" + name, Limit: 2000,
			})
			if err != nil {
				notes = append(notes, "无法读取节点 Pod 列表："+err.Error())
			} else {
				row["pod_count"] = len(pods.Items)
				row["daemonset_pods"] = 0
				row["unmanaged_pods"] = 0
				for _, pod := range pods.Items {
					owner := metav1.GetControllerOf(&pod)
					if owner != nil && owner.Kind == "DaemonSet" {
						row["daemonset_pods"] = row["daemonset_pods"].(int) + 1
					} else if owner == nil {
						row["unmanaged_pods"] = row["unmanaged_pods"].(int) + 1
					}
				}
			}
		}
		if len(notes) > 0 {
			row["warnings"] = notes
			warnings++
		}
		results = append(results, row)
	}
	httpx.OK(c, gin.H{"action": in.Action, "items": results, "warning_count": warnings})
}

func validateNodeBatchLabels(p *nodeBatchLabelsParams) error {
	if len(p.Items) == 0 {
		if len(p.Nodes) == 0 || len(p.Nodes) > 200 {
			return httpx.BadRequest("nodes must contain 1..200 names")
		}
		if len(p.Labels) == 0 {
			return httpx.BadRequest("labels is required")
		}
		items := make([]nodeBatchLabelItem, 0, len(p.Nodes))
		for _, name := range p.Nodes {
			items = append(items, nodeBatchLabelItem{Name: name, Labels: p.Labels})
		}
		p.Items = items
	}
	if len(p.Items) > 200 {
		return httpx.BadRequest("at most 200 nodes per batch")
	}
	for _, item := range p.Items {
		if errs := validation.IsDNS1123Subdomain(item.Name); len(errs) > 0 {
			return httpx.BadRequest("invalid node name " + item.Name)
		}
		if len(item.Labels) == 0 {
			return httpx.BadRequest("each item needs at least one label")
		}
		for key, value := range item.Labels {
			if errs := validation.IsQualifiedName(key); len(errs) > 0 {
				return httpx.BadRequest("invalid label key " + key)
			}
			if value != nil {
				if errs := validation.IsValidLabelValue(*value); len(errs) > 0 {
					return httpx.BadRequest("invalid label value for " + key)
				}
			}
		}
	}
	return nil
}

func validateNodeBatchTaints(p *nodeBatchTaintsParams) error {
	if len(p.Items) == 0 {
		if len(p.Nodes) == 0 || len(p.Nodes) > 200 {
			return httpx.BadRequest("nodes must contain 1..200 names")
		}
		items := make([]nodeBatchTaintItem, 0, len(p.Nodes))
		for _, name := range p.Nodes {
			items = append(items, nodeBatchTaintItem{Name: name, Taints: p.Taints})
		}
		p.Items = items
	}
	if len(p.Items) > 200 {
		return httpx.BadRequest("at most 200 nodes per batch")
	}
	for _, item := range p.Items {
		if errs := validation.IsDNS1123Subdomain(item.Name); len(errs) > 0 {
			return httpx.BadRequest("invalid node name " + item.Name)
		}
		hasOperations := len(item.Add) > 0 || len(item.Remove) > 0
		if hasOperations && len(item.Taints) > 0 {
			return httpx.BadRequest("full taints cannot be combined with add/remove operations")
		}
		if !hasOperations && len(item.Taints) == 0 {
			return httpx.BadRequest("each item needs taints or an add/remove operation")
		}
		taints := make([]corev1.Taint, 0, len(item.Taints)+len(item.Add)+len(item.Remove))
		taints = append(taints, item.Taints...)
		taints = append(taints, item.Add...)
		taints = append(taints, item.Remove...)
		for _, taint := range taints {
			if errs := validation.IsQualifiedName(taint.Key); len(errs) > 0 {
				return httpx.BadRequest("invalid taint key " + taint.Key)
			}
			if taint.Value != "" {
				if errs := validation.IsValidLabelValue(taint.Value); len(errs) > 0 {
					return httpx.BadRequest("invalid taint value for " + taint.Key)
				}
			}
			switch taint.Effect {
			case corev1.TaintEffectNoSchedule, corev1.TaintEffectPreferNoSchedule, corev1.TaintEffectNoExecute:
			default:
				return httpx.BadRequest("taint effect must be NoSchedule, PreferNoSchedule or NoExecute")
			}
		}
	}
	return nil
}

func executeNodeBatchLabels(ctx context.Context, kc *kubeClients, pl changePayload) (any, error) {
	var p nodeBatchLabelsParams
	if err := decodeParams(pl.Params, &p); err != nil {
		return nil, err
	}
	results := make([]gin.H, 0, len(p.Items))
	failed := 0
	for _, item := range p.Items {
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": item.Labels}})
		n, err := kc.cs.CoreV1().Nodes().Patch(ctx, item.Name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: fieldManager})
		if err != nil {
			failed++
			results = append(results, gin.H{"name": item.Name, "status": "failed", "error": err.Error()})
			continue
		}
		results = append(results, gin.H{"name": n.Name, "status": "updated", "labels": n.Labels})
	}
	return gin.H{"failed": failed, "total": len(results), "items": results}, nil
}

func executeNodeBatchTaints(ctx context.Context, kc *kubeClients, pl changePayload) (any, error) {
	var p nodeBatchTaintsParams
	if err := decodeParams(pl.Params, &p); err != nil {
		return nil, err
	}
	results := make([]gin.H, 0, len(p.Items))
	failed := 0
	for _, item := range p.Items {
		n, err := kc.cs.CoreV1().Nodes().Get(ctx, item.Name, metav1.GetOptions{})
		if err == nil {
			if len(item.Add) == 0 && len(item.Remove) == 0 {
				n.Spec.Taints = item.Taints
			} else {
				for _, remove := range item.Remove {
					for i := 0; i < len(n.Spec.Taints); {
						current := n.Spec.Taints[i]
						if current.Key == remove.Key && current.Value == remove.Value && current.Effect == remove.Effect {
							n.Spec.Taints = append(n.Spec.Taints[:i], n.Spec.Taints[i+1:]...)
							continue
						}
						i++
					}
				}
				for _, add := range item.Add {
					found := false
					for i := range n.Spec.Taints {
						if n.Spec.Taints[i].Key == add.Key && n.Spec.Taints[i].Effect == add.Effect {
							n.Spec.Taints[i] = add
							found = true
							break
						}
					}
					if !found {
						n.Spec.Taints = append(n.Spec.Taints, add)
					}
				}
			}
			n, err = kc.cs.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{FieldManager: fieldManager})
		}
		if err != nil {
			failed++
			results = append(results, gin.H{"name": item.Name, "status": "failed", "error": err.Error()})
			continue
		}
		results = append(results, gin.H{"name": n.Name, "status": "updated", "taints": n.Spec.Taints})
	}
	return gin.H{"failed": failed, "total": len(results), "items": results}, nil
}
