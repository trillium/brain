//go:build cgo

package embeddeddolt_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/steveyegge/beads/internal/storage/schema"
)

// These tests cover the move of brain's migrations out of the main series
// (internal/storage/schema/brain_series.go). A brain 1.1 database recorded
// the ISA columns, the isa_sections table and the slug index as main-series
// 0054-0056 — the numbers upstream 1.3 gives its lease columns, leases table
// and comments keyset index. Opening it with a 1.3 binary must apply
// upstream's 0054-0066 (not skip them as "already applied"), keep brain's
// schema and data, and record brain's three migrations in the brain cursor.

const brainOneOneMainTop = 53 // the last upstream migration a brain 1.1 database ran

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// seedBrainOneOneDatabase builds the shape a brain 1.1 binary left: upstream's
// main series through 0053, then brain's three bodies applied and recorded as
// main-series 0054-0056 with their content hashes, and one ISA-shaped bead.
func seedBrainOneOneDatabase(t *testing.T, ctx context.Context, numbers []int) string {
	t.Helper()
	dataDir := seedMainSchemaAt(t, ctx, brainOneOneMainTop)
	conn, closeConn := openPinnedConn(t, ctx, dataDir)
	defer closeConn()
	names := schema.BrainMigrationNames()
	if len(names) != len(numbers) {
		t.Fatalf("brain series has %d migrations, test numbers %d", len(names), len(numbers))
	}
	for i, name := range names {
		body, err := schema.BrainMigrationSQL(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := schema.DrainCall(ctx, conn, body); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO schema_migrations (version, content_hash) VALUES (?, ?)", numbers[i], sha256Hex(body)); err != nil {
			t.Fatalf("record %s as %d: %v", name, numbers[i], err)
		}
	}
	if _, err := conn.ExecContext(ctx,
		"INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, slug, isa_phase, isa_progress_m, isa_progress_n) "+
			"VALUES ('brn-isa1', 'ISA bead', '', '', '', '', 'open', 2, 'isa', 'isa-bead', 'observe', 3, 7)"); err != nil {
		t.Fatalf("insert ISA bead: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "CALL DOLT_ADD('-A')"); err != nil {
		t.Fatalf("DOLT_ADD -A: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "CALL DOLT_COMMIT('-m', 'test: brain 1.1 database')"); err != nil {
		t.Fatalf("DOLT_COMMIT: %v", err)
	}
	return dataDir
}

func TestBrainNumberedMigrationsMoveOutAndUpstreamSeriesApplies(t *testing.T) {
	requireEmbedded(t)
	if !schema.BrainSeriesEnabled() {
		t.Fatal("the embeddeddolt package must turn the brain series on")
	}
	ctx := t.Context()
	dataDir := seedBrainOneOneDatabase(t, ctx, []int{54, 55, 56})

	conn, closeConn := openPinnedConn(t, ctx, dataDir)
	defer closeConn()
	if _, err := schema.MigrateUp(ctx, conn); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}

	// The main series is upstream's, all of it, with upstream's hashes at
	// the numbers brain had used.
	var maxV, count int
	if err := conn.QueryRowContext(ctx, "SELECT COALESCE(MAX(version),0), COUNT(*) FROM schema_migrations").Scan(&maxV, &count); err != nil {
		t.Fatal(err)
	}
	if maxV != schema.LatestVersion() || count != schema.LatestVersion() {
		t.Fatalf("schema_migrations max=%d count=%d, want both %d", maxV, count, schema.LatestVersion())
	}
	for v, name := range map[int]string{54: "0054_add_lease_columns.up.sql", 55: "0055_move_leases_to_table.up.sql", 56: "0056_add_comments_keyset_index.up.sql"} {
		body, err := schema.MigrationSQL(name)
		if err != nil {
			t.Fatal(err)
		}
		var got string
		if err := conn.QueryRowContext(ctx, "SELECT content_hash FROM schema_migrations WHERE version = ?", v).Scan(&got); err != nil {
			t.Fatalf("version %d: %v", v, err)
		}
		if got != sha256Hex(body) {
			t.Errorf("schema_migrations %d hash = %s, want upstream %s's", v, got, name)
		}
	}

	// Brain's three migrations are in the brain cursor, under their bodies'
	// hashes, and were not re-run.
	for i, name := range schema.BrainMigrationNames() {
		body, _ := schema.BrainMigrationSQL(name)
		var got string
		if err := conn.QueryRowContext(ctx, "SELECT content_hash FROM brain_schema_migrations WHERE version = ?", i+1).Scan(&got); err != nil {
			t.Fatalf("brain version %d: %v", i+1, err)
		}
		if got != sha256Hex(body) {
			t.Errorf("brain_schema_migrations %d hash = %s, want %s", i+1, got, sha256Hex(body))
		}
	}
	if v, err := schema.CurrentBrainVersion(ctx, conn); err != nil || v != schema.LatestBrainVersion() {
		t.Fatalf("brain cursor = %d (%v), want %d", v, err, schema.LatestBrainVersion())
	}

	// Upstream's 0054-0056 objects exist: they were applied, not skipped.
	for _, q := range []string{
		"SELECT COUNT(*) FROM leases",
		"SELECT COUNT(row_lock) FROM issues",
		"SELECT COUNT(storage_class) FROM issues",
	} {
		var n int
		if err := conn.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Errorf("%s: %v (an upstream migration was skipped)", q, err)
		}
	}

	// Brain's schema and data are untouched.
	var slug, phase string
	var m, n int
	if err := conn.QueryRowContext(ctx, "SELECT slug, isa_phase, isa_progress_m, isa_progress_n FROM issues WHERE id = 'brn-isa1'").Scan(&slug, &phase, &m, &n); err != nil {
		t.Fatalf("read ISA bead: %v", err)
	}
	if slug != "isa-bead" || phase != "observe" || m != 3 || n != 7 {
		t.Fatalf("ISA bead = %q %q %d/%d, want isa-bead observe 3/7", slug, phase, m, n)
	}
	var sections int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM isa_sections").Scan(&sections); err != nil {
		t.Fatalf("isa_sections: %v", err)
	}

	// The pass committed everything it did.
	dirty, err := statusTables(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 0 {
		t.Fatalf("working set dirty after the pass: %v", dirty)
	}

	// A second pass has nothing to do.
	applied, err := schema.MigrateUp(ctx, conn)
	if err != nil {
		t.Fatalf("second MigrateUp: %v", err)
	}
	if applied != 0 {
		t.Fatalf("second MigrateUp applied %d, want 0", applied)
	}
}

// A database whose brain rows sit below an upstream row cannot be healed by
// moving them out (upstream's lower migrations would stay unapplied below the
// cursor), so the pass refuses and writes nothing.
func TestBrainNumberedMigrationsBelowAnUpstreamRowRefuse(t *testing.T) {
	requireEmbedded(t)
	ctx := t.Context()
	dataDir := seedBrainOneOneDatabase(t, ctx, []int{54, 55, 57})
	conn, closeConn := openPinnedConn(t, ctx, dataDir)
	defer closeConn()
	// An upstream-looking row between brain's.
	if _, err := conn.ExecContext(ctx, "INSERT INTO schema_migrations (version, content_hash) VALUES (56, ?)", sha256Hex("not a brain body")); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'test: mixed numbering')"); err != nil {
		t.Fatal(err)
	}

	_, err := schema.MigrateUp(ctx, conn)
	var notTop *schema.ErrBrainRowsNotAtTop
	if !errors.As(err, &notTop) {
		t.Fatalf("MigrateUp err = %v, want *schema.ErrBrainRowsNotAtTop", err)
	}
	var brainRows int
	err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM brain_schema_migrations").Scan(&brainRows)
	if err == nil && brainRows != 0 {
		t.Fatalf("refused heal still recorded %d brain rows", brainRows)
	}
	if err != nil && !isMissingTable(err) {
		t.Fatalf("probe brain cursor: %v", err)
	}
}

func isMissingTable(err error) bool {
	return err != nil && (errors.Is(err, sql.ErrNoRows) || containsAny(err.Error(), "not found", "doesn't exist"))
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
