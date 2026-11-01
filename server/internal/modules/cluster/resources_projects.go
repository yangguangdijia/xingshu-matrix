package cluster

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/apimachinery/pkg/util/validation"
)

type projectRow struct {
	Name            string                `json:"name"`
	Phase           corev1.NamespacePhase `json:"phase"`
	Labels          map[string]string     `json:"labels"`
	Age             string                `json:"age"`
	CreatedAt       time.Time             `json:"created_at"`
	UID             string                `json:"uid"`
	ResourceVersion string                `json:"resource_version"`
	ResourceQuotas  []projectQuotaRow     `json:"resource_quotas"`
	LimitRanges     []projectLimitRow     `json:"limit_ranges"`
	QuotaRead       projectResourceRead   `json:"quota_read"`
	LimitRead       projectResourceRead   `json:"limit_read"`
	Truncated       bool                  `json:"truncated"`
}

type projectResourceRead struct {
	State     string `json:"state"`
	Complete  bool   `json:"complete"`
	Truncated bool   `json:"truncated"`
	Error     string `json:"error,omitempty"`
}

func projectReadStatus(truncated bool, err error) projectResourceRead {
	if err != nil {
		return projectResourceRead{State: resourceReadState(err), Error: "Kubernetes resource inventory is unavailable"}
	}
	return projectResourceRead{State: resourceListState(truncated), Complete: !truncated, Truncated: truncated}
}

type projectQuotaRow struct {
	Namespace       string                      `json:"namespace"`
	Name            string                      `json:"name"`
	UID             string                      `json:"uid"`
	ResourceVersion string                      `json:"resource_version"`
	Hard            map[string]string           `json:"hard"`
	Used            map[string]string           `json:"used"`
	StatusHard      map[string]string           `json:"status_hard"`
	Scopes          []corev1.ResourceQuotaScope `json:"scopes"`
	ScopeSelector   *corev1.ScopeSelector       `json:"scope_selector"`
}

type projectLimitRow struct {
	Namespace       string                  `json:"namespace"`
	Name            string                  `json:"name"`
	UID             string                  `json:"uid"`
	ResourceVersion string                  `json:"resource_version"`
	Limits          []corev1.LimitRangeItem `json:"limits"`
}

// RegisterProjectQuotaRoutes takes the existing /clusters group. Call
// RegisterProjectQuotaApprovals after assigning m.d and before serving writes.
func (m *Module) RegisterProjectQuotaRoutes(g *gin.RouterGroup) {
	g.GET("/:id/projects", rbac.RequirePerm(PermNSRead), m.listProjects)
	g.GET("/:id/projects/:namespace/quotas", rbac.RequirePerm(PermNSRead), m.listProjectQuotas)
	g.GET("/:id/projects/:namespace/limits", rbac.RequirePerm(PermNSRead), m.listProjectLimits)
	g.POST("/:id/projects", rbac.RequirePerm(PermNSWrite), m.projectQuotaWrite("project_create"))
	g.POST("/:id/quotas", rbac.RequirePerm(PermNSWrite), m.projectQuotaWrite("quota_create"))
	g.PATCH("/:id/quotas/:ns/:name", rbac.RequirePerm(PermNSWrite), m.projectQuotaWrite("quota_update"))
	g.POST("/:id/limitranges", rbac.RequirePerm(PermNSWrite), m.projectQuotaWrite("limitrange_create"))
	g.PATCH("/:id/limitranges/:ns/:name", rbac.RequirePerm(PermNSWrite), m.projectQuotaWrite("limitrange_update"))
}

func (m *Module) listProjects(c *gin.Context) {
	if _, err := labels.Parse(c.Query("label_selector")); err != nil {
		httpx.Fail(c, httpx.BadRequest("invalid namespace label_selector"))
		return
	}
	cl, kc, ok := m.resourceKube(c)
	if !ok {
		return
	}
	items, sources, err := readProjectRows(c.Request.Context(), kc, c.Query("label_selector"), time.Now())
	if err != nil {
		httpx.Fail(c, resourceAPIError(err))
		return
	}
	truncated, complete := false, true
	for _, source := range sources {
		truncated = truncated || source.Truncated
		complete = complete && source.Complete
	}
	state := resourceListState(truncated)
	if !complete && !truncated {
		state = "partial"
	}
	httpx.OK(c, gin.H{"cluster_id": cl.ID, "items": items, "total": len(items), "sources": sources, "state": state, "complete": complete, "truncated": truncated, "limit": resourceListCap})
}

func readProjectRows(ctx context.Context, kc *kubeClients, selector string, now time.Time) ([]projectRow, map[string]projectResourceRead, error) {
	if kc == nil || kc.cs == nil {
		return nil, nil, resourceUnavailable()
	}
	if _, err := labels.Parse(selector); err != nil {
		return nil, nil, httpx.BadRequest("invalid namespace label_selector")
	}
	ctx, cancel := context.WithTimeout(ctx, resourceReadTimeout)
	defer cancel()
	namespaces, nsTruncated, err := readResourcePages(ctx, metav1.ListOptions{LabelSelector: selector}, func(ctx context.Context, opts metav1.ListOptions) ([]corev1.Namespace, string, error) {
		list, err := kc.cs.CoreV1().Namespaces().List(ctx, opts)
		if err != nil {
			return nil, "", err
		}
		if list == nil {
			return nil, "", errors.New("nil namespace list")
		}
		return list.Items, list.Continue, nil
	})
	if err != nil {
		return nil, nil, err
	}
	quotas, quotaTruncated, quotaErr := readProjectQuotas(ctx, kc, "")
	limits, limitTruncated, limitErr := readProjectLimits(ctx, kc, "")
	sources := map[string]projectResourceRead{
		"namespaces":      projectReadStatus(nsTruncated, nil),
		"resource_quotas": projectReadStatus(quotaTruncated, quotaErr),
		"limit_ranges":    projectReadStatus(limitTruncated, limitErr),
	}
	quotaByNS := make(map[string][]projectQuotaRow)
	for _, quota := range quotas {
		quotaByNS[quota.Namespace] = append(quotaByNS[quota.Namespace], projectQuotaView(&quota))
	}
	limitByNS := make(map[string][]projectLimitRow)
	for _, limit := range limits {
		limitByNS[limit.Namespace] = append(limitByNS[limit.Namespace], projectLimitView(&limit))
	}
	items := make([]projectRow, 0, len(namespaces))
	for _, ns := range namespaces {
		row := projectRow{
			Name: ns.Name, Phase: ns.Status.Phase, Labels: ns.Labels,
			CreatedAt: ns.CreationTimestamp.Time, UID: string(ns.UID), ResourceVersion: ns.ResourceVersion,
			ResourceQuotas: make([]projectQuotaRow, 0), LimitRanges: make([]projectLimitRow, 0),
			QuotaRead: sources["resource_quotas"], LimitRead: sources["limit_ranges"],
			Truncated: quotaTruncated || limitTruncated,
		}
		if row.Labels == nil {
			row.Labels = map[string]string{}
		}
		// Missing creation times remain unknown rather than becoming a synthetic age.
		if !ns.CreationTimestamp.IsZero() {
			row.Age = duration.HumanDuration(now.Sub(ns.CreationTimestamp.Time))
		}
		if values, ok := quotaByNS[ns.Name]; ok {
			row.ResourceQuotas = values
		}
		if values, ok := limitByNS[ns.Name]; ok {
			row.LimitRanges = values
		}
		sort.Slice(row.ResourceQuotas, func(i, j int) bool { return row.ResourceQuotas[i].Name < row.ResourceQuotas[j].Name })
		sort.Slice(row.LimitRanges, func(i, j int) bool { return row.LimitRanges[i].Name < row.LimitRanges[j].Name })
		items = append(items, row)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, sources, nil
}

func readProjectQuotas(ctx context.Context, kc *kubeClients, namespace string) ([]corev1.ResourceQuota, bool, error) {
	if kc == nil || kc.cs == nil {
		return nil, false, resourceUnavailable()
	}
	return readResourcePages(ctx, metav1.ListOptions{}, func(ctx context.Context, opts metav1.ListOptions) ([]corev1.ResourceQuota, string, error) {
		list, err := kc.cs.CoreV1().ResourceQuotas(namespace).List(ctx, opts)
		if err != nil {
			return nil, "", err
		}
		if list == nil {
			return nil, "", errors.New("nil quota list")
		}
		return list.Items, list.Continue, nil
	})
}

func readProjectLimits(ctx context.Context, kc *kubeClients, namespace string) ([]corev1.LimitRange, bool, error) {
	if kc == nil || kc.cs == nil {
		return nil, false, resourceUnavailable()
	}
	return readResourcePages(ctx, metav1.ListOptions{}, func(ctx context.Context, opts metav1.ListOptions) ([]corev1.LimitRange, string, error) {
		list, err := kc.cs.CoreV1().LimitRanges(namespace).List(ctx, opts)
		if err != nil {
			return nil, "", err
		}
		if list == nil {
			return nil, "", errors.New("nil LimitRange list")
		}
		return list.Items, list.Continue, nil
	})
}

func (m *Module) listProjectQuotas(c *gin.Context) {
	m.listProjectResource(c, false)
}

func (m *Module) listProjectLimits(c *gin.Context) {
	m.listProjectResource(c, true)
}

func (m *Module) listProjectResource(c *gin.Context, limits bool) {
	namespace := c.Param("namespace")
	if len(validation.IsDNS1123Label(namespace)) != 0 {
		httpx.Fail(c, httpx.BadRequest("invalid namespace"))
		return
	}
	cl, kc, ok := m.resourceKube(c)
	if !ok {
		return
	}
	if kc.cs == nil {
		httpx.Fail(c, resourceUnavailable())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), resourceReadTimeout)
	defer cancel()
	// Kubernetes lists in a nonexistent namespace can return an empty list.
	// Check the namespace first so absence cannot be presented as unconfigured.
	if _, err := kc.cs.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{}); err != nil {
		httpx.Fail(c, resourceAPIError(err))
		return
	}
	var items any
	var truncated bool
	var err error
	if limits {
		var rows []corev1.LimitRange
		rows, truncated, err = readProjectLimits(ctx, kc, namespace)
		projected := []projectLimitRow{}
		for _, row := range rows {
			projected = append(projected, projectLimitView(&row))
		}
		sort.Slice(projected, func(i, j int) bool { return projected[i].Name < projected[j].Name })
		items = projected
	} else {
		var rows []corev1.ResourceQuota
		rows, truncated, err = readProjectQuotas(ctx, kc, namespace)
		projected := []projectQuotaRow{}
		for _, row := range rows {
			projected = append(projected, projectQuotaView(&row))
		}
		sort.Slice(projected, func(i, j int) bool { return projected[i].Name < projected[j].Name })
		items = projected
	}
	if err != nil {
		httpx.Fail(c, resourceAPIError(err))
		return
	}
	httpx.OK(c, gin.H{"cluster_id": cl.ID, "namespace": namespace, "items": items, "state": resourceListState(truncated), "complete": !truncated, "truncated": truncated, "limit": resourceListCap})
}

func projectQuotaView(quota *corev1.ResourceQuota) projectQuotaRow {
	scopes := append([]corev1.ResourceQuotaScope{}, quota.Spec.Scopes...)
	return projectQuotaRow{
		Namespace: quota.Namespace, Name: quota.Name, UID: string(quota.UID), ResourceVersion: quota.ResourceVersion,
		Hard: resList(quota.Spec.Hard), Used: resList(quota.Status.Used), StatusHard: resList(quota.Status.Hard),
		Scopes: scopes, ScopeSelector: quota.Spec.ScopeSelector.DeepCopy(),
	}
}

func projectLimitView(limit *corev1.LimitRange) projectLimitRow {
	items := append([]corev1.LimitRangeItem{}, limit.Spec.Limits...)
	return projectLimitRow{
		Namespace: limit.Namespace, Name: limit.Name, UID: string(limit.UID), ResourceVersion: limit.ResourceVersion,
		Limits: items,
	}
}
