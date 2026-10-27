package ansible

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrUnavailable = errors.New("embedded Ansible runtime unavailable")
	ErrExecution   = errors.New("Ansible playbook failed")
)

type Input struct {
	Inventory  []byte
	Vars       []byte
	KnownHosts []byte
	Playbook   []byte
}

func RuntimeVersion(ctx context.Context) (string, error) {
	executable, err := exec.LookPath("ansible-playbook")
	if err != nil {
		return "", ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, executable, "--version").Output()
	if err != nil {
		return "", ErrUnavailable
	}
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	if line == "" {
		return "", ErrUnavailable
	}
	return line, nil
}

func Run(ctx context.Context, in Input) error {
	executable, err := exec.LookPath("ansible-playbook")
	if err != nil {
		return ErrUnavailable
	}
	return runWithExecutable(ctx, in, executable)
}

func runWithExecutable(ctx context.Context, in Input, executable string) error {
	if len(in.Inventory) == 0 || len(in.KnownHosts) == 0 || len(in.Playbook) == 0 {
		return errors.New("Ansible inputs are incomplete")
	}
	dir, err := os.MkdirTemp("", "matrix-ansible-*")
	if err != nil {
		return errors.New("could not prepare private Ansible workspace")
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0700); err != nil {
		return errors.New("could not secure private Ansible workspace")
	}
	for _, name := range []string{"local", "control"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			return errors.New("could not prepare private Ansible workspace")
		}
	}
	files := map[string][]byte{
		"inventory.yml": in.Inventory,
		"known_hosts":   in.KnownHosts,
		"playbook.yml":  in.Playbook,
	}
	if len(in.Vars) > 0 {
		files["vars.json"] = in.Vars
	}
	for name, data := range files {
		if err := writePrivateFile(filepath.Join(dir, name), data); err != nil {
			return errors.New("could not write private Ansible input")
		}
	}
	config := runnerConfig(dir)
	configPath := filepath.Join(dir, "ansible.cfg")
	if err := writePrivateFile(configPath, []byte(config)); err != nil {
		return errors.New("could not write private Ansible configuration")
	}
	args := []string{"-i", filepath.Join(dir, "inventory.yml")}
	if len(in.Vars) > 0 {
		args = append(args, "-e", "@"+filepath.Join(dir, "vars.json"))
	}
	args = append(args, filepath.Join(dir, "playbook.yml"))
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Env = withEnv(os.Environ(), map[string]string{
		"ANSIBLE_CONFIG":                 configPath,
		"ANSIBLE_HOST_KEY_CHECKING":      "True",
		"ANSIBLE_NOCOLOR":                "1",
		"ANSIBLE_DISPLAY_SKIPPED_HOSTS":  "False",
		"ANSIBLE_DISPLAY_OK_HOSTS":       "False",
		"ANSIBLE_SSH_PASSWORD_MECHANISM": "ssh_askpass",
		"ANSIBLE_LOCAL_TEMP":             filepath.Join(dir, "local"),
		"ANSIBLE_REMOTE_TEMP":            "/tmp",
		"ANSIBLE_KEEP_REMOTE_FILES":      "False",
		"HOME":                           dir,
	})
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return ErrExecution
	}
	return nil
}

func withEnv(env []string, overrides map[string]string) []string {
	out := make([]string, 0, len(env)+len(overrides))
	for _, item := range env {
		key, _, ok := strings.Cut(item, "=")
		if _, replace := overrides[key]; ok && replace {
			continue
		}
		out = append(out, item)
	}
	for key, value := range overrides {
		out = append(out, key+"="+value)
	}
	return out
}

func writePrivateFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func runnerConfig(dir string) string {
	return fmt.Sprintf(`[defaults]
host_key_checking = True
retry_files_enabled = False
interpreter_python = auto_silent
forks = 10
local_tmp = %s
remote_tmp = /tmp
keep_remote_files = False

[ssh_connection]
ssh_args = -o UserKnownHostsFile=%s -o GlobalKnownHostsFile=/dev/null -o StrictHostKeyChecking=yes -o ConnectTimeout=15
control_path_dir = %s
password_mechanism = ssh_askpass
usetty = False
`, filepath.Join(dir, "local"), filepath.Join(dir, "known_hosts"), filepath.Join(dir, "control"))
}
