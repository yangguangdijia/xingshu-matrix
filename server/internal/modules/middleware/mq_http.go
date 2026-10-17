package middleware

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

func httpClient(inst *Instance) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsConfig(inst)
	return &http.Client{Timeout: 20 * time.Second, Transport: tr}
}

func tlsConfig(inst *Instance) *tls.Config {
	return &tls.Config{InsecureSkipVerify: inst.Auth.TLSSkipVerify} //nolint:gosec // per-instance opt-in
}

// escVhost escapes a path segment, including "/" (default vhost "/" → %2F).
func escVhost(s string) string { return url.PathEscape(s) }

// ---------------------------------------------------------------- RabbitMQ (Management HTTP API)

// rabbitDo calls the RabbitMQ management API at endpoints[0] with basic auth.
func rabbitDo(ctx context.Context, inst *Instance, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, inst.Endpoints[0]+path, rd)
	if err != nil {
		return err
	}
	req.SetBasicAuth(inst.Auth.Username, inst.Auth.Password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient(inst).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		var e struct{ Error, Reason string }
		if json.Unmarshal(data, &e) == nil && e.Reason != "" {
			msg = e.Error + ": " + e.Reason
		}
		if resp.StatusCode == http.StatusNotFound {
			return httpx.NotFound("rabbitmq: " + msg)
		}
		return fmt.Errorf("rabbitmq %s %s: HTTP %d %s", method, path, resp.StatusCode, msg)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// rabbitGet proxies a read endpoint; ?vhost= narrows queues/exchanges, other
// query params (page, page_size, name, use_regex, columns, sort) are passed through.
func (m *Module) rabbitGet(path string) gin.HandlerFunc {
	return func(c *gin.Context) {
		inst, ok := m.instanceFor(c, TypeRabbitMQ)
		if !ok {
			return
		}
		p := path
		q := c.Request.URL.Query()
		if vh := q.Get("vhost"); vh != "" && (path == "/api/queues" || path == "/api/exchanges") {
			p += "/" + escVhost(vh)
			q.Del("vhost")
		}
		if path == "/api/queues" && q.Get("page") == "" {
			// management API pagination keeps large brokers responsive
			q.Set("page", "1")
			q.Set("page_size", "100")
		}
		if len(q) > 0 {
			p += "?" + q.Encode()
		}
		var out json.RawMessage
		if err := rabbitDo(c.Request.Context(), inst, http.MethodGet, p, nil, &out); err != nil {
			upstream(c, err)
			return
		}
		httpx.OK(c, out)
	}
}

type createQueueInput struct {
	VHost      string         `json:"vhost"`
	Name       string         `json:"name" binding:"required,max=255"`
	Durable    *bool          `json:"durable"`
	AutoDelete bool           `json:"auto_delete"`
	Arguments  map[string]any `json:"arguments"`
}

func (m *Module) rabbitCreateQueue(c *gin.Context) {
	inst, ok := m.instanceFor(c, TypeRabbitMQ)
	if !ok {
		return
	}
	var in createQueueInput
	if !httpx.Bind(c, &in) {
		return
	}
	if in.VHost == "" {
		in.VHost = "/"
	}
	durable := in.Durable == nil || *in.Durable
	m.runChange(c, inst, change{Op: "rabbitmq.create_queue", InstanceID: inst.ID, VHost: in.VHost, Queue: in.Name,
		Durable: durable, AutoDelete: in.AutoDelete, Arguments: in.Arguments}, "创建 RabbitMQ 队列 "+in.Name)
}

func (m *Module) rabbitDeleteQueue(c *gin.Context) {
	inst, ok := m.instanceFor(c, TypeRabbitMQ)
	if !ok {
		return
	}
	vh, _ := url.PathUnescape(c.Param("vhost"))
	name, _ := url.PathUnescape(c.Param("name"))
	m.runChange(c, inst, change{Op: "rabbitmq.delete_queue", InstanceID: inst.ID, VHost: vh, Queue: name}, "删除 RabbitMQ 队列 "+name)
}

func rabbitOverview(ctx context.Context, inst *Instance) (any, error) {
	var ov struct {
		ClusterName     string          `json:"cluster_name"`
		RabbitMQVersion string          `json:"rabbitmq_version"`
		ErlangVersion   string          `json:"erlang_version"`
		MessageStats    json.RawMessage `json:"message_stats"`
		QueueTotals     json.RawMessage `json:"queue_totals"`
		ObjectTotals    json.RawMessage `json:"object_totals"`
	}
	if err := rabbitDo(ctx, inst, http.MethodGet, "/api/overview", nil, &ov); err != nil {
		return nil, err
	}
	var nodes []struct {
		Name      string `json:"name"`
		Running   bool   `json:"running"`
		MemUsed   int64  `json:"mem_used"`
		MemLimit  int64  `json:"mem_limit"`
		MemAlarm  bool   `json:"mem_alarm"`
		DiskFree  int64  `json:"disk_free"`
		DiskAlarm bool   `json:"disk_free_alarm"`
		Uptime    int64  `json:"uptime"`
	}
	_ = rabbitDo(ctx, inst, http.MethodGet, "/api/nodes", nil, &nodes)
	return gin.H{"type": TypeRabbitMQ, "instance": inst.Name, "cluster_name": ov.ClusterName, "version": ov.RabbitMQVersion,
		"erlang_version": ov.ErlangVersion, "message_stats": ov.MessageStats, "queue_totals": ov.QueueTotals,
		"object_totals": ov.ObjectTotals, "nodes": nodes}, nil
}

// ---------------------------------------------------------------- RocketMQ (rocketmq-dashboard REST)

// rocketGetData calls a rocketmq-dashboard endpoint and returns its "data"
// field. Dashboard responses are {"status":0,"data":...,"errMsg":null}. When
// the instance has a username the dashboard login (/login/login.do) is
// performed first and its session cookie reused.
func rocketGetData(ctx context.Context, inst *Instance, path string) (json.RawMessage, error) {
	cl := httpClient(inst)
	base := inst.Endpoints[0]
	if inst.Auth.Username != "" {
		jar, _ := cookiejar.New(nil)
		cl.Jar = jar
		form := url.Values{"username": {inst.Auth.Username}, "password": {inst.Auth.Password}}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/login/login.do", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := cl.Do(req)
		if err != nil {
			return nil, fmt.Errorf("rocketmq-dashboard login: %w", err)
		}
		var lr struct {
			Status int    `json:"status"`
			ErrMsg string `json:"errMsg"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&lr)
		resp.Body.Close()
		if resp.StatusCode >= 300 || lr.Status != 0 {
			return nil, fmt.Errorf("rocketmq-dashboard login failed: HTTP %d %s", resp.StatusCode, lr.ErrMsg)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("rocketmq-dashboard %s: HTTP %d %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var env struct {
		Status int             `json:"status"`
		Data   json.RawMessage `json:"data"`
		ErrMsg string          `json:"errMsg"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("rocketmq-dashboard %s: unexpected response (login required?): %w", path, err)
	}
	if env.Status != 0 {
		return nil, fmt.Errorf("rocketmq-dashboard %s: %s", path, env.ErrMsg)
	}
	return env.Data, nil
}

func (m *Module) rocketGet(path string) gin.HandlerFunc {
	return func(c *gin.Context) {
		inst, ok := m.instanceFor(c, TypeRocketMQ)
		if !ok {
			return
		}
		data, err := rocketGetData(c.Request.Context(), inst, path)
		if err != nil {
			upstream(c, err)
			return
		}
		httpx.OK(c, data)
	}
}
