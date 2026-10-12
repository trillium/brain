// Package main — brain_host_plan.go
//
// The plan: everything 'brain host promote' will do, worked out without
// changing anything. It reads this machine (the layout) and asks the target
// (hello, probe), turns that into the exact files and paths for the other end,
// and lists every reason the promotion must refuse. '--dry-run' prints it; a
// real run computes it again before each start and each resume.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// promoteOptions are the knobs of 'brain host promote'. They are saved with the
// promotion so a resume repeats the original, not whatever flags were typed.
type promoteOptions struct {
	Target     string   `json:"target"`
	SSHOptions []string `json:"ssh_options,omitempty"`
	PeerBin    string   `json:"peer_bin,omitempty"`

	// Where the brain is served from on the new host.
	Bind           string `json:"bind,omitempty"`
	Port           int    `json:"port,omitempty"`
	RemotesAPIPort int    `json:"remotesapi_port,omitempty"`
	ServiceKind    string `json:"service,omitempty"` // auto | launchd | process | external
	ServiceLabel   string `json:"service_label,omitempty"`

	// What moves.
	ExtraDatabases []string `json:"extra_databases,omitempty"`
	IncludeLegacy  bool     `json:"include_legacy,omitempty"`
	Services       []string `json:"services,omitempty"`
	LinkDirs       []string `json:"link_dirs,omitempty"`
	SkipWrappers   []string `json:"skip_wrappers,omitempty"`
	NoDoltcfg      bool     `json:"no_doltcfg,omitempty"`

	// What happens to the old host and the clients.
	StopSourceServer bool     `json:"stop_source_server,omitempty"`
	Clients          []string `json:"clients,omitempty"`

	// Safety valves.
	MoveAside      bool `json:"move_aside,omitempty"`
	ReplaceStale   bool `json:"replace_stale,omitempty"`
	ForceWrappers  bool `json:"force_wrappers,omitempty"`
	AllowOlderDolt bool `json:"allow_older_dolt,omitempty"`
	NoSeed         bool `json:"no_seed,omitempty"`
	NoSmoke        bool `json:"no_smoke,omitempty"`

	// Where this machine's own server is, when it cannot be discovered.
	SourcePort    int    `json:"source_port,omitempty"`
	SourceDataDir string `json:"source_data_dir,omitempty"`

	// Run control (not part of the promotion's identity).
	DryRun    bool   `json:"-"`
	Yes       bool   `json:"-"`
	StopAfter string `json:"-"`
}

// storeMove is one store directory and where it goes.
type storeMove struct {
	Name      string
	SrcRoot   string
	DstRoot   string
	SrcBeads  string
	Metadata  string // rebased metadata.json for the new host
	MetaNotes []string
}

// promotePlan is the worked-out promotion.
type promotePlan struct {
	ID         string
	Generation int
	Source     struct{ Home, Hostname, Dolt, DoltVersion, Version string }
	Layout     *hostLayout
	Hello      *peerHelloResp
	Probe      *peerProbeResp

	TargetHome string
	DataDir    string
	Bind       string
	Port       int
	Service    string // kind that will be used
	StagingDir string

	DBBytes    map[string]int64
	StoreBytes int64
	Stores     []storeMove
	Registry   map[string]storeEntry
	Wrappers   []peerFile
	Helpers    []peerFile
	Links      []peerLinkRef
	Services   []peerServiceSpec
	Unmoved    []layoutService // candidate jobs the operator has not declared
	Clients    []string

	Blockers []string
	Warnings []string
	Notes    []string
}

func (pl *promotePlan) block(format string, a ...any) {
	pl.Blockers = append(pl.Blockers, fmt.Sprintf(format, a...))
}
func (pl *promotePlan) warn(format string, a ...any) {
	pl.Warnings = append(pl.Warnings, fmt.Sprintf(format, a...))
}
func (pl *promotePlan) note(format string, a ...any) {
	pl.Notes = append(pl.Notes, fmt.Sprintf(format, a...))
}

var execBdRE = regexp.MustCompile(`(?m)^(\s*exec\s+)bd(\s)`)

// wrapperMarkerPrefix tags host wrappers this tool wrote, so a later install
// can tell them from hand-made files.
const hostWrapperMarker = "# brain-host-wrapper:"

// moveWrapper turns a wrapper from the old host into the one for the new host.
func moveWrapper(w layoutFile, id, srcHome, dstHome string, srcPort, dstPort int, peerBin, bdOnPath string, pl *promotePlan) peerFile {
	text := rebaseWrapper(w.Content, srcHome, dstHome, srcPort, dstPort)
	// Wrappers end in 'exec bd'. That must be a brain build on the new host:
	// if the 'bd' it finds is not the binary that answered, name the binary.
	if peerBin != "" && !sameBinary(bdOnPath, peerBin) && execBdRE.MatchString(text) {
		text = execBdRE.ReplaceAllString(text, "${1}"+remoteQuote(peerBin)+"${2}")
	}
	if i := strings.IndexByte(text, '\n'); i >= 0 && strings.HasPrefix(text, "#!") {
		text = text[:i+1] + hostWrapperMarker + " moved by 'brain host promote " + id + "'; paths rebased, edit freely.\n" + text[i+1:]
	}
	dst, _ := rebasePath(w.Path, srcHome, dstHome)
	return peerFile{Path: dst, Content: text, Mode: w.Mode}
}

func sameBinary(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ra, ea := filepath.EvalSymlinks(a)
	rb, eb := filepath.EvalSymlinks(b)
	if ea != nil || eb != nil {
		return a == b
	}
	return ra == rb
}

// buildPlan works out the promotion. It changes nothing on either machine.
func (p *promoter) buildPlan(ctx context.Context) (*promotePlan, error) {
	opts := p.opts
	home := p.home
	pl := &promotePlan{ID: p.st.ID}

	layout, err := discoverLayout(ctx, home, opts)
	if err != nil {
		return nil, err
	}
	pl.Layout = layout
	pl.Warnings = append(pl.Warnings, layout.Warnings...)
	pl.Source.Home = home
	pl.Source.Hostname, _ = os.Hostname()
	pl.Source.Version = Version + "+brain." + BrainVersion
	_, pl.Source.DoltVersion, _ = doltBinary()

	host, err := loadHostConfig(home)
	if err != nil {
		return nil, err
	}
	srcGen := 1
	if host != nil {
		srcGen = host.Generation
		switch host.Role {
		case roleMoved:
			pl.block("this machine is no longer the brain host (it moved to %s); promote from the current host", host.MovedTo)
		case roleFrozen:
			if host.PromoteID != p.st.ID {
				pl.block("this host is frozen by promotion %s; finish or roll that back first", host.PromoteID)
			}
		}
	}
	if rc, rerr := loadRemoteConfig(); rerr == nil && rc.Host != "" {
		pl.block("this machine is a client of %s, not a brain host; run the promotion on the host", rc.Host)
	}

	// The target.
	conn := p.peer
	hello, err := conn.hello(ctx, opts.PeerBin, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot talk to the target: %w", err)
	}
	pl.Hello = hello
	pl.TargetHome = hello.Home
	if hello.MachineID == machineID(home) {
		pl.block("the target %s is this machine", opts.Target)
	}
	if hello.Version != pl.Source.Version {
		pl.warn("the target runs brain %s, this machine %s (the promotion protocol matches, so it can proceed)", hello.Version, pl.Source.Version)
	}
	if !hello.Rsync {
		pl.block("rsync is not installed on the target")
	}
	if _, rerr := exec.LookPath("rsync"); rerr != nil {
		pl.block("rsync is not installed on this machine")
	}
	switch {
	case hello.DoltPath == "":
		pl.block("dolt is not installed on the target")
	case pl.Source.DoltVersion != "" && hello.DoltVersion != "" && compareVersions(hello.DoltVersion, pl.Source.DoltVersion) < 0 && !opts.AllowOlderDolt:
		pl.block("the target's dolt (%s) is older than this machine's (%s): a database written by a newer dolt may not open (--allow-older-dolt to try anyway)", hello.DoltVersion, pl.Source.DoltVersion)
	}
	if hello.GenerationOrZero() >= srcGen {
		srcGen = hello.GenerationOrZero()
	}
	pl.Generation = srcGen + 1

	srcPort := layout.Server.Port
	dstHome := hello.Home
	var ok bool
	if pl.DataDir, ok = rebasePath(layout.Server.DataDir, home, dstHome); !ok {
		pl.block("the Dolt data directory %s is outside the home directory; promotion moves it only to the same place under the target's home", layout.Server.DataDir)
	}
	pl.Port = opts.Port
	if pl.Port == 0 {
		pl.Port = srcPort
	}
	pl.Bind = opts.Bind
	if pl.Bind == "" {
		pl.Bind = "127.0.0.1"
	}
	if layout.Server.Bind != "" && layout.Server.Bind != pl.Bind && !isLoopbackHost(layout.Server.Bind) {
		pl.warn("the old server listens on %s; the new one listens on %s. Clients reach the brain through ssh (client mode), not the database port (--bind to listen wider)", layout.Server.Bind, pl.Bind)
	}
	if layout.Server.RemotesAPIPort > 0 && opts.RemotesAPIPort == 0 {
		pl.warn("the old server also serves Dolt's remotesapi on port %d (used for dolt push/pull between machines); the new one will not — pass --remotesapi-port to serve it, and set DOLT_REMOTE_PASSWORD for it yourself", layout.Server.RemotesAPIPort)
	}
	pl.Service = opts.ServiceKind
	if pl.Service == "" || pl.Service == "auto" {
		pl.Service = "process"
		if hello.OS == "darwin" && hello.LaunchctlReady {
			pl.Service = "launchd"
		}
	}
	if pl.Service == "process" {
		pl.warn("the new server will be a background process, not a managed service: it does not restart after a reboot or logout (--service launchd on macOS)")
	}
	if pl.DataDir != "" {
		pl.StagingDir = filepath.Join(filepath.Dir(pl.DataDir), ".brain-promote-"+p.st.ID)
	}

	// Sizes and space.
	pl.DBBytes = map[string]int64{}
	var dbTotal int64
	for _, db := range layout.Databases {
		n := hostDirSize(filepath.Join(layout.Server.DataDir, db))
		pl.DBBytes[db] = n
		dbTotal += n
	}
	if need := dbTotal + dbTotal/5; freeBytes(home) < need {
		pl.block("not enough free space here for the backup staging: need about %s, have %s", humanBytes(need), humanBytes(freeBytes(home)))
	}

	// Stores, registry, wrappers, helpers, links.
	reg, err := loadStoresRegistry()
	if err != nil {
		return nil, err
	}
	pl.Registry = map[string]storeEntry{}
	for name, e := range reg {
		if np, moved := rebasePath(e.Path, home, dstHome); moved {
			e.Path = np
		}
		pl.Registry[name] = e
	}
	for _, s := range layout.Stores {
		final, moved := rebasePath(s.Root, home, dstHome)
		if !moved {
			pl.block("store %s lives outside the home directory (%s); promotion moves stores only under the target's home", s.Name, s.Root)
			continue
		}
		sm := storeMove{Name: s.Name, SrcRoot: s.Root, DstRoot: final, SrcBeads: s.BeadsDir}
		raw, rerr := os.ReadFile(filepath.Join(s.BeadsDir, "metadata.json")) //nolint:gosec // registered store
		if rerr == nil {
			out, notes, merr := rebaseMetadata(raw, home, dstHome, srcPort, pl.Port)
			if merr != nil {
				pl.block("store %s: %v", s.Name, merr)
			} else {
				sm.Metadata, sm.MetaNotes = string(out), notes
			}
		}
		pl.Stores = append(pl.Stores, sm)
		for _, n := range sm.MetaNotes {
			pl.warn("store %s: %s", s.Name, n)
		}
	}
	for _, s := range layout.Missing {
		pl.warn("store %s is registered but %s is not a store on this machine; only its registry entry moves", s.Name, s.BeadsDir)
	}
	for _, w := range layout.Wrappers {
		pl.Wrappers = append(pl.Wrappers, moveWrapper(w, p.st.ID, home, dstHome, srcPort, pl.Port, hello.PeerBin, hello.BdOnPath, pl))
	}
	if len(layout.Wrappers) > 0 && !sameBinary(hello.BdOnPath, hello.PeerBin) {
		pl.warn("on the target, 'bd' is %s, not the brain build (%s): the moved wrappers are pointed at the brain build directly",
			hostOrNone(hello.BdOnPath), hello.PeerBin)
	}
	for _, h := range layout.Helpers {
		dst, _ := rebasePath(h.Path, home, dstHome)
		pl.Helpers = append(pl.Helpers, peerFile{Path: dst, Content: rebaseText(h.Content, home, dstHome), Mode: h.Mode})
	}
	for _, l := range layout.Links {
		dst, _ := rebasePath(l.Path, home, dstHome)
		pl.Links = append(pl.Links, peerLinkRef{Path: dst, Target: rebaseText(l.Target, home, dstHome)})
	}
	if len(layout.Wrappers) == 0 {
		pl.warn("no store wrappers were found in %s: the new host will have the data but no store commands", layout.BinDir)
	}

	// Role-bound jobs the operator declared move; the rest are listed.
	for _, svc := range layout.Services {
		if !svc.Declared {
			pl.Unmoved = append(pl.Unmoved, svc)
			continue
		}
		spec, serr := planService(svc, home, dstHome, srcPort, pl.Port)
		if serr != nil {
			pl.block("service %s: %v", svc.Label, serr)
			continue
		}
		pl.Services = append(pl.Services, *spec)
	}
	for _, u := range pl.Unmoved {
		pl.note("launchd job %s (%s) is not part of the move; declare it with --service %s if it belongs to the host role", u.Label, u.Reason, u.Label)
	}
	if layout.DoltCfg && !opts.NoDoltcfg {
		pl.note("the Dolt user and privilege files (%s/.doltcfg) are copied with the data (--no-doltcfg to leave them)", layout.Server.DataDir)
	}
	if len(layout.Foreign) > 0 {
		pl.note("the old Dolt server also holds %d other database(s) that are not part of the brain and are not moved (e.g. %s); they stay on the old host", len(layout.Foreign), joinMax(layout.Foreign, 4))
	}

	// Clients.
	seen := map[string]bool{}
	if host != nil {
		for _, c := range host.Clients {
			if !seen[c.Target] {
				seen[c.Target] = true
				pl.Clients = append(pl.Clients, c.Target)
			}
		}
	}
	for _, c := range opts.Clients {
		if !seen[c] {
			seen[c] = true
			pl.Clients = append(pl.Clients, c)
		}
	}
	sort.Strings(pl.Clients)

	// Ask the target what it already holds.
	probe, err := p.probeTarget(ctx, pl)
	if err != nil {
		return nil, err
	}
	pl.Probe = probe
	p.evaluateProbe(pl)
	if pl.Service == "external" {
		pl.warn("service 'external': brain will not start the new server; start it yourself on port %d with data dir %s before the serve phase", pl.Port, pl.DataDir)
	}
	if opts.StopSourceServer && layout.Server.Service.Kind == "external" {
		pl.block("--stop-source-server: the old server is not managed by launchd or by brain (it was started by hand), so brain cannot stop it; stop it yourself or leave it running")
	}
	return pl, nil
}

func hostOrNone(s string) string {
	if s == "" {
		return "not found"
	}
	return s
}

func joinMax(list []string, n int) string {
	if len(list) <= n {
		return strings.Join(list, ", ")
	}
	return strings.Join(list[:n], ", ") + ", …"
}

// GenerationOrZero is the target's recorded generation (0 when it has none).
func (h *peerHelloResp) GenerationOrZero() int { return h.Generation }

// planService prepares a launchd job to move: its plist and the scripts it runs.
func planService(svc layoutService, srcHome, dstHome string, srcPort, dstPort int) (*peerServiceSpec, error) {
	xml, err := exec.Command("plutil", "-convert", "xml1", "-o", "-", svc.Plist).Output() //nolint:gosec // plist from a directory listing
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", svc.Plist, err)
	}
	dstPlist, _ := rebasePath(svc.Plist, srcHome, dstHome)
	spec := &peerServiceSpec{
		Label:    svc.Label,
		Plist:    peerFile{Path: dstPlist, Content: rebasePortPins(rebaseText(string(xml), srcHome, dstHome), srcPort, dstPort), Mode: 0o644},
		Disabled: svc.Disabled,
	}
	for _, f := range svc.Files {
		data, rerr := os.ReadFile(f) //nolint:gosec // file named in the job's own arguments
		if rerr != nil {
			continue
		}
		info, _ := os.Stat(f)
		mode := uint32(0o644)
		if info != nil {
			mode = uint32(info.Mode().Perm())
		}
		dst, _ := rebasePath(f, srcHome, dstHome)
		spec.Files = append(spec.Files, peerFile{Path: dst, Content: rebaseWrapper(string(data), srcHome, dstHome, srcPort, dstPort), Mode: mode})
	}
	return spec, nil
}

func (p *promoter) probeTarget(ctx context.Context, pl *promotePlan) (*peerProbeResp, error) {
	req := peerProbeReq{
		ID: p.st.ID, Source: machineID(p.home), DataDir: pl.DataDir, Port: pl.Port,
		Databases: pl.Layout.Databases,
	}
	for _, db := range pl.Layout.Databases {
		req.NeedBytes += pl.DBBytes[db]
	}
	req.NeedBytes = req.NeedBytes*2 + req.NeedBytes/2
	for _, s := range pl.Stores {
		req.StoreRoots = append(req.StoreRoots, s.DstRoot)
	}
	for _, w := range pl.Wrappers {
		req.Wrappers = append(req.Wrappers, peerFileRef{Path: w.Path, SHA: sha256Hex([]byte(w.Content))})
	}
	for _, h := range pl.Helpers {
		req.Wrappers = append(req.Wrappers, peerFileRef{Path: h.Path, SHA: sha256Hex([]byte(h.Content))})
	}
	req.Links = pl.Links
	for _, s := range pl.Services {
		req.Plists = append(req.Plists, peerFileRef{Path: s.Plist.Path, SHA: sha256Hex([]byte(s.Plist.Content))})
	}
	var resp peerProbeResp
	if err := p.peer.call(ctx, "probe", req, &resp); err != nil {
		return nil, fmt.Errorf("asking the target what it already holds: %w", err)
	}
	if resp.FreeBytes > 0 && resp.FreeBytes < req.NeedBytes {
		pl.block("not enough free space on the target: need about %s, it has %s", humanBytes(req.NeedBytes), humanBytes(resp.FreeBytes))
	}
	return &resp, nil
}

// evaluateProbe turns what the target holds into refusals and warnings. This is
// where "the target already holds a different live brain" is decided.
func (p *promoter) evaluateProbe(pl *promotePlan) {
	pr := pl.Probe
	srcID := machineID(p.home)
	staleOK := pr.Role == roleMoved && pr.MovedTo != "" && p.opts.ReplaceStale

	switch pr.Role {
	case roleHost, roleFrozen:
		if pr.PromoteID != p.st.ID {
			pl.block("the target is already a brain host (generation %d since promotion %s): it holds a different live brain; move or retire that one first", pr.Generation, pr.PromoteID)
		}
	case roleMoved:
		if pr.MovedTo != "" && !p.opts.ReplaceStale {
			pl.warn("the target used to be the host and moved to %s; it still holds that old copy", pr.MovedTo)
		}
	}
	if pr.ServerListening && !pr.ServerOurs {
		pl.block("something is already listening on port %d on the target and this promotion did not start it (a different Dolt server, possibly a different live brain)", pl.Port)
	}
	for _, db := range pr.DatabasesPresent {
		if containsStr(pr.DatabasesOurs, db) {
			continue
		}
		if staleOK && pr.OldServerPort > 0 {
			pl.block("the target's old Dolt server (port %d) is still running on the data that would be replaced; stop it first (promote with --stop-source-server next time, or stop that process yourself)", pr.OldServerPort)
			continue
		}
		if staleOK {
			pl.warn("the target already holds a database named %s from before it moved away; it will be renamed aside, not deleted (--replace-stale)", db)
			continue
		}
		pl.block("the target already has a database named %s in %s that this promotion did not create: it holds a different brain (if it is the old copy from before this machine gave up the host role, use --replace-stale)", db, pl.DataDir)
	}
	for _, r := range pr.StoreRootsPresent {
		if containsStr(pr.StoreRootsOurs, r) {
			continue
		}
		if p.opts.MoveAside || staleOK {
			pl.warn("%s already exists on the target; it will be renamed aside (not deleted)", r)
			continue
		}
		pl.block("%s already exists on the target and is not part of this promotion (--move-aside renames it out of the way)", r)
	}
	for _, w := range pr.WrapperConflicts {
		if p.opts.ForceWrappers {
			pl.warn("%s exists on the target and will be replaced (--force-wrappers); the original is kept in the undo journal", w)
		} else {
			pl.block("%s already exists on the target and was not generated by 'brain remote install' (--force-wrappers replaces it, --skip-wrapper NAME leaves it)", w)
		}
	}
	for _, l := range pr.LinkConflicts {
		if p.opts.ForceWrappers {
			pl.warn("%s exists on the target and will be replaced (--force-wrappers)", l)
		} else {
			pl.block("%s already exists on the target and points somewhere else (--force-wrappers replaces it)", l)
		}
	}
	for _, c := range pr.PlistConflicts {
		pl.block("%s already exists on the target with different content", c)
	}
	for _, r := range pr.RenameBlocked {
		pl.block("%s is on a different filesystem from the staging area on the target, so the staged copy cannot be moved into place atomically", r)
	}
	if pr.ClientOf != "" {
		pl.note("the target is currently a client of %s; it stops being one when the promotion activates", pr.ClientOf)
	}
	_ = srcID
}

// ---------------------------------------------------------------------------
// Printing
// ---------------------------------------------------------------------------

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// print writes the plan for people.
func (pl *promotePlan) print(w io.Writer, opts promoteOptions) {
	l := pl.Layout
	fmt.Fprintf(w, "Promotion %s: move the brain host role\n", pl.ID)
	fmt.Fprintf(w, "  from  this machine (%s)\n", pl.Source.Home)
	fmt.Fprintf(w, "  to    %s  (home %s, %s/%s, brain %s, dolt %s)\n", opts.Target, pl.TargetHome, pl.Hello.OS, pl.Hello.Arch, pl.Hello.Version, hostOrNone(pl.Hello.DoltVersion))
	fmt.Fprintf(w, "  generation %d → %d\n\n", pl.Generation-1, pl.Generation)

	fmt.Fprintf(w, "What moves\n")
	var dbTotal int64
	for _, db := range l.Databases {
		dbTotal += pl.DBBytes[db]
		fmt.Fprintf(w, "  database  %-24s %10s   copied with dolt backup, proved by hash\n", db, humanBytes(pl.DBBytes[db]))
	}
	fmt.Fprintf(w, "  server    %s:%d (data %s) → %s:%d (data %s), kept running as: %s\n",
		hostOrNone(l.Server.Bind), l.Server.Port, l.Server.DataDir, pl.Bind, pl.Port, pl.DataDir, pl.Service)
	fmt.Fprintf(w, "  stores    %d store directories (config + markdown mirrors), %d wrappers, %d scripts they source, %d agent links, registry + env\n",
		len(pl.Stores), len(pl.Wrappers), len(pl.Helpers), len(pl.Links))
	for _, s := range pl.Services {
		state := ""
		if s.Disabled {
			state = " (disabled here; stays disabled there)"
		}
		fmt.Fprintf(w, "  job       launchd %s%s\n", s.Label, state)
	}
	if len(pl.Clients) > 0 {
		fmt.Fprintf(w, "  clients   %s will be pointed at the new host\n", strings.Join(pl.Clients, ", "))
	}

	fmt.Fprintf(w, "\nWhat happens, in order\n")
	steps := []string{
		"preflight   check both machines; refuse if the target holds a different live brain",
		"seed        copy the database while writes continue (so the pause below is short)",
		"files       copy store directories and mirrors to the target's staging area",
		"freeze      pause writes on this machine; wait for in-flight commands; hash the database",
		"delta       copy what changed during the pause",
		"restore     rebuild the database on the target and prove its hash equals the frozen one",
		"serve       start the Dolt server on the target on the restored database",
		"parity      compare every store's rows (and `count`) between the two copies",
		"fence       re-hash this machine's database: nothing may have changed since the freeze",
		"flip        activate the target as host; turn this machine into a client of it",
		"clients     point the known clients at the new host",
		"verify      write-free end-to-end checks through the new host",
	}
	for i, s := range steps {
		fmt.Fprintf(w, "  %2d. %s\n", i+1, s)
	}
	fmt.Fprintf(w, "\nThe old host's data is never modified or deleted. Until you remove it yourself, 'brain host rollback' puts everything back.\n")
	if opts.StopSourceServer {
		fmt.Fprintf(w, "The old Dolt server will be stopped after the flip (--stop-source-server).\n")
	} else {
		fmt.Fprintf(w, "The old Dolt server keeps running, with the brain refusing writes on this machine (use --stop-source-server to stop it).\n")
	}

	if len(pl.Warnings) > 0 {
		fmt.Fprintf(w, "\nWarnings\n")
		for _, x := range pl.Warnings {
			fmt.Fprintf(w, "  ! %s\n", x)
		}
	}
	if len(pl.Notes) > 0 {
		fmt.Fprintf(w, "\nNotes\n")
		for _, x := range pl.Notes {
			fmt.Fprintf(w, "  - %s\n", x)
		}
	}
	if len(pl.Blockers) > 0 {
		fmt.Fprintf(w, "\nREFUSED — %d reason(s)\n", len(pl.Blockers))
		for _, x := range pl.Blockers {
			fmt.Fprintf(w, "  ✗ %s\n", x)
		}
	} else {
		fmt.Fprintf(w, "\nNo blockers. Pause on this machine will last about as long as one incremental backup + one restore of %s.\n", humanBytes(dbTotal))
	}
}

var _ = runtime.GOOS
