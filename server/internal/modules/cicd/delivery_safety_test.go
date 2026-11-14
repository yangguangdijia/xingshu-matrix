package cicd

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- scan gate

func scanVulns() []ScanVuln {
	return []ScanVuln{
		{ID: "CVE-2024-0001", Package: "openssl", Installed: "1.0", Fixed: "1.1", Severity: "CRITICAL"},
		{ID: "CVE-2024-0002", Package: "zlib", Installed: "1.2", Fixed: "", Severity: "HIGH"},
		{ID: "CVE-2024-0003", Package: "curl", Installed: "7.0", Fixed: "7.1", Severity: "medium"},
		// duplicate of the first finding (same id + package + version)
		{ID: "cve-2024-0001", Package: "openssl", Installed: "1.0", Fixed: "1.1", Severity: "CRITICAL"},
	}
}

func normalizedPolicy(t *testing.T, p ScanPolicy) ScanPolicy {
	t.Helper()
	if err := p.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return p
}

func TestScanGateBlocksAboveThreshold(t *testing.T) {
	pol := normalizedPolicy(t, ScanPolicy{}) // default CRITICAL,HIGH
	sum, passed, top := evaluateScan(scanVulns(), pol)
	if passed {
		t.Fatal("gate passed with CRITICAL/HIGH findings")
	}
	if sum.Total != 3 || sum.Critical != 1 || sum.High != 1 || sum.Medium != 1 || sum.Blocking != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	if len(top) != 3 || !top[0].Blocking || !top[1].Blocking || top[2].Blocking {
		t.Fatalf("blocking findings must sort first: %+v", top)
	}
	msg := gateMessage(sum, pol)
	if !strings.Contains(msg, "CRITICAL 1") || !strings.Contains(msg, "HIGH 1") || strings.Contains(msg, "MEDIUM") {
		t.Fatalf("gate message = %q", msg)
	}
}

func TestScanGatePassesWithinThreshold(t *testing.T) {
	cases := map[string]ScanPolicy{
		"only critical blocks, critical ignored": {BlockSeverities: []string{"critical"}, IgnoreCVEs: []string{"cve-2024-0001"}},
		"ignore unfixed + ignored critical":      {IgnoreCVEs: []string{"CVE-2024-0001"}, IgnoreUnfixed: true},
		"report only":                            {ReportOnly: true},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			pol := normalizedPolicy(t, p)
			sum, passed, _ := evaluateScan(scanVulns(), pol)
			if !passed || sum.Blocking != 0 {
				t.Fatalf("gate blocked: %+v", sum)
			}
		})
	}
	// Severity below the gate never blocks.
	pol := normalizedPolicy(t, ScanPolicy{})
	if _, passed, _ := evaluateScan([]ScanVuln{{ID: "CVE-1", Package: "p", Severity: "LOW"}}, pol); !passed {
		t.Fatal("LOW finding blocked a CRITICAL,HIGH gate")
	}
	if _, passed, _ := evaluateScan(nil, pol); !passed {
		t.Fatal("empty scan blocked")
	}
}

func TestScanPolicyNormalizeDefaults(t *testing.T) {
	p := normalizedPolicy(t, ScanPolicy{BlockSeverities: []string{"high", "CRITICAL", "high"}, TopN: 999})
	if p.Source != scanSourceTrivy || strings.Join(p.BlockSeverities, ",") != "CRITICAL,HIGH" || p.TopN != maxScanTopN {
		t.Fatalf("normalized = %+v", p)
	}
	ro := normalizedPolicy(t, ScanPolicy{ReportOnly: true})
	if len(ro.BlockSeverities) != 0 || ro.blocks("CRITICAL") {
		t.Fatalf("report-only policy blocks: %+v", ro)
	}
	for _, bad := range []ScanPolicy{{Source: "clair"}, {BlockSeverities: []string{"SEVERE"}}, {IgnoreCVEs: []string{"bad id!"}}} {
		if err := bad.normalize(); err == nil {
			t.Fatalf("normalize(%+v) accepted", bad)
		}
	}
}

// A scanner that is not available fails the stage instead of passing it.
func TestScanScannerMissing(t *testing.T) {
	m := &Module{}
	app := &Application{ImageRepo: "harbor.example.com/team/web"}
	run := &Run{ID: 1, ImageTag: "abc"}
	logf := func(string, ...any) {}

	// trivy: no runner cluster
	stages := []Stage{{Name: "build", Type: "build"}, {Name: "scan", Type: "scan", Scan: &ScanPolicy{}}}
	err := m.scanStage(context.Background(), nil, run, app, stages, 1, "job", logf)
	if err == nil || !strings.Contains(err.Error(), "runner cluster is not available") {
		t.Fatalf("trivy without runner: %v", err)
	}
	// harbor: no registry credential (rejected at save time and at run time)
	harbor := []Stage{{Name: "build", Type: "build"}, {Name: "scan", Type: "scan", Scan: &ScanPolicy{Source: "harbor"}}}
	if err := validateScanStage(&harbor[1], app, harbor, 1); err == nil || !strings.Contains(err.Error(), "registry credential") {
		t.Fatalf("harbor without credential validated: %v", err)
	}
	if _, err := m.harborScan(context.Background(), "harbor.example.com/team/web:abc", 0, ScanPolicy{Source: "harbor"}, time.Minute, logf); err == nil {
		t.Fatal("harbor scan without credential succeeded")
	}
	// nothing to scan
	none := []Stage{{Name: "scan", Type: "scan", Scan: &ScanPolicy{}}}
	if err := m.scanStage(context.Background(), nil, run, &Application{}, none, 0, "job", logf); err == nil || !strings.Contains(err.Error(), "no image") {
		t.Fatalf("scan without image: %v", err)
	}
	if err := validateScanStage(&none[0], &Application{}, none, 0); err == nil {
		t.Fatal("scan stage without image validated")
	}
}

// ---------------------------------------------------------------- delivery dedup

func TestDeliveryDedupKey(t *testing.T) {
	hdr := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}
	cases := []struct {
		name         string
		h            http.Header
		provider, id string
	}{
		{"github", hdr("X-GitHub-Event", "push", "X-GitHub-Delivery", "d-1"), "github", "d-1"},
		{"gitee", hdr("X-Gitee-Event", "Push Hook", "X-Gitee-Delivery", "g-1"), "gitee", "g-1"},
		{"gitea before github", hdr("X-Gitea-Delivery", "t-1", "X-GitHub-Delivery", "t-1"), "gitea", "t-1"},
		{"gogs", hdr("X-Gogs-Delivery", "o-1"), "gogs", "o-1"},
		{"gitlab idempotency key", hdr("X-Gitlab-Event", "Push Hook", "Idempotency-Key", "k-1", "X-Gitlab-Event-UUID", "u-1"), "gitlab", "k-1"},
		{"gitlab uuid fallback", hdr("X-Gitlab-Token", "s", "X-Gitlab-Event-UUID", "u-1"), "gitlab", "u-1"},
		{"gitlab none", hdr("X-Gitlab-Event", "Push Hook"), "gitlab", ""},
		{"no id", hdr("X-GitHub-Event", "push"), "", ""},
		{"too long", hdr("X-GitHub-Delivery", strings.Repeat("a", 129)), "", ""},
		{"control chars", hdr("X-GitHub-Delivery", "a b"), "", ""},
	}
	for _, c := range cases {
		p, id := deliveryKey(c.h)
		if p != c.provider || id != c.id {
			t.Errorf("%s: got %q/%q want %q/%q", c.name, p, id, c.provider, c.id)
		}
	}
}

// A redelivery carries the same delivery id, so it maps to the same
// (provider, delivery_id) key the unique index de-duplicates on; a new
// delivery gets a different key.
func TestDeliveryDedupRedeliverySameKey(t *testing.T) {
	first := http.Header{}
	first.Set("X-Gitee-Event", "Push Hook")
	first.Set("X-Gitee-Delivery", "abc-123")
	retry := first.Clone()
	p1, id1 := deliveryKey(first)
	p2, id2 := deliveryKey(retry)
	if p1 != p2 || id1 != id2 || id1 == "" {
		t.Fatalf("redelivery key differs: %s/%s vs %s/%s", p1, id1, p2, id2)
	}
	other := first.Clone()
	other.Set("X-Gitee-Delivery", "abc-124")
	if _, id3 := deliveryKey(other); id3 == id1 {
		t.Fatal("distinct deliveries share a key")
	}
}

// ---------------------------------------------------------------- Gitee signature

func giteeSign(secret string, ms int64) (ts, token string) {
	ts = strconv.FormatInt(ms, 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	return ts, base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestGiteeSignature(t *testing.T) {
	now := time.UnixMilli(1_760_000_000_000)
	ts, tok := giteeSign("s3cret", now.UnixMilli())

	if !verifyGiteeSignature(tok, ts, "s3cret", now) {
		t.Fatal("valid signature rejected")
	}
	if !verifyGiteeSignature(url.QueryEscape(tok), ts, "s3cret", now) {
		t.Fatal("URL-encoded signature rejected")
	}
	if verifyGiteeSignature(tok, ts, "other", now) {
		t.Fatal("signature accepted with the wrong secret")
	}
	if verifyGiteeSignature("bm90LWEtc2lnbmF0dXJl", ts, "s3cret", now) {
		t.Fatal("wrong signature accepted")
	}
	if verifyGiteeSignature(tok, "not-a-number", "s3cret", now) {
		t.Fatal("non-numeric timestamp accepted")
	}
	// expired / future timestamps beyond the allowed skew
	oldTS, oldTok := giteeSign("s3cret", now.Add(-giteeSignatureSkew-time.Second).UnixMilli())
	if verifyGiteeSignature(oldTok, oldTS, "s3cret", now) {
		t.Fatal("expired signature accepted")
	}
	futTS, futTok := giteeSign("s3cret", now.Add(giteeSignatureSkew+time.Second).UnixMilli())
	if verifyGiteeSignature(futTok, futTS, "s3cret", now) {
		t.Fatal("future signature accepted")
	}
	edgeTS, edgeTok := giteeSign("s3cret", now.Add(-giteeSignatureSkew+time.Second).UnixMilli())
	if !verifyGiteeSignature(edgeTok, edgeTS, "s3cret", now) {
		t.Fatal("signature inside the skew window rejected")
	}
}

func TestGiteeVerifyWebhook(t *testing.T) {
	now := time.UnixMilli(1_760_000_000_000)
	ts, tok := giteeSign("s3cret", now.UnixMilli())
	h := http.Header{}
	h.Set("X-Gitee-Token", tok)
	h.Set("X-Gitee-Timestamp", ts)
	if p, err := verifyWebhookAt(h, nil, "s3cret", now); err != nil || p != "gitee" {
		t.Fatalf("signed: %q %v", p, err)
	}
	if _, err := verifyWebhookAt(h, nil, "s3cret", now.Add(2*giteeSignatureSkew)); err == nil {
		t.Fatal("expired signed delivery accepted")
	}
	// With a timestamp the token must be a signature, never the plain password.
	h.Set("X-Gitee-Token", "s3cret")
	if _, err := verifyWebhookAt(h, nil, "s3cret", now); err == nil {
		t.Fatal("plain password accepted as a signature")
	}
	// Without a timestamp the token is the plain WebHook password.
	plain := http.Header{}
	plain.Set("X-Gitee-Token", "s3cret")
	if p, err := verifyWebhookAt(plain, nil, "s3cret", now); err != nil || p != "gitee" {
		t.Fatalf("password: %q %v", p, err)
	}
	plain.Set("X-Gitee-Token", "wrong")
	if _, err := verifyWebhookAt(plain, nil, "s3cret", now); err == nil {
		t.Fatal("wrong password accepted")
	}
}
