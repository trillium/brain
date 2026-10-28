package main

// A client wrapper must exec the real bd binary, never itself or another
// generated wrapper. These tests cover the guard, the client sync that uses it
// (including the store named 'brain'), the backup/restore that keeps modes, and
// the rollout smoke check that catches a self-exec wrapper.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storeregistry"
	"github.com/steveyegge/beads/internal/storeregistry/regtest"
)

func TestWrapperTargetRefusal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes and symlinks")
	}
	dir := t.TempDir()
	bd := filepath.Join(dir, "bd")
	mustWrite(t, bd, "\x7fELF-not-a-wrapper\n", 0o755)
	wrapper := filepath.Join(dir, "brain")
	mustWrite(t, wrapper, remoteWrapperBody("brain", bd), 0o755)
	syncWrapper := filepath.Join(dir, "task")
	mustWrite(t, syncWrapper, syncWrapperBody(storeregistry.New("task"), shimEnv{Home: dir}), 0o755)
	link := filepath.Join(dir, "bd-link")
	if err := os.Symlink(bd, link); err != nil {
		t.Fatal(err)
	}
	linkToWrapper := filepath.Join(dir, "looks-real")
	if err := os.Symlink(wrapper, linkToWrapper); err != nil {
		t.Fatal(err)
	}
	noExec := filepath.Join(dir, "noexec")
	mustWrite(t, noExec, "binary", 0o644)

	cases := []struct {
		name, target, wrapper, want string // want "" = allowed
	}{
		{"real binary", bd, filepath.Join(dir, "task"), ""},
		{"real binary through a symlink", link, filepath.Join(dir, "inbox"), ""},
		{"the wrapper itself", wrapper, wrapper, "wrapper itself"},
		{"the wrapper itself, not yet written", filepath.Join(dir, "new"), filepath.Join(dir, "new"), "cannot be resolved"},
		{"a symlink to the wrapper", linkToWrapper, wrapper, "wrapper itself"},
		{"another remote wrapper", wrapper, filepath.Join(dir, "task2"), "itself a generated wrapper"},
		{"a sync wrapper", syncWrapper, filepath.Join(dir, "task2"), "itself a generated wrapper"},
		{"a symlink to a wrapper", linkToWrapper, filepath.Join(dir, "task2"), "itself a generated wrapper"},
		{"a relative path", "bd", wrapper, "not an absolute path"},
		{"no path", "", wrapper, "not an absolute path"},
		{"a missing file", filepath.Join(dir, "gone"), wrapper, "cannot be resolved"},
		{"a directory", dir, wrapper, "not a regular file"},
		{"not executable", noExec, wrapper, "not executable"},
	}
	for _, c := range cases {
		got := wrapperTargetRefusal(c.target, c.wrapper)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: refusal = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRemoteInstallRefusesAWrapperTargetThatIsAWrapper(t *testing.T) {
	dir := t.TempDir()
	bd := filepath.Join(dir, "bd")
	mustWrite(t, bd, "binary", 0o755)
	brain := filepath.Join(dir, "brain")
	mustWrite(t, brain, remoteWrapperBody("brain", bd), 0o755)

	got, err := installRemoteWrappers(dir, brain, "", []string{"task"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Action != "skipped" || !strings.Contains(got[0].Note, "refused") {
		t.Fatalf("install with a wrapper as the exec target must refuse: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "task")); err == nil {
		t.Error("no wrapper may be written whose exec target is a wrapper")
	}
	got, _ = installRemoteWrappers(dir, bd, "", []string{"task"}, false, false)
	if got[0].Action != "created" {
		t.Errorf("the real binary is accepted: %+v", got)
	}
}

// clientEnv is a client machine (its own HOME, a relay stub that serves the
// registry of a fleet's first machine) the way the MacBook is set up.
type clientEnv struct {
	t    *testing.T
	home string
	bin  string // the real bd binary
	src  registrySource
}

func newClientEnv(t *testing.T, stores ...string) *clientEnv {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	f := newFleet(t)
	a := f.addMachine("mini0")
	f.adopt(a)
	for _, s := range stores {
		a.createStore(s)
	}
	ctx := context.Background()
	snap, _, err := storeregistry.ReadSnapshot(ctx, a.db)
	if err != nil {
		t.Fatal(err)
	}
	machines, _ := storeregistry.ReadMachines(ctx, a.db)
	wire := registrySnapshotJSON{State: snap.State, Entries: snap.Entries, Found: true, UnifiedDB: regtest.Database, Machines: machines}
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "export.json"), string(jsonBytes(wire)), 0o644)
	ssh := filepath.Join(dir, "ssh")
	mustWrite(t, ssh, `#!/bin/sh
for last; do :; done
case "$last" in
  *"'registry' 'export'"*) cat "$FAKE_EXPORT" ;;
  *"'registry' 'report'"*) : ;;
  *) echo "unexpected: $last" >&2; exit 9 ;;
esac
`, 0o755)

	home := t.TempDir()
	for _, d := range []string{".local/bin", ".config/brain"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(home, ".config/brain/remote.yaml"), "host: me@relay\n", 0o644)
	t.Setenv("HOME", home)
	t.Setenv(envMachine, "macbook")
	t.Setenv(envRemoteSSH, ssh)
	t.Setenv("FAKE_EXPORT", filepath.Join(dir, "export.json"))
	t.Setenv("BRAIN_BINARY", "")
	_ = os.Unsetenv(envRemoteOff)

	bin := filepath.Join(home, ".local/bin/bd")
	mustWrite(t, bin, "\x7fELF-the-real-bd\n", 0o755)
	src, ok := clientSourceIfConfigured()
	if !ok {
		t.Fatal("remote.yaml should make this machine a client")
	}
	return &clientEnv{t: t, home: home, bin: bin, src: src}
}

func (c *clientEnv) sync() *syncReport {
	c.t.Helper()
	rep, err := runRegistrySync(context.Background(), syncOptions{Source: c.src})
	if err != nil {
		c.t.Fatal(err)
	}
	return rep
}

func (c *clientEnv) read(rel string) string {
	b, _ := os.ReadFile(filepath.Join(c.home, rel))
	return string(b)
}

// The incident: ~/.local/bin/brain is a generated wrapper, and the store named
// 'brain' got a wrapper that exec'd it. With the real binary as the target, the
// sync writes exactly what 'brain remote install' writes.
func TestClientSyncWritesTheSameWrapperAsRemoteInstall(t *testing.T) {
	c := newClientEnv(t, "brain", "recipes")
	t.Setenv("BRAIN_BINARY", c.bin)

	// The state before the bad sync: wrappers written by remote install, `brain` among them.
	for _, n := range []string{"brain", "recipes"} {
		mustWrite(t, filepath.Join(c.home, ".local/bin", n), remoteWrapperBody(n, c.bin)+"# stale\n", 0o755)
	}
	rep := c.sync()
	if rep.Status != "converged" {
		t.Fatalf("sync: %+v", rep)
	}

	ref := t.TempDir()
	if _, err := installRemoteWrappers(ref, c.bin, "", []string{"brain", "recipes"}, false, false); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"brain", "recipes"} {
		want, _ := os.ReadFile(filepath.Join(ref, n))
		got := c.read(".local/bin/" + n)
		if got != string(want) {
			t.Errorf("%s: sync wrote\n%s\nremote install writes\n%s", n, got, want)
		}
		if !strings.Contains(got, "exec "+remoteQuote(c.bin)+` "$@"`) {
			t.Errorf("%s must exec the real bd binary:\n%s", n, got)
		}
		if strings.Contains(got, remoteQuote(filepath.Join(c.home, ".local/bin/brain"))) {
			t.Errorf("%s execs the 'brain' wrapper:\n%s", n, got)
		}
		st, err := os.Stat(filepath.Join(c.home, ".local/bin", n))
		if err != nil || st.Mode().Perm() != 0o755 {
			t.Errorf("%s: wrapper mode %v %v", n, st, err)
		}
	}
}

// Without BRAIN_BINARY the target is the running binary, as for remote install,
// never a path derived from the store name 'brain'.
func TestClientSyncDefaultTargetIsTheRunningBinaryNotTheBrainWrapper(t *testing.T) {
	c := newClientEnv(t, "brain")
	rep := c.sync()
	if rep.Status != "converged" {
		t.Fatalf("sync: %+v", rep)
	}
	self, _ := clientBrainBinary("")
	for _, n := range []string{"brain", "task"} {
		body := c.read(".local/bin/" + n)
		if !strings.Contains(body, "exec "+remoteQuote(self)+` "$@"`) {
			t.Errorf("%s must exec %s:\n%s", n, self, body)
		}
	}
	if strings.Contains(c.read(".local/bin/brain"), filepath.Join(c.home, ".local/bin/brain")) {
		t.Error("the wrapper for the store 'brain' must not exec ~/.local/bin/brain")
	}
}

func TestClientSyncRefusesAWrapperThatWouldExecItselfOrAnotherWrapper(t *testing.T) {
	c := newClientEnv(t, "brain", "recipes")
	brainPath := filepath.Join(c.home, ".local/bin/brain")
	good := remoteWrapperBody("brain", c.bin)
	mustWrite(t, brainPath, good, 0o755)
	recipes := filepath.Join(c.home, ".local/bin/recipes")
	mustWrite(t, recipes, remoteWrapperBody("recipes", c.bin)+"# stale\n", 0o755)
	staleRecipes := c.read(".local/bin/recipes")

	link := filepath.Join(c.home, "bd-elsewhere")
	if err := os.Symlink(brainPath, link); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"the old default, ~/.local/bin/brain (a generated wrapper)": brainPath,
		"a symlink that resolves to a generated wrapper":            link,
		"a wrapper of another store":                                recipes,
		"a binary that is not there":                                filepath.Join(c.home, ".local/bin/nope"),
	} {
		t.Setenv("BRAIN_BINARY", target)
		rep := c.sync()
		if rep.Status != "partial" {
			t.Errorf("%s: a refused sync is partial, got %s", name, rep.Status)
		}
		if len(rep.Failed) == 0 || !strings.Contains(strings.Join(rep.Failed, "\n"), "refused") {
			t.Errorf("%s: the refusal must be reported: %v", name, rep.Failed)
		}
		if c.read(".local/bin/brain") != good {
			t.Errorf("%s: the wrapper for 'brain' was rewritten:\n%s", name, c.read(".local/bin/brain"))
		}
		if c.read(".local/bin/recipes") != staleRecipes {
			t.Errorf("%s: a refused sync must leave the other wrappers as they were", name)
		}
		if _, err := os.Stat(filepath.Join(c.home, ".local/bin/task")); err == nil {
			t.Errorf("%s: no new wrapper may be written", name)
		}
	}
	if applied, ok := readAppliedState(); ok && applied.Epoch != 0 {
		t.Errorf("a refused sync must not mark the machine converged: %+v", applied)
	}
}

// The target is re-checked when the wrapper is written, so a plan made while the
// target was fine cannot write a looping wrapper once it has changed.
func TestApplyRechecksTheTargetBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	bd := filepath.Join(dir, "bd")
	mustWrite(t, bd, "binary", 0o755)
	path := filepath.Join(dir, "task")
	plan := &syncPlan{Actions: []syncAction{{Kind: "wrapper", Name: "task", Op: "create", Path: path,
		body: []byte(remoteWrapperBody("task", bd)), mode: 0o755, execTarget: bd}}}
	mustWrite(t, bd, remoteWrapperBody("bd", "/nowhere"), 0o755) // the target became a wrapper
	res := applySyncPlan(shimEnv{Home: dir}, plan)
	if len(res.Failed) != 1 || !strings.Contains(res.Failed[0].Err.Error(), "refused") {
		t.Fatalf("expected a refusal: %+v", res)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the wrapper must not be written")
	}
}

// ---------------------------------------------------------------------------
// backup and restore keep the executable bit
// ---------------------------------------------------------------------------

func TestSyncBackupAndRestoreKeepModes(t *testing.T) {
	c := newClientEnv(t, "recipes")
	t.Setenv("BRAIN_BINARY", c.bin)
	old := "#!/bin/sh\n# brain-remote-wrapper: generated by 'brain remote install'\nexec '/old/bd' \"$@\"\n"
	for _, n := range []string{"recipes", "task"} {
		mustWrite(t, filepath.Join(c.home, ".local/bin", n), old, 0o755)
	}
	if err := os.Chmod(filepath.Join(c.home, ".local/bin/task"), 0o750); err != nil {
		t.Fatal(err)
	}
	rep := c.sync()
	if rep.Backup == "" {
		t.Fatalf("a sync that replaces wrappers keeps a backup: %+v", rep)
	}
	for _, n := range []string{"recipes", "task"} {
		st, err := os.Stat(filepath.Join(rep.Backup, ".local/bin", n))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o100 == 0 {
			t.Errorf("the backup of %s lost its executable bit: %v", n, st.Mode())
		}
	}
	if st, _ := os.Stat(filepath.Join(rep.Backup, ".local/bin/task")); st.Mode().Perm() != 0o750 {
		t.Errorf("the backup keeps the original mode, got %v", st.Mode().Perm())
	}
	if c.read(".local/bin/recipes") == old {
		t.Fatal("the sync should have replaced the wrapper")
	}

	// A backup written by an older build (0600 copies) still restores executable
	// wrappers: restore.sh makes everything it puts in ~/.local/bin executable.
	if err := os.Chmod(filepath.Join(rep.Backup, ".local/bin/recipes"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".local/bin/recipes", ".local/bin/task"} {
		if err := os.Chmod(filepath.Join(c.home, rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.Command("sh", filepath.Join(rep.Backup, "restore.sh")).CombinedOutput()
	if err != nil {
		t.Fatalf("restore.sh: %v\n%s", err, out)
	}
	for _, n := range []string{"recipes", "task"} {
		st, err := os.Stat(filepath.Join(c.home, ".local/bin", n))
		if err != nil || st.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable after restore: %v %v", n, st, err)
		}
		if c.read(".local/bin/"+n) != old {
			t.Errorf("%s was not restored", n)
		}
	}
	if st, _ := os.Stat(filepath.Join(c.home, ".local/bin/task")); st.Mode().Perm() != 0o755 {
		t.Errorf("restore makes a wrapper in ~/.local/bin 755, got %v", st.Mode().Perm())
	}
}

// ---------------------------------------------------------------------------
// the rollout smoke check
// ---------------------------------------------------------------------------

// runSmoke runs scriptSmoke on this machine with HOME set to home.
func runSmoke(t *testing.T, home, store string, live []string, secs int) (string, int, time.Duration) {
	t.Helper()
	cmd := exec.Command("sh", "-c", scriptSmoke(store, live, secs))
	cmd.Env = append(os.Environ(), "HOME="+home)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code, time.Since(start)
}

func TestRolloutSmokeScript_CatchesSelfExecWithoutRunningIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	home := t.TempDir()
	bin := filepath.Join(home, ".local/bin")
	trap := filepath.Join(home, "ran")
	mustWrite(t, filepath.Join(bin, "bd"), "#!/bin/sh\ntouch "+remoteQuote(trap)+"\nexit 0\n", 0o755)
	realBd := filepath.Join(bin, "bd")

	// A correct wrapper and the incident's: `brain` exec'ing itself, and one wrapper
	// exec'ing another.
	mustWrite(t, filepath.Join(bin, "task"), remoteWrapperBody("task", realBd), 0o755)
	out, code, _ := runSmoke(t, home, "", []string{"task"}, 5)
	if code != 0 || !strings.Contains(out, "wrappers=1") {
		t.Fatalf("a correct wrapper passes: %d %s", code, out)
	}

	mustWrite(t, filepath.Join(bin, "brain"), remoteWrapperBody("brain", filepath.Join(bin, "brain")), 0o755)
	mustWrite(t, filepath.Join(bin, "inbox"), remoteWrapperBody("inbox", filepath.Join(bin, "task")), 0o755)
	out, code, _ = runSmoke(t, home, "task", []string{"task", "brain", "inbox"}, 5)
	if code != 45 {
		t.Fatalf("self-exec and wrapper-exec must fail the smoke check with 45, got %d:\n%s", code, out)
	}
	if !strings.Contains(out, "bad: brain: exec target") || !strings.Contains(out, "wrapper itself") {
		t.Errorf("the self-exec wrapper is named: %s", out)
	}
	if !strings.Contains(out, "bad: inbox:") || !strings.Contains(out, "itself a generated wrapper") {
		t.Errorf("the wrapper that execs a wrapper is named: %s", out)
	}
	if strings.Contains(out, "bad: task") {
		t.Errorf("the correct wrapper is not blamed: %s", out)
	}
	if strings.Contains(out, "read=") {
		t.Errorf("no real read is attempted once a wrapper is bad: %s", out)
	}
	if _, err := os.Stat(trap); err == nil {
		t.Error("the structural check must not run anything")
	}
}

func TestRolloutSmokeScript_ReadHasAWatchdog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	home := t.TempDir()
	bin := filepath.Join(home, ".local/bin")
	// A bd that answers, and a store whose command never returns.
	mustWrite(t, filepath.Join(bin, "bd"), "#!/bin/sh\nexit 0\n", 0o755)
	mustWrite(t, filepath.Join(bin, "task"), remoteWrapperBody("task", filepath.Join(bin, "bd")), 0o755)
	out, code, _ := runSmoke(t, home, "task", []string{"task"}, 5)
	if code != 0 || !strings.Contains(out, "read=task") {
		t.Fatalf("a store that answers passes: %d %s", code, out)
	}

	mustWrite(t, filepath.Join(bin, "bd"), "#!/bin/sh\nexec sleep 60\n", 0o755)
	out, code, took := runSmoke(t, home, "task", []string{"task"}, 1)
	if code != 47 || !strings.Contains(out, "did not answer within 1s") {
		t.Fatalf("a hanging read is killed and reported: %d %s", code, out)
	}
	if took > 15*time.Second {
		t.Errorf("the watchdog did not end the read promptly: %v", took)
	}
}

func TestRolloutSmokeScript_DirectWrappersAreCountedNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	home := t.TempDir()
	bin := filepath.Join(home, ".local/bin")
	mustWrite(t, filepath.Join(bin, "bd"), "#!/bin/sh\nexit 0\n", 0o755)
	mustWrite(t, filepath.Join(bin, "recipes"), syncWrapperBody(storeregistry.New("recipes"), shimEnv{Home: home}), 0o755)
	mustWrite(t, filepath.Join(bin, "unrelated"), "#!/bin/sh\nexec /bin/false\n", 0o755) // not generated: ignored
	out, code, _ := runSmoke(t, home, "", []string{"recipes", "task"}, 5)
	if code != 0 || !strings.Contains(out, "wrappers=1 dynamic=1") {
		t.Fatalf("a direct wrapper is counted and not followed: %d %s", code, out)
	}
}

func TestRolloutSmokeScript_OnlyLiveWrappersAreChecked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	home := t.TempDir()
	bin := filepath.Join(home, ".local/bin")
	mustWrite(t, filepath.Join(bin, "bd"), "#!/bin/sh\nexit 0\n", 0o755)
	realBd := filepath.Join(bin, "bd")
	mustWrite(t, filepath.Join(bin, "chores"), remoteWrapperBody("chores", realBd), 0o755)
	mustWrite(t, filepath.Join(bin, "todo"), remoteWrapperBody("todo", realBd), 0o755) // an alias
	// Backups carry the marker but no exec target (or a dead one): none is a command.
	marked := "#!/bin/sh\n# Auto-generated by 'brain stores sync'\necho old\n"
	for _, n := range []string{"chores.pre-remote-route-20260924", "friction.pre-remote-route-20260924", "chores.bak", "chores.orig", "stale"} {
		mustWrite(t, filepath.Join(bin, n), marked, 0o755)
	}
	live := []string{"chores", "todo"}
	out, code, _ := runSmoke(t, home, "chores", live, 5)
	if code != 0 {
		t.Fatalf("backups and unregistered files must not fail the check: %d\n%s", code, out)
	}
	if !strings.Contains(out, "wrappers=2 dynamic=0") {
		t.Errorf("only the two live wrappers are counted: %s", out)
	}
	for _, n := range []string{"chores.pre-remote-route-20260924", "friction.pre-remote-route-20260924", "chores.bak", "chores.orig", "stale"} {
		if !strings.Contains(out, n) || !strings.Contains(out, "skipped=") {
			t.Errorf("%s is listed as skipped: %s", n, out)
		}
	}

	// A live wrapper that execs itself still fails, backups beside it or not.
	mustWrite(t, filepath.Join(bin, "chores"), remoteWrapperBody("chores", filepath.Join(bin, "chores")), 0o755)
	out, code, _ = runSmoke(t, home, "chores", live, 5)
	if code != 45 || !strings.Contains(out, "bad: chores: exec target") || !strings.Contains(out, "wrapper itself") {
		t.Fatalf("a live self-exec wrapper still fails: %d\n%s", code, out)
	}
	if strings.Contains(out, "bad: chores.pre") || strings.Contains(out, "bad: stale") {
		t.Errorf("backups are never blamed: %s", out)
	}
}

const (
	fakeBackupDir = "/Users/macbook/.local/share/brain/registry-backup/20261022-120000"
	fakeSyncOut   = "macbook: epoch 6 → 7, 60 change(s) (backup: " + fakeBackupDir + ")\n  UPDATE wrapper task\nconverged at epoch 7 (60 stores)\n"
)

func clientHarness(t *testing.T) (*rolloutHarness, *fakeMachine) {
	h := newHarness(t)
	m := clientWithNewestStore(h)
	m.applied, m.registryAt, m.syncOutput = 6, 6, fakeSyncOut
	return h, m
}

func TestRolloutApply_SmokeCatchesASelfExecWrapperAfterConvergeAndPrintsTheRestore(t *testing.T) {
	h, m := clientHarness(t)
	m.smokeBad = "brain: exec target /Users/macbook/.local/bin/brain is the wrapper itself (re-executes forever)"
	rep := h.do(true, "macbook")

	if got := stepStatus(rep, "macbook", "converge"); got != statDone {
		t.Fatalf("converge = %s", got)
	}
	if got := stepStatus(rep, "macbook", "smoke"); got != statFail {
		t.Fatalf("smoke = %s, want FAIL", got)
	}
	if got := stepStatus(rep, "macbook", "prove"); got != statBlocked {
		t.Errorf("prove must not run after a failed smoke check, got %s", got)
	}
	var smoke stepOutcome
	for _, s := range machineReport(rep, "macbook").Steps {
		if s.Step == "smoke" {
			smoke = s
		}
	}
	wantCmd := "ssh macbook 'sh '\\''" + fakeBackupDir + "/restore.sh'\\'''"
	if !strings.Contains(smoke.Detail, "the wrapper itself") || !strings.Contains(smoke.Fix, wantCmd) {
		t.Errorf("the failure names the wrapper and the exact restore command:\n detail: %s\n fix: %s\n want %s", smoke.Detail, smoke.Fix, wantCmd)
	}
	if !strings.Contains(smoke.Fix, "to put them back as they were") {
		t.Errorf("fix: %s", smoke.Fix)
	}
	if m.restores != 0 {
		t.Error("nothing is restored automatically")
	}
	if rep.Stopped == nil || rep.Stopped.Step != "smoke" || !strings.Contains(rep.Stopped.Fix, fakeBackupDir) {
		t.Errorf("the run stops at smoke with the restore command: %+v", rep.Stopped)
	}

	rec := h.records.rows["macbook"]
	if rec.Outcome != storeregistry.RolloutFailed || rec.Step != "smoke" {
		t.Fatalf("the failure is recorded: %+v", rec)
	}
	if !strings.HasPrefix(rec.Detail, "RESTORE: ") || !strings.Contains(rec.Detail, fakeBackupDir+"/restore.sh") {
		t.Errorf("the record carries the restore command first: %q", rec.Detail)
	}
}

func TestRolloutApply_AFailedProveAfterASyncPrintsTheRestoreToo(t *testing.T) {
	h, m := clientHarness(t)
	m.storeBroken = true // the read through the newest store fails at prove
	rep := h.do(true, "macbook")
	if got := stepStatus(rep, "macbook", "smoke"); got != statOK && got != statDone {
		t.Fatalf("smoke = %s", got)
	}
	if got := stepStatus(rep, "macbook", "prove"); got != statFail {
		t.Fatalf("prove = %s", got)
	}
	if rep.Stopped == nil || rep.Stopped.Step != "prove" || !strings.Contains(rep.Stopped.Fix, fakeBackupDir+"/restore.sh") {
		t.Errorf("a prove failure after a sync says how to restore: %+v", rep.Stopped)
	}
}

func TestRolloutApply_NoRestoreHintWhenTheSyncChangedNothing(t *testing.T) {
	h, m := clientHarness(t)
	m.syncOutput = "macbook: epoch 6 → 7, 0 change(s)\nconverged at epoch 7 (60 stores)\n"
	m.smokeBad = "brain: exec target x is the wrapper itself"
	rep := h.do(true, "macbook")
	if rep.Stopped == nil || rep.Stopped.Step != "smoke" {
		t.Fatalf("stopped: %+v", rep.Stopped)
	}
	if strings.Contains(rep.Stopped.Fix, "restore.sh") {
		t.Errorf("there is nothing to restore: %s", rep.Stopped.Fix)
	}
}

func TestRolloutSmokeRunsBetweenConvergeAndProve(t *testing.T) {
	var ids []string
	for _, s := range installSteps() {
		ids = append(ids, s.ID)
	}
	joined := strings.Join(ids, ",")
	if !strings.Contains(joined, "converge,smoke,prove") {
		t.Errorf("steps: %s", joined)
	}
}

func TestRolloutPlan_SmokeIsPendingUntilTheSyncRuns(t *testing.T) {
	h, _ := clientHarness(t)
	rep := h.do(false, "macbook")
	if got := stepStatus(rep, "macbook", "smoke"); got != statPending {
		t.Errorf("smoke in a plan with a pending sync = %s", got)
	}
}
