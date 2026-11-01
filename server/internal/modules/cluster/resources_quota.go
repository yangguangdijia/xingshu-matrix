package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const projectQuotaApprovalType = "cluster.project-quota"

type projectQuotaParams struct {
	Namespace       string                    `json:"namespace"`
	Name            string                    `json:"name"`
	UID             types.UID                 `json:"uid,omitempty"`
	ResourceVersion string                    `json:"resource_version,omitempty"`
	Spec            *corev1.ResourceQuotaSpec `json:"spec"`
}

type projectLimitParams struct {
	Namespace       string                 `json:"namespace"`
	Name            string                 `json:"name"`
	UID             types.UID              `json:"uid,omitempty"`
	ResourceVersion string                 `json:"resource_version,omitempty"`
	Spec            *corev1.LimitRangeSpec `json:"spec"`
}

type projectQuotaMutation struct {
	ClusterID  int64               `json:"cluster_id"`
	Cluster    string              `json:"cluster"`
	Action     string              `json:"action"`
	Quota      *projectQuotaParams `json:"quota,omitempty"`
	LimitRange *projectLimitParams `json:"limit_range,omitempty"`
}

// RegisterProjectQuotaApprovals must be called once after m.d is assigned.
// Missing initialization disables quota writes, including nonproduction writes.
func (m *Module) RegisterProjectQuotaApprovals() error {
	if m == nil || m.d == nil || m.d.Approval == nil {
		return resourceUnavailable()
	}
	return m.d.Approval.Register(approval.TypeSpec{
		Type: projectQuotaApprovalType, Module: "cluster",
		Description:      "Namespace ResourceQuota and LimitRange changes",
		CreatePermission: PermNSWrite, Timeout: time.Minute,
		Handler: approval.HandlerFunc(m.onProjectQuotaApproved),
	})
}

func decodeProjectInput(raw []byte, target any) error {
	if len(raw) == 0 || len(raw) > 256<<10 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return httpx.BadRequest("a structured resource configuration of at most 256 KiB is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		// Decoder errors can include submitted field values.
		return httpx.BadRequest("invalid resource configuration or unknown fields")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return httpx.BadRequest("resource configuration must contain one JSON object")
	}
	return nil
}

func validateProjectInput(input nsParams) error {
	if len(validation.IsDNS1123Label(input.Name)) != 0 {
		return httpx.BadRequest("invalid namespace name")
	}
	if len(input.Labels) > 64 {
		return httpx.BadRequest("too many namespace labels")
	}
	for key, value := range input.Labels {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
			return httpx.BadRequest("invalid namespace label")
		}
	}
	return nil
}

func (m *Module) projectQuotaWrite(action string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p := rbac.PrincipalFrom(c)
		if !p.Can(PermNSWrite) {
			httpx.Fail(c, httpx.Forbidden("namespace write permission is required"))
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256<<10)
		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			httpx.Fail(c, httpx.BadRequest("invalid or oversized resource configuration"))
			return
		}
		var input nsParams
		mutation := projectQuotaMutation{Action: action}
		switch action {
		case "project_create":
			err = decodeProjectInput(raw, &input)
			if err == nil {
				err = validateProjectInput(input)
			}
		case "quota_create", "quota_update":
			mutation.Quota = &projectQuotaParams{}
			err = decodeProjectInput(raw, mutation.Quota)
			if err == nil && action == "quota_update" {
				err = bindProjectRouteIdentity(c, &mutation.Quota.Namespace, &mutation.Quota.Name)
			}
			if err == nil {
				err = validateProjectQuotaMutation(mutation)
			}
		case "limitrange_create", "limitrange_update":
			mutation.LimitRange = &projectLimitParams{}
			err = decodeProjectInput(raw, mutation.LimitRange)
			if err == nil && action == "limitrange_update" {
				err = bindProjectRouteIdentity(c, &mutation.LimitRange.Namespace, &mutation.LimitRange.Name)
			}
			if err == nil {
				err = validateProjectQuotaMutation(mutation)
			}
		default:
			err = httpx.BadRequest("unsupported project change")
		}
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		cl, kc, ok := m.resourceKube(c)
		if !ok {
			return
		}
		mutation.ClusterID, mutation.Cluster = cl.ID, cl.Name
		audit.SetAction(c, action)
		if action == "project_create" {
			audit.SetResource(c, "namespace", input.Name)
			audit.SetDetail(c, gin.H{"name": input.Name})
			if err := preflightProjectNamespace(c.Request.Context(), kc, input); err != nil {
				httpx.Fail(c, err)
				return
			}
			normalized, _ := json.Marshal(input)
			pl := changePayload{ClusterID: cl.ID, Cluster: cl.Name, Action: "ns_create", Name: input.Name, Params: normalized}
			if err := validateChange(&pl); err != nil {
				httpx.Fail(c, err)
				return
			}
			result, ticket, err := m.runChange(c.Request.Context(), p, cl, pl, c.Query("reason"))
			if err != nil {
				httpx.Fail(c, err)
				return
			}
			if ticket != nil {
				approval.RespondPending(c, ticket)
				return
			}
			httpx.OK(c, result)
			return
		}
		name, namespace := projectMutationIdentity(mutation)
		audit.SetResource(c, "namespace-resource", namespace+"/"+name)
		audit.SetDetail(c, gin.H{"namespace": namespace, "name": name, "action": action})
		result, ticket, err := m.runProjectQuotaMutation(c.Request.Context(), p, cl, kc, mutation, c.Query("reason"))
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		if ticket != nil {
			approval.RespondPending(c, ticket)
			return
		}
		httpx.OK(c, result)
	}
}

func preflightProjectNamespace(ctx context.Context, kc *kubeClients, input nsParams) error {
	if err := validateProjectInput(input); err != nil {
		return err
	}
	if kc == nil || kc.cs == nil {
		return resourceUnavailable()
	}
	ctx, cancel := context.WithTimeout(ctx, resourceReadTimeout)
	defer cancel()
	result, err := kc.cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: input.Name, Labels: input.Labels},
	}, metav1.CreateOptions{FieldManager: fieldManager, DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		return resourceAPIError(err)
	}
	if result == nil {
		return resourceUnavailable()
	}
	return nil
}

func bindProjectRouteIdentity(c *gin.Context, namespace, name *string) error {
	if (*namespace != "" && *namespace != c.Param("ns")) || (*name != "" && *name != c.Param("name")) {
		return httpx.BadRequest("resource identity does not match route")
	}
	*namespace, *name = c.Param("ns"), c.Param("name")
	return nil
}

func projectMutationIdentity(mutation projectQuotaMutation) (string, string) {
	if mutation.Quota != nil {
		return mutation.Quota.Name, mutation.Quota.Namespace
	}
	if mutation.LimitRange != nil {
		return mutation.LimitRange.Name, mutation.LimitRange.Namespace
	}
	return "", ""
}

func (m *Module) projectQuotaReady() bool {
	if m == nil || m.d == nil || m.d.Approval == nil {
		return false
	}
	for _, spec := range m.d.Approval.Types() {
		if spec.Type == projectQuotaApprovalType {
			return true
		}
	}
	return false
}

func (m *Module) runProjectQuotaMutation(ctx context.Context, p *rbac.Principal, cl *Cluster, kc *kubeClients, mutation projectQuotaMutation, reason string) (any, *approval.Ticket, error) {
	if !m.projectQuotaReady() {
		return nil, nil, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "project quota approval handler is unavailable; writes are disabled")
	}
	if !p.Can(PermNSWrite) {
		return nil, nil, httpx.Forbidden("namespace write permission and cluster environment access are required")
	}
	if err := resourceClusterAccess(p, cl); err != nil {
		return nil, nil, err
	}
	if mutation.ClusterID != cl.ID || mutation.Cluster != cl.Name {
		return nil, nil, httpx.Conflict("cluster identity changed")
	}
	if err := validateProjectQuotaMutation(mutation); err != nil {
		return nil, nil, err
	}
	// Admission and Kubernetes RBAC must accept exactly this configuration before
	// it can become a ticket. Dry-run failure never falls back to a real write.
	if _, err := executeProjectQuotaMutation(ctx, kc, mutation, true); err != nil {
		return nil, nil, err
	}
	name, namespace := projectMutationIdentity(mutation)
	ticket, pending, err := m.d.Approval.Gate(ctx, p, approval.CreateInput{
		Type: projectQuotaApprovalType, Env: cl.Env, Reason: reason, Payload: mutation,
		Title: fmt.Sprintf("[%s] %s %s/%s", cl.Name, mutation.Action, namespace, name),
	})
	if err != nil {
		return nil, nil, err
	}
	if pending {
		return nil, ticket, nil
	}
	result, err := executeProjectQuotaMutation(ctx, kc, mutation, false)
	return result, nil, err
}

func (m *Module) onProjectQuotaApproved(ctx context.Context, ticket *approval.Ticket) (result any, resultErr error) {
	if ticket == nil || ticket.Type != projectQuotaApprovalType || ticket.Status != approval.StatusApproved ||
		ticket.ID <= 0 || ticket.ApplicantID <= 0 || m == nil || m.d == nil || m.d.Audit == nil {
		return nil, resourceUnavailable()
	}
	var mutation projectQuotaMutation
	started := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			m.auditProjectQuotaExecution(ctx, ticket, mutation, started, resourceUnavailable())
			panic(recovered)
		}
		m.auditProjectQuotaExecution(ctx, ticket, mutation, started, resultErr)
	}()
	if err := decodeProjectInput(ticket.Payload, &mutation); err != nil {
		mutation = projectQuotaMutation{}
		return nil, err
	}
	if err := validateProjectQuotaMutation(mutation); err != nil {
		mutation = projectQuotaMutation{}
		return nil, err
	}
	if m.d.RBAC == nil || m.d.DB == nil || m.d.Crypto == nil || m.clients == nil {
		return nil, resourceUnavailable()
	}
	cl, err := m.loadCluster(ctx, mutation.ClusterID)
	if err != nil {
		return nil, resourceUnavailable()
	}
	if cl.Env != ticket.Env || cl.Name != mutation.Cluster || strings.TrimSpace(cl.Env) == "" {
		return nil, httpx.Conflict("cluster identity or approval environment changed")
	}
	_, permissions, scope, err := m.d.RBAC.UserAccess(ctx, ticket.ApplicantID)
	if err != nil {
		return nil, resourceUnavailable()
	}
	p := &rbac.Principal{UserID: ticket.ApplicantID, Username: ticket.ApplicantName, Permissions: permissions, EnvScope: scope}
	if !p.Can(PermNSWrite) || !p.CanEnv(cl.Env) {
		return nil, httpx.Forbidden("applicant permissions or environment access revoked")
	}
	kc, err := m.kube(ctx, cl)
	if err != nil {
		return nil, resourceUnavailable()
	}
	if _, err := executeProjectQuotaMutation(ctx, kc, mutation, true); err != nil {
		return nil, err
	}
	return executeProjectQuotaMutation(ctx, kc, mutation, false)
}

func (m *Module) auditProjectQuotaExecution(ctx context.Context, ticket *approval.Ticket, mutation projectQuotaMutation, started time.Time, err error) {
	name, namespace := projectMutationIdentity(mutation)
	detail, _ := json.Marshal(gin.H{
		"ticket_id": ticket.ID, "cluster_id": mutation.ClusterID, "env": ticket.Env,
		"namespace": namespace, "name": name, "action": mutation.Action,
	})
	entry := audit.Entry{
		UserID: &ticket.ApplicantID, Username: ticket.ApplicantName, Method: "APPROVAL",
		Resource: "namespace-resource", ResourceID: namespace + "/" + name,
		Action: "project_quota.execute", StatusCode: http.StatusOK, Result: audit.ResultSuccess,
		DurationMs: int(time.Since(started).Milliseconds()), CreatedAt: started, Detail: detail,
	}
	if err != nil {
		entry.StatusCode, entry.Result = http.StatusInternalServerError, audit.ResultFailure
		var platform *httpx.Error
		if errors.As(err, &platform) {
			entry.StatusCode = platform.Status
		}
		// Execution errors and resource specs may contain upstream data; neither
		// belongs in the audit event.
	}
	m.d.Audit.Record(ctx, entry)
}

func validateProjectQuotaMutation(mutation projectQuotaMutation) error {
	name, namespace := projectMutationIdentity(mutation)
	if len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 {
		return httpx.BadRequest("valid resource name and namespace are required")
	}
	update := strings.HasSuffix(mutation.Action, "_update")
	var uid types.UID
	var rv string
	switch mutation.Action {
	case "quota_create", "quota_update":
		if mutation.Quota == nil || mutation.LimitRange != nil || mutation.Quota.Spec == nil {
			return httpx.BadRequest("one ResourceQuota spec is required")
		}
		p := mutation.Quota
		uid, rv = p.UID, p.ResourceVersion
		if err := validateProjectQuotaSpec(*p.Spec); err != nil {
			return err
		}
	case "limitrange_create", "limitrange_update":
		if mutation.LimitRange == nil || mutation.Quota != nil || mutation.LimitRange.Spec == nil {
			return httpx.BadRequest("one LimitRange spec is required")
		}
		p := mutation.LimitRange
		uid, rv = p.UID, p.ResourceVersion
		if err := validateProjectLimitSpec(*p.Spec); err != nil {
			return err
		}
	default:
		return httpx.BadRequest("unsupported project resource action")
	}
	if update && (uid == "" || rv == "") {
		return httpx.BadRequest("updates require uid and resource_version")
	}
	if !update && (uid != "" || rv != "") {
		return httpx.BadRequest("creates must not contain update preconditions")
	}
	if len(uid) > 128 || len(rv) > 128 {
		return httpx.BadRequest("invalid resource identity preconditions")
	}
	return nil
}

func validateProjectResources(resources corev1.ResourceList, required bool) error {
	if (required && len(resources) == 0) || len(resources) > 128 {
		return httpx.BadRequest("resource quantities must contain between 1 and 128 entries")
	}
	for name, quantity := range resources {
		if len(validation.IsQualifiedName(string(name))) != 0 || quantity.Sign() < 0 {
			return httpx.BadRequest("invalid resource name or negative quantity")
		}
	}
	return nil
}

func validProjectQuotaScope(scope corev1.ResourceQuotaScope) bool {
	switch scope {
	case corev1.ResourceQuotaScopeTerminating, corev1.ResourceQuotaScopeNotTerminating,
		corev1.ResourceQuotaScopeBestEffort, corev1.ResourceQuotaScopeNotBestEffort,
		corev1.ResourceQuotaScopePriorityClass, corev1.ResourceQuotaScopeCrossNamespacePodAffinity:
		return true
	}
	return false
}

func validateProjectQuotaSpec(spec corev1.ResourceQuotaSpec) error {
	if err := validateProjectResources(spec.Hard, true); err != nil {
		return err
	}
	seen := map[corev1.ResourceQuotaScope]bool{}
	for _, scope := range spec.Scopes {
		if !validProjectQuotaScope(scope) || seen[scope] {
			return httpx.BadRequest("invalid or duplicate quota scope")
		}
		seen[scope] = true
	}
	if spec.ScopeSelector != nil {
		if len(spec.ScopeSelector.MatchExpressions) == 0 || len(spec.ScopeSelector.MatchExpressions) > 32 {
			return httpx.BadRequest("quota scope_selector must contain 1 to 32 expressions")
		}
		for _, expression := range spec.ScopeSelector.MatchExpressions {
			if !validProjectQuotaScope(expression.ScopeName) || len(expression.Values) > 64 {
				return httpx.BadRequest("invalid quota scope selector")
			}
			switch expression.Operator {
			case corev1.ScopeSelectorOpIn, corev1.ScopeSelectorOpNotIn:
				if len(expression.Values) == 0 {
					return httpx.BadRequest("quota In/NotIn selectors require values")
				}
			case corev1.ScopeSelectorOpExists, corev1.ScopeSelectorOpDoesNotExist:
				if len(expression.Values) != 0 {
					return httpx.BadRequest("quota Exists selectors do not accept values")
				}
			default:
				return httpx.BadRequest("invalid quota scope selector operator")
			}
			for _, value := range expression.Values {
				if len(value) > 253 {
					return httpx.BadRequest("quota scope value is too long")
				}
			}
		}
	}
	return nil
}

func validateProjectLimitSpec(spec corev1.LimitRangeSpec) error {
	if len(spec.Limits) == 0 || len(spec.Limits) > 16 {
		return httpx.BadRequest("LimitRange requires 1 to 16 limit items")
	}
	seen := map[corev1.LimitType]bool{}
	for _, item := range spec.Limits {
		switch item.Type {
		case corev1.LimitTypePod, corev1.LimitTypeContainer, corev1.LimitTypePersistentVolumeClaim:
		default:
			return httpx.BadRequest("unsupported LimitRange type")
		}
		if seen[item.Type] {
			return httpx.BadRequest("duplicate LimitRange type")
		}
		seen[item.Type] = true
		groups := []corev1.ResourceList{item.Min, item.Max, item.Default, item.DefaultRequest, item.MaxLimitRequestRatio}
		count := 0
		for _, resources := range groups {
			count += len(resources)
			if err := validateProjectResources(resources, false); err != nil {
				return err
			}
			if item.Type == corev1.LimitTypePersistentVolumeClaim {
				for name := range resources {
					if name != corev1.ResourceStorage {
						return httpx.BadRequest("PVC limits only support storage")
					}
				}
			}
		}
		if count == 0 {
			return httpx.BadRequest("LimitRange item must configure at least one quantity")
		}
		if item.Type != corev1.LimitTypeContainer && (len(item.Default) != 0 || len(item.DefaultRequest) != 0) {
			return httpx.BadRequest("only Container limits support defaults")
		}
		if item.Type == corev1.LimitTypePersistentVolumeClaim && len(item.MaxLimitRequestRatio) != 0 {
			return httpx.BadRequest("PVC limits do not support maxLimitRequestRatio")
		}
		for _, ratio := range item.MaxLimitRequestRatio {
			if ratio.AsApproximateFloat64() < 1 {
				return httpx.BadRequest("limit/request ratio must be at least 1")
			}
		}
		for name, minimum := range item.Min {
			if maximum, ok := item.Max[name]; ok && minimum.Cmp(maximum) > 0 {
				return httpx.BadRequest("minimum exceeds maximum")
			}
		}
		for _, defaults := range []corev1.ResourceList{item.Default, item.DefaultRequest} {
			for name, value := range defaults {
				if minimum, ok := item.Min[name]; ok && value.Cmp(minimum) < 0 {
					return httpx.BadRequest("default is below minimum")
				}
				if maximum, ok := item.Max[name]; ok && value.Cmp(maximum) > 0 {
					return httpx.BadRequest("default exceeds maximum")
				}
			}
		}
		for name, request := range item.DefaultRequest {
			if limit, ok := item.Default[name]; ok && request.Cmp(limit) > 0 {
				return httpx.BadRequest("default request exceeds default limit")
			}
		}
	}
	return nil
}

// Only private, gated callers use dryRun=false. Exact UID/resourceVersion
// preconditions prevent delayed approvals from overwriting changed resources.
func executeProjectQuotaMutation(ctx context.Context, kc *kubeClients, mutation projectQuotaMutation, dryRun bool) (any, error) {
	if err := validateProjectQuotaMutation(mutation); err != nil {
		return nil, err
	}
	if kc == nil || kc.cs == nil {
		return nil, resourceUnavailable()
	}
	ctx, cancel := context.WithTimeout(ctx, resourceReadTimeout)
	defer cancel()
	createOptions := metav1.CreateOptions{FieldManager: fieldManager}
	updateOptions := metav1.UpdateOptions{FieldManager: fieldManager}
	if dryRun {
		createOptions.DryRun = []string{metav1.DryRunAll}
		updateOptions.DryRun = []string{metav1.DryRunAll}
	}
	name, namespace := projectMutationIdentity(mutation)
	ns, namespaceErr := kc.cs.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if namespaceErr != nil {
		return nil, resourceAPIError(namespaceErr)
	}
	if ns == nil {
		return nil, resourceUnavailable()
	}
	if mutation.Quota != nil {
		p := mutation.Quota
		value := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Spec: *p.Spec.DeepCopy()}
		var result *corev1.ResourceQuota
		var err error
		if mutation.Action == "quota_update" {
			current, getErr := kc.cs.CoreV1().ResourceQuotas(namespace).Get(ctx, name, metav1.GetOptions{})
			if getErr != nil {
				return nil, resourceAPIError(getErr)
			}
			if current == nil {
				return nil, resourceUnavailable()
			}
			if current.UID != p.UID || current.ResourceVersion != p.ResourceVersion {
				return nil, httpx.Conflict("ResourceQuota changed; reload and submit a new approval")
			}
			value = current.DeepCopy()
			value.Spec = *p.Spec.DeepCopy()
			result, err = kc.cs.CoreV1().ResourceQuotas(namespace).Update(ctx, value, updateOptions)
		} else {
			result, err = kc.cs.CoreV1().ResourceQuotas(namespace).Create(ctx, value, createOptions)
		}
		if err != nil {
			return nil, resourceAPIError(err)
		}
		if result == nil {
			return nil, resourceUnavailable()
		}
		return projectQuotaView(result), nil
	}
	p := mutation.LimitRange
	value := &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Spec: *p.Spec.DeepCopy()}
	var result *corev1.LimitRange
	var err error
	if mutation.Action == "limitrange_update" {
		current, getErr := kc.cs.CoreV1().LimitRanges(namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return nil, resourceAPIError(getErr)
		}
		if current == nil {
			return nil, resourceUnavailable()
		}
		if current.UID != p.UID || current.ResourceVersion != p.ResourceVersion {
			return nil, httpx.Conflict("LimitRange changed; reload and submit a new approval")
		}
		value = current.DeepCopy()
		value.Spec = *p.Spec.DeepCopy()
		result, err = kc.cs.CoreV1().LimitRanges(namespace).Update(ctx, value, updateOptions)
	} else {
		result, err = kc.cs.CoreV1().LimitRanges(namespace).Create(ctx, value, createOptions)
	}
	if err != nil {
		return nil, resourceAPIError(err)
	}
	if result == nil {
		return nil, resourceUnavailable()
	}
	return projectLimitView(result), nil
}
