package automation

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

func TestParseStepsRejectsUnknownAndUnsafeInputs(t *testing.T) {
	for _, raw := range []string{
		`[{"type":"ansible.shell"}]`,
		`[{"type":"ansible.directory","path":"/etc/cron.d"}]`,
		`[{"type":"ansible.directory","path":"/opt/../etc"}]`,
		`[{"type":"ansible.ping","extra":"field"}]`,
		`[] []`,
	} {
		if _, err := parseSteps(json.RawMessage(raw)); err == nil {
			t.Errorf("parseSteps(%s) unexpectedly succeeded", raw)
		}
	}
	if _, err := parseSteps(json.RawMessage(`[{"type":"ansible.directory","path":"/opt/matrix","mode":"0750"}]`)); err != nil {
		t.Fatalf("valid directory step rejected: %v", err)
	}
}

func TestValidatePlaybookAllowlistAndTargetRestrictions(t *testing.T) {
	valid := `- name: configure
  hosts: all
  gather_facts: false
  tasks:
    - name: check connectivity
      ansible.builtin.ping: {}
    - block:
        - name: create directory
          ansible.builtin.file:
            path: /opt/matrix
            state: directory
      rescue:
        - name: report failure
          ansible.builtin.fail:
            msg: task failed
  handlers:
    - name: reload service
      ansible.builtin.service:
        name: nginx
        state: reloaded
`
	if err := validatePlaybook(valid); err != nil {
		t.Fatalf("valid playbook rejected: %v", err)
	}

	invalid := []string{
		strings.Replace(valid, "hosts: all", "hosts: localhost", 1),
		strings.Replace(valid, "gather_facts: false", "connection: local", 1),
		`- hosts: all
  tasks:
    - ansible.builtin.command:
        cmd: id
      delegate_to: localhost`,
		`- hosts: all
  tasks:
    - ansible.builtin.debug:
        msg: "{{ lookup  ('pipe', 'id') }}"`,
		`- hosts: all
  vars:
    ansible_connection: local
  tasks: []`,
		`- hosts: all
  tasks:
    - ansible.builtin.foo: {}`,
		`- hosts: all
  tasks: []
  handlers: invalid`,
		`- hosts: all
  tasks: &shared
    - ansible.builtin.ping: {}
  handlers: *shared`,
	}
	for _, raw := range invalid {
		if err := validatePlaybook(raw); err == nil {
			t.Errorf("unsafe or unsupported playbook unexpectedly accepted:\n%s", raw)
		}
	}
	if err := validatePlaybook(strings.Repeat("x", maxAnsiblePlaybookBytes+1)); err == nil {
		t.Fatal("oversized playbook unexpectedly accepted")
	}
}

func TestPreflightChallengeIsOpaqueAndOneUseTokenIsHashable(t *testing.T) {
	first, err := makePreflightChallenge(ansibleRunPayload{Targets: []sshTarget{{Password: "do-not-return"}}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := makePreflightChallenge(ansibleRunPayload{Targets: []sshTarget{{Password: "do-not-return"}}})
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == second.Token || first.TokenHash == second.TokenHash {
		t.Fatal("preflight confirmation tokens must be unique")
	}
	got, err := hashPreflightToken(first.Token)
	if err != nil || got != first.TokenHash {
		t.Fatalf("token hash mismatch: got %q, err %v", got, err)
	}
	if _, err := hashPreflightToken("not-a-token"); err == nil {
		t.Fatal("malformed preflight token accepted")
	}
	serialized, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(first.Token, "do-not-return") {
		t.Fatal("confirmation token must be opaque")
	}
	if !strings.Contains(string(serialized), "do-not-return") {
		t.Fatal("server-side challenge must retain the SSH credentials until they are encrypted")
	}
}

func TestInventoryUsesPreflightVerifiedHostKeyAndKeepsOutputInternal(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, err := ssh.NewPublicKey(private.Public())
	if err != nil {
		t.Fatal(err)
	}
	key := public.Type() + " " + base64.StdEncoding.EncodeToString(public.Marshal())
	target := sshTarget{
		IP: "192.0.2.10", Port: 2222, Username: "deploy", Password: "inventory-only-secret",
		BecomePassword: "sudo-only-secret", PublicKey: key, Fingerprint: ssh.FingerprintSHA256(public),
	}
	inventoryBytes, knownHosts, err := inventoryAndKnownHosts(ansibleRunPayload{Targets: []sshTarget{target}})
	if err != nil {
		t.Fatal(err)
	}
	var inventory inventoryFile
	if err := yaml.Unmarshal(inventoryBytes, &inventory); err != nil {
		t.Fatal(err)
	}
	host := inventory.All.Hosts["target-01"]
	if host["ansible_password"] != target.Password || host["ansible_become_password"] != target.BecomePassword {
		t.Fatal("SSH and sudo credentials were not passed to the private inventory")
	}
	if strings.Contains(string(knownHosts), target.Password) || !strings.Contains(string(knownHosts), "[192.0.2.10]:2222 "+key) {
		t.Fatal("known_hosts should contain only the verified endpoint key")
	}
	target.Fingerprint = "SHA256:client-submitted-value"
	if _, _, err := inventoryAndKnownHosts(ansibleRunPayload{Targets: []sshTarget{target}}); err == nil {
		t.Fatal("a mismatched host-key fingerprint was accepted")
	}
}

func TestApprovalPayloadContainsRunIDOnly(t *testing.T) {
	data, err := json.Marshal(ansibleApprovalPayload(42))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"run_id":42}` {
		t.Fatalf("approval payload unexpectedly contains more than the run ID: %s", data)
	}
}

func TestWorkflowReadRedactsLiteralSecrets(t *testing.T) {
	steps := redactAutomationJSON(json.RawMessage(`[{"type":"noop","name":"token=step-secret"}]`))
	playbook := redactAutomationPlaybook(`- hosts: all
  vars:
    api_token: playbook-secret
  tasks:
    - ansible.builtin.debug:
        msg: "Authorization: Bearer bearer-secret"
`)
	for _, secret := range []string{"step-secret", "playbook-secret", "bearer-secret"} {
		if strings.Contains(string(steps), secret) || strings.Contains(playbook, secret) {
			t.Fatalf("workflow read exposed %q: steps=%s playbook=%s", secret, steps, playbook)
		}
	}
	if !strings.Contains(playbook, "[REDACTED]") || !strings.Contains(string(steps), "[REDACTED]") {
		t.Fatalf("expected redaction markers: steps=%s playbook=%s", steps, playbook)
	}
}

func TestWorkflowConfigRejectsInlineSecretsAndAcceptsReferences(t *testing.T) {
	for _, playbook := range []string{
		`- hosts: all
  vars:
    api_token: plain-token
  tasks:
    - ansible.builtin.debug:
        msg: ok
`,
		`- hosts: all
  tasks:
    - ansible.builtin.debug:
        msg: "password=plain-password"
`,
	} {
		if err := validateAutomationSecrets(json.RawMessage(`[]`), playbook, "workflow", "", ""); err == nil {
			t.Fatalf("inline secret was accepted in playbook: %s", playbook)
		}
	}
	if err := validateAutomationSecrets(json.RawMessage(`[{"type":"noop","name":"token=plain-token"}]`), "", "workflow", "", ""); err == nil {
		t.Fatal("inline secret was accepted in workflow steps")
	}
	if err := validateAutomationSecrets(json.RawMessage(`[]`), `- hosts: all
  vars:
    api_token: "{{ vault_api_token }}"
  tasks:
    - ansible.builtin.debug:
        msg: "token={{ vault_api_token }}"
`, "workflow", "", ""); err != nil {
		t.Fatalf("secret references were rejected: %v", err)
	}
}
