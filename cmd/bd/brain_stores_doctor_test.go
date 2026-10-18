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

	// The connection verbs are exempt; a store-touching verb is not.
	for _, argv := range [][]string{
		{"brain", "stores", "host"},
		{"brain", "stores", "host", "set"},
		{"brain", "stores", "rewrite-wrappers"},
	} {
		cmd, _, err := rootCmd.Find(argv)
		if err != nil || cmd == nil {
			t.Fatalf("rootCmd.Find(%v) failed: %v", argv, err)
		}
		if !storesCommandCanRunWithoutStore(cmd) {
			t.Errorf("%v must run without a beads database — a broken caller store is what it repairs", argv)
		}
	}
	for _, argv := range [][]string{
		{"brain", "stores", "list"},
		{"brain", "stores", "env"},
	} {
		cmd, _, _ := rootCmd.Find(argv)
		if cmd != nil && storesCommandCanRunWithoutStore(cmd) {
			t.Errorf("%v must not be exempt from store resolution", argv)
		}
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

// A store read that prints advisories and then fails is reported by its CAUSE,
// not by the first warning: the reader has to act on the error line, and the
// warning is often unrelated (store permissions, credentials-file mode).
func TestProbeStoreReportsTheErrorNotThePrecedingWarning(t *testing.T) {
	isolatedHome(t)
	// The exact shape a real failed read has (observed against a stale server):
	// two advisories, then Error 1045 from the server the wrapper resolved.
	stub := fakeBd(t, `echo "Warning: /tmp/x/.beads has permissions 0755 (recommended: 0700). Run: chmod 700 /tmp/x/.beads"
echo "WARNING: credentials file /tmp/c has overly permissive permissions (0644)."
echo "Consider running: chmod 600 /tmp/c"
echo "Error: failed to open database: failed to check if database \"brain_unified\" exists on server 127.0.0.1:47111: Error 1045 (28000): Access denied for user 'brain'" >&2
exit 1`)

	h := probeStore(stub, "task", storeEntry{Path: "/nonexistent/task/.beads"}, 10*time.Second)
	if h.Status != "fail" {
		t.Fatalf("status = %q, want fail", h.Status)
	}
	if !strings.Contains(h.Reason, "Error 1045") || !strings.Contains(h.Reason, "127.0.0.1:47111") {
		t.Errorf("reason must name the real failure: %q", h.Reason)
	}
	if strings.Contains(h.Reason, "permissions") {
		t.Errorf("reason blamed a warning instead of the error: %q", h.Reason)
	}
	// The advisories are still available as detail, not thrown away.
	if !strings.Contains(h.Detail, "Warning:") {
		t.Errorf("detail lost the advisories: %q", h.Detail)
	}
}

func TestFailureLinePrefersTheErrorLine(t *testing.T) {
	in := []byte("\nWarning: something advisory\n\nError: the real cause\nmore text\n")
	if got := failureLine(in); got != "Error: the real cause" {
		t.Errorf("failureLine = %q", got)
	}
	if got := failureLine([]byte("\n  first\nsecond\n")); got != "first" {
		t.Errorf("failureLine without an error line = %q, want the first non-empty line", got)
	}
	if got := failureLine(nil); got != "" {
		t.Errorf("failureLine(nil) = %q", got)
	}
	if got := failureLine([]byte("Fatal: boom\n")); got != "Fatal: boom" {
		t.Errorf("failureLine = %q", got)
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
// Registry-path healing and wrapper misroute detection: the two ways a
// registered store silently stops being the store it says it is.
// ---------------------------------------------------------------------------

func TestProbeStore_HealedRegistryPathIsHealthy(t *testing.T) {
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
	// This machine's copy of the store exists; the registry still names the
	// home the registry was written in.
	real := filepath.Join(home, "data", "task", ".beads")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("mkdir store: %v", err)
	}

	h := probeStore("unused", "task", storeEntry{Path: "/Users/someone-else/data/task/.beads"}, 10*time.Second)

	if h.Status != "ok" {
		t.Fatalf("status = %q, want ok — the store answers from a healed registry path (reason=%q)", h.Status, h.Reason)
	}
	if h.Path != real {
		t.Errorf("resolved path = %q, want the healed %q", h.Path, real)
	}
	if h.Recorded != "/Users/someone-else/data/task/.beads" {
		t.Errorf("recorded path = %q, want the value the registry held", h.Recorded)
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

func TestWrapperMisroute_FailsWhenWrapperResolvesAnotherStore(t *testing.T) {
	home := isolatedHome(t)
	scratch := filepath.Join(home, "data", "scratch", ".beads")
	brain := filepath.Join(home, "data", "brain", ".beads")
	// The scratch wrapper resolves brain's workspace: reads answer from the
	// wrong store and writes would land there (robots-bfenw, robots-3pfky).
	writeWrapperForStore(t, home, "scratch", brain)

	stores := map[string]storeEntry{
		"scratch": {Path: scratch},
		"brain":   {Path: brain},
	}
	status, reason := wrapperMisroute("scratch", stores)
	if status != "fail" {
		t.Fatalf("status = %q, want fail (reason=%q)", status, reason)
	}
	if !strings.Contains(reason, `"brain"`) {
		t.Errorf("reason must name the store it misroutes to; got %q", reason)
	}
}

func TestWrapperMisroute_FailsWhenPinnedPathDoesNotExist(t *testing.T) {
	home := isolatedHome(t)
	// The wrapper points somewhere the registry never named, and nothing is
	// there: bd would answer from whatever it discovers instead.
	writeWrapperForStore(t, home, "job", filepath.Join(home, "data", "job-old", ".beads"))

	stores := map[string]storeEntry{"job": {Path: filepath.Join(home, "data", "job", ".beads")}}
	status, reason := wrapperMisroute("job", stores)
	if status != "fail" {
		t.Fatalf("status = %q, want fail — the wrapper points at a path that is not there (reason=%q)", status, reason)
	}
}

func TestWrapperMisroute_OKWhenWrapperAgreesWithRegistry(t *testing.T) {
	home := isolatedHome(t)
	dir := filepath.Join(home, "data", "task", ".beads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeWrapperForStore(t, home, "task", dir)

	stores := map[string]storeEntry{"task": {Path: dir}}
	if status, reason := wrapperMisroute("task", stores); status != "" {
		t.Fatalf("status = %q, want no finding (reason=%q)", status, reason)
	}
}

func TestWrapperMisroute_SkipsForwardingWrapper(t *testing.T) {
	home := isolatedHome(t)
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A 'brain remote install' wrapper forwards the command to another host and
	// never opens a store here: it must not be judged as a misroute.
	body := "#!/bin/sh\n" + remoteWrapperMarker + " generated by 'brain remote install'\nBRAIN_REMOTE_STORE=task exec brain \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "task"), []byte(body), 0o755); err != nil { //nolint:gosec
		t.Fatalf("writing wrapper: %v", err)
	}

	stores := map[string]storeEntry{"task": {Path: filepath.Join(home, "data", "task", ".beads")}}
	if status, reason := wrapperMisroute("task", stores); status != "" {
		t.Fatalf("forwarding wrapper judged as %q (%s)", status, reason)
	}
}

// storeHostRecordForTest points the doctor at a scratch store-host record.
func storeHostRecordForTest(t *testing.T, home string) string {
	t.Helper()
	path := writeScratchStoreHostRecord(t, home)
	t.Setenv(configfile.StoreHostFileEnvVar, path)
	return path
}

// doctorFlagsForTest restores the doctor's package-level flag state after a test
// that calls runBrainStoresDoctor directly.
func doctorFlagsForTest(t *testing.T) {
	t.Helper()
	priorJSON, priorJobs, priorTimeout := storesDoctorJSON, storesDoctorJobs, storesDoctorTimeout
	priorStrict, priorFix := storesDoctorStrict, storesDoctorFix
	storesDoctorJSON, storesDoctorStrict = false, false
	storesDoctorJobs = 1
	storesDoctorTimeout = 5 * time.Second
	t.Cleanup(func() {
		storesDoctorJSON, storesDoctorJobs, storesDoctorTimeout = priorJSON, priorJobs, priorTimeout
		storesDoctorStrict, storesDoctorFix = priorStrict, priorFix
	})
}

// 'brain doctor --fix' is the repair half of the drift check: it converges a
// machine whose wrapper pins a host onto the one store-host record, backs up
// what it rewrites, and reports the drift it cleared. In the default dry-run
// mode the same walk reports the drift and every file is left alone.
func TestDoctorFixRepointsDriftedWrapper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell wrappers are not the Windows shape")
	}
	home := isolatedHome(t)
	doctorFlagsForTest(t)
	noStoreHostProbe(t)
	storeHostRecordForTest(t, home)

	// bd stand-in: the wrapper resolves the store through the record, so the
	// probe only needs to succeed; the drift is in the wrapper's own pins.
	binDir := t.TempDir()
	stubBd := filepath.Join(binDir, "bd")
	if err := os.WriteFile(stubBd, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // test stub
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	beadsDir := filepath.Join(home, "data", "task", ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"),
		[]byte(`{"dolt_mode":"server","dolt_database":"brain_unified"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveStoresRegistry(map[string]storeEntry{"task": {Path: beadsDir}}); err != nil {
		t.Fatal(err)
	}

	wrapper := filepath.Join(home, ".local", "bin", "task")
	if err := os.MkdirAll(filepath.Dir(wrapper), 0o755); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	// A server-mode wrapper that reads the shared record from nowhere: no
	// run-time host block and no pin, so the store falls back to its own
	// metadata instead of the one record. It still answers, which is what makes
	// this drift invisible without an explicit check.
	original := "#!/bin/sh\nexport BEADS_DIR=\"$HOME/data/task/.beads\"\nexec bd \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(original), 0o755); err != nil { //nolint:gosec // test wrapper
		t.Fatal(err)
	}

	// The dry run (the default) reports the drift, names the fix, and writes
	// nothing: it is a warning, so it does not fail the run without --strict.
	storesDoctorFix = false
	dryRun := captureStderr(t, func() { runBrainStoresDoctor(nil, nil) })
	if !strings.Contains(dryRun, "host drift found") || !strings.Contains(dryRun, "brain doctor --fix") {
		t.Errorf("dry run did not report the drift and the fix:\n%s", dryRun)
	}
	body, err := os.ReadFile(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != original {
		t.Errorf("a dry run rewrote the wrapper:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "state", "brain", "wrapper-backups")); err == nil {
		t.Error("a dry run wrote a backup")
	}

	// --fix: the wrapper is converged on the record (and backed up).
	storesDoctorFix = true
	fixed := captureStderr(t, func() { runBrainStoresDoctor(nil, nil) })
	if !strings.Contains(fixed, "host drift repaired") {
		t.Errorf("--fix did not report the repair:\n%s", fixed)
	}
	body, err = os.ReadFile(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if _, removed := stripBakedServerPins(text); removed {
		t.Errorf("the baked pin survived --fix:\n%s", text)
	}
	if !strings.Contains(text, "_store_host_file=") {
		t.Errorf("--fix did not add the shared host block:\n%s", text)
	}
	backups, err := filepath.Glob(filepath.Join(home, ".local", "state", "brain", "wrapper-backups", "*", "task"))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one", backups)
	}
	backed, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(backed) != original {
		t.Errorf("backup is not the pre-fix wrapper:\n%s", backed)
	}
}
