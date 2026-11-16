package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/brainupgrade"
	"github.com/steveyegge/beads/internal/storage/schema"
	"github.com/steveyegge/beads/internal/testutil"
)

// writeFedStoreMeta lays a store's .beads directory out under dir with the
// metadata a federation store carries, and returns the .beads path.
func writeFedStoreMeta(t *testing.T, dir string, meta map[string]any) string {
	t.Helper()
	beads := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beads, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beads, "metadata.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return beads
}

func fedMeta(port int, database, global string) map[string]any {
	m := map[string]any{
		"database":         "dolt",
		"backend":          "dolt",
		"dolt_mode":        "server",
		"dolt_server_host": "127.0.0.1",
		"dolt_server_port": port,
		"dolt_database":    database,
	}
	if global != "" {
		m["global_dolt_database"] = global
	}
	return m
}

func TestResolveFederationStores(t *testing.T) {
	root := t.TempDir()
	registry := map[string]storeEntry{
		"alpha":   {Path: writeFedStoreMeta(t, filepath.Join(root, "alpha"), fedMeta(3307, "brain_unified", "beads_global"))},
		"beta":    {Path: writeFedStoreMeta(t, filepath.Join(root, "beta"), fedMeta(3307, "brain_unified", ""))},
		"faraway": {Path: writeFedStoreMeta(t, filepath.Join(root, "faraway"), fedMeta(3309, "other", ""))},
		"broken":  {Path: filepath.Join(root, "broken", ".beads")},
	}
	stores, err := resolveFederationStores(registry, nil, 3307)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]fedStore{}
	for _, st := range stores {
		got[st.Name] = st
	}
	if len(stores) != 4 || stores[0].Name != "alpha" || stores[3].Name != "faraway" {
		t.Fatalf("stores not sorted by name: %+v", stores)
	}
	if st := got["alpha"]; st.Status != "ok" || st.Database != "brain_unified" || st.Global != "beads_global" {
		t.Errorf("alpha = %+v", st)
	}
	if st := got["beta"]; st.Status != "ok" || st.Global != "" {
		t.Errorf("beta = %+v", st)
	}
	if st := got["faraway"]; st.Status != "elsewhere" || !strings.Contains(st.Error, "3309") {
		t.Errorf("faraway = %+v", st)
	}
	if st := got["broken"]; st.Status != "unreadable" {
		t.Errorf("broken = %+v", st)
	}

	only, err := resolveFederationStores(registry, []string{"beta"}, 3307)
	if err != nil || len(only) != 1 || only[0].Name != "beta" {
		t.Fatalf("--store beta: %v %+v", err, only)
	}
	if _, err := resolveFederationStores(registry, []string{"nope"}, 3307); err == nil {
		t.Fatal("an unknown --store must be an error")
	}
}

func TestPlanFederationDatabases(t *testing.T) {
	stores := []fedStore{
		{Name: "alpha", Status: "ok", Database: "brain_unified", Global: "beads_global"},
		{Name: "beta", Status: "ok", Database: "brain_unified"},
		{Name: "solo", Status: "ok", Database: "solo", Global: "solo"},
		{Name: "faraway", Status: "elsewhere", Database: "other"},
		{Name: "broken", Status: "unreadable"},
	}
	dbs := planFederationDatabases(stores)
	if len(dbs) != 3 {
		t.Fatalf("want brain_unified, beads_global, solo; got %+v", dbs)
	}
	byName := map[string]fedDatabase{}
	for _, d := range dbs {
		byName[d.Name] = d
	}
	if d := byName["brain_unified"]; strings.Join(d.Stores, ",") != "alpha,beta" || len(d.GlobalFor) != 0 {
		t.Errorf("brain_unified = %+v", d)
	}
	if d := byName["beads_global"]; len(d.Stores) != 0 || strings.Join(d.GlobalFor, ",") != "alpha" {
		t.Errorf("beads_global = %+v", d)
	}
	if d := byName["solo"]; strings.Join(d.Stores, ",") != "solo" || len(d.GlobalFor) != 0 {
		t.Errorf("a global database equal to the store's own is not listed twice: %+v", d)
	}
	if _, ok := byName["other"]; ok {
		t.Error("a store on another server contributes no database to this run")
	}
}

func TestCompareFedState(t *testing.T) {
	want := fedState{DBHash: "h1", Head: "c1", Issues: 3, Tables: map[string]string{"issues": "t1", "events": "t2"}}
	same := fedState{DBHash: "h1", Head: "c1", Issues: 3, Tables: map[string]string{"issues": "t1", "events": "t2"}}
	if diffs := compareFedState(want, same, false, true); len(diffs) != 0 {
		t.Errorf("identical states differ: %v", diffs)
	}
	drift := fedState{DBHash: "h2", Head: "c2", Issues: 4, Tables: map[string]string{"issues": "t9", "extra": "t3"}}
	diffs := compareFedState(want, drift, false, false)
	joined := strings.Join(diffs, "\n")
	for _, s := range []string{"dolt_hashof_db", "HEAD", "issues rows", "table issues:", "table events missing", "table extra not in"} {
		if !strings.Contains(joined, s) {
			t.Errorf("diffs lack %q:\n%s", s, joined)
		}
	}
	// A write during the backup: the backup may be newer than the pre-sync
	// reading, so only history and table presence are checked.
	newer := fedState{DBHash: "h2", Head: "c2", Issues: 4, Tables: map[string]string{"issues": "t9", "events": "t2"}}
	if diffs := compareFedState(want, newer, true, true); len(diffs) != 0 {
		t.Errorf("a newer backup with the pre-sync HEAD in its history must pass: %v", diffs)
	}
	if diffs := compareFedState(want, newer, true, false); len(diffs) != 1 || !strings.Contains(diffs[0], "not in the backup's history") {
		t.Errorf("a newer backup without the pre-sync HEAD must fail: %v", diffs)
	}
	gone := fedState{DBHash: "h2", Head: "c2", Issues: 4, Tables: map[string]string{"issues": "t9"}}
	if diffs := compareFedState(want, gone, true, true); len(diffs) != 1 || !strings.Contains(diffs[0], "table events missing") {
		t.Errorf("a missing table fails even when the database changed: %v", diffs)
	}
}

func TestCheckFederationDest(t *testing.T) {
	root := t.TempDir()
	mk := func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	dataDir := mk("server", "data")
	mk("server", "data", ".doltcfg")
	mk("server", "data", "brain_unified", ".dolt", "noms")
	storeBeads := mk("data", "person", ".beads")
	mk("dotdolt-config-only", ".dolt") // ~/.dolt holds Dolt's global config, never a database
	fine := mk("backups")
	unsafeBackup := mk("server", "data", "brain_unified", ".dolt", "noms")
	if err := os.Symlink(unsafeBackup, filepath.Join(fine, "brain_unified")); err != nil {
		t.Fatal(err)
	}
	unsafeScratch := filepath.Join(root, "server", "data", "scratch")

	cases := []struct {
		name, dest, want string
	}{
		{"empty", "", "required"},
		{"relative", "backups", "absolute"},
		{"root", string(filepath.Separator), "filesystem root"},
		{"inside the data dir", filepath.Join(dataDir, "backups"), "sql-server data directory"},
		{"the data dir itself", dataDir, "sql-server data directory"},
		{"inside a database", filepath.Join(dataDir, "brain_unified", "backups"), "Dolt database"},
		{"inside a store", filepath.Join(storeBeads, "backup"), "registered store directory"},
		{"beside a store's .beads", filepath.Join(root, "data", "person", "backups"), "registered store directory"},
		{"beside a global config dir", filepath.Join(root, "dotdolt-config-only", "backups"), ""},
		{"fine", fine, ""},
		{"fine, not yet created", filepath.Join(fine, "new"), ""},
		{"scratch inside the data dir", unsafeScratch, "sql-server data directory"},
	}
	for _, c := range cases {
		err := checkFederationDest(c.dest, []string{storeBeads})
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected refusal: %v", c.name, err)
		case c.want != "" && err == nil:
			t.Errorf("%s: %s must be refused (%s)", c.name, c.dest, c.want)
		case c.want != "" && !strings.Contains(err.Error(), c.want):
			t.Errorf("%s: error %q lacks %q", c.name, err, c.want)
		}
	}
	if err := checkFederationBackupTarget(fine, "brain_unified", []string{storeBeads}); err == nil {
		t.Fatal("a symlinked per-database backup target must be refused")
	}
}

func TestFederationBackupVerbSkipsStoreInit(t *testing.T) {
	if !isFederationBackupVerb(brainFederationBackupCmd) {
		t.Fatal("brain federation-backup must run without opening the caller's store")
	}
	if isFederationBackupVerb(brainCmd) || isFederationBackupVerb(nil) {
		t.Fatal("only the verb itself skips store init")
	}
}

// TestFederationBackupEndToEnd runs the verb against a Dolt sql-server of its
// own: two registered stores share one database, a third names a database
// the server does not hold. The backups land under --dest, are proved on a
// second server, and 'bd list' succeeds for both stores.
func TestFederationBackupEndToEnd(t *testing.T) {
	testutil.RequireDoltCLIOnly(t)
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt binary not on PATH")
	}
	bd := buildBDUnderTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	root := t.TempDir()
	srv, err := brainupgrade.StartServer(ctx, "dolt", filepath.Join(root, "server", "data"))
	if err != nil {
		t.Fatalf("starting dolt sql-server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	ep := srv.Endpoint()
	rootDB, err := ep.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rootDB.ExecContext(ctx, "CREATE DATABASE fedtest"); err != nil {
		t.Fatal(err)
	}
	_ = rootDB.Close()
	c, err := ep.Conn(ctx, "fedtest")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := schema.MigrateUp(ctx, c.Conn); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	for _, id := range []string{"alpha-1", "beta-1"} {
		if _, err := c.ExecContext(ctx, "INSERT INTO issues (id, title, description, design, acceptance_criteria, notes) VALUES (?, ?, '', '', '', '')", id, "seed "+id); err != nil {
			t.Fatal(err)
		}
	}
	if err := schema.DrainCall(ctx, c.Conn, "CALL DOLT_COMMIT('-Am', 'seed')"); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()

	stores := filepath.Join(root, "stores")
	alpha := writeFedStoreMeta(t, filepath.Join(stores, "alpha"), fedMeta(srv.Port, "fedtest", "beads_global"))
	beta := writeFedStoreMeta(t, filepath.Join(stores, "beta"), fedMeta(srv.Port, "fedtest", ""))
	ghost := writeFedStoreMeta(t, filepath.Join(stores, "ghost"), fedMeta(srv.Port, "nowhere", ""))
	registry := filepath.Join(root, "stores.yaml")
	if err := os.WriteFile(registry, []byte("stores:\n    alpha:\n        path: "+alpha+"\n    beta:\n        path: "+beta+"\n    ghost:\n        path: "+ghost+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "backups")

	run := func(extra ...string) ([]byte, error) {
		args := append([]string{"brain", "federation-backup", "--registry", registry, "--port", strconv.Itoa(srv.Port), "--user", "root", "--dest", dest, "--bd-bin", bd, "--json"}, extra...)
		cmd := exec.CommandContext(ctx, bd, args...) //nolint:gosec // the binary this test built
		cmd.Env = append(os.Environ(), "BEADS_DOLT_PASSWORD=", "BEADS_CREDENTIALS_FILE="+filepath.Join(root, "no-credentials"), "BD_DISABLE_METRICS=1", "HOME="+root)
		return cmd.CombinedOutput()
	}
	out, err := run()
	if err != nil {
		t.Fatalf("federation-backup: %v\n%s", err, out)
	}
	var rep fedReport
	if err := json.Unmarshal(out[strings.Index(string(out), "{"):], &rep); err != nil {
		t.Fatalf("report is not JSON: %v\n%s", err, out)
	}
	if !rep.OK {
		t.Fatalf("report not OK: %+v", rep.Failures)
	}
	byName := map[string]fedDatabase{}
	for _, d := range rep.Databases {
		byName[d.Name] = d
	}
	ft, ok := byName["fedtest"]
	if !ok || ft.Status != "verified" || strings.Join(ft.Stores, ",") != "alpha,beta" {
		t.Fatalf("fedtest = %+v", ft)
	}
	if ft.Before.Issues != 2 || ft.Verify == nil || ft.Verify.Got.Issues != 2 || ft.Changed {
		t.Errorf("fedtest proof = before %+v verify %+v changed %v", ft.Before, ft.Verify, ft.Changed)
	}
	if len(ft.Verify.Lists) != 2 || !ft.Verify.Lists[0].OK || !ft.Verify.Lists[1].OK || ft.Verify.Lists[0].Rows != 1 {
		t.Errorf("bd list must succeed for both stores with --limit 1: %+v", ft.Verify.Lists)
	}
	for _, name := range []string{"beads_global", "nowhere"} {
		if d := byName[name]; d.Status != "absent" {
			t.Errorf("%s must be reported absent, got %+v", name, d)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "fedtest", "manifest")); err != nil {
		t.Errorf("backup not at <dest>/fedtest: %v", err)
	}
	if _, err := os.Stat(rep.ReportPath); err != nil {
		t.Errorf("report file: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dest, ".scratch")); len(entries) != 0 {
		t.Errorf("scratch not removed: %v", entries)
	}

	// A second run syncs into the same backup and proves it again.
	out, err = run()
	if err != nil {
		t.Fatalf("second federation-backup: %v\n%s", err, out)
	}

	// A destination inside the server's data directory is refused before
	// anything is backed up.
	cmd := exec.CommandContext(ctx, bd, "brain", "federation-backup", "--registry", registry, "--port", strconv.Itoa(srv.Port), "--user", "root", "--dest", filepath.Join(root, "server", "data", "backups")) //nolint:gosec // the binary this test built
	cmd.Env = append(os.Environ(), "BEADS_DOLT_PASSWORD=", "HOME="+root)
	out, err = cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "data directory") {
		t.Fatalf("a --dest inside the data directory must be refused: %v\n%s", err, out)
	}
}
