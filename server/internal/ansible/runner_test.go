package ansible

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunnerConfigUsesPrivateControllerAndBoundedRemoteTemp(t *testing.T) {
	config := runnerConfig(filepath.Join(os.TempDir(), "matrix-private"))
	for _, want := range []string{
		"local_tmp = " + filepath.Join(os.TempDir(), "matrix-private", "local"),
		"remote_tmp = /tmp",
		"keep_remote_files = False",
		"StrictHostKeyChecking=yes",
		"GlobalKnownHostsFile=/dev/null",
		"password_mechanism = ssh_askpass",
		"usetty = False",
	} {
		if !strings.Contains(config, want) {
			t.Errorf("runner config missing %q", want)
		}
	}
	if strings.Contains(config, "~/.ansible/tmp") {
		t.Fatal("runner must not use the unbounded per-user Ansible temp directory")
	}
}

func TestRunWithExecutableDiscardsOutputAndRemovesPrivateWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX executable fixture")
	}
	root := t.TempDir()
	marker := filepath.Join(root, "ansible-env.txt")
	script := filepath.Join(root, "fake-ansible")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n%s\\n' \"$ANSIBLE_CONFIG\" \"$HOME|$ANSIBLE_LOCAL_TEMP\" > \"$MATRIX_ANSIBLE_TEST_MARKER\"\nprintf '%s\\n' 'raw-inventory-secret'\nprintf '%s\\n' 'raw-ssh-output-secret' >&2\nexit 23\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MATRIX_ANSIBLE_TEST_MARKER", marker)

	err := runWithExecutable(context.Background(), Input{
		Inventory:  []byte("inventory secret"),
		KnownHosts: []byte("host key"),
		Playbook:   []byte("playbook"),
	}, script)
	if !errors.Is(err, ErrExecution) {
		t.Fatalf("error = %v, want generic execution error", err)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "raw-ssh-output") {
		t.Fatalf("execution error exposed raw output: %v", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("helper env marker = %q", data)
	}
	if _, err := os.Stat(lines[0]); !os.IsNotExist(err) {
		t.Fatalf("private Ansible workspace remains after execution: %v", err)
	}
	homeAndLocal := strings.Split(lines[1], "|")
	if len(homeAndLocal) != 2 || filepath.Dir(homeAndLocal[1]) != homeAndLocal[0] {
		t.Fatalf("Ansible controller temp paths are not scoped to its private workspace: %q", lines[1])
	}
}
