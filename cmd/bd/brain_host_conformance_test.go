package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
)

const pinnedLoopbackWrapper = "#!/bin/sh\nexport BEADS_DOLT_SERVER_HOST=127.0.0.1\nexport BEADS_DOLT_SERVER_PORT=3307\nexport BEADS_DIR=$HOME/x/.beads\nexec bd \"$@\"\n"

func pinsByPath(pins []hostPin) map[string]hostPin {
	m := map[string]hostPin{}
	for _, p := range pins {
		m[p.Path] = p
	}
	return m
}

func conformanceItems(d []driftItem, kind string) []driftItem {
	var out []driftItem
	for _, x := range d {
		if x.Kind == kind {
			out = append(out, x)
		}
	}
	return out
}

// Finding 1: a wrapper that sets the host unconditionally, including a copy in
// an agent bin directory, is found; a wrapper that reads the shared file is not.
func TestScanHostWrappersFindsUnconditionalPinsInAgentBinCopies(t *testing.T) {
	home := t.TempDir()
	local := filepath.Join(home, ".local/bin")
	agent := filepath.Join(home, ".pi/agent/bin")
	mustWrite(t, filepath.Join(local, "brain"), pinnedLoopbackWrapper, 0o755)
	// A copy (not a link) in the agent dir, pinned the same way.
	mustWrite(t, filepath.Join(agent, "chores"), pinnedLoopbackWrapper, 0o755)
	mustWrite(t, filepath.Join(local, "chores"), strings.Replace(generatedHostBlockWrapper, "{BLOCK}", sharedServerHostBlock, 1), 0o755)
	// A link to a wrapper already scanned is the same file, not a second finding.
	if err := os.Symlink(filepath.Join(local, "brain"), filepath.Join(agent, "brain")); err != nil {
		t.Fatal(err)
	}
	// The conforming hand-written pattern: defaults, then source the shared file.
	conforming := "#!/bin/sh\nBEADS_DOLT_SERVER_HOST=127.0.0.1\nBEADS_DOLT_SERVER_PORT=3307\n. \"$HOME/.config/brain/store-host.env\"\nexport BEADS_DOLT_SERVER_HOST BEADS_DOLT_SERVER_PORT\nexec bd \"$@\"\n"
	mustWrite(t, filepath.Join(local, "inbox"), conforming, 0o755)
	// A pin AFTER the source overrides the file again.
	overridden := "#!/bin/sh\n. \"$HOME/.config/brain/store-host.env\"\nexport BEADS_DOLT_SERVER_HOST=127.0.0.1\nexec bd \"$@\"\n"
	mustWrite(t, filepath.Join(local, "review"), overridden, 0o755)
	// Stale backups are not active wrappers.
	mustWrite(t, filepath.Join(agent, "chores.bak-repoint-1"), pinnedLoopbackWrapper, 0o755)

	s := scanHostWrappers(home)
	got := pinsByPath(s.Pins)
	for _, want := range []string{filepath.Join(local, "brain"), filepath.Join(agent, "chores"), filepath.Join(local, "review")} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing pin in %s (all: %v)", want, got)
		}
	}
	for _, not := range []string{filepath.Join(local, "inbox"), filepath.Join(local, "chores"), filepath.Join(agent, "brain"), filepath.Join(agent, "chores.bak-repoint-1")} {
		if _, ok := got[not]; ok {
			t.Errorf("%s must not be reported as a pin", not)
		}
	}
	if p := got[filepath.Join(agent, "chores")]; !p.Agent || p.LinkTo != filepath.Join(local, "chores") {
		t.Errorf("agent copy = %+v, want Agent with LinkTo the ~/.local/bin wrapper", p)
	}
	if s.AgentCopies != 1 {
		t.Errorf("agent copies = %d, want 1 (the link is not a copy)", s.AgentCopies)
	}
}

// The pin findings point at the existing repair, or at a link for agent copies,
// and an agreeing pin is still a finding.
func TestClassifyHostAuditReportsAgreeingWrapperPinsWithAFix(t *testing.T) {
	o := fleetAuditOptions{Live: liveMini0, MainSHA: "aaaaaaaaaaaa"}
	a := &hostAudit{Stamp: stamped(), StoreHost: storeHostView{Present: true, Host: liveMini0.Host, Port: liveMini0.Port},
		Pins: []hostPin{
			{Name: "task", Path: "/h/.local/bin/task", Source: "wrapper", Host: liveMini0.Host, Port: liveMini0.Port, Line: "export BEADS_DOLT_SERVER_HOST=100.84.99.99"},
			{Name: "chores", Path: "/h/.pi/agent/bin/chores", Source: "wrapper", Agent: true, LinkTo: "/h/.local/bin/chores", Host: liveMini0.Host, Port: liveMini0.Port},
			{Name: "orphan", Path: "/h/.pi/agent/bin/orphan", Source: "wrapper", Agent: true, Host: liveMini0.Host},
		}, Stores: []storeAudit{okStore("task")}}
	d, _ := classifyHostAudit(fleetMember{Name: "m"}, a, o)
	if kinds(d) != "pin,pin,pin" {
		t.Fatalf("drift = %+v", d)
	}
	if !d[0].Repairable || !strings.Contains(d[0].Fix, "brain stores rewrite-wrappers") {
		t.Errorf("a ~/.local/bin pin is fixed by rewrite-wrappers: %+v", d[0])
	}
	if d[1].Repairable || d[1].Fix != "replace the copy with a link to the store wrapper (rewrite-wrappers only repairs ~/.local/bin): ln -sf /h/.local/bin/chores /h/.pi/agent/bin/chores" {
		t.Errorf("agent copy fix = %+v", d[1])
	}
	if d[2].Repairable || !strings.Contains(d[2].Fix, "remove /h/.pi/agent/bin/orphan") {
		t.Errorf("agent copy with no counterpart: %+v", d[2])
	}
}

// rewrite-wrappers repairs what the check flags and leaves the conforming pattern.
func TestRewriteWrappersFixesWhatTheUnconditionalPinCheckFlags(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".local/bin/brain")
	mustWrite(t, path, pinnedLoopbackWrapper, 0o755)
	if len(scanHostWrappers(home).Pins) != 2 {
		t.Fatalf("setup: want the host and port pins found")
	}
	if _, err := rewriteWrapperInPlace(path, true); err != nil {
		t.Fatal(err)
	}
	if pins := scanHostWrappers(home).Pins; len(pins) != 0 {
		t.Errorf("rewrite-wrappers left pins the check still flags: %+v", pins)
	}
}

// Finding 2: store-host.env naming a loopback address with no Dolt server on
// this machine is a relay.
func TestClassifyHostAuditReportsRelayStoreHost(t *testing.T) {
	loop := storeHostView{Path: "/h/store-host.env", Present: true, Host: "127.0.0.1", Port: 3307}
	o := fleetAuditOptions{Live: hostAddr{Host: "127.0.0.1", Port: 3307}, MainSHA: "aaaaaaaaaaaa"}
	run := func(m fleetMember, a *hostAudit, o fleetAuditOptions) []driftItem {
		a.Stamp, a.Stores = stamped(), []storeAudit{okStore("t")}
		d, _ := classifyHostAudit(m, a, o)
		return d
	}

	// mini1's case: the orchestrator's own store-host.env is the "live host", so
	// only the relay check can see it. The scan found no Dolt on this machine.
	d := run(fleetMember{Name: "mini1"}, &hostAudit{StoreHost: loop, ServerScan: true, Servers: []localServer{{Port: 9999}}}, o)
	items := conformanceItems(d, driftRelay)
	if len(items) != 1 || !strings.Contains(items[0].Detail, "relay") || !strings.Contains(items[0].Fix, "stop the relay") || !strings.Contains(items[0].Fix, "rewrite-wrappers") {
		t.Fatalf("relay finding = %+v", d)
	}
	// With a known live host the fix names it.
	o2 := fleetAuditOptions{Live: liveMini0, MainSHA: "aaaaaaaaaaaa"}
	d = run(fleetMember{Name: "mini1"}, &hostAudit{StoreHost: loop, ServerScan: true}, o2)
	if kinds(d) != "store-host" {
		t.Errorf("the existing store-host mismatch item already says it; no second relay item: %+v", d)
	}
	// The Dolt host itself, a roster-declared forwarder, and a scan that did not
	// run are not relays.
	if d := run(fleetMember{Name: "mini0"}, &hostAudit{StoreHost: loop, ServerScan: true, Servers: []localServer{{Port: 3307}}}, o); len(conformanceItems(d, driftRelay)) != 0 {
		t.Errorf("a machine running Dolt on the port is not a relay: %+v", d)
	}
	if d := run(fleetMember{Name: "mini3", StoreHost: "127.0.0.1:3307"}, &hostAudit{StoreHost: loop, ServerScan: true}, o); len(conformanceItems(d, driftRelay)) != 0 {
		t.Errorf("a declared forwarder is not reported: %+v", d)
	}
	if d := run(fleetMember{Name: "mini1"}, &hostAudit{StoreHost: loop}, o); len(conformanceItems(d, driftRelay)) != 0 {
		t.Errorf("unknown servers must not be guessed at: %+v", d)
	}
}

// Finding 2, fleet level: hosts that do not resolve to the same store host.
func TestFleetStoreHostDisagreement(t *testing.T) {
	roster := []fleetMember{{Name: "mini0"}, {Name: "mini1"}, {Name: "macbook"}, {Name: "mini3", StoreHost: "127.0.0.1:3307"}}
	host := func(h string, servers ...localServer) fleetHostResult {
		return fleetHostResult{Audit: &hostAudit{ServerScan: true, Servers: servers, StoreHost: storeHostView{Present: true, Host: h, Port: 3307}}}
	}
	results := []fleetHostResult{
		{Name: "mini0", Audit: host("127.0.0.1", localServer{Port: 3307}).Audit}, // runs Dolt: loopback is itself
		{Name: "mini1", Audit: host("127.0.0.1").Audit},                          // relay
		{Name: "macbook", Audit: host("100.84.99.99").Audit},
		{Name: "mini3", Audit: host("127.0.0.1").Audit}, // declared forwarder
	}
	d := fleetStoreHostDisagreement(roster, results)
	if d == nil || d.Kind != driftHostsDisagree || !strings.Contains(d.Detail, "mini1=127.0.0.1:3307") || !strings.Contains(d.Detail, "macbook=100.84.99.99:3307") || strings.Contains(d.Detail, "mini0") || strings.Contains(d.Detail, "mini3") {
		t.Fatalf("disagreement = %+v", d)
	}
	results[1] = fleetHostResult{Name: "mini1", Audit: host("100.84.99.99").Audit}
	if d := fleetStoreHostDisagreement(roster, results); d != nil {
		t.Errorf("agreeing hosts must not be reported: %+v", d)
	}
}

// Finding 3: no credential entry for the host:port the setting resolves to.
func TestClassifyHostAuditReportsMissingCredentialEntry(t *testing.T) {
	o := fleetAuditOptions{Live: liveMini0, MainSHA: "aaaaaaaaaaaa"}
	sh := storeHostView{Path: "/h/store-host.env", Present: true, Host: liveMini0.Host, Port: liveMini0.Port}
	run := func(c *credentialFact, stores ...storeAudit) []driftItem {
		if stores == nil {
			stores = []storeAudit{okStore("t")}
		}
		d, _ := classifyHostAudit(fleetMember{Name: "mini1"}, &hostAudit{Stamp: stamped(), StoreHost: sh, Creds: c, Stores: stores}, o)
		return d
	}
	d := run(&credentialFact{Path: "/h/.config/beads/credentials", FileExists: true, Sections: []string{"127.0.0.1:3307"}})
	if kinds(d) != "credentials" || !strings.Contains(d[0].Detail, "[127.0.0.1:3307]") || !strings.Contains(d[0].Fix, "[100.84.99.99:3307]") {
		t.Fatalf("credentials finding = %+v", d)
	}
	if strings.Contains(d[0].Fix, "password=secret") {
		t.Error("never print a password")
	}
	for name, c := range map[string]*credentialFact{
		"entry present":   {FileExists: true, Sections: []string{"100.84.99.99:3307"}, Entry: true},
		"password in env": {FileExists: true, Sections: []string{"127.0.0.1:3307"}, EnvPassword: true},
		"no file at all":  {},
	} {
		if d := run(c); len(d) != 0 {
			t.Errorf("%s: unexpected drift %+v", name, d)
		}
	}
	// No file and reads failing: the missing entry is the probable cause.
	if d := run(&credentialFact{Path: "/h/c"}, storeAudit{Name: "t", Read: probeResult{Status: "fail", Reason: "1045"}}); len(conformanceItems(d, driftCredentials)) != 1 {
		t.Errorf("failing reads with no credentials entry: %+v", d)
	}
}

func TestReadCredentialFactNamesSectionsNeverPasswords(t *testing.T) {
	dir := t.TempDir()
	cred := filepath.Join(dir, "credentials")
	mustWrite(t, cred, "[127.0.0.1:3307]\npassword=hunter2\n", 0o600)
	t.Setenv("BEADS_CREDENTIALS_FILE", cred)
	t.Setenv("BEADS_DOLT_PASSWORD", "")
	f := readCredentialFact(storeHostView{Path: filepath.Join(dir, "store-host.env"), Present: true, Host: "100.84.99.99", Port: 3307})
	if f.Entry || !f.FileExists || len(f.Sections) != 1 {
		t.Fatalf("fact = %+v", f)
	}
	f = readCredentialFact(storeHostView{Present: true, Host: "127.0.0.1", Port: 3307})
	if !f.Entry {
		t.Errorf("the loopback entry exists: %+v", f)
	}
	// A password defined by store-host.env itself needs no credentials entry.
	shFile := filepath.Join(dir, "store-host.env")
	mustWrite(t, shFile, "BEADS_DOLT_PASSWORD=pw\n", 0o600)
	if f := readCredentialFact(storeHostView{Path: shFile, Present: true, Host: "100.84.99.99", Port: 3307}); !f.EnvPassword {
		t.Errorf("password in store-host.env: %+v", f)
	}
}

// Finding 4: a wrapper whose BEADS_DIR is not on this host.
func TestScanHostWrappersReportsMissingBeadsDir(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, "present/.beads/x"), "x", 0o644)
	local := filepath.Join(home, ".local/bin")
	agent := filepath.Join(home, ".pi/agent/bin")
	old := "#!/bin/sh\nexport BEADS_DIR=/Users/trilliumsmith/code/status/.beads\nexec bd \"$@\"\n"
	mustWrite(t, filepath.Join(local, "status"), "#!/bin/sh\nexport BEADS_DIR=$HOME/code/status/.beads\nexec bd \"$@\"\n", 0o755)
	mustWrite(t, filepath.Join(local, "chores"), "#!/bin/sh\nexport BEADS_DIR=\"$HOME/present/.beads\"\nexec bd \"$@\"\n", 0o755)
	mustWrite(t, filepath.Join(local, "dyn"), "#!/bin/sh\nexport BEADS_DIR=\"$(pwd)/.beads\"\nexec bd \"$@\"\n", 0o755)
	mustWrite(t, filepath.Join(agent, "workflows"), old, 0o755)
	mustWrite(t, filepath.Join(local, "workflows"), old, 0o755)

	s := scanHostWrappers(home)
	got := map[string]missingBeadsDir{}
	for _, m := range s.MissingDirs {
		got[m.Path] = m
	}
	if len(got) != 3 {
		t.Fatalf("missing dirs = %+v, want status, local workflows and agent workflows only", s.MissingDirs)
	}
	if m := got[filepath.Join(agent, "workflows")]; !m.Agent || m.LinkTo != filepath.Join(local, "workflows") || m.BeadsDir != "/Users/trilliumsmith/code/status/.beads" {
		t.Errorf("agent copy = %+v", m)
	}

	a := &hostAudit{MissingDirs: s.MissingDirs}
	d := classifyConformance(fleetMember{Name: "mini1"}, a, hostAddr{}, hostAddr{}, false)
	if len(d) != 3 {
		t.Fatalf("items = %+v", d)
	}
	byPath := map[string]driftItem{}
	for _, x := range d {
		byPath[x.Subject] = x
	}
	if f := byPath[filepath.Join(agent, "workflows")].Fix; !strings.HasPrefix(f, "replace the copy with a link") {
		t.Errorf("agent fix = %q", f)
	}
	if f := byPath[filepath.Join(local, "workflows")].Fix; !strings.Contains(f, "brain stores rewrite-wrappers") {
		t.Errorf("foreign-home fix = %q", f)
	}
	if f := byPath[filepath.Join(local, "status")].Fix; !strings.Contains(f, "restore the store directory") {
		t.Errorf("gone-store fix = %q", f)
	}
}

// Finding 5: metadata.json naming a different host than store-host.env.
func TestClassifyHostAuditGroupsMetadataHostDisagreement(t *testing.T) {
	o := fleetAuditOptions{Live: liveMini0, MainSHA: "aaaaaaaaaaaa"}
	sh := storeHostView{Path: "/h/store-host.env", Present: true, Host: liveMini0.Host, Port: liveMini0.Port}
	pins := []metadataPin{
		{Store: "a", BeadsDir: "/h/a/.beads", Host: "127.0.0.1", Port: 3307},
		{Store: "b", BeadsDir: "/h/b/.beads", Host: "127.0.0.1", Port: 3307},
		{Store: "c", BeadsDir: "/h/c/.beads", Host: "100.84.99.99", Port: 3307}, // agrees
		{Store: "d", BeadsDir: "/h/d/.beads", Port: 3399},                       // port only
	}
	d, _ := classifyHostAudit(fleetMember{Name: "mini1"}, &hostAudit{Stamp: stamped(), StoreHost: sh, MetaPins: pins, Stores: []storeAudit{okStore("a")}}, o)
	items := conformanceItems(d, driftMetadataHost)
	if len(items) != 2 {
		t.Fatalf("items = %+v", d)
	}
	var loop driftItem
	for _, x := range items {
		if strings.Contains(x.Detail, "127.0.0.1:3307") {
			loop = x
		}
	}
	if strings.Join(loop.Subjects, ",") != "a,b" || !strings.Contains(loop.Fix, "BEADS_DIR=/h/a/.beads bd dolt set host 100.84.99.99") || !strings.Contains(loop.Fix, "bd dolt set port 3307") {
		t.Errorf("loopback group = %+v", loop)
	}
	// On the machine that runs Dolt a loopback metadata host is the same server.
	d, _ = classifyHostAudit(fleetMember{Name: "mini0"}, &hostAudit{Stamp: stamped(), StoreHost: sh, ServerScan: true, Servers: []localServer{{Port: 3307}},
		MetaPins: pins[:2], Stores: []storeAudit{okStore("a")}}, o)
	if len(d) != 0 {
		t.Errorf("loopback metadata on the Dolt host drifted: %+v", d)
	}
}

func TestMetadataServerPinAgainstWarnsOnlyWhenItDisagrees(t *testing.T) {
	home := isolatedHome(t)
	beadsDir := filepath.Join(home, "data", "task", ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeWrapperForStore(t, home, "task", beadsDir)
	if err := (&configfile.Config{DoltServerHost: "127.0.0.1", DoltServerPort: 3307}).Save(beadsDir); err != nil {
		t.Fatal(err)
	}
	entry := storeEntry{Path: beadsDir}
	mini0 := storeHostView{Present: true, Host: "100.84.99.99", Port: 3307}
	status, reason := metadataServerPinAgainst("task", entry, mini0, nil)
	if status != "warn" || !strings.Contains(reason, "bd dolt set host 100.84.99.99") {
		t.Fatalf("status=%q reason=%q", status, reason)
	}
	if status, _ := metadataServerPinAgainst("task", entry, storeHostView{Present: true, Host: "127.0.0.1", Port: 3307}, nil); status != "info" {
		t.Errorf("an agreeing pin stays informational, got %q", status)
	}
	if status, _ := metadataServerPinAgainst("task", entry, mini0, &hostAudit{ServerScan: true, Servers: []localServer{{Port: 3307}}}); status != "info" {
		t.Errorf("loopback on the Dolt host stays informational, got %q", status)
	}
	if status, _ := metadataServerPinAgainst("task", entry, storeHostView{}, nil); status != "info" {
		t.Errorf("without a store-host.env to compare with it stays informational, got %q", status)
	}
}

// The single-host doctor reports the mini1 state end to end.
func TestLocalHostFindingsReproducesTheMini1State(t *testing.T) {
	home := isolatedHome(t)
	t.Setenv("BEADS_CREDENTIALS_FILE", filepath.Join(home, "credentials"))
	t.Setenv("BEADS_DOLT_PASSWORD", "")
	t.Setenv(configfile.StoreHostKeyHost, "")
	mustWrite(t, filepath.Join(home, "credentials"), "[127.0.0.1:3307]\npassword=x\n", 0o600)
	shFile := filepath.Join(home, ".config/brain/store-host.env")
	mustWrite(t, shFile, "BEADS_DOLT_SERVER_HOST=127.0.0.1\nBEADS_DOLT_SERVER_PORT=3307\n", 0o644)
	mustWrite(t, filepath.Join(home, "x/.beads/metadata.json"), "{}", 0o644)
	mustWrite(t, filepath.Join(home, ".local/bin/brain"), pinnedLoopbackWrapper, 0o755)
	mustWrite(t, filepath.Join(home, ".pi/agent/bin/status"), "#!/bin/sh\nexport BEADS_DIR=/Users/gone/status/.beads\nexec bd \"$@\"\n", 0o755)

	view := readStoreHostView(home)
	d := localHostFindings(home, view, &hostAudit{ServerScan: true}, nil)
	got := map[string]int{}
	for _, x := range d {
		got[x.Kind]++
		if x.Fix == "" {
			t.Errorf("%s finding has no fix command: %+v", x.Kind, x)
		}
	}
	// brain pins host and port (two lines, two items); the relay, the missing
	// directory. No credentials finding: the loopback entry exists and the
	// setting still says loopback.
	if got[driftPin] != 2 || got[driftRelay] != 1 || got[driftBeadsDir] != 1 || got[driftCredentials] != 0 {
		t.Errorf("findings by kind = %v\n%+v", got, d)
	}

	var out bytes.Buffer
	printHostFindings(&out, d)
	text := out.String()
	for _, want := range []string{"WARN host relay  " + shFile, "WARN host pin  " + filepath.Join(home, ".local/bin/brain"),
		"WARN host wrapper-beads-dir  " + filepath.Join(home, ".pi/agent/bin/status"), "fix: brain stores rewrite-wrappers"} {
		if !strings.Contains(text, want) {
			t.Errorf("text output lacks %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "\n     fix: ") != len(d) {
		t.Errorf("every finding needs its fix line:\n%s", text)
	}

	// Without a process scan, nothing says this machine runs no Dolt server.
	if got := conformanceItems(localHostFindings(home, view, &hostAudit{}, nil), driftRelay); len(got) != 0 {
		t.Errorf("relay reported without a server scan: %+v", got)
	}

	// Pointing the setting at the real host without a credential entry is the
	// follow-on failure the doctor must warn about before it happens.
	mustWrite(t, shFile, "BEADS_DOLT_SERVER_HOST=100.84.99.99\nBEADS_DOLT_SERVER_PORT=3307\n", 0o644)
	d = localHostFindings(home, readStoreHostView(home), &hostAudit{ServerScan: true}, nil)
	got = map[string]int{}
	for _, x := range d {
		got[x.Kind]++
	}
	if got[driftCredentials] != 1 || got[driftRelay] != 0 {
		t.Errorf("after repointing: %v\n%+v", got, d)
	}
	// Nothing is reported when store-host.env is absent (reads fail loudly then).
	if d := localHostFindings(home, storeHostView{}, &hostAudit{ServerScan: true}, nil); d != nil {
		t.Errorf("no store-host.env: %+v", d)
	}
}

// A literal default followed by a guarded, one-line or variable sourcing of
// store-host.env conforms: the file overrides it.
func TestScanWrapperPinsSourcedForms(t *testing.T) {
	const pin = "#!/bin/sh\nexport BEADS_DOLT_SERVER_HOST=127.0.0.1\nexport BEADS_DOLT_SERVER_PORT=3307\n"
	for name, src := range map[string]string{
		"plain":    `. "$HOME/.config/brain/store-host.env"`,
		"guarded":  `[ -f "$HOME/.config/brain/store-host.env" ] && . "$HOME/.config/brain/store-host.env"`,
		"one-line": `if [ -f ~/.config/brain/store-host.env ]; then . ~/.config/brain/store-host.env; fi`,
		"variable": "f=\"$HOME/.config/brain/store-host.env\"\n[ -r \"$f\" ] && . \"$f\"",
	} {
		if got := scanWrapperPins(pin + src + "\nexec bd \"$@\"\n"); len(got) != 0 {
			t.Errorf("%s: conforming wrapper reported as pinned: %+v", name, got)
		}
	}
	for name, src := range map[string]string{
		"commented": `# . "$HOME/.config/brain/store-host.env"`,
		"other":     `[ -f "$HOME/.config/brain/store-host.env" ] && . "$HOME/other.env"`,
	} {
		if got := scanWrapperPins(pin + src + "\nexec bd \"$@\"\n"); len(got) != 2 {
			t.Errorf("%s: want both pins reported, got %+v", name, got)
		}
	}
}

// A metadata.json naming only a loopback host on the Dolt host itself uses the
// store-host.env port, which the local server listens on.
func TestMetaPinAgreesHostOnlyOnDoltHost(t *testing.T) {
	want := hostAddr{Host: "100.84.99.99", Port: 3307}
	p := metadataPin{Store: "task", Host: "127.0.0.1"}
	if !metaPinAgrees(&hostAudit{ServerScan: true, Servers: []localServer{{Port: 3307}}}, p, want) {
		t.Error("host-only loopback pin on the Dolt host must agree")
	}
	if metaPinAgrees(&hostAudit{ServerScan: true, Servers: []localServer{{Port: 3308}}}, p, want) {
		t.Error("a local server on another port is not the store host")
	}
	if metaPinAgrees(&hostAudit{ServerScan: true, Servers: []localServer{{Port: 3307}}}, metadataPin{Host: "127.0.0.1", Port: 3308}, want) {
		t.Error("a different metadata port must not agree")
	}
}
