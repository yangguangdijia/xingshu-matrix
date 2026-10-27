package provision

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestNormalizeRegistryMirror(t *testing.T) {
	tests := []struct {
		name    string
		input   RegistryMirror
		want    RegistryMirror
		wantErr bool
	}{
		{name: "default direct", want: RegistryMirror{Provider: "dockerhub"}},
		{name: "DaoCloud preset", input: RegistryMirror{Provider: "daocloud"}, want: RegistryMirror{Provider: "daocloud", Endpoint: "https://m.daocloud.io"}},
		{name: "Alibaba endpoint", input: RegistryMirror{Provider: "aliyun", Endpoint: "https://tenant.mirror.aliyuncs.com"}, want: RegistryMirror{Provider: "aliyun", Endpoint: "https://tenant.mirror.aliyuncs.com"}},
		{name: "normalize endpoint scheme", input: RegistryMirror{Provider: "custom", Endpoint: "HTTPS://registry.example"}, want: RegistryMirror{Provider: "custom", Endpoint: "https://registry.example"}},
		{name: "Ali requires account endpoint", input: RegistryMirror{Provider: "aliyun"}, wantErr: true},
		{name: "custom requires endpoint", input: RegistryMirror{Provider: "custom"}, wantErr: true},
		{name: "reject credentials", input: RegistryMirror{Provider: "custom", Endpoint: "https://user:pass@registry.example"}, wantErr: true},
		{name: "reject query", input: RegistryMirror{Provider: "custom", Endpoint: "https://registry.example?token=x"}, wantErr: true},
		{name: "reject unsupported scheme", input: RegistryMirror{Provider: "custom", Endpoint: "file:///etc/passwd"}, wantErr: true},
		{name: "direct cannot have endpoint", input: RegistryMirror{Provider: "dockerhub", Endpoint: "https://registry.example"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeRegistryMirror(test.input)
			if (err != nil) != test.wantErr {
				t.Fatalf("normalizeRegistryMirror() error = %v, wantErr %t", err, test.wantErr)
			}
			if err == nil && got != test.want {
				t.Fatalf("normalizeRegistryMirror() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestRegistryMirrorYAML(t *testing.T) {
	data, err := registryMirrorYAML(RegistryMirror{Provider: "daocloud"})
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Mirrors map[string]struct {
			Endpoint []string          `yaml:"endpoint"`
			Rewrite  map[string]string `yaml:"rewrite"`
		} `yaml:"mirrors"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	dockerHub := config.Mirrors["docker.io"]
	if len(dockerHub.Endpoint) != 1 || dockerHub.Endpoint[0] != "https://m.daocloud.io" {
		t.Fatalf("unexpected Docker Hub endpoint: %+v", dockerHub.Endpoint)
	}
	if dockerHub.Rewrite["^(.*)$"] != "docker.io/$1" {
		t.Fatalf("DaoCloud path rewrite missing: %+v", dockerHub.Rewrite)
	}
	if direct, err := registryMirrorYAML(RegistryMirror{}); err != nil || direct != nil {
		t.Fatalf("direct mode should not write registries.yaml, got %q, err=%v", direct, err)
	}
}

func TestInstallerWritesMirrorConfigBeforeK3sStart(t *testing.T) {
	r := freshRequest(1)
	r.RegistryMirror = RegistryMirror{Provider: "daocloud"}
	p, err := makePlan(&r, true)
	if err != nil {
		t.Fatal(err)
	}
	script := installScript(&r, p, 0, "join-token")
	mirrorPos := strings.Index(script, "registries.yaml")
	installerPos := strings.Index(script, "curl --connect-timeout")
	if mirrorPos < 0 || installerPos < 0 || mirrorPos > installerPos {
		t.Fatalf("registry mirror must be written before K3s starts:\n%s", script)
	}
}
