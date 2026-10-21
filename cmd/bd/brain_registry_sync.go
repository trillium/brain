package main

// 'brain stores sync': the one idempotent reconcile. It reads the registry
// snapshot (from the shared database, or through the relay host on a client
// machine), diffs it against this machine's shims, applies the difference, and
// reports back what it applied. Everything else (the launchd poller, the
// ride-along on every command, create/rename/remove on the machine that made
// the change) is just a trigger for this function.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/storeregistry"
)

// registrySource is where a machine reads the registry from and reports to.
type registrySource interface {
	// Snapshot returns the registry; found is false when the database has none yet.
	Snapshot(ctx context.Context) (snap storeregistry.Snapshot, found bool, err error)
	ProjectID(ctx context.Context, store string) (string, error)
	UnifiedDB(ctx context.Context) string
	Report(ctx context.Context, m storeregistry.Machine) error
	Machines(ctx context.Context) ([]storeregistry.Machine, error)
	Role() (role, relay string)
}

// dbSource reads the registry straight from the shared database.
type dbSource struct{ db *sql.DB }

func (s dbSource) Snapshot(ctx context.Context) (storeregistry.Snapshot, bool, error) {
	return storeregistry.ReadSnapshot(ctx, s.db)
}

func (s dbSource) ProjectID(ctx context.Context, store string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		"SELECT value FROM brain_unified_metadata WHERE `store` = ? AND `key` = '_project_id'", store).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (s dbSource) UnifiedDB(ctx context.Context) string {
	var name sql.NullString
	if err := s.db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&name); err != nil {
		return ""
	}
	return name.String
}

func (s dbSource) Report(ctx context.Context, m storeregistry.Machine) error {
	return storeregistry.ReportMachine(ctx, s.db, m, time.Now().UTC())
}

func (s dbSource) Machines(ctx context.Context) ([]storeregistry.Machine, error) {
	return storeregistry.ReadMachines(ctx, s.db)
}

func (s dbSource) Role() (string, string) { return "direct", "" }

// registryDBFunc opens the shared database. Tests replace it.
var registryDBFunc = func() (*sql.DB, error) { return storePrefixDB() }

// registrySnapshotJSON is the wire form of 'brain stores registry export --json'.
type registrySnapshotJSON struct {
	State     storeregistry.State     `json:"state"`
	Entries   []storeregistry.Entry   `json:"entries"`
	UnifiedDB string                  `json:"unified_db"`
	Found     bool                    `json:"found"`
	Machines  []storeregistry.Machine `json:"machines"`
}

// remoteSource reads the registry through the relay host (a client machine such
// as the macbook): one hop to the host, which answers from the database.
type remoteSource struct{ cfg *remoteConfig }

func (s remoteSource) Snapshot(ctx context.Context) (storeregistry.Snapshot, bool, error) {
	w, err := s.export(ctx)
	if err != nil {
		return storeregistry.Snapshot{}, false, err
	}
	if !w.Found {
		return storeregistry.Snapshot{}, false, nil
	}
	if got := storeregistry.Digest(w.Entries); got != w.State.Digest {
		return storeregistry.Snapshot{}, true, fmt.Errorf("%w: the relay host sent entries that do not match its digest", storeregistry.ErrTornRead)
	}
	return storeregistry.Snapshot{State: w.State, Entries: w.Entries}, true, nil
}

func (s remoteSource) ProjectID(context.Context, string) (string, error) {
	return "", errors.New("a client machine has no store directories to create")
}
func (s remoteSource) UnifiedDB(context.Context) string { return "" }
func (s remoteSource) Report(ctx context.Context, m storeregistry.Machine) error {
	data, _ := json.Marshal(m)
	_, err := remoteCapture(ctx, s.cfg, "brain", "stores", "registry", "report", "--machine-json", string(data))
	return err
}
func (s remoteSource) Machines(ctx context.Context) ([]storeregistry.Machine, error) {
	w, err := s.export(ctx)
	if err != nil {
		return nil, err
	}
	return w.Machines, nil
}

func (s remoteSource) export(ctx context.Context) (registrySnapshotJSON, error) {
	var w registrySnapshotJSON
	out, err := remoteCapture(ctx, s.cfg, "brain", "stores", "registry", "export", "--json")
	if err != nil && len(out) == 0 {
		return w, err
	}
	start := strings.IndexByte(string(out), '{')
	if start < 0 {
		return w, fmt.Errorf("the host printed no registry JSON: %q", strings.TrimSpace(string(out)))
	}
	if err := json.Unmarshal(out[start:], &w); err != nil {
		return w, fmt.Errorf("parsing the host's registry: %w", err)
	}
	return w, nil
}

func (s remoteSource) Role() (string, string) { return "client", s.cfg.Host }

// clientSourceIfConfigured returns a remoteSource when this machine is a
// client (a brain host is configured and BRAIN_REMOTE is not off).
func clientSourceIfConfigured() (registrySource, bool) {
	if strings.EqualFold(os.Getenv(envRemoteOff), "off") {
		return nil, false
	}
	cfg, err := loadRemoteConfig()
	if err != nil || cfg.Host == "" || cfg.validate() != nil {
		return nil, false
	}
	return remoteSource{cfg: cfg}, true
}

// ---------------------------------------------------------------------------
// lock
// ---------------------------------------------------------------------------

const registryLockStale = 5 * time.Minute

// acquireRegistryLock takes the per-machine sync lock. With wait == 0 it tries
// once. A lock older than registryLockStale (a crashed sync) is taken over.
func acquireRegistryLock(wait time.Duration) (release func(), ok bool) {
	path := homeFile(registryLockPath)
	if path == "" {
		return func() {}, true
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(path) }, true
		}
		if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > registryLockStale {
			_ = os.Remove(path)
			continue
		}
		if !time.Now().Before(deadline) {
			return nil, false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// sync
// ---------------------------------------------------------------------------

type syncOptions struct {
	Source  registrySource
	DryRun  bool
	Force   bool
	Wait    time.Duration // how long to wait for the sync lock (0 = try once)
	Machine string
	Trigger string // timer | ride-along | create | manual ...
	Out     io.Writer
	Quiet   bool
}

// syncReport is the outcome of one sync.
type syncReport struct {
	Status      string       `json:"status"` // converged | partial | not-adopted | busy | dry-run | skipped
	Machine     string       `json:"machine"`
	Role        string       `json:"role"`
	PrevEpoch   int64        `json:"previous_epoch"`
	Epoch       int64        `json:"epoch"`
	Digest      string       `json:"digest"`
	Changed     int          `json:"changed"`
	Failed      []string     `json:"failed,omitempty"`
	Missing     []string     `json:"missing,omitempty"`
	Orphans     []string     `json:"orphans,omitempty"`
	LocalOnly   []string     `json:"local_only,omitempty"`
	Backup      string       `json:"backup,omitempty"`
	Actions     []syncAction `json:"actions"`
	WrappersOK  int          `json:"wrappers_ok"`
	ActiveCount int          `json:"active"`
}

// Converged reports whether this machine now matches the registry.
func (r *syncReport) Converged() bool { return r.Status == "converged" }

func newShimEnv(ctx context.Context, src registrySource, machine string) shimEnv {
	home, _ := os.UserHomeDir()
	role, relay := src.Role()
	env := shimEnv{Home: home, Machine: machine, Role: role, Relay: relay}
	env.Host = os.Getenv("BEADS_DOLT_SERVER_HOST")
	if p, err := strconv.Atoi(os.Getenv("BEADS_DOLT_SERVER_PORT")); err == nil {
		env.Port = p
	}
	if self, err := os.Executable(); err == nil {
		env.BrainBinary = self
	}
	if role == "direct" {
		env.UnifiedDB = src.UnifiedDB(ctx)
		env.ProjectID = func(store string) (string, error) { return src.ProjectID(ctx, store) }
	}
	if role == "client" {
		// A client's wrappers hand over to the brain binary on PATH, as 'brain remote install' writes them.
		env.BrainBinary = filepath.Join(home, ".local", "bin", "brain")
		if p := os.Getenv("BRAIN_BINARY"); p != "" {
			env.BrainBinary = p
		}
	}
	return env
}

// runRegistrySync is the reconcile. It is safe to call at any time.
func runRegistrySync(ctx context.Context, opt syncOptions) (*syncReport, error) {
	machine := opt.Machine
	if machine == "" {
		machine = machineName()
	}
	role, _ := opt.Source.Role()
	rep := &syncReport{Machine: machine, Role: role}
	if os.Getenv(envRegistrySyncing) != "" {
		rep.Status = "skipped" // a sync never triggers a sync
		return rep, nil
	}

	if !opt.DryRun {
		release, ok := acquireRegistryLock(opt.Wait)
		if !ok {
			rep.Status = "busy"
			return rep, nil
		}
		defer release()
		_ = os.Setenv(envRegistrySyncing, "1")
		defer func() { _ = os.Unsetenv(envRegistrySyncing) }()
	}

	snap, found, err := opt.Source.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if !found {
		rep.Status = "not-adopted"
		return rep, nil
	}
	applied, _ := readAppliedState()
	rep.PrevEpoch = applied.Epoch
	rep.Epoch, rep.Digest = snap.State.Epoch, snap.State.Digest

	env := newShimEnv(ctx, opt.Source, machine)
	plan, err := buildSyncPlan(env, snap)
	if err != nil {
		return nil, err
	}
	rep.Actions = plan.Actions
	rep.Changed = plan.Changes()
	rep.Orphans, rep.LocalOnly = plan.Orphans, plan.LocalOnly
	rep.ActiveCount = plan.Active

	if opt.DryRun {
		rep.Status = "dry-run"
		rep.WrappersOK = plan.WrappersOK
		rep.Missing = flattenMissing(plan.Missing)
		return rep, nil
	}

	var res applyResult
	if rep.Changed > 0 {
		res = applySyncPlan(env, plan)
		rep.Backup = res.Backup
		rep.Actions = plan.Actions
	}
	for _, f := range res.Failed {
		rep.Failed = append(rep.Failed, fmt.Sprintf("%s %s: %v", f.Action.Kind, f.Action.Name, f.Err))
	}
	// Re-plan against the disk: what is still missing is what the sync could not give.
	after, perr := buildSyncPlan(env, snap)
	if perr == nil {
		rep.WrappersOK = after.WrappersOK
		rep.Missing = flattenMissing(after.Missing)
		rep.Orphans, rep.LocalOnly = after.Orphans, after.LocalOnly
	}
	outstanding := len(rep.Failed) > 0 || len(rep.Missing) > 0 || (perr == nil && after.Changes() > 0)
	if outstanding {
		rep.Status = "partial"
	} else {
		rep.Status = "converged"
	}

	if rep.Status == "converged" {
		now := time.Now().UTC().Format(time.RFC3339)
		_ = writeJSONFileAtomic(homeFile(registryAppliedPath), appliedState{Epoch: snap.State.Epoch, Digest: snap.State.Digest, AppliedAt: now, Machine: machine, Role: role}, 0o644)
	}
	_ = writeJSONFileAtomic(homeFile(registryCachePath), registryCache{Epoch: snap.State.Epoch, Digest: snap.State.Digest,
		At: time.Now().UTC().Format(time.RFC3339), Entries: snap.Entries}, 0o644)

	if rep.Status != "converged" {
		markRegistryFailure()
	} else {
		clearRegistryFailure()
	}
	reportToDatabase(ctx, opt.Source, machine, role, rep, snap)
	if rep.Changed > 0 && rep.Status == "converged" || rep.PrevEpoch != rep.Epoch {
		runSyncHooks(rep)
	}
	return rep, nil
}

func flattenMissing(m map[string][]string) []string {
	var out []string
	for kind, names := range m {
		for _, n := range names {
			out = append(out, kind+":"+n)
		}
	}
	sort.Strings(out)
	return out
}

func reportToDatabase(ctx context.Context, src registrySource, machine, role string, rep *syncReport, snap storeregistry.Snapshot) {
	_, relay := src.Role()
	applied, _ := readAppliedState()
	missing := map[string][]string{}
	for _, m := range rep.Missing {
		kind, name, _ := strings.Cut(m, ":")
		missing[kind] = append(missing[kind], name)
	}
	mj, _ := json.Marshal(missing)
	oj, _ := json.Marshal(rep.Orphans)
	lastErr := ""
	if len(rep.Failed) > 0 {
		lastErr = strings.Join(rep.Failed, "; ")
		if len(lastErr) > 1000 {
			lastErr = lastErr[:1000]
		}
	}
	_ = src.Report(ctx, storeregistry.Machine{
		Machine: machine, Role: role, Relay: relay,
		AppliedEpoch: applied.Epoch, AppliedDigest: applied.Digest,
		WrappersOK: rep.WrappersOK, Missing: string(mj), Orphans: string(oj),
		BdVersion: Version, LastError: lastErr,
	})
	_ = snap
}

// runSyncHooks runs every executable in ~/.config/brain/sync.d after the shims
// changed (the place to kick a long-running consumer such as the beads bridge:
// 'launchctl kickstart -k gui/$UID/<label>'). Best effort, bounded, never fatal.
func runSyncHooks(rep *syncReport) {
	dir := homeFile(registryHooksDir)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		st, err := os.Stat(p)
		if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		c := exec.CommandContext(ctx, p) //nolint:gosec // the operator's own hook directory
		c.Env = append(os.Environ(), "BRAIN_REGISTRY_EPOCH="+strconv.FormatInt(rep.Epoch, 10),
			"BRAIN_REGISTRY_PREV_EPOCH="+strconv.FormatInt(rep.PrevEpoch, 10), "BRAIN_REGISTRY_MACHINE="+rep.Machine)
		c.WaitDelay = time.Second
		_ = c.Run()
		cancel()
	}
}

// printSyncReport renders a report for a human.
func printSyncReport(w io.Writer, rep *syncReport, dryRun bool) {
	switch rep.Status {
	case "not-adopted":
		fmt.Fprintln(w, "store registry: not adopted yet in this database (run 'brain stores reconcile' once)")
		return
	case "busy":
		fmt.Fprintln(w, "store registry: another sync is running here; nothing to do")
		return
	case "skipped":
		return
	}
	if dryRun {
		fmt.Fprintf(w, "plan for %s (%s) against registry epoch %d (applied here: %d):\n", rep.Machine, rep.Role, rep.Epoch, rep.PrevEpoch)
		n := 0
		for _, a := range rep.Actions {
			if a.Op == "" {
				continue
			}
			label := strings.ToUpper(a.Op)
			target := a.Name
			if target == "" {
				target = filepath.Base(a.Path)
			}
			fmt.Fprintf(w, "  %-6s %-8s %-20s %s\n", label, a.Kind, target, a.Reason)
			n++
		}
		if n == 0 {
			fmt.Fprintln(w, "  nothing to do: this machine matches the registry")
		}
		for _, o := range rep.Orphans {
			fmt.Fprintf(w, "  note: %s was removed from the registry; its wrapper is not generated by brain, so it was left alone\n", o)
		}
		for _, l := range rep.LocalOnly {
			fmt.Fprintf(w, "  note: %s is in stores.yaml but not in the registry (local-only; 'brain stores reconcile' adopts it)\n", l)
		}
		return
	}
	fmt.Fprintf(w, "%s: epoch %d → %d, %d change(s)", rep.Machine, rep.PrevEpoch, rep.Epoch, rep.Changed)
	if rep.Backup != "" {
		fmt.Fprintf(w, " (backup: %s)", rep.Backup)
	}
	fmt.Fprintln(w)
	for _, a := range rep.Actions {
		if a.changes() {
			fmt.Fprintf(w, "  %-6s %-8s %s\n", strings.ToUpper(a.Op), a.Kind, strings.TrimSpace(a.Name+" "+a.Reason))
		}
	}
	for _, f := range rep.Failed {
		fmt.Fprintf(w, "  FAILED %s\n", f)
	}
	for _, m := range rep.Missing {
		fmt.Fprintf(w, "  still missing: %s\n", m)
	}
	if rep.Status == "converged" {
		fmt.Fprintf(w, "converged at epoch %d (%d stores)\n", rep.Epoch, rep.ActiveCount)
	}
}
