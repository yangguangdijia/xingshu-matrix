package provision

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAnsibleInventoryAndPinnedKnownHosts(t *testing.T) {
	host := Host{IP: "10.0.0.5", Port: 2222, Username: "deploy", Password: "private-password"}
	data, err := buildAnsibleInventory(host)
	if err != nil {
		t.Fatal(err)
	}
	var inventory ansibleInventory
	if err := yaml.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	target := inventory.All.Hosts["target"]
	if target.Address != host.IP || target.Port != host.Port || target.User != host.Username ||
		target.Password != host.Password || !target.Become {
		t.Fatalf("inventory lost SSH settings: %+v", target)
	}
	if got := knownHostsEntry(host, "ssh-ed25519 AAAA"); got != "[10.0.0.5]:2222 ssh-ed25519 AAAA\n" {
		t.Fatalf("known_hosts entry = %q", got)
	}
	root := host
	root.Username = "root"
	rootInventory, err := buildAnsibleInventory(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(rootInventory, &inventory); err != nil {
		t.Fatal(err)
	}
	if inventory.All.Hosts["target"].Become {
		t.Fatal("root inventory should not require sudo")
	}
	if !strings.Contains(installPlaybook, "ansible.builtin.raw") || !strings.Contains(installPlaybook, "no_log: true") {
		t.Fatal("bootstrap playbook must run without target-side Python and hide command output")
	}
}
