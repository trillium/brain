//go:build windows

package main

import (
	"os"
	"os/exec"
)

// Windows has no process groups to signal here; the context deadline still
// kills ssh itself.
func prepareForwardCommand(cmd *exec.Cmd, stdin *os.File) {}

func relayForwardSignals(cmd *exec.Cmd) func() { return func() {} }
