package provision

import (
	"fmt"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
)

const registryMirrorSettingKey = "provision.registry_mirror"

type RegistryMirror struct {
	Provider string `json:"provider"`
	Endpoint string `json:"endpoint"`
}

func normalizeRegistryMirror(m RegistryMirror) (RegistryMirror, error) {
	m.Provider = strings.ToLower(strings.TrimSpace(m.Provider))
	m.Endpoint = strings.TrimSpace(m.Endpoint)
	if m.Provider == "" {
		m.Provider = "dockerhub"
	}
	switch m.Provider {
	case "dockerhub":
		if m.Endpoint != "" {
			return RegistryMirror{}, fmt.Errorf("Docker Hub direct mode does not take a mirror endpoint")
		}
	case "aliyun":
		if m.Endpoint == "" {
			return RegistryMirror{}, fmt.Errorf("enter the accelerator endpoint assigned by your Alibaba Cloud account")
		}
	case "daocloud":
		if m.Endpoint == "" {
			m.Endpoint = "https://m.daocloud.io"
		}
	case "custom":
		if m.Endpoint == "" {
			return RegistryMirror{}, fmt.Errorf("enter a custom mirror endpoint")
		}
	default:
		return RegistryMirror{}, fmt.Errorf("provider must be dockerhub, aliyun, daocloud or custom")
	}
	if m.Endpoint != "" {
		u, err := url.Parse(m.Endpoint)
		if err != nil || (strings.ToLower(u.Scheme) != "https" && strings.ToLower(u.Scheme) != "http") || u.Hostname() == "" ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(m.Endpoint, "\r\n\t ") {
			return RegistryMirror{}, fmt.Errorf("mirror endpoint must be an HTTP(S) base URL without credentials, query or fragment")
		}
		u.Scheme = strings.ToLower(u.Scheme)
		m.Endpoint = strings.TrimRight(u.String(), "/")
	}
	return m, nil
}

type registryMirrorConfig struct {
	Mirrors map[string]registryMirrorEntry `yaml:"mirrors"`
}

type registryMirrorEntry struct {
	Endpoint []string          `yaml:"endpoint"`
	Rewrite  map[string]string `yaml:"rewrite,omitempty"`
}

func registryMirrorYAML(m RegistryMirror) ([]byte, error) {
	m, err := normalizeRegistryMirror(m)
	if err != nil {
		return nil, err
	}
	if m.Endpoint == "" {
		return nil, nil
	}
	entry := registryMirrorEntry{Endpoint: []string{m.Endpoint}}
	if m.Provider == "daocloud" {
		entry.Rewrite = map[string]string{"^(.*)$": "docker.io/$1"}
	}
	return yaml.Marshal(registryMirrorConfig{
		Mirrors: map[string]registryMirrorEntry{"docker.io": entry},
	})
}
