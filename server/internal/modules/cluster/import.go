package cluster

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corevalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	maxKubeconfigBytes        = 1 << 20
	maxKubeconfigRequestBytes = 2*maxKubeconfigBytes + (64 << 10)
	maxKubeconfigContexts     = 50
	clusterHealthWorkers      = 4
)

type kubeconfigImportItem struct {
	Context     string `json:"context" binding:"required"`
	Name        string `json:"name" binding:"required,max=128"`
	Env         string `json:"env" binding:"max=64"`
	Description string `json:"description"`
}

func oneImportEnvironment(items []kubeconfigImportItem) (string, error) {
	if len(items) == 0 || len(items) > maxKubeconfigContexts {
		return "", errors.New("items must contain 1..50 contexts")
	}
	env := ""
	for _, item := range items {
		current := strings.TrimSpace(item.Env)
		if current == "" || len(current) > 64 {
			return "", errors.New("a valid environment is required")
		}
		if env == "" {
			env = current
		} else if env != current {
			return "", errors.New("batch import across multiple environments is not supported")
		}
	}
	return env, nil
}

type kubeconfigImportResult struct {
	Context string `json:"context"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	ID      int64  `json:"id,omitempty"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

type preparedKubeconfigImport struct {
	Config  string
	Server  string
	Version string
	Err     error
}

type clusterHealthRow struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Env          string    `json:"env"`
	APIServer    string    `json:"api_server"`
	APIStatus    string    `json:"api_status"`
	HealthStatus string    `json:"health_status"`
	Version      string    `json:"version"`
	NodesTotal   int       `json:"nodes_total"`
	NodesReady   int       `json:"nodes_ready"`
	HealthScore  *int      `json:"health_score"`
	CheckedAt    time.Time `json:"checked_at"`
	Error        string    `json:"error,omitempty"`
}

type kubeconfigContext struct {
	Name      string `json:"name"`
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Server    string `json:"server"`
	Current   bool   `json:"current"`
	Error     string `json:"error,omitempty"`
}

func kubeconfigContexts(raw string) ([]kubeconfigContext, error) {
	if len(raw) > maxKubeconfigBytes {
		return nil, errors.New("kubeconfig is too large")
	}
	cfg, err := clientcmd.Load([]byte(raw))
	if err != nil {
		return nil, err
	}
	if len(cfg.Contexts) == 0 {
		return nil, errors.New("kubeconfig contains no contexts")
	}
	if len(cfg.Contexts) > maxKubeconfigContexts {
		return nil, errors.New("kubeconfig contains too many contexts")
	}
	names := make([]string, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	items := make([]kubeconfigContext, 0, len(names))
	for _, name := range names {
		ctx := cfg.Contexts[name]
		item := kubeconfigContext{Name: name, Current: name == cfg.CurrentContext}
		if ctx == nil {
			item.Error = "context entry is empty"
			items = append(items, item)
			continue
		}
		item.Cluster, item.Namespace = ctx.Cluster, ctx.Namespace
		cl, ok := cfg.Clusters[ctx.Cluster]
		if !ok || cl == nil {
			item.Error = "context refers to a missing cluster"
			items = append(items, item)
			continue
		}
		if ctx.AuthInfo != "" && cfg.AuthInfos[ctx.AuthInfo] == nil {
			item.Error = "context refers to a missing credential entry"
			items = append(items, item)
			continue
		}
		if err := validateKubeconfigFiles(cl, cfg.AuthInfos[ctx.AuthInfo]); err != nil {
			item.Error = err.Error()
			items = append(items, item)
			continue
		}
		server, err := displayAPIServer(cl.Server)
		if err != nil {
			item.Error = err.Error()
			items = append(items, item)
			continue
		}
		item.Server = server
		if _, err := safeAPIServer(cl.Server); err != nil {
			item.Error = err.Error()
		}
		items = append(items, item)
	}
	return items, nil
}

func displayAPIServer(raw string) (string, error) {
	server := strings.TrimSpace(raw)
	if server == "" {
		return "", errors.New("cluster has no API server address")
	}
	parsed, err := url.Parse(server)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return "", errors.New("cluster has an invalid API server address")
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String(), nil
}

func safeAPIServer(raw string) (string, error) {
	display, err := displayAPIServer(raw)
	if err != nil {
		return "", err
	}
	parsed, _ := url.Parse(strings.TrimSpace(raw))
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errors.New("cluster has an invalid API server address")
	}
	return display, nil
}

func validateKubeconfigFiles(cluster *clientcmdapi.Cluster, auth *clientcmdapi.AuthInfo) error {
	if cluster != nil && cluster.CertificateAuthority != "" {
		return errors.New("certificate-authority file paths are not supported; embed the certificate data")
	}
	if auth == nil {
		return nil
	}
	if auth.Exec != nil {
		return errors.New("exec credential plugins are not supported for server-side cluster imports")
	}
	if auth.ClientCertificate != "" || auth.ClientKey != "" || auth.TokenFile != "" {
		return errors.New("credential file paths are not supported; embed credential data")
	}
	return nil
}

func kubeconfigForContext(raw, contextName string) (string, string, error) {
	if len(raw) > maxKubeconfigBytes {
		return "", "", errors.New("kubeconfig is too large")
	}
	cfg, err := clientcmd.Load([]byte(raw))
	if err != nil {
		return "", "", err
	}
	if contextName == "" {
		contextName = cfg.CurrentContext
		if contextName == "" && len(cfg.Contexts) == 1 {
			for name := range cfg.Contexts {
				contextName = name
			}
		}
		if contextName == "" {
			return "", "", errors.New("current context is required")
		}
	}
	kctx, ok := cfg.Contexts[contextName]
	if !ok || kctx == nil {
		return "", "", errors.New("context not found")
	}
	cluster, ok := cfg.Clusters[kctx.Cluster]
	if !ok || cluster == nil {
		return "", "", errors.New("context refers to a missing cluster")
	}
	var auth *clientcmdapi.AuthInfo
	if kctx.AuthInfo != "" {
		auth, ok = cfg.AuthInfos[kctx.AuthInfo]
		if !ok || auth == nil {
			return "", "", errors.New("context refers to a missing credential entry")
		}
	}
	if err := validateKubeconfigFiles(cluster, auth); err != nil {
		return "", "", err
	}
	server, err := safeAPIServer(cluster.Server)
	if err != nil {
		return "", "", err
	}
	if kctx.Namespace != "" {
		if errs := corevalidation.IsDNS1123Label(kctx.Namespace); len(errs) > 0 {
			return "", "", errors.New("context has an invalid namespace")
		}
	}
	// Store only the selected context's cluster and identity so credentials
	// from sibling contexts are not copied into every registry entry.
	selected := &clientcmdapi.Config{
		Kind:           cfg.Kind,
		APIVersion:     cfg.APIVersion,
		Preferences:    cfg.Preferences,
		Clusters:       map[string]*clientcmdapi.Cluster{kctx.Cluster: cluster},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{},
		Contexts:       map[string]*clientcmdapi.Context{contextName: kctx},
		CurrentContext: contextName,
	}
	if auth != nil {
		selected.AuthInfos[kctx.AuthInfo] = auth
	}
	normalized, err := clientcmd.Write(*selected)
	if err != nil {
		return "", "", err
	}
	return string(normalized), server, nil
}

func (m *Module) listKubeconfigContexts(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxKubeconfigRequestBytes)
	var in struct {
		Kubeconfig string `json:"kubeconfig" binding:"required"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	items, err := kubeconfigContexts(in.Kubeconfig)
	if err != nil {
		httpx.Fail(c, httpx.BadRequest("invalid kubeconfig"))
		return
	}
	httpx.OK(c, gin.H{"items": items, "total": len(items)})
}

func (m *Module) importKubeconfigContexts(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxKubeconfigRequestBytes)
	var in struct {
		Kubeconfig string                 `json:"kubeconfig" binding:"required"`
		Items      []kubeconfigImportItem `json:"items" binding:"required"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	if len(in.Items) == 0 || len(in.Items) > maxKubeconfigContexts {
		httpx.Fail(c, httpx.BadRequest("items must contain 1..50 contexts"))
		return
	}
	env, err := oneImportEnvironment(in.Items)
	if err != nil {
		httpx.Fail(c, httpx.BadRequest(err.Error()))
		return
	}
	if m.d == nil || m.d.Crypto == nil {
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "cluster credential encryption is unavailable"))
		return
	}
	principal := rbac.PrincipalFrom(c)
	if principal == nil {
		httpx.Fail(c, httpx.Unauthorized("authentication required"))
		return
	}
	metadata, err := kubeconfigContexts(in.Kubeconfig)
	if err != nil {
		httpx.Fail(c, httpx.BadRequest("invalid kubeconfig"))
		return
	}
	byContext := make(map[string]kubeconfigContext, len(metadata))
	for _, item := range metadata {
		byContext[item.Name] = item
	}
	names, contexts := map[string]bool{}, map[string]bool{}
	items := make([]registryImportEntry, 0, len(in.Items))
	for i := range in.Items {
		item := in.Items[i]
		item.Name = strings.TrimSpace(item.Name)
		item.Context = strings.TrimSpace(item.Context)
		item.Env = strings.TrimSpace(item.Env)
		if errs := corevalidation.IsDNS1123Subdomain(item.Name); len(errs) > 0 || len(item.Name) > 128 {
			httpx.Fail(c, httpx.BadRequest("invalid cluster name"))
			return
		}
		if item.Env != env {
			httpx.Fail(c, httpx.BadRequest("batch import environment changed during validation"))
			return
		}
		if item.Context == "" || contexts[item.Context] {
			httpx.Fail(c, httpx.BadRequest("context names must be non-empty and unique"))
			return
		}
		if names[item.Name] {
			httpx.Fail(c, httpx.BadRequest("cluster names must be unique"))
			return
		}
		contexts[item.Context], names[item.Name] = true, true
		ctxMeta, ok := byContext[item.Context]
		if !ok {
			httpx.Fail(c, httpx.BadRequest("selected context does not exist"))
			return
		}
		if ctxMeta.Error != "" {
			httpx.Fail(c, httpx.BadRequest("selected context is not importable: "+ctxMeta.Error))
			return
		}
		if !principal.CanEnv(item.Env) {
			httpx.Fail(c, httpx.Forbidden("no access to environment "+item.Env))
			return
		}
		raw, server, err := kubeconfigForContext(in.Kubeconfig, item.Context)
		if err != nil {
			httpx.Fail(c, httpx.BadRequest("selected context is not importable"))
			return
		}
		items = append(items, registryImportEntry{Input: clusterInput{
			Name: item.Name, Env: item.Env, Description: item.Description,
			APIServer: server, Kubeconfig: raw, Context: item.Context,
		}})
	}

	ctx := c.Request.Context()
	res, ticket, err := m.runRegistryChange(ctx, principal, "import", nil, "batch-import", env,
		registryMutation{Imports: items, Actor: principal.Username}, c.Query("reason"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "cluster", "batch-import")
	audit.SetAction(c, "cluster.import")
	audit.SetDetail(c, gin.H{"environment": env, "requested": len(items), "contexts": importAuditNames(items)})
	if ticket != nil {
		approval.RespondPending(c, ticket)
		return
	}
	httpx.OK(c, res)
}

func importAuditNames(entries []registryImportEntry) []gin.H {
	items := make([]gin.H, 0, len(entries))
	for _, item := range entries {
		items = append(items, gin.H{"context": item.Input.Context, "name": item.Input.Name})
	}
	return items
}

func (m *Module) clusterHealth(c *gin.Context) {
	page := httpx.ParsePage(c)
	if page.Size > 40 {
		page.Size = 40
	}
	ctx := c.Request.Context()
	var scope []string
	if principal := rbac.PrincipalFrom(c); principal != nil {
		scope = principal.EnvScope
	}
	where, args := clusterListFilter(c.Query("env"), c.Query("keyword"), scope)
	var total int64
	if err := m.d.DB.QueryRow(ctx, `SELECT count(*) FROM cluster_clusters `+where, args...).Scan(&total); err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	args = append(args, page.Limit(), page.Offset())
	rows, err := m.d.DB.Query(ctx, `SELECT `+clusterCols+` FROM cluster_clusters `+where+
		` ORDER BY id LIMIT $`+strconv.Itoa(len(args)-1)+` OFFSET $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	clusters := make([]Cluster, 0, page.Size)
	for rows.Next() {
		cl, scanErr := scanCluster(rows)
		if scanErr != nil {
			rows.Close()
			httpx.Fail(c, scanErr)
			return
		}
		clusters = append(clusters, *cl)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	rows.Close()

	items := make([]clusterHealthRow, len(clusters))
	jobs := make(chan int)
	workers := len(clusters)
	if workers > clusterHealthWorkers {
		workers = clusterHealthWorkers
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				items[index] = m.checkClusterHealth(ctx, &clusters[index])
			}
		}()
	}
	for i := range clusters {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	result := httpx.NewPageResult(items, total, page)
	httpx.OK(c, gin.H{
		"items": result.Items, "total": result.Total, "page": result.Page, "page_size": result.PageSize,
		"health_score_basis": "Ready 节点数 / 节点总数；不代表控制平面或工作负载 SLO。",
	})
}

func (m *Module) checkClusterHealth(ctx context.Context, cl *Cluster) clusterHealthRow {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	row := clusterHealthRow{
		ID: cl.ID, Name: cl.Name, Env: cl.Env, APIServer: cl.APIServer,
		APIStatus: "unreachable", HealthStatus: "unknown", Version: cl.Version,
	}
	kc, err := m.kube(checkCtx, cl)
	if err == nil {
		row.Version, err = serverVersion(checkCtx, kc.cs)
	}
	if err == nil {
		row.APIStatus = "connected"
		nodes, listErr := listClusterHealthNodes(checkCtx, kc.cs)
		if listErr != nil {
			row.Error = "node readiness unavailable: " + listErr.Error()
		} else {
			row.NodesTotal, row.NodesReady = nodes.total, nodes.ready
			if nodes.total > 0 {
				score := nodes.ready * 100 / nodes.total
				row.HealthScore = &score
				if nodes.ready == nodes.total {
					row.HealthStatus = "healthy"
				} else {
					row.HealthStatus = "degraded"
				}
			}
		}
	} else {
		row.Error = err.Error()
	}
	row.CheckedAt = time.Now().UTC()
	status, lastError, version := "connected", "", row.Version
	if row.APIStatus != "connected" {
		status, lastError, version = "error", row.Error, ""
	}
	_, _ = m.d.DB.Exec(ctx, `UPDATE cluster_clusters
		SET status=$2,last_error=$3,version=CASE WHEN $4='' THEN version ELSE $4 END,checked_at=$5
		WHERE id=$1`, cl.ID, status, lastError, version, row.CheckedAt)
	return row
}

type nodeReadiness struct {
	total int
	ready int
}

func listClusterHealthNodes(ctx context.Context, cs kubernetes.Interface) (nodeReadiness, error) {
	result := nodeReadiness{}
	continuation := ""
	for {
		page, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 1000, Continue: continuation})
		if err != nil {
			return nodeReadiness{}, err
		}
		for i := range page.Items {
			result.total++
			for _, condition := range page.Items[i].Status.Conditions {
				if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
					result.ready++
					break
				}
			}
		}
		continuation = page.Continue
		if continuation == "" {
			return result, nil
		}
	}
}
