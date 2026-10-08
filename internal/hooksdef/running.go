package hooksdef

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// RunningPrefix names the marker files a bd process keeps in the store's
// .beads directory for as long as it has a hook running: hooks-running.<pid>.
//
// The marker exists because an environment variable alone is a courtesy a
// hook can scrub (`env -u BD_INSIDE_HOOK`). The store directory is not: any bd
// process that is about to write to this store looks here first, and refuses
// if one of its own ancestors is a bd process with a hook running. A hook
// cannot reach the store without going through such a check, and cannot
// pretend its way out of its own ancestry.
const RunningPrefix = "hooks-running."

// markRunning records that this process has a hook running against the store
// whose hooks.d holds def. The returned function removes the marker. A hook
// whose definition has no path (tests) is not marked.
func markRunning(def Definition) (release func(), err error) {
	if def.Path == "" || runtime.GOOS == "windows" {
		return func() {}, nil
	}
	beadsDir := filepath.Dir(filepath.Dir(def.Path))
	path := filepath.Join(beadsDir, RunningPrefix+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("marking the store as running a hook: %w", err)
	}
	return func() { _ = os.Remove(path) }, nil
}

// AncestorRunningHook reports whether this process was started, directly or
// through any chain of children, by a bd process that currently has a hook
// running against the store at beadsDir. Markers left by dead processes are
// removed. Failure to inspect the process tree is treated as "yes": a write
// that cannot prove it is not the work of a hook is refused.
func AncestorRunningHook(beadsDir string) bool {
	if beadsDir == "" || runtime.GOOS == "windows" {
		return false
	}
	markers, _ := filepath.Glob(filepath.Join(beadsDir, RunningPrefix+"*"))
	if len(markers) == 0 {
		return false
	}
	var ancestors map[int]bool
	for _, m := range markers {
		pid, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(m), RunningPrefix))
		if err != nil {
			continue
		}
		if !processAlive(pid) {
			_ = os.Remove(m) // stale: its owner died without cleaning up
			continue
		}
		if ancestors == nil {
			var ok bool
			if ancestors, ok = ancestry(); !ok {
				return true
			}
		}
		if ancestors[pid] {
			return true
		}
	}
	return false
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscallZero) == nil
}

// ancestry returns this process's ancestor pids, found with ps. ok is false
// when the tree could not be read.
func ancestry() (map[int]bool, bool) {
	out := map[int]bool{}
	pid := os.Getppid()
	for depth := 0; pid > 1 && depth < 64; depth++ {
		out[pid] = true
		raw, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return nil, false
		}
		next, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			return nil, false
		}
		pid = next
	}
	return out, true
}
