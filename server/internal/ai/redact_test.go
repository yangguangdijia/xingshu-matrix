package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactSecrets(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		leak  []string // must not appear in output
		keep  []string // must still appear
		exact string   // optional exact expected output
	}{
		{name: "password=", in: "login with user=root password=S3cr3t! now", leak: []string{"S3cr3t!"}, keep: []string{"user=root", "password=******", "now"}},
		{name: "passwd colon", in: "passwd: hunter2", exact: "passwd: ******"},
		{name: "pwd", in: "host 10.0.0.5 pwd=abc123", exact: "host 10.0.0.5 pwd=******"},
		{name: "token", in: "token=ghx_abcdef123456&x=1", exact: "token=******&x=1"},
		{name: "secret yaml", in: "client_secret: 'q w e'", exact: "client_secret: '******'"},
		{name: "env style", in: "-e MYSQL_ROOT_PASSWORD=rootpw -e TZ=Asia/Shanghai", exact: "-e MYSQL_ROOT_PASSWORD=****** -e TZ=Asia/Shanghai"},
		{name: "ak sk", in: "ak=AKXXXXXXXX sk=SKYYYYYYYYYY region=cn", exact: "ak=****** sk=****** region=cn"},
		{name: "access key id", in: "AccessKeyId: LTAI5tAbCdEfGhIjKlMn AccessKeySecret: zzzzzzzz", leak: []string{"LTAI5tAbCdEfGhIjKlMn", "zzzzzzzz"}},
		{name: "aws key", in: "export AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE", leak: []string{"AKIAIOSFODNN7EXAMPLE"}},
		{name: "bare aws key", in: "key AKIAIOSFODNN7EXAMPLE in log", exact: "key ****** in log"},
		{name: "json", in: `{"username":"admin","password":"p@ss\"w"}`, leak: []string{"p@ss"}, keep: []string{`"username":"admin"`}},
		{name: "json spaced", in: `{"password": "abc", "port": 22}`, exact: `{"password": "******", "port": 22}`},
		{name: "escaped json", in: `{\"token\":\"tok123\"}`, exact: `{\"token\":\"******\"}`},
		{name: "authorization bearer", in: "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig", exact: "Authorization: Bearer ******"},
		{name: "authorization basic header json", in: `"Authorization":"Basic dXNlcjpwYXNz"`, exact: `"Authorization":"Basic ******"`},
		{name: "authorization raw", in: "authorization=abcdef", exact: "authorization=******"},
		{name: "url userinfo", in: "git clone https://bob:pa55@git.example.com/repo.git", exact: "git clone https://bob:******@git.example.com/repo.git"},
		{name: "redis url empty user", in: "redis://:redispw@10.0.0.1:6379/0", exact: "redis://:******@10.0.0.1:6379/0"},
		{name: "url without creds untouched", in: "http://10.0.0.5:8080/health", exact: "http://10.0.0.5:8080/health"},
		{name: "private key", in: "key:\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\nAAAA\n-----END OPENSSH PRIVATE KEY-----\ndone", leak: []string{"b3BlbnNzaC1rZXktdjEAAAAA"}, keep: []string{"BEGIN OPENSSH PRIVATE KEY", "done"}},
		{name: "rsa key truncated", in: "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA", leak: []string{"MIIEowIBAAKCAQEA"}},
		{name: "cli flag", in: "mytool --password hunter2 --user root", exact: "mytool --password ****** --user root"},
		{name: "sshpass", in: "sshpass -p 'x y' ssh root@h", exact: "sshpass -p '******' ssh root@h"},
		{name: "mysql -p", in: "mysql -uroot -pTopSecret -h db", exact: "mysql -uroot -p****** -h db"},
		{name: "chinese", in: "帮我部署 nginx 到 10.0.0.5，账号 root 密码是 Abc@123，端口 22", leak: []string{"Abc@123"}, keep: []string{"root", "端口 22", "10.0.0.5"}},
		{name: "chinese colon", in: "口令：qwerty", exact: "口令：******"},
		{name: "no false positive", in: "max_tokens=1024 disk=50G task_id=7 passwordless login", exact: "max_tokens=1024 disk=50G task_id=7 passwordless login"},
		{name: "openai key", in: "use sk-proj_abcdefghijklmnopqrstuvwx", leak: []string{"abcdefghijklmnopqrstuvwx"}},
		{name: "empty", in: "", exact: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RedactSecrets(c.in)
			if c.exact != "" || c.in == "" {
				if got != c.exact {
					t.Fatalf("RedactSecrets(%q)\n got %q\nwant %q", c.in, got, c.exact)
				}
			}
			for _, l := range c.leak {
				if strings.Contains(got, l) {
					t.Fatalf("secret %q leaked: %q", l, got)
				}
			}
			for _, k := range c.keep {
				if !strings.Contains(got, k) {
					t.Fatalf("expected %q to be kept: %q", k, got)
				}
			}
			if again := RedactSecrets(got); again != got {
				t.Fatalf("not idempotent: %q -> %q", got, again)
			}
		})
	}
}

func TestRedactJSONStaysValid(t *testing.T) {
	in := `{"a":"password=x1","b":{"secret":"y2","n":1},"auth":"Authorization: Bearer zz"}`
	got := RedactSecrets(in)
	if !json.Valid([]byte(got)) {
		t.Fatalf("invalid json after redaction: %s", got)
	}
	for _, l := range []string{"x1", "y2", "zz"} {
		if strings.Contains(got, l) {
			t.Fatalf("leak %q in %s", l, got)
		}
	}
}

func TestRedactMessages(t *testing.T) {
	in := []Message{
		TextMessage("user", "服务器 10.0.0.5 root 密码: P@ssw0rd 帮我装 nginx"),
		{Role: "assistant", Content: json.RawMessage("null"), ToolCalls: []ToolCall{{ID: "c1"}}},
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"token=abc"}]`)},
	}
	in[1].ToolCalls[0].Function.Name = "aiops_onboard"
	in[1].ToolCalls[0].Function.Arguments = `{"address":"10.0.0.5","password":"P@ssw0rd"}`
	out := RedactMessages(in)
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "P@ssw0rd") || strings.Contains(string(b), "token=abc") {
		t.Fatalf("leak: %s", b)
	}
	var s string
	if err := json.Unmarshal(out[0].Content, &s); err != nil || !strings.Contains(s, "密码: ******") {
		t.Fatalf("content = %s (%v)", out[0].Content, err)
	}
	if !json.Valid(out[2].Content) {
		t.Fatalf("multipart content invalid: %s", out[2].Content)
	}
	// input untouched
	if !strings.Contains(in[1].ToolCalls[0].Function.Arguments, "P@ssw0rd") {
		t.Fatal("input was modified")
	}
}
