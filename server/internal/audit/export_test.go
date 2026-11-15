package audit

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/backup/objstore"
)

// memSource is an in-memory chainSource holding a hash-chained slice.
type memSource struct {
	rows    []ChainRow
	failAt  int64 // StreamChainRows errors when it reaches this id
	streams int
}

func (m *memSource) ExportIDRange(_ context.Context, from, to time.Time) (int64, int64, bool, error) {
	var first, last int64
	for _, r := range m.rows {
		if !r.CreatedAt.Before(from) && r.CreatedAt.Before(to) {
			if first == 0 || r.ID < first {
				first = r.ID
			}
			if r.ID > last {
				last = r.ID
			}
		}
	}
	return first, last, first != 0, nil
}

func (m *memSource) StreamChainRows(_ context.Context, first, last int64, fn func(ChainRow) error) error {
	m.streams++
	for _, r := range m.rows {
		if r.ID < first || r.ID > last {
			continue
		}
		if m.failAt != 0 && r.ID == m.failAt {
			return errors.New("db gone")
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

type memWriter struct{ entries []Entry }

func (w *memWriter) WriteBatch(_ context.Context, e []Entry) error {
	w.entries = append(w.entries, e...)
	return nil
}

var exportBase = time.Date(2026, 11, 1, 8, 0, 0, 123456000, time.UTC)

// chained builds n rows (ids 10..) one minute apart, linked from GenesisHash.
func chained(n int) []ChainRow {
	prev := GenesisHash
	uid := int64(7)
	out := make([]ChainRow, 0, n)
	for i := 0; i < n; i++ {
		r := ChainRow{
			ID: int64(10 + i), UserID: &uid, Username: "张三", IP: "10.0.0.1", Method: "POST",
			Path: "/api/v1/users", Resource: "users", ResourceID: "3", Action: "create", StatusCode: 201,
			Result: ResultSuccess, DurationMs: 12, RequestID: "req", UserAgent: "ua <b>&",
			Detail: json.RawMessage(`{"data": {"b": 1.50, "a": "x"}}`), CreatedAt: exportBase.Add(time.Duration(i) * time.Minute),
		}
		if i == 2 {
			r.UserID = nil
			r.Username = "=cmd|' /C calc'!A0" // formula injection attempt
			r.Detail = nil
		}
		r.PrevHash = prev
		r.Hash = CanonicalHash(prev, r)
		prev = r.Hash
		out = append(out, r)
	}
	return out
}

func parseJSONL(t *testing.T, body []byte) []ChainRow {
	t.Helper()
	var rows []ChainRow
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		r, err := ParseExportRow(sc.Bytes())
		if err != nil {
			t.Fatalf("parse %q: %v", sc.Text(), err)
		}
		rows = append(rows, r)
	}
	return rows
}

func TestJSONLExportVerifiesOffline(t *testing.T) {
	src := &memSource{rows: chained(5)}
	var buf bytes.Buffer
	st, err := writeExport(context.Background(), src, 11, 14, newJSONLWriter(&buf))
	if err != nil {
		t.Fatal(err)
	}
	if st.Rows != 4 || st.FirstID != 11 || st.LastID != 14 || st.LastHash != src.rows[4].Hash {
		t.Fatalf("stats %+v", st)
	}
	if !strings.Contains(buf.String(), `"prev_hash"`) || !strings.Contains(buf.String(), `ua <b>&`) {
		t.Fatalf("missing fields or html-escaped: %s", buf.String())
	}
	rows := parseJSONL(t, buf.Bytes())
	if len(rows) != 4 {
		t.Fatalf("rows %d", len(rows))
	}
	// offline: anchor is the first exported row's prev_hash
	if n, at, kind := VerifyRows(rows[0].PrevHash, rows); kind != "" || n != 4 {
		t.Fatalf("offline verify: n=%d at=%d kind=%s", n, at, kind)
	}
	// tamper one field in the exported text -> hash mismatch on that row
	tampered := bytes.Replace(buf.Bytes(), []byte(`"ip":"10.0.0.1"`), []byte(`"ip":"10.0.0.9"`), 1)
	rows = parseJSONL(t, tampered)
	if _, at, kind := VerifyRows(rows[0].PrevHash, rows); kind != BreakHashMismatch || at != 11 {
		t.Fatalf("tamper not detected: at=%d kind=%s", at, kind)
	}
	// dropping a line -> prev mismatch on its successor
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	rows = parseJSONL(t, []byte(lines[0]+"\n"+lines[2]+"\n"))
	if _, at, kind := VerifyRows(rows[0].PrevHash, rows); kind != BreakPrevMismatch || at != 13 {
		t.Fatalf("deletion not detected: at=%d kind=%s", at, kind)
	}
}

func TestCSVExport(t *testing.T) {
	src := &memSource{rows: chained(3)}
	var buf bytes.Buffer
	if _, err := writeExport(context.Background(), src, 10, 12, newCSVWriter(&buf)); err != nil {
		t.Fatal(err)
	}
	body := strings.TrimPrefix(buf.String(), "\ufeff")
	if len(body) == buf.Len() {
		t.Fatal("missing BOM")
	}
	recs, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 4 || strings.Join(recs[0], ",") != strings.Join(CSVColumns, ",") {
		t.Fatalf("csv header/rows: %v", recs)
	}
	last := recs[3]
	if last[3] != "'=cmd|' /C calc'!A0" || last[2] != "" || last[15] != "" {
		t.Fatalf("formula not neutralised / null handling: %q", last)
	}
	if last[16] != src.rows[2].PrevHash || last[17] != src.rows[2].Hash || last[1] != "2026-11-01T08:02:00.123456Z" {
		t.Fatalf("hash/time columns: %q", last)
	}
	// empty export still has the header
	buf.Reset()
	w := newCSVWriter(&buf)
	if err := w.Flush(); err != nil || !strings.Contains(buf.String(), "prev_hash,hash") {
		t.Fatalf("empty csv %q %v", buf.String(), err)
	}
}

func TestParseRange(t *testing.T) {
	cases := []struct {
		from, to string
		ok       bool
	}{
		{"2026-11-01T00:00:00+08:00", "2026-11-02T00:00:00+08:00", true},
		{"1762000000", "1762086400", true},
		{"", "2026-11-02T00:00:00Z", false},
		{"2026-11-01T00:00:00Z", "", false},
		{"yesterday", "2026-11-02T00:00:00Z", false},
		{"2026-11-02T00:00:00Z", "2026-11-01T00:00:00Z", false},
		{"2026-11-01T00:00:00Z", "2026-11-01T00:00:00Z", false},
		{"2025-01-01T00:00:00Z", "2026-11-01T00:00:00Z", false},
	}
	for _, c := range cases {
		_, _, err := parseRange(c.from, c.to)
		if (err == nil) != c.ok {
			t.Errorf("parseRange(%q,%q) err=%v", c.from, c.to, err)
		}
	}
}

func testExporter(t *testing.T, src *memSource) (*exporter, *memWriter, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	aw := &memWriter{}
	x := &exporter{src: src, audit: aw, archiveDir: t.TempDir(), tmpDir: t.TempDir(),
		now: func() time.Time { return time.Unix(1763100000, 0) }, busy: make(chan struct{}, 1)}
	r := gin.New()
	r.Use(func(c *gin.Context) { // stands in for the audit middleware: expose overrides
		c.Next()
		c.Header("X-Test-Action", c.GetString(keyAction))
	})
	r.GET("/audit/export", x.handleExport)
	r.POST("/audit/archive", x.handleArchive)
	return x, aw, r
}

func do(r http.Handler, method, url, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, url, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	r.ServeHTTP(w, req)
	return w
}

const qRange = "from=2026-11-01T08:00:00Z&to=2026-11-01T08:03:00Z"

func TestHandleExport(t *testing.T) {
	src := &memSource{rows: chained(5)}
	_, aw, r := testExporter(t, src)

	for _, q := range []string{"", "from=2026-11-01T08:00:00Z", qRange + "&format=xml"} {
		if w := do(r, "GET", "/audit/export?"+q, ""); w.Code != 400 {
			t.Fatalf("%q: want 400, got %d %s", q, w.Code, w.Body)
		}
	}
	if len(aw.entries) != 0 {
		t.Fatal("rejected requests must not stream or record an export")
	}

	w := do(r, "GET", "/audit/export?"+qRange, "")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/x-ndjson") {
		t.Fatalf("export: %d %v", w.Code, w.Header())
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "audit-20261101T080000Z-20261101T080300Z.jsonl") {
		t.Fatalf("content-disposition %q", cd)
	}
	rows := parseJSONL(t, w.Body.Bytes())
	if len(rows) != 3 || rows[0].ID != 10 || rows[2].ID != 12 {
		t.Fatalf("exported %d rows", len(rows))
	}
	if len(aw.entries) != 1 {
		t.Fatalf("audit entries %d", len(aw.entries))
	}
	e := aw.entries[0]
	if e.Resource != "audit" || e.Action != "export" || e.Result != ResultSuccess || !strings.Contains(string(e.Detail), `"rows":3`) {
		t.Fatalf("audit entry %+v %s", e, e.Detail)
	}

	w = do(r, "GET", "/audit/export?"+qRange+"&format=CSV", "")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/csv") || !strings.Contains(w.Body.String(), "prev_hash") {
		t.Fatalf("csv export %d %s", w.Code, w.Body)
	}

	// empty range: 200 with an empty body, still audited
	w = do(r, "GET", "/audit/export?from=2020-01-01T00:00:00Z&to=2020-01-02T00:00:00Z", "")
	if w.Code != 200 || w.Body.Len() != 0 || len(aw.entries) != 3 {
		t.Fatalf("empty export %d %q entries=%d", w.Code, w.Body, len(aw.entries))
	}
}

func TestHandleExportInterrupted(t *testing.T) {
	src := &memSource{rows: chained(5), failAt: 12}
	_, aw, r := testExporter(t, src)
	w := do(r, "GET", "/audit/export?"+qRange, "")
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[2], exportErrorMarker) {
		t.Fatalf("want 2 rows + marker, got %q", lines)
	}
	e := aw.entries[len(aw.entries)-1]
	if e.Result != ResultFailure || !strings.Contains(string(e.Detail), `"complete":false`) {
		t.Fatalf("failure not audited: %+v %s", e, e.Detail)
	}
}

func TestHandleArchiveLocal(t *testing.T) {
	src := &memSource{rows: chained(5)}
	x, _, r := testExporter(t, src)
	w := do(r, "POST", "/audit/archive", `{"from":"2026-11-01T08:00:00Z","to":"2026-11-01T09:00:00Z"}`)
	if w.Code != 200 || w.Header().Get("X-Test-Action") != "archive" {
		t.Fatalf("archive: %d %s action=%q", w.Code, w.Body, w.Header().Get("X-Test-Action"))
	}
	var env struct {
		Data ArchiveResult `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	res := env.Data
	if res.Storage != "local" || res.Rows != 5 || res.FirstID != 10 || res.LastID != 14 ||
		!strings.HasSuffix(res.File, ".jsonl.gz") || strings.Contains(res.File, "/") {
		t.Fatalf("result %+v", res)
	}
	raw, err := os.ReadFile(filepath.Join(x.archiveDir, "audit", res.File))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != res.SHA256 || int64(len(raw)) != res.Size {
		t.Fatal("sha256/size mismatch")
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	rows := parseJSONL(t, plain)
	if n, _, kind := VerifyRows(GenesisHash, rows); kind != "" || n != 5 || rows[4].Hash != res.LastHash {
		t.Fatalf("archive does not verify: n=%d kind=%s", n, kind)
	}
	tmps, _ := os.ReadDir(x.tmpDir)
	if len(tmps) != 0 {
		t.Fatalf("temp file left behind: %v", tmps)
	}
}

type memStore struct {
	objstore.Store
	keys map[string][]byte
}

func (m *memStore) Kind() string     { return "s3" }
func (m *memStore) Describe() string { return "s3://bucket/p" }
func (m *memStore) Put(_ context.Context, key string, r io.Reader, size int64) (string, error) {
	b, _ := io.ReadAll(r)
	if int64(len(b)) != size {
		return "", errors.New("size mismatch")
	}
	m.keys[key] = b
	return "s3://bucket/p/" + key, nil
}

func TestHandleArchiveObjectStoreAndErrors(t *testing.T) {
	src := &memSource{rows: chained(3)}
	x, _, r := testExporter(t, src)
	ms := &memStore{keys: map[string][]byte{}}
	x.archive = ms
	w := do(r, "POST", "/audit/archive", `{"from":"2026-11-01T08:00:00Z","to":"2026-11-01T09:00:00Z"}`)
	if w.Code != 200 || len(ms.keys) != 1 || !strings.Contains(w.Body.String(), `"storage":"s3"`) {
		t.Fatalf("object store archive: %d %s", w.Code, w.Body)
	}
	for k := range ms.keys {
		if !strings.HasPrefix(k, "audit/audit-20261101T080000Z-20261101T090000Z-10-12-") {
			t.Fatalf("key %q", k)
		}
	}
	// empty range, bad body, bad range
	for _, body := range []string{`{"from":"2020-01-01T00:00:00Z","to":"2020-01-02T00:00:00Z"}`, `not json`, `{"from":"2026-11-02T00:00:00Z","to":"2026-11-01T00:00:00Z"}`} {
		if w := do(r, "POST", "/audit/archive", body); w.Code != 400 {
			t.Fatalf("%s: want 400 got %d", body, w.Code)
		}
	}
	// one archive at a time
	x.busy <- struct{}{}
	if w := do(r, "POST", "/audit/archive", `{"from":"2026-11-01T08:00:00Z","to":"2026-11-01T09:00:00Z"}`); w.Code != 409 {
		t.Fatalf("busy: want 409 got %d", w.Code)
	}
	<-x.busy
	// stream failure -> 500 and nothing stored
	src.failAt = 11
	n := len(ms.keys)
	if w := do(r, "POST", "/audit/archive", `{"from":"2026-11-01T08:00:00Z","to":"2026-11-01T09:00:00Z"}`); w.Code != 500 || len(ms.keys) != n {
		t.Fatalf("failure: %d keys=%d", w.Code, len(ms.keys))
	}
}

func TestExportRoutesRequirePermission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	newExporter(&PgStore{}).register(r)
	for _, c := range []struct{ m, u string }{{"GET", "/audit/export?" + qRange}, {"POST", "/audit/archive"}} {
		if w := do(r, c.m, c.u, ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without principal: %d", c.m, c.u, w.Code)
		}
	}
}

func TestDefaultArchiveStore(t *testing.T) {
	x := newExporter(&PgStore{})
	if l, ok := x.store().(*objstore.Local); !ok || l.Dir != DefaultArchiveDir {
		t.Fatalf("default store %#v", x.store())
	}
}
