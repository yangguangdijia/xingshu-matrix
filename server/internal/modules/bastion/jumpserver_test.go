package bastion

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ---------- 改密流程（假 SSH 主机 + 假凭据存储） ----------

type fakeBox struct{ failEncrypt bool }

func (b fakeBox) Encrypt(s string) (string, error) {
	if b.failEncrypt {
		return "", errors.New("boom")
	}
	return "enc:" + s, nil
}
func (fakeBox) Decrypt(s string) (string, error) {
	if !strings.HasPrefix(s, "enc:") {
		return "", errors.New("bad envelope")
	}
	return strings.TrimPrefix(s, "enc:"), nil
}

// fakeHost 模拟一台主机上的账号密码。
type fakeHost struct {
	password    string
	changeErr   error // Change 返回的错误
	applyOnFail bool  // Change 返回错误时密码是否其实已改（例如改完连接断了）
	loginDown   bool  // 所有登录都失败（主机不可达）
	changes     int
}

func (h *fakeHost) Change(_ *rotationTarget, oldPw, newPw string) error {
	h.changes++
	if h.loginDown || oldPw != h.password {
		return errOldLogin
	}
	if h.changeErr != nil {
		if h.applyOnFail {
			h.password = newPw
		}
		return h.changeErr
	}
	h.password = newPw
	return nil
}

func (h *fakeHost) Login(_ *rotationTarget, pw string) error {
	if h.loginDown || pw != h.password {
		return errors.New("auth failed")
	}
	return nil
}

type fakeStore struct {
	secret, pending string
	savePendingErr  error
	commitErr       error
	commits         int
	discards        int
}

func (s *fakeStore) SavePending(_ context.Context, _ int64, enc string) error {
	if s.savePendingErr != nil {
		return s.savePendingErr
	}
	s.pending = enc
	return nil
}
func (s *fakeStore) Commit(context.Context, int64) error {
	s.commits++
	if s.commitErr != nil {
		return s.commitErr
	}
	if s.pending == "" {
		return errors.New("no pending")
	}
	s.secret, s.pending = s.pending, ""
	return nil
}
func (s *fakeStore) Discard(context.Context, int64) error {
	s.discards++
	s.pending = ""
	return nil
}

func rotTarget(proto, auth string) *rotationTarget {
	return &rotationTarget{
		acc:   &Account{ID: 7, Username: "ops", AuthType: auth, Secret: "enc:old-pass"},
		asset: &Asset{ID: 3, Name: "web-01", Protocol: proto},
	}
}

func fixedGen(int) (string, error) { return "New-Pass-123456", nil }

func TestRotationSuccessCommitsNewSecret(t *testing.T) {
	host, st := &fakeHost{password: "old-pass"}, &fakeStore{secret: "enc:old-pass"}
	status, msg := rotation{host: host, store: st, box: fakeBox{}, gen: fixedGen}.Rotate(context.Background(), rotTarget("ssh", "password"), 20)
	if status != "succeeded" || st.secret != "enc:New-Pass-123456" || st.pending != "" || host.password != "New-Pass-123456" {
		t.Fatalf("status=%s msg=%s store=%+v host=%+v", status, msg, st, host)
	}
}

func TestRotationChangeFailsOldStillWorksRollsBack(t *testing.T) {
	host := &fakeHost{password: "old-pass", changeErr: errors.New("passwd: Authentication token manipulation error")}
	st := &fakeStore{secret: "enc:old-pass"}
	status, msg := rotation{host: host, store: st, box: fakeBox{}, gen: fixedGen}.Rotate(context.Background(), rotTarget("ssh", "password"), 20)
	if status != "failed" || !strings.Contains(msg, "原密码仍可用") {
		t.Fatalf("status=%s msg=%s", status, msg)
	}
	if st.secret != "enc:old-pass" || st.pending != "" || st.discards != 1 || st.commits != 0 {
		t.Fatalf("pending secret must be discarded, store=%+v", st)
	}
}

func TestRotationVerifyFailsButHostChangedKeepsNewSecret(t *testing.T) {
	// Change 报错，但主机上密码其实已改：旧密码失效，新密码可用 —— 视为成功前的「验证」通过以外的路径
	host := &fakeHost{password: "old-pass", changeErr: errors.New("connection reset"), applyOnFail: true}
	st := &fakeStore{secret: "enc:old-pass"}
	status, msg := rotation{host: host, store: st, box: fakeBox{}, gen: fixedGen}.Rotate(context.Background(), rotTarget("ssh", "password"), 20)
	if status != "succeeded" || !strings.Contains(msg, "新密码已生效") {
		t.Fatalf("status=%s msg=%s", status, msg)
	}
	if st.secret != "enc:New-Pass-123456" {
		t.Fatalf("new secret must be kept when the old password no longer works, store=%+v", st)
	}
}

func TestRotationBothPasswordsFailCommitsAndFlags(t *testing.T) {
	// 改密命令执行后连接中断，新旧密码都无法确认：保留新密码并提示人工核对
	host := &fakeHost{password: "old-pass", changeErr: errors.New("connection reset"), applyOnFail: true}
	st := &fakeStore{secret: "enc:old-pass"}
	r := rotation{host: &flakyHost{fakeHost: host}, store: st, box: fakeBox{}, gen: fixedGen}
	status, msg := r.Rotate(context.Background(), rotTarget("ssh", "password"), 20)
	if status != "failed" || !strings.Contains(msg, "人工核对") || st.secret != "enc:New-Pass-123456" {
		t.Fatalf("status=%s msg=%s store=%+v", status, msg, st)
	}
}

// flakyHost 改密后所有登录都失败（例如 sshd 重启中）。
type flakyHost struct{ *fakeHost }

func (f *flakyHost) Login(*rotationTarget, string) error { return errors.New("connection refused") }

func TestRotationHostUnreachableDoesNotLoseOldSecret(t *testing.T) {
	// 主机不可达：改密前就登录失败，主机一定没改 —— 丢弃新密码，原凭据不变。
	host := &fakeHost{password: "old-pass", loginDown: true}
	st := &fakeStore{secret: "enc:old-pass"}
	status, msg := rotation{host: host, store: st, box: fakeBox{}, gen: fixedGen}.Rotate(context.Background(), rotTarget("ssh", "password"), 20)
	if status != "failed" || !strings.Contains(msg, "未改密") || st.commits != 0 || st.discards != 1 || st.secret != "enc:old-pass" {
		t.Fatalf("status=%s msg=%s store=%+v", status, msg, st)
	}
}

func TestRotationPendingSaveFailureNeverTouchesHost(t *testing.T) {
	host := &fakeHost{password: "old-pass"}
	st := &fakeStore{secret: "enc:old-pass", savePendingErr: errors.New("db down")}
	status, msg := rotation{host: host, store: st, box: fakeBox{}, gen: fixedGen}.Rotate(context.Background(), rotTarget("ssh", "password"), 20)
	if status != "failed" || host.changes != 0 || host.password != "old-pass" || !strings.Contains(msg, "未改动主机") {
		t.Fatalf("status=%s msg=%s host=%+v", status, msg, host)
	}
}

func TestRotationCommitFailureReported(t *testing.T) {
	host := &fakeHost{password: "old-pass"}
	st := &fakeStore{secret: "enc:old-pass", commitErr: errors.New("db down")}
	status, msg := rotation{host: host, store: st, box: fakeBox{}, gen: fixedGen}.Rotate(context.Background(), rotTarget("ssh", "password"), 20)
	if status != "failed" || !strings.Contains(msg, "待生效") || st.pending == "" {
		t.Fatalf("status=%s msg=%s store=%+v", status, msg, st)
	}
}

func TestRotationSkipsKeysAndNonSSH(t *testing.T) {
	r := rotation{host: &fakeHost{}, store: &fakeStore{}, box: fakeBox{}, gen: fixedGen}
	if st, _ := r.Rotate(context.Background(), rotTarget("ssh", "private_key"), 20); st != "skipped" {
		t.Fatalf("private key should be skipped, got %s", st)
	}
	if st, _ := r.Rotate(context.Background(), rotTarget("mysql", "password"), 20); st != "skipped" {
		t.Fatalf("database account should be skipped, got %s", st)
	}
	if st, _ := (rotation{host: &fakeHost{}, store: &fakeStore{}, box: fakeBox{failEncrypt: true}, gen: fixedGen}).Rotate(context.Background(), rotTarget("ssh", "password"), 20); st != "failed" {
		t.Fatalf("encrypt failure must fail, got %s", st)
	}
}

// ---------- SQL 执行器分级路由 ----------

type fakeRunner struct {
	reads, execs int
	lastLimit    int
	err          error
}

func (f *fakeRunner) ReadOnly(_ context.Context, _ string, limit int) (*DBResult, error) {
	f.reads++
	f.lastLimit = limit
	return &DBResult{Columns: []string{"phone"}, Rows: [][]any{{"13812345678"}, {int64(1)}}, RowCount: 2}, f.err
}
func (f *fakeRunner) Exec(context.Context, string) (*DBResult, error) {
	f.execs++
	return &DBResult{RowsAffected: 3}, f.err
}

func TestDBExecutorRoutesByLevel(t *testing.T) {
	mask := &Masker{rules: []compiledMask{{re: regexp.MustCompile(`1[3-9]\d{9}`), repl: "1**********"}}}
	cases := []struct {
		sql      string
		approved bool
		level    string
		status   string
		reads    int
		execs    int
		submits  int
	}{
		{"SELECT phone FROM users WHERE id=1", false, SQLRead, "executed", 1, 0, 0},
		{"show tables", false, SQLRead, "executed", 1, 0, 0},
		{"UPDATE users SET name='a' WHERE id=1", false, SQLWrite, "pending", 0, 0, 1},
		{"DELETE FROM users", false, SQLHigh, "pending", 0, 0, 1},
		{"DROP TABLE users", false, SQLHigh, "pending", 0, 0, 1},
		{"DROP DATABASE prod", false, SQLForbidden, "rejected", 0, 0, 0},
		{"SELECT 1; DELETE FROM users", false, SQLForbidden, "rejected", 0, 0, 0},
		{"GRANT ALL ON *.* TO x", false, SQLForbidden, "rejected", 0, 0, 0},
		{"UPDATE users SET name='a' WHERE id=1", true, SQLWrite, "executed", 0, 1, 0},
		{"SELECT * FROM t FOR UPDATE", false, SQLWrite, "pending", 0, 0, 1},
	}
	for _, tc := range cases {
		r := &fakeRunner{}
		submits := 0
		ex := &dbExecutor{runner: r, mask: mask, limit: 50, submit: func(context.Context, string, string) (int64, error) { submits++; return 42, nil }}
		out := ex.Run(context.Background(), tc.sql, tc.approved)
		if out.Level != tc.level || out.Status != tc.status || r.reads != tc.reads || r.execs != tc.execs || submits != tc.submits {
			t.Fatalf("%q approved=%v: out=%+v reads=%d execs=%d submits=%d", tc.sql, tc.approved, out, r.reads, r.execs, submits)
		}
		if tc.status == "pending" && out.TicketID != 42 {
			t.Fatalf("%q: ticket id missing: %+v", tc.sql, out)
		}
		if tc.level == SQLRead {
			if r.lastLimit != 50 {
				t.Fatalf("limit not passed: %d", r.lastLimit)
			}
			if out.Result.Rows[0][0] != "1**********" || out.Result.Rows[1][0] != int64(1) {
				t.Fatalf("result not masked: %+v", out.Result.Rows)
			}
		}
	}
}

func TestDBExecutorFailsClosed(t *testing.T) {
	// 没有审批服务：变更直接拒绝，不执行
	r := &fakeRunner{}
	out := (&dbExecutor{runner: r}).Run(context.Background(), "INSERT INTO t VALUES (1)", false)
	if out.Status != "rejected" || r.execs != 0 {
		t.Fatalf("out=%+v execs=%d", out, r.execs)
	}
	// 提交审批失败：拒绝
	out = (&dbExecutor{runner: r, submit: func(context.Context, string, string) (int64, error) { return 0, errors.New("no reason") }}).Run(context.Background(), "INSERT INTO t VALUES (1)", false)
	if out.Status != "rejected" || r.execs != 0 {
		t.Fatalf("out=%+v", out)
	}
	// 禁止语句即使标记为已审批也不执行
	out = (&dbExecutor{runner: r}).Run(context.Background(), "SHUTDOWN", true)
	if out.Status != "rejected" || r.execs != 0 || r.reads != 0 {
		t.Fatalf("forbidden executed: %+v", out)
	}
	// 只读执行出错：failed，限额越界回落到默认值
	r = &fakeRunner{err: context.DeadlineExceeded}
	out = (&dbExecutor{runner: r, limit: 999999}).Run(context.Background(), "SELECT 1", false)
	if out.Status != "failed" || !strings.Contains(out.Error, "超时") || r.lastLimit != dbDefaultLimit {
		t.Fatalf("out=%+v limit=%d", out, r.lastLimit)
	}
}

func TestSQLSummaryStripsLiterals(t *testing.T) {
	s := sqlSummary("UPDATE users SET password='Secret#1' /* c */ WHERE phone='13812345678'")
	if strings.Contains(s, "Secret") || strings.Contains(s, "13812345678") {
		t.Fatalf("summary leaks literals: %s", s)
	}
}

// ---------- 登录 ACL ----------

func TestLoginACLMatching(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	mon10 := time.Date(2026, 11, 2, 10, 0, 0, 0, loc) // 周一 10:00
	sun23 := time.Date(2026, 11, 8, 23, 30, 0, 0, loc)
	who := aclSubject{UserID: 5, Roles: []string{"ops"}, Groups: []string{"9"}}
	ip := net.ParseIP("10.1.2.3")
	rules := []LoginACL{
		{ID: 1, Name: "夜间禁止", Priority: 1, SubjectType: "all", TimeFrom: "22:00", TimeTo: "06:00", Action: "deny", Enabled: true},
		{ID: 2, Name: "办公网放行", Priority: 2, SubjectType: "role", Subject: "ops", IPRanges: []string{"10.1.0.0/16"}, Weekdays: []int{1, 2, 3, 4, 5}, Action: "allow", Enabled: true},
		{ID: 3, Name: "其他来源拒绝", Priority: 3, SubjectType: "all", IPRanges: []string{"*"}, Action: "deny", Enabled: true},
		{ID: 4, Name: "停用规则", Priority: 0, SubjectType: "all", Action: "deny", Enabled: false},
	}
	if r := EvaluateACL(rules, who, 1, ip, mon10); r == nil || r.ID != 2 {
		t.Fatalf("weekday office hours should hit allow rule, got %+v", r)
	}
	if r := EvaluateACL(rules, who, 1, ip, sun23); r == nil || r.ID != 1 {
		t.Fatalf("night should hit deny rule, got %+v", r)
	}
	if r := EvaluateACL(rules, who, 1, net.ParseIP("192.168.1.1"), mon10); r == nil || r.ID != 3 {
		t.Fatalf("other source should be denied, got %+v", r)
	}
	if r := EvaluateACL(rules[:2], aclSubject{UserID: 6}, 1, ip, mon10); r != nil {
		t.Fatalf("no match expected, got %+v", r)
	}
	grp := LoginACL{SubjectType: "group", Subject: "9", AssetIDs: []int64{7}, Action: "deny", Enabled: true}
	if !MatchACL(&grp, who, 7, ip, mon10) || MatchACL(&grp, who, 8, ip, mon10) {
		t.Fatal("group + asset scope mismatch")
	}
	user := LoginACL{SubjectType: "user", Subject: "5", IPRanges: []string{"10.1.2.1-10.1.2.9"}, Action: "deny", Enabled: true}
	if !MatchACL(&user, who, 1, ip, mon10) || MatchACL(&user, who, 1, net.ParseIP("10.1.2.10"), mon10) {
		t.Fatal("ip range mismatch")
	}
}

func TestACLInputValidation(t *testing.T) {
	bad := []aclIn{
		{Name: "a", SubjectType: "user", Action: "deny"},
		{Name: "a", SubjectType: "all", Action: "deny", IPRanges: []string{"10.0.0.300"}},
		{Name: "a", SubjectType: "all", Action: "deny", Weekdays: []int{0}},
		{Name: "a", SubjectType: "all", Action: "deny", TimeFrom: "09:00"},
		{Name: "a", SubjectType: "all", Action: "deny", TimeFrom: "9:00", TimeTo: "18:00"},
		{Name: "a", SubjectType: "all", Action: "deny", TimeFrom: "09:00", TimeTo: "09:00"},
	}
	for i, in := range bad {
		in := in
		if in.check() == nil {
			t.Fatalf("case %d should fail: %+v", i, in)
		}
	}
	good := aclIn{Name: "ok", SubjectType: "all", Subject: "x", Action: "allow", IPRanges: []string{" 10.0.0.0/8 ", "", "::1"}, Weekdays: []int{1, 7}, TimeFrom: "09:00", TimeTo: "18:30"}
	if err := good.check(); err != nil || good.Subject != "" || len(good.IPRanges) != 2 || good.Priority != 100 {
		t.Fatalf("err=%v in=%+v", err, good)
	}
}

// ---------- 授权合并、过滤允许动作、平台 ----------

func TestMergeGrants(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	rows := []grantRow{
		{AssetID: 1, AccountID: 10, Actions: []string{"connect", "copy"}, ExpiresAt: &exp},
		{AssetID: 1, AccountID: 10, Actions: []string{"paste", "connect"}},
		{AssetID: 2, AccountID: 20, Actions: []string{"upload"}},
	}
	g := mergeGrants(rows)
	if len(g) != 1 || g[0].AssetID != 1 || strings.Join(g[0].Actions, ",") != "connect,copy,paste" || g[0].ExpiresAt != nil {
		t.Fatalf("merge = %+v", g)
	}
}

func TestRuleInputValidation(t *testing.T) {
	in := ruleIn{Name: "r", SubjectType: "group", Subject: "3"}
	if in.check() == nil {
		t.Fatal("rule without assets or nodes must fail")
	}
	in = ruleIn{Name: "r", SubjectType: "group", Subject: "3", NodeIDs: []int64{1}, Actions: []string{"connect", "connect", "copy"}}
	if err := in.check(); err != nil || len(in.Actions) != 2 {
		t.Fatalf("err=%v actions=%v", err, in.Actions)
	}
	in = ruleIn{Name: "r", SubjectType: "user", Subject: "3", AssetIDs: []int64{1}, Actions: []string{"rm"}}
	if in.check() == nil {
		t.Fatal("unknown action must fail")
	}
	s, e := time.Now(), time.Now().Add(-time.Hour)
	in = ruleIn{Name: "r", SubjectType: "user", Subject: "3", AssetIDs: []int64{1}, StartsAt: &s, ExpiresAt: &e}
	if in.check() == nil {
		t.Fatal("expires before starts must fail")
	}
}

func TestAllowFilterShortCircuits(t *testing.T) {
	rs := []FilterRule{
		{ID: 1, Name: "allow-tmp", Re: regexp.MustCompile(`^rm -rf /tmp/`), Action: "allow"},
		{ID: 2, Name: "rm", Re: regexp.MustCompile(`rm\s+-rf`), Action: "deny"},
	}
	p := NewInputProcessor(rs)
	remote, _, ev := p.Input([]byte("rm -rf /tmp/x\rrm -rf /var\r"))
	if len(ev) != 2 || ev[0].Action != "allow" || ev[0].RuleID != 1 || ev[1].Action != "deny" {
		t.Fatalf("events = %+v", ev)
	}
	if !strings.HasPrefix(string(remote), "rm -rf /tmp/x\r") {
		t.Fatalf("remote = %q", remote)
	}
}

func TestFilterInputRejectsEmptyMatch(t *testing.T) {
	in := filterIn{Name: "x", Pattern: ".*", Action: "deny"}
	if in.check() == nil {
		t.Fatal("pattern matching empty command must fail")
	}
	in = filterIn{Name: "x", Pattern: "^reboot", Action: "allow"}
	if err := in.check(); err != nil || in.GroupName != "默认规则组" || in.Priority != 100 {
		t.Fatalf("err=%v in=%+v", err, in)
	}
}

func TestPlatformKindAndProtocols(t *testing.T) {
	for typ, want := range map[string][2]string{"linux": {"host", "ssh"}, "windows": {"host", "rdp"}, "mysql": {"database", "mysql"}, "postgresql": {"database", "postgres"}} {
		c, p := platformKind(typ)
		if c != want[0] || p != want[1] {
			t.Fatalf("%s => %s %s", typ, c, p)
		}
	}
	if !validProtocol("mysql") || !validProtocol("postgres") || validProtocol("rdp") {
		t.Fatal("validProtocol")
	}
	if !nodeNameOK("Web 集群") || nodeNameOK("a/b") || nodeNameOK(" ") {
		t.Fatal("nodeNameOK")
	}
}
