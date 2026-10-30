package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/configfile"
)

// Generated wrappers must never carry the generating machine's home path.
func TestGeneratedWrappersAreHomeRelative(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	beadsDir := filepath.Join(home, "data", "recipes", ".beads")

	embedded, err := writeStoreWrapper("recipes", beadsDir, "")
	if err != nil {
		t.Fatal(err)
	}
	server, err := writeServerStoreWrapper("chores", filepath.Join(home, "data", "chores", ".beads"), "", "127.0.0.1", 3307, "chores")
	if err != nil {
		t.Fatal(err)
	}
	alias, err := writeStoreWrapperForAlias("rcp", beadsDir, "", "recipes")
	if err != nil {
		t.Fatal(err)
	}
	for _, wp := range []string{embedded, server, alias} {
		body, err := os.ReadFile(wp)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), home) {
			t.Errorf("%s contains absolute home %q:\n%s", wp, home, body)
		}
		if !strings.Contains(string(body), `"$HOME/data/`) {
			t.Errorf("%s has no $HOME-relative path:\n%s", wp, body)
		}
	}
	body, _ := os.ReadFile(server)
	if !strings.Contains(string(body), `export BRAIN_KNOWLEDGE_ROOT="$HOME/data/chores"`) {
		t.Errorf("knowledge root not home-relative:\n%s", body)
	}
}

// writeSharedStoreHostEnv gives the test HOME the machine's shared store host
// file, so a repointed wrapper's runtime block can resolve a host when probed.
func writeSharedStoreHostEnv(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path := configfile.DefaultStoreHostPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path,
		[]byte("BEADS_DOLT_SERVER_HOST=10.9.9.9\nBEADS_DOLT_SERVER_PORT=3311\n"), 0o600); err != nil {
		t.Fatalf("write store-host.env: %v", err)
	}
}

// A wrapper that baked a host pin is repointed once: the pin is dropped, the
// runtime block is installed, and a second pass changes nothing — including
// the block it just installed.
func TestRewriteWrapperInPlace_RepointsOnceAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old")
	body := "#!/bin/sh\nexport BEADS_DIR=\"/data/x/.beads\"\n" +
		"export BEADS_DOLT_SERVER_HOST=10.0.0.1\n" +
		"export BEADS_DOLT_SERVER_PORT=3307\n" +
		"exec bd \"$@\"\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // test wrapper
		t.Fatalf("write: %v", err)
	}

	repointed, err := rewriteWrapperInPlace(path, true)
	if err != nil || !repointed {
		t.Fatalf("rewriteWrapperInPlace = %v, %v; want a repoint", repointed, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(after), "export BEADS_DOLT_SERVER_HOST=10.0.0.1") {
		t.Errorf("baked host pin survived the repoint:\n%s", after)
	}
	if !strings.Contains(string(after), `export BEADS_DOLT_SERVER_HOST="$_want_host"`) {
		t.Errorf("runtime block missing after the repoint:\n%s", after)
	}

	again, err := rewriteWrapperInPlace(path, true)
	if err != nil || again {
		t.Fatalf("second rewrite = %v, %v; want an unchanged no-op", again, err)
	}
}

// A wrapper that is rewritten is backed up first, so the repoint is reversible:
// the .bak-repoint-<timestamp> copy is the pre-rewrite file, and it carries no
// exec bit so host promotion never installs it.
func TestRewriteWrapperInPlace_BacksUpBeforeRewriting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old")
	body := "#!/bin/sh\nexport BEADS_DIR=\"/data/x/.beads\"\n" +
		"export BEADS_DOLT_SERVER_HOST=10.0.0.1\n" +
		"exec bd \"$@\"\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // test wrapper
		t.Fatalf("write: %v", err)
	}

	if _, err := rewriteWrapperInPlace(path, true); err != nil {
		t.Fatalf("rewriteWrapperInPlace: %v", err)
	}
	backups := backupPaths(t, dir)
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one", backups)
	}
	backup, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatalf("reading backup: %v", err)
	}
	if string(backup) != body {
		t.Errorf("backup = %q, want the pre-rewrite wrapper", backup)
	}
	info, err := os.Stat(backups[0])
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if info.Mode()&0o111 != 0 {
		t.Errorf("backup is executable (mode %v); host promotion would install it", info.Mode())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading wrapper: %v", err)
	}
	if string(after) == body {
		t.Error("wrapper was not rewritten")
	}
}

// backupPaths returns the repoint backup files in dir.
func backupPaths(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var out []string
	for _, e := range ents {
		if strings.Contains(e.Name(), wrapperBackupMarker) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// The run-time host block must run before EVERY bd invocation, not just the
// final exec: a wrapper with an earlier bd call (the fleet's inbox create-emit
// shape) must hand the shared server to that call too — and a block an older
// build left above the last exec must be moved to that position.
func TestRewriteAllStoreWrappers_BlockCoversEveryBdInvocation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binDir := t.TempDir()
	writeExec(t, filepath.Join(binDir, "bd"), "#!/bin/sh\necho \"host=$BEADS_DOLT_SERVER_HOST port=$BEADS_DOLT_SERVER_PORT\"\n")
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	writeSharedStoreHostEnv(t, home)

	beadsDir := filepath.Join(home, "data", "inbox", ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&configfile.Config{DoltMode: configfile.DoltModeServer}).Save(beadsDir); err != nil {
		t.Fatal(err)
	}
	wdir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(wdir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExec(t, filepath.Join(wdir, "inbox"),
		"#!/bin/sh\nexport BEADS_DIR=\"$HOME/data/inbox/.beads\"\n"+
			"if [ \"${1:-}\" = \"create\" ]; then\n  bd \"$@\"\n  exit $?\nfi\n"+
			sharedServerHostBlock+"exec bd \"$@\"\n")

	res := rewriteAllStoreWrappers("bd", map[string]storeEntry{"inbox": {Path: beadsDir}}, 1, 10*time.Second)
	if len(res) != 1 || res[0].Status != "ok" {
		t.Fatalf("results = %+v, want the wrapper verified ok", res)
	}

	cmd := exec.Command(filepath.Join(wdir, "inbox"), "create") //nolint:gosec // the wrapper under test
	cmd.Env = []string{"HOME=" + home, "PATH=" + binDir + ":" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the rewritten wrapper: %v (%s)", err, out)
	}
	if !strings.Contains(string(out), "host=10.9.9.9 port=3311") {
		t.Errorf("the earlier bd call got %q, want the shared file's host and port", strings.TrimSpace(string(out)))
	}
}

// A pin that is the only statement of a branch must be neutralized, not
// deleted: deleting the line leaves a wrapper that no longer parses.
func TestRewriteWrapperInPlace_NeutralizesMultiLineConditionalPin(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no POSIX shell to validate with")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "inbox")
	body := "#!/bin/sh\n" +
		"export BEADS_DIR=\"$HOME/data/inbox/.beads\"\n" +
		"if [ -z \"${BEADS_DOLT_SERVER_HOST+x}\" ]; then\n" +
		"  export BEADS_DOLT_SERVER_HOST=127.0.0.1\n" +
		"fi\n" +
		"exec bd \"$@\"\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // test wrapper
		t.Fatalf("write: %v", err)
	}

	if _, err := rewriteWrapperInPlace(path, true); err != nil {
		t.Fatalf("rewriteWrapperInPlace: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(after), "BEADS_DOLT_SERVER_HOST=127.0.0.1") {
		t.Errorf("the stale pin survived:\n%s", after)
	}
	if !strings.Contains(string(after), "then\n") || !strings.Contains(string(after), "fi\n") {
		t.Errorf("the branch structure was damaged:\n%s", after)
	}
	if err := checkShellSyntax(string(after)); err != nil {
		t.Errorf("the rewritten wrapper does not parse: %v\n%s", err, after)
	}
	if pin, found := bakedServerPinIn(path); found {
		t.Errorf("a pin %q is still detected after the rewrite:\n%s", pin, after)
	}
}

// The writer must never install a wrapper that does not parse: a candidate that
// fails `sh -n` is refused, the live file is left alone, and the backup stays.
func TestRewriteWrapperInPlace_RefusesUnparseableCandidate(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no POSIX shell to validate with")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "broken")
	body := "#!/bin/sh\n" +
		"export BEADS_DIR=\"/Users/someone/data/broken/.beads\"\n" +
		"if true; then\n" +
		"fi\n" +
		"exec bd \"$@\"\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // test wrapper
		t.Fatalf("write: %v", err)
	}

	_, err := rewriteWrapperInPlace(path, true)
	if err == nil {
		t.Fatal("expected the unparseable candidate to be refused")
	}
	if !strings.Contains(err.Error(), "does not parse") {
		t.Errorf("error = %v, want a syntax refusal", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read: %v", readErr)
	}
	if string(after) != body {
		t.Errorf("the live wrapper was overwritten:\n%s", after)
	}
	if got := backupPaths(t, dir); len(got) != 1 {
		t.Errorf("backups = %v, want the repoint backup left in place", got)
	}
}

func TestShellPathExpr(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	for in, want := range map[string]string{
		"/home/u/data/x/.beads": `"$HOME/data/x/.beads"`,
		"/home/u":               `"$HOME"`,
		"/home/u/a$b":           `"$HOME/a\$b"`,
		"/home/user/x":          `"/home/user/x"`, // prefix of a sibling, not under home
		"/data/x":               `"/data/x"`,
	} {
		if got := shellPathExpr(in); got != want {
			t.Errorf("shellPathExpr(%q) = %s, want %s", in, got, want)
		}
	}
}

// Existing wrappers are patched in place: only home paths change, hand-added
// env survives, wrappers are never created, and alias wrappers are covered.
func TestRewriteAllStoreWrappersVerifiesEach(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binDir := t.TempDir()
	writeExec(t, filepath.Join(binDir, "bd"), "#!/bin/sh\n[ -d \"$BEADS_DIR\" ] || { echo \"no database at $BEADS_DIR\" >&2; exit 1; }\n")
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	writeSharedStoreHostEnv(t, home)
	good := filepath.Join(home, "data", "good", ".beads")
	if err := os.MkdirAll(good, 0o755); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	// The store's own metadata.json is what classifies the wrapper as a
	// shared-server one; without it the wrapper is reported unclassified.
	if err := (&configfile.Config{DoltMode: configfile.DoltModeServer}).Save(good); err != nil {
		t.Fatal(err)
	}
	stores := map[string]storeEntry{
		"good":   {Path: good},
		"gone":   {Path: filepath.Join(home, "data", "gone", ".beads")},
		"nowrap": {Path: good},
	}
	wdir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(wdir, 0o755); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	writeExec(t, filepath.Join(wdir, "good"), "#!/bin/sh\nexport BEADS_DIR=/Users/someone/data/good/.beads\nexport BRAIN_KNOWLEDGE_ROOT=\"/Users/someone/data/good\"\nexport BRAIN_EXFIL_FLAT=1\nexport CUSTOM_PIN=keepme\nexec bd \"$@\"\n")
	writeExec(t, filepath.Join(wdir, "gone"), "#!/bin/sh\nexec env BEADS_DIR=\""+filepath.Join(home, "data", "gone", ".beads")+"\" BD_NAME=gone bd \"$@\"\n")
	writeExec(t, filepath.Join(wdir, "gd"), "#!/bin/sh\n# Auto-generated by 'brain stores alias good gd'.\nexec env BEADS_DIR=\"$HOME/data/good/.beads\" BD_NAME=\"good\" bd \"$@\"\n")
	writeExec(t, filepath.Join(wdir, "unrelated"), "#!/bin/sh\nexport BEADS_DIR=/Users/someone/x\n")

	res := rewriteAllStoreWrappers("bd", stores, 2, 10*time.Second)
	byName := map[string]wrapperRewrite{}
	for _, r := range res {
		byName[r.Name] = r
	}
	if byName["good"].Status != "ok" || byName["gd"].Status != "ok" {
		t.Errorf("good/gd should verify: %+v %+v", byName["good"], byName["gd"])
	}
	if byName["gone"].Status != "fail" || !strings.Contains(byName["gone"].Reason, "no database") {
		t.Errorf("gone should fail loudly: %+v", byName["gone"])
	}
	if _, ok := byName["nowrap"]; ok {
		t.Errorf("store without a wrapper must be skipped: %+v", byName["nowrap"])
	}
	if _, ok := byName["unrelated"]; ok {
		t.Errorf("non-store script must be left alone")
	}
	if _, err := os.Stat(filepath.Join(wdir, "nowrap")); err == nil {
		t.Error("wrapper was created for a store that had none")
	}
	for _, n := range []string{"good", "gd"} {
		body, _ := os.ReadFile(filepath.Join(wdir, n))
		if strings.Contains(string(body), "/Users/someone") || strings.Contains(string(body), home) {
			t.Errorf("%s not rewritten home-relative:\n%s", n, body)
		}
		if !strings.Contains(string(body), `BEADS_DIR="$HOME/data/good/.beads"`) {
			t.Errorf("%s BEADS_DIR wrong:\n%s", n, body)
		}
	}
	body, _ := os.ReadFile(filepath.Join(wdir, "good"))
	for _, want := range []string{`BRAIN_KNOWLEDGE_ROOT="$HOME/data/good"`, "BRAIN_EXFIL_FLAT=1", "CUSTOM_PIN=keepme", `exec bd "$@"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("good lost %q:\n%s", want, body)
		}
	}
	if f := failedRewrites(res); len(f) != 1 || f[0].Name != "gone" {
		t.Errorf("failedRewrites = %+v", f)
	}

	// The backups a repoint leaves behind are not wrappers themselves.
	firstBackups := backupPaths(t, wdir)
	if len(firstBackups) == 0 {
		t.Fatal("no repoint backups were written")
	}
	res = rewriteAllStoreWrappers("bd", stores, 2, 10*time.Second)
	for _, r := range res {
		if strings.Contains(r.Name, wrapperBackupMarker) {
			t.Errorf("backup %q was treated as a wrapper", r.Name)
		}
	}
	if got := backupPaths(t, wdir); len(got) != len(firstBackups) {
		t.Errorf("backups = %v after a second pass, want %d and no backup of a backup", got, len(firstBackups))
	}
}

// A registry carried from another machine names a foreign path, so the store's
// own metadata.json is reachable only through the wrapper's own BEADS_DIR. The
// wrapper's baked host pin must still be dropped, so the store reaches the
// shared server instead of the stale host (task-8m5x on exactly these
// machines).
func TestRewriteAllStoreWrappers_RepointsFromTheWrappersOwnBeadsDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binDir := t.TempDir()
	writeExec(t, filepath.Join(binDir, "bd"), "#!/bin/sh\necho \"host=$BEADS_DOLT_SERVER_HOST port=$BEADS_DOLT_SERVER_PORT\"\n")
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	writeSharedStoreHostEnv(t, home)
	localBeads := filepath.Join(home, "data", "task", ".beads")
	if err := os.MkdirAll(localBeads, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&configfile.Config{DoltMode: configfile.DoltModeServer}).Save(localBeads); err != nil {
		t.Fatal(err)
	}
	wdir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(wdir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExec(t, filepath.Join(wdir, "task"),
		"#!/bin/sh\nexport BEADS_DIR=\"$HOME/data/task/.beads\"\nexport BEADS_DOLT_SERVER_HOST=10.0.0.1\nexport BEADS_DOLT_SERVER_PORT=3307\nexec bd \"$@\"\n")

	stores := map[string]storeEntry{"task": {Path: "/Users/someone-else/data/task/.beads"}}
	res := rewriteAllStoreWrappers("bd", stores, 1, 10*time.Second)
	if len(res) != 1 || res[0].Status != "ok" {
		t.Fatalf("results = %+v, want the wrapper verified ok", res)
	}
	if !res[0].Repointed {
		t.Error("the wrapper was not repointed at the shared store host config")
	}

	// The wrapper the store's own shell uses must hand the shared file's server to
	// bd, not the stale baked pin.
	cmd := exec.Command(filepath.Join(wdir, "task")) //nolint:gosec // the wrapper under test
	cmd.Env = []string{"HOME=" + home, "PATH=" + binDir + ":" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the rewritten wrapper: %v (%s)", err, out)
	}
	if !strings.Contains(string(out), "host=10.9.9.9 port=3311") {
		t.Errorf("rewritten wrapper handed %q, want the shared file's host and port", strings.TrimSpace(string(out)))
	}
}

// A host pin written in a shape the rewrite cannot safely replace (a
// conditional default, not a whole-line assignment) must make the wrapper fail
// rather than be reported verified: the operator must never be told the repoint
// happened while the stale host survives (task-8m5x).
func TestRewriteAllStoreWrappers_ReportsConditionalPinAsFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binDir := t.TempDir()
	writeExec(t, filepath.Join(binDir, "bd"), "#!/bin/sh\necho ok\n")
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	writeSharedStoreHostEnv(t, home)

	beadsDir := filepath.Join(home, "data", "inbox", ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&configfile.Config{DoltMode: configfile.DoltModeServer}).Save(beadsDir); err != nil {
		t.Fatal(err)
	}
	wdir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(wdir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExec(t, filepath.Join(wdir, "inbox"),
		"#!/bin/sh\nif [ -z \"${BEADS_DOLT_SERVER_HOST+x}\" ]; then BEADS_DOLT_SERVER_HOST=127.0.0.1; fi\nexport BEADS_DOLT_SERVER_HOST\nexec bd \"$@\"\n")

	res := rewriteAllStoreWrappers("bd", map[string]storeEntry{"inbox": {Path: beadsDir}}, 1, 10*time.Second)
	if len(res) != 1 {
		t.Fatalf("results = %+v, want one wrapper", res)
	}
	if res[0].Status != "fail" || !strings.Contains(res[0].Reason, "baked Dolt host pin remains") {
		t.Fatalf("results = %+v, want a failure naming the surviving pin", res)
	}
}

// TestRequireSharedStoreHostFile locks the up-front refusal: the repoint verb
// must not touch a wrapper until the shared file exists, and a process env value
// alone does not satisfy that.
func TestRequireSharedStoreHostFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(configfile.StoreHostKeyHost, "")
	t.Setenv(configfile.StoreHostKeyPort, "")

	err := requireSharedStoreHostFile()
	if err == nil {
		t.Fatal("expected a refusal when the shared file is absent")
	}
	if !strings.Contains(err.Error(), configfile.StoreHostKeyHost) || !strings.Contains(err.Error(), configfile.StoreHostKeyPort) {
		t.Errorf("the refusal must name both keys; got: %v", err)
	}

	t.Setenv(configfile.StoreHostKeyHost, "10.0.0.1")
	t.Setenv(configfile.StoreHostKeyPort, "3307")
	if err := requireSharedStoreHostFile(); err == nil {
		t.Fatal("a process env value alone must not satisfy the precondition")
	}

	hostFile := configfile.DefaultStoreHostPath()
	if err := os.MkdirAll(filepath.Dir(hostFile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(hostFile, []byte("BEADS_DOLT_SERVER_HOST=10.0.0.1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := requireSharedStoreHostFile(); err == nil {
		t.Fatal("a present-but-unusable file must surface the validation error")
	}

	if err := os.WriteFile(hostFile, []byte("BEADS_DOLT_SERVER_HOST=10.0.0.1\nBEADS_DOLT_SERVER_PORT=3307\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := requireSharedStoreHostFile(); err != nil {
		t.Errorf("a usable file must pass: %v", err)
	}
}

func TestBakedServerPinLine_RecognizesWrapperShapes(t *testing.T) {
	pins := []string{
		"export BEADS_DOLT_SERVER_HOST=10.0.0.1",
		"BEADS_DOLT_SERVER_PORT=3307",
		`if [ -z "${BEADS_DOLT_SERVER_HOST+x}" ]; then BEADS_DOLT_SERVER_HOST=127.0.0.1; fi`,
		"exec env BEADS_DOLT_SERVER_HOST=10.0.0.1 bd list",
	}
	for _, pin := range pins {
		if !bakedServerPinLine(pin) {
			t.Errorf("bakedServerPinLine(%q) = false, want a pin", pin)
		}
	}
	notPins := []string{
		`export BEADS_DOLT_SERVER_HOST="$_want_host"`,
		"export BEADS_DOLT_SERVER_HOST",
		`_want_host="${BEADS_DOLT_SERVER_HOST:-}"`,
		"# BEADS_DOLT_SERVER_HOST=127.0.0.1 documents the old default",
		`[ "$BEADS_DOLT_SERVER_HOST" = "127.0.0.1" ]`,
		`echo "beads: write BEADS_DOLT_SERVER_HOST=<host> and BEADS_DOLT_SERVER_PORT=<port> there, or export them for this shell" >&2`,
	}
	for _, line := range notPins {
		if bakedServerPinLine(line) {
			t.Errorf("bakedServerPinLine(%q) = true, want not a pin", line)
		}
	}
}

// A wrapper written by an older build carries the run-time block immediately
// above the last exec, so an earlier bd call (the fleet's emit-hook shape) never
// saw the shared server. The repoint pass must move the block to the canonical
// position even though '_store_host_file=' is already present.
func TestCanonicalizeWrapperHost_RepositionsAnExistingBlock(t *testing.T) {
	old := "#!/bin/sh\n" +
		"export BEADS_DIR=\"$HOME/data/inbox/.beads\"\n" +
		"if [ \"${1:-}\" = create ]; then\n" +
		"  bd \"$@\"\n" +
		"fi\n" +
		sharedServerHostBlock +
		"exec bd \"$@\"\n"

	out, changed := canonicalizeWrapperHost(old)
	if !changed {
		t.Fatal("an existing block in the old position must be repositioned")
	}
	blockStart := strings.Index(out, sharedServerHostBlock)
	firstBD := strings.Index(out, "bd \"$@\"")
	if blockStart < 0 || firstBD < 0 || blockStart > firstBD {
		t.Fatalf("the runtime block must precede the first bd call:\n%s", out)
	}

	again, changedAgain := canonicalizeWrapperHost(out)
	if changedAgain || again != out {
		t.Fatalf("a second canonicalize pass changed the wrapper again:\n%s", again)
	}
}

// A store provisioned with --embedded has no metadata.json by design: it keeps
// its minimal wrapper and is reported as skipped, not as a failure, so
// 'brain remote set/install' does not fail on a host that has one.
func TestRewriteAllStoreWrappers_SkipsEmbeddedStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binDir := t.TempDir()
	writeExec(t, filepath.Join(binDir, "bd"), "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	writeSharedStoreHostEnv(t, home)

	beadsDir := filepath.Join(home, "data", "scratch", ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("issue-prefix: scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wdir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(wdir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExec(t, filepath.Join(wdir, "scratch"),
		"#!/bin/sh\nexport BEADS_DIR=\"$HOME/data/scratch/.beads\"\nexec bd \"$@\"\n")

	res := rewriteAllStoreWrappers("bd", map[string]storeEntry{"scratch": {Path: beadsDir}}, 1, 10*time.Second)
	if len(res) != 1 || res[0].Status != "embedded" {
		t.Fatalf("results = %+v, want the embedded store reported as skipped", res)
	}
	if f := failedRewrites(res); len(f) != 0 {
		t.Errorf("failedRewrites = %+v, want embedded skipped", f)
	}
	body, err := os.ReadFile(filepath.Join(wdir, "scratch"))
	if err != nil {
		t.Fatalf("reading wrapper: %v", err)
	}
	if strings.Contains(string(body), "_store_host_file=") {
		t.Errorf("an embedded store's wrapper must not get the runtime host block:\n%s", body)
	}
}

func hostWithRewriteResult(t *testing.T, stdout string, code int) *remoteConfig {
	t.Helper()
	_, hostBin := remoteEnv(t)
	writeExec(t, filepath.Join(hostBin, "brain"), "#!/bin/sh\ncat <<'EOF'\n"+stdout+"\nEOF\nexit "+string(rune('0'+code))+"\n")
	return &remoteConfig{Host: "me@host", BinDir: hostBin}
}

func TestVerifyHostWrappersListsFailures(t *testing.T) {
	cfg := hostWithRewriteResult(t, `{"total":3,"failed":2,"stores":[{"name":"a","status":"ok"},{"name":"b","status":"fail","reason":"no database at /x"},{"name":"c","status":"fail","reason":"boom"}]}`, 1)
	_, err := verifyHostWrappers(t.Context(), cfg)
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, want := range []string{"2 of 3", "b: no database at /x", "c: boom"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestVerifyHostWrappersOK(t *testing.T) {
	cfg := hostWithRewriteResult(t, `{"total":1,"failed":0,"stores":[{"name":"a","status":"ok"}]}`, 0)
	n, err := verifyHostWrappers(t.Context(), cfg)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestVerifyHostWrappersHostCrashIsAnError(t *testing.T) {
	cfg := hostWithRewriteResult(t, `brain: command not found`, 1)
	if _, err := verifyHostWrappers(t.Context(), cfg); err == nil {
		t.Fatal("expected error when host prints no result")
	}
}
