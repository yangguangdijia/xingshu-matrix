package cluster

import (
	"context"
	"strings"
	"testing"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
)

func TestInstallerValidateRequest(t *testing.T) {
	cases := []struct {
		req  app.ComponentInstallRequest
		want string
	}{
		{app.ComponentInstallRequest{ClusterID: 0, Key: "cert-manager"}, "cluster_id"},
		{app.ComponentInstallRequest{ClusterID: 1, Key: " "}, "不能为空"},
		{app.ComponentInstallRequest{ClusterID: 1, Key: "no-such-thing"}, "未知组件"},
	}
	for _, c := range cases {
		if _, err := compValidateInstallRequest(c.req); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%+v: got %v, want %q", c.req, err, c.want)
		}
	}
	key, err := compValidateInstallRequest(app.ComponentInstallRequest{ClusterID: 3, Key: " cert-manager "})
	if err != nil || key != "cert-manager" {
		t.Fatalf("valid request: key=%q err=%v", key, err)
	}
}

func TestInstallerPlanRejectsMissingDependencies(t *testing.T) {
	err := compPlanCheck("istio-gateway", map[string]bool{})
	if err == nil || !strings.Contains(err.Error(), "缺少依赖") || !strings.Contains(err.Error(), "istio-base") || !strings.Contains(err.Error(), "istiod") {
		t.Fatalf("want missing istio-base/istiod, got %v", err)
	}
	if err := compPlanCheck("istio-gateway", map[string]bool{"istio-base": true, "istiod": true}); err != nil {
		t.Fatalf("deps installed: %v", err)
	}
	if got := compMissingDependencies("istio-gateway", map[string]bool{"istio-base": true}); len(got) != 1 || got[0] != "istiod" {
		t.Fatalf("missing deps = %v", got)
	}
}

func TestInstallerPlanRejectsConflicts(t *testing.T) {
	err := compPlanCheck("kyverno", map[string]bool{"gatekeeper": true})
	if err == nil || !strings.Contains(err.Error(), "冲突") || !strings.Contains(err.Error(), "gatekeeper") {
		t.Fatalf("want conflict with gatekeeper, got %v", err)
	}
	if err := compPlanCheck("kyverno", map[string]bool{}); err != nil {
		t.Fatalf("no conflict expected: %v", err)
	}
}

func TestInstallerPrincipalAndServices(t *testing.T) {
	if err := compCheckPrincipal(nil, "prod"); err == nil {
		t.Fatal("nil principal must be rejected")
	}
	m := &Module{}
	if _, err := m.InstallComponent(context.Background(), nil, app.ComponentInstallRequest{ClusterID: 1, Key: "cert-manager"}); err == nil || !strings.Contains(err.Error(), "不可用") {
		t.Fatalf("want services unavailable, got %v", err)
	}
	if _, err := m.ComponentStatus(context.Background(), 1, "nope"); err == nil || !strings.Contains(err.Error(), "未知组件") {
		t.Fatalf("want unknown component, got %v", err)
	}
	st, err := m.ComponentStatus(context.Background(), 1, "cert-manager")
	if err == nil || st.Tier != ComponentTierRequired {
		t.Fatalf("want tier required + unavailable error, got %+v %v", st, err)
	}
}
