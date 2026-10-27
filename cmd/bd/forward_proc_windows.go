//go:build windows

package main

import "os/exec"

// Windows has no process groups to signal here; the context deadline still
// kills ssh itself.
func prepareForwardCommand(cmd *exec.Cmd, tty bool) {}

func relayForwardSignals(cmd *exec.Cmd, tty bool) func() { return func() {} }
