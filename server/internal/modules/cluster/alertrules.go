package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// Alert rules are stored in PrometheusRule resources and applied only after
// the cluster change gate permits the mutation.

var promRuleGVR = schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1", Resource: "prometheusrules"}
var alertRuleNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var promDurationRe = regexp.MustCompile(`^(?:[0-9]+(?:ms|s|m|h|d|w|y))+$`)

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

func validateAlertRuleInput(in alertRuleInput) error {
	if errs := validation.IsDNS1123Label(in.Namespace); len(errs) > 0 {
		return httpx.BadRequest("invalid PrometheusRule namespace")
	}
	if strings.TrimSpace(in.RuleGroup) == "" {
		return httpx.BadRequest("rule_group is required")
	}
	if strings.TrimSpace(in.Name) == "" || !alertRuleNameRe.MatchString(in.Name) {
		return httpx.BadRequest("alert name must contain only letters, digits and underscores and must not start with a digit")
	}
	if strings.TrimSpace(in.Expr) == "" || len(in.Expr) > 4096 {
		return httpx.BadRequest("expr is required and must not exceed 4096 bytes")
	}
	if in.For != "" && !promDurationRe.MatchString(in.For) {
		return httpx.BadRequest("for must be a Prometheus duration such as 30s or 5m")
	}
	if in.Severity != "" && in.Severity != "critical" && in.Severity != "warning" && in.Severity != "info" {
		return httpx.BadRequest("severity must be critical, warning or info")
	}
	return nil
}

// createAlertRule routes PrometheusRule mutations through cluster.change.
func (m *Module) createAlertRule(c *gin.Context) {
	cl, ok := m.clusterFor(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	var in alertRuleInput
	if !httpx.Bind(c, &in) {
		return
	}
	in.Namespace = strings.TrimSpace(in.Namespace)
	in.RuleGroup = strings.TrimSpace(in.RuleGroup)
	in.Name = strings.TrimSpace(in.Name)
	in.Expr = strings.TrimSpace(in.Expr)
	in.For = strings.TrimSpace(in.For)
	in.Severity = strings.TrimSpace(in.Severity)
	in.Summary = strings.TrimSpace(in.Summary)
	if err := validateAlertRuleInput(in); err != nil {
		httpx.Fail(c, err)
		return
	}
	params, err := json.Marshal(in)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	pl := changePayload{ClusterID: cl.ID, Cluster: cl.Name, Action: "alert_rule_create",
		Namespace: in.Namespace, Kind: "prometheusrules", Name: "matrix-" + in.RuleGroup, Params: params}
	audit.SetAction(c, pl.Action)
	audit.SetDetail(c, gin.H{"namespace": in.Namespace, "rule_group": in.RuleGroup, "name": in.Name, "severity": in.Severity})
	result, ticket, err := m.runChange(c.Request.Context(), rbac.PrincipalFrom(c), cl, pl, c.Query("reason"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if ticket != nil {
		approval.RespondPending(c, ticket)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"code": 0, "message": "created", "data": result})
}

func (m *Module) applyAlertRule(parent context.Context, cl *Cluster, in alertRuleInput) (any, error) {
	if err := validateAlertRuleInput(in); err != nil {
		return nil, err
	}
	kc, err := m.kube(parent, cl)
	if err != nil {
		return nil, httpx.BadRequest("无法连接集群")
	}
	ctx, cancel := context.WithTimeout(parent, 20e9)
	defer cancel()
	ri := kc.dyn.Resource(promRuleGVR).Namespace(in.Namespace)
	ruleName := "matrix-" + in.RuleGroup
	obj, err := ri.Get(ctx, ruleName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, httpx.BadRequest("读取 PrometheusRule 失败")
		}
		obj = &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "monitoring.coreos.com/v1", "kind": "PrometheusRule",
			"metadata": map[string]any{"name": ruleName, "namespace": in.Namespace},
			"spec":     map[string]any{"groups": []any{}},
		}}
	}
	groups, _, _ := unstructured.NestedSlice(obj.Object, "spec", "groups")
	for _, group := range groups {
		groupMap, _ := group.(map[string]any)
		rules, _ := groupMap["rules"].([]any)
		for _, existing := range rules {
			ruleMap, _ := existing.(map[string]any)
			if str(ruleMap["alert"]) == in.Name {
				return nil, httpx.Conflict("告警名称已存在于该 PrometheusRule")
			}
		}
	}
	rule := map[string]any{"alert": in.Name, "expr": in.Expr}
	if in.For != "" {
		rule["for"] = in.For
	}
	if in.Severity != "" {
		rule["labels"] = map[string]any{"severity": in.Severity}
	}
	if in.Summary != "" {
		rule["annotations"] = map[string]any{"summary": in.Summary}
	}
	found := false
	for i, group := range groups {
		groupMap, _ := group.(map[string]any)
		if str(groupMap["name"]) == in.RuleGroup {
			rules, _ := groupMap["rules"].([]any)
			groupMap["rules"] = append(rules, rule)
			groups[i], found = groupMap, true
			break
		}
	}
	if !found {
		groups = append(groups, map[string]any{"name": in.RuleGroup, "rules": []any{rule}})
	}
	if err := unstructured.SetNestedSlice(obj.Object, groups, "spec", "groups"); err != nil {
		return nil, httpx.BadRequest("PrometheusRule 内容无效")
	}
	if obj.GetResourceVersion() == "" {
		_, err = ri.Create(ctx, obj, metav1.CreateOptions{})
	} else {
		_, err = ri.Update(ctx, obj, metav1.UpdateOptions{})
	}
	if err != nil {
		return nil, httpx.BadRequest("写入 PrometheusRule 失败")
	}
	return gin.H{"prometheus_rule": ruleName, "namespace": in.Namespace}, nil
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
