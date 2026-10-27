package hosts

import (
	"context"
	"strings"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
)

const hostDiagnosticCommand = `set +e
{
	printf '[host]\n'
	uname -a
	uptime
	printf '[pressure]\n'
	cat /proc/pressure/cpu /proc/pressure/memory /proc/pressure/io 2>/dev/null
	printf '[filesystems]\n'
	df -Pk / /var 2>/dev/null
	printf '[services]\n'
	for unit in kubelet k3s k3s-agent containerd; do
		printf '%s=' "$unit"
		systemctl is-active "$unit" 2>/dev/null || true
	done
	printf '[top_processes]\n'
	ps -eo comm,pcpu,pmem --sort=-pcpu 2>/dev/null | head -n 11
	printf '[recent_kernel_warnings]\n'
	journalctl -k -p warning --since '-10 minutes' -n 20 --no-pager -o short-iso 2>/dev/null
} 2>&1 | head -c 12000`

func (m *Module) diagnosticHost(ctx context.Context, env string, nodeIPs []string) (*Host, string, error) {
	ips := make([]string, 0, len(nodeIPs))
	seen := map[string]struct{}{}
	for _, ip := range nodeIPs {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		if _, ok := seen[ip]; ok {
			continue
		}
		seen[ip] = struct{}{}
		ips = append(ips, ip)
	}
	if strings.TrimSpace(env) == "" || len(ips) == 0 || m.st == nil || m.st.db == nil {
		return nil, "unavailable", nil
	}
	rows, err := m.st.db.Query(ctx, "SELECT "+hostCols+" FROM hosts_hosts WHERE env=$1 AND ip=ANY($2::text[])", env, ips)
	if err != nil {
		return nil, "unavailable", err
	}
	defer rows.Close()
	matches := make([]*Host, 0, 2)
	for rows.Next() {
		host, scanErr := scanHost(rows)
		if scanErr != nil {
			return nil, "unavailable", scanErr
		}
		matches = append(matches, host)
	}
	if err := rows.Err(); err != nil {
		return nil, "unavailable", err
	}
	host, status := chooseDiagnosticHost(matches, env, ips)
	if status != "available" {
		return nil, status, nil
	}
	if !host.Enrolled {
		return nil, "not_registered", nil
	}
	if host.LastSeenAt == nil {
		return nil, "offline", nil
	}
	age := time.Since(*host.LastSeenAt)
	if age < -30*time.Second || age >= onlineWindow {
		return nil, "stale", nil
	}
	if !host.Online || m.hub == nil || !m.hub.Connected(host.ID) {
		return nil, "offline", nil
	}
	return host, "available", nil
}

func (m *Module) Snapshot(ctx context.Context, env string, nodeIPs []string) (app.HostDiagnosticLookup, error) {
	host, status, err := m.diagnosticHost(ctx, env, nodeIPs)
	if err != nil || status != "available" {
		return app.HostDiagnosticLookup{Status: status}, err
	}

	facts := host.Facts
	disks := make([]app.HostDiagnosticDisk, 0, min(len(facts.Disks), 8))
	for _, disk := range facts.Disks {
		if len(disks) == 8 {
			break
		}
		disks = append(disks, app.HostDiagnosticDisk{
			Mount: disk.Mount, FSType: disk.FSType, Total: disk.Total, Used: disk.Used, Percent: disk.Percent,
		})
	}
	return app.HostDiagnosticLookup{
		Status: "available",
		Snapshot: app.HostDiagnosticSnapshot{
			Name: host.Name, IP: host.IP, AgentVersion: host.AgentVersion, LastSeenAt: *host.LastSeenAt,
			OS: facts.OS, Platform: facts.Platform, PlatformVer: facts.PlatformVersion, Kernel: facts.Kernel,
			Arch: facts.Arch, CPUCores: facts.CPUCores, CPUPercent: facts.CPUPercent, Load1: facts.Load1,
			MemTotal: facts.MemTotal, MemUsed: facts.MemUsed, MemPercent: facts.MemPercent,
			Uptime: facts.Uptime, Disks: disks,
		},
	}, nil
}

func (m *Module) Inspect(ctx context.Context, env string, nodeIPs []string) (app.HostDiagnosticInspection, error) {
	host, status, err := m.diagnosticHost(ctx, env, nodeIPs)
	if err != nil || status != "available" {
		return app.HostDiagnosticInspection{Status: status}, err
	}
	if !strings.EqualFold(strings.TrimSpace(host.Facts.OS), "linux") {
		return app.HostDiagnosticInspection{Status: "unsupported"}, nil
	}
	output, err := m.hub.runDiagnostic(ctx, host.ID, 20*time.Second)
	if err != nil {
		return app.HostDiagnosticInspection{Status: "unavailable"}, err
	}
	if len(output) > 12000 {
		output = output[:12000]
	}
	return app.HostDiagnosticInspection{Status: "available", CapturedAt: time.Now().UTC(), Output: output}, nil
}

func chooseDiagnosticHost(matches []*Host, env string, nodeIPs []string) (*Host, string) {
	allowed := make(map[string]struct{}, len(nodeIPs))
	for _, ip := range nodeIPs {
		allowed[ip] = struct{}{}
	}
	exact := make([]*Host, 0, len(matches))
	seen := map[int64]struct{}{}
	for _, host := range matches {
		if host == nil || host.Env != env {
			continue
		}
		if _, ok := allowed[host.IP]; !ok {
			continue
		}
		if _, ok := seen[host.ID]; ok {
			continue
		}
		seen[host.ID] = struct{}{}
		exact = append(exact, host)
	}
	switch len(exact) {
	case 0:
		return nil, "not_registered"
	case 1:
		return exact[0], "available"
	default:
		return nil, "ambiguous"
	}
}

var _ app.HostDiagnosticProvider = (*Module)(nil)
var _ app.HostDiagnosticInspectionProvider = (*Module)(nil)
