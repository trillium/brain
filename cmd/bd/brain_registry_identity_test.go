package main

// Tests of the identity row (`_project_id`) of registered stores and of the
// poller checks (incident 2026-11-24): every writer of a registry entry writes
// the identity in the same transaction, the doctor FAILs a store without one
// and names the repair, --repair-identity never overwrites, sync installs the
// poller, and the fleet doctor flags a machine without a working poller.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storeregistry"
	"github.com/steveyegge/beads/internal/storeregistry/regtest"
)

func storeEntryFor(name string) storeregistry.Entry {
	return storeregistry.Entry{Name: name, Kind: storeregistry.KindStore, Store: name,
		State: storeregistry.StateActive, RelPath: "data/" + name + "/.beads", WrapperKind: storeregistry.WrapperServer}
}

func writeProjectID(t *testing.T, m *machine, store, id string) {
	t.Helper()
	mustWrite(t, filepath.Join(m.home, "data", store, ".beads", "metadata.json"), `{"project_id":"`+id+`"}`+"\n", 0o644)
}

func identityRow(t *testing.T, f *fleet, store string) (string, bool) {
	t.Helper()
	var v string
	err := f.srv.DB.QueryRow("SELECT value FROM brain_unified_metadata WHERE `store` = ? AND `key` = '_project_id'", store).Scan(&v)
	if err != nil {
		return "", false
	}
	return v, true
}

func TestStoresAddWritesIdentityInTheRegistryTransaction(t *testing.T) {
	f := newFleet(t)
	a := f.addMachine("mini0")
	f.adopt(a)
	writeProjectID(t, a, "notes", "p-notes")

	meta := registryMeta()
	meta.InTx = identityBackfillInTx([]storeregistry.Entry{storeEntryFor("notes")})
	if _, err := storeregistry.Mutate(context.Background(), a.db, meta, storeregistry.PlanCreate(storeEntryFor("notes"))); err != nil {
		t.Fatal(err)
	}
	if v, ok := identityRow(t, f, "notes"); !ok || v != "p-notes" {
		t.Fatalf("identity row after add = %q, %v", v, ok)
	}

	// A failure later in the same transaction rolls the identity back with the entry.
	writeProjectID(t, a, "ghost", "p-ghost")
	meta = registryMeta()
	meta.InTx = chainInTx(identityBackfillInTx([]storeregistry.Entry{storeEntryFor("ghost")}),
		func(context.Context, storeregistry.DBTX) error { return errors.New("boom") })
	if _, err := storeregistry.Mutate(context.Background(), a.db, meta, storeregistry.PlanCreate(storeEntryFor("ghost"))); err == nil {
		t.Fatal("expected the mutation to fail")
	}
	if _, ok := identityRow(t, f, "ghost"); ok {
		t.Error("identity row survived a rolled-back registry write")
	}
	snap, _, _ := storeregistry.ReadSnapshot(context.Background(), a.db)
	for _, e := range snap.Entries {
		if e.Store == "ghost" {
			t.Error("registry entry survived the rollback")
		}
	}
}

func TestStoresAddRejectsMissingProjectIDBeforeLocalWrite(t *testing.T) {
	f := newFleet(t)
	a := f.addMachine("mini0")
	f.adopt(a)
	a.use()
	for _, tc := range []struct {
		name  string
		dir   string
		setup func(*testing.T)
	}{
		{name: "adopted", dir: filepath.Join(a.home, "data", "notes", ".beads")},
		{name: "registry off", dir: filepath.Join(a.home, "data", "notes", ".beads"), setup: func(t *testing.T) { t.Setenv(envRegistryOff, "off") }},
		{name: "local only", dir: filepath.Join(a.home, "data", "notes", ".beads"), setup: func(t *testing.T) {
			old := storesLocalOnly
			storesLocalOnly = true
			t.Cleanup(func() { storesLocalOnly = old })
		}},
		{name: "outside home", dir: filepath.Join(t.TempDir(), ".beads")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup(t)
			}
			before, err := os.ReadFile(homeFile(storesYamlPath))
			if err != nil {
				t.Fatal(err)
			}
			err = brainStoresAddCmd.RunE(brainStoresAddCmd, []string{"notes", tc.dir})
			if err == nil || !strings.Contains(err.Error(), "notes") || !strings.Contains(err.Error(), "metadata.json") {
				t.Fatalf("missing identity error = %v", err)
			}
			after, err := os.ReadFile(homeFile(storesYamlPath))
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Error("stores.yaml changed after rejected add")
			}
		})
	}
}

func TestReconcileRejectsMissingProjectIDBeforeAdoption(t *testing.T) {
	if port := os.Getenv("BRAIN_RECONCILE_TEST_PORT"); port != "" {
		db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%s)/%s", port, regtest.Database))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		registryDBFunc = func() (*sql.DB, error) { return db, nil }
		rootCtx = context.Background()
		reconcileApply = true
		brainStoresReconcileCmd.Run(brainStoresReconcileCmd, nil)
		t.Fatal("reconcile unexpectedly returned")
	}

	f := newFleet(t)
	a := f.addMachine("mini0")
	cmd := exec.Command(os.Args[0], "-test.run=^TestReconcileRejectsMissingProjectIDBeforeAdoption$")
	cmd.Env = append(os.Environ(),
		"BRAIN_RECONCILE_TEST_PORT="+fmt.Sprint(f.srv.Port()),
		"HOME="+a.home,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("reconcile unexpectedly succeeded")
	}
	if !strings.Contains(string(out), "project_id") {
		t.Fatalf("reconcile output = %q", out)
	}
	_, found, err := storeregistry.ReadSnapshot(context.Background(), a.db)
	if err != nil || found {
		t.Fatalf("registry adopted after rejected reconcile: found=%v err=%v", found, err)
	}
}

func TestStoresCreateWritesIdentityWithTheEntry(t *testing.T) {
	f := newFleet(t)
	a := f.addMachine("mini0")
	f.adopt(a)
	a.createStore("recipes")
	if v, ok := identityRow(t, f, "recipes"); !ok || v != "proj-recipes" {
		t.Fatalf("identity row after create = %q, %v", v, ok)
	}
}

func TestIdentityBackfillNeverOverwritesAnExistingRow(t *testing.T) {
	f := newFleet(t)
	a := f.addMachine("mini0")
	f.adopt(a)
	f.mustExec("REPLACE INTO brain_unified_metadata (`store`,`key`,value) VALUES ('notes', '_project_id', 'db-id')")
	writeProjectID(t, a, "notes", "file-id")
	meta := registryMeta()
	meta.InTx = identityBackfillInTx([]storeregistry.Entry{storeEntryFor("notes")})
	if _, err := storeregistry.Mutate(context.Background(), a.db, meta, storeregistry.PlanCreate(storeEntryFor("notes"))); err != nil {
		t.Fatal(err)
	}
	if v, _ := identityRow(t, f, "notes"); v != "db-id" {
		t.Errorf("existing identity overwritten: %q", v)
	}
}

func TestReconcileWritesIdentityForAdoptedStores(t *testing.T) {
	f := newFleet(t)
	a := f.addMachine("mini0")
	a.use()
	// task has a metadata.json id on this machine but no row in the database yet.
	mustWrite(t, filepath.Join(a.home, "data/task/.beads/metadata.json"), `{"project_id":"p-task"}`+"\n", 0o644)
	if err := os.MkdirAll(filepath.Join(a.home, "data/task/.beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.mustExec("REPLACE INTO brain_stores (`store`,`registered`,`source_database`,`beads_dir`,`project_id`,`schema_version`,`template_source`,`imported_at`) VALUES ('task', 1, 'brain_unified', '', 'p-task', '', 0, '2026-10-21 00:00:00')")
	meta := registryMeta()
	e := storeEntryFor("task")
	e.RelPath = "data/task/.beads"
	meta.InTx = identityBackfillInTx([]storeregistry.Entry{e})
	if _, err := storeregistry.Mutate(context.Background(), a.db, meta, storeregistry.PlanBackfill([]storeregistry.Entry{e})); err != nil {
		t.Fatal(err)
	}
	if v, ok := identityRow(t, f, "task"); !ok || v != "p-task" {
		t.Fatalf("reconcile did not write the identity: %q, %v", v, ok)
	}
}

func TestDoctorFailsAStoreWithoutIdentityAndNamesTheRepair(t *testing.T) {
	f := newFleet(t)
	a := f.addMachine("mini0")
	f.adopt(a)
	f.mustExec("DELETE FROM brain_unified_metadata WHERE `store` = 'task' AND `key` = '_project_id'")

	snap, _, _ := storeregistry.ReadSnapshot(context.Background(), a.db)
	probs := identityProblems(context.Background(), a.db, snap.Entries)
	if len(probs) != 1 || probs[0].Store != "task" {
		t.Fatalf("problems = %+v", probs)
	}
	if !strings.Contains(probs[0].String(), "task") || !strings.Contains(probs[0].String(), "brain stores doctor --repair-identity") {
		t.Errorf("must name the store and the repair: %s", probs[0])
	}

	res := identityAudit{Problems: probs}.apply([]storeHealth{{Name: "task", Status: "ok"}})
	if res[0].Status != "fail" || !strings.Contains(res[0].Reason, "--repair-identity") {
		t.Errorf("doctor row: %+v", res[0])
	}

	// The fleet doctor reports it too, and the fleet is not converged.
	rep := fleetDoctorFor(t, f, a)
	if rep.Converged || len(rep.Consistent) == 0 || !strings.HasPrefix(rep.Consistent[0], "FAIL ") ||
		!strings.Contains(rep.Consistent[0], `"task"`) || !strings.Contains(rep.Consistent[0], "--repair-identity") {
		t.Errorf("fleet doctor: %+v", rep.Consistent)
	}

	if err := saveStoresRegistry(map[string]storeEntry{}); err != nil {
		t.Fatal(err)
	}
	results := localIdentityAudit(context.Background()).apply(nil)
	if len(results) != 1 || results[0].Name != "task" || results[0].Status != "fail" {
		t.Errorf("empty local registry identity audit = %+v", results)
	}
}

func TestRelayClientIdentityAuditIsNonClean(t *testing.T) {
	audit := identityAuditFor(context.Background(), nil, nil, true)
	results := audit.apply(nil)
	if len(results) != 1 || results[0].Status != "fail" || results[0].Reason != "identity unverified (relay client)" {
		t.Fatalf("ordinary doctor relay audit = %+v", results)
	}
	fleet := audit.fleetFindings()
	if len(fleet) != 1 || fleet[0] != "FAIL identity unverified (relay client)" {
		t.Fatalf("fleet doctor relay audit = %+v", fleet)
	}
}

func TestRepairIdentityBackfillsRefusesConflictsAndReportsNoLocalID(t *testing.T) {
	f := newFleet(t)
	a := f.addMachine("mini0")
	f.adopt(a)
	ctx := context.Background()
	// Three registered stores: one repairable, one in conflict, one this machine cannot repair.
	for _, s := range []string{"fixable", "clash", "blind"} {
		meta := registryMeta()
		if _, err := storeregistry.Mutate(ctx, a.db, meta, storeregistry.PlanCreate(storeEntryFor(s))); err != nil {
			t.Fatal(err)
		}
	}
	writeProjectID(t, a, "fixable", "p-fixable")
	writeProjectID(t, a, "clash", "file-id")
	f.mustExec("REPLACE INTO brain_unified_metadata (`store`,`key`,value) VALUES ('clash', '_project_id', 'db-id')")
	f.mustExec("DELETE FROM brain_unified_metadata WHERE `store` = 'task' AND `key` = '_project_id'")

	snap, _, _ := storeregistry.ReadSnapshot(ctx, a.db)
	res, err := repairIdentity(ctx, a.db, snap.Entries)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]identityRepairResult{}
	for _, r := range res {
		got[r.Store] = r
	}
	if got["fixable"].Status != "repaired" || got["clash"].Status != "conflict" || got["blind"].Status != "no-local-id" {
		t.Fatalf("results: %+v", res)
	}
	if got["task"].Status != "no-local-id" { // task's metadata.json is "{}"
		t.Errorf("task: %+v", got["task"])
	}
	if v, _ := identityRow(t, f, "fixable"); v != "p-fixable" {
		t.Errorf("fixable not backfilled: %q", v)
	}
	if v, _ := identityRow(t, f, "clash"); v != "db-id" {
		t.Errorf("conflicting row was overwritten: %q", v)
	}
	if printIdentityRepair(io.Discard, res) {
		t.Error("unrepaired stores must make the repair report not-ok")
	}
	// Running it again changes nothing for the repaired store.
	res, err = repairIdentity(ctx, a.db, snap.Entries)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Store == "fixable" {
			t.Errorf("fixable should be done: %+v", r)
		}
	}
}

func TestSyncErrorCarriesTheRepairHint(t *testing.T) {
	msg := (&missingIdentityError{Store: "notes"}).Error()
	for _, want := range []string{"the database has no project identity for \"notes\"", "brain stores doctor --repair-identity", "brain stores sync"} {
		if !strings.Contains(msg, want) {
			t.Errorf("hint %q missing from %q", want, msg)
		}
	}
}

// captureIdentityStdout runs fn and returns what it printed to os.Stdout.
func captureIdentityStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = orig
	_ = w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

func TestEnsurePollerInstalledInstallsOnceAndPrintsOneLine(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the poller is a launchd agent")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	calls := 0
	origInstall := installPollerFn
	t.Cleanup(func() { installPollerFn = origInstall })
	installPollerFn = func(string, int, bool, bool) (string, error) {
		calls++
		mustWrite(t, filepath.Join(home, "Library/LaunchAgents", pollerLabel+".plist"), "<plist/>\n", 0o644)
		return "installed\n", nil
	}
	out := captureIdentityStdout(t, ensurePollerInstalled)
	if calls != 1 || strings.Count(strings.TrimSpace(out), "\n") != 0 || !strings.Contains(out, "installed it") {
		t.Errorf("calls=%d out=%q", calls, out)
	}
	// Already installed: silent, no second install.
	if out := captureIdentityStdout(t, ensurePollerInstalled); out != "" || calls != 1 {
		t.Errorf("second run: calls=%d out=%q", calls, out)
	}
}

func TestEnsurePollerInstalledReportsAFailureInOneLine(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the poller is a launchd agent")
	}
	t.Setenv("HOME", t.TempDir())
	origInstall := installPollerFn
	t.Cleanup(func() { installPollerFn = origInstall })
	installPollerFn = func(string, int, bool, bool) (string, error) { return "", errors.New("no wrapper") }
	out := captureIdentityStdout(t, ensurePollerInstalled)
	if strings.Count(strings.TrimSpace(out), "\n") != 0 || !strings.Contains(out, "installing it failed") {
		t.Errorf("out=%q", out)
	}
}

func TestFleetSyncCommandForwardsNoPoller(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(envMachine, "laptop")
	mustWrite(t, filepath.Join(home, fleetRosterPath), "machines:\n  - name: mini0\n", 0o644)
	orig := sshFleetRunner
	t.Cleanup(func() { sshFleetRunner = orig })
	origExit := syncExit
	t.Cleanup(func() { syncExit = origExit })
	t.Cleanup(func() {
		syncMachine, syncDryRun, syncNoPoller = "", false, false
		_ = brainStoresSyncCmd.Flags().Set("machine", "")
		_ = brainStoresSyncCmd.Flags().Set("dry-run", "false")
		_ = brainStoresSyncCmd.Flags().Set("no-poller", "false")
	})
	var got []string
	sshFleetRunner = func(_ context.Context, _ fleetMember, args ...string) ([]byte, error) {
		got = append([]string(nil), args...)
		return nil, nil
	}
	exitCode := -1
	syncExit = func(code int) { exitCode = code }
	brainStoresSyncCmd.SetArgs([]string{"--machine", "mini0", "--dry-run", "--no-poller"})
	if err := brainStoresSyncCmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if exitCode != 0 {
		t.Fatalf("sync exit = %d", exitCode)
	}
	if strings.Join(got, " ") != "stores sync --dry-run --no-poller" {
		t.Errorf("remote sync arguments = %q", got)
	}
}

func TestFleetDoctorFlagsAMachineWithoutAWorkingPoller(t *testing.T) {
	f := newFleet(t)
	a, b := f.addMachine("mini0"), f.addMachine("laptop")
	f.adopt(a)
	b.anyCommand()
	if rep := fleetDoctorFor(t, f, a, b); !rep.Converged {
		t.Fatalf("baseline: %s", rep.Summary)
	}

	// Poller not installed on laptop: epoch is current, still flagged with the install command.
	if err := os.Remove(filepath.Join(b.home, "Library/LaunchAgents", pollerLabel+".plist")); err != nil {
		t.Fatal(err)
	}
	rep := fleetDoctorFor(t, f, a, b)
	var lap fleetMachineResult
	for _, r := range rep.Machines {
		if r.Name == "laptop" {
			lap = r
		}
	}
	if rep.Converged || lap.Verdict != verdictDrifted || !strings.Contains(lap.Detail, "not installed") ||
		lap.Fix != "on laptop: brain stores poller install" {
		t.Errorf("no poller: %+v", lap)
	}

	// Lagging and no poller: both are reported.
	a.createStore("recipes")
	rep = fleetDoctorFor(t, f, a, b)
	for _, r := range rep.Machines {
		if r.Name == "laptop" {
			lap = r
		}
	}
	if lap.Verdict != verdictLagging || !strings.Contains(lap.Detail, "not installed") ||
		!strings.Contains(lap.Fix, "brain stores sync --machine laptop") || !strings.Contains(lap.Fix, "brain stores poller install") {
		t.Errorf("lagging + no poller: %+v", lap)
	}

	// Installed but not loaded.
	mustWrite(t, filepath.Join(b.home, "Library/LaunchAgents", pollerLabel+".plist"), "<plist/>\n", 0o644)
	b.anyCommand()
	orig := pollerLoaded
	t.Cleanup(func() { pollerLoaded = orig })
	pollerLoaded = func() bool { return false }
	rep = fleetDoctorFor(t, f, a, b)
	for _, r := range rep.Machines {
		if r.Name == "laptop" {
			lap = r
		}
	}
	if rep.Converged || !strings.Contains(lap.Detail, "not loaded") {
		t.Errorf("unloaded poller: %+v", lap)
	}
}
