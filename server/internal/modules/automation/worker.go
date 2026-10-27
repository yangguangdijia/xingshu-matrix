package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/ansible"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const automationMaxConcurrentRuns = 2

type claimedRun struct {
	ID          int64
	WorkflowID  int64
	Version     int
	TriggeredBy int64
	Env         string
	PayloadEnc  string
	Steps       []workflowStep
	Playbook    string
	ParseError  error
}

func (m *Module) Start(ctx context.Context, _ *app.Deps) error {
	if err := m.recoverInterruptedRuns(ctx); err != nil {
		return err
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.dispatch(ctx)
	}()
	return nil
}

func (m *Module) recoverInterruptedRuns(ctx context.Context) error {
	_, err := m.d.DB.Exec(ctx, `UPDATE automation_runs SET status='failed',
		error='server restarted during execution; inspect target hosts before retrying',
		approval_id=NULL,payload_enc='',finished_at=now(),updated_at=now() WHERE status IN ('running','cancelling')`)
	return err
}

func (m *Module) Stop(ctx context.Context) error {
	m.mu.Lock()
	for _, cancel := range m.cancels {
		cancel()
	}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Module) dispatch(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	slots := make(chan struct{}, automationMaxConcurrentRuns)
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		for len(slots) < cap(slots) {
			if ctx.Err() != nil {
				return
			}
			run, err := m.claim(ctx)
			if err != nil {
				if !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
					if m.d.Logger != nil {
						m.d.Logger.Error("claim automation run", "error", err)
					}
				}
				break
			}
			slots <- struct{}{}
			m.wg.Add(1)
			workers.Add(1)
			go func(run *claimedRun) {
				defer m.wg.Done()
				defer workers.Done()
				defer func() { <-slots }()
				m.execute(ctx, run)
			}(run)
		}
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-ticker.C:
			_, _ = m.d.DB.Exec(ctx, `DELETE FROM automation_ansible_preflights WHERE expires_at<=now()`)
			_, _ = m.d.DB.Exec(ctx, `UPDATE automation_runs SET status='failed',error='approval expired before execution',
				payload_enc='',finished_at=now(),updated_at=now()
				WHERE status='awaiting_approval' AND created_at<now()-interval '24 hours'`)
			_, _ = m.d.DB.Exec(ctx, `UPDATE automation_runs SET status='failed',error='queued run expired before execution',
				payload_enc='',finished_at=now(),updated_at=now()
				WHERE status='queued' AND payload_enc<>'' AND created_at<now()-interval '24 hours'`)
		}
	}
}

func (m *Module) claim(ctx context.Context) (*claimedRun, error) {
	run := &claimedRun{}
	var rawSteps json.RawMessage
	err := m.d.DB.QueryRow(ctx, `WITH selected AS (
			SELECT r.id,v.steps,v.playbook FROM automation_runs r
			JOIN automation_workflow_versions v ON v.workflow_id=r.workflow_id AND v.version=r.version
			WHERE r.status='queued' ORDER BY r.id FOR UPDATE OF r SKIP LOCKED LIMIT 1
		)
		UPDATE automation_runs r SET status='running',started_at=now(),updated_at=now()
		FROM selected s WHERE r.id=s.id
		RETURNING r.id,r.workflow_id,r.version,r.triggered_by,r.env,r.payload_enc,s.steps,s.playbook`,
	).Scan(&run.ID, &run.WorkflowID, &run.Version, &run.TriggeredBy, &run.Env, &run.PayloadEnc, &rawSteps, &run.Playbook)
	if err != nil {
		return nil, err
	}
	run.Steps, run.ParseError = parseSteps(rawSteps)
	return run, nil
}

func (m *Module) execute(parent context.Context, run *claimedRun) {
	defer func() {
		if recover() != nil {
			m.finish(run.ID, "failed", "automation worker failed", `["Automation worker failed"]`)
		}
	}()

	ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
	m.mu.Lock()
	m.cancels[run.ID] = cancel
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		delete(m.cancels, run.ID)
		m.mu.Unlock()
	}()

	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tag, err := m.d.DB.Exec(ctx, `UPDATE automation_runs SET updated_at=now()
					WHERE id=$1 AND status='running'`, run.ID)
				if err != nil || tag.RowsAffected() == 0 {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-heartbeatDone }()

	if run.ParseError != nil {
		m.finish(run.ID, "failed", "workflow version contains unsupported steps", `["Workflow version contains unsupported steps"]`)
		return
	}
	if err := m.checkRunPermission(ctx, run); err != nil {
		m.finish(run.ID, "failed", "run permission or environment scope was revoked", "[]")
		return
	}
	m.logRun(run.ID, fmt.Sprintf("Started workflow %d v%d (%d steps)", run.WorkflowID, run.Version, len(run.Steps)))
	if hasAnsibleDefinition(run.Steps, run.Playbook) {
		if run.PayloadEnc == "" || m.d.Crypto == nil {
			m.finish(run.ID, "failed", "encrypted SSH credentials are unavailable", `["Ansible credentials unavailable"]`)
			return
		}
		data, err := m.d.Crypto.DecryptBytes(run.PayloadEnc, []byte("automation:run:"+strconv.FormatInt(run.ID, 10)))
		if err != nil {
			m.finish(run.ID, "failed", "cannot decrypt encrypted SSH credentials", `["Unable to open encrypted SSH credentials"]`)
			return
		}
		var payload ansibleRunPayload
		if json.Unmarshal(data, &payload) != nil {
			m.finish(run.ID, "failed", "invalid encrypted Ansible run payload", `["Invalid encrypted Ansible run payload"]`)
			return
		}
		if err := runAnsible(ctx, run.Steps, run.Playbook, payload); err != nil {
			if errors.Is(err, ansible.ErrUnavailable) {
				m.finish(run.ID, "failed", "embedded Ansible runtime unavailable; run the Matrix platform image", `["Embedded Ansible runtime unavailable"]`)
			} else if ctx.Err() != nil {
				m.finish(run.ID, "cancelled", "run was cancelled or timed out", `["Ansible run cancelled or timed out"]`)
			} else {
				m.finish(run.ID, "failed", "Ansible execution failed; verify target Python, sudo access and task requirements", `["Ansible execution failed; raw task output and credentials were withheld"]`)
			}
			return
		}
		m.logRun(run.ID, "Ansible playbook completed successfully")
	} else {
		m.logRun(run.ID, "Workflow contains no executable Ansible steps")
	}
	m.finish(run.ID, "succeeded", "", `["Workflow completed successfully"]`)
}

func (m *Module) checkRunPermission(ctx context.Context, run *claimedRun) error {
	if m.d.RBAC == nil {
		return errors.New("RBAC service unavailable")
	}
	_, perms, scope, err := m.d.RBAC.UserAccess(ctx, run.TriggeredBy)
	if err != nil {
		return err
	}
	p := &rbac.Principal{UserID: run.TriggeredBy, Permissions: perms, EnvScope: scope}
	if !p.Can("automation:run") {
		return errors.New("automation run permission revoked")
	}
	if hasAnsibleDefinition(run.Steps, run.Playbook) && (!p.Can("host:exec") || !p.CanEnv(run.Env)) {
		return errors.New("host execution permission or environment scope revoked")
	}
	return nil
}

func (m *Module) enqueueRun(ctx context.Context, id int64) error {
	tag, err := m.d.DB.Exec(ctx, `UPDATE automation_runs SET status='queued',approval_id=NULL,updated_at=now()
		WHERE id=$1 AND status='awaiting_approval'`, id)
	if err == nil && tag.RowsAffected() == 0 {
		err = httpx.Conflict("automation run is cancelled, expired or already queued")
	}
	if err == nil {
		m.signal()
	}
	return err
}

func (m *Module) OnApproved(ctx context.Context, ticket *approval.Ticket) (any, error) {
	var payload struct {
		RunID int64 `json:"run_id"`
	}
	if err := ticket.DecodePayload(&payload); err != nil || payload.RunID < 1 {
		return nil, httpx.Validation("invalid automation approval payload")
	}
	var env string
	var applicant int64
	if err := m.d.DB.QueryRow(ctx, `SELECT env,triggered_by FROM automation_runs
		WHERE id=$1 AND status='awaiting_approval'`, payload.RunID).Scan(&env, &applicant); err != nil {
		return nil, err
	}
	if m.d.RBAC == nil {
		return nil, httpx.Conflict("RBAC service unavailable")
	}
	_, perms, scope, err := m.d.RBAC.UserAccess(ctx, applicant)
	if err != nil {
		return nil, err
	}
	p := &rbac.Principal{UserID: applicant, Permissions: perms, EnvScope: scope}
	if !p.Can("automation:run") || !p.Can("host:exec") || !p.CanEnv(env) {
		m.finish(payload.RunID, "failed", "run permission or environment scope was revoked", "[]")
		return nil, httpx.Forbidden("automation run permission or environment scope was revoked")
	}
	if err := m.enqueueRun(ctx, payload.RunID); err != nil {
		return nil, err
	}
	return map[string]any{"run_id": payload.RunID, "status": "queued"}, nil
}

func (m *Module) OnRejected(ctx context.Context, ticket *approval.Ticket) error {
	var payload struct {
		RunID int64 `json:"run_id"`
	}
	if err := ticket.DecodePayload(&payload); err != nil || payload.RunID < 1 {
		return httpx.Validation("invalid automation approval payload")
	}
	_, err := m.d.DB.Exec(ctx, `UPDATE automation_runs SET status='cancelled',approval_id=NULL,payload_enc='',
		error='environment approval was rejected or cancelled',finished_at=now(),updated_at=now()
		WHERE id=$1 AND status='awaiting_approval'`, payload.RunID)
	return err
}

func (m *Module) logRun(id int64, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	line := time.Now().UTC().Format(time.RFC3339) + " " + redactAutomationText(text)
	_, _ = m.d.DB.Exec(ctx, `UPDATE automation_runs
		SET logs=logs||jsonb_build_array($2::text),updated_at=now()
		WHERE id=$1 AND status IN ('awaiting_approval','queued','running','cancelling')`, id, line)
}

func (m *Module) finish(id int64, status, message, logs string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	message = redactAutomationText(message)
	logs = string(redactAutomationJSON(json.RawMessage(logs)))
	statusGuard := "status IN ('awaiting_approval','queued','running','cancelling')"
	if status == "succeeded" {
		statusGuard = "status='running'"
	}
	_, _ = m.d.DB.Exec(ctx, `UPDATE automation_runs SET status=$2,error=$3,finished_at=now(),updated_at=now(),
		payload_enc='',logs=logs||$4::jsonb
		WHERE id=$1 AND `+statusGuard, id, status, message, logs)
}

func (m *Module) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
