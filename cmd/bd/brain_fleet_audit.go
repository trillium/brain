package main

// 'brain stores host-audit': the facts one machine can report about itself for
// 'brain stores doctor --fleet' (build stamp, store-host.env, every host pin
// in wrappers and the env files they source, a read probe, a write probe and
// the backup age of each store, and the Dolt servers running here). It decides
// nothing: the orchestrator compares the facts with the live host and the repo
// (brain_fleet_drift.go). Read-only except for the opt-in write probe, whose
// footprint is documented on probeStoreWrite.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/configfile"
)

// maxScannedScriptBytes bounds the files the pin scan reads: wrappers and env
// files are tiny; anything bigger is a real binary or data file.
const maxScannedScriptBytes = 256 * 1024

// storeHostView is this machine's shared store-host.env as written.
type storeHostView struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	Host    string `json:"host,omitempty"`
	Port    int    `json:"port,omitempty"`
	Error   string `json:"error,omitempty"`
}

// hostPin is one literal Dolt host/port assignment found outside
// store-host.env: baked into a wrapper, or in an env file a wrapper sources
// (or that sits in ~/.config/brain-remote).
type hostPin struct {
	Name   string `json:"name"`   // wrapper or env file basename
	Path   string `json:"path"`   // file holding the assignment
	Source string `json:"source"` // "wrapper" | "env-file"
	Via    string `json:"via,omitempty"`
	Host   string `json:"host,omitempty"`
	Port   int    `json:"port,omitempty"`
	Line   string `json:"line"`
}

// probeResult is one read, write or backup check of one store.
type probeResult struct {
	Status string `json:"status"` // "ok" | "fail" | "skipped" | "stale" | "none"
	Reason string `json:"reason,omitempty"`
	Millis int64  `json:"ms,omitempty"`
	// AgeSeconds is the backup age (backup checks only).
	AgeSeconds int64 `json:"age_seconds,omitempty"`
}

type storeAudit struct {
	Name   string       `json:"name"`
	Read   probeResult  `json:"read"`
	Write  *probeResult `json:"write,omitempty"`
	Backup *probeResult `json:"backup,omitempty"`
}

// localServer is a Dolt sql-server process found on this machine.
type localServer struct {
	Host string `json:"host,omitempty"`
	Port int    `json:"port"`
	Args string `json:"args"`
}

type hostAudit struct {
	Machine      string        `json:"machine"`
	BdVersion    string        `json:"bd_version"`
	Stamp        buildStamp    `json:"stamp"`
	StoreHost    storeHostView `json:"store_host"`
	EnvHost      string        `json:"env_host,omitempty"`
	Pins         []hostPin     `json:"pins"`
	WrapperCount int           `json:"wrappers"`
	Stores       []storeAudit  `json:"stores"`
	Servers      []localServer `json:"local_servers,omitempty"`
	Notes        []string      `json:"notes,omitempty"`
}

type auditOptions struct {
	WriteProbe bool
	Home       string
	Jobs       int
	Timeout    time.Duration
	BackupMax  time.Duration
	StoresFunc func() (map[string]storeEntry, error)
	Self       string
	Servers    func() []localServer
}

// readStoreHostView reads home/.config/brain/store-host.env as written.
func readStoreHostView(home string) storeHostView {
	v := storeHostView{Path: filepath.Join(home, ".config", "brain", configfile.StoreHostFileName)}
	f, err := os.Open(v.Path) //nolint:gosec // our own config file
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			v.Error = err.Error()
		}
		return v
	}
	defer func() { _ = f.Close() }()
	v.Present = true
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimPrefix(strings.TrimSpace(sc.Text()), "export ")
		if strings.HasPrefix(line, "#") {
			continue
		}
		k, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		switch strings.TrimSpace(k) {
		case configfile.StoreHostKeyHost:
			v.Host = val
		case configfile.StoreHostKeyPort:
			v.Port, _ = strconv.Atoi(val)
		}
	}
	if v.Host == "" || v.Port == 0 {
		v.Error = "does not define both " + configfile.StoreHostKeyHost + " and " + configfile.StoreHostKeyPort
	}
	return v
}

var (
	pinDefaultRe = regexp.MustCompile(`^\$\{[A-Za-z_]+:?-([^}]*)\}$`)
	sourceLineRe = regexp.MustCompile(`^\s*(?:\.|source)\s+("[^"]+"|'[^']+'|\S+)`)
)

// pinLiteral reduces an assigned value to its literal: a ${VAR:-default}
// default counts as the pin, any other expansion is not a literal.
func pinLiteral(v string) string {
	v = strings.Trim(strings.TrimSpace(v), wrapperQuoteTrim)
	if m := pinDefaultRe.FindStringSubmatch(v); m != nil {
		v = m[1]
	}
	if strings.Contains(v, "$") {
		return ""
	}
	return v
}

// scanPinsInBody returns every literal host/port assignment in a script body
// (the generated run-time host block is not a pin: bakedServerPinLine skips it).
func scanPinsInBody(body string) []hostPin {
	var out []hostPin
	for _, line := range strings.Split(body, "\n") {
		if !bakedServerPinLine(line) {
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

func readScript(path string) (string, bool) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxScannedScriptBytes {
		return "", false
	}
	data, err := os.ReadFile(path) //nolint:gosec // wrapper or env file under the user's home
	if err != nil {
		return "", false
	}
	return string(data), true
}

// collectHostPins finds the host pins on this machine: baked into
// ~/.local/bin wrappers, in env files those wrappers source, and in any
// ~/.config/brain-remote/*.env shim file. It also returns the wrapper count.
func collectHostPins(home string) (pins []hostPin, wrappers int) {
	sharedFile := filepath.Join(home, ".config", "brain", configfile.StoreHostFileName)
	seenEnv := map[string]bool{sharedFile: true}
	addEnv := func(path, name, via string) {
		if seenEnv[path] {
			return
		}
		seenEnv[path] = true
		body, ok := readScript(path)
		if !ok {
			return
		}
		for _, p := range scanPinsInBody(body) {
			p.Name, p.Path, p.Source, p.Via = name, path, "env-file", via
			pins = append(pins, p)
		}
	}
	expand := func(v string) string {
		v = strings.Trim(v, `"'`)
		v = strings.ReplaceAll(v, "${HOME}", home)
		v = strings.ReplaceAll(v, "$HOME", home)
		if strings.HasPrefix(v, "~/") {
			v = filepath.Join(home, v[2:])
		}
		return filepath.Clean(v)
	}
	binDir := filepath.Join(home, ".local", "bin")
	ents, _ := os.ReadDir(binDir)
	for _, e := range ents {
		if e.IsDir() || strings.Contains(e.Name(), wrapperBackupMarker) {
			continue
		}
		path := filepath.Join(binDir, e.Name())
		body, ok := readScript(path)
		if !ok || !strings.HasPrefix(body, "#!") {
			continue
		}
		wrappers++
		for _, p := range scanPinsInBody(body) {
			p.Name, p.Path, p.Source = e.Name(), path, "wrapper"
			pins = append(pins, p)
		}
		for _, line := range strings.Split(body, "\n") {
			if m := sourceLineRe.FindStringSubmatch(line); m != nil && !strings.HasPrefix(strings.TrimSpace(line), "#") {
				addEnv(expand(m[1]), filepath.Base(expand(m[1])), e.Name())
			}
		}
	}
	shimDir := filepath.Join(home, ".config", "brain-remote")
	if es, err := os.ReadDir(shimDir); err == nil {
		for _, e := range es {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".env") {
				addEnv(filepath.Join(shimDir, e.Name()), e.Name(), "")
			}
		}
	}
	sort.SliceStable(pins, func(i, j int) bool {
		if pins[i].Path != pins[j].Path {
			return pins[i].Path < pins[j].Path
		}
		return pins[i].Line < pins[j].Line
	})
	return pins, wrappers
}

var doltServerPortRe = regexp.MustCompile(`(?:^|\s)(?:-P|--port)[= ]\s*(\d+)`)
var doltServerHostRe = regexp.MustCompile(`(?:^|\s)(?:-H|--host)[= ]\s*(\S+)`)

// parseDoltServers extracts the 'dolt sql-server' processes from `ps` args
// lines. A server started without a port flag listens on Dolt's default 3306
// unless a config file sets another; it is reported with the flag value or 0.
func parseDoltServers(psOut string) []localServer {
	var out []localServer
	for _, line := range strings.Split(psOut, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "dolt") || !strings.Contains(line, "sql-server") {
			continue
		}
		s := localServer{Args: line}
		if m := doltServerPortRe.FindStringSubmatch(line); m != nil {
			s.Port, _ = strconv.Atoi(m[1])
		}
		if m := doltServerHostRe.FindStringSubmatch(line); m != nil {
			s.Host = m[1]
		}
		out = append(out, s)
	}
	return out
}

func discoverDoltServers() []localServer {
	out, err := exec.Command("ps", "-axo", "args=").Output() //nolint:gosec // fixed argv
	if err != nil {
		return nil
	}
	return parseDoltServers(string(out))
}

// storeSQL runs one statement through a store's own wrapper (the way an agent
// reaches the store) and returns its combined output.
func storeSQL(ctx context.Context, wrapper string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, wrapper, args...) //nolint:gosec // wrapper from our registry
	c.Env = append(os.Environ(), "BRAIN_NO_AUTO_FEATURE_REQUEST=1")
	c.WaitDelay = probeWaitDelay
	out, err := c.CombinedOutput()
	return string(out), err
}

const (
	writeProbeTablePrefix = "brain_fleet_probe_"
	writeProbeSkip        = "not yet supported in embedded mode"
)

// probeStoreWrite proves a store accepts a write and reads it back, through
// its own wrapper. Footprint: it creates the table brain_fleet_probe_<nonce>
// (its own table, so concurrent probes of one store from several machines
// never share or drop each other's table), inserts one row keyed by the nonce, reads that row back, deletes it and drops
// the table. No issue, dependency or other bead data is read or written; when
// every step succeeds the store ends as it began (the table is gone). A
// failure part-way still attempts the delete and drop, and says if that
// cleanup failed so the leftover table can be removed by hand. Dolt may record
// the create/drop as working-set changes that auto-commit folds into history.
func probeStoreWrite(ctx context.Context, wrapper string, nonce string) probeResult {
	started := time.Now()
	r := probeResult{Status: "ok"}
	finish := func() probeResult { r.Millis = time.Since(started).Milliseconds(); return r }
	run := func(args ...string) (string, error) { return storeSQL(ctx, wrapper, args...) }
	writeProbeTable := writeProbeTablePrefix + strings.ReplaceAll(nonce, "-", "_")

	cleanup := func() error {
		var errs []string
		if out, err := run("sql", "DELETE FROM "+writeProbeTable+" WHERE id='"+nonce+"'"); err != nil && !strings.Contains(out, "doesn't exist") && !strings.Contains(out, "not found") {
			errs = append(errs, "delete: "+firstLine([]byte(out)))
		}
		if out, err := run("sql", "DROP TABLE IF EXISTS "+writeProbeTable); err != nil {
			errs = append(errs, "drop: "+firstLine([]byte(out)))
		}
		if len(errs) > 0 {
			return errors.New(strings.Join(errs, "; "))
		}
		return nil
	}
	fail := func(step, out string, err error) probeResult {
		if strings.Contains(out, writeProbeSkip) {
			r.Status, r.Reason = "skipped", "embedded store: no SQL server to probe"
			return finish()
		}
		msg := firstLine([]byte(out))
		if msg == "" {
			msg = err.Error()
		}
		r.Status, r.Reason = "fail", step+": "+msg
		if cerr := cleanup(); cerr != nil {
			r.Reason += " (cleanup failed, remove table " + writeProbeTable + " by hand: " + cerr.Error() + ")"
		}
		return finish()
	}

	if out, err := run("sql", "CREATE TABLE IF NOT EXISTS "+writeProbeTable+" (id VARCHAR(64) PRIMARY KEY, ts BIGINT NOT NULL)"); err != nil {
		return fail("create probe table", out, err)
	}
	if out, err := run("sql", fmt.Sprintf("INSERT INTO %s (id, ts) VALUES ('%s', %d)", writeProbeTable, nonce, time.Now().Unix())); err != nil {
		return fail("insert", out, err)
	}
	out, err := run("sql", "SELECT id FROM "+writeProbeTable+" WHERE id='"+nonce+"'")
	if err != nil {
		return fail("read back", out, err)
	}
	if !strings.Contains(out, nonce) {
		return fail("read back", "", errors.New("the row just written is not visible"))
	}
	if cerr := cleanup(); cerr != nil {
		r.Status, r.Reason = "fail", "cleanup failed, remove table "+writeProbeTable+" by hand: "+cerr.Error()
	}
	return finish()
}

var backupTimestampRe = regexp.MustCompile(`"(?:timestamp|last_backup|last_sync)"\s*:\s*"([^"]+)"`)

// backupAge reads `<wrapper> backup status --json` and returns the age of the
// last backup. It never changes anything.
func backupAge(ctx context.Context, wrapper string, now time.Time) probeResult {
	out, err := storeSQL(ctx, wrapper, "backup", "status", "--json")
	if err != nil {
		return probeResult{Status: "fail", Reason: "backup status: " + firstLine([]byte(out))}
	}
	for _, m := range backupTimestampRe.FindAllStringSubmatch(out, -1) {
		if t, perr := time.Parse(time.RFC3339Nano, m[1]); perr == nil && !t.IsZero() && t.Year() > 2000 {
			return probeResult{Status: "ok", AgeSeconds: int64(now.Sub(t).Seconds())}
		}
	}
	return probeResult{Status: "none", Reason: "no backup has been recorded"}
}

// buildHostAudit gathers this machine's facts.
func buildHostAudit(ctx context.Context, o auditOptions) hostAudit {
	home := o.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if o.Jobs < 1 {
		o.Jobs = 4
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultStoreProbeTimeout
	}
	a := hostAudit{Machine: machineName(), BdVersion: Version, Stamp: currentBuildStamp(),
		StoreHost: readStoreHostView(home), EnvHost: os.Getenv(configfile.StoreHostKeyHost)}
	a.Pins, a.WrapperCount = collectHostPins(home)
	if o.Servers != nil {
		a.Servers = o.Servers()
	}
	loader := o.StoresFunc
	if loader == nil {
		loader = loadStoresRegistry
	}
	stores, err := loader()
	if err != nil {
		a.Notes = append(a.Notes, "cannot load the store registry: "+firstErrLine(err))
		return a
	}
	self := o.Self
	if self == "" {
		self, _ = os.Executable()
	}
	names := sortedKeys(stores)
	a.Stores = make([]storeAudit, len(names))
	sem := make(chan struct{}, o.Jobs)
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, name string) {
			defer wg.Done()
			defer func() { <-sem }()
			s := storeAudit{Name: name}
			h := probeStore(self, name, stores[name], o.Timeout)
			s.Read = probeResult{Status: "ok", Millis: h.Millis}
			if h.Status == "fail" {
				s.Read.Status, s.Read.Reason = "fail", h.Reason
			}
			if wp, ok := storeWrapperPath(name); ok && s.Read.Status == "ok" {
				if !isRemoteWrapper(wp) {
					bctx, cancel := context.WithTimeout(ctx, o.Timeout)
					b := backupAge(bctx, wp, time.Now())
					cancel()
					s.Backup = &b
					if o.WriteProbe {
						wctx, wcancel := context.WithTimeout(ctx, 3*o.Timeout)
						w := probeStoreWrite(wctx, wp, fmt.Sprintf("fleetprobe-%d-%d", time.Now().UnixNano(), i))
						wcancel()
						s.Write = &w
					}
				}
			}
			a.Stores[i] = s
		}(i, name)
	}
	wg.Wait()
	return a
}

var (
	hostAuditWriteProbe bool
	hostAuditBackupMax  time.Duration
)

var brainStoresHostAuditCmd = &cobra.Command{
	Use:   "host-audit",
	Short: "Report this machine's store-host, host pins, per-store probes and build stamp (used by 'stores doctor --fleet')",
	Long: `Reports, as JSON, what this machine says about itself: the build stamp, the
shared store-host.env, every literal Dolt host/port pin in the ~/.local/bin
wrappers and the env files they source (including ~/.config/brain-remote/*.env),
a read probe of every store through its wrapper, the age of each store's last
backup, and the Dolt servers running here. It decides nothing; 'brain stores
doctor --fleet' compares these facts across machines.

With --write-probe it also writes through each store's wrapper: it creates its
own table brain_fleet_probe_<nonce>, inserts one row, reads it back, deletes it and drops
the table. No bead data is touched; see docs/brain/FLEET_DOCTOR.md.`,
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		ctx := rootCtx
		if ctx == nil {
			ctx = context.Background()
		}
		a := buildHostAudit(ctx, auditOptions{WriteProbe: hostAuditWriteProbe, BackupMax: hostAuditBackupMax, Servers: discoverDoltServers})
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(a)
	},
}

func init() {
	brainStoresHostAuditCmd.Flags().BoolVar(&hostAuditWriteProbe, "write-probe", false, "Also write+read+delete a probe row in each store (see --help)")
	brainStoresHostAuditCmd.Flags().DurationVar(&hostAuditBackupMax, "backup-max-age", 48*time.Hour, "Accepted for symmetry with the fleet view; the audit reports ages only")
	brainStoresCmd.AddCommand(brainStoresHostAuditCmd)
}
