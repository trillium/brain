//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// prepareOutboxCommand puts a subscriber command in its own process group and
// makes a timeout kill the whole group, so a script's background children do
// not outlive the timeout holding the output pipe open. WaitDelay bounds the
// pipe wait for anything that escaped the group.
func prepareOutboxCommand(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	c.WaitDelay = outboxCommandWaitDelay
}
