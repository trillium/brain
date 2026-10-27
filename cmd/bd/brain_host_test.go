package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRebasePathAndText(t *testing.T) {
	if got, ok := rebasePath("/Users/me/data/inbox/.beads", "/Users/me", "/home/you"); !ok || got != "/home/you/data/inbox/.beads" {
		t.Errorf("rebasePath = %q, %v", got, ok)
	}
	if got, ok := rebasePath("/Users/me", "/Users/me", "/home/you"); !ok || got != "/home/you" {
		t.Errorf("the home directory itself: %q, %v", got, ok)
	}
	if got, ok := rebasePath("/Users/mellon/x", "/Users/me", "/home/you"); ok || got != "/Users/mellon/x" {
		t.Errorf("a sibling directory with a shared prefix must not move: %q, %v", got, ok)
	}
	in := `export BEADS_DIR="/Users/me/data/x/.beads"
export OTHER=/Users/mellon/data
cd /Users/me
echo '/Users/me/'`
	want := `export BEADS_DIR="/home/you/data/x/.beads"
export OTHER=/Users/mellon/data
cd /home/you
echo '/home/you/'`
	if got := rebaseText(in, "/Users/me", "/home/you"); got != want {
		t.Errorf("rebaseText:\n%s\nwant:\n%s", got, want)
	}
	if got := rebaseText(in, "/Users/me", "/Users/me"); got != in {
		t.Error("same home must be a no-op")
	}
}

func TestRebasePortPinsOnlyTouchesTheMovedServer(t *testing.T) {
	in := "export BEADS_DOLT_SERVER_PORT=3307\nexport BEADS_DOLT_SERVER_PORT=\"3307\"\nexport BEADS_DOLT_SERVER_PORT=9999\n"
	got := rebasePortPins(in, 3307, 4400)
	if strings.Count(got, "=4400") != 1 || !strings.Contains(got, `="4400"`) || !strings.Contains(got, "=9999") {
		t.Errorf("rebasePortPins = %q", got)
	}
	if rebasePortPins(in, 3307, 3307) != in {
		t.Error("same port must be a no-op")
	}
}

func TestRebaseMetadataKeepsUnknownFields(t *testing.T) {
	in := `{"database":"dolt","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":3307,"dolt_database":"brain_unified","project_id":"p-1","future_field":{"a":[1,2]}}`
	out, notes, err := rebaseMetadata([]byte(in), "/Users/me", "/home/you", 3307, 4400)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"dolt_server_port": 4400`, `"project_id": "p-1"`, `"future_field"`, `"dolt_database": "brain_unified"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
	if len(notes) != 0 {
		t.Errorf("unexpected notes %v", notes)
	}
	_, notes, _ = rebaseMetadata([]byte(`{"dolt_server_port":5555,"dolt_server_host":"db.example"}`), "/a", "/b", 3307, 4400)
	if len(notes) != 2 {
		t.Errorf("a foreign port and host should each be noted: %v", notes)
	}
	if _, _, err := rebaseMetadata([]byte("not json"), "/a", "/b", 1, 2); err == nil {
		t.Error("bad json must be an error")
	}
}

func TestMoveWrapperRewritesPathsPortsAndBinary(t *testing.T) {
	w := layoutFile{Path: "/Users/me/.local/bin/task", Name: "task", Mode: 0o755, Content: "#!/bin/sh\nexport BEADS_DIR=\"/Users/me/data/task/.beads\"\nexport BEADS_DOLT_SERVER_PORT=3307\nexec bd \"$@\"\n"}
	pl := &promotePlan{}
	got := moveWrapper(w, "20261012T000000Z-aaaa", "/Users/me", "/home/you", 3307, 4400, "/home/you/.local/bin/brain", "/opt/homebrew/bin/bd", pl)
	if got.Path != "/home/you/.local/bin/task" {
		t.Errorf("path %q", got.Path)
	}
	for _, want := range []string{"/home/you/data/task/.beads", "BEADS_DOLT_SERVER_PORT=4400", "exec '/home/you/.local/bin/brain' \"$@\"", hostWrapperMarker} {
		if !strings.Contains(got.Content, want) {
			t.Errorf("missing %q in\n%s", want, got.Content)
		}
	}
	if !strings.HasPrefix(got.Content, "#!/bin/sh\n"+hostWrapperMarker) {
		t.Error("the marker must be the second line")
	}
	// When bd on the target already is the brain build, 'exec bd' stays.
	same := moveWrapper(w, "20261012T000000Z-aaaa", "/Users/me", "/home/you", 3307, 3307, "/x/bd", "/x/bd", pl)
	if !strings.Contains(same.Content, "exec bd \"$@\"") {
		t.Errorf("exec bd must stay when bd is the brain build:\n%s", same.Content)
	}
}

func TestJournalUndoRestoresEverything(t *testing.T) {
	dir := t.TempDir()
	j, err := openJournal(filepath.Join(dir, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(dir, "existing")
	created := filepath.Join(dir, "sub", "created")
	link := filepath.Join(dir, "link")
	removed := filepath.Join(dir, "removed")
	moveFrom, moveTo := filepath.Join(dir, "from"), filepath.Join(dir, "to")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(existing, []byte("original"), 0o640))
	must(os.WriteFile(removed, []byte("keep me"), 0o600))
	must(os.Symlink("/old/target", link))
	must(os.Mkdir(moveFrom, 0o755))
	must(os.WriteFile(filepath.Join(moveFrom, "f"), []byte("x"), 0o644))

	must(j.writeFile(existing, []byte("first change"), 0o644))
	must(j.writeFile(existing, []byte("second change"), 0o644)) // the journal must keep the ORIGINAL
	must(j.writeFile(created, []byte("new"), 0o644))
	must(j.writeSymlink(link, "/new/target"))
	must(j.removeFile(removed))
	must(j.rename(moveFrom, moveTo))

	steps, err := j.undo(nil)
	must(err)
	if len(steps) == 0 {
		t.Fatal("undo did nothing")
	}
	if b, _ := os.ReadFile(existing); string(b) != "original" {
		t.Errorf("existing file = %q, want the original", b)
	}
	if info, _ := os.Stat(existing); info.Mode().Perm() != 0o640 {
		t.Errorf("mode not restored: %v", info.Mode().Perm())
	}
	if _, err := os.Lstat(created); err == nil {
		t.Error("a file the promotion created must be removed")
	}
	if got, _ := os.Readlink(link); got != "/old/target" {
		t.Errorf("link = %q", got)
	}
	if b, _ := os.ReadFile(removed); string(b) != "keep me" {
		t.Errorf("removed file not restored: %q", b)
	}
	if _, err := os.Stat(filepath.Join(moveFrom, "f")); err != nil {
		t.Errorf("rename not undone: %v", err)
	}
	// A second undo is a no-op.
	if steps, err := j.undo(nil); err != nil || len(steps) != 0 {
		t.Errorf("second undo: %v %v", steps, err)
	}
}

func TestJournalRenameIsResumable(t *testing.T) {
	dir := t.TempDir()
	j, _ := openJournal(filepath.Join(dir, "j"))
	from, to := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.Mkdir(from, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := j.rename(from, to); err != nil {
		t.Fatal(err)
	}
	if err := j.rename(from, to); err != nil {
		t.Errorf("repeating a finished rename must succeed: %v", err)
	}
	other := filepath.Join(dir, "c")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := j.rename(other, to); err == nil {
		t.Error("renaming onto an existing path must fail")
	}
}

func TestHostConfigRoundTrip(t *testing.T) {
	home := t.TempDir()
	if cfg, err := loadHostConfig(home); cfg != nil || err != nil {
		t.Fatalf("missing file: %v %v", cfg, err)
	}
	want := &hostConfig{
		Role: roleMoved, Generation: 3, PromoteID: "20261012T000000Z-aaaa", MovedTo: "me@new", MovedAt: nowStamp(),
		Server:   &hostServer{DataDir: "/d", Bind: "127.0.0.1", Port: 3307, Databases: []string{"brain_unified"}, Service: hostService{Kind: "process"}},
		Clients:  []hostClient{{Target: "c1"}},
		Services: []string{"com.x"},
		Lineage:  []lineageEntry{{PromoteID: "20261012T000000Z-aaaa", Generation: 3, From: "m1", To: "m2"}},
	}
	if err := saveHostConfig(home, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadHostConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	if got.Role != want.Role || got.Generation != 3 || got.MovedTo != "me@new" || got.Server.Port != 3307 || len(got.Lineage) != 1 || got.Lineage[0].From != "m1" || got.Services[0] != "com.x" {
		t.Errorf("round trip: %+v", got)
	}
	if err := os.WriteFile(hostConfigFile(home), []byte("role: wizard\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadHostConfig(home); err == nil {
		t.Error("an unknown role must be an error")
	}
}

func TestMovedNoticeRoundTripAndLegacy(t *testing.T) {
	n := movedNotice{Target: "me@new", PromoteID: "20261012T000000Z-aaaa", Generation: 4, From: "abcd1234", At: "2026-10-12"}
	got := parseMovedNotice([]byte(n.line()))
	if got == nil || *got != n || !got.complete() {
		t.Fatalf("round trip: %+v", got)
	}
	legacy := parseMovedNotice([]byte(hostMovedMarker + "me@new — moved"))
	if legacy == nil || legacy.Target != "me@new" || legacy.complete() {
		t.Errorf("a notice without facts is parsed but incomplete: %+v", legacy)
	}
	if parseMovedNotice([]byte("some other stderr line")) != nil {
		t.Error("not a notice")
	}
}

func TestMovedFilterSwallowsOnlyTheMarkerLine(t *testing.T) {
	cases := []struct {
		name     string
		writes   []string
		wantOut  string
		wantMove bool
	}{
		{"marker alone", []string{(movedNotice{Target: "h", PromoteID: "20261012T000000Z-aaaa", Generation: 2, From: "f"}).line() + "\n"}, "", true},
		{"marker split across writes", []string{hostMovedMarker + "h promote=20261012T000000Z-aaaa", " generation=2 from=f\nrest\n"}, "rest\n", true},
		{"ordinary error", []string{"Error: boom\n"}, "Error: boom\n", false},
		{"short output without newline", []string{"hi"}, "hi", false},
	}
	for _, c := range cases {
		var out bytes.Buffer
		f := &movedFilter{dst: &out}
		for _, w := range c.writes {
			_, _ = f.Write([]byte(w))
		}
		f.flush()
		if out.String() != c.wantOut || (f.notice != nil) != c.wantMove {
			t.Errorf("%s: out=%q notice=%v", c.name, out.String(), f.notice)
		}
	}
}

func TestLineageContainsRequiresOrderAndIdentity(t *testing.T) {
	lin := []lineageEntry{{PromoteID: "p1", Generation: 2, From: "A"}, {PromoteID: "p2", Generation: 3, From: "B"}}
	ok := []movedNotice{{PromoteID: "p1", Generation: 2, From: "A"}, {PromoteID: "p2", Generation: 3, From: "B"}}
	if err := lineageContains(lin, ok); err != nil {
		t.Errorf("genuine chain: %v", err)
	}
	for name, hops := range map[string][]movedNotice{
		"wrong sender":     {{PromoteID: "p1", Generation: 2, From: "Z"}},
		"wrong generation": {{PromoteID: "p1", Generation: 9, From: "A"}},
		"unknown id":       {{PromoteID: "zz", Generation: 2, From: "A"}},
		"out of order":     {{PromoteID: "p2", Generation: 3, From: "B"}, {PromoteID: "p1", Generation: 2, From: "A"}},
	} {
		if err := lineageContains(lin, hops); err == nil {
			t.Errorf("%s must not verify", name)
		}
	}
}

// fakeNet is a stand-in for the network of machines followMove probes.
type fakeNet map[string]struct {
	machine string
	host    *hostConfig
	forward *forwardRecord
	err     error
}

func withFakeNet(t *testing.T, net fakeNet) {
	t.Helper()
	old := followProbe
	followProbe = func(_ context.Context, target string, _ []string) (*peerHelloResp, *peerStatusResp, error) {
		m, ok := net[target]
		if !ok {
			return nil, nil, errors.New("no route to host")
		}
		if m.err != nil {
			return nil, nil, m.err
		}
		return &peerHelloResp{MachineID: m.machine}, &peerStatusResp{MachineID: m.machine, Host: m.host, Forward: m.forward}, nil
	}
	t.Cleanup(func() { followProbe = old })
}

func notice(target, id string, gen int, from string) *movedNotice {
	return &movedNotice{Target: target, PromoteID: id, Generation: gen, From: from, At: "2026-10-12"}
}

const (
	pid1 = "20261012T000001Z-aaaa1111"
	pid2 = "20261012T000002Z-bbbb2222"
)

func TestFollowMoveSingleHopAndChain(t *testing.T) {
	cfg := &remoteConfig{Host: "old"}
	// old → new
	withFakeNet(t, fakeNet{"new": {machine: "M-new", host: &hostConfig{Role: roleHost, Generation: 2, Lineage: []lineageEntry{{PromoteID: pid1, Generation: 2, From: "M-old", To: "M-new"}}}}})
	final, hops, err := followMove(context.Background(), cfg, notice("new", pid1, 2, "M-old"))
	if err != nil || final != "new" || len(hops) != 1 {
		t.Fatalf("single hop: %q %v %v", final, hops, err)
	}
	// old → mid → last: mid has itself moved on and holds a forwarding record.
	withFakeNet(t, fakeNet{
		"mid": {machine: "M-mid", host: &hostConfig{Role: roleMoved, Generation: 2, Lineage: []lineageEntry{{PromoteID: pid1, Generation: 2, From: "M-old", To: "M-mid"}, {PromoteID: pid2, Generation: 3, From: "M-mid", To: "M-last"}}},
			forward: &forwardRecord{To: "last", PromoteID: pid2, Generation: 3, FromMachine: "M-mid"}},
		"last": {machine: "M-last", host: &hostConfig{Role: roleHost, Generation: 3, Lineage: []lineageEntry{{PromoteID: pid1, Generation: 2, From: "M-old", To: "M-mid"}, {PromoteID: pid2, Generation: 3, From: "M-mid", To: "M-last"}}}},
	})
	final, hops, err = followMove(context.Background(), cfg, notice("mid", pid1, 2, "M-old"))
	if err != nil || final != "last" || len(hops) != 2 {
		t.Fatalf("chain: %q %v %v", final, hops, err)
	}
}

func TestFollowMoveRefusesWhatItCannotVerify(t *testing.T) {
	cfg := &remoteConfig{Host: "old"}
	hostOK := &hostConfig{Role: roleHost, Generation: 2, Lineage: []lineageEntry{{PromoteID: pid1, Generation: 2, From: "M-old"}}}
	cases := []struct {
		name string
		net  fakeNet
		n    *movedNotice
		want string
	}{
		{"unreachable", fakeNet{}, notice("new", pid1, 2, "M-old"), "could not be verified"},
		{"not a brain host at all", fakeNet{"new": {machine: "M", host: nil}}, notice("new", pid1, 2, "M-old"), "not a brain host"},
		{"claims the role without having received it", fakeNet{"new": {machine: "M", host: &hostConfig{Role: roleHost, Generation: 2}}}, notice("new", pid1, 2, "M-old"), "does not confirm"},
		{"a third machine that merely got named", fakeNet{"new": {machine: "M", host: hostOK}}, notice("new", pid1, 2, "M-someone-else"), "does not confirm"},
		{"older generation than the move", fakeNet{"new": {machine: "M", host: &hostConfig{Role: roleHost, Generation: 1, Lineage: hostOK.Lineage}}}, notice("new", pid1, 2, "M-old"), "older than the move"},
		{"moved but no forwarding record", fakeNet{"new": {machine: "M", host: &hostConfig{Role: roleMoved, Generation: 2, Lineage: hostOK.Lineage}}}, notice("new", pid1, 2, "M-old"), "no forwarding record"},
		{"a notice from an older host", fakeNet{}, &movedNotice{Target: "new"}, "older brain"},
		{"a loop back to the first host", fakeNet{"mid": {machine: "M-mid", host: &hostConfig{Role: roleMoved, Generation: 2, Lineage: hostOK.Lineage},
			forward: &forwardRecord{To: "old", PromoteID: pid2, Generation: 3, FromMachine: "M-mid"}}}, notice("mid", pid1, 2, "M-old"), "loop"},
		{"an option-looking target", fakeNet{}, notice("-oProxyCommand=x", pid1, 2, "M-old"), "not a valid ssh target"},
	}
	for _, c := range cases {
		withFakeNet(t, c.net)
		final, _, err := followMove(context.Background(), cfg, c.n)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: final=%q err=%v, want error containing %q", c.name, final, err, c.want)
		}
	}
}

func TestFollowMoveRefusesLongChains(t *testing.T) {
	net := fakeNet{}
	var lin []lineageEntry
	for i := 1; i <= 8; i++ {
		id := fmt.Sprintf("20261012T0000%02dZ-cccc%04d", i, i)
		lin = append(lin, lineageEntry{PromoteID: id, Generation: i + 1, From: fmt.Sprintf("M%d", i-1)})
		net[fmt.Sprintf("h%d", i)] = struct {
			machine string
			host    *hostConfig
			forward *forwardRecord
			err     error
		}{machine: fmt.Sprintf("M%d", i), host: &hostConfig{Role: roleMoved, Generation: i + 1, Lineage: append([]lineageEntry{}, lin...)},
			forward: &forwardRecord{To: fmt.Sprintf("h%d", i+1), PromoteID: fmt.Sprintf("20261012T0000%02dZ-cccc%04d", i+1, i+1), Generation: i + 2, FromMachine: fmt.Sprintf("M%d", i)}}
	}
	withFakeNet(t, net)
	_, _, err := followMove(context.Background(), &remoteConfig{Host: "h0"}, notice("h1", "20261012T000001Z-cccc0001", 2, "M0"))
	if err == nil {
		t.Error("an eight-hop chain must be refused")
	}
}

func TestActiveForwardIgnoresAStaleRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	rec := &forwardRecord{To: "new", PromoteID: pid1, Generation: 2, FromMachine: "M-old", MovedAt: nowStamp()}
	data, _ := marshalForwardRecord(rec)
	if err := os.MkdirAll(filepath.Dir(forwardRecordFile(home)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forwardRecordFile(home), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if activeForward(home) == nil {
		t.Fatal("a record with no host.yaml is active")
	}
	_ = saveHostConfig(home, &hostConfig{Role: roleMoved, Generation: 1})
	if activeForward(home) == nil {
		t.Fatal("a moved host's record is active")
	}
	_ = saveHostConfig(home, &hostConfig{Role: roleHost, Generation: 3})
	if activeForward(home) != nil {
		t.Error("this machine became the host again at a newer generation: the record is stale")
	}
}

func TestHostGateEarlyAnswersOnlyForwardedCalls(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(envHostGate, "")
	rec := &forwardRecord{To: "new", PromoteID: pid1, Generation: 2, FromMachine: "M-old", MovedAt: nowStamp()}
	data, _ := marshalForwardRecord(rec)
	_ = os.MkdirAll(filepath.Dir(forwardRecordFile(home)), 0o755)
	_ = os.WriteFile(forwardRecordFile(home), data, 0o644)

	t.Setenv(envRemoteForward, "")
	if handled, _ := hostGateEarly([]string{"list"}); handled {
		t.Error("a local call must not be redirected by the early gate")
	}
	t.Setenv(envRemoteForward, "1")
	if handled, code := hostGateEarly([]string{"list"}); !handled || code != hostMovedExit {
		t.Errorf("a forwarded call must be redirected: %v %d", handled, code)
	}
	if handled, _ := hostGateEarly([]string{"host", "status"}); handled {
		t.Error("'host' verbs are never redirected")
	}
	t.Setenv(envHostGate, "off")
	if handled, _ := hostGateEarly([]string{"list"}); handled {
		t.Error("BRAIN_HOST_GATE=off bypasses the gate")
	}
}

func TestHostRoleGate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(envHostGate, "")
	mk := func(use string) *cobra.Command {
		root := &cobra.Command{Use: "bd"}
		c := &cobra.Command{Use: use}
		root.AddCommand(c)
		return c
	}
	create, list, hostCmd := mk("create"), mk("list"), mk("host")

	if err := hostRoleGate(create); err != nil {
		t.Errorf("no host.yaml: %v", err)
	}
	_ = saveHostConfig(home, &hostConfig{Role: roleFrozen, PromoteID: pid1, FrozenSince: nowStamp()})
	if err := hostRoleGate(create); err == nil || !strings.Contains(err.Error(), "frozen") {
		t.Errorf("a frozen host must refuse writes: %v", err)
	}
	if err := hostRoleGate(list); err != nil {
		t.Errorf("a frozen host still reads: %v", err)
	}
	if err := hostRoleGate(hostCmd); err != nil {
		t.Errorf("'host' verbs always run: %v", err)
	}
	_ = saveHostConfig(home, &hostConfig{Role: roleMoved, MovedTo: "new", MovedAt: nowStamp()})
	if err := hostRoleGate(list); err == nil || !strings.Contains(err.Error(), "no longer the brain host") {
		t.Errorf("a moved host refuses even reads (they would be stale): %v", err)
	}
	if err := hostRoleGate(hostCmd); err != nil {
		t.Errorf("'host' verbs run on a moved host: %v", err)
	}
	t.Setenv(envHostGate, "off")
	if err := hostRoleGate(create); err != nil {
		t.Errorf("the bypass: %v", err)
	}
}

func TestCompareManifests(t *testing.T) {
	a := &dbManifest{Database: "d", Hash: "h1", Tables: map[string]rowDigest{"issues": {Rows: 3, Digest: "x"}},
		Stores: []storeManifest{{Store: "inbox", Tables: map[string]rowDigest{"issues": {Rows: 2, Digest: "a:b"}}}}}
	b := &dbManifest{Database: "d", Hash: "h1", Tables: map[string]rowDigest{"issues": {Rows: 3, Digest: "x"}},
		Stores: []storeManifest{{Store: "inbox", Tables: map[string]rowDigest{"issues": {Rows: 2, Digest: "a:b"}}}}}
	if d := compareManifests(a, b); len(d) != 0 {
		t.Errorf("identical manifests differ: %v", d)
	}
	b.Hash = "h2"
	b.Tables["issues"] = rowDigest{Rows: 4, Digest: "y"}
	b.Stores[0].Tables["issues"] = rowDigest{Rows: 1, Digest: "z"}
	b.Stores = append(b.Stores, storeManifest{Store: "extra"})
	if d := compareManifests(a, b); len(d) != 4 {
		t.Errorf("want 4 differences (hash, table, store row, extra store), got %v", d)
	}
}

func TestVersionAndCountParsing(t *testing.T) {
	if v := parseDoltVersion("dolt version 2.2.3\nWarning: old"); v != "2.2.3" {
		t.Errorf("version %q", v)
	}
	for _, c := range []struct {
		a, b string
		want int
	}{{"2.2.3", "2.1.10", 1}, {"2.1.10", "2.2.3", -1}, {"2.2", "2.2.0", 0}, {"10.0.0", "9.9.9", 1}} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%s,%s)=%d", c.a, c.b, got)
		}
	}
	for in, want := range map[string]int64{`{"count": 504, "schema_version": 1}`: 504, "42\n": 42, "Warning: x\n{\"count\":7}": 7} {
		got, err := parseCountOutput([]byte(in))
		if err != nil || got != want {
			t.Errorf("parseCountOutput(%q)=%d,%v", in, got, err)
		}
	}
	if _, err := parseCountOutput([]byte("Error: nope")); err == nil {
		t.Error("an error message is not a count")
	}
	if humanBytes(383*1024*1024) != "383.0 MiB" {
		t.Errorf("humanBytes %q", humanBytes(383*1024*1024))
	}
}

// evaluateProbe is where "the target already holds a different live brain" is decided.
func TestEvaluateProbeRefusesADifferentBrain(t *testing.T) {
	p := &promoter{st: &promoteState{ID: "20261012T000000Z-aaaa"}, home: t.TempDir()}
	mk := func(pr peerProbeResp, opts promoteOptions) *promotePlan {
		p.opts = opts
		pl := &promotePlan{Port: 3307, DataDir: "/t/data/.beads", Probe: &pr}
		p.evaluateProbe(pl)
		return pl
	}
	if pl := mk(peerProbeResp{}, promoteOptions{}); len(pl.Blockers) != 0 {
		t.Errorf("an empty target must be fine: %v", pl.Blockers)
	}
	if pl := mk(peerProbeResp{Role: roleHost, Generation: 4, PromoteID: "someone-else"}, promoteOptions{}); len(pl.Blockers) != 1 || !strings.Contains(pl.Blockers[0], "already a brain host") {
		t.Errorf("a host from another promotion: %v", pl.Blockers)
	}
	if pl := mk(peerProbeResp{Role: roleHost, Generation: 3, PromoteID: "20261012T000000Z-aaaa"}, promoteOptions{}); len(pl.Blockers) != 0 {
		t.Errorf("the target this very promotion already activated must be fine on resume: %v", pl.Blockers)
	}
	if pl := mk(peerProbeResp{ServerListening: true}, promoteOptions{}); len(pl.Blockers) != 1 {
		t.Errorf("a stranger on the port: %v", pl.Blockers)
	}
	if pl := mk(peerProbeResp{ServerListening: true, ServerOurs: true}, promoteOptions{}); len(pl.Blockers) != 0 {
		t.Errorf("our own staging server on the port: %v", pl.Blockers)
	}
	if pl := mk(peerProbeResp{DatabasesPresent: []string{"brain_unified"}}, promoteOptions{}); len(pl.Blockers) != 1 || !strings.Contains(pl.Blockers[0], "different brain") {
		t.Errorf("a database that is not ours: %v", pl.Blockers)
	}
	if pl := mk(peerProbeResp{DatabasesPresent: []string{"brain_unified"}, DatabasesOurs: []string{"brain_unified"}}, promoteOptions{}); len(pl.Blockers) != 0 {
		t.Errorf("a database this promotion placed: %v", pl.Blockers)
	}
	former := peerProbeResp{Role: roleMoved, MovedTo: "other", DatabasesPresent: []string{"brain_unified"}}
	if pl := mk(former, promoteOptions{ReplaceStale: true}); len(pl.Blockers) != 0 {
		t.Errorf("a former host's stale copy is replaceable with --replace-stale: %v", pl.Blockers)
	}
	former.OldServerPort = 3307
	if pl := mk(former, promoteOptions{ReplaceStale: true}); len(pl.Blockers) != 1 || !strings.Contains(pl.Blockers[0], "still running") {
		t.Errorf("its old server must be stopped first: %v", pl.Blockers)
	}
	if pl := mk(peerProbeResp{StoreRootsPresent: []string{"/t/data/inbox"}}, promoteOptions{}); len(pl.Blockers) != 1 {
		t.Errorf("a store directory in the way: %v", pl.Blockers)
	}
	if pl := mk(peerProbeResp{StoreRootsPresent: []string{"/t/data/inbox"}}, promoteOptions{MoveAside: true}); len(pl.Blockers) != 0 {
		t.Errorf("--move-aside: %v", pl.Blockers)
	}
	if pl := mk(peerProbeResp{WrapperConflicts: []string{"/t/.local/bin/brain"}}, promoteOptions{}); len(pl.Blockers) != 1 {
		t.Errorf("a wrapper that is not ours: %v", pl.Blockers)
	}
}

func TestFileConflictLeavesGeneratedClientWrappersReplaceable(t *testing.T) {
	dir := t.TempDir()
	generated := filepath.Join(dir, "gen")
	_ = os.WriteFile(generated, []byte(remoteWrapperBody("task", "/bin/brain")), 0o755)
	handmade := filepath.Join(dir, "hand")
	_ = os.WriteFile(handmade, []byte("#!/bin/sh\necho mine\n"), 0o755)
	same := filepath.Join(dir, "same")
	_ = os.WriteFile(same, []byte("content"), 0o755)
	if fileConflict(filepath.Join(dir, "missing"), "x") {
		t.Error("a missing file is no conflict")
	}
	if fileConflict(generated, "different") {
		t.Error("a generated client wrapper is ours to replace")
	}
	if !fileConflict(handmade, "different") {
		t.Error("a hand-made file must not be replaced silently")
	}
	if fileConflict(same, sha256Hex([]byte("content"))) {
		t.Error("an identical file is no conflict")
	}
}

func TestSummarizeUndoFoldsFileSteps(t *testing.T) {
	steps := []string{"removed /a", "removed /b", "restored /c", "restored link /d", "moved /x back to /y", "stopped the dolt server"}
	got := strings.Join(summarizeUndo(steps), "|")
	for _, want := range []string{"removed 2 files", "restored 1 file", "restored 1 link", "moved 1 directory back", "stopped the dolt server"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q lacks %q", got, want)
		}
	}
}

func TestLaunchdServerPlistAndServiceCalls(t *testing.T) {
	home := t.TempDir()
	m := newServiceManager(home)
	var calls []string
	m.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	svc := m.newServerService("launchd", "")
	if svc.Label != "com.brain.dolt-server" || !strings.HasSuffix(svc.Plist, "Library/LaunchAgents/com.brain.dolt-server.plist") {
		t.Fatalf("default service: %+v", svc)
	}
	plist := launchdServerPlist(svc, serverSpec{DoltBin: "/opt/homebrew/bin/dolt", DataDir: "/Users/me/data & more/.beads", Bind: "127.0.0.1", Port: 3307, RemotesAPIPort: 3310})
	for _, want := range []string{"<string>com.brain.dolt-server</string>", "<string>sql-server</string>", "<string>3307</string>", "--remotesapi-port", "<string>3310</string>",
		"/Users/me/data &amp; more/.beads", "<key>KeepAlive</key>", "<key>RunAtLoad</key>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	// Stopping a launchd server disables it (so it does not return at login) and
	// unloads it; undoing the journal enables and loads it again, in that order.
	j, err := openJournal(filepath.Join(home, "j"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.stop(context.Background(), svc, freePort(t), true, nil, j); err != nil {
		t.Fatal(err)
	}
	if _, err := j.undo(m); err != nil {
		t.Fatal(err)
	}
	uid := m.domain()
	want := []string{
		"launchctl disable " + uid + "/com.brain.dolt-server",
		"launchctl bootout " + uid + "/com.brain.dolt-server",
		"launchctl enable " + uid + "/com.brain.dolt-server",
		"launchctl bootstrap " + uid + " " + svc.Plist,
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("launchctl calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	if validLaunchdLabel("bad label;rm") || !validLaunchdLabel("com.pai.brain-sync") {
		t.Error("label validation")
	}
	// An externally managed server is never stopped by brain.
	if err := m.stop(context.Background(), hostService{Kind: "external"}, 1, true, nil, nil); err == nil {
		t.Error("brain must refuse to stop a server it does not manage")
	}
}

func TestPeerActivateInstallsRenderJobIdempotently(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			_ = c.Close()
		}
	}()
	var calls []string
	svc := &serviceManager{home: home, run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}}
	env := &peerEnv{home: home, svc: svc}
	const id = "promote-render-job-1"
	st := &incomingState{ID: id, ServerStarted: true, ServerPort: ln.Addr().(*net.TCPAddr).Port}
	if err := os.MkdirAll(env.incomingDir(id), 0o755); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	if err := env.saveIncoming(st); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(home, pollerPlistDirPath, renderJobLabel+".plist")
	for round := 1; round <= 2; round++ {
		calls = nil
		if _, err := env.activate(context.Background(), peerActivateReq{ID: id, Generation: 2}); err != nil {
			t.Fatalf("activate round %d: %v", round, err)
		}
		if _, err := os.Stat(plist); err != nil {
			t.Fatalf("round %d: render job plist not written: %v", round, err)
		}
		cfg, err := loadHostConfig(home)
		if err != nil || cfg == nil || cfg.Role != roleHost {
			t.Fatalf("round %d: host.yaml = %+v, %v", round, cfg, err)
		}
		n := 0
		for _, s := range cfg.Services {
			if s == renderJobLabel {
				n++
			}
		}
		if n != 1 {
			t.Errorf("round %d: services %v should declare %s exactly once", round, cfg.Services, renderJobLabel)
		}
		loaded := false
		for _, c := range calls {
			if strings.HasPrefix(c, "launchctl bootstrap") && strings.HasSuffix(c, plist) {
				loaded = true
			}
		}
		if !loaded {
			t.Errorf("round %d: render job was not loaded; calls: %v", round, calls)
		}
	}
}
