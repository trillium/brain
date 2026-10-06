package issueops

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
)

// This file teaches the config layer the unified database's shape
// (docs/design/brain-single-database.md, "What changes shape" and the
// pre-cutover checklist item "the config/metadata seeding story").
//
// In the unified database the template store's own single-valued tables
// (config, metadata, local_metadata, custom_statuses, custom_types) exist
// only so bd can open the database at all — the build seeds them from the
// template store. Every store's real rows live in the re-keyed
// brain_unified_<table> copies, keyed by an added store column. A wrapper
// re-pointed at the unified database must therefore address its own rows:
// reads and writes of settings route to brain_unified_config filtered by
// the store's namespace, and the same story for metadata, local_metadata,
// and the derived custom-status/type tables.
//
// The namespace comes from the wrapper, not from the database: store
// wrappers export BD_NAME (see internal/utils/id_parser.go for the same
// source for id prefixes). With no BD_NAME the caller cannot single out a
// namespace, so every helper here degrades to the template-seeded
// single-valued tables — the pre-cutover behaviour — rather than guessing.
//
// The database's own identity is never consulted beyond discovering WHICH
// database this connection is attached to, so the settings a wrapper reads
// and writes are its namespace's, not whoever the template happened to be.

// UnifiedScope says whether the connected database is the unified database
// and which namespace a wrapper on it addresses.
type UnifiedScope struct {
	// Unified is true when the connected database carries the re-keyed
	// brain_unified_config table.
	Unified bool
	// Store is the wrapper-pinned namespace (BD_NAME). Empty when no
	// wrapper name is set; then scoped routing cannot happen.
	Store string
}

// Enabled reports whether scoped routing can happen at all: the database is
// unified AND the wrapper named a namespace.
func (sc UnifiedScope) Enabled() bool { return sc.Unified && sc.Store != "" }

// storeNamespace reads the wrapper-pinned namespace. Keep in step with
// change_events.go's store-name resolution: the wrapper exports BD_NAME.
func storeNamespace() string {
	return strings.TrimSpace(envStoreNamespace())
}

// envStoreNamespace is the single read of the wrapper's namespace variable.
// Kept in one place so a future registry-style source (like the stores.yaml
// name the wrapper already mirrors) can replace it without callers caring.
func envStoreNamespace() string { return os.Getenv("BD_NAME") }

// unifiedScopeCache memoises the probing answer per connected database, so
// the two extra information queries run once per database per process. The
// unified shape does not flip inside a process lifetime; UnifyScopeCache is
// a deliberate process-wide assumption, not a per-call read.
var unifiedScopeCache sync.Map // dbname string -> UnifiedScope

// unifyScopeOverrideForTest replaces probeUnifiedScope when a test needs to
// pin the answer (set it, run, then restore). Nil means "use the probe".
var unifyScopeOverrideForTest func(ctx context.Context, q DBTX) UnifiedScope

// probeUnifiedScope discovers the connected database's name and whether it
// carries the re-keyed config table. Any probe error means "not unified":
// degradation to the template-seeded tables is the documented fallback.
func probeUnifiedScope(ctx context.Context, q DBTX) UnifiedScope {
	var dbname string
	if err := q.QueryRowContext(ctx, "select database()").Scan(&dbname); err != nil || dbname == "" {
		return UnifiedScope{}
	}
	if v, ok := unifiedScopeCache.Load(dbname); ok {
		return v.(UnifiedScope)
	}
	var unified int
	_ = q.QueryRowContext(ctx,
		"select count(*) from information_schema.tables where table_schema = ? and table_name = 'brain_unified_config'",
		dbname).Scan(&unified)
	sc := UnifiedScope{Unified: unified > 0, Store: storeNamespace()}
	unifiedScopeCache.Store(dbname, sc)
	return sc
}

// UnifiedScopeForTx answers the scope for the connected database behind a
// transaction or connection handle (both satisfy DBTX). Probe failures
// degrade to the legacy scope permanently for that database.
func UnifiedScopeForTx(ctx context.Context, q DBTX) UnifiedScope {
	if unifyScopeOverrideForTest != nil {
		return unifyScopeOverrideForTest(ctx, q)
	}
	return probeUnifiedScope(ctx, q)
}

// ── Table selectors ────────────────────────────────────────────────
//
// Each selector answers the table a statement should address. Re-keyed
// tables gain a leading store column and fold the original composite key
// behind it; their DDL comes from brainunify.NamespacedDDL (see the build),
// so the column shapes below are the shapes the build creates.

type unifiedTable int

const (
	utConfig unifiedTable = iota
	utMetadata
	utLocalMetadata
	utCustomStatuses
	utCustomTypes
)

// legacyName is the single-valued table's original name.
func (t unifiedTable) legacyName() string {
	switch t {
	case utConfig:
		return "config"
	case utMetadata:
		return "metadata"
	case utLocalMetadata:
		return "local_metadata"
	case utCustomStatuses:
		return "custom_statuses"
	case utCustomTypes:
		return "custom_types"
	}
	return ""
}

// rekeyedName is the brain_unified_ copy's name (brainunify.NamespacedPrefix
// + legacy name — keep in step with internal/brainunify/schema.go).
func (t unifiedTable) rekeyedName() string {
	return "brain_unified_" + t.legacyName()
}

// name answers the table to address under this scope.
func (sc UnifiedScope) name(t unifiedTable) string {
	if sc.Enabled() {
		return t.rekeyedName()
	}
	return t.legacyName()
}

// scopeFilter renders the namespace predicate for a re-keyed table: a
// leading `store = ?` plus its argument. Legacy statements get nothing.
func (sc UnifiedScope) scopeFilter(prefix string) (string, []any) {
	if !sc.Enabled() {
		return "", nil
	}
	return prefix + "`store` = ?", []any{sc.Store}
}

// configReadQuery / configReadArgs answer the SELECT of one config row under
// this scope. Shared by the InTx readers and the stray direct reads so every
// caller resolves the same statement for the same scope.
func (sc UnifiedScope) configReadQuery() string {
	if sc.Enabled() {
		return "SELECT value FROM " + sc.name(utConfig) + " WHERE `store` = ? AND `key` = ?"
	}
	return "SELECT value FROM config WHERE `key` = ?"
}

func (sc UnifiedScope) configReadArgs() []any {
	if !sc.Enabled() {
		return nil
	}
	return []any{sc.Store}
}

// customTable answers the table a custom-config statement should address
// and the namespace WHERE-clause (with trailing space) under this scope.
func (sc UnifiedScope) customTable(t unifiedTable) (string, string) {
	if sc.Enabled() {
		return t.rekeyedName(), "WHERE `store` = ? "
	}
	return t.legacyName(), ""
}

// customTableArgs is the namespace prefix of a scoped statement's argument
// list: the store argument when scoped, nothing for legacy.
func (sc UnifiedScope) customTableArgs() []any { return sc.configReadArgs() }

// deleteAllCustomRowsInTx clears the namespace's rows (or all rows on a
// legacy database) from the addressed custom table.
func (sc UnifiedScope) deleteAllCustomRowsInTx(ctx context.Context, tx DBTX, t unifiedTable) (sql.Result, error) {
	table, filter := sc.customTable(t)
	query := "DELETE FROM " + table
	var args []any
	if filter != "" {
		query += " WHERE `store` = ?"
		args = sc.customTableArgs()
	}
	return tx.ExecContext(ctx, query, args...)
}

// insertCustomStatusRowInTx / insertCustomTypeRowInTx write one derived
// custom row into the table the scope addresses. The wrapper's namespace
// rides in the leading store column only on the unified database.
func (sc UnifiedScope) insertCustomStatusRowInTx(ctx context.Context, tx DBTX, name, category string) error {
	if sc.Enabled() {
		_, err := tx.ExecContext(ctx,
			"INSERT INTO "+sc.name(utCustomStatuses)+" (`store`, name, category) VALUES (?, ?, ?)",
			sc.Store, name, category)
		return err
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO custom_statuses (name, category) VALUES (?, ?)", name, category)
	return err
}

func (sc UnifiedScope) insertCustomTypeRowInTx(ctx context.Context, tx DBTX, name string) error {
	if sc.Enabled() {
		_, err := tx.ExecContext(ctx,
			"INSERT INTO "+sc.name(utCustomTypes)+" (`store`, name) VALUES (?, ?)",
			sc.Store, name)
		return err
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO custom_types (name) VALUES (?)", name)
	return err
}

// metadataWriteInTx writes the is-blocked recompute coordination slots
// (blocked_merge.go) into the table the scope addresses, with the leading
// store column on the unified database.
func (sc UnifiedScope) metadataWriteInTx(ctx context.Context, tx DBTX, verb string, key, value string) error {
	var args []any
	var query string
	if sc.Enabled() {
		query = verb + " INTO " + sc.name(utMetadata) + " (`store`, `key`) VALUES (?, ?)"
		args = []any{sc.Store, key}
	} else {
		query = verb + " INTO metadata (`key`) VALUES (?)"
		args = []any{key}
	}
	if value != "" {
		if sc.Enabled() {
			query = verb + " INTO " + sc.name(utMetadata) + " (`store`, `key`, value) VALUES (?, ?, ?)"
			args = append(args, value)
		} else {
			query = verb + " INTO metadata (`key`, value) VALUES (?, ?)"
			args = append(args, value)
		}
	}
	_, err := tx.ExecContext(ctx, query, args...)
	return err
}

// metadataIgnoreQuery answers the INSERT IGNORE of a metadata row with a
// value under this scope (the blocked-merge self-heal marker).
func (sc UnifiedScope) metadataIgnoreQuery() string {
	if sc.Enabled() {
		return "INSERT IGNORE INTO " + sc.name(utMetadata) + " (`store`, `key`, value) VALUES (?, ?, ?)"
	}
	return "INSERT IGNORE INTO metadata (`key`, value) VALUES (?, ?)"
}

// metadataWriteArgs prefixes the row's value arguments with the namespace.
func (sc UnifiedScope) metadataWriteArgs(values ...any) []any {
	if !sc.Enabled() {
		return values
	}
	return append([]any{sc.Store}, values...)
}

// metadataReadQuery answers the SELECT of one metadata row's value.
func (sc UnifiedScope) metadataReadQuery() string {
	if sc.Enabled() {
		return "SELECT value FROM " + sc.name(utMetadata) + " WHERE `store` = ? AND `key` = ?"
	}
	return "SELECT value FROM metadata WHERE `key` = ?"
}

// metadataReadArgs prefixes a metadata key argument with the namespace.
func (sc UnifiedScope) metadataReadArgs(key string) []any {
	if !sc.Enabled() {
		return []any{key}
	}
	return []any{sc.Store, key}
}

// metadataDeleteQuery answers the DELETE of one metadata row.
func (sc UnifiedScope) metadataDeleteQuery() string {
	if sc.Enabled() {
		return "DELETE FROM " + sc.name(utMetadata) + " WHERE `store` = ? AND `key` = ?"
	}
	return "DELETE FROM metadata WHERE `key` = ?"
}

// ResetUnifiedScopeCacheForTest clears the memoised probe answers. Test-only:
// package-scoped tests must not inherit one database's cached answer in
// another's expectations.
func ResetUnifiedScopeCacheForTest() { unifiedScopeCache = sync.Map{} }
