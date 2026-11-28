//go:build unix

// Package procgroup terminates a started process together with all its descendants.
// On Unix the child leads its own process group and the whole group receives SIGKILL;
// on Windows the tree is removed with taskkill /T /F. A killed tree gives no terminal
// event, so callers must record such a run as Unknown, not as success or failure.
package procgroup

import (
	"os/exec"
	"syscall"
)

// Configure must be called before cmd.Start so the child becomes a process group leader.
func Configure(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// Attach is a no-op on Unix: a process group does not outlive-kill itself when the owner dies,
// so an owner crash leaves the group running. Only Windows contains the tree with a Job Object.
func Attach(pid int) error { return nil }

// Release is a no-op on Unix.
func Release(pid int) {}

// Kill sends SIGKILL to the process group led by pid.
func Kill(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
