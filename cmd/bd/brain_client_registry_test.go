package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClientIdentityFrom(t *testing.T) {
	cases := []struct{ user, host, want string }{
		{"trillium", "mac-mini", "trillium@mac-mini"},
		{" trillium ", " mac-mini ", "trillium@mac-mini"},
		{"", "mac-mini", "unknown@mac-mini"},
		{"trillium", "", "trillium@unknown"},
		{"", "", "unknown@unknown"},
		{"bad\nname", "mac-mini", ""}, // a control character is refused, not stored
		{strings.Repeat("x", clientFieldMax+1), "h", ""},
	}
	for _, c := range cases {
		if got := clientIdentityFrom(c.user, c.host); got != c.want {
			t.Errorf("clientIdentityFrom(%q, %q) = %q, want %q", c.user, c.host, got, c.want)
		}
	}
}

func TestApplyClientHeartbeatThrottlesAndUpdates(t *testing.T) {
	reg := &clientRegistry{}
	base := time.Date(2026, 2, 14, 10, 0, 0, 0, time.UTC)
	rec := clientRecord{ID: "trillium@mac-mini", Host: "me@host.ts.net", Version: "1.0+brain.0.5.1"}

	if !applyClientHeartbeat(reg, rec, base) {
		t.Fatal("the first heartbeat must be written")
	}
	if len(reg.Clients) != 1 || reg.Clients[0].FirstSeen != "2026-02-14T10:00:00Z" || reg.Clients[0].LastSeen != reg.Clients[0].FirstSeen {
		t.Fatalf("first record: %+v", reg.Clients)
	}
	// A repeat a few seconds later is free: no rewrite.
	if applyClientHeartbeat(reg, rec, base.Add(5*time.Second)) {
		t.Error("a repeat inside the heartbeat interval must not rewrite the file")
	}
	if reg.Clients[0].LastSeen != "2026-02-14T10:00:00Z" {
		t.Errorf("throttled call moved last seen: %s", reg.Clients[0].LastSeen)
	}
	// Past the interval it is refreshed, keeping first seen.
	if !applyClientHeartbeat(reg, rec, base.Add(2*clientHeartbeatInterval)) {
		t.Fatal("a stale record must be refreshed")
	}
	if reg.Clients[0].LastSeen != "2026-02-14T10:02:00Z" || reg.Clients[0].FirstSeen != "2026-02-14T10:00:00Z" {
		t.Errorf("refresh: %+v", reg.Clients[0])
	}
	// A version or host change is recorded immediately, however recent.
	up := rec
	up.Version = "1.1+brain.0.5.2"
	if !applyClientHeartbeat(reg, up, base.Add(2*clientHeartbeatInterval+time.Second)) {
		t.Fatal("a version change must be recorded")
	}
	if reg.Clients[0].Version != up.Version {
		t.Errorf("version not updated: %+v", reg.Clients[0])
	}
	// A record with an unparseable stamp is refreshed rather than trusted.
	reg.Clients[0].LastSeen = "not a stamp"
	if !applyClientHeartbeat(reg, up, base.Add(2*clientHeartbeatInterval+2*time.Second)) {
		t.Error("a damaged last-seen stamp must be rewritten")
	}
}

func TestClientRegistryIsSortedAndBounded(t *testing.T) {
	reg := &clientRegistry{}
	now := time.Date(2026, 2, 14, 10, 0, 0, 0, time.UTC)
	for i := 0; i < clientRegistryMax+3; i++ {
		id := clientIdentityFrom("user", "m"+string(rune('a'+i%26))+string(rune('a'+i/26)))
		if !applyClientHeartbeat(reg, clientRecord{ID: id}, now.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("heartbeat %d must be written", i)
		}
	}
	if len(reg.Clients) != clientRegistryMax {
		t.Fatalf("registry must be capped at %d, got %d", clientRegistryMax, len(reg.Clients))
	}
	for i := 1; i < len(reg.Clients); i++ {
		if reg.Clients[i-1].ID > reg.Clients[i].ID {
			t.Fatalf("records must stay sorted by id: %q > %q", reg.Clients[i-1].ID, reg.Clients[i].ID)
		}
	}
}

func TestRecordClientHeartbeatFromForwardedCall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(envRemoteForward, "1")
	t.Setenv(envRemoteClient, "trillium@mac-mini")
	t.Setenv(envRemoteClientHost, "me@host.ts.net")
	t.Setenv(envRemoteClientVersion, "1.1.0+brain.0.5.1")
	t.Setenv(envHostGate, "")

	recordClientHeartbeat()
	reg, err := loadClientRegistry(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Clients) != 1 {
		t.Fatalf("one forwarded call must record one client: %+v", reg.Clients)
	}
	got := reg.Clients[0]
	if got.ID != "trillium@mac-mini" || got.Host != "me@host.ts.net" || got.Version != "1.1.0+brain.0.5.1" {
		t.Fatalf("recorded: %+v", got)
	}
	if _, err := time.Parse(hostTimeFormat, got.LastSeen); err != nil {
		t.Errorf("last_seen %q is not a stamp: %v", got.LastSeen, err)
	}
	if !strings.HasPrefix(readFileString(t, clientRegistryFile(home)), "# brain client heartbeats") {
		t.Error("the heartbeat file must carry its explanatory header")
	}

	// A second call within the interval leaves the file alone.
	first := got.LastSeen
	recordClientHeartbeat()
	if reg, _ = loadClientRegistry(home); reg.Clients[0].LastSeen != first {
		t.Errorf("a repeat within the interval rewrote the file: %s -> %s", first, reg.Clients[0].LastSeen)
	}

	// A version change is picked up right away.
	t.Setenv(envRemoteClientVersion, "1.1.0+brain.0.5.2")
	recordClientHeartbeat()
	if reg, _ = loadClientRegistry(home); reg.Clients[0].Version != "1.1.0+brain.0.5.2" {
		t.Errorf("version change not recorded: %+v", reg.Clients[0])
	}

	// A local (not forwarded) call records nothing.
	if err := os.Remove(clientRegistryFile(home)); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envRemoteForward, "")
	recordClientHeartbeat()
	if _, err := os.Stat(clientRegistryFile(home)); !os.IsNotExist(err) {
		t.Error("a local call must not write a heartbeat")
	}
}

func TestRecordClientHeartbeatIgnoresUnusableInput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(envHostGate, "")
	t.Setenv(envRemoteForward, "1")
	t.Setenv(envRemoteClientHost, "")
	t.Setenv(envRemoteClientVersion, "")

	for _, id := range []string{"", "no-at-sign", "a@b\nc", strings.Repeat("x", clientFieldMax+1) + "@h"} {
		t.Setenv(envRemoteClient, id)
		recordClientHeartbeat()
		if _, err := os.Stat(clientRegistryFile(home)); !os.IsNotExist(err) {
			t.Fatalf("client %q must not be recorded", id)
		}
	}

	// A damaged file is left alone rather than crashing every command.
	if err := os.MkdirAll(filepath.Dir(clientRegistryFile(home)), 0o755); err != nil {
		t.Fatal(err)
	}
	damaged := "clients: [ not: valid"
	if err := os.WriteFile(clientRegistryFile(home), []byte(damaged), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envRemoteClient, "trillium@mac-mini")
	recordClientHeartbeat()
	if got := readFileString(t, clientRegistryFile(home)); got != damaged {
		t.Errorf("a damaged file must not be rewritten: %q", got)
	}
}

// TestRemoteScriptCarriesClientIdentity runs the exact script a client sends to
// the host, with the host's command replaced by a probe, and checks the
// environment the host's brain binary would see.
func TestRemoteScriptCarriesClientIdentity(t *testing.T) {
	hostBin := t.TempDir()
	probe := filepath.Join(hostBin, "task")
	writeExec(t, probe, `#!/bin/sh
printf 'id=%s\nhost=%s\nver=%s\noff=%s\nfwd=%s\n' "$BRAIN_REMOTE_CLIENT" "$BRAIN_REMOTE_CLIENT_HOST" "$BRAIN_REMOTE_CLIENT_VERSION" "$BRAIN_REMOTE" "$BRAIN_REMOTE_FORWARDED"
`)
	cfg := &remoteConfig{Host: "me@host.ts.net", BinDir: hostBin}
	script := cfg.remoteScript()
	if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil { //nolint:gosec // fixed test script
		t.Fatalf("the forwarding script must parse: %v\n%s", err, out)
	}
	out, err := exec.Command("sh", "-c", script, "task").CombinedOutput() //nolint:gosec // fixed test script
	if err != nil {
		t.Fatalf("running the forwarding script: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	want := map[string]string{
		"id":   "id=" + clientIdentity(),
		"host": "host=me@host.ts.net",
		"ver":  "ver=" + combinedVersion(),
		"off":  "off=off",
		"fwd":  "fwd=1",
	}
	for _, l := range lines {
		k, _, _ := strings.Cut(l, "=")
		if w, ok := want[k]; ok {
			if l != w {
				t.Errorf("host env %s = %q, want %q", k, l, w)
			}
			delete(want, k)
		}
	}
	if len(want) != 0 {
		t.Errorf("the host never saw %v (output: %q)", want, out)
	}
	// The client names itself, and the script still disables further forwarding.
	if !strings.Contains(script, envRemoteClient+"=") || !strings.Contains(script, "BRAIN_REMOTE=off") {
		t.Errorf("script lost its client identity or its forwarding guard: %s", script)
	}
}

func TestMergeClientRows(t *testing.T) {
	cfg := &hostConfig{Clients: []hostClient{{Target: "me@mac-mini.ts.net", Added: "2026-01-02"}}}
	reg := &clientRegistry{Clients: []clientRecord{
		{ID: "trillium@mac-mini", Host: "me@host.ts.net", Version: "1.0", FirstSeen: "2026-02-01T00:00:00Z", LastSeen: "2026-02-14T10:00:00Z"},
		{ID: "stranger@other", Version: "2.0", LastSeen: "2026-02-13T10:00:00Z"},
	}}
	rows := mergeClientRows(cfg, reg)
	if len(rows) != 2 {
		t.Fatalf("registered and observed must merge into two rows: %+v", rows)
	}
	// Most recently seen first.
	if rows[0].Client != "trillium@mac-mini" || !rows[0].Registered || !rows[0].Seen {
		t.Fatalf("the registered client must be merged with its heartbeat: %+v", rows[0])
	}
	if rows[0].Target != "me@mac-mini.ts.net" || rows[0].Added != "2026-01-02" || rows[0].Version != "1.0" {
		t.Errorf("merged row lost registration or version: %+v", rows[0])
	}
	if rows[1].Client != "stranger@other" || rows[1].Registered || !rows[1].Seen {
		t.Errorf("an unregistered client must still be listed: %+v", rows[1])
	}

	// A registered client that has never connected, and no heartbeats at all.
	rows = mergeClientRows(&hostConfig{Clients: []hostClient{{Target: "me@laptop", Added: "2026-01-03"}}}, &clientRegistry{})
	if len(rows) != 1 || rows[0].Seen || !rows[0].Registered || rows[0].Client != "me@laptop" {
		t.Fatalf("registered but unseen: %+v", rows)
	}

	// An exact id match needs no name heuristic.
	rows = mergeClientRows(&hostConfig{Clients: []hostClient{{Target: "trillium@mac-mini"}}},
		&clientRegistry{Clients: []clientRecord{{ID: "trillium@mac-mini", LastSeen: "2026-02-14T10:00:00Z"}}})
	if len(rows) != 1 || !rows[0].Registered || !rows[0].Seen {
		t.Fatalf("exact match: %+v", rows)
	}

	// An ambiguous machine name must not merge two registrations into one.
	ambiguous := &hostConfig{Clients: []hostClient{{Target: "a@mac-mini.ts.net"}, {Target: "b@mac-mini.ts.net"}}}
	rows = mergeClientRows(ambiguous, &clientRegistry{Clients: []clientRecord{{ID: "trillium@mac-mini", LastSeen: "2026-02-14T10:00:00Z"}}})
	if len(rows) != 3 {
		t.Fatalf("an ambiguous name must not merge: %+v", rows)
	}
	for _, r := range rows {
		if r.Registered && r.Seen {
			t.Errorf("no registration may claim an ambiguous heartbeat: %+v", r)
		}
	}
}

func TestClientListText(t *testing.T) {
	now := time.Date(2026, 2, 14, 10, 30, 0, 0, time.UTC)
	empty := clientListText("trillium@mac-mini", nil, now)
	for _, want := range []string{"No clients yet", "brain host clients add <ssh-target>", "brain remote"} {
		if !strings.Contains(empty, want) {
			t.Errorf("the empty state must mention %q:\n%s", want, empty)
		}
	}
	rows := []clientRow{
		{Client: "trillium@mac-mini", Version: "1.1.0+brain.0.5.1", LastSeen: "2026-02-14T10:25:00Z", Seen: true, Registered: true, Target: "me@mac-mini.ts.net", Host: "me@oldhost.ts.net"},
		{Client: "me@laptop", Registered: true, Target: "me@laptop"},
		{Client: "stranger@other", Seen: true, LastSeen: "2026-02-12T10:00:00Z", Host: "me@host.ts.net"},
	}
	text := clientListText("trillium@mac-mini", rows, now)
	for _, want := range []string{
		"brain host clients — trillium@mac-mini",
		"CLIENT", "VERSION", "LAST SEEN", "HOST", "REGISTERED",
		"trillium@mac-mini", "1.1.0+brain.0.5.1", "5m ago", "me@oldhost.ts.net", "me@mac-mini.ts.net",
		"me@laptop", "never seen", "yes",
		"stranger@other", "2d ago", "me@host.ts.net", "no (seen connecting)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the table must show %q:\n%s", want, text)
		}
	}
	// A client that never reported a host shows the dash in that column, not
	// a gap: the registered target is the REGISTERED column's job.
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "  me@laptop ") && !strings.Contains(line, "—") {
			t.Errorf("a hostless row must show — in the HOST column:\n%s", text)
		}
	}
	if !strings.Contains(humanAgo("2026-01-01T00:00:00Z", now), "2026-01-01") {
		t.Errorf("an old heartbeat should show its date: %s", humanAgo("2026-01-01T00:00:00Z", now))
	}
	if got := humanAgo("garbage", now); got != "garbage" {
		t.Errorf("an unparseable stamp must pass through: %q", got)
	}
}

func TestClientListPayload(t *testing.T) {
	home := t.TempDir()
	rows := []clientRow{{Client: "a@b", Seen: true, LastSeen: "2026-02-14T10:00:00Z"}}
	data, err := json.Marshal(clientListPayload(home, "a@b", rows, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"host", "config", "clients_config", "clients", "registered", "seen", "heartbeat_after"} {
		if _, ok := got[key]; !ok {
			t.Errorf("json payload is missing %q: %s", key, data)
		}
	}
	if got["config"] != hostConfigFile(home) || got["clients_config"] != clientRegistryFile(home) {
		t.Errorf("payload must name the files it read: %s", data)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestHostClientsListCommand runs the real command, so the wiring (the
// subcommand, --json, and the host-verb annotation that keeps it from opening a
// store) is exercised and not just its helpers.
func TestHostClientsListCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldJSON := jsonOutput
	t.Cleanup(func() { jsonOutput = oldJSON })

	// It is reachable as 'brain host clients list' and never opens a store.
	cmd, _, err := rootCmd.Find([]string{"host", "clients", "list"})
	if err != nil || cmd == nil || cmd.Name() != "list" {
		t.Fatalf("'host clients list' is not in the command tree: %v %v", cmd, err)
	}
	if !isBrainHostVerb(cmd) || isBrainRemoteVerb(cmd) {
		t.Errorf("'host clients list' must be a brain-host verb and not a remote verb")
	}

	// Nothing configured: the empty state teaches how to register a client.
	jsonOutput = false
	out := captureStdout(t, func() error { return brainHostClientsListCmd.RunE(brainHostClientsListCmd, nil) })
	for _, want := range []string{"No clients yet", "brain host clients add <ssh-target>"} {
		if !strings.Contains(out, want) {
			t.Errorf("empty state must mention %q:\n%s", want, out)
		}
	}

	// A registered client and a heartbeat that matches it.
	if err := saveHostConfig(home, &hostConfig{Role: roleHost, Generation: 1,
		Clients: []hostClient{{Target: "me@mac-mini.ts.net", Added: "2026-01-02"}}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envRemoteForward, "1")
	t.Setenv(envHostGate, "")
	t.Setenv(envRemoteClient, "trillium@mac-mini")
	t.Setenv(envRemoteClientHost, "me@host.ts.net")
	t.Setenv(envRemoteClientVersion, "1.1.0+brain.0.5.1")
	recordClientHeartbeat()

	out = captureStdout(t, func() error { return brainHostClientsListCmd.RunE(brainHostClientsListCmd, nil) })
	if !strings.Contains(out, "me@mac-mini.ts.net") || !strings.Contains(out, "1.1.0+brain.0.5.1") {
		t.Errorf("the registered client and its heartbeat must share a row:\n%s", out)
	}
	// The heartbeat's own report of where it pointed is a column of its own,
	// distinct from the registered target a promotion will use.
	if !strings.Contains(out, "me@host.ts.net") {
		t.Errorf("the table must show the host the client pointed at:\n%s", out)
	}

	jsonOutput = true
	out = captureStdout(t, func() error { return brainHostClientsListCmd.RunE(brainHostClientsListCmd, nil) })
	var payload struct {
		SchemaVersion int         `json:"schema_version"`
		Clients       []clientRow `json:"clients"`
		Registered    int         `json:"registered"`
		Seen          int         `json:"seen"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("--json must print one JSON object: %v\n%s", err, out)
	}
	if payload.SchemaVersion == 0 || len(payload.Clients) != 1 || payload.Registered != 1 || payload.Seen != 1 {
		t.Errorf("unexpected payload: %s", out)
	}
	if payload.Clients[0].Version != "1.1.0+brain.0.5.1" || payload.Clients[0].Target != "me@mac-mini.ts.net" {
		t.Errorf("unexpected row: %+v", payload.Clients[0])
	}
}

func TestHostClientsAddCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A new registration says what registration means and where to see both
	// halves: registered now, connected only once the client calls.
	out := captureStdout(t, func() error {
		return brainHostClientsAddCmd.RunE(brainHostClientsAddCmd, []string{"me@laptop.ts.net"})
	})
	for _, want := range []string{"me@laptop.ts.net", "never seen", "brain host clients list"} {
		if !strings.Contains(out, want) {
			t.Errorf("the add output must mention %q:\n%s", want, out)
		}
	}

	// Re-adding the same target answers instead of staying silent, and does
	// not record it twice.
	out = captureStdout(t, func() error {
		return brainHostClientsAddCmd.RunE(brainHostClientsAddCmd, []string{"me@laptop.ts.net"})
	})
	if !strings.Contains(out, "already registered") {
		t.Errorf("re-adding a registered target must say so:\n%s", out)
	}
	cfg, err := loadHostConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Clients) != 1 || cfg.Clients[0].Target != "me@laptop.ts.net" {
		t.Errorf("re-adding must not duplicate the target: %+v", cfg.Clients)
	}
}

func TestFindClientRows(t *testing.T) {
	rows := []clientRow{
		{Client: "trillium@laptop", Target: "me@laptop.ts.net", Registered: true, Seen: true},
		{Client: "me@desktop.ts.net", Target: "me@desktop.ts.net", Registered: true},
		{Client: "stranger@other", Seen: true},
	}
	cases := []struct {
		arg  string
		want []int
	}{
		{"trillium@laptop", []int{0}},   // the client's own name
		{"me@laptop.ts.net", []int{0}},  // its registered ssh target
		{"ME@LAPTOP.TS.NET", []int{0}},  // case does not matter
		{"laptop", []int{0}},            // the machine name, when one row owns it
		{"me@desktop.ts.net", []int{1}}, // registered but never seen
		{"stranger@other", []int{2}},    // seen but never registered
		{"other", []int{2}},
		{"nobody@nowhere", nil},
		{"", nil},
	}
	for _, c := range cases {
		got := findClientRows(rows, c.arg)
		if len(got) != len(c.want) {
			t.Errorf("findClientRows(%q) = %v, want %v", c.arg, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("findClientRows(%q) = %v, want %v", c.arg, got, c.want)
				break
			}
		}
	}
	// A machine name two rows share proves nothing, so it matches neither;
	// an exact name still matches even when the machine name is ambiguous.
	ambiguous := []clientRow{
		{Client: "a@laptop", Target: "a@laptop", Registered: true},
		{Client: "b@laptop", Target: "b@laptop", Registered: true},
	}
	if got := findClientRows(ambiguous, "laptop"); got != nil {
		t.Errorf("an ambiguous machine name must not match (got %v)", got)
	}
	if got := findClientRows(ambiguous, "a@laptop"); len(got) != 1 || got[0] != 0 {
		t.Errorf("an exact name must still match: %v", got)
	}
}

// TestHostClientsRemoveCommand runs the real command: removing a client must
// take the whole row out of 'brain host clients list', for a registered client
// and for one that only ever connected.
func TestHostClientsRemoveCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := saveHostConfig(home, &hostConfig{Role: roleHost, Generation: 1, Clients: []hostClient{
		{Target: "me@laptop.ts.net", Added: "2026-01-02"},
		{Target: "me@desktop.ts.net", Added: "2026-01-03"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := saveClientRegistry(home, &clientRegistry{Clients: []clientRecord{
		{ID: "trillium@laptop", Host: "me@host.ts.net", Version: "1.1.0", FirstSeen: "2026-02-01T10:00:00Z", LastSeen: "2026-02-14T10:00:00Z"},
		{ID: "stranger@other", Version: "2.0", FirstSeen: "2026-02-13T10:00:00Z", LastSeen: "2026-02-13T10:00:00Z"},
	}}); err != nil {
		t.Fatal(err)
	}

	// The registered target and the heartbeat merged into its row both go, so
	// the client is really gone rather than re-listed as "seen connecting".
	out := captureStdout(t, func() error {
		return brainHostClientsRemoveCmd.RunE(brainHostClientsRemoveCmd, []string{"me@laptop.ts.net"})
	})
	for _, want := range []string{"forgotten", "registration and heartbeat", "next call"} {
		if !strings.Contains(out, want) {
			t.Errorf("the remove output must mention %q:\n%s", want, out)
		}
	}
	cfg, err := loadHostConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Clients) != 1 || cfg.Clients[0].Target != "me@desktop.ts.net" {
		t.Errorf("the other registration must survive: %+v", cfg.Clients)
	}
	reg, err := loadClientRegistry(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Clients) != 1 || reg.Clients[0].ID != "stranger@other" {
		t.Errorf("only the forgotten client's heartbeat may go: %+v", reg.Clients)
	}

	// A client that only ever connected has no ssh target to name it by, so
	// its own name (and the machine name it owns) must work.
	out = captureStdout(t, func() error {
		return brainHostClientsRemoveCmd.RunE(brainHostClientsRemoveCmd, []string{"stranger@other"})
	})
	if !strings.Contains(out, "heartbeat removed") {
		t.Errorf("a seen-only client must report a heartbeat removal:\n%s", out)
	}
	if reg, _ = loadClientRegistry(home); len(reg.Clients) != 0 {
		t.Errorf("the heartbeat must be gone: %+v", reg.Clients)
	}
	if cfg, _ = loadHostConfig(home); cfg == nil {
		t.Fatal("host.yaml disappeared")
	}
	if rows := mergeClientRows(cfg, reg); len(rows) != 1 || rows[0].Client != "me@desktop.ts.net" {
		t.Errorf("only the registered, never-seen client may remain: %+v", rows)
	}

	// The machine name reaches a registered client too.
	out = captureStdout(t, func() error {
		return brainHostClientsRemoveCmd.RunE(brainHostClientsRemoveCmd, []string{"desktop"})
	})
	if !strings.Contains(out, "registration removed") {
		t.Errorf("a machine name must find the registered client:\n%s", out)
	}
	if cfg, _ = loadHostConfig(home); cfg == nil {
		t.Fatal("host.yaml disappeared")
	}
	if rows := mergeClientRows(cfg, reg); len(rows) != 0 {
		t.Errorf("every client must be gone: %+v", rows)
	}

	// An unknown name is an error, not a cheerful lie that changed nothing.
	if err := brainHostClientsRemoveCmd.RunE(brainHostClientsRemoveCmd, []string{"nobody@nowhere"}); err == nil {
		t.Error("removing an unknown client must fail")
	} else if !strings.Contains(err.Error(), "brain host clients list") {
		t.Errorf("the error must point at the list: %v", err)
	}
}

// TestHostClientsRemoveDoesNotInventAHost guards the pathway from the other
// side: a remove on a machine with no host.yaml must fail and must not create
// one, which would claim this machine is the host.
func TestHostClientsRemoveDoesNotInventAHost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := brainHostClientsRemoveCmd.RunE(brainHostClientsRemoveCmd, []string{"me@laptop"}); err == nil {
		t.Fatal("removing a client from an unconfigured machine must fail")
	}
	if _, err := os.Stat(hostConfigFile(home)); !os.IsNotExist(err) {
		t.Errorf("a failed remove must not write host.yaml: %v", err)
	}
	if _, err := os.Stat(clientRegistryFile(home)); !os.IsNotExist(err) {
		t.Errorf("a failed remove must not write clients.yaml: %v", err)
	}
}

func TestThisClientIndex(t *testing.T) {
	rows := []clientRow{
		{Client: "trillium@laptop", Seen: true, LastSeen: "2026-02-14T10:25:00Z", Registered: true, Target: "me@laptop.ts.net"},
		{Client: "me@desktop.ts.net", Registered: true, Target: "me@desktop.ts.net"},
		{Client: "stranger@other", Seen: true, LastSeen: "2026-02-12T10:00:00Z"},
	}
	cases := []struct {
		you  string
		want int
	}{
		{"trillium@laptop", 0},  // the client's own name
		{"TRILIUM@LAPTOP", 0},   // case does not matter
		{"me@laptop.ts.net", 0}, // its registered target shares the machine name
		{"someone@desktop", 1},  // registered under a different name, never seen
		{"nobody@nowhere", -1},  // not this machine
		{"", -1},                // nothing to match
	}
	for _, c := range cases {
		if got := thisClientIndex(rows, c.you); got != c.want {
			t.Errorf("thisClientIndex(%q) = %d, want %d", c.you, got, c.want)
		}
	}
	// Two registered targets share a machine name: refuse to guess.
	ambiguous := []clientRow{
		{Client: "a@laptop", Registered: true},
		{Client: "b@laptop", Registered: true},
	}
	if got := thisClientIndex(ambiguous, "c@laptop"); got != -1 {
		t.Errorf("an ambiguous machine name must not be marked (got %d)", got)
	}
}

func TestClientListTextForMarksThisMachine(t *testing.T) {
	now := time.Date(2026, 2, 14, 10, 30, 0, 0, time.UTC)
	rows := []clientRow{
		{Client: "trillium@laptop", Version: "1.1.0", LastSeen: "2026-02-14T10:25:00Z", Seen: true, Registered: true, Target: "me@laptop.ts.net"},
		{Client: "stranger@other", Seen: true, LastSeen: "2026-02-12T10:00:00Z"},
	}
	// The host-side view has no machine to mark.
	if got := clientListTextFor("me@host", rows, now, ""); strings.Contains(got, "this machine") {
		t.Errorf("no marker without a caller:\n%s", got)
	}
	got := clientListTextFor("me@host", rows, now, "trillium@laptop")
	if strings.Count(got, "← this machine") != 1 {
		t.Errorf("exactly one row is this machine:\n%s", got)
	}
	if !strings.Contains(got, "me@laptop.ts.net  ← this machine") {
		t.Errorf("the marker must sit on this machine's row:\n%s", got)
	}
}
