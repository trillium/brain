package schema

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/steveyegge/beads/internal/testutil"
)

func TestPendingMigrationDirtyTablesDetectsMigration0043Dependencies(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM schema_migrations`).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(42))

	touched, err := mainSource.pendingMigrationDirtyTables(context.Background(), db, map[string]dirtyTableState{
		"dependencies": {},
	})
	if err != nil {
		t.Fatalf("pendingMigrationDirtyTables: %v", err)
	}
	if len(touched) != 1 || touched[0] != "dependencies" {
		t.Fatalf("touched = %v, want [dependencies]", touched)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestIgnoredPendingMigrationDirtyTablesDetectsWispDependencies(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM ignored_schema_migrations`).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(2))

	touched, err := ignoredSource.pendingMigrationDirtyTables(context.Background(), db, map[string]dirtyTableState{
		"wisp_dependencies": {},
	})
	if err != nil {
		t.Fatalf("pendingMigrationDirtyTables: %v", err)
	}
	if len(touched) != 1 || touched[0] != "wisp_dependencies" {
		t.Fatalf("touched = %v, want [wisp_dependencies]", touched)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestMigrationSQLTouchesTableStatementForms(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want bool
	}{
		{
			name: "rename table source",
			sql:  "RENAME TABLE dependencies TO old_dependencies",
			want: true,
		},
		{
			name: "rename table target",
			sql:  "RENAME TABLE old_dependencies TO dependencies",
			want: true,
		},
		{
			name: "create index on table",
			sql:  "CREATE INDEX idx_dep_type ON dependencies (type)",
			want: true,
		},
		{
			name: "create unique index on quoted table",
			sql:  "CREATE UNIQUE INDEX idx_dep_type ON `dependencies` (type)",
			want: true,
		},
		{
			name: "create view named table",
			sql:  "CREATE OR REPLACE VIEW dependencies AS SELECT 1",
			want: true,
		},
		{
			name: "select only",
			sql:  "SELECT * FROM dependencies",
			want: false,
		},
		{
			name: "unrelated ddl",
			sql:  "ALTER TABLE comments ADD COLUMN reviewed_at DATETIME",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := migrationSQLTouchesTable(tt.sql, "dependencies"); got != tt.want {
				t.Fatalf("migrationSQLTouchesTable(%q) = %v, want %v", tt.sql, got, tt.want)
			}
		})
	}
}

func TestCheckNoDuplicateVersionsPanicsWithBothFilenames(t *testing.T) {
	files := []migrationFile{
		{version: 7, name: "0007_create_metadata.up.sql"},
		{version: 12, name: "0012_create_routes.up.sql"},
		{version: 7, name: "0007_create_duplicate.up.sql"},
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on duplicate version, got none")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("expected string panic, got %T: %v", r, r)
		}
		for _, want := range []string{
			"duplicate migration version 7",
			"0007_create_metadata.up.sql",
			"0007_create_duplicate.up.sql",
			"renumber one before commit",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("panic message %q missing expected substring %q", msg, want)
			}
		}
	}()
	checkNoDuplicateVersions(files)
}

func TestDirtyTableSignatureRejectsUnsafeTableName(t *testing.T) {
	_, err := dirtyTableSignature(context.Background(), nil, "issues'); SELECT 1; --")
	if err == nil {
		t.Fatal("expected unsafe table name error")
	}
	if !strings.Contains(err.Error(), "unsafe dolt status table name") {
		t.Fatalf("error = %v, want unsafe table name context", err)
	}
}

func TestMigration0035HandlesLegacyWispDependenciesShape(t *testing.T) {
	upSQL, err := os.ReadFile("migrations/0035_migrate_infra_to_wisps.up.sql")
	if err != nil {
		t.Fatalf("read 0035 up migration: %v", err)
	}
	downSQL, err := os.ReadFile("migrations/0035_migrate_infra_to_wisps.down.sql")
	if err != nil {
		t.Fatalf("read 0035 down migration: %v", err)
	}

	up := string(upSQL)
	for _, want := range []string{
		"@has_split_wisp_dependencies",
		"INSERT IGNORE INTO wisp_dependencies (issue_id, depends_on_id, type, created_at, created_by, metadata, thread_id)",
		"INSERT IGNORE INTO wisp_dependencies (issue_id, depends_on_issue_id, depends_on_wisp_id, depends_on_external, type, created_at, created_by, metadata, thread_id)",
	} {
		if !strings.Contains(up, want) {
			t.Fatalf("0035 up migration missing legacy/split branch marker %q", want)
		}
	}

	down := string(downSQL)
	for _, want := range []string{
		"@has_split_wisp_dependencies",
		"SELECT issue_id, depends_on_id, type, created_at, created_by, metadata, thread_id FROM wisp_dependencies",
		"SELECT issue_id, COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external), type, created_at, created_by, metadata, thread_id FROM wisp_dependencies",
	} {
		if !strings.Contains(down, want) {
			t.Fatalf("0035 down migration missing legacy/split branch marker %q", want)
		}
	}
}

func TestMigration0047HandlesLegacyWispDependenciesShape(t *testing.T) {
	sql, err := os.ReadFile("migrations/0047_recompute_mixed_is_blocked.up.sql")
	if err != nil {
		t.Fatalf("read 0047 up migration: %v", err)
	}

	body := string(sql)
	for _, want := range []string{
		"@wisp_dependencies_needs_split",
		"ALTER TABLE wisp_dependencies ADD COLUMN depends_on_issue_id",
		"ALTER TABLE wisp_dependencies ADD COLUMN depends_on_wisp_id",
		"ALTER TABLE wisp_dependencies ADD COLUMN depends_on_id VARCHAR(255) AS",
		"cd.depends_on_issue_id",
		"d.depends_on_wisp_id",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("0047 migration missing legacy wisp_dependencies compatibility marker %q", want)
		}
	}
}

func TestCLICompatibleMigration0046UsesFreshSchemaDDLOnly(t *testing.T) {
	got := cliCompatibleMigrationSQL("0046_add_is_blocked.up.sql", "source migration")
	for _, want := range []string{
		"ALTER TABLE issues ADD COLUMN is_blocked TINYINT(1) NOT NULL DEFAULT 0",
		"CREATE INDEX idx_issues_is_blocked ON issues(is_blocked, status)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("0046 CLI migration missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"UPDATE issues",
		"WITH RECURSIVE",
		"directly_blocked",
		"recursively_blocked",
		"parent-child",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("0046 CLI migration contains dead backfill marker %q", forbidden)
		}
	}
}

func TestCLICompatibleMigration0008MatchesRuntimeChildCountersFK(t *testing.T) {
	got := cliCompatibleMigrationSQL("0008_create_child_counters.up.sql", "source migration")
	if want := "CONSTRAINT fk_counter_parent FOREIGN KEY (parent_id) REFERENCES issues(id) ON DELETE CASCADE ON UPDATE CASCADE"; !strings.Contains(got, want) {
		t.Fatalf("0008 CLI migration missing %q", want)
	}
}

func TestCLICompatibleMigration0032UsesDirectDropColumn(t *testing.T) {
	got := cliCompatibleMigrationSQL("0032_drop_schema_migrations_applied_at.up.sql", "source migration")
	if want := "ALTER TABLE schema_migrations DROP COLUMN applied_at"; !strings.Contains(got, want) {
		t.Fatalf("0032 CLI migration missing %q", want)
	}
	for _, forbidden := range []string{
		"PREPARE",
		"EXECUTE",
		"DEALLOCATE",
		"@needs_drop",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("0032 CLI migration contains prepared-DDL marker %q", forbidden)
		}
	}
}

func TestMigration0050SourceUsesIdempotentInformationSchemaGuards(t *testing.T) {
	upSQL, err := os.ReadFile("migrations/0050_add_isa_columns.up.sql")
	if err != nil {
		t.Fatalf("read 0050 up migration: %v", err)
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
			t.Errorf("0050 missing idempotency guard %q for column %s", guardVar, col)
		}
		if !strings.Contains(up, "ADD COLUMN "+col+" ") && !strings.Contains(up, "ADD COLUMN "+col+"\n") {
			t.Errorf("0050 missing ADD COLUMN clause for %s", col)
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
			t.Errorf("0050 missing INFORMATION_SCHEMA check %q", want)
		}
	}
}

func TestMigration0050DownDropsAllIsaColumns(t *testing.T) {
	downSQL, err := os.ReadFile("migrations/0050_add_isa_columns.down.sql")
	if err != nil {
		t.Fatalf("read 0050 down migration: %v", err)
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
			t.Errorf("0050 down missing DROP COLUMN %s", col)
		}
	}
}

func TestMigration0051CreatesIsaSectionsTable(t *testing.T) {
	upSQL, err := os.ReadFile("migrations/0051_create_isa_sections.up.sql")
	if err != nil {
		t.Fatalf("read 0051 up migration: %v", err)
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
			t.Errorf("0051 missing required clause %q", want)
		}
	}

	downSQL, err := os.ReadFile("migrations/0051_create_isa_sections.down.sql")
	if err != nil {
		t.Fatalf("read 0051 down migration: %v", err)
	}
	if !strings.Contains(string(downSQL), "DROP TABLE IF EXISTS isa_sections") {
		t.Error("0051 down migration missing DROP TABLE IF EXISTS isa_sections")
	}
}

func TestCLICompatibleMigration0050UsesDirectAddColumnDDL(t *testing.T) {
	got := cliCompatibleMigrationSQL("0050_add_isa_columns.up.sql", "source migration")
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
			t.Fatalf("0050 CLI migration missing %q", want)
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
			t.Fatalf("0050 CLI migration contains prepared-DDL marker %q", forbidden)
		}
	}
}

func TestCLICompatibleMigration0049UsesDirectLongtextDDL(t *testing.T) {
	got := cliCompatibleMigrationSQL("0049_longtext_large_content_columns.up.sql", "source migration")
	for _, want := range []string{
		"ALTER TABLE issues MODIFY COLUMN description LONGTEXT NOT NULL",
		"MODIFY COLUMN design LONGTEXT NOT NULL",
		"MODIFY COLUMN acceptance_criteria LONGTEXT NOT NULL",
		"MODIFY COLUMN notes LONGTEXT NOT NULL",
		"ALTER TABLE issues MODIFY COLUMN close_reason LONGTEXT DEFAULT ''",
		"ALTER TABLE wisps MODIFY COLUMN description LONGTEXT NOT NULL DEFAULT ''",
		"ALTER TABLE wisps MODIFY COLUMN close_reason LONGTEXT DEFAULT ''",
		"ALTER TABLE comments MODIFY COLUMN text LONGTEXT NOT NULL",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("0049 CLI migration missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"PREPARE",
		"EXECUTE",
		"DEALLOCATE",
		"@issues_needs_fix",
		"@comments_needs_fix",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("0049 CLI migration contains prepared-DDL marker %q", forbidden)
		}
	}
}

func TestCLICompatibleMigration0039PreservesRuntimeChildCountersFK(t *testing.T) {
	got := cliCompatibleMigrationSQL("0039_drop_child_counters_fk.up.sql", "source migration")
	if strings.TrimSpace(got) != "SELECT 1;" {
		t.Fatalf("0039 CLI migration = %q, want SELECT 1", got)
	}
}

func TestAllMigrationsSQLUsesDirectDDLForKnownCLIIncompatibilities(t *testing.T) {
	got := AllMigrationsSQL()
	for _, want := range []string{
		"CONSTRAINT fk_counter_parent FOREIGN KEY (parent_id) REFERENCES issues(id) ON DELETE CASCADE ON UPDATE CASCADE",
		"ALTER TABLE schema_migrations DROP COLUMN applied_at",
		"ALTER TABLE issues MODIFY COLUMN close_reason LONGTEXT DEFAULT ''",
		"ALTER TABLE comments MODIFY COLUMN text LONGTEXT NOT NULL",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("AllMigrationsSQL missing direct CLI DDL %q", want)
		}
	}
	for _, forbidden := range []string{
		"COLUMN_NAME = 'applied_at'",
		"ALTER TABLE child_counters DROP FOREIGN KEY fk_counter_parent",
		"@issues_cr_needs_fix",
		"@comments_needs_fix",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("AllMigrationsSQL contains source prepared-DDL guard %q", forbidden)
		}
	}
}

func TestAllMigrationsSQLAppliesThroughDoltCLIAndRecordsLatestVersion(t *testing.T) {
	testutil.RequireDoltBinary(t)

	dir := filepath.Join(t.TempDir(), "cli-bundle")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create CLI bundle dir: %v", err)
	}
	runDoltCommand(t, dir, "init", "--name", "test", "--email", "test@example.com")
	runDoltSQL(t, dir, AllMigrationsSQL())

	rows := queryDoltCSV(t, dir, `
SELECT COALESCE(MAX(version), 0) AS max_version, COUNT(*) AS version_count
FROM schema_migrations`)
	if len(rows) != 1 {
		t.Fatalf("schema_migrations query returned %d rows, want 1", len(rows))
	}
	want := strconv.Itoa(LatestVersion())
	if got := rows[0]["max_version"]; got != want {
		t.Fatalf("MAX(version) = %s, want %s", got, want)
	}
	if got := rows[0]["version_count"]; got != want {
		t.Fatalf("COUNT(*) = %s, want %s", got, want)
	}

	requireDoltNoRows(t, dir, `
SELECT column_name
FROM information_schema.columns
WHERE table_schema = DATABASE()
  AND table_name = 'schema_migrations'
  AND column_name = 'applied_at'`, "schema_migrations.applied_at")
	requireDoltFKRules(t, dir, "fk_comments_issue", "CASCADE", "CASCADE")
	requireDoltColumnShape(t, dir, "comments", "text", "longtext", "NO")
	requireDoltColumnShape(t, dir, "issues", "description", "longtext", "NO")
	requireDoltColumnShape(t, dir, "wisps", "description", "longtext", "NO")
	requireDoltColumnShape(t, dir, "wisps", "no_history", "tinyint(1)", "YES")
	requireDoltColumnShape(t, dir, "wisps", "started_at", "datetime", "YES")
	requireDoltColumnShape(t, dir, "wisps", "wisp_type", "varchar(32)", "YES")

	// 0050 ISA columns on issues.
	requireDoltColumnShape(t, dir, "issues", "slug", "varchar(255)", "YES")
	requireDoltColumnShape(t, dir, "issues", "isa_phase", "varchar(32)", "YES")
	requireDoltColumnShape(t, dir, "issues", "isa_progress_m", "int", "YES")
	requireDoltColumnShape(t, dir, "issues", "isa_progress_n", "int", "YES")
	requireDoltColumnShape(t, dir, "issues", "isa_effort", "varchar(8)", "YES")
	requireDoltColumnShape(t, dir, "issues", "isa_mode", "varchar(32)", "YES")
	requireDoltColumnShape(t, dir, "issues", "isa_started_at", "datetime", "YES")
	requireDoltColumnShape(t, dir, "issues", "isa_updated_at", "datetime", "YES")

	// 0051 isa_sections table and its columns.
	requireDoltColumnShape(t, dir, "isa_sections", "issue_id", "varchar(255)", "NO")
	requireDoltColumnShape(t, dir, "isa_sections", "section_name", "varchar(64)", "NO")
	requireDoltColumnShape(t, dir, "isa_sections", "body", "longtext", "NO")
	requireDoltColumnShape(t, dir, "isa_sections", "updated_at", "timestamp", "NO")
}

// TestSourceMigrations0050And0051AreIdempotentThroughDoltCLI applies the source
// .up.sql files (not the CLI-compatible direct DDL bundle) twice against a
// fresh Dolt database to verify the INFORMATION_SCHEMA-guarded ADD COLUMN
// pattern and CREATE TABLE IF NOT EXISTS together produce no errors on the
// second pass — the contract called out in ISC-22.
func TestSourceMigrations0050And0051AreIdempotentThroughDoltCLI(t *testing.T) {
	testutil.RequireDoltBinary(t)

	upSQL0050, err := os.ReadFile("migrations/0050_add_isa_columns.up.sql")
	if err != nil {
		t.Fatalf("read 0050 up: %v", err)
	}
	upSQL0051, err := os.ReadFile("migrations/0051_create_isa_sections.up.sql")
	if err != nil {
		t.Fatalf("read 0051 up: %v", err)
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
	// CLI direct-DDL bundle). The PREPARE/EXECUTE guards in 0050 should be a
	// no-op because AllMigrationsSQL already added the columns; the
	// CREATE TABLE IF NOT EXISTS in 0051 should be a no-op for the same
	// reason.
	runDoltSQL(t, dir, string(upSQL0050))
	runDoltSQL(t, dir, string(upSQL0051))

	// Second pass — must not error. This is the idempotency contract.
	runDoltSQL(t, dir, string(upSQL0050))
	runDoltSQL(t, dir, string(upSQL0051))

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

func runDoltCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("dolt", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dolt %v failed in %s: %v\nOutput: %s", args, dir, err, output)
	}
}

func runDoltSQL(t *testing.T, dir, query string) {
	t.Helper()
	args := []string{"sql", "-q", query}
	runDoltCommand(t, dir, args...)
}

func queryDoltCSV(t *testing.T, dir, query string) []map[string]string {
	t.Helper()
	cmd := exec.Command("dolt", "sql", "-q", query, "-r", "csv")
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dolt sql query failed in %s: %v\nQuery: %s\nOutput: %s", dir, err, query, output)
	}
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil
	}
	records, err := csv.NewReader(strings.NewReader(trimmed)).ReadAll()
	if err != nil {
		t.Fatalf("parse dolt CSV output: %v\nRaw: %s", err, output)
	}
	if len(records) < 2 {
		return nil
	}
	headers := records[0]
	rows := make([]map[string]string, 0, len(records)-1)
	for _, record := range records[1:] {
		row := make(map[string]string, len(headers))
		for i, header := range headers {
			if i < len(record) {
				row[header] = record[i]
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func requireDoltNoRows(t *testing.T, dir, query, subject string) {
	t.Helper()
	if rows := queryDoltCSV(t, dir, query); len(rows) != 0 {
		t.Fatalf("%s query returned %d rows, want none: %v", subject, len(rows), rows)
	}
}

func requireDoltFKRules(t *testing.T, dir, constraintName, wantUpdate, wantDelete string) {
	t.Helper()
	rows := queryDoltCSV(t, dir, fmt.Sprintf(`
SELECT update_rule AS update_rule, delete_rule AS delete_rule
FROM information_schema.referential_constraints
WHERE constraint_schema = DATABASE()
  AND constraint_name = %s`, doltSQLString(constraintName)))
	if len(rows) != 1 {
		t.Fatalf("%s FK query returned %d rows, want 1: %v", constraintName, len(rows), rows)
	}
	if got := rows[0]["update_rule"]; got != wantUpdate {
		t.Fatalf("%s UPDATE_RULE = %s, want %s", constraintName, got, wantUpdate)
	}
	if got := rows[0]["delete_rule"]; got != wantDelete {
		t.Fatalf("%s DELETE_RULE = %s, want %s", constraintName, got, wantDelete)
	}
}

func requireDoltColumnShape(t *testing.T, dir, tableName, columnName, wantType, wantNullable string) {
	t.Helper()
	rows := queryDoltCSV(t, dir, fmt.Sprintf(`
SELECT column_type AS column_type, is_nullable AS is_nullable
FROM information_schema.columns
WHERE table_schema = DATABASE()
  AND table_name = %s
  AND column_name = %s`, doltSQLString(tableName), doltSQLString(columnName)))
	if len(rows) != 1 {
		t.Fatalf("%s.%s column query returned %d rows, want 1: %v", tableName, columnName, len(rows), rows)
	}
	if got := rows[0]["column_type"]; got != wantType {
		t.Fatalf("%s.%s COLUMN_TYPE = %s, want %s", tableName, columnName, got, wantType)
	}
	if got := rows[0]["is_nullable"]; got != wantNullable {
		t.Fatalf("%s.%s IS_NULLABLE = %s, want %s", tableName, columnName, got, wantNullable)
	}
}

func doltSQLString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func TestStageSchemaTablesSkipsIgnoredTables(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`(?s)SELECT s\.table_name, s\.staged\s+FROM dolt_status s\s+WHERE NOT EXISTS`).
		WillReturnRows(sqlmock.NewRows([]string{"table_name", "staged"}).
			AddRow("schema_migrations", false))
	mock.ExpectQuery(`(?s)SELECT t\.TABLE_NAME\s+FROM INFORMATION_SCHEMA\.TABLES t\s+WHERE .*NOT EXISTS`).
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME"}).
			AddRow("schema_migrations"))
	mock.ExpectExec(`CALL DOLT_ADD\('-f', \?\)`).
		WithArgs("schema_migrations").
		WillReturnResult(sqlmock.NewResult(0, 1))

	staged, err := stageSchemaTables(context.Background(), db, map[string]dirtyTableState{})
	if err != nil {
		t.Fatalf("stageSchemaTables: %v", err)
	}
	if !staged {
		t.Fatal("stageSchemaTables staged = false, want true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestUnstageIgnoredTablesResetsExistingIgnoredTables(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`(?s)SELECT s\.table_name, s\.staged\s+FROM dolt_status s\s+WHERE EXISTS`).
		WillReturnRows(sqlmock.NewRows([]string{"table_name", "staged"}).
			AddRow("ignored_schema_migrations", true).
			AddRow("wisp_dependencies", true).
			AddRow("wisps", false))
	mock.ExpectExec(`CALL DOLT_RESET\(\?\)`).
		WithArgs("ignored_schema_migrations").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`CALL DOLT_RESET\(\?\)`).
		WithArgs("wisp_dependencies").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := unstageIgnoredTables(context.Background(), db); err != nil {
		t.Fatalf("unstageIgnoredTables: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
