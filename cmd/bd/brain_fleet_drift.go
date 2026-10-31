package main

// The fleet drift view: 'brain stores doctor --fleet' asks every roster
// machine for its host-audit facts and compares them with the live store host
// and the repo's main. Anything that disagrees is a driftItem; the command
// exits non-zero when there is any. The orphan check (per-database id diff
// between an old/stray Dolt server and the live one) lives here too.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Drift kinds.
const (
	driftUnreachable   = "unreachable"
	driftAuditMissing  = "audit-unavailable"
	driftBinary        = "binary"
	driftStoreHost     = "store-host"
	driftPin           = "pin"
	driftReadProbe     = "read-probe"
	driftWriteProbe    = "write-probe"
	driftBackup        = "backup"
	driftOrphans       = "orphans"
	driftLiveHost      = "live-host"
	driftNoLiveHost    = "no-live-host"
	pinRepairWrapper   = "wrapper"
	pinRepairEnvFile   = "env-file"
	maxOrphanSampleIDs = 20
)

// driftItem is one disagreement the fleet view found.
type driftItem struct {
	Machine string `json:"machine"`
	Kind    string `json:"kind"`
	Subject string `json:"subject,omitempty"`
	Detail  string `json:"detail"`
	Fix     string `json:"fix,omitempty"`
	// Repairable is true only for drift --repair may fix: wrapper host pins,
	// through rewrite-wrappers.
	Repairable bool `json:"repairable,omitempty"`
}

// hostAddr is a Dolt server address.
type hostAddr struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func (h hostAddr) String() string { return net.JoinHostPort(h.Host, strconv.Itoa(h.Port)) }

func parseHostAddr(s string) (hostAddr, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(s))
	if err != nil {
		return hostAddr{}, fmt.Errorf("%q is not host:port", s)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 || p > 65535 || host == "" {
		return hostAddr{}, fmt.Errorf("%q is not host:port", s)
	}
	return hostAddr{Host: host, Port: p}, nil
}

// fleetHostResult is one machine's audit and the drift found in it.
type fleetHostResult struct {
	Name      string         `json:"name"`
	Reachable bool           `json:"reachable"`
	Error     string         `json:"error,omitempty"`
	Audit     *hostAudit     `json:"audit,omitempty"`
	Binary    *binaryCheck   `json:"binary,omitempty"`
	Drift     []driftItem    `json:"drift,omitempty"`
	PinCount  int            `json:"pin_count"`
	Repaired  *repairOutcome `json:"repair,omitempty"`
}

type repairOutcome struct {
	Attempted bool   `json:"attempted"`
	Status    string `json:"status"` // "ok" | "fail" | "refused" | "skipped"
	Detail    string `json:"detail,omitempty"`
}

// fleetAuditOptions are the knobs of the audit half of the fleet view.
type fleetAuditOptions struct {
	Live        hostAddr
	LiveSource  string
	OldHosts    []hostAddr
	WriteProbe  bool
	BackupMax   time.Duration
	Repair      bool
	MainSHA     string
	IsAncestor  func(string) bool
	Local       func(ctx context.Context, writeProbe bool) hostAudit
	Self        string
	Scanner     idScanner
	LiveChecker func(ctx context.Context, a hostAddr) error
	// SelfRepair runs rewrite-wrappers on this machine (the roster member that
	// is the orchestrator); other machines go through the fleet runner.
	SelfRepair func(ctx context.Context) ([]byte, error)
}

// binaryArgs is the host-audit command line sent to a machine.
func hostAuditArgs(o fleetAuditOptions) []string {
	args := []string{"stores", "host-audit"}
	if o.WriteProbe {
		args = append(args, "--write-probe")
	}
	return args
}

func fetchHostAudit(ctx context.Context, m fleetMember, run fleetRunner, o fleetAuditOptions) (*hostAudit, error) {
	if m.Name == o.Self && o.Local != nil {
		a := o.Local(ctx, o.WriteProbe)
		return &a, nil
	}
	out, err := run(ctx, m, hostAuditArgs(o)...)
	start := strings.IndexByte(string(out), '{')
	if start < 0 {
		if err != nil {
			return nil, err
		}
		// The host answered but not with an audit: an older brain prints its
		// 'stores' help for a subcommand it does not have.
		return nil, errAuditUnsupported
	}
	var a hostAudit
	if jerr := json.Unmarshal(out[start:], &a); jerr != nil {
		return nil, jerr
	}
	return &a, nil
}

// expectedStoreHost is the store host a machine should use: its roster
// store_host when declared, else the live host.
func expectedStoreHost(m fleetMember, o fleetAuditOptions) hostAddr {
	if m.StoreHost != "" {
		exp, _ := parseHostAddr(m.StoreHost)
		return exp
	}
	return o.Live
}

// brainWrapperPinsHost reports whether the brain wrapper, which the audit runs
// through, itself exports host; the pin check already judges that pin.
func brainWrapperPinsHost(pins []hostPin, host string) bool {
	for _, p := range pins {
		if p.Host == host && ((p.Source == "wrapper" && p.Name == "brain") || (p.Source == "env-file" && p.Via == "brain")) {
			return true
		}
	}
	return false
}

// classifyHostAudit compares one machine's facts with the live host and the
// repo. It is pure.
func classifyHostAudit(m fleetMember, a *hostAudit, o fleetAuditOptions) (drift []driftItem, bin binaryCheck) {
	add := func(kind, subject, detail, fix string, repairable bool) {
		drift = append(drift, driftItem{Machine: m.Name, Kind: kind, Subject: subject, Detail: detail, Fix: fix, Repairable: repairable})
	}
	bin = classifyBinary(a.Stamp, o.MainSHA, o.IsAncestor)
	if bin.Status == "warn" {
		add(driftBinary, "bd", bin.Reason+" (installed "+a.BdVersion+")", bin.Fix, false)
	}

	exp := expectedStoreHost(m, o)
	want := exp
	sh := a.StoreHost
	switch {
	case !sh.Present:
		add(driftStoreHost, sh.Path, "store-host.env is missing: wrappers on this machine have no shared host to read",
			"write BEADS_DOLT_SERVER_HOST="+exp.Host+" and BEADS_DOLT_SERVER_PORT="+strconv.Itoa(exp.Port)+" to "+sh.Path+" (doctor --repair never edits it)", false)
	case sh.Error != "":
		add(driftStoreHost, sh.Path, "store-host.env is unusable: "+sh.Error, "fix the file by hand", false)
	case exp.Host != "" && (sh.Host != exp.Host || sh.Port != exp.Port):
		detail := fmt.Sprintf("store-host.env says %s:%d but the live host is %s", sh.Host, sh.Port, o.Live)
		fix := "edit store-host.env by hand (doctor --repair never edits it)"
		if m.StoreHost != "" {
			detail = fmt.Sprintf("store-host.env says %s:%d but the roster expects %s (live host %s)", sh.Host, sh.Port, exp, o.Live)
		} else if hostIsLoopback(sh.Host) {
			detail += "; a loopback host is right only on the Dolt host itself or behind a forwarder to it"
			fix = "if that is intended, declare store_host: " + sh.Host + ":" + strconv.Itoa(sh.Port) + " for " + m.Name + " in fleet.yaml; otherwise edit store-host.env by hand"
		}
		add(driftStoreHost, sh.Path, detail, fix, false)
	}
	if a.EnvHost != "" && exp.Host != "" && a.EnvHost != exp.Host && !brainWrapperPinsHost(a.Pins, a.EnvHost) && !(sh.Present && sh.Error == "" && a.EnvHost == sh.Host) {
		add(driftStoreHost, "BEADS_DOLT_SERVER_HOST", fmt.Sprintf("this machine's process environment sets BEADS_DOLT_SERVER_HOST=%s, which overrides store-host.env; expected %s", a.EnvHost, exp), "unset it in the shell profile / launchd plist", false)
	}

	// A pin disagrees with what the machine should use: its store-host.env when
	// valid, else the live host. rewrite-wrappers re-pins to store-host.env, so
	// a wrapper pin is repairable only when store-host.env agrees with the
	// expected store host.
	shValid := sh.Present && sh.Error == ""
	if shValid {
		want = hostAddr{Host: sh.Host, Port: sh.Port}
	}
	shAgrees := shValid && exp.Host != "" && sh.Host == exp.Host && sh.Port == exp.Port
	for _, p := range a.Pins {
		if want.Host == "" || (p.Host == "" && p.Port == 0) {
			continue
		}
		if (p.Host != "" && p.Host != want.Host) || (p.Port != 0 && p.Port != want.Port) {
			detail := fmt.Sprintf("%s pins %s but the host should be %s: %s", p.Source, pinAddr(p), want, p.Line)
			fix := "brain stores rewrite-wrappers on " + m.Name + " (or doctor --fleet --repair)"
			if shValid && exp.Host != "" && !shAgrees {
				detail = fmt.Sprintf("%s pins %s but store-host.env says %s, and the expected store host is %s: %s", p.Source, pinAddr(p), want, exp, p.Line)
			}
			switch {
			case exp.Host == "":
				fix = "establish the live store host first (pass --live-host host:port), then run brain stores rewrite-wrappers on " + m.Name + " if store-host.env agrees"
			case !shAgrees:
				fix = "correct store-host.env on " + m.Name + " first (doctor --repair never edits it), then run brain stores rewrite-wrappers"
			}
			if p.Source != "wrapper" {
				fix = "remove the pin from " + p.Path + " by hand (rewrite-wrappers does not edit env files)"
			}
			add(driftPin, p.Path, detail, fix, p.Source == "wrapper" && shAgrees)
		}
	}

	backups := map[string][]string{} // reason -> store names
	for _, s := range a.Stores {
		if s.Read.Status == "fail" {
			add(driftReadProbe, s.Name, "read probe failed: "+s.Read.Reason, "", false)
			continue
		}
		if s.Write != nil && s.Write.Status == "fail" {
			add(driftWriteProbe, s.Name, "write probe failed: "+s.Write.Reason, "", false)
		}
		if s.Backup != nil && o.BackupMax > 0 {
			var reason string
			switch {
			case s.Backup.Status == "fail":
				reason = "backup status unreadable: " + s.Backup.Reason
			case s.Backup.Status == "none":
				reason = "no backup has ever been recorded"
			case time.Duration(s.Backup.AgeSeconds)*time.Second > o.BackupMax:
				reason = fmt.Sprintf("last backup is over %s old (limit %s)", (time.Duration(s.Backup.AgeSeconds) * time.Second).Round(24*time.Hour), o.BackupMax)
			}
			if reason != "" {
				backups[reason] = append(backups[reason], s.Name)
			}
		}
	}
	// One item per distinct reason, not per store.
	for _, reason := range sortedKeys(backups) {
		names := backups[reason]
		sort.Strings(names)
		sample := strings.Join(names, " ")
		if len(names) > 5 {
			sample = strings.Join(names[:5], " ") + fmt.Sprintf(" …(+%d)", len(names)-5)
		}
		add(driftBackup, fmt.Sprintf("%d store(s)", len(names)), reason+": "+sample, "bd backup sync", false)
	}
	return drift, bin
}

func hostIsLoopback(h string) bool {
	return h == "localhost" || h == "::1" || strings.HasPrefix(h, "127.")
}

func pinAddr(p hostPin) string {
	h := p.Host
	if h == "" {
		h = "?"
	}
	if p.Port != 0 {
		return h + ":" + strconv.Itoa(p.Port)
	}
	return h
}

// ---------------------------------------------------------------------------
// orphan check
// ---------------------------------------------------------------------------

// idScanner lists the issue ids of every database on a Dolt server. Tests
// replace it with a fake.
type idScanner func(ctx context.Context, addr hostAddr) (map[string]map[string]struct{}, error)

var systemDatabases = map[string]bool{"information_schema": true, "mysql": true, "performance_schema": true, "sys": true, "dolt_cluster": true}

// scanServerIDs is the real scanner: SHOW DATABASES, then SELECT id FROM
// issues for every database that has an issues table. Read-only.
func scanServerIDs(ctx context.Context, addr hostAddr) (map[string]map[string]struct{}, error) {
	cfg := mysql.NewConfig()
	cfg.User = envOr("BEADS_DOLT_SERVER_USER", "root")
	cfg.Passwd = os.Getenv("BEADS_DOLT_PASSWORD")
	cfg.Net, cfg.Addr = "tcp", addr.String()
	cfg.Timeout, cfg.ReadTimeout = 5*time.Second, 60*time.Second
	conn, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	rows, err := conn.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	var dbs []string
	for rows.Next() {
		var d string
		if rows.Scan(&d) == nil && !systemDatabases[d] {
			dbs = append(dbs, d)
		}
	}
	_ = rows.Close()
	out := map[string]map[string]struct{}{}
	for _, d := range dbs {
		q := "SELECT id FROM `" + strings.ReplaceAll(d, "`", "``") + "`.issues" //nolint:gosec // identifier backtick-escaped; databases come from SHOW DATABASES
		r, err := conn.QueryContext(ctx, q)
		if err != nil {
			continue // no issues table: not a bead database
		}
		ids := map[string]struct{}{}
		for r.Next() {
			var id string
			if r.Scan(&id) == nil {
				ids[id] = struct{}{}
			}
		}
		_ = r.Close()
		out[d] = ids
	}
	return out, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// orphanDB is the ids one database holds on a server but not on the live host.
type orphanDB struct {
	Database string   `json:"database"`
	Count    int      `json:"count"`
	Sample   []string `json:"sample_ids"`
	// LiveMissing is true when the live host has no such database at all.
	LiveMissing bool `json:"live_database_missing,omitempty"`
}

// orphanServer is the orphan finding for one non-live server.
type orphanServer struct {
	Server  string     `json:"server"`
	Origin  string     `json:"origin"` // "configured old host" | "stray server on <machine>"
	Error   string     `json:"error,omitempty"`
	Orphans []orphanDB `json:"orphans,omitempty"`
	Replay  []string   `json:"replay,omitempty"`
	Scanned int        `json:"databases_scanned"`
	Total   int        `json:"orphan_ids"`
}

// diffOrphans computes the orphan ids of one server against the live server.
func diffOrphans(old, live map[string]map[string]struct{}) (out []orphanDB, total int) {
	for _, db := range sortedKeys(old) {
		liveIDs, liveHas := live[db]
		var missing []string
		for id := range old[db] {
			if _, ok := liveIDs[id]; !ok {
				missing = append(missing, id)
			}
		}
		if len(missing) == 0 {
			continue
		}
		sort.Strings(missing)
		o := orphanDB{Database: db, Count: len(missing), LiveMissing: !liveHas}
		o.Sample = missing
		if len(o.Sample) > maxOrphanSampleIDs {
			o.Sample = o.Sample[:maxOrphanSampleIDs]
		}
		total += len(missing)
		out = append(out, o)
	}
	return out, total
}

// replayHint is the command an operator may run to copy a database's orphans
// to the live host. It is only printed, never run.
func replayHint(server, live hostAddr, db string) string {
	return fmt.Sprintf("BEADS_DOLT_SERVER_HOST=%s BEADS_DOLT_SERVER_PORT=%d <store> export -o %s.orphans.jsonl  &&  BEADS_DOLT_SERVER_HOST=%s BEADS_DOLT_SERVER_PORT=%d <store> import %s.orphans.jsonl   # review the file first; db %s",
		server.Host, server.Port, db, live.Host, live.Port, db, db)
}

type orphanTarget struct {
	Addr   hostAddr
	Origin string
	// Required targets (an operator's --old-host) are drift when they cannot
	// be scanned; a discovered stray server that cannot be scanned is not.
	Required bool
}

// checkOrphans scans the live server and each target and reports ids that are
// only on the target.
func checkOrphans(ctx context.Context, scan idScanner, live hostAddr, targets []orphanTarget) ([]orphanServer, []driftItem) {
	var res []orphanServer
	var drift []driftItem
	liveIDs, lerr := scan(ctx, live)
	seen := map[string]bool{live.String(): true}
	for _, t := range targets {
		if seen[t.Addr.String()] {
			continue
		}
		seen[t.Addr.String()] = true
		os_ := orphanServer{Server: t.Addr.String(), Origin: t.Origin}
		if lerr != nil {
			os_.Error = "live host unreadable: " + firstErrLine(lerr)
			res = append(res, os_)
			drift = append(drift, driftItem{Machine: "fleet", Kind: driftOrphans, Subject: t.Addr.String(), Detail: "cannot compare with the live host: " + firstErrLine(lerr)})
			continue
		}
		ids, err := scan(ctx, t.Addr)
		if err != nil {
			os_.Error = firstErrLine(err)
			res = append(res, os_)
			if t.Required {
				drift = append(drift, driftItem{Machine: "fleet", Kind: driftOrphans, Subject: t.Addr.String(),
					Detail: "cannot scan " + t.Origin + " " + t.Addr.String() + " for orphan beads: " + os_.Error,
					Fix:    "make " + t.Addr.String() + " reachable with valid credentials, or drop it from --old-host"})
			}
			continue
		}
		os_.Scanned = len(ids)
		os_.Orphans, os_.Total = diffOrphans(ids, liveIDs)
		for _, o := range os_.Orphans {
			os_.Replay = append(os_.Replay, replayHint(t.Addr, live, o.Database))
		}
		res = append(res, os_)
		if os_.Total > 0 {
			var parts []string
			for _, o := range os_.Orphans {
				parts = append(parts, fmt.Sprintf("%s:%d", o.Database, o.Count))
			}
			drift = append(drift, driftItem{Machine: "fleet", Kind: driftOrphans, Subject: t.Addr.String(),
				Detail: fmt.Sprintf("%d bead id(s) exist on %s (%s) but not on the live host %s [%s]", os_.Total, t.Addr, t.Origin, live, strings.Join(parts, " ")),
				Fix:    "review, then replay (offered, never automatic): see replay commands in --json output"})
		}
	}
	return res, drift
}

// ---------------------------------------------------------------------------
// the audit half of the fleet report
// ---------------------------------------------------------------------------

type fleetAuditReport struct {
	LiveHost   string            `json:"live_host"`
	LiveSource string            `json:"live_host_source"`
	LiveError  string            `json:"live_host_error,omitempty"`
	Hosts      []fleetHostResult `json:"hosts"`
	Orphans    []orphanServer    `json:"orphan_check,omitempty"`
	Drift      []driftItem       `json:"drift"`
	Clean      bool              `json:"clean"`
	Repair     bool              `json:"repair_requested,omitempty"`
}

func tcpDial(ctx context.Context, a hostAddr) error {
	d := net.Dialer{Timeout: 5 * time.Second}
	c, err := d.DialContext(ctx, "tcp", a.String())
	if err != nil {
		return err
	}
	return c.Close()
}

// runFleetAudit asks every roster machine for its facts, classifies drift,
// optionally repairs wrapper pins, then runs the orphan check.
func runFleetAudit(ctx context.Context, roster []fleetMember, run fleetRunner, o fleetAuditOptions, timeout time.Duration) *fleetAuditReport {
	rep := &fleetAuditReport{LiveHost: o.Live.String(), LiveSource: o.LiveSource, Repair: o.Repair}
	if o.Live.Host == "" {
		rep.LiveHost = ""
		rep.LiveError = "the live store host is unknown: pass --live-host host:port or write store-host.env on this machine"
		rep.Drift = append(rep.Drift, driftItem{Machine: "fleet", Kind: driftNoLiveHost, Detail: rep.LiveError})
	} else {
		check := o.LiveChecker
		if check == nil {
			check = tcpDial
		}
		if err := check(ctx, o.Live); err != nil {
			rep.LiveError = firstErrLine(err)
			rep.Drift = append(rep.Drift, driftItem{Machine: "fleet", Kind: driftLiveHost, Subject: o.Live.String(), Detail: "the live store host does not answer: " + rep.LiveError})
		}
	}

	results := make([]fleetHostResult, len(roster))
	var wg sync.WaitGroup
	for i, m := range roster {
		wg.Add(1)
		go func(i int, m fleetMember) {
			defer wg.Done()
			mctx, cancel := context.WithTimeout(ctx, 4*timeout)
			defer cancel()
			r := fleetHostResult{Name: m.Name}
			a, err := fetchHostAudit(mctx, m, run, o)
			if err != nil {
				r.Error = firstErrLine(err)
				if isUnknownCommand(err) {
					r.Reachable = true
					r.Drift = []driftItem{{Machine: m.Name, Kind: driftAuditMissing, Detail: "installed brain predates 'stores host-audit': it has no build stamp and cannot report pins or probes", Fix: binaryFixHint}}
				} else {
					r.Drift = []driftItem{{Machine: m.Name, Kind: driftUnreachable, Detail: "unreachable: " + r.Error, Fix: "wake or reach " + m.Name + " (this tool never touches an unreachable host)"}}
				}
				results[i] = r
				return
			}
			r.Reachable, r.Audit = true, a
			drift, bin := classifyHostAudit(m, a, o)
			r.Binary, r.Drift = &bin, drift
			files := map[string]bool{}
			for _, p := range a.Pins {
				files[p.Path] = true
			}
			r.PinCount = len(files) // distinct wrappers/env files, not assignment lines
			results[i] = r
		}(i, m)
	}
	wg.Wait()

	if o.Repair {
		for i := range results {
			repairHost(ctx, roster[i], run, o, &results[i])
		}
	}

	var targets []orphanTarget
	for _, h := range o.OldHosts {
		targets = append(targets, orphanTarget{Addr: h, Origin: "configured old host", Required: true})
	}
	var unscannable []orphanServer
	if o.Live.Host != "" {
		for i, r := range results {
			if r.Audit == nil {
				continue
			}
			for _, s := range r.Audit.Servers {
				host := strayServerHost(s, roster[i], r.Name == o.Self)
				origin := "Dolt server running on " + r.Name
				if s.Port == 0 {
					unscannable = append(unscannable, orphanServer{Server: host, Origin: origin, Error: "port not on its command line: " + s.Args})
					continue
				}
				targets = append(targets, orphanTarget{Addr: hostAddr{Host: host, Port: s.Port}, Origin: origin})
			}
		}
	}
	if o.Scanner != nil && o.Live.Host != "" && len(targets) > 0 {
		var od []driftItem
		rep.Orphans, od = checkOrphans(ctx, o.Scanner, o.Live, targets)
		rep.Drift = append(rep.Drift, od...)
	}
	rep.Orphans = append(rep.Orphans, unscannable...)

	rep.Hosts = results
	for _, r := range results {
		rep.Drift = append(rep.Drift, r.Drift...)
	}
	sort.SliceStable(rep.Drift, func(i, j int) bool { return rep.Drift[i].Machine < rep.Drift[j].Machine })
	rep.Clean = len(rep.Drift) == 0
	return rep
}

// strayServerHost is the address the orchestrator dials for a Dolt server a
// machine reported. A wildcard bind, or a loopback bind on another machine,
// is reached through the machine's roster ssh host (without any user@).
func strayServerHost(s localServer, m fleetMember, self bool) string {
	h := s.Host
	ip := net.ParseIP(h)
	loopback := h == "localhost" || (ip != nil && ip.IsLoopback())
	if h == "" || (ip != nil && ip.IsUnspecified()) || (loopback && !self) {
		h = m.SSH
		if at := strings.LastIndexByte(h, '@'); at >= 0 {
			h = h[at+1:]
		}
	}
	return h
}

var errAuditUnsupported = errors.New("unknown command \"host-audit\"")

func isUnknownCommand(err error) bool {
	s := err.Error()
	return strings.Contains(s, "unknown command") || strings.Contains(s, "unknown flag")
}

// repairHost fixes wrapper host pins on one reachable machine by running the
// existing rewrite-wrappers verb, then re-audits it. It never installs
// binaries, edits store-host.env, touches Dolt servers or acts on an
// unreachable machine, and it leaves env-file pins to the operator.
func repairHost(ctx context.Context, m fleetMember, run fleetRunner, o fleetAuditOptions, r *fleetHostResult) {
	if !r.Reachable || r.Audit == nil {
		return
	}
	pins, fixable := false, false
	for _, d := range r.Drift {
		pins = pins || d.Kind == driftPin
		fixable = fixable || d.Repairable
	}
	if !pins {
		return
	}
	sh, exp := r.Audit.StoreHost, expectedStoreHost(m, o)
	refuse := func(detail string) { r.Repaired = &repairOutcome{Status: "refused", Detail: detail} }
	switch {
	case !sh.Present || sh.Error != "":
		refuse("store-host.env is missing or unusable on " + m.Name + "; rewrite-wrappers refuses, and --repair never writes it")
		return
	case exp.Host == "":
		refuse("the live store host is unknown, so store-host.env on " + m.Name + " cannot be confirmed; --repair rewrites pins only when store-host.env agrees with the live host")
		return
	case sh.Host != exp.Host || sh.Port != exp.Port:
		refuse(fmt.Sprintf("store-host.env on %s says %s:%d but the expected store host is %s; rewrite-wrappers would point its wrappers there, and --repair never edits store-host.env", m.Name, sh.Host, sh.Port, exp))
		return
	case !fixable:
		return
	}
	out := &repairOutcome{Attempted: true}
	r.Repaired = out
	rctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var raw []byte
	var err error
	if m.Name == o.Self && o.SelfRepair != nil {
		raw, err = o.SelfRepair(rctx)
	} else {
		raw, err = run(rctx, m, "stores", "rewrite-wrappers", "--json")
	}
	results, perr := parseRewriteResults(raw)
	switch {
	case perr != nil && err != nil:
		out.Status, out.Detail = "fail", firstErrLine(err)
	case perr != nil:
		out.Status, out.Detail = "fail", firstErrLine(perr)
	default:
		if failed := failedRewrites(results); len(failed) > 0 {
			out.Status = "fail"
			out.Detail = fmt.Sprintf("%d of %d wrappers did not pass rewrite-wrappers", len(failed), len(results))
		} else {
			out.Status = "ok"
			out.Detail = fmt.Sprintf("%d wrappers rewritten and verified", len(results))
		}
	}
	// Re-audit read-only so the report shows what is left, not what was.
	failedRepair := out.Status == "fail"
	a2o := o
	a2o.WriteProbe = false
	a2, aerr := fetchHostAudit(rctx, m, run, a2o)
	if aerr != nil {
		if failedRepair {
			r.Drift = append(r.Drift, driftItem{Machine: m.Name, Kind: "repair", Detail: "rewrite-wrappers did not complete: " + out.Detail})
		}
		return
	}
	r.Audit = a2
	drift, bin := classifyHostAudit(m, a2, o)
	r.Binary, r.Drift = &bin, drift
	if failedRepair {
		r.Drift = append(r.Drift, driftItem{Machine: m.Name, Kind: "repair", Detail: "rewrite-wrappers did not complete: " + out.Detail, Fix: "run 'brain stores rewrite-wrappers' on " + m.Name})
	}
}

// runLocalRewrite runs this binary's rewrite-wrappers verb (the orchestrating
// machine's own repair).
func runLocalRewrite(ctx context.Context) ([]byte, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	c := exec.CommandContext(ctx, self, "stores", "rewrite-wrappers", "--json") //nolint:gosec // our own binary
	c.WaitDelay = 2 * time.Second
	out, err := c.Output()
	return out, err
}

func driftMachines(d []driftItem) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range d {
		if !seen[x.Machine] {
			seen[x.Machine] = true
			out = append(out, x.Machine)
		}
	}
	return out
}

// fleetAuditOptionsFromFlags builds the audit options for the real command.
func fleetAuditOptionsFromFlags(ctx context.Context) (fleetAuditOptions, error) {
	home, _ := os.UserHomeDir()
	o := fleetAuditOptions{WriteProbe: !fleetNoWriteProbe, BackupMax: fleetBackupMax, Repair: fleetRepair,
		Self: machineName(), Scanner: scanServerIDs, SelfRepair: runLocalRewrite}
	switch {
	case fleetLiveHost != "":
		a, err := parseHostAddr(fleetLiveHost)
		if err != nil {
			return o, fmt.Errorf("--live-host: %w", err)
		}
		o.Live, o.LiveSource = a, "--live-host"
	default:
		if v := readStoreHostView(home); v.Present && v.Error == "" {
			o.Live, o.LiveSource = hostAddr{Host: v.Host, Port: v.Port}, v.Path
		}
	}
	for _, h := range fleetOldHosts {
		a, err := parseHostAddr(h)
		if err != nil {
			return o, fmt.Errorf("--old-host: %w", err)
		}
		o.OldHosts = append(o.OldHosts, a)
	}
	repo := os.Getenv("BRAIN_REPO")
	if repo == "" {
		repo = SourceRepo
	}
	o.MainSHA, o.IsAncestor = repoMain(ctx, repo)
	o.Local = func(c context.Context, writeProbe bool) hostAudit {
		return buildHostAudit(c, auditOptions{WriteProbe: writeProbe, Servers: discoverDoltServers})
	}
	return o, nil
}

func printFleetAudit(w interface{ Write([]byte) (int, error) }, a *fleetAuditReport) {
	live := a.LiveHost
	if live == "" {
		live = "(unknown)"
	}
	fmt.Fprintf(w, "\nlive store host: %s (from %s)\n", live, a.LiveSource)
	for _, h := range a.Hosts {
		switch {
		case h.Audit == nil && h.Reachable:
			fmt.Fprintf(w, "%-12s %-11s %s\n", h.Name, "AUDIT-N/A", h.Error)
		case h.Audit == nil:
			fmt.Fprintf(w, "%-12s %-11s %s\n", h.Name, "UNREACHABLE", h.Error)
		default:
			sh := "store-host.env missing"
			if h.Audit.StoreHost.Present {
				sh = fmt.Sprintf("store-host.env %s:%d", h.Audit.StoreHost.Host, h.Audit.StoreHost.Port)
			}
			rOK, wOK, wN := 0, 0, 0
			for _, s := range h.Audit.Stores {
				if s.Read.Status == "ok" {
					rOK++
				}
				if s.Write != nil {
					wN++
					if s.Write.Status == "ok" {
						wOK++
					}
				}
			}
			stamp := "unstamped"
			if h.Audit.Stamp.Commit != "" {
				stamp = shortCommit(h.Audit.Stamp.Commit)
			}
			fmt.Fprintf(w, "%-12s bd %s (%s)  %s  pins %d  read %d/%d  write %d/%d\n", h.Name, h.Audit.BdVersion, stamp, sh, h.PinCount, rOK, len(h.Audit.Stores), wOK, wN)
		}
		if h.Repaired != nil {
			fmt.Fprintf(w, "%-12s repair: %s %s\n", "", h.Repaired.Status, h.Repaired.Detail)
		}
	}
	for _, o := range a.Orphans {
		if o.Error != "" {
			fmt.Fprintf(w, "orphan check %s (%s): not scanned: %s\n", o.Server, o.Origin, o.Error)
		} else {
			fmt.Fprintf(w, "orphan check %s (%s): %d orphan id(s) in %d database(s)\n", o.Server, o.Origin, o.Total, len(o.Orphans))
		}
		for _, r := range o.Replay {
			fmt.Fprintf(w, "    replay (offered, never automatic): %s\n", r)
		}
	}
	for _, d := range a.Drift {
		fmt.Fprintf(w, "DRIFT %-12s %-16s %s: %s\n", d.Machine, d.Kind, d.Subject, d.Detail)
		if d.Fix != "" {
			fmt.Fprintf(w, "      fix: %s\n", d.Fix)
		}
	}
	if a.Clean {
		fmt.Fprintln(w, "FLEET AUDIT CLEAN")
	} else {
		fmt.Fprintf(w, "FLEET AUDIT: %d drift item(s)\n", len(a.Drift))
	}
}
