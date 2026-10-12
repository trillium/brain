package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// twoHostSSH is an ssh stand-in with two machines: "old" answers every call
// with a moved notice (exit 75), "new" runs the command as the single-host
// fake does.
func twoHostSSH(t *testing.T, notice string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "twohostssh")
	body := `#!/bin/sh
host=
while [ $# -gt 0 ]; do
  case "$1" in
    -o) shift 2 ;;
    -O) shift 2; exit 0 ;;
    -t|-T) shift ;;
    --) shift; break ;;
    *) host=$1; shift ;;
  esac
done
if [ "$host" = old ]; then
  printf '%s\n' ` + remoteQuote(notice) + ` >&2
  exit 75
fi
exec sh -c "$*"
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // test helper
		t.Fatal(err)
	}
	return script
}

func movedFixture(t *testing.T, net fakeNet) (cfg *remoteConfig, home string) {
	t.Helper()
	home, hostBin := remoteEnv(t)
	hostTask(t, hostBin)
	n := movedNotice{Target: "new", PromoteID: pid1, Generation: 2, From: "M-old", At: "2026-10-12"}
	t.Setenv(envRemoteSSH, twoHostSSH(t, n.line()))
	withFakeNet(t, net)
	cfg = &remoteConfig{Host: "old", BinDir: hostBin}
	if err := saveRemoteConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg, home
}

var genuineNew = fakeNet{"new": {machine: "M-new", host: &hostConfig{Role: roleHost, Generation: 2,
	Lineage: []lineageEntry{{PromoteID: pid1, Generation: 2, From: "M-old", To: "M-new"}}}}}

func TestForwardRemoteFollowsAVerifiedMoveOnce(t *testing.T) {
	cfg, _ := movedFixture(t, genuineNew)
	stdout, stderr, code := runForward(t, cfg, "task", []string{"list", "x"}, "")
	if code != 7 { // the fake host command's own status: it ran, on the new host
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "[list]") || !strings.Contains(stdout, "[x]") {
		t.Errorf("the command did not run on the new host: %q", stdout)
	}
	// Exactly one line says the client re-pointed; the old host's notice is not echoed.
	if n := strings.Count(stderr, "re-pointed this client from old to new"); n != 1 {
		t.Errorf("want one re-pointed line, got %d in %q", n, stderr)
	}
	if strings.Contains(stderr, hostMovedMarker) {
		t.Errorf("the raw notice leaked to the user: %q", stderr)
	}
	saved, err := loadRemoteConfig()
	if err != nil || saved.Host != "new" {
		t.Errorf("remote.yaml not rewritten: %+v %v", saved, err)
	}
}

func TestForwardRemoteRefusesAMoveItCannotVerify(t *testing.T) {
	cfg, _ := movedFixture(t, fakeNet{"new": {machine: "M-new", host: &hostConfig{Role: roleHost, Generation: 2}}}) // no lineage
	stdout, stderr, code := runForward(t, cfg, "task", []string{"list"}, "")
	if code != hostMovedUnverifiedExit {
		t.Errorf("exit %d, want %d", code, hostMovedUnverifiedExit)
	}
	if strings.Contains(stdout, "[list]") {
		t.Errorf("the command must not run anywhere when the move is unverified: %q", stdout)
	}
	if !strings.Contains(stderr, "could not be verified") || !strings.Contains(stderr, "nothing was changed") {
		t.Errorf("unclear failure: %q", stderr)
	}
	saved, _ := loadRemoteConfig()
	if saved.Host != "old" {
		t.Errorf("remote.yaml must not change: %+v", saved)
	}
}

func TestForwardRemoteWontReplayAConsumedPipe(t *testing.T) {
	cfg, _ := movedFixture(t, genuineNew)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteString("piped input")
	_ = w.Close()
	dir := t.TempDir()
	out, _ := os.Create(filepath.Join(dir, "out"))
	errf, _ := os.Create(filepath.Join(dir, "err"))
	code := forwardRemote(cfg, "task", []string{"stdin"}, false, r, out, errf)
	eb, _ := os.ReadFile(errf.Name())
	if code != hostMovedExit || !strings.Contains(string(eb), "cannot be replayed") {
		t.Errorf("exit %d, stderr %q", code, eb)
	}
	if saved, _ := loadRemoteConfig(); saved.Host != "new" {
		t.Errorf("the client should still have been re-pointed: %+v", saved)
	}
}

func TestForwardRemoteEnvOverrideIsNotRewritten(t *testing.T) {
	cfg, _ := movedFixture(t, genuineNew)
	t.Setenv(envRemoteHost, "old")
	cfg.Host = "old"
	_, stderr, _ := runForward(t, cfg, "task", []string{"list"}, "")
	if !strings.Contains(stderr, "was not rewritten") {
		t.Errorf("stderr %q", stderr)
	}
	t.Setenv(envRemoteHost, "")
	if saved, _ := loadRemoteConfig(); saved.Host != "old" {
		t.Errorf("an env-pinned host must not rewrite the file: %+v", saved)
	}
}
