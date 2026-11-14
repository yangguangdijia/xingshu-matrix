package audit

import (
	"encoding/json"
	"testing"
	"time"
)

func sampleRow(id int64, detail string) ChainRow {
	uid := int64(7)
	return ChainRow{
		ID: id, UserID: &uid, Username: "alice", IP: "10.0.0.1", Method: "POST",
		Path: "/api/v1/hosts", Resource: "host", ResourceID: "42", Action: "create",
		StatusCode: 200, Result: ResultSuccess, DurationMs: 12, RequestID: "req-1",
		UserAgent: "curl/8", Detail: json.RawMessage(detail),
		CreatedAt: time.Date(2026, 11, 14, 10, 0, 0, 123456789, time.FixedZone("CST", 8*3600)),
	}
}

func buildChain(n int) []ChainRow {
	rows := make([]ChainRow, n)
	prev := GenesisHash
	for i := range rows {
		r := sampleRow(int64(i+1), `{"a":1,"b":"x"}`)
		r.PrevHash = prev
		r.Hash = CanonicalHash(prev, r)
		prev = r.Hash
		rows[i] = r
	}
	return rows
}

func TestCanonicalStableAcrossKeyOrder(t *testing.T) {
	a := CanonicalHash(GenesisHash, sampleRow(1, `{"b":{"y":2,"x":[1,"<&>"]},"a":true}`))
	b := CanonicalHash(GenesisHash, sampleRow(1, ` { "a" : true , "b" : { "x" : [ 1 , "<&>" ] , "y" : 2 } } `))
	if a != b {
		t.Fatalf("hash depends on key order/whitespace: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("want 64 hex chars, got %d", len(a))
	}
	if c := CanonicalHash("other", sampleRow(1, `{"a":true}`)); c == a {
		t.Fatal("prev hash not bound")
	}
	// time zone and sub-microsecond digits do not matter; microseconds do
	r1, r2 := sampleRow(1, ""), sampleRow(1, "")
	r2.CreatedAt = r1.CreatedAt.UTC().Add(-789 * time.Nanosecond)
	if CanonicalHash("", r1) != CanonicalHash("", r2) {
		t.Fatal("time not normalised to UTC microseconds")
	}
	r2.CreatedAt = r2.CreatedAt.Add(time.Microsecond)
	if CanonicalHash("", r1) == CanonicalHash("", r2) {
		t.Fatal("microsecond change not detected")
	}
	// nil user vs set user differ; empty detail equals JSON null
	r3 := sampleRow(1, "null")
	r4 := sampleRow(1, "")
	if CanonicalHash("", r3) != CanonicalHash("", r4) {
		t.Fatal("empty detail should equal null")
	}
	r4.UserID = nil
	if CanonicalHash("", r3) == CanonicalHash("", r4) {
		t.Fatal("user_id nil not distinguished")
	}
}

func TestCanonicalNumbersAndText(t *testing.T) {
	cases := map[string]string{"1.50": "1.5", "1.5e0": "1.5", "100": "100", "-0.0": "-0", "1e21": "1e+21", "12345678901234567890": "12345678901234567890"}
	for in, want := range cases {
		if got := canonicalNumber(in); got != want {
			t.Errorf("canonicalNumber(%s)=%s want %s", in, got, want)
		}
	}
	if CanonicalHash("", sampleRow(1, `{"v":1.50}`)) != CanonicalHash("", sampleRow(1, `{"v":1.5}`)) {
		t.Fatal("float forms should match")
	}
	// large integers keep full precision (no float64 rounding)
	if CanonicalHash("", sampleRow(1, `{"v":9007199254740993}`)) == CanonicalHash("", sampleRow(1, `{"v":9007199254740992}`)) {
		t.Fatal("integer literal should be kept verbatim")
	}
	a, b := sampleRow(1, ""), sampleRow(1, "")
	a.Username = "bob\x00"
	b.Username = "bob"
	if CanonicalHash("", a) != CanonicalHash("", b) {
		t.Fatal("NUL not stripped")
	}
	a.Username = "bad\xffutf8"
	_ = CanonicalHash("", a) // must not panic
}

func TestVerifyRows(t *testing.T) {
	rows := buildChain(5)
	if n, at, k := VerifyRows(GenesisHash, rows); n != 5 || at != 0 || k != "" {
		t.Fatalf("clean chain: %d %d %q", n, at, k)
	}

	tampered := buildChain(5)
	tampered[2].Username = "mallory"
	if n, at, k := VerifyRows(GenesisHash, tampered); n != 2 || at != 3 || k != BreakHashMismatch {
		t.Fatalf("tamper: %d %d %q", n, at, k)
	}

	detail := buildChain(5)
	detail[1].Detail = json.RawMessage(`{"a":2,"b":"x"}`)
	if _, at, k := VerifyRows(GenesisHash, detail); at != 2 || k != BreakHashMismatch {
		t.Fatalf("detail tamper: %d %q", at, k)
	}

	deleted := append(append([]ChainRow{}, rows[:2]...), rows[3:]...)
	if n, at, k := VerifyRows(GenesisHash, deleted); n != 2 || at != 4 || k != BreakPrevMismatch {
		t.Fatalf("delete: %d %d %q", n, at, k)
	}

	unchained := buildChain(5)
	unchained[4].Hash, unchained[4].PrevHash = "", ""
	if n, at, k := VerifyRows(GenesisHash, unchained); n != 4 || at != 5 || k != BreakUnchained {
		t.Fatalf("unchained: %d %d %q", n, at, k)
	}

	if _, at, k := VerifyRows("wrong-anchor", rows); at != 1 || k != BreakPrevMismatch {
		t.Fatalf("anchor: %d %q", at, k)
	}
}
