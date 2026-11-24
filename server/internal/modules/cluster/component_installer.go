package cluster

// component_installer.go — *Module implements app.ComponentInstaller so other
// modules (aiops) can install reviewed catalog components and poll their state
// without going through the HTTP handlers.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

var _ app.ComponentInstaller = (*Module)(nil)

// errComponentInstallerNotWired is returned until the HTTP install flow
// (preflight digest → approval ticket → startComponentOperation) is split into
// a gin-free function that InstallComponent can call.
var errComponentInstallerNotWired = errors.New("组件安装内部入口尚未从 HTTP 处理器中拆出，暂不支持由 AI 运维直接安装；请在集群组件页面执行预检与安装")

// componentInstallStarter is the seam for the gin-free install entry point.
// nil until installComponent is refactored (see report).
var componentInstallStarter func(ctx context.Context, m *Module, p *rbac.Principal, cl *Cluster, in componentInstallRequest) (string, error)

// InstallComponent validates and starts one component installation and
// returns its operation id.
func (m *Module) InstallComponent(ctx context.Context, p *rbac.Principal, req app.ComponentInstallRequest) (string, error) {
	key, err := compValidateInstallRequest(req)
	if err != nil {
		return "", err
	}
	if m == nil || m.d == nil || m.d.DB == nil || m.d.Crypto == nil {
		return "", errors.New("集群组件服务不可用")
	}
	cl, err := m.loadCluster(ctx, req.ClusterID)
	if err != nil {
		return "", fmt.Errorf("集群 %d 不存在或无法读取：%w", req.ClusterID, err)
	}
	if err := compCheckPrincipal(p, cl.Env); err != nil {
		return "", err
	}
	in := componentInstallRequest{Component: key, Environment: cl.Env, Namespace: key, ReleaseName: key,
		Version: req.Version, Values: req.Values, Confirm: true, Reason: "AI 运维工单发起的组件安装"}
	if in.Version == "" {
		if def, ok := componentDefinition(key); ok && len(def.Versions) == 1 {
			in.Version = def.Versions[0]
		}
	}
	if _, _, err := validateComponentRequest(key, in); err != nil {
		return "", fmt.Errorf("组件 %s 的安装参数校验失败：%w", key, err)
	}
	installed, err := m.compInstalledSet(ctx, cl)
	if err != nil {
		return "", err
	}
	if err := compPlanCheck(key, installed); err != nil {
		return "", err
	}
	if componentInstallStarter == nil {
		return "", errComponentInstallerNotWired
	}
	return componentInstallStarter(ctx, m, p, cl, in)
}

// ComponentStatus reports tier, install state and missing dependencies.
func (m *Module) ComponentStatus(ctx context.Context, clusterID int64, key string) (app.ComponentStatus, error) {
	key = strings.TrimSpace(key)
	if err := compValidateKey(key); err != nil {
		return app.ComponentStatus{}, err
	}
	st := app.ComponentStatus{Key: key, Tier: componentTierFor(key).Tier}
	if clusterID <= 0 {
		return st, errors.New("cluster_id 必须为正整数")
	}
	if m == nil || m.d == nil || m.d.DB == nil {
		return st, errors.New("集群组件服务不可用")
	}
	cl, err := m.loadCluster(ctx, clusterID)
	if err != nil {
		return st, fmt.Errorf("集群 %d 不存在或无法读取：%w", clusterID, err)
	}
	caps, err := m.compCapabilities(ctx, cl)
	if err != nil {
		return st, err
	}
	capability := caps[key]
	st.Installed = capability.Available
	st.Healthy = capability.Available && compCapabilityHealthy(capability)
	st.Message = capability.Error
	st.MissingDependencies = compMissingDependencies(key, installedFromCapabilities(caps))
	return st, nil
}

func (m *Module) compCapabilities(ctx context.Context, cl *Cluster) (map[string]ClusterCapability, error) {
	kc, err := m.kube(ctx, cl)
	if err != nil || kc == nil {
		return nil, errors.New("集群客户端不可用，无法探测组件状态")
	}
	workloads := listComponentWorkloads(ctx, kc)
	out := map[string]ClusterCapability{}
	for _, def := range sortedComponentCatalog() {
		out[def.Key] = componentCatalogCapability(ctx, kc, def.Key, workloads)
	}
	return out, nil
}

func (m *Module) compInstalledSet(ctx context.Context, cl *Cluster) (map[string]bool, error) {
	caps, err := m.compCapabilities(ctx, cl)
	if err != nil {
		return nil, err
	}
	return installedFromCapabilities(caps), nil
}

func compCapabilityHealthy(c ClusterCapability) bool {
	if c.Error != "" {
		return false
	}
	switch strings.ToLower(c.State) {
	case "available", "installed", "registered":
		return true
	}
	return false
}

// --- pure logic (unit tested) ---

func compValidateKey(key string) error {
	if key == "" {
		return errors.New("组件 key 不能为空")
	}
	if _, ok := componentDependencyGraph()[key]; !ok {
		return fmt.Errorf("未知组件 %q，不在已审核的组件目录中", key)
	}
	return nil
}

func compValidateInstallRequest(req app.ComponentInstallRequest) (string, error) {
	if req.ClusterID <= 0 {
		return "", errors.New("cluster_id 必须为正整数")
	}
	key := strings.TrimSpace(req.Key)
	if err := compValidateKey(key); err != nil {
		return "", err
	}
	return key, nil
}

func compCheckPrincipal(p *rbac.Principal, env string) error {
	if p == nil {
		return errors.New("缺少操作人身份，拒绝安装组件")
	}
	if strings.TrimSpace(env) == "" || !p.CanEnv(env) {
		return fmt.Errorf("操作人无权访问环境 %q 的集群", env)
	}
	if !componentPrincipalCanInstall(p) {
		return fmt.Errorf("操作人缺少组件安装权限（需要 %s、%s、%s）", componentWritePermission, PermRBACWrite, PermConfigWrite)
	}
	return nil
}

// compMissingDependencies lists dependencies of key that are not installed.
func compMissingDependencies(key string, installed map[string]bool) []string {
	graph := componentDependencyGraph()
	seen := map[string]bool{}
	var out []string
	var walk func(k string)
	walk = func(k string) {
		for _, dep := range graph[k].DependsOn {
			if seen[dep] {
				continue
			}
			seen[dep] = true
			if !installed[dep] {
				out = append(out, dep)
			}
			walk(dep)
		}
	}
	walk(key)
	sort.Strings(out)
	return out
}

// compPlanCheck rejects an install whose dependencies are missing or which
// conflicts with an installed component.
func compPlanCheck(key string, installed map[string]bool) error {
	_, missing, conflicts, err := planInstall([]string{key}, installed)
	if err != nil {
		return fmt.Errorf("组件 %s 安装规划失败：%w", key, err)
	}
	missing = uniqueSortedStrings(append(missing, compMissingDependencies(key, installed)...))
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "缺少依赖："+strings.Join(missing, "、"))
	}
	if len(conflicts) > 0 {
		cs := make([]string, 0, len(conflicts))
		for _, c := range conflicts {
			cs = append(cs, c[0]+" 与 "+c[1])
		}
		parts = append(parts, "存在冲突："+strings.Join(cs, "、"))
	}
	if len(parts) > 0 {
		return fmt.Errorf("拒绝安装组件 %s，%s", key, strings.Join(parts, "；"))
	}
	return nil
}

func init() { componentInstallStarter = compSubmitInstall }

// compSubmitInstall is the gin-free counterpart of installComponent: fetch and
// verify the curated chart (the same digest verification preflight uses),
// record the operation, pass the approval gate, then start it.
func compSubmitInstall(ctx context.Context, m *Module, p *rbac.Principal, cl *Cluster, in componentInstallRequest) (string, error) {
	if m.d.Approval == nil || m.d.RBAC == nil {
		return "", errors.New("组件安装服务不可用（审批或权限服务未就绪）")
	}
	def, vals, err := validateComponentRequest(in.Component, in)
	if err != nil {
		return "", fmt.Errorf("组件安装参数校验失败：%w", err)
	}
	kc, err := m.kube(ctx, cl)
	if err != nil {
		return "", errors.New("无法连接集群")
	}
	in.Environment, in.Component = cl.Env, def.Key
	chartData, digest, err := fetchVerifiedComponentChart(ctx, def, "")
	if err != nil {
		return "", errors.New("获取或校验官方 Chart 失败")
	}
	in.ChartDigest = digest
	if err = prepareVerifiedComponentChart(ctx, kc, chartData, vals, in); err != nil {
		return "", fmt.Errorf("Chart 预检失败：%w", err)
	}
	id := uuid.NewString()
	raw, err := json.Marshal(componentOperationSnapshot{
		ClusterID: cl.ID, ClusterName: cl.Name, Environment: cl.Env, ClusterFingerprint: componentClusterFingerprint(cl), Component: def.Key,
		ChartDigest: digest, Request: in,
	})
	if err != nil {
		return "", err
	}
	encrypted, err := m.d.Crypto.EncryptBytes(raw, []byte("cluster-component:"+id))
	if err != nil {
		return "", errors.New("组件配置加密不可用")
	}
	if _, err = m.d.DB.Exec(ctx, `INSERT INTO cluster_component_operations
		(id,cluster_id,cluster_name,environment,component,namespace,release_name,chart_version,chart_digest,config_encrypted,status,created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'preflight',$11)`,
		id, cl.ID, cl.Name, cl.Env, def.Key, in.Namespace, in.ReleaseName, in.Version, digest, encrypted, p.UserID); err != nil {
		return "", err
	}
	redacted := make(map[string]string, len(in.Values))
	for k := range in.Values {
		redacted[k] = "[redacted]"
	}
	pl := componentTicketPayload{OperationID: id, ClusterID: cl.ID, Environment: cl.Env,
		Component: def.Key, Namespace: in.Namespace, ReleaseName: in.ReleaseName,
		Version: in.Version, ChartDigest: digest, Values: redacted}
	ticket, pending, err := m.d.Approval.Gate(ctx, p, approval.CreateInput{Type: componentApprovalType,
		Title: fmt.Sprintf("[%s] install %s/%s", cl.Name, in.Namespace, in.ReleaseName), Env: cl.Env, Reason: in.Reason, Payload: pl})
	if err != nil {
		m.finishComponentOperation(context.WithoutCancel(ctx), id, "failed", "approval request failed", "")
		return "", errors.New("创建审批单失败")
	}
	if pending {
		if _, err := m.d.DB.Exec(ctx, `UPDATE cluster_component_operations
			SET approval_id=$2,status='pending_approval',updated_at=now()
			WHERE id=$1 AND status='preflight' AND approval_id IS NULL`, id, ticket.ID); err != nil {
			return "", errors.New("关联审批单失败")
		}
		return id, nil
	}
	if _, err := m.startComponentOperation(ctx, id, cl, chartData, vals, in, false); err != nil {
		return id, errors.New("组件安装已提交但启动失败，需要在组件页核对")
	}
	return id, nil
}
