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
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/repo"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/kube-openapi/pkg/validation/validate"
)

const (
	componentChartIndexLimit    = 32 << 20
	componentChartArchiveLimit  = 32 << 20
	componentChartExpandedLimit = 128 << 20
	componentRenderLimit        = 16 << 20
	componentDocumentLimit      = 1024
)

// The approved digest binds the complete upstream archive, including bundled
// dependencies. No repository, version or URL is supplied by the user.
func fetchVerifiedComponentChart(ctx context.Context, def ComponentDefinition, expectedDigest string) (*chart.Chart, string, error) {
	client := componentVerifiedHTTPClient()
	defer client.CloseIdleConnections()
	return fetchVerifiedComponentChartWithClient(ctx, def, expectedDigest, client)
}

func fetchVerifiedComponentChartWithClient(ctx context.Context, def ComponentDefinition, expectedDigest string, client *http.Client) (*chart.Chart, string, error) {
	profile, ok := componentProfileFor(def.Key)
	if !ok || !def.Installable || profile.Chart != def.Chart ||
		def.RepoURL != "https://"+profile.RepoHost+profile.RepoPath ||
		len(def.Versions) != 1 || len(profile.Versions) != 1 || profile.Versions[0] != def.Versions[0] {
		return nil, "", errors.New("component is outside the verified chart allowlist")
	}
	base, err := componentVerifiedURL(def.RepoURL, false)
	if err != nil {
		return nil, "", errors.New("curated chart repository URL rejected")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	fetch := func(u *url.URL, limit int64) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("curated chart server returned HTTP %d", resp.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil || int64(len(data)) > limit {
			return nil, errors.New("curated chart response exceeded its size limit")
		}
		return data, nil
	}

	indexURL := *base
	indexURL.Path = strings.TrimRight(base.Path, "/") + "/index.yaml"
	indexData, err := fetch(&indexURL, componentChartIndexLimit)
	if err != nil {
		return nil, "", componentFetchError("index", err)
	}
	index := &repo.IndexFile{}
	if err := yaml.Unmarshal(indexData, index); err != nil {
		return nil, "", errors.New("curated chart index is invalid")
	}
	entry, err := index.Get(def.Chart, def.Versions[0])
	if err != nil || entry == nil || entry.Metadata == nil || len(entry.URLs) != 1 {
		return nil, "", errors.New("pinned chart is absent from the repository index")
	}
	digest := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(entry.Digest), "sha256:"))
	if len(digest) != 64 {
		return nil, "", errors.New("repository index has no valid SHA-256 chart digest")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return nil, "", errors.New("repository chart digest is invalid")
	}
	if expectedDigest != "" {
		if strings.TrimSpace(expectedDigest) != expectedDigest || expectedDigest != digest {
			return nil, "", errors.New("chart digest does not match the pinned digest")
		}
	}
	archiveBase := *base
	archiveBase.Path = strings.TrimRight(base.Path, "/") + "/"
	archiveURL, err := archiveBase.Parse(entry.URLs[0])
	if err != nil {
		return nil, "", errors.New("chart archive URL is invalid")
	}
	archiveURL, err = componentVerifiedURL(archiveURL.String(), true)
	if err != nil || !stringInList(componentArchiveURLs(profile), archiveURL.String()) {
		return nil, "", errors.New("chart archive URL rejected")
	}
	archive, err := fetch(archiveURL, componentChartArchiveLimit)
	if err != nil {
		return nil, "", componentFetchError("archive", err)
	}
	sum := sha256.Sum256(archive)
	if hex.EncodeToString(sum[:]) != digest {
		return nil, "", errors.New("curated chart archive digest mismatch")
	}
	if err := componentCheckChartArchive(archive); err != nil {
		return nil, "", errors.New("chart archive failed bounded archive validation")
	}
	if entry.Metadata.Name != def.Chart || entry.Metadata.Version != def.Versions[0] {
		return nil, "", errors.New("repository index chart metadata mismatch")
	}
	loaded, err := loader.LoadArchive(bytes.NewReader(archive))
	if err != nil || loaded == nil || loaded.Metadata == nil {
		return nil, "", errors.New("chart archive could not be loaded")
	}
	if loaded.Metadata.Name != def.Chart || loaded.Metadata.Version != def.Versions[0] ||
		!componentChartDependenciesAllowed(loaded, profile) ||
		(profile.CRDStrategy == "none" && len(loaded.CRDObjects()) != 0) {
		return nil, "", errors.New("chart contents differ from the reviewed chart contract")
	}
	return loaded, digest, nil
}

func componentFetchError(phase string, err error) error {
	// Never include signed redirect URLs or transport error URLs in logs.
	var requestError *url.Error
	if errors.As(err, &requestError) {
		err = requestError.Err
	}
	reason := "network or HTTPS validation failed"
	var networkError net.Error
	var dnsError *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		reason = "request deadline exceeded"
	case errors.As(err, &dnsError):
		reason = "DNS resolution failed"
	case errors.As(err, &networkError) && networkError.Timeout():
		reason = "network timeout"
	case strings.HasPrefix(err.Error(), "curated chart server returned HTTP "):
		reason = err.Error()
	case strings.HasPrefix(err.Error(), "curated chart response exceeded"):
		reason = "response size limit exceeded"
	case strings.Contains(err.Error(), "redirect"):
		reason = "redirect rejected by pinned artifact policy"
	}
	return fmt.Errorf("curated chart %s fetch failed: %s", phase, reason)
}

func componentVerifiedURL(raw string, archive bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		u.Port() != "" || u.Opaque != "" || u.RawPath != "" ||
		strings.Contains(u.Path, "\\") || path.Clean(u.Path) != u.Path && u.Path != "" {
		return nil, errors.New("invalid HTTPS URL")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if u.Host != host {
		return nil, errors.New("non-canonical URL host")
	}
	if !archive {
		allowed := false
		for _, profile := range curatedComponentProfiles {
			allowed = allowed || host == profile.RepoHost && u.Path == profile.RepoPath
		}
		if !allowed {
			return nil, errors.New("repository URL is not curated")
		}
	} else {
		allowed := false
		for _, profile := range curatedComponentProfiles {
			allowed = allowed || stringInList(componentArchiveURLs(profile), u.String())
		}
		if !allowed {
			return nil, errors.New("archive URL is not curated")
		}
	}
	return u, nil
}

func componentArchiveURLs(p componentProfile) []string {
	if len(p.Versions) != 1 {
		return nil
	}
	version := p.Versions[0]
	file := p.Chart + "-" + version + ".tgz"
	urls := []string{"https://" + p.RepoHost + p.RepoPath + "/" + file}
	project, tag := "", p.Chart+"-"+version
	switch p.Key {
	case "traefik":
		project, tag = "traefik/traefik-helm-chart", "v"+version
		urls = append(urls, "https://traefik.github.io/charts/traefik/"+file)
	case "argocd":
		project = "argoproj/argo-helm"
	case "jenkins":
		project = "jenkinsci/helm-charts"
	case "prometheus", "alertmanager":
		project = "prometheus-community/helm-charts"
	case "tempo":
		project = "grafana/helm-charts"
	case "loki":
		project, tag = "grafana/helm-charts", "helm-loki-"+version
	case "jaeger":
		project = "jaegertracing/helm-charts"
	case "cert-manager":
		urls = append(urls, "https://charts.jetstack.io/charts/"+file)
	}
	if project != "" {
		urls = append(urls, "https://github.com/"+project+"/releases/download/"+tag+"/"+file)
	}
	return urls
}

func componentChartDependenciesAllowed(ch *chart.Chart, p componentProfile) bool {
	for _, dep := range ch.Dependencies() {
		if dep.Metadata == nil || !stringInList(p.ChartDependencies, dep.Metadata.Name) ||
			!componentChartDependenciesAllowed(dep, p) {
			return false
		}
	}
	return true
}

func componentPinnedReleaseURL(raw string) (*url.URL, error) {
	u, err := componentVerifiedURL(raw, true)
	if err != nil || u.Host != "github.com" {
		return nil, errors.New("chart release URL is not the pinned artifact")
	}
	return u, nil
}

func componentAllowedReleaseRedirect(req *http.Request, via []*http.Request) error {
	if req == nil || len(via) == 0 || len(via) > 2 {
		return errors.New("invalid release redirect chain")
	}
	if _, err := componentPinnedReleaseURL(via[0].URL.String()); err != nil {
		return err
	}
	for i := 1; i < len(via); i++ {
		if err := componentReleaseAssetURL(via[i].URL); err != nil {
			return err
		}
		if via[i].URL.Path != req.URL.Path {
			return errors.New("release asset redirect changed the pinned artifact path")
		}
	}
	return componentReleaseAssetURL(req.URL)
}

func componentReleaseAssetURL(u *url.URL) error {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Port() != "" ||
		u.Fragment != "" || u.Opaque != "" || path.Clean(u.Path) != u.Path ||
		!strings.HasPrefix(u.Path, "/github-production-release-asset/") {
		return errors.New("release asset redirect URL rejected")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if u.Host != host || (host != "release-assets.githubusercontent.com" &&
		host != "objects.githubusercontent.com") {
		return errors.New("release asset redirect host rejected")
	}
	query := u.Query()
	for key, values := range query {
		switch key {
		case "sp", "sv", "sr", "spr", "se", "sig", "sk", "skt", "ske",
			"skoid", "sktid", "sks", "skv", "rscd", "rsct", "jwt",
			"response-content-disposition", "response-content-type":
			if len(values) != 1 || values[0] == "" {
				return errors.New("release asset signature query rejected")
			}
		default:
			return errors.New("release asset query rejected")
		}
	}
	if query.Get("sig") == "" || query.Get("se") == "" {
		return errors.New("release asset signature is incomplete")
	}
	return nil
}

func componentVerifiedHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          4,
		MaxConnsPerHost:       2,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil || port != "443" {
				return nil, errors.New("invalid outbound chart address")
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil || len(ips) == 0 || len(ips) > 16 {
				return nil, errors.New("chart host resolution failed")
			}
			for _, item := range ips {
				ip := item.IP
				if v4 := ip.To4(); v4 != nil {
					ip = v4
				}
				if !componentGlobalUnicast(ip) {
					return nil, errors.New("chart host resolved to a non-public address")
				}
			}
			var last error
			for _, item := range ips {
				ip := item.IP
				if v4 := ip.To4(); v4 != nil {
					ip = v4
				}
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				last = err
			}
			return nil, last
		},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   40 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many chart redirects")
			}
			if len(via) > 0 && via[0].URL.Host == "github.com" {
				return componentAllowedReleaseRedirect(req, via)
			}
			if len(via) == 0 || req.URL.String() != via[0].URL.String() {
				return errors.New("chart storage redirect changed the pinned URL")
			}
			return nil
		},
	}
}

func componentGlobalUnicast(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip == nil || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || !ip.IsGlobalUnicast() {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		for _, block := range []string{
			"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
			"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
			"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15",
			"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		} {
			prefix := netip.MustParsePrefix(block)
			if prefix.Contains(addr) {
				return false
			}
		}
		return true
	}
	if addr.Is6() {
		for _, block := range []string{
			"::/128", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48",
			"100::/64", "2001::/23", "2001:db8::/32", "2002::/16",
			"3fff::/20", "fc00::/7", "fe80::/10", "ff00::/8",
		} {
			prefix := netip.MustParsePrefix(block)
			if prefix.Contains(addr) {
				return false
			}
		}
		if !netip.MustParsePrefix("2000::/3").Contains(addr) {
			return false
		}
		return true
	}
	return false
}

func componentCheckChartArchive(data []byte) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer gz.Close()
	uncompressed, err := io.ReadAll(io.LimitReader(gz, componentChartExpandedLimit+1))
	if err != nil || int64(len(uncompressed)) > componentChartExpandedLimit {
		return errors.New("expanded archive is too large")
	}
	tr := tar.NewReader(bytes.NewReader(uncompressed))
	files := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		files++
		if files > 4096 || h.Size < 0 {
			return errors.New("archive entry count or size invalid")
		}
		if path.IsAbs(h.Name) || path.Clean(h.Name) != h.Name || strings.HasPrefix(h.Name, "../") || strings.Contains(h.Name, "\\") {
			return errors.New("unsafe archive path")
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA && h.Typeflag != tar.TypeDir {
			return errors.New("unsupported archive entry type")
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return err
		}
	}
	return nil
}

func prepareVerifiedComponentChart(ctx context.Context, kc *kubeClients, ch *chart.Chart, values map[string]any, in componentInstallRequest) error {
	if kc == nil || kc.cs == nil || kc.dyn == nil || kc.mapper == nil || ch == nil ||
		ch.Metadata == nil {
		return errors.New("verified chart or cluster client unavailable")
	}
	def, ok := componentDefinition(in.Component)
	if in.Component == "" {
		in.Component = ch.Metadata.Name
		def, ok = componentDefinition(in.Component)
	}
	if !ok || !def.Installable || ch.Metadata.Name != def.Chart || ch.Metadata.Version != in.Version {
		return errors.New("verified chart or component profile unavailable")
	}
	profile, _ := componentProfileFor(in.Component)
	if !componentChartDependenciesAllowed(ch, profile) {
		return errors.New("chart contains unreviewed dependencies")
	}
	if profile.CRDStrategy == "none" && len(ch.CRDObjects()) > 0 {
		return errors.New("chart contains unreviewed CRDs")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	namespace, err := kc.cs.CoreV1().Namespaces().Get(ctx, in.Namespace, metav1GetOptions())
	if err != nil || namespace == nil || namespace.Status.Phase != corev1.NamespaceActive {
		return errors.New("target namespace is unavailable")
	}
	releases, err := kc.cs.CoreV1().Secrets(in.Namespace).List(ctx, metav1.ListOptions{
		Limit: 1, LabelSelector: "owner=helm,name=" + in.ReleaseName, FieldSelector: "type=helm.sh/release.v1",
	})
	if err != nil {
		return errors.New("Helm release conflict check failed")
	}
	if len(releases.Items) > 0 {
		return errors.New("a Helm release with this name already exists")
	}
	discoveryClient := kc.cs.Discovery()
	if discoveryClient == nil || discoveryClient.RESTClient() == nil {
		return errors.New("Kubernetes readiness client is unavailable")
	}
	if _, err := discoveryClient.RESTClient().Get().AbsPath("/readyz").Do(ctx).Raw(); err != nil {
		return errors.New("Kubernetes API is not ready")
	}
	versionBody, err := discoveryClient.RESTClient().Get().AbsPath("/version").Do(ctx).Raw()
	if err != nil || len(versionBody) > 64<<10 {
		return errors.New("Kubernetes version discovery failed")
	}
	var serverVersion struct {
		GitVersion string `json:"gitVersion"`
	}
	if err := json.Unmarshal(versionBody, &serverVersion); err != nil {
		return errors.New("Kubernetes version discovery failed")
	}
	kubeVersion, err := chartutil.ParseKubeVersion(serverVersion.GitVersion)
	if err != nil {
		return errors.New("Kubernetes version is invalid")
	}
	if ch.Metadata.KubeVersion != "" {
		constraint, err := semver.NewConstraint(ch.Metadata.KubeVersion)
		if err != nil || !constraint.Check(mustSemver(kubeVersion.Version)) {
			return httpx.Conflict("target Kubernetes version does not satisfy chart requirements: " + ch.Metadata.KubeVersion)
		}
	}
	if profile.KubeVersion != "" {
		constraint, err := semver.NewConstraint(profile.KubeVersion)
		if err != nil || !constraint.Check(mustSemver(kubeVersion.Version)) {
			return httpx.Conflict("target Kubernetes version is incompatible with the reviewed component profile: " + profile.KubeVersion)
		}
	}
	groups, discoveryErr := capabilityAPIGroups(ctx, kc)
	if discoveryErr != nil || groups == nil || len(groups.Groups) > 128 {
		return errors.New("Kubernetes API capability discovery failed")
	}
	if err := componentVerifyDependencies(ctx, kc, profile); err != nil {
		return err
	}
	coreResources, discoveryErr := capabilityAPIResources(ctx, kc, "v1")
	if discoveryErr != nil || coreResources == nil {
		return errors.New("Kubernetes core API discovery failed")
	}
	resourceLists := []*metav1.APIResourceList{coreResources}
	for _, group := range groups.Groups {
		if group.PreferredVersion.GroupVersion == "" {
			return errors.New("Kubernetes API preferred version is unavailable")
		}
		resources, err := capabilityAPIResources(ctx, kc, group.PreferredVersion.GroupVersion)
		if err != nil || resources == nil {
			return errors.New("Kubernetes API capability discovery failed")
		}
		resourceLists = append(resourceLists, resources)
	}
	apiVersions := chartutil.VersionSet{"v1"}
	for _, list := range resourceLists {
		if list == nil {
			continue
		}
		apiVersions = append(apiVersions, list.GroupVersion)
		for _, resource := range list.APIResources {
			apiVersions = append(apiVersions, list.GroupVersion+"/"+resource.Kind)
		}
	}
	if in.Component == "traefik" && !apiVersions.Has("networking.k8s.io/v1/IngressClass") {
		return errors.New("required networking.k8s.io/v1 IngressClass API is unavailable")
	}
	caps := chartutil.Capabilities{KubeVersion: *kubeVersion, APIVersions: apiVersions}
	if err := chartutil.ProcessDependencies(ch, values); err != nil {
		return errors.New("chart dependencies are not bundled or values are invalid")
	}
	renderValues, err := chartutil.ToRenderValues(ch, values, chartutil.ReleaseOptions{
		Name: in.ReleaseName, Namespace: in.Namespace, Revision: 1, IsInstall: true,
	}, &caps)
	if err != nil {
		return errors.New("chart values failed validation")
	}
	rendered, err := (engine.Engine{}).Render(ch, renderValues)
	if err != nil {
		return errors.New("chart rendering failed")
	}
	for _, crd := range ch.CRDObjects() {
		rendered["matrix-crds/"+crd.Filename] = string(crd.File.Data)
	}
	objects, err := componentRenderedObjects(rendered, in.Namespace, profile)
	if err != nil || len(objects) == 0 {
		return errors.New("chart rendered no acceptable Kubernetes resources")
	}
	if err := componentVerifyPVCs(ctx, kc, objects); err != nil {
		return err
	}
	for _, obj := range objects {
		gv, err := schema.ParseGroupVersion(obj.GetAPIVersion())
		futureAPI := false
		if err != nil {
			return errors.New("rendered resource API version is invalid")
		}
		mapping, err := kc.mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: obj.GetKind()}, gv.Version)
		if err != nil {
			kc.mapper.Reset()
			mapping, err = kc.mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: obj.GetKind()}, gv.Version)
		}
		if err != nil && meta.IsNoMatchError(err) {
			mapping, err = componentCRDMapping(objects, obj)
			futureAPI = err == nil
		}
		if err != nil {
			return errors.New("rendered resource API is not installed")
		}
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			if obj.GetNamespace() == "" {
				obj.SetNamespace(in.Namespace)
			}
			if obj.GetNamespace() != in.Namespace {
				return errors.New("chart resource targets a different namespace")
			}
		} else if obj.GetNamespace() != "" {
			return errors.New("cluster-scoped chart resource has a namespace")
		}
		gvr := mapping.Resource
		var resource dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			resource = kc.dyn.Resource(gvr).Namespace(in.Namespace)
		} else {
			resource = kc.dyn.Resource(gvr)
		}
		reuseCRD := false
		if !futureAPI {
			if existing, err := resource.Get(ctx, obj.GetName(), metav1.GetOptions{}); err == nil {
				if obj.GetKind() == "CustomResourceDefinition" && componentReusableCRD(existing, obj) {
					reuseCRD = true
				} else {
					return errors.New("chart resource already exists; adoption is forbidden")
				}
			} else if !apierrors.IsNotFound(err) {
				return errors.New("existing chart resource check failed")
			}
		}
		verbs := []string{"get", "create", "list", "watch", "delete"}
		if reuseCRD {
			verbs = []string{"get", "list", "watch"}
		}
		for _, verb := range verbs {
			name := obj.GetName()
			if verb != "get" {
				name = ""
			}
			review := &authv1.SelfSubjectAccessReview{Spec: authv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authv1.ResourceAttributes{
					Verb: verb, Group: mapping.Resource.Group, Resource: mapping.Resource.Resource,
					Namespace: obj.GetNamespace(), Name: name,
				},
			}}
			result, err := kc.cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
			if err != nil || result == nil || !result.Status.Allowed {
				return errors.New("Kubernetes permissions are insufficient for rendered chart resources")
			}
		}
		if futureAPI {
			if err := componentValidateFutureResource(objects, obj); err != nil {
				return err
			}
			continue
		}
		if reuseCRD {
			continue
		}
		if _, err := resource.Create(ctx, obj, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}); err != nil {
			return errors.New("Kubernetes server-side dry-run rejected rendered chart resources")
		}
	}
	// Helm stores releases as Secrets and reads workloads/events while waiting.
	for _, verb := range []string{"get", "list", "create", "update", "delete"} {
		review := &authv1.SelfSubjectAccessReview{Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{Verb: verb, Group: "", Resource: "secrets", Namespace: in.Namespace},
		}}
		result, err := kc.cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
		if err != nil || result == nil || !result.Status.Allowed {
			return errors.New("Kubernetes Helm release storage permissions are insufficient")
		}
	}
	for _, resource := range []struct{ group, name string }{{"", "pods"}, {"apps", "replicasets"}} {
		for _, verb := range []string{"get", "list", "watch"} {
			review := &authv1.SelfSubjectAccessReview{Spec: authv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authv1.ResourceAttributes{Verb: verb, Group: resource.group, Resource: resource.name, Namespace: in.Namespace},
			}}
			result, err := kc.cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
			if err != nil || result == nil || !result.Status.Allowed {
				return errors.New("Kubernetes workload readiness permissions are insufficient")
			}
		}
	}
	componentFreezeChart(ch, objects)
	return nil
}

func componentRenderedObjects(rendered map[string]string, namespace string, profile componentProfile) ([]*unstructured.Unstructured, error) {
	objects := make([]*unstructured.Unstructured, 0)
	seen := make(map[string]struct{})
	var total int
	files := make([]string, 0, len(rendered))
	for file := range rendered {
		files = append(files, file)
	}
	sort.Strings(files)
	for _, file := range files {
		text := rendered[file]
		if path.Base(file) == "NOTES.txt" {
			continue
		}
		total += len(text)
		if total > componentRenderLimit {
			return nil, errors.New("rendered chart exceeds size limit")
		}
		decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(text), 4096)
		for {
			raw := map[string]any{}
			err := decoder.Decode(&raw)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			if len(raw) == 0 {
				continue
			}
			obj := &unstructured.Unstructured{Object: raw}
			if obj.GetAPIVersion() == "" || obj.GetKind() == "" || obj.GetName() == "" {
				return nil, errors.New("chart rendered an unsupported or unnamed resource")
			}
			if obj.GetKind() == "CustomResourceDefinition" || obj.GetAPIVersion() == "apiextensions.k8s.io/v1" {
				if profile.CRDStrategy != "reviewed" {
					return nil, errors.New("chart CRD is outside the reviewed profile")
				}
			}
			hook := obj.GetAnnotations()["helm.sh/hook"]
			if hook != "" && componentTestHook(hook) {
				continue
			}
			if err := componentCheckHook(obj, profile); err != nil {
				return nil, err
			}
			gv, err := schema.ParseGroupVersion(obj.GetAPIVersion())
			if err != nil {
				return nil, errors.New("chart rendered an invalid API version")
			}
			effectiveNamespace := obj.GetNamespace()
			if effectiveNamespace == "" {
				effectiveNamespace = namespace
			}
			key := gv.Group + "/" + gv.Version + "/" + obj.GetKind() + "/" + effectiveNamespace + "/" + obj.GetName()
			if _, exists := seen[key]; exists {
				return nil, errors.New("chart rendered duplicate Kubernetes resources")
			}
			seen[key] = struct{}{}
			if len(objects) >= componentDocumentLimit {
				return nil, errors.New("chart rendered too many resources")
			}
			objects = append(objects, obj)
		}
	}
	sort.SliceStable(objects, func(i, j int) bool {
		return objects[i].GetKind() == "CustomResourceDefinition" && objects[j].GetKind() != "CustomResourceDefinition"
	})
	return objects, nil
}

func componentTestHook(hook string) bool {
	for _, event := range strings.Split(hook, ",") {
		if event = strings.TrimSpace(event); event != "test" && event != "test-success" && event != "test-failure" {
			return false
		}
	}
	return true
}

func componentCheckHook(obj *unstructured.Unstructured, p componentProfile) error {
	hook := obj.GetAnnotations()["helm.sh/hook"]
	if hook == "" {
		for key := range obj.GetAnnotations() {
			if strings.HasPrefix(key, "helm.sh/hook-") {
				return errors.New("orphan Helm hook annotation")
			}
		}
		return nil
	}
	if p.Key != "argocd" || p.HookStrategy != "reviewed" ||
		!strings.HasSuffix(obj.GetName(), "-redis-secret-init") ||
		!stringInList([]string{"Job", "Role", "RoleBinding", "ServiceAccount"}, obj.GetKind()) {
		return errors.New("chart hook is outside the reviewed lifecycle")
	}
	for _, event := range strings.Split(hook, ",") {
		if event = strings.TrimSpace(event); event != "pre-install" && event != "pre-upgrade" {
			return errors.New("chart hook lifecycle is not allowed")
		}
	}
	for _, policy := range strings.Split(obj.GetAnnotations()["helm.sh/hook-delete-policy"], ",") {
		if policy = strings.TrimSpace(policy); policy != "" && policy != "before-hook-creation" && policy != "hook-succeeded" {
			return errors.New("chart hook deletion policy is not allowed")
		}
	}
	return nil
}

var componentCRDGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

func componentCRDReady(obj *unstructured.Unstructured) bool {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, raw := range conditions {
		if condition, ok := raw.(map[string]any); ok && condition["type"] == "Established" && condition["status"] == "True" {
			return obj.GetDeletionTimestamp() == nil
		}
	}
	return false
}

func componentReusableCRD(existing, wanted *unstructured.Unstructured) bool {
	if existing == nil || wanted == nil || !componentCRDReady(existing) {
		return false
	}
	wantedSpec, err := componentCanonicalCRDSpec(wanted)
	if err != nil {
		return false
	}
	existingSpec, err := componentCanonicalCRDSpec(existing)
	return err == nil && apiequality.Semantic.DeepDerivative(wantedSpec, existingSpec)
}

func componentCanonicalCRDSpec(obj *unstructured.Unstructured) (any, error) {
	spec, found := obj.Object["spec"]
	if !found {
		return nil, errors.New("CRD spec is unavailable")
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var canonical any
	if err := decoder.Decode(&canonical); err != nil {
		return nil, err
	}
	return canonical, nil
}

func componentVerifyDependencies(ctx context.Context, kc *kubeClients, p componentProfile) error {
	for _, key := range p.Dependencies {
		if key == "istio-base" || key == "istiod" {
			for _, name := range []string{"virtualservices.networking.istio.io", "destinationrules.networking.istio.io", "gateways.networking.istio.io", "authorizationpolicies.security.istio.io", "peerauthentications.security.istio.io"} {
				obj, err := kc.dyn.Resource(componentCRDGVR).Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return fmt.Errorf("required dependency CRD %s is not installed or readable: %w", name, err)
				}
				if !componentCRDReady(obj) {
					return errors.New("required dependency CRD is not established: " + name)
				}
				var crd apiextv1.CustomResourceDefinition
				data, _ := json.Marshal(obj.Object)
				if json.Unmarshal(data, &crd) != nil || len(crd.Spec.Versions) == 0 {
					return errors.New("required dependency CRD is invalid")
				}
				version := ""
				for _, v := range crd.Spec.Versions {
					if v.Served && v.Storage {
						version = v.Name
					}
				}
				if version == "" {
					return errors.New("required dependency CRD has no served storage version")
				}
				gvr := schema.GroupVersionResource{Group: crd.Spec.Group, Version: version, Resource: crd.Spec.Names.Plural}
				if _, err := kc.dyn.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
					return fmt.Errorf("required dependency API %s is not readable: %w", name, err)
				}
			}
		}
		if key != "istio-base" {
			workloads := listComponentWorkloads(ctx, kc)
			if workloads.err != nil || workloads.truncated {
				return errors.New("component dependency discovery is incomplete")
			}
			capability := componentCapabilityContext(ctx, kc, key, componentProbeForCatalogKey(key), workloads)
			if capability.State != "available" {
				return errors.New("required component dependency is not installed and ready: " + key)
			}
		}
	}
	return nil
}

func componentCRDMapping(objects []*unstructured.Unstructured, obj *unstructured.Unstructured) (*meta.RESTMapping, error) {
	for _, candidate := range objects {
		if candidate.GetKind() != "CustomResourceDefinition" {
			continue
		}
		var crd apiextv1.CustomResourceDefinition
		data, _ := json.Marshal(candidate.Object)
		if json.Unmarshal(data, &crd) != nil {
			continue
		}
		for _, version := range crd.Spec.Versions {
			if version.Served && crd.Spec.Group+"/"+version.Name == obj.GetAPIVersion() && crd.Spec.Names.Kind == obj.GetKind() {
				scope := meta.RESTScopeRoot
				if crd.Spec.Scope == apiextv1.NamespaceScoped {
					scope = meta.RESTScopeNamespace
				}
				return &meta.RESTMapping{Resource: schema.GroupVersionResource{Group: crd.Spec.Group, Version: version.Name, Resource: crd.Spec.Names.Plural}, GroupVersionKind: schema.GroupVersionKind{Group: crd.Spec.Group, Version: version.Name, Kind: obj.GetKind()}, Scope: scope}, nil
			}
		}
	}
	return nil, errors.New("rendered resource has no served API or reviewed CRD")
}

func componentValidateFutureResource(objects []*unstructured.Unstructured, obj *unstructured.Unstructured) error {
	for _, candidate := range objects {
		if candidate.GetKind() != "CustomResourceDefinition" {
			continue
		}
		var crd apiextv1.CustomResourceDefinition
		data, _ := json.Marshal(candidate.Object)
		if json.Unmarshal(data, &crd) != nil {
			continue
		}
		for _, version := range crd.Spec.Versions {
			if !version.Served || crd.Spec.Group+"/"+version.Name != obj.GetAPIVersion() || crd.Spec.Names.Kind != obj.GetKind() {
				continue
			}
			if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
				return errors.New("future custom resource has no reviewed validation schema")
			}
			var schema spec.Schema
			data, err := json.Marshal(version.Schema.OpenAPIV3Schema)
			if err != nil || json.Unmarshal(data, &schema) != nil {
				return errors.New("custom resource schema cannot be read")
			}
			if !validate.NewSchemaValidator(&schema, nil, "", strfmt.Default).Validate(obj.Object).IsValid() {
				return errors.New("custom resource failed reviewed CRD schema validation")
			}
			return nil
		}
	}
	return errors.New("custom resource validation schema is unavailable")
}

// Freeze the already validated render, including chart-generated secrets. Helm
// evaluates only Files.Get; embedded application templates remain literal data.
func componentFreezeChart(ch *chart.Chart, objects []*unstructured.Unstructured) {
	ch.Templates = nil
	ch.Files = nil
	ch.SetDependencies()
	metadata := *ch.Metadata
	metadata.Dependencies = nil
	ch.Metadata = &metadata
	for i, obj := range objects {
		data, _ := json.Marshal(obj.Object)
		if obj.GetKind() == "CustomResourceDefinition" {
			ch.Files = append(ch.Files, &chart.File{Name: fmt.Sprintf("crds/%d.json", i), Data: data})
			continue
		}
		name := fmt.Sprintf("matrix-rendered/%d.json", i)
		ch.Files = append(ch.Files, &chart.File{Name: name, Data: data})
		ch.Templates = append(ch.Templates, &chart.File{Name: fmt.Sprintf("templates/%d.yaml", i), Data: []byte(fmt.Sprintf("{{ .Files.Get %q }}", name))})
	}
}

func componentVerifyPVCs(ctx context.Context, kc *kubeClients, objects []*unstructured.Unstructured) error {
	claims := append([]*unstructured.Unstructured(nil), objects...)
	for _, obj := range objects {
		templates, _, err := unstructured.NestedSlice(obj.Object, "spec", "volumeClaimTemplates")
		if err != nil {
			return errors.New("invalid chart volume claim templates")
		}
		for _, raw := range templates {
			data, ok := raw.(map[string]any)
			if !ok {
				return errors.New("invalid chart volume claim template")
			}
			claim := &unstructured.Unstructured{Object: data}
			claim.SetKind("PersistentVolumeClaim")
			claims = append(claims, claim)
		}
	}
	for _, obj := range claims {
		if obj.GetKind() != "PersistentVolumeClaim" {
			continue
		}
		name, found, _ := unstructured.NestedString(obj.Object, "spec", "storageClassName")
		if found && name == "" {
			return errors.New("PVC without dynamic storage provisioning is unsupported")
		}
		if !found {
			continueToken := ""
			seenTokens := make(map[string]struct{})
			for page := 0; page < 50; page++ {
				list, err := kc.cs.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{Limit: 100, Continue: continueToken})
				if err != nil {
					return errors.New("storage class discovery failed")
				}
				if list == nil {
					return errors.New("storage class discovery returned no result")
				}
				for _, class := range list.Items {
					if class.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" ||
						class.Annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true" {
						if name != "" && name != class.Name {
							return errors.New("multiple default storage classes are ambiguous")
						}
						name = class.Name
					}
				}
				continueToken = list.Continue
				if continueToken == "" {
					break
				}
				if _, exists := seenTokens[continueToken]; exists {
					return errors.New("storage class discovery returned a repeated page")
				}
				seenTokens[continueToken] = struct{}{}
				if page == 49 {
					return errors.New("storage class discovery exceeded page limit")
				}
			}
		}
		if !found && name == "" {
			return errors.New("PVC chart requires a discoverable storage class")
		}
		if _, err := kc.cs.StorageV1().StorageClasses().Get(ctx, name, metav1.GetOptions{}); err != nil {
			return errors.New("chart storage class is unavailable")
		}
	}
	return nil
}

func runVerifiedComponentInstall(ctx context.Context, kc *kubeClients, ch *chart.Chart, values map[string]any, in componentInstallRequest) (string, error) {
	if kc == nil || kc.rest == nil {
		return "failed", errors.New("cluster client unavailable")
	}
	if err := prepareComponentCRDInstall(ctx, kc, ch, values, in); err != nil {
		return "unknown", err
	}
	cfg := new(action.Configuration)
	if err := cfg.Init(kubeRESTClientGetter{kc: kc, namespace: in.Namespace}, in.Namespace, "secret", func(string, ...interface{}) {}); err != nil {
		return "failed", errors.New("Helm initialization failed")
	}
	return runVerifiedComponentInstallWithConfig(ctx, cfg, ch, values, in)
}

func prepareComponentCRDInstall(ctx context.Context, kc *kubeClients, ch *chart.Chart, values map[string]any, in componentInstallRequest) error {
	if len(ch.CRDObjects()) > 0 {
		if err := componentEstablishCRDs(ctx, kc, ch); err != nil {
			return errors.New("reviewed CRD phase failed; inspect CRDs before retrying")
		}
		files := ch.Files[:0]
		for _, file := range ch.Files {
			if !strings.HasPrefix(file.Name, "crds/") {
				files = append(files, file)
			}
		}
		ch.Files = files
		kc.mapper.Reset()
		// Newly served custom resources now receive the mandatory API-server
		// validation, including admission/CEL checks, before Helm writes them.
		if err := prepareVerifiedComponentChart(ctx, kc, ch, values, in); err != nil {
			return errors.New("post-CRD server-side validation failed; no Helm workloads installed")
		}
	}
	return nil
}

func componentEstablishCRDs(ctx context.Context, kc *kubeClients, ch *chart.Chart) error {
	resource := kc.dyn.Resource(componentCRDGVR)
	for _, crd := range ch.CRDObjects() {
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal(crd.File.Data, &obj.Object); err != nil {
			return err
		}
		existing, err := resource.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err == nil {
			if !componentReusableCRD(existing, obj) {
				return errors.New("existing CRD differs from approved schema")
			}
			continue
		}
		if !apierrors.IsNotFound(err) {
			return errors.New("CRD read failed")
		}
		if _, err := resource.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
			return errors.New("CRD creation failed")
		}
		if err := wait.PollUntilContextTimeout(ctx, time.Second, 75*time.Second, true, func(ctx context.Context) (bool, error) {
			current, err := resource.Get(ctx, obj.GetName(), metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return componentReusableCRD(current, obj), nil
		}); err != nil {
			return errors.New("CRD did not establish with the approved schema")
		}
	}
	return nil
}

func runVerifiedComponentInstallWithConfig(ctx context.Context, cfg *action.Configuration, ch *chart.Chart, values map[string]any, in componentInstallRequest) (string, error) {
	if cfg == nil || ch == nil || ch.Metadata == nil {
		return "failed", errors.New("chart is outside the supported install contract")
	}
	if in.Component == "" {
		in.Component = ch.Metadata.Name
	}
	profile, ok := componentProfileFor(in.Component)
	if !ok || !profile.Installable || ch.Metadata.Name != profile.Chart || ch.Metadata.Version != in.Version ||
		len(profile.Versions) != 1 || profile.Versions[0] != in.Version {
		return "failed", errors.New("chart is outside the supported install contract")
	}
	if len(ch.CRDObjects()) != 0 {
		return "failed", errors.New("CRDs require the reviewed staged installation path")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	install := action.NewInstall(cfg)
	install.ReleaseName = in.ReleaseName
	install.Namespace = in.Namespace
	install.Version = in.Version
	install.CreateNamespace = false
	install.DependencyUpdate = false
	install.SkipCRDs = true
	install.DisableHooks = profile.HookStrategy != "reviewed"
	install.Wait = true
	install.WaitForJobs = true
	install.Timeout = 9 * time.Minute
	release, err := install.RunWithContext(ctx, ch, values)
	if err != nil {
		status := "unknown"
		if current, getErr := action.NewGet(cfg).Run(in.ReleaseName); getErr == nil &&
			current != nil && current.Info != nil {
			status = current.Info.Status.String()
		}
		return status, errors.New("Helm installation failed")
	}
	if release == nil || release.Info == nil {
		return "unknown", errors.New("Helm did not report a release outcome")
	}
	if release.Info.Status.String() != "deployed" {
		return release.Info.Status.String(), errors.New("Helm did not report a deployed release")
	}
	return release.Info.Status.String(), nil
}

func metav1GetOptions() metav1.GetOptions { return metav1.GetOptions{} }

func mustSemver(value string) *semver.Version {
	v, _ := semver.NewVersion(value)
	return v
}
