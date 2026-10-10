//go:build windows

package main

import "os/exec"

// prepareOutboxCommand bounds the pipe wait; Windows has no process groups to
// signal here.
func prepareOutboxCommand(c *exec.Cmd) {
	c.WaitDelay = outboxCommandWaitDelay
}
