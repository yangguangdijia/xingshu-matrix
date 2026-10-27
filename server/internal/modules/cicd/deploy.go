package cicd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

var errRejected = errors.New("deployment approval rejected")

// Release is one deploy (or rollback) of an image to an application env.
type Release struct {
	ID            int64      `json:"id"`
	AppID         int64      `json:"app_id"`
	Env           string     `json:"env"`
	RunID         *int64     `json:"run_id"`
	Image         string     `json:"image"`
	PreviousImage string     `json:"previous_image"`
	Kind          string     `json:"kind"`
	Status        string     `json:"status"`
	Message       string     `json:"message"`
	CreatedBy     string     `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	FinishedAt    *time.Time `json:"finished_at"`
}

const relCols = `id,app_id,env,run_id,image,previous_image,kind,status,message,created_by,created_at,finished_at`

func (m *Module) queryReleases(ctx context.Context, q string, args ...any) ([]*Release, error) {
	rows, err := m.db.Query(ctx, `SELECT `+relCols+` FROM cicd_releases WHERE `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Release{}
	for rows.Next() {
		var r Release
		if err := rows.Scan(&r.ID, &r.AppID, &r.Env, &r.RunID, &r.Image, &r.PreviousImage, &r.Kind, &r.Status, &r.Message,
			&r.CreatedBy, &r.CreatedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// deployPayload is the approval ticket payload of type cicd.deploy.
type deployPayload struct {
	Mode         string `json:"mode"` // stage: a pipeline run waits; direct: deploy/rollback API
	RunID        int64  `json:"run_id,omitempty"`
	StageIdx     int    `json:"stage_idx,omitempty"`
	AppID        int64  `json:"app_id"`
	App          string `json:"app"`
	Env          string `json:"env"`
	TargetID     int64  `json:"target_id"`
	TargetConfig string `json:"target_config"`
	Image        string `json:"image"`
	Kind         string `json:"kind"`
}

type deployHandler struct{ m *Module }

func envTargetFingerprint(env *EnvTarget) string {
	if env == nil {
		return ""
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (h *deployHandler) OnApproved(ctx context.Context, t *approval.Ticket) (any, error) {
	var p deployPayload
	if err := t.DecodePayload(&p); err != nil {
		return nil, err
	}
	if h == nil || h.m == nil || h.m.d == nil || h.m.d.RBAC == nil || t == nil {
		return nil, httpx.Conflict("RBAC service unavailable")
	}
	if p.Mode == "stage" {
		run, err := h.m.loadRun(ctx, p.RunID)
		if err != nil {
			return nil, err
		}
		if run.TriggeredByID != t.ApplicantID || run.AppID != p.AppID {
			return nil, httpx.Forbidden("pipeline approval applicant or target changed")
		}
		pipe, err := h.m.loadPipe(ctx, run.PipelineID)
		if err != nil {
			return nil, err
		}
		if p.StageIdx < 0 || p.StageIdx >= len(pipe.Stages) {
			return nil, httpx.Conflict("pipeline approval stage no longer exists")
		}
		stage := pipe.Stages[p.StageIdx]
		if stage.Type != "deploy" || stage.DeployEnv != p.Env || p.Env != t.Env {
			return nil, httpx.Conflict("pipeline approval target environment changed")
		}
		a, err := h.m.loadApp(ctx, run.AppID)
		if err != nil {
			return nil, err
		}
		env, err := a.env(stage.DeployEnv)
		if err != nil || env.Name != t.Env || env.TargetID != p.TargetID ||
			envTargetFingerprint(env) == "" || envTargetFingerprint(env) != p.TargetConfig {
			return nil, httpx.Conflict("pipeline approval application target changed")
		}
		if err := h.m.validateDeploymentTarget(ctx, env); err != nil {
			return nil, err
		}
		if err := h.reauthorize(ctx, t, t.Env); err != nil {
			return nil, err
		}
		tag, err := h.m.db.Exec(ctx, `UPDATE cicd_run_stages SET status='approved' WHERE run_id=$1 AND idx=$2 AND status='awaiting_approval'`, p.RunID, p.StageIdx)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return nil, errors.New("run stage is no longer waiting for approval")
		}
		return gin.H{"run_id": p.RunID, "stage": p.StageIdx, "approved": true}, nil
	}
	a, err := h.m.loadApp(ctx, p.AppID)
	if err != nil {
		return nil, err
	}
	env, err := a.env(p.Env)
	if err != nil {
		return nil, err
	}
	if t.Env != env.Name || p.App != a.Name {
		return nil, httpx.Conflict("deployment approval target changed")
	}
	if env.TargetID != p.TargetID || envTargetFingerprint(env) == "" || envTargetFingerprint(env) != p.TargetConfig {
		return nil, httpx.Conflict("deployment destination changed after approval was requested")
	}
	if err := h.m.validateDeploymentTarget(ctx, env); err != nil {
		return nil, err
	}
	if err := h.reauthorize(ctx, t, env.Name); err != nil {
		return nil, err
	}
	return h.m.doDeploy(ctx, a, env, p.Image, nil, p.Kind, t.ApplicantName, func(f string, x ...any) {
		h.m.log.Info("deploy", "ticket", t.ID, "msg", fmt.Sprintf(f, x...))
	})
}

func (h *deployHandler) reauthorize(ctx context.Context, t *approval.Ticket, env string) error {
	if h == nil || h.m == nil || h.m.d == nil || h.m.d.RBAC == nil {
		return httpx.Conflict("RBAC service unavailable")
	}
	_, permissions, scope, err := h.m.d.RBAC.UserAccess(ctx, t.ApplicantID)
	if err != nil {
		return err
	}
	p := &rbac.Principal{UserID: t.ApplicantID, Username: t.ApplicantName, Permissions: permissions, EnvScope: scope}
	if !p.Can("cicd:deploy") || !p.CanEnv(env) {
		return httpx.Forbidden("applicant deployment permission or environment scope was revoked")
	}
	return nil
}

func (h *deployHandler) OnRejected(ctx context.Context, t *approval.Ticket) error {
	var p deployPayload
	if err := t.DecodePayload(&p); err != nil || p.Mode != "stage" {
		return err
	}
	_, err := h.m.db.Exec(ctx, `UPDATE cicd_run_stages SET status='rejected' WHERE run_id=$1 AND idx=$2 AND status='awaiting_approval'`, p.RunID, p.StageIdx)
	return err
}

// deployStage runs a pipeline deploy stage, waiting for approval when the
// target environment's policy requires it.
func (m *Module) deployStage(ctx context.Context, run *Run, a *Application, s Stage, idx int, logf func(string, ...any)) error {
	env, err := a.env(s.DeployEnv)
	if err != nil {
		return err
	}
	if err := m.validateDeploymentTarget(ctx, env); err != nil {
		return err
	}
	targetConfig := envTargetFingerprint(env)
	if targetConfig == "" {
		return httpx.Conflict("deployment destination could not be pinned")
	}
	image := s.Image
	if image == "" {
		image = a.ImageRepo + ":" + run.ImageTag
	}
	p := &rbac.Principal{UserID: run.TriggeredByID, Username: run.TriggeredBy}
	if m.d.Approval == nil {
		return httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "审批服务不可用，部署已拒绝")
	}
	t, pending, err := m.d.Approval.Gate(ctx, p, approval.CreateInput{
		Type: "cicd.deploy", Title: fmt.Sprintf("部署 %s → %s (%s)", a.Name, env.Name, image), Env: env.Name,
		Reason: fmt.Sprintf("pipeline run #%d", run.ID),
		Payload: deployPayload{Mode: "stage", RunID: run.ID, StageIdx: idx, AppID: a.ID, App: a.Name, Env: env.Name,
			TargetID: env.TargetID, TargetConfig: targetConfig, Image: image, Kind: "deploy"},
	})
	if err != nil {
		return err
	}
	if pending {
		if _, err := m.db.Exec(ctx, `UPDATE cicd_run_stages SET status='awaiting_approval', approval_id=$3 WHERE run_id=$1 AND idx=$2`, run.ID, idx, t.ID); err != nil {
			return err
		}
		logf("environment %s requires approval: waiting on ticket #%d", env.Name, t.ID)
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			var st string
			if err := m.db.QueryRow(ctx, `SELECT status FROM cicd_run_stages WHERE run_id=$1 AND idx=$2`, run.ID, idx).Scan(&st); err != nil {
				continue
			}
			if st == "approved" {
				logf("ticket #%d approved", t.ID)
				m.setStage(run.ID, idx, "running", "", "")
				break
			}
			if st == "rejected" {
				return errRejected
			}
		}
		current, err := m.loadApp(ctx, a.ID)
		if err != nil {
			return err
		}
		currentEnv, err := current.env(s.DeployEnv)
		if err != nil || currentEnv.TargetID != env.TargetID ||
			envTargetFingerprint(currentEnv) != targetConfig {
			return httpx.Conflict("pipeline deployment destination changed after approval")
		}
		if err := m.validateDeploymentTarget(ctx, currentEnv); err != nil {
			return err
		}
		a, env = current, currentEnv
	}
	runID := run.ID
	_, err = m.doDeploy(ctx, a, env, image, &runID, "deploy", run.TriggeredBy, logf)
	return err
}

// doDeploy records a release and performs it synchronously.
func (m *Module) doDeploy(ctx context.Context, a *Application, env *EnvTarget, image string, runID *int64, kind, by string, logf func(string, ...any)) (*Release, error) {
	if err := m.validateDeploymentTarget(ctx, env); err != nil {
		return nil, err
	}
	id, err := m.newRelease(ctx, a, env, image, runID, kind, by)
	if err != nil {
		return nil, err
	}
	return m.execRelease(ctx, id, a, env, image, logf)
}

func (m *Module) validateDeploymentTarget(ctx context.Context, env *EnvTarget) error {
	if env == nil || env.TargetID <= 0 || strings.TrimSpace(env.Name) == "" {
		return httpx.Conflict("deployment target is incomplete")
	}
	var targetEnv string
	if err := m.db.QueryRow(ctx, `SELECT env FROM cicd_targets WHERE id=$1`, env.TargetID).Scan(&targetEnv); err != nil {
		return fmt.Errorf("deployment target %d: %w", env.TargetID, notFound(err))
	}
	if !targetEnvMatches(targetEnv, env.Name) {
		return httpx.Conflict("deployment target environment does not match application environment")
	}
	return nil
}

func targetEnvMatches(targetEnv, applicationEnv string) bool {
	return strings.TrimSpace(targetEnv) != "" && strings.TrimSpace(targetEnv) == strings.TrimSpace(applicationEnv)
}

func (m *Module) newRelease(ctx context.Context, a *Application, env *EnvTarget, image string, runID *int64, kind, by string) (int64, error) {
	var id int64
	err := m.db.QueryRow(ctx, `INSERT INTO cicd_releases(app_id,env,run_id,image,kind,created_by) VALUES($1,$2,$3,$4,$5,$6) RETURNING id`,
		a.ID, env.Name, runID, image, kind, by).Scan(&id)
	return id, err
}

func (m *Module) execRelease(ctx context.Context, id int64, a *Application, env *EnvTarget, image string, logf func(string, ...any)) (*Release, error) {
	if err := m.validateDeploymentTarget(ctx, env); err != nil {
		return nil, err
	}
	prev, err := m.applyImage(ctx, a, env, image, id, logf)
	status, msg := "succeeded", "rolled out"
	if err != nil {
		status, msg = "failed", err.Error()
	}
	_, _ = m.db.Exec(context.Background(), `UPDATE cicd_releases SET status=$2, message=$3, previous_image=$4, finished_at=now() WHERE id=$1`, id, status, msg, prev)
	rels, qerr := m.queryReleases(context.Background(), `id=$1`, id)
	if qerr != nil || len(rels) == 0 {
		return nil, errors.Join(err, qerr)
	}
	return rels[0], err
}

// applyImage rolls image out to env and returns the image it replaced.
func (m *Module) applyImage(ctx context.Context, a *Application, env *EnvTarget, image string, releaseID int64, logf func(string, ...any)) (string, error) {
	cs, runnerNS, kc, err := m.targetClient(ctx, env.TargetID)
	if err != nil {
		return "", err
	}
	timeout := time.Duration(env.RolloutTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	if env.Helm != nil {
		var prev string
		rels, _ := m.queryReleases(ctx, `app_id=$1 AND env=$2 AND status='succeeded' ORDER BY id DESC LIMIT 1`, a.ID, env.Name)
		if len(rels) > 0 {
			prev = rels[0].Image
		}
		return prev, helmDeploy(ctx, cs, runnerNS, kc, env, image, releaseID, timeout, logf)
	}
	prev, err := patchImage(ctx, cs, env.Namespace, env.Deployment, env.Container, image)
	if err != nil {
		return prev, err
	}
	logf("patched deployment %s/%s container %s: %s -> %s", env.Namespace, env.Deployment, env.Container, prev, image)
	return prev, waitRollout(ctx, cs, env.Namespace, env.Deployment, timeout, logf)
}

// patchImage sets a container image with a strategic merge patch and returns the previous image.
func patchImage(ctx context.Context, cs kubeIface, ns, name, container, image string) (string, error) {
	d, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get deployment %s/%s: %w", ns, name, err)
	}
	prev := ""
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == container {
			prev = c.Image
		}
	}
	if prev == "" {
		return "", fmt.Errorf("deployment %s/%s has no container %q", ns, name, container)
	}
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{"matrix.cicd/deployed-at": time.Now().UTC().Format(time.RFC3339)}},
		"spec":     map[string]any{"containers": []map[string]string{{"name": container, "image": image}}},
	}}})
	if _, err := cs.AppsV1().Deployments(ns).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{FieldManager: "matrix-cicd"}); err != nil {
		return prev, fmt.Errorf("patch deployment: %w", err)
	}
	return prev, nil
}

// rolloutStatus mirrors `kubectl rollout status` for a Deployment.
func rolloutStatus(d *appsv1.Deployment) (done bool, msg string, err error) {
	if d.Generation > d.Status.ObservedGeneration {
		return false, "waiting for deployment spec update to be observed", nil
	}
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Reason == "ProgressDeadlineExceeded" {
			return false, "", fmt.Errorf("deployment %q exceeded its progress deadline", d.Name)
		}
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	switch {
	case s.UpdatedReplicas < want:
		return false, fmt.Sprintf("%d of %d updated replicas are available", s.UpdatedReplicas, want), nil
	case s.Replicas > s.UpdatedReplicas:
		return false, fmt.Sprintf("%d old replicas are pending termination", s.Replicas-s.UpdatedReplicas), nil
	case s.AvailableReplicas < s.UpdatedReplicas:
		return false, fmt.Sprintf("%d of %d updated replicas are available", s.AvailableReplicas, s.UpdatedReplicas), nil
	}
	return true, fmt.Sprintf("deployment %q successfully rolled out", d.Name), nil
}

func waitRollout(ctx context.Context, cs kubeIface, ns, name string, timeout time.Duration, logf func(string, ...any)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := ""
	for {
		d, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			done, msg, rerr := rolloutStatus(d)
			if rerr != nil {
				return rerr
			}
			if msg != last {
				logf("%s", msg)
				last = msg
			}
			if done {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("rollout of %s/%s not finished within %s", ns, name, timeout)
		case <-time.After(2 * time.Second):
		}
	}
}

func splitImage(img string) (repo, tag string) {
	slash := strings.LastIndex(img, "/")
	if i := strings.LastIndex(img, ":"); i > slash {
		return img[:i], img[i+1:]
	}
	return img, "latest"
}

// helmDeploy runs `helm upgrade --install` as a Job on the target cluster
// (in the target's runner namespace) using the target kubeconfig.
func helmDeploy(ctx context.Context, cs kubeIface, runnerNS string, kubeconfig []byte, env *EnvTarget, image string, releaseID int64, timeout time.Duration, logf func(string, ...any)) error {
	h := env.Helm
	if err := validateHelmSet([]EnvTarget{*env}); err != nil {
		return err
	}
	rc := &runnerCtx{cs: cs, ns: runnerNS, secret: fmt.Sprintf("cicd-helm-%d", releaseID)}
	if err := rc.createSecret(ctx, map[string]string{"config": string(kubeconfig)}, 0); err != nil {
		return fmt.Errorf("create kubeconfig secret: %w", err)
	}
	defer func() {
		_ = cs.CoreV1().Secrets(runnerNS).Delete(context.Background(), rc.secret, metav1.DeleteOptions{})
	}()
	repo, tag := splitImage(image)
	rk, tk := h.ImageRepoKey, h.ImageTagKey
	if rk == "" {
		rk = "image.repository"
	}
	if tk == "" {
		tk = "image.tag"
	}
	args := []string{"upgrade", "--install", h.Release, h.Chart, "-n", env.Namespace, "--wait", "--hide-notes",
		"--timeout", fmt.Sprintf("%ds", int(timeout.Seconds())), "--set", rk + "=" + repo, "--set", tk + "=" + tag}
	if h.Repo != "" {
		args = append(args, "--repo", h.Repo)
	}
	if h.Version != "" {
		args = append(args, "--version", h.Version)
	}
	for _, s := range h.Set {
		args = append(args, "--set", s)
	}
	spec := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		Volumes:       []corev1.Volume{{Name: "kube", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: rc.secret}}}},
		Containers: []corev1.Container{{Name: stepContainer, Image: HelmImage, Args: args,
			Env:          []corev1.EnvVar{{Name: "KUBECONFIG", Value: "/kube/config"}},
			VolumeMounts: []corev1.VolumeMount{{Name: "kube", MountPath: "/kube", ReadOnly: true}}}},
	}
	logf("helm deployment release=%s namespace=%s version=%s extra_values=%d",
		h.Release, env.Namespace, h.Version, len(h.Set))
	return runJob(ctx, cs, runnerNS, fmt.Sprintf("cicd-helm-%d", releaseID), 0, spec, int(timeout.Seconds())+300, logf)
}

// ---------------------------------------------------------------- HTTP

func (m *Module) listReleases(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	q, args := `app_id=$1`, []any{id}
	if env := c.Query("env"); env != "" {
		q, args = q+` AND env=$2`, append(args, env)
	}
	rels, err := m.queryReleases(c, q+` ORDER BY id DESC LIMIT 200`, args...)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, rels)
}

type deployIn struct {
	Env       string `json:"env" binding:"required"`
	Image     string `json:"image"`
	ReleaseID int64  `json:"release_id"`
	Reason    string `json:"reason"`
}

func (m *Module) deployHTTP(c *gin.Context) { m.deployOrRollback(c, "deploy") }

func (m *Module) rollbackHTTP(c *gin.Context) { m.deployOrRollback(c, "rollback") }

func (m *Module) deployOrRollback(c *gin.Context, kind string) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	var in deployIn
	if !httpx.Bind(c, &in) {
		return
	}
	a, err := m.loadApp(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	env, err := a.env(in.Env)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if err := m.validateDeploymentTarget(c, env); err != nil {
		httpx.Fail(c, err)
		return
	}
	if err := validateHelmSet([]EnvTarget{*env}); err != nil {
		httpx.Fail(c, err)
		return
	}
	p := rbac.PrincipalFrom(c)
	if !p.CanEnv(env.Name) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope: "+env.Name))
		return
	}
	image := in.Image
	if kind == "rollback" {
		// release_id: redeploy that release's image; otherwise restore the image the latest successful release replaced.
		q, args := `app_id=$1 AND env=$2 AND status='succeeded' ORDER BY id DESC LIMIT 1`, []any{a.ID, env.Name}
		if in.ReleaseID > 0 {
			q, args = `app_id=$1 AND env=$2 AND id=$3`, append(args, in.ReleaseID)
		}
		rels, err := m.queryReleases(c, q, args...)
		if err != nil || len(rels) == 0 {
			httpx.Fail(c, httpx.NotFound("no release to roll back to"))
			return
		}
		image = rels[0].PreviousImage
		if in.ReleaseID > 0 {
			image = rels[0].Image
		}
		if image == "" {
			httpx.Fail(c, httpx.Conflict("release has no previous image recorded"))
			return
		}
	}
	if image == "" {
		httpx.Fail(c, httpx.Validation("image required"))
		return
	}
	if m.d.Approval == nil {
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "审批服务不可用，部署已拒绝"))
		return
	}
	t, pending, err := m.d.Approval.Gate(c, p, approval.CreateInput{
		Type: "cicd.deploy", Title: fmt.Sprintf("%s %s → %s (%s)", map[string]string{"deploy": "部署", "rollback": "回滚"}[kind], a.Name, env.Name, image),
		Env: env.Name, Reason: in.Reason,
		Payload: deployPayload{Mode: "direct", AppID: a.ID, App: a.Name, Env: env.Name,
			TargetID: env.TargetID, TargetConfig: envTargetFingerprint(env), Image: image, Kind: kind},
	})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "cicd_application", fmt.Sprint(a.ID))
	audit.SetDetail(c, gin.H{"env": env.Name, "image": image, "kind": kind})
	if pending {
		approval.RespondPending(c, t)
		return
	}
	relID, err := m.newRelease(c, a, env, image, nil, kind, p.Username)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		_, err := m.execRelease(context.Background(), relID, a, env, image, func(f string, x ...any) {
			m.log.Info("deploy", "release", relID, "msg", fmt.Sprintf(f, x...))
		})
		if err != nil {
			m.log.Warn("deploy failed", "release", relID, "err", err)
		}
	}()
	rels, _ := m.queryReleases(c, `id=$1`, relID)
	if len(rels) > 0 {
		httpx.OK(c, rels[0])
		return
	}
	httpx.OK(c, gin.H{"id": relID})
}
