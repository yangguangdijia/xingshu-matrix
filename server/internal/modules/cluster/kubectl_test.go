package cluster

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestParseKubectl(t *testing.T) {
	a, err := parseKubectl(`kubectl get pods -n kube-system -o wide -l "app=web"`, "default")
	if err != nil || a.verb != "get" || a.ns != "kube-system" || a.output != "wide" || a.selector != "app=web" || len(a.pos) != 1 {
		t.Fatalf("got %+v %v", a, err)
	}
	a, err = parseKubectl(`exec web-1 -c app -- sh -c "ls -l /"`, "")
	if err != nil || a.verb != "exec" || a.ns != "default" || a.container != "app" || len(a.execArgs) != 3 || a.execArgs[2] != "ls -l /" {
		t.Fatalf("exec got %+v %v", a, err)
	}
	a, err = parseKubectl(`scale deploy/web --replicas=3`, "app")
	if err != nil || a.replicas == nil || *a.replicas != 3 {
		t.Fatalf("scale got %+v %v", a, err)
	}
	for _, bad := range []string{"get pods | grep x", "get pods; rm -rf /", "logs x -f", "get pods > a", "get 'pods", "get pods --bogus"} {
		if _, err := parseKubectl(bad, "default"); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestKindAndName(t *testing.T) {
	k, n, err := kindAndName([]string{"deploy/web"})
	if err != nil || k != "deployments" || n != "web" {
		t.Fatal(k, n, err)
	}
	if _, _, err := kindAndName([]string{"svc/web"}); err == nil {
		t.Fatal("services must not be accepted for change commands")
	}
}

func TestStripSecret(t *testing.T) {
	o := &unstructured.Unstructured{Object: map[string]any{"kind": "Secret", "metadata": map[string]any{"name": "s",
		"annotations": map[string]any{"kubectl.kubernetes.io/last-applied-configuration": "{\"data\":{}}"}},
		"data": map[string]any{"password": "c2VjcmV0"}}}
	stripObject(o)
	if o.Object["data"].(map[string]any)["password"] != "<已隐藏>" {
		t.Fatal("secret value not redacted")
	}
	if _, ok := o.GetAnnotations()["kubectl.kubernetes.io/last-applied-configuration"]; ok {
		t.Fatal("last-applied annotation leaks secret")
	}
}
