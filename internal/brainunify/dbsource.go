package brainunify

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql" //nolint:revive // registers the "mysql" driver used for both ends of the migration
)

// readOnlySource is a Dolt sql-server connection that exists only to be read
// from. Every statement it can issue passes through guardQuery, so a future
// edit cannot accidentally turn an inventory read into a write against a live
// store.
//
// The caches below memoise information_schema lookups. Those are expensive on
// Dolt — repeated once per duplicated id, a plan turns into a multi-minute
// stall — and they cannot change under a read-only connection.
type readOnlySource struct {
	db        *sql.DB
	schemaMu  sync.Mutex
	colOnce   map[string][]string
	typOnce   map[string]map[string]string
	tableOnce map[string]bool
}

// ReadOnlySource is a Dolt sql-server connection that exists only to be read
// from. It is exported under this name so a caller outside the package (the
// CLI) can hold one, while the read-only enforcement stays internal.
type ReadOnlySource = readOnlySource

// OpenReadOnlySource opens a read-only connection to a Dolt sql-server.
func OpenReadOnlySource(_ context.Context, host string, port int) (*ReadOnlySource, error) {
	return OpenSource(host, port)
}

// errNonQuery is returned when code tries to issue a non-SELECT through the
// source connection. It exists to make the production-safety property
// testable rather than a comment.
type errNonQuery struct{ stmt string }

func (e *errNonQuery) Error() string {
	return fmt.Sprintf("refusing to issue a non-SELECT statement against a production source: %.80q", e.stmt)
}

// query runs a read-only statement. SHOW, DESCRIBE and WITH are permitted
// because the planner needs them; everything else is refused.
func (s *readOnlySource) query(ctx context.Context, stmt string, args ...any) (*sql.Rows, error) {
	if !isReadOnly(stmt) {
		return nil, &errNonQuery{stmt: stmt}
	}
	return s.db.QueryContext(ctx, stmt, args...)
}

// queryRow runs a read-only statement expected to return at most one row.
func (s *readOnlySource) queryRow(ctx context.Context, stmt string, args ...any) *sql.Row {
	// sql.Row cannot report the refusal error, so callers that must not
	// write use query(); this helper is only used for statements that
	// guardQuery already accepted.
	if !isReadOnly(stmt) {
		return nil
	}
	return s.db.QueryRowContext(ctx, stmt, args...)
}

// isReadOnly reports whether a statement may run against production.
func isReadOnly(stmt string) bool {
	fields := strings.Fields(strings.TrimSpace(stmt))
	if len(fields) == 0 {
		return false
	}
	switch strings.ToUpper(fields[0]) {
	case "SELECT", "SHOW", "DESCRIBE", "DESC", "EXPLAIN", "WITH":
		return true
	default:
		return false
	}
}

// OpenSource connects to a Dolt sql-server for reading. The caller supplies
// the coordinates; nothing here discovers or starts a server, so a plan run
// can never be the thing that brings up a writer against production.
func OpenSource(host string, port int) (*readOnlySource, error) {
	if host == "" {
		host = "127.0.0.1"
	}
	if port == 0 {
		port = 3307
	}
	dsn := fmt.Sprintf("root@tcp(%s:%d)/?parseTime=false&multiStatements=false", host, port)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening source dolt server %s:%d: %w", host, port, err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging source dolt server %s:%d: %w", host, port, err)
	}
	return &readOnlySource{db: db}, nil
}

// PingDatabase confirms a named database exists and answers a read. It is how
// the verifier checks it was pointed at the right server before it starts
// comparing.
func (s *readOnlySource) PingDatabase(ctx context.Context, database string) error {
	if database == "" {
		return nil
	}
	rows, err := s.query(ctx, fmt.Sprintf("select count(*) from `%s`.`issues`", database))
	if err != nil {
		return fmt.Errorf("database %s is not readable on this server: %w", database, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return fmt.Errorf("database %s returned no rows", database)
	}
	var n int64
	if err := rows.Scan(&n); err != nil {
		return err
	}
	return nil
}

// Close releases the source connection.
func (s *readOnlySource) Close() error { return s.db.Close() }

// systemDatabases are never stores.
var systemDatabases = map[string]bool{
	"information_schema": true,
	"mysql":              true,
}

// Databases lists the databases on the server, excluding the MySQL system
// databases, in sorted order.
func (s *readOnlySource) Databases(ctx context.Context) ([]string, error) {
	rows, err := s.query(ctx, "show databases")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		name = strings.TrimSpace(name)
		if name == "" || systemDatabases[name] {
			continue
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// BaseTables lists a database's base tables (views excluded: ready_issues and
// blocked_issues are derived, and copying them would double-count). Dolt
// system tables live in the dolt_ schema and so do not appear here.
func (s *readOnlySource) BaseTables(ctx context.Context, database string) ([]string, error) {
	stmt := `select table_name from information_schema.tables
	        where table_schema = ? and table_type = 'BASE TABLE'
	        order by table_name`
	rows, err := s.query(ctx, stmt, database)
	if err != nil {
		return nil, fmt.Errorf("listing tables of %s: %w", database, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// Columns lists a table's columns in ordinal order, memoised per table.
func (s *readOnlySource) Columns(ctx context.Context, database, table string) ([]string, error) {
	key := database + "." + table
	s.schemaMu.Lock()
	if cached, ok := s.colOnce[key]; ok {
		s.schemaMu.Unlock()
		return cached, nil
	}
	s.schemaMu.Unlock()

	stmt := `select column_name from information_schema.columns
	        where table_schema = ? and table_name = ?
	        order by ordinal_position`
	rows, err := s.query(ctx, stmt, database, table)
	if err != nil {
		return nil, fmt.Errorf("listing columns of %s.%s: %w", database, table, err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.schemaMu.Lock()
	if s.colOnce == nil {
		s.colOnce = map[string][]string{}
	}
	s.colOnce[key] = out
	s.schemaMu.Unlock()
	return out, nil
}

// Fingerprint computes an order-independent content digest of a table, or of
// the subset selected by where (which the verifier uses to scope a table's
// rows to one namespace inside the unified database).
//
// The digest is count + total length + XOR of per-row CRC32. Each field is
// tagged so that a NULL and an empty string do not fingerprint alike, and the
// XOR makes the result independent of row order, so a unified database that
// received the same rows in a different order still matches.
func (s *readOnlySource) Fingerprint(ctx context.Context, database, table, where string) (Fingerprint, error) {
	cols, err := s.Columns(ctx, database, table)
	if err != nil {
		return Fingerprint{}, err
	}
	if len(cols) == 0 {
		return Fingerprint{}, nil
	}
	fields, err := s.fingerprintFields(ctx, database, table, cols)
	if err != nil {
		return Fingerprint{}, err
	}
	inner := fmt.Sprintf("select concat_ws('\x1f', %s) as rowtext from `%s`.`%s`", strings.Join(fields, ", "), database, table)
	if strings.TrimSpace(where) != "" {
		inner += " where " + where
	}
	stmt := fmt.Sprintf(
		"select count(*) as n, ifnull(sum(length(rowtext)),0) as b, ifnull(bit_xor(crc32(rowtext)),0) as h from (%s) t",
		inner)
	row := s.queryRow(ctx, stmt)
	if row == nil {
		return Fingerprint{}, fmt.Errorf("refusing non-SELECT fingerprint for %s.%s", database, table)
	}
	// Dolt returns SUM() and BIT_XOR() as doubles, so the two aggregate
	// columns are scanned as float64 and narrowed. Every value involved is
	// far below 2^53, where a double is exact.
	var rows int64
	var bytes, hash float64
	if err := row.Scan(&rows, &bytes, &hash); err != nil {
		return Fingerprint{}, fmt.Errorf("fingerprinting %s.%s: %w", database, table, err)
	}
	fp := Fingerprint{Rows: rows, Bytes: int64(bytes), Hash: int64(hash)}
	if err := fp.checkInvariant(database, table); err != nil {
		return Fingerprint{}, err
	}
	return fp, nil
}

// FingerprintByGroup computes per-group fingerprints of a table in a single
// query, grouping by a leading substring of scopeCol (the id prefix) or, when
// scopeCol is empty, by the whole value of groupCol.
//
// One grouped query per source replaces one query per source per namespace,
// which is the difference between a verification that finishes and one that
// runs for hours: on the live federation there are 55 sources and roughly 60
// namespaces, and the cross product is the wrong shape for this job.
//
// exclude removes rows whose scopeCol value is listed, which is how the
// collision losers are kept out of the expected side.
func (s *readOnlySource) FingerprintByGroup(ctx context.Context, database, table, scopeCol, groupCol string, exclude []string) (map[string]Fingerprint, error) {
	cols, err := s.Columns(ctx, database, table)
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return map[string]Fingerprint{}, nil
	}
	fields, err := s.fingerprintFields(ctx, database, table, cols)
	if err != nil {
		return nil, err
	}
	rowText := fmt.Sprintf("concat_ws('\x1f', %s)", strings.Join(fields, ", "))

	groupExpr := groupCol
	if groupExpr == "" {
		// NULL group keys (interactions.issue_id is nullable) are folded to
		// a sentinel on both sides so the two sides agree on the bucket.
		groupExpr = fmt.Sprintf("ifnull(substring_index(`%s`, '-', 1), '(no-namespace)')", scopeCol)
	}

	stmt := fmt.Sprintf(
		"select ifnull(g, '(null)') as grp, count(*) as n, ifnull(sum(length(rowtext)),0) as b, ifnull(bit_xor(crc32(rowtext)),0) as h "+
			"from (select %s as g, %s as rowtext from `%s`.`%s`%s) t group by g",
		groupExpr, rowText, database, table, exclusionClause(scopeCol, exclude))
	rows, err := s.query(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("fingerprinting %s.%s by group: %w", database, table, err)
	}
	defer rows.Close()
	out := map[string]Fingerprint{}
	for rows.Next() {
		// A Scan consumes the whole remaining row, so the group key and the
		// three aggregates are scanned together.
		var g string
		var n int64
		var b, h float64
		if err := rows.Scan(&g, &n, &b, &h); err != nil {
			return nil, err
		}
		fp := Fingerprint{Rows: n, Bytes: int64(b), Hash: int64(h)}
		if err := fp.checkInvariant(database, table); err != nil {
			return nil, err
		}
		out[g] = fp
	}
	return out, rows.Err()
}

// exclusionClause renders a WHERE fragment dropping the listed keys, or the
// empty string when there is nothing to exclude.
func exclusionClause(scopeCol string, exclude []string) string {
	if len(exclude) == 0 || scopeCol == "" {
		return ""
	}
	quoted := make([]string, 0, len(exclude))
	for _, e := range exclude {
		quoted = append(quoted, quoteLiteral(e))
	}
	return fmt.Sprintf(" where `%s` not in (%s)", scopeCol, strings.Join(quoted, ","))
}

// issueIDRow is the identity of a bead as far as collision analysis needs.
type issueIDRow struct {
	ID          string
	UpdatedAt   string
	CreatedAt   string
	ContentHash string
}

// IssueIdentities reads (id, created_at, updated_at, content_hash) for every
// bead in a database. Reading only these columns keeps the pass cheap enough
// to run against the live server.
func (s *readOnlySource) IssueIdentities(ctx context.Context, database string) ([]issueIDRow, error) {
	stmt := fmt.Sprintf(
		"select `id`, ifnull(cast(`updated_at` as char),''), ifnull(cast(`created_at` as char),''), ifnull(`content_hash`,'') from `%s`.`issues`",
		database)
	rows, err := s.query(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("reading issue identities from %s: %w", database, err)
	}
	defer rows.Close()
	var out []issueIDRow
	for rows.Next() {
		var r issueIDRow
		if err := rows.Scan(&r.ID, &r.UpdatedAt, &r.CreatedAt, &r.ContentHash); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// IssuePrefixes counts a database's ids per namespace prefix.
func (s *readOnlySource) IssuePrefixes(ctx context.Context, database string) (map[string]int64, error) {
	stmt := fmt.Sprintf(
		"select substring_index(`id`,'-',1) as p, count(*) as n from `%s`.`issues` group by p",
		database)
	rows, err := s.query(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("counting prefixes in %s: %w", database, err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var p string
		var n int64
		if err := rows.Scan(&p, &n); err != nil {
			return nil, err
		}
		out[p] = n
	}
	return out, rows.Err()
}

// CountRows returns a table's row count.
func (s *readOnlySource) CountRows(ctx context.Context, database, table string) (int64, error) {
	stmt := fmt.Sprintf("select count(*) from `%s`.`%s`", database, table)
	row := s.queryRow(ctx, stmt)
	if row == nil {
		return 0, fmt.Errorf("refusing non-SELECT count for %s.%s", database, table)
	}
	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("counting %s.%s: %w", database, table, err)
	}
	return n, nil
}

// ColumnTypes maps each column of a table to its SQL type, memoised. It is
// used to recognise json columns, whose values cannot be empty: a source
// store that wrote an empty string into a json column cannot be inserted
// verbatim.
func (s *readOnlySource) ColumnTypes(ctx context.Context, database, table string) (map[string]string, error) {
	key := database + "." + table
	s.schemaMu.Lock()
	if cached, ok := s.typOnce[key]; ok {
		s.schemaMu.Unlock()
		return cached, nil
	}
	s.schemaMu.Unlock()

	stmt := `select column_name, data_type from information_schema.columns
	        where table_schema = ? and table_name = ?`
	rows, err := s.query(ctx, stmt, database, table)
	if err != nil {
		return nil, fmt.Errorf("reading column types of %s.%s: %w", database, table, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return nil, err
		}
		out[name] = strings.ToLower(typ)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.schemaMu.Lock()
	if s.typOnce == nil {
		s.typOnce = map[string]map[string]string{}
	}
	s.typOnce[key] = out
	s.schemaMu.Unlock()
	return out, nil
}

// JSONColumns lists the json-typed columns of a table.
func (s *readOnlySource) JSONColumns(ctx context.Context, database, table string) ([]string, error) {
	types, err := s.ColumnTypes(ctx, database, table)
	if err != nil {
		return nil, err
	}
	var out []string
	for col, typ := range types {
		if typ == "json" {
			out = append(out, col)
		}
	}
	sort.Strings(out)
	return out, nil
}

// HasTable reports whether a database has a base table, memoised per table.
func (s *readOnlySource) HasTable(ctx context.Context, database, table string) (bool, error) {
	key := database + "." + table
	s.schemaMu.Lock()
	if cached, ok := s.tableOnce[key]; ok {
		s.schemaMu.Unlock()
		return cached, nil
	}
	s.schemaMu.Unlock()

	stmt := `select count(*) from information_schema.tables
	        where table_schema = ? and table_name = ? and table_type = 'BASE TABLE'`
	row := s.queryRow(ctx, stmt, database, table)
	if row == nil {
		return false, fmt.Errorf("refusing non-SELECT existence check for %s.%s", database, table)
	}
	var n int64
	if err := row.Scan(&n); err != nil {
		return false, err
	}
	got := n > 0
	s.schemaMu.Lock()
	if s.tableOnce == nil {
		s.tableOnce = map[string]bool{}
	}
	s.tableOnce[key] = got
	s.schemaMu.Unlock()
	return got, nil
}

// CreateTableDDL returns the CREATE TABLE statement for a table, used to give
// the unified database exactly the schema its sources already have instead of
// a hand-copied approximation that would drift.
func (s *readOnlySource) CreateTableDDL(ctx context.Context, database, table string) (string, error) {
	row := s.queryRow(ctx, fmt.Sprintf("show create table `%s`.`%s`", database, table))
	if row == nil {
		return "", fmt.Errorf("refusing non-SELECT DDL read for %s.%s", database, table)
	}
	var name, ddl string
	if err := row.Scan(&name, &ddl); err != nil {
		return "", fmt.Errorf("reading DDL for %s.%s: %w", database, table, err)
	}
	return ddl, nil
}

// MetadataValue reads one key from a database's metadata table.
func (s *readOnlySource) MetadataValue(ctx context.Context, database, key string) (string, error) {
	ok, err := s.HasTable(ctx, database, "metadata")
	if err != nil || !ok {
		return "", err
	}
	row := s.queryRow(ctx, fmt.Sprintf("select `value` from `%s`.`metadata` where `key` = ?", database), key)
	if row == nil {
		return "", fmt.Errorf("refusing non-SELECT metadata read for %s", database)
	}
	var v string
	if err := row.Scan(&v); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return v, nil
}

// CopyRows streams a source table's rows to fn in batches of size batch. Only
// the columns the unified schema actually has are selected, so a source that
// gained a column the template lacks is reported by the caller rather than
// failing mid-insert with an opaque SQL error.

// IssueRow is one bead read in full, as a column/value map with every value
// rendered as a string. Reading the whole row is what lets a duplicated id be
// compared field by field: a differing content_hash column says the two copies
// were touched at different times, while a differing title says the data
// itself disagrees, and only the second kind is worth blocking a migration on.
func (s *readOnlySource) IssueRow(ctx context.Context, database, id string) (map[string]string, error) {
	cols, err := s.Columns(ctx, database, "issues")
	if err != nil {
		return nil, err
	}
	quoted := make([]string, 0, len(cols))
	for _, c := range cols {
		quoted = append(quoted, "`"+c+"`")
	}
	stmt := fmt.Sprintf("select %s from `%s`.`issues` where `id` = ?", strings.Join(quoted, ", "), database)
	rows, err := s.query(ctx, stmt, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, fmt.Errorf("%s.%s has no row with id %q", database, "issues", id)
	}
	values := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(cols))
	for i, col := range cols {
		switch v := values[i].(type) {
		case nil:
			out[col] = ""
		case []byte:
			out[col] = string(v)
		default:
			out[col] = fmt.Sprint(v)
		}
	}
	return out, nil
}

// CopyRowsResult is what a copy pass copied, digested from the rows themselves
// rather than from a second read of the source.
type CopyRowsResult struct {
	Fingerprint
	// Yielded is the number of rows handed to the caller.
	Yielded int64
	// Groups are the per-namespace digests of the rows yielded, keyed by the
	// same prefix rule the server would use, so a verification can prove not
	// only that every row arrived but that it arrived in the namespace it
	// belongs to.
	Groups map[string]Fingerprint
}

// CopyRows streams a source table's rows to fn in batches bounded by both row
// count and total size, because a single events row can carry a whole session
// transcript and an unbounded batch would exceed the server's packet limit.
// the columns the caller names are selected, so a source that gained a column
// the unified schema lacks is reported by the caller rather than failing
// mid-insert with an opaque SQL error.
//
// It deliberately computes no digest. An earlier version reproduced the
// server's column encoding in Go, and the two implementations drifted by a few
// bytes per row — indistinguishable from real corruption, and worth less than
// the measurement cost. There is now exactly one encoding, the SQL in
// fingerprintFields, and both sides of every comparison run it.
func (s *readOnlySource) CopyRows(ctx context.Context, database, table string, cols []string, jsonCols []string, fn func(rows [][]any) error) error {
	if len(cols) == 0 {
		return nil
	}
	quoted := make([]string, 0, len(cols))
	for _, c := range cols {
		quoted = append(quoted, "`"+c+"`")
	}
	stmt := fmt.Sprintf("select %s from `%s`.`%s`", strings.Join(quoted, ", "), database, table)
	rows, err := s.query(ctx, stmt)
	if err != nil {
		return fmt.Errorf("reading %s.%s: %w", database, table, err)
	}
	defer rows.Close()

	const (
		batchRows  = 200
		batchBytes = 8 << 20
	)
	batch := make([][]any, 0, batchRows)
	size := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := fn(batch); err != nil {
			return err
		}
		batch = batch[:0]
		size = 0
		return nil
	}
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return fmt.Errorf("scanning %s.%s: %w", database, table, err)
		}
		batch = append(batch, normalizeRow(values, jsonIdx(cols, jsonCols)))
		for _, v := range values {
			if b, ok := v.([]byte); ok {
				size += len(b)
			} else if s, ok := v.(string); ok {
				size += len(s)
			}
		}
		if len(batch) == batchRows || size >= batchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return flush()
}

// jsonIdx maps each column position to whether it is json-typed, so the row
// normaliser can coerce without re-deriving it per row.
func jsonIdx(cols, jsonCols []string) map[int]bool {
	if len(jsonCols) == 0 {
		return nil
	}
	set := make(map[string]bool, len(jsonCols))
	for _, c := range jsonCols {
		set[c] = true
	}
	out := map[int]bool{}
	for i, c := range cols {
		if set[c] {
			out[i] = true
		}
	}
	return out
}

// normalizeRow converts driver values into types the target accepts. The
// driver hands back []byte for every text-typed column over the text
// protocol, and a []byte bound to a datetime parameter is rejected; strings
// are accepted by every column type this migration copies.
//
// json-typed columns get one correction: a store that wrote an empty string
// into a json column holds a value no json column can accept, so an empty or
// whitespace-only value becomes {}. That is the smallest change that makes
// the row insertable, and the migration's rule is that no value is invented —
// an absent json document is exactly what {} states.
func normalizeRow(values []any, jsonIdx map[int]bool) []any {
	out := make([]any, len(values))
	for i, v := range values {
		switch t := v.(type) {
		case []byte:
			if jsonIdx[i] && strings.TrimSpace(string(t)) == "" {
				out[i] = "{}"
				continue
			}
			out[i] = string(t)
		case time.Time:
			out[i] = t.UTC().Format("2006-01-02 15:04:05")
		default:
			if jsonIdx[i] {
				if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
					out[i] = "{}"
					continue
				}
			}
			out[i] = v
		}
	}
	return out
}

// fingerprintFields renders every column as a digest-safe literal.
//
// \x01 marks NULL and \x02 marks a present value, so a migration that turns
// NULL into an empty string changes the fingerprint — and that difference is
// exactly what the verification exists to catch.
//
// Values are wrapped in hex() because some columns hold bytes that are not
// valid text (federation_peers carries a binary token). Concatenating those
// into a string makes the server reject the query outright, which would mean
// the one table that cannot be compared is the one nobody notices. hex() is
// reversible, so it stays byte-exact. lower() pins the case so the same
// digest can be recomputed client-side from the rows a copy is sending.
//
// json columns are the exception: Dolt rejects hex() on them, and a json
// document is always valid text, so those are cast to char. The choice is made
// from the column's declared type, and both sides of every comparison build
// their fields the same way.
func (s *readOnlySource) fingerprintFields(ctx context.Context, database, table string, cols []string) ([]string, error) {
	types, err := s.ColumnTypes(ctx, database, table)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		expr := fmt.Sprintf("lower(hex(`%s`))", c)
		if types[c] == "json" {
			expr = fmt.Sprintf("cast(`%s` as char)", c)
		}
		out = append(out, fmt.Sprintf("ifnull(concat('\x02', %s), '\x01')", expr))
	}
	return out, nil
}

// checkInvariant refuses a fingerprint that reports rows but no content.
//
// Every row contributes at least one character per column — the NULL marker is
// \x01 and a present value is \x02 followed by its encoding — so a table with
// rows always has bytes. A zero here means the aggregate columns were not read,
// not that the table is empty, and letting that through would turn a broken
// measurement into a confident "no difference". This is the check that would
// have caught the scan bug that silently discarded the bytes and hash of every
// database-state table.
func (f Fingerprint) checkInvariant(database, table string) error {
	if f.Rows > 0 && f.Bytes == 0 {
		return fmt.Errorf("fingerprint of %s.%s reports %d rows but zero content bytes: the aggregate columns were not read", database, table, f.Rows)
	}
	return nil
}
