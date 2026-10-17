package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testLocalVersion is the combined token every fake host reports; testDoctor
// gives the doctor the same token so the version check passes by default.
const testLocalVersion = "1.1.0-rc.1+brain.0.5.1"

// fakeDoctorHost is an injected stand-in for the brain host: no ssh, no
// network, no store. Each method's behaviour is a field the test sets, and
// every Run is recorded so a test can assert what was asked.
type fakeDoctorHost struct {
	reach  func(ctx context.Context) (time.Duration, error)
	run    func(ctx context.Context, name string, args ...string) ([]byte, error)
	stores func(ctx context.Context) (names, missing []string, err error)

	mu    sync.Mutex
	calls []string
}

func (f *fakeDoctorHost) Reachable(ctx context.Context) (time.Duration, error) {
	if f.reach == nil {
		return time.Millisecond, nil
	}
	return f.reach(ctx)
}

func (f *fakeDoctorHost) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	f.mu.Unlock()
	if f.run == nil {
		return nil, errors.New("no host command configured")
	}
	return f.run(ctx, name, args...)
}

func (f *fakeDoctorHost) Stores(ctx context.Context) ([]string, []string, error) {
	if f.stores == nil {
		return nil, nil, nil
	}
	return f.stores(ctx)
}

func (f *fakeDoctorHost) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// testDoctor builds a doctor whose every local fact points at a sandbox: a
// temp HOME that is also the "account" home, a config path inside it, a temp
// wrapper directory, and an injected fake host. Nothing here can read the real
// ~/.config or reach a real machine.
func testDoctor(t *testing.T, host remoteDoctorHost) *remoteDoctor {
	t.Helper()
	t.Setenv(envRemoteHost, "")
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	cfg := &remoteConfig{Host: "me@host"}
	return &remoteDoctor{
		host:         host,
		cfg:          cfg,
		home:         home,
		accountHome:  home,
		username:     "me",
		configPath:   filepath.Join(home, remoteConfigPath),
		binDir:       bin,
		localVersion: testLocalVersion,
		jobs:         4,
		timeout:      5 * time.Second,
	}
}

func checkNamed(checks []remoteDoctorCheck, name string) remoteDoctorCheck {
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	return remoteDoctorCheck{Name: name, Status: "missing"}
}

func TestRemoteDoctorConfigCatchesWrongHome(t *testing.T) {
	d := testDoctor(t, &fakeDoctorHost{})
	d.home = t.TempDir() // a home that is not this account's home
	d.accountHome = t.TempDir()

	c := d.checkConfig()
	if c.Status != doctorStatusFail {
		t.Fatalf("status = %s, want fail (%+v)", c.Status, c)
	}
	if !strings.Contains(c.Detail, "account's home") {
		t.Errorf("detail should name the account's home: %q", c.Detail)
	}
	if !strings.Contains(c.Fix, "HOME=") {
		t.Errorf("fix should say how to repair HOME: %q", c.Fix)
	}
	if code := remoteDoctorExitCode([]remoteDoctorCheck{c}, false); code != remoteDoctorExitConfig {
		t.Errorf("exit code = %d, want %d", code, remoteDoctorExitConfig)
	}
}

func TestRemoteDoctorConfigCatchesConfigOutsideHome(t *testing.T) {
	d := testDoctor(t, &fakeDoctorHost{})
	d.configPath = filepath.Join(t.TempDir(), "remote.yaml") // another directory entirely

	c := d.checkConfig()
	if c.Status != doctorStatusFail || !strings.Contains(c.Detail, "outside HOME") {
		t.Fatalf("expected a fail naming the escape from HOME, got %+v", c)
	}
}

func TestRemoteDoctorConfigAcceptsOwnConfigFile(t *testing.T) {
	d := testDoctor(t, &fakeDoctorHost{})
	if err := os.MkdirAll(filepath.Dir(d.configPath), 0o755); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	if err := os.WriteFile(d.configPath, []byte("host: me@host\n"), 0o644); err != nil { //nolint:gosec // test config
		t.Fatal(err)
	}
	c := d.checkConfig()
	if c.Status != doctorStatusPass {
		t.Fatalf("status = %s, want pass (%+v)", c.Status, c)
	}
	if !strings.Contains(c.Detail, d.configPath) {
		t.Errorf("detail should name the config path: %q", c.Detail)
	}
}

func TestRemoteDoctorConfigWarnsOnEnvOverride(t *testing.T) {
	d := testDoctor(t, &fakeDoctorHost{})
	t.Setenv(envRemoteHost, "other@host")
	c := d.checkConfig()
	if c.Status != doctorStatusWarn || !strings.Contains(c.Detail, envRemoteHost) {
		t.Fatalf("expected a warn about $%s, got %+v", envRemoteHost, c)
	}
}

func TestRemoteDoctorNoHostIsAConfigFailure(t *testing.T) {
	d := testDoctor(t, &fakeDoctorHost{})
	d.cfg.Host = ""

	checks := d.run(t.Context())
	cfg := checkNamed(checks, "config")
	if cfg.Status != doctorStatusFail || !strings.Contains(cfg.Fix, "brain remote set") {
		t.Fatalf("config check = %+v", cfg)
	}
	if code := remoteDoctorExitCode(checks, false); code != remoteDoctorExitConfig {
		t.Errorf("exit code = %d, want %d", code, remoteDoctorExitConfig)
	}
}

func TestRemoteDoctorUnreachableHostSkipsDependentChecks(t *testing.T) {
	host := &fakeDoctorHost{reach: func(context.Context) (time.Duration, error) {
		return 3 * time.Millisecond, errors.New("brain: cannot reach the brain host me@host over ssh.\n  Nothing ran on the host")
	}}
	d := testDoctor(t, host)

	checks := d.run(t.Context())
	ssh := checkNamed(checks, "ssh")
	if ssh.Status != doctorStatusFail {
		t.Fatalf("ssh = %+v", ssh)
	}
	if !strings.Contains(ssh.Fix, "ssh -o BatchMode=yes me@host true") {
		t.Errorf("ssh fix should name the link test: %q", ssh.Fix)
	}
	for _, name := range []string{"version", "wrappers", "latency"} {
		if got := checkNamed(checks, name).Status; got != doctorStatusSkip {
			t.Errorf("%s = %s, want skip when the host is unreachable", name, got)
		}
	}
	if code := remoteDoctorExitCode(checks, false); code != remoteDoctorExitSSH {
		t.Errorf("exit code = %d, want %d", code, remoteDoctorExitSSH)
	}
	if len(host.recorded()) != 0 {
		t.Errorf("no command should run on an unreachable host: %v", host.recorded())
	}
}

func TestRemoteDoctorVersionMismatchAndMatch(t *testing.T) {
	mismatch := &fakeDoctorHost{run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "brain" && strings.Join(args, " ") == "version --combined" {
			return []byte("1.1.0-rc.1+brain.0.5.0\n"), nil
		}
		return nil, errors.New("unexpected call")
	}}
	d := testDoctor(t, mismatch)
	c := d.checkVersion(t.Context(), true)
	if c.Status != doctorStatusFail || !strings.Contains(c.Detail, "client 1.1.0-rc.1+brain.0.5.1, host 1.1.0-rc.1+brain.0.5.0") {
		t.Fatalf("mismatch check = %+v", c)
	}
	if !strings.Contains(c.Fix, "make build") {
		t.Errorf("fix should say how to reinstall: %q", c.Fix)
	}
	if code := remoteDoctorExitCode([]remoteDoctorCheck{c}, false); code != remoteDoctorExitVersion {
		t.Errorf("exit code = %d, want %d", code, remoteDoctorExitVersion)
	}

	match := &fakeDoctorHost{run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("1.1.0-rc.1+brain.0.5.1\n"), nil
	}}
	d = testDoctor(t, match)
	if c := d.checkVersion(t.Context(), true); c.Status != doctorStatusPass {
		t.Fatalf("match check = %+v", c)
	}
}

func TestRemoteDoctorVersionFallsBackToHumanLine(t *testing.T) {
	host := &fakeDoctorHost{run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if strings.Join(args, " ") == "version --combined" {
			return nil, errors.New("unknown flag: --combined")
		}
		return []byte("bd version 1.1.0-rc.1+brain.0.5.1 (dev)\n"), nil
	}}
	d := testDoctor(t, host)
	if c := d.checkVersion(t.Context(), true); c.Status != doctorStatusPass {
		t.Fatalf("fallback check = %+v", c)
	}
}

func TestParseHostVersion(t *testing.T) {
	cases := map[string]string{
		"1.1.0+brain.0.5.1\n":                    "1.1.0+brain.0.5.1",
		"bd version 1.1.0+brain.0.5.1 (dev)\n":   "1.1.0+brain.0.5.1",
		"\n\nbd version 1.1.0+brain.0.5.1 (dev)": "1.1.0+brain.0.5.1",
		"":                                       "",
	}
	for in, want := range cases {
		if got := parseHostVersion([]byte(in)); got != want {
			t.Errorf("parseHostVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClientWrapperStoreParsing(t *testing.T) {
	cases := map[string]string{
		"BRAIN_REMOTE_STORE='task' exec '/bin/brain' \"$@\"\n": "task",
		"BRAIN_REMOTE_STORE=task exec brain \"$@\"\n":          "task",
		`export BRAIN_REMOTE_STORE="inbox"` + "\n":             "inbox",
		"BRAIN_REMOTE_STORE=day; exec brain \"$@\"\n":          "day",
		"#!/bin/sh\nexec /bin/brain \"$@\"\n":                  "",
	}
	for in, want := range cases {
		got, ok := clientWrapperStore(in)
		if want == "" {
			if ok {
				t.Errorf("clientWrapperStore(%q) = %q, want none", in, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("clientWrapperStore(%q) = %q (%v), want %q", in, got, ok, want)
		}
	}
}

func TestStorePrefixFromInfoParsing(t *testing.T) {
	prefix, ok := storePrefixFromInfo([]byte("notice: migrated\n{\"config\":{\"issue_prefix\":\"task\",\"x\":\"y\"}}\n"))
	if !ok || prefix != "task" {
		t.Fatalf("prefix = %q (%v), want task", prefix, ok)
	}
	if _, ok := storePrefixFromInfo([]byte("{\"config\":{}}")); ok {
		t.Error("an empty prefix must not be reported as found")
	}
	if _, ok := storePrefixFromInfo([]byte("no json here")); ok {
		t.Error("output without JSON must not report a prefix")
	}
}

// hostWithPrefixes is a fake host offering the given stores, each reporting the
// prefix in the map through 'info --json'.
func hostWithPrefixes(stores map[string]string) *fakeDoctorHost {
	names := make([]string, 0, len(stores))
	for n := range stores {
		names = append(names, n)
	}
	return &fakeDoctorHost{
		stores: func(context.Context) ([]string, []string, error) { return names, nil, nil },
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "brain" && strings.Join(args, " ") == "version --combined" {
				return []byte(testLocalVersion + "\n"), nil
			}
			if strings.Join(args, " ") != "info --json" {
				return nil, errors.New("unexpected call: " + name + " " + strings.Join(args, " "))
			}
			prefix, ok := stores[name]
			if !ok {
				return nil, errors.New("no such store")
			}
			return []byte(`{"config":{"issue_prefix":"` + prefix + `"}}`), nil
		},
	}
}

func writeClientWrapper(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil { //nolint:gosec // test wrapper
		t.Fatal(err)
	}
}

func TestRemoteDoctorWrapperWithoutStoreEnvFails(t *testing.T) {
	d := testDoctor(t, hostWithPrefixes(map[string]string{"task": "task"}))
	writeClientWrapper(t, d.binDir, "task", "#!/bin/sh\nexec /bin/brain \"$@\"\n")

	c := d.checkWrappers(t.Context(), true)
	if c.Status != doctorStatusFail || !strings.Contains(c.Detail, envRemoteStore) {
		t.Fatalf("check = %+v", c)
	}
	if !strings.Contains(c.Fix, "brain remote install") {
		t.Errorf("fix should point at the installer: %q", c.Fix)
	}
	if code := remoteDoctorExitCode([]remoteDoctorCheck{c}, false); code != remoteDoctorExitWrappers {
		t.Errorf("exit code = %d, want %d", code, remoteDoctorExitWrappers)
	}
}

func TestRemoteDoctorWrapperPinnedToAnotherStoreFails(t *testing.T) {
	d := testDoctor(t, hostWithPrefixes(map[string]string{"task": "task", "inbox": "inbox"}))
	writeClientWrapper(t, d.binDir, "task", "#!/bin/sh\nBRAIN_REMOTE_STORE='inbox' exec /bin/brain \"$@\"\n")
	writeClientWrapper(t, d.binDir, "inbox", "#!/bin/sh\nBRAIN_REMOTE_STORE='inbox' exec /bin/brain \"$@\"\n")

	c := d.checkWrappers(t.Context(), true)
	if c.Status != doctorStatusFail || !strings.Contains(c.Detail, "would run store inbox") {
		t.Fatalf("check = %+v", c)
	}
}

func TestRemoteDoctorStoreResolvingToAnotherStorePrefixFails(t *testing.T) {
	d := testDoctor(t, hostWithPrefixes(map[string]string{"task": "inbox", "inbox": "inbox"}))
	for _, name := range []string{"task", "inbox"} {
		writeClientWrapper(t, d.binDir, name, "#!/bin/sh\nBRAIN_REMOTE_STORE='"+name+"' exec /bin/brain \"$@\"\n")
	}

	c := d.checkWrappers(t.Context(), true)
	if c.Status != doctorStatusFail || !strings.Contains(c.Detail, "points at another store's database") {
		t.Fatalf("check = %+v", c)
	}
	if !strings.Contains(c.Fix, "brain stores rewrite-wrappers") {
		t.Errorf("fix should point at the host's rewriter: %q", c.Fix)
	}
}

func TestRemoteDoctorStorePrefixMismatchWarns(t *testing.T) {
	d := testDoctor(t, hostWithPrefixes(map[string]string{"task": "todo"}))
	writeClientWrapper(t, d.binDir, "task", "#!/bin/sh\nBRAIN_REMOTE_STORE='task' exec /bin/brain \"$@\"\n")

	checks := d.run(t.Context())
	wrappers := checkNamed(checks, "wrappers")
	if wrappers.Status != doctorStatusWarn || !strings.Contains(wrappers.Detail, `reports prefix "todo"`) {
		t.Fatalf("check = %+v", wrappers)
	}
	if code := remoteDoctorExitCode(checks, false); code != 0 {
		t.Errorf("warnings alone must exit 0, got %d", code)
	}
	if code := remoteDoctorExitCode(checks, true); code != remoteDoctorExitWarnings {
		t.Errorf("--strict must map a warning to bit %d, got %d", remoteDoctorExitWarnings, code)
	}
}

func TestRemoteDoctorMissingAndStaleClientWrappersWarn(t *testing.T) {
	d := testDoctor(t, hostWithPrefixes(map[string]string{"task": "task"}))
	// A generated wrapper for a store the host no longer has.
	writeClientWrapper(t, d.binDir, "ghost", remoteWrapperBody("ghost", filepath.Join(d.binDir, "brain")))

	c := d.checkWrappers(t.Context(), true)
	if c.Status != doctorStatusWarn {
		t.Fatalf("check = %+v", c)
	}
	if !strings.Contains(c.Detail, "no client wrapper for store \"task\"") {
		t.Errorf("detail should report the missing wrapper: %q", c.Detail)
	}
	if !strings.Contains(c.Detail, "no longer has") || !strings.Contains(c.Fix, "brain remote install --prune") {
		t.Errorf("detail/fix should report the stale wrapper: %q / %q", c.Detail, c.Fix)
	}
}

func TestRemoteDoctorHostStoreWithoutWrapperFails(t *testing.T) {
	host := &fakeDoctorHost{stores: func(context.Context) ([]string, []string, error) {
		return []string{"task"}, []string{"ghost"}, nil
	}}
	d := testDoctor(t, host)
	writeClientWrapper(t, d.binDir, "task", "#!/bin/sh\nBRAIN_REMOTE_STORE='task' exec /bin/brain \"$@\"\n")

	c := d.checkWrappers(t.Context(), true)
	if c.Status != doctorStatusFail || !strings.Contains(c.Detail, `"ghost"`) {
		t.Fatalf("check = %+v", c)
	}
}

func TestRemoteDoctorUnreadableHostRegistryFails(t *testing.T) {
	host := &fakeDoctorHost{stores: func(context.Context) ([]string, []string, error) {
		return nil, nil, errors.New("brain stores list on me@host: exit status 1: boom")
	}}
	d := testDoctor(t, host)
	c := d.checkWrappers(t.Context(), true)
	if c.Status != doctorStatusFail || !strings.Contains(c.Fix, "brain stores list") {
		t.Fatalf("check = %+v", c)
	}
}

func TestRemoteDoctorLatencyStatuses(t *testing.T) {
	cases := []struct {
		name   string
		warm   time.Duration
		status string
	}{
		{"fast", 80 * time.Millisecond, doctorStatusPass},
		{"slow", 1500 * time.Millisecond, doctorStatusWarn},
		{"very slow", 6 * time.Second, doctorStatusFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := &fakeDoctorHost{reach: func(context.Context) (time.Duration, error) {
				return tc.warm, nil
			}}
			d := testDoctor(t, host)
			c := d.checkLatency(t.Context(), true)
			if c.Status != tc.status {
				t.Fatalf("latency = %s, want %s (%+v)", c.Status, tc.status, c)
			}
			if c.Millis != tc.warm.Milliseconds() {
				t.Errorf("latency ms = %d, want %d", c.Millis, tc.warm.Milliseconds())
			}
			want := 0
			if tc.status == doctorStatusFail {
				want = remoteDoctorExitLatency
			}
			if code := remoteDoctorExitCode([]remoteDoctorCheck{c}, false); code != want {
				t.Errorf("exit code = %d, want %d", code, want)
			}
		})
	}
}

func TestRemoteDoctorLatencyFailureIsSkippedWithoutHost(t *testing.T) {
	d := testDoctor(t, &fakeDoctorHost{})
	d.cfg.Host = ""
	if c := d.checkLatency(t.Context(), true); c.Status != doctorStatusSkip {
		t.Fatalf("latency = %+v, want skip", c)
	}
	if c := d.checkLatency(t.Context(), false); c.Status != doctorStatusSkip {
		t.Fatalf("latency with an unreachable host = %+v, want skip", c)
	}
}

func TestRemoteDoctorExitCodeBitmask(t *testing.T) {
	checks := []remoteDoctorCheck{
		{Name: "config", Status: doctorStatusPass},
		{Name: "ssh", Status: doctorStatusFail},
		{Name: "version", Status: doctorStatusFail},
		{Name: "wrappers", Status: doctorStatusWarn},
		{Name: "latency", Status: doctorStatusSkip},
	}
	if got := remoteDoctorExitCode(checks, false); got != remoteDoctorExitSSH|remoteDoctorExitVersion {
		t.Errorf("exit code = %d, want %d", got, remoteDoctorExitSSH|remoteDoctorExitVersion)
	}
	if got := remoteDoctorExitCode(checks, true); got != remoteDoctorExitSSH|remoteDoctorExitVersion|remoteDoctorExitWarnings {
		t.Errorf("strict exit code = %d, want %d", got, remoteDoctorExitSSH|remoteDoctorExitVersion|remoteDoctorExitWarnings)
	}
	all := []remoteDoctorCheck{
		{Name: "config", Status: doctorStatusFail},
		{Name: "ssh", Status: doctorStatusFail},
		{Name: "version", Status: doctorStatusFail},
		{Name: "wrappers", Status: doctorStatusFail},
		{Name: "latency", Status: doctorStatusFail},
	}
	if got, want := remoteDoctorExitCode(all, false), 31; got != want {
		t.Errorf("all-fail exit code = %d, want %d", got, want)
	}
}

func TestRemoteDoctorReportCountsAndJSON(t *testing.T) {
	d := testDoctor(t, &fakeDoctorHost{})
	checks := []remoteDoctorCheck{
		{Name: "config", Status: doctorStatusPass, Detail: "ok"},
		{Name: "ssh", Status: doctorStatusFail, Detail: "nope", Fix: "do the thing"},
		{Name: "wrappers", Status: doctorStatusWarn, Detail: "hmm"},
		{Name: "latency", Status: doctorStatusSkip},
	}
	code := remoteDoctorExitCode(checks, false)
	r := remoteDoctorReportFor(d, checks, code)
	if r.Passed != 1 || r.Failed != 1 || r.Warnings != 1 || r.Skipped != 1 || r.OK || r.ExitCode != remoteDoctorExitSSH {
		t.Fatalf("report = %+v", r)
	}

	var buf bytes.Buffer
	emitRemoteDoctor(&buf, false, r)
	text := buf.String()
	for _, want := range []string{"PASS  config", "FAIL  ssh", "fix: do the thing", "WARN  wrappers", "skip  latency", "(exit 2)"} {
		if !strings.Contains(text, want) {
			t.Errorf("text report missing %q:\n%s", want, text)
		}
	}

	buf.Reset()
	emitRemoteDoctor(&buf, true, r)
	var got remoteDoctorReport
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json report does not parse: %v\n%s", err, buf.String())
	}
	if got.ExitCode != code || got.Failed != 1 || len(got.Checks) != 4 || got.Host != "me@host" {
		t.Errorf("json report = %+v", got)
	}
}

func TestRemoteDoctorIsALocalBrainRemoteVerb(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"remote", "doctor"})
	if err != nil {
		t.Fatalf("brain remote doctor is not registered: %v", err)
	}
	if !isBrainRemoteVerb(cmd) {
		t.Error("brain remote doctor must be a brain remote verb so it runs on the client")
	}
}

// TestRemoteDoctorEndToEndWithFakeSSH runs the whole command through the real
// ssh code path against the fake ssh from brain_remote_test.go: a fake host
// with a fake brain and two fake store wrappers, a temp HOME, and client
// wrappers generated by the real installer. It must report a clean bill of
// health and exit 0 without touching any real machine.
func TestRemoteDoctorEndToEndWithFakeSSH(t *testing.T) {
	home, hostBin := remoteEnv(t)

	writeExec(t, filepath.Join(hostBin, "brain"), `#!/bin/sh
case "$1 $2" in
  "version --combined") echo `+remoteQuote(combinedVersion())+`;;
  "stores list") echo '[{"name":"inbox"},{"name":"task"}]';;
esac
exit 0
`)
	for _, name := range []string{"inbox", "task"} {
		writeExec(t, filepath.Join(hostBin, name),
			"#!/bin/sh\nif [ \"$1\" = info ]; then echo '{\"config\":{\"issue_prefix\":\""+name+"\"}}'; fi\n")
	}

	clientBin := filepath.Join(home, "clientbin")
	if err := os.MkdirAll(clientBin, 0o755); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	if _, err := installRemoteWrappers(clientBin, filepath.Join(clientBin, "brain"), []string{"inbox", "task"}, false, false); err != nil {
		t.Fatal(err)
	}

	cfg := &remoteConfig{Host: "me@host", BinDir: hostBin}
	d := &remoteDoctor{
		host:         sshDoctorHost{cfg: cfg},
		cfg:          cfg,
		home:         home,
		accountHome:  home,
		username:     "me",
		configPath:   filepath.Join(home, remoteConfigPath),
		binDir:       clientBin,
		localVersion: combinedVersion(),
		jobs:         2,
		timeout:      10 * time.Second,
	}

	checks := d.run(t.Context())
	if code := remoteDoctorExitCode(checks, false); code != 0 {
		t.Fatalf("expected a clean bill of health, got exit %d: %+v", code, checks)
	}
	for _, c := range checks {
		if c.Status != doctorStatusPass {
			t.Errorf("%s = %s: %s %s", c.Name, c.Status, c.Detail, c.Fix)
		}
	}
}
