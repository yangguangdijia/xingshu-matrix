// Package provision installs fresh K3s clusters over pinned SSH connections.
package provision

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

type Host struct {
	IP          string `json:"ip"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	Password    string `json:"password,omitempty"`
	Fingerprint string `json:"fingerprint"`
}

type Components struct {
	Traefik      bool `json:"traefik"`
	ServiceLB    bool `json:"servicelb"`
	LocalStorage bool `json:"local_storage"`
	Metrics      bool `json:"metrics_server"`
}

type Request struct {
	Name           string         `json:"name"`
	Env            string         `json:"env"`
	Version        string         `json:"version"`
	Mode           string         `json:"mode"`
	Endpoint       string         `json:"endpoint"`
	CNI            string         `json:"cni"`
	RegistryMirror RegistryMirror `json:"registry_mirror"`
	Components     Components     `json:"components"`
	Hosts          []Host         `json:"hosts"`
	Confirmed      bool           `json:"confirmed"`
}

type PlannedHost struct {
	IP          string `json:"ip"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	Role        string `json:"role"`
	Fingerprint string `json:"fingerprint"`
}

type Plan struct {
	Method         string         `json:"method"`
	Version        string         `json:"version"`
	Masters        int            `json:"masters"`
	Workers        int            `json:"workers"`
	HA             bool           `json:"ha"`
	Endpoint       string         `json:"endpoint"`
	CNI            string         `json:"cni"`
	RegistryMirror RegistryMirror `json:"registry_mirror"`
	Components     Components     `json:"components"`
	Hosts          []PlannedHost  `json:"hosts"`
	Warnings       []string       `json:"warnings"`
}

var (
	nameRE    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	versionRE = regexp.MustCompile(`^v1\.[0-9]{1,2}\.[0-9]{1,3}\+k3s[1-9][0-9]?$`)
	userRE    = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]{0,63}$`)
	dnsRE     = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.-]{0,251}[a-zA-Z0-9])?$`)
)

func makePlan(r *Request, pinned bool) (*Plan, error) {
	r.Name, r.Env = strings.TrimSpace(r.Name), strings.TrimSpace(r.Env)
	if !nameRE.MatchString(r.Name) || r.Env == "" || len(r.Env) > 64 {
		return nil, fmt.Errorf("name must be a DNS label and environment is required")
	}
	if !versionRE.MatchString(r.Version) {
		return nil, fmt.Errorf("an explicit K3s release version is required, e.g. v1.x.y+k3sN")
	}
	if len(r.Hosts) < 1 || len(r.Hosts) > 30 {
		return nil, fmt.Errorf("provide 1 to 30 fresh Linux hosts")
	}
	if r.Mode == "" {
		r.Mode = "auto"
	}
	masters := 1
	switch r.Mode {
	case "auto":
		if len(r.Hosts) >= 3 {
			masters = 3
		}
	case "single":
	case "ha":
		if len(r.Hosts) < 3 {
			return nil, fmt.Errorf("HA needs at least three hosts")
		}
		masters = 3
	default:
		return nil, fmt.Errorf("mode must be auto, single or ha")
	}
	if r.CNI == "" {
		r.CNI = "vxlan"
	}
	if r.CNI != "vxlan" && r.CNI != "wireguard-native" {
		return nil, fmt.Errorf("supported CNI backends: vxlan, wireguard-native")
	}
	var err error
	r.RegistryMirror, err = normalizeRegistryMirror(r.RegistryMirror)
	if err != nil {
		return nil, err
	}
	r.Endpoint = strings.TrimSpace(r.Endpoint)
	if r.Endpoint == "" && masters == 1 {
		r.Endpoint = r.Hosts[0].IP
	}
	if r.Endpoint == "" || (net.ParseIP(r.Endpoint) == nil && (!dnsRE.MatchString(r.Endpoint) || strings.Contains(r.Endpoint, ".."))) {
		return nil, fmt.Errorf("provide an API endpoint IP or DNS name; HA requires a preconfigured load balancer/VIP on port 6443")
	}
	p := &Plan{Method: "k3s", Version: r.Version, Masters: masters, Workers: len(r.Hosts) - masters,
		HA: masters == 3, Endpoint: "https://" + net.JoinHostPort(r.Endpoint, "6443"),
		CNI: r.CNI, RegistryMirror: r.RegistryMirror, Components: r.Components, Hosts: []PlannedHost{},
		Warnings: []string{"Only fresh Linux systemd hosts are supported. Existing Kubernetes installations are never overwritten.",
			"Installation needs outbound HTTPS to K3s releases and container registries. Cancellation does not uninstall partially installed nodes.",
			"Allow TCP 6443/10250, server TCP 2379/2380 for HA, and Flannel UDP 8472 or WireGuard UDP 51820 between nodes."}}
	if r.RegistryMirror.Provider == "aliyun" {
		p.Warnings = append(p.Warnings, "Alibaba Cloud's Docker Hub accelerator is account-specific, has stopped syncing latest images, is limited to personal non-commercial use, and may return HTTP 403 outside Alibaba Cloud ECS.")
	}
	if r.RegistryMirror.Endpoint != "" {
		p.Warnings = append(p.Warnings, "The selected mirror applies to Docker Hub image pulls for newly provisioned nodes only; existing clusters and other registries are unchanged.")
		if strings.HasPrefix(r.RegistryMirror.Endpoint, "http://") {
			p.Warnings = append(p.Warnings, "The selected mirror uses unencrypted HTTP; image pulls can be intercepted or modified in transit.")
		}
	}
	if p.HA {
		p.Warnings = append(p.Warnings, "The API endpoint must already load-balance the three servers. Matrix does not configure your external load balancer.")
	}
	seen := map[string]bool{}
	for i := range r.Hosts {
		h := &r.Hosts[i]
		ip := net.ParseIP(strings.TrimSpace(h.IP))
		if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
			return nil, fmt.Errorf("host %d needs a routable IPv4 address; this installer does not configure dual-stack networking", i+1)
		}
		h.IP = ip.String()
		if seen[h.IP] {
			return nil, fmt.Errorf("duplicate host IP %s", h.IP)
		}
		seen[h.IP] = true
		if h.Port == 0 {
			h.Port = 22
		}
		if h.Port < 1 || h.Port > 65535 || !userRE.MatchString(h.Username) || h.Password == "" || len(h.Password) > 4096 {
			return nil, fmt.Errorf("host %d needs a valid SSH port, username and password", i+1)
		}
		if pinned && !strings.HasPrefix(h.Fingerprint, "SHA256:") {
			return nil, fmt.Errorf("run preflight and confirm the SSH fingerprint of host %d", i+1)
		}
		role := "worker"
		if i < masters {
			role = "master"
		}
		p.Hosts = append(p.Hosts, PlannedHost{h.IP, h.Port, h.Username, role, h.Fingerprint})
	}
	return p, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
