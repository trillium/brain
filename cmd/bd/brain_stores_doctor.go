package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
)

// defaultStoreProbeTimeout bounds a single store read so one wedged store
// cannot hang the whole federation walk.
const defaultStoreProbeTimeout = 20 * time.Second

// probeWaitDelay is how long a killed probe gets to release its output pipes
// before they are closed out from under it.
const probeWaitDelay = 2 * time.Second

var (
	storesDoctorJSON    bool
	storesDoctorJobs    int
	storesDoctorTimeout time.Duration
	storesDoctorStrict  bool
)

// storeUnreadableRe matches the exact line bd emits when the store it was
// pointed at is missing or half-provisioned. Exit status is the primary signal;
// this is the belt-and-braces check for modes that report the error and still
// exit 0. It is deliberately anchored to the whole error line rather than a
// loose "Error:" match, so neither a bead title echoed by 'list' nor a longer
// message sharing the prefix can fail a store that is in fact healthy.
var storeUnreadableRe = regexp.MustCompile(`(?mi)^Error: no beads database found\r?$`)

// storeHealth is the per-store result of a doctor probe.
type storeHealth struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Wrapper string `json:"wrapper,omitempty"` // CLI wrapper actually probed, if any
	Probe   string `json:"probe"`             // "wrapper" | "direct"
	Status  string `json:"status"`            // "ok" | "warn" | "fail"
	Reason  string `json:"reason,omitempty"`  // one-line human cause
	Detail  string `json:"detail,omitempty"`  // first lines of the failing output
	Millis  int64  `json:"ms"`
}

var brainStoresDoctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Assert every store in the registry answers a read",
	Long: `Walk every store registered in ~/.config/pai/stores.yaml and assert it
answers a read. The registry is the source of truth for which stores must
work; anything registered but unreadable is a provisioning bug.

Each store is probed the way an agent would reach it: its CLI wrapper at
~/.local/bin/<name> is invoked with 'list --limit 1'. Stores registered
with --no-wrapper are probed directly with BEADS_DIR pinned, and reported
as a warning so the missing wrapper stays visible.

A store fails when the probe exits non-zero, times out, or prints
'no beads database found' — the signature of a half-provisioned store that
was registered but never initialized. Such a store is silently non-functional
for as long as nobody happens to use it; for queue-shaped stores (staleness,
review, inbox) 'silently empty' is indistinguishable from 'nothing to do',
so the failure is invisible by construction until something asserts it.

Run it from a scheduler (launchd/cron) or a session-start probe so a
half-provisioned store surfaces within a day rather than a week.

Exit codes:
  0 — every store answered a read
  1 — at least one store failed (or, with --strict, produced a warning)`,
	Args: cobra.NoArgs,
	Run:  runBrainStoresDoctor,
}

func runBrainStoresDoctor(_ *cobra.Command, _ []string) {
	stores, err := loadStoresRegistry()
	if err != nil {
		FatalError("loading registry: %v", err)
	}
	if len(stores) == 0 {
		fmt.Println("No stores registered. Use 'brain stores create <name>' or 'brain stores add <name> <beads-dir>'.")
		return
	}

	self, err := os.Executable()
	if err != nil || self == "" {
		// Fallback to argv[0] — fine for interactive use.
		self = os.Args[0]
	}

	if storesDoctorJobs < 1 {
		storesDoctorJobs = 1
	}
	timeout := storesDoctorTimeout
	if timeout <= 0 {
		timeout = defaultStoreProbeTimeout
	}

	names := sortedKeys(stores)
	results := make([]storeHealth, len(names))
	sem := make(chan struct{}, storesDoctorJobs)
	var wg sync.WaitGroup

	started := time.Now()
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = probeStore(self, name, stores[name], timeout)
		}(i, name)
	}
	wg.Wait()
	elapsed := time.Since(started)

	okCount, warnCount, failCount := 0, 0, 0
	failed := make([]string, 0, len(names))
	for _, r := range results {
		switch r.Status {
		case "fail":
			failCount++
			failed = append(failed, r.Name)
		case "warn":
			warnCount++
		default:
			okCount++
		}
	}

	if storesDoctorJSON {
		summary := map[string]interface{}{
			"stores":   results,
			"ok":       okCount,
			"warnings": warnCount,
			"failed":   failCount,
			"total":    len(results),
			"failing":  failed,
			"ms":       elapsed.Milliseconds(),
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(summary)
	} else {
		width := 0
		for _, r := range results {
			if len(r.Name) > width {
				width = len(r.Name)
			}
		}
		for _, r := range results {
			label := map[string]string{"ok": "ok  ", "warn": "WARN", "fail": "FAIL"}[r.Status]
			line := fmt.Sprintf("%s  %-*s  %5dms", label, width, r.Name, r.Millis)
			if r.Reason != "" {
				line += "  " + r.Reason
			}
			fmt.Fprintln(os.Stderr, line)
			if r.Detail != "" && r.Status == "fail" {
				for _, dl := range strings.Split(r.Detail, "\n") {
					fmt.Fprintf(os.Stderr, "      %s\n", dl)
				}
			}
		}
		fmt.Fprintf(os.Stderr, "\nFederation: %d/%d stores answered a read (%d failed, %d warnings) in %.1fs\n",
			okCount+warnCount, len(results), failCount, warnCount, elapsed.Seconds())
		if failCount > 0 {
			fmt.Fprintf(os.Stderr, "FAILING STORES: %s\n", strings.Join(failed, " "))
		}
	}

	if failCount > 0 || (storesDoctorStrict && warnCount > 0) {
		os.Exit(1)
	}
}

// probeStore runs one read against a single registered store and classifies the
// outcome. It never returns an error: every failure mode is folded into the
// returned storeHealth so one broken store cannot abort the federation walk.
func probeStore(self, name string, entry storeEntry, timeout time.Duration) storeHealth {
	beadsDir := expandPath(entry.Path)
	h := storeHealth{Name: name, Path: beadsDir}

	wrapper, wrapperOK := storeWrapperPath(name)
	h.Wrapper = wrapper

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var sub *exec.Cmd
	if wrapperOK {
		// Probe the way an agent reaches the store: through its own wrapper,
		// which pins BEADS_DIR plus any store-specific env (dolt server mode,
		// knowledge roots) that a bare BEADS_DIR would miss.
		h.Probe = "wrapper"
		sub = exec.CommandContext(ctx, wrapper, "list", "--limit", "1")
		sub.Env = os.Environ()
	} else {
		// Registered with --no-wrapper (or the wrapper was lost): fall back to
		// a direct read so we can still tell "no CLI" from "store is broken".
		h.Probe = "direct"
		// self is this binary's own path (os.Executable), not caller input.
		sub = exec.CommandContext(ctx, self, "list", "--limit", "1") //nolint:gosec // G702: self is os.Executable()
		sub.Env = append(os.Environ(), "BEADS_DIR="+beadsDir, "BD_NAME="+name)
	}
	// Never let a health probe trigger the auto-features a real session would.
	sub.Env = append(sub.Env, "BRAIN_NO_AUTO_FEATURE_REQUEST=1")
	// Killing the probe on timeout is not enough: a grandchild (dolt, a shell
	// wrapper's exec target) can hold the output pipe open and keep Wait
	// blocked long past the deadline. WaitDelay closes the pipes shortly after
	// the kill so the timeout is a real bound, not a suggestion.
	sub.WaitDelay = probeWaitDelay

	started := time.Now()
	out, runErr := sub.CombinedOutput()
	h.Millis = time.Since(started).Milliseconds()

	pathMissing := false
	if _, statErr := os.Stat(beadsDir); statErr != nil {
		pathMissing = true
	}

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		h.Status = "fail"
		h.Reason = fmt.Sprintf("read timed out after %s", timeout)
	case runErr != nil:
		h.Status = "fail"
		h.Reason = firstLine(out)
		if h.Reason == "" {
			h.Reason = runErr.Error()
		}
		h.Detail = headLines(out, 3)
	case storeUnreadableRe.Match(out):
		// Exited 0 but said it could not read — treat the text as truth.
		h.Status = "fail"
		h.Reason = firstLine(out)
		h.Detail = headLines(out, 3)
	case pathMissing:
		// The read worked, so the store is reachable, but the registry points
		// at a path that no longer exists — a stale entry worth fixing.
		h.Status = "warn"
		h.Reason = fmt.Sprintf("registry path missing: %s", beadsDir)
	case !wrapperOK:
		h.Status = "warn"
		h.Reason = fmt.Sprintf("no CLI wrapper at %s (probed directly)", wrapper)
	default:
		h.Status = "ok"
	}

	if h.Status == "fail" && pathMissing {
		h.Reason = fmt.Sprintf("%s (registry path missing: %s)", h.Reason, beadsDir)
	}
	return h
}

// storeWrapperPath returns the conventional CLI wrapper location for a store and
// whether an executable file is actually there.
func storeWrapperPath(name string) (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	path := filepath.Join(home, ".local", "bin", name)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return path, false
	}
	return path, true
}

func firstLine(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return ""
}

func headLines(b []byte, n int) string {
	lines := make([]string, 0, n)
	for _, line := range strings.Split(string(b), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			lines = append(lines, s)
			if len(lines) == n {
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}

func init() {
	brainStoresDoctorCmd.Flags().BoolVar(&storesDoctorJSON, "json", false,
		"Emit a structured JSON object instead of per-store text lines")
	brainStoresDoctorCmd.Flags().IntVar(&storesDoctorJobs, "jobs", 8,
		"Probe this many stores concurrently")
	brainStoresDoctorCmd.Flags().DurationVar(&storesDoctorTimeout, "timeout", defaultStoreProbeTimeout,
		"Per-store read timeout")
	brainStoresDoctorCmd.Flags().BoolVar(&storesDoctorStrict, "strict", false,
		"Exit non-zero on warnings too (missing wrapper, stale registry path)")

	brainStoresCmd.AddCommand(brainStoresDoctorCmd)
}
