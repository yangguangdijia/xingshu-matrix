package audit

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeRow assigns vals to Scan destinations positionally (nil -> zero value).
type fakeRow struct {
	vals []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.vals) {
		return errors.New("fakeRow: column count mismatch")
	}
	for i, d := range dest {
		dv := reflect.ValueOf(d).Elem()
		if r.vals[i] == nil {
			dv.Set(reflect.Zero(dv.Type()))
			continue
		}
		dv.Set(reflect.ValueOf(r.vals[i]))
	}
	return nil
}

// fakeTx simulates audit_logs + the live segment for the chain writer.
type fakeTx struct {
	pgx.Tx // unused methods panic
	seg    struct {
		startID, headID *int64
		anchor, head    string
		count           int64
	}
	rows   map[int64]*ChainRow
	nextID int64
	calls  []string
}

func newFakeTx() *fakeTx {
	f := &fakeTx{rows: map[int64]*ChainRow{}, nextID: 100}
	f.seg.anchor = GenesisHash
	return f
}

func (f *fakeTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case sql == lockLiveSQL:
		f.calls = append(f.calls, "lock")
		var start, head any
		if f.seg.startID != nil {
			start = f.seg.startID
		}
		if f.seg.headID != nil {
			head = f.seg.headID
		}
		return fakeRow{vals: []any{int64(1), start, f.seg.anchor, head, f.seg.head}}
	case strings.HasPrefix(sql, "INSERT INTO audit_logs"):
		f.calls = append(f.calls, "insert")
		f.nextID++
		created := time.Date(2026, 11, 14, 2, 0, 0, 0, time.UTC)
		if args[14] != nil {
			created = args[14].(time.Time).Truncate(time.Microsecond)
		}
		var detail []byte
		if args[13] != nil {
			detail = []byte(args[13].(string))
		}
		r := &ChainRow{ID: f.nextID, UserID: args[0].(*int64), Username: args[1].(string), IP: args[2].(string),
			Method: args[3].(string), Path: args[4].(string), Resource: args[5].(string), ResourceID: args[6].(string),
			Action: args[7].(string), StatusCode: args[8].(int), Result: args[9].(string), DurationMs: args[10].(int),
			RequestID: args[11].(string), UserAgent: args[12].(string), Detail: detail, CreatedAt: created}
		f.rows[r.ID] = r
		var uid any
		if r.UserID != nil {
			uid = r.UserID
		}
		return fakeRow{vals: []any{r.ID, uid, r.Username, r.IP, r.Method, r.Path, r.Resource, r.ResourceID, r.Action,
			r.StatusCode, r.Result, r.DurationMs, r.RequestID, r.UserAgent, detail, r.CreatedAt}}
	}
	return fakeRow{err: errors.New("unexpected query: " + sql)}
}

func (f *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.HasPrefix(sql, "UPDATE audit_logs SET prev_hash"):
		f.calls = append(f.calls, "set_hash")
		r := f.rows[args[0].(int64)]
		r.PrevHash, r.Hash = args[1].(string), args[2].(string)
	case strings.HasPrefix(sql, "UPDATE audit_chain_segments"):
		f.calls = append(f.calls, "advance")
		f.seg.headID, f.seg.head = args[1].(*int64), args[2].(string)
		f.seg.count += args[3].(int64)
		if f.seg.startID == nil {
			f.seg.startID = args[4].(*int64)
		}
	default:
		return pgconn.CommandTag{}, errors.New("unexpected exec: " + sql)
	}
	return pgconn.CommandTag{}, nil
}

func (f *fakeTx) ordered() []ChainRow {
	var out []ChainRow
	for id := int64(101); id <= f.nextID; id++ {
		out = append(out, *f.rows[id])
	}
	return out
}

func testEntry(action string) Entry {
	uid := int64(3)
	return Entry{UserID: &uid, Username: "bob", Method: "POST", Path: "/api/v1/x", Resource: "x",
		Action: action, StatusCode: 200, Detail: json.RawMessage(`{"k":[1,2.50]}`)}
}

func TestWriteChainedLinksFromGenesisAndAdvancesHead(t *testing.T) {
	ctx := context.Background()
	f := newFakeTx()
	if _, err := writeChainedBatch(ctx, f, []Entry{testEntry("a"), testEntry("b")}); err != nil {
		t.Fatal(err)
	}
	want := []string{"lock", "insert", "set_hash", "insert", "set_hash", "advance"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("call order %v, want %v", f.calls, want)
	}
	id, err := WriteChained(ctx, f, Entry{Action: "c", CreatedAt: time.Now()})
	if err != nil || id != 103 {
		t.Fatalf("WriteChained id=%d err=%v", id, err)
	}
	rows := f.ordered()
	if rows[0].PrevHash != GenesisHash {
		t.Fatalf("first row must chain to genesis, got %q", rows[0].PrevHash)
	}
	if rows[2].Result != ResultSuccess {
		t.Fatalf("empty result should default to success")
	}
	if n, at, kind := VerifyRows(GenesisHash, rows); n != 3 || kind != "" {
		t.Fatalf("chain does not verify: n=%d at=%d kind=%s", n, at, kind)
	}
	if f.seg.startID == nil || *f.seg.startID != 101 {
		t.Fatalf("start_id should be first chained id 101, got %v", f.seg.startID)
	}
	if *f.seg.headID != 103 || f.seg.head != rows[2].Hash || f.seg.count != 3 {
		t.Fatalf("segment head not advanced: id=%d hash=%s count=%d", *f.seg.headID, f.seg.head, f.seg.count)
	}
}

func TestChainHeadPrevFallbacks(t *testing.T) {
	if p := (&chainHead{}).prev(); p != GenesisHash {
		t.Fatalf("empty head/anchor must fall back to GenesisHash, got %s", p)
	}
	if p := (&chainHead{anchor: "aa"}).prev(); p != "aa" {
		t.Fatalf("empty head should use anchor, got %s", p)
	}
	if p := (&chainHead{anchor: "aa", head: "bb"}).prev(); p != "bb" {
		t.Fatalf("head must win, got %s", p)
	}
	sid := int64(5)
	h := &chainHead{startID: &sid}
	h.link(ChainRow{ID: 9})
	if *h.startID != 5 {
		t.Fatalf("existing start_id must not move")
	}
}

func TestChainVerifierAcrossChunks(t *testing.T) {
	rows := buildChain(10)
	v := &chainVerifier{prev: GenesisHash}
	for i := 0; i < len(rows); i += 3 {
		end := min(i+3, len(rows))
		if !v.feed(rows[i:end]) {
			t.Fatalf("unexpected break at %d (%s)", v.breakAt, v.kind)
		}
	}
	if v.checked != 10 || v.lastID != 10 {
		t.Fatalf("checked=%d lastID=%d", v.checked, v.lastID)
	}

	rows[7].Username = "mallory" // tamper in the third chunk
	v = &chainVerifier{prev: GenesisHash}
	for i := 0; i < len(rows) && v.feed(rows[i:min(i+3, len(rows))]); i += 3 {
	}
	if v.kind != BreakHashMismatch || v.breakAt != 8 || v.checked != 7 || v.lastID != 7 {
		t.Fatalf("got kind=%s at=%d checked=%d last=%d", v.kind, v.breakAt, v.checked, v.lastID)
	}
}
