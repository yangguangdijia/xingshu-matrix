package cluster

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const helmSecretType = "helm.sh/release.v1"
const resourceListCap = 5000
const resourceReadTimeout = 15 * time.Second

// These helpers belong to the Helm/CRD extension, not the shared resource APIs.
// Upstream error bodies may include submitted objects or Secret data.
func resourceUnavailable() error {
	return httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "cluster resource client is unavailable")
}

func resourceAPIError(err error) error {
	var existing *httpx.Error
	if errors.As(err, &existing) {
		return err
	}
	state := resourceReadState(err)
	detail := gin.H{"state": state, "complete": false}
	switch {
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return httpx.Forbidden("Kubernetes resource access is forbidden").WithDetails(detail)
	case apierrors.IsNotFound(err):
		return httpx.NotFound("Kubernetes resource or API is not installed or not found").WithDetails(detail)
	case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err):
		return httpx.Conflict("Kubernetes resource changed or already exists; reload before retrying")
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return httpx.BadRequest("Kubernetes rejected the resource configuration")
	case apierrors.IsTooManyRequests(err):
		return httpx.NewError(http.StatusTooManyRequests, httpx.CodeTooManyRequests, "Kubernetes resource requests are rate limited")
	case errors.Is(err, context.DeadlineExceeded), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return httpx.NewError(http.StatusGatewayTimeout, httpx.CodeUpstream, "Kubernetes resource request timed out")
	default:
		return httpx.NewError(http.StatusBadGateway, httpx.CodeUpstream, "Kubernetes resource request failed").WithDetails(detail)
	}
}

func resourceReadState(err error) string {
	switch {
	case err == nil:
		return "available"
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return "forbidden"
	case apierrors.IsNotFound(err):
		return "absent"
	default:
		return "failed"
	}
}

// RegisterResourceRoutes takes the authenticated /clusters router group.
func (m *Module) RegisterResourceRoutes(g *gin.RouterGroup) {
	m.registerHelmReleaseRoutes(g)
	m.registerCRDRoutes(g)
}

func (m *Module) resourceKube(c *gin.Context) (*Cluster, *kubeClients, bool) {
	p := rbac.PrincipalFrom(c)
	if p == nil {
		httpx.Fail(c, httpx.Unauthorized("authentication required"))
		return nil, nil, false
	}
	if m == nil || m.d == nil || m.d.DB == nil || m.d.Crypto == nil || m.clients == nil {
		httpx.Fail(c, resourceUnavailable())
		return nil, nil, false
	}
	cl, ok := m.clusterFor(c)
	if !ok {
		return nil, nil, false
	}
	if err := resourceClusterAccess(p, cl); err != nil {
		httpx.Fail(c, err)
		return nil, nil, false
	}
	kc, err := m.kube(c.Request.Context(), cl)
	if err != nil || kc == nil {
		httpx.Fail(c, resourceUnavailable())
		return nil, nil, false
	}
	return cl, kc, true
}

func resourceClusterAccess(p *rbac.Principal, cl *Cluster) error {
	if p == nil {
		return httpx.Unauthorized("authentication required")
	}
	if cl == nil {
		return resourceUnavailable()
	}
	if strings.TrimSpace(cl.Env) == "" || !p.CanEnv(cl.Env) {
		return httpx.Forbidden("cluster environment access is required")
	}
	return nil
}

func (m *Module) registerHelmReleaseRoutes(g *gin.RouterGroup) {
	read := rbac.RequireAnyPerm(PermWorkloadRead, PermConfigRead)
	g.GET("/:id/helm/releases", read, m.listHelmReleases)
	g.GET("/:id/helm/releases/:namespace/:name", read, m.helmReleaseHistory)
	g.GET("/:id/helm/releases/:namespace/:name/history", read, m.helmReleaseHistory)
}

// Only this projection may leave the decoder. In particular Helm's config,
// manifest, chart templates and hooks must never reach responses or errors.
type HelmReleaseRevision struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	Revision     int    `json:"revision"`
	Chart        string `json:"chart"`
	ChartVersion string `json:"chart_version"`
	AppVersion   string `json:"app_version"`
	Status       string `json:"status"`
	Updated      string `json:"updated"`
	Source       string `json:"source"`
	ParseError   string `json:"parse_error"`
}

type HelmReleaseRow struct {
	HelmReleaseRevision
	HistoryCount     int                   `json:"history_count"`
	HistoryComplete  bool                  `json:"history_complete"`
	CorruptRevisions []HelmReleaseRevision `json:"corrupt_revisions"`
}

func decodeHelmRelease(data []byte) (HelmReleaseRevision, error) {
	var out HelmReleaseRevision
	if len(data) == 0 || len(data) > 4<<20 {
		return out, fmt.Errorf("release payload missing or too large")
	}
	// The Kubernetes JSON decoder has already decoded the outer Secret base64.
	decoded, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return out, fmt.Errorf("invalid Helm base64")
	}
	if len(decoded) >= 2 && decoded[0] == 0x1f && decoded[1] == 0x8b {
		z, err := gzip.NewReader(bytes.NewReader(decoded))
		if err != nil {
			return out, fmt.Errorf("invalid Helm gzip")
		}
		decoded, err = io.ReadAll(io.LimitReader(z, (16<<20)+1))
		_ = z.Close()
		if err != nil || len(decoded) > 16<<20 {
			return out, fmt.Errorf("invalid or oversized Helm gzip")
		}
	}
	var release struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Version   int    `json:"version"`
		Info      struct {
			Status       string `json:"status"`
			LastDeployed string `json:"last_deployed"`
		} `json:"info"`
		Chart struct {
			Metadata struct {
				Name       string `json:"name"`
				Version    string `json:"version"`
				AppVersion string `json:"appVersion"`
			} `json:"metadata"`
		} `json:"chart"`
	}
	if json.Unmarshal(decoded, &release) != nil || release.Name == "" || release.Namespace == "" || release.Version < 1 {
		return out, fmt.Errorf("invalid Helm release metadata")
	}
	out = HelmReleaseRevision{Name: release.Name, Namespace: release.Namespace, Revision: release.Version, Chart: release.Chart.Metadata.Name, ChartVersion: release.Chart.Metadata.Version, AppVersion: release.Chart.Metadata.AppVersion, Status: release.Info.Status, Updated: release.Info.LastDeployed, Source: helmSecretType}
	return out, nil
}

func helmRevision(s corev1.Secret) HelmReleaseRevision {
	name, rev := s.Labels["name"], 0
	if strings.HasPrefix(s.Name, "sh.helm.release.v1.") {
		key := strings.TrimPrefix(s.Name, "sh.helm.release.v1.")
		if i := strings.LastIndex(key, ".v"); i > 0 {
			name = key[:i]
			rev, _ = strconv.Atoi(key[i+2:])
		}
	}
	if rev == 0 {
		rev, _ = strconv.Atoi(s.Labels["version"])
	}
	if name == "" {
		name = s.Name
	}
	fallback := HelmReleaseRevision{Name: name, Namespace: s.Namespace, Revision: rev, Status: s.Labels["status"], Updated: s.CreationTimestamp.UTC().Format(time.RFC3339), Source: helmSecretType}
	out, err := decodeHelmRelease(s.Data["release"])
	if err != nil {
		fallback.ParseError = err.Error()
		return fallback
	}
	if out.Name != name || out.Namespace != s.Namespace || (rev > 0 && out.Revision != rev) {
		fallback.ParseError = "Helm release identity does not match Secret metadata"
		return fallback
	}
	return out
}

func readHelmRevisions(ctx context.Context, kc *kubeClients, namespace string) ([]HelmReleaseRevision, bool, error) {
	return readHelmRevisionsFor(ctx, kc, namespace, "")
}

func readHelmRevisionsFor(ctx context.Context, kc *kubeClients, namespace, release string) ([]HelmReleaseRevision, bool, error) {
	if kc == nil || kc.cs == nil {
		return nil, false, resourceUnavailable()
	}
	ctx, cancel := context.WithTimeout(ctx, resourceReadTimeout)
	defer cancel()
	out := []HelmReleaseRevision{}
	// Helm labels are not authoritative: history must include revisions with
	// missing/stale labels too. The Secret type and decoded identity are checked.
	opts := metav1.ListOptions{Limit: 500, FieldSelector: "type=" + helmSecretType}
	seen := map[string]bool{}
	scanned := 0
	truncated := false
	for {
		page, err := kc.cs.CoreV1().Secrets(namespace).List(ctx, opts)
		if err != nil {
			return nil, false, resourceAPIError(err)
		}
		if page == nil {
			return nil, false, resourceAPIError(errors.New("nil Secret list"))
		}
		for _, s := range page.Items {
			if scanned == resourceListCap {
				truncated = true
				break
			}
			scanned++
			if string(s.Type) != helmSecretType {
				continue
			}
			r := helmRevision(s)
			if namespace != "" && r.Namespace != namespace {
				continue
			}
			if release == "" || r.Name == release {
				out = append(out, r)
			}
		}
		if truncated {
			break
		}
		if page.Continue == "" {
			break
		}
		if scanned >= resourceListCap || len(seen) >= resourceListCap {
			truncated = true
			break
		}
		if seen[page.Continue] {
			return nil, false, httpx.NewError(502, httpx.CodeUpstream, "Kubernetes pagination did not advance")
		}
		seen[page.Continue] = true
		opts.Continue = page.Continue
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Revision > b.Revision
	})
	return out, truncated, nil
}

func aggregateHelmReleases(revisions []HelmReleaseRevision, truncated bool) []HelmReleaseRow {
	out := []HelmReleaseRow{}
	index := map[string]int{}
	for _, r := range revisions {
		key := r.Namespace + "/" + r.Name
		i, ok := index[key]
		if !ok {
			i = len(out)
			index[key] = i
			out = append(out, HelmReleaseRow{HelmReleaseRevision: r, HistoryComplete: !truncated, CorruptRevisions: []HelmReleaseRevision{}})
		}
		out[i].HistoryCount++
		if r.Revision > out[i].Revision {
			out[i].HelmReleaseRevision = r
		}
		if r.ParseError != "" {
			out[i].CorruptRevisions = append(out[i].CorruptRevisions, r)
		}
	}
	return out
}

func (m *Module) listHelmReleases(c *gin.Context) {
	if ns := c.Query("namespace"); ns != "" && len(validation.IsDNS1123Label(ns)) != 0 {
		httpx.Fail(c, httpx.BadRequest("invalid namespace"))
		return
	}
	cl, kc, ok := m.resourceKube(c)
	if !ok {
		return
	}
	rows, truncated, err := readHelmRevisions(c.Request.Context(), kc, c.Query("namespace"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"cluster_id": cl.ID, "items": aggregateHelmReleases(rows, truncated), "state": resourceListState(truncated), "truncated": truncated, "complete": !truncated, "limit": resourceListCap, "source": helmSecretType})
}

func (m *Module) helmReleaseHistory(c *gin.Context) {
	if len(validation.IsDNS1123Label(c.Param("namespace"))) != 0 || len(validation.IsDNS1123Subdomain(c.Param("name"))) != 0 {
		httpx.Fail(c, httpx.BadRequest("a valid namespace and release name are required"))
		return
	}
	cl, kc, ok := m.resourceKube(c)
	if !ok {
		return
	}
	rows, truncated, err := readHelmRevisionsFor(c.Request.Context(), kc, c.Param("namespace"), c.Param("name"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	items := []HelmReleaseRevision{}
	for _, r := range rows {
		if r.Name == c.Param("name") {
			items = append(items, r)
		}
	}
	if len(items) == 0 && !truncated {
		httpx.Fail(c, httpx.NotFound("Helm release not found"))
		return
	}
	httpx.OK(c, gin.H{"cluster_id": cl.ID, "items": items, "history_count": len(items), "state": resourceListState(truncated), "truncated": truncated, "complete": !truncated, "limit": resourceListCap, "source": helmSecretType})
}

func resourceListState(truncated bool) string {
	if truncated {
		return "truncated"
	}
	return "available"
}

// All inventory readers enforce both item and page bounds, including when an
// upstream ignores ListOptions.Limit or returns empty pages with continue tokens.
func readResourcePages[T any](ctx context.Context, opts metav1.ListOptions, fetch func(context.Context, metav1.ListOptions) ([]T, string, error)) ([]T, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, resourceReadTimeout)
	defer cancel()
	opts.Limit = 500
	items := []T{}
	seen := map[string]bool{}
	for page := 0; page < resourceListCap; page++ {
		rows, next, err := fetch(ctx, opts)
		if err != nil {
			return nil, false, err
		}
		remaining := resourceListCap - len(items)
		if len(rows) > remaining {
			return append(items, rows[:remaining]...), true, nil
		}
		items = append(items, rows...)
		if next == "" {
			return items, false, nil
		}
		if len(items) == resourceListCap {
			return items, true, nil
		}
		if seen[next] {
			return nil, false, errors.New("Kubernetes pagination did not advance")
		}
		seen[next] = true
		opts.Continue = next
	}
	return items, true, nil
}
