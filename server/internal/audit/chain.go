package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ChainVersion prefixes every canonical payload so the format can evolve.
const ChainVersion = "xsaudit/v1"

// GenesisHash is the default anchor of a fresh chain segment (64 zeros).
var GenesisHash = strings.Repeat("0", 64)

// Verification break kinds returned by VerifyRows.
const (
	BreakHashMismatch = "hash_mismatch"
	BreakPrevMismatch = "prev_mismatch"
	BreakUnchained    = "unchained"
)

// ChainRow mirrors every column of audit_logs, including the chain columns.
type ChainRow struct {
	ID         int64
	UserID     *int64
	Username   string
	IP         string
	Method     string
	Path       string
	Resource   string
	ResourceID string
	Action     string
	StatusCode int
	Result     string
	DurationMs int
	RequestID  string
	UserAgent  string
	Detail     json.RawMessage
	CreatedAt  time.Time
	PrevHash   string
	Hash       string
}

// CanonicalHash returns the hex SHA-256 of the canonical form of e chained to
// prevHash. e.PrevHash and e.Hash are ignored; prevHash is used instead.
// Payload: "xsaudit/v1\n" + JSON array in fixed column order.
func CanonicalHash(prevHash string, e ChainRow) string {
	var b bytes.Buffer
	b.WriteString(ChainVersion)
	b.WriteByte('\n')
	b.WriteByte('[')
	b.WriteString(strconv.FormatInt(e.ID, 10))
	for _, s := range []string{prevHash, e.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000Z07:00")} {
		b.WriteByte(',')
		writeString(&b, s)
	}
	b.WriteByte(',')
	if e.UserID == nil {
		b.WriteString("null")
	} else {
		b.WriteString(strconv.FormatInt(*e.UserID, 10))
	}
	for _, s := range []string{e.Username, e.IP, e.Method, e.Path, e.Resource, e.ResourceID, e.Action} {
		b.WriteByte(',')
		writeString(&b, s)
	}
	b.WriteByte(',')
	b.WriteString(strconv.Itoa(e.StatusCode))
	b.WriteByte(',')
	writeString(&b, e.Result)
	b.WriteByte(',')
	b.WriteString(strconv.Itoa(e.DurationMs))
	for _, s := range []string{e.RequestID, e.UserAgent} {
		b.WriteByte(',')
		writeString(&b, s)
	}
	b.WriteByte(',')
	writeDetail(&b, e.Detail)
	b.WriteByte(']')
	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:])
}

// VerifyRows walks rows (ascending id) from anchor. checked counts rows that
// verified; on the first break it returns that row's id and the break kind.
// A deleted row surfaces as prev_mismatch on its successor.
func VerifyRows(anchor string, rows []ChainRow) (checked int, breakAt int64, kind string) {
	prev := anchor
	for _, r := range rows {
		if r.Hash == "" {
			return checked, r.ID, BreakUnchained
		}
		if r.PrevHash != prev {
			return checked, r.ID, BreakPrevMismatch
		}
		if CanonicalHash(prev, r) != r.Hash {
			return checked, r.ID, BreakHashMismatch
		}
		prev = r.Hash
		checked++
	}
	return checked, 0, ""
}

func cleanText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", "")
}

func writeString(b *bytes.Buffer, s string) {
	enc := json.NewEncoder(b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(cleanText(s)) // strings never fail
	b.Truncate(b.Len() - 1)      // drop Encoder's trailing newline
}

func writeDetail(b *bytes.Buffer, raw json.RawMessage) {
	if len(bytes.TrimSpace(raw)) == 0 {
		b.WriteString("null")
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		writeString(b, string(raw)) // not valid JSON: hash it as text
		return
	}
	writeValue(b, v)
}

func writeValue(b *bytes.Buffer, v any) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case string:
		writeString(b, t)
	case json.Number:
		b.WriteString(canonicalNumber(string(t)))
	case []any:
		b.WriteByte('[')
		for i, x := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeValue(b, x)
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k)
			b.WriteByte(':')
			writeValue(b, t[k])
		}
		b.WriteByte('}')
	}
}

// canonicalNumber keeps integer literals verbatim; anything else (fraction or
// exponent) is reformatted with FormatFloat('g', -1).
func canonicalNumber(s string) string {
	if !strings.ContainsAny(s, ".eE") {
		return s
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
