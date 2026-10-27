package automation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const automationConfigApprovalType = "automation.config"
const automationSystemScope = "system"

type automationConfigMutation struct {
	Kind        string          `json:"kind"`
	WorkflowID  int64           `json:"workflow_id,omitempty"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Trigger     string          `json:"trigger,omitempty"`
	Steps       json.RawMessage `json:"steps"`
	Playbook    string          `json:"playbook"`
	CreatedBy   int64           `json:"created_by"`
}

type automationConfigTicketPayload struct {
	ChangeID int64 `json:"change_id"`
}

func (m *Module) gateConfigMutation(c *gin.Context, mutation automationConfigMutation) bool {
	if m == nil || m.d == nil || m.d.Approval == nil || m.d.Crypto == nil {
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "审批或凭据加密服务不可用，流程配置变更已拒绝"))
		return true
	}
	p := rbac.PrincipalFrom(c)
	if p == nil {
		httpx.Fail(c, httpx.Unauthorized("authentication required"))
		return true
	}
	raw, err := json.Marshal(mutation)
	if err != nil {
		httpx.Fail(c, httpx.Internal(errors.New("could not stage automation configuration change")))
		return true
	}
	encrypted, err := m.d.Crypto.Encrypt(string(raw))
	if err != nil {
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "无法安全暂存流程配置变更，变更已拒绝"))
		return true
	}
	_, _ = m.d.DB.Exec(c, `DELETE FROM automation_config_changes WHERE created_at < now() - interval '30 days'`)
	var changeID int64
	if err := m.d.DB.QueryRow(c, `INSERT INTO automation_config_changes(payload_enc) VALUES($1) RETURNING id`, encrypted).Scan(&changeID); err != nil {
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "无法安全暂存流程配置变更，变更已拒绝"))
		return true
	}
	ticket, pending, err := m.d.Approval.Gate(c, p, approval.CreateInput{
		Type: automationConfigApprovalType, Env: automationSystemScope,
		Title:   "Automation workflow configuration change",
		Reason:  "An automation workflow configuration change is waiting for system policy evaluation",
		Payload: automationConfigTicketPayload{ChangeID: changeID},
	})
	if err != nil {
		_, _ = m.d.DB.Exec(c, `DELETE FROM automation_config_changes WHERE id=$1`, changeID)
		httpx.Fail(c, err)
		return true
	}
	if !pending {
		_, _ = m.d.DB.Exec(c, `DELETE FROM automation_config_changes WHERE id=$1`, changeID)
		return false
	}
	if _, err := m.d.DB.Exec(c, `UPDATE automation_config_changes SET ticket_env=$2,ready=true WHERE id=$1`, changeID, ticket.Env); err != nil {
		_, _ = m.d.Approval.Cancel(c, p, ticket.ID)
		_, _ = m.d.DB.Exec(c, `DELETE FROM automation_config_changes WHERE id=$1`, changeID)
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "无法关联审批单，流程配置变更已拒绝"))
		return true
	}
	approval.RespondPending(c, ticket)
	return true
}

type automationConfigHandler struct{ m *Module }

func (h *automationConfigHandler) OnApproved(ctx context.Context, ticket *approval.Ticket) (any, error) {
	if h == nil || h.m == nil || h.m.d == nil || h.m.d.Crypto == nil || ticket == nil {
		return nil, errors.New("automation configuration approval is unavailable")
	}
	var payload automationConfigTicketPayload
	if err := ticket.DecodePayload(&payload); err != nil || payload.ChangeID < 1 {
		return nil, errors.New("invalid automation configuration approval")
	}
	defer func() {
		_, _ = h.m.d.DB.Exec(context.WithoutCancel(ctx), `DELETE FROM automation_config_changes WHERE id=$1`, payload.ChangeID)
	}()
	var encrypted, env string
	var ready bool
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := h.m.d.DB.QueryRow(ctx, `SELECT payload_enc,ticket_env,ready FROM automation_config_changes WHERE id=$1`, payload.ChangeID).
			Scan(&encrypted, &env, &ready)
		if err != nil {
			return nil, errors.New("automation configuration change is unavailable")
		}
		if ready {
			break
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("automation configuration approval was not prepared")
		case <-ticker.C:
		}
	}
	if env != automationSystemScope || ticket.Env != env {
		return nil, errors.New("automation configuration environment does not match the ticket")
	}
	raw, err := h.m.d.Crypto.Decrypt(encrypted)
	if err != nil {
		return nil, errors.New("automation configuration change could not be opened")
	}
	var mutation automationConfigMutation
	if json.Unmarshal([]byte(raw), &mutation) != nil {
		return nil, errors.New("invalid automation configuration change")
	}
	if err := validateAutomationConfigMutation(mutation); err != nil {
		return nil, errors.New("automation configuration is no longer valid")
	}
	if err := h.m.reauthorizeConfigChange(ctx, ticket, mutation); err != nil {
		return nil, errors.New("automation configuration permission or scope was revoked")
	}
	result, err := h.m.applyConfigMutation(ctx, mutation)
	if err != nil {
		return nil, errors.New("automation configuration change could not be applied")
	}
	return result, nil
}

func (h *automationConfigHandler) OnRejected(ctx context.Context, ticket *approval.Ticket) error {
	if h == nil || h.m == nil || h.m.d == nil || ticket == nil {
		return nil
	}
	var payload automationConfigTicketPayload
	if err := ticket.DecodePayload(&payload); err != nil || payload.ChangeID < 1 {
		return nil
	}
	_, err := h.m.d.DB.Exec(ctx, `DELETE FROM automation_config_changes WHERE id=$1`, payload.ChangeID)
	return err
}

func (m *Module) reauthorizeConfigChange(ctx context.Context, ticket *approval.Ticket, mutation automationConfigMutation) error {
	if m.d.RBAC == nil || ticket == nil {
		return httpx.Conflict("RBAC service unavailable")
	}
	_, permissions, scope, err := m.d.RBAC.UserAccess(ctx, ticket.ApplicantID)
	if err != nil {
		return err
	}
	p := &rbac.Principal{UserID: ticket.ApplicantID, Permissions: permissions, EnvScope: scope}
	if !p.Can("automation:manage") || !p.CanEnv(automationSystemScope) || mutation.CreatedBy != ticket.ApplicantID {
		return httpx.Forbidden("automation manage permission or system scope was revoked")
	}
	return nil
}

func validateAutomationConfigMutation(change automationConfigMutation) error {
	if change.CreatedBy < 1 {
		return errors.New("invalid configuration owner")
	}
	if change.Kind == "workflow.create" && (change.Name == "" || len(change.Name) > 120) {
		return errors.New("invalid workflow name")
	}
	if change.Kind == "workflow.version" && change.WorkflowID < 1 {
		return errors.New("invalid workflow id")
	}
	if change.Kind != "workflow.create" && change.Kind != "workflow.version" {
		return errors.New("unsupported workflow configuration change")
	}
	if len(change.Steps) == 0 && change.Playbook == "" {
		return errors.New("workflow steps or playbook are required")
	}
	if len(change.Steps) == 0 {
		change.Steps = json.RawMessage(`[]`)
	}
	if _, err := parseSteps(change.Steps); err != nil {
		return err
	}
	if change.Playbook != "" {
		if err := validatePlaybook(change.Playbook); err != nil {
			return err
		}
	}
	return validateAutomationSecrets(change.Steps, change.Playbook, change.Name, change.Description, change.Trigger)
}

func (m *Module) applyConfigMutation(ctx context.Context, change automationConfigMutation) (any, error) {
	switch change.Kind {
	case "workflow.create":
		tx, err := m.d.DB.Begin(ctx)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(ctx)
		var id int64
		if err := tx.QueryRow(ctx, `INSERT INTO automation_workflows(name,description,trigger,created_by)
			VALUES($1,$2,$3,$4) RETURNING id`, change.Name, change.Description, change.Trigger, change.CreatedBy).Scan(&id); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO automation_workflow_versions(workflow_id,version,steps,playbook,created_by)
			VALUES($1,1,$2,$3,$4)`, id, change.Steps, change.Playbook, change.CreatedBy); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return map[string]any{"id": id, "version": 1}, nil
	case "workflow.version":
		tx, err := m.d.DB.Begin(ctx)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(ctx)
		var version int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM automation_workflow_versions WHERE workflow_id=$1`, change.WorkflowID).Scan(&version); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO automation_workflow_versions(workflow_id,version,steps,playbook,created_by)
			VALUES($1,$2,$3,$4,$5)`, change.WorkflowID, version, change.Steps, change.Playbook, change.CreatedBy); err != nil {
			return nil, err
		}
		tag, err := tx.Exec(ctx, `UPDATE automation_workflows SET current_version=$2,updated_at=now() WHERE id=$1`, change.WorkflowID, version)
		if err != nil || tag.RowsAffected() == 0 {
			return nil, errors.New("workflow not found")
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return map[string]any{"workflow_id": change.WorkflowID, "version": version}, nil
	default:
		return nil, httpx.Validation("unsupported automation configuration change")
	}
}
