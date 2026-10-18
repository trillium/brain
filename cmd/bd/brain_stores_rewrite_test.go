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
	dir := filepath.Join(home, ".config", "brain")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, configfile.StoreHostFileName),
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
