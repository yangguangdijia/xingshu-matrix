package cicd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func advert(lines ...string) string {
	var b bytes.Buffer
	for _, l := range lines {
		if l == "" {
			b.WriteString("0000")
			continue
		}
		b.Write(pktLine(l + "\n"))
	}
	return b.String()
}

const shaA = "1111111111111111111111111111111111111111"
const shaB = "2222222222222222222222222222222222222222"

func TestParseRefAdvertisementSymref(t *testing.T) {
	body := advert("# service=git-upload-pack", "",
		shaA+" HEAD\x00multi_ack symref=HEAD:refs/heads/develop agent=git/2",
		shaB+" refs/heads/main", shaA+" refs/heads/develop", shaA+" refs/tags/v1", "")
	res, err := parseRefAdvertisement(strings.NewReader(body), true)
	if err != nil {
		t.Fatal(err)
	}
	if res.DefaultBranch != "develop" || res.Head != shaA || len(res.Branches) != 2 {
		t.Fatalf("unexpected %+v", res)
	}
}

func TestParseRefAdvertisementRejectsNonGit(t *testing.T) {
	if _, err := parseRefAdvertisement(strings.NewReader("<html>login</html>"), true); err == nil {
		t.Fatal("expected error for non-git body")
	}
}

func TestProbeSmartHTTPBasicAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "bot" || p != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/org/repo.git/info/refs" || r.URL.Query().Get("service") != "git-upload-pack" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, advert("# service=git-upload-pack", "", shaA+" refs/heads/main\x00agent=x", ""))
	}))
	defer srv.Close()
	res, err := probeGitRefs(context.Background(), srv.URL+"/org/repo.git", "bot", "s3cret")
	if err != nil || res.DefaultBranch != "main" || res.Head != shaA {
		t.Fatalf("probe: %+v %v", res, err)
	}
	if _, err := probeGitRefs(context.Background(), srv.URL+"/org/repo.git", "bot", "wrong"); err != errProbeAuth {
		t.Fatalf("want auth error, got %v", err)
	}
}

func TestProbeGitDaemon(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 512)
		n, _ := c.Read(buf)
		got <- string(buf[:n])
		_, _ = io.WriteString(c, advert(shaA+" HEAD\x00symref=HEAD:refs/heads/main", shaA+" refs/heads/main", shaB+" refs/heads/broken", ""))
	}()
	res, err := probeGitRefs(context.Background(), "git://"+ln.Addr().String()+"/demo.git", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if req := <-got; !strings.Contains(req, "git-upload-pack /demo.git\x00host=") {
		t.Fatalf("request %q", req)
	}
	if len(res.Branches) != 2 || res.DefaultBranch != "main" {
		t.Fatalf("unexpected %+v", res)
	}
}

func TestProbeRejectsSSH(t *testing.T) {
	if _, err := probeGitRefs(context.Background(), "git@github.com:org/repo.git", "", ""); err == nil {
		t.Fatal("ssh must not be probed")
	}
}

func TestDockerConfigJSON(t *testing.T) {
	raw, err := dockerConfigJSON("10.42.0.1:5000", "ci", "pw")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Auths map[string]struct{ Auth string } `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Auths["10.42.0.1:5000"].Auth != base64.StdEncoding.EncodeToString([]byte("ci:pw")) {
		t.Fatalf("bad config %s", raw)
	}
}

func TestValidateCredential(t *testing.T) {
	cases := []struct {
		in Credential
		ok bool
	}{
		{Credential{Name: "gh-bot", Type: "git_token", Secret: "x"}, true},
		{Credential{Name: "Bad_Name", Type: "git_token", Secret: "x"}, false},
		{Credential{Name: "reg", Type: "registry", Username: "u", Secret: "x"}, false},
		{Credential{Name: "reg", Type: "registry", Username: "u", Secret: "x", Host: "ghcr.io"}, true},
		{Credential{Name: "key", Type: "ssh_key", Secret: "not a key"}, false},
		{Credential{Name: "key", Type: "ssh_key", Secret: "-----BEGIN OPENSSH PRIVATE KEY-----\nx\n-----END OPENSSH PRIVATE KEY-----"}, true},
	}
	for i, c := range cases {
		in := c.in
		if err := validateCredential(&in, true); (err == nil) != c.ok {
			t.Errorf("case %d: err=%v", i, err)
		}
	}
	if validateApplicationRepoURL("git@github.com:org/repo.git") != nil {
		t.Error("scp-style SSH URL should be accepted")
	}
}

func TestStagePodCredentials(t *testing.T) {
	a := &Application{ImageRepo: "10.42.0.1:5000/demo/app", Dockerfile: "Dockerfile", ContextDir: ".", sshClone: true, RepoURL: "git@h:o/r.git"}
	r := &Run{ID: 7, Branch: "main", ImageTag: "run-7"}
	spec := stagePod(a, r, Stage{Name: "image", Type: "build", registryKey: "dockerconfig-1"}, "cicd-run-7")
	var docker, ssh bool
	for _, v := range spec.Volumes {
		if v.Name == "docker-config" && v.Secret.SecretName == "cicd-run-7" && v.Secret.Items[0].Key == "dockerconfig-1" {
			docker = true
		}
		if v.Name == "git-ssh" && v.Secret.Items[0].Key == "ssh_key" && *v.Secret.DefaultMode == 0o400 {
			ssh = true
		}
	}
	if !docker || !ssh {
		t.Fatalf("volumes %+v", spec.Volumes)
	}
	found := false
	for _, e := range spec.InitContainers[0].Env {
		if e.Name == "GIT_SSH_COMMAND" && strings.Contains(e.Value, "/root/.ssh-key/id") {
			found = true
		}
	}
	if !found {
		t.Fatal("GIT_SSH_COMMAND missing")
	}
	_ = url.URL{}
}
