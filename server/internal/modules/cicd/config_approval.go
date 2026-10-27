package cicd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const cicdConfigApprovalType = "cicd.config"

type cicdConfigMutation struct {
	Kind              string        `json:"kind"`
	ID                int64         `json:"id,omitempty"`
	Table             string        `json:"table,omitempty"`
	Resource          string        `json:"resource,omitempty"`
	Target            *targetIn     `json:"target,omitempty"`
	Jenkins           *jenkinsIn    `json:"jenkins,omitempty"`
	Application       *Application  `json:"application,omitempty"`
	Pipeline          *Pipeline     `json:"pipeline,omitempty"`
	Argo              *argoConfigIn `json:"argocd,omitempty"`
	HelmRepo          *helmRepoIn   `json:"helm_repo,omitempty"`
	ExistingSecretEnc string        `json:"existing_secret_enc,omitempty"`
	WebhookSecret     string        `json:"webhook_secret,omitempty"`
	Envs              []string      `json:"envs"`
	ExpectedUpdatedAt *time.Time    `json:"expected_updated_at,omitempty"`
}

type cicdConfigTicketPayload struct {
	ChangeID int64 `json:"change_id"`
}

func (m *Module) gateConfigMutation(c *gin.Context, mutation cicdConfigMutation, extra gin.H) bool {
	if m.d == nil || m.d.Approval == nil || m.d.Crypto == nil {
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "审批或凭据加密服务不可用，配置变更已拒绝"))
		return true
	}
	p := rbac.PrincipalFrom(c)
	if p == nil {
		httpx.Fail(c, httpx.Unauthorized("authentication required"))
		return true
	}
	mutation.Envs = normalizedEnvs(mutation.Envs)
	if len(mutation.Envs) == 0 {
		mutation.Envs = []string{"system"}
	}
	raw, err := json.Marshal(mutation)
	if err != nil {
		httpx.Fail(c, httpx.Internal(errors.New("could not stage CICD configuration change")))
		return true
	}
	encrypted, err := m.d.Crypto.Encrypt(string(raw))
	if err != nil {
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "无法安全暂存配置变更，变更已拒绝"))
		return true
	}
	_, _ = m.db.Exec(c, `DELETE FROM cicd_config_changes WHERE created_at < now() - interval '30 days'`)
	var changeID int64
	if err := m.db.QueryRow(c, `INSERT INTO cicd_config_changes(payload_enc) VALUES($1) RETURNING id`, encrypted).Scan(&changeID); err != nil {
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "无法安全暂存配置变更，变更已拒绝"))
		return true
	}

	var tickets []*approval.Ticket
	for _, env := range mutation.Envs {
		ticket, pending, gateErr := m.d.Approval.Gate(c, p, approval.CreateInput{
			Type: cicdConfigApprovalType, Env: env, Title: "CICD configuration change",
			Reason:  "A CICD configuration change is waiting for environment policy evaluation",
			Payload: cicdConfigTicketPayload{ChangeID: changeID},
		})
		if gateErr != nil {
			cancelCICDConfigTickets(c, m.d.Approval, p, tickets)
			_, _ = m.db.Exec(c, `DELETE FROM cicd_config_changes WHERE id=$1`, changeID)
			httpx.Fail(c, gateErr)
			return true
		}
		if pending {
			tickets = append(tickets, ticket)
			if len(tickets) > 1 {
				cancelCICDConfigTickets(c, m.d.Approval, p, tickets)
				_, _ = m.db.Exec(c, `DELETE FROM cicd_config_changes WHERE id=$1`, changeID)
				httpx.Fail(c, httpx.Validation("split this change: more than one environment requires approval"))
				return true
			}
		}
	}
	if len(tickets) == 0 {
		_, _ = m.db.Exec(c, `DELETE FROM cicd_config_changes WHERE id=$1`, changeID)
		return false
	}
	if _, err := m.db.Exec(c, `UPDATE cicd_config_changes SET ticket_env=$2,ready=true WHERE id=$1`, changeID, tickets[0].Env); err != nil {
		cancelCICDConfigTickets(c, m.d.Approval, p, tickets)
		_, _ = m.db.Exec(c, `DELETE FROM cicd_config_changes WHERE id=$1`, changeID)
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "无法关联审批单，配置变更已拒绝"))
		return true
	}
	data := gin.H{"approval_required": true, "ticket": tickets[0]}
	for key, value := range extra {
		data[key] = value
	}
	c.JSON(http.StatusAccepted, httpx.Envelope{
		Code: httpx.CodeApprovalRequired, Message: "该环境的配置变更需要审批，已创建审批单",
		Data: data, RequestID: httpx.RequestIDFrom(c),
	})
	return true
}

func cancelCICDConfigTickets(ctx context.Context, service *approval.Service, p *rbac.Principal, tickets []*approval.Ticket) {
	for _, ticket := range tickets {
		if ticket != nil {
			_, _ = service.Cancel(ctx, p, ticket.ID)
		}
	}
}

func normalizedEnvs(envs []string) []string {
	set := make(map[string]struct{}, len(envs))
	for _, env := range envs {
		if env = strings.TrimSpace(env); env != "" {
			set[env] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for env := range set {
		out = append(out, env)
	}
	sort.Strings(out)
	return out
}

type cicdConfigHandler struct{ m *Module }

func (h *cicdConfigHandler) OnApproved(ctx context.Context, ticket *approval.Ticket) (any, error) {
	if h == nil || h.m == nil || h.m.d == nil || h.m.db == nil || ticket == nil || h.m.d.Crypto == nil {
		return nil, errors.New("CICD configuration approval is unavailable")
	}
	var payload cicdConfigTicketPayload
	if err := ticket.DecodePayload(&payload); err != nil || payload.ChangeID < 1 {
		return nil, errors.New("invalid CICD configuration approval")
	}
	defer func() {
		_, _ = h.m.db.Exec(context.WithoutCancel(ctx), `DELETE FROM cicd_config_changes WHERE id=$1`, payload.ChangeID)
	}()
	var encoded, env string
	ready := false
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := h.m.db.QueryRow(ctx, `SELECT payload_enc,ticket_env,ready FROM cicd_config_changes WHERE id=$1`, payload.ChangeID).
			Scan(&encoded, &env, &ready)
		if err != nil {
			return nil, errors.New("CICD configuration change is unavailable")
		}
		if ready {
			break
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("CICD configuration approval was not prepared")
		case <-ticker.C:
		}
	}
	if env == "" || ticket.Env != env {
		return nil, errors.New("CICD configuration environment does not match the ticket")
	}
	raw, err := h.m.d.Crypto.Decrypt(encoded)
	if err != nil {
		return nil, errors.New("CICD configuration change could not be opened")
	}
	var mutation cicdConfigMutation
	if json.Unmarshal([]byte(raw), &mutation) != nil {
		return nil, errors.New("invalid CICD configuration change")
	}
	if err := h.m.reauthorizeConfigChange(ctx, ticket, mutation); err != nil {
		return nil, errors.New("CICD configuration permission or environment scope was revoked")
	}
	if err := h.m.validateConfigChange(ctx, mutation); err != nil {
		return nil, errors.New("CICD configuration is no longer valid")
	}
	id, err := h.m.applyCICDConfigMutation(ctx, mutation)
	if err != nil {
		return nil, errors.New("CICD configuration change could not be applied")
	}
	return map[string]any{"kind": mutation.Kind, "id": id, "applied": true}, nil
}

func (h *cicdConfigHandler) OnRejected(ctx context.Context, ticket *approval.Ticket) error {
	if h == nil || h.m == nil || h.m.db == nil || ticket == nil {
		return nil
	}
	var payload cicdConfigTicketPayload
	if err := ticket.DecodePayload(&payload); err != nil || payload.ChangeID < 1 {
		return nil
	}
	_, err := h.m.db.Exec(ctx, `DELETE FROM cicd_config_changes WHERE id=$1`, payload.ChangeID)
	return err
}

func (m *Module) reauthorizeConfigChange(ctx context.Context, ticket *approval.Ticket, mutation cicdConfigMutation) error {
	if m.d.RBAC == nil || ticket == nil {
		return httpx.Conflict("RBAC service unavailable")
	}
	_, permissions, scope, err := m.d.RBAC.UserAccess(ctx, ticket.ApplicantID)
	if err != nil {
		return err
	}
	p := &rbac.Principal{UserID: ticket.ApplicantID, Permissions: permissions, EnvScope: scope}
	if !p.Can("cicd:manage") {
		return httpx.Forbidden("applicant CICD manage permission was revoked")
	}
	for _, env := range normalizedEnvs(mutation.Envs) {
		if !p.CanEnv(env) {
			return httpx.Forbidden("applicant environment scope was revoked")
		}
	}
	return nil
}

func (m *Module) applyCICDConfigMutation(ctx context.Context, change cicdConfigMutation) (int64, error) {
	switch change.Kind {
	case "target.create":
		if change.Target == nil || m.d.Crypto == nil {
			return 0, errors.New("invalid target configuration")
		}
		encrypted, err := m.d.Crypto.Encrypt(change.Target.Kubeconfig)
		if err != nil {
			return 0, err
		}
		var id int64
		err = m.db.QueryRow(ctx, `INSERT INTO cicd_targets(name,kubeconfig_enc,namespace,env) VALUES($1,$2,$3,$4) RETURNING id`,
			change.Target.Name, encrypted, change.Target.Namespace, change.Target.Env).Scan(&id)
		return id, err
	case "jenkins.create":
		if change.Jenkins == nil || m.d.Crypto == nil {
			return 0, errors.New("invalid Jenkins configuration")
		}
		encrypted := ""
		var err error
		if change.Jenkins.Token != "" {
			encrypted, err = m.d.Crypto.Encrypt(change.Jenkins.Token)
			if err != nil {
				return 0, err
			}
		}
		var id int64
		err = m.db.QueryRow(ctx, `INSERT INTO cicd_jenkins(name,url,username,token_enc) VALUES($1,$2,$3,$4) RETURNING id`,
			change.Jenkins.Name, strings.TrimRight(change.Jenkins.URL, "/"), change.Jenkins.Username, encrypted).Scan(&id)
		return id, err
	case "application.create":
		if change.Application == nil || m.d.Crypto == nil {
			return 0, errors.New("invalid application configuration")
		}
		in := change.Application
		in.defaults()
		webhookEnc, err := m.d.Crypto.Encrypt(change.WebhookSecret)
		if err != nil {
			return 0, err
		}
		gitEnc := ""
		if in.GitToken != "" {
			gitEnc, err = m.d.Crypto.Encrypt(in.GitToken)
			if err != nil {
				return 0, err
			}
		}
		envs, err := json.Marshal(in.Envs)
		if err != nil {
			return 0, err
		}
		var id int64
		err = m.db.QueryRow(ctx, `INSERT INTO cicd_applications(name,repo_url,branch,dockerfile,context_dir,image_repo,git_username,git_token_enc,webhook_secret_enc,envs)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
			in.Name, in.RepoURL, in.Branch, in.Dockerfile, in.ContextDir, in.ImageRepo, in.GitUsername, gitEnc, webhookEnc, envs).Scan(&id)
		return id, err
	case "application.update":
		if change.Application == nil || m.d.Crypto == nil || change.ExpectedUpdatedAt == nil {
			return 0, errors.New("invalid application update")
		}
		in := change.Application
		in.defaults()
		gitEnc := change.ExistingSecretEnc
		var err error
		if in.GitToken != "" && in.GitToken != "********" {
			gitEnc, err = m.d.Crypto.Encrypt(in.GitToken)
			if err != nil {
				return 0, err
			}
		}
		envs, err := json.Marshal(in.Envs)
		if err != nil {
			return 0, err
		}
		tag, err := m.db.Exec(ctx, `UPDATE cicd_applications SET name=$2,repo_url=$3,branch=$4,dockerfile=$5,context_dir=$6,image_repo=$7,
			git_username=$8,git_token_enc=$9,envs=$10,updated_at=now() WHERE id=$1 AND updated_at=$11`,
			change.ID, in.Name, in.RepoURL, in.Branch, in.Dockerfile, in.ContextDir, in.ImageRepo, in.GitUsername, gitEnc, envs, *change.ExpectedUpdatedAt)
		if err != nil || tag.RowsAffected() == 0 {
			return 0, httpx.Conflict("application changed while approval was pending")
		}
		return change.ID, nil
	case "application.rotate_webhook":
		if change.ID < 1 || change.WebhookSecret == "" || change.ExpectedUpdatedAt == nil || m.d.Crypto == nil {
			return 0, errors.New("invalid webhook secret change")
		}
		encrypted, err := m.d.Crypto.Encrypt(change.WebhookSecret)
		if err != nil {
			return 0, err
		}
		tag, err := m.db.Exec(ctx, `UPDATE cicd_applications SET webhook_secret_enc=$2,updated_at=now() WHERE id=$1 AND updated_at=$3`,
			change.ID, encrypted, *change.ExpectedUpdatedAt)
		if err != nil || tag.RowsAffected() == 0 {
			return 0, httpx.Conflict("application changed while approval was pending")
		}
		return change.ID, nil
	case "pipeline.save":
		if change.Pipeline == nil {
			return 0, errors.New("invalid pipeline configuration")
		}
		in := change.Pipeline
		stages, err := json.Marshal(in.Stages)
		if err != nil {
			return 0, err
		}
		if change.ID == 0 {
			var id int64
			err := m.db.QueryRow(ctx, `INSERT INTO cicd_pipelines(app_id,name,runner_target_id,stages,on_push) VALUES($1,$2,$3,$4,$5) RETURNING id`,
				in.AppID, in.Name, in.RunnerTargetID, stages, in.OnPush).Scan(&id)
			return id, err
		}
		if change.ExpectedUpdatedAt == nil {
			return 0, errors.New("pipeline update version is unavailable")
		}
		tag, err := m.db.Exec(ctx, `UPDATE cicd_pipelines SET app_id=$2,name=$3,runner_target_id=$4,stages=$5,on_push=$6,updated_at=now()
			WHERE id=$1 AND updated_at=$7`, change.ID, in.AppID, in.Name, in.RunnerTargetID, stages, in.OnPush, *change.ExpectedUpdatedAt)
		if err != nil || tag.RowsAffected() == 0 {
			return 0, httpx.Conflict("pipeline changed while approval was pending")
		}
		return change.ID, nil
	case "argocd.create", "argocd.update":
		if change.Argo == nil || m.d.Crypto == nil {
			return 0, errors.New("invalid Argo CD configuration")
		}
		enc := change.ExistingSecretEnc
		if change.Argo.Token != "" && change.Argo.Token != argoTokenMask {
			var err error
			enc, err = m.encryptArgoToken(change.Argo.Token)
			if err != nil {
				return 0, err
			}
		}
		if change.Kind == "argocd.create" {
			var id int64
			err := m.db.QueryRow(ctx, `INSERT INTO cicd_argocd(name,url,token_enc,env,project) VALUES($1,$2,$3,$4,$5) RETURNING id`,
				change.Argo.Name, change.Argo.URL, enc, change.Argo.Env, change.Argo.Project).Scan(&id)
			return id, err
		}
		if change.ExpectedUpdatedAt == nil {
			return 0, errors.New("Argo CD configuration version is unavailable")
		}
		tag, err := m.db.Exec(ctx, `UPDATE cicd_argocd SET name=$2,url=$3,token_enc=$4,env=$5,project=$6,updated_at=now()
			WHERE id=$1 AND updated_at=$7`, change.ID, change.Argo.Name, change.Argo.URL, enc, change.Argo.Env, change.Argo.Project, *change.ExpectedUpdatedAt)
		if err != nil || tag.RowsAffected() == 0 {
			return 0, httpx.Conflict("Argo CD configuration changed while approval was pending")
		}
		return change.ID, nil
	case "argocd.delete":
		if change.ID < 1 || change.ExpectedUpdatedAt == nil {
			return 0, errors.New("Argo CD configuration version is unavailable")
		}
		tag, err := m.db.Exec(ctx, `DELETE FROM cicd_argocd WHERE id=$1 AND updated_at=$2`, change.ID, *change.ExpectedUpdatedAt)
		if err != nil || tag.RowsAffected() == 0 {
			return 0, httpx.Conflict("Argo CD configuration changed while approval was pending")
		}
		return change.ID, nil
	case "helm.repo.create":
		if change.HelmRepo == nil {
			return 0, errors.New("invalid Helm repository configuration")
		}
		var id int64
		err := m.db.QueryRow(ctx, `INSERT INTO cicd_helm_repos(name,url,env)
			VALUES($1,$2,$3) RETURNING id`, change.HelmRepo.Name, change.HelmRepo.URL, change.HelmRepo.Env).Scan(&id)
		return id, err
	case "helm.repo.update":
		if change.ID < 1 || change.ExpectedUpdatedAt == nil || change.HelmRepo == nil {
			return 0, errors.New("Helm repository version is unavailable")
		}
		tag, err := m.db.Exec(ctx, `UPDATE cicd_helm_repos SET name=$2,url=$3,env=$4,updated_at=now()
			WHERE id=$1 AND updated_at=$5`, change.ID, change.HelmRepo.Name, change.HelmRepo.URL, change.HelmRepo.Env, *change.ExpectedUpdatedAt)
		if err != nil || tag.RowsAffected() == 0 {
			return 0, httpx.Conflict("Helm repository changed while approval was pending")
		}
		return change.ID, nil
	case "helm.repo.delete":
		if change.ID < 1 || change.ExpectedUpdatedAt == nil {
			return 0, errors.New("Helm repository version is unavailable")
		}
		tag, err := m.db.Exec(ctx, `DELETE FROM cicd_helm_repos WHERE id=$1 AND updated_at=$2`, change.ID, *change.ExpectedUpdatedAt)
		if err != nil || tag.RowsAffected() == 0 {
			return 0, httpx.Conflict("Helm repository changed or is in use")
		}
		return change.ID, nil
	case "helm.repo.sync":
		if change.ID < 1 || change.ExpectedUpdatedAt == nil {
			return 0, errors.New("Helm repository version is unavailable")
		}
		return m.syncHelmRepo(ctx, change.ID, change.ExpectedUpdatedAt)
	case "row.delete":
		if change.ID < 1 {
			return 0, httpx.Validation("invalid configuration id")
		}
		table := map[string]string{
			"cicd_targets": "target", "cicd_jenkins": "jenkins",
			"cicd_applications": "application", "cicd_pipelines": "pipeline",
		}
		if table[change.Table] != change.Resource {
			return 0, httpx.Validation("invalid CICD configuration type")
		}
		if change.ExpectedUpdatedAt == nil {
			return 0, errors.New("configuration version is unavailable")
		}
		tag, err := m.db.Exec(ctx, "DELETE FROM "+change.Table+" WHERE id=$1 AND updated_at=$2", change.ID, *change.ExpectedUpdatedAt)
		if err != nil || tag.RowsAffected() == 0 {
			return 0, httpx.Conflict("configuration changed while approval was pending")
		}
		return change.ID, nil
	default:
		return 0, fmt.Errorf("unsupported CICD configuration change %q", change.Kind)
	}
}

func (m *Module) deleteConfigEnvs(ctx context.Context, table string, id int64) (cicdConfigMutation, error) {
	change := cicdConfigMutation{Kind: "row.delete", ID: id, Table: table}
	var updated time.Time
	var envs []string
	switch table {
	case "cicd_targets":
		var env string
		if err := m.db.QueryRow(ctx, `SELECT env,updated_at FROM cicd_targets WHERE id=$1`, id).Scan(&env, &updated); err != nil {
			return change, notFound(err)
		}
		envs = append(envs, env)
		change.Resource = "target"
	case "cicd_jenkins":
		if err := m.db.QueryRow(ctx, `SELECT updated_at FROM cicd_jenkins WHERE id=$1`, id).Scan(&updated); err != nil {
			return change, notFound(err)
		}
		change.Resource = "jenkins"
	case "cicd_applications":
		app, err := m.loadApp(ctx, id)
		if err != nil {
			return change, err
		}
		updated = app.UpdatedAt
		for _, env := range app.Envs {
			envs = append(envs, env.Name)
		}
		change.Resource = "application"
	case "cicd_pipelines":
		pipeline, err := m.loadPipe(ctx, id)
		if err != nil {
			return change, err
		}
		updated = pipeline.UpdatedAt
		runnerEnv, err := m.targetConfigEnv(ctx, pipeline.RunnerTargetID)
		if err != nil {
			return change, err
		}
		envs = append(envs, runnerEnv)
		for _, stage := range pipeline.Stages {
			if stage.Type == "deploy" {
				envs = append(envs, stage.DeployEnv)
			}
		}
		change.Resource = "pipeline"
	default:
		return change, httpx.Validation("unsupported CICD configuration type")
	}
	change.Envs = normalizedEnvs(envs)
	change.ExpectedUpdatedAt = &updated
	return change, nil
}

func (m *Module) validateConfigChange(ctx context.Context, change cicdConfigMutation) error {
	switch change.Kind {
	case "target.create":
		if change.Target == nil {
			return httpx.Validation("invalid target configuration")
		}
		if inlineSecretText(change.Target.Name) || inlineSecretText(change.Target.Namespace) {
			return httpx.Validation("target configuration must not contain inline secrets")
		}
		if _, err := clientcmd.RESTConfigFromKubeConfig([]byte(change.Target.Kubeconfig)); err != nil {
			return httpx.Validation("invalid kubeconfig")
		}
	case "application.create", "application.update":
		if change.Application == nil {
			return httpx.Validation("invalid application configuration")
		}
		if err := validateApplicationText(change.Application); err != nil {
			return err
		}
		if err := validateApplicationRepoURL(change.Application.RepoURL); err != nil {
			return err
		}
		if err := validateApplicationImageRepo(change.Application.ImageRepo); err != nil {
			return err
		}
		if err := validateEnvs(change.Application.Envs); err != nil {
			return err
		}
		if err := m.validateApplicationTargets(ctx, change.Application.Envs); err != nil {
			return err
		}
	case "pipeline.save":
		if change.Pipeline == nil {
			return httpx.Validation("invalid pipeline configuration")
		}
		app, err := m.loadApp(ctx, change.Pipeline.AppID)
		if err != nil {
			return httpx.Conflict("application changed while approval was pending")
		}
		if err := validateStages(change.Pipeline.Stages, app); err != nil {
			return err
		}
	case "argocd.create", "argocd.update":
		if change.Argo == nil {
			return httpx.Validation("invalid Argo CD configuration")
		}
		if _, err := validateArgoConfigInput(change.Argo, change.Kind == "argocd.create"); err != nil {
			return err
		}
	case "helm.repo.create", "helm.repo.update":
		if change.HelmRepo == nil {
			return httpx.Validation("invalid Helm repository configuration")
		}
		if err := validateHelmRepoInput(change.HelmRepo); err != nil {
			return err
		}
		if change.Kind == "helm.repo.update" {
			if change.ID < 1 || change.ExpectedUpdatedAt == nil {
				return httpx.Validation("Helm repository version is unavailable")
			}
			var env string
			var updated time.Time
			if err := m.db.QueryRow(ctx, `SELECT env,updated_at FROM cicd_helm_repos WHERE id=$1`, change.ID).Scan(&env, &updated); err != nil {
				return httpx.Conflict("Helm repository changed while approval was pending")
			}
			if env != change.HelmRepo.Env || !updated.Equal(*change.ExpectedUpdatedAt) {
				return httpx.Conflict("Helm repository changed while approval was pending")
			}
		}
	case "helm.repo.delete", "helm.repo.sync":
		if change.ID < 1 || change.ExpectedUpdatedAt == nil {
			return httpx.Validation("Helm repository version is unavailable")
		}
		var updated time.Time
		if err := m.db.QueryRow(ctx, `SELECT updated_at FROM cicd_helm_repos WHERE id=$1`, change.ID).Scan(&updated); err != nil {
			return httpx.Conflict("Helm repository changed while approval was pending")
		}
		if !updated.Equal(*change.ExpectedUpdatedAt) {
			return httpx.Conflict("Helm repository changed while approval was pending")
		}
	}
	return nil
}
