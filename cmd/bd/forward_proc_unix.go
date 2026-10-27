//go:build !windows

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// forwardKillGrace is how long ssh gets after SIGTERM before it is killed.
const forwardKillGrace = 2 * time.Second

// prepareForwardCommand puts the forwarded ssh in its own process group and
// makes a canceled context stop the whole group, so ssh and anything it
// spawned (a ProxyCommand, a mux master helper) die with bd instead of
// outliving it. A session whose stdin is a terminal stays in the foreground
// group: Setpgid would stop ssh with SIGTTIN when it reads the terminal.
func prepareForwardCommand(cmd *exec.Cmd, stdin *os.File) {
	if fdIsTerminal(stdin) {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		pgid := cmd.Process.Pid
		time.AfterFunc(forwardKillGrace, func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
		return syscall.Kill(-pgid, syscall.SIGTERM)
	}
	cmd.WaitDelay = forwardKillGrace
}

// relayForwardSignals passes SIGTERM, SIGINT and SIGHUP received by bd on to
// the child's process group. Call it after cmd.Start; the returned func stops
// the relay.
func relayForwardSignals(cmd *exec.Cmd) func() {
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || cmd.Process == nil {
		return func() {}
	}
	pid := cmd.Process.Pid
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-ch:
				if sig, ok := s.(syscall.Signal); ok {
					_ = syscall.Kill(-pid, sig)
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}
