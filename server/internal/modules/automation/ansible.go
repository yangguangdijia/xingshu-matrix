package automation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"

	ansibleruntime "github.com/yangguangdijia/xingshu-matrix/server/internal/ansible"
)

const maxAnsibleTargets = 20
const maxAnsiblePlaybookBytes = 256 * 1024

var (
	linuxNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)
	packageRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+_.:@-]{0,127}$`)
	modeRE      = regexp.MustCompile(`^[0-7]{3,4}$`)
	envNameRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)
	lookupRE    = regexp.MustCompile(`(?i)\b(?:lookup|query)\s*\(`)
)

var allowedBuiltinModules = map[string]bool{
	"ansible.builtin.apt": true, "ansible.builtin.assert": true, "ansible.builtin.command": true,
	"ansible.builtin.copy": true, "ansible.builtin.cron": true, "ansible.builtin.debug": true,
	"ansible.builtin.dnf": true, "ansible.builtin.fail": true, "ansible.builtin.file": true,
	"ansible.builtin.group": true, "ansible.builtin.hostname": true, "ansible.builtin.lineinfile": true,
	"ansible.builtin.package": true, "ansible.builtin.ping": true, "ansible.builtin.raw": true,
	"ansible.builtin.reboot": true, "ansible.builtin.replace": true, "ansible.builtin.service": true,
	"ansible.builtin.set_fact": true, "ansible.builtin.setup": true, "ansible.builtin.shell": true,
	"ansible.builtin.stat": true, "ansible.builtin.sysctl": true, "ansible.builtin.systemd": true,
	"ansible.builtin.systemd_service": true, "ansible.builtin.uri": true, "ansible.builtin.user": true,
	"ansible.builtin.wait_for": true, "ansible.builtin.yum": true,
}

type workflowStep struct {
	Type    string `json:"type"`
	Name    string `json:"name,omitempty"`
	Package string `json:"package,omitempty"`
	Service string `json:"service,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
	Path    string `json:"path,omitempty"`
	Mode    string `json:"mode,omitempty"`
}

type sshTarget struct {
	IP             string `json:"ip"`
	Port           int    `json:"port"`
	Username       string `json:"username"`
	Password       string `json:"password"`
	BecomePassword string `json:"become_password,omitempty"`
	PublicKey      string `json:"public_key"`
	Fingerprint    string `json:"fingerprint"`
}

type ansibleRunPayload struct {
	Targets []sshTarget `json:"targets"`
}

type ansiblePreflightChallenge struct {
	Token     string
	TokenHash string
	Payload   ansibleRunPayload
}

type ansiblePreflightInput struct {
	Env            string `json:"env" binding:"required"`
	IPs            string `json:"ips" binding:"required"`
	Port           int    `json:"port"`
	Username       string `json:"username" binding:"required"`
	Password       string `json:"password" binding:"required"`
	BecomePassword string `json:"become_password"`
}

type sshHostKey struct {
	IP          string `json:"ip"`
	Port        int    `json:"port"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
}

type inventoryFile struct {
	All struct {
		Hosts map[string]map[string]any `yaml:"hosts"`
	} `yaml:"all"`
}

func parseSteps(raw json.RawMessage) ([]workflowStep, error) {
	if len(raw) == 0 || len(raw) > 32*1024 {
		return nil, errors.New("steps must be a JSON array under 32 KiB")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var steps []workflowStep
	if err := dec.Decode(&steps); err != nil {
		return nil, errors.New("steps must be a JSON array of supported actions")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, errors.New("steps must contain one JSON value")
	}
	if len(steps) > 25 {
		return nil, errors.New("a workflow may contain at most 25 steps")
	}
	for i := range steps {
		step := &steps[i]
		step.Type = strings.TrimSpace(step.Type)
		step.Name = strings.TrimSpace(step.Name)
		switch step.Type {
		case "noop", "ansible.ping":
		case "ansible.package_present":
			if !packageRE.MatchString(step.Package) {
				return nil, fmt.Errorf("step %d requires a valid package name", i+1)
			}
		case "ansible.service_started":
			if !linuxNameRE.MatchString(step.Service) {
				return nil, fmt.Errorf("step %d requires a valid service name", i+1)
			}
		case "ansible.directory":
			if !path.IsAbs(step.Path) || path.Clean(step.Path) != step.Path || step.Path == "/" ||
				strings.Contains(step.Path, `\`) || !allowedDirectoryPath(step.Path) {
				return nil, fmt.Errorf("step %d requires a clean absolute directory path other than /", i+1)
			}
			if step.Mode != "" && !modeRE.MatchString(step.Mode) {
				return nil, fmt.Errorf("step %d has an invalid directory mode", i+1)
			}
		default:
			return nil, fmt.Errorf("step %d uses unsupported type %q", i+1, step.Type)
		}
		if len(step.Name) > 128 || strings.ContainsAny(step.Name, "\r\n") {
			return nil, fmt.Errorf("step %d has an invalid name", i+1)
		}
	}
	return steps, nil
}

func allowedDirectoryPath(value string) bool {
	for _, root := range []string{"/opt/", "/srv/", "/var/lib/", "/var/log/", "/tmp/", "/home/"} {
		if strings.HasPrefix(value, root) {
			return true
		}
	}
	return false
}

func hasAnsibleSteps(steps []workflowStep) bool {
	for _, step := range steps {
		if strings.HasPrefix(step.Type, "ansible.") {
			return true
		}
	}
	return false
}

func hasAnsibleDefinition(steps []workflowStep, playbook string) bool {
	return strings.TrimSpace(playbook) != "" || hasAnsibleSteps(steps)
}

func validatePlaybook(raw string) error {
	if len(strings.TrimSpace(raw)) == 0 || len(raw) > maxAnsiblePlaybookBytes {
		return errors.New("Ansible playbook must be between 1 byte and 256 KiB")
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &root); err != nil {
		return errors.New("invalid Ansible playbook YAML")
	}
	if containsYAMLAlias(&root) {
		return errors.New("YAML aliases and anchors are not supported")
	}
	var plays []map[string]any
	if err := yaml.Unmarshal([]byte(raw), &plays); err != nil || len(plays) == 0 || len(plays) > maxAnsibleTargets {
		return errors.New("playbook must contain between 1 and 20 plays")
	}
	for index, play := range plays {
		for key := range play {
			switch key {
			case "name", "hosts", "gather_facts", "tasks", "handlers", "become", "become_user",
				"become_method", "vars", "tags", "strategy", "serial", "max_fail_percentage",
				"any_errors_fatal", "force_handlers", "order":
			default:
				return fmt.Errorf("play %d uses unsupported play setting %q", index+1, key)
			}
		}
		hosts, ok := play["hosts"].(string)
		if !ok || hosts != "all" {
			return fmt.Errorf("play %d must target hosts: all", index+1)
		}
		tasks, ok := play["tasks"].([]any)
		if !ok {
			return fmt.Errorf("play %d requires a tasks list", index+1)
		}
		if err := validateTaskList(tasks, index+1); err != nil {
			return err
		}
		if handlers, ok := play["handlers"].([]any); ok {
			if err := validateTaskList(handlers, index+1); err != nil {
				return err
			}
		} else if _, exists := play["handlers"]; exists {
			return fmt.Errorf("play %d handlers must be a task list", index+1)
		}
		if containsControllerLookup(play) {
			return errors.New("controller-side lookup expressions are not supported")
		}
	}
	return nil
}

func containsYAMLAlias(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.AliasNode || node.Anchor != "" {
		return true
	}
	for _, child := range node.Content {
		if containsYAMLAlias(child) {
			return true
		}
	}
	return false
}

var allowedTaskFields = map[string]bool{
	"name": true, "when": true, "tags": true, "become": true, "become_user": true,
	"become_method": true, "register": true, "changed_when": true, "failed_when": true,
	"ignore_errors": true, "no_log": true, "notify": true, "loop": true, "loop_control": true,
	"until": true, "retries": true, "delay": true, "vars": true, "block": true,
	"rescue": true, "always": true, "check_mode": true, "run_once": true,
}

func validateTaskList(tasks []any, play int) error {
	if len(tasks) > 200 {
		return fmt.Errorf("play %d has more than 200 tasks", play)
	}
	for i, raw := range tasks {
		task, ok := raw.(map[string]any)
		if !ok || len(task) == 0 {
			return fmt.Errorf("play %d task %d must be a mapping", play, i+1)
		}
		actions := 0
		for key, value := range task {
			if allowedTaskFields[key] {
				if key == "block" || key == "rescue" || key == "always" {
					block, ok := value.([]any)
					if !ok {
						return fmt.Errorf("play %d task %d %s must be a task list", play, i+1, key)
					}
					if err := validateTaskList(block, play); err != nil {
						return err
					}
				}
				continue
			}
			if allowedBuiltinModules[key] {
				actions++
				continue
			}
			return fmt.Errorf("play %d task %d uses unsupported action or task setting %q", play, i+1, key)
		}
		if actions != 1 && !(actions == 0 && hasTaskBlock(task)) {
			return fmt.Errorf("play %d task %d must use exactly one ansible.builtin module", play, i+1)
		}
		if containsControllerLookup(task) {
			return errors.New("controller-side lookup expressions are not supported")
		}
	}
	return nil
}

func hasTaskBlock(task map[string]any) bool {
	return task["block"] != nil || task["rescue"] != nil || task["always"] != nil
}

func containsControllerLookup(value any) bool {
	switch typed := value.(type) {
	case string:
		return lookupRE.MatchString(typed)
	case []any:
		for _, item := range typed {
			if containsControllerLookup(item) {
				return true
			}
		}
	case map[string]any:
		for key, item := range typed {
			if strings.HasPrefix(strings.ToLower(key), "ansible_") || containsControllerLookup(item) {
				return true
			}
		}
	}
	return false
}

func parsePreflightInput(in ansiblePreflightInput) (string, []sshTarget, error) {
	in.Env = strings.TrimSpace(in.Env)
	in.Username = strings.TrimSpace(in.Username)
	if !envNameRE.MatchString(in.Env) {
		return "", nil, errors.New("environment name is required and must be a valid environment identifier")
	}
	if !linuxNameRE.MatchString(in.Username) {
		return "", nil, errors.New("invalid SSH username")
	}
	if len(in.Password) == 0 || len(in.Password) > 1024 || len(in.BecomePassword) > 1024 {
		return "", nil, errors.New("SSH and sudo passwords must be at most 1024 characters")
	}
	if in.Port == 0 {
		in.Port = 22
	}
	if in.Port < 1 || in.Port > 65535 {
		return "", nil, errors.New("SSH port must be between 1 and 65535")
	}
	parts := strings.FieldsFunc(in.IPs, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	if len(parts) == 0 || len(parts) > maxAnsibleTargets {
		return "", nil, fmt.Errorf("provide between 1 and %d target IPs", maxAnsibleTargets)
	}
	targets := make([]sshTarget, 0, len(parts))
	seen := map[string]bool{}
	for _, rawIP := range parts {
		ip := net.ParseIP(strings.TrimSpace(rawIP))
		if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
			return "", nil, fmt.Errorf("invalid or unsupported target IP %q", rawIP)
		}
		canonical := ip.String()
		if seen[canonical] {
			return "", nil, fmt.Errorf("duplicate target IP %q", canonical)
		}
		seen[canonical] = true
		targets = append(targets, sshTarget{
			IP: canonical, Port: in.Port, Username: in.Username, Password: in.Password,
			BecomePassword: in.BecomePassword,
		})
	}
	return in.Env, targets, nil
}

func captureHostKey(ctx context.Context, target sshTarget) (sshHostKey, error) {
	addr := net.JoinHostPort(target.IP, strconv.Itoa(target.Port))
	var publicKey ssh.PublicKey
	cfg := &ssh.ClientConfig{
		User: target.Username, Auth: []ssh.AuthMethod{ssh.Password(target.Password)},
		Timeout: 10 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			publicKey = key
			return nil
		},
	}
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return sshHostKey{}, errors.New("SSH connection failed")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(12 * time.Second))
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	clientConn, channels, requests, err := ssh.NewClientConn(conn, addr, cfg)
	stop()
	if err != nil {
		return sshHostKey{}, errors.New("SSH authentication or host key preflight failed")
	}
	defer clientConn.Close()
	client := ssh.NewClient(clientConn, channels, requests)
	defer client.Close()
	if publicKey == nil {
		return sshHostKey{}, errors.New("SSH host key was not captured")
	}
	return sshHostKey{
		IP: target.IP, Port: target.Port,
		PublicKey:   publicKey.Type() + " " + base64.StdEncoding.EncodeToString(publicKey.Marshal()),
		Fingerprint: ssh.FingerprintSHA256(publicKey),
	}, nil
}

func makePreflightChallenge(payload ansibleRunPayload) (*ansiblePreflightChallenge, error) {
	var tokenBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return nil, errors.New("could not create SSH confirmation token")
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes[:])
	sum := sha256.Sum256([]byte(token))
	return &ansiblePreflightChallenge{
		Token: token, TokenHash: hex.EncodeToString(sum[:]), Payload: payload,
	}, nil
}

func hashPreflightToken(token string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		return "", errors.New("invalid SSH preflight token")
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:]), nil
}

func inventoryAndKnownHosts(payload ansibleRunPayload) ([]byte, []byte, error) {
	if len(payload.Targets) == 0 || len(payload.Targets) > maxAnsibleTargets {
		return nil, nil, errors.New("invalid Ansible target count")
	}
	inventory := inventoryFile{}
	inventory.All.Hosts = make(map[string]map[string]any, len(payload.Targets))
	var knownHosts strings.Builder
	for i, target := range payload.Targets {
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(target.PublicKey))
		if err != nil || ssh.FingerprintSHA256(key) != target.Fingerprint {
			return nil, nil, errors.New("SSH host key confirmation is invalid")
		}
		ip := net.ParseIP(target.IP)
		if target.Port < 1 || target.Port > 65535 || ip == nil || ip.IsUnspecified() || ip.IsLoopback() ||
			ip.IsMulticast() || ip.IsLinkLocalUnicast() || !linuxNameRE.MatchString(target.Username) ||
			target.Password == "" || len(target.Password) > 1024 || len(target.BecomePassword) > 1024 {
			return nil, nil, errors.New("invalid Ansible SSH target")
		}
		name := fmt.Sprintf("target-%02d", i+1)
		host := map[string]any{
			"ansible_host": target.IP, "ansible_port": target.Port, "ansible_user": target.Username,
			"ansible_password": target.Password, "ansible_become": target.Username != "root",
			"ansible_ssh_common_args": "-o PreferredAuthentications=password",
		}
		if target.Username != "root" && target.BecomePassword != "" {
			host["ansible_become_password"] = target.BecomePassword
		}
		inventory.All.Hosts[name] = host
		hostName := target.IP
		if target.Port != 22 {
			hostName = "[" + target.IP + "]:" + strconv.Itoa(target.Port)
		}
		knownHosts.WriteString(hostName + " " + key.Type() + " " + base64.StdEncoding.EncodeToString(key.Marshal()) + "\n")
	}
	data, err := yaml.Marshal(inventory)
	if err != nil {
		return nil, nil, errors.New("could not encode Ansible inventory")
	}
	return data, []byte(knownHosts.String()), nil
}

func buildPlaybook(steps []workflowStep) ([]byte, error) {
	tasks := make([]map[string]any, 0, len(steps))
	for i, step := range steps {
		if step.Type == "noop" {
			continue
		}
		name := step.Name
		if name == "" {
			name = fmt.Sprintf("Matrix step %d", i+1)
		}
		task := map[string]any{"name": name}
		switch step.Type {
		case "ansible.ping":
			task["ansible.builtin.ping"] = map[string]any{}
		case "ansible.package_present":
			task["ansible.builtin.package"] = map[string]any{"name": step.Package, "state": "present"}
		case "ansible.service_started":
			args := map[string]any{"name": step.Service, "state": "started"}
			if step.Enabled != nil {
				args["enabled"] = *step.Enabled
			}
			task["ansible.builtin.service"] = args
		case "ansible.directory":
			mode := step.Mode
			if mode == "" {
				mode = "0755"
			}
			task["ansible.builtin.file"] = map[string]any{"path": step.Path, "state": "directory", "mode": mode}
		default:
			return nil, errors.New("workflow contains an unsupported action")
		}
		tasks = append(tasks, task)
	}
	playbook := []map[string]any{{
		"name": "Matrix controlled automation", "hosts": "all", "gather_facts": false, "tasks": tasks,
	}}
	data, err := yaml.Marshal(playbook)
	if err != nil {
		return nil, errors.New("could not encode Ansible playbook")
	}
	return data, nil
}

func runAnsible(ctx context.Context, steps []workflowStep, playbookText string, payload ansibleRunPayload) error {
	var playbook []byte
	var err error
	if strings.TrimSpace(playbookText) != "" {
		if err := validatePlaybook(playbookText); err != nil {
			return err
		}
		playbook = []byte(playbookText)
	} else {
		playbook, err = buildPlaybook(steps)
		if err != nil {
			return err
		}
	}
	inventory, knownHosts, err := inventoryAndKnownHosts(payload)
	if err != nil {
		return err
	}
	return ansibleruntime.Run(ctx, ansibleruntime.Input{
		Inventory: inventory, KnownHosts: knownHosts, Playbook: playbook,
	})
}
