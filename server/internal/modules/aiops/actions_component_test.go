package aiops

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

type compFakeInstaller struct {
	installErr error
	statuses   []app.ComponentStatus
	calls      int
	gotReq     app.ComponentInstallRequest
}

func (f *compFakeInstaller) InstallComponent(_ context.Context, _ *rbac.Principal, req app.ComponentInstallRequest) (string, error) {
	f.gotReq = req
	return "op-123", f.installErr
}

func (f *compFakeInstaller) ComponentStatus(_ context.Context, _ int64, key string) (app.ComponentStatus, error) {
	i := f.calls
	f.calls++
	if i >= len(f.statuses) {
		i = len(f.statuses) - 1
	}
	st := f.statuses[i]
	st.Key = key
	return st, nil
}

func TestCompActionRegistered(t *testing.T) {
	for name, risk := range map[string]string{"component.install": RiskChange, "component.status": RiskRead} {
		a, ok := actions[name]
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if a.Spec().Risk != risk || a.Spec().Permission != "aiops:execute" {
			t.Fatalf("%s spec = %+v", name, a.Spec())
		}
		if _, err := a.Rollback(context.Background(), nil, nil, ""); !errors.Is(err, ErrNoRollback) {
			t.Fatalf("%s rollback = %v", name, err)
		}
	}
}

func TestCompActionValidate(t *testing.T) {
	inst := compInstallAction{}
	ec := &ExecContext{ClusterID: 7}
	bad := []string{``, `{"key":"cert-manager"}`, `{"cluster_id":7,"key":"Bad Key"}`, `{"cluster_id":8,"key":"loki"}`,
		`{"cluster_id":7,"key":"loki","version":"1 2"}`, `{"cluster_id":7,"key":"loki","extra":1}`}
	for _, raw := range bad {
		var e *ExecContext
		if raw != `{"key":"cert-manager"}` {
			e = ec
		}
		if _, _, err := inst.Validate(context.Background(), e, json.RawMessage(raw)); err == nil {
			t.Fatalf("expected error for %s", raw)
		}
	}
	out, risk, err := inst.Validate(context.Background(), ec, json.RawMessage(`{"key":" loki ","version":"v1.2.3"}`))
	if err != nil || risk != RiskChange || !strings.Contains(string(out), `"cluster_id":7`) || !strings.Contains(string(out), `"key":"loki"`) {
		t.Fatalf("validate = %s %s %v", out, risk, err)
	}
	if _, _, err := (compStatusAction{}).Validate(context.Background(), ec, json.RawMessage(`{"key":"loki","version":"1"}`)); err == nil {
		t.Fatal("status must reject version")
	}
}

func TestCompActionInstallerNil(t *testing.T) {
	ec := &ExecContext{Deps: &app.Deps{}, Principal: &rbac.Principal{}}
	_, err := (compInstallAction{}).Execute(context.Background(), ec, json.RawMessage(`{"cluster_id":1,"key":"loki"}`))
	if err == nil || !strings.Contains(err.Error(), "未接入") {
		t.Fatalf("want not-wired error, got %v", err)
	}
}

func TestCompActionInstallRejectsDependency(t *testing.T) {
	f := &compFakeInstaller{installErr: errors.New("拒绝安装组件 istiod，缺少依赖：istio-base")}
	ec := &ExecContext{Deps: &app.Deps{Components: f}, Principal: &rbac.Principal{}}
	_, err := (compInstallAction{}).Execute(context.Background(), ec, json.RawMessage(`{"cluster_id":1,"key":"istiod"}`))
	if err == nil || !strings.Contains(err.Error(), "istio-base") {
		t.Fatalf("want dependency rejection, got %v", err)
	}
	f.installErr = nil
	out, err := (compInstallAction{}).Execute(context.Background(), ec, json.RawMessage(`{"cluster_id":1,"key":"istiod","version":"1.22.0"}`))
	if err != nil || !strings.Contains(out, "operation_id=op-123") || f.gotReq.Version != "1.22.0" {
		t.Fatalf("install = %q %v %+v", out, err, f.gotReq)
	}
}

func TestCompActionVerifyPolls(t *testing.T) {
	f := &compFakeInstaller{statuses: []app.ComponentStatus{{}, {}, {Installed: true, Healthy: true, Tier: "required"}}}
	out, err := compPollInstalled(context.Background(), f, 1, "cert-manager", time.Second, time.Millisecond)
	if err != nil || !strings.Contains(out, "已安装") || f.calls != 3 {
		t.Fatalf("poll = %q %v calls=%d", out, err, f.calls)
	}
	g := &compFakeInstaller{statuses: []app.ComponentStatus{{}}}
	if _, err := compPollInstalled(context.Background(), g, 1, "loki", 5*time.Millisecond, time.Millisecond); err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("want timeout, got %v", err)
	}
}
