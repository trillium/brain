package schema

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
)

// The brain migration series.
//
// Brain's own schema (the ISA columns, the isa_sections table and the slug
// unique index) used to ride the main numbered series. Every rebase onto a
// newer upstream renumbered it (0050-0052, then 0052-0054, then 0054-0056),
// and upstream 1.3 claimed 0054-0066 for itself, so a database that had
// recorded brain's 0054-0056 would have read as "already at 56" to the 1.3
// runner and silently skipped upstream's 0054-0056 (the lease columns, the
// leases table, the comments keyset index).
//
// The series now lives on its own: migrations/brain/*.up.sql, cursor table
// brain_schema_migrations, applied right after the main series in the same
// MigrateUp pass. Its numbering never meets upstream's again. The file bodies
// are byte-identical to the old numbered files, so the content hashes recorded
// in a database's schema_migrations identify brain's rows exactly whatever
// number they were recorded under.

//go:embed migrations/brain/*.up.sql
var upBrainMigrations embed.FS

// brainSeriesEnabled turns the brain series (and the move of brain-numbered
// rows out of the main series) on for MigrateUp, migrationWorkNeeded and
// AllMigrationsSQL. The store packages that open databases turn it on at init
// (brain_schema.go in dolt, embeddeddolt and uow), so every brain binary runs
// it; this package's own tests leave it off unless a test turns it on, which
// keeps upstream's exact-statement migration tests describing upstream's pass.
var brainSeriesEnabled atomic.Bool

// EnableBrainSeries turns the brain migration series on for this process.
func EnableBrainSeries() { brainSeriesEnabled.Store(true) }

// BrainSeriesEnabled reports whether the brain migration series is on.
func BrainSeriesEnabled() bool { return brainSeriesEnabled.Load() }

// SetBrainSeriesForTest sets the switch and returns a function restoring the
// previous value.
func SetBrainSeriesForTest(on bool) func() {
	prev := brainSeriesEnabled.Load()
	brainSeriesEnabled.Store(on)
	return func() { brainSeriesEnabled.Store(prev) }
}

var brainSource = migrationSource{
	files:       upBrainMigrations,
	dir:         "migrations/brain",
	cursorTable: "brain_schema_migrations",
}

// BrainSchemaCursorTable is the cursor table of the brain migration series.
const BrainSchemaCursorTable = "brain_schema_migrations"

// BrainMigrationSQL returns the frozen body of a brain-series migration by
// file name (e.g. "0001_brain_isa_columns.up.sql"), for tests outside this
// package that rebuild a database brain migrated under main-series numbers.
func BrainMigrationSQL(name string) (string, error) {
	data, err := brainSource.files.ReadFile(brainSource.dir + "/" + name)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// BrainMigrationNames lists the brain-series migration files in order.
func BrainMigrationNames() []string {
	var out []string
	for _, f := range brainSource.list() {
		out = append(out, f.name)
	}
	return out
}

// LatestBrainVersion answers the highest migration in the brain series.
func LatestBrainVersion() int { return brainSource.latest() }

// CurrentBrainVersion answers the brain series cursor of the connected
// database (0 when the cursor table does not exist yet).
func CurrentBrainVersion(ctx context.Context, db DBConn) (int, error) {
	return brainSource.currentVersion(ctx, db)
}

// brainMigrationHashes maps the content hash of every brain migration body to
// its version in the brain series. The bodies never changed across the
// renumberings, so one hash per migration covers every number it was ever
// recorded under in schema_migrations.
func brainMigrationHashes() map[string]int {
	out := map[string]int{}
	for _, f := range brainSource.list() {
		data, err := brainSource.files.ReadFile(brainSource.dir + "/" + f.name)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		out[hex.EncodeToString(sum[:])] = f.version
	}
	return out
}

// BrainNumberedRow is one schema_migrations row that records a brain migration
// under a main-series number.
type BrainNumberedRow struct {
	MainVersion  int
	BrainVersion int
	ContentHash  string
}

// FindBrainNumberedMainRows lists the schema_migrations rows whose content
// hash is a brain migration body. An empty result means the database never ran
// brain's migrations through the main series (a fresh 1.3 database, or plain
// upstream beads).
func FindBrainNumberedMainRows(ctx context.Context, db DBConn) ([]BrainNumberedRow, error) {
	present, err := schemaTableExists(ctx, db, mainSource.cursorTable)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	hasHash, err := mainSource.hasContentHashColumn(ctx, db)
	if err != nil || !hasHash {
		return nil, err
	}
	known := brainMigrationHashes()
	rows, err := db.QueryContext(ctx, "SELECT version, COALESCE(content_hash, '') FROM "+mainSource.cursorTable+" ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", mainSource.cursorTable, err)
	}
	defer func() { _ = rows.Close() }()
	var found []BrainNumberedRow
	for rows.Next() {
		var version int
		var hash string
		if err := rows.Scan(&version, &hash); err != nil {
			return nil, fmt.Errorf("reading %s: %w", mainSource.cursorTable, err)
		}
		if bv, ok := known[strings.ToLower(strings.TrimSpace(hash))]; ok {
			found = append(found, BrainNumberedRow{MainVersion: version, BrainVersion: bv, ContentHash: hash})
		}
	}
	return found, rows.Err()
}

// ErrBrainRowsNotAtTop is returned when brain's rows in schema_migrations are
// not the highest versions recorded there. Moving them out would then leave a
// main cursor above upstream migrations that never ran, which is the silent
// skip this heal exists to prevent, so the heal refuses instead.
type ErrBrainRowsNotAtTop struct {
	BrainVersions []int
	Above         []int
}

func (e *ErrBrainRowsNotAtTop) Error() string {
	return fmt.Sprintf("schema_migrations records brain migrations at %v but upstream migrations %v above them; "+
		"moving brain's rows out would leave upstream migrations below the cursor unapplied. "+
		"Restore from a backup taken before the mixed numbering, or repair the cursor by hand", e.BrainVersions, e.Above)
}

// healBrainNumberedMainMigrations moves brain's rows out of schema_migrations
// into brain_schema_migrations, so the main cursor falls back to the last
// upstream migration that actually ran and the 1.3 series applies on top of
// it. Brain's own migrations are not re-run: their rows land in the brain
// cursor under their brain version, with their recorded hash.
//
// It refuses (ErrBrainRowsNotAtTop) unless brain's rows are the top of the
// main series, and it commits its own change before the migration pass reads
// the working set, so the pass's dirty-table guards see a clean start. It
// reports whether it changed anything.
func healBrainNumberedMainMigrations(ctx context.Context, db DBConn) (bool, error) {
	found, err := FindBrainNumberedMainRows(ctx, db)
	if err != nil {
		return false, fmt.Errorf("finding brain-numbered schema migrations: %w", err)
	}
	if len(found) == 0 {
		return false, nil
	}
	lowest := found[0].MainVersion
	brainAt := map[int]bool{}
	var brainVersions []int
	for _, r := range found {
		brainAt[r.MainVersion] = true
		brainVersions = append(brainVersions, r.MainVersion)
		if r.MainVersion < lowest {
			lowest = r.MainVersion
		}
	}
	above, err := mainVersionsAbove(ctx, db, lowest)
	if err != nil {
		return false, err
	}
	var foreign []int
	for _, v := range above {
		if !brainAt[v] {
			foreign = append(foreign, v)
		}
	}
	if len(foreign) > 0 {
		sort.Ints(brainVersions)
		return false, &ErrBrainRowsNotAtTop{BrainVersions: brainVersions, Above: foreign}
	}

	if _, err := db.ExecContext(ctx, brainSource.bootstrapSQL()); err != nil {
		return false, fmt.Errorf("creating %s: %w", brainSource.cursorTable, err)
	}
	if _, err := brainSource.ensureContentHashColumn(ctx, db); err != nil {
		return false, err
	}
	for _, r := range found {
		if _, err := db.ExecContext(ctx,
			"INSERT IGNORE INTO "+brainSource.cursorTable+" (version, content_hash) VALUES (?, ?)",
			r.BrainVersion, r.ContentHash); err != nil {
			return false, fmt.Errorf("recording brain migration %d in %s: %w", r.BrainVersion, brainSource.cursorTable, err)
		}
		if _, err := db.ExecContext(ctx,
			"DELETE FROM "+mainSource.cursorTable+" WHERE version = ?", r.MainVersion); err != nil {
			return false, fmt.Errorf("removing brain migration from %s version %d: %w", mainSource.cursorTable, r.MainVersion, err)
		}
	}
	for _, table := range []string{mainSource.cursorTable, brainSource.cursorTable} {
		if err := DrainCall(ctx, db, "CALL DOLT_ADD('-f', ?)", table); err != nil {
			return false, fmt.Errorf("dolt add %s: %w", table, err)
		}
	}
	if err := DrainCall(ctx, db, "CALL DOLT_COMMIT('-m', ?)",
		fmt.Sprintf("schema: move brain migrations %v out of the main series into %s", brainVersions, brainSource.cursorTable)); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "nothing to commit") {
			return false, fmt.Errorf("committing the brain migration move: %w", err)
		}
	}
	fmt.Fprintf(stderr, "Moved brain migrations recorded at %v into %s; upstream migrations above %d will apply.\n",
		brainVersions, brainSource.cursorTable, lowest-1)
	return true, nil
}

// mainVersionsAbove lists the schema_migrations versions at or above floor.
func mainVersionsAbove(ctx context.Context, db DBConn, floor int) ([]int, error) {
	rows, err := db.QueryContext(ctx, "SELECT version FROM "+mainSource.cursorTable+" WHERE version >= ? ORDER BY version", floor)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", mainSource.cursorTable, err)
	}
	defer func() { _ = rows.Close() }()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// brainSeriesSQL is the brain series in the fresh-bundle form AllMigrationsSQL
// emits for the main series: the cursor table, each body, and its cursor row.
func brainSeriesSQL() string {
	var b strings.Builder
	b.WriteString(brainSource.bootstrapSQL())
	b.WriteString(";\n")
	for _, f := range brainSource.list() {
		data, err := brainSource.files.ReadFile(brainSource.dir + "/" + f.name)
		if err != nil {
			continue
		}
		b.WriteString(cliCompatibleBrainMigrationSQL(f.name, string(data)))
		sum := sha256.Sum256(data)
		fmt.Fprintf(&b, "\nINSERT IGNORE INTO %s (version, content_hash) VALUES (%d, '%s');\n",
			brainSource.cursorTable, f.version, hex.EncodeToString(sum[:]))
	}
	return b.String()
}

// cliCompatibleBrainMigrationSQL is cliCompatibleMigrationSQL for the brain
// series. Fresh bundles have no issues rows yet, so the INFORMATION_SCHEMA
// guards in the runtime bodies (which exist for upgrading existing databases,
// and which the Dolt CLI does not execute as prepared ALTERs) are replaced by
// the direct schema delta.
func cliCompatibleBrainMigrationSQL(name, sqlText string) string {
	switch name {
	case "0001_brain_isa_columns.up.sql":
		return cliBrainMigration0001IsaColumns
	case "0003_brain_slug_unique.up.sql":
		return cliBrainMigration0003SlugUnique
	default:
		return sqlText
	}
}

const cliBrainMigration0001IsaColumns = `ALTER TABLE issues ADD COLUMN slug VARCHAR(255) DEFAULT NULL;
ALTER TABLE issues ADD COLUMN isa_phase VARCHAR(32) DEFAULT NULL;
ALTER TABLE issues ADD COLUMN isa_progress_m INT DEFAULT NULL;
ALTER TABLE issues ADD COLUMN isa_progress_n INT DEFAULT NULL;
ALTER TABLE issues ADD COLUMN isa_effort VARCHAR(8) DEFAULT NULL;
ALTER TABLE issues ADD COLUMN isa_mode VARCHAR(32) DEFAULT NULL;
ALTER TABLE issues ADD COLUMN isa_started_at DATETIME NULL DEFAULT NULL;
ALTER TABLE issues ADD COLUMN isa_updated_at DATETIME NULL DEFAULT NULL;`

const cliBrainMigration0003SlugUnique = `CREATE UNIQUE INDEX idx_issues_slug_unique ON issues (slug);`
