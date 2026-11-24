package aiops

// actions_component.go — catalog component actions backed by
// Deps.Components (implemented by the cluster module).
//
// Read:   component.status
// Change: component.install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
)

const (
	compPermExecute   = "aiops:execute"
	compVerifyTimeout = 10 * time.Minute
	compOpMarker      = "operation_id="
)

// Test seams.
var (
	compVerifyInterval = 5 * time.Second
	compNow            = time.Now
)

var compKeyRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
var compVersionRE = regexp.MustCompile(`^v?[0-9A-Za-z][0-9A-Za-z.+-]{0,63}$`)

type compParams struct {
	ClusterID int64  `json:"cluster_id"`
	Key       string `json:"key"`
	Version   string `json:"version,omitempty"`
}

func compSchema(withVersion bool) json.RawMessage {
	props := `"cluster_id":{"type":"integer","minimum":1,"description":"集群 ID"},"key":{"type":"string","description":"组件目录 key，如 cert-manager"}`
	if withVersion {
		props += `,"version":{"type":"string","description":"版本，留空使用推荐版本"}`
	}
	return json.RawMessage(`{"type":"object","additionalProperties":false,"required":["cluster_id","key"],"properties":{` + props + `}}`)
}

// compParse decodes and normalises params. cluster_id falls back to the case
// cluster; a mismatch with the case cluster is rejected.
func compParse(ec *ExecContext, raw json.RawMessage, allowVersion bool) (compParams, error) {
	var p compParams
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if len(bytes.TrimSpace(raw)) == 0 {
		return p, errors.New("缺少参数 cluster_id 与 key")
	}
	if err := dec.Decode(&p); err != nil {
		return p, fmt.Errorf("参数格式不正确：%v", err)
	}
	if p.ClusterID == 0 && ec != nil {
		p.ClusterID = ec.ClusterID
	}
	if p.ClusterID <= 0 {
		return p, errors.New("cluster_id 必须为正整数")
	}
	if ec != nil && ec.ClusterID > 0 && p.ClusterID != ec.ClusterID {
		return p, fmt.Errorf("cluster_id %d 与工单关联集群 %d 不一致", p.ClusterID, ec.ClusterID)
	}
	p.Key = strings.TrimSpace(p.Key)
	if !compKeyRE.MatchString(p.Key) {
		return p, fmt.Errorf("组件 key「%s」不合法", p.Key)
	}
	p.Version = strings.TrimSpace(p.Version)
	if p.Version != "" {
		if !allowVersion {
			return p, errors.New("查看组件状态不接受 version 参数")
		}
		if !compVersionRE.MatchString(p.Version) {
			return p, fmt.Errorf("版本「%s」不合法", p.Version)
		}
	}
	return p, nil
}

func compInstaller(ec *ExecContext) (app.ComponentInstaller, error) {
	if ec == nil || ec.Deps == nil || ec.Deps.Components == nil {
		return nil, errors.New("组件安装服务未接入（集群模块未提供 ComponentInstaller），无法执行组件操作")
	}
	return ec.Deps.Components, nil
}

func compFormatStatus(st app.ComponentStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "组件：%s\n分级：%s\n", st.Key, st.Tier)
	state := "未安装"
	if st.Installed {
		state = "已安装"
		if !st.Healthy {
			state = "已安装（异常）"
		}
	}
	fmt.Fprintf(&b, "状态：%s\n", state)
	if st.Version != "" {
		fmt.Fprintf(&b, "版本：%s\n", st.Version)
	}
	if len(st.MissingDependencies) > 0 {
		fmt.Fprintf(&b, "缺失依赖：%s\n", strings.Join(st.MissingDependencies, "、"))
	}
	if st.Message != "" {
		fmt.Fprintf(&b, "说明：%s\n", st.Message)
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---- component.status ----

type compStatusAction struct{}

func (compStatusAction) Name() string { return "component.status" }

func (compStatusAction) Spec() ActionSpec {
	return ActionSpec{Name: "component.status", Title: "查看组件状态",
		Description: "查看集群中某个目录组件的分级、安装状态、健康状况与缺失依赖。",
		Risk:        RiskRead, Channel: "component", Params: compSchema(false), Permission: compPermExecute, ReadOnly: true}
}

func (a compStatusAction) Validate(_ context.Context, ec *ExecContext, raw json.RawMessage) (json.RawMessage, string, error) {
	p, err := compParse(ec, raw, false)
	if err != nil {
		return nil, "", err
	}
	out, _ := json.Marshal(p)
	return out, RiskRead, nil
}

func (a compStatusAction) Describe(_ context.Context, ec *ExecContext, raw json.RawMessage) (string, string, string, error) {
	p, err := compParse(ec, raw, false)
	if err != nil {
		return "", "", "", err
	}
	title := fmt.Sprintf("查看集群 %d 的组件 %s 状态", p.ClusterID, p.Key)
	return title, "只读：探测组件是否安装及依赖情况。", "在平台「集群 → 组件」页面查看 " + p.Key + " 的状态。", nil
}

func (a compStatusAction) Execute(ctx context.Context, ec *ExecContext, raw json.RawMessage) (string, error) {
	p, err := compParse(ec, raw, false)
	if err != nil {
		return "", err
	}
	ci, err := compInstaller(ec)
	if err != nil {
		return "", err
	}
	st, err := ci.ComponentStatus(ctx, p.ClusterID, p.Key)
	if err != nil {
		return "", fmt.Errorf("查询组件 %s 状态失败：%w", p.Key, err)
	}
	return compFormatStatus(st), nil
}

func (compStatusAction) Rollback(context.Context, *ExecContext, json.RawMessage, string) (string, error) {
	return "", ErrNoRollback
}

// ---- component.install ----

type compInstallAction struct{}

func (compInstallAction) Name() string { return "component.install" }

func (compInstallAction) Spec() ActionSpec {
	return ActionSpec{Name: "component.install", Title: "安装集群组件",
		Description: "从已审核的组件目录安装或升级一个组件；缺少依赖或与已装组件冲突时拒绝。安装异步执行，随后轮询至已安装（最长 10 分钟）。",
		Risk:        RiskChange, Channel: "component", Params: compSchema(true), Permission: compPermExecute}
}

func (a compInstallAction) Validate(_ context.Context, ec *ExecContext, raw json.RawMessage) (json.RawMessage, string, error) {
	p, err := compParse(ec, raw, true)
	if err != nil {
		return nil, "", err
	}
	out, _ := json.Marshal(p)
	return out, RiskChange, nil
}

func (a compInstallAction) Describe(_ context.Context, ec *ExecContext, raw json.RawMessage) (string, string, string, error) {
	p, err := compParse(ec, raw, true)
	if err != nil {
		return "", "", "", err
	}
	ver := p.Version
	if ver == "" {
		ver = "推荐版本"
	}
	title := fmt.Sprintf("在集群 %d 安装组件 %s（%s）", p.ClusterID, p.Key, ver)
	preview := "安装前检查依赖与冲突；通过后以平台审核过的 Helm Chart 安装，并轮询状态至已安装。"
	manual := fmt.Sprintf("在平台「集群 %d → 组件 → %s」执行预检后点击安装。", p.ClusterID, p.Key)
	return title, preview, manual, nil
}

func (a compInstallAction) Execute(ctx context.Context, ec *ExecContext, raw json.RawMessage) (string, error) {
	p, err := compParse(ec, raw, true)
	if err != nil {
		return "", err
	}
	ci, err := compInstaller(ec)
	if err != nil {
		return "", err
	}
	if ec.Principal == nil {
		return "", errors.New("缺少审核人身份，拒绝安装组件")
	}
	id, err := ci.InstallComponent(ctx, ec.Principal, app.ComponentInstallRequest{ClusterID: p.ClusterID, Key: p.Key, Version: p.Version})
	if err != nil {
		return "", fmt.Errorf("安装组件 %s 失败：%w", p.Key, err)
	}
	return fmt.Sprintf("已提交组件 %s 安装，%s%s", p.Key, compOpMarker, id), nil
}

// Verify polls ComponentStatus until the component is installed or
// compVerifyTimeout elapses. (Not part of the Action interface yet; the
// executor can call it via a type assertion.)
func (a compInstallAction) Verify(ctx context.Context, ec *ExecContext, raw json.RawMessage, _ string) (string, error) {
	p, err := compParse(ec, raw, true)
	if err != nil {
		return "", err
	}
	ci, err := compInstaller(ec)
	if err != nil {
		return "", err
	}
	return compPollInstalled(ctx, ci, p.ClusterID, p.Key, compVerifyTimeout, compVerifyInterval)
}

func compPollInstalled(ctx context.Context, ci app.ComponentInstaller, clusterID int64, key string, timeout, interval time.Duration) (string, error) {
	deadline := compNow().Add(timeout)
	var last app.ComponentStatus
	var lastErr error
	for {
		st, err := ci.ComponentStatus(ctx, clusterID, key)
		if err == nil {
			last, lastErr = st, nil
			if st.Installed {
				return compFormatStatus(st), nil
			}
		} else {
			lastErr = err
		}
		if !compNow().Before(deadline) {
			if lastErr != nil {
				return "", fmt.Errorf("等待组件 %s 安装完成超时（%s），最后一次查询失败：%v", key, timeout, lastErr)
			}
			return compFormatStatus(last), fmt.Errorf("等待组件 %s 安装完成超时（%s）", key, timeout)
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return "", fmt.Errorf("等待组件 %s 安装被取消：%w", key, ctx.Err())
		case <-t.C:
		}
	}
}

func (compInstallAction) Rollback(context.Context, *ExecContext, json.RawMessage, string) (string, error) {
	return "", ErrNoRollback
}

func init() {
	registerAction(compStatusAction{})
	registerAction(compInstallAction{})
}
