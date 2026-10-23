package observability

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

const maxBody = 32 << 20 // 32 MiB upstream response cap

// client is an authenticated HTTP client bound to one data source.
type client struct {
	ds   *DataSource
	http *http.Client
}

func newClient(ds *DataSource) *client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if ds.TLSSkipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in per datasource
	}
	to := time.Duration(ds.TimeoutSec) * time.Second
	if to <= 0 {
		to = 30 * time.Second
	}
	return &client{ds: ds, http: &http.Client{Transport: tr, Timeout: to}}
}

func upstreamErr(format string, a ...any) error {
	return httpx.NewError(http.StatusBadGateway, httpx.CodeUpstream, fmt.Sprintf(format, a...))
}

// do performs a request against ds.URL+path and returns the raw body for 2xx.
func (c *client) do(ctx context.Context, method, path string, q url.Values, body any) ([]byte, error) {
	u := c.ds.URL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch c.ds.AuthType {
	case "basic":
		req.SetBasicAuth(c.ds.Username, c.ds.secretPlain)
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+c.ds.secretPlain)
	}
	if org := c.ds.opt("org_id"); org != "" {
		req.Header.Set("X-Scope-OrgID", org) // Loki / Tempo multi-tenancy
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, upstreamErr("%s %s: %v", c.ds.Type, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, upstreamErr("%s read: %v", c.ds.Type, err)
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(b))
		var pe struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &pe) == nil && pe.Error != "" {
			msg = pe.Error
		}
		if len(msg) > 500 {
			msg = msg[:500]
		}
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity {
			return nil, httpx.BadRequest(fmt.Sprintf("%s rejected query: %s", c.ds.Type, msg))
		}
		return nil, upstreamErr("%s %s: HTTP %d: %s", c.ds.Type, path, resp.StatusCode, msg)
	}
	return b, nil
}

func (c *client) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	b, err := c.do(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return upstreamErr("%s %s: invalid JSON: %v", c.ds.Type, path, err)
	}
	return nil
}

// Test checks connectivity with a cheap type-specific endpoint.
func (c *client) Test(ctx context.Context) (any, error) {
	var out any
	var err error
	switch c.ds.Type {
	case "prometheus":
		err = c.getJSON(ctx, "/api/v1/status/buildinfo", nil, &out)
	case "loki":
		err = c.getJSON(ctx, "/loki/api/v1/labels", nil, &out)
	case "elasticsearch":
		err = c.getJSON(ctx, "/", nil, &out)
	case "alertmanager":
		err = c.getJSON(ctx, "/api/v2/status", nil, &out)
	case "jaeger":
		err = c.getJSON(ctx, "/api/services", nil, &out)
	case "tempo":
		var b []byte
		b, err = c.do(ctx, http.MethodGet, "/api/echo", nil, nil)
		out = strings.TrimSpace(string(b))
	}
	return out, err
}

// ------------------------------------------------------------------ Prometheus

const maxQueryLen = 8192

// promEnvelope is Prometheus' standard API response.
type promEnvelope struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType string          `json:"errorType,omitempty"`
	Error     string          `json:"error,omitempty"`
	Warnings  []string        `json:"warnings,omitempty"`
}

func (c *client) prom(ctx context.Context, path string, q url.Values) (*promEnvelope, error) {
	var env promEnvelope
	if err := c.getJSON(ctx, path, q, &env); err != nil {
		return nil, err
	}
	if env.Status != "success" {
		return nil, httpx.BadRequest("prometheus: " + env.ErrorType + ": " + env.Error)
	}
	return &env, nil
}

// parseTime accepts RFC3339, unix seconds (float) or relative "now-1h".
func parseTime(s string, def time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	if s == "now" {
		return time.Now(), nil
	}
	if strings.HasPrefix(s, "now-") {
		d, err := parseDuration(s[4:])
		if err != nil {
			return time.Time{}, err
		}
		return time.Now().Add(-d), nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		if f > 1e15 { // milliseconds / nanoseconds guard
			for f > 1e11 {
				f /= 1000
			}
		}
		sec := int64(f)
		return time.Unix(sec, int64((f-float64(sec))*1e9)), nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, httpx.Validation("invalid time " + strconv.Quote(s))
	}
	return t, nil
}

var promDurRe = regexp.MustCompile(`^(\d+)(ms|s|m|h|d|w|y)$`)

// parseDuration accepts Go durations and Prometheus units (1d, 2w) or bare seconds.
func parseDuration(s string) (time.Duration, error) {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(f * float64(time.Second)), nil
	}
	if m := promDurRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := map[string]time.Duration{"ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour,
			"d": 24 * time.Hour, "w": 7 * 24 * time.Hour, "y": 365 * 24 * time.Hour}[m[2]]
		return time.Duration(n) * unit, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, httpx.Validation("invalid duration " + strconv.Quote(s))
	}
	return d, nil
}

func validatePromQL(q string) error {
	q = strings.TrimSpace(q)
	if q == "" {
		return httpx.Validation("query is required")
	}
	if len(q) > maxQueryLen {
		return httpx.Validation("query too long")
	}
	return nil
}

func fmtUnix(t time.Time) string { return strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', 3, 64) }

// PromInstant runs /api/v1/query.
func (c *client) PromInstant(ctx context.Context, query, at string) (*promEnvelope, error) {
	if err := validatePromQL(query); err != nil {
		return nil, err
	}
	q := url.Values{"query": {query}}
	if at != "" {
		t, err := parseTime(at, time.Now())
		if err != nil {
			return nil, err
		}
		q.Set("time", fmtUnix(t))
	}
	return c.prom(ctx, "/api/v1/query", q)
}

// PromRange runs /api/v1/query_range; step defaults to (end-start)/250, max 11000 points.
func (c *client) PromRange(ctx context.Context, query, start, end, step string) (*promEnvelope, error) {
	if err := validatePromQL(query); err != nil {
		return nil, err
	}
	now := time.Now()
	e, err := parseTime(end, now)
	if err != nil {
		return nil, err
	}
	s, err := parseTime(start, e.Add(-time.Hour))
	if err != nil {
		return nil, err
	}
	if !s.Before(e) {
		return nil, httpx.Validation("start must be before end")
	}
	var st time.Duration
	if step == "" {
		st = e.Sub(s) / 250
		if st < time.Second {
			st = time.Second
		}
	} else if st, err = parseDuration(step); err != nil {
		return nil, err
	}
	if st <= 0 {
		return nil, httpx.Validation("step must be positive")
	}
	if e.Sub(s)/st > 11000 {
		return nil, httpx.Validation("too many points (>11000): increase step or shrink range")
	}
	q := url.Values{"query": {query}, "start": {fmtUnix(s)}, "end": {fmtUnix(e)},
		"step": {strconv.FormatFloat(st.Seconds(), 'f', -1, 64)}}
	return c.prom(ctx, "/api/v1/query_range", q)
}

// PromMeta proxies labels / label values / series with match[] and optional range.
func (c *client) PromMeta(ctx context.Context, path string, match []string, start, end string, requireMatch bool) (*promEnvelope, error) {
	if requireMatch && len(match) == 0 {
		return nil, httpx.Validation("at least one match[] selector is required")
	}
	q := url.Values{}
	for _, m := range match {
		if err := validatePromQL(m); err != nil {
			return nil, err
		}
		q.Add("match[]", m)
	}
	for k, v := range map[string]string{"start": start, "end": end} {
		if v != "" {
			t, err := parseTime(v, time.Now())
			if err != nil {
				return nil, err
			}
			q.Set(k, fmtUnix(t))
		}
	}
	return c.prom(ctx, path, q)
}

// ------------------------------------------------------------------------ Logs

// LogQuery is the unified log query request.
type LogQuery struct {
	Datasource int64  `json:"datasource" binding:"required"`
	Query      string `json:"query"`
	From       string `json:"from"`
	To         string `json:"to"`
	Limit      int    `json:"limit"`
	Index      string `json:"index,omitempty"` // elasticsearch override
}

// LogRow is a normalized log line.
type LogRow struct {
	TS      time.Time         `json:"ts"`
	Level   string            `json:"level"`
	Service string            `json:"service"`
	Host    string            `json:"host"`
	Message string            `json:"message"`
	Labels  map[string]string `json:"labels"`
}

// LogResult is returned by the unified log query.
type LogResult struct {
	Datasource string   `json:"datasource"`
	Engine     string   `json:"engine"`
	Query      string   `json:"query"` // the query actually sent upstream
	Total      int64    `json:"total"`
	Rows       []LogRow `json:"rows"`
}

func (q *LogQuery) bounds() (time.Time, time.Time, error) {
	now := time.Now()
	to, err := parseTime(q.To, now)
	if err != nil {
		return to, to, err
	}
	from, err := parseTime(q.From, to.Add(-time.Hour))
	if err != nil {
		return from, to, err
	}
	if !from.Before(to) {
		return from, to, httpx.Validation("from must be before to")
	}
	if q.Limit <= 0 {
		q.Limit = 100
	}
	if q.Limit > 5000 {
		q.Limit = 5000
	}
	return from, to, nil
}

var levelRe = regexp.MustCompile(`(?i)\b(trace|debug|info|warn|warning|error|err|fatal|panic|critical)\b`)

func normLevel(l string) string {
	switch strings.ToLower(l) {
	case "warning", "warn":
		return "warn"
	case "err", "error":
		return "error"
	case "critical", "fatal", "panic", "crit":
		return "fatal"
	case "":
		return ""
	}
	return strings.ToLower(l)
}

func pick(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := m[k]; v != "" {
			return v
		}
	}
	return ""
}

func fillRow(r *LogRow) {
	if r.Level == "" {
		r.Level = pick(r.Labels, "level", "detected_level", "severity", "log.level", "lvl", "loglevel")
	}
	if r.Level == "" {
		if m := levelRe.FindStringSubmatch(firstN(r.Message, 200)); m != nil {
			r.Level = m[1]
		}
	}
	r.Level = normLevel(r.Level)
	if r.Service == "" {
		r.Service = pick(r.Labels, "service_name", "service", "service.name", "app", "app_kubernetes_io_name",
			"kubernetes.labels.app", "container", "kubernetes.container_name", "kubernetes.container.name", "job")
	}
	if r.Host == "" {
		r.Host = pick(r.Labels, "host", "hostname", "host.name", "host.hostname", "node_name", "instance", "pod",
			"kubernetes.pod_name", "kubernetes.pod.name", "agent.hostname")
	}
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// QueryLogs dispatches to Loki or Elasticsearch.
func (c *client) QueryLogs(ctx context.Context, q *LogQuery) (*LogResult, error) {
	from, to, err := q.bounds()
	if err != nil {
		return nil, err
	}
	switch c.ds.Type {
	case "loki":
		return c.lokiQuery(ctx, q, from, to)
	case "elasticsearch":
		return c.esQuery(ctx, q, from, to)
	}
	return nil, httpx.BadRequest("datasource type " + c.ds.Type + " does not support log queries")
}

func (c *client) lokiQuery(ctx context.Context, q *LogQuery, from, to time.Time) (*LogResult, error) {
	if strings.TrimSpace(q.Query) == "" {
		return nil, httpx.Validation("LogQL query is required, e.g. {app=\"api\"} |= \"error\"")
	}
	if len(q.Query) > maxQueryLen {
		return nil, httpx.Validation("query too long")
	}
	params := url.Values{"query": {q.Query}, "start": {strconv.FormatInt(from.UnixNano(), 10)},
		"end": {strconv.FormatInt(to.UnixNano(), 10)}, "limit": {strconv.Itoa(q.Limit)}, "direction": {"backward"}}
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Stream map[string]string `json:"stream"`
				Values [][]string        `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/loki/api/v1/query_range", params, &resp); err != nil {
		return nil, err
	}
	if resp.Data.ResultType != "" && resp.Data.ResultType != "streams" {
		return nil, httpx.BadRequest("LogQL returned " + resp.Data.ResultType + "; use a log (stream) query, metric queries go to Prometheus-style endpoints")
	}
	rows := []LogRow{}
	for _, s := range resp.Data.Result {
		for _, v := range s.Values {
			if len(v) < 2 {
				continue
			}
			ns, _ := strconv.ParseInt(v[0], 10, 64)
			r := LogRow{TS: time.Unix(0, ns).UTC(), Message: v[1], Labels: s.Stream}
			if r.Labels == nil {
				r.Labels = map[string]string{}
			}
			fillRow(&r)
			rows = append(rows, r)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].TS.After(rows[j].TS) })
	if len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	return &LogResult{Datasource: c.ds.Name, Engine: "loki", Query: q.Query, Total: int64(len(rows)), Rows: rows}, nil
}

var (
	kqlBoolRe  = regexp.MustCompile(`(^|\s)(and|or|not)(\s)`)
	kqlColonRe = regexp.MustCompile(`([\w.@-]+)\s*:\s*`)
)

// translateQuery turns a KQL-like `field:value and field2:"x y"` string into a
// Lucene query_string query (uppercase boolean operators, no space after colon).
// Empty input matches everything.
func translateQuery(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "*"
	}
	// operate only outside double-quoted phrases
	var b strings.Builder
	parts := strings.Split(s, `"`)
	for i, p := range parts {
		if i%2 == 0 {
			p = kqlColonRe.ReplaceAllString(p, "$1:")
			for kqlBoolRe.MatchString(p) { // loop: adjacent operators share whitespace
				p = kqlBoolRe.ReplaceAllStringFunc(p, strings.ToUpper)
			}
		}
		if i > 0 {
			b.WriteByte('"')
		}
		b.WriteString(p)
	}
	return b.String()
}

func (c *client) esQuery(ctx context.Context, q *LogQuery, from, to time.Time) (*LogResult, error) {
	index := q.Index
	if index == "" {
		index = c.ds.opt("index")
	}
	if index == "" {
		return nil, httpx.Validation("elasticsearch index required (datasource options.index or request index)")
	}
	if strings.ContainsAny(index, "/?#") {
		return nil, httpx.Validation("invalid index")
	}
	tf := c.ds.opt("time_field")
	if tf == "" {
		tf = "@timestamp"
	}
	lucene := translateQuery(q.Query)
	body := map[string]any{
		"size":             q.Limit,
		"track_total_hits": true,
		"sort":             []any{map[string]any{tf: map[string]any{"order": "desc"}}},
		"query": map[string]any{"bool": map[string]any{
			"must": []any{map[string]any{"query_string": map[string]any{
				"query": lucene, "default_operator": "AND", "analyze_wildcard": true, "lenient": true}}},
			"filter": []any{map[string]any{"range": map[string]any{tf: map[string]any{
				"gte": from.UTC().Format(time.RFC3339Nano), "lte": to.UTC().Format(time.RFC3339Nano),
				"format": "strict_date_optional_time"}}}},
		}},
	}
	raw, err := c.do(ctx, http.MethodPost, "/"+url.PathEscape(index)+"/_search", nil, body)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Hits struct {
			Total json.RawMessage `json:"total"`
			Hits  []struct {
				Index  string         `json:"_index"`
				ID     string         `json:"_id"`
				Source map[string]any `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, upstreamErr("elasticsearch: invalid JSON: %v", err)
	}
	var total int64
	var tobj struct {
		Value int64 `json:"value"`
	}
	if json.Unmarshal(resp.Hits.Total, &tobj) == nil {
		total = tobj.Value
	} else {
		_ = json.Unmarshal(resp.Hits.Total, &total)
	}
	mf := c.ds.opt("message_field")
	rows := make([]LogRow, 0, len(resp.Hits.Hits))
	for _, h := range resp.Hits.Hits {
		flat := map[string]string{}
		flatten("", h.Source, flat)
		r := LogRow{Labels: flat}
		r.TS, _ = time.Parse(time.RFC3339Nano, flat[tf])
		if mf != "" {
			r.Message = flat[mf]
		}
		if r.Message == "" {
			r.Message = pick(flat, "message", "log", "msg", "event.original", "log.original")
		}
		for _, k := range []string{mf, "message", "log", "msg", tf} {
			delete(flat, k)
		}
		flat["_index"], flat["_id"] = h.Index, h.ID
		fillRow(&r)
		rows = append(rows, r)
	}
	return &LogResult{Datasource: c.ds.Name, Engine: "elasticsearch", Query: lucene, Total: total, Rows: rows}, nil
}

// flatten turns nested JSON into dotted keys with string values.
func flatten(prefix string, v any, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, vv := range t {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flatten(key, vv, out)
		}
	case nil:
	case string:
		out[prefix] = t
	case []any:
		b, _ := json.Marshal(t)
		out[prefix] = string(b)
	default:
		out[prefix] = fmt.Sprint(t)
	}
}

// ---------------------------------------------------------------------- Traces

// TraceSummary is a normalized search hit.
type TraceSummary struct {
	TraceID       string    `json:"trace_id"`
	RootService   string    `json:"root_service"`
	RootOperation string    `json:"root_operation"`
	Start         time.Time `json:"start"`
	DurationMs    float64   `json:"duration_ms"`
	SpanCount     int       `json:"span_count,omitempty"`
	Services      []string  `json:"services,omitempty"`
	Error         bool      `json:"error"`
}

// Span is a normalized span.
type Span struct {
	SpanID     string            `json:"span_id"`
	ParentID   string            `json:"parent_id"`
	Service    string            `json:"service"`
	Operation  string            `json:"operation"`
	Start      time.Time         `json:"start"`
	DurationUs int64             `json:"duration_us"`
	Error      bool              `json:"error"`
	Tags       map[string]string `json:"tags"`
}

// Trace is a normalized full trace.
type Trace struct {
	TraceID string `json:"trace_id"`
	Spans   []Span `json:"spans"`
}

// TraceSearch are the search parameters (service/operation/tags apply to both backends).
type TraceSearch struct {
	Service, Operation, Tags, Query, MinDuration, MaxDuration, From, To string
	Limit                                                               int
}

type jaegerTrace struct {
	TraceID string `json:"traceID"`
	Spans   []struct {
		TraceID       string `json:"traceID"`
		SpanID        string `json:"spanID"`
		OperationName string `json:"operationName"`
		References    []struct {
			RefType string `json:"refType"`
			SpanID  string `json:"spanID"`
		} `json:"references"`
		StartTime int64 `json:"startTime"`
		Duration  int64 `json:"duration"`
		Tags      []struct {
			Key   string `json:"key"`
			Value any    `json:"value"`
		} `json:"tags"`
		ProcessID string `json:"processID"`
	} `json:"spans"`
	Processes map[string]struct {
		ServiceName string `json:"serviceName"`
		Tags        []struct {
			Key   string `json:"key"`
			Value any    `json:"value"`
		} `json:"tags"`
	} `json:"processes"`
}

func (jt *jaegerTrace) normalize() Trace {
	t := Trace{TraceID: jt.TraceID, Spans: []Span{}}
	for _, s := range jt.Spans {
		sp := Span{SpanID: s.SpanID, Operation: s.OperationName, Start: time.UnixMicro(s.StartTime).UTC(),
			DurationUs: s.Duration, Service: jt.Processes[s.ProcessID].ServiceName, Tags: map[string]string{}}
		// OpenTelemetry resource attributes (including k8s.cluster.name) are
		// represented as Jaeger process tags, rather than span tags.
		for _, tg := range jt.Processes[s.ProcessID].Tags {
			sp.Tags[tg.Key] = fmt.Sprint(tg.Value)
		}
		for _, r := range s.References {
			if r.RefType == "CHILD_OF" || sp.ParentID == "" {
				sp.ParentID = r.SpanID
			}
		}
		for _, tg := range s.Tags {
			value := fmt.Sprint(tg.Value)
			sp.Tags[tg.Key] = value
			if (tg.Key == "error" && value == "true") || (tg.Key == "otel.status_code" && strings.EqualFold(value, "ERROR")) || (tg.Key == "http.status_code" && strings.HasPrefix(value, "5")) {
				sp.Error = true
			}
		}
		t.Spans = append(t.Spans, sp)
	}
	return t
}

func summarize(t Trace) TraceSummary {
	s := TraceSummary{TraceID: t.TraceID, SpanCount: len(t.Spans)}
	svc := map[string]bool{}
	var minStart, maxEnd time.Time
	for i, sp := range t.Spans {
		svc[sp.Service] = true
		end := sp.Start.Add(time.Duration(sp.DurationUs) * time.Microsecond)
		if i == 0 || sp.Start.Before(minStart) {
			minStart = sp.Start
		}
		if end.After(maxEnd) {
			maxEnd = end
		}
		if sp.ParentID == "" && s.RootService == "" {
			s.RootService, s.RootOperation = sp.Service, sp.Operation
		}
		s.Error = s.Error || sp.Error
	}
	for k := range svc {
		s.Services = append(s.Services, k)
	}
	sort.Strings(s.Services)
	s.Start = minStart
	s.DurationMs = float64(maxEnd.Sub(minStart).Microseconds()) / 1000
	return s
}

// TraceServices lists services known to the tracing backend.
func (c *client) TraceServices(ctx context.Context) ([]string, error) {
	switch c.ds.Type {
	case "jaeger":
		var r struct {
			Data []string `json:"data"`
		}
		err := c.getJSON(ctx, "/api/services", nil, &r)
		return r.Data, err
	case "tempo":
		var r struct {
			TagValues []any `json:"tagValues"`
		}
		if err := c.getJSON(ctx, "/api/v2/search/tag/resource.service.name/values", nil, &r); err != nil {
			return nil, err
		}
		out := []string{}
		for _, v := range r.TagValues {
			if m, ok := v.(map[string]any); ok {
				out = append(out, fmt.Sprint(m["value"]))
			} else {
				out = append(out, fmt.Sprint(v))
			}
		}
		return out, nil
	}
	return nil, httpx.BadRequest("not a tracing datasource")
}

// SearchTraces searches Jaeger /api/traces or Tempo /api/search.
func (c *client) SearchTraces(ctx context.Context, s TraceSearch) ([]TraceSummary, error) {
	now := time.Now()
	end, err := parseTime(s.To, now)
	if err != nil {
		return nil, err
	}
	start, err := parseTime(s.From, end.Add(-time.Hour))
	if err != nil {
		return nil, err
	}
	if s.Limit <= 0 || s.Limit > 1000 {
		s.Limit = 20
	}
	out := []TraceSummary{}
	switch c.ds.Type {
	case "jaeger":
		if s.Service == "" {
			return nil, httpx.Validation("service is required for Jaeger search")
		}
		q := url.Values{"service": {s.Service}, "start": {strconv.FormatInt(start.UnixMicro(), 10)},
			"end": {strconv.FormatInt(end.UnixMicro(), 10)}, "limit": {strconv.Itoa(s.Limit)}}
		for k, v := range map[string]string{"operation": s.Operation, "tags": s.Tags, "minDuration": s.MinDuration, "maxDuration": s.MaxDuration} {
			if v != "" {
				q.Set(k, v)
			}
		}
		var r struct {
			Data []jaegerTrace `json:"data"`
		}
		if err := c.getJSON(ctx, "/api/traces", q, &r); err != nil {
			return nil, err
		}
		for i := range r.Data {
			out = append(out, summarize(r.Data[i].normalize()))
		}
	case "tempo":
		q := url.Values{"start": {strconv.FormatInt(start.Unix(), 10)}, "end": {strconv.FormatInt(end.Unix(), 10)},
			"limit": {strconv.Itoa(s.Limit)}}
		if s.Query != "" { // TraceQL
			q.Set("q", s.Query)
		} else {
			var tags []string
			if s.Service != "" {
				tags = append(tags, "service.name="+s.Service)
			}
			if s.Operation != "" {
				tags = append(tags, "name="+s.Operation)
			}
			if s.Tags != "" { // Jaeger-style JSON {"k":"v"} or logfmt k=v
				var m map[string]string
				if json.Unmarshal([]byte(s.Tags), &m) == nil {
					for k, v := range m {
						tags = append(tags, k+"="+v)
					}
				} else {
					tags = append(tags, s.Tags)
				}
			}
			if len(tags) > 0 {
				q.Set("tags", strings.Join(tags, " "))
			}
			if s.MinDuration != "" {
				q.Set("minDuration", s.MinDuration)
			}
			if s.MaxDuration != "" {
				q.Set("maxDuration", s.MaxDuration)
			}
		}
		var r struct {
			Traces []struct {
				TraceID           string `json:"traceID"`
				RootServiceName   string `json:"rootServiceName"`
				RootTraceName     string `json:"rootTraceName"`
				StartTimeUnixNano string `json:"startTimeUnixNano"`
				DurationMs        int64  `json:"durationMs"`
			} `json:"traces"`
		}
		if err := c.getJSON(ctx, "/api/search", q, &r); err != nil {
			return nil, err
		}
		for _, t := range r.Traces {
			ns, _ := strconv.ParseInt(t.StartTimeUnixNano, 10, 64)
			out = append(out, TraceSummary{TraceID: t.TraceID, RootService: t.RootServiceName, RootOperation: t.RootTraceName,
				Start: time.Unix(0, ns).UTC(), DurationMs: float64(t.DurationMs)})
		}
	default:
		return nil, httpx.BadRequest("not a tracing datasource")
	}
	return out, nil
}

var traceIDRe = regexp.MustCompile(`^[0-9a-fA-F]{1,32}$`)

// GetTrace fetches one trace by id.
func (c *client) GetTrace(ctx context.Context, id string) (*Trace, error) {
	if !traceIDRe.MatchString(id) {
		return nil, httpx.Validation("invalid trace id")
	}
	switch c.ds.Type {
	case "jaeger":
		var r struct {
			Data []jaegerTrace `json:"data"`
		}
		if err := c.getJSON(ctx, "/api/traces/"+id, nil, &r); err != nil {
			return nil, err
		}
		if len(r.Data) == 0 {
			return nil, httpx.NotFound("trace not found")
		}
		t := r.Data[0].normalize()
		return &t, nil
	case "tempo":
		raw, err := c.do(ctx, http.MethodGet, "/api/traces/"+id, nil, nil)
		if err != nil {
			return nil, err
		}
		return parseOTLPTrace(id, raw)
	}
	return nil, httpx.BadRequest("not a tracing datasource")
}

type otlpAttr struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

func attrVal(v map[string]any) string {
	for _, k := range []string{"stringValue", "intValue", "boolValue", "doubleValue"} {
		if x, ok := v[k]; ok {
			return fmt.Sprint(x)
		}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// otlpID converts a base64 (protobuf JSON) or hex id to lowercase hex.
func otlpID(s string) string {
	if s == "" {
		return ""
	}
	if (len(s) == 16 || len(s) == 32) && traceIDRe.MatchString(s) {
		return strings.ToLower(s)
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return hex.EncodeToString(b)
	}
	return s
}

func parseOTLPTrace(id string, raw []byte) (*Trace, error) {
	type otlpSpan struct {
		SpanID            string     `json:"spanId"`
		ParentSpanID      string     `json:"parentSpanId"`
		Name              string     `json:"name"`
		StartTimeUnixNano string     `json:"startTimeUnixNano"`
		EndTimeUnixNano   string     `json:"endTimeUnixNano"`
		Attributes        []otlpAttr `json:"attributes"`
		Status            struct {
			Code any `json:"code"`
		} `json:"status"`
	}
	type scope struct {
		Spans []otlpSpan `json:"spans"`
	}
	type batch struct {
		Resource struct {
			Attributes []otlpAttr `json:"attributes"`
		} `json:"resource"`
		ScopeSpans                  []scope `json:"scopeSpans"`
		InstrumentationLibrarySpans []scope `json:"instrumentationLibrarySpans"`
	}
	var r struct {
		Batches       []batch `json:"batches"`
		ResourceSpans []batch `json:"resourceSpans"`
		Trace         *struct {
			Batches       []batch `json:"batches"`
			ResourceSpans []batch `json:"resourceSpans"`
		} `json:"trace"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, upstreamErr("tempo: invalid JSON: %v", err)
	}
	batches := append(r.Batches, r.ResourceSpans...)
	if r.Trace != nil {
		batches = append(batches, r.Trace.Batches...)
		batches = append(batches, r.Trace.ResourceSpans...)
	}
	t := &Trace{TraceID: strings.ToLower(id), Spans: []Span{}}
	for _, b := range batches {
		svc := ""
		for _, a := range b.Resource.Attributes {
			if a.Key == "service.name" {
				svc = attrVal(a.Value)
			}
		}
		for _, sc := range append(b.ScopeSpans, b.InstrumentationLibrarySpans...) {
			for _, s := range sc.Spans {
				st, _ := strconv.ParseInt(s.StartTimeUnixNano, 10, 64)
				en, _ := strconv.ParseInt(s.EndTimeUnixNano, 10, 64)
				sp := Span{SpanID: otlpID(s.SpanID), ParentID: otlpID(s.ParentSpanID), Service: svc, Operation: s.Name,
					Start: time.Unix(0, st).UTC(), DurationUs: (en - st) / 1000, Tags: map[string]string{}}
				for _, a := range s.Attributes {
					sp.Tags[a.Key] = attrVal(a.Value)
				}
				code := fmt.Sprint(s.Status.Code)
				sp.Error = code == "2" || code == "STATUS_CODE_ERROR"
				t.Spans = append(t.Spans, sp)
			}
		}
	}
	if len(t.Spans) == 0 {
		return nil, httpx.NotFound("trace not found")
	}
	return t, nil
}

// ---------------------------------------------------------------- Alertmanager

// SilenceInput is the create-silence payload (Alertmanager v2 schema).
type SilenceInput struct {
	Datasource int64 `json:"datasource" binding:"required"`
	Matchers   []struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		IsRegex bool   `json:"isRegex"`
		IsEqual *bool  `json:"isEqual,omitempty"`
	} `json:"matchers" binding:"required,min=1"`
	StartsAt string `json:"startsAt"`
	EndsAt   string `json:"endsAt"`
	Duration string `json:"duration"` // alternative to endsAt, e.g. "2h"
	Comment  string `json:"comment" binding:"required"`
}

func (c *client) ListSilences(ctx context.Context, filter []string) (any, error) {
	q := url.Values{}
	for _, f := range filter {
		q.Add("filter", f)
	}
	var out any
	err := c.getJSON(ctx, "/api/v2/silences", q, &out)
	return out, err
}

func (c *client) CreateSilence(ctx context.Context, in *SilenceInput, createdBy string) (string, error) {
	now := time.Now()
	start, err := parseTime(in.StartsAt, now)
	if err != nil {
		return "", err
	}
	var end time.Time
	if in.EndsAt != "" {
		if end, err = parseTime(in.EndsAt, now); err != nil {
			return "", err
		}
	} else {
		dur := 2 * time.Hour
		if in.Duration != "" {
			if dur, err = parseDuration(in.Duration); err != nil {
				return "", err
			}
		}
		end = start.Add(dur)
	}
	if !end.After(start) {
		return "", httpx.Validation("endsAt must be after startsAt")
	}
	for _, mt := range in.Matchers {
		if mt.Name == "" {
			return "", httpx.Validation("matcher name required")
		}
	}
	body := map[string]any{"matchers": in.Matchers, "startsAt": start.UTC().Format(time.RFC3339),
		"endsAt": end.UTC().Format(time.RFC3339), "createdBy": createdBy, "comment": in.Comment}
	raw, err := c.do(ctx, http.MethodPost, "/api/v2/silences", nil, body)
	if err != nil {
		return "", err
	}
	var r struct {
		SilenceID string `json:"silenceID"`
	}
	_ = json.Unmarshal(raw, &r)
	return r.SilenceID, nil
}

var silenceIDRe = regexp.MustCompile(`^[0-9a-fA-F-]{8,64}$`)

func (c *client) DeleteSilence(ctx context.Context, id string) error {
	if !silenceIDRe.MatchString(id) {
		return httpx.Validation("invalid silence id")
	}
	_, err := c.do(ctx, http.MethodDelete, "/api/v2/silence/"+id, nil, nil)
	return err
}
