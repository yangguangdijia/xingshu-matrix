package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"k8s.io/client-go/rest"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/tunnel/wire"
)

// sessionHandler admits only requests carrying the session secret, strips it,
// and hands them to next (the kube-apiserver reverse proxy).
func sessionHandler(secret string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !wire.SecretEqual(wire.BearerToken(r.Header), secret) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"invalid tunnel session credentials","reason":"Unauthorized","code":401}` + "\n"))
			return
		}
		r.Header.Del("Authorization")
		// Never forward front-proxy identity headers; the agent's own
		// ServiceAccount (plus RBAC-checked Impersonate-*) is the identity.
		for k := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-remote-") {
				r.Header.Del(k)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// newUpstreamHandler reverse-proxies to the kube-apiserver described by kcfg,
// authenticating with kcfg's credentials (the projected ServiceAccount token
// file is re-read by client-go's cached token source). The transport is
// HTTP/1.1-only so 101 upgrades (exec/attach/port-forward) work.
func newUpstreamHandler(kcfg *rest.Config, log *slog.Logger) (http.Handler, error) {
	if kcfg == nil || kcfg.Host == "" {
		return nil, errors.New("kube-apiserver host is empty")
	}
	host := kcfg.Host
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	target, err := url.Parse(host)
	if err != nil || target.Host == "" {
		return nil, fmt.Errorf("invalid kube-apiserver host")
	}
	target.Path = strings.TrimRight(target.Path, "/")

	tlsCfg, err := rest.TLSConfigFor(kcfg)
	if err != nil {
		return nil, fmt.Errorf("kube-apiserver TLS config: %w", err)
	}
	if tlsCfg == nil && target.Scheme == "https" {
		tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if tlsCfg != nil {
		tlsCfg.NextProtos = []string{"http/1.1"}
	}
	base := &http.Transport{
		Proxy: nil, // in-cluster traffic never goes through an egress proxy
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig:       tlsCfg,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	cfg := rest.CopyConfig(kcfg)
	cfg.UserAgent = "" // keep the platform client's User-Agent
	rt, err := rest.HTTPWrappersForConfig(cfg, base)
	if err != nil {
		return nil, fmt.Errorf("kube-apiserver auth wrappers: %w", err)
	}

	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
		},
		Transport:     rt,
		FlushInterval: -1,
		ErrorLog:      slog.NewLogLogger(log.Handler(), slog.LevelDebug),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Method and path only: queries, headers and bodies stay out of logs.
			log.Warn("kube-apiserver proxy error", "method", r.Method, "path", r.URL.Path, "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"cluster agent could not reach the kube-apiserver","reason":"ServiceUnavailable","code":502}` + "\n"))
		},
	}, nil
}
