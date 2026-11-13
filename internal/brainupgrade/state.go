// Package brainupgrade moves a brain Dolt database from the beads 1.1 schema
// to the beads 1.3 schema behind a verified backup.
//
// The steps are separate because they have different safety properties:
//
//	plan      reads the database and reports what the migration would do
//	backup    syncs a Dolt-native backup, opens it as a database on a Dolt
//	          server of its own and compares it with the database, table by
//	          table
//	apply     migrates a database whose verified backup still matches it
//	verify    compares the migrated database with the state its backup recorded
//	rollback  puts the backup back in place of the database (online through
//	          DOLT_BACKUP restore, or offline by laying the backup out in place
//	          of the stopped server's database directory) and proves the result
//	          is the backed-up state
//	rehearse  runs apply, verify and an offline rollback on a copy laid out
//	          from the backup, on a Dolt server of its own, never on the
//	          database itself
//
// The comparison is the B-13 whole-row digest: per table, the row count and
// bit_xor over every row's md5(json_object(<every column>)), which is
// order-independent. A backup or a rollback must reproduce the database
// exactly (also dolt_hashof_db, which covers the working set and the
// dolt-ignored tables). A migration must keep every row; the columns the 1.3
// migrations rewrite on purpose are left out of its digest and reported.
package brainupgrade

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/storage/schema"
)

// The brain migration series lives beside upstream's; without it MigrateUp
// would not move brain's rows out of schema_migrations.
func init() { schema.EnableBrainSeries() }

// rewrittenColumns are the columns the beads 1.3 migrations rewrite in place.
// Verify compares every other column of every row; for these it reports that
// the migration rewrote them.
var rewrittenColumns = map[string][]string{
	// Aux rows get content-derived ids (0061 and the re-key pass it anchors).
	"events":               {"id"},
	"comments":             {"id"},
	"issue_snapshots":      {"id"},
	"compaction_snapshots": {"id"},
	// Dependency ids are re-derived from the edge (the re-key ignored 0026
	// forces once more).
	"dependencies":      {"id"},
	"wisp_dependencies": {"id"},
	// is_blocked is recomputed with a NULL-safe waits-for gate (0059 for
	// issues, ignored 0015 for wisps).
	"issues": {"is_blocked"},
	"wisps":  {"is_blocked"},
}

// RewrittenColumns returns the columns of table the migration may rewrite.
func RewrittenColumns(table string) []string {
	return append([]string(nil), rewrittenColumns[table]...)
}

// TableState is one table's contents, as digests.
type TableState struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Rows    int64    `json:"rows"`
	// Digest covers every column.
	Digest string `json:"digest"`
	// StableColumns are Columns without the ones the migration rewrites, and
	// StableDigest covers just them.
	StableColumns []string `json:"stable_columns"`
	StableDigest  string   `json:"stable_digest"`
	// Hash is dolt_hashof_table.
	Hash string `json:"hash"`
}

// DatabaseState is a database's identity and contents at one moment.
type DatabaseState struct {
	Name string `json:"name"`
	// DBHash is dolt_hashof_db(): the whole database, working set included.
	DBHash string `json:"db_hash"`
	// Head is HEAD's commit hash, which covers its whole ancestry.
	Head   string             `json:"head"`
	Schema schema.UpgradePlan `json:"schema"`
	Tables []TableState       `json:"tables"`
	// Remotes and Backups are the repository's registrations (name=url).
	// They are repository config, not data: a backup does not carry them,
	// so a rollback puts them back from here.
	Remotes []string  `json:"remotes,omitempty"`
	Backups []string  `json:"backups,omitempty"`
	TakenAt time.Time `json:"taken_at"`
}

// Table returns the named table's state.
func (s DatabaseState) Table(name string) (TableState, bool) {
	for _, t := range s.Tables {
		if t.Name == name {
			return t, true
		}
	}
	return TableState{}, false
}

// Identity reads the database's name and dolt_hashof_db().
func Identity(ctx context.Context, conn *sql.Conn) (name, dbHash string, err error) {
	var nameNull sql.NullString
	if err := conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&nameNull); err != nil {
		return "", "", fmt.Errorf("reading the database name: %w", err)
	}
	if !nameNull.Valid || nameNull.String == "" {
		return "", "", fmt.Errorf("the connection has no database selected")
	}
	if err := conn.QueryRowContext(ctx, "SELECT DOLT_HASHOF_DB()").Scan(&dbHash); err != nil {
		return "", "", fmt.Errorf("reading dolt_hashof_db() of %s: %w", nameNull.String, err)
	}
	return nameNull.String, dbHash, nil
}

// Capture reads the state of the database conn has selected. It issues reads
// only.
func Capture(ctx context.Context, conn *sql.Conn) (DatabaseState, error) {
	var st DatabaseState
	var err error
	if st.Name, st.DBHash, err = Identity(ctx, conn); err != nil {
		return st, err
	}
	st.TakenAt = time.Now().UTC()
	if err := conn.QueryRowContext(ctx, "SELECT DOLT_HASHOF('HEAD')").Scan(&st.Head); err != nil {
		return st, fmt.Errorf("reading HEAD of %s: %w", st.Name, err)
	}
	if st.Remotes, err = registrations(ctx, conn, "dolt_remotes"); err != nil {
		return st, err
	}
	if st.Backups, err = registrations(ctx, conn, "dolt_backups"); err != nil {
		return st, err
	}
	if st.Schema, err = schema.PlanUpgrade(ctx, conn); err != nil {
		return st, fmt.Errorf("reading the schema cursors of %s: %w", st.Name, err)
	}
	tables, err := ListTables(ctx, conn)
	if err != nil {
		return st, err
	}
	for _, name := range tables {
		ts, err := CaptureTable(ctx, conn, name)
		if err != nil {
			return st, err
		}
		st.Tables = append(st.Tables, ts)
	}
	return st, nil
}

// registrations lists a dolt_remotes or dolt_backups table as name=url.
func registrations(ctx context.Context, conn *sql.Conn, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, "SELECT name, url FROM "+table+" ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name, u string
		if err := rows.Scan(&name, &u); err != nil {
			return nil, err
		}
		out = append(out, name+"="+u)
	}
	return out, rows.Err()
}

// ListTables lists the base tables of the selected database, sorted. The
// dolt-ignored tables are included: they live in the working set, and a
// backup carries them.
func ListTables(ctx context.Context, conn *sql.Conn) ([]string, error) {
	rows, err := conn.QueryContext(ctx, "SHOW FULL TABLES")
	if err != nil {
		return nil, fmt.Errorf("listing tables: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			return nil, fmt.Errorf("listing tables: %w", err)
		}
		if strings.EqualFold(kind, "BASE TABLE") {
			names = append(names, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing tables: %w", err)
	}
	sort.Strings(names)
	return names, nil
}

// Columns lists a table's columns in table order.
func Columns(ctx context.Context, conn *sql.Conn, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx,
		"SELECT column_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? ORDER BY ordinal_position",
		table)
	if err != nil {
		return nil, fmt.Errorf("listing the columns of %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("listing the columns of %s: %w", table, err)
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

// CaptureTable reads one table's columns, row count, digests and hash.
func CaptureTable(ctx context.Context, conn *sql.Conn, table string) (TableState, error) {
	ts := TableState{Name: table}
	var err error
	if ts.Columns, err = Columns(ctx, conn, table); err != nil {
		return ts, err
	}
	ts.StableColumns = stableColumns(table, ts.Columns)
	if ts.Rows, ts.Digest, err = Digest(ctx, conn, table, ts.Columns); err != nil {
		return ts, err
	}
	if len(ts.StableColumns) == len(ts.Columns) {
		ts.StableDigest = ts.Digest
	} else if _, ts.StableDigest, err = Digest(ctx, conn, table, ts.StableColumns); err != nil {
		return ts, err
	}
	if err := conn.QueryRowContext(ctx, "SELECT DOLT_HASHOF_TABLE(?)", table).Scan(&ts.Hash); err != nil {
		return ts, fmt.Errorf("reading dolt_hashof_table(%s): %w", table, err)
	}
	return ts, nil
}

func stableColumns(table string, cols []string) []string {
	skip := map[string]bool{}
	for _, c := range rewrittenColumns[table] {
		skip[c] = true
	}
	var out []string
	for _, c := range cols {
		if !skip[c] {
			out = append(out, c)
		}
	}
	return out
}

// Digest is the B-13 whole-row digest of table over cols: the row count and
// bit_xor over conv(substr(md5(json_object(<col>, `col`, ...)), 1, 15), 16, 10),
// which does not depend on row order.
func Digest(ctx context.Context, conn *sql.Conn, table string, cols []string) (int64, string, error) {
	if len(cols) == 0 {
		return 0, "", fmt.Errorf("digest of %s: no columns", table)
	}
	pairs := make([]string, 0, len(cols))
	for _, c := range cols {
		pairs = append(pairs, fmt.Sprintf("'%s', %s", strings.ReplaceAll(c, "'", "''"), quoteIdent(c)))
	}
	q := fmt.Sprintf("SELECT COUNT(*), CAST(IFNULL(BIT_XOR(CONV(SUBSTR(MD5(JSON_OBJECT(%s)), 1, 15), 16, 10)), 0) AS CHAR) FROM %s",
		strings.Join(pairs, ", "), quoteIdent(table))
	var n int64
	var digest string
	if err := conn.QueryRowContext(ctx, q).Scan(&n, &digest); err != nil {
		return 0, "", fmt.Errorf("digest of %s: %w", table, err)
	}
	return n, digest, nil
}

func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// CompareIdentical lists every way got differs from want, for a state that
// must reproduce want exactly (a restored backup, a rollback).
func CompareIdentical(want, got DatabaseState) []string {
	var diffs []string
	if want.DBHash != got.DBHash {
		diffs = append(diffs, fmt.Sprintf("dolt_hashof_db %s, want %s", got.DBHash, want.DBHash))
	}
	if want.Head != got.Head {
		diffs = append(diffs, fmt.Sprintf("HEAD %s, want %s", got.Head, want.Head))
	}
	gotTables := map[string]TableState{}
	for _, t := range got.Tables {
		gotTables[t.Name] = t
	}
	for _, w := range want.Tables {
		g, ok := gotTables[w.Name]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("table %s missing", w.Name))
			continue
		}
		delete(gotTables, w.Name)
		if g.Rows != w.Rows || g.Digest != w.Digest {
			diffs = append(diffs, fmt.Sprintf("table %s: rows %d digest %s, want rows %d digest %s", w.Name, g.Rows, g.Digest, w.Rows, w.Digest))
		}
		if g.Hash != w.Hash {
			diffs = append(diffs, fmt.Sprintf("table %s: dolt_hashof_table %s, want %s", w.Name, g.Hash, w.Hash))
		}
	}
	extra := make([]string, 0, len(gotTables))
	for name := range gotTables {
		extra = append(extra, name)
	}
	sort.Strings(extra)
	for _, name := range extra {
		diffs = append(diffs, fmt.Sprintf("table %s not in the backed-up state", name))
	}
	return diffs
}
