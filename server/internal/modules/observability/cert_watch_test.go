package observability

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// selfSigned returns a PEM certificate (and its key) valid until notAfter.
func selfSigned(t *testing.T, cn string, notAfter time.Time) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"星枢测试"}},
		NotBefore:    notAfter.Add(-365 * 24 * time.Hour),
		NotAfter:     notAfter,
		DNSNames:     []string{cn, "www." + cn},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

func TestParseCertPEM(t *testing.T) {
	na := time.Now().Add(40 * 24 * time.Hour).Truncate(time.Second)
	crt, key := selfSigned(t, "pay.example.com", na)
	// key first, then two certs: the first CERTIFICATE block (leaf) wins.
	other, _ := selfSigned(t, "ca.example.com", na.Add(1000*time.Hour))
	c, err := parseCertPEM(append(append(append([]byte{}, key...), crt...), other...))
	if err != nil {
		t.Fatal(err)
	}
	if !c.NotAfter.Equal(na.UTC()) || c.Subject.CommonName != "pay.example.com" {
		t.Fatalf("got %v %s", c.NotAfter, c.Subject.CommonName)
	}
	var ci certInfo
	fillFromCert(&ci, c)
	if ci.NotAfter == nil || ci.DNSNames != "pay.example.com,www.pay.example.com" || !strings.Contains(ci.Subject, "CN=pay.example.com") {
		t.Fatalf("fill: %+v", ci)
	}
	if _, err := parseCertPEM([]byte("not pem")); err == nil {
		t.Fatal("garbage must fail")
	}
	if _, err := parseCertPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1, 2, 3}})); err == nil {
		t.Fatal("bad DER must fail")
	}
	if _, err := parseCertPEM(nil); err == nil {
		t.Fatal("empty must fail")
	}
}

func TestCertDaysAndStatus(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	th := []int{30, 14, 7, 1}
	cases := []struct {
		d      time.Duration
		days   int
		status string
	}{
		{60 * 24 * time.Hour, 60, "ok"},
		{30*24*time.Hour + time.Hour, 30, "warning"},
		{8 * 24 * time.Hour, 8, "warning"},
		{7*24*time.Hour + time.Minute, 7, "critical"},
		{2 * time.Hour, 0, "critical"},
		{-time.Minute, -1, "expired"},
	}
	for _, c := range cases {
		d := certDaysLeft(now.Add(c.d), now)
		if d != c.days || certStatusFor(d, th) != c.status {
			t.Errorf("%v: got %d %s, want %d %s", c.d, d, certStatusFor(d, th), c.days, c.status)
		}
	}
}

func TestNormalizeThresholds(t *testing.T) {
	got, err := normalizeThresholds([]int{7, 30, 1, 14, 7})
	if err != nil || !reflect.DeepEqual(got, []int{30, 14, 7, 1}) {
		t.Fatalf("got %v %v", got, err)
	}
	for _, bad := range [][]int{nil, {0}, {366}, {1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}} {
		if _, err := normalizeThresholds(bad); err == nil {
			t.Errorf("%v must fail", bad)
		}
	}
	in := certWatchConfigInput{IntervalHours: 0, Thresholds: []int{30}}
	if in.validate() == nil {
		t.Fatal("interval 0 must fail")
	}
}

func TestNoticeThresholds(t *testing.T) {
	th := []int{30, 14, 7, 1}
	cases := []struct {
		days    int
		target  int
		crossed []int
		ok      bool
	}{
		{45, 0, nil, false},
		{30, 30, []int{30}, true},
		{20, 30, []int{30}, true},
		{5, 7, []int{30, 14, 7}, true},
		{1, 1, []int{30, 14, 7, 1}, true},
		{0, 1, []int{30, 14, 7, 1}, true},
		{-3, 0, []int{30, 14, 7, 1, 0}, true},
	}
	for _, c := range cases {
		target, crossed, ok := noticeThresholds(c.days, th)
		if ok != c.ok || target != c.target || !reflect.DeepEqual(crossed, c.crossed) {
			t.Errorf("days %d: got %d %v %v", c.days, target, crossed, ok)
		}
	}
}

// memClaimer mirrors dbCertClaimer against an in-memory obs_cert_notices.
type memClaimer struct{ sent map[string]bool }

func (m *memClaimer) key(ci *certInfo, t int) string {
	return fmt.Sprintf("%d|%s|%s|%s|%d|%d", ci.ClusterID, ci.Source, ci.Namespace, ci.Name, ci.NotAfter.Unix(), t)
}

func (m *memClaimer) Claim(_ context.Context, ci *certInfo, target int, crossed []int) (bool, error) {
	if m.sent[m.key(ci, target)] {
		return false, nil
	}
	for _, t := range crossed {
		m.sent[m.key(ci, t)] = true
	}
	return true, nil
}

func TestCertNoticeDedup(t *testing.T) {
	th := []int{30, 14, 7, 1}
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	crt, _ := selfSigned(t, "shop.example.com", start.Add(40*24*time.Hour))
	c, err := parseCertPEM(crt)
	if err != nil {
		t.Fatal(err)
	}
	ci := &certInfo{Source: certSourceSecret, ClusterID: 3, ClusterName: "prod-bj-1", Env: "prod", Namespace: "shop", Name: "shop-tls"}
	fillFromCert(ci, c)
	broken := &certInfo{Source: certSourceDomain, Name: "down.example.com:443", Error: "连接超时"}
	cl := &memClaimer{sent: map[string]bool{}}

	// Scan every 6 hours for 45 days: exactly one notice per threshold, plus one when expired.
	var fired []int
	for h := 0; h <= 45*24; h += 6 {
		now := start.Add(time.Duration(h) * time.Hour)
		ns, err := decideCertNotices(context.Background(), []*certInfo{ci, broken}, th, now, cl)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range ns {
			fired = append(fired, n.Threshold)
		}
	}
	if !reflect.DeepEqual(fired, []int{30, 14, 7, 1, 0}) {
		t.Fatalf("fired %v", fired)
	}

	// First seen at 5 days left: only the 7-day notice, and 30/14 never fire afterwards.
	late := &certInfo{Source: certSourceDomain, Name: "late.example.com:443"}
	fillFromCert(late, c)
	cl2 := &memClaimer{sent: map[string]bool{}}
	now := c.NotAfter.Add(-5*24*time.Hour - time.Hour)
	ns, _ := decideCertNotices(context.Background(), []*certInfo{late}, th, now, cl2)
	if len(ns) != 1 || ns[0].Threshold != 7 || ns[0].DaysLeft != 5 {
		t.Fatalf("late: %+v", ns)
	}
	if ns, _ := decideCertNotices(context.Background(), []*certInfo{late}, th, now.Add(time.Hour), cl2); len(ns) != 0 {
		t.Fatalf("repeat must be deduped: %+v", ns)
	}

	// Renewal (new NotAfter) starts counting again.
	renewed := *late
	na := c.NotAfter.Add(90 * 24 * time.Hour)
	renewed.NotAfter = &na
	if ns, _ := decideCertNotices(context.Background(), []*certInfo{&renewed}, th, na.Add(-10*24*time.Hour), cl2); len(ns) != 1 || ns[0].Threshold != 14 {
		t.Fatalf("renewed: %+v", ns)
	}
}

func TestCertNotifyEventRenders(t *testing.T) {
	na := time.Now().Add(6*24*time.Hour + time.Hour)
	ci := &certInfo{Source: certSourceSecret, ClusterID: 1, ClusterName: "prod-bj-1", Env: "prod", Namespace: "shop", Name: "shop-tls",
		DNSNames: "shop.example.com", NotAfter: &na}
	ev := certNotifyEvent(certNotice{Cert: ci, Threshold: 7, DaysLeft: 6}, "https://m.example.com/#/observe/certificates", time.Now())
	if ev.Kind != "certificate" || ev.Status != "expiring" || ev.Severity != "critical" || !strings.HasPrefix(ev.Key, "certificate:secret:1:shop/shop-tls:") {
		t.Fatalf("event %+v", ev)
	}
	b := builtinTemplates["certificate"]
	title, body, err := renderTemplate(b.TitleTpl, b.BodyTpl, ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(title, "证书即将到期") || !strings.Contains(title, "剩余 6 天") || !strings.Contains(body, "命名空间：shop") || !strings.Contains(body, "域名：shop.example.com") {
		t.Fatalf("render %q / %q", title, body)
	}
	exp := certNotifyEvent(certNotice{Cert: ci, Threshold: 0, DaysLeft: -1}, "", time.Now())
	if exp.Status != "expired" || !strings.Contains(exp.Summary, "已于") {
		t.Fatalf("expired %+v", exp)
	}
	if !validKind("certificate") || eventKinds[len(eventKinds)-1] != "test" {
		t.Fatalf("event kinds %v", eventKinds)
	}
}

func TestCertDomainInput(t *testing.T) {
	ok := []struct {
		in, host string
		port     int
	}{
		{"Example.COM", "example.com", 443},
		{"https://pay.example.com/path?x=1", "pay.example.com", 443},
		{"api.example.com:8443", "api.example.com", 8443},
		{"10.0.0.8", "10.0.0.8", 443},
		{"[::1]:9443", "::1", 9443},
	}
	for _, c := range ok {
		in := certDomainInput{Host: c.in}
		if err := in.validate(); err != nil || in.Host != c.host || in.Port != c.port {
			t.Errorf("%q: %v %q %d", c.in, err, in.Host, in.Port)
		}
	}
	for _, bad := range []certDomainInput{{Host: ""}, {Host: "*.example.com"}, {Host: "bad host"}, {Host: "a..b"},
		{Host: "x.com", Port: 70000}, {Host: "x.com:80", Port: 443}, {Host: "-x.com"}} {
		b := bad
		if b.validate() == nil {
			t.Errorf("%+v must fail", bad)
		}
	}
}

func TestProbeDomainCert(t *testing.T) {
	na := time.Now().Add(20 * 24 * time.Hour).Truncate(time.Second)
	crt, key := selfSigned(t, "localhost", na)
	pair, err := tls.X509KeyPair(crt, key)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	srv.StartTLS()
	defer srv.Close()
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	c, err := probeDomainCert(context.Background(), host, p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.NotAfter.Equal(na.UTC()) {
		t.Fatalf("not after %v want %v", c.NotAfter, na)
	}
	// A closed port fails with a readable message, not a raw error.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().(*net.TCPAddr)
	l.Close()
	if _, err := probeDomainCert(context.Background(), "127.0.0.1", addr.Port); err == nil || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("closed port: %v", err)
	}
}
