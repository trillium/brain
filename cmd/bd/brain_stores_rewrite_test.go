package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// rewriteAllStoreWrappers regenerates each wrapper and fails stores whose
// wrapper does not resolve a database.
func TestRewriteAllStoreWrappersVerifiesEach(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binDir := t.TempDir()
	// Fake bd: resolves a database only when BEADS_DIR exists.
	writeExec(t, filepath.Join(binDir, "bd"), "#!/bin/sh\n[ -d \"$BEADS_DIR\" ] || { echo \"no database at $BEADS_DIR\" >&2; exit 1; }\n")
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	good := filepath.Join(home, "data", "good", ".beads")
	if err := os.MkdirAll(good, 0o755); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	stores := map[string]storeEntry{
		"good": {Path: good},
		"gone": {Path: filepath.Join(home, "data", "gone", ".beads")},
	}
	// A stale hardcoded wrapper that the rewrite must replace.
	if err := os.MkdirAll(filepath.Join(home, ".local", "bin"), 0o755); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	writeExec(t, filepath.Join(home, ".local", "bin", "good"), "#!/bin/sh\nexport BEADS_DIR=/Users/someone/data/good/.beads\nexec bd \"$@\"\n")

	res := rewriteAllStoreWrappers("bd", stores, 2, 10*time.Second)
	byName := map[string]wrapperRewrite{}
	for _, r := range res {
		byName[r.Name] = r
	}
	if byName["good"].Status != "ok" {
		t.Errorf("good: %+v", byName["good"])
	}
	if byName["gone"].Status != "fail" || !strings.Contains(byName["gone"].Reason, "no database") {
		t.Errorf("gone should fail loudly: %+v", byName["gone"])
	}
	body, _ := os.ReadFile(filepath.Join(home, ".local", "bin", "good"))
	if strings.Contains(string(body), "/Users/someone") || strings.Contains(string(body), home) {
		t.Errorf("stale wrapper not rewritten home-relative:\n%s", body)
	}
	if f := failedRewrites(res); len(f) != 1 || f[0].Name != "gone" {
		t.Errorf("failedRewrites = %+v", f)
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
