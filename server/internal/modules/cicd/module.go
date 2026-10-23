// Package cicd implements applications, pipelines executed as Kubernetes Jobs
// (git-clone initContainer + Kaniko build / test / deploy), releases with
// rollback, approval-gated deploys, Git webhooks and optional Jenkins stages.
//
// The module keeps its own "runner targets" (kubeconfig encrypted with
// Deps.Crypto) and does not depend on the cluster module.
package cicd

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/ai"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

//go:embed migrations/*.sql
var migFS embed.FS

const maxConcurrentRuns = 4

// Module is the cicd feature module.
type Module struct {
	d    *app.Deps
	db   *pgxpool.Pool
	log  *slog.Logger
	http *http.Client

	mu      sync.Mutex
	cancels map[int64]context.CancelFunc
	wake    chan struct{}
	wg      sync.WaitGroup

	// kube builds a clientset from kubeconfig bytes (overridden in tests).
	kube func(kubeconfig []byte) (kubernetes.Interface, error)
}

// New returns the cicd module.
func New() *Module {
	return &Module{
		cancels: map[int64]context.CancelFunc{},
		wake:    make(chan struct{}, 1),
		http:    &http.Client{Timeout: 60 * time.Second},
		kube:    defaultKube,
		log:     slog.Default(),
	}
}

func defaultKube(kc []byte) (kubernetes.Interface, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kc)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	cfg.Timeout = 0 // log streams are long-lived
	return kubernetes.NewForConfig(cfg)
}

func (m *Module) Name() string { return "cicd" }

func (m *Module) Migrations() fs.FS { sub, _ := fs.Sub(migFS, "migrations"); return sub }

func (m *Module) Permissions() []rbac.Permission {
	return []rbac.Permission{
		rbac.P("cicd", "cicd", "read", "查看应用、流水线、运行记录与日志"),
		rbac.P("cicd", "cicd", "manage", "管理应用、流水线、执行集群与 Jenkins"),
		rbac.P("cicd", "cicd", "run", "运行 / 取消流水线"),
		rbac.P("cicd", "cicd", "deploy", "部署与回滚应用"),
	}
}

// Register mounts routes and registers the approval type and AI tools.
func (m *Module) Register(r *gin.RouterGroup, d *app.Deps) {
	m.d, m.db = d, d.DB
	if d.Logger != nil {
		m.log = d.Logger
	}
	if err := d.Approval.Register(approval.TypeSpec{
		Type: "cicd.deploy", Module: "cicd", Description: "应用部署 / 回滚到受控环境",
		CreatePermission: "cicd:deploy", Timeout: 30 * time.Minute, Handler: &deployHandler{m},
	}); err != nil {
		m.log.Error("register approval type", "err", err)
	}
	m.registerTools(d.AITools)

	g := r.Group("/cicd")
	read, manage := rbac.RequirePerm("cicd:read"), rbac.RequirePerm("cicd:manage")
	run, deploy := rbac.RequirePerm("cicd:run"), rbac.RequirePerm("cicd:deploy")

	g.GET("/targets", read, m.listTargets)
	g.POST("/targets", manage, m.createTarget)
	g.DELETE("/targets/:id", manage, m.deleteRow("cicd_targets", "target"))

	g.GET("/jenkins", read, m.listJenkins)
	g.POST("/jenkins", manage, m.createJenkins)
	g.DELETE("/jenkins/:id", manage, m.deleteRow("cicd_jenkins", "jenkins"))

	g.GET("/applications", read, m.listApps)
	g.POST("/applications", manage, m.createApp)
	g.GET("/applications/:id", read, m.getApp)
	g.PUT("/applications/:id", manage, m.updateApp)
	g.DELETE("/applications/:id", manage, m.deleteRow("cicd_applications", "application"))
	g.POST("/applications/:id/webhook-secret", manage, m.rotateWebhookSecret)
	g.GET("/applications/:id/inventory", read, m.applicationInventory)
	g.GET("/applications/:id/releases", read, m.listReleases)
	g.POST("/applications/:id/deploy", deploy, m.deployHTTP)
	g.POST("/applications/:id/rollback", deploy, m.rollbackHTTP)

	g.GET("/pipelines", read, m.listPipelines)
	g.POST("/pipelines", manage, m.createPipeline)
	g.GET("/pipelines/:id", read, m.getPipeline)
	g.PUT("/pipelines/:id", manage, m.updatePipeline)
	g.DELETE("/pipelines/:id", manage, m.deleteRow("cicd_pipelines", "pipeline"))
	g.POST("/pipelines/:id/run", run, m.runPipelineHTTP)

	g.GET("/runs", read, m.listRuns)
	g.GET("/runs/:id", read, m.getRun)
	g.GET("/runs/:id/logs", read, m.runLogs)
	g.POST("/runs/:id/cancel", run, m.cancelRun)

	if d.Public != nil {
		d.Public.POST("/cicd/webhooks/:appId", m.webhook)
	}
}

// Start launches the run dispatcher.
func (m *Module) Start(ctx context.Context, d *app.Deps) error {
	// Runs left running by a previous process cannot be resumed (their log
	// streams and approval waits are gone): mark them failed.
	_, err := m.db.Exec(ctx, `UPDATE cicd_runs SET status='failed', error='server restarted during run', finished_at=now()
		WHERE status IN ('running','cancelling')`)
	if err != nil {
		return err
	}
	m.wg.Add(1)
	go func() { defer m.wg.Done(); m.dispatch(ctx) }()
	return nil
}

// Stop waits for in-flight runs to observe cancellation.
func (m *Module) Stop(ctx context.Context) error {
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}

// ---------------------------------------------------------------- types

// EnvTarget is a deploy destination of an application.
type EnvTarget struct {
	Name              string    `json:"name"` // environment name (dev/test/prod), used for RBAC env scope and approval policy
	TargetID          int64     `json:"target_id"`
	Namespace         string    `json:"namespace"`
	Deployment        string    `json:"deployment,omitempty"`
	Container         string    `json:"container,omitempty"`
	Helm              *HelmSpec `json:"helm,omitempty"`
	RolloutTimeoutSec int       `json:"rollout_timeout_sec,omitempty"`
}

// HelmSpec deploys via `helm upgrade --install` run as a Job on the target cluster.
type HelmSpec struct {
	Release      string   `json:"release"`
	Chart        string   `json:"chart"`             // e.g. oci://registry/charts/app or bitnami/nginx
	Repo         string   `json:"repo,omitempty"`    // --repo URL
	Version      string   `json:"version,omitempty"` // --version
	ImageRepoKey string   `json:"image_repo_key,omitempty"`
	ImageTagKey  string   `json:"image_tag_key,omitempty"`
	Set          []string `json:"set,omitempty"` // extra --set k=v
}

// Application is a deployable service built from a Git repository.
type Application struct {
	ID            int64       `json:"id"`
	Name          string      `json:"name" binding:"required,max=64"`
	RepoURL       string      `json:"repo_url" binding:"required"`
	Branch        string      `json:"branch"`
	Dockerfile    string      `json:"dockerfile"`
	ContextDir    string      `json:"context_dir"`
	ImageRepo     string      `json:"image_repo"`
	GitUsername   string      `json:"git_username"`
	GitToken      string      `json:"git_token,omitempty"` // input; output masked
	WebhookSecret string      `json:"webhook_secret,omitempty"`
	Envs          []EnvTarget `json:"envs"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`

	gitTokenEnc, webhookEnc string
}

func (a *Application) env(name string) (*EnvTarget, error) {
	for i := range a.Envs {
		if a.Envs[i].Name == name {
			return &a.Envs[i], nil
		}
	}
	return nil, httpx.Validation("application has no env " + name)
}

// Stage is one step of a pipeline.
type Stage struct {
	Name           string            `json:"name"`
	Type           string            `json:"type"` // build | test | script | deploy | jenkins
	Image          string            `json:"image,omitempty"`
	Commands       []string          `json:"commands,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	RegistrySecret string            `json:"registry_secret,omitempty"` // dockerconfigjson Secret in runner namespace (build)
	ExtraArgs      []string          `json:"extra_args,omitempty"`      // extra Kaniko args (build)
	DeployEnv      string            `json:"deploy_env,omitempty"`      // deploy
	JenkinsID      int64             `json:"jenkins_id,omitempty"`      // jenkins
	JenkinsJob     string            `json:"jenkins_job,omitempty"`
	Params         map[string]string `json:"params,omitempty"`
	TimeoutSec     int               `json:"timeout_sec,omitempty"`
}

// Pipeline is an ordered list of stages for an application.
type Pipeline struct {
	ID             int64     `json:"id"`
	AppID          int64     `json:"app_id" binding:"required"`
	Name           string    `json:"name" binding:"required,max=64"`
	RunnerTargetID int64     `json:"runner_target_id" binding:"required"`
	Stages         []Stage   `json:"stages" binding:"required"`
	OnPush         bool      `json:"on_push"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func validateStages(st []Stage, a *Application) error {
	if len(st) == 0 {
		return httpx.Validation("pipeline needs at least one stage")
	}
	for i, s := range st {
		if s.Name == "" || len(s.Name) > 40 || !nameRe.MatchString(s.Name) {
			return httpx.Validation(fmt.Sprintf("stage %d: name must be a DNS label (<=40)", i))
		}
		switch s.Type {
		case "build":
			if a.ImageRepo == "" && s.Image == "" {
				return httpx.Validation(fmt.Sprintf("stage %s: application image_repo required for build", s.Name))
			}
		case "test", "script":
			if s.Image == "" || len(s.Commands) == 0 {
				return httpx.Validation(fmt.Sprintf("stage %s: image and commands required", s.Name))
			}
		case "deploy":
			if _, err := a.env(s.DeployEnv); err != nil {
				return httpx.Validation(fmt.Sprintf("stage %s: unknown deploy_env %q", s.Name, s.DeployEnv))
			}
		case "jenkins":
			if s.JenkinsID == 0 || s.JenkinsJob == "" {
				return httpx.Validation(fmt.Sprintf("stage %s: jenkins_id and jenkins_job required", s.Name))
			}
		default:
			return httpx.Validation(fmt.Sprintf("stage %s: unknown type %q", s.Name, s.Type))
		}
	}
	return nil
}

func validateEnvs(envs []EnvTarget) error {
	seen := map[string]bool{}
	for _, e := range envs {
		if e.Name == "" || seen[e.Name] || e.TargetID == 0 || e.Namespace == "" {
			return httpx.Validation("env: unique name, target_id and namespace are required")
		}
		seen[e.Name] = true
		if e.Helm == nil && (e.Deployment == "" || e.Container == "") {
			return httpx.Validation("env " + e.Name + ": deployment+container or helm required")
		}
		if e.Helm != nil && (e.Helm.Release == "" || e.Helm.Chart == "") {
			return httpx.Validation("env " + e.Name + ": helm.release and helm.chart required")
		}
	}
	return nil
}

func randSecret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.ErrNotFound
	}
	return err
}

// ---------------------------------------------------------------- targets

type targetIn struct {
	Name       string `json:"name" binding:"required,max=64"`
	Kubeconfig string `json:"kubeconfig" binding:"required"`
	Namespace  string `json:"namespace"`
}

func (m *Module) listTargets(c *gin.Context) {
	rows, err := m.db.Query(c, `SELECT id,name,namespace,kubeconfig_enc,created_at FROM cicd_targets ORDER BY id`)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer rows.Close()
	out := []gin.H{}
	for rows.Next() {
		var id int64
		var name, ns, enc string
		var at time.Time
		if err := rows.Scan(&id, &name, &ns, &enc, &at); err != nil {
			httpx.Fail(c, err)
			return
		}
		out = append(out, gin.H{"id": id, "name": name, "namespace": ns, "kubeconfig": "********", "created_at": at})
	}
	httpx.OK(c, out)
}

func (m *Module) createTarget(c *gin.Context) {
	var in targetIn
	if !httpx.Bind(c, &in) {
		return
	}
	if in.Namespace == "" {
		in.Namespace = "matrix-cicd"
	}
	if _, err := clientcmd.RESTConfigFromKubeConfig([]byte(in.Kubeconfig)); err != nil {
		httpx.Fail(c, httpx.Validation("invalid kubeconfig: "+err.Error()))
		return
	}
	enc, err := m.d.Crypto.Encrypt(in.Kubeconfig)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	var id int64
	if err := m.db.QueryRow(c, `INSERT INTO cicd_targets(name,kubeconfig_enc,namespace) VALUES($1,$2,$3) RETURNING id`,
		in.Name, enc, in.Namespace).Scan(&id); err != nil {
		httpx.Fail(c, httpx.Conflict("target name exists").Wrap(err))
		return
	}
	audit.SetResource(c, "cicd_target", fmt.Sprint(id))
	httpx.Created(c, gin.H{"id": id, "name": in.Name, "namespace": in.Namespace})
}

// targetClient returns a clientset and runner namespace for target id.
func (m *Module) targetClient(ctx context.Context, id int64) (kubernetes.Interface, string, []byte, error) {
	var enc, ns string
	if err := m.db.QueryRow(ctx, `SELECT kubeconfig_enc, namespace FROM cicd_targets WHERE id=$1`, id).Scan(&enc, &ns); err != nil {
		return nil, "", nil, fmt.Errorf("target %d: %w", id, notFound(err))
	}
	kc, err := m.d.Crypto.Decrypt(enc)
	if err != nil {
		return nil, "", nil, err
	}
	cs, err := m.kube([]byte(kc))
	return cs, ns, []byte(kc), err
}

func (m *Module) deleteRow(table, res string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		tag, err := m.db.Exec(c, "DELETE FROM "+table+" WHERE id=$1", id)
		if err != nil {
			httpx.Fail(c, httpx.Conflict(res+" is in use").Wrap(err))
			return
		}
		if tag.RowsAffected() == 0 {
			httpx.Fail(c, httpx.ErrNotFound)
			return
		}
		audit.SetResource(c, "cicd_"+res, fmt.Sprint(id))
		httpx.OK(c, gin.H{"deleted": id})
	}
}

// ---------------------------------------------------------------- jenkins

type jenkinsIn struct {
	Name     string `json:"name" binding:"required,max=64"`
	URL      string `json:"url" binding:"required,url"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

func (m *Module) listJenkins(c *gin.Context) {
	rows, err := m.db.Query(c, `SELECT id,name,url,username,created_at FROM cicd_jenkins ORDER BY id`)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer rows.Close()
	out := []gin.H{}
	for rows.Next() {
		var id int64
		var n, u, user string
		var at time.Time
		if err := rows.Scan(&id, &n, &u, &user, &at); err != nil {
			httpx.Fail(c, err)
			return
		}
		out = append(out, gin.H{"id": id, "name": n, "url": u, "username": user, "token": "********", "created_at": at})
	}
	httpx.OK(c, out)
}

func (m *Module) createJenkins(c *gin.Context) {
	var in jenkinsIn
	if !httpx.Bind(c, &in) {
		return
	}
	enc := ""
	if in.Token != "" {
		var err error
		if enc, err = m.d.Crypto.Encrypt(in.Token); err != nil {
			httpx.Fail(c, err)
			return
		}
	}
	var id int64
	if err := m.db.QueryRow(c, `INSERT INTO cicd_jenkins(name,url,username,token_enc) VALUES($1,$2,$3,$4) RETURNING id`,
		in.Name, strings.TrimRight(in.URL, "/"), in.Username, enc).Scan(&id); err != nil {
		httpx.Fail(c, httpx.Conflict("jenkins name exists").Wrap(err))
		return
	}
	audit.SetResource(c, "cicd_jenkins", fmt.Sprint(id))
	httpx.Created(c, gin.H{"id": id, "name": in.Name, "url": in.URL, "username": in.Username, "token": crypto.Mask(in.Token)})
}

func (m *Module) jenkinsClient(ctx context.Context, id int64) (*jenkins, error) {
	var u, user, enc string
	if err := m.db.QueryRow(ctx, `SELECT url,username,token_enc FROM cicd_jenkins WHERE id=$1`, id).Scan(&u, &user, &enc); err != nil {
		return nil, fmt.Errorf("jenkins %d: %w", id, notFound(err))
	}
	tok := ""
	if enc != "" {
		var err error
		if tok, err = m.d.Crypto.Decrypt(enc); err != nil {
			return nil, err
		}
	}
	return newJenkins(u, user, tok), nil
}

// ---------------------------------------------------------------- applications

const appCols = `id,name,repo_url,branch,dockerfile,context_dir,image_repo,git_username,git_token_enc,webhook_secret_enc,envs,created_at,updated_at`

func scanApp(row pgx.Row) (*Application, error) {
	var a Application
	var envs []byte
	if err := row.Scan(&a.ID, &a.Name, &a.RepoURL, &a.Branch, &a.Dockerfile, &a.ContextDir, &a.ImageRepo, &a.GitUsername,
		&a.gitTokenEnc, &a.webhookEnc, &envs, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, notFound(err)
	}
	if err := json.Unmarshal(envs, &a.Envs); err != nil {
		return nil, err
	}
	return &a, nil
}

func (m *Module) loadApp(ctx context.Context, id int64) (*Application, error) {
	return scanApp(m.db.QueryRow(ctx, `SELECT `+appCols+` FROM cicd_applications WHERE id=$1`, id))
}

func (a *Application) public() *Application {
	cp := *a
	cp.GitToken, cp.WebhookSecret = "", ""
	if a.gitTokenEnc != "" {
		cp.GitToken = "********"
	}
	if cp.Envs == nil {
		cp.Envs = []EnvTarget{}
	}
	return &cp
}

func (a *Application) defaults() {
	if a.Branch == "" {
		a.Branch = "main"
	}
	if a.Dockerfile == "" {
		a.Dockerfile = "Dockerfile"
	}
	if a.ContextDir == "" {
		a.ContextDir = "."
	}
	if a.Envs == nil {
		a.Envs = []EnvTarget{}
	}
}

func (m *Module) listApps(c *gin.Context) {
	p := httpx.ParsePage(c)
	var total int64
	if err := m.db.QueryRow(c, `SELECT count(*) FROM cicd_applications`).Scan(&total); err != nil {
		httpx.Fail(c, err)
		return
	}
	rows, err := m.db.Query(c, `SELECT `+appCols+` FROM cicd_applications ORDER BY id DESC LIMIT $1 OFFSET $2`, p.Limit(), p.Offset())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer rows.Close()
	items := []*Application{}
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		items = append(items, a.public())
	}
	httpx.OK(c, httpx.NewPageResult(items, total, p))
}

func (m *Module) getApp(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	a, err := m.loadApp(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, a.public())
}

func (m *Module) createApp(c *gin.Context) {
	var in Application
	if !httpx.Bind(c, &in) {
		return
	}
	in.defaults()
	if err := validateEnvs(in.Envs); err != nil {
		httpx.Fail(c, err)
		return
	}
	secret := randSecret()
	wenc, err := m.d.Crypto.Encrypt(secret)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	tenc := ""
	if in.GitToken != "" {
		if tenc, err = m.d.Crypto.Encrypt(in.GitToken); err != nil {
			httpx.Fail(c, err)
			return
		}
	}
	envs, _ := json.Marshal(in.Envs)
	a, err := scanApp(m.db.QueryRow(c, `INSERT INTO cicd_applications(name,repo_url,branch,dockerfile,context_dir,image_repo,git_username,git_token_enc,webhook_secret_enc,envs)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING `+appCols,
		in.Name, in.RepoURL, in.Branch, in.Dockerfile, in.ContextDir, in.ImageRepo, in.GitUsername, tenc, wenc, envs))
	if err != nil {
		httpx.Fail(c, httpx.Conflict("application name exists").Wrap(err))
		return
	}
	audit.SetResource(c, "cicd_application", fmt.Sprint(a.ID))
	out := a.public()
	out.WebhookSecret = secret // shown once
	httpx.Created(c, out)
}

func (m *Module) updateApp(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	cur, err := m.loadApp(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	var in Application
	if !httpx.Bind(c, &in) {
		return
	}
	in.defaults()
	if err := validateEnvs(in.Envs); err != nil {
		httpx.Fail(c, err)
		return
	}
	tenc := cur.gitTokenEnc
	if in.GitToken != "" && in.GitToken != "********" {
		if tenc, err = m.d.Crypto.Encrypt(in.GitToken); err != nil {
			httpx.Fail(c, err)
			return
		}
	}
	envs, _ := json.Marshal(in.Envs)
	a, err := scanApp(m.db.QueryRow(c, `UPDATE cicd_applications SET name=$2,repo_url=$3,branch=$4,dockerfile=$5,context_dir=$6,image_repo=$7,
		git_username=$8,git_token_enc=$9,envs=$10,updated_at=now() WHERE id=$1 RETURNING `+appCols,
		id, in.Name, in.RepoURL, in.Branch, in.Dockerfile, in.ContextDir, in.ImageRepo, in.GitUsername, tenc, envs))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "cicd_application", fmt.Sprint(id))
	httpx.OK(c, a.public())
}

func (m *Module) rotateWebhookSecret(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	secret := randSecret()
	enc, err := m.d.Crypto.Encrypt(secret)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	tag, err := m.db.Exec(c, `UPDATE cicd_applications SET webhook_secret_enc=$2, updated_at=now() WHERE id=$1`, id, enc)
	if err != nil || tag.RowsAffected() == 0 {
		httpx.Fail(c, httpx.ErrNotFound)
		return
	}
	audit.SetResource(c, "cicd_application", fmt.Sprint(id))
	httpx.OK(c, gin.H{"webhook_secret": secret, "url": "/api/v1/cicd/webhooks/" + fmt.Sprint(id)})
}

// ---------------------------------------------------------------- pipelines

const pipeCols = `id,app_id,name,runner_target_id,stages,on_push,created_at,updated_at`

func scanPipe(row pgx.Row) (*Pipeline, error) {
	var p Pipeline
	var st []byte
	if err := row.Scan(&p.ID, &p.AppID, &p.Name, &p.RunnerTargetID, &st, &p.OnPush, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, notFound(err)
	}
	return &p, json.Unmarshal(st, &p.Stages)
}

func (m *Module) loadPipe(ctx context.Context, id int64) (*Pipeline, error) {
	return scanPipe(m.db.QueryRow(ctx, `SELECT `+pipeCols+` FROM cicd_pipelines WHERE id=$1`, id))
}

func (m *Module) listPipelines(c *gin.Context) {
	q := `SELECT ` + pipeCols + ` FROM cicd_pipelines`
	args := []any{}
	if v := c.Query("app_id"); v != "" {
		q += ` WHERE app_id=$1`
		args = append(args, v)
	}
	rows, err := m.db.Query(c, q+` ORDER BY id DESC LIMIT 500`, args...)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer rows.Close()
	out := []*Pipeline{}
	for rows.Next() {
		p, err := scanPipe(rows)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		out = append(out, p)
	}
	httpx.OK(c, out)
}

func (m *Module) getPipeline(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	p, err := m.loadPipe(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, p)
}

func (m *Module) savePipeline(c *gin.Context, id int64) {
	var in Pipeline
	if !httpx.Bind(c, &in) {
		return
	}
	a, err := m.loadApp(c, in.AppID)
	if err != nil {
		httpx.Fail(c, httpx.Validation("unknown app_id"))
		return
	}
	if err := validateStages(in.Stages, a); err != nil {
		httpx.Fail(c, err)
		return
	}
	st, _ := json.Marshal(in.Stages)
	var p *Pipeline
	if id == 0 {
		p, err = scanPipe(m.db.QueryRow(c, `INSERT INTO cicd_pipelines(app_id,name,runner_target_id,stages,on_push) VALUES($1,$2,$3,$4,$5) RETURNING `+pipeCols,
			in.AppID, in.Name, in.RunnerTargetID, st, in.OnPush))
	} else {
		p, err = scanPipe(m.db.QueryRow(c, `UPDATE cicd_pipelines SET app_id=$2,name=$3,runner_target_id=$4,stages=$5,on_push=$6,updated_at=now() WHERE id=$1 RETURNING `+pipeCols,
			id, in.AppID, in.Name, in.RunnerTargetID, st, in.OnPush))
	}
	if err != nil {
		if !errors.Is(err, httpx.ErrNotFound) {
			err = httpx.Conflict("pipeline name exists or runner_target_id invalid").Wrap(err)
		}
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "cicd_pipeline", fmt.Sprint(p.ID))
	if id == 0 {
		httpx.Created(c, p)
	} else {
		httpx.OK(c, p)
	}
}

func (m *Module) createPipeline(c *gin.Context) { m.savePipeline(c, 0) }

func (m *Module) updatePipeline(c *gin.Context) {
	if id, ok := httpx.ParamID(c, "id"); ok {
		m.savePipeline(c, id)
	}
}

// ---------------------------------------------------------------- AI tools

func (m *Module) registerTools(reg *ai.ToolRegistry) {
	if reg == nil {
		return
	}
	_ = reg.Register(ai.Tool{
		Name: "cicd_list_runs", Module: "cicd", Permission: "cicd:read",
		Description: "列出最近的流水线运行记录（可按 pipeline_id / app_id / status 过滤）",
		Parameters: json.RawMessage(`{"type":"object","properties":{"pipeline_id":{"type":"integer"},"app_id":{"type":"integer"},
			"status":{"type":"string","enum":["queued","running","succeeded","failed","cancelled"]},"limit":{"type":"integer","maximum":50}}}`),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				PipelineID, AppID int64
				Status            string
				Limit             int
			}
			var raw struct {
				PipelineID int64  `json:"pipeline_id"`
				AppID      int64  `json:"app_id"`
				Status     string `json:"status"`
				Limit      int    `json:"limit"`
			}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &raw); err != nil {
					return nil, err
				}
			}
			a.PipelineID, a.AppID, a.Status, a.Limit = raw.PipelineID, raw.AppID, raw.Status, raw.Limit
			if a.Limit <= 0 || a.Limit > 50 {
				a.Limit = 20
			}
			runs, _, err := m.queryRuns(ctx, a.PipelineID, a.AppID, a.Status, a.Limit, 0)
			return runs, err
		},
	})
	_ = reg.Register(ai.Tool{
		Name: "cicd_run_pipeline", Module: "cicd", Permission: "cicd:run", RequiresConfirmation: true,
		Description: "触发一次流水线运行；包含部署阶段且目标环境需要审批时，运行会在部署阶段等待审批",
		Parameters: json.RawMessage(`{"type":"object","required":["pipeline_id"],"properties":{"pipeline_id":{"type":"integer"},
			"branch":{"type":"string"},"commit":{"type":"string"}}}`),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var in runIn
			var raw struct {
				PipelineID int64 `json:"pipeline_id"`
				runIn
			}
			if err := json.Unmarshal(args, &raw); err != nil || raw.PipelineID == 0 {
				return nil, httpx.Validation("pipeline_id required")
			}
			in = raw.runIn
			p := rbac.FromContext(ctx)
			if p == nil {
				return nil, httpx.Unauthorized("no principal")
			}
			return m.startRun(ctx, p, raw.PipelineID, in, "ai")
		},
	})
}
