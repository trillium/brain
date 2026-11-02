package main

// Host conformance: the facts and checks that tell whether this machine really
// reads its store host from the one shared setting (store-host.env) or only
// happens to agree with it today. They feed 'brain stores host-audit', so both
// 'stores doctor --fleet' and the single-host 'stores doctor' report them, each
// with a concrete fix command. Nothing here edits credentials, stops a service
// or writes store-host.env; wrapper pins are repaired by the existing
// 'brain stores rewrite-wrappers'.
//
// The class of failure (mini1, 2026-11-02): store-host.env named a local relay
// (127.0.0.1:3307) that forwarded to the real store host, wrappers baked the
// same loopback pin and never read the file, wrapper copies in ~/.pi/agent/bin
// did the same, the credentials file only knew the loopback address, a few
// wrappers pointed at a home that was gone, and 65 stores' metadata.json named
// 127.0.0.1 for every caller that runs bd with BEADS_DIR directly. Everything
// agreed with everything else, so nothing reported it, until the relay stopped.

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/steveyegge/beads/internal/configfile"
)

// Drift kinds added by the conformance checks.
const (
	driftRelay         = "relay"
	driftCredentials   = "credentials"
	driftBeadsDir      = "wrapper-beads-dir"
	driftMetadataHost  = "metadata-host"
	driftHostsDisagree = "store-host-disagree"
)

// storeHostVarRe is a shell variable assigned a path naming store-host.env.
var storeHostVarRe = regexp.MustCompile(`^\s*(?:export\s+|local\s+)?([A-Za-z_][A-Za-z0-9_]*)=\S*` + regexp.QuoteMeta(configfile.StoreHostFileName))

// agentBinDirs are the agent bin directories whose wrappers brain installs
// links into or copies from (see ensureAgentLink). Wrapper copies there are
// scanned like ~/.local/bin wrappers.
func agentBinDirs(home string) []string {
	return []string{filepath.Join(home, filepath.FromSlash(agentLinkDirPath))}
}

// missingBeadsDir is a wrapper whose BEADS_DIR does not exist on this machine.
type missingBeadsDir struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	BeadsDir string `json:"beads_dir"`
	Agent    bool   `json:"agent_copy,omitempty"`
	LinkTo   string `json:"link_to,omitempty"`
}

// metadataPin is a store whose metadata.json names its own Dolt host or port.
type metadataPin struct {
	Store    string `json:"store"`
	BeadsDir string `json:"beads_dir"`
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
}

// credentialFact says whether a password exists for the server store-host.env
// names. Passwords are never reported, only section names.
type credentialFact struct {
	Path        string   `json:"path"`
	FileExists  bool     `json:"file_exists"`
	Sections    []string `json:"sections,omitempty"`
	Entry       bool     `json:"entry"`
	EnvPassword bool     `json:"env_password,omitempty"`
}

// scanWrapperPins is scanPinsInBody for a store wrapper. A literal host/port
// assignment that a later sourcing of store-host.env follows (plain, guarded
// by a test, or through a variable naming the file) is only a default: the
// sourced file overrides it. The generated run-time block reads the process
// environment first, so with it a pre-set value wins and stays a pin.
func scanWrapperPins(body string) []hostPin {
	lastSource := -1
	fileVars := map[string]bool{}
	for i, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if m := storeHostVarRe.FindStringSubmatch(line); m != nil {
			fileVars[m[1]] = true
		}
		for _, m := range sourceLineRe.FindAllStringSubmatch(line, -1) {
			target := strings.Trim(m[1], `"'`)
			if strings.Contains(target, configfile.StoreHostFileName) || fileVars[strings.Trim(target, "${}")] {
				lastSource = i
			}
		}
	}
	generated := strings.Contains(body, "_want_host")
	var out []hostPin
	for i, line := range strings.Split(body, "\n") {
		if !bakedServerPinLine(line) {
			continue
		}
		if lastSource > i && !generated {
			continue
		}
		p := hostPin{Line: strings.TrimSpace(line)}
		if v, ok := lineAssignedValue(line, wrapperHostAssignRe); ok {
			p.Host = pinLiteral(v)
		}
		if v, ok := lineAssignedValue(line, wrapperPortAssignRe); ok {
			p.Port, _ = strconv.Atoi(pinLiteral(v))
		}
		out = append(out, p)
	}
	return out
}

// readCredentialFact looks up the credential entry for the host:port
// store-host.env names.
func readCredentialFact(sh storeHostView) *credentialFact {
	path := configfile.CredentialsFilePath()
	f := &credentialFact{Path: path}
	f.Sections, f.FileExists = configfile.CredentialSections(path)
	if sh.Host == "" || sh.Port == 0 {
		return f
	}
	key := hostAddr{Host: sh.Host, Port: sh.Port}.String()
	for _, s := range f.Sections {
		f.Entry = f.Entry || s == key
	}
	f.EnvPassword = os.Getenv("BEADS_DOLT_PASSWORD") != "" || fileDefinesKey(sh.Path, "BEADS_DOLT_PASSWORD")
	return f
}

func fileDefinesKey(path, key string) bool {
	f, err := os.Open(path) //nolint:gosec // our own config file
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimPrefix(strings.TrimSpace(sc.Text()), "export ")
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == key && strings.Trim(strings.TrimSpace(v), `"'`) != "" {
			return true
		}
	}
	return false
}

// storeMetadataPin reads the Dolt host/port a store's metadata.json names,
// looking where the doctor looks: the wrapper's own BEADS_DIR, else the
// registry path.
func storeMetadataPin(name string, entry storeEntry) (metadataPin, bool) {
	beadsDir := expandPath(entry.Path)
	if wrapper, ok := storeWrapperPath(name); ok {
		if pinned, ok := wrapperBeadsDir(wrapper); ok {
			beadsDir = pinned
		}
	}
	if beadsDir == "" {
		return metadataPin{}, false
	}
	cfg, err := configfile.Load(beadsDir)
	if err != nil || cfg == nil || (cfg.DoltServerHost == "" && cfg.DoltServerPort <= 0) {
		return metadataPin{}, false
	}
	return metadataPin{Store: name, BeadsDir: beadsDir, Host: cfg.DoltServerHost, Port: cfg.DoltServerPort}, true
}

func collectMetadataPins(_ string, stores map[string]storeEntry) []metadataPin {
	var out []metadataPin
	for _, name := range sortedKeys(stores) {
		if p, ok := storeMetadataPin(name, stores[name]); ok {
			out = append(out, p)
		}
	}
	return out
}

// localDoltOn reports whether a Dolt sql-server on this machine listens on port
// (a server started without a port flag, port 0, might be on any).
func (a *hostAudit) localDoltOn(port int) bool {
	for _, s := range a.Servers {
		if s.Port == port || s.Port == 0 {
			return true
		}
	}
	return false
}

// metadataPinFix is the command that points one store's metadata.json at the
// shared store host.
func metadataPinFix(dir string, want hostAddr) string {
	return fmt.Sprintf("BEADS_DIR=%s bd dolt set host %s && BEADS_DIR=%s bd dolt set port %d", dir, want.Host, dir, want.Port)
}

// classifyConformance holds the checks that stand on this machine's own facts.
// want is the host:port a valid store-host.env names (zero when the file is
// missing or unusable); exp the host the machine should use. storeHostDrifted
// is true when classifyHostAudit already reported store-host.env itself, so the
// relay finding does not repeat it.
func classifyConformance(m fleetMember, a *hostAudit, want, exp hostAddr, storeHostDrifted bool) (items []driftItem) {
	add := func(kind, subject, detail, fix string) {
		items = append(items, driftItem{Machine: m.Name, Kind: kind, Subject: subject, Detail: detail, Fix: fix})
	}
	// A loopback store host with no Dolt server on this machine is a relay.
	if want.Host != "" && hostIsLoopback(want.Host) && a.ServerScan && !a.localDoltOn(want.Port) && m.StoreHost == "" && !storeHostDrifted {
		fix := "point store-host.env at the machine that runs Dolt"
		if exp.Host != "" && !hostIsLoopback(exp.Host) {
			fix = "point store-host.env at " + exp.String()
		}
		fix += " (doctor never edits it), add a [host:port] entry for it to the credentials file, run 'brain stores rewrite-wrappers', then stop the relay; if a forwarder is intended, declare store_host: " +
			want.String() + " for " + m.Name + " in fleet.yaml"
		add(driftRelay, a.StoreHost.Path, fmt.Sprintf("store-host.env names %s but no Dolt server runs on %s: this machine reaches the store through a local relay/forwarder and stops working when it does", want, m.Name), fix)
	}

	for _, d := range a.MissingDirs {
		fix := "remove " + d.Path + ", or restore the store directory " + d.BeadsDir
		switch {
		case d.Agent && d.LinkTo != "":
			fix = "replace the copy with a link to the store wrapper: ln -sf " + d.LinkTo + " " + d.Path
		case !d.Agent && foreignHomeRe.MatchString(d.BeadsDir):
			fix = "brain stores rewrite-wrappers (makes the home path $HOME-relative); if the store is gone, remove " + d.Path
		}
		add(driftBeadsDir, d.Path, "wrapper's BEADS_DIR does not exist on this machine: "+d.BeadsDir, fix)
	}

	if want.Host == "" {
		return items // no usable store-host.env: the store-host item already says so
	}
	if c := a.Creds; c != nil && !c.Entry && !c.EnvPassword {
		anyReadFail := false
		for _, s := range a.Stores {
			anyReadFail = anyReadFail || s.Read.Status == "fail"
		}
		// A file that holds entries for other servers but none for this one is
		// the mismatch; an absent file is only a finding when reads fail.
		if len(c.Sections) > 0 || anyReadFail {
			have := "it has no entries"
			if len(c.Sections) > 0 {
				have = "it only has [" + strings.Join(c.Sections, "], [") + "]"
			}
			add(driftCredentials, c.Path, fmt.Sprintf("no credential entry for %s, the server store-host.env names (%s)", want, have),
				"add a ["+want.String()+"] section with password=... to "+c.Path+" (doctor never writes credentials); skip if that server takes no password")
		}
	}

	// A store's metadata.json is what every caller that runs bd with BEADS_DIR
	// and no wrapper follows (hooks, the bridge, launchd jobs).
	groups := map[string][]metadataPin{}
	for _, p := range a.MetaPins {
		if metaPinAgrees(a, p, want) {
			continue
		}
		groups[pinAddr(hostPin{Host: p.Host, Port: p.Port})] = append(groups[pinAddr(hostPin{Host: p.Host, Port: p.Port})], p)
	}
	for _, key := range sortedKeys(groups) {
		g := groups[key]
		var names, dirs []string
		for _, p := range g {
			names = append(names, p.Store)
			dirs = append(dirs, p.BeadsDir)
		}
		sample := strings.Join(names, " ")
		if len(names) > 5 {
			sample = strings.Join(names[:5], " ") + fmt.Sprintf(" …(+%d)", len(names)-5)
		}
		fix := metadataPinFix(dirs[0], want)
		if len(dirs) > 1 {
			fix += "   # repeat for each store (all named in --json subjects)"
		}
		add(driftMetadataHost, fmt.Sprintf("%d store(s)", len(g)),
			fmt.Sprintf("metadata.json of %d store(s) names %s but store-host.env says %s: %s. A bare bd run with BEADS_DIR (hooks, bridge, launchd jobs) follows metadata.json, so these break when %s stops answering", len(g), key, want, sample, key),
			fix)
		items[len(items)-1].Subjects = names
	}
	return items
}

// metaPinAgrees reports whether a store's metadata.json host:port is the same
// server as store-host.env: equal, or loopback on the machine that runs the
// Dolt server itself.
func metaPinAgrees(a *hostAudit, p metadataPin, want hostAddr) bool {
	port := p.Port
	if port == 0 {
		port = want.Port
	}
	portOK := port == want.Port
	hostOK := p.Host == "" || p.Host == want.Host ||
		(hostIsLoopback(p.Host) && hostIsLoopback(want.Host)) ||
		(hostIsLoopback(p.Host) && a.ServerScan && a.localDoltOn(port))
	return hostOK && portOK
}

// fleetStoreHostDisagreement reports hosts that do not resolve to the same
// store host. A machine that declares a forwarder in fleet.yaml, or that runs
// the Dolt server itself behind a loopback address, is the exception.
func fleetStoreHostDisagreement(roster []fleetMember, results []fleetHostResult) *driftItem {
	byAddr := map[string][]string{}
	for i, r := range results {
		if r.Audit == nil || i >= len(roster) || roster[i].StoreHost != "" {
			continue
		}
		sh := r.Audit.StoreHost
		if !sh.Present || sh.Error != "" {
			continue
		}
		if hostIsLoopback(sh.Host) && r.Audit.ServerScan && r.Audit.localDoltOn(sh.Port) {
			continue
		}
		key := hostAddr{Host: sh.Host, Port: sh.Port}.String()
		byAddr[key] = append(byAddr[key], r.Name)
	}
	if len(byAddr) < 2 {
		return nil
	}
	var parts []string
	for _, k := range sortedKeys(byAddr) {
		sort.Strings(byAddr[k])
		parts = append(parts, strings.Join(byAddr[k], ",")+"="+k)
	}
	return &driftItem{Machine: "fleet", Kind: driftHostsDisagree, Subject: "store-host.env",
		Detail: "machines do not resolve to the same store host: " + strings.Join(parts, "; "),
		Fix:    "make every store-host.env name the one store host (machines that reach it through a forwarder declare store_host in fleet.yaml); doctor never edits it"}
}

// localHostFindings runs the host conformance checks on this machine for the
// single-host 'stores doctor'. It reuses the fleet classification with this
// machine's own store-host.env as the expected host, drops what the doctor
// already reports another way (binary identity, per-store probes), and leaves
// metadata.json to the per-store lines. local carries the process scan. Nothing is reported when store-host.env
// is missing or unusable: the store reads fail loudly then.
func localHostFindings(home string, sh storeHostView, local *hostAudit, results []storeHealth) []driftItem {
	if !sh.Present || sh.Error != "" {
		return nil
	}
	scan := scanHostWrappers(home)
	a := &hostAudit{Machine: machineName(), StoreHost: sh, Servers: local.Servers, ServerScan: local.ServerScan,
		Pins: scan.Pins, WrapperCount: scan.Wrappers, AgentCopies: scan.AgentCopies, MissingDirs: scan.MissingDirs,
		Creds: readCredentialFact(sh), EnvHost: os.Getenv(configfile.StoreHostKeyHost)}
	for _, r := range results {
		st := probeResult{Status: "ok"}
		if r.Status == "fail" {
			st = probeResult{Status: "fail", Reason: r.Reason}
		}
		a.Stores = append(a.Stores, storeAudit{Name: r.Name, Read: st})
	}
	exp := hostAddr{Host: sh.Host, Port: sh.Port}
	drift, _ := classifyHostAudit(fleetMember{Name: a.Machine}, a, fleetAuditOptions{Live: exp})
	var out []driftItem
	for _, d := range drift {
		switch d.Kind {
		case driftPin, driftRelay, driftCredentials, driftBeadsDir, driftStoreHost:
			out = append(out, d)
		}
	}
	return out
}
