package audit

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/backup/objstore"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// Export / archive permissions. audit:export and audit:manage must be in the
// core permission catalog (rbac.CorePermissions) for non-admin roles to be
// granted them; "*" (super admin) always passes.
const (
	PermAuditExport = "audit:export"
	PermAuditManage = "audit:manage"
)

// Export formats.
const (
	FormatJSONL = "jsonl"
	FormatCSV   = "csv"
)

const (
	exportMaxRange = 366 * 24 * time.Hour
	exportChunk    = 1000
	// DefaultArchiveDir is used when no object store and no archive dir are
	// configured (matches config.ArchiveConfig's default).
	DefaultArchiveDir = "/var/lib/matrix/archive"
	archiveKeyPrefix  = "audit/"
	// exportErrorMarker is the last JSONL line / CSV row written when a stream
	// fails after the response has started (status and headers already sent).
	exportErrorMarker = "export interrupted"
)

// canonicalTimeLayout is the timestamp form CanonicalHash hashes; exports use
// it verbatim so an offline verifier can recompute every hash.
const canonicalTimeLayout = "2006-01-02T15:04:05.000000Z07:00"

// ExportRow is one JSONL line of an audit export: every column CanonicalHash
// covers plus prev_hash/hash. created_at is UTC with microseconds.
type ExportRow struct {
	ID         int64           `json:"id"`
	CreatedAt  string          `json:"created_at"`
	UserID     *int64          `json:"user_id"`
	Username   string          `json:"username"`
	IP         string          `json:"ip"`
	Method     string          `json:"method"`
	Path       string          `json:"path"`
	Resource   string          `json:"resource"`
	ResourceID string          `json:"resource_id"`
	Action     string          `json:"action"`
	StatusCode int             `json:"status_code"`
	Result     string          `json:"result"`
	DurationMs int             `json:"duration_ms"`
	RequestID  string          `json:"request_id"`
	UserAgent  string          `json:"user_agent"`
	Detail     json.RawMessage `json:"detail"`
	PrevHash   string          `json:"prev_hash"`
	Hash       string          `json:"hash"`
}

func toExportRow(r ChainRow) ExportRow {
	detail := json.RawMessage("null")
	if d := strings.TrimSpace(string(r.Detail)); d != "" {
		if json.Valid([]byte(d)) {
			detail = json.RawMessage(d)
		} else { // CanonicalHash hashes invalid JSON as a string; keep it one
			b, _ := json.Marshal(cleanText(d))
			detail = b
		}
	}
	return ExportRow{
		ID: r.ID, CreatedAt: r.CreatedAt.UTC().Format(canonicalTimeLayout), UserID: r.UserID,
		Username: r.Username, IP: r.IP, Method: r.Method, Path: r.Path, Resource: r.Resource,
		ResourceID: r.ResourceID, Action: r.Action, StatusCode: r.StatusCode, Result: r.Result,
		DurationMs: r.DurationMs, RequestID: r.RequestID, UserAgent: r.UserAgent, Detail: detail,
		PrevHash: r.PrevHash, Hash: r.Hash,
	}
}

// ParseExportRow decodes one JSONL export line back into a ChainRow so it can
// be fed to VerifyRows (anchor = the first row's prev_hash).
func ParseExportRow(line []byte) (ChainRow, error) {
	var x ExportRow
	if err := json.Unmarshal(line, &x); err != nil {
		return ChainRow{}, err
	}
	if x.ID == 0 {
		return ChainRow{}, errors.New("audit export: line has no id")
	}
	t, err := time.Parse(time.RFC3339Nano, x.CreatedAt)
	if err != nil {
		return ChainRow{}, fmt.Errorf("audit export: created_at: %w", err)
	}
	r := ChainRow{
		ID: x.ID, UserID: x.UserID, Username: x.Username, IP: x.IP, Method: x.Method, Path: x.Path,
		Resource: x.Resource, ResourceID: x.ResourceID, Action: x.Action, StatusCode: x.StatusCode,
		Result: x.Result, DurationMs: x.DurationMs, RequestID: x.RequestID, UserAgent: x.UserAgent,
		CreatedAt: t, PrevHash: x.PrevHash, Hash: x.Hash,
	}
	if d := strings.TrimSpace(string(x.Detail)); d != "" && d != "null" {
		r.Detail = json.RawMessage(d)
	}
	return r, nil
}

// ---------------------------------------------------------------- writers

type rowWriter interface {
	Write(r ChainRow) error
	// Fail appends the interruption marker after a mid-stream error.
	Fail() error
	Flush() error
}

type jsonlWriter struct {
	bw  *bufio.Writer
	enc *json.Encoder
}

func newJSONLWriter(w io.Writer) *jsonlWriter {
	bw := bufio.NewWriterSize(w, 64<<10)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	return &jsonlWriter{bw: bw, enc: enc}
}

func (j *jsonlWriter) Write(r ChainRow) error { return j.enc.Encode(toExportRow(r)) }
func (j *jsonlWriter) Fail() error {
	return j.enc.Encode(map[string]string{"error": exportErrorMarker})
}
func (j *jsonlWriter) Flush() error { return j.bw.Flush() }

// CSVColumns is the header row of a CSV export.
var CSVColumns = []string{"id", "created_at", "user_id", "username", "ip", "method", "path", "resource", "resource_id",
	"action", "status_code", "result", "duration_ms", "request_id", "user_agent", "detail", "prev_hash", "hash"}

type csvWriter struct {
	bw     *bufio.Writer
	cw     *csv.Writer
	header bool
}

func newCSVWriter(w io.Writer) *csvWriter {
	bw := bufio.NewWriterSize(w, 64<<10)
	return &csvWriter{bw: bw, cw: csv.NewWriter(bw)}
}

// csvCell neutralises spreadsheet formula injection: a text cell starting with
// = + - @ TAB or CR gets a leading apostrophe. Offline verification should use
// JSONL, which is written verbatim.
func csvCell(s string) string {
	s = cleanText(s)
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func (w *csvWriter) writeHeader() error {
	if w.header {
		return nil
	}
	w.header = true
	if _, err := w.bw.WriteString("\ufeff"); err != nil { // BOM so spreadsheet tools read UTF-8 (中文)
		return err
	}
	return w.cw.Write(CSVColumns)
}

func (w *csvWriter) Write(r ChainRow) error {
	if err := w.writeHeader(); err != nil {
		return err
	}
	x := toExportRow(r)
	uid := ""
	if x.UserID != nil {
		uid = strconv.FormatInt(*x.UserID, 10)
	}
	detail := ""
	if string(x.Detail) != "null" {
		detail = string(x.Detail)
	}
	return w.cw.Write([]string{
		strconv.FormatInt(x.ID, 10), x.CreatedAt, uid, csvCell(x.Username), csvCell(x.IP), csvCell(x.Method),
		csvCell(x.Path), csvCell(x.Resource), csvCell(x.ResourceID), csvCell(x.Action), strconv.Itoa(x.StatusCode),
		csvCell(x.Result), strconv.Itoa(x.DurationMs), csvCell(x.RequestID), csvCell(x.UserAgent), csvCell(detail),
		x.PrevHash, x.Hash,
	})
}

func (w *csvWriter) Fail() error {
	if err := w.writeHeader(); err != nil {
		return err
	}
	return w.cw.Write([]string{"#" + exportErrorMarker})
}

func (w *csvWriter) Flush() error {
	if err := w.writeHeader(); err != nil { // empty export still gets a header
		return err
	}
	w.cw.Flush()
	if err := w.cw.Error(); err != nil {
		return err
	}
	return w.bw.Flush()
}

func newRowWriter(format string, w io.Writer) rowWriter {
	if format == FormatCSV {
		return newCSVWriter(w)
	}
	return newJSONLWriter(w)
}

// ---------------------------------------------------------------- source

// chainSource reads audit rows for export. PgStore implements it.
type chainSource interface {
	// ExportIDRange returns the smallest and largest id whose created_at is in
	// [from, to); ok is false when there is none.
	ExportIDRange(ctx context.Context, from, to time.Time) (first, last int64, ok bool, err error)
	// StreamChainRows calls fn for every row with first <= id <= last in id order.
	StreamChainRows(ctx context.Context, first, last int64, fn func(ChainRow) error) error
}

// ExportIDRange implements chainSource.
func (s *PgStore) ExportIDRange(ctx context.Context, from, to time.Time) (int64, int64, bool, error) {
	var first, last *int64
	err := s.DB.QueryRow(ctx, `SELECT min(id), max(id) FROM audit_logs WHERE created_at >= $1 AND created_at < $2`, from, to).
		Scan(&first, &last)
	if err != nil {
		return 0, 0, false, err
	}
	if first == nil || last == nil {
		return 0, 0, false, nil
	}
	return *first, *last, true, nil
}

// StreamChainRows implements chainSource with keyset pages of exportChunk rows,
// so no connection is held while the client reads slowly.
func (s *PgStore) StreamChainRows(ctx context.Context, first, last int64, fn func(ChainRow) error) error {
	next := first
	for next <= last {
		page, err := s.chainPage(ctx, next, last)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, r := range page {
			if err := fn(r); err != nil {
				return err
			}
		}
		next = page[len(page)-1].ID + 1
	}
	return nil
}

func (s *PgStore) chainPage(ctx context.Context, from, last int64) ([]ChainRow, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+chainRowCols+` FROM audit_logs WHERE id >= $1 AND id <= $2 ORDER BY id LIMIT $3`,
		from, last, exportChunk)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ChainRow, 0, exportChunk)
	for rows.Next() {
		var r ChainRow
		var detail []byte
		var prev, hash *string
		if err := rows.Scan(&r.ID, &r.UserID, &r.Username, &r.IP, &r.Method, &r.Path, &r.Resource, &r.ResourceID, &r.Action,
			&r.StatusCode, &r.Result, &r.DurationMs, &r.RequestID, &r.UserAgent, &detail, &r.CreatedAt, &prev, &hash); err != nil {
			return nil, err
		}
		r.Detail = detail
		if prev != nil {
			r.PrevHash = *prev
		}
		if hash != nil {
			r.Hash = *hash
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// exportStats summarises a written export.
type exportStats struct {
	Rows     int
	FirstID  int64
	LastID   int64
	LastHash string
}

// writeExport streams [first,last] through w. On a source/write error after
// rows were produced the interruption marker is appended (best effort).
func writeExport(ctx context.Context, src chainSource, first, last int64, w rowWriter) (exportStats, error) {
	var st exportStats
	err := src.StreamChainRows(ctx, first, last, func(r ChainRow) error {
		if err := w.Write(r); err != nil {
			return err
		}
		if st.Rows == 0 {
			st.FirstID = r.ID
		}
		st.Rows++
		st.LastID, st.LastHash = r.ID, r.Hash
		return nil
	})
	if err != nil {
		_ = w.Fail()
		_ = w.Flush()
		return st, err
	}
	return st, w.Flush()
}

// ---------------------------------------------------------------- handlers

// exporter serves GET /audit/export and POST /audit/archive.
type exporter struct {
	src        chainSource
	audit      Writer         // explicit audit entry for the (GET) export
	archive    objstore.Store // nil: local directory archiveDir
	archiveDir string
	tmpDir     string
	now        func() time.Time
	busy       chan struct{} // one archive at a time
}

func newExporter(s *PgStore) *exporter {
	return &exporter{src: s, audit: s, archive: s.Archive, archiveDir: s.ArchiveDir, now: time.Now, busy: make(chan struct{}, 1)}
}

func (x *exporter) store() objstore.Store {
	if x.archive != nil {
		return x.archive
	}
	dir := x.archiveDir
	if dir == "" {
		dir = DefaultArchiveDir
	}
	return &objstore.Local{Dir: dir}
}

func (x *exporter) register(r gin.IRoutes) {
	r.GET("/audit/export", rbac.RequirePerm(PermAuditExport), x.handleExport)
	r.POST("/audit/archive", rbac.RequirePerm(PermAuditManage), x.handleArchive)
}

func parseTimeValue(name, v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, httpx.Validation(name + " is required")
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(n, 0).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, httpx.Validation("invalid time for " + name + " (RFC3339 or unix seconds)")
	}
	return t.UTC(), nil
}

// parseRange validates a required [from, to) window of at most exportMaxRange.
func parseRange(fromS, toS string) (time.Time, time.Time, error) {
	from, err := parseTimeValue("from", fromS)
	if err != nil {
		return from, from, err
	}
	to, err := parseTimeValue("to", toS)
	if err != nil {
		return from, to, err
	}
	if !to.After(from) {
		return from, to, httpx.Validation("to must be after from")
	}
	if to.Sub(from) > exportMaxRange {
		return from, to, httpx.Validation("range must not exceed 366 days")
	}
	return from, to, nil
}

func rangeName(from, to time.Time) string {
	const l = "20060102T150405Z"
	return "audit-" + from.UTC().Format(l) + "-" + to.UTC().Format(l)
}

func (x *exporter) handleExport(c *gin.Context) {
	start := x.now()
	from, to, err := parseRange(c.Query("from"), c.Query("to"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	format := strings.ToLower(strings.TrimSpace(c.DefaultQuery("format", FormatJSONL)))
	if format != FormatJSONL && format != FormatCSV {
		httpx.Fail(c, httpx.Validation("format must be jsonl or csv"))
		return
	}
	ctx := c.Request.Context()
	first, last, ok, err := x.src.ExportIDRange(ctx, from, to)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	ctype := "application/x-ndjson; charset=utf-8"
	if format == FormatCSV {
		ctype = "text/csv; charset=utf-8"
	}
	h := c.Writer.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Disposition", `attachment; filename="`+rangeName(from, to)+"."+format+`"`)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	c.Status(200)
	w := newRowWriter(format, c.Writer)
	var st exportStats
	var streamErr error
	if ok {
		st, streamErr = writeExport(ctx, x.src, first, last, w)
	} else {
		streamErr = w.Flush()
	}
	if streamErr != nil {
		_ = c.Error(streamErr)
	}
	x.recordExport(c, start, from, to, format, st, streamErr)
}

// recordExport writes the audit entry for an export (GET requests are not
// recorded by the middleware). It survives the client disconnecting.
func (x *exporter) recordExport(c *gin.Context, start, from, to time.Time, format string, st exportStats, streamErr error) {
	if x.audit == nil {
		return
	}
	e := FromGin(c, start)
	e.Resource, e.ResourceID, e.Action = "audit", "export", "export"
	detail := map[string]any{
		"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339), "format": format,
		"rows": st.Rows, "first_id": st.FirstID, "last_id": st.LastID, "complete": streamErr == nil,
	}
	if streamErr != nil {
		e.Result = ResultFailure
		detail["error"] = trunc(streamErr.Error(), 500)
	}
	e.Detail, _ = json.Marshal(map[string]any{"data": detail})
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 10*time.Second)
	defer cancel()
	if err := x.audit.WriteBatch(ctx, []Entry{e}); err != nil {
		_ = c.Error(fmt.Errorf("audit export: record: %w", err))
	}
}

// ArchiveRequest is the body of POST /audit/archive.
type ArchiveRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// ArchiveResult is the response of POST /audit/archive.
type ArchiveResult struct {
	File     string `json:"file"`
	Storage  string `json:"storage"` // s3 | local
	Location string `json:"location"`
	Rows     int    `json:"rows"`
	FirstID  int64  `json:"first_id"`
	LastID   int64  `json:"last_id"`
	LastHash string `json:"last_hash"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	From     string `json:"from"`
	To       string `json:"to"`
}

func (x *exporter) handleArchive(c *gin.Context) {
	SetResource(c, "audit", "archive")
	SetAction(c, "archive")
	var in ArchiveRequest
	if !httpx.Bind(c, &in) {
		return
	}
	from, to, err := parseRange(in.From, in.To)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	select {
	case x.busy <- struct{}{}:
		defer func() { <-x.busy }()
	default:
		httpx.Fail(c, httpx.Conflict("another audit archive is running"))
		return
	}
	res, err := x.archiveRange(c.Request.Context(), from, to)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	SetDetail(c, res)
	httpx.OK(c, res)
}

// archiveRange writes [from,to) as gzip JSONL to a temp file (object stores
// need the size up front), then puts it under audit/<name>.jsonl.gz.
func (x *exporter) archiveRange(ctx context.Context, from, to time.Time) (*ArchiveResult, error) {
	first, last, ok, err := x.src.ExportIDRange(ctx, from, to)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, httpx.Validation("no audit records in this time range")
	}
	tmp, err := os.CreateTemp(x.tmpDir, "audit-archive-*.jsonl.gz")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	sum := sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(tmp, sum))
	st, err := writeExport(ctx, x.src, first, last, newJSONLWriter(gz))
	if err != nil {
		return nil, fmt.Errorf("audit archive: export: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	size, err := tmp.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%s-%d-%d-%d.jsonl.gz", rangeName(from, to), st.FirstID, st.LastID, x.now().Unix())
	key := archiveKeyPrefix + name
	if !objstore.ValidKey(key) {
		return nil, fmt.Errorf("audit archive: invalid key %q", key)
	}
	dst := x.store()
	loc, err := dst.Put(ctx, key, tmp, size)
	if err != nil {
		return nil, fmt.Errorf("audit archive: store: %w", err)
	}
	if dst.Kind() == "local" { // report as <archive dir>/<key>
		loc = strings.TrimRight(dst.Describe(), "/") + "/" + key
	}
	return &ArchiveResult{
		File: name, Storage: dst.Kind(), Location: loc, Rows: st.Rows, FirstID: st.FirstID, LastID: st.LastID,
		LastHash: st.LastHash, Size: size, SHA256: hex.EncodeToString(sum.Sum(nil)),
		From: from.Format(time.RFC3339), To: to.Format(time.RFC3339),
	}, nil
}

var _ chainSource = (*PgStore)(nil)
