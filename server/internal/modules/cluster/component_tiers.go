package cluster

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// Component tiers for the component center. Tier data is kept beside the
// catalog (keyed by component key) so ComponentDefinition stays unchanged.
const (
	ComponentTierRequired    = "required"
	ComponentTierRecommended = "recommended"
	ComponentTierOptional    = "optional"
)

var componentTierOrder = []string{ComponentTierRequired, ComponentTierRecommended, ComponentTierOptional}

type componentTierInfo struct {
	Tier       string
	TierReason string
	// DependsOn adds planning dependencies on top of the curated profile's
	// Dependencies; both are merged by componentDependencyGraph.
	DependsOn []string
	Conflicts []string
}

var componentTiers = map[string]componentTierInfo{
	// 必装
	"metrics-server": {Tier: ComponentTierRequired, TierReason: "HPA 与 kubectl top 依赖资源指标 API，缺失会导致自动扩缩容不可用。"},
	"cert-manager":   {Tier: ComponentTierRequired, TierReason: "统一签发与续期 TLS 证书，入口与 Webhook 证书依赖它。"},
	"prometheus":     {Tier: ComponentTierRequired, TierReason: "平台监控、告警规则与容量视图的指标来源。"},
	// 推荐
	"traefik":                {Tier: ComponentTierRecommended, TierReason: "当前唯一开放新安装的 Ingress 控制器，替代已退役的 ingress-nginx。"},
	"alertmanager":           {Tier: ComponentTierRecommended, TierReason: "将 Prometheus 告警去重、分组并通知到值班渠道。", DependsOn: []string{"prometheus"}},
	"loki":                   {Tier: ComponentTierRecommended, TierReason: "集中收集容器日志，排障时可按标签检索。"},
	"argocd":                 {Tier: ComponentTierRecommended, TierReason: "以 GitOps 方式管理应用发布，变更可审计、可回滚。"},
	"local-path-provisioner": {Tier: ComponentTierRecommended, TierReason: "无云盘的自建集群需要一个默认 StorageClass 才能创建 PVC。"},
	"velero":                 {Tier: ComponentTierRecommended, TierReason: "备份集群资源与持久卷，用于灾难恢复与迁移。"},
	"kyverno":                {Tier: ComponentTierRecommended, TierReason: "以 Kubernetes 原生策略做准入校验与基线约束。", Conflicts: []string{"gatekeeper"}},
	// 选装
	"istio-base":    {Tier: ComponentTierOptional, TierReason: "仅在需要服务网格时安装，提供 Istio CRD。"},
	"istiod":        {Tier: ComponentTierOptional, TierReason: "服务网格控制面，仅在启用 Istio 时需要。", DependsOn: []string{"istio-base"}},
	"istio-gateway": {Tier: ComponentTierOptional, TierReason: "Istio 网格入口网关，仅在启用 Istio 时需要。", DependsOn: []string{"istio-base", "istiod"}},
	"tempo":         {Tier: ComponentTierOptional, TierReason: "分布式追踪存储，按需启用。"},
	"jaeger":        {Tier: ComponentTierOptional, TierReason: "分布式追踪查询与存储，与 Tempo 二选一即可。"},
	"jenkins":       {Tier: ComponentTierOptional, TierReason: "传统 CI 流水线，已有其他 CI 时无需安装。"},
	"gatekeeper":    {Tier: ComponentTierOptional, TierReason: "基于 OPA 的准入策略；与 Kyverno 职责重叠，二者只建议启用其一。", Conflicts: []string{"kyverno"}},
	"elasticsearch": {Tier: ComponentTierOptional, TierReason: "仅发现既有安装，当前不提供新安装。"},
	"ingress-nginx": {Tier: ComponentTierOptional, TierReason: "仅发现既有安装；上游已退役，新集群请使用 Traefik。"},
}

func componentTierFor(key string) componentTierInfo {
	if info, ok := componentTiers[key]; ok {
		return info
	}
	return componentTierInfo{Tier: ComponentTierOptional, TierReason: "未分级组件，默认按选装处理。"}
}

// allComponentDefinitions returns the reviewed catalog followed by the
// display-only extra entries, sorted by key.
func allComponentDefinitions() []ComponentDefinition {
	out := sortedComponentCatalog()
	seen := make(map[string]bool, len(out))
	for _, d := range out {
		seen[d.Key] = true
	}
	for _, d := range extraComponentCatalog {
		if !seen[d.Key] {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

type componentGraphNode struct {
	DependsOn []string
	Conflicts []string
}

// componentDependencyGraph merges profile Dependencies with tier DependsOn and
// makes conflicts symmetric.
func componentDependencyGraph() map[string]componentGraphNode {
	graph := map[string]componentGraphNode{}
	for _, def := range allComponentDefinitions() {
		info := componentTierFor(def.Key)
		graph[def.Key] = componentGraphNode{
			DependsOn: uniqueSortedStrings(append(append([]string(nil), def.Dependencies...), info.DependsOn...)),
			Conflicts: uniqueSortedStrings(info.Conflicts),
		}
	}
	for key, node := range graph {
		for _, other := range node.Conflicts {
			o := graph[other]
			if !stringInList(o.Conflicts, key) {
				o.Conflicts = uniqueSortedStrings(append(o.Conflicts, key))
				graph[other] = o
			}
		}
	}
	return graph
}

func uniqueSortedStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// planInstall orders the requested keys so dependencies come first, reports
// dependencies that are neither installed nor requested (transitively), and
// lists conflicting pairs among requested and installed components.
func planInstall(keys []string, installed map[string]bool) (order []string, missing []string, conflicts [][2]string, err error) {
	return planInstallWithGraph(componentDependencyGraph(), keys, installed)
}

func planInstallWithGraph(graph map[string]componentGraphNode, keys []string, installed map[string]bool) (order []string, missing []string, conflicts [][2]string, err error) {
	requested := map[string]bool{}
	reqList := make([]string, 0, len(keys))
	for _, raw := range keys {
		key := strings.TrimSpace(raw)
		if key == "" {
			return nil, nil, nil, errors.New("组件标识不能为空")
		}
		if _, ok := graph[key]; !ok {
			return nil, nil, nil, fmt.Errorf("未知组件：%s", key)
		}
		if !requested[key] {
			requested[key] = true
			reqList = append(reqList, key)
		}
	}
	if len(reqList) == 0 {
		return nil, nil, nil, errors.New("请至少选择一个组件")
	}
	sort.Strings(reqList)

	// Cycle detection over everything reachable from the request.
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var post []string
	missingSet := map[string]bool{}
	var visit func(key string) error
	visit = func(key string) error {
		switch color[key] {
		case grey:
			idx := 0
			for i, k := range stack {
				if k == key {
					idx = i
					break
				}
			}
			cycle := append(append([]string(nil), stack[idx:]...), key)
			return fmt.Errorf("检测到组件依赖环：%s", strings.Join(cycle, " -> "))
		case black:
			return nil
		}
		color[key] = grey
		stack = append(stack, key)
		node, ok := graph[key]
		if !ok {
			return fmt.Errorf("组件 %s 依赖未登记的组件", stack[len(stack)-2])
		}
		for _, dep := range node.DependsOn {
			if _, known := graph[dep]; !known {
				return fmt.Errorf("组件 %s 依赖未登记的组件 %s", key, dep)
			}
			if !requested[dep] && !installed[dep] {
				missingSet[dep] = true
			}
			if err := visit(dep); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		color[key] = black
		post = append(post, key)
		return nil
	}
	for _, key := range reqList {
		if err := visit(key); err != nil {
			return nil, nil, nil, err
		}
	}
	for _, key := range post {
		if requested[key] {
			order = append(order, key)
		}
	}
	for key := range missingSet {
		missing = append(missing, key)
	}
	sort.Strings(missing)

	active := map[string]bool{}
	for k := range requested {
		active[k] = true
	}
	for k, v := range installed {
		if v {
			active[k] = true
		}
	}
	seenPair := map[[2]string]bool{}
	for _, key := range reqList {
		for _, other := range graph[key].Conflicts {
			if !active[other] {
				continue
			}
			pair := [2]string{key, other}
			if pair[0] > pair[1] {
				pair = [2]string{other, key}
			}
			if !seenPair[pair] {
				seenPair[pair] = true
				conflicts = append(conflicts, pair)
			}
		}
	}
	sort.Slice(conflicts, func(i, j int) bool {
		if conflicts[i][0] != conflicts[j][0] {
			return conflicts[i][0] < conflicts[j][0]
		}
		return conflicts[i][1] < conflicts[j][1]
	})
	return order, missing, conflicts, nil
}

type componentPlanRequest struct {
	Keys []string `json:"keys"`
}

type componentOverviewItem struct {
	Key                 string              `json:"key"`
	Name                string              `json:"name"`
	Category            string              `json:"category"`
	Tier                string              `json:"tier"`
	TierReason          string              `json:"tier_reason"`
	DependsOn           []string            `json:"depends_on"`
	Conflicts           []string            `json:"conflicts"`
	Installable         bool                `json:"installable"`
	Notes               string              `json:"notes,omitempty"`
	StatusKnown         bool                `json:"status_known"`
	Installed           bool                `json:"installed"`
	State               string              `json:"state"`
	Error               string              `json:"error,omitempty"`
	MissingDependencies []string            `json:"missing_dependencies"`
	Definition          ComponentDefinition `json:"definition"`
}

// componentInstalledState probes the existing (reviewed) catalog only; the
// display-only extras are reported as unknown. ok=false means the cluster
// client was unavailable and only catalog data can be returned.
func (m *Module) componentInstalledState(c *gin.Context, cl *Cluster) (map[string]ClusterCapability, bool) {
	kc, err := m.kube(c.Request.Context(), cl)
	if err != nil || kc == nil {
		return nil, false
	}
	ctx := c.Request.Context()
	workloads := listComponentWorkloads(ctx, kc)
	out := map[string]ClusterCapability{}
	for _, def := range sortedComponentCatalog() {
		out[def.Key] = componentCatalogCapability(ctx, kc, def.Key, workloads)
	}
	return out, true
}

func installedFromCapabilities(caps map[string]ClusterCapability) map[string]bool {
	installed := map[string]bool{}
	for key, capability := range caps {
		if capability.Available {
			installed[key] = true
		}
	}
	return installed
}

// handleComponentPlan: POST /:id/components/plan {"keys": [...]}
func (m *Module) handleComponentPlan(c *gin.Context) {
	cl, ok := m.componentScopeAccess(c, false)
	if !ok {
		return
	}
	var req componentPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "请求体格式错误，需要 {\"keys\": [...]}"))
		return
	}
	caps, statusKnown := m.componentInstalledState(c, cl)
	installed := installedFromCapabilities(caps)
	order, missing, conflicts, err := planInstall(req.Keys, installed)
	if err != nil {
		httpx.Fail(c, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, err.Error()))
		return
	}
	notInstallable := []string{}
	defs := map[string]ComponentDefinition{}
	for _, d := range allComponentDefinitions() {
		defs[d.Key] = d
	}
	for _, key := range order {
		if !defs[key].Installable {
			notInstallable = append(notInstallable, key)
		}
	}
	conflictOut := make([][]string, 0, len(conflicts))
	for _, pair := range conflicts {
		conflictOut = append(conflictOut, []string{pair[0], pair[1]})
	}
	if order == nil {
		order = []string{}
	}
	if missing == nil {
		missing = []string{}
	}
	installedKeys := make([]string, 0, len(installed))
	for k := range installed {
		installedKeys = append(installedKeys, k)
	}
	sort.Strings(installedKeys)
	httpx.OK(c, gin.H{
		"cluster_id":      cl.ID,
		"order":           order,
		"missing":         missing,
		"conflicts":       conflictOut,
		"not_installable": notInstallable,
		"installed":       installedKeys,
		"status_known":    statusKnown,
		"ready":           len(missing) == 0 && len(conflicts) == 0 && len(notInstallable) == 0,
	})
}

// handleComponentOverview: GET /:id/components/overview, grouped by tier.
func (m *Module) handleComponentOverview(c *gin.Context) {
	cl, ok := m.componentScopeAccess(c, false)
	if !ok {
		return
	}
	caps, statusKnown := m.componentInstalledState(c, cl)
	installed := installedFromCapabilities(caps)
	graph := componentDependencyGraph()
	groups := map[string][]componentOverviewItem{}
	for _, tier := range componentTierOrder {
		groups[tier] = []componentOverviewItem{}
	}
	for _, def := range allComponentDefinitions() {
		info := componentTierFor(def.Key)
		node := graph[def.Key]
		item := componentOverviewItem{
			Key: def.Key, Name: def.Name, Category: def.Category,
			Tier: info.Tier, TierReason: info.TierReason,
			DependsOn: nonNil(node.DependsOn), Conflicts: nonNil(node.Conflicts),
			Installable: def.Installable, Notes: def.Notes,
			State: "unknown", MissingDependencies: []string{}, Definition: def,
		}
		if capability, found := caps[def.Key]; found {
			item.StatusKnown = true
			item.Installed = capability.Available
			item.State = capability.State
			item.Error = capability.Error
		}
		if statusKnown {
			for _, dep := range node.DependsOn {
				if !installed[dep] {
					item.MissingDependencies = append(item.MissingDependencies, dep)
				}
			}
		}
		groups[info.Tier] = append(groups[info.Tier], item)
	}
	tiers := make([]gin.H, 0, len(componentTierOrder))
	for _, tier := range componentTierOrder {
		tiers = append(tiers, gin.H{"tier": tier, "label": componentTierLabel(tier), "items": groups[tier]})
	}
	httpx.OK(c, gin.H{"cluster_id": cl.ID, "status_known": statusKnown, "tiers": tiers})
}

func componentTierLabel(tier string) string {
	switch tier {
	case ComponentTierRequired:
		return "必装"
	case ComponentTierRecommended:
		return "推荐"
	default:
		return "选装"
	}
}

func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
