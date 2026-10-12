//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// detachedProcAttr starts a child in its own session, so it outlives the
// command (and the ssh session) that started it.
func detachedProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// terminatePid sends SIGTERM, waits for the process (and the port, when given)
// to go away, and escalates to SIGKILL only after 30 seconds.
func terminatePid(pid, port int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) && (port == 0 || !portListening(port)) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	time.Sleep(500 * time.Millisecond)
	if pidAlive(pid) {
		return fmt.Errorf("process %d did not exit", pid)
	}
	return nil
}

// lockPromotion takes an exclusive lock on path for the life of the process, so
// two promotions (or a promotion and a rollback) cannot run at once. The
// returned function releases it.
func lockPromotion(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // our own lock file
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // fd fits int
		_ = f.Close()
		return nil, fmt.Errorf("another 'brain host' operation is running on this machine (lock %s)", path)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil //nolint:gosec // fd fits int
}

// freeBytes is the space available to an unprivileged user on the filesystem
// holding path (the nearest existing parent when path does not exist yet).
func freeBytes(path string) int64 {
	for p := path; p != "" && p != "/" && p != "."; p = filepath.Dir(p) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(p, &st); err == nil {
			return int64(st.Bavail) * int64(st.Bsize) //nolint:gosec // block counts fit int64
		}
	}
	return 0
}
