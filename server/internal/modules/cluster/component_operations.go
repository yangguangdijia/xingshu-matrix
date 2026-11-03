package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"helm.sh/helm/v3/pkg/chart"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const componentApprovalType = "cluster.component.install"
const componentWritePermission = "workload:write"

var componentKeyRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
var componentValueRE = regexp.MustCompile(`^[a-zA-Z0-9._/-]{1,128}$`)
var componentInstallSlots = make(chan struct{}, 4)

type componentInstallRequest struct {
	Component   string            `json:"-"`
	Environment string            `json:"environment"`
	Namespace   string            `json:"namespace"`
	ReleaseName string            `json:"release_name"`
	Version     string            `json:"version"`
	ChartDigest string            `json:"chart_digest,omitempty"`
	Values      map[string]string `json:"values"`
	Confirm     bool              `json:"confirm"`
	Reason      string            `json:"reason"`
}

type componentOperation struct {
	ID          string    `json:"operation_id"`
	ClusterID   int64     `json:"cluster_id"`
	ClusterName string    `json:"cluster_name"`
	Environment string    `json:"environment"`
	Component   string    `json:"component"`
	Namespace   string    `json:"namespace"`
	ReleaseName string    `json:"release_name"`
	Version     string    `json:"version"`
	Status      string    `json:"status"`
	HelmStatus  string    `json:"helm_status,omitempty"`
	ApprovalID  *int64    `json:"approval_id,omitempty"`
	Error       string    `json:"error,omitempty"`
	LogExcerpt  string    `json:"log_excerpt,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type componentTicketPayload struct {
	OperationID string            `json:"operation_id"`
	ClusterID   int64             `json:"cluster_id"`
	Environment string            `json:"environment"`
	Component   string            `json:"component"`
	Namespace   string            `json:"namespace"`
	ReleaseName string            `json:"release_name"`
	Version     string            `json:"version"`
	ChartDigest string            `json:"chart_digest"`
	Values      map[string]string `json:"values"`
}

type componentOperationSnapshot struct {
	ClusterID          int64                   `json:"cluster_id"`
	ClusterName        string                  `json:"cluster_name"`
	Environment        string                  `json:"environment"`
	ClusterFingerprint string                  `json:"cluster_fingerprint"`
	Component          string                  `json:"component"`
	ChartDigest        string                  `json:"chart_digest"`
	Request            componentInstallRequest `json:"request"`
}

func (m *Module) RegisterComponentApprovals() {
	if m.d != nil && m.d.Approval != nil {
		m.d.Approval.MustRegister(approval.TypeSpec{
			Type: componentApprovalType, Module: "cluster",
			Description: "受限 Helm 组件安装", Timeout: 20 * time.Minute,
			Handler: componentApprovalHandler{module: m},
			SanitizePayload: func(raw json.RawMessage) json.RawMessage {
				var p componentTicketPayload
				if json.Unmarshal(raw, &p) != nil {
					return json.RawMessage(`{"payload_redacted":true}`)
				}
				for key := range p.Values {
					p.Values[key] = "[redacted]"
				}
				b, _ := json.Marshal(p)
				return b
			},
		})
	}
}

type componentApprovalHandler struct{ module *Module }

func (h componentApprovalHandler) OnApproved(ctx context.Context, ticket *approval.Ticket) (any, error) {
	if h.module == nil {
		return nil, errors.New("component approval services unavailable")
	}
	return h.module.onComponentApproved(ctx, ticket)
}

func (h componentApprovalHandler) OnRejected(ctx context.Context, ticket *approval.Ticket) error {
	if h.module == nil || h.module.d == nil || h.module.d.DB == nil || ticket == nil ||
		ticket.ID < 1 || ticket.ApplicantID < 1 || ticket.Type != componentApprovalType {
		return errors.New("component operation store unavailable")
	}
	var payload componentTicketPayload
	if err := ticket.DecodePayload(&payload); err != nil || payload.OperationID == "" ||
		payload.ClusterID < 1 || payload.Environment == "" || payload.Component == "" || ticket.Env != payload.Environment {
		return errors.New("invalid component approval payload")
	}
	_, err := h.module.d.DB.Exec(ctx, `UPDATE cluster_component_operations
		SET status='rejected',approval_id=COALESCE(approval_id,$2),config_encrypted='',error_message='approval rejected',finished_at=now(),updated_at=now()
		WHERE id=$1 AND cluster_id=$3 AND environment=$4 AND component=$5
		AND created_by=$6 AND status IN ('preflight','pending_approval') AND (approval_id IS NULL OR approval_id=$2)`,
		payload.OperationID, ticket.ID, payload.ClusterID, payload.Environment, payload.Component, ticket.ApplicantID)
	if err != nil {
		return errors.New("component operation rejection persistence failed")
	}
	return nil
}

func (m *Module) RegisterComponentRoutes(g *gin.RouterGroup) {
	read := rbac.RequireAnyPerm(PermWorkloadRead, PermConfigRead)
	g.GET("/:id/components", read, m.listComponentStatus)
	g.GET("/:id/components/catalog", read, m.listComponentCatalog)
	g.POST("/:id/components/:component/preflight", rbac.RequirePerm(componentWritePermission), m.preflightComponent)
	g.POST("/:id/components/:component/install", rbac.RequirePerm(componentWritePermission), m.installComponent)
	g.GET("/:id/components/operations/:operation", read, m.getComponentOperation)
}

func (m *Module) listComponentCatalog(c *gin.Context) {
	if _, _, ok := m.componentAccess(c, false); !ok {
		return
	}
	httpx.OK(c, gin.H{"items": sortedComponentCatalog()})
}

func (m *Module) listComponentStatus(c *gin.Context) {
	cl, kc, ok := m.componentAccess(c, false)
	if !ok {
		return
	}
	workloads := listComponentWorkloads(c.Request.Context(), kc)
	items := make([]gin.H, 0)
	complete := workloads.err == nil
	truncated := workloads.truncated
	for _, def := range sortedComponentCatalog() {
		capability := componentCatalogCapability(c.Request.Context(), kc, def.Key, workloads)
		if capability.Truncated || capability.State == "error" || capability.State == "forbidden" {
			complete = false
		}
		truncated = truncated || capability.Truncated
		items = append(items, gin.H{"key": def.Key, "supported": def.Supported,
			"state": capability.State, "items": capability.Items, "error": capability.Error,
			"truncated": capability.Truncated})
	}
	httpx.OK(c, gin.H{"cluster_id": cl.ID, "items": items, "complete": complete, "truncated": truncated})
}

func componentCatalogCapability(ctx context.Context, kc *kubeClients, key string, workloads workloadInventory) ClusterCapability {
	if key == "istio-base" {
		err := componentVerifyDependencies(ctx, kc, componentProfile{Dependencies: []string{"istio-base"}})
		if err == nil {
			return ClusterCapability{Available: true, State: "available", Items: []CapabilityItem{}}
		}
		if apierrors.IsNotFound(err) {
			return emptyCapability("missing", "required Istio CRDs are not installed")
		}
		if apierrors.IsForbidden(err) {
			return emptyCapability("forbidden", "required Istio CRDs or APIs are not readable")
		}
		return emptyCapability("error", "Istio CRD discovery failed or CRDs are not established")
	}
	probe := componentProbeForCatalogKey(key)
	if key == "traefik" {
		probe = componentProbe{markers: []string{"traefik"}}
	}
	return componentCapabilityContext(ctx, kc, key, probe, workloads)
}

func (m *Module) componentAccess(c *gin.Context, write bool) (*Cluster, *kubeClients, bool) {
	cl, ok := m.componentScopeAccess(c, write)
	if !ok {
		return nil, nil, false
	}
	kc, err := m.kube(c.Request.Context(), cl)
	if err != nil {
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "cluster client unavailable"))
		return nil, nil, false
	}
	return cl, kc, true
}

func (m *Module) componentScopeAccess(c *gin.Context, write bool) (*Cluster, bool) {
	if m.d == nil || m.d.DB == nil || m.d.Crypto == nil {
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "cluster services unavailable"))
		return nil, false
	}
	cl, ok := m.clusterFor(c)
	if !ok {
		return nil, false
	}
	p := rbac.PrincipalFrom(c)
	if p == nil || strings.TrimSpace(cl.Env) == "" || !p.CanEnv(cl.Env) {
		httpx.Fail(c, httpx.Forbidden("cluster environment access is required"))
		return nil, false
	}
	if write && !componentPrincipalCanInstall(p) {
		httpx.Fail(c, httpx.Forbidden("component installation permission is required"))
		return nil, false
	}
	return cl, true
}

func decodeComponentRequest(c *gin.Context) (componentInstallRequest, error) {
	var in componentInstallRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		return in, httpx.BadRequest("invalid component request")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return in, httpx.BadRequest("invalid component request")
	}
	queryReason := strings.TrimSpace(c.Query("reason"))
	if queryReason != "" && in.Reason != "" && strings.TrimSpace(in.Reason) != queryReason {
		return in, httpx.BadRequest("conflicting component change reasons")
	}
	if queryReason != "" {
		in.Reason = queryReason
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if len(validation.IsDNS1123Label(in.Namespace)) != 0 ||
		len(validation.IsDNS1123Label(in.ReleaseName)) != 0 ||
		len(in.ReleaseName) > 53 || in.Reason == "" || utf8.RuneCountInString(in.Reason) > 500 {
		return in, httpx.BadRequest("invalid namespace, release name, or reason")
	}
	return in, nil
}

func validateComponentRequest(key string, in componentInstallRequest) (ComponentDefinition, map[string]any, error) {
	def, ok := componentDefinition(key)
	if !ok || !componentKeyRE.MatchString(key) {
		return def, nil, httpx.NotFound("component is not in the curated catalog")
	}
	if !def.Supported || !def.Installable {
		return def, nil, httpx.Conflict("component is not supported by the current executor")
	}
	if len(validation.IsDNS1123Label(in.Namespace)) != 0 ||
		len(validation.IsDNS1123Label(in.ReleaseName)) != 0 || len(in.ReleaseName) > 53 {
		return def, nil, httpx.BadRequest("invalid namespace or release name")
	}
	if in.Environment != "" && (strings.TrimSpace(in.Environment) != in.Environment || len(in.Environment) > 64) {
		return def, nil, httpx.BadRequest("invalid environment")
	}
	if !contains(def.Versions, in.Version) {
		return def, nil, httpx.BadRequest("chart version is not allowed")
	}
	if len(in.Values) > 16 {
		return def, nil, httpx.BadRequest("too many chart values")
	}
	values := componentDefaultValues(key)
	profile, _ := componentProfileFor(key)
	input := make(map[string]string, len(profile.Fields))
	for _, field := range profile.Fields {
		if field.Default != "" {
			input[field.Key] = field.Default
		}
	}
	for path, value := range in.Values {
		input[path] = value
	}
	for path, value := range input {
		if !stringInList(def.Fields, path) {
			return def, nil, httpx.BadRequest("chart value path is not allowed")
		}
		field := componentFieldByKey(profile.Fields, path)
		switch field.Type {
		case "integer":
			replicas, err := strconv.Atoi(value)
			if err != nil || replicas < 1 || replicas > 10 {
				return def, nil, httpx.BadRequest("replica count must be between 1 and 10")
			}
			if err := setComponentValue(values, strings.Split(path, "."), replicas); err != nil {
				return def, nil, httpx.BadRequest("chart value path is invalid")
			}
		case "port":
			port, err := strconv.Atoi(value)
			if err != nil || port < 1024 || port > 65535 {
				return def, nil, httpx.BadRequest("port must be between 1024 and 65535")
			}
			if err := setComponentValue(values, strings.Split(path, "."), port); err != nil {
				return def, nil, httpx.BadRequest("chart value path is invalid")
			}
		case "enum":
			if !stringInList(field.Options, value) {
				return def, nil, httpx.BadRequest("service type is invalid")
			}
			if err := setComponentValue(values, strings.Split(path, "."), value); err != nil {
				return def, nil, httpx.BadRequest("chart value path is invalid")
			}
		case "boolean":
			if value != "true" && value != "false" {
				return def, nil, httpx.BadRequest("boolean chart value is invalid")
			}
			if err := setComponentValue(values, strings.Split(path, "."), value == "true"); err != nil {
				return def, nil, httpx.BadRequest("chart value path is invalid")
			}
		case "quantity":
			quantity, err := resource.ParseQuantity(value)
			if err != nil || quantity.Sign() <= 0 || len(value) > 32 {
				return def, nil, httpx.BadRequest("resource quantity must be positive")
			}
			if err := setComponentValue(values, strings.Split(path, "."), value); err != nil {
				return def, nil, httpx.BadRequest("chart value path is invalid")
			}
		case "storageClass":
			if len(validation.IsDNS1123Subdomain(value)) != 0 {
				return def, nil, httpx.BadRequest("storage class name is invalid")
			}
			if err := setComponentValue(values, strings.Split(path, "."), value); err != nil {
				return def, nil, httpx.BadRequest("chart value path is invalid")
			}
		default:
			if !componentValueRE.MatchString(value) {
				return def, nil, httpx.BadRequest("chart value is invalid")
			}
			if err := setComponentValue(values, strings.Split(path, "."), value); err != nil {
				return def, nil, httpx.BadRequest("chart value path is invalid")
			}
		}
	}
	return def, values, nil
}

func componentFieldByKey(fields []ComponentField, key string) ComponentField {
	for _, field := range fields {
		if field.Key == key {
			return field
		}
	}
	return ComponentField{Type: "string"}
}

func stringInList(values []string, v string) bool {
	for _, candidate := range values {
		if candidate == v {
			return true
		}
	}
	return false
}

func contains(values []string, v string) bool { return stringInList(values, v) }

func setComponentValue(root map[string]any, path []string, value any) error {
	if len(path) == 0 {
		return errors.New("empty value path")
	}
	if len(path) == 1 {
		root[path[0]] = value
		return nil
	}
	child, ok := root[path[0]].(map[string]any)
	if !ok {
		child = map[string]any{}
		root[path[0]] = child
	}
	return setComponentValue(child, path[1:], value)
}

func (m *Module) preflightComponent(c *gin.Context) {
	cl, ok := m.componentScopeAccess(c, true)
	if !ok {
		return
	}
	in, err := decodeComponentRequest(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if in.Environment != "" && in.Environment != cl.Env {
		httpx.Fail(c, httpx.Conflict("submitted environment does not match cluster environment"))
		return
	}
	def, vals, err := validateComponentRequest(c.Param("component"), in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	kc, err := m.kube(c.Request.Context(), cl)
	if err != nil {
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "cluster client unavailable"))
		return
	}
	chartData, digest, err := fetchVerifiedComponentChart(c.Request.Context(), def, "")
	if err != nil {
		httpx.Fail(c, httpx.NewError(http.StatusBadGateway, httpx.CodeUpstream, "curated chart fetch or version validation failed"))
		return
	}
	in.Environment = cl.Env
	in.Component = def.Key
	if err := prepareVerifiedComponentChart(c.Request.Context(), kc, chartData, vals, in); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"ready": true, "component": def.Key, "chart": chartData.Metadata.Name, "version": chartData.Metadata.Version, "chart_digest": digest, "namespace": in.Namespace, "release_name": in.ReleaseName, "values": vals})
}

func (m *Module) installComponent(c *gin.Context) {
	if m.d == nil || m.d.DB == nil || m.d.Crypto == nil || m.d.Approval == nil || m.d.RBAC == nil {
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "component installation services unavailable"))
		return
	}
	cl, ok := m.componentScopeAccess(c, true)
	if !ok {
		return
	}
	in, err := decodeComponentRequest(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if !in.Confirm {
		httpx.Fail(c, httpx.BadRequest("explicit confirmation is required"))
		return
	}
	if len(in.ChartDigest) != 64 {
		httpx.Fail(c, httpx.BadRequest("the reviewed preflight chart digest is required"))
		return
	}
	if in.Environment != "" && in.Environment != cl.Env {
		httpx.Fail(c, httpx.Conflict("submitted environment does not match cluster environment"))
		return
	}
	def, vals, err := validateComponentRequest(c.Param("component"), in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	kc, err := m.kube(c.Request.Context(), cl)
	if err != nil {
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "cluster client unavailable"))
		return
	}
	in.Environment = cl.Env
	in.Component = def.Key
	chartData, digest, err := fetchVerifiedComponentChart(c.Request.Context(), def, in.ChartDigest)
	if err != nil {
		httpx.Fail(c, httpx.NewError(http.StatusBadGateway, httpx.CodeUpstream, "curated chart fetch or version validation failed"))
		return
	}
	if err = prepareVerifiedComponentChart(c.Request.Context(), kc, chartData, vals, in); err != nil {
		httpx.Fail(c, err)
		return
	}
	p := rbac.PrincipalFrom(c)
	id := uuid.NewString()
	raw, err := json.Marshal(componentOperationSnapshot{
		ClusterID: cl.ID, ClusterName: cl.Name, Environment: cl.Env, ClusterFingerprint: componentClusterFingerprint(cl), Component: def.Key,
		ChartDigest: digest, Request: in,
	})
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	encrypted, err := m.d.Crypto.EncryptBytes(raw, []byte("cluster-component:"+id))
	if err != nil {
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "component configuration encryption unavailable"))
		return
	}
	_, err = m.d.DB.Exec(c.Request.Context(), `INSERT INTO cluster_component_operations
		(id,cluster_id,cluster_name,environment,component,namespace,release_name,chart_version,chart_digest,config_encrypted,status,created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'preflight',$11)`,
		id, cl.ID, cl.Name, cl.Env, def.Key, in.Namespace, in.ReleaseName, in.Version, digest, encrypted, p.UserID)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	redactedValues := make(map[string]string, len(in.Values))
	for key := range in.Values {
		redactedValues[key] = "[redacted]"
	}
	pl := componentTicketPayload{OperationID: id, ClusterID: cl.ID, Environment: cl.Env,
		Component: def.Key, Namespace: in.Namespace, ReleaseName: in.ReleaseName,
		Version: in.Version, ChartDigest: digest, Values: redactedValues}
	ticket, pending, err := m.d.Approval.Gate(c.Request.Context(), p, approval.CreateInput{Type: componentApprovalType, Title: fmt.Sprintf("[%s] install %s/%s", cl.Name, in.Namespace, in.ReleaseName), Env: cl.Env, Reason: in.Reason, Payload: pl})
	if err != nil {
		m.finishComponentOperation(context.WithoutCancel(c.Request.Context()), id, "failed", "approval request failed", "")
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "approval request failed"))
		return
	}
	if pending {
		tag, err := m.d.DB.Exec(c.Request.Context(), `UPDATE cluster_component_operations
			SET approval_id=$2,status='pending_approval',updated_at=now()
			WHERE id=$1 AND status='preflight' AND approval_id IS NULL`, id, ticket.ID)
		if err != nil {
			httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "component approval linkage unavailable"))
			return
		}
		if tag.RowsAffected() != 1 {
			var status string
			var linkedApprovalID *int64
			if queryErr := m.d.DB.QueryRow(c.Request.Context(), `SELECT status,approval_id FROM cluster_component_operations WHERE id=$1`, id).Scan(&status, &linkedApprovalID); queryErr != nil ||
				linkedApprovalID == nil || *linkedApprovalID != ticket.ID ||
				(status != "pending_approval" && status != "running" && status != "succeeded" && status != "audit_pending" && status != "unknown" && status != "failed" && status != "rejected") {
				httpx.Fail(c, httpx.NewError(http.StatusConflict, httpx.CodeConflict, "component approval could not be linked"))
				return
			}
		}
		c.JSON(http.StatusAccepted, httpx.Envelope{Code: httpx.CodeApprovalRequired, Message: "审批单已创建", Data: gin.H{"approval_required": true, "ticket": ticket, "operation_id": id}, RequestID: httpx.RequestIDFrom(c)})
		return
	}
	op, err := m.startComponentOperation(c.Request.Context(), id, cl, chartData, vals, in, false)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, httpx.Envelope{Code: httpx.CodeUnavailable,
			Message: "component operation requires reconciliation", Data: gin.H{"operation_id": id}})
		return
	}
	audit.SetAction(c, "component.install")
	c.JSON(http.StatusAccepted, httpx.Envelope{Data: op, RequestID: httpx.RequestIDFrom(c)})
}

func (m *Module) onComponentApproved(ctx context.Context, ticket *approval.Ticket) (result any, retErr error) {
	if ticket == nil || m == nil || m.d == nil || m.d.DB == nil || ticket.ID < 1 ||
		ticket.ApplicantID < 1 || ticket.Type != componentApprovalType {
		return nil, errors.New("component approval services unavailable")
	}
	var payload componentTicketPayload
	if err := ticket.DecodePayload(&payload); err != nil {
		return nil, httpx.BadRequest("invalid component approval payload")
	}
	if _, err := uuid.Parse(payload.OperationID); err != nil {
		return nil, httpx.BadRequest("invalid component approval payload")
	}
	scopeVerified := false
	defer func() {
		if retErr != nil {
			approvalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if scopeVerified {
				_, _ = m.d.DB.Exec(approvalCtx, `UPDATE cluster_component_operations
				SET status='failed',config_encrypted='',error_message='approval execution preflight failed',finished_at=now(),updated_at=now()
				WHERE id=$1 AND status='pending_approval' AND (approval_id IS NULL OR approval_id=$2)`,
					payload.OperationID, ticket.ID)
			}
			retErr = errors.New("component approval execution failed")
		}
	}()
	if m.d.RBAC == nil || m.d.Crypto == nil {
		return nil, errors.New("component approval services unavailable")
	}
	var clusterID, applicantID int64
	var approvalID *int64
	var clusterName, env, encrypted, component, namespace, releaseName, version, digest string
	err := m.d.DB.QueryRow(ctx, `SELECT cluster_id,cluster_name,environment,component,namespace,release_name,chart_version,chart_digest,config_encrypted,created_by,approval_id FROM cluster_component_operations WHERE id=$1 AND status IN ('preflight','pending_approval')`, payload.OperationID).
		Scan(&clusterID, &clusterName, &env, &component, &namespace, &releaseName, &version, &digest, &encrypted, &applicantID, &approvalID)
	if err != nil {
		return nil, httpx.NotFound("component operation not found")
	}
	cl, err := m.loadCluster(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	if cl.Name != clusterName || cl.Env != env || ticket.Env != env || ticket.ApplicantID != applicantID ||
		(approvalID != nil && *approvalID != ticket.ID) ||
		payload.ClusterID != clusterID || payload.Environment != env || payload.Component != component ||
		payload.Namespace != namespace || payload.ReleaseName != releaseName || payload.Version != version ||
		payload.ChartDigest != digest {
		return nil, httpx.Conflict("cluster identity or approval scope changed")
	}
	scopeVerified = true
	_, perms, scope, err := m.d.RBAC.UserAccess(ctx, applicantID)
	if err != nil {
		return nil, err
	}
	p := &rbac.Principal{UserID: applicantID, Permissions: perms, EnvScope: scope}
	if !componentPrincipalCanInstall(p) || !p.CanEnv(env) {
		return nil, httpx.Forbidden("component installation permission or environment scope revoked")
	}
	kc, err := m.kube(ctx, cl)
	if err != nil {
		return nil, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "cluster client unavailable")
	}
	raw, err := m.d.Crypto.DecryptBytes(encrypted, []byte("cluster-component:"+payload.OperationID))
	if err != nil {
		return nil, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "component configuration decryption failed")
	}
	var saved componentOperationSnapshot
	if err = json.Unmarshal(raw, &saved); err != nil || saved.ClusterID != clusterID ||
		saved.ClusterName != cl.Name || saved.Environment != cl.Env || saved.Component != component ||
		saved.ClusterFingerprint != componentClusterFingerprint(cl) ||
		saved.ChartDigest != digest || saved.Request.Namespace != namespace ||
		saved.Request.ChartDigest != digest ||
		saved.Request.ReleaseName != releaseName || saved.Request.Version != version ||
		!componentApprovalRedactionsMatch(payload.Values, saved.Request.Values) ||
		saved.Request.Environment != cl.Env {
		return nil, httpx.BadRequest("invalid stored component configuration")
	}
	saved.Request.Component = component
	_, vals, err := validateComponentRequest(component, saved.Request)
	if err != nil {
		return nil, httpx.BadRequest("stored component configuration is invalid")
	}
	def, _ := componentDefinition(component)
	chartData, checkedDigest, err := fetchVerifiedComponentChart(ctx, def, digest)
	if err != nil {
		return nil, httpx.NewError(http.StatusBadGateway, httpx.CodeUpstream, "curated chart fetch failed")
	}
	if checkedDigest != digest {
		return nil, httpx.Conflict("curated chart changed after approval")
	}
	if err := prepareVerifiedComponentChart(ctx, kc, chartData, vals, saved.Request); err != nil {
		return nil, err
	}
	if approvalID == nil {
		tag, err := m.d.DB.Exec(ctx, `UPDATE cluster_component_operations SET approval_id=$2,status='pending_approval',updated_at=now() WHERE id=$1 AND status IN ('preflight','pending_approval') AND approval_id IS NULL`, payload.OperationID, ticket.ID)
		if err != nil || tag.RowsAffected() != 1 {
			var linkedID int64
			if err != nil || m.d.DB.QueryRow(ctx, `SELECT approval_id FROM cluster_component_operations WHERE id=$1`, payload.OperationID).Scan(&linkedID) != nil || linkedID != ticket.ID {
				return nil, httpx.Conflict("component approval is already claimed")
			}
		}
	}
	return m.startComponentOperation(ctx, payload.OperationID, cl, chartData, vals, saved.Request, true)
}

func componentApprovalRedactionsMatch(redacted, submitted map[string]string) bool {
	if len(redacted) != len(submitted) {
		return false
	}
	for key := range submitted {
		if redacted[key] != "[redacted]" {
			return false
		}
	}
	return true
}

func (m *Module) startComponentOperation(ctx context.Context, id string, cl *Cluster, ch *chart.Chart, values map[string]any, in componentInstallRequest, approved bool) (componentOperation, error) {
	op := componentOperation{ID: id, ClusterID: cl.ID, ClusterName: cl.Name, Environment: cl.Env,
		Component: in.Component, Namespace: in.Namespace, ReleaseName: in.ReleaseName, Version: in.Version, Status: "running"}
	select {
	case componentInstallSlots <- struct{}{}:
	default:
		if err := m.finishComponentOperation(ctx, id, "failed", "component installer is at capacity; no installation started", ""); err != nil {
			return op, err
		}
		op.Status = "failed"
		return op, nil
	}
	claimed, err := m.claimComponentOperation(ctx, id, cl, approved)
	if err != nil {
		<-componentInstallSlots
		return componentOperation{}, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "component operation claim failed")
	}
	if !claimed {
		<-componentInstallSlots
		return componentOperation{}, httpx.Conflict("component operation has already been claimed")
	}
	// The durable claim precedes dispatch. Request/proxy cancellation cannot
	// abandon a Helm mutation, and process loss is reconciled as unknown.
	runComponentBackground(ctx, func(workerCtx context.Context) {
		defer func() {
			<-componentInstallSlots
			if recover() != nil {
				finishCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = m.finishComponentOperation(finishCtx, id, "unknown", "component worker interrupted; inspect Helm release before retrying", "")
			}
		}()
		_, _ = m.executeClaimedComponentOperation(workerCtx, id, cl, ch, values, in)
	})
	return op, nil
}

func runComponentBackground(ctx context.Context, work func(context.Context)) {
	go func() {
		workerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 12*time.Minute)
		defer cancel()
		work(workerCtx)
	}()
}

func (m *Module) executeClaimedComponentOperation(ctx context.Context, id string, cl *Cluster, ch *chart.Chart, values map[string]any, in componentInstallRequest) (componentOperation, error) {
	component := in.Component
	if _, _, err := validateComponentRequest(component, in); err != nil {
		_ = m.finishComponentOperation(context.WithoutCancel(ctx), id, "failed", "stored component configuration failed validation", "")
		return componentOperation{}, httpx.BadRequest("stored component configuration is invalid")
	}
	kc, err := m.kube(ctx, cl)
	if err != nil {
		_ = m.finishComponentOperation(context.WithoutCancel(ctx), id, "failed", "cluster client unavailable before installation", "")
		return componentOperation{}, resourceUnavailable()
	}
	helmStatus, err := runVerifiedComponentInstall(ctx, kc, ch, values, in)
	if err != nil {
		msg := safeComponentError(err)
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = m.d.DB.Exec(finishCtx, `UPDATE cluster_component_operations SET status=$2,helm_status=$3,error_message=$4,config_encrypted='',finished_at=now(),updated_at=now() WHERE id=$1 AND status='running'`, id, componentFailureStatus(helmStatus), helmStatus, msg)
		return componentOperation{}, httpx.NewError(http.StatusBadGateway, httpx.CodeUpstream, msg)
	}
	if err := m.finalizeComponentSuccess(ctx, id, cl, component, in.Namespace, in.ReleaseName, helmStatus); err != nil {
		_ = m.finishComponentOperation(context.WithoutCancel(ctx), id, "audit_pending", "Helm completed; audit persistence requires reconciliation", "")
		return componentOperation{}, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "audit persistence failed; operation outcome requires reconciliation")
	}
	loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	op, err := m.loadComponentOperation(loadCtx, id)
	if err != nil {
		return componentOperation{}, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "component operation result unavailable")
	}
	return op, nil
}

func (m *Module) claimComponentOperation(ctx context.Context, id string, cl *Cluster, approved bool) (bool, error) {
	if m == nil || m.d == nil || m.d.DB == nil || cl == nil {
		return false, errors.New("component operation store unavailable")
	}
	expectedStatus := "preflight"
	if approved {
		expectedStatus = "pending_approval"
	}
	tag, err := m.d.DB.Exec(ctx, `UPDATE cluster_component_operations SET status='running',started_at=now(),updated_at=now() WHERE id=$1 AND cluster_id=$2 AND environment=$3 AND status=$4`, id, cl.ID, cl.Env, expectedStatus)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func componentPrincipalCanInstall(p *rbac.Principal) bool {
	return p != nil && p.Can(componentWritePermission) && p.Can(PermRBACWrite) && p.Can(PermConfigWrite)
}

func componentFailureStatus(helmStatus string) string {
	if helmStatus == "failed" {
		return "failed"
	}
	return "unknown"
}

type kubeRESTClientGetter struct {
	kc        *kubeClients
	namespace string
}

func (g kubeRESTClientGetter) ToRESTConfig() (*rest.Config, error) {
	return rest.CopyConfig(g.kc.rest), nil
}
func (g kubeRESTClientGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	return memory.NewMemCacheClient(g.kc.cs.Discovery()), nil
}
func (g kubeRESTClientGetter) ToRESTMapper() (meta.RESTMapper, error) { return g.kc.mapper, nil }
func (g kubeRESTClientGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	base := clientcmd.NewNonInteractiveClientConfig(clientcmdapi.Config{}, "", &clientcmd.ConfigOverrides{}, nil)
	return componentClientConfig{base: base, rest: g.kc.rest, namespace: g.namespace}
}

type componentClientConfig struct {
	base      clientcmd.ClientConfig
	rest      *rest.Config
	namespace string
}

func (c componentClientConfig) ClientConfig() (*rest.Config, error) {
	return rest.CopyConfig(c.rest), nil
}
func (c componentClientConfig) Namespace() (string, bool, error)        { return c.namespace, true, nil }
func (c componentClientConfig) RawConfig() (clientcmdapi.Config, error) { return c.base.RawConfig() }
func (c componentClientConfig) ConfigAccess() clientcmd.ConfigAccess    { return c.base.ConfigAccess() }

func (m *Module) finishComponentOperation(ctx context.Context, id, status, message, logs string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if len(message) > 256 {
		message = message[:256]
	}
	if len(logs) > 4096 {
		logs = logs[:4096]
	}
	query := `UPDATE cluster_component_operations SET status=$2,error_message=$3,log_excerpt=$4,config_encrypted=CASE WHEN $2 IN ('failed','rejected','audit_pending','unknown') THEN '' ELSE config_encrypted END,finished_at=CASE WHEN $2 IN ('succeeded','failed','rejected','unknown') THEN now() ELSE finished_at END,updated_at=now() WHERE id=$1`
	args := []any{id, status, message, logs}
	if status == "audit_pending" {
		query = `UPDATE cluster_component_operations SET status='audit_pending',helm_status='deployed',error_message=$2,config_encrypted='',finished_at=now(),updated_at=now() WHERE id=$1 AND status='running'`
		args = []any{id, message}
	}
	_, err := m.d.DB.Exec(ctx, query, args...)
	return err
}
func (m *Module) finalizeComponentSuccess(ctx context.Context, id string, cl *Cluster, component, ns, release, helmStatus string) error {
	if m.d.DB == nil || helmStatus != "deployed" {
		return errors.New("Helm outcome is not confirmed")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	tx, err := m.d.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var userID int64
	if err := tx.QueryRow(ctx, `SELECT created_by FROM cluster_component_operations WHERE id=$1 AND cluster_id=$2 AND environment=$3 AND status='running' FOR UPDATE`, id, cl.ID, cl.Env).Scan(&userID); err != nil {
		return err
	}
	detail := string(componentJSON(map[string]string{"operation_id": id, "namespace": ns, "release": release}))
	_, err = tx.Exec(ctx, `INSERT INTO audit_logs(user_id,username,ip,method,path,resource,resource_id,action,status_code,result,duration_ms,request_id,user_agent,detail,created_at)
		VALUES ($1,'component-installer','', 'POST',$2,'cluster',$3,'component.install',200,'success',0,'','',$4::jsonb,now())`,
		userID, "/api/v1/clusters/"+fmt.Sprint(cl.ID)+"/components/"+component+"/install", fmt.Sprint(cl.ID), detail)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE cluster_component_operations SET status='succeeded',helm_status=$2,config_encrypted='',error_message='',finished_at=now(),updated_at=now() WHERE id=$1 AND status='running'`, id, helmStatus); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func componentJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func safeComponentError(error) string {
	return "Helm installation failed; inspect the sanitized operation status"
}

func (m *Module) loadComponentOperation(ctx context.Context, id string) (componentOperation, error) {
	var o componentOperation
	err := m.d.DB.QueryRow(ctx, `SELECT id::text,cluster_id,cluster_name,environment,component,namespace,release_name,chart_version,status,helm_status,approval_id,error_message,log_excerpt,created_at,updated_at FROM cluster_component_operations WHERE id=$1`, id).
		Scan(&o.ID, &o.ClusterID, &o.ClusterName, &o.Environment, &o.Component, &o.Namespace, &o.ReleaseName, &o.Version, &o.Status, &o.HelmStatus, &o.ApprovalID, &o.Error, &o.LogExcerpt, &o.CreatedAt, &o.UpdatedAt)
	return o, err
}
func (m *Module) getComponentOperation(c *gin.Context) {
	cl, ok := m.componentScopeAccess(c, false)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("operation"))
	if err != nil {
		httpx.Fail(c, httpx.BadRequest("invalid operation id"))
		return
	}
	o, err := m.loadComponentOperation(c.Request.Context(), id.String())
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Fail(c, httpx.NotFound("component operation not found"))
		return
	}
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	if o.ClusterID != cl.ID || o.Environment != cl.Env {
		httpx.Fail(c, httpx.NotFound("component operation not found"))
		return
	}
	p := rbac.PrincipalFrom(c)
	if p == nil || !p.Can("workload:read") && !p.Can(PermConfigRead) {
		httpx.Fail(c, httpx.Forbidden("component operation read permission is required"))
		return
	}
	if o.Status == "preflight" {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		_, staleErr := m.d.DB.Exec(ctx, `UPDATE cluster_component_operations
			SET status=CASE WHEN approval_id IS NULL THEN 'failed' ELSE 'unknown' END,
			error_message=CASE WHEN approval_id IS NULL THEN 'operation setup interrupted' ELSE 'approval execution interrupted; inspect ticket before retry' END,
			config_encrypted='',finished_at=now(),updated_at=now()
			WHERE id=$1 AND cluster_id=$2 AND environment=$3 AND status='preflight'
			AND created_at < now()-interval '20 minutes'`, o.ID, cl.ID, cl.Env)
		cancel()
		if staleErr != nil {
			httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "component operation status unavailable"))
			return
		}
		o, err = m.loadComponentOperation(c.Request.Context(), id.String())
		if err != nil {
			httpx.Fail(c, httpx.Internal(err))
			return
		}
	}
	if o.Status == "pending_approval" {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		_, staleErr := m.d.DB.Exec(ctx, `UPDATE cluster_component_operations
			SET status='failed',error_message='approval linkage interrupted',config_encrypted='',finished_at=now(),updated_at=now()
			WHERE id=$1 AND cluster_id=$2 AND environment=$3 AND status='pending_approval'
			AND approval_id IS NULL AND created_at < now()-interval '20 minutes'`, o.ID, cl.ID, cl.Env)
		cancel()
		if staleErr != nil {
			httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "component operation status unavailable"))
			return
		}
		ctx, cancel = context.WithTimeout(c.Request.Context(), 5*time.Second)
		_, staleErr = m.d.DB.Exec(ctx, `UPDATE cluster_component_operations o
			SET status=CASE WHEN a.status IN ('rejected','cancelled') THEN 'rejected' ELSE 'failed' END,
			error_message='approval did not execute',config_encrypted='',finished_at=now(),updated_at=now()
			FROM approval_tickets a
			WHERE o.id=$1 AND o.cluster_id=$2 AND o.environment=$3 AND o.status='pending_approval'
			AND a.id=o.approval_id AND a.type='cluster.component.install' AND a.env=o.environment
			AND a.applicant_id=o.created_by AND a.status IN ('failed','rejected','cancelled')`,
			o.ID, cl.ID, cl.Env)
		cancel()
		if staleErr != nil {
			httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "component operation status unavailable"))
			return
		}
		o, err = m.loadComponentOperation(c.Request.Context(), id.String())
		if err != nil {
			httpx.Fail(c, httpx.Internal(err))
			return
		}
	}
	if o.Status == "running" {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		_, staleErr := m.d.DB.Exec(ctx, `UPDATE cluster_component_operations
			SET status='unknown',error_message='execution interrupted; inspect Helm release before any retry',finished_at=now(),updated_at=now()
			WHERE id=$1 AND status='running' AND started_at < now()-interval '20 minutes'`, o.ID)
		cancel()
		if staleErr != nil {
			httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "component operation status unavailable"))
			return
		}
		o, err = m.loadComponentOperation(c.Request.Context(), id.String())
		if err != nil {
			httpx.Fail(c, httpx.Internal(err))
			return
		}
	}
	if o.Status == "audit_pending" {
		if err := m.retryComponentAudit(c.Request.Context(), o, cl); err == nil {
			if refreshed, loadErr := m.loadComponentOperation(c.Request.Context(), id.String()); loadErr == nil {
				o = refreshed
			}
		}
	}
	httpx.OK(c, o)
}

func (m *Module) retryComponentAudit(ctx context.Context, o componentOperation, cl *Cluster) error {
	if o.Status != "audit_pending" || cl == nil || o.ClusterID != cl.ID || o.Environment != cl.Env {
		return errors.New("component audit retry scope mismatch")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	tx, err := m.d.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var userID int64
	var helmStatus string
	var component, namespace, releaseName string
	if err := tx.QueryRow(ctx, `SELECT created_by,helm_status,component,namespace,release_name FROM cluster_component_operations WHERE id=$1 AND cluster_id=$2 AND environment=$3 AND status='audit_pending' FOR UPDATE`, o.ID, cl.ID, cl.Env).Scan(&userID, &helmStatus, &component, &namespace, &releaseName); err != nil {
		return err
	}
	if helmStatus != "deployed" {
		return errors.New("component Helm outcome is not confirmed")
	}
	detail := string(componentJSON(map[string]string{"operation_id": o.ID, "namespace": namespace, "release": releaseName}))
	if _, err := tx.Exec(ctx, `INSERT INTO audit_logs(user_id,username,ip,method,path,resource,resource_id,action,status_code,result,duration_ms,request_id,user_agent,detail,created_at)
		VALUES ($1,$2,'', 'POST',$3,'cluster',$4,'component.install',200,'success',0,'','',$5::jsonb,now())`,
		userID, fmt.Sprint(userID), "/api/v1/clusters/"+fmt.Sprint(cl.ID)+"/components/"+component+"/install", fmt.Sprint(cl.ID), detail); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE cluster_component_operations SET status='succeeded',error_message='',config_encrypted='',finished_at=now(),updated_at=now() WHERE id=$1 AND status='audit_pending' AND helm_status='deployed'`, o.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func componentClusterFingerprint(cl *Cluster) string {
	if cl == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(cl.APIServer + "\x00" + cl.encKubeconfig))
	return hex.EncodeToString(sum[:])
}
