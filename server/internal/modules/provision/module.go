package provision

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

//go:embed migrations/*.sql
var migrations embed.FS

const approvalType = "cluster.provision"

type Module struct {
	d  *app.Deps
	wg sync.WaitGroup
}

func New() *Module                { return &Module{} }
func (*Module) Name() string      { return "provision" }
func (*Module) Migrations() fs.FS { sub, _ := fs.Sub(migrations, "migrations"); return sub }

func (m *Module) Register(r *gin.RouterGroup, d *app.Deps) {
	m.d = d
	g := r.Group("/provision")
	g.POST("/preflight", rbac.RequirePerm("cluster:manage", "host:exec"), m.check)
	g.POST("/jobs", rbac.RequirePerm("cluster:manage", "host:exec"), m.create)
	g.GET("/registry-mirror", rbac.RequirePerm("setting:read"), m.getRegistryMirror)
	g.PUT("/registry-mirror", rbac.RequirePerm("setting:manage"), m.putRegistryMirror)
	g.GET("/jobs", rbac.RequirePerm("cluster:read"), m.list)
	g.GET("/jobs/:id", rbac.RequirePerm("cluster:read"), m.get)
	g.POST("/jobs/:id/cancel", rbac.RequirePerm("cluster:manage", "host:exec"), m.cancel)
	if d.Approval != nil {
		d.Approval.MustRegister(approval.TypeSpec{Type: approvalType, Module: "provision",
			Description: "通过 SSH 安装新的 Kubernetes 集群", CreatePermission: "cluster:manage",
			Handler: m})
	}
}

func (m *Module) currentRegistryMirror(ctx context.Context) (RegistryMirror, error) {
	if m.d.Settings == nil {
		return normalizeRegistryMirror(RegistryMirror{})
	}
	var mirror RegistryMirror
	err := m.d.Settings.Get(ctx, registryMirrorSettingKey, &mirror)
	if errors.Is(err, httpx.ErrNotFound) {
		return normalizeRegistryMirror(RegistryMirror{})
	}
	if err != nil {
		return RegistryMirror{}, err
	}
	return normalizeRegistryMirror(mirror)
}

func (m *Module) getRegistryMirror(c *gin.Context) {
	mirror, err := m.currentRegistryMirror(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, mirror)
}

func (m *Module) putRegistryMirror(c *gin.Context) {
	var mirror RegistryMirror
	if !httpx.Bind(c, &mirror) {
		return
	}
	mirror, err := normalizeRegistryMirror(mirror)
	if err != nil {
		httpx.Fail(c, httpx.Validation(err.Error()))
		return
	}
	value, err := json.Marshal(mirror)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "settings", registryMirrorSettingKey)
	audit.SetDetail(c, gin.H{"provider": mirror.Provider})
	if m.d.Settings == nil {
		httpx.Fail(c, httpx.Conflict("settings service unavailable"))
		return
	}
	_, err = m.d.Settings.Put(c.Request.Context(), registryMirrorSettingKey, value, false,
		"镜像源用于平台新建 K3s 集群的 Docker Hub 镜像拉取", rbac.PrincipalFrom(c).UserID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, mirror)
}

func (m *Module) check(c *gin.Context) {
	var req Request
	if !httpx.Bind(c, &req) {
		return
	}
	mirror, err := m.currentRegistryMirror(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	req.RegistryMirror = mirror
	p, err := makePlan(&req, false)
	if err != nil {
		httpx.Fail(c, httpx.Validation(err.Error()))
		return
	}
	if !rbac.PrincipalFrom(c).CanEnv(req.Env) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Minute)
	defer cancel()
	for i, h := range req.Hosts {
		fp, _, err := preflight(ctx, h, i < p.Masters, true, req.CNI)
		if err != nil {
			httpx.Fail(c, httpx.Validation(err.Error()))
			return
		}
		p.Hosts[i].Fingerprint = fp
	}
	httpx.OK(c, p)
}

type Job struct {
	ID          int64           `json:"id"`
	Name        string          `json:"name"`
	Env         string          `json:"env"`
	Plan        json.RawMessage `json:"plan"`
	Status      string          `json:"status"`
	Logs        json.RawMessage `json:"logs"`
	Error       string          `json:"error"`
	ClusterID   *int64          `json:"cluster_id"`
	CreatedBy   int64           `json:"created_by"`
	CreatedName string          `json:"created_name"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	FinishedAt  *time.Time      `json:"finished_at"`
}

const jobCols = "id,name,env,plan,status,logs,error,cluster_id,created_by,created_name,created_at,updated_at,finished_at"

func scanJob(row pgx.Row) (*Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.Name, &j.Env, &j.Plan, &j.Status, &j.Logs, &j.Error, &j.ClusterID, &j.CreatedBy, &j.CreatedName, &j.CreatedAt, &j.UpdatedAt, &j.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = httpx.NotFound("deployment job not found")
	}
	return &j, err
}

func (m *Module) jobFor(c *gin.Context) (*Job, error) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		return nil, httpx.Validation("invalid job id")
	}
	j, err := scanJob(m.d.DB.QueryRow(c.Request.Context(), "SELECT "+jobCols+" FROM provision_jobs WHERE id=$1", id))
	if err == nil && !rbac.PrincipalFrom(c).CanEnv(j.Env) {
		return nil, httpx.Forbidden("environment not in your scope")
	}
	return j, err
}

func (m *Module) list(c *gin.Context) {
	p := rbac.PrincipalFrom(c)
	scope := p.EnvScope
	all := scope == nil
	for _, e := range scope {
		all = all || e == "*"
	}
	rows, err := m.d.DB.Query(c.Request.Context(), "SELECT "+jobCols+" FROM provision_jobs WHERE $1::boolean OR env=ANY($2::text[]) ORDER BY id DESC LIMIT 100", all, scope)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer rows.Close()
	items := []*Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		items = append(items, j)
	}
	if err := rows.Err(); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"items": items})
}

func (m *Module) get(c *gin.Context) {
	j, err := m.jobFor(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, j)
}

func (m *Module) create(c *gin.Context) {
	var req Request
	if !httpx.Bind(c, &req) {
		return
	}
	currentMirror, err := m.currentRegistryMirror(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	requestMirror, err := normalizeRegistryMirror(req.RegistryMirror)
	if err != nil {
		httpx.Fail(c, httpx.Validation(err.Error()))
		return
	}
	if requestMirror != currentMirror {
		httpx.Fail(c, httpx.Conflict("镜像源设置已变化，请重新执行连接预检"))
		return
	}
	req.RegistryMirror = currentMirror
	plan, err := makePlan(&req, true)
	if err != nil || !req.Confirmed {
		if err == nil {
			err = errors.New("confirm SSH fingerprints, role plan and installation impact")
		}
		httpx.Fail(c, httpx.Validation(err.Error()))
		return
	}
	p := rbac.PrincipalFrom(c)
	if !p.CanEnv(req.Env) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope"))
		return
	}
	if m.d.Approval == nil {
		httpx.Fail(c, httpx.Conflict("approval service unavailable"))
		return
	}
	data, _ := json.Marshal(req)
	encrypted, err := m.d.Crypto.EncryptBytes(data, []byte("provision:"+req.Name))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	planData, _ := json.Marshal(plan)
	ctx := c.Request.Context()
	tx, err := m.d.DB.Begin(ctx)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer tx.Rollback(ctx)
	var exists bool
	err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM cluster_clusters WHERE name=$1)", req.Name).Scan(&exists)
	if err != nil || exists {
		httpx.Fail(c, httpx.Conflict("cluster name already exists or cannot be checked"))
		return
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO provision_jobs(name,env,plan,request_enc,created_by,created_name)
		VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, req.Name, req.Env, planData, encrypted, p.UserID, p.Username).Scan(&id)
	if err == nil {
		for _, h := range req.Hosts {
			if _, err = tx.Exec(ctx, "INSERT INTO provision_host_locks(ip,job_id) VALUES($1,$2)", h.IP, id); err != nil {
				break
			}
		}
	}
	if err != nil {
		httpx.Fail(c, httpx.Conflict("cluster name or target hosts are already reserved by an active deployment"))
		return
	}
	if err = tx.Commit(ctx); err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "provision", fmt.Sprint(id))
	audit.SetDetail(c, gin.H{"name": req.Name, "hosts": len(req.Hosts), "masters": plan.Masters})
	t, pending, err := m.d.Approval.Gate(ctx, p, approval.CreateInput{Type: approvalType,
		Title: "安装 Kubernetes 集群 " + req.Name, Env: req.Env, Payload: gin.H{"job_id": id}})
	if err != nil {
		m.finish(id, "failed", "could not request environment approval", nil)
		httpx.Fail(c, err)
		return
	}
	if pending {
		approval.RespondPending(c, t)
		return
	}
	if err = m.enqueue(ctx, id, p, req.Env); err != nil {
		m.finish(id, "failed", "could not queue deployment", nil)
		httpx.Fail(c, err)
		return
	}
	httpx.Created(c, gin.H{"id": id, "status": "queued"})
}

func (m *Module) enqueue(ctx context.Context, id int64, p *rbac.Principal, env string) error {
	if !p.Can("cluster:manage") || !p.Can("host:exec") || !p.CanEnv(env) {
		return httpx.Forbidden("cluster provisioning permission denied")
	}
	tag, err := m.d.DB.Exec(ctx, `UPDATE provision_jobs SET status='queued',updated_at=now()
		WHERE id=$1 AND created_by=$2 AND env=$3 AND status='awaiting_approval' AND created_at > now()-interval '24 hours'`,
		id, p.UserID, env)
	if err == nil && tag.RowsAffected() == 0 {
		err = httpx.Conflict("deployment is cancelled, expired or does not belong to applicant")
	}
	return err
}

func (m *Module) OnApproved(ctx context.Context, t *approval.Ticket) (any, error) {
	var payload struct {
		JobID int64 `json:"job_id"`
	}
	if err := t.DecodePayload(&payload); err != nil {
		return nil, err
	}
	if m.d.RBAC == nil {
		return nil, httpx.Conflict("RBAC service unavailable")
	}
	_, perms, scope, err := m.d.RBAC.UserAccess(ctx, t.ApplicantID)
	if err != nil {
		return nil, err
	}
	p := &rbac.Principal{UserID: t.ApplicantID, Username: t.ApplicantName, Permissions: perms, EnvScope: scope}
	err = m.enqueue(ctx, payload.JobID, p, t.Env)
	return gin.H{"job_id": payload.JobID, "status": "queued"}, err
}

func (m *Module) OnRejected(ctx context.Context, t *approval.Ticket) error {
	var payload struct {
		JobID int64 `json:"job_id"`
	}
	if err := t.DecodePayload(&payload); err != nil {
		return err
	}
	_, err := m.d.DB.Exec(ctx, `UPDATE provision_jobs SET status='cancelled',request_enc='',finished_at=now(),updated_at=now()
		WHERE id=$1 AND created_by=$2 AND env=$3 AND status='awaiting_approval'`, payload.JobID, t.ApplicantID, t.Env)
	if err == nil {
		m.releaseLocks(ctx)
	}
	return err
}

func (m *Module) cancel(c *gin.Context) {
	j, err := m.jobFor(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	tag, err := m.d.DB.Exec(c.Request.Context(), `UPDATE provision_jobs SET
		status=CASE WHEN status='running' THEN 'cancelling' ELSE 'cancelled' END,
		request_enc=CASE WHEN status='running' THEN request_enc ELSE '' END,
		finished_at=CASE WHEN status='running' THEN NULL ELSE now() END, updated_at=now()
		WHERE id=$1 AND status IN ('awaiting_approval','queued','running')`, j.ID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Fail(c, httpx.Conflict("job is already terminal or cancelling"))
		return
	}
	m.releaseLocks(c.Request.Context())
	httpx.OK(c, gin.H{"id": j.ID, "cancel_requested": true})
}

func (m *Module) releaseLocks(ctx context.Context) {
	_, _ = m.d.DB.Exec(ctx, `DELETE FROM provision_host_locks l USING provision_jobs j
		WHERE l.job_id=j.id AND j.status IN ('succeeded','failed','cancelled')`)
}

func (m *Module) finish(id int64, status, message string, clusterID *int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = m.d.DB.Exec(ctx, `UPDATE provision_jobs SET status=$2,error=$3,cluster_id=$4,request_enc='',
		updated_at=now(),finished_at=now() WHERE id=$1 AND status NOT IN ('succeeded','failed','cancelled')`, id, status, message, clusterID)
	m.releaseLocks(ctx)
}

func (m *Module) log(id int64, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = m.d.DB.Exec(ctx, `UPDATE provision_jobs SET logs=logs||jsonb_build_array($2::text),updated_at=now() WHERE id=$1`, id, time.Now().UTC().Format(time.RFC3339)+" "+text)
}

func (m *Module) Start(ctx context.Context, _ *app.Deps) error {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Never replay installation after a lost worker lease.
				_, _ = m.d.DB.Exec(ctx, `UPDATE provision_jobs SET status='failed',error='worker stopped; inspect partially installed hosts before retrying',
					request_enc='',finished_at=now(),updated_at=now() WHERE status IN ('running','cancelling') AND updated_at<now()-interval '2 minutes'`)
				_, _ = m.d.DB.Exec(ctx, `UPDATE provision_jobs SET status='cancelled',request_enc='',finished_at=now(),updated_at=now(),
					error='approval or queue expired' WHERE status IN ('awaiting_approval','queued') AND created_at<now()-interval '24 hours'`)
				m.releaseLocks(ctx)
				var id int64
				var encrypted, name, by string
				err := m.d.DB.QueryRow(ctx, `UPDATE provision_jobs SET status='running',started_at=now(),updated_at=now()
					WHERE id=(SELECT id FROM provision_jobs WHERE status='queued' ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1)
					RETURNING id,request_enc,name,created_name`).Scan(&id, &encrypted, &name, &by)
				if err == nil {
					m.execute(ctx, id, encrypted, name, by)
				}
			}
		}
	}()
	return nil
}

func (m *Module) Stop(ctx context.Context) error {
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Module) execute(parent context.Context, id int64, encrypted, name, by string) {
	ctx, cancel := context.WithTimeout(parent, 45*time.Minute)
	defer cancel()
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				tag, err := m.d.DB.Exec(ctx, `UPDATE provision_jobs SET updated_at=now() WHERE id=$1 AND status='running'`, id)
				if err != nil || tag.RowsAffected() == 0 {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-monitorDone }()
	fail := func(message string) {
		state := "failed"
		var dbState string
		queryCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = m.d.DB.QueryRow(queryCtx, "SELECT status FROM provision_jobs WHERE id=$1", id).Scan(&dbState)
		if dbState == "cancelling" {
			state = "cancelled"
		}
		m.finish(id, state, message+"; inspect hosts for partial installation; no automatic uninstall was performed", nil)
	}
	data, err := m.d.Crypto.DecryptBytes(encrypted, []byte("provision:"+name))
	var req Request
	if err != nil || json.Unmarshal(data, &req) != nil {
		fail("cannot decrypt deployment request")
		return
	}
	plan, err := makePlan(&req, true)
	if err != nil {
		fail("invalid persisted deployment plan")
		return
	}
	var applicant int64
	if err = m.d.DB.QueryRow(ctx, "SELECT created_by FROM provision_jobs WHERE id=$1 AND status='running'", id).Scan(&applicant); err != nil || m.d.RBAC == nil {
		fail("deployment applicant is unavailable")
		return
	}
	_, perms, scope, err := m.d.RBAC.UserAccess(ctx, applicant)
	principal := &rbac.Principal{UserID: applicant, Username: by, Permissions: perms, EnvScope: scope}
	if err != nil || !principal.Can("cluster:manage") || !principal.Can("host:exec") || !principal.CanEnv(req.Env) {
		fail("deployment permissions or environment scope were revoked")
		return
	}
	knownHostKeys := make([]string, 0, len(req.Hosts))
	for i, h := range req.Hosts {
		m.log(id, "Preflight "+h.IP)
		_, key, err := preflight(ctx, h, i < plan.Masters, false, req.CNI)
		if err != nil {
			fail("preflight failed on " + h.IP)
			return
		}
		knownHostKeys = append(knownHostKeys, key)
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		fail("cannot generate cluster join token")
		return
	}
	token := hex.EncodeToString(tokenBytes)
	for i, h := range req.Hosts {
		m.log(id, "Install "+plan.Hosts[i].Role+" "+h.IP+" with embedded Ansible")
		stepCtx, stop := context.WithTimeout(ctx, 12*time.Minute)
		err := installWithAnsible(stepCtx, h, knownHostKeys[i], installScript(&req, plan, i, token))
		stop()
		if err != nil {
			if err.Error() == "embedded Ansible runtime unavailable" {
				fail("embedded Ansible runtime is unavailable; run the Matrix platform image")
			} else {
				fail("Ansible installation failed on " + h.IP)
			}
			return
		}
		m.log(id, "Ansible completed "+plan.Hosts[i].Role+" "+h.IP)
	}
	m.log(id, "Verify node readiness and API endpoint")
	verify := "set -eu\ni=0\nwhile [ \"$(/usr/local/bin/k3s kubectl get nodes --no-headers | wc -l)\" -lt " + strconv.Itoa(len(req.Hosts)) + " ]; do\n" +
		"i=$((i+1)); test \"$i\" -lt 60; sleep 5\ndone\n/usr/local/bin/k3s kubectl wait nodes --all --for=condition=Ready --timeout=300s >/dev/null\n" +
		"test \"$(/usr/local/bin/k3s kubectl get nodes --no-headers | wc -l)\" -eq " + strconv.Itoa(len(req.Hosts)) +
		"\ncat /etc/rancher/k3s/k3s.yaml\n"
	raw, _, _, err := runSSH(ctx, req.Hosts[0], verify, false)
	if err != nil {
		fail("node readiness check failed")
		return
	}
	kc, err := clientcmd.Load([]byte(raw))
	if err != nil {
		fail("cannot load installed kubeconfig")
		return
	}
	for _, c := range kc.Clusters {
		c.Server = plan.Endpoint
		c.InsecureSkipTLSVerify = false
	}
	kubeconfig, err := clientcmd.Write(*kc)
	if err != nil {
		fail("cannot prepare cluster access")
		return
	}
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		fail("invalid installed kubeconfig")
		return
	}
	config.Timeout = 15 * time.Second
	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		fail("cannot create cluster client")
		return
	}
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil || len(nodes.Items) != len(req.Hosts) {
		fail("platform cannot verify expected nodes through configured API endpoint")
		return
	}
	for _, node := range nodes.Items {
		ready := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = true
			}
		}
		if !ready {
			fail("a node is not Ready")
			return
		}
	}
	secret, err := m.d.Crypto.Encrypt(string(kubeconfig))
	if err != nil {
		fail("cannot encrypt installed cluster credentials")
		return
	}
	tx, err := m.d.DB.Begin(ctx)
	if err != nil {
		fail("cluster installed but registration transaction unavailable")
		return
	}
	defer tx.Rollback(context.Background())
	var state string
	err = tx.QueryRow(ctx, "SELECT status FROM provision_jobs WHERE id=$1 FOR UPDATE", id).Scan(&state)
	if err != nil || state != "running" {
		_ = tx.Rollback(ctx)
		fail("deployment cancelled before registration")
		return
	}
	var clusterID int64
	err = tx.QueryRow(ctx, `INSERT INTO cluster_clusters(name,env,description,api_server,kubeconfig,version,status,checked_at,created_by)
		VALUES($1,$2,'Installed by Matrix via SSH (K3s)',$3,$4,$5,'connected',now(),$6) RETURNING id`,
		req.Name, req.Env, plan.Endpoint, secret, req.Version, by).Scan(&clusterID)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE provision_jobs SET status='succeeded',error='',cluster_id=$2,request_enc='',
			logs=logs||jsonb_build_array('All nodes Ready; cluster registered'::text),updated_at=now(),finished_at=now() WHERE id=$1`, id, clusterID)
	}
	if err == nil {
		_, err = tx.Exec(ctx, "DELETE FROM provision_host_locks WHERE job_id=$1", id)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		fail("cluster installed but registration failed; recover kubeconfig from the first server")
		return
	}
}
