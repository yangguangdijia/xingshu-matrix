package cluster

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// kubeClients bundles the clients for one cluster.
type kubeClients struct {
	rest    *rest.Config
	cs      kubernetes.Interface
	dyn     dynamic.Interface
	metrics metricsv.Interface // nil when it cannot be built
	mapper  *restmapper.DeferredDiscoveryRESTMapper
}

type cacheEntry struct {
	key string // cluster updated_at, so edits invalidate
	kc  *kubeClients
}

// clientCache caches clientsets per cluster ID.
type clientCache struct {
	mu sync.Mutex
	m  map[int64]cacheEntry
}

func newClientCache() *clientCache { return &clientCache{m: map[int64]cacheEntry{}} }

func (c *clientCache) get(cl *Cluster, kubeconfig func() (string, error)) (*kubeClients, error) {
	key := cl.UpdatedAt.UTC().Format(time.RFC3339Nano)
	c.mu.Lock()
	if e, ok := c.m[cl.ID]; ok && e.key == key {
		c.mu.Unlock()
		return e.kc, nil
	}
	c.mu.Unlock()
	raw, err := kubeconfig()
	if err != nil {
		return nil, err
	}
	kc, err := buildClients(raw, "")
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.m[cl.ID] = cacheEntry{key: key, kc: kc}
	c.mu.Unlock()
	return kc, nil
}

func (c *clientCache) drop(id int64) {
	c.mu.Lock()
	delete(c.m, id)
	c.mu.Unlock()
}

// normalizeKubeconfig validates raw, optionally switches current-context, and
// returns the (possibly rewritten) kubeconfig plus its API server URL.
func normalizeKubeconfig(raw, kctx string) (string, string, error) {
	cfg, err := clientcmd.Load([]byte(raw))
	if err != nil {
		return "", "", err
	}
	if kctx != "" {
		if _, ok := cfg.Contexts[kctx]; !ok {
			return "", "", errors.New("context " + kctx + " not found")
		}
		cfg.CurrentContext = kctx
	}
	cc, err := clientcmd.NewDefaultClientConfig(*cfg, nil).ClientConfig()
	if err != nil {
		return "", "", err
	}
	out, err := clientcmd.Write(*cfg)
	if err != nil {
		return "", "", err
	}
	return string(out), cc.Host, nil
}

func restFromKubeconfig(raw, kctx string) (*rest.Config, error) {
	cfg, err := clientcmd.Load([]byte(raw))
	if err != nil {
		return nil, err
	}
	over := &clientcmd.ConfigOverrides{CurrentContext: kctx}
	rc, err := clientcmd.NewDefaultClientConfig(*cfg, over).ClientConfig()
	if err != nil {
		return nil, err
	}
	rc.Timeout = 30 * time.Second
	rc.QPS, rc.Burst = 50, 100
	rc.UserAgent = "xingshu-matrix"
	return rc, nil
}

func buildClients(raw, kctx string) (*kubeClients, error) {
	rc, err := restFromKubeconfig(raw, kctx)
	if err != nil {
		return nil, err
	}
	return clientsFromRest(rc)
}

func clientsFromRest(rc *rest.Config) (*kubeClients, error) {
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	kc := &kubeClients{rest: rc, cs: cs, dyn: dyn,
		mapper: restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(cs.Discovery()))}
	if mc, err := metricsv.NewForConfig(rc); err == nil {
		kc.metrics = mc
	}
	return kc, nil
}

// streamConfig is rc without the request timeout (for log follow).
func streamConfig(rc *rest.Config) *rest.Config {
	c := rest.CopyConfig(rc)
	c.Timeout = 0
	return c
}

func serverVersion(ctx context.Context, cs kubernetes.Interface) (string, error) {
	type res struct {
		v   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		v, err := cs.Discovery().ServerVersion()
		if err != nil {
			ch <- res{err: err}
			return
		}
		ch <- res{v: v.GitVersion}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(15 * time.Second):
		return "", errors.New("timeout contacting API server")
	}
}

// kubeErr maps Kubernetes API errors to platform errors.
func kubeErr(err error) error {
	var he *httpx.Error
	if errors.As(err, &he) {
		return err
	}
	switch {
	case apierrors.IsNotFound(err):
		return httpx.NotFound(err.Error())
	case apierrors.IsAlreadyExists(err), apierrors.IsConflict(err):
		return httpx.Conflict(err.Error())
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return httpx.NewError(http.StatusForbidden, httpx.CodeForbidden, "kubernetes: "+err.Error())
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return httpx.BadRequest(err.Error())
	case apierrors.IsTooManyRequests(err):
		return httpx.NewError(http.StatusTooManyRequests, httpx.CodeTooManyRequests, err.Error())
	}
	return httpx.NewError(http.StatusBadGateway, httpx.CodeUpstream, "kubernetes: "+err.Error())
}
