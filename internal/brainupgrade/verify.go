package brainupgrade

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/steveyegge/beads/internal/storage/schema"
)

// Table statuses in a MigrationReport.
const (
	// TableIdentical: every column of every row the table held is unchanged.
	TableIdentical = "identical"
	// TableRewritten: every row is kept and every column other than the ones
	// the migration rewrites on purpose is unchanged; those were rewritten.
	TableRewritten = "rewritten"
	// TableChanged: rows or columns the migration has no business touching
	// differ. A failure.
	TableChanged = "changed"
	// TableMissing: the table is gone, or a column it had is. A failure.
	TableMissing = "missing"
	// TableCursor: a migration cursor, which the migration advances; the
	// schema check (every series at latest) covers it.
	TableCursor = "cursor"
)

// cursorTables are the migration cursors.
var cursorTables = map[string]bool{
	"schema_migrations":           true,
	"ignored_schema_migrations":   true,
	schema.BrainSchemaCursorTable: true,
}

// TableCheck compares one table before and after the migration. The
// digests are over the columns the table had before.
type TableCheck struct {
	Table              string   `json:"table"`
	Status             string   `json:"status"`
	RowsBefore         int64    `json:"rows_before"`
	RowsAfter          int64    `json:"rows_after"`
	DigestBefore       string   `json:"digest_before"`
	DigestAfter        string   `json:"digest_after,omitempty"`
	StableDigestBefore string   `json:"stable_digest_before"`
	StableDigestAfter  string   `json:"stable_digest_after,omitempty"`
	Rewritten          []string `json:"rewritten,omitempty"`
	AddedColumns       []string `json:"added_columns,omitempty"`
	Detail             string   `json:"detail,omitempty"`
}

// MigrationReport is the post-migration verification of one database.
type MigrationReport struct {
	Database     string             `json:"database"`
	DBHashBefore string             `json:"db_hash_before"`
	DBHashAfter  string             `json:"db_hash_after"`
	Schema       schema.UpgradePlan `json:"schema"`
	Tables       []TableCheck       `json:"tables"`
	NewTables    []string           `json:"new_tables,omitempty"`
	Failures     []string           `json:"failures,omitempty"`
}

// OK reports whether the migration verified.
func (r MigrationReport) OK() bool { return len(r.Failures) == 0 }

// VerifyMigrated compares the migrated database conn has selected with
// before, the state its backup recorded. It fails on any schema series not at
// its latest version, on any table or column that is gone, and on any row
// count or digest difference outside the columns the migration rewrites.
func VerifyMigrated(ctx context.Context, conn *sql.Conn, before DatabaseState) (MigrationReport, error) {
	rep := MigrationReport{Database: before.Name, DBHashBefore: before.DBHash}
	name, dbHash, err := Identity(ctx, conn)
	if err != nil {
		return rep, err
	}
	if name != before.Name {
		return rep, fmt.Errorf("connected to %s, but the backed-up state is of %s", name, before.Name)
	}
	rep.DBHashAfter = dbHash
	if rep.Schema, err = schema.PlanUpgrade(ctx, conn); err != nil {
		return rep, err
	}
	if rep.Schema.Pending() {
		rep.Failures = append(rep.Failures, fmt.Sprintf("schema not at latest: main %d/%d, ignored %d/%d, brain %d/%d, brain rows in schema_migrations %d",
			rep.Schema.MainEffective, rep.Schema.MainLatest, rep.Schema.IgnoredCurrent, rep.Schema.IgnoredLatest,
			rep.Schema.BrainCurrent, rep.Schema.BrainLatest, len(rep.Schema.BrainRowsInMain)))
	}

	tables, err := ListTables(ctx, conn)
	if err != nil {
		return rep, err
	}
	present := map[string]bool{}
	for _, t := range tables {
		present[t] = true
	}
	for _, b := range before.Tables {
		check := TableCheck{Table: b.Name, RowsBefore: b.Rows, DigestBefore: b.Digest, StableDigestBefore: b.StableDigest}
		if !present[b.Name] {
			check.Status, check.Detail = TableMissing, "table no longer exists"
			rep.Tables = append(rep.Tables, check)
			rep.Failures = append(rep.Failures, fmt.Sprintf("table %s no longer exists", b.Name))
			continue
		}
		delete(present, b.Name)
		if err := checkTable(ctx, conn, b, &check); err != nil {
			return rep, err
		}
		if check.Status == TableChanged || check.Status == TableMissing {
			rep.Failures = append(rep.Failures, fmt.Sprintf("table %s: %s", b.Name, check.Detail))
		}
		rep.Tables = append(rep.Tables, check)
	}
	for name := range present {
		rep.NewTables = append(rep.NewTables, name)
	}
	sort.Strings(rep.NewTables)
	return rep, nil
}

func checkTable(ctx context.Context, conn *sql.Conn, b TableState, check *TableCheck) error {
	after, err := Columns(ctx, conn, b.Name)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, c := range after {
		have[c] = true
	}
	had := map[string]bool{}
	for _, c := range b.Columns {
		had[c] = true
		if !have[c] {
			check.Status, check.Detail = TableMissing, fmt.Sprintf("column %s no longer exists", c)
			return nil
		}
	}
	for _, c := range after {
		if !had[c] {
			check.AddedColumns = append(check.AddedColumns, c)
		}
	}
	if check.RowsAfter, check.StableDigestAfter, err = Digest(ctx, conn, b.Name, b.StableColumns); err != nil {
		return err
	}
	if len(b.StableColumns) == len(b.Columns) {
		check.DigestAfter = check.StableDigestAfter
	} else if _, check.DigestAfter, err = Digest(ctx, conn, b.Name, b.Columns); err != nil {
		return err
	}
	switch {
	case cursorTables[b.Name]:
		check.Status = TableCursor
	case check.RowsAfter != b.Rows:
		check.Status, check.Detail = TableChanged, fmt.Sprintf("rows %d, before %d", check.RowsAfter, b.Rows)
	case check.StableDigestAfter != b.StableDigest:
		check.Status, check.Detail = TableChanged, "contents differ outside the columns the migration rewrites"
	case check.DigestAfter == b.Digest:
		check.Status = TableIdentical
	default:
		check.Status = TableRewritten
		check.Rewritten = RewrittenColumns(b.Name)
	}
	return nil
}
