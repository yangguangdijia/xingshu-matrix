// Command matrix-cluster-agent runs inside a member cluster and opens a
// reverse tunnel to the Matrix platform, so the platform can reach this
// cluster's kube-apiserver without inbound connectivity or a stored kubeconfig.
//
// Environment:
//
//	MATRIX_SERVER               platform base URL, e.g. https://matrix.example.com (https required)
//	MATRIX_CLUSTER_TOKEN_FILE   path to the cluster token (preferred; re-read on every connect)
//	MATRIX_CLUSTER_TOKEN        cluster token (when no file is given)
//	MATRIX_CA_FILE              optional extra CA bundle for the platform
//	MATRIX_TLS_SKIP_VERIFY      "true" disables platform certificate checks (not recommended)
//	MATRIX_ALLOW_INSECURE_HTTP  "true" permits an http:// MATRIX_SERVER (development only)
//
// The agent uses its ServiceAccount (in-cluster config) for the kube-apiserver
// and needs "get namespaces" to read the kube-system UID as its fingerprint.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/tunnel"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/version"
)

type config struct {
	ConnectURL    string // ws(s)://.../api/v1/cluster-agent/connect
	TokenFile     string
	Token         string
	CAFile        string
	SkipVerify    bool
	AllowInsecure bool
	HealthAddr    string
}

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	healthAddr := flag.String("health-addr", ":8081", "listen address for /healthz and /readyz")
	flag.Parse()
	if *showVersion {
		v := version.Get()
		fmt.Printf("matrix-cluster-agent %s (commit %s, built %s, %s, %s)\n", v.Version, v.Commit, v.BuildTime, v.GoVersion, v.Platform)
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	cfg.HealthAddr = *healthAddr
	if cfg.AllowInsecure && strings.HasPrefix(cfg.ConnectURL, "ws://") {
		log.Warn("MATRIX_ALLOW_INSECURE_HTTP=true: the cluster token and tunnel traffic are sent unencrypted")
	}
	if cfg.SkipVerify {
		log.Warn("MATRIX_TLS_SKIP_VERIFY=true: the platform certificate is not verified")
	}
	tlsCfg, err := platformTLS(cfg)
	if err != nil {
		log.Error("invalid TLS configuration", "error", err)
		os.Exit(2)
	}

	kcfg, err := rest.InClusterConfig()
	if err != nil {
		log.Error("in-cluster config unavailable", "error", err)
		os.Exit(1)
	}
	proxy, err := newUpstreamHandler(kcfg, log)
	if err != nil {
		log.Error("cannot build kube-apiserver proxy", "error", err)
		os.Exit(1)
	}
	fpCfg := rest.CopyConfig(kcfg)
	fpCfg.Timeout = 15 * time.Second
	cs, err := kubernetes.NewForConfig(fpCfg)
	if err != nil {
		log.Error("cannot build kubernetes client", "error", err)
		os.Exit(1)
	}

	a := newAgent(cfg, tlsCfg, proxy, log)
	a.fingerprint = func(ctx context.Context) (string, error) {
		ns, err := cs.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		return string(ns.UID), nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	health := &http.Server{Addr: cfg.HealthAddr, Handler: a.healthHandler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health server failed", "error", err)
			stop()
		}
	}()

	log.Info("matrix-cluster-agent starting", "version", version.Version, "server", redactURL(cfg.ConnectURL))
	a.run(ctx)

	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = health.Shutdown(sctx)
	log.Info("matrix-cluster-agent stopped")
}

// loadConfig reads the agent configuration from env.
func loadConfig(getenv func(string) string) (config, error) {
	var c config
	var err error
	if c.AllowInsecure, err = envBool(getenv, "MATRIX_ALLOW_INSECURE_HTTP"); err != nil {
		return c, err
	}
	if c.SkipVerify, err = envBool(getenv, "MATRIX_TLS_SKIP_VERIFY"); err != nil {
		return c, err
	}
	raw := strings.TrimSpace(getenv("MATRIX_SERVER"))
	if raw == "" {
		return c, errors.New("MATRIX_SERVER is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return c, errors.New("MATRIX_SERVER must be an absolute URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return c, errors.New("MATRIX_SERVER must not contain credentials, a query or a fragment")
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		if !c.AllowInsecure {
			return c, errors.New("MATRIX_SERVER must use https:// (set MATRIX_ALLOW_INSECURE_HTTP=true for development)")
		}
		u.Scheme = "ws"
	default:
		return c, fmt.Errorf("MATRIX_SERVER scheme %q is not supported", u.Scheme)
	}
	p := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(p, "/api/v1") {
		p += "/api/v1"
	}
	u.Path = p + tunnel.ConnectPath
	c.ConnectURL = u.String()

	c.TokenFile = strings.TrimSpace(getenv("MATRIX_CLUSTER_TOKEN_FILE"))
	c.Token = strings.TrimSpace(getenv("MATRIX_CLUSTER_TOKEN"))
	if c.TokenFile == "" && c.Token == "" {
		return c, errors.New("MATRIX_CLUSTER_TOKEN_FILE or MATRIX_CLUSTER_TOKEN is required")
	}
	if c.TokenFile != "" {
		if _, err := readToken(c); err != nil {
			return c, err
		}
	}
	c.CAFile = strings.TrimSpace(getenv("MATRIX_CA_FILE"))
	c.HealthAddr = ":8081"
	return c, nil
}

func envBool(getenv func(string) string, key string) (bool, error) {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", key)
	}
	return b, nil
}

// readToken returns the cluster token, preferring the file so rotated secrets
// are picked up on the next connect.
func readToken(c config) (string, error) {
	if c.TokenFile != "" {
		b, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return "", fmt.Errorf("read MATRIX_CLUSTER_TOKEN_FILE: %w", err)
		}
		t := strings.TrimSpace(string(b))
		if t == "" {
			return "", errors.New("MATRIX_CLUSTER_TOKEN_FILE is empty")
		}
		return t, nil
	}
	if c.Token == "" {
		return "", errors.New("cluster token is empty")
	}
	return c.Token, nil
}

func platformTLS(c config) (*tls.Config, error) {
	t := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.SkipVerify} //nolint:gosec // opt-in, warned
	if c.CAFile != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read MATRIX_CA_FILE: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("MATRIX_CA_FILE contains no PEM certificates")
		}
		t.RootCAs = pool
	}
	return t, nil
}

func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	return u.String()
}
