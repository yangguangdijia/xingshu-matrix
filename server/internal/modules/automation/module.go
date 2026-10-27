package automation

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

//go:embed migrations/*.sql
var migFS embed.FS

const ansibleApprovalType = "automation.ansible"

type Module struct {
	d       *app.Deps
	mu      sync.Mutex
	cancels map[int64]context.CancelFunc
	wake    chan struct{}
	wg      sync.WaitGroup
}

func New() *Module {
	return &Module{cancels: map[int64]context.CancelFunc{}, wake: make(chan struct{}, 1)}
}
func (m *Module) Name() string      { return "automation" }
func (m *Module) Migrations() fs.FS { sub, _ := fs.Sub(migFS, "migrations"); return sub }
func (m *Module) Permissions() []rbac.Permission {
	return []rbac.Permission{
		rbac.P("automation", "automation", "read", "查看编排、版本和执行记录"),
		rbac.P("automation", "automation", "manage", "管理编排定义和版本"),
		rbac.P("automation", "automation", "run", "触发、重试和取消编排执行"),
	}
}
func (m *Module) Register(r *gin.RouterGroup, d *app.Deps) {
	m.d = d
	if d.Approval != nil {
		d.Approval.MustRegister(approval.TypeSpec{
			Type: ansibleApprovalType, Module: "automation", Description: "在受控主机上执行 Ansible 自动化",
			CreatePermission: "automation:run", Timeout: time.Minute, Handler: m,
		})
		d.Approval.MustRegister(approval.TypeSpec{
			Type: automationConfigApprovalType, Module: "automation", Description: "修改自动化流程配置",
			CreatePermission: "automation:manage", Timeout: 30 * time.Minute, Handler: &automationConfigHandler{m},
		})
	}
	g := r.Group("/automation")
	g.GET("/workflows", rbac.RequirePerm("automation:read"), m.list)
	g.POST("/workflows", rbac.RequirePerm("automation:manage"), m.create)
	g.GET("/workflows/:id", rbac.RequirePerm("automation:read"), m.get)
	g.POST("/workflows/:id/versions", rbac.RequirePerm("automation:manage"), m.version)
	g.POST("/workflows/:id/trigger", rbac.RequirePerm("automation:run"), m.trigger)
	g.POST("/ansible/preflight", rbac.RequirePerm("automation:run", "host:exec"), m.ansiblePreflight)
	g.GET("/ansible/runtime", rbac.RequirePerm("automation:read"), m.ansibleRuntime)
	g.GET("/runs", rbac.RequirePerm("automation:read"), m.runs)
	g.GET("/runs/:id", rbac.RequirePerm("automation:read"), m.runDetail)
	g.POST("/runs/:id/retry", rbac.RequirePerm("automation:run"), m.retry)
	g.POST("/runs/:id/cancel", rbac.RequirePerm("automation:run"), m.cancel)
}

type workflowInput struct {
	Name        string          `json:"name" binding:"required,max=120"`
	Description string          `json:"description"`
	Trigger     string          `json:"trigger"`
	Steps       json.RawMessage `json:"steps"`
	Playbook    string          `json:"playbook"`
}

func (m *Module) create(c *gin.Context) {
	var in workflowInput
	if c.ShouldBindJSON(&in) != nil || strings.TrimSpace(in.Name) == "" {
		httpx.Fail(c, httpx.Validation("name is required"))
		return
	}
	if len(in.Steps) == 0 && strings.TrimSpace(in.Playbook) == "" {
		httpx.Fail(c, httpx.Validation("steps or playbook are required"))
		return
	}
	if len(in.Steps) == 0 {
		in.Steps = json.RawMessage(`[]`)
	}
	if _, err := parseSteps(in.Steps); err != nil {
		httpx.Fail(c, httpx.Validation("invalid workflow steps"))
		return
	}
	if in.Playbook != "" {
		if err := validatePlaybook(in.Playbook); err != nil {
			httpx.Fail(c, httpx.Validation("invalid Ansible playbook"))
			return
		}
	}
	p := rbac.PrincipalFrom(c)
	if p == nil {
		httpx.Fail(c, httpx.Unauthorized("authentication required"))
		return
	}
	change := automationConfigMutation{
		Kind: "workflow.create", Name: in.Name, Description: in.Description, Trigger: in.Trigger,
		Steps: in.Steps, Playbook: in.Playbook, CreatedBy: p.UserID,
	}
	if err := validateAutomationConfigMutation(change); err != nil {
		httpx.Fail(c, httpx.Validation("workflow configuration contains unsupported or inline secret values"))
		return
	}
	if m.gateConfigMutation(c, change) {
		return
	}
	result, err := m.applyConfigMutation(c, change)
	if err != nil {
		httpx.Fail(c, httpx.Conflict("workflow configuration could not be applied"))
		return
	}
	httpx.Created(c, result)
}
func (m *Module) list(c *gin.Context) {
	limit, offset := page(c)
	rows, err := m.d.DB.Query(c, `SELECT id,name,description,trigger,status,current_version,created_at,updated_at FROM automation_workflows ORDER BY id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	defer rows.Close()
	items := []gin.H{}
	for rows.Next() {
		var id int64
		var n, d, t, s string
		var v int
		var a, u time.Time
		if err := rows.Scan(&id, &n, &d, &t, &s, &v, &a, &u); err != nil {
			httpx.Fail(c, httpx.Internal(err))
			return
		}
		items = append(items, gin.H{"id": id, "name": redactAutomationText(n), "description": redactAutomationText(d),
			"trigger": redactAutomationText(t), "status": s, "current_version": v, "created_at": a, "updated_at": u})
	}
	httpx.OK(c, gin.H{"items": items, "page_size": limit, "offset": offset})
}
func (m *Module) get(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var n, d, t, s string
	var v int
	var created, updated time.Time
	var steps json.RawMessage
	var playbook string
	err := m.d.DB.QueryRow(c, `SELECT w.name,w.description,w.trigger,w.status,w.current_version,w.created_at,w.updated_at,
		COALESCE(v.steps,'[]'::jsonb),COALESCE(v.playbook,'')
		FROM automation_workflows w LEFT JOIN automation_workflow_versions v
		ON v.workflow_id=w.id AND v.version=w.current_version WHERE w.id=$1`, id).
		Scan(&n, &d, &t, &s, &v, &created, &updated, &steps, &playbook)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Fail(c, httpx.NotFound("workflow not found"))
		return
	}
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	httpx.OK(c, gin.H{"id": id, "name": redactAutomationText(n), "description": redactAutomationText(d),
		"trigger": redactAutomationText(t), "status": s, "current_version": v,
		"steps": redactAutomationJSON(steps), "playbook": redactAutomationPlaybook(playbook),
		"created_at": created, "updated_at": updated})
}
func (m *Module) version(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in struct {
		Steps    json.RawMessage `json:"steps"`
		Playbook string          `json:"playbook"`
	}
	if c.ShouldBindJSON(&in) != nil || (len(in.Steps) == 0 && in.Playbook == "") {
		httpx.Fail(c, httpx.Validation("steps or playbook are required"))
		return
	}
	if len(in.Steps) == 0 {
		in.Steps = json.RawMessage(`[]`)
	}
	if _, err := parseSteps(in.Steps); err != nil {
		httpx.Fail(c, httpx.Validation("invalid workflow steps"))
		return
	}
	if in.Playbook != "" {
		if err := validatePlaybook(in.Playbook); err != nil {
			httpx.Fail(c, httpx.Validation("invalid Ansible playbook"))
			return
		}
	}
	p := rbac.PrincipalFrom(c)
	if p == nil {
		httpx.Fail(c, httpx.Unauthorized("authentication required"))
		return
	}
	var exists bool
	if err := m.d.DB.QueryRow(c, `SELECT EXISTS(SELECT 1 FROM automation_workflows WHERE id=$1)`, id).Scan(&exists); err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	if !exists {
		httpx.Fail(c, httpx.NotFound("workflow not found"))
		return
	}
	change := automationConfigMutation{
		Kind: "workflow.version", WorkflowID: id, Steps: in.Steps, Playbook: in.Playbook, CreatedBy: p.UserID,
	}
	if err := validateAutomationConfigMutation(change); err != nil {
		httpx.Fail(c, httpx.Validation("workflow configuration contains unsupported or inline secret values"))
		return
	}
	if m.gateConfigMutation(c, change) {
		return
	}
	result, err := m.applyConfigMutation(c, change)
	if err != nil {
		httpx.Fail(c, httpx.Conflict("workflow configuration could not be applied"))
		return
	}
	httpx.Created(c, result)
}

type triggerInput struct {
	Env            string `json:"env"`
	PreflightToken string `json:"preflight_token"`
}

func (m *Module) trigger(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var v int
	var rawSteps json.RawMessage
	var playbook string
	err := m.d.DB.QueryRow(c, `SELECT w.current_version,v.steps,v.playbook
		FROM automation_workflows w JOIN automation_workflow_versions v
		ON v.workflow_id=w.id AND v.version=w.current_version
		WHERE w.id=$1 AND w.status='active'`, id).Scan(&v, &rawSteps, &playbook)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Fail(c, httpx.NotFound("active workflow not found"))
		return
	}
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	steps, err := parseSteps(rawSteps)
	if err != nil {
		httpx.Fail(c, httpx.Conflict("workflow version contains unsupported steps"))
		return
	}
	var in triggerInput
	if c.ShouldBindJSON(&in) != nil {
		httpx.Fail(c, httpx.Validation("invalid automation run input"))
		return
	}
	p := rbac.PrincipalFrom(c)
	useAnsible := hasAnsibleDefinition(steps, playbook)
	status := "queued"
	var tokenHash string
	var runPayload ansibleRunPayload
	if useAnsible {
		if m.d.Crypto == nil {
			httpx.Fail(c, httpx.Conflict("credential encryption service unavailable"))
			return
		}
		if !p.Can("host:exec") {
			httpx.Fail(c, httpx.Forbidden("permission denied: host:exec"))
			return
		}
		in.Env = strings.TrimSpace(in.Env)
		if !envNameRE.MatchString(in.Env) || !p.CanEnv(in.Env) {
			httpx.Fail(c, httpx.Forbidden("a valid environment in your scope is required for Ansible runs"))
			return
		}
		tokenHash, err = hashPreflightToken(in.PreflightToken)
		if err != nil {
			httpx.Fail(c, httpx.Validation("run SSH preflight and confirm the host keys before execution"))
			return
		}
		if m.d.Approval == nil {
			httpx.Fail(c, httpx.Conflict("approval service unavailable"))
			return
		}
		status = "awaiting_approval"
	} else if in.Env != "" || in.PreflightToken != "" {
		httpx.Fail(c, httpx.Validation("this workflow has no Ansible steps"))
		return
	}

	ctx := c.Request.Context()
	tx, err := m.d.DB.Begin(ctx)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	defer tx.Rollback(ctx)
	if useAnsible {
		var challengeEnv, encryptedChallenge string
		err = tx.QueryRow(ctx, `SELECT env,payload_enc FROM automation_ansible_preflights
			WHERE token_hash=$1 AND created_by=$2 AND expires_at>now() FOR UPDATE`,
			tokenHash, p.UserID).Scan(&challengeEnv, &encryptedChallenge)
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Fail(c, httpx.Conflict("SSH preflight expired or already used; run preflight again"))
			return
		}
		if err != nil {
			httpx.Fail(c, httpx.Internal(err))
			return
		}
		if challengeEnv != in.Env {
			httpx.Fail(c, httpx.Conflict("SSH preflight environment does not match the selected run environment"))
			return
		}
		data, decryptErr := m.d.Crypto.DecryptBytes(encryptedChallenge, []byte("automation:preflight:"+tokenHash))
		if decryptErr != nil || json.Unmarshal(data, &runPayload) != nil {
			httpx.Fail(c, httpx.Conflict("SSH preflight credentials could not be opened; run preflight again"))
			return
		}
		if _, _, err := inventoryAndKnownHosts(runPayload); err != nil {
			httpx.Fail(c, httpx.Conflict("SSH preflight host key data is invalid; run preflight again"))
			return
		}
	}
	var run int64
	err = tx.QueryRow(ctx, `INSERT INTO automation_runs(workflow_id,version,status,triggered_by,env)
		VALUES($1,$2,$3,$4,$5) RETURNING id`, id, v, status, p.UserID, in.Env).Scan(&run)
	if err == nil && useAnsible {
		data, marshalErr := json.Marshal(runPayload)
		if marshalErr != nil {
			err = marshalErr
		} else {
			encrypted, encryptErr := m.d.Crypto.EncryptBytes(data, []byte("automation:run:"+strconv.FormatInt(run, 10)))
			if encryptErr != nil {
				err = encryptErr
			} else {
				_, err = tx.Exec(ctx, "UPDATE automation_runs SET payload_enc=$2 WHERE id=$1", run, encrypted)
			}
		}
		if err == nil {
			_, err = tx.Exec(ctx, "DELETE FROM automation_ansible_preflights WHERE token_hash=$1", tokenHash)
		}
	}
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	if err = tx.Commit(ctx); err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	audit.SetResource(c, "automation", strconv.FormatInt(run, 10))
	if useAnsible {
		audit.SetDetail(c, gin.H{"workflow_id": id, "version": v, "env": in.Env,
			"targets": len(runPayload.Targets), "engine": "ansible"})
		ticket, pending, gateErr := m.d.Approval.Gate(ctx, p, approval.CreateInput{
			Type: ansibleApprovalType, Env: in.Env,
			Title:   "执行自动化流程 #" + strconv.FormatInt(id, 10),
			Reason:  "Ansible 受控步骤将在审批通过后执行",
			Payload: ansibleApprovalPayload(run),
		})
		if gateErr != nil {
			m.finish(run, "failed", "could not request environment approval", "[]")
			httpx.Fail(c, gateErr)
			return
		}
		if pending {
			if _, err := m.d.DB.Exec(ctx, `UPDATE automation_runs SET approval_id=$2 WHERE id=$1 AND status='awaiting_approval'`, run, ticket.ID); err != nil {
				_, _ = m.d.Approval.Cancel(ctx, p, ticket.ID)
				m.finish(run, "failed", "could not link approval ticket to run", "[]")
				httpx.Fail(c, httpx.Internal(err))
				return
			}
			approval.RespondPending(c, ticket)
			return
		}
		_, _ = m.d.DB.Exec(ctx, `UPDATE automation_runs SET approval_id=NULL WHERE id=$1`, run)
		if err := m.enqueueRun(ctx, run); err != nil {
			m.finish(run, "failed", "could not queue Ansible run", "[]")
			httpx.Fail(c, err)
			return
		}
	} else {
		audit.SetDetail(c, gin.H{"workflow_id": id, "version": v})
		m.signal()
	}
	httpx.Created(c, gin.H{"id": run, "status": "queued", "mode": "controlled"})
}

func ansibleApprovalPayload(runID int64) gin.H {
	return gin.H{"run_id": runID}
}

func (m *Module) runs(c *gin.Context) {
	limit, offset := page(c)
	p := rbac.PrincipalFrom(c)
	all := p.EnvScope == nil
	for _, env := range p.EnvScope {
		all = all || env == "*"
	}
	rows, err := m.d.DB.Query(c, `SELECT r.id,r.workflow_id,r.version,r.status,r.triggered_by,r.env,r.started_at,r.finished_at,r.error,
		COALESCE(v.steps,'[]'::jsonb),COALESCE(v.playbook,'')
		FROM automation_runs r JOIN automation_workflow_versions v
		ON v.workflow_id=r.workflow_id AND v.version=r.version
		WHERE $1::boolean OR r.env='' OR r.env=ANY($2::text[])
		ORDER BY r.id DESC LIMIT $3 OFFSET $4`, all, p.EnvScope, limit, offset)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	defer rows.Close()
	items := []gin.H{}
	for rows.Next() {
		var id, w, u int64
		var v int
		var s, env string
		var st, ft *time.Time
		var e *string
		var rawSteps json.RawMessage
		var playbook string
		if rows.Scan(&id, &w, &v, &s, &u, &env, &st, &ft, &e, &rawSteps, &playbook) != nil {
			httpx.Fail(c, httpx.Internal(errors.New("scan automation run")))
			return
		}
		steps, parseErr := parseSteps(rawSteps)
		requiresAnsible := playbook != "" || parseErr != nil || hasAnsibleSteps(steps)
		items = append(items, gin.H{"id": id, "workflow_id": w, "version": v, "status": s, "triggered_by": u,
			"engine": func() string {
				if requiresAnsible {
					return "ansible"
				}
				return "workflow"
			}(), "env": env, "started_at": st, "finished_at": ft, "error": redactAutomationError(e)})
	}
	if err := rows.Err(); err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	httpx.OK(c, gin.H{"items": items, "page_size": limit, "offset": offset})
}

func (m *Module) runDetail(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var workflowID int64
	var version int
	var status, env, logs string
	var created time.Time
	var started, finished *time.Time
	var errText *string
	var rawSteps json.RawMessage
	var playbook string
	err := m.d.DB.QueryRow(c.Request.Context(), `SELECT r.workflow_id,r.version,r.status,r.env,r.logs::text,r.created_at,r.started_at,r.finished_at,r.error,
		v.steps,v.playbook
		FROM automation_runs r JOIN automation_workflow_versions v
		ON v.workflow_id=r.workflow_id AND v.version=r.version WHERE r.id=$1`, id).
		Scan(&workflowID, &version, &status, &env, &logs, &created, &started, &finished, &errText, &rawSteps, &playbook)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Fail(c, httpx.NotFound("automation run not found"))
		return
	}
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	if env != "" && !rbac.PrincipalFrom(c).CanEnv(env) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope"))
		return
	}
	var logItems []string
	if err := json.Unmarshal([]byte(logs), &logItems); err != nil {
		logItems = []string{}
	}
	for i := range logItems {
		logItems[i] = redactAutomationText(logItems[i])
	}
	steps, parseErr := parseSteps(rawSteps)
	requiresAnsible := playbook != "" || parseErr != nil || hasAnsibleSteps(steps)
	httpx.OK(c, gin.H{"id": id, "workflow_id": workflowID, "version": version, "status": status, "env": env,
		"engine": func() string {
			if requiresAnsible {
				return "ansible"
			}
			return "workflow"
		}(), "logs": logItems, "created_at": created, "started_at": started, "finished_at": finished,
		"error": redactAutomationError(errText)})
}

func redactAutomationError(value *string) any {
	if value == nil {
		return nil
	}
	return redactAutomationText(*value)
}

func (m *Module) retry(c *gin.Context) { m.transition(c, true) }

func (m *Module) cancel(c *gin.Context) { m.transition(c, false) }

func (m *Module) transition(c *gin.Context, retry bool) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	var status, env string
	var encrypted string
	var workflowID int64
	var version int
	var approvalID *int64
	err := m.d.DB.QueryRow(ctx, "SELECT status,env,payload_enc,workflow_id,version,approval_id FROM automation_runs WHERE id=$1", id).
		Scan(&status, &env, &encrypted, &workflowID, &version, &approvalID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Fail(c, httpx.NotFound("automation run not found"))
		return
	}
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	if env != "" && !rbac.PrincipalFrom(c).CanEnv(env) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope"))
		return
	}
	if retry {
		if status != "failed" {
			httpx.Fail(c, httpx.Conflict("only failed runs can be retried"))
			return
		}
		var rawSteps json.RawMessage
		var playbook string
		err = m.d.DB.QueryRow(ctx, `SELECT steps,playbook FROM automation_workflow_versions
			WHERE workflow_id=$1 AND version=$2`, workflowID, version).Scan(&rawSteps, &playbook)
		if err != nil {
			httpx.Fail(c, httpx.Internal(err))
			return
		}
		steps, parseErr := parseSteps(rawSteps)
		if parseErr != nil {
			httpx.Fail(c, httpx.Conflict("workflow version contains unsupported steps"))
			return
		}
		if hasAnsibleDefinition(steps, playbook) || encrypted != "" {
			httpx.Fail(c, httpx.Conflict("SSH credentials are cleared after a run; trigger a new run with fresh credentials"))
			return
		}
		tag, err := m.d.DB.Exec(ctx, `UPDATE automation_runs SET status='queued',started_at=NULL,finished_at=NULL,error=NULL,
			attempt=attempt+1,updated_at=now() WHERE id=$1 AND status='failed'`, id)
		if err != nil || tag.RowsAffected() == 0 {
			httpx.Fail(c, httpx.Conflict("run cannot be retried"))
			return
		}
		m.signal()
		httpx.OK(c, gin.H{"id": id, "status": "queued"})
		return
	}
	tag, err := m.d.DB.Exec(ctx, `UPDATE automation_runs SET
		status=CASE WHEN status='running' THEN 'cancelling' ELSE 'cancelled' END,
		approval_id=NULL,payload_enc='',
		finished_at=CASE WHEN status='running' THEN NULL ELSE now() END,updated_at=now()
		WHERE id=$1 AND status IN ('awaiting_approval','queued','running')`, id)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Fail(c, httpx.Conflict("run cannot be cancelled"))
		return
	}
	m.mu.Lock()
	cancel := m.cancels[id]
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if status == "awaiting_approval" && approvalID != nil && m.d.Approval != nil {
		_, _ = m.d.Approval.Cancel(ctx, rbac.PrincipalFrom(c), *approvalID)
	}
	httpx.OK(c, gin.H{"id": id, "cancel_requested": true})
}

func idParam(c *gin.Context) (int64, bool) {
	id, e := strconv.ParseInt(c.Param("id"), 10, 64)
	if e != nil || id < 1 {
		httpx.Fail(c, httpx.Validation("invalid id"))
		return 0, false
	}
	return id, true
}
func page(c *gin.Context) (int, int) {
	n, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	if n < 1 {
		n = 50
	}
	if n > 200 {
		n = 200
	}
	o, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if o < 0 {
		o = 0
	}
	return n, o
}
