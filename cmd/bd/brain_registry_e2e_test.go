package main

// End-to-end tests of the store registry against a real (throwaway) Dolt
// server and several temp HOMEs standing in for machines. No real store is
// created, renamed or removed anywhere: every "machine" is a temp directory
// and the database is an isolated sql-server under t.TempDir().
//
// The headline test is the captain's acceptance test: create a store on
// machine A, then machine B runs any ordinary command, and afterwards B's
// registry list contains the store and its command exists, with no manual step.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/storeregistry"
	"github.com/steveyegge/beads/internal/storeregistry/regtest"
)

// fleet is a set of fake machines sharing one database.
type fleet struct {
	t   *testing.T
	srv *regtest.Server
}

type machine struct {
	f    *fleet
	name string
	home string
	db   *sql.DB
}

func newFleet(t *testing.T) *fleet {
	t.Helper()
	if os.Getenv("BRAIN_REGISTRY") != "" {
		t.Setenv("BRAIN_REGISTRY", "")
	}
	f := &fleet{t: t, srv: regtest.New(t)}
	orig := registryDBFunc
	t.Cleanup(func() { registryDBFunc = orig })
	return f
}

// addMachine creates a machine whose HOME already has the `task` store the way
// a long-lived machine does: a directory, an unmanaged (not generated) wrapper,
// a stores.yaml line and an agent-link directory.
func (f *fleet) addMachine(name string) *machine {
	f.t.Helper()
	m := &machine{f: f, name: name, home: f.t.TempDir(), db: f.srv.Open(f.t)}
	for _, d := range []string{".local/bin", ".pi/agent/bin", "data/tasks/.beads", ".config/brain"} {
		if err := os.MkdirAll(filepath.Join(m.home, d), 0o755); err != nil {
			f.t.Fatal(err)
		}
	}
	mustWrite(f.t, filepath.Join(m.home, "data/tasks/.beads/metadata.json"), "{}\n", 0o644)
	mustWrite(f.t, filepath.Join(m.home, ".local/bin/task"), "#!/bin/sh\n# Brain federation wrapper — hand written\nexec bd \"$@\"\n", 0o755)
	if err := os.Symlink(filepath.Join(m.home, ".local/bin/task"), filepath.Join(m.home, ".pi/agent/bin/task")); err != nil {
		f.t.Fatal(err)
	}
	m.use()
	if err := saveStoresRegistry(map[string]storeEntry{"task": {Path: filepath.Join(m.home, "data/tasks/.beads")}}); err != nil {
		f.t.Fatal(err)
	}
	return m
}

// use makes m the machine the next calls act as.
func (m *machine) use() {
	m.f.t.Helper()
	m.f.t.Setenv("HOME", m.home)
	m.f.t.Setenv(envMachine, m.name)
	m.f.t.Setenv(envRegistrySyncing, "")
	_ = os.Unsetenv(envRegistrySyncing)
	db := m.db
	registryDBFunc = func() (*sql.DB, error) { return db, nil }
}

func (m *machine) has(rel string) bool {
	_, err := os.Lstat(filepath.Join(m.home, rel))
	return err == nil
}

func (m *machine) read(rel string) string {
	data, _ := os.ReadFile(filepath.Join(m.home, rel))
	return string(data)
}

func (m *machine) stores() map[string]storeEntry {
	m.use()
	s, err := loadStoresRegistry()
	if err != nil {
		m.f.t.Fatal(err)
	}
	return s
}

// anyCommand is what PersistentPreRunE does on every ordinary command.
func (m *machine) anyCommand() string {
	m.use()
	var out bytes.Buffer
	rideAlongWith(context.Background(), dbSource{db: m.db}, m.db, &out)
	return out.String()
}

func mustWrite(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// adopt runs the one-time reconcile on m, as an operator would on the machine
// whose stores.yaml is the truth.
func (f *fleet) adopt(m *machine) storeregistry.Result {
	f.t.Helper()
	m.use()
	ctx := context.Background()
	for _, s := range []string{"task"} {
		f.mustExec("REPLACE INTO brain_stores (`store`,`registered`,`source_database`,`beads_dir`,`project_id`,`schema_version`,`template_source`,`imported_at`) VALUES (?, 1, 'brain_unified', '', ?, '', 0, '2026-10-21 00:00:00')", s, "proj-"+s)
		f.mustExec("REPLACE INTO brain_unified_metadata (`store`,`key`,value) VALUES (?, '_project_id', ?)", s, "proj-"+s)
	}
	snap, _, err := storeregistry.ReadSnapshot(ctx, m.db)
	if err != nil {
		f.t.Fatal(err)
	}
	plan, err := planReconcile(ctx, m.db, snap, false, reconcileDecisionFile{Decisions: map[string]reconcileDecision{}})
	if err != nil {
		f.t.Fatal(err)
	}
	res, err := applyReconcile(ctx, m.db, plan)
	if err != nil {
		f.t.Fatal(err)
	}
	syncThisMachine(ctx, m.db, res.Epoch)
	return res
}

func (f *fleet) mustExec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.srv.DB.Exec(q, args...); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
}

// createStore does what `brain stores create` does in an adopted fleet, minus
// opening a bd store against the throwaway server.
func (m *machine) createStore(name string) int64 {
	m.f.t.Helper()
	m.use()
	rel := "data/" + name + "/.beads"
	reg := &createRegistry{DB: m.db, Entry: storeregistry.Entry{Name: name, Kind: storeregistry.KindStore, Store: name,
		State: storeregistry.StateActive, RelPath: rel, WrapperKind: storeregistry.WrapperServer}}
	if err := registerCreatedStore(context.Background(), reg, regtest.Database, name, "proj-"+name); err != nil {
		m.f.t.Fatal(err)
	}
	syncThisMachine(context.Background(), m.db, reg.Epoch)
	return reg.Epoch
}

func (m *machine) removeStore(name string) int64 {
	m.f.t.Helper()
	m.use()
	res, err := storeregistry.Mutate(context.Background(), m.db, registryMeta(), storeregistry.PlanRemove(name))
	if err != nil {
		m.f.t.Fatal(err)
	}
	syncThisMachine(context.Background(), m.db, res.Epoch)
	return res.Epoch
}

func (m *machine) renameStore(oldName, newName string) int64 {
	m.f.t.Helper()
	m.use()
	// What `brain stores rename` does locally: move the directory.
	if err := os.Rename(filepath.Join(m.home, "data", oldName), filepath.Join(m.home, "data", newName)); err != nil {
		m.f.t.Fatal(err)
	}
	res, err := registryRenameStore(context.Background(), m.db, oldName, newName, oldName, "data/"+newName+"/.beads", false)
	if err != nil {
		m.f.t.Fatal(err)
	}
	syncThisMachine(context.Background(), m.db, res.Epoch)
	return res.Epoch
}

// fakeBD is a stand-in bd that reports how a wrapper resolved it.
func fakeBDScript(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-bd")
	mustWrite(t, p, "#!/bin/sh\necho \"BEADS_DIR=$BEADS_DIR BD_NAME=$BD_NAME ARGS=$*\"\n", 0o755)
	return p
}

func runWrapper(t *testing.T, m *machine, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(m.home, ".local/bin", name), args...)
	cmd.Env = append(os.Environ(), "HOME="+m.home, "BD_BIN="+fakeBDScript(t))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper %s: %v\n%s", name, err, out)
	}
	return strings.TrimSpace(string(out))
}

// ---------------------------------------------------------------------------
// The captain's acceptance test.
// ---------------------------------------------------------------------------

func TestAutomaticPropagation_CreateRenameRemove(t *testing.T) {
	f := newFleet(t)
	a, b, c := f.addMachine("mini0"), f.addMachine("mini1"), f.addMachine("laptop")
	f.adopt(a)
	// B and C ride along once so everyone starts converged on epoch 1.
	b.anyCommand()
	c.anyCommand()

	// ---- A creates a store. Nobody runs anything on B or C by hand.
	epoch := a.createStore("recipes")
	for _, rel := range []string{".local/bin/recipes", ".pi/agent/bin/recipes", "data/recipes/.beads/metadata.json"} {
		if !a.has(rel) {
			t.Fatalf("machine A is missing %s right after create", rel)
		}
	}
	if _, ok := a.stores()["recipes"]; !ok {
		t.Fatal("machine A's stores.yaml does not list the new store")
	}

	// ---- B runs ANY ordinary brain command (here: the ride-along every command makes).
	said := b.anyCommand()
	if !strings.Contains(said, "store list changed (epoch 1 -> 2): running brain stores sync") {
		t.Errorf("B's command should say what it did, got %q", said)
	}
	if _, ok := b.stores()["recipes"]; !ok {
		t.Fatal("B's 'brain stores list' does not show the store created on A")
	}
	if !strings.Contains(b.read(".config/brain/stores.env"), `BRAIN_STORE_RECIPES=`) {
		t.Error("B's stores.env was not regenerated")
	}
	if !b.has(".pi/agent/bin/recipes") {
		t.Error("B has no agent link for the new store")
	}
	if got := runWrapper(t, b, "recipes", "list"); !strings.Contains(got, "BD_NAME=recipes") ||
		!strings.Contains(got, filepath.Join(b.home, "data/recipes/.beads")) {
		t.Errorf("B's new wrapper does not resolve the store: %q", got)
	}
	if !strings.Contains(b.read("data/recipes/.beads/metadata.json"), "proj-recipes") {
		t.Errorf("B's BEADS_DIR skeleton does not carry the store's project identity:\n%s", b.read("data/recipes/.beads/metadata.json"))
	}
	// Runtime resolution finds it too (no wrapper needed).
	b.use()
	if e, ok := cachedEntry("recipes"); !ok || e.RelPath != "data/recipes/.beads" {
		t.Errorf("runtime resolution cannot see the store: %+v %v", e, ok)
	}
	// And B told the database it is converged: the creator's "k of n".
	if line := fleetConvergenceLine(context.Background(), a.db, epoch); !strings.Contains(line, "2 of 3 machines converged") {
		t.Errorf("want 2 of 3 converged (A and B; C has not run a command), got %q", line)
	}

	// ---- C was offline: its first command catches up (it missed nothing that matters).
	c.anyCommand()
	if _, ok := c.stores()["recipes"]; !ok || !c.has(".local/bin/recipes") {
		t.Fatal("the offline machine did not catch up on its first command")
	}
	if line := fleetConvergenceLine(context.Background(), a.db, epoch); !strings.Contains(line, "3 of 3 machines converged") {
		t.Errorf("want 3 of 3 converged, got %q", line)
	}

	// ---- Rename propagates the same way.
	a.renameStore("recipes", "meals")
	b.anyCommand()
	if !b.has(".local/bin/meals") || b.has(".local/bin/recipes") {
		t.Errorf("B should have the new name and not the old: meals=%v recipes=%v", b.has(".local/bin/meals"), b.has(".local/bin/recipes"))
	}
	if _, ok := b.stores()["meals"]; !ok {
		t.Error("B's stores.yaml lacks the new name")
	}
	if _, ok := b.stores()["recipes"]; ok {
		t.Error("B's stores.yaml still lists the old name")
	}
	if got := runWrapper(t, b, "meals", "list"); !strings.Contains(got, "BD_NAME=meals") {
		t.Errorf("renamed wrapper: %q", got)
	}
	// The database keeps the namespace under its old name and resolves the new one to it.
	var canonical string
	if err := f.srv.DB.QueryRow("SELECT `store` FROM brain_store_aliases WHERE alias = 'meals'").Scan(&canonical); err != nil || canonical != "recipes" {
		t.Errorf("rename must leave an alias meals -> recipes in the database: %q %v", canonical, err)
	}
	// B's old data directory is untouched (never moved or deleted on another machine).
	if !b.has("data/recipes/.beads/metadata.json") {
		t.Error("another machine's rename must not delete the old data directory")
	}

	// ---- Remove propagates: generated wrapper, link and list entry go; data and rows stay.
	a.createStore("scratchpad")
	b.anyCommand()
	c.anyCommand()
	if !b.has(".local/bin/scratchpad") {
		t.Fatal("setup: B should have scratchpad")
	}
	a.removeStore("scratchpad")
	for _, m := range []*machine{a, b, c} {
		m.anyCommand()
		if m.has(".local/bin/scratchpad") || m.has(".pi/agent/bin/scratchpad") {
			t.Errorf("%s still has the removed store's wrapper or link", m.name)
		}
		if _, ok := m.stores()["scratchpad"]; ok {
			t.Errorf("%s still lists the removed store", m.name)
		}
		if !m.has("data/scratchpad/.beads/metadata.json") {
			t.Errorf("%s lost the removed store's data directory: remove must never delete data", m.name)
		}
		if m.has(".local/bin/task") == false {
			t.Errorf("%s lost an unrelated wrapper", m.name)
		}
	}
	var n int
	if err := f.srv.DB.QueryRow("SELECT COUNT(*) FROM brain_stores WHERE `store` = 'scratchpad'").Scan(&n); err != nil || n != 1 {
		t.Errorf("remove must never delete database rows: count=%d err=%v", n, err)
	}

	// ---- Proof: the fleet doctor says CONVERGED.
	rep := fleetDoctorFor(t, f, a, b, c)
	if !rep.Converged {
		t.Fatalf("fleet should be converged: %s", rep.Summary)
	}
	if !strings.HasPrefix(rep.Summary, "CONVERGED epoch ") {
		t.Errorf("summary: %q", rep.Summary)
	}
}

var fleetRunMu sync.Mutex

// fleetDoctorFor runs the fleet doctor with the machines served in-process.
func fleetDoctorFor(t *testing.T, f *fleet, ms ...*machine) *fleetReport {
	t.Helper()
	var roster []fleetMember
	byName := map[string]*machine{}
	for _, m := range ms {
		roster = append(roster, fleetMember{Name: m.name, SSH: m.name})
		byName[m.name] = m
	}
	ctx := context.Background()
	ms[0].use()
	run := func(ctx context.Context, fm fleetMember, args ...string) ([]byte, error) {
		// The fake machines share this process's HOME: serve one at a time.
		fleetRunMu.Lock()
		defer fleetRunMu.Unlock()
		m := byName[fm.Name]
		if m == nil {
			return nil, fmt.Errorf("ssh: could not resolve hostname %s", fm.Name)
		}
		m.use()
		v := buildVerifyReport(ctx, dbSource{db: m.db}, 0)
		return jsonBytes(v), nil
	}
	// Run as a machine that is not in the roster, so every member goes through the runner.
	t.Setenv(envMachine, "doctor-host")
	rep, err := runFleetDoctor(ctx, dbSource{db: ms[0].db}, ms[0].db, roster, nil, run, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// ---------------------------------------------------------------------------
// Offline catch-up at the next poll, half-applied syncs, idempotence.
// ---------------------------------------------------------------------------

func TestOfflineMachineCatchesUpAtNextPoll(t *testing.T) {
	f := newFleet(t)
	a, d := f.addMachine("mini0"), f.addMachine("macmini")
	f.adopt(a)
	d.anyCommand()
	// D is offline through four changes.
	a.createStore("one")
	a.createStore("two")
	a.renameStore("one", "uno")
	a.removeStore("two")

	// The poller fires: `brain stores sync --quiet --trigger timer`.
	d.use()
	rep, err := runRegistrySync(context.Background(), syncOptions{Source: dbSource{db: d.db}, Trigger: "timer"})
	if err != nil || rep.Status != "converged" {
		t.Fatalf("poll: %+v %v", rep, err)
	}
	if rep.PrevEpoch != 1 || rep.Epoch != 5 {
		t.Errorf("one step from epoch 1 to 5, got %d -> %d", rep.PrevEpoch, rep.Epoch)
	}
	if !d.has(".local/bin/uno") || d.has(".local/bin/two") || d.has(".local/bin/one") {
		t.Errorf("catch-up result wrong: uno=%v two=%v one=%v", d.has(".local/bin/uno"), d.has(".local/bin/two"), d.has(".local/bin/one"))
	}
}

func TestHalfAppliedSyncIsFinishedByTheNextRun(t *testing.T) {
	f := newFleet(t)
	a, b := f.addMachine("mini0"), f.addMachine("mini1")
	f.adopt(a)
	b.anyCommand()
	a.createStore("alpha")
	a.createStore("beta")

	// Break B halfway: a regular file where beta's data directory must go.
	b.use()
	mustWrite(t, filepath.Join(b.home, "data/beta"), "in the way", 0o644)
	rep, err := runRegistrySync(context.Background(), syncOptions{Source: dbSource{db: b.db}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != "partial" {
		t.Fatalf("status = %s, want partial", rep.Status)
	}
	if !b.has(".local/bin/alpha") {
		t.Error("alpha should have been applied even though beta failed")
	}
	if b.has(".local/bin/beta") {
		t.Error("beta's wrapper must wait for its directory")
	}
	if got, _ := readAppliedState(); got.Epoch == 3 {
		t.Error("a partial sync must not claim the epoch as applied")
	}
	ms, _ := storeregistry.ReadMachines(context.Background(), f.srv.DB)
	var bAck *storeregistry.Machine
	for i := range ms {
		if ms[i].Machine == "mini1" {
			bAck = &ms[i]
		}
	}
	if bAck == nil || bAck.AppliedEpoch >= 3 || bAck.LastError == "" {
		t.Errorf("the partial state must be reported to the database: %+v", bAck)
	}

	// Fix the obstruction; the next run finishes the job and converges.
	if err := os.Remove(filepath.Join(b.home, "data/beta")); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(b.home, ".config/brain/registry.failed"))
	rep, err = runRegistrySync(context.Background(), syncOptions{Source: dbSource{db: b.db}})
	if err != nil || rep.Status != "converged" {
		t.Fatalf("second run: %+v %v", rep, err)
	}
	if !b.has(".local/bin/beta") {
		t.Error("beta should exist after the second run")
	}
}

func TestSyncIsIdempotent(t *testing.T) {
	f := newFleet(t)
	a, b := f.addMachine("mini0"), f.addMachine("mini1")
	f.adopt(a)
	a.createStore("recipes")
	b.use()
	if _, err := runRegistrySync(context.Background(), syncOptions{Source: dbSource{db: b.db}}); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, b.home)
	rep, err := runRegistrySync(context.Background(), syncOptions{Source: dbSource{db: b.db}})
	if err != nil || rep.Changed != 0 || rep.Status != "converged" {
		t.Fatalf("second sync must change nothing: %+v %v", rep, err)
	}
	if after := snapshotTree(t, b.home); after != before {
		t.Errorf("the tree changed on an idempotent run:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestDryRunChangesNothingAndPrintsAPlan(t *testing.T) {
	f := newFleet(t)
	a, b := f.addMachine("mini0"), f.addMachine("mini1")
	f.adopt(a)
	a.createStore("recipes")
	b.use()
	before := snapshotTree(t, b.home)
	rep, err := runRegistrySync(context.Background(), syncOptions{Source: dbSource{db: b.db}, DryRun: true})
	if err != nil || rep.Status != "dry-run" {
		t.Fatalf("%+v %v", rep, err)
	}
	if snapshotTree(t, b.home) != before {
		t.Error("--dry-run changed the disk")
	}
	var out bytes.Buffer
	printSyncReport(&out, rep, true)
	for _, want := range []string{"plan for mini1", "CREATE", "recipes", "BEADS_DIR skeleton", "agent link missing"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the plan should mention %q:\n%s", want, out.String())
		}
	}
}

func TestSyncNeverTouchesUnmanagedFilesAndReportsOrphans(t *testing.T) {
	f := newFleet(t)
	a, b := f.addMachine("mini0"), f.addMachine("mini1")
	f.adopt(a)
	a.createStore("recipes")
	// B already has a hand-written `recipes` command and a hand-written wrapper
	// for a store that is later removed.
	b.use()
	mustWrite(t, filepath.Join(b.home, ".local/bin/recipes"), "#!/bin/sh\n# mine\necho hand-written\n", 0o755)
	if _, err := runRegistrySync(context.Background(), syncOptions{Source: dbSource{db: b.db}}); err != nil {
		t.Fatal(err)
	}
	if got := b.read(".local/bin/recipes"); !strings.Contains(got, "hand-written") {
		t.Errorf("sync overwrote an unmanaged wrapper:\n%s", got)
	}
	a.removeStore("recipes")
	b.use()
	rep, err := runRegistrySync(context.Background(), syncOptions{Source: dbSource{db: b.db}})
	if err != nil {
		t.Fatal(err)
	}
	if !b.has(".local/bin/recipes") {
		t.Error("sync removed a wrapper brain did not generate")
	}
	if len(rep.Orphans) != 1 || rep.Orphans[0] != "recipes" {
		t.Errorf("the unmanaged wrapper should be reported as an orphan: %v", rep.Orphans)
	}
	// An unmanaged orphan is a note, not a failure to converge.
	if rep.Status != "converged" {
		t.Errorf("status = %s", rep.Status)
	}
}

func TestBackupAndRestoreOfReplacedWrapper(t *testing.T) {
	f := newFleet(t)
	a, b := f.addMachine("mini0"), f.addMachine("mini1")
	f.adopt(a)
	a.createStore("recipes")
	b.use()
	if _, err := runRegistrySync(context.Background(), syncOptions{Source: dbSource{db: b.db}}); err != nil {
		t.Fatal(err)
	}
	// A generated-but-stale wrapper (older template) is replaced, with a backup.
	stale := "#!/bin/sh\n# Auto-generated by 'brain stores create recipes'.\nexec env BEADS_DIR=x BD_NAME=recipes bd \"$@\"\n"
	mustWrite(t, filepath.Join(b.home, ".local/bin/recipes"), stale, 0o755)
	rep, err := runRegistrySync(context.Background(), syncOptions{Source: dbSource{db: b.db}})
	if err != nil || rep.Backup == "" {
		t.Fatalf("expected a backup dir: %+v %v", rep, err)
	}
	saved, err := os.ReadFile(filepath.Join(rep.Backup, ".local/bin/recipes"))
	if err != nil || string(saved) != stale {
		t.Errorf("the replaced wrapper was not backed up: %q %v", saved, err)
	}
	if _, err := os.Stat(filepath.Join(rep.Backup, "restore.sh")); err != nil {
		t.Errorf("no restore.sh: %v", err)
	}
}

func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var sb strings.Builder
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if strings.HasPrefix(rel, ".local/share/brain/registry-backup") || strings.HasSuffix(rel, "registry.applied") || strings.HasSuffix(rel, "registry.cache.json") {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			tgt, _ := os.Readlink(p)
			fmt.Fprintf(&sb, "L %s -> %s\n", rel, tgt)
		} else if !info.IsDir() {
			data, _ := os.ReadFile(p)
			fmt.Fprintf(&sb, "F %s %d %x\n", rel, info.Mode().Perm(), data)
		}
		return nil
	})
	return sb.String()
}

// ---------------------------------------------------------------------------
// Ride-along: catch-up, offline, concurrency, loop protection, cost.
// ---------------------------------------------------------------------------

func TestRideAlongOfflineNeverBlocksAndWarnsOncePerHour(t *testing.T) {
	f := newFleet(t)
	a, b := f.addMachine("mini0"), f.addMachine("mini1")
	f.adopt(a)
	b.use()
	// An unreachable database: the read errors out. The command must carry on.
	var out bytes.Buffer
	start := time.Now()
	rideAlongWith(context.Background(), dbSource{db: b.db}, erroringTx{}, &out)
	if time.Since(start) > 3*time.Second {
		t.Errorf("an unreachable database blocked the command for %s", time.Since(start))
	}
	if !strings.Contains(out.String(), "store registry unreachable") || !strings.Contains(out.String(), "continuing") {
		t.Errorf("want one warning, got %q", out.String())
	}
	out.Reset()
	rideAlongWith(context.Background(), dbSource{db: b.db}, erroringTx{}, &out)
	if out.Len() != 0 {
		t.Errorf("the second offline command within the hour must stay quiet, got %q", out.String())
	}
	if b.has(".local/bin/task") == false {
		t.Error("local shims must be untouched while offline")
	}
}

type erroringTx struct{}

func (erroringTx) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, fmt.Errorf("dial tcp: i/o timeout")
}
func (erroringTx) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, fmt.Errorf("dial tcp: i/o timeout")
}
func (erroringTx) QueryRowContext(ctx context.Context, q string, a ...any) *sql.Row {
	// A closed pool returns a Row whose Scan fails, like a dead connection.
	db, _ := sql.Open("mysql", "root@tcp(127.0.0.1:1)/x?timeout=1s")
	return db.QueryRowContext(ctx, q, a...)
}

func TestRideAlongCurrentMachineIsSilentAndCheap(t *testing.T) {
	f := newFleet(t)
	a := f.addMachine("mini0")
	f.adopt(a)
	var out bytes.Buffer
	a.use()
	rideAlongWith(context.Background(), dbSource{db: a.db}, a.db, &out)
	if out.Len() != 0 {
		t.Errorf("a converged machine must not print anything: %q", out.String())
	}
	const n = 200
	start := time.Now()
	for i := 0; i < n; i++ {
		rideAlongWith(context.Background(), dbSource{db: a.db}, a.db, &out)
	}
	per := time.Since(start) / n
	t.Logf("ride-along check on a converged machine: %s per command", per)
	if per > 5*time.Millisecond {
		t.Errorf("the epoch check costs %s per command; want a few ms at most", per)
	}
}

func TestRideAlongTwoConcurrentCommandsSyncOnce(t *testing.T) {
	f := newFleet(t)
	a, b := f.addMachine("mini0"), f.addMachine("mini1")
	f.adopt(a)
	b.anyCommand()
	for i := 0; i < 6; i++ {
		a.createStore(fmt.Sprintf("s%d", i))
	}
	b.use()
	var wg sync.WaitGroup
	reports := make([]*syncReport, 4)
	for i := range reports {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each "command" is a sync attempt that does not wait for the lock.
			r, err := runRegistrySync(context.Background(), syncOptions{Source: dbSource{db: b.db}})
			if err != nil {
				t.Error(err)
				return
			}
			reports[i] = r
		}(i)
	}
	wg.Wait()
	applied, busy := 0, 0
	for _, r := range reports {
		if r == nil {
			continue
		}
		switch {
		case r.Status == "busy":
			busy++
		case r.Changed > 0:
			applied++
		}
	}
	if applied != 1 {
		t.Errorf("exactly one of the concurrent commands should have applied the changes, got %d (busy %d)", applied, busy)
	}
	if !b.has(".local/bin/s5") {
		t.Error("the machine did not converge")
	}
}

func TestSyncNeverTriggersSync(t *testing.T) {
	f := newFleet(t)
	a, b := f.addMachine("mini0"), f.addMachine("mini1")
	f.adopt(a)
	a.createStore("recipes")
	b.use()
	// While a sync runs, the guard variable is set for everything it starts.
	t.Setenv(envRegistrySyncing, "1")
	if !registryRideAlongSkipped(brainStoresListCmd) {
		t.Error("the ride-along must be skipped while a sync is running")
	}
	rep, err := runRegistrySync(context.Background(), syncOptions{Source: dbSource{db: b.db}})
	if err != nil || rep.Status != "skipped" {
		t.Errorf("a nested sync must be skipped: %+v %v", rep, err)
	}
	if b.has(".local/bin/recipes") {
		t.Error("the nested sync applied something")
	}
	// And the registry commands themselves never ride along.
	_ = os.Unsetenv(envRegistrySyncing)
	for _, cmd := range []*cobra.Command{brainStoresSyncCmd, brainStoresVerifyCmd, brainStoresDoctorCmd, brainStoresReconcileCmd, brainStoresRegistryExportCmd} {
		if !registryRideAlongSkipped(cmd) {
			t.Errorf("%s must not trigger the ride-along", cmd.CommandPath())
		}
	}
	if registryRideAlongSkipped(brainStoresListCmd) {
		t.Error("'stores list' is an ordinary command and rides along")
	}
}

func TestRideAlongNotifyPolicyOnlyTells(t *testing.T) {
	f := newFleet(t)
	a, b := f.addMachine("mini0"), f.addMachine("mini1")
	f.adopt(a)
	a.createStore("recipes")
	t.Setenv(envRegistryPolicy, "notify")
	said := b.anyCommand()
	if !strings.Contains(said, "run 'brain stores sync'") {
		t.Errorf("notify policy should only tell: %q", said)
	}
	if b.has(".local/bin/recipes") {
		t.Error("notify policy must not sync")
	}
}

func TestRideAlongOffSwitch(t *testing.T) {
	t.Setenv(envRegistryOff, "off")
	if !registryRideAlongSkipped(brainStoresListCmd) {
		t.Error("BRAIN_REGISTRY=off must disable the ride-along")
	}
}

// ---------------------------------------------------------------------------
// Fleet doctor: converged / lagging / drifted / asleep-ok / unreachable.
// ---------------------------------------------------------------------------

func TestFleetDoctorClassifiesEveryMachine(t *testing.T) {
	f := newFleet(t)
	a, b, c := f.addMachine("mini0"), f.addMachine("mini1"), f.addMachine("laptop")
	f.adopt(a)
	b.anyCommand()
	c.anyCommand()
	a.createStore("recipes")
	b.anyCommand()
	// c is behind (has not run a command since the create).

	rep := fleetDoctorFor(t, f, a, b, c)
	got := map[string]fleetMachineResult{}
	for _, r := range rep.Machines {
		got[r.Name] = r
	}
	if got["mini0"].Verdict != verdictConverged || got["mini1"].Verdict != verdictConverged {
		t.Errorf("A and B converged: %+v", got)
	}
	if got["laptop"].Verdict != verdictLagging {
		t.Errorf("laptop should be lagging: %+v", got["laptop"])
	}
	if got["laptop"].Fix != "brain stores sync --machine laptop" {
		t.Errorf("exact fix command wanted, got %q", got["laptop"].Fix)
	}
	if rep.Converged || len(rep.Failing) != 1 || rep.Failing[0] != "laptop" {
		t.Errorf("fleet must not be converged: %+v", rep)
	}
	if !strings.HasPrefix(rep.Summary, "NOT CONVERGED") || !strings.Contains(rep.Summary, "laptop lagging") {
		t.Errorf("summary: %q", rep.Summary)
	}

	// Drift: same epoch, but a wrapper deleted by hand on B.
	c.anyCommand()
	if err := os.Remove(filepath.Join(b.home, ".local/bin/recipes")); err != nil {
		t.Fatal(err)
	}
	rep = fleetDoctorFor(t, f, a, b, c)
	for _, r := range rep.Machines {
		if r.Name == "mini1" {
			if r.Verdict != verdictDrifted || !strings.Contains(r.Detail, "wrappers:recipes") || r.Fix != "brain stores sync --machine mini1" {
				t.Errorf("mini1 should be drifted with the exact fix: %+v", r)
			}
		}
	}
	// Fixing it by the printed command converges.
	b.use()
	if _, err := runRegistrySync(context.Background(), syncOptions{Source: dbSource{db: b.db}}); err != nil {
		t.Fatal(err)
	}
	if rep = fleetDoctorFor(t, f, a, b, c); !rep.Converged {
		t.Errorf("after the fix: %s", rep.Summary)
	}
}

func TestFleetDoctorUnreachableMachines(t *testing.T) {
	f := newFleet(t)
	a, b := f.addMachine("mini0"), f.addMachine("macbook")
	f.adopt(a)
	b.anyCommand() // macbook acks epoch 1, then goes to sleep

	ctx := context.Background()
	roster := []fleetMember{{Name: "mini0", SSH: "mini0"}, {Name: "macbook", SSH: "macbook"}}
	run := func(ctx context.Context, fm fleetMember, args ...string) ([]byte, error) {
		if fm.Name == "macbook" {
			return nil, fmt.Errorf("ssh: connect to host macbook port 22: Operation timed out")
		}
		fleetRunMu.Lock()
		defer fleetRunMu.Unlock()
		a.use()
		return jsonBytes(buildVerifyReport(ctx, dbSource{db: a.db}, 0)), nil
	}
	t.Setenv(envMachine, "doctor-host")
	a.use()
	t.Setenv(envMachine, "doctor-host")

	// Nothing changed since its last ack: asleep-ok, not a failure.
	rep, err := runFleetDoctor(ctx, dbSource{db: a.db}, a.db, roster, nil, run, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Converged {
		t.Errorf("an unreachable machine that missed nothing is not a failure: %s", rep.Summary)
	}
	for _, r := range rep.Machines {
		if r.Name == "macbook" && r.Verdict != verdictAsleepOK {
			t.Errorf("macbook: %+v", r)
		}
	}

	// A change lands while it sleeps: now it is a failure, and says how stale.
	a.createStore("recipes")
	t.Setenv(envMachine, "doctor-host")
	rep, err = runFleetDoctor(ctx, dbSource{db: a.db}, a.db, roster, nil, run, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Converged {
		t.Fatal("a sleeping machine that missed a change must fail the proof")
	}
	for _, r := range rep.Machines {
		if r.Name == "macbook" && (r.Verdict != verdictUnreachable || !strings.Contains(r.Detail, "last report")) {
			t.Errorf("macbook: %+v", r)
		}
	}
}

func TestFleetDoctorReportsRegistryInconsistencyFirst(t *testing.T) {
	f := newFleet(t)
	a := f.addMachine("mini0")
	f.adopt(a)
	// A database store the registry does not know.
	f.mustExec("REPLACE INTO brain_stores (`store`,`registered`,`source_database`,`beads_dir`,`project_id`,`schema_version`,`template_source`,`imported_at`) VALUES ('ghost', 1, 'brain_unified', '', 'p', '', 0, '2026-10-21 00:00:00')")
	rep := fleetDoctorFor(t, f, a)
	if rep.Converged || len(rep.Consistent) != 1 || !strings.Contains(rep.Consistent[0], "ghost") {
		t.Errorf("a registered store missing from the registry is a registry bug: %+v", rep)
	}
}

func TestRosterDefaultsAndValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, err := loadFleetRoster("", nil)
	if err != nil || len(m) != 3 || m[0].Name != "mini0" || m[1].Name != "mini1" || m[2].Name != "macbook" {
		t.Fatalf("default roster: %+v %v", m, err)
	}
	only, err := loadFleetRoster("", []string{"macbook"})
	if err != nil || len(only) != 1 || only[0].SSH != "macbook" {
		t.Fatalf("--machines: %+v %v", only, err)
	}
	p := filepath.Join(t.TempDir(), "fleet.yaml")
	mustWrite(t, p, "machines:\n  - name: lnx\n    ssh: me@lnx.example.net\n  - name: bad\n    ssh: \"-oProxyCommand=evil\"\n", 0o644)
	if _, err := loadFleetRoster(p, nil); err == nil {
		t.Error("an ssh target that looks like an option must be refused")
	}
	mustWrite(t, p, "machines:\n  - name: lnx\n    ssh: me@lnx.example.net\n", 0o644)
	if m, err := loadFleetRoster(p, nil); err != nil || len(m) != 1 || m[0].SSH != "me@lnx.example.net" {
		t.Errorf("configured roster: %+v %v", m, err)
	}
}

func jsonBytes(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}
