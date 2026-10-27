//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A forwarded call that outlives BRAIN_REMOTE_DEADLINE must take every
// descendant of ssh down with it, not leave them reparented to init.
func TestForwardDeadlineKillsDescendants(t *testing.T) {
	remoteEnv(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	script := filepath.Join(dir, "slowssh")
	body := "#!/bin/sh\nsleep 60 &\necho $! > " + remoteQuote(pidFile) + "\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}
	t.Setenv(envRemoteSSH, script)
	t.Setenv(envRemoteDeadline, "0.5")

	start := time.Now()
	_, _, code := runForward(t, &remoteConfig{Host: "me@host"}, "task", []string{"list"}, "")
	if code == 0 {
		t.Errorf("exit code = 0, want a failure after the deadline")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("forward took %v; deadline not enforced", d)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived the deadline", pid)
}
