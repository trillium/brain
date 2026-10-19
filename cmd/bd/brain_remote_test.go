package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeSSH writes a stand-in for ssh that behaves like the real one where it
// matters: it skips the options, takes the host, joins the remaining words
// with spaces and hands the result to a shell, so every quoting mistake in the
// client shows up exactly as it would against a real host. A host named
// "unreachable" fails the way ssh does (exit 255). Each call's argv is logged.
func fakeSSH(t *testing.T, dir string) (script, log string) {
	t.Helper()
	log = filepath.Join(dir, "ssh.log")
	script = filepath.Join(dir, "fakessh")
	body := `#!/bin/sh
printf '%s\n' "$*" >> ` + remoteQuote(log) + `
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
if [ "$host" = unreachable ]; then
  echo "ssh: connect to host unreachable port 22: No route to host" >&2
  exit 255
fi
exec sh -c "$*"
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // test helper
		t.Fatal(err)
	}
	return script, log
}

// remoteEnv points HOME and ssh at a sandbox and returns the host's bin dir.
func remoteEnv(t *testing.T) (home, hostBin string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	script, _ := fakeSSH(t, t.TempDir())
	t.Setenv(envRemoteSSH, script)
	t.Setenv(envRemoteOff, "")
	t.Setenv(envRemoteHost, "")
	t.Setenv(envRemoteStore, "")
	hostBin = filepath.Join(t.TempDir(), "hostbin")
	if err := os.MkdirAll(hostBin, 0o755); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	return home, hostBin
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // test helper
		t.Fatal(err)
	}
}

func TestRemoteQuoteRoundTrips(t *testing.T) {
	cases := []string{
		"", "plain", "two words", "it's a title", "'", "''", `a"b`, `back\slash`, `\`,
		"$HOME", "`id`", "$(id)", "*", "a;b|c&d", "line1\nline2", "tab\there", "--flag", "-n",
		"ünïcode ✓", "trailing space ", " leading", "a'b'c'd",
	}
	for _, in := range cases {
		out, err := exec.Command("sh", "-c", "printf %s "+remoteQuote(in)).Output() //nolint:gosec // test
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if string(out) != in {
			t.Errorf("remoteQuote(%q) round-trips to %q", in, out)
		}
	}
}

func TestRemoteConfigValidate(t *testing.T) {
	ok := []remoteConfig{
		{Host: "me@macbook.hippo-tilapia.ts.net"},
		{Host: "100.74.138.74"},
		{Host: "mini", BinDir: "~/bin", SSHOptions: []string{"IdentityFile=~/.ssh/brain"}},
		{Host: "me@[fe80::1]"},
	}
	for _, c := range ok {
		if err := c.validate(); err != nil {
			t.Errorf("%+v: unexpected error %v", c, err)
		}
	}
	bad := []remoteConfig{
		{},
		{Host: "-oProxyCommand=evil"},
		{Host: "host name"},
		{Host: "host;rm"},
		{Host: "h", BinDir: `/x"; rm -rf ~; "`},
		{Host: "h", BinDir: "/a b"},
		{Host: "h", SSHOptions: []string{"ProxyCommand=a b"}},
		{Host: "h", SSHOptions: []string{"-oFoo"}},
	}
	for _, c := range bad {
		if err := c.validate(); err == nil {
			t.Errorf("%+v: expected a validation error", c)
		}
	}
	if got := (&remoteConfig{BinDir: "~/bin"}).binDir(); got != "$HOME/bin" {
		t.Errorf("binDir ~ expansion = %q", got)
	}
	if got := (&remoteConfig{}).binDir(); got != defaultRemoteBinDir {
		t.Errorf("default binDir = %q", got)
	}
}

func TestRemoteConfigSaveLoadAndEnvOverride(t *testing.T) {
	remoteEnv(t)
	cfg, err := loadRemoteConfig()
	if err != nil || cfg.Host != "" {
		t.Fatalf("no file should load as empty config, got %+v, %v", cfg, err)
	}
	want := &remoteConfig{Host: "me@host", BinDir: "$HOME/bin", ConnectTimeout: 9, SSHOptions: []string{"Port=2222"}}
	if err := saveRemoteConfig(want); err != nil {
		t.Fatal(err)
	}
	got, err := loadRemoteConfig()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip: got %+v (%v), want %+v", got, err, want)
	}
	t.Setenv(envRemoteHost, "other@host")
	got, _ = loadRemoteConfig()
	if got.Host != "other@host" || got.BinDir != "$HOME/bin" {
		t.Fatalf("env override: %+v", got)
	}
}

func TestSSHArgsMultiplexAndTTY(t *testing.T) {
	remoteEnv(t)
	cfg := &remoteConfig{Host: "me@host"}
	args := strings.Join(cfg.sshArgs(false, "task", []string{"list"}), " ")
	for _, want := range []string{"BatchMode=yes", "ControlMaster=auto", "ControlPersist=10m", "ControlPath=", "ConnectTimeout=5", " -T ", " me@host -- "} {
		if !strings.Contains(args, want) {
			t.Errorf("ssh args missing %q: %s", want, args)
		}
	}
	if strings.Contains(args, " -t ") {
		t.Errorf("non-tty call must not request a tty: %s", args)
	}
	if !strings.Contains(strings.Join(cfg.sshArgs(true, "task", nil), " "), " -t ") {
		t.Error("tty call must request a tty")
	}
}

func TestRemoteForwardingActive(t *testing.T) {
	remoteEnv(t)
	if _, _, active, err := remoteForwardingActive([]string{"list"}); active || err != nil {
		t.Fatalf("no config must not forward: active=%v err=%v", active, err)
	}
	if err := saveRemoteConfig(&remoteConfig{Host: "me@host"}); err != nil {
		t.Fatal(err)
	}
	cfg, name, active, err := remoteForwardingActive([]string{"search", "x"})
	if !active || err != nil || name != "brain" || cfg.Host != "me@host" {
		t.Fatalf("brain command: %+v %q %v %v", cfg, name, active, err)
	}
	// 'brain remote …' is always local…
	if _, _, active, _ := remoteForwardingActive([]string{"remote", "status"}); active {
		t.Error("'brain remote' must run locally")
	}
	// …but 'task remote' belongs to the host's task command.
	t.Setenv(envRemoteStore, "task")
	if _, name, active, _ := remoteForwardingActive([]string{"remote"}); !active || name != "task" {
		t.Errorf("store wrapper must forward everything: %q %v", name, active)
	}
	t.Setenv(envRemoteStore, "../evil")
	if _, _, _, err := remoteForwardingActive([]string{"x"}); err == nil {
		t.Error("a store name with a path separator must be rejected")
	}
	t.Setenv(envRemoteStore, "")
	t.Setenv(envRemoteOff, "off")
	if _, _, active, _ := remoteForwardingActive([]string{"list"}); active {
		t.Error("BRAIN_REMOTE=off must disable forwarding")
	}
	t.Setenv(envRemoteOff, "")
	t.Setenv(envRemoteHost, "-oProxyCommand=x")
	if _, _, _, err := remoteForwardingActive([]string{"list"}); err == nil {
		t.Error("an option-looking host must be rejected")
	}
}

// An explicit BEADS_DIR naming a local beads project must win over a client
// config in HOME: the store it names is opened here, not forwarded to the host.
func TestRemoteForwardingYieldsToLocalBeadsDir(t *testing.T) {
	remoteEnv(t)
	if err := saveRemoteConfig(&remoteConfig{Host: "me@host"}); err != nil {
		t.Fatal(err)
	}

	// Not set: the client config forwards as before.
	t.Setenv("BEADS_DIR", "")
	if _, _, active, _ := remoteForwardingActive([]string{"list"}); !active {
		t.Fatal("no BEADS_DIR must still forward")
	}

	// Set to a directory without project files: not a local store, still forwards.
	empty := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(empty, 0o755); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	t.Setenv("BEADS_DIR", empty)
	if _, _, active, _ := remoteForwardingActive([]string{"list"}); !active {
		t.Fatal("BEADS_DIR without project files must still forward")
	}

	// Set to a local beads project: runs here.
	local := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(local, 0o755); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "metadata.json"), []byte("{}"), 0o644); err != nil { //nolint:gosec // test file
		t.Fatal(err)
	}
	t.Setenv("BEADS_DIR", local)
	if _, _, active, err := remoteForwardingActive([]string{"list"}); active || err != nil {
		t.Fatalf("BEADS_DIR with a local project must not forward: active=%v err=%v", active, err)
	}
}

// hostTask installs a fake "task" command on the fake host. It prints each
// argument in brackets, echoes stdin, reports whether it was told not to
// forward onward, and exits with the status named by $TASK_EXIT or 7.
func hostTask(t *testing.T, hostBin string) {
	t.Helper()
	writeExec(t, filepath.Join(hostBin, "task"), `#!/bin/sh
for a in "$@"; do printf '[%s]\n' "$a"; done
printf 'off=%s\n' "$BRAIN_REMOTE"
case ":$PATH:" in *":`+hostBin+`:"*) pb=yes;; *) pb=no;; esac
printf 'path-has-bin=%s\n' "$pb"
if [ "${1:-}" = stdin ]; then printf 'stdin=<%s>\n' "$(cat)"; fi
echo oops >&2
exit 7
`)
}

func runForward(t *testing.T, cfg *remoteConfig, name string, args []string, stdin string) (stdout, stderr string, code int) {
	t.Helper()
	dir := t.TempDir()
	in, err := os.Create(filepath.Join(dir, "in"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.WriteString(stdin); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	out, _ := os.Create(filepath.Join(dir, "out"))
	errf, _ := os.Create(filepath.Join(dir, "err"))
	code = forwardRemote(cfg, name, args, false, in, out, errf)
	ob, _ := os.ReadFile(out.Name())
	eb, _ := os.ReadFile(errf.Name())
	return string(ob), string(eb), code
}

func TestForwardPreservesArgsStdinAndExitCode(t *testing.T) {
	_, hostBin := remoteEnv(t)
	hostTask(t, hostBin)
	cfg := &remoteConfig{Host: "me@host", BinDir: hostBin}

	args := []string{"create", "Don't panic: it's a \"quoted\" title", "", "$HOME", "*", "a;b", "two  spaces", "multi\nline", `back\slash`, "--priority=1"}
	stdout, stderr, code := runForward(t, cfg, "task", args, "")
	if code != 7 {
		t.Errorf("exit code = %d, want 7 (the host command's status)", code)
	}
	if !strings.Contains(stderr, "oops") {
		t.Errorf("stderr not relayed: %q", stderr)
	}
	var want strings.Builder
	for _, a := range args {
		want.WriteString("[" + a + "]\n")
	}
	if !strings.HasPrefix(stdout, want.String()) {
		t.Errorf("arguments were altered in transit.\n got: %q\nwant prefix: %q", stdout, want.String())
	}
	for _, line := range []string{"off=off", "path-has-bin=yes"} {
		if !strings.Contains(stdout, line) {
			t.Errorf("host environment missing %q in %q", line, stdout)
		}
	}

	stdout, _, _ = runForward(t, cfg, "task", []string{"stdin"}, "body line 1\nit's body line 2\n")
	if !strings.Contains(stdout, "stdin=<body line 1\nit's body line 2>") {
		t.Errorf("stdin not relayed: %q", stdout)
	}
}

func TestForwardUnreachableHostSaysSo(t *testing.T) {
	remoteEnv(t)
	cfg := &remoteConfig{Host: "unreachable"}
	stdout, stderr, code := runForward(t, cfg, "task", []string{"list"}, "")
	if code != sshConnectFailed {
		t.Errorf("exit code = %d, want %d", code, sshConnectFailed)
	}
	if stdout != "" {
		t.Errorf("unexpected stdout %q", stdout)
	}
	for _, want := range []string{"cannot reach the brain host unreachable", "BRAIN_REMOTE=off", "remote.yaml"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q: %q", want, stderr)
		}
	}
}

func TestWrapperInstallRules(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "brain")
	writeExec(t, self, "#!/bin/sh\n")

	foreign := filepath.Join(dir, "week")
	writeExec(t, foreign, "#!/bin/sh\necho mine\n")
	bd := filepath.Join(dir, "bd")
	writeExec(t, bd, "#!/bin/sh\necho real bd\n")

	names := []string{"bd", "brain", "day", "task", "week"}
	got, err := installRemoteWrappers(dir, self, names, false, false)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]string{}
	for _, a := range got {
		actions[a.Name] = a.Action
	}
	want := map[string]string{"bd": "skipped", "brain": "skipped", "day": "created", "task": "created", "week": "skipped"}
	if !reflect.DeepEqual(actions, want) {
		t.Fatalf("actions = %v, want %v", actions, want)
	}
	if b, _ := os.ReadFile(bd); !strings.Contains(string(b), "real bd") {
		t.Error("bd was overwritten")
	}
	if b, _ := os.ReadFile(foreign); !strings.Contains(string(b), "mine") {
		t.Error("a foreign file was overwritten without --force")
	}
	if !isRemoteWrapper(filepath.Join(dir, "task")) {
		t.Error("task wrapper lacks the marker")
	}

	// Re-running is a no-op; --force takes over the foreign file but still never bd.
	got, _ = installRemoteWrappers(dir, self, names, false, false)
	for _, a := range got {
		if a.Name == "task" && a.Action != "unchanged" {
			t.Errorf("second install of task = %s, want unchanged", a.Action)
		}
	}
	got, _ = installRemoteWrappers(dir, self, names, true, false)
	for _, a := range got {
		if a.Name == "week" && a.Action != "updated" {
			t.Errorf("--force on week = %s, want updated", a.Action)
		}
		if a.Name == "bd" && a.Action != "skipped" {
			t.Errorf("--force must never write bd, got %s", a.Action)
		}
	}
	if b, _ := os.ReadFile(bd); !strings.Contains(string(b), "real bd") {
		t.Error("bd was overwritten by --force")
	}

	// Dry run writes nothing.
	dry := t.TempDir()
	if _, err := installRemoteWrappers(dry, self, []string{"task"}, false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dry, "task")); err == nil {
		t.Error("dry run wrote a wrapper")
	}

	// Prune removes only generated wrappers that are no longer wanted.
	pruned := pruneRemoteWrappers(dir, []string{"day", "task"}, false)
	var removed []string
	for _, p := range pruned {
		removed = append(removed, p.Name)
	}
	if !reflect.DeepEqual(removed, []string{"week"}) { // week was --force-replaced, so it is generated now
		t.Errorf("pruned %v, want [week]", removed)
	}
	if _, err := os.Stat(bd); err != nil {
		t.Error("prune removed bd")
	}
	if _, err := os.Stat(self); err != nil {
		t.Error("prune removed the brain binary")
	}
}

func TestInstalledWrapperForwardsAsItsStore(t *testing.T) {
	dir := t.TempDir()
	fakeBrain := filepath.Join(dir, "brain")
	writeExec(t, fakeBrain, "#!/bin/sh\nprintf 'store=%s\\n' \"$"+envRemoteStore+"\"\nfor a in \"$@\"; do printf '[%s]\\n' \"$a\"; done\n")
	if _, err := installRemoteWrappers(dir, fakeBrain, []string{"inbox"}, false, false); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(filepath.Join(dir, "inbox"), "list", "it's here", "").Output() //nolint:gosec // test
	if err != nil {
		t.Fatal(err)
	}
	want := "store=inbox\n[list]\n[it's here]\n[]\n"
	if string(out) != want {
		t.Errorf("wrapper output %q, want %q", out, want)
	}
}

func TestParseStoreRegistryJSON(t *testing.T) {
	in := []byte("notice: registry migrated\n[\n {\"name\":\"task\",\"path\":\"/x\"},\n {\"name\":\"day\"},\n {\"name\":\"../bad\"},\n {\"name\":\"bd\"}\n]\n")
	got, err := parseStoreRegistryJSON(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"bd", "day", "task"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if _, err := parseStoreRegistryJSON([]byte("no stores here")); err == nil {
		t.Error("expected an error when the host prints no JSON")
	}
}

func TestHostStoreNamesReadsHostRegistryAndWrappers(t *testing.T) {
	_, hostBin := remoteEnv(t)
	writeExec(t, filepath.Join(hostBin, "brain"), `#!/bin/sh
if [ "$1 $2 $3" = "stores list --json" ]; then
  echo '[{"name":"day"},{"name":"inbox"},{"name":"task"},{"name":"ghost"}]'
fi
`)
	for _, n := range []string{"day", "inbox", "task"} {
		writeExec(t, filepath.Join(hostBin, n), "#!/bin/sh\n")
	}
	cfg := &remoteConfig{Host: "me@host", BinDir: hostBin}
	names, missing, err := hostStoreNames(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"day", "inbox", "task"}; !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if want := []string{"ghost"}; !reflect.DeepEqual(missing, want) {
		t.Errorf("missing = %v, want %v", missing, want)
	}
}

func TestShadowNotes(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	writeExec(t, filepath.Join(a, "task"), "#!/bin/sh\n")
	writeExec(t, filepath.Join(b, "task"), "#!/bin/sh\n")
	writeExec(t, filepath.Join(b, "day"), "#!/bin/sh\n")
	path := a + string(os.PathListSeparator) + b

	notes := shadowNotes(a, []string{"task", "day", "nothing"}, path)
	if len(notes) != 2 || !strings.Contains(notes[0], "shadows "+filepath.Join(b, "task")) || !strings.Contains(notes[1], "shadows "+filepath.Join(b, "day")) {
		t.Errorf("first on PATH: %v", notes)
	}
	notes = shadowNotes(b, []string{"task", "day"}, path)
	if len(notes) != 1 || !strings.Contains(notes[0], "is shadowed by "+filepath.Join(a, "task")) {
		t.Errorf("later on PATH: %v", notes)
	}
}

func TestBrainRemoteVerbsSkipStoreAndDoltRemoteDoesNot(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"remote", "set"})
	if err != nil || !isBrainRemoteVerb(cmd) {
		t.Fatalf("brain remote set must be a brain remote verb (%v)", err)
	}
	dolt, _, err := rootCmd.Find([]string{"dolt", "remote", "add"})
	if err == nil && isBrainRemoteVerb(dolt) {
		t.Error("'dolt remote add' must keep opening its store")
	}
}

func TestRemoteInstallCommandEndToEnd(t *testing.T) {
	home, hostBin := remoteEnv(t)
	writeExec(t, filepath.Join(hostBin, "brain"), "#!/bin/sh\n[ \"$2\" = rewrite-wrappers ] && { echo '{\"total\":2,\"failed\":0,\"stores\":[{\"name\":\"task\",\"status\":\"ok\"},{\"name\":\"inbox\",\"status\":\"ok\"}]}'; exit 0; }\necho '[{\"name\":\"task\"},{\"name\":\"inbox\"}]'\n")
	writeExec(t, filepath.Join(hostBin, "task"), "#!/bin/sh\n")
	writeExec(t, filepath.Join(hostBin, "inbox"), "#!/bin/sh\n")
	if err := saveRemoteConfig(&remoteConfig{Host: "me@host", BinDir: hostBin}); err != nil {
		t.Fatal(err)
	}
	clientBin := filepath.Join(home, "bin")
	remoteInstallDir, remoteInstallBin = clientBin, filepath.Join(clientBin, "brain")
	t.Cleanup(func() { remoteInstallDir, remoteInstallBin = "", "" })
	brainRemoteInstallCmd.Run(brainRemoteInstallCmd, nil)
	for _, n := range []string{"task", "inbox"} {
		if !isRemoteWrapper(filepath.Join(clientBin, n)) {
			t.Errorf("%s wrapper not installed", n)
		}
	}
}

func TestHostCommandExiting255IsNotReportedAsUnreachable(t *testing.T) {
	_, hostBin := remoteEnv(t)
	writeExec(t, filepath.Join(hostBin, "task"), "#!/bin/sh\necho 'boom: the command failed' >&2\nexit 255\n")
	cfg := &remoteConfig{Host: "me@host", BinDir: hostBin}
	_, stderr, code := runForward(t, cfg, "task", []string{"x"}, "")
	if code != 255 {
		t.Errorf("exit code = %d, want 255", code)
	}
	if strings.Contains(stderr, "cannot reach") || !strings.Contains(stderr, "boom") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestLooksLikeSSHFailure(t *testing.T) {
	for _, s := range []string{
		"ssh: connect to host 100.64.0.99 port 22: Operation timed out",
		"ssh: Could not resolve hostname nope.invalid: nodename nor servname provided",
		"me@host: Permission denied (publickey).",
		"Host key verification failed.",
		"kex_exchange_identification: read: Connection reset by peer",
	} {
		if !looksLikeSSHFailure([]byte(s)) {
			t.Errorf("%q should read as an ssh failure", s)
		}
	}
	for _, s := range []string{"", "Error: no issue found matching \"x\"", "boom", "Write failed: Broken pipe"} {
		if looksLikeSSHFailure([]byte(s)) {
			t.Errorf("%q should not read as an ssh failure", s)
		}
	}
}
