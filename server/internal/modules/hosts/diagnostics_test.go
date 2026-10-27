package hosts

import (
	"strings"
	"testing"
)

func TestChooseDiagnosticHostRequiresUniqueExactMatch(t *testing.T) {
	host := &Host{ID: 1, IP: "10.0.0.8", Env: "prod"}
	if got, status := chooseDiagnosticHost([]*Host{{ID: 1, IP: "10.0.0.80", Env: "prod"}}, "prod", []string{"10.0.0.8"}); got != nil || status != "not_registered" {
		t.Fatalf("similar IP must not match: %v %q", got, status)
	}
	if got, status := chooseDiagnosticHost([]*Host{host}, "prod", []string{"10.0.0.8"}); got != host || status != "available" {
		t.Fatalf("unique exact-IP host should be selected: %v %q", got, status)
	}
	if got, status := chooseDiagnosticHost([]*Host{host}, "staging", []string{"10.0.0.8"}); got != nil || status != "not_registered" {
		t.Fatalf("different environment must not match: %v %q", got, status)
	}
	other := &Host{ID: 2, IP: "10.0.0.8", Env: "prod"}
	if got, status := chooseDiagnosticHost([]*Host{host, other}, "prod", []string{"10.0.0.8"}); got != nil || status != "ambiguous" {
		t.Fatalf("ambiguous host mapping must not select either host: %v %q", got, status)
	}
}

func TestHostDiagnosticCommandUsesOnlyFixedReadOnlyChecks(t *testing.T) {
	for _, check := range []string{
		"uname -a", "uptime", "/proc/pressure/cpu", "df -Pk / /var",
		"systemctl is-active", "ps -eo comm,pcpu,pmem", "journalctl -k -p warning",
		"head -c 12000",
	} {
		if !strings.Contains(hostDiagnosticCommand, check) {
			t.Errorf("fixed diagnostic command is missing %q", check)
		}
	}
	for _, forbidden := range []string{"rm -", "systemctl restart", "kubectl", "curl ", "ssh ", "${", "`"} {
		if strings.Contains(hostDiagnosticCommand, forbidden) {
			t.Errorf("diagnostic command contains unexpected operation %q", forbidden)
		}
	}
}
