//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// detachProcess starts the daemon in a new session so it survives the parent
// shell and does not receive terminal signals.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
