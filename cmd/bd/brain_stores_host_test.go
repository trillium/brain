package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
)

// A secret never leaves the record: the report names the credential file, says
// whether it is there, and prints no password even when one is present.
func TestStoreHostReportNeverPrintsPassword(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	noStoreHostProbe(t)
	t.Setenv(configfile.StoreHostKeyUser, "")
	t.Setenv(configfile.StoreHostKeyCredentialsFile, "")
	t.Setenv("BEADS_DOLT_PASSWORD", "")

	creds := filepath.Join(home, ".config", "beads", "credentials")
	if err := os.MkdirAll(filepath.Dir(creds), 0o700); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	const secret = "hunter" + "2-that-must-not-print"
	if err := os.WriteFile(creds, []byte("[127.0.0.1:3307]\npassword="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	recordPath := filepath.Join(home, ".config", "brain", "store-host.env")
	if err := os.MkdirAll(filepath.Dir(recordPath), 0o755); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	body := "BEADS_DOLT_SERVER_HOST=127.0.0.1\nBEADS_DOLT_SERVER_PORT=3307\n" +
		configfile.StoreHostKeyCredentialsFile + "=" + creds + "\n"
	if err := os.WriteFile(recordPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(configfile.StoreHostFileEnvVar, recordPath)

	report := readStoreHostReport()
	if report.Creds != creds || !report.CredsOK {
		t.Errorf("credential reference not reported: %+v", report)
	}
	if report.User != configfile.DefaultDoltServerUser || report.UserFrom != "default" {
		t.Errorf("user fallback = %q (%s)", report.User, report.UserFrom)
	}

	out := captureStdout(t, func() error { printStoreHostReport(report); return nil })
	if strings.Contains(out, secret) {
		t.Errorf("the password leaked into the report:\n%s", out)
	}
	if !strings.Contains(out, creds) {
		t.Errorf("the report should name the credentials file:\n%s", out)
	}
}

// A record that carries a secret is reported by key name (never by value) and
// exits non-zero: the record must not become a credential store.
func TestStoreHostReportFlagsSecretsInRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	noStoreHostProbe(t)
	recordPath := filepath.Join(home, ".config", "brain", "store-host.env")
	if err := os.MkdirAll(filepath.Dir(recordPath), 0o755); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	body := "BEADS_DOLT_SERVER_HOST=127.0.0.1\nBEADS_DOLT_SERVER_PORT=3307\nBEADS_DOLT_PASSWORD=" + "oops" + "\n"
	if err := os.WriteFile(recordPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(configfile.StoreHostFileEnvVar, recordPath)

	report := readStoreHostReport()
	if len(report.Secrets) != 1 || report.Secrets[0] != "BEADS_DOLT_PASSWORD" {
		t.Fatalf("secrets = %v, want [BEADS_DOLT_PASSWORD]", report.Secrets)
	}
	out := captureStdout(t, func() error { printStoreHostReport(report); return nil })
	if strings.Contains(out, "oops") {
		t.Errorf("the secret value leaked:\n%s", out)
	}
	if !strings.Contains(out, "BEADS_DOLT_PASSWORD") {
		t.Errorf("the offending key should be named:\n%s", out)
	}
}

// writeStoreHostRecord is the record's only writer: it preserves lines it was
// not asked to change, is 0600, and refuses a secret-shaped value.
func TestWriteStoreHostRecordPreservesAndProtects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path := filepath.Join(home, ".config", "brain", "store-host.env")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	prior := "# a hand-written note\nBEADS_DOLT_SERVER_HOST=127.0.0.1\nBEADS_DOLT_SERVER_PORT=3307\nBEADS_DOLT_SERVER_USER=root\nCUSTOM_SETTING=keep\n"
	if err := os.WriteFile(path, []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(configfile.StoreHostFileEnvVar, path)

	written, err := writeStoreHostRecord("mini1.tailnet", 3307, "", "")
	if err != nil {
		t.Fatalf("writeStoreHostRecord: %v", err)
	}
	if written != path {
		t.Errorf("wrote %s, want %s", written, path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{"# a hand-written note", "CUSTOM_SETTING=keep", "BEADS_DOLT_SERVER_HOST=mini1.tailnet", "BEADS_DOLT_SERVER_PORT=3307"} {
		if !strings.Contains(text, want) {
			t.Errorf("record lost %q:\n%s", want, text)
		}
	}
	// A key the caller did not pass is not the caller's to drop.
	if !strings.Contains(text, "BEADS_DOLT_SERVER_USER=root") {
		t.Errorf("an unrelated existing key was dropped:\n%s", text)
	}
	if n := strings.Count(text, "BEADS_DOLT_SERVER_HOST="); n != 1 {
		t.Errorf("host key written %d times:\n%s", n, text)
	}

	// A key the caller does pass is replaced, not duplicated.
	creds := filepath.Join(home, ".config", "beads", "credentials")
	if _, err := writeStoreHostRecord("mini2.tailnet", 3310, "brain", creds); err != nil {
		t.Fatalf("writeStoreHostRecord with credentials: %v", err)
	}
	body, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text = string(body)
	for _, want := range []string{
		"BEADS_DOLT_SERVER_HOST=mini2.tailnet",
		"BEADS_DOLT_SERVER_PORT=3310",
		"BEADS_DOLT_SERVER_USER=brain",
		configfile.StoreHostKeyCredentialsFile + "=" + creds,
		"# a hand-written note",
		"CUSTOM_SETTING=keep",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("record lost %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "BEADS_DOLT_SERVER_USER=root") || strings.Count(text, "BEADS_DOLT_SERVER_HOST=") != 1 {
		t.Errorf("passed keys were not replaced exactly once:\n%s", text)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("record permissions = %04o, want 0600", perm)
	}

	if _, err := writeStoreHostRecord("mysql://"+"user:"+"pw"+"@host", 3307, "", ""); err == nil {
		t.Error("a secret-shaped host must be refused")
	}
	if _, err := writeStoreHostRecord("host", 3307, "user with spaces", ""); err == nil {
		t.Error("a secret-shaped user must be refused")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != text {
		t.Errorf("a refused write modified the record:\n%s", after)
	}
}

// The doctor's one-line connection summary names the missing record and the
// exact command that writes it.
func TestStoreHostRecordSummaryNamesTheFix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	noStoreHostProbe(t)
	t.Setenv(configfile.StoreHostKeyHost, "")
	t.Setenv(configfile.StoreHostKeyPort, "")
	t.Setenv(configfile.StoreHostFileEnvVar, filepath.Join(home, ".config", "brain", "store-host.env"))

	summary := storeHostRecordSummary()
	if !strings.Contains(summary, "MISSING") || !strings.Contains(summary, "brain stores host set") {
		t.Errorf("summary = %q", summary)
	}

	path := writeScratchStoreHostRecord(t, home)
	t.Setenv(configfile.StoreHostFileEnvVar, path)
	// 127.0.0.1:3307 is unlikely to answer in a unit test environment; the
	// summary must still name the record and the server it resolved.
	summary = storeHostRecordSummary()
	if !strings.Contains(summary, path) || !strings.Contains(summary, "127.0.0.1:3307") {
		t.Errorf("summary = %q", summary)
	}
}

// A record that names no server is not silently treated as 127.0.0.1.
func TestEnsureStoreHostRecordFromEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	noStoreHostProbe(t)
	recordPath := filepath.Join(home, ".config", "brain", "store-host.env")
	t.Setenv(configfile.StoreHostFileEnvVar, recordPath)
	t.Setenv(configfile.StoreHostKeyHost, "")
	t.Setenv(configfile.StoreHostKeyPort, "")

	// Nothing in the environment: nothing is written or invented.
	got, err := ensureStoreHostRecordFromEnv()
	if err != nil || got != "" {
		t.Fatalf("ensureStoreHostRecordFromEnv = %q, %v; want no-op", got, err)
	}
	if _, err := os.Stat(recordPath); err == nil {
		t.Fatal("a record was written with no host in the environment")
	}

	t.Setenv(configfile.StoreHostKeyHost, "mini1.tailnet")
	t.Setenv(configfile.StoreHostKeyPort, "3307")
	got, err = ensureStoreHostRecordFromEnv()
	if err != nil || got != recordPath {
		t.Fatalf("ensureStoreHostRecordFromEnv = %q, %v; want %s", got, err, recordPath)
	}
	resolved, err := configfile.Resolve()
	if err != nil {
		t.Fatalf("Resolve after writing the record: %v", err)
	}
	// The process env still supplies the values (it wins per key), and the
	// record now carries them for every wrapper that no longer has a shell.
	if resolved.Host != "mini1.tailnet" || resolved.Port != 3307 {
		t.Errorf("resolved = %+v", resolved)
	}
	if fileHost, filePort, ok := configfile.StoreHostFromFile(); !ok || fileHost != "mini1.tailnet" || filePort != 3307 {
		t.Errorf("record = %s:%d (ok=%v), want mini1.tailnet:3307", fileHost, filePort, ok)
	}
}

// noStoreHostProbe keeps the doctor's connection check offline in unit tests.
func noStoreHostProbe(t *testing.T) {
	t.Helper()
	prior := storeHostProbe
	storeHostProbe = 0
	t.Cleanup(func() { storeHostProbe = prior })
}

// The store-host record is reachable as both 'brain stores host' and its setter.
func TestBrainStoresHostCmdRegistered(t *testing.T) {
	found, _, err := rootCmd.Find([]string{"brain", "stores", "host"})
	if err != nil || found == nil || found.Name() != "host" {
		t.Fatalf("brain stores host not registered: %v %v", found, err)
	}
	set, _, err := rootCmd.Find([]string{"brain", "stores", "host", "set"})
	if err != nil || set == nil || set.Name() != "set" {
		t.Fatalf("brain stores host set not registered: %v %v", set, err)
	}
}

// 'brain doctor' is the same probe as 'brain stores doctor' and must run from a
// directory with no beads database, like its sibling.
func TestBrainDoctorCmdRegisteredWithoutStore(t *testing.T) {
	found, _, err := rootCmd.Find([]string{"brain", "doctor"})
	if err != nil || found == nil || found.Name() != "doctor" || found.Parent().Name() != "brain" {
		t.Fatalf("brain doctor not registered: %v %v", found, err)
	}
	if !storesCommandCanRunWithoutStore(found) {
		t.Error("brain doctor must run without a beads database")
	}
	if found.Flags().Lookup("fix") == nil {
		t.Error("brain doctor must expose --fix")
	}
}
