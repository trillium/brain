package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBrainStoresDoctorCmd_Registered(t *testing.T) {
	found, _, err := rootCmd.Find([]string{"brain", "stores", "doctor"})
	if err != nil {
		t.Fatalf("rootCmd.Find([brain stores doctor]) error: %v", err)
	}
	if found == nil || found.Name() != "doctor" || found.Parent().Name() != "stores" {
		t.Fatal("brain stores doctor command not registered")
	}
}

func TestStoresCommandCanRunWithoutStore(t *testing.T) {
	storesDoctor, _, _ := rootCmd.Find([]string{"brain", "stores", "doctor"})
	if storesDoctor == nil {
		t.Fatal("brain stores doctor not found")
	}
	if !storesCommandCanRunWithoutStore(storesDoctor) {
		t.Error("stores doctor must run without a beads database — it is the probe for the case where there isn't one")
	}

	storesList, _, _ := rootCmd.Find([]string{"brain", "stores", "list"})
	if storesList != nil && storesCommandCanRunWithoutStore(storesList) {
		t.Error("only 'doctor' is exempt from store resolution, not every stores subcommand")
	}

	topDoctor, _, _ := rootCmd.Find([]string{"doctor"})
	if topDoctor != nil && topDoctor.Name() == "doctor" && storesCommandCanRunWithoutStore(topDoctor) {
		t.Error("top-level 'bd doctor' must not be confused with 'stores doctor'")
	}

	if storesCommandCanRunWithoutStore(nil) {
		t.Error("nil command must not be exempt")
	}
}

// fakeBd writes an executable stand-in for the bd binary that emits the given
// output and exit code, so probe classification can be tested without a store.
func fakeBd(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub not supported on Windows")
	}
	path := filepath.Join(t.TempDir(), "fake-bd")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec
		t.Fatalf("writing stub: %v", err)
	}
	return path
}

// isolatedHome points HOME at an empty dir so storeWrapperPath finds no wrapper
// and probeStore takes the direct-read path.
func isolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func TestProbeStore_FailsWhenReadExitsNonZero(t *testing.T) {
	isolatedHome(t)
	// The exact symptom of the half-provisioned staleness store (robots-nka3).
	stub := fakeBd(t, `echo "Error: no beads database found" >&2; exit 1`)

	h := probeStore(stub, "staleness", storeEntry{Path: "/nonexistent/staleness/.beads"}, 10*time.Second)

	if h.Status != "fail" {
		t.Fatalf("status = %q, want fail (reason=%q)", h.Status, h.Reason)
	}
	if h.Probe != "direct" {
		t.Errorf("probe = %q, want direct", h.Probe)
	}
	if h.Reason == "" {
		t.Error("failing store must carry a one-line reason")
	}
	// A missing registry path is the actionable detail; it must reach the operator.
	if want := "/nonexistent/staleness/.beads"; !strings.Contains(h.Reason, want) {
		t.Errorf("reason %q does not mention the missing path %q", h.Reason, want)
	}
}

func TestProbeStore_FailsWhenReadErrorsButExitsZero(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	stub := fakeBd(t, `echo "Error: no beads database found"; exit 0`)

	h := probeStore(stub, "inbox", storeEntry{Path: dir}, 10*time.Second)

	if h.Status != "fail" {
		t.Fatalf("status = %q, want fail — a zero exit does not make an unreadable store healthy", h.Status)
	}
}

func TestProbeStore_OKWhenReadSucceeds(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	stub := fakeBd(t, `echo "no issues found"; exit 0`)

	h := probeStore(stub, "review", storeEntry{Path: dir}, 10*time.Second)

	// No wrapper exists under the isolated HOME, so a healthy store is a warning.
	if h.Status != "warn" {
		t.Fatalf("status = %q, want warn (reason=%q)", h.Status, h.Reason)
	}
	if !strings.Contains(h.Reason, "no CLI wrapper") {
		t.Errorf("reason = %q, want a missing-wrapper warning", h.Reason)
	}
}

func TestProbeStore_ListOutputMentioningErrorsStaysHealthy(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	// A bead title can legitimately contain error-ish words; that must not fail
	// a store that answered the read.
	stub := fakeBd(t, `echo "robots-nka3  Error: no beads database found on every call"; exit 0`)

	h := probeStore(stub, "robots", storeEntry{Path: dir}, 10*time.Second)

	if h.Status == "fail" {
		t.Fatalf("a bead title echoed by 'list' must not fail the store (reason=%q)", h.Reason)
	}
}

func TestProbeStore_LongerMessageSharingThePrefixStaysHealthy(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	// The match is the whole error line, not its prefix: a message that merely
	// starts the same way must not fail a store that answered the read.
	stub := fakeBd(t, `echo "Error: no beads database maintenance completed"; exit 0`)

	h := probeStore(stub, "chores", storeEntry{Path: dir}, 10*time.Second)

	if h.Status == "fail" {
		t.Fatalf("prefix-only match must not fail a healthy store (reason=%q)", h.Reason)
	}
}

func TestProbeStore_UsesWrapperWhenPresent(t *testing.T) {
	home := isolatedHome(t)
	dir := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	wrapper := filepath.Join(binDir, "task")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("writing wrapper: %v", err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("wrapper convention is POSIX-only")
	}
	// Stub bd fails loudly: if the wrapper is not preferred, the test fails.
	stub := fakeBd(t, `echo "Error: no beads database found" >&2; exit 1`)

	h := probeStore(stub, "task", storeEntry{Path: dir}, 10*time.Second)

	if h.Probe != "wrapper" {
		t.Fatalf("probe = %q, want wrapper — agents reach stores through the wrapper", h.Probe)
	}
	if h.Status != "ok" {
		t.Fatalf("status = %q, want ok (reason=%q)", h.Status, h.Reason)
	}
}

func TestProbeStore_WarnsWhenRegistryPathIsStale(t *testing.T) {
	home := isolatedHome(t)
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("wrapper convention is POSIX-only")
	}
	wrapper := filepath.Join(binDir, "ideas")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("writing wrapper: %v", err)
	}

	h := probeStore("unused", "ideas", storeEntry{Path: "/nonexistent/ideas/.beads"}, 10*time.Second)

	if h.Status != "warn" {
		t.Fatalf("status = %q, want warn — the read worked but the registry path is stale", h.Status)
	}
	if !strings.Contains(h.Reason, "registry path missing") {
		t.Errorf("reason = %q, want a stale-path warning", h.Reason)
	}
}

func TestProbeStore_FailsOnTimeout(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	stub := fakeBd(t, `sleep 30`)

	start := time.Now()
	h := probeStore(stub, "lifespan", storeEntry{Path: dir}, 150*time.Millisecond)
	elapsed := time.Since(start)

	if h.Status != "fail" {
		t.Fatalf("status = %q, want fail on timeout", h.Status)
	}
	if !strings.Contains(h.Reason, "timed out") {
		t.Errorf("reason = %q, want a timeout reason", h.Reason)
	}
	if elapsed > 10*time.Second {
		t.Errorf("probe took %s — a wedged store must not hang the federation walk", elapsed)
	}
}

func TestFirstLineAndHeadLines(t *testing.T) {
	out := []byte("\n\nError: no beads database found\nHint: run 'bd where'\nor set BEADS_DIR\nextra\n")
	if got, want := firstLine(out), "Error: no beads database found"; got != want {
		t.Errorf("firstLine = %q, want %q", got, want)
	}
	got := headLines(out, 2)
	want := "Error: no beads database found\nHint: run 'bd where'"
	if got != want {
		t.Errorf("headLines = %q, want %q", got, want)
	}
	if firstLine([]byte("   \n\t\n")) != "" {
		t.Error("firstLine of blank output must be empty")
	}
	if headLines(nil, 3) != "" {
		t.Error("headLines of nil must be empty")
	}
}
