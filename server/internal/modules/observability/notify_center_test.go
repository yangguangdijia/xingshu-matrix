package observability

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- templates

func TestRenderBuiltinAlertTemplate(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	a := Alert{ID: 7, DatasourceID: 2, Fingerprint: "fp1", Alertname: "HighCPU", Severity: "critical", Status: "firing", Env: "prod", StartsAt: &start,
		Labels:      map[string]string{"cluster": "prod-bj", "namespace": "pay", "pod": "api-1"},
		Annotations: map[string]string{"summary": "CPU 95%"}}
	ev := alertEvent(a, "https://m.example.com/#/observe/alerts/7")
	b := builtinTemplates["alert"]
	title, body, err := renderTemplate(b.TitleTpl, b.BodyTpl, &ev)
	if err != nil {
		t.Fatal(err)
	}
	if title != "【告警触发】[严重] HighCPU" {
		t.Fatalf("title %q", title)
	}
	for _, want := range []string{"集群：prod-bj", "命名空间：pay", "资源：pod/api-1", "摘要：CPU 95%", "开始时间：2026-01-02 11:04:05", "环境：生产", "查看：https://m.example.com/#/observe/alerts/7"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "\n\n") {
		t.Errorf("blank lines not collapsed:\n%s", body)
	}
	if ev.Key != "alert:2:fp1:firing" {
		t.Errorf("key %q", ev.Key)
	}
	ev.Status = "resolved"
	title, _, _ = renderTemplate(b.TitleTpl, b.BodyTpl, &ev)
	if !strings.HasPrefix(title, "【告警恢复】") {
		t.Errorf("resolved title %q", title)
	}
}

func TestRenderCustomTemplateFuncsAndErrors(t *testing.T) {
	ev := sampleEvent("alert")
	title, body, err := renderTemplate(`{{upper .Severity}}-{{.AlertName | trunc 4}}`, `{{label "pod" .Labels}}|{{default "无" .Description}}|{{.Count}}`, &ev)
	if err != nil {
		t.Fatal(err)
	}
	if title != "CRITICAL-Kube…" || body != "payment-api-7d9f8-x2l4k|无|1" {
		t.Fatalf("got %q / %q", title, body)
	}
	if _, _, err := renderTemplate(`{{.AlertName`, `x`, &ev); err == nil || !strings.Contains(err.Error(), "标题模板语法错误") {
		t.Fatalf("want syntax error, got %v", err)
	}
	if _, _, err := renderTemplate(`x`, `{{.NoSuchField}}`, &ev); err == nil {
		t.Fatal("unknown field must fail at execution")
	}
	if _, _, err := renderTemplate(`x`, `{{range $i := .Labels}}{{printf "%5000s" "a"}}{{end}}{{range $i := .Labels}}{{printf "%9000s" "a"}}{{end}}`, &ev); err == nil {
		t.Fatal("oversized output must fail")
	}
	for _, k := range eventKinds {
		ev := sampleEvent(k)
		b := builtinTemplates[k]
		if _, _, err := renderTemplate(b.TitleTpl, b.BodyTpl, &ev); err != nil {
			t.Errorf("builtin %s: %v", k, err)
		}
	}
	in := templateInput{Name: "x", EventKind: "incident", TitleTpl: "{{.AlertName}}", BodyTpl: "{{.Bogus}}"}
	if in.validate() == nil {
		t.Fatal("template validation must execute against sample data")
	}
}

func TestCustomTemplateFallsBackToBuiltin(t *testing.T) {
	m := &Module{}
	ev := sampleEvent("alert")
	ev.extra = "\nAI 诊断：内存泄漏"
	_, body, err := m.renderFor(context.Background(), &Channel{Templates: map[string]int64{}}, &ev)
	if err != nil || !strings.HasSuffix(body, "AI 诊断：内存泄漏") {
		t.Fatalf("supplement not appended: %v %q", err, body)
	}
}

// ------------------------------------------------------------------ routing

func sub(id int64, mod func(*Subscription)) *Subscription {
	s := &Subscription{ID: id, Name: "s", Enabled: true, EventKinds: []string{"alert"}, ChannelIDs: []int64{1}, MatchLabels: map[string]string{}}
	if mod != nil {
		mod(s)
	}
	_ = s.compile()
	return s
}

func TestSubscriptionMatch(t *testing.T) {
	ev := sampleEvent("alert") // critical, prod, prod-bj-1, namespace payment
	cases := []struct {
		name string
		s    *Subscription
		want bool
	}{
		{"all alerts", sub(1, nil), true},
		{"disabled", sub(1, func(s *Subscription) { s.Enabled = false }), false},
		{"no channels", sub(1, func(s *Subscription) { s.ChannelIDs = nil }), false},
		{"other kind", sub(1, func(s *Subscription) { s.EventKinds = []string{"incident"} }), false},
		{"status firing", sub(1, func(s *Subscription) { s.Statuses = []string{"firing"} }), true},
		{"status resolved only", sub(1, func(s *Subscription) { s.Statuses = []string{"resolved"} }), false},
		{"severity", sub(1, func(s *Subscription) { s.Severities = []string{"critical"} }), true},
		{"severity miss", sub(1, func(s *Subscription) { s.Severities = []string{"warning", "info"} }), false},
		{"env", sub(1, func(s *Subscription) { s.Envs = []string{"PROD"} }), true},
		{"env miss", sub(1, func(s *Subscription) { s.Envs = []string{"test"} }), false},
		{"cluster", sub(1, func(s *Subscription) { s.Clusters = []string{"prod-bj-1"} }), true},
		{"cluster miss", sub(1, func(s *Subscription) { s.Clusters = []string{"prod-sh-1"} }), false},
		{"label exact", sub(1, func(s *Subscription) { s.MatchLabels = map[string]string{"namespace": "payment"} }), true},
		{"label exact miss", sub(1, func(s *Subscription) { s.MatchLabels = map[string]string{"namespace": "order"} }), false},
		{"label regex", sub(1, func(s *Subscription) { s.MatchLabels = map[string]string{"pod": "~payment-.*"} }), true},
		{"label regex anchored", sub(1, func(s *Subscription) { s.MatchLabels = map[string]string{"pod": "~api"} }), false},
		{"label negation", sub(1, func(s *Subscription) { s.MatchLabels = map[string]string{"namespace": "!kube-system"} }), true},
		{"label negation miss", sub(1, func(s *Subscription) { s.MatchLabels = map[string]string{"namespace": "!payment"} }), false},
		{"missing label", sub(1, func(s *Subscription) { s.MatchLabels = map[string]string{"team": "sre"} }), false},
		{"field as label", sub(1, func(s *Subscription) { s.MatchLabels = map[string]string{"cluster": "~prod-.*"} }), true},
	}
	for _, c := range cases {
		if got := c.s.Match(&ev); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestSubscriptionInputValidation(t *testing.T) {
	ok := subscriptionInput{Name: "x", EventKinds: []string{"incident", "pipeline"}, Statuses: []string{"declared", "failed"}, ChannelIDs: []int64{3, 3, 4}}
	s, err := ok.toSub()
	if err != nil || len(s.ChannelIDs) != 2 {
		t.Fatalf("valid input rejected: %v %+v", err, s)
	}
	bad := []subscriptionInput{
		{Name: "x", ChannelIDs: []int64{1}},
		{Name: "x", EventKinds: []string{"alert"}},
		{Name: "x", EventKinds: []string{"alert"}, Statuses: []string{"declared"}, ChannelIDs: []int64{1}},
		{Name: "x", EventKinds: []string{"alert"}, MatchLabels: map[string]string{"a": "~("}, ChannelIDs: []int64{1}},
		{Name: "x", EventKinds: []string{"alert"}, MatchLabels: map[string]string{"bad key": "v"}, ChannelIDs: []int64{1}},
		{Name: "x", EventKinds: []string{"alert"}, DedupMinutes: 5000, ChannelIDs: []int64{1}},
		{Name: "x", EventKinds: []string{"alert"}, Quiet: QuietHours{Enabled: true, Start: "25:00", End: "08:00"}, ChannelIDs: []int64{1}},
		{Name: "x", EventKinds: []string{"test"}, ChannelIDs: []int64{1}},
	}
	for i, in := range bad {
		if _, err := in.toSub(); err == nil {
			t.Errorf("case %d should fail", i)
		}
	}
}

func TestQuietHours(t *testing.T) {
	sh, _ := time.LoadLocation("Asia/Shanghai")
	at := func(d, h, mi int) time.Time { return time.Date(2026, 3, d, h, mi, 0, 0, sh) } // 2026-03-06 is a Friday
	q := QuietHours{Enabled: true, Start: "22:00", End: "08:00", Timezone: "Asia/Shanghai"}
	if !q.active(at(6, 23, 0)) || !q.active(at(7, 7, 59)) || q.active(at(7, 8, 0)) || q.active(at(6, 12, 0)) {
		t.Fatal("overnight window wrong")
	}
	q.Weekdays = []int{1, 2, 3, 4, 5} // weekday nights only
	if !q.active(at(7, 2, 0)) {
		t.Fatal("Saturday 02:00 belongs to Friday night")
	}
	if q.active(at(8, 2, 0)) {
		t.Fatal("Sunday 02:00 belongs to Saturday night, not configured")
	}
	day := QuietHours{Enabled: true, Start: "12:00", End: "13:30", Timezone: "Asia/Shanghai"}
	if !day.active(at(6, 12, 30)) || day.active(at(6, 13, 30)) {
		t.Fatal("daytime window wrong")
	}
	if (QuietHours{Start: "00:00", End: "23:59"}).active(at(6, 1, 0)) {
		t.Fatal("disabled window must be inactive")
	}
}

// -------------------------------------------------------- planning / limits

func TestDedupLimiter(t *testing.T) {
	l := newDedupLimiter()
	now := time.Now()
	if l.hit("k", 10*time.Minute, now) {
		t.Fatal("first send must pass")
	}
	if !l.hit("k", 10*time.Minute, now.Add(9*time.Minute)) {
		t.Fatal("repeat within window must be limited")
	}
	if l.hit("k", 10*time.Minute, now.Add(11*time.Minute)) {
		t.Fatal("after window must pass")
	}
	l.forget("k")
	if l.hit("k", 10*time.Minute, now.Add(12*time.Minute)) {
		t.Fatal("forgotten key must pass")
	}
	if l.hit("z", 0, now) || l.hit("z", 0, now) {
		t.Fatal("window 0 disables limiting")
	}
}

func TestPlanDeliveries(t *testing.T) {
	now := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)
	chans := map[int64]*Channel{
		1: {ID: 1, Name: "feishu", Type: "feishu", Enabled: true},
		2: {ID: 2, Name: "mail", Type: "email", Enabled: true, Severities: []string{"warning"}}, // legacy channel gate
		3: {ID: 3, Name: "slack", Type: "slack", Enabled: true},
	}
	a1, a2 := sampleEvent("alert"), sampleEvent("alert")
	a2.Key = "alert:other"
	inc := sampleEvent("incident")
	subs := []*Subscription{
		sub(10, func(s *Subscription) { s.ChannelIDs = []int64{1, 2} }),
		sub(11, func(s *Subscription) { s.ChannelIDs = []int64{1, 3}; s.Aggregate = true }),
		sub(12, func(s *Subscription) { s.EventKinds = []string{"incident"}; s.ChannelIDs = []int64{2, 99} }),
	}
	plan, sup := planDeliveries([]*NotifyEvent{&a1, &a2, &inc}, subs, chans, now, nil)
	if len(sup) != 0 {
		t.Fatalf("unexpected suppression %+v", sup)
	}
	got := map[string]int{}
	for _, d := range plan {
		got[d.ch.Name] += len(d.events)
		if d.ch.ID == 3 && len(d.events) != 2 {
			t.Errorf("aggregated slack delivery should hold both alerts, has %d", len(d.events))
		}
	}
	// feishu: both alerts once each (sub 10 wins, sub 11 skipped); mail: critical alerts blocked by gate, incident passes; slack: aggregated pair.
	if got["feishu"] != 2 || got["mail"] != 1 || got["slack"] != 2 {
		t.Fatalf("deliveries %v", got)
	}

	quiet := sub(20, func(s *Subscription) {
		s.Quiet = QuietHours{Enabled: true, Start: "00:00", End: "23:59", Timezone: "UTC"}
	})
	plan, sup = planDeliveries([]*NotifyEvent{&a1}, []*Subscription{quiet}, chans, now, nil)
	if len(plan) != 0 || len(sup) != 1 || sup[0].Status != "suppressed" || !strings.Contains(sup[0].Reason, "静默") {
		t.Fatalf("quiet hours not applied: %v %+v", plan, sup)
	}
	quiet.Quiet.AllowCritical = true
	if plan, _ = planDeliveries([]*NotifyEvent{&a1}, []*Subscription{quiet}, chans, now, nil); len(plan) != 1 {
		t.Fatal("critical must pass quiet hours when allowed")
	}

	limited := sub(30, func(s *Subscription) { s.DedupMinutes = 10 })
	l := newDedupLimiter()
	recently := func(s *Subscription, ch *Channel, ev *NotifyEvent) bool {
		return l.hit(limiterKey(s, ch, ev), time.Duration(s.DedupMinutes)*time.Minute, now)
	}
	if plan, _ = planDeliveries([]*NotifyEvent{&a1}, []*Subscription{limited}, chans, now, recently); len(plan) != 1 {
		t.Fatal("first event must be sent")
	}
	plan, sup = planDeliveries([]*NotifyEvent{&a1}, []*Subscription{limited}, chans, now, recently)
	if len(plan) != 0 || len(sup) != 1 || !strings.Contains(sup[0].Reason, "限频") {
		t.Fatalf("rate limit not applied: %v %+v", plan, sup)
	}
	if sup[0].SubscriptionID == nil || *sup[0].SubscriptionID != 30 || sup[0].ChannelName != "feishu" {
		t.Fatalf("record fields %+v", sup[0])
	}
}

func TestRenderGroup(t *testing.T) {
	m := &Module{}
	var evs []*NotifyEvent
	for i := 0; i < 12; i++ {
		e := sampleEvent("alert")
		if i%2 == 1 {
			e.Status = "resolved"
		}
		evs = append(evs, &e)
	}
	title, body, err := m.renderGroup(context.Background(), &delivery{ch: &Channel{}, events: evs})
	if err != nil {
		t.Fatal(err)
	}
	if title != "【告警汇总】共 12 条（触发 6 / 已恢复 6）" || !strings.Contains(body, "其余 2 条已省略") || !strings.Contains(body, "10. ") {
		t.Fatalf("group %q\n%s", title, body)
	}
}

// --------------------------------------------------------------- signatures

func TestDingtalkAndFeishuSignatures(t *testing.T) {
	now := time.UnixMilli(1700000000123)
	u := dingtalkSignedURL("https://oapi.dingtalk.com/robot/send?access_token=x", "SECabc", now)
	pu, _ := url.Parse(u)
	ts := pu.Query().Get("timestamp")
	h := hmac.New(sha256.New, []byte("SECabc"))
	h.Write([]byte(ts + "\nSECabc"))
	if ts != "1700000000123" || pu.Query().Get("sign") != base64.StdEncoding.EncodeToString(h.Sum(nil)) || pu.Query().Get("access_token") != "x" {
		t.Fatalf("dingtalk url %s", u)
	}
	h = hmac.New(sha256.New, []byte("1700000000\nfs"))
	if feishuSign("fs", 1700000000) != base64.StdEncoding.EncodeToString(h.Sum(nil)) {
		t.Fatal("feishu sign")
	}
}

func TestWebhookSignatureAndHeaders(t *testing.T) {
	var gotBody []byte
	var gotHdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHdr = r.Header.Clone()
		w.WriteHeader(204)
	}))
	defer srv.Close()
	ch := &Channel{Name: "itsm", Type: "webhook", Config: ChannelConfig{Webhook: srv.URL + "/hook", Secret: "k3y", Headers: map[string]string{"Authorization": "Bearer t"}}}
	ev := sampleEvent("pipeline")
	pol := netPolicy{BlockPrivate: true, AllowHosts: []string{"127.0.0.1"}}
	if err := newSender(pol).send(context.Background(), ch, Message{Title: "T", Body: "B", Event: &ev}); err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte("k3y"))
	mac.Write(gotBody)
	if gotHdr.Get("X-Matrix-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatal("v1 signature mismatch")
	}
	mac = hmac.New(sha256.New, []byte("k3y"))
	mac.Write([]byte(gotHdr.Get("X-Matrix-Timestamp") + "."))
	mac.Write(gotBody)
	if gotHdr.Get("X-Matrix-Signature-V2") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatal("v2 signature mismatch")
	}
	if gotHdr.Get("Authorization") != "Bearer t" || !strings.HasPrefix(gotHdr.Get("Content-Type"), "application/json") {
		t.Fatalf("headers %v", gotHdr)
	}
	var p map[string]any
	_ = json.Unmarshal(gotBody, &p)
	evp, _ := p["event"].(map[string]any)
	if p["title"] != "T" || evp["kind"] != "pipeline" || evp["status"] != "failed" {
		t.Fatalf("payload %s", gotBody)
	}
}

func TestBuildPayloadPerType(t *testing.T) {
	msg := Message{Title: "标题", Body: strings.Repeat("长", 3000)}
	wc := buildPayload(&Channel{Type: "wecom"}, msg, time.Now())
	content := wc["markdown"].(map[string]any)["content"].(string)
	if len(content) > 4000 || !strings.HasSuffix(content, "（已截断）") || !strings.HasPrefix(content, "**标题**") {
		t.Fatalf("wecom content len %d", len(content))
	}
	if s := buildPayload(&Channel{Type: "slack"}, Message{Title: "a", Body: "b"}, time.Now()); s["text"] != "*a*\nb" {
		t.Fatalf("slack %v", s)
	}
	if f := buildPayload(&Channel{Type: "feishu"}, Message{Title: "a", Body: "b"}, time.Now()); f["msg_type"] != "text" {
		t.Fatalf("feishu %v", f)
	}
}

// --------------------------------------------------------------------- SSRF

func TestInternalIPAndPolicy(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fd00::1", "fe80::1"} {
		if !isInternalIP(net.ParseIP(s)) {
			t.Errorf("%s should be internal", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2001:4860:4860::8888"} {
		if isInternalIP(net.ParseIP(s)) {
			t.Errorf("%s should be public", s)
		}
	}
	p := netPolicy{BlockPrivate: true, AllowHosts: []string{"127.0.0.1:2525", "10.0.0.0/8", "stub.local"}}
	if err := p.normalize(); err != nil {
		t.Fatal(err)
	}
	if p.checkIP(net.ParseIP("127.0.0.1"), "2525") != nil || p.checkIP(net.ParseIP("127.0.0.1"), "80") == nil {
		t.Fatal("host:port allow entry")
	}
	if p.checkIP(net.ParseIP("10.9.9.9"), "443") != nil || !p.hostListed("stub.local", "1") {
		t.Fatal("cidr / name allow entry")
	}
	if (netPolicy{}).checkIP(metadataIP, "80") == nil {
		t.Fatal("metadata address is always blocked unless listed")
	}
	if (netPolicy{}).checkIP(net.ParseIP("10.0.0.1"), "80") != nil {
		t.Fatal("block_private=false allows private ranges")
	}
	if (&netPolicy{AllowHosts: []string{"10.0.0.0/33"}}).normalize() == nil {
		t.Fatal("bad CIDR accepted")
	}
	ctx := context.Background()
	if p.checkURL(ctx, "http://127.0.0.1:8080/x", false) == nil {
		t.Fatal("unlisted loopback must be rejected")
	}
	if p.checkURL(ctx, "http://localhost/x", false) == nil {
		t.Fatal("localhost must be rejected")
	}
	if p.checkURL(ctx, "http://127.0.0.1:2525/hook", true) != nil {
		t.Fatal("listed stub may use http even for robots")
	}
	if p.checkURL(ctx, "http://93.184.216.34/hook", true) == nil {
		t.Fatal("robots require https")
	}
	if p.checkURL(ctx, "https://u:p@93.184.216.34/hook", false) == nil {
		t.Fatal("credentials in URL must be rejected")
	}
}

func TestSenderBlocksInternalTargetsAndRedirects(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/redir" {
			http.Redirect(w, r, "http://169.254.169.254/latest", http.StatusFound)
			return
		}
		io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()
	ch := &Channel{Type: "wecom", Config: ChannelConfig{Webhook: srv.URL + "/send"}}
	err := newSender(defaultNetPolicy()).send(context.Background(), ch, Message{Title: "t", Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "内网") || hits != 0 {
		t.Fatalf("loopback must be blocked at dial time: %v (hits %d)", err, hits)
	}
	allow := netPolicy{BlockPrivate: true, AllowHosts: []string{"127.0.0.1"}}
	if err := newSender(allow).send(context.Background(), ch, Message{Title: "t", Body: "b"}); err != nil {
		t.Fatalf("allow-listed stub: %v", err)
	}
	ch.Config.Webhook = srv.URL + "/redir"
	if err := newSender(allow).send(context.Background(), ch, Message{Title: "t", Body: "b"}); err == nil || !strings.Contains(err.Error(), "重定向") {
		t.Fatalf("redirect must not be followed: %v", err)
	}
}

// -------------------------------------------------------------------- email

// smtpStub is a minimal SMTP receiver like the sandbox's :2525 stub.
func smtpStub(t *testing.T) (addr string, got chan string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	got = make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		defer ln.Close()
		r := bufio.NewReader(c)
		w := func(s string) { _, _ = io.WriteString(c, s+"\r\n") }
		w("220 stub")
		var data strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					w("250 ok")
					got <- data.String()
					continue
				}
				data.WriteString(line)
				continue
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				w("250 stub")
			case cmd == "DATA":
				inData = true
				w("354 go")
			case cmd == "QUIT":
				w("221 bye")
				return
			default:
				w("250 ok")
			}
		}
	}()
	return ln.Addr().String(), got
}

func TestSendMailThroughStubAndPolicy(t *testing.T) {
	addr, got := smtpStub(t)
	host, port, _ := net.SplitHostPort(addr)
	cfg := ChannelConfig{Host: host, From: "alert@example.com", To: []string{"ops@example.com"}}
	cfg.Port = atoiOr(port)
	ch := &Channel{Type: "email", Config: cfg}
	if err := newSender(defaultNetPolicy()).send(context.Background(), ch, Message{Title: "x", Body: "y"}); err == nil {
		t.Fatal("custom SMTP on loopback must be blocked by default")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pol := netPolicy{BlockPrivate: true, AllowHosts: []string{addr}}
	if err := newSender(pol).send(ctx, ch, Message{Title: "告警\r\nBcc: evil@x.com", Body: "正文"}); err != nil {
		t.Fatal(err)
	}
	msg := <-got
	if !strings.Contains(msg, "Subject: =?UTF-8?b?") || strings.Contains(msg, "\r\nBcc:") {
		t.Fatalf("mail %q", msg)
	}
}

func atoiOr(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

func TestSystemMailRequiresConfig(t *testing.T) {
	ch := &Channel{Type: "email", Config: ChannelConfig{UseSystem: true, To: []string{"a@b.c"}}}
	if err := newSender(defaultNetPolicy()).send(context.Background(), ch, Message{}); err == nil || !strings.Contains(err.Error(), "系统邮件") {
		t.Fatalf("want system mail error, got %v", err)
	}
}

// ---------------------------------------------------------------- channels

func TestValidateChannelAndMasking(t *testing.T) {
	cfg := ChannelConfig{Webhook: "https://itsm.example.com/h", Headers: map[string]string{"X-Token": "abcdefghijkl"}}
	if err := validateChannel("webhook", &cfg); err != nil {
		t.Fatal(err)
	}
	for _, h := range []map[string]string{{"Host": "x"}, {"Bad Header": "x"}, {"X-A": "a\r\nb"}, {"X-Matrix-Signature": "x"}} {
		c := ChannelConfig{Webhook: "https://x.example.com", Headers: h}
		if validateChannel("webhook", &c) == nil {
			t.Errorf("header %v accepted", h)
		}
	}
	if err := validateChannel("email", &ChannelConfig{UseSystem: true, To: []string{"ops@example.com"}}); err != nil {
		t.Fatal(err)
	}
	if validateChannel("email", &ChannelConfig{Host: "smtp.example.com:25", From: "a@b.c", To: []string{"x@y.z"}}) == nil {
		t.Fatal("host with port accepted")
	}
	if validateChannel("email", &ChannelConfig{UseSystem: true, To: []string{"x@y.z\r\nBcc: e@v.il"}}) == nil {
		t.Fatal("header injection in address accepted")
	}
	if validateChannel("teams", &ChannelConfig{}) == nil {
		t.Fatal("unknown type accepted")
	}
	ch := Channel{Config: ChannelConfig{Webhook: "https://open.feishu.cn/open-apis/bot/v2/hook/abcdef123456", Secret: "s3cretvalue1", Headers: map[string]string{"X-Token": "abcdefghijkl"}}}
	mk := ch.masked()
	if strings.Contains(mk.Config.Webhook, "abcdef123456") || mk.Config.Secret == "s3cretvalue1" || mk.Config.Headers["X-Token"] == "abcdefghijkl" {
		t.Fatalf("not masked: %+v", mk.Config)
	}
	if ch.Config.Headers["X-Token"] != "abcdefghijkl" {
		t.Fatal("masking must not mutate the original")
	}
	n := mk.Config
	n.Headers = map[string]string{"X-Token": mk.Config.Headers["X-Token"], "X-New": "v"}
	mergeSecrets(&n, ch.Config)
	if n.Webhook != ch.Config.Webhook || n.Secret != ch.Config.Secret || n.Headers["X-Token"] != "abcdefghijkl" || n.Headers["X-New"] != "v" {
		t.Fatalf("merge %+v", n)
	}
}

func TestChannelAcceptsOnlyGatesAlerts(t *testing.T) {
	ch := &Channel{Enabled: true, Envs: []string{"prod"}, Severities: []string{"critical"}}
	a := sampleEvent("alert")
	p := sampleEvent("pipeline")
	if !channelAccepts(ch, &a) || !channelAccepts(ch, &p) {
		t.Fatal("gate")
	}
	a.Env = "test"
	if channelAccepts(ch, &a) {
		t.Fatal("env gate")
	}
	ch.Enabled = false
	if channelAccepts(ch, &p) {
		t.Fatal("disabled channel")
	}
}

// ------------------------------------------------------- events and records

func TestEventMappers(t *testing.T) {
	d := time.Now()
	inc := incidentEvent(9, 3, "assigned", "bob", "指派给 alice", "支付超时", "desc", "P2", "pay", "prod", "alice", d, "https://m")
	if inc.Severity != "critical" || inc.Link != "https://m/#/oncall/inc/3" || inc.Labels["assigned_to"] != "alice" || inc.Summary != "指派给 alice" {
		t.Fatalf("incident %+v", inc)
	}
	if incidentEvent(1, 3, "declared", "", "m", "t", "desc", "P4", "", "", "", d, "").Summary != "desc" {
		t.Fatal("declared uses description")
	}
	st, fin := d.Add(-90*time.Second), d
	pl := pipelineEvent(5, "failed", "push", "ci", "main", "0123456789abcdef", "", "deploy", "api", "c1", "prod", &st, &fin, "")
	if pl.AlertName != "api / deploy" || pl.Severity != "warning" || pl.Labels["commit"] != "0123456789ab" || pl.Summary != "耗时 1m30s" {
		t.Fatalf("pipeline %+v", pl)
	}
	ap := approvalEvent(4, "cluster.scale", "扩容", "prod", "大促", "zl", d, "https://m")
	if ap.Severity != "warning" || ap.Link != "https://m/#/approvals" || ap.Key != "approval:4" {
		t.Fatalf("approval %+v", ap)
	}
	for _, ev := range []*NotifyEvent{inc, pl, ap} {
		b := builtinTemplates[ev.Kind]
		if _, _, err := renderTemplate(b.TitleTpl, b.BodyTpl, ev); err != nil {
			t.Errorf("%s render: %v", ev.Kind, err)
		}
	}
}

func TestRecordFilterAndResult(t *testing.T) {
	where, args, err := recordFilter(map[string]string{"channel_id": "3", "status": "failed", "event_kind": "alert", "q": "50%_x", "since": "1700000000"}, []string{"prod"})
	if err != nil {
		t.Fatal(err)
	}
	want := " WHERE channel_id=$1 AND event_kind=$2 AND status=$3 AND (title ILIKE $4 OR channel_name ILIKE $4 OR event_key ILIKE $4 OR reason ILIKE $4) AND created_at >= $5 AND (env = '' OR env = ANY($6))"
	if where != want || len(args) != 6 || args[3] != `%50\%\_x%` {
		t.Fatalf("where %q args %v", where, args)
	}
	for _, q := range []map[string]string{{"channel_id": "x"}, {"status": "ok"}, {"event_kind": "foo"}, {"since": "yesterday"}} {
		if _, _, err := recordFilter(q, nil); err == nil {
			t.Errorf("filter %v accepted", q)
		}
	}
	if w, _, _ := recordFilter(map[string]string{}, nil); w != "" {
		t.Fatal("empty filter")
	}
	r := &notifyRecord{}
	r.setResult(io.ErrUnexpectedEOF)
	if r.Status != "failed" || r.Reason == "" {
		t.Fatal("failed result")
	}
	r.setResult(nil)
	if r.Status != "success" || r.Reason != "" {
		t.Fatal("success result")
	}
}

func TestScrubURLError(t *testing.T) {
	u := "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=SECRETKEY123"
	err := scrubURLError(&url.Error{Op: "Post", URL: u, Err: io.EOF}, u)
	if strings.Contains(err.Error(), "SECRETKEY123") {
		t.Fatalf("token leaked: %v", err)
	}
}
