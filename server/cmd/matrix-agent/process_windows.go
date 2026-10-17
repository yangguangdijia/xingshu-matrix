//go:build windows

package main

import "os/exec"

func configureProcessCommand(cmd *exec.Cmd) {
	// Windows does not expose Unix process groups through syscall.SysProcAttr.
	// exec.CommandContext cancels the child process when the context expires.
}
