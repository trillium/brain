package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Two scratch hosts on one machine, two real Dolt servers, the real binary, and
// an ssh stand-in that runs each command under the other host's HOME. Skipped
// when dolt or rsync are missing, or with -short.

const e2eFakeSSH = `#!/bin/sh
host=
while [ $# -gt 0 ]; do
  case "$1" in
    -o) shift 2 ;;
    -O) shift 2; exit 0 ;;
    -t|-T) shift ;;
    -l) shift 2 ;;
    --) shift; break ;;
    -*) shift ;;
    *) host=$1; shift; break ;;
  esac
done
[ "$1" = "--" ] && shift
label=${host#*@}
home="$FAKE_SSH_HOMES/$label"
[ -d "$home" ] || { echo "ssh: Could not resolve hostname $label" >&2; exit 255; }
exec env -i HOME="$home" USER=proof LOGNAME=proof PATH="$E2E_TOOLS:/usr/bin:/bin:/usr/sbin:/sbin" BD_DISABLE_METRICS=1 TMPDIR="$TMPDIR" sh -c "$*"
`

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

type e2eHost struct {
	t    *testing.T
	home string
	env  []string
}

func (h *e2eHost) run(args ...string) (string, error) {
	h.t.Helper()
	cmd := exec.Command(filepath.Join(h.home, ".local", "bin", "bd"), args...) //nolint:gosec // the binary this test just built
	cmd.Env = h.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (h *e2eHost) sql(t *testing.T, port int, q string) string {
	t.Helper()
	cmd := exec.Command("dolt", "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--no-tls", "-u", "root", "-p", "", "sql", "-r", "csv", "-q", q)
	cmd.Env = h.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sql %q on :%d: %v: %s", q, port, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestHostPromoteEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end promotion needs real dolt servers")
	}
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not installed")
	}
	bd := buildBDUnderTest(t)
	base, err := os.MkdirTemp("", "hp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	homes := filepath.Join(base, "home")
	tools := filepath.Dir(doltPath)
	sshPath := filepath.Join(base, "fakessh")
	if err := os.WriteFile(sshPath, []byte(e2eFakeSSH), 0o755); err != nil { //nolint:gosec // test helper
		t.Fatal(err)
	}
	mkHost := func(label string) *e2eHost {
		home := filepath.Join(homes, label)
		if err := os.MkdirAll(filepath.Join(home, ".local", "bin"), 0o755); err != nil { //nolint:gosec // test dir
			t.Fatal(err)
		}
		data, err := os.ReadFile(bd) //nolint:gosec // the binary this test just built
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".local", "bin", "bd"), data, 0o755); err != nil { //nolint:gosec // test binary
			t.Fatal(err)
		}
		h := &e2eHost{t: t, home: home}
		h.env = []string{"HOME=" + home, "USER=proof", "LOGNAME=proof", "BD_DISABLE_METRICS=1", "TMPDIR=" + os.TempDir(),
			"PATH=" + filepath.Join(home, ".local", "bin") + ":" + tools + ":/usr/bin:/bin:/usr/sbin:/sbin",
			"BRAIN_REMOTE_SSH=" + sshPath, "FAKE_SSH_HOMES=" + homes, "E2E_TOOLS=" + tools}
		return h
	}
	a, b := mkHost("a"), mkHost("b")
	aPort, bPort := freePort(t), freePort(t)
	t.Cleanup(func() {
		for _, h := range []*e2eHost{a, b} {
			if pid := readPid(filepath.Join(h.home, ".config", "brain", "dolt-server.pid")); pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	// Host a: a small unified-shaped database, served by a process-mode server.
	for _, kv := range [][]string{{"user.name", "proof"}, {"user.email", "proof@localhost"}} {
		c := exec.Command(doltPath, "config", "--global", "--add", kv[0], kv[1]) //nolint:gosec // dolt found by LookPath
		c.Env = a.env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("dolt config: %v %s", err, out)
		}
	}
	dataA := filepath.Join(a.home, "data", ".beads")
	dbDir := filepath.Join(dataA, "brain_unified")
	if err := os.MkdirAll(dbDir, 0o755); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init"},
		{"sql", "-q", "create table issues (id varchar(64) primary key, title text); create table brain_store_prefixes (prefix varchar(255) primary key, store varchar(128) not null); " +
			"insert into brain_store_prefixes values ('s1','s1'),('s2','s2'); insert into issues values ('s1-aaa','one'),('s1-bbb','two'),('s2-ccc','three');"},
		{"add", "-A"}, {"commit", "-m", "fixture"},
	} {
		c := exec.Command(doltPath, args...) //nolint:gosec // dolt found by LookPath
		c.Dir, c.Env = dbDir, a.env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("dolt %v: %v %s", args, err, out)
		}
	}
	svcA := newServiceManager(a.home).newServerService("process", "")
	if err := newServiceManager(a.home).startProcess(context.Background(), svcA,
		serverSpec{DoltBin: doltPath, DataDir: dataA, Bind: "127.0.0.1", Port: aPort}, nil); err != nil {
		t.Fatalf("starting host a's server: %v", err)
	}
	reg := "stores:\n"
	for _, s := range []string{"s1", "s2"} {
		beads := filepath.Join(a.home, "data", s, ".beads")
		if err := os.MkdirAll(beads, 0o755); err != nil { //nolint:gosec // test dir
			t.Fatal(err)
		}
		meta := fmt.Sprintf(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":%d,"dolt_database":"brain_unified","project_id":"p-%s"}`, aPort, s)
		_ = os.WriteFile(filepath.Join(beads, "metadata.json"), []byte(meta), 0o644) //nolint:gosec // test fixture
		_ = os.MkdirAll(filepath.Join(a.home, "data", s, "entries"), 0o755)          //nolint:gosec // test dir
		_ = os.WriteFile(filepath.Join(a.home, "data", s, "entries", "note.md"), []byte("# "+s+"\n"), 0o644)
		wrapper := fmt.Sprintf("#!/bin/sh\nexport BEADS_DIR=\"$HOME/data/%s/.beads\"\nexport BD_NAME=%s\nexec bd \"$@\"\n", s, s)
		_ = os.WriteFile(filepath.Join(a.home, ".local", "bin", s), []byte(wrapper), 0o755) //nolint:gosec // executable wrapper
		reg += fmt.Sprintf("    %s:\n        path: %s\n", s, beads)
	}
	_ = os.MkdirAll(filepath.Join(a.home, ".config", "brain"), 0o755)                                                  //nolint:gosec // test dir
	if err := os.WriteFile(filepath.Join(a.home, ".config", "brain", "stores.yaml"), []byte(reg), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	// b starts as a client of a, as a machine joining a brain does.
	if err := os.MkdirAll(filepath.Join(b.home, ".config", "brain"), 0o755); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(b.home, ".config", "brain", "remote.yaml"), []byte("host: a\n"), 0o644) //nolint:gosec // test fixture

	// The plan alone changes nothing.
	out, err := a.run("host", "promote", "b", "--dry-run", "--port", strconv.Itoa(bPort), "--service", "process")
	if err != nil || !strings.Contains(out, "No blockers") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(a.home, ".config", "brain", "host.yaml")); err == nil {
		t.Fatal("a dry run must not write host.yaml")
	}

	// Run to the freeze and stop: this machine is frozen, nothing has switched.
	out, err = a.run("host", "promote", "b", "--yes", "--no-smoke", "--port", strconv.Itoa(bPort), "--service", "process", "--stop-source-server", "--stop-after", "freeze")
	if err != nil {
		t.Fatalf("promote --stop-after freeze: %v\n%s", err, out)
	}
	if st, _ := a.run("host", "status"); !strings.Contains(st, "FROZEN") || !strings.Contains(st, "IN PROGRESS") {
		t.Errorf("status after the freeze:\n%s", st)
	}
	if cfg, _ := loadHostConfig(a.home); cfg == nil || cfg.Role != roleFrozen {
		t.Fatalf("host.yaml after the freeze: %+v", cfg)
	}

	// Run it again: it resumes and finishes.
	out, err = a.run("host", "promote", "b", "--yes")
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Resuming promotion") || !strings.Contains(out, "complete: b is the brain host") {
		t.Errorf("resume output:\n%s", out)
	}

	// Both machines agree with the story.
	bc, err := loadHostConfig(b.home)
	if err != nil || bc == nil || bc.Role != roleHost || bc.Generation != 2 || bc.Server == nil || bc.Server.Port != bPort || len(bc.Lineage) != 1 {
		t.Fatalf("new host's host.yaml: %+v %v", bc, err)
	}
	ac, _ := loadHostConfig(a.home)
	if ac == nil || ac.Role != roleMoved || ac.MovedTo != "b" {
		t.Fatalf("old host's host.yaml: %+v", ac)
	}
	if rec, _ := loadForwardRecord(a.home); rec == nil || rec.To != "b" || rec.PromoteID != bc.PromoteID {
		t.Errorf("forwarding record: %+v", rec)
	}
	if _, err := os.Stat(filepath.Join(b.home, ".config", "brain", "remote.yaml")); err == nil {
		t.Error("the new host must stop being a client")
	}
	if rc, _ := loadRemoteConfig2(a.home); rc != "b" {
		t.Errorf("the old host should now be a client of b, remote.yaml host = %q", rc)
	}
	if w, _ := os.ReadFile(filepath.Join(a.home, ".local", "bin", "s1")); !strings.Contains(string(w), remoteWrapperMarker) {
		t.Errorf("the old host's wrapper should forward now:\n%s", w)
	}
	nw, _ := os.ReadFile(filepath.Join(b.home, ".local", "bin", "s1"))
	if !strings.Contains(string(nw), "$HOME/data/s1/.beads") || !strings.Contains(string(nw), hostWrapperMarker) || !strings.Contains(string(nw), "exec bd") {
		t.Errorf("the new host's wrapper:\n%s", nw)
	}
	meta, _ := os.ReadFile(filepath.Join(b.home, "data", "s1", ".beads", "metadata.json"))
	if !strings.Contains(string(meta), fmt.Sprintf(`"dolt_server_port": %d`, bPort)) {
		t.Errorf("the staged store's metadata must follow the new port:\n%s", meta)
	}
	if note, _ := os.ReadFile(filepath.Join(b.home, "data", "s2", "entries", "note.md")); string(note) != "# s2\n" {
		t.Errorf("the markdown mirror did not arrive: %q", note)
	}
	if got := b.sql(t, bPort, "use brain_unified; select count(*) from issues"); !strings.HasSuffix(got, "3") {
		t.Errorf("the new host's database: %q", got)
	}
	ha := b.sql(t, bPort, "use brain_unified; select dolt_hashof_db()")
	if want := ac.Server; want == nil || portListening(want.Port) {
		t.Errorf("the old server should be stopped: %+v", want)
	}
	// The old copy is untouched: hash it offline and compare.
	oc := exec.Command(doltPath, "sql", "-r", "csv", "-q", "select dolt_hashof_db()") //nolint:gosec // dolt found by LookPath
	oc.Dir, oc.Env = dbDir, a.env
	oo, err := oc.CombinedOutput()
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(oo)), lastLine(ha)) {
		t.Errorf("the old copy changed: %v %s vs %s", err, oo, ha)
	}
	// Running the promotion again is a no-op, not an error.
	if out, err := a.run("host", "promote", "b", "--yes"); err != nil || !strings.Contains(out, "Already done") {
		t.Errorf("repeating a finished promotion: %v\n%s", err, out)
	}
	// Promoting somewhere else from a machine that is no longer the host is refused.
	if out, err := a.run("host", "promote", "c", "--dry-run"); err == nil || !strings.Contains(out+err.Error(), "no longer the brain host") {
		t.Errorf("promote from a former host: %v\n%s", err, out)
	}

	// Rollback: nothing was written to b, so it is clean and complete.
	out, err = a.run("host", "rollback")
	if err != nil {
		t.Fatalf("rollback: %v\n%s", err, out)
	}
	if !portListening(aPort) {
		t.Error("rollback must start the old server again")
	}
	if portListening(bPort) {
		t.Error("rollback must stop the new server")
	}
	if w, _ := os.ReadFile(filepath.Join(a.home, ".local", "bin", "s1")); !strings.Contains(string(w), "BEADS_DIR") || strings.Contains(string(w), remoteWrapperMarker) {
		t.Errorf("the old wrapper should be back:\n%s", w)
	}
	if _, err := os.Stat(filepath.Join(a.home, ".config", "brain", "remote.yaml")); err == nil {
		t.Error("rollback must remove the client config it added")
	}
	if cfg, _ := loadHostConfig(a.home); cfg == nil || cfg.Role != roleHost || cfg.Generation != 3 || len(cfg.Lineage) != 2 {
		t.Errorf("after rollback, the old host is the host again one generation on: %+v", cfg)
	}
	if rc, _ := loadRemoteConfig2(b.home); rc != "a" {
		t.Errorf("b should be a client of a again, remote.yaml host = %q", rc)
	}
	if rec, _ := loadForwardRecord(b.home); rec == nil || rec.To != "a" {
		t.Errorf("b should forward followed clients back to a: %+v", rec)
	}
	_ = time.Second
}

// loadRemoteConfig2 reads host from a home's remote.yaml without touching $HOME.
func loadRemoteConfig2(home string) (string, error) {
	data, err := os.ReadFile(filepath.Join(home, remoteConfigPath)) //nolint:gosec // test file
	if err != nil {
		return "", err
	}
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, "host:") {
			return strings.TrimSpace(strings.TrimPrefix(l, "host:")), nil
		}
	}
	return "", nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
