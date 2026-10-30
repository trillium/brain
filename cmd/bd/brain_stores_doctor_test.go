package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/configfile"
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

func TestDoctorExitCode(t *testing.T) {
	oldStrict := storesDoctorStrict
	defer func() { storesDoctorStrict = oldStrict }()

	storesDoctorStrict = false
	if got := doctorExitCode(0, 0); got != 0 {
		t.Errorf("clean run exits %d, want 0", got)
	}
	if got := doctorExitCode(0, 2); got != 0 {
		t.Errorf("warnings without --strict exit %d, want 0", got)
	}
	if got := doctorExitCode(1, 0); got != 1 {
		t.Errorf("one failure exits %d, want 1", got)
	}

	storesDoctorStrict = true
	if got := doctorExitCode(0, 1); got != 1 {
		t.Errorf("warnings with --strict exit %d, want 1", got)
	}
}

// TestDoctorRegistrySentinel guards the scheduler contract behind robots-j4g9:
// every 'stores doctor' exit 1 carries a 'FAILING STORES:' line, and the
// registry-unreadable sentinel must stay a single whitespace-free token so the
// scheduler files one bead for it instead of splitting it into several.
func TestDoctorRegistrySentinel(t *testing.T) {
	if doctorRegistrySentinel == "" {
		t.Fatal("registry sentinel must be non-empty")
	}
	if strings.Contains(doctorRegistrySentinel, " ") {
		t.Errorf("sentinel %q must not contain spaces — the scheduler splits FAILING STORES on whitespace", doctorRegistrySentinel)
	}
}

// ---------------------------------------------------------------------------
// Registry paths as recorded and wrapper misroute detection: the two ways a
// registered store silently stops being the store it says it is.
// ---------------------------------------------------------------------------

// A registry carried from another machine names that machine's home. The probe
// reports the registry path as recorded — it never guesses a $HOME-equivalent
// folder and never rewrites the entry.
func TestProbeStore_ReportsForeignRegistryPathAsRecorded(t *testing.T) {
	home := isolatedHome(t)
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("wrapper convention is POSIX-only")
	}
	wrapper := filepath.Join(binDir, "task")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("writing wrapper: %v", err)
	}
	// A coincidental $HOME copy must not be adopted as the store.
	real := filepath.Join(home, "data", "task", ".beads")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("mkdir store: %v", err)
	}
	recorded := "/Users/someone-else/data/task/.beads"

	h := probeStore("unused", "task", storeEntry{Path: recorded}, 10*time.Second)

	if h.Path != recorded {
		t.Errorf("resolved path = %q, want the recorded %q", h.Path, recorded)
	}
	if h.Status != "warn" || !strings.Contains(h.Reason, "registry path missing") {
		t.Fatalf("status = %q reason = %q, want a stale-path warning", h.Status, h.Reason)
	}
}

// A store whose metadata pins its own Dolt host or port diverges from the
// shared server on a bare bd run, so the doctor reports it as informational:
// it is shown, but it does not make --strict fail.
// TestTallyStoreHealth_InformationalDoesNotWarn locks the exit and summary
// contract: an informational metadata-pin finding still counts as an answered
// (and ok) store, but --strict does not fail on it.
func TestTallyStoreHealth_InformationalDoesNotWarn(t *testing.T) {
	oldStrict := storesDoctorStrict
	storesDoctorStrict = true
	defer func() { storesDoctorStrict = oldStrict }()

	tally := tallyStoreHealth([]storeHealth{{Status: "info"}, {Status: "ok"}, {Status: "warn"}})
	if tally.warn != 1 {
		t.Errorf("warn = %d, want only the warned store", tally.warn)
	}
	if tally.ok != 1 || tally.info != 1 {
		t.Errorf("ok/info = %d/%d, want 1/1", tally.ok, tally.info)
	}
	if tally.answered != 3 {
		t.Errorf("answered = %d, want 3 (ok, informational and warned stores all answered)", tally.answered)
	}
	if tally.ok+tally.info != 2 {
		t.Errorf("ok+informational = %d, want 2 stores reported ok", tally.ok+tally.info)
	}
	if code := doctorExitCode(0, 0); code != 0 {
		t.Errorf("an informational finding alone must not fail --strict; exit = %d", code)
	}
}

func TestMetadataServerPin_ReportsPinnedStore(t *testing.T) {
	home := isolatedHome(t)
	beadsDir := filepath.Join(home, "data", "task", ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeWrapperForStore(t, home, "task", beadsDir)
	if err := (&configfile.Config{DoltServerHost: "10.0.0.1", DoltServerPort: 3307}).Save(beadsDir); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}

	status, reason := metadataServerPin("task", storeEntry{Path: beadsDir})
	if status != "info" {
		t.Fatalf("status = %q, want info (reason=%q)", status, reason)
	}
	if !strings.Contains(reason, "dolt_server_host=10.0.0.1") || !strings.Contains(reason, "dolt_server_port=3307") {
		t.Errorf("reason = %q, want both pins named", reason)
	}
}

// A store whose metadata carries no server pin is not reported.
func TestMetadataServerPin_IgnoresUnpinnedStore(t *testing.T) {
	home := isolatedHome(t)
	beadsDir := filepath.Join(home, "data", "task", ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := (&configfile.Config{}).Save(beadsDir); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}

	if status, reason := metadataServerPin("task", storeEntry{Path: beadsDir}); status != "" {
		t.Fatalf("status = %q (%s), want no finding", status, reason)
	}
}

// A comment that documents an override is not the wrapper's effective
// BEADS_DIR: the doctor must compare the assignment the wrapper actually makes.
func TestWrapperBeadsDir_SkipsCommentedOverrides(t *testing.T) {
	home := isolatedHome(t)
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("wrapper convention is POSIX-only")
	}
	wrapper := filepath.Join(binDir, "inbox")
	body := "#!/bin/sh\n# BEADS_DIR=/tmp/itest/.beads INBOX_EVENTS_FILE=/tmp/x inbox list\n" +
		"export BEADS_DIR=\"$HOME/data/inbox/.beads\"\nexec bd \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(body), 0o755); err != nil { //nolint:gosec
		t.Fatalf("writing wrapper: %v", err)
	}

	want := filepath.Join(home, "data", "inbox", ".beads")
	got, ok := wrapperBeadsDir(wrapper)
	if !ok || got != want {
		t.Fatalf("wrapperBeadsDir = %q, %v; want %q", got, ok, want)
	}
}

// writeWrapperForStore writes a wrapper pinning BEADS_DIR to a literal path.
func writeWrapperForStore(t *testing.T, home, name, beadsDir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("wrapper convention is POSIX-only")
	}
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "#!/bin/sh\nexport BEADS_DIR=\"" + beadsDir + "\"\nexport BD_NAME=\"" + name + "\"\necho ok\n"
	if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil { //nolint:gosec
		t.Fatalf("writing wrapper: %v", err)
	}
}
