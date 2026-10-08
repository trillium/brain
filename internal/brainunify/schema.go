package brainunify

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Scope classifies how a table maps into the unified database. The
// classification is derived from the table's own columns rather than a
// hardcoded list, so a table added by a later schema migration is classified
// the same way as today's without editing this file.
type Scope int

const (
	// ScopeIssues is the issues table itself. Rows are addressed by id
	// prefix, which is the namespace.
	ScopeIssues Scope = iota
	// ScopeIssueChild is a table with an issue_id column. Its rows fold in
	// unchanged: the id it references already carries the namespace.
	ScopeIssueChild
	// ScopeDatabaseState is a table whose rows describe the database itself
	// (config, metadata, counters, schema bookkeeping). Several stores
	// cannot share these key spaces, so they are re-keyed by store. This is
	// the one class of data that does not fold unchanged, and it is
	// re-keyed rather than dropped so no value is lost.
	ScopeDatabaseState
)

// String renders the scope for reports.
func (s Scope) String() string {
	switch s {
	case ScopeIssues:
		return "issues"
	case ScopeIssueChild:
		return "issue-child"
	case ScopeDatabaseState:
		return "database-state"
	default:
		return "unknown"
	}
}

// NamespacedPrefix is the prefix of the re-keyed copies of database-state
// tables. brain_unified_config holds every store's config rows with a store
// column, so a key like "issue_prefix" can exist once per store instead of
// colliding once globally.
const NamespacedPrefix = "brain_unified_"

// brainTables are the tables the builder adds. They are the whole of the
// "where did this row come from" record that separate databases used to carry
// implicitly through their physical location.
var brainTables = []string{
	"brain_stores",
	"brain_store_aliases",
	"brain_store_prefixes",
	"brain_unify_collisions",
	"brain_unify_import_log",
	"brain_unify_source_commits",
	"brain_unify_source_fingerprints",
}

// BrainTableDDL is the schema of the provenance tables. Each carries a stated
// reason: these tables exist so that the namespace mapping is queryable
// without re-deriving it from the source databases after they are gone.
var BrainTableDDL = map[string]string{
	"brain_stores": `CREATE TABLE brain_stores (
  store varchar(128) NOT NULL,
  registered tinyint(1) NOT NULL DEFAULT 0,
  source_database varchar(255) NOT NULL,
  beads_dir varchar(1024) NOT NULL DEFAULT '',
  project_id varchar(64) NOT NULL DEFAULT '',
  schema_version varchar(32) NOT NULL DEFAULT '',
  template_source tinyint(1) NOT NULL DEFAULT 0,
  imported_at datetime NOT NULL,
  PRIMARY KEY (store)
)`,
	// brain_store_aliases maps the name a store wrapper pins (BD_NAME) to the
	// store it is recorded under in brain_stores, for the wrappers whose name
	// differs from the store's: decide and decisions are two wrappers of one
	// store, and an unregistered database is a store named "db:<name>". The
	// runtime resolves BD_NAME through it (issueops.canonicalStoreName).
	"brain_store_aliases": `CREATE TABLE brain_store_aliases (
  alias varchar(128) NOT NULL,
  store varchar(128) NOT NULL,
  PRIMARY KEY (alias)
)`,
	"brain_store_prefixes": `CREATE TABLE brain_store_prefixes (
  prefix varchar(255) NOT NULL,
  store varchar(128) NOT NULL,
  owner_reason varchar(64) NOT NULL DEFAULT '',
  declared_by text NOT NULL,
  observed_by text NOT NULL,
  bead_count bigint NOT NULL DEFAULT 0,
  ambiguous tinyint(1) NOT NULL DEFAULT 0,
  PRIMARY KEY (prefix),
  KEY idx_brain_prefix_store (store)
)`,
	// brain_unify_collisions has one row per duplicated id. resolution says what
	// became of it. 'merged-identical': the copies were identical, winner and
	// losers name the stores, one bead remains, and losing_row holds the skipped
	// copy in full. 'conflict-bead': the copies differ, nothing was skipped,
	// winner is empty, copy_ids maps each copy's store to the id it was minted
	// under (the original id is the conflict bead), and copy_hashes maps each
	// store to a digest of that copy's rows as they should be in the merged
	// database.
	"brain_unify_collisions": `CREATE TABLE brain_unify_collisions (
  id varchar(255) NOT NULL,
  prefix varchar(255) NOT NULL,
  owner varchar(128) NOT NULL,
  winner varchar(128) NOT NULL,
  losers text NOT NULL,
  reason varchar(64) NOT NULL,
  divergent tinyint(1) NOT NULL DEFAULT 0,
  winner_hash varchar(64) NOT NULL DEFAULT '',
  loser_hashes text NOT NULL,
  losing_row longtext NOT NULL,
  resolution varchar(24) NOT NULL DEFAULT 'merged-identical',
  copy_ids text NOT NULL,
  copy_hashes text NOT NULL,
  PRIMARY KEY (id),
  KEY idx_brain_collision_prefix (prefix)
)`,
	"brain_unify_import_log": `CREATE TABLE brain_unify_import_log (
  store varchar(128) NOT NULL,
  table_name varchar(255) NOT NULL,
  source_rows bigint NOT NULL DEFAULT 0,
  imported_rows bigint NOT NULL DEFAULT 0,
  skipped_rows bigint NOT NULL DEFAULT 0,
  source_hash bigint NOT NULL DEFAULT 0,
  imported_hash bigint NOT NULL DEFAULT 0,
  source_bytes bigint NOT NULL DEFAULT 0,
  imported_bytes bigint NOT NULL DEFAULT 0,
  verified tinyint(1) NOT NULL DEFAULT 0,
  note text NOT NULL,
  PRIMARY KEY (store, table_name)
)`,
	// brain_unify_source_commits records, per source, the Dolt commit the
	// build read at the moment it read it. A replay reads its starting point
	// here: everything a source wrote after this commit is what the replay
	// has to carry into the unified database. Without it a replay would have
	// to re-diff the whole source — and a merged database built before this
	// table exists (the first builds) is simply not replayable: rebuild it.
	"brain_unify_source_commits": `CREATE TABLE brain_unify_source_commits (
  store varchar(128) NOT NULL,
  source_database varchar(255) NOT NULL,
  commit_hash varchar(64) NOT NULL,
  recorded_at datetime NOT NULL,
  PRIMARY KEY (store)
)`,
	// brain_unify_source_fingerprints records what the build actually read
	// from each source, per store, per table, per namespace. Verification
	// compares the unified database against this rather than against a live
	// re-read: brain's stores are written continuously (the lifespan ledger
	// alone takes hundreds of rows an hour), so re-reading production after
	// the build measures how much the federation moved, not whether the
	// migration was correct. Reading this table also makes a verification
	// reproducible — it compares a snapshot to a snapshot.
	"brain_unify_source_fingerprints": `CREATE TABLE brain_unify_source_fingerprints (
  store varchar(128) NOT NULL,
  table_name varchar(255) NOT NULL,
  group_name varchar(255) NOT NULL,
  row_count bigint NOT NULL DEFAULT 0,
  byte_count bigint NOT NULL DEFAULT 0,
  hash_value bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (store, table_name, group_name)
)`,
}

// TablePlan is what the builder needs to know about one table of one source.
type TablePlan struct {
	Table string
	Scope Scope
	// Target is the table rows are written into: the same name for issues
	// and issue children, NamespacedPrefix+name for database state.
	Target string
	// Columns are the columns copied, in the target's column order.
	Columns []string
	// PrimaryKey are the source table's primary key columns, used to build
	// the re-keyed database-state table and the load order.
	PrimaryKey []string
	// ScopeColumn is the column a row is filtered on when verifying a
	// namespace's slice of a shared table. Empty for database state.
	ScopeColumn string
}

// ScopeColumnName returns the column that addresses a row's namespace: the id
// itself for a table of beads, the referencing id for bead children, and
// nothing for database state, which the unified database addresses by its
// store column.
func (t TablePlan) ScopeColumnName() string {
	if t.Scope == ScopeDatabaseState {
		return ""
	}
	return t.ScopeColumn
}

// issueScopeColumn is the most common bead reference column, named here so the
// verifier and the classifier agree on what "the issue column" means.
const issueScopeColumn = "issue_id"

// NamespacedName returns the re-keyed table name for a database-state table.
func NamespacedName(table string) string { return NamespacedPrefix + table }

// beadScopeColumns are the columns that hold a reference to a bead id. A table
// carrying one is namespace-addressable after unification, because the id it
// references already names the namespace, so the table folds in unchanged.
//
// The set is enumerated rather than guessed because the schema uses more than
// one name: child_counters and wisp_child_counters reference their parent
// through parent_id rather than issue_id. A table with neither is
// database-state.
var beadScopeColumns = []string{"issue_id", "parent_id"}

// beadTables are tables that are themselves collections of beads. Their
// namespace is their own primary key, so they fold in unchanged and are
// addressed by their id. wisps belongs here: it has no parent column at all,
// because a wisp's id is itself a hierarchical bead id.
var beadTables = map[string]string{
	"issues": "id",
	"wisps":  "id",
}

// Classify decides a table's scope from its columns and key, returning the
// scope and the column that addresses a row's namespace (empty for database
// state, which the unified database addresses by its store column).
func Classify(table string, cols, pk []string) (Scope, string) {
	if c, ok := beadTables[table]; ok && contains(cols, c) {
		if table == "issues" {
			return ScopeIssues, c
		}
		return ScopeIssueChild, c
	}
	for _, c := range beadScopeColumns {
		if contains(cols, c) {
			return ScopeIssueChild, c
		}
	}
	return ScopeDatabaseState, ""
}

// PlanTables classifies every source table and resolves where its rows go.
// metaOf returns a table's columns and primary key. Tables that are the
// builder's own are skipped, so a re-run cannot compound them.
func PlanTables(tables []string, metaOf func(string) ([]string, []string, error)) ([]TablePlan, error) {
	var out []TablePlan
	for _, t := range tables {
		if strings.HasPrefix(t, NamespacedPrefix) || contains(brainTables, t) {
			continue
		}
		cols, pk, err := metaOf(t)
		if err != nil {
			return nil, err
		}
		if len(cols) == 0 {
			return nil, fmt.Errorf("table %s has no columns", t)
		}
		scope, scopeCol := Classify(t, cols, pk)
		tp := TablePlan{Table: t, Scope: scope, Target: t, Columns: cols, PrimaryKey: pk, ScopeColumn: scopeCol}
		switch scope {
		case ScopeIssueChild, ScopeIssues:
			if scopeCol == "" {
				return nil, fmt.Errorf("table %s is bead-scoped but has no scope column", t)
			}
		case ScopeDatabaseState:
			tp.Target = NamespacedName(t)
			if len(pk) == 0 {
				return nil, fmt.Errorf("database-state table %s has no primary key, so it cannot be re-keyed by store", t)
			}
		}
		out = append(out, tp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Table < out[j].Table })
	return out, nil
}

// PrimaryKeys reads the primary key columns of a table, in key order.
func (s *readOnlySource) PrimaryKeys(ctx context.Context, database, table string) ([]string, error) {
	stmt := `select column_name from information_schema.key_column_usage
	        where table_schema = ? and table_name = ? and constraint_name = 'PRIMARY'
	        order by ordinal_position`
	rows, err := s.query(ctx, stmt, database, table)
	if err != nil {
		return nil, fmt.Errorf("reading primary key of %s.%s: %w", database, table, err)
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

// ForeignKeys maps a table to the tables it references, used only to order the
// load so parents land before children.
func (s *readOnlySource) ForeignKeys(ctx context.Context, database, table string) ([]string, error) {
	stmt := `select distinct referenced_table_name from information_schema.key_column_usage
	        where table_schema = ? and table_name = ? and referenced_table_name is not null`
	rows, err := s.query(ctx, stmt, database, table)
	if err != nil {
		return nil, fmt.Errorf("reading foreign keys of %s.%s: %w", database, table, err)
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

// LoadOrder topologically sorts tables so that a referenced table is written
// before the table referencing it. Cycles (which the schema forbids but a
// future migration could introduce) fall back to alphabetical order within
// the cycle rather than failing the load.
func LoadOrder(tables []string, refsOf func(string) []string) []string {
	byName := map[string]bool{}
	for _, t := range tables {
		byName[t] = true
	}
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var order []string
	var visit func(string)
	visit = func(name string) {
		switch color[name] {
		case black:
			return
		case gray:
			return // cycle: break it deterministically
		}
		color[name] = gray
		var deps []string
		for _, d := range refsOf(name) {
			if byName[d] {
				deps = append(deps, d)
			}
		}
		sort.Strings(deps)
		for _, d := range deps {
			visit(d)
		}
		color[name] = black
		order = append(order, name)
	}
	names := append([]string(nil), tables...)
	sort.Strings(names)
	for _, n := range names {
		visit(n)
	}
	return order
}

// NamespacedDDL builds the CREATE TABLE for a re-keyed database-state table:
// the source's own column definitions, unchanged, plus a leading store
// column, under a primary key of (store, <source primary key>). The body is
// taken verbatim from the source's DDL so no column type is ever retyped by
// hand — the only edit is dropping the source's own PRIMARY KEY clause,
// because the re-keyed table declares its own.
func NamespacedDDL(targetTable, sourceDDL, storeCol string, pk []string) (string, error) {
	open := strings.Index(sourceDDL, "(")
	last := strings.LastIndex(sourceDDL, ")")
	if open < 0 || last < open {
		return "", fmt.Errorf("cannot parse source DDL: %.80q", sourceDDL)
	}
	body := strings.TrimSuffix(strings.TrimSpace(sourceDDL[open+1:last]), ",")
	body = stripPrimaryKeyClause(body)
	body = stripConstraints(body)
	if body == "" {
		return "", fmt.Errorf("source DDL for %s has no columns", targetTable)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE `%s` (\n  `%s` varchar(128) NOT NULL,\n  %s,\n  PRIMARY KEY (`%s`",
		targetTable, storeCol, body, storeCol)
	for _, col := range pk {
		fmt.Fprintf(&b, ", `%s`", col)
	}
	b.WriteString(")\n)")
	return b.String(), nil
}

// stripConstraints removes foreign keys, checks and secondary indexes from a
// re-keyed table's DDL.
//
// The foreign keys are the reason this is necessary rather than tidy: they
// name constraints globally within a schema, and the unified database also
// creates the source table under its original name, so keeping them produces
// "duplicate foreign key constraint name". The references they expressed were
// to the source's own tables, which now hold every store's rows.
//
// Secondary indexes are dropped for the same reason the re-keyed table exists:
// its rows come from every store, so an index tuned for one store's query
// pattern no longer describes them. These tables are small per-store state
// (config, metadata, counters), not query surfaces.
func stripConstraints(body string) string {
	lines := splitTopLevel(body)
	var kept []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
		upper := strings.ToUpper(trimmed)
		if strings.HasPrefix(upper, "CONSTRAINT ") ||
			strings.HasPrefix(upper, "PRIMARY KEY") ||
			strings.HasPrefix(upper, "KEY ") ||
			strings.HasPrefix(upper, "UNIQUE KEY") ||
			strings.HasPrefix(upper, "UNIQUE INDEX") ||
			strings.HasPrefix(upper, "INDEX ") ||
			strings.HasPrefix(upper, "FULLTEXT ") ||
			strings.HasPrefix(upper, "CHECK ") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, ",\n  ")
}

// stripPrimaryKeyClause removes a PRIMARY KEY definition, tolerating the
// trailing commas the DDL formatter leaves behind.
func stripPrimaryKeyClause(body string) string {
	lines := splitTopLevel(body)
	var kept []string
	for _, line := range lines {
		if _, ok := primaryKeyColumns(line); ok {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, ",\n  ")
}

// primaryKeyColumns reports whether a DDL line is a PRIMARY KEY clause and, if
// so, its comma-separated column list.
func primaryKeyColumns(line string) (string, bool) {
	trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
	if !strings.HasPrefix(strings.ToUpper(trimmed), "PRIMARY KEY") {
		return "", false
	}
	open := strings.Index(trimmed, "(")
	closeIdx := strings.LastIndex(trimmed, ")")
	if open < 0 || closeIdx < open {
		return "", true
	}
	return trimmed[open+1 : closeIdx], true
}

// splitTopLevel splits a DDL body on commas that are not nested inside
// parentheses and not inside a string literal. A column default can legitimately
// contain a comma ("DEFAULT 'x,y'"), and splitting there would corrupt the
// generated DDL.
func splitTopLevel(body string) []string {
	var out []string
	depth, start := 0, 0
	inQuote := byte(0)
	for i := 0; i < len(body); i++ {
		c := body[i]
		if inQuote != 0 {
			switch c {
			case '\\':
				i++ // skip the escaped byte
			case inQuote:
				inQuote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			inQuote = c
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(body[start:i]))
				start = i + 1
			}
		}
	}
	if rest := strings.TrimSpace(body[start:]); rest != "" {
		out = append(out, rest)
	}
	return out
}

// contains reports membership in a small string slice.
func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// escapeLike escapes the LIKE wildcards in a literal prefix. Prefixes are
// operator-controlled, but escaping keeps a store named "a_b" from matching
// "aXb-" ids and silently inflating a fingerprint.
func escapeLike(s string) string {
	return strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(s)
}
