package brainupgrade_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/brainunify"
	"github.com/steveyegge/beads/internal/brainupgrade"
	"github.com/steveyegge/beads/internal/storage/schema"
	"github.com/steveyegge/beads/internal/testutil"
)

const fixtureDB = "brain_unified"

// startServer starts an isolated Dolt sql-server and returns its endpoint.
func startServer(t *testing.T, ctx context.Context) brainupgrade.Endpoint {
	t.Helper()
	srv, err := brainunify.StartIsolatedServer(ctx, "dolt", t.TempDir())
	if err != nil {
		t.Fatalf("starting dolt sql-server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return brainupgrade.Endpoint{Host: "127.0.0.1", Port: srv.Port, User: "root"}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// seedBrainOneOne builds, on ep, the shape a brain 1.1 binary left:
// upstream's main series through 0053, brain's three migrations recorded as
// main-series 0054-0056, and beads with comments, events and a dependency.
func seedBrainOneOne(t *testing.T, ctx context.Context, ep brainupgrade.Endpoint) {
	t.Helper()
	root, err := ep.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.ExecContext(ctx, "CREATE DATABASE "+fixtureDB); err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	c, err := ep.Conn(ctx, fixtureDB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := schema.MigrateUpTo(ctx, c.Conn, 53); err != nil {
		t.Fatalf("MigrateUpTo(53): %v", err)
	}
	for i, name := range schema.BrainMigrationNames() {
		body, err := schema.BrainMigrationSQL(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.DrainCall(ctx, c.Conn, body); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := c.ExecContext(ctx, "INSERT INTO schema_migrations (version, content_hash) VALUES (?, ?)", 54+i, sha256Hex(body)); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := c.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, id := range []string{"brain-a1", "brain-a2", "brain-isa-q1"} {
		kind := "task"
		if strings.Contains(id, "isa") {
			kind = "isa"
		}
		exec("INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, slug) VALUES (?, ?, 'body', '', '', '', 'open', 2, ?, ?)",
			id, "Bead "+id, kind, "slug-"+id)
	}
	exec("INSERT INTO isa_sections (issue_id, section_name, body) VALUES ('brain-isa-q1', 'goal', 'what done looks like')")
	exec("INSERT INTO comments (id, issue_id, author, text) VALUES ('11111111-1111-7111-8111-111111111111', 'brain-a1', 'alice', 'a comment')")
	exec("INSERT INTO events (id, issue_id, event_type, actor, comment) VALUES ('22222222-2222-7222-8222-222222222222', 'brain-a1', 'created', 'alice', 'made')")
	exec("CALL DOLT_ADD('-A')")
	exec("CALL DOLT_COMMIT('-m', 'brain 1.1 fixture')")
}

func conn(t *testing.T, ctx context.Context, ep brainupgrade.Endpoint) *brainupgrade.Conn {
	t.Helper()
	c, err := ep.Conn(ctx, fixtureDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// backupVerified takes and verifies a backup of the fixture.
func backupVerified(t *testing.T, ctx context.Context, live brainupgrade.Endpoint) brainupgrade.Backup {
	t.Helper()
	url, err := brainupgrade.BackupURL(t.TempDir(), fixtureDB)
	if err != nil {
		t.Fatal(err)
	}
	st, err := brainupgrade.TakeBackup(ctx, conn(t, ctx, live).Conn, url)
	if err != nil {
		t.Fatalf("TakeBackup: %v", err)
	}
	b := brainupgrade.Backup{Database: fixtureDB, URL: url, State: st}
	diffs, err := brainupgrade.VerifyBackup(ctx, startServer(t, ctx), b)
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	if len(diffs) > 0 {
		t.Fatalf("a fresh backup does not reproduce the database: %v", diffs)
	}
	b.Verified = true
	return b
}

func TestPlanReportsTheBrainRowsAndTheUpstreamSeries(t *testing.T) {
	testutil.RequireDoltCLIOnly(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	live := startServer(t, ctx)
	seedBrainOneOne(t, ctx, live)

	plan, err := schema.PlanUpgrade(ctx, conn(t, ctx, live).Conn)
	if err != nil {
		t.Fatal(err)
	}
	if plan.MainRecorded != 56 || plan.MainEffective != 53 || len(plan.BrainRowsInMain) != 3 {
		t.Fatalf("plan = recorded %d, effective %d, brain rows %v; want 56, 53, three", plan.MainRecorded, plan.MainEffective, plan.BrainRowsInMain)
	}
	if len(plan.PendingMain) != schema.LatestVersion()-53 || !strings.HasPrefix(plan.PendingMain[0], "0054_") {
		t.Fatalf("pending main = %v, want 0054 through %d", plan.PendingMain, schema.LatestVersion())
	}
	if plan.BrainCurrent != schema.LatestBrainVersion() || len(plan.PendingBrain) != 0 {
		t.Fatalf("brain series = %d pending %v; the brain rows already ran", plan.BrainCurrent, plan.PendingBrain)
	}
	if plan.HealRefusal != "" {
		t.Fatalf("unexpected heal refusal: %s", plan.HealRefusal)
	}
}

func TestBackupApplyVerifyRollback(t *testing.T) {
	testutil.RequireDoltCLIOnly(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()
	live := startServer(t, ctx)
	seedBrainOneOne(t, ctx, live)
	b := backupVerified(t, ctx, live)

	res, err := brainupgrade.Apply(ctx, conn(t, ctx, live).Conn, b)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Applied == 0 {
		t.Fatal("Apply ran no migration on a 1.1 database")
	}
	if !res.Report.OK() {
		t.Fatalf("the migrated database does not verify: %v", res.Report.Failures)
	}
	statuses := map[string]string{}
	for _, tc := range res.Report.Tables {
		statuses[tc.Table] = tc.Status
	}
	// The aux ids are re-derived from content, so the rows stay and the id
	// column alone changes.
	if statuses["comments"] != brainupgrade.TableRewritten || statuses["events"] != brainupgrade.TableRewritten {
		t.Errorf("comments %q, events %q: want both rewritten", statuses["comments"], statuses["events"])
	}
	if statuses["isa_sections"] != brainupgrade.TableIdentical {
		t.Errorf("isa_sections %q: brain's own table must come through identical", statuses["isa_sections"])
	}
	if !strings.Contains(strings.Join(res.Report.NewTables, " "), schema.BrainSchemaCursorTable) {
		t.Errorf("new tables %v: want the brain cursor", res.Report.NewTables)
	}

	rb, err := brainupgrade.Rollback(ctx, live, b)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if !rb.OK() {
		t.Fatalf("the rollback is not the backed-up state: %v", rb.Diffs)
	}
	plan, err := schema.PlanUpgrade(ctx, conn(t, ctx, live).Conn)
	if err != nil {
		t.Fatal(err)
	}
	if plan.MainRecorded != 56 || len(plan.BrainRowsInMain) != 3 {
		t.Fatalf("after rollback: recorded %d, brain rows %d; want the 1.1 cursor back", plan.MainRecorded, len(plan.BrainRowsInMain))
	}
}

func TestApplyRefusesADatabaseThatChangedSinceItsBackup(t *testing.T) {
	testutil.RequireDoltCLIOnly(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	live := startServer(t, ctx)
	seedBrainOneOne(t, ctx, live)
	b := backupVerified(t, ctx, live)

	c := conn(t, ctx, live)
	if _, err := c.ExecContext(ctx, "UPDATE issues SET title = 'edited after the backup' WHERE id = 'brain-a2'"); err != nil {
		t.Fatal(err)
	}
	if _, err := brainupgrade.Apply(ctx, c.Conn, b); !errors.Is(err, brainupgrade.ErrChanged) {
		t.Fatalf("Apply after a write = %v, want ErrChanged", err)
	}
	unverified := b
	unverified.Verified = false
	if _, err := brainupgrade.Apply(ctx, c.Conn, unverified); err == nil || !strings.Contains(err.Error(), "never verified") {
		t.Fatalf("Apply on an unverified backup = %v, want a refusal", err)
	}
	plan, err := schema.PlanUpgrade(ctx, c.Conn)
	if err != nil {
		t.Fatal(err)
	}
	if plan.MainRecorded != 56 {
		t.Fatalf("a refused Apply migrated anyway: main cursor %d", plan.MainRecorded)
	}
}

func TestVerifyCatchesWhatTheMigrationMustNotDo(t *testing.T) {
	testutil.RequireDoltCLIOnly(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()
	live := startServer(t, ctx)
	seedBrainOneOne(t, ctx, live)
	b := backupVerified(t, ctx, live)
	c := conn(t, ctx, live)
	if res, err := brainupgrade.Apply(ctx, c.Conn, b); err != nil || !res.Report.OK() {
		t.Fatalf("Apply: %v %v", err, res.Report.Failures)
	}

	cases := []struct {
		name, tamper, table, want string
	}{
		{"a lost row", "DELETE FROM issues WHERE id = 'brain-a2'", "issues", brainupgrade.TableChanged},
		{"an edited column", "UPDATE comments SET text = 'altered' WHERE issue_id = 'brain-a1'", "comments", brainupgrade.TableChanged},
		{"a dropped column", "ALTER TABLE isa_sections DROP COLUMN body", "isa_sections", brainupgrade.TableMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.ExecContext(ctx, tc.tamper); err != nil {
				t.Fatalf("%s: %v", tc.tamper, err)
			}
			rep, err := brainupgrade.VerifyMigrated(ctx, c.Conn, b.State)
			if err != nil {
				t.Fatal(err)
			}
			if rep.OK() {
				t.Fatal("verify passed a database that lost data")
			}
			for _, check := range rep.Tables {
				if check.Table == tc.table && check.Status != tc.want {
					t.Errorf("%s: status %q, want %q", tc.table, check.Status, tc.want)
				}
			}
		})
	}
}

func TestRehearseRunsOnACopyAndLeavesTheDatabaseAlone(t *testing.T) {
	testutil.RequireDoltCLIOnly(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()
	live := startServer(t, ctx)
	seedBrainOneOne(t, ctx, live)
	b := backupVerified(t, ctx, live)

	reh, err := brainupgrade.Rehearse(ctx, startServer(t, ctx), b)
	if err != nil {
		t.Fatalf("Rehearse: %v", err)
	}
	if !reh.OK() {
		t.Fatalf("rehearsal failed: copy %v, verify %v, rollback %v", reh.CopyDiffs, reh.Apply.Report.Failures, reh.Rollback.Diffs)
	}
	if reh.After.Schema.Pending() {
		t.Fatalf("the rehearsed copy is not at latest: %+v", reh.After.Schema)
	}
	_, dbHash, err := brainupgrade.Identity(ctx, conn(t, ctx, live).Conn)
	if err != nil {
		t.Fatal(err)
	}
	if dbHash != b.State.DBHash {
		t.Fatalf("the rehearsal changed the database it was rehearsed for: %s -> %s", b.State.DBHash, dbHash)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := &brainupgrade.Manifest{Server: "127.0.0.1:3307"}
	m.Put(brainupgrade.Backup{Database: "a", URL: "file:///x/a"})
	m.Put(brainupgrade.Backup{Database: "b", URL: "file:///x/b"})
	m.Put(brainupgrade.Backup{Database: "a", URL: "file:///x/a2", Verified: true})
	if err := m.Save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := brainupgrade.LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := got.Get("a")
	if !ok || a.URL != "file:///x/a2" || !a.Verified || len(got.Backups) != 2 {
		t.Fatalf("manifest round trip = %+v", got)
	}
	if _, err := brainupgrade.LoadManifest(filepath.Join(dir, "none")); err == nil {
		t.Fatal("a directory without a manifest must be an error")
	}
}

func TestRemotesNamesEveryDoltRemote(t *testing.T) {
	testutil.RequireDoltCLIOnly(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	live := startServer(t, ctx)
	seedBrainOneOne(t, ctx, live)
	c := conn(t, ctx, live)
	if got, err := brainupgrade.Remotes(ctx, c.Conn); err != nil || len(got) != 0 {
		t.Fatalf("Remotes on a database without any = %v, %v", got, err)
	}
	url := "file://" + filepath.ToSlash(t.TempDir())
	if err := schema.DrainCall(ctx, c.Conn, "CALL DOLT_REMOTE('add', 'peer', ?)", url); err != nil {
		t.Fatal(err)
	}
	got, err := brainupgrade.Remotes(ctx, c.Conn)
	if err != nil || len(got) != 1 || got[0] != "peer="+url {
		t.Fatalf("Remotes = %v, %v; want [peer=%s]", got, err, url)
	}
}
