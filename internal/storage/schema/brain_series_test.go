package schema

// Tests for the brain migration series (brain_series.go): the ISA columns,
// the isa_sections table and the slug unique index. These were the fork's
// main-series 0054-0056 tests before the series moved out of the main
// numbering; the bodies they read are byte-identical to those files.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/testutil"
)

func TestBrainMigration0001SourceUsesIdempotentInformationSchemaGuards(t *testing.T) {
	upSQL, err := os.ReadFile("migrations/brain/0001_brain_isa_columns.up.sql")
	if err != nil {
		t.Fatalf("read 0054 up migration: %v", err)
	}
	up := string(upSQL)
	// Every ISA column needs an INFORMATION_SCHEMA-gated ADD COLUMN so the
	// migration is safe to re-run. Dolt does not support
	// `ADD COLUMN IF NOT EXISTS`, hence the prepared-DDL guard pattern that
	// migrations 0046 and 0049 established.
	for _, col := range []string{
		"slug",
		"isa_phase",
		"isa_progress_m",
		"isa_progress_n",
		"isa_effort",
		"isa_mode",
		"isa_started_at",
		"isa_updated_at",
	} {
		guardVar := "@needs_" + col
		if col == "slug" {
			guardVar = "@needs_slug"
		}
		if !strings.Contains(up, guardVar) {
			t.Errorf("0054 missing idempotency guard %q for column %s", guardVar, col)
		}
		if !strings.Contains(up, "ADD COLUMN "+col+" ") && !strings.Contains(up, "ADD COLUMN "+col+"\n") {
			t.Errorf("0054 missing ADD COLUMN clause for %s", col)
		}
	}
	for _, want := range []string{
		"COLUMN_NAME = 'slug'",
		"COLUMN_NAME = 'isa_phase'",
		"COLUMN_NAME = 'isa_progress_m'",
		"COLUMN_NAME = 'isa_progress_n'",
		"COLUMN_NAME = 'isa_effort'",
		"COLUMN_NAME = 'isa_mode'",
		"COLUMN_NAME = 'isa_started_at'",
		"COLUMN_NAME = 'isa_updated_at'",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("0054 missing INFORMATION_SCHEMA check %q", want)
		}
	}
}

func TestBrainMigration0001DownDropsAllIsaColumns(t *testing.T) {
	downSQL, err := os.ReadFile("migrations/brain/0001_brain_isa_columns.down.sql")
	if err != nil {
		t.Fatalf("read 0054 down migration: %v", err)
	}
	down := string(downSQL)
	for _, col := range []string{
		"slug",
		"isa_phase",
		"isa_progress_m",
		"isa_progress_n",
		"isa_effort",
		"isa_mode",
		"isa_started_at",
		"isa_updated_at",
	} {
		if !strings.Contains(down, "DROP COLUMN "+col) {
			t.Errorf("0054 down missing DROP COLUMN %s", col)
		}
	}
}

func TestBrainMigration0002CreatesIsaSectionsTable(t *testing.T) {
	upSQL, err := os.ReadFile("migrations/brain/0002_brain_isa_sections.up.sql")
	if err != nil {
		t.Fatalf("read 0055 up migration: %v", err)
	}
	up := string(upSQL)
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS isa_sections",
		"issue_id     VARCHAR(255) NOT NULL",
		"section_name VARCHAR(64)  NOT NULL",
		"body         LONGTEXT     NOT NULL",
		"updated_at   TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP",
		"PRIMARY KEY (issue_id, section_name)",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("0055 missing required clause %q", want)
		}
	}

	downSQL, err := os.ReadFile("migrations/brain/0002_brain_isa_sections.down.sql")
	if err != nil {
		t.Fatalf("read 0055 down migration: %v", err)
	}
	if !strings.Contains(string(downSQL), "DROP TABLE IF EXISTS isa_sections") {
		t.Error("0055 down migration missing DROP TABLE IF EXISTS isa_sections")
	}
}

// TestBrainMigration0003SourceUsesIdempotentInformationSchemaGuard verifies that
// the 0056 up migration uses the INFORMATION_SCHEMA-gated PREPARE/EXECUTE
// pattern — Dolt does not support `CREATE UNIQUE INDEX IF NOT EXISTS`.
func TestBrainMigration0003SourceUsesIdempotentInformationSchemaGuard(t *testing.T) {
	upSQL, err := os.ReadFile("migrations/brain/0003_brain_slug_unique.up.sql")
	if err != nil {
		t.Fatalf("read 0056 up migration: %v", err)
	}
	up := string(upSQL)
	for _, want := range []string{
		"@needs_idx",
		"INFORMATION_SCHEMA.STATISTICS",
		"INDEX_NAME   = 'idx_issues_slug_unique'",
		"CREATE UNIQUE INDEX idx_issues_slug_unique ON issues (slug)",
		"PREPARE stmt FROM @sql",
		"DEALLOCATE PREPARE stmt",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("0056 up missing required fragment %q", want)
		}
	}
}

// TestBrainMigration0003DownDropsSlugUniqueIndex verifies the down migration drops
// the UNIQUE index with an INFORMATION_SCHEMA check.
func TestBrainMigration0003DownDropsSlugUniqueIndex(t *testing.T) {
	downSQL, err := os.ReadFile("migrations/brain/0003_brain_slug_unique.down.sql")
	if err != nil {
		t.Fatalf("read 0056 down migration: %v", err)
	}
	down := string(downSQL)
	for _, want := range []string{
		"INFORMATION_SCHEMA.STATISTICS",
		"INDEX_NAME   = 'idx_issues_slug_unique'",
		"DROP INDEX idx_issues_slug_unique ON issues",
	} {
		if !strings.Contains(down, want) {
			t.Errorf("0056 down missing required fragment %q", want)
		}
	}
}

// TestCLICompatibleBrainMigration0003UsesDirectCreateIndexDDL verifies the
// CLI-direct DDL bundle for 0056 emits a plain CREATE UNIQUE INDEX.
func TestCLICompatibleBrainMigration0003UsesDirectCreateIndexDDL(t *testing.T) {
	got := cliCompatibleBrainMigrationSQL("0003_brain_slug_unique.up.sql", "source migration")
	if !strings.Contains(got, "CREATE UNIQUE INDEX idx_issues_slug_unique ON issues (slug)") {
		t.Fatalf("0056 CLI migration missing direct DDL, got %q", got)
	}
	for _, forbidden := range []string{
		"PREPARE",
		"EXECUTE",
		"DEALLOCATE",
		"@needs_idx",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("0056 CLI migration contains prepared-DDL marker %q", forbidden)
		}
	}
}

// TestBrainMigration0003EnforcesSlugUniquenessAndAllowsMultipleNulls is the
// behavioural contract: after applying all migrations, two rows with the same
// non-NULL slug must collide, while multiple NULL-slug rows must coexist.
func TestBrainMigration0003EnforcesSlugUniquenessAndAllowsMultipleNulls(t *testing.T) {
	testutil.RequireDoltBinary(t)

	dir := filepath.Join(t.TempDir(), "slug-unique")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create slug-unique test dir: %v", err)
	}
	runDoltCommand(t, dir, "init", "--name", "test", "--email", "test@example.com")
	runDoltSQL(t, dir, AllMigrationsSQL())

	idxRows := queryDoltCSV(t, dir, `
SELECT INDEX_NAME
FROM INFORMATION_SCHEMA.STATISTICS
WHERE TABLE_SCHEMA = DATABASE()
  AND TABLE_NAME = 'issues'
  AND INDEX_NAME = 'idx_issues_slug_unique'`)
	if len(idxRows) != 1 {
		t.Fatalf("idx_issues_slug_unique presence: got %d rows, want 1: %v", len(idxRows), idxRows)
	}

	runDoltSQL(t, dir, `
INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, created_at, updated_at)
VALUES ('iss-null-1', 'null slug A', '', '', '', '', 'open', 4, 'task', NOW(), NOW()),
       ('iss-null-2', 'null slug B', '', '', '', '', 'open', 4, 'task', NOW(), NOW());`)

	runDoltSQL(t, dir, `
INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, slug, created_at, updated_at)
VALUES ('iss-slug-1', 'first slug', '', '', '', '', 'open', 4, 'isa', 'shared-slug', NOW(), NOW());`)

	dupCmd := exec.Command("dolt", "sql", "-q", `
INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type, slug, created_at, updated_at)
VALUES ('iss-slug-2', 'duplicate slug', '', '', '', '', 'open', 4, 'isa', 'shared-slug', NOW(), NOW());`)
	dupCmd.Dir = dir
	dupOut, dupErr := dupCmd.CombinedOutput()
	if dupErr == nil {
		t.Fatalf("duplicate slug insert should fail UNIQUE, but succeeded. Output: %s", dupOut)
	}

	dupRows := queryDoltCSV(t, dir, `
SELECT id FROM issues WHERE slug = 'shared-slug'`)
	if len(dupRows) != 1 {
		t.Fatalf("rows with slug='shared-slug' = %d, want 1: %v", len(dupRows), dupRows)
	}
}

// TestBrainMigration0003IsIdempotentThroughDoltCLI applies the source 0056.up.sql
// twice and asserts the second pass is a no-op.
func TestBrainMigration0003IsIdempotentThroughDoltCLI(t *testing.T) {
	testutil.RequireDoltBinary(t)

	upSQL0056, err := os.ReadFile("migrations/brain/0003_brain_slug_unique.up.sql")
	if err != nil {
		t.Fatalf("read 0056 up: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "idempotent-0056")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create idempotent test dir: %v", err)
	}
	runDoltCommand(t, dir, "init", "--name", "test", "--email", "test@example.com")
	runDoltSQL(t, dir, AllMigrationsSQL())

	runDoltSQL(t, dir, string(upSQL0056))
	runDoltSQL(t, dir, string(upSQL0056))
}

func TestCLICompatibleBrainMigration0001UsesDirectAddColumnDDL(t *testing.T) {
	got := cliCompatibleBrainMigrationSQL("0001_brain_isa_columns.up.sql", "source migration")
	for _, want := range []string{
		"ALTER TABLE issues ADD COLUMN slug VARCHAR(255) DEFAULT NULL",
		"ALTER TABLE issues ADD COLUMN isa_phase VARCHAR(32) DEFAULT NULL",
		"ALTER TABLE issues ADD COLUMN isa_progress_m INT DEFAULT NULL",
		"ALTER TABLE issues ADD COLUMN isa_progress_n INT DEFAULT NULL",
		"ALTER TABLE issues ADD COLUMN isa_effort VARCHAR(8) DEFAULT NULL",
		"ALTER TABLE issues ADD COLUMN isa_mode VARCHAR(32) DEFAULT NULL",
		"ALTER TABLE issues ADD COLUMN isa_started_at DATETIME NULL DEFAULT NULL",
		"ALTER TABLE issues ADD COLUMN isa_updated_at DATETIME NULL DEFAULT NULL",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("0054 CLI migration missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"PREPARE",
		"EXECUTE",
		"DEALLOCATE",
		"@needs_slug",
		"@needs_isa_phase",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("0054 CLI migration contains prepared-DDL marker %q", forbidden)
		}
	}
}

// TestSourceBrainMigrations0001And0002AreIdempotentThroughDoltCLI applies the source
// .up.sql files (not the CLI-compatible direct DDL bundle) twice against a
// fresh Dolt database to verify the INFORMATION_SCHEMA-guarded ADD COLUMN
// pattern and CREATE TABLE IF NOT EXISTS together produce no errors on the
// second pass — the contract called out in ISC-22.
func TestSourceBrainMigrations0001And0002AreIdempotentThroughDoltCLI(t *testing.T) {
	testutil.RequireDoltBinary(t)

	upSQL0054, err := os.ReadFile("migrations/brain/0001_brain_isa_columns.up.sql")
	if err != nil {
		t.Fatalf("read 0054 up: %v", err)
	}
	upSQL0055, err := os.ReadFile("migrations/brain/0002_brain_isa_sections.up.sql")
	if err != nil {
		t.Fatalf("read 0055 up: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "idempotent-isa")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create idempotent test dir: %v", err)
	}
	runDoltCommand(t, dir, "init", "--name", "test", "--email", "test@example.com")

	// Stand up the issues table the migrations depend on, using the existing
	// fresh-bundle DDL so all prior columns are in place.
	runDoltSQL(t, dir, AllMigrationsSQL())

	// First pass against the source migrations (the runtime path, not the
	// CLI direct-DDL bundle). The PREPARE/EXECUTE guards in 0054 should be a
	// no-op because AllMigrationsSQL already added the columns; the
	// CREATE TABLE IF NOT EXISTS in 0055 should be a no-op for the same
	// reason.
	runDoltSQL(t, dir, string(upSQL0054))
	runDoltSQL(t, dir, string(upSQL0055))

	// Second pass — must not error. This is the idempotency contract.
	runDoltSQL(t, dir, string(upSQL0054))
	runDoltSQL(t, dir, string(upSQL0055))

	// Shape spot-check: still single columns, single table, after two passes.
	rows := queryDoltCSV(t, dir, `
SELECT COUNT(*) AS n
FROM information_schema.columns
WHERE table_schema = DATABASE()
  AND table_name = 'issues'
  AND column_name IN ('slug', 'isa_phase', 'isa_progress_m', 'isa_progress_n',
                      'isa_effort', 'isa_mode', 'isa_started_at',
                      'isa_updated_at')`)
	if len(rows) != 1 || rows[0]["n"] != "8" {
		t.Fatalf("expected exactly 8 ISA columns after two idempotent passes, got: %v", rows)
	}

	rows = queryDoltCSV(t, dir, `
SELECT COUNT(*) AS n
FROM information_schema.tables
WHERE table_schema = DATABASE()
  AND table_name = 'isa_sections'`)
	if len(rows) != 1 || rows[0]["n"] != "1" {
		t.Fatalf("expected isa_sections table to exist exactly once after two idempotent passes, got: %v", rows)
	}
}
