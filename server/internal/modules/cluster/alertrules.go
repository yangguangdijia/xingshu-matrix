package cluster

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// 告警规则：直接读写集群里的 PrometheusRule（monitoring.coreos.com/v1），
// 不再只停留在 Alertmanager 侧配置。写规则后由 Prometheus Operator 自动生效。

var promRuleGVR = schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1", Resource: "prometheusrules"}

// AlertRule 是给前端用的扁平结构：一条 rule = 一个 (规则组, 告警名)
type AlertRule struct {
	RuleGroup string `json:"rule_group"`
	Name      string `json:"name"`
	Expr      string `json:"expr"`
	For       string `json:"for"`
	Severity  string `json:"severity"`
	Summary   string `json:"summary"`
	Namespace string `json:"namespace"`
	Source    string `json:"source"` // PrometheusRule 名称
}

func (m *Module) listAlertRules(c *gin.Context) {
	cl, ok := m.clusterFor(c)
	if !ok {
		return
	}
	kc, err := m.kube(c.Request.Context(), cl)
	if err != nil {
		httpx.Fail(c, httpx.BadRequest("无法连接集群："+err.Error()))
		return
	}
	ns := c.Query("namespace")
	ri := kc.dyn.Resource(promRuleGVR)
	var list *unstructured.UnstructuredList
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20e9)
	defer cancel()
	if ns != "" {
		list, err = ri.Namespace(ns).List(ctx, metav1.ListOptions{})
	} else {
		list, err = ri.List(ctx, metav1.ListOptions{})
	}
	if err != nil {
		httpx.Fail(c, httpx.BadRequest("读取 PrometheusRule 失败（集群可能未安装 prometheus-operator）："+err.Error()))
		return
	}
	out := make([]AlertRule, 0, 32)
	for _, item := range list.Items {
		groups, _, _ := unstructured.NestedSlice(item.Object, "spec", "groups")
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			gname, _ := gm["name"].(string)
			rules, _ := gm["rules"].([]any)
			for _, r := range rules {
				rm, _ := r.(map[string]any)
				if rm == nil {
					continue
				}
				labels, _ := rm["labels"].(map[string]any)
				ann, _ := rm["annotations"].(map[string]any)
				out = append(out, AlertRule{
					RuleGroup: gname,
					Name:      str(rm["alert"]),
					Expr:      str(rm["expr"]),
					For:       str(rm["for"]),
					Severity:  str(labels["severity"]),
					Summary:   str(ann["summary"]),
					Namespace: item.GetNamespace(),
					Source:    item.GetName(),
				})
			}
		}
	}
	httpx.OK(c, gin.H{"items": out, "total": len(out)})
}

type alertRuleInput struct {
	Namespace string `json:"namespace" binding:"required"`
	RuleGroup string `json:"rule_group" binding:"required,max=128"`
	Name      string `json:"name" binding:"required,max=128"`
	Expr      string `json:"expr" binding:"required"`
	For       string `json:"for" binding:"max=32"`
	Severity  string `json:"severity" binding:"max=32"`
	Summary   string `json:"summary" binding:"max=512"`
}

// createAlertRule 在指定命名空间写入/追加一条 PrometheusRule 规则（同名规则组则追加）。
func (m *Module) createAlertRule(c *gin.Context) {
	cl, ok := m.clusterFor(c)
	if !ok {
		return
	}
	var in alertRuleInput
	if !httpx.Bind(c, &in) {
		return
	}
	kc, err := m.kube(c.Request.Context(), cl)
	if err != nil {
		httpx.Fail(c, httpx.BadRequest("无法连接集群："+err.Error()))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20e9)
	defer cancel()
	ri := kc.dyn.Resource(promRuleGVR).Namespace(in.Namespace)
	ruleName := "matrix-" + in.RuleGroup
	obj, err := ri.Get(ctx, ruleName, metav1.GetOptions{})
	if err != nil {
		obj = &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "monitoring.coreos.com/v1",
			"kind":       "PrometheusRule",
			"metadata":   map[string]any{"name": ruleName, "namespace": in.Namespace},
			"spec":       map[string]any{"groups": []any{}},
		}}
	}
	rule := map[string]any{
		"alert": in.Name,
		"expr":  in.Expr,
	}
	if in.For != "" {
		rule["for"] = in.For
	}
	if in.Severity != "" {
		rule["labels"] = map[string]any{"severity": in.Severity}
	}
	if in.Summary != "" {
		rule["annotations"] = map[string]any{"summary": in.Summary}
	}
	groups, _, _ := unstructured.NestedSlice(obj.Object, "spec", "groups")
	found := false
	for i, g := range groups {
		gm, _ := g.(map[string]any)
		if str(gm["name"]) != in.RuleGroup {
			continue
		}
		rules, _ := gm["rules"].([]any)
		gm["rules"] = append(rules, rule)
		groups[i] = gm
		found = true
		break
	}
	if !found {
		groups = append(groups, map[string]any{"name": in.RuleGroup, "rules": []any{rule}})
	}
	_ = unstructured.SetNestedSlice(obj.Object, groups, "spec", "groups")
	if obj.GetResourceVersion() == "" {
		_, err = ri.Create(ctx, obj, metav1.CreateOptions{})
	} else {
		_, err = ri.Update(ctx, obj, metav1.UpdateOptions{})
	}
	if err != nil {
		httpx.Fail(c, httpx.BadRequest("写入 PrometheusRule 失败："+err.Error()))
		return
	}
	c.JSON(http.StatusCreated, gin.H{"code": 0, "message": "created", "data": gin.H{"prometheus_rule": ruleName, "namespace": in.Namespace}})
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func (m *Module) registerAlertRuleRoutes(g *gin.RouterGroup) {
	g.GET("/:id/alert-rules", rbac.RequirePerm(PermClusterRead), m.listAlertRules)
	g.POST("/:id/alert-rules", rbac.RequirePerm(PermClusterManage), m.createAlertRule)
}
