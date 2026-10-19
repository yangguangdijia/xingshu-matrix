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
			Description: "Kubernetes 集群变更（扩缩容/重启/删除/节点维护/YAML 应用）",
			Timeout:     15 * time.Minute,
			Handler:     approval.HandlerFunc(m.onApproved),
		})
	}
	if d.AITools != nil {
		m.registerAITools(d)
	}

	g := r.Group("/clusters")
	g.GET("", rbac.RequirePerm(PermClusterRead), m.listClusters)
	g.POST("", rbac.RequirePerm(PermClusterManage), m.createCluster)
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

	g.GET("/:id/namespaces", rbac.RequirePerm(PermNSRead), m.listNamespaces)
	g.POST("/:id/namespaces", rbac.RequirePerm(PermNSWrite), m.change("ns_create"))
	g.DELETE("/:id/namespaces/:ns", rbac.RequirePerm(PermNSWrite), m.change("ns_delete"))

	g.GET("/:id/events", rbac.RequirePerm(PermWorkloadRead), m.listEvents)
	g.GET("/:id/workloads/:kind", rbac.RequirePerm(PermWorkloadRead), m.listWorkloads)
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

func (m *Module) listClusters(c *gin.Context) {
	p := httpx.ParsePage(c)
	ctx := c.Request.Context()
	where, args := "WHERE 1=1", []any{}
	if env := c.Query("env"); env != "" {
		args = append(args, env)
		where += fmt.Sprintf(" AND env=$%d", len(args))
	}
	if kw := c.Query("keyword"); kw != "" {
		args = append(args, "%"+kw+"%")
		where += fmt.Sprintf(" AND name ILIKE $%d", len(args))
	}
	if pr := rbac.PrincipalFrom(c); pr != nil && pr.EnvScope != nil {
		args = append(args, pr.EnvScope)
		where += fmt.Sprintf(" AND env = ANY($%d)", len(args))
	}
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

func (m *Module) getCluster(c *gin.Context) {
	if cl, ok := m.clusterFor(c); ok {
		httpx.OK(c, cl)
	}
}

// prepareKubeconfig validates the kubeconfig, optionally pins a context and
// fills in the API server address.
func prepareKubeconfig(in *clusterInput) error {
	kc, server, err := normalizeKubeconfig(in.Kubeconfig, in.Context)
	if err != nil {
		return httpx.BadRequest("invalid kubeconfig: " + err.Error())
	}
	in.Kubeconfig = kc
	if in.APIServer == "" {
		in.APIServer = server
	}
	return nil
}

func (m *Module) createCluster(c *gin.Context) {
	var in clusterInput
	if !httpx.Bind(c, &in) {
		return
	}
	if p := rbac.PrincipalFrom(c); p != nil && !p.CanEnv(in.Env) {
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
	enc, err := m.d.Crypto.Encrypt(in.Kubeconfig)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	by := ""
	if p := rbac.PrincipalFrom(c); p != nil {
		by = p.Username
	}
	ctx := c.Request.Context()
	cl, err := scanCluster(m.d.DB.QueryRow(ctx, `INSERT INTO cluster_clusters(name,env,description,api_server,kubeconfig,created_by)
		VALUES($1,$2,$3,$4,$5,$6) RETURNING `+clusterCols, in.Name, in.Env, in.Description, in.APIServer, enc, by))
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			err = httpx.Conflict("cluster name already exists")
		}
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "cluster", fmt.Sprint(cl.ID))
	audit.SetDetail(c, gin.H{"name": in.Name, "env": in.Env, "api_server": in.APIServer})
	m.probe(ctx, cl)
	httpx.Created(c, cl)
}

func (m *Module) updateCluster(c *gin.Context) {
	cl, ok := m.clusterFor(c)
	if !ok {
		return
	}
	var in clusterInput
	if !httpx.Bind(c, &in) {
		return
	}
	if p := rbac.PrincipalFrom(c); p != nil && !p.CanEnv(in.Env) {
		httpx.Fail(c, httpx.Forbidden("no access to environment "+in.Env))
		return
	}
	enc := cl.encKubeconfig
	if strings.TrimSpace(in.Kubeconfig) != "" && !strings.Contains(in.Kubeconfig, "***") {
		if err := prepareKubeconfig(&in); err != nil {
			httpx.Fail(c, err)
			return
		}
		var err error
		if enc, err = m.d.Crypto.Encrypt(in.Kubeconfig); err != nil {
			httpx.Fail(c, httpx.Internal(err))
			return
		}
	}
	if in.APIServer == "" {
		in.APIServer = cl.APIServer
	}
	ctx := c.Request.Context()
	upd, err := scanCluster(m.d.DB.QueryRow(ctx, `UPDATE cluster_clusters SET name=$2,env=$3,description=$4,api_server=$5,
		kubeconfig=$6,updated_at=now() WHERE id=$1 RETURNING `+clusterCols, cl.ID, in.Name, in.Env, in.Description, in.APIServer, enc))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	m.clients.drop(cl.ID)
	audit.SetDetail(c, gin.H{"name": in.Name, "env": in.Env, "kubeconfig_changed": enc != cl.encKubeconfig})
	if enc != cl.encKubeconfig {
		m.probe(ctx, upd)
	}
	httpx.OK(c, upd)
}

func (m *Module) deleteCluster(c *gin.Context) {
	cl, ok := m.clusterFor(c)
	if !ok {
		return
	}
	if _, err := m.d.DB.Exec(c.Request.Context(), `DELETE FROM cluster_clusters WHERE id=$1`, cl.ID); err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	m.clients.drop(cl.ID)
	httpx.OK(c, gin.H{"id": cl.ID})
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
	var in struct {
		Kubeconfig string `json:"kubeconfig" binding:"required"`
		Context    string `json:"context"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	kc, err := buildClients(in.Kubeconfig, in.Context)
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
	ClusterID int64           `json:"cluster_id"`
	Cluster   string          `json:"cluster"`
	Action    string          `json:"action"`
	Namespace string          `json:"namespace,omitempty"`
	Kind      string          `json:"kind,omitempty"`
	Name      string          `json:"name,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
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
		audit.SetAction(c, action)
		audit.SetDetail(c, pl)
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
func (m *Module) runChange(ctx context.Context, p *rbac.Principal, cl *Cluster, pl changePayload, reason string) (any, *approval.Ticket, error) {
	if m.d.Approval != nil && p != nil {
		title := fmt.Sprintf("[%s] %s %s", cl.Name, pl.Action, strings.Trim(strings.Join([]string{pl.Namespace, pl.Kind, pl.Name}, "/"), "/"))
		t, pending, err := m.d.Approval.Gate(ctx, p, approval.CreateInput{Type: changeType, Title: title, Env: cl.Env, Reason: reason, Payload: pl})
		if err != nil {
			return nil, nil, err
		}
		if pending {
			return nil, t, nil
		}
	}
	res, err := m.execute(ctx, cl, pl)
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
	if p := rbac.FromContext(ctx); p != nil && !p.CanEnv(cl.Env) {
		return nil, fmt.Errorf("applicant has no access to environment %s", cl.Env)
	}
	return m.execute(ctx, cl, pl)
}

func (m *Module) execute(ctx context.Context, cl *Cluster, pl changePayload) (any, error) {
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
