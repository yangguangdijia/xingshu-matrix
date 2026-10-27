// Package cluster manages Kubernetes clusters via client-go: cluster registry
// (encrypted kubeconfig), nodes, namespaces, workloads, pod logs, events and
// server-side apply. Writes on environments whose policy requires approval go
// through approval.Gate with ticket type "cluster.change".
package cluster

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

//go:embed migrations/*.sql
var migFS embed.FS

const changeType = "cluster.change"
const registryChangeType = "cluster.registry"

// Permission codes.
const (
	PermClusterRead   = "cluster:read"
	PermClusterManage = "cluster:manage"
	PermNodeRead      = "node:read"
	PermNodeWrite     = "node:write"
	PermNSRead        = "namespace:read"
	PermNSWrite       = "namespace:write"
	PermWorkloadRead  = "workload:read"
	PermWorkloadWrite = "workload:write"
	PermNetworkWrite  = "network:write"
)

// Module is the cluster module.
type Module struct {
	d       *app.Deps
	log     *slog.Logger
	clients *clientCache
}

// New returns the module.
func New() *Module { return &Module{clients: newClientCache()} }

func (m *Module) Name() string { return "cluster" }

func (m *Module) Migrations() fs.FS { sub, _ := fs.Sub(migFS, "migrations"); return sub }

func (m *Module) Permissions() []rbac.Permission {
	return []rbac.Permission{
		rbac.P("cluster", "cluster", "read", "查看集群"),
		rbac.P("cluster", "cluster", "manage", "新增/编辑/删除集群"),
		rbac.P("cluster", "node", "read", "查看节点"),
		rbac.P("cluster", "node", "write", "节点封锁/驱逐/标签/污点"),
		rbac.P("cluster", "namespace", "read", "查看命名空间"),
		rbac.P("cluster", "namespace", "write", "创建/删除命名空间"),
		rbac.P("cluster", "workload", "read", "查看工作负载、日志、事件"),
		rbac.P("cluster", "workload", "write", "扩缩容/重启/删除/YAML 应用"),
		rbac.P("cluster", "network", "write", "创建/更新/删除 Kubernetes NetworkPolicy"),
		rbac.P("cluster", "network", "probe", "在指定 Pod 容器内运行受限 TCP 连通性探测"),
		rbac.P("cluster", "storage", "read", "查看 PV/PVC/StorageClass 与 VolumeSnapshot"),
		rbac.P("cluster", "storage", "write", "创建/修改/删除 PV/PVC/StorageClass 与 VolumeSnapshot"),
		rbac.P("cluster", "config", "read", "查看 ConfigMap 与 Secret 元数据"),
		rbac.P("cluster", "config", "write", "创建/修改/删除 ConfigMap 与 Secret"),
	}
}

func (m *Module) Register(r *gin.RouterGroup, d *app.Deps) {
	m.d = d
	m.log = d.Logger
	if m.log == nil {
		m.log = slog.Default()
	}
	if d.Approval != nil {
		d.Approval.MustRegister(approval.TypeSpec{
			Type: changeType, Module: "cluster",
			Description:     "Kubernetes 集群变更（工作负载、节点维护、NetworkPolicy 与 YAML 应用）",
			Timeout:         15 * time.Minute,
			Handler:         approval.HandlerFunc(m.onApproved),
			SanitizePayload: sanitizeApprovalPayload,
		})
		d.Approval.MustRegister(approval.TypeSpec{
			Type: registryChangeType, Module: "cluster",
			Description: "集群注册配置变更",
			Timeout:     15 * time.Minute,
			Handler:     registryApprovalHandler{module: m},
			SanitizePayload: func(json.RawMessage) json.RawMessage {
				return json.RawMessage(`{"change_id_redacted":true}`)
			},
		})
	}
	if d.AITools != nil {
		m.registerAITools(d)
	}

	g := r.Group("/clusters")
	g.GET("", rbac.RequirePerm(PermClusterRead), m.listClusters)
	g.GET("/health", rbac.RequirePerm(PermClusterRead), m.clusterHealth)
	g.POST("", rbac.RequirePerm(PermClusterManage), m.createCluster)
	g.POST("/contexts", rbac.RequirePerm(PermClusterManage), m.listKubeconfigContexts)
	g.POST("/import", rbac.RequirePerm(PermClusterManage), m.importKubeconfigContexts)
	g.POST("/test", rbac.RequirePerm(PermClusterManage), m.testRaw)
	g.GET("/:id", rbac.RequirePerm(PermClusterRead), m.getCluster)
	g.PUT("/:id", rbac.RequirePerm(PermClusterManage), m.updateCluster)
	g.DELETE("/:id", rbac.RequirePerm(PermClusterManage), m.deleteCluster)
	g.POST("/:id/test", rbac.RequirePerm(PermClusterRead), m.testCluster)

	g.GET("/:id/nodes", rbac.RequirePerm(PermNodeRead), m.listNodes)
	m.registerAlertRuleRoutes(g)
	g.GET("/:id/nodes/:node", rbac.RequirePerm(PermNodeRead), m.getNode)
	g.POST("/:id/nodes/:node/cordon", rbac.RequirePerm(PermNodeWrite), m.change("cordon"))
	g.POST("/:id/nodes/:node/uncordon", rbac.RequirePerm(PermNodeWrite), m.change("uncordon"))
	g.POST("/:id/nodes/:node/drain", rbac.RequirePerm(PermNodeWrite), m.change("drain"))
	g.PATCH("/:id/nodes/:node/labels", rbac.RequirePerm(PermNodeWrite), m.change("node_labels"))
	g.PUT("/:id/nodes/:node/taints", rbac.RequirePerm(PermNodeWrite), m.change("node_taints"))
	g.POST("/:id/node-preflight", rbac.RequirePerm(PermNodeRead), m.nodePreflight)
	g.GET("/:id/node-groups", rbac.RequirePerm(PermNodeRead), m.nodeGroups)
	g.POST("/:id/node-batch-labels", rbac.RequirePerm(PermNodeWrite), m.change("node_batch_labels"))
	g.PUT("/:id/node-batch-taints", rbac.RequirePerm(PermNodeWrite), m.change("node_batch_taints"))

	g.GET("/:id/namespaces", rbac.RequirePerm(PermNSRead), m.listNamespaces)
	g.POST("/:id/namespaces", rbac.RequirePerm(PermNSWrite), m.change("ns_create"))
	g.DELETE("/:id/namespaces/:ns", rbac.RequirePerm(PermNSWrite), m.change("ns_delete"))

	g.GET("/:id/events", rbac.RequirePerm(PermWorkloadRead), m.listEvents)
	g.GET("/:id/network/services", rbac.RequirePerm(PermWorkloadRead), m.listNetworkServices)
	g.GET("/:id/network/services/:ns/:name", rbac.RequirePerm(PermWorkloadRead), m.getService)
	g.POST("/:id/network/services", rbac.RequirePerm(PermNetworkWrite), m.change("service_create"))
	g.PUT("/:id/network/services/:ns/:name", rbac.RequirePerm(PermNetworkWrite), m.change("service_update"))
	g.DELETE("/:id/network/services/:ns/:name", rbac.RequirePerm(PermNetworkWrite), m.change("service_delete"))
	g.GET("/:id/network/ingresses", rbac.RequirePerm(PermWorkloadRead), m.listIngresses)
	g.GET("/:id/network/ingresses/:ns/:name", rbac.RequirePerm(PermWorkloadRead), m.getIngress)
	g.POST("/:id/network/ingresses", rbac.RequirePerm(PermNetworkWrite), m.change("ingress_create"))
	g.PUT("/:id/network/ingresses/:ns/:name", rbac.RequirePerm(PermNetworkWrite), m.change("ingress_update"))
	g.PATCH("/:id/network/ingresses/:ns/:name/annotations", rbac.RequirePerm(PermNetworkWrite), m.change("ingress_annotations"))
	g.DELETE("/:id/network/ingresses/:ns/:name", rbac.RequirePerm(PermNetworkWrite), m.change("ingress_delete"))
	g.GET("/:id/network/policies", rbac.RequirePerm(PermWorkloadRead), m.listNetworkPolicies)
	g.POST("/:id/network/policies/:ns/:name", rbac.RequirePerm(PermNetworkWrite), m.change("network_policy_create"))
	g.PUT("/:id/network/policies/:ns/:name", rbac.RequirePerm(PermNetworkWrite), m.change("network_policy_update"))
	g.DELETE("/:id/network/policies/:ns/:name", rbac.RequirePerm(PermNetworkWrite), m.change("network_policy_delete"))
	g.GET("/:id/network/configuration", rbac.RequirePerm(PermWorkloadRead), m.networkConfiguration)
	g.POST("/:id/network/probe", rbac.RequirePerm(PermWorkloadRead, "network:probe"), m.networkProbe)

	storageRead := rbac.RequireAnyPerm(PermWorkloadRead, PermStorageRead)
	g.GET("/:id/storage/classes", storageRead, m.listStorageClasses)
	g.GET("/:id/storage/volumes", storageRead, m.listPersistentVolumes)
	g.GET("/:id/storage/claims", storageRead, m.listPersistentVolumeClaims)
	g.GET("/:id/storage/snapshots", storageRead, m.listVolumeSnapshots)
	g.GET("/:id/storage/capabilities", storageRead, m.storageCapabilities)
	g.POST("/:id/storage/classes", rbac.RequirePerm(PermStorageWrite), m.change("sc_create"))
	g.PUT("/:id/storage/classes/:name", rbac.RequirePerm(PermStorageWrite), m.change("sc_update"))
	g.DELETE("/:id/storage/classes/:name", rbac.RequirePerm(PermStorageWrite), m.change("sc_delete"))
	g.POST("/:id/storage/claims", rbac.RequirePerm(PermStorageWrite), m.change("pvc_create"))
	g.PUT("/:id/storage/claims/:ns/:name", rbac.RequirePerm(PermStorageWrite), m.change("pvc_update"))
	g.DELETE("/:id/storage/claims/:ns/:name", rbac.RequirePerm(PermStorageWrite), m.change("pvc_delete"))
	g.POST("/:id/storage/claims/batch-delete", rbac.RequirePerm(PermStorageWrite), m.change("pvc_batch_delete"))
	g.POST("/:id/storage/volumes", rbac.RequirePerm(PermStorageWrite), m.change("pv_create"))
	g.PUT("/:id/storage/volumes/:name", rbac.RequirePerm(PermStorageWrite), m.change("pv_update"))
	g.DELETE("/:id/storage/volumes/:name", rbac.RequirePerm(PermStorageWrite), m.change("pv_delete"))
	g.POST("/:id/storage/snapshots", rbac.RequirePerm(PermStorageWrite), m.change("snapshot_create"))
	g.DELETE("/:id/storage/snapshots/:ns/:name", rbac.RequirePerm(PermStorageWrite), m.change("snapshot_delete"))

	configRead := rbac.RequireAnyPerm(PermWorkloadRead, PermConfigRead)
	g.GET("/:id/config/configmaps", configRead, m.listConfigMaps)
	g.GET("/:id/config/configmaps/:ns/:name", configRead, m.getConfigMap)
	g.POST("/:id/config/configmaps", rbac.RequirePerm(PermConfigWrite), m.change("configmap_create"))
	g.PUT("/:id/config/configmaps/:ns/:name", rbac.RequirePerm(PermConfigWrite), m.change("configmap_update"))
	g.DELETE("/:id/config/configmaps/:ns/:name", rbac.RequirePerm(PermConfigWrite), m.change("configmap_delete"))
	g.POST("/:id/config/configmaps/batch-delete", rbac.RequirePerm(PermConfigWrite), m.change("configmap_batch_delete"))
	g.GET("/:id/config/secrets", configRead, m.listSecrets)
	g.GET("/:id/config/secrets/:ns/:name", configRead, m.getSecret)
	g.POST("/:id/config/secrets", rbac.RequirePerm(PermConfigWrite), m.change("secret_create"))
	g.PUT("/:id/config/secrets/:ns/:name", rbac.RequirePerm(PermConfigWrite), m.change("secret_update"))
	g.DELETE("/:id/config/secrets/:ns/:name", rbac.RequirePerm(PermConfigWrite), m.change("secret_delete"))
	g.POST("/:id/config/secrets/batch-delete", rbac.RequirePerm(PermConfigWrite), m.change("secret_batch_delete"))
	g.GET("/:id/workloads/:kind", rbac.RequirePerm(PermWorkloadRead), m.listWorkloads)
	g.GET("/:id/hpa", rbac.RequirePerm(PermWorkloadRead), m.listHPAs)
	g.POST("/:id/hpa", rbac.RequirePerm(PermWorkloadWrite), m.change("hpa_create"))
	g.PUT("/:id/hpa/:ns/:name", rbac.RequirePerm(PermWorkloadWrite), m.change("hpa_update"))
	g.DELETE("/:id/hpa/:ns/:name", rbac.RequirePerm(PermWorkloadWrite), m.change("hpa_delete"))
	g.GET("/:id/cronjobs", rbac.RequirePerm(PermWorkloadRead), m.listCronJobs)
	g.POST("/:id/cronjobs", rbac.RequirePerm(PermWorkloadWrite), m.change("cronjob_create"))
	g.PUT("/:id/cronjobs/:ns/:name", rbac.RequirePerm(PermWorkloadWrite), m.change("cronjob_update"))
	g.DELETE("/:id/cronjobs/:ns/:name", rbac.RequirePerm(PermWorkloadWrite), m.change("cronjob_delete"))
	g.POST("/:id/cronjobs/:ns/:name/run", rbac.RequirePerm(PermWorkloadWrite), m.change("cronjob_run"))
	g.GET("/:id/cronjobs/:ns/:name/jobs", rbac.RequirePerm(PermWorkloadRead), m.listCronJobRuns)
	g.GET("/:id/namespaces/:ns/:kind/:name", rbac.RequirePerm(PermWorkloadRead), m.getWorkloadYAML)
	g.POST("/:id/namespaces/:ns/:kind/:name/scale", rbac.RequirePerm(PermWorkloadWrite), m.change("scale"))
	g.POST("/:id/namespaces/:ns/:kind/:name/restart", rbac.RequirePerm(PermWorkloadWrite), m.change("restart"))
	g.DELETE("/:id/namespaces/:ns/:kind/:name", rbac.RequirePerm(PermWorkloadWrite), m.change("delete"))
	g.GET("/:id/namespaces/:ns/pods/:name/logs", rbac.RequirePerm(PermWorkloadRead), m.podLogs)
	g.POST("/:id/apply", rbac.RequirePerm(PermWorkloadWrite), m.change("apply"))
}

// ---------------------------------------------------------------- store

// Cluster is a registered Kubernetes cluster.
type Cluster struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Env         string     `json:"env"`
	Description string     `json:"description"`
	APIServer   string     `json:"api_server"`
	Kubeconfig  string     `json:"kubeconfig,omitempty"` // masked in responses
	Version     string     `json:"version"`
	Status      string     `json:"status"`
	LastError   string     `json:"last_error"`
	CheckedAt   *time.Time `json:"checked_at"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`

	encKubeconfig string
}

const clusterCols = `id,name,env,description,api_server,kubeconfig,version,status,last_error,checked_at,created_by,created_at,updated_at`

func scanCluster(row pgx.Row) (*Cluster, error) {
	var c Cluster
	err := row.Scan(&c.ID, &c.Name, &c.Env, &c.Description, &c.APIServer, &c.encKubeconfig, &c.Version,
		&c.Status, &c.LastError, &c.CheckedAt, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, httpx.NotFound("cluster not found")
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if server, err := displayAPIServer(c.APIServer); err == nil {
		c.APIServer = server
	} else {
		c.APIServer = ""
	}
	c.Kubeconfig = crypto.Mask(c.encKubeconfig)
	return &c, nil
}

func (m *Module) loadCluster(ctx context.Context, id int64) (*Cluster, error) {
	return scanCluster(m.d.DB.QueryRow(ctx, `SELECT `+clusterCols+` FROM cluster_clusters WHERE id=$1`, id))
}

func (m *Module) loadClusterByName(ctx context.Context, name string) (*Cluster, error) {
	return scanCluster(m.d.DB.QueryRow(ctx, `SELECT `+clusterCols+` FROM cluster_clusters WHERE name=$1`, name))
}

// clusterFor loads the cluster in the path and enforces the caller's env scope.
func (m *Module) clusterFor(c *gin.Context) (*Cluster, bool) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return nil, false
	}
	cl, err := m.loadCluster(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return nil, false
	}
	if p := rbac.PrincipalFrom(c); p != nil && !p.CanEnv(cl.Env) {
		httpx.Fail(c, httpx.Forbidden("no access to environment "+cl.Env))
		return nil, false
	}
	audit.SetResource(c, "cluster", fmt.Sprint(cl.ID))
	return cl, true
}

func (m *Module) kube(ctx context.Context, cl *Cluster) (*kubeClients, error) {
	return m.clients.get(cl, func() (string, error) { return m.d.Crypto.Decrypt(cl.encKubeconfig) })
}

type clusterInput struct {
	Name        string `json:"name" binding:"required,max=128"`
	Env         string `json:"env" binding:"max=64"`
	Description string `json:"description"`
	APIServer   string `json:"api_server" binding:"max=512"`
	Kubeconfig  string `json:"kubeconfig"`
	Context     string `json:"context"`
}

type registryApprovalPayload struct {
	ChangeID int64 `json:"change_id"`
}

type registryMutation struct {
	Input        clusterInput          `json:"input,omitempty"`
	Imports      []registryImportEntry `json:"imports,omitempty"`
	PreviousEnv  string                `json:"previous_env,omitempty"`
	PreviousName string                `json:"previous_name,omitempty"`
	Actor        string                `json:"actor,omitempty"`
}

type registryImportEntry struct {
	Input   clusterInput `json:"input"`
	Version string       `json:"version,omitempty"`
}

type registryChangeRow struct {
	ID          int64
	Env         string
	Action      string
	ClusterID   *int64
	Cluster     string
	ApplicantID int64
	PayloadEnc  string
}

func registryPayloadAAD(row registryChangeRow) []byte {
	return []byte(fmt.Sprintf("cluster-registry:%d:%s:%s:%d", row.ID, row.Env, row.Action, row.ApplicantID))
}

func encryptRegistryMutation(cipher *crypto.Cipher, row registryChangeRow, mutation registryMutation) (string, error) {
	raw, err := json.Marshal(mutation)
	if err != nil {
		return "", err
	}
	return cipher.EncryptBytes(raw, registryPayloadAAD(row))
}

func decryptRegistryMutation(cipher *crypto.Cipher, row registryChangeRow) (registryMutation, error) {
	raw, err := cipher.DecryptBytes(row.PayloadEnc, registryPayloadAAD(row))
	if err != nil {
		return registryMutation{}, err
	}
	var mutation registryMutation
	if err := json.Unmarshal(raw, &mutation); err != nil {
		return registryMutation{}, err
	}
	return mutation, nil
}

func (m *Module) stageRegistryChange(ctx context.Context, p *rbac.Principal, action string, clusterID *int64, cluster, env string, mutation registryMutation) (int64, error) {
	if p == nil || m.d == nil || m.d.DB == nil || m.d.Crypto == nil {
		return 0, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "集群注册审批暂存服务不可用")
	}
	tx, err := m.d.DB.Begin(ctx)
	if err != nil {
		return 0, httpx.Internal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, _ = tx.Exec(ctx, `DELETE FROM cluster_registry_changes WHERE expires_at <= now()`)
	row := registryChangeRow{Env: env, Action: action, ClusterID: clusterID, Cluster: cluster, ApplicantID: p.UserID}
	err = tx.QueryRow(ctx, `INSERT INTO cluster_registry_changes
		(env,action,cluster_id,cluster,applicant_id,payload_enc)
		VALUES($1,$2,$3,$4,$5,'') RETURNING id`, row.Env, row.Action, row.ClusterID, row.Cluster, row.ApplicantID).Scan(&row.ID)
	if err != nil {
		return 0, httpx.Internal(err)
	}
	row.PayloadEnc, err = encryptRegistryMutation(m.d.Crypto, row, mutation)
	if err != nil {
		return 0, httpx.Internal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE cluster_registry_changes SET payload_enc=$2 WHERE id=$1`, row.ID, row.PayloadEnc); err != nil {
		return 0, httpx.Internal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, httpx.Internal(err)
	}
	return row.ID, nil
}

func (m *Module) clearRegistryChange(ctx context.Context, id int64) {
	if m.d != nil && m.d.DB != nil && id > 0 {
		_, _ = m.d.DB.Exec(ctx, `DELETE FROM cluster_registry_changes WHERE id=$1`, id)
	}
}

func (m *Module) runRegistryChange(ctx context.Context, p *rbac.Principal, action string, clusterID *int64, cluster, env string, mutation registryMutation, reason string) (any, *approval.Ticket, error) {
	if m.d == nil || m.d.Approval == nil {
		return nil, nil, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "审批服务不可用，集群注册变更已拒绝")
	}
	if p == nil || !p.Can(PermClusterManage) || !p.CanEnv(env) || strings.TrimSpace(env) == "" {
		return nil, nil, httpx.Forbidden("集群注册权限或环境范围不足")
	}
	id, err := m.stageRegistryChange(ctx, p, action, clusterID, cluster, env, mutation)
	if err != nil {
		return nil, nil, err
	}
	payload := registryApprovalPayload{ChangeID: id}
	title := fmt.Sprintf("cluster %s: %s", action, cluster)
	ticket, pending, err := m.d.Approval.Gate(ctx, p, approval.CreateInput{
		Type: registryChangeType, Title: title, Env: env, Reason: reason, Payload: payload,
	})
	if err != nil {
		m.clearRegistryChange(context.WithoutCancel(ctx), id)
		return nil, nil, err
	}
	if pending {
		return nil, ticket, nil
	}
	result, err := m.executeRegistryChange(ctx, id, p.UserID, env)
	m.clearRegistryChange(context.WithoutCancel(ctx), id)
	return result, nil, err
}

type registryApprovalHandler struct{ module *Module }

func (h registryApprovalHandler) OnApproved(ctx context.Context, ticket *approval.Ticket) (any, error) {
	var payload registryApprovalPayload
	if err := ticket.DecodePayload(&payload); err != nil || payload.ChangeID <= 0 {
		return nil, httpx.BadRequest("集群注册审批载荷无效")
	}
	defer h.module.clearRegistryChange(context.WithoutCancel(ctx), payload.ChangeID)
	result, err := h.module.executeRegistryChange(ctx, payload.ChangeID, ticket.ApplicantID, ticket.Env)
	return result, err
}

func (h registryApprovalHandler) OnRejected(ctx context.Context, ticket *approval.Ticket) error {
	var payload registryApprovalPayload
	if err := ticket.DecodePayload(&payload); err != nil || payload.ChangeID <= 0 {
		return nil
	}
	h.module.clearRegistryChange(ctx, payload.ChangeID)
	return nil
}

func (m *Module) executeRegistryChange(ctx context.Context, id, applicantID int64, expectedEnv string) (any, error) {
	var row registryChangeRow
	var clusterID int64
	err := m.d.DB.QueryRow(ctx, `SELECT id,env,action,cluster_id,cluster,applicant_id,payload_enc
		FROM cluster_registry_changes WHERE id=$1 AND expires_at>now()`, id).
		Scan(&row.ID, &row.Env, &row.Action, &clusterID, &row.Cluster, &row.ApplicantID, &row.PayloadEnc)
	if err != nil {
		return nil, httpx.Conflict("集群注册审批暂存已失效或不存在")
	}
	if clusterID > 0 {
		row.ClusterID = &clusterID
	}
	if row.ApplicantID != applicantID || row.Env != expectedEnv || m.d.RBAC == nil {
		return nil, httpx.Conflict("集群注册审批身份或环境已变更")
	}
	_, permissions, scope, err := m.d.RBAC.UserAccess(ctx, applicantID)
	if err != nil {
		return nil, err
	}
	principal := &rbac.Principal{UserID: applicantID, Permissions: permissions, EnvScope: scope}
	if !principal.Can(PermClusterManage) || !principal.CanEnv(row.Env) {
		return nil, httpx.Forbidden("申请人的集群权限或环境范围已撤销")
	}
	mutation, err := decryptRegistryMutation(m.d.Crypto, row)
	if err != nil {
		return nil, httpx.Internal(err)
	}

	var current *Cluster
	if row.Action == "update" || row.Action == "delete" {
		if row.ClusterID == nil {
			return nil, httpx.Conflict("集群注册审批缺少目标集群")
		}
		current, err = m.loadCluster(ctx, *row.ClusterID)
		if err != nil {
			return nil, err
		}
		if current.Env != mutation.PreviousEnv || current.Env != row.Env || current.Name != mutation.PreviousName || current.Name != row.Cluster {
			return nil, httpx.Conflict("集群注册审批目标已变更")
		}
	}

	switch row.Action {
	case "create":
		if mutation.Input.Env != row.Env || mutation.Input.Name != row.Cluster || mutation.Input.Kubeconfig == "" {
			return nil, httpx.Conflict("集群注册审批内容不匹配")
		}
		encrypted, err := m.d.Crypto.Encrypt(mutation.Input.Kubeconfig)
		if err != nil {
			return nil, httpx.Internal(err)
		}
		created, err := scanCluster(m.d.DB.QueryRow(ctx, `INSERT INTO cluster_clusters
			(name,env,description,api_server,kubeconfig,created_by)
			VALUES($1,$2,$3,$4,$5,$6) RETURNING `+clusterCols,
			mutation.Input.Name, row.Env, mutation.Input.Description, mutation.Input.APIServer, encrypted, mutation.Actor))
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate key") {
				return nil, httpx.Conflict("cluster name already exists")
			}
			return nil, httpx.Internal(errors.New("cluster registration failed"))
		}
		return created, nil
	case "update":
		in := mutation.Input
		if in.Env != row.Env || strings.TrimSpace(in.Name) == "" {
			return nil, httpx.BadRequest("cluster identity is invalid")
		}
		encrypted := current.encKubeconfig
		if in.Kubeconfig != "" {
			encrypted, err = m.d.Crypto.Encrypt(in.Kubeconfig)
			if err != nil {
				return nil, httpx.Internal(err)
			}
		}
		updated, err := scanCluster(m.d.DB.QueryRow(ctx, `UPDATE cluster_clusters SET name=$2,description=$3,api_server=$4,
			kubeconfig=$5,updated_at=now() WHERE id=$1 RETURNING `+clusterCols,
			current.ID, in.Name, in.Description, in.APIServer, encrypted))
		if err != nil {
			return nil, httpx.Internal(errors.New("cluster update failed"))
		}
		m.clients.drop(current.ID)
		return updated, nil
	case "delete":
		tag, err := m.d.DB.Exec(ctx, `DELETE FROM cluster_clusters WHERE id=$1 AND env=$2 AND name=$3`, current.ID, row.Env, row.Cluster)
		if err != nil || tag.RowsAffected() != 1 {
			return nil, httpx.Conflict("cluster changed before deletion")
		}
		m.clients.drop(current.ID)
		return gin.H{"id": current.ID}, nil
	case "import":
		return m.executeRegistryImport(ctx, row, mutation)
	default:
		return nil, httpx.BadRequest("unknown cluster registry action")
	}
}

func (m *Module) executeRegistryImport(ctx context.Context, row registryChangeRow, mutation registryMutation) (any, error) {
	if len(mutation.Imports) == 0 {
		return nil, httpx.BadRequest("cluster import approval is empty")
	}
	type prepared struct {
		entry registryImportEntry
		enc   string
	}
	items := make([]prepared, len(mutation.Imports))
	for i, entry := range mutation.Imports {
		if entry.Input.Env != row.Env || strings.TrimSpace(entry.Input.Kubeconfig) == "" {
			return nil, httpx.Conflict("cluster import environment or credentials do not match approval")
		}
		version, err := m.testKubeconfig(ctx, entry.Input.Kubeconfig)
		if err != nil {
			return nil, httpx.BadRequest("a cluster connection test failed; no clusters were imported")
		}
		entry.Version = version
		encrypted, err := m.d.Crypto.Encrypt(entry.Input.Kubeconfig)
		if err != nil {
			return nil, httpx.Internal(err)
		}
		items[i] = prepared{entry: entry, enc: encrypted}
	}
	tx, err := m.d.DB.Begin(ctx)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	created := make([]gin.H, 0, len(items))
	for _, item := range items {
		var id int64
		err = tx.QueryRow(ctx, `INSERT INTO cluster_clusters
			(name,env,description,api_server,kubeconfig,version,status,last_error,checked_at,created_by)
			VALUES($1,$2,$3,$4,$5,$6,'connected','',now(),$7) RETURNING id`,
			item.entry.Input.Name, row.Env, item.entry.Input.Description, item.entry.Input.APIServer,
			item.enc, item.entry.Version, mutation.Actor).Scan(&id)
		if err != nil {
			return nil, httpx.Conflict("cluster registration batch failed; no clusters were imported")
		}
		created = append(created, gin.H{"id": id, "name": item.entry.Input.Name, "context": item.entry.Input.Context,
			"version": item.entry.Version, "status": "connected"})
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, httpx.Internal(err)
	}
	return gin.H{"created": len(created), "failed": 0, "total": len(created), "items": created}, nil
}

func (m *Module) testKubeconfig(ctx context.Context, raw string) (string, error) {
	kc, err := buildClients(raw, "")
	if err != nil {
		return "", err
	}
	return serverVersion(ctx, kc.cs)
}

func (m *Module) listClusters(c *gin.Context) {
	p := httpx.ParsePage(c)
	ctx := c.Request.Context()
	var scope []string
	if pr := rbac.PrincipalFrom(c); pr != nil {
		scope = pr.EnvScope
	}
	where, args := clusterListFilter(c.Query("env"), c.Query("keyword"), scope)
	var total int64
	if err := m.d.DB.QueryRow(ctx, `SELECT count(*) FROM cluster_clusters `+where, args...).Scan(&total); err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	args = append(args, p.Limit(), p.Offset())
	rows, err := m.d.DB.Query(ctx, fmt.Sprintf(`SELECT %s FROM cluster_clusters %s ORDER BY id LIMIT $%d OFFSET $%d`,
		clusterCols, where, len(args)-1, len(args)), args...)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	defer rows.Close()
	items := []Cluster{}
	for rows.Next() {
		cl, err := scanCluster(rows)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		items = append(items, *cl)
	}
	httpx.OK(c, httpx.NewPageResult(items, total, p))
}

func clusterListFilter(env, keyword string, envScope []string) (string, []any) {
	where, args := "WHERE 1=1", []any{}
	if env != "" {
		args = append(args, env)
		where += fmt.Sprintf(" AND env=$%d", len(args))
	}
	if keyword != "" {
		args = append(args, "%"+keyword+"%")
		where += fmt.Sprintf(" AND name ILIKE $%d", len(args))
	}
	if envScope != nil && !containsEnv(envScope, "*") {
		args = append(args, envScope)
		where += fmt.Sprintf(" AND env = ANY($%d)", len(args))
	}
	return where, args
}

func containsEnv(scope []string, env string) bool {
	for _, value := range scope {
		if value == env {
			return true
		}
	}
	return false
}

func (m *Module) getCluster(c *gin.Context) {
	if cl, ok := m.clusterFor(c); ok {
		httpx.OK(c, cl)
	}
}

// prepareKubeconfig validates the kubeconfig, optionally pins a context and
// fills in the API server address.
func prepareKubeconfig(in *clusterInput) error {
	kc, server, err := kubeconfigForContext(in.Kubeconfig, in.Context)
	if err != nil {
		return httpx.BadRequest("invalid kubeconfig")
	}
	in.Kubeconfig = kc
	in.APIServer = server
	return nil
}

func (m *Module) createCluster(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxKubeconfigRequestBytes)
	var in clusterInput
	if !httpx.Bind(c, &in) {
		return
	}
	p := rbac.PrincipalFrom(c)
	if p == nil || strings.TrimSpace(in.Env) == "" {
		httpx.Fail(c, httpx.BadRequest("an environment is required"))
		return
	}
	if !p.CanEnv(in.Env) {
		httpx.Fail(c, httpx.Forbidden("no access to environment "+in.Env))
		return
	}
	if strings.TrimSpace(in.Kubeconfig) == "" {
		httpx.Fail(c, httpx.BadRequest("kubeconfig is required"))
		return
	}
	if err := prepareKubeconfig(&in); err != nil {
		httpx.Fail(c, err)
		return
	}
	res, ticket, err := m.runRegistryChange(c.Request.Context(), p, "create", nil, in.Name, in.Env,
		registryMutation{Input: in, Actor: p.Username}, c.Query("reason"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "cluster", in.Name)
	audit.SetAction(c, "cluster.create")
	audit.SetDetail(c, gin.H{"name": in.Name, "env": in.Env, "api_server": in.APIServer})
	if ticket != nil {
		approval.RespondPending(c, ticket)
		return
	}
	httpx.Created(c, res)
}

func (m *Module) updateCluster(c *gin.Context) {
	cl, ok := m.clusterFor(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxKubeconfigRequestBytes)
	var in clusterInput
	if !httpx.Bind(c, &in) {
		return
	}
	p := rbac.PrincipalFrom(c)
	if p == nil || !p.CanEnv(in.Env) {
		httpx.Fail(c, httpx.Forbidden("no access to environment "+in.Env))
		return
	}
	if in.Env != cl.Env {
		httpx.Fail(c, httpx.BadRequest("cluster environment changes are not supported"))
		return
	}
	if strings.TrimSpace(in.Kubeconfig) != "" && !strings.Contains(in.Kubeconfig, "***") {
		if err := prepareKubeconfig(&in); err != nil {
			httpx.Fail(c, err)
			return
		}
	} else {
		in.Kubeconfig = ""
	}
	if in.APIServer == "" {
		in.APIServer = cl.APIServer
	}
	res, ticket, err := m.runRegistryChange(c.Request.Context(), p, "update", &cl.ID, cl.Name, cl.Env,
		registryMutation{Input: in, PreviousEnv: cl.Env, PreviousName: cl.Name, Actor: p.Username}, c.Query("reason"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetAction(c, "cluster.update")
	audit.SetDetail(c, gin.H{"name": in.Name, "env": in.Env, "kubeconfig_changed": in.Kubeconfig != ""})
	if ticket != nil {
		approval.RespondPending(c, ticket)
		return
	}
	httpx.OK(c, res)
}

func (m *Module) deleteCluster(c *gin.Context) {
	cl, ok := m.clusterFor(c)
	if !ok {
		return
	}
	p := rbac.PrincipalFrom(c)
	res, ticket, err := m.runRegistryChange(c.Request.Context(), p, "delete", &cl.ID, cl.Name, cl.Env,
		registryMutation{PreviousEnv: cl.Env, PreviousName: cl.Name}, c.Query("reason"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetAction(c, "cluster.delete")
	audit.SetDetail(c, gin.H{"name": cl.Name, "env": cl.Env})
	if ticket != nil {
		approval.RespondPending(c, ticket)
		return
	}
	httpx.OK(c, res)
}

// probe calls /version and persists status (best effort).
func (m *Module) probe(ctx context.Context, cl *Cluster) map[string]any {
	res := map[string]any{"ok": false}
	kc, err := m.kube(ctx, cl)
	var ver string
	if err == nil {
		ver, err = serverVersion(ctx, kc.cs)
	}
	status, msg := "connected", ""
	if err != nil {
		status, msg = "error", err.Error()
		res["error"] = msg
	} else {
		res["ok"], res["version"] = true, ver
		cl.Version = ver
	}
	cl.Status, cl.LastError = status, msg
	_, _ = m.d.DB.Exec(ctx, `UPDATE cluster_clusters SET status=$2,last_error=$3,version=CASE WHEN $4='' THEN version ELSE $4 END,checked_at=now() WHERE id=$1`,
		cl.ID, status, msg, ver)
	return res
}

func (m *Module) testCluster(c *gin.Context) {
	if cl, ok := m.clusterFor(c); ok {
		httpx.OK(c, m.probe(c.Request.Context(), cl))
	}
}

// testRaw tests a kubeconfig before saving it.
func (m *Module) testRaw(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxKubeconfigRequestBytes)
	var in struct {
		Kubeconfig string `json:"kubeconfig" binding:"required"`
		Context    string `json:"context"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	raw, _, err := kubeconfigForContext(in.Kubeconfig, in.Context)
	if err != nil {
		httpx.OK(c, gin.H{"ok": false, "error": err.Error()})
		return
	}
	kc, err := buildClients(raw, "")
	if err != nil {
		httpx.OK(c, gin.H{"ok": false, "error": err.Error()})
		return
	}
	ver, err := serverVersion(c.Request.Context(), kc.cs)
	if err != nil {
		httpx.OK(c, gin.H{"ok": false, "error": err.Error(), "api_server": kc.rest.Host})
		return
	}
	httpx.OK(c, gin.H{"ok": true, "version": ver, "api_server": kc.rest.Host})
}

// ---------------------------------------------------------------- gated changes

// changePayload is stored in approval tickets and re-executed by onApproved.
type changePayload struct {
	ClusterID       int64           `json:"cluster_id"`
	Cluster         string          `json:"cluster"`
	Action          string          `json:"action"`
	Namespace       string          `json:"namespace,omitempty"`
	Kind            string          `json:"kind,omitempty"`
	Name            string          `json:"name,omitempty"`
	Params          json.RawMessage `json:"params,omitempty"`
	EncryptedParams string          `json:"encrypted_params,omitempty"`
	SecretKeys      []string        `json:"secret_keys,omitempty"`
	ParamsRedacted  bool            `json:"params_redacted,omitempty"`
}

// change returns a handler that builds a changePayload from the request and
// runs it, through approval.Gate when the cluster env requires it.
func (m *Module) change(action string) gin.HandlerFunc {
	return func(c *gin.Context) {
		cl, ok := m.clusterFor(c)
		if !ok {
			return
		}
		pl := changePayload{ClusterID: cl.ID, Cluster: cl.Name, Action: action,
			Namespace: c.Param("ns"), Kind: c.Param("kind"), Name: c.Param("name")}
		if n := c.Param("node"); n != "" {
			pl.Name = n
		}
		if c.Request.ContentLength != 0 && c.Request.Body != nil {
			if strings.HasPrefix(action, "network_policy_") {
				c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256<<10)
			}
			switch {
			case strings.HasPrefix(action, "pvc_"), strings.HasPrefix(action, "sc_"),
				strings.HasPrefix(action, "snapshot_"), strings.HasPrefix(action, "pv_"):
				c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 512<<10)
			case strings.HasPrefix(action, "configmap_"), strings.HasPrefix(action, "secret_"):
				c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2<<20)
			case strings.HasPrefix(action, "service_"), strings.HasPrefix(action, "ingress_"):
				c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 512<<10)
			case strings.HasPrefix(action, "node_batch_"):
				c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 512<<10)
			}
			var raw json.RawMessage
			if !httpx.Bind(c, &raw) {
				return
			}
			pl.Params = raw
		}
		if err := validateChange(&pl); err != nil {
			httpx.Fail(c, err)
			return
		}
		if err := m.encryptSecretParams(&pl); err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetAction(c, action)
		audit.SetDetail(c, sanitizeChangeForDisplay(pl))
		res, t, err := m.runChange(c.Request.Context(), rbac.PrincipalFrom(c), cl, pl, c.Query("reason"))
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		if t != nil {
			approval.RespondPending(c, t)
			return
		}
		httpx.OK(c, res)
	}
}

// runChange gates and executes. It returns a ticket when approval is pending.
// Missing approval service, principal or environment policy is an error, never
// an implicit allow: Kubernetes writes must always pass the server-side gate.
func (m *Module) runChange(ctx context.Context, p *rbac.Principal, cl *Cluster, pl changePayload, reason string) (any, *approval.Ticket, error) {
	if m.d.Approval == nil {
		return nil, nil, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "审批服务不可用，集群变更已拒绝")
	}
	if p == nil {
		return nil, nil, httpx.Unauthorized("authentication required for cluster change")
	}
	if strings.TrimSpace(cl.Env) == "" {
		return nil, nil, httpx.Forbidden("集群未设置环境，无法执行变更审批")
	}
	title := fmt.Sprintf("[%s] %s %s", cl.Name, pl.Action, strings.Trim(strings.Join([]string{pl.Namespace, pl.Kind, pl.Name}, "/"), "/"))
	t, pending, err := m.d.Approval.Gate(ctx, p, approval.CreateInput{Type: changeType, Title: title, Env: cl.Env, Reason: reason, Payload: pl})
	if err != nil {
		return nil, nil, err
	}
	if pending {
		return nil, t, nil
	}
	res, err := m.executeChange(ctx, cl, pl)
	return res, nil, err
}

func (m *Module) onApproved(ctx context.Context, t *approval.Ticket) (any, error) {
	var pl changePayload
	if err := t.DecodePayload(&pl); err != nil {
		return nil, err
	}
	cl, err := m.loadCluster(ctx, pl.ClusterID)
	if err != nil {
		return nil, err
	}
	if m.d.RBAC == nil || cl.Env != t.Env || cl.Name != pl.Cluster {
		return nil, httpx.Conflict("cluster identity or approval environment changed")
	}
	_, permissions, scope, err := m.d.RBAC.UserAccess(ctx, t.ApplicantID)
	if err != nil {
		return nil, err
	}
	p := &rbac.Principal{UserID: t.ApplicantID, Username: t.ApplicantName, Permissions: permissions, EnvScope: scope}
	permission := changePermission(pl.Action)
	if !p.Can(permission) || !p.CanEnv(cl.Env) {
		return nil, httpx.Forbidden("applicant permissions or environment scope revoked")
	}
	return m.executeChange(ctx, cl, pl)
}

func (m *Module) executeChange(ctx context.Context, cl *Cluster, pl changePayload) (any, error) {
	if pl.Action == "alert_rule_create" {
		var in alertRuleInput
		if err := decodeParams(pl.Params, &in); err != nil {
			return nil, err
		}
		if err := validateAlertRuleInput(in); err != nil {
			return nil, err
		}
		return m.applyAlertRule(ctx, cl, in)
	}
	return m.execute(ctx, cl, pl)
}

func isSecretWrite(action string) bool {
	return action == "secret_create" || action == "secret_update"
}

func secretParamsAAD(pl changePayload) []byte {
	return []byte(fmt.Sprintf("cluster-change:%d:%s", pl.ClusterID, pl.Action))
}

func (m *Module) encryptSecretParams(pl *changePayload) error {
	if !isSecretWrite(pl.Action) {
		return nil
	}
	if m.d == nil || m.d.Crypto == nil {
		return httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "Secret 加密服务不可用，变更已拒绝")
	}
	var params secretParams
	if err := decodeParams(pl.Params, &params); err != nil {
		return err
	}
	pl.SecretKeys = make([]string, 0, len(params.Data))
	for key := range params.Data {
		pl.SecretKeys = append(pl.SecretKeys, key)
	}
	sort.Strings(pl.SecretKeys)
	encrypted, err := m.d.Crypto.EncryptBytes(pl.Params, secretParamsAAD(*pl))
	if err != nil {
		return httpx.Internal(err)
	}
	pl.Params = nil
	pl.EncryptedParams = encrypted
	return nil
}

func sanitizeChangeForDisplay(pl changePayload) changePayload {
	if isSecretWrite(pl.Action) {
		pl.Params = nil
		pl.EncryptedParams = ""
		pl.ParamsRedacted = true
	}
	return pl
}

func sanitizeApprovalPayload(raw json.RawMessage) json.RawMessage {
	var pl changePayload
	if err := json.Unmarshal(raw, &pl); err != nil {
		return json.RawMessage(`{"payload_redacted":true}`)
	}
	if !isSecretWrite(pl.Action) {
		return raw
	}
	safe, err := json.Marshal(sanitizeChangeForDisplay(pl))
	if err != nil {
		return json.RawMessage(`{"payload_redacted":true}`)
	}
	return safe
}

func changePermission(action string) string {
	switch action {
	case "alert_rule_create":
		return PermClusterManage
	case "cordon", "uncordon", "drain", "node_labels", "node_taints",
		"node_batch_labels", "node_batch_taints":
		return PermNodeWrite
	case "ns_create", "ns_delete":
		return PermNSWrite
	case "network_policy_create", "network_policy_update", "network_policy_delete":
		return PermNetworkWrite
	case "hpa_create", "hpa_update", "hpa_delete",
		"cronjob_create", "cronjob_update", "cronjob_delete", "cronjob_run":
		return PermWorkloadWrite
	case "sc_create", "sc_update", "sc_delete", "pvc_create", "pvc_update", "pvc_delete",
		"pvc_batch_delete", "pv_create", "pv_update", "pv_delete", "snapshot_create", "snapshot_delete",
		"service_create", "service_update", "service_delete",
		"ingress_create", "ingress_update", "ingress_delete", "ingress_annotations",
		"configmap_create", "configmap_update", "configmap_delete", "configmap_batch_delete",
		"secret_create", "secret_update", "secret_delete", "secret_batch_delete":
		if strings.HasPrefix(action, "sc_") || strings.HasPrefix(action, "pvc_") || strings.HasPrefix(action, "pv_") || strings.HasPrefix(action, "snapshot_") {
			return PermStorageWrite
		}
		if strings.HasPrefix(action, "configmap_") || strings.HasPrefix(action, "secret_") {
			return PermConfigWrite
		}
		return PermNetworkWrite
	}
	return PermWorkloadWrite
}

func (m *Module) execute(ctx context.Context, cl *Cluster, pl changePayload) (any, error) {
	if pl.EncryptedParams != "" {
		if m.d == nil || m.d.Crypto == nil {
			return nil, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "Secret 解密服务不可用，变更已拒绝")
		}
		params, err := m.d.Crypto.DecryptBytes(pl.EncryptedParams, secretParamsAAD(pl))
		if err != nil {
			return nil, httpx.Internal(err)
		}
		pl.Params = params
		pl.EncryptedParams = ""
	}
	kc, err := m.kube(ctx, cl)
	if err != nil {
		return nil, httpx.BadRequest("cluster client: " + err.Error())
	}
	res, err := executeOn(ctx, kc, pl)
	if err != nil {
		return nil, kubeErr(err)
	}
	return res, nil
}
