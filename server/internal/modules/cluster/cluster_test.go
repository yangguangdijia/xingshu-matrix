package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	matrixcrypto "github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
)

func kubeconfigFor(server string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: c1
  cluster: {server: %q, insecure-skip-tls-verify: true}
users:
- name: u1
  user: {token: abc}
contexts:
- name: ctx1
  context: {cluster: c1, user: u1}
current-context: ctx1
`, server)
}

func TestKubeconfigAndServerVersion(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" || r.Header.Get("Authorization") != "Bearer abc" {
			http.Error(w, "nope "+r.URL.Path+" "+r.Header.Get("Authorization"), 404)
			return
		}
		_, _ = w.Write([]byte(`{"major":"1","minor":"31","gitVersion":"v1.31.4"}`))
	}))
	defer srv.Close()
	kc, server, err := normalizeKubeconfig(kubeconfigFor(srv.URL), "ctx1")
	if err != nil || server != srv.URL {
		t.Fatalf("normalize: %v %q", err, server)
	}
	if _, _, err := normalizeKubeconfig(kc, "missing"); err == nil {
		t.Fatal("expected missing context error")
	}
	cl, err := buildClients(kc, "")
	if err != nil {
		t.Fatal(err)
	}
	v, err := serverVersion(context.Background(), cl.cs)
	if err != nil || v != "v1.31.4" {
		t.Fatalf("version %q %v", v, err)
	}
}

func TestSecretApprovalPayloadEncryptsAndRedactsValues(t *testing.T) {
	cipher, err := matrixcrypto.New([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	m := &Module{d: &app.Deps{Crypto: cipher}}
	raw := json.RawMessage(`{"name":"db-auth","namespace":"prod","data":{"password":"super-secret-value"}}`)
	pl := changePayload{ClusterID: 42, Cluster: "prod-main", Action: "secret_create", Namespace: "prod", Name: "db-auth", Params: raw}
	if err := m.encryptSecretParams(&pl); err != nil {
		t.Fatal(err)
	}
	if len(pl.Params) != 0 || pl.EncryptedParams == "" {
		t.Fatalf("secret params should be encrypted: %+v", pl)
	}
	if strings.Contains(pl.EncryptedParams, "super-secret-value") {
		t.Fatal("ciphertext contains plaintext secret")
	}
	encoded, err := json.Marshal(pl)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "super-secret-value") {
		t.Fatal("approval storage payload contains plaintext secret")
	}
	public := sanitizeApprovalPayload(encoded)
	if strings.Contains(string(public), "super-secret-value") || strings.Contains(string(public), pl.EncryptedParams) {
		t.Fatal("public approval payload exposes secret material")
	}
	if !strings.Contains(string(public), `"secret_keys":["password"]`) || !strings.Contains(string(public), `"params_redacted":true`) {
		t.Fatalf("public payload is missing safe metadata: %s", public)
	}
	decrypted, err := cipher.DecryptBytes(pl.EncryptedParams, secretParamsAAD(pl))
	if err != nil || string(decrypted) != string(raw) {
		t.Fatalf("encrypted payload did not round-trip: %v", err)
	}
	wrongContext := pl
	wrongContext.ClusterID++
	if _, err := cipher.DecryptBytes(pl.EncryptedParams, secretParamsAAD(wrongContext)); err == nil {
		t.Fatal("ciphertext should be bound to cluster identity")
	}
	row := secretRow(corev1.Secret{Data: map[string][]byte{"password": []byte("super-secret-value")}})
	rowJSON, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rowJSON), "super-secret-value") || strings.Contains(string(rowJSON), `"data"`) {
		t.Fatalf("Secret API row exposes its data: %s", rowJSON)
	}
}

func TestRegistryMutationEncryptsKubeconfigAndApprovalPayloadContainsOnlyID(t *testing.T) {
	cipher, err := matrixcrypto.New([]byte(strings.Repeat("r", 32)))
	if err != nil {
		t.Fatal(err)
	}
	row := registryChangeRow{ID: 19, Env: "prod", Action: "create", Cluster: "prod-a", ApplicantID: 7}
	secret := "kubeconfig-token-do-not-store"
	stored, err := encryptRegistryMutation(cipher, row, registryMutation{
		Input: clusterInput{Name: "prod-a", Env: "prod", Kubeconfig: secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, secret) {
		t.Fatal("registry staging ciphertext contains plaintext kubeconfig")
	}
	row.PayloadEnc = stored
	decoded, err := decryptRegistryMutation(cipher, row)
	if err != nil || decoded.Input.Kubeconfig != secret {
		t.Fatalf("encrypted registry payload failed round trip: %v", err)
	}
	encoded, err := json.Marshal(registryApprovalPayload{ChangeID: row.ID})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "kubeconfig") {
		t.Fatalf("approval ticket payload must contain only the stage id: %s", encoded)
	}
	row.Env = "dev"
	if _, err := decryptRegistryMutation(cipher, row); err == nil {
		t.Fatal("registry ciphertext must be bound to environment metadata")
	}
}

func TestBatchImportRequiresSingleEnvironment(t *testing.T) {
	for _, test := range []struct {
		name  string
		items []kubeconfigImportItem
		want  string
		fail  bool
	}{
		{name: "single environment", items: []kubeconfigImportItem{{Env: "staging"}, {Env: " staging "}}, want: "staging"},
		{name: "mixed environments", items: []kubeconfigImportItem{{Env: "prod"}, {Env: "dev"}}, fail: true},
		{name: "missing environment", items: []kubeconfigImportItem{{Env: ""}}, fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := oneImportEnvironment(test.items)
			if test.fail && err == nil {
				t.Fatal("expected invalid batch environment")
			}
			if !test.fail && (err != nil || got != test.want) {
				t.Fatalf("environment=%q err=%v, want %q", got, err, test.want)
			}
		})
	}
}

func TestKubeconfigContextsListsOnlySafeContextMetadata(t *testing.T) {
	raw := `apiVersion: v1
kind: Config
clusters:
- name: prod
  cluster: {server: "https://prod-user:prod-password@prod.example:6443/proxy?token=query-secret#fragment", insecure-skip-tls-verify: true}
- name: dev
  cluster: {server: "https://dev.example:6443", insecure-skip-tls-verify: true}
users:
- name: prod-user
  user: {token: top-secret}
- name: dev-user
  user: {token: another-secret}
contexts:
- name: z-prod
  context: {cluster: prod, user: prod-user, namespace: workloads}
- name: a-dev
  context: {cluster: dev, user: dev-user}
current-context: z-prod
`
	items, err := kubeconfigContexts(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Name != "a-dev" || items[1].Name != "z-prod" {
		t.Fatalf("contexts should be stable and sorted: %+v", items)
	}
	if items[0].Server != "https://dev.example:6443" || items[0].Current {
		t.Fatalf("unexpected dev metadata: %+v", items[0])
	}
	if items[1].Server != "https://prod.example:6443/proxy" || !items[1].Current || items[1].Namespace != "workloads" {
		t.Fatalf("unexpected prod metadata: %+v", items[1])
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	for _, credential := range []string{"secret", "prod-password", "query-secret", "fragment"} {
		if strings.Contains(string(encoded), credential) {
			t.Fatalf("context metadata leaked %q: %s", credential, encoded)
		}
	}
	if strings.Contains(string(encoded), "token") {
		t.Fatalf("context metadata leaked credentials: %s", encoded)
	}
}

func TestKubeconfigContextsRejectsMissingAndExcessiveContexts(t *testing.T) {
	for _, raw := range []string{
		"apiVersion: v1\nkind: Config\n",
		strings.Repeat("x", maxKubeconfigBytes+1),
		func() string {
			var b strings.Builder
			b.WriteString("apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster: {server: https://example.test}\ncontexts:\n")
			for i := 0; i <= maxKubeconfigContexts; i++ {
				fmt.Fprintf(&b, "- name: ctx-%02d\n  context: {cluster: c}\n", i)
			}
			return b.String()
		}(),
	} {
		if _, err := kubeconfigContexts(raw); err == nil {
			t.Fatal("expected invalid/oversized kubeconfig contexts to be rejected")
		}
	}
}

func TestKubeconfigForContextKeepsOnlySelectedCredentials(t *testing.T) {
	raw := `apiVersion: v1
kind: Config
clusters:
- name: dev
  cluster: {server: https://dev.example:6443}
- name: qa
  cluster: {server: https://qa.example:6443}
users:
- name: dev-user
  user: {token: dev-token}
- name: qa-user
  user: {token: qa-token}
contexts:
- name: dev-context
  context: {cluster: dev, user: dev-user, namespace: dev}
- name: qa-context
  context: {cluster: qa, user: qa-user, namespace: qa}
current-context: dev-context
`
	selected, server, err := kubeconfigForContext(raw, "qa-context")
	if err != nil {
		t.Fatal(err)
	}
	if server != "https://qa.example:6443" {
		t.Fatalf("unexpected API server %q", server)
	}
	cfg, err := clientcmd.Load([]byte(selected))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Clusters) != 1 || cfg.Clusters["qa"] == nil || len(cfg.AuthInfos) != 1 ||
		cfg.AuthInfos["qa-user"] == nil || len(cfg.Contexts) != 1 || cfg.Contexts["qa-context"] == nil ||
		cfg.CurrentContext != "qa-context" {
		t.Fatalf("selected kubeconfig contains unrelated context data: %+v", cfg)
	}
	if cfg.AuthInfos["qa-user"].Token != "qa-token" {
		t.Fatal("selected identity was not preserved")
	}
}

func TestKubeconfigImportRejectsExecutableAndFileCredentials(t *testing.T) {
	tests := []string{
		`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: https://cluster.example, certificate-authority: /etc/passwd}
users:
- name: u
  user: {token: abc}
contexts:
- name: ctx
  context: {cluster: c, user: u}
current-context: ctx
`,
		`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: https://cluster.example}
users:
- name: u
  user: {exec: {apiVersion: client.authentication.k8s.io/v1, command: /bin/sh}}
contexts:
- name: ctx
  context: {cluster: c, user: u}
current-context: ctx
`,
		`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: https://cluster.example}
users:
- name: u
  user: {tokenFile: /etc/passwd}
contexts:
- name: ctx
  context: {cluster: c, user: u}
current-context: ctx
`,
	}
	for _, raw := range tests {
		if _, _, err := kubeconfigForContext(raw, "ctx"); err == nil {
			t.Fatal("expected unsafe kubeconfig credential reference to be rejected")
		}
		items, err := kubeconfigContexts(raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].Error == "" {
			t.Fatalf("unsafe context should be marked non-importable: %+v", items)
		}
	}
}

func TestSafeAPIServerRejectsEmbeddedCredentialsAndQuery(t *testing.T) {
	for _, raw := range []string{
		"https://user:password@cluster.example",
		"https://cluster.example?token=secret",
		"https://cluster.example#credential",
		"file:///etc/passwd",
	} {
		if _, err := safeAPIServer(raw); err == nil {
			t.Fatalf("expected API server %q to be rejected", raw)
		}
	}
}

func TestListClusterHealthNodesUsesRealNodeConditions(t *testing.T) {
	ready := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-ready"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		}},
	}
	notReady := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-not-ready"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
		}},
	}
	cs := fake.NewSimpleClientset(ready, notReady)
	result, err := listClusterHealthNodes(context.Background(), cs)
	if err != nil {
		t.Fatal(err)
	}
	if result.total != 2 || result.ready != 1 {
		t.Fatalf("node health counted total=%d ready=%d, want total=2 ready=1", result.total, result.ready)
	}
}

func TestClusterListFilterHonorsEnvironmentScope(t *testing.T) {
	tests := []struct {
		name      string
		scope     []string
		wantWhere string
		wantArgs  []any
	}{
		{name: "nil means all environments", scope: nil, wantWhere: "WHERE 1=1"},
		{name: "wildcard means all environments", scope: []string{"*"}, wantWhere: "WHERE 1=1"},
		{name: "empty scope means no environments", scope: []string{}, wantWhere: "WHERE 1=1 AND env = ANY($1)", wantArgs: []any{[]string{}}},
		{name: "scoped environments", scope: []string{"prod", "staging"}, wantWhere: "WHERE 1=1 AND env = ANY($1)", wantArgs: []any{[]string{"prod", "staging"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			where, args := clusterListFilter("", "", tt.scope)
			if where != tt.wantWhere {
				t.Fatalf("where %q, want %q", where, tt.wantWhere)
			}
			if fmt.Sprint(args) != fmt.Sprint(tt.wantArgs) {
				t.Fatalf("args %v, want %v", args, tt.wantArgs)
			}
		})
	}

	where, args := clusterListFilter("prod", "api", []string{"prod", "staging"})
	wantWhere := "WHERE 1=1 AND env=$1 AND name ILIKE $2 AND env = ANY($3)"
	if where != wantWhere || fmt.Sprint(args) != fmt.Sprint([]any{"prod", "%api%", []string{"prod", "staging"}}) {
		t.Fatalf("combined filter = %q %v", where, args)
	}
}

func TestValidateChange(t *testing.T) {
	ok := changePayload{Action: "scale", Kind: "deploy", Params: json.RawMessage(`{"replicas":3}`)}
	if err := validateChange(&ok); err != nil || ok.Kind != "deployments" {
		t.Fatalf("scale: %v %s", err, ok.Kind)
	}
	for _, bad := range []changePayload{
		{Action: "scale", Kind: "deployments"},
		{Action: "scale", Kind: "daemonsets", Params: json.RawMessage(`{"replicas":1}`)},
		{Action: "restart", Kind: "pods"},
		{Action: "apply", Params: json.RawMessage(`{"yaml":"  "}`)},
		{Action: "apply", Params: json.RawMessage(`{"yaml":"apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata:\n  name: allow-api\n  namespace: prod\nspec:\n  podSelector: {}\n"}`)},
		{Action: "rm -rf"},
	} {
		if err := validateChange(&bad); err == nil {
			t.Fatalf("expected error for %+v", bad)
		}
	}
}

func TestGenericApplyAllowsOnlySupportedWorkloads(t *testing.T) {
	for _, manifest := range []string{
		"apiVersion: v1\nkind: Secret\nmetadata:\n  name: credentials\n  namespace: prod\ndata:\n  token: c2VjcmV0\n",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\n  namespace: prod\n",
		"apiVersion: v1\nkind: Service\nmetadata:\n  name: api\n  namespace: prod\n",
		"apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: data\n  namespace: prod\n",
		"apiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata:\n  name: web\n  namespace: prod\n",
		"apiVersion: snapshot.storage.k8s.io/v1\nkind: VolumeSnapshot\nmetadata:\n  name: backup\n  namespace: prod\n",
		"apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: custom\n  namespace: prod\n",
		"apiVersion: autoscaling/v2\nkind: HorizontalPodAutoscaler\nmetadata:\n  name: api\n  namespace: prod\n",
		"apiVersion: example.com/v1\nkind: Credential\nmetadata:\n  name: arbitrary\n  namespace: prod\n",
		"apiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: Secret\n  metadata:\n    name: nested\n    namespace: prod\n",
	} {
		if err := validateApplyYAML(manifest); err == nil {
			t.Fatalf("generic apply accepted dedicated resource:\n%s", manifest)
		}
	}
	for _, manifest := range []string{
		"apiVersion: v1\nkind: Pod\nmetadata:\n  name: api\n  namespace: prod\n",
		"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\n  namespace: prod\n",
		"apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: api\n  namespace: prod\n",
		"apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: agent\n  namespace: prod\n",
		"apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: task\n  namespace: prod\n",
		"apiVersion: batch/v1\nkind: CronJob\nmetadata:\n  name: task\n  namespace: prod\n",
	} {
		if err := validateApplyYAML(manifest); err != nil {
			t.Fatalf("generic apply rejected supported workload:\n%s\n%v", manifest, err)
		}
	}
}

func TestValidateNetworkPolicyChange(t *testing.T) {
	valid := changePayload{
		Action: "network_policy_create", Namespace: "prod", Name: "allow-api",
		Params: json.RawMessage(`{"labels":{"team":"platform"},"spec":{"podSelector":{"matchLabels":{"app":"api"}},"egress":[{}]}}`),
	}
	if err := validateChange(&valid); err != nil {
		t.Fatal(err)
	}
	var normalized networkPolicyParams
	if err := decodeNetworkPolicyParams(valid.Params, &normalized); err != nil {
		t.Fatal(err)
	}
	if normalized.Spec == nil || len(normalized.Spec.PolicyTypes) != 2 ||
		normalized.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress ||
		normalized.Spec.PolicyTypes[1] != networkingv1.PolicyTypeEgress {
		t.Fatalf("expected default policy types for egress: %+v", normalized.Spec)
	}

	for _, bad := range []changePayload{
		{Action: "network_policy_create", Namespace: "prod", Name: "bad/name", Params: json.RawMessage(`{"spec":{}}`)},
		{Action: "network_policy_create", Namespace: "prod", Name: "missing-spec"},
		{Action: "network_policy_create", Namespace: "prod", Name: "unknown-field", Params: json.RawMessage(`{"spec":{"unknown":true}}`)},
		{Action: "network_policy_update", Namespace: "prod", Name: "allow-api", Params: json.RawMessage(`{"uid":"uid-1","spec":{}}`)},
		{Action: "network_policy_delete", Namespace: "prod", Name: "allow-api", Params: json.RawMessage(`{"uid":"uid-1","resource_version":"1","spec":{}}`)},
		{Action: "network_policy_create", Namespace: "prod", Name: "bad-selector", Params: json.RawMessage(`{"spec":{"podSelector":{"matchExpressions":[{"key":"app","operator":"Unknown"}]}}}`)},
		{Action: "network_policy_create", Namespace: "prod", Name: "wrong-direction", Params: json.RawMessage(`{"spec":{"policyTypes":["Egress"],"ingress":[{}]}}`)},
		{Action: "network_policy_create", Namespace: "prod", Name: "bad-peer", Params: json.RawMessage(`{"spec":{"ingress":[{"from":[{"podSelector":{"matchExpressions":[{"key":"app","operator":"Unknown"}]}}]}]}}`)},
		{Action: "network_policy_create", Namespace: "prod", Name: "bad-cidr", Params: json.RawMessage(`{"spec":{"egress":[{"to":[{"ipBlock":{"cidr":"not-a-cidr"}}]}]}}`)},
	} {
		if err := validateChange(&bad); err == nil {
			t.Fatalf("expected NetworkPolicy validation error for %+v", bad)
		}
	}
	if got := changePermission("network_policy_update"); got != PermNetworkWrite {
		t.Fatalf("approval re-check permission %q", got)
	}
	for _, action := range []string{"pv_create", "pv_update", "pv_delete"} {
		if got := changePermission(action); got != PermStorageWrite {
			t.Errorf("approval re-check permission for %s = %q, want %q", action, got, PermStorageWrite)
		}
	}
}

func TestNetworkPolicyCreateUpdateDelete(t *testing.T) {
	ctx := context.Background()
	initial := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "allow-api", Namespace: "prod", UID: types.UID("uid-1"),
			ResourceVersion: "rv-1", Labels: map[string]string{"team": "old"},
		},
		Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}},
	}
	cs := fake.NewSimpleClientset(initial)
	kc := &kubeClients{cs: cs}

	updatedSpec := `{"podSelector":{"matchLabels":{"app":"api"}},"policyTypes":["Ingress"],"ingress":[{"from":[{"podSelector":{"matchLabels":{"app":"frontend"}}}]}]}`
	updateParams := json.RawMessage(`{"uid":"uid-1","resource_version":"rv-1","labels":{"team":"platform"},"spec":` + updatedSpec + `}`)
	res, err := executeOn(ctx, kc, changePayload{
		Action: "network_policy_update", Namespace: "prod", Name: "allow-api", Params: updateParams,
	})
	if err != nil {
		t.Fatal(err)
	}
	row := res.(NetworkPolicyRow)
	if row.UID != "uid-1" || row.ResourceVersion != "rv-1" || row.Labels["team"] != "platform" || len(row.Ingress) != 1 {
		t.Fatalf("unexpected updated policy row: %+v", row)
	}

	_, err = executeOn(ctx, kc, changePayload{
		Action: "network_policy_update", Namespace: "prod", Name: "allow-api",
		Params: json.RawMessage(`{"uid":"stale-uid","resource_version":"rv-1","spec":{}}`),
	})
	if err == nil {
		t.Fatal("expected stale policy identity to conflict")
	}

	_, err = executeOn(ctx, kc, changePayload{
		Action: "network_policy_delete", Namespace: "prod", Name: "allow-api",
		Params: json.RawMessage(`{"uid":"uid-1","resource_version":"rv-1"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.NetworkingV1().NetworkPolicies("prod").Get(ctx, "allow-api", metav1.GetOptions{}); err == nil {
		t.Fatal("NetworkPolicy still exists after delete")
	}

	created, err := executeOn(ctx, kc, changePayload{
		Action: "network_policy_create", Namespace: "prod", Name: "allow-web",
		Params: json.RawMessage(`{"labels":{"team":"platform"},"spec":{"podSelector":{"matchLabels":{"app":"web"}}}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if row := created.(NetworkPolicyRow); row.Name != "allow-web" || len(row.PolicyTypes) != 1 || row.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Fatalf("unexpected created policy row: %+v", row)
	}
}

func TestValidateResourcePreconditions(t *testing.T) {
	if err := validateResourcePreconditions(json.RawMessage(`{"uid":"uid-1","resource_version":"rv-1"}`), "Service"); err != nil {
		t.Fatalf("valid delete preconditions: %v", err)
	}
	for _, raw := range []json.RawMessage{
		nil,
		json.RawMessage(`{}`),
		json.RawMessage(`{"uid":"uid-1"}`),
		json.RawMessage(`{"resource_version":"rv-1"}`),
	} {
		if err := validateResourcePreconditions(raw, "Service"); err == nil {
			t.Errorf("expected precondition error for %s", raw)
		}
	}
}

func TestPodRowIncludesUIDForIdentityCheckedProbe(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "api-0", UID: "pod-uid"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	row := podRow(pod)
	if row.UID != "pod-uid" || row.Phase != "Running" || len(row.Containers) != 1 || row.Containers[0] != "app" {
		t.Fatalf("unexpected Pod row: %+v", row)
	}
}

func TestProbeCommandValidatesTargetAndBuildsArgumentVector(t *testing.T) {
	input := networkProbeInput{Namespace: "prod", Pod: "api-0", UID: "pod-uid", Container: "app", Host: "api.internal.", Port: 8443}
	command, err := probeCommand(input)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"nc", "-z", "-w", "3", "api.internal.", "8443"}
	if fmt.Sprint(command) != fmt.Sprint(want) {
		t.Fatalf("command %v, want %v", command, want)
	}
	for _, host := range []string{"-e", "api..internal", "bad_label.internal", "api.internal;id"} {
		input.Host = host
		if _, err := probeCommand(input); err == nil {
			t.Fatalf("expected invalid host %q to be rejected", host)
		}
	}
	input.Host = "127.0.0.1"
	input.Port = 65536
	if _, err := probeCommand(input); err == nil {
		t.Fatal("expected invalid port to be rejected")
	}
}

func TestCordonLabelsAndDrain(t *testing.T) {
	ctrl := true
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"node-role.kubernetes.io/worker": ""}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	web := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "default",
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web", Controller: &ctrl}}},
		Spec: corev1.PodSpec{NodeName: "n1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	ds := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-1", Namespace: "kube-system",
		OwnerReferences: []metav1.OwnerReference{{Kind: "DaemonSet", Name: "agent", Controller: &ctrl}}},
		Spec: corev1.PodSpec{NodeName: "n1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	cs := fake.NewSimpleClientset(node, web, ds, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}})
	var evicted []string
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		ev := a.(k8stesting.CreateAction).GetObject().(*policyv1.Eviction)
		evicted = append(evicted, ev.Namespace+"/"+ev.Name)
		return true, nil, nil
	})
	kc := &kubeClients{cs: cs}
	ctx := context.Background()

	if _, err := executeOn(ctx, kc, changePayload{Action: "cordon", Name: "n1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := executeOn(ctx, kc, changePayload{Action: "node_labels", Name: "n1", Params: json.RawMessage(`{"labels":{"zone":"a"}}`)}); err != nil {
		t.Fatal(err)
	}
	n, _ := cs.CoreV1().Nodes().Get(ctx, "n1", metav1.GetOptions{})
	if !n.Spec.Unschedulable || n.Labels["zone"] != "a" {
		t.Fatalf("node not patched: %+v", n)
	}
	row := nodeRow(*n)
	if row.Status != "Ready,SchedulingDisabled" || len(row.Roles) != 1 {
		t.Fatalf("row %+v", row)
	}
	res, err := executeOn(ctx, kc, changePayload{Action: "drain", Name: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["evicted"] != 1 || len(evicted) != 1 || evicted[0] != "default/web-1" {
		t.Fatalf("drain result %+v evicted=%v", m, evicted)
	}
	rows, n2, err := listWorkloadRows(ctx, cs, "deployments", "default", metav1.ListOptions{})
	if err != nil || n2 != 1 || rows.([]WorkloadRow)[0].Name != "web" {
		t.Fatalf("list deployments %v %v", rows, err)
	}
}

func TestStreamSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/logs", nil)
	streamSSE(c.Request.Context(), c, strings.NewReader("line1\nline2\n"))
	body := w.Body.String()
	if !strings.Contains(body, "event:log\ndata:line1") || !strings.Contains(body, "event:end") {
		t.Fatalf("sse body %q", body)
	}
}

func TestRoutesRegister(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	New().Register(r.Group("/api/v1"), &app.Deps{})
	if n := len(r.Routes()); n < 25 {
		t.Fatalf("routes %d", n)
	}
}
