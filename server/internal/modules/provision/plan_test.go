package provision

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func freshRequest(n int) Request {
	r := Request{Name: "test-cluster", Env: "test", Version: "v1.31.4+k3s1", Mode: "auto", Endpoint: "api.example.com", CNI: "vxlan"}
	for i := 1; i <= n; i++ {
		r.Hosts = append(r.Hosts, Host{IP: fmt.Sprintf("10.0.0.%d", i), Username: "root", Password: "secret", Fingerprint: "SHA256:test"})
	}
	return r
}

func TestRolePlanningAndSecretFreePlan(t *testing.T) {
	for _, tc := range []struct{ hosts, masters, workers int }{{1, 1, 0}, {2, 1, 1}, {3, 3, 0}, {5, 3, 2}} {
		r := freshRequest(tc.hosts)
		p, err := makePlan(&r, true)
		if err != nil || p.Masters != tc.masters || p.Workers != tc.workers {
			t.Fatalf("hosts=%d: plan=%+v err=%v", tc.hosts, p, err)
		}
		data, _ := json.Marshal(p)
		if strings.Contains(string(data), "secret") || strings.Contains(string(data), "password") {
			t.Fatal("plan leaked SSH credentials")
		}
	}
}

func TestRejectInvalidDeployment(t *testing.T) {
	tests := []func(*Request){
		func(r *Request) { r.Hosts[1].IP = r.Hosts[0].IP },
		func(r *Request) { r.Hosts[0].IP = "127.0.0.1" },
		func(r *Request) { r.Hosts[0].IP = "::ffff:127.0.0.1" },
		func(r *Request) { r.Hosts[0].Fingerprint = "" },
		func(r *Request) { r.Hosts[0].Username = "root;echo bad" },
		func(r *Request) { r.Version = "latest;echo bad" },
		func(r *Request) { r.Endpoint = "https://api.example.com:6443" },
		func(r *Request) { r.Endpoint = "" },
		func(r *Request) { r.Env = "" },
		func(r *Request) { r.CNI = "unsupported" },
		func(r *Request) { r.RegistryMirror = RegistryMirror{Provider: "aliyun"} },
	}
	for i, change := range tests {
		r := freshRequest(3)
		change(&r)
		if _, err := makePlan(&r, true); err == nil {
			t.Fatalf("accepted invalid request %d", i)
		}
	}
	r := freshRequest(2)
	r.Mode = "ha"
	if _, err := makePlan(&r, true); err == nil {
		t.Fatal("accepted two-node HA")
	}
}

func TestInstallerPinsReleaseAndSeparatesRoles(t *testing.T) {
	r := freshRequest(4)
	p, _ := makePlan(&r, true)
	for i := range r.Hosts {
		script := installScript(&r, p, i, "private-join-token")
		if !strings.Contains(script, "INSTALL_K3S_VERSION='v1.31.4+k3s1'") {
			t.Fatal("release is not pinned")
		}
		if strings.Contains(script, r.Hosts[i].Password) || strings.Contains(script, "private-join-token") {
			t.Fatal("credentials exposed in script text")
		}
		encoded := strings.Split(strings.Split(script, "printf %s '")[1], "'")[0]
		data, err := base64.StdEncoding.DecodeString(encoded)
		var cfg map[string]any
		if err != nil || json.Unmarshal(data, &cfg) != nil || cfg["token"] != "private-join-token" {
			t.Fatal("join configuration missing")
		}
		if (cfg["cluster-init"] == true) != (i == 0) {
			t.Fatal("only first HA server may bootstrap etcd")
		}
		if i > 0 && cfg["server"] != "https://10.0.0.1:6443" {
			t.Fatal("joining node lacks the bootstrap server")
		}
		if !strings.Contains(script, "test ! -e /etc/rancher/k3s") {
			t.Fatal("installer can overwrite an existing installation")
		}
	}
}

func TestBoundedOutput(t *testing.T) {
	b := &limitedBuffer{n: 4}
	if n, err := b.Write([]byte("123456")); err != nil || n != 6 || b.b.String() != "1234" {
		t.Fatal("output must consume writes without retaining unbounded data")
	}
}
