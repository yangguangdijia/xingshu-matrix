package cicd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// DeployerServiceAccount is the ServiceAccount (in the runner namespace) that
// Helm Jobs use on clusters without a stored kubeconfig (agent clusters). The
// cluster onboarding manifest creates it when CI/CD deploy access is enabled.
const DeployerServiceAccount = "matrix-cicd-deployer"

const defaultRunnerNamespace = "matrix-cicd"

const (
	msgTargetKubeconfigRejected = "执行集群不再接受 kubeconfig，请先在 集群管理 中接入该集群，再选择已注册集群创建执行集群"
	msgDeployerSAMissing        = "目标集群缺少 matrix-cicd-deployer ServiceAccount，请在集群接入清单中启用 CI/CD 部署权限"
)

// targetIn is the create / staged-change body of an execution target. New
// targets reference a registered cluster; Kubeconfig is accepted only so that
// it can be rejected explicitly.
type targetIn struct {
	Name       string `json:"name" binding:"required,max=64"`
	ClusterID  int64  `json:"cluster_id"`
	Namespace  string `json:"namespace"`
	Env        string `json:"env" binding:"max=64"`
	Kubeconfig string `json:"kubeconfig,omitempty"`
}

// targetUpdateIn links a target to a registered cluster and/or changes its
// runner namespace.
type targetUpdateIn struct {
	ClusterID  int64   `json:"cluster_id"`
	Namespace  *string `json:"namespace"`
	Env        string  `json:"env" binding:"max=64"`
	Kubeconfig string  `json:"kubeconfig,omitempty"`
}

// targetRow is a stored execution target. Encrypted kubeconfig is only read
// for legacy rows that have no cluster_id.
type targetRow struct {
	ID            int64
	Name          string
	Namespace     string
	Env           string
	ClusterID     int64
	KubeconfigEnc string
	UpdatedAt     time.Time
}

func (t *targetRow) linked() bool { return t != nil && t.ClusterID > 0 }

// targetConn is a resolved execution target.
type targetConn struct {
	cs        kubernetes.Interface
	namespace string
	row       *targetRow
	cluster   *app.ClusterRef // set for cluster-linked targets
	legacyKC  []byte          // set for legacy kubeconfig targets
}

// helmAuth tells a Helm Job how to reach the target cluster: a kubeconfig
// mounted from a Secret, or the in-cluster ServiceAccount token.
type helmAuth struct {
	kubeconfig     []byte
	serviceAccount string
}

func defaultRESTKube(cfg *rest.Config) (kubernetes.Interface, error) {
	return kubernetes.NewForConfig(cfg)
}

// clusterAccess returns the shared cluster registry or fails closed.
func (m *Module) clusterAccess() (app.ClusterAccess, error) {
	if m == nil || m.d == nil || m.d.Clusters == nil {
		return nil, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "集群注册服务不可用，已拒绝访问执行集群")
	}
	return m.d.Clusters, nil
}

func (m *Module) loadTarget(ctx context.Context, id int64) (*targetRow, error) {
	if m.loadTargetRow != nil {
		return m.loadTargetRow(ctx, id)
	}
	var row targetRow
	var clusterID *int64
	err := m.db.QueryRow(ctx, `SELECT id,name,namespace,env,cluster_id,COALESCE(kubeconfig_enc,''),updated_at FROM cicd_targets WHERE id=$1`, id).
		Scan(&row.ID, &row.Name, &row.Namespace, &row.Env, &clusterID, &row.KubeconfigEnc, &row.UpdatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	if clusterID != nil {
		row.ClusterID = *clusterID
	}
	return &row, nil
}

// linkedCluster resolves the registered cluster of a linked target and checks
// that its environment still equals the target environment.
func (m *Module) linkedCluster(ctx context.Context, row *targetRow) (app.ClusterRef, error) {
	access, err := m.clusterAccess()
	if err != nil {
		return app.ClusterRef{}, err
	}
	ref, err := access.Cluster(ctx, row.ClusterID)
	if err != nil {
		if errors.Is(err, app.ErrClusterNotFound) {
			return app.ClusterRef{}, httpx.Conflict(fmt.Sprintf("执行集群 %s 关联的集群 #%d 已不存在", row.Name, row.ClusterID))
		}
		return app.ClusterRef{}, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "无法读取执行集群关联的集群").Wrap(err)
	}
	if strings.TrimSpace(ref.Env) != strings.TrimSpace(row.Env) {
		return app.ClusterRef{}, httpx.Conflict(fmt.Sprintf("执行集群 %s 的环境 %q 与集群 %s 当前环境 %q 不一致，已拒绝", row.Name, row.Env, ref.Name, ref.Env))
	}
	return ref, nil
}

// targetClient resolves an execution target to a clientset and runner
// namespace. Cluster-linked targets go through Deps.Clusters; legacy targets
// decrypt their own kubeconfig.
func (m *Module) targetClient(ctx context.Context, id int64) (*targetConn, error) {
	row, err := m.loadTarget(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("target %d: %w", id, err)
	}
	conn := &targetConn{namespace: row.Namespace, row: row}
	if !row.linked() {
		if row.KubeconfigEnc == "" {
			return nil, httpx.Conflict(fmt.Sprintf("执行集群 %s 未关联集群", row.Name))
		}
		if m.d == nil || m.d.Crypto == nil {
			return nil, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "凭据加密服务不可用")
		}
		kc, err := m.d.Crypto.Decrypt(row.KubeconfigEnc)
		if err != nil {
			return nil, err
		}
		kube := m.kube
		if kube == nil {
			kube = defaultKube
		}
		if conn.cs, err = kube([]byte(kc)); err != nil {
			return nil, err
		}
		conn.legacyKC = []byte(kc)
		return conn, nil
	}
	ref, err := m.linkedCluster(ctx, row)
	if err != nil {
		return nil, err
	}
	access, _ := m.clusterAccess()
	cfg, err := access.RESTConfig(ctx, row.ClusterID)
	if err != nil {
		if errors.Is(err, app.ErrClusterOffline) {
			return nil, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable,
				fmt.Sprintf("集群 %s 的 Agent 未连接，无法访问执行集群", ref.Name)).Wrap(err)
		}
		return nil, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable,
			fmt.Sprintf("无法连接集群 %s", ref.Name)).Wrap(err)
	}
	if cfg == nil {
		return nil, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, fmt.Sprintf("无法连接集群 %s", ref.Name))
	}
	cfg.Timeout = 0 // log streams are long-lived
	restKube := m.restKube
	if restKube == nil {
		restKube = defaultRESTKube
	}
	if conn.cs, err = restKube(cfg); err != nil {
		return nil, err
	}
	conn.cluster = &ref
	return conn, nil
}

// helmAuthFor chooses how a Helm Job reaches the cluster of conn.
func (m *Module) helmAuthFor(ctx context.Context, conn *targetConn) (helmAuth, error) {
	if conn == nil || conn.row == nil {
		return helmAuth{}, errors.New("execution target is unavailable")
	}
	if !conn.row.linked() {
		if len(conn.legacyKC) == 0 {
			return helmAuth{}, httpx.Conflict("执行集群缺少 kubeconfig")
		}
		return helmAuth{kubeconfig: conn.legacyKC}, nil
	}
	access, err := m.clusterAccess()
	if err != nil {
		return helmAuth{}, err
	}
	kc, err := access.Kubeconfig(ctx, conn.row.ClusterID)
	switch {
	case errors.Is(err, app.ErrNoKubeconfig):
		return helmAuth{serviceAccount: DeployerServiceAccount}, nil
	case err != nil:
		return helmAuth{}, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "无法读取集群凭据").Wrap(err)
	case len(kc) == 0:
		return helmAuth{}, httpx.Conflict("集群未返回 kubeconfig，已拒绝 Helm 部署")
	}
	return helmAuth{kubeconfig: kc}, nil
}

// ---------------------------------------------------------------- HTTP

type targetListRow struct {
	ID        int64
	Name      string
	Namespace string
	Env       string
	ClusterID int64
	Legacy    bool
	CreatedAt time.Time
}

// targetListItem shapes one list entry. It never carries credential material:
// legacy rows only get a fixed mask.
func targetListItem(row targetListRow, ref *app.ClusterRef) gin.H {
	item := gin.H{
		"id": row.ID, "name": row.Name, "namespace": row.Namespace, "env": row.Env,
		"cluster_id": nil, "cluster_name": "", "connect_mode": "",
		"legacy_kubeconfig": row.Legacy, "created_at": row.CreatedAt,
	}
	if row.ClusterID > 0 {
		item["cluster_id"] = row.ClusterID
	}
	if ref != nil {
		item["cluster_name"] = ref.Name
		item["cluster_env"] = ref.Env
		item["connect_mode"] = ref.ConnectMode
	}
	if row.Legacy {
		item["kubeconfig"] = "********"
		item["connect_mode"] = app.ClusterConnectKubeconfig
	}
	return item
}

func (m *Module) listTargets(c *gin.Context) {
	rows, err := m.db.Query(c, `SELECT id,name,namespace,env,COALESCE(cluster_id,0),COALESCE(kubeconfig_enc,'')<>'',created_at
		FROM cicd_targets ORDER BY id`)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	var list []targetListRow
	for rows.Next() {
		var r targetListRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Namespace, &r.Env, &r.ClusterID, &r.Legacy, &r.CreatedAt); err != nil {
			rows.Close()
			httpx.Fail(c, err)
			return
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		httpx.Fail(c, err)
		return
	}
	access, _ := m.clusterAccess()
	refs := map[int64]*app.ClusterRef{}
	out := make([]gin.H, 0, len(list))
	for _, r := range list {
		var ref *app.ClusterRef
		if r.ClusterID > 0 && access != nil {
			cached, seen := refs[r.ClusterID]
			if !seen {
				if got, err := access.Cluster(c, r.ClusterID); err == nil {
					cached = &got
				}
				refs[r.ClusterID] = cached
			}
			ref = cached
		}
		out = append(out, targetListItem(r, ref))
	}
	httpx.OK(c, out)
}

func validTargetNamespace(ns string) bool { return len(ns) <= 63 && nameRe.MatchString(ns) }

// validateTargetCluster checks that clusterID is registered and resolves the
// target environment (defaulting to the cluster env). An explicit env that
// differs from the cluster env is rejected.
func (m *Module) validateTargetCluster(ctx context.Context, clusterID int64, env string) (app.ClusterRef, string, error) {
	if clusterID <= 0 {
		return app.ClusterRef{}, "", httpx.Validation("cluster_id is required: 请选择已在 集群管理 中注册的集群")
	}
	access, err := m.clusterAccess()
	if err != nil {
		return app.ClusterRef{}, "", err
	}
	ref, err := access.Cluster(ctx, clusterID)
	if err != nil {
		if errors.Is(err, app.ErrClusterNotFound) {
			return app.ClusterRef{}, "", httpx.Validation(fmt.Sprintf("集群 #%d 未注册，请先在 集群管理 中接入", clusterID))
		}
		return app.ClusterRef{}, "", httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "无法读取集群注册信息").Wrap(err)
	}
	clusterEnv := strings.TrimSpace(ref.Env)
	env = strings.TrimSpace(env)
	if env == "" {
		env = clusterEnv
	}
	if env != clusterEnv {
		return app.ClusterRef{}, "", httpx.Validation(fmt.Sprintf("执行集群环境 %q 必须与集群 %s 的环境 %q 一致", env, ref.Name, clusterEnv))
	}
	if env != "" && !validArgoEnv(env) {
		return app.ClusterRef{}, "", httpx.Validation("env must use up to 64 letters, digits, dots, hyphens or underscores")
	}
	return ref, env, nil
}

// prepareTargetCreate validates a create request and returns the staged change.
func (m *Module) prepareTargetCreate(ctx context.Context, p *rbac.Principal, in *targetIn) (cicdConfigMutation, error) {
	if strings.TrimSpace(in.Kubeconfig) != "" {
		return cicdConfigMutation{}, httpx.Validation(msgTargetKubeconfigRejected)
	}
	in.Kubeconfig = ""
	in.Name, in.Namespace = strings.TrimSpace(in.Name), strings.TrimSpace(in.Namespace)
	if inlineSecretText(in.Name) || inlineSecretText(in.Namespace) {
		return cicdConfigMutation{}, httpx.Validation("target configuration must not contain inline secrets")
	}
	if in.Namespace == "" {
		in.Namespace = defaultRunnerNamespace
	}
	if !validTargetNamespace(in.Namespace) {
		return cicdConfigMutation{}, httpx.Validation("namespace must be a DNS label")
	}
	_, env, err := m.validateTargetCluster(ctx, in.ClusterID, in.Env)
	if err != nil {
		return cicdConfigMutation{}, err
	}
	in.Env = env
	if !p.CanEnv(in.Env) {
		return cicdConfigMutation{}, httpx.Forbidden("environment not in your scope: " + in.Env)
	}
	return cicdConfigMutation{Kind: "target.create", Target: in, Envs: []string{in.Env}}, nil
}

func (m *Module) createTarget(c *gin.Context) {
	var in targetIn
	if !httpx.Bind(c, &in) {
		return
	}
	change, err := m.prepareTargetCreate(c, rbac.PrincipalFrom(c), &in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if m.gateConfigMutation(c, change, nil) {
		return
	}
	id, err := m.applyCICDConfigMutation(c, change)
	if err != nil {
		httpx.Fail(c, httpx.Conflict("target name exists").Wrap(errors.New("configuration write failed")))
		return
	}
	audit.SetResource(c, "cicd_target", fmt.Sprint(id))
	audit.SetDetail(c, gin.H{"cluster_id": in.ClusterID, "env": in.Env})
	httpx.Created(c, gin.H{"id": id, "name": in.Name, "namespace": in.Namespace, "env": in.Env, "cluster_id": in.ClusterID})
}

// prepareTargetUpdate validates an update of cur and returns the staged change.
// Linking sets cluster_id (and clears the stored kubeconfig on apply); a
// linked target can never go back to a raw kubeconfig.
func (m *Module) prepareTargetUpdate(ctx context.Context, p *rbac.Principal, cur *targetRow, in *targetUpdateIn) (cicdConfigMutation, error) {
	if strings.TrimSpace(in.Kubeconfig) != "" {
		return cicdConfigMutation{}, httpx.Validation(msgTargetKubeconfigRejected)
	}
	next := &targetIn{Name: cur.Name, ClusterID: cur.ClusterID, Namespace: cur.Namespace, Env: cur.Env}
	if in.Namespace != nil {
		next.Namespace = strings.TrimSpace(*in.Namespace)
		if next.Namespace == "" {
			next.Namespace = defaultRunnerNamespace
		}
	}
	if inlineSecretText(next.Namespace) || !validTargetNamespace(next.Namespace) {
		return cicdConfigMutation{}, httpx.Validation("namespace must be a DNS label")
	}
	if in.ClusterID > 0 {
		next.ClusterID = in.ClusterID
	}
	if next.ClusterID > 0 {
		// A target that already has an environment keeps it: the cluster must match.
		want := strings.TrimSpace(cur.Env)
		if want == "" {
			want = strings.TrimSpace(in.Env)
		} else if env := strings.TrimSpace(in.Env); env != "" && env != want {
			return cicdConfigMutation{}, httpx.Validation("执行集群环境不可通过关联操作修改")
		}
		_, env, err := m.validateTargetCluster(ctx, next.ClusterID, want)
		if err != nil {
			return cicdConfigMutation{}, err
		}
		next.Env = env
	} else if strings.TrimSpace(in.Env) != "" && strings.TrimSpace(in.Env) != strings.TrimSpace(cur.Env) {
		return cicdConfigMutation{}, httpx.Validation("旧版执行集群只能在关联集群时确定环境")
	}
	for _, env := range []string{cur.Env, next.Env} {
		if env = strings.TrimSpace(env); env != "" && !p.CanEnv(env) {
			return cicdConfigMutation{}, httpx.Forbidden("environment not in your scope: " + env)
		}
	}
	updated := cur.UpdatedAt
	return cicdConfigMutation{Kind: "target.update", ID: cur.ID, Target: next,
		Envs: []string{cur.Env, next.Env}, ExpectedUpdatedAt: &updated}, nil
}

func (m *Module) updateTarget(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	var in targetUpdateIn
	if !httpx.Bind(c, &in) {
		return
	}
	if strings.TrimSpace(in.Kubeconfig) != "" {
		httpx.Fail(c, httpx.Validation(msgTargetKubeconfigRejected))
		return
	}
	cur, err := m.loadTarget(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	change, err := m.prepareTargetUpdate(c, rbac.PrincipalFrom(c), cur, &in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if m.gateConfigMutation(c, change, nil) {
		return
	}
	if _, err := m.applyCICDConfigMutation(c, change); err != nil {
		httpx.Fail(c, httpx.Conflict("target changed while the request was pending").Wrap(errors.New("configuration write failed")))
		return
	}
	audit.SetResource(c, "cicd_target", fmt.Sprint(id))
	audit.SetDetail(c, gin.H{"cluster_id": change.Target.ClusterID, "env": change.Target.Env, "linked_from_legacy": !cur.linked() && change.Target.ClusterID > 0})
	httpx.OK(c, gin.H{"id": id, "name": change.Target.Name, "namespace": change.Target.Namespace,
		"env": change.Target.Env, "cluster_id": change.Target.ClusterID})
}

// validateStagedTarget re-validates a staged target change at approval time:
// the cluster may have been removed or moved to another environment while the
// change waited.
func (m *Module) validateStagedTarget(ctx context.Context, change cicdConfigMutation) error {
	t := change.Target
	if t == nil {
		return httpx.Validation("invalid target configuration")
	}
	if strings.TrimSpace(t.Kubeconfig) != "" {
		return httpx.Validation(msgTargetKubeconfigRejected)
	}
	if inlineSecretText(t.Name) || inlineSecretText(t.Namespace) || !validTargetNamespace(t.Namespace) {
		return httpx.Validation("target configuration must not contain inline secrets")
	}
	if change.Kind == "target.update" {
		if change.ID < 1 || change.ExpectedUpdatedAt == nil {
			return httpx.Validation("target version is unavailable")
		}
		cur, err := m.loadTarget(ctx, change.ID)
		if err != nil || !cur.UpdatedAt.Equal(*change.ExpectedUpdatedAt) {
			return httpx.Conflict("target changed while approval was pending")
		}
		if t.ClusterID == 0 {
			if cur.linked() {
				return httpx.Conflict("target changed while approval was pending")
			}
			return nil // namespace-only change of a legacy target
		}
	}
	// The env was fixed when the change was staged: require an exact match now.
	_, env, err := m.validateTargetCluster(ctx, t.ClusterID, t.Env)
	if err != nil {
		return err
	}
	if env != strings.TrimSpace(t.Env) {
		return httpx.Conflict("cluster environment changed while approval was pending")
	}
	return nil
}

// ---------------------------------------------------------------- legacy

func defaultKube(kc []byte) (kubernetes.Interface, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kc)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	cfg.Timeout = 0 // log streams are long-lived
	return kubernetes.NewForConfig(cfg)
}
