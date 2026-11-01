package cluster

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func TestAgentTokenRoundTrip(t *testing.T) {
	token, hash, err := newAgentToken()
	if err != nil {
		t.Fatal(err)
	}
	if !validAgentTokenFormat(token) || len(hash) != 64 {
		t.Fatalf("unexpected token shape %d / hash %d", len(token), len(hash))
	}
	if !agentTokenMatches(token, hash) {
		t.Fatal("token should match its hash")
	}
	other, _, _ := newAgentToken()
	for _, bad := range []string{other, "", token + "x", strings.TrimPrefix(token, agentTokenPrefix)} {
		if agentTokenMatches(bad, hash) {
			t.Fatalf("token %q must not match", bad)
		}
	}
	if agentTokenMatches(token, "") {
		t.Fatal("empty stored hash must never match")
	}
}

func TestAgentManifestInputValidation(t *testing.T) {
	ok := agentManifestInput{ServerURL: "https://matrix.example.com/"}
	if err := ok.normalize("img:1"); err != nil {
		t.Fatal(err)
	}
	if ok.ServerURL != "https://matrix.example.com" || ok.Namespace != agentDefaultNamespace || ok.Image != "img:1" {
		t.Fatalf("defaults not applied: %+v", ok)
	}
	bad := []agentManifestInput{
		{ServerURL: "http://matrix.example.com"},
		{ServerURL: "ftp://matrix.example.com"},
		{ServerURL: "https://user:pw@matrix.example.com"},
		{ServerURL: "https://matrix.example.com/api"},
		{ServerURL: "https://matrix.example.com?x=1"},
		{ServerURL: "https://matrix.example.com", Namespace: "Bad_NS"},
		{ServerURL: "https://matrix.example.com", Image: "evil image; rm -rf"},
	}
	for _, in := range bad {
		if err := in.normalize("img:1"); err == nil {
			t.Fatalf("expected rejection for %+v", in)
		}
	}
	insecure := agentManifestInput{ServerURL: "http://10.0.0.5:30080", AllowInsecure: true}
	if err := insecure.normalize("img:1"); err != nil || !insecure.AllowInsecure {
		t.Fatalf("explicit HTTP should be allowed: %v", err)
	}
}

func TestRenderAgentManifest(t *testing.T) {
	token, _, _ := newAgentToken()
	in := agentManifestInput{ServerURL: "https://matrix.example.com", EnableCICD: true}
	if err := in.normalize("docker.io/xingshu/matrix-cluster-agent:v1"); err != nil {
		t.Fatal(err)
	}
	out, err := renderAgentManifest("prod-east\ninjected: true", token, in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "\ninjected: true") {
		t.Fatal("cluster name must not inject YAML")
	}
	docs := strings.Split(out, "\n---\n")
	kinds := map[string]int{}
	var deploy appsv1.Deployment
	var secret corev1.Secret
	for _, doc := range docs[1:] {
		var meta struct{ Kind string }
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatalf("invalid YAML document: %v\n%s", err, doc)
		}
		kinds[meta.Kind]++
		switch meta.Kind {
		case "Deployment":
			if err := yaml.Unmarshal([]byte(doc), &deploy); err != nil {
				t.Fatal(err)
			}
		case "Secret":
			if err := yaml.Unmarshal([]byte(doc), &secret); err != nil {
				t.Fatal(err)
			}
		}
	}
	if kinds["Namespace"] != 2 || kinds["ServiceAccount"] != 2 || kinds["ClusterRoleBinding"] != 2 || kinds["Secret"] != 1 || kinds["Deployment"] != 1 {
		t.Fatalf("unexpected kinds %v", kinds)
	}
	if secret.StringData["token"] != token {
		t.Fatal("token secret missing")
	}
	c := deploy.Spec.Template.Spec.Containers[0]
	if c.Image != "docker.io/xingshu/matrix-cluster-agent:v1" || deploy.Spec.Template.Spec.ServiceAccountName != agentServiceAccount {
		t.Fatalf("unexpected container %+v", c)
	}
	for _, e := range c.Env {
		if strings.Contains(e.Value, token) {
			t.Fatal("token must come from the Secret file, not a plain env var")
		}
		if e.Name == "MATRIX_ALLOW_INSECURE_HTTP" {
			t.Fatal("insecure flag must be absent for https")
		}
	}
	if c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Fatal("privilege escalation must be disabled")
	}
	noCICD := in
	noCICD.EnableCICD = false
	out2, _ := renderAgentManifest("prod", token, noCICD)
	if strings.Contains(out2, cicdDeployerAccount) {
		t.Fatal("CI/CD deployer must be opt-in")
	}
	if _, err := renderAgentManifest("prod", "not-a-token", in); err == nil {
		t.Fatal("invalid token must be rejected")
	}
}
