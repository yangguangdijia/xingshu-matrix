package cluster

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Masterminds/semver/v3"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	helmfake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestCuratedComponentProfilesAreRunnableAndStructured(t *testing.T) {
	want := []string{"traefik", "istio-base", "istiod", "istio-gateway", "cert-manager", "argocd", "jenkins", "prometheus", "alertmanager", "loki", "tempo", "jaeger"}
	for _, key := range want {
		def, ok := componentDefinition(key)
		if !ok || !def.Supported || !def.Installable || len(def.Versions) != 1 || len(def.ConfigurationFields) == 0 && key != "istio-base" {
			t.Fatalf("profile %q is not installable and structured: %#v", key, def)
		}
		profile, ok := componentProfileFor(key)
		if !ok || profile.RepoHost == "" || profile.Chart == "" || len(profile.Versions) != 1 {
			t.Fatalf("profile %q lacks a bounded chart source", key)
		}
	}
}

func TestCuratedDependenciesAndRetiredIngress(t *testing.T) {
	if d, _ := componentDefinition("istio-gateway"); len(d.Dependencies) != 2 || d.Dependencies[0] != "istio-base" || d.Dependencies[1] != "istiod" {
		t.Fatal("Istio gateway dependency ordering is not explicit")
	}
	if d, _ := componentDefinition("alertmanager"); len(d.Dependencies) != 0 || d.Chart != "alertmanager" {
		t.Fatal("Alertmanager must use the independent reviewed chart")
	}
	if d, _ := componentDefinition("ingress-nginx"); d.Supported || d.Installable {
		t.Fatal("retired ingress-nginx must remain discovery-only")
	}
}

func TestProfileValueTypesAreValidated(t *testing.T) {
	base := componentInstallRequest{Namespace: "obs", ReleaseName: "tempo", Version: "1.24.4"}
	if _, _, err := validateComponentRequest("tempo", componentInstallRequest{Namespace: "obs", ReleaseName: "tempo", Version: "1.24.4", Values: map[string]string{"replicas": "0"}}); err == nil {
		t.Fatal("zero replica value accepted")
	}
	if _, _, err := validateComponentRequest("traefik", componentInstallRequest{Namespace: "edge", ReleaseName: "traefik", Version: "41.7.1", Values: map[string]string{"service.type": "ExternalName"}}); err == nil {
		t.Fatal("unsupported service type accepted")
	}
	if _, _, err := validateComponentRequest("tempo", base); err != nil {
		t.Fatalf("default profile request rejected: %v", err)
	}
}

type componentFixtureTransport func(*http.Request) (*http.Response, error)

func (f componentFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func componentFixtureArchive(t *testing.T, p componentProfile) []byte {
	t.Helper()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tr := tar.NewWriter(gz)
	metadata, _ := json.Marshal(chart.Metadata{APIVersion: "v2", Name: p.Chart, Version: p.Versions[0]})
	files := map[string][]byte{
		"Chart.yaml":            metadata,
		"values.yaml":           []byte("{}"),
		"templates/config.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}-config\n  namespace: {{ .Release.Namespace }}\ndata:\n  values: {{ toJson .Values | quote }}\n"),
	}
	for name, data := range files {
		if err := tr.WriteHeader(&tar.Header{Name: p.Chart + "/" + name, Mode: 0600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func componentFixtureClient(t *testing.T, p componentProfile, archive []byte, advertisedURL, advertisedDigest string) *http.Client {
	t.Helper()
	return &http.Client{Transport: componentFixtureTransport(func(r *http.Request) (*http.Response, error) {
		var body []byte
		switch r.URL.String() {
		case "https://" + p.RepoHost + p.RepoPath + "/index.yaml":
			body = []byte(fmt.Sprintf("apiVersion: v1\nentries:\n  %s:\n  - name: %s\n    version: %s\n    digest: %s\n    urls:\n    - %s\n", p.Chart, p.Chart, p.Versions[0], advertisedDigest, advertisedURL))
		case advertisedURL:
			body = archive
		default:
			t.Errorf("unexpected fixture fetch: %s", r.URL.Host+r.URL.Path)
			return nil, errors.New("unreviewed fetch")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}
}

func TestComponentProfilesFetchRenderAndHelmSDKFixtures(t *testing.T) {
	for _, def := range sortedComponentCatalog() {
		if !def.Installable {
			continue
		}
		t.Run(def.Key, func(t *testing.T) {
			p, _ := componentProfileFor(def.Key)
			data := componentFixtureArchive(t, p)
			sum := sha256.Sum256(data)
			digest := hex.EncodeToString(sum[:])
			client := componentFixtureClient(t, p, data, componentArchiveURLs(p)[0], digest)
			ch, gotDigest, err := fetchVerifiedComponentChartWithClient(context.Background(), def, digest, client)
			if err != nil || gotDigest != digest {
				t.Fatalf("verified fetch: %v", err)
			}
			in := componentInstallRequest{Component: def.Key, Namespace: "test", ReleaseName: "matrix-" + def.Key, Version: def.Versions[0]}
			_, values, err := validateComponentRequest(def.Key, in)
			if err != nil {
				t.Fatal(err)
			}
			cfg := &action.Configuration{Releases: storage.Init(driver.NewMemory()), KubeClient: &helmfake.PrintingKubeClient{Out: io.Discard}, Capabilities: chartutil.DefaultCapabilities.Copy(), Log: func(string, ...interface{}) {}}
			cfg.Capabilities.KubeVersion = chartutil.KubeVersion{Version: "v1.34.0", Major: "1", Minor: "34"}
			status, err := runVerifiedComponentInstallWithConfig(context.Background(), cfg, ch, values, in)
			if err != nil || status != "deployed" {
				t.Fatalf("SDK install result %q: %v", status, err)
			}
			stored, err := action.NewGet(cfg).Run(in.ReleaseName)
			if err != nil || !strings.Contains(stored.Manifest, "kind: ConfigMap") {
				t.Fatalf("rendered release missing: %v", err)
			}
			if !reflect.DeepEqual(stored.Config, values) {
				t.Fatal("profile settings changed during Helm execution")
			}
		})
	}
}

func TestComponentFetchBindsDigestProfileAndArtifact(t *testing.T) {
	def, _ := componentDefinition("traefik")
	p, _ := componentProfileFor(def.Key)
	data := componentFixtureArchive(t, p)
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	for _, scenario := range []string{"changed-approved-digest", "tampered-archive", "different-profile-archive", "unreviewed-repository"} {
		t.Run(scenario, func(t *testing.T) {
			input, archive, archiveURL, expected := def, data, componentArchiveURLs(p)[0], digest
			switch scenario {
			case "changed-approved-digest":
				expected = strings.Repeat("0", 64)
			case "tampered-archive":
				archive = append([]byte(nil), data...)
				archive[len(archive)-1] ^= 1
			case "different-profile-archive":
				other, _ := componentProfileFor("jenkins")
				archiveURL = componentArchiveURLs(other)[0]
			case "unreviewed-repository":
				input.RepoURL = "https://example.test"
			}
			if _, _, err := fetchVerifiedComponentChartWithClient(context.Background(), input, expected, componentFixtureClient(t, p, archive, archiveURL, digest)); err == nil {
				t.Fatal("unsafe fetch accepted")
			}
		})
	}
}

func TestComponentArchiveAndRedirectURLsAreExact(t *testing.T) {
	for _, p := range curatedComponentProfiles {
		for _, raw := range componentArchiveURLs(p) {
			if _, err := componentVerifiedURL(raw, true); err != nil {
				t.Fatalf("pinned archive rejected: %s", p.Key)
			}
			for _, bad := range []string{raw + "?proxy=x", strings.Replace(raw, ".tgz", "-other.tgz", 1), strings.Replace(raw, "https://", "http://", 1)} {
				if _, err := componentVerifiedURL(bad, true); err == nil {
					t.Fatal("unreviewed archive URL accepted")
				}
			}
		}
	}
	for _, raw := range []string{"https://github.com/evil/chart.tgz", "https://charts.jenkins.io/arbitrary.tgz", "https://grafana.github.io/helm-charts/%2e%2e/tempo.tgz", "https://127.0.0.1/chart.tgz", "https://charts.jenkins.io./jenkins-5.10.4.tgz"} {
		if _, err := componentVerifiedURL(raw, true); err == nil {
			t.Fatal("unsafe URL accepted")
		}
	}
	p, _ := componentProfileFor("traefik")
	pinned := componentArchiveURLs(p)
	origin, _ := http.NewRequest("GET", pinned[len(pinned)-1], nil)
	target, _ := http.NewRequest("GET", "https://release-assets.githubusercontent.com/github-production-release-asset/123/chart.tgz?se=x&sig=x", nil)
	if err := componentAllowedReleaseRedirect(target, []*http.Request{origin}); err != nil {
		t.Fatal(err)
	}
	unrelated, _ := http.NewRequest("GET", "https://github.com/unreviewed/releases/download/v1/chart.tgz", nil)
	if err := componentAllowedReleaseRedirect(target, []*http.Request{unrelated}); err == nil {
		t.Fatal("unreviewed redirect origin accepted")
	}
	cross, _ := http.NewRequest("GET", componentArchiveURLs(p)[0], nil)
	if err := componentVerifiedHTTPClient().CheckRedirect(cross, []*http.Request{origin}); err == nil {
		t.Fatal("cross-repository redirect accepted")
	}
}

func TestComponentProfilesUseTypedDefaultsAndPreserveApprovalValues(t *testing.T) {
	in := componentInstallRequest{Component: "tempo", Namespace: "obs", ReleaseName: "tempo", Version: "1.24.4", ChartDigest: strings.Repeat("a", 64),
		Values: map[string]string{"replicas": "2", "persistence.enabled": "true", "persistence.storageClass": "fast-csi", "service.type": "NodePort", "resources.requests.cpu": "150m"}}
	_, before, err := validateComponentRequest("tempo", in)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(componentOperationSnapshot{Component: "tempo", ChartDigest: in.ChartDigest, Request: in})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot componentOperationSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	_, after, err := validateComponentRequest(snapshot.Component, snapshot.Request)
	if err != nil || !reflect.DeepEqual(in.Values, snapshot.Request.Values) || !reflect.DeepEqual(before, after) || snapshot.Request.ChartDigest != in.ChartDigest {
		t.Fatal("approval snapshot changed submitted fields")
	}
	if after["replicas"] != 2 || after["persistence"].(map[string]any)["enabled"] != true {
		t.Fatal("typed values lost")
	}
	for _, value := range []string{"not-a-quantity", "-1", "0", "1;exec"} {
		in.Values["resources.requests.cpu"] = value
		if _, _, err := validateComponentRequest("tempo", in); err == nil {
			t.Fatalf("invalid quantity accepted: %s", value)
		}
	}
	defaults := componentDefaultValues("loki")
	defaults["deploymentMode"] = "mutated"
	if componentDefaultValues("loki")["deploymentMode"] != "SingleBinary" {
		t.Fatal("profile defaults are shared mutable state")
	}
}

func componentTestCRD() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]any{"name": "widgets.example.test"},
		"spec": map[string]any{"group": "example.test", "scope": "Namespaced", "names": map[string]any{"plural": "widgets", "kind": "Widget"},
			"versions": []any{map[string]any{"name": "v1", "served": true, "storage": true, "schema": map[string]any{"openAPIV3Schema": map[string]any{"type": "object", "properties": map[string]any{"spec": map[string]any{"type": "object", "required": []any{"size"}, "properties": map[string]any{"size": map[string]any{"type": "integer", "minimum": int64(1)}}}}}}}}},
	}}
}

func TestComponentCRDSchemaFreezeAndEstablishOrdering(t *testing.T) {
	crd := componentTestCRD()
	widget := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "example.test/v1", "kind": "Widget", "metadata": map[string]any{"name": "sample"}, "spec": map[string]any{"size": int64(1)}}}
	objects := []*unstructured.Unstructured{crd, widget}
	mapping, err := componentCRDMapping(objects, widget)
	if err != nil || mapping.Resource.Resource != "widgets" {
		t.Fatalf("future mapping failed: %v", err)
	}
	if err := componentValidateFutureResource(objects, widget); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(widget.Object, int64(0), "spec", "size")
	if err := componentValidateFutureResource(objects, widget); err == nil {
		t.Fatal("invalid custom resource passed schema validation")
	}
	_ = unstructured.SetNestedField(widget.Object, int64(1), "spec", "size")
	literal := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "literal"}, "data": map[string]any{"config": "{{ application.template }}"}}}
	ch := &chart.Chart{Metadata: &chart.Metadata{Name: "traefik", Version: "41.7.1"}}
	componentFreezeChart(ch, append(objects, literal))
	if len(ch.CRDObjects()) != 1 {
		t.Fatal("CRD was not separated from Helm workload templates")
	}
	vals, err := chartutil.ToRenderValues(ch, map[string]any{}, chartutil.ReleaseOptions{Name: "test", Namespace: "test", IsInstall: true}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(ch, vals)
	if err != nil {
		t.Fatalf("frozen application templates evaluated: %v", err)
	}
	if !strings.Contains(fmt.Sprint(rendered), "{{ application.template }}") {
		t.Fatal("application template data changed")
	}
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	creates := 0
	dyn.PrependReactor("create", "customresourcedefinitions", func(action ktesting.Action) (bool, runtime.Object, error) {
		creates++
		obj := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		_ = unstructured.SetNestedSlice(obj.Object, []any{map[string]any{"type": "Established", "status": "True"}}, "status", "conditions")
		return true, obj, dyn.Tracker().Create(componentCRDGVR, obj, "")
	})
	kc := &kubeClients{dyn: dyn}
	if err := componentEstablishCRDs(context.Background(), kc, ch); err != nil {
		t.Fatal(err)
	}
	if creates != 1 {
		t.Fatalf("CRD create count: %d", creates)
	}
	if err := componentEstablishCRDs(context.Background(), kc, ch); err != nil || creates != 1 {
		t.Fatal("identical established CRD was not reused")
	}
	for _, action := range dyn.Actions() {
		if action.GetResource() != componentCRDGVR {
			t.Fatal("CRD phase wrote a workload")
		}
	}
	_ = unstructured.SetNestedField(crd.Object, "different.test", "spec", "group")
	componentFreezeChart(ch, []*unstructured.Unstructured{crd})
	if err := componentEstablishCRDs(context.Background(), kc, ch); err == nil {
		t.Fatal("conflicting CRD was adopted")
	}
}

func TestComponentReusableCRDNormalizesJSONNumbersAndChecksFullSpec(t *testing.T) {
	wanted := componentTestCRD()
	wantedData, err := json.Marshal(wanted.Object)
	if err != nil {
		t.Fatal(err)
	}
	var chartObject map[string]any
	if err := json.Unmarshal(wantedData, &chartObject); err != nil {
		t.Fatal(err)
	}
	chartCRD := &unstructured.Unstructured{Object: chartObject}
	existing := wanted.DeepCopy()
	_ = unstructured.SetNestedSlice(existing.Object, []any{map[string]any{"type": "Established", "status": "True"}}, "status", "conditions")
	if !componentReusableCRD(existing, chartCRD) {
		t.Fatal("JSON-number representation difference rejected an otherwise identical CRD")
	}
	_ = unstructured.SetNestedField(existing.Object, "different.test", "spec", "group")
	if componentReusableCRD(existing, chartCRD) {
		t.Fatal("CRD with a different reviewed spec was accepted")
	}
}

func TestComponentDependencyCRDsMustBeInstalledEstablishedAndReadable(t *testing.T) {
	for _, scenario := range []string{"ready", "missing", "forbidden", "error", "not-established", "custom-api-forbidden"} {
		t.Run(scenario, func(t *testing.T) {
			listKinds := map[schema.GroupVersionResource]string{}
			for _, name := range []string{"virtualservices.networking.istio.io", "destinationrules.networking.istio.io", "gateways.networking.istio.io", "authorizationpolicies.security.istio.io", "peerauthentications.security.istio.io"} {
				split := strings.SplitN(name, ".", 2)
				listKinds[schema.GroupVersionResource{Group: split[1], Version: "v1", Resource: split[0]}] = "DependencyList"
			}
			dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
			dyn.PrependReactor("*", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
				if scenario == "missing" {
					return true, nil, apierrors.NewNotFound(action.GetResource().GroupResource(), "dependency")
				}
				if scenario == "forbidden" || scenario == "custom-api-forbidden" && action.GetVerb() == "list" {
					return true, nil, apierrors.NewForbidden(action.GetResource().GroupResource(), "dependency", errors.New("denied"))
				}
				if scenario == "error" {
					return true, nil, errors.New("connection failed")
				}
				if action.GetVerb() == "list" {
					return true, &unstructured.UnstructuredList{}, nil
				}
				name := action.(ktesting.GetAction).GetName()
				split := strings.SplitN(name, ".", 2)
				obj := componentTestCRD()
				obj.SetName(name)
				_ = unstructured.SetNestedField(obj.Object, split[1], "spec", "group")
				_ = unstructured.SetNestedField(obj.Object, split[0], "spec", "names", "plural")
				if scenario != "not-established" {
					_ = unstructured.SetNestedSlice(obj.Object, []any{map[string]any{"type": "Established", "status": "True"}}, "status", "conditions")
				}
				return true, obj, nil
			})
			err := componentVerifyDependencies(context.Background(), &kubeClients{dyn: dyn}, componentProfile{Dependencies: []string{"istio-base"}})
			if (err == nil) != (scenario == "ready") {
				t.Fatalf("dependency result: %v", err)
			}
		})
	}
}

func TestComponentReviewedHookLifecycle(t *testing.T) {
	p, _ := componentProfileFor("argocd")
	obj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "batch/v1", "kind": "Job", "metadata": map[string]any{"name": "argo-redis-secret-init", "annotations": map[string]any{"helm.sh/hook": "pre-install,pre-upgrade", "helm.sh/hook-delete-policy": "before-hook-creation,hook-succeeded"}}}}
	if err := componentCheckHook(obj, p); err != nil {
		t.Fatal(err)
	}
	obj.SetName("unreviewed")
	if err := componentCheckHook(obj, p); err == nil {
		t.Fatal("arbitrary hook accepted")
	}
	obj.SetName("argo-redis-secret-init")
	obj.SetAnnotations(map[string]string{"helm.sh/hook": "post-delete"})
	if err := componentCheckHook(obj, p); err == nil {
		t.Fatal("unreviewed lifecycle accepted")
	}
}

func TestComponentWorkerSurvivesRequestCancellationAndHasDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := make(chan error, 1)
	runComponentBackground(ctx, func(worker context.Context) {
		deadline, ok := worker.Deadline()
		if worker.Err() != nil || !ok || time.Until(deadline) > 12*time.Minute {
			result <- errors.New("worker inherited cancellation or lacks bounded deadline")
			return
		}
		result <- nil
	})
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
}

func TestComponentMaintainedCompatibilityIsTargetSpecific(t *testing.T) {
	for _, key := range []string{"istio-base", "istiod", "istio-gateway", "cert-manager"} {
		def, _ := componentDefinition(key)
		if !def.Supported || !def.Installable {
			t.Fatalf("%s must remain an enabled engine profile", key)
		}
		constraint, err := semver.NewConstraint(def.KubeVersion)
		if err != nil {
			t.Fatal(err)
		}
		if constraint.Check(mustSemver("1.29.0")) || !constraint.Check(mustSemver("1.34.0")) || constraint.Check(mustSemver("1.37.0")) {
			t.Fatalf("%s has incorrect bounded target compatibility: %s", key, def.KubeVersion)
		}
	}
}

// This opt-in check contacts official chart repositories only. It never creates
// Kubernetes clients or installs charts into a cluster.
func TestComponentOfficialChartsFetchAndRender(t *testing.T) {
	if os.Getenv("MATRIX_TEST_LIVE_COMPONENT_CHART") != "1" {
		t.Skip("official network chart fetch/render is opt-in")
	}
	for _, def := range sortedComponentCatalog() {
		if !def.Installable {
			continue
		}
		t.Run(def.Key, func(t *testing.T) {
			t.Parallel()
			ch, _, err := fetchVerifiedComponentChart(context.Background(), def, "")
			if err != nil {
				t.Fatal(err)
			}
			p, _ := componentProfileFor(def.Key)
			_, values, err := validateComponentRequest(def.Key, componentInstallRequest{Namespace: "test", ReleaseName: "matrix-" + def.Key, Version: def.Versions[0]})
			if err != nil {
				t.Fatal(err)
			}
			if err := chartutil.ProcessDependencies(ch, values); err != nil {
				t.Fatal(err)
			}
			caps := chartutil.DefaultCapabilities.Copy()
			caps.KubeVersion = chartutil.KubeVersion{Version: "v1.34.0", Major: "1", Minor: "34"}
			caps.APIVersions = append(caps.APIVersions, "monitoring.coreos.com/v1")
			vals, err := chartutil.ToRenderValues(ch, values, chartutil.ReleaseOptions{Name: "matrix-" + def.Key, Namespace: "test", IsInstall: true}, caps)
			if err != nil {
				t.Fatal(err)
			}
			rendered, err := engine.Render(ch, vals)
			if err != nil {
				t.Fatal(err)
			}
			for _, crd := range ch.CRDObjects() {
				rendered[crd.Filename] = string(crd.File.Data)
			}
			objects, err := componentRenderedObjects(rendered, "test", p)
			if err != nil || len(objects) == 0 {
				t.Fatalf("official render: %v", err)
			}
			t.Logf("official profile %s chart=%s version=%s rendered=%d", def.Key, def.Chart, def.Versions[0], len(objects))
			constraint, err := semver.NewConstraint(p.KubeVersion)
			if err != nil || !constraint.Check(mustSemver("1.34.0")) {
				t.Fatal("profile compatibility constraint rejected fixture target")
			}
		})
	}
}
