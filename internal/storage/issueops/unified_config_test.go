package issueops

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"regexp"
	"sync"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/steveyegge/beads/internal/types"
)

// pinScope replaces the scope resolver for the duration of a test so the
// statements a call issues can be asserted without a live Dolt server.
func pinScope(t *testing.T, sc UnifiedScope) {
	t.Helper()
	restore := unifyScopeOverrideForTest
	unifyScopeOverrideForTest = func(ctx context.Context, q DBTX) UnifiedScope { return sc }
	t.Cleanup(func() { unifyScopeOverrideForTest = restore })
}

func newMock(t *testing.T) (sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return mock, db
}

func txOf(ctx context.Context, t *testing.T, mock sqlmock.Sqlmock, db *sql.DB) *sql.Tx {
	t.Helper()
	mock.ExpectBegin()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	return tx
}

// TestUnifiedScopeLegacyByDefault pins the pre-unification behaviour: with
// no override and a probe that fails (no information_schema on the mock),
// every statement addresses the original single-valued tables.
func TestUnifiedScopeLegacyByDefault(t *testing.T) {
	sc := UnifiedScope{}
	pinScope(t, sc)
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)

	mock.ExpectExec(regexp.QuoteMeta("REPLACE INTO config (`key`, value) VALUES (?, ?)")).
		WithArgs("issue_prefix", "task").
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := SetConfigInTx(ctx, tx, "issue_prefix", "task-"); err != nil {
		t.Fatalf("SetConfigInTx: %v", err)
	}
	mock.ExpectCommit()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("legacy config write changed: %v", err)
	}
}

// TestUnifiedScopedConfigRouting pins the unified shape: config statements
// address brain_unified_config and carry the wrapper's namespace.
func TestUnifiedScopedConfigRouting(t *testing.T) {
	sc := UnifiedScope{Unified: true, Store: "task"}
	pinScope(t, sc)
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)

	mock.ExpectExec(regexp.QuoteMeta("REPLACE INTO brain_unified_config (`store`, `key`, value) VALUES (?, ?, ?)")).
		WithArgs("task", "issue_prefix", "task").
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := SetConfigInTx(ctx, tx, "issue_prefix", "task-"); err != nil {
		t.Fatalf("SetConfigInTx scoped: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM brain_unified_config WHERE `store` = ? AND `key` = ?")).
		WithArgs("task", "kv.memory.model").
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("claude"))
	got, err := GetConfigInTx(ctx, tx, "kv.memory.model")
	if err != nil {
		t.Fatalf("GetConfigInTx scoped: %v", err)
	}
	if got != "claude" {
		t.Fatalf("got %q", got)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT `key`, value FROM brain_unified_config WHERE `store` = ?")).
		WithArgs("task").
		WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).AddRow("a", "1"))
	all, err := GetAllConfigInTx(ctx, tx)
	if err != nil {
		t.Fatalf("GetAllConfigInTx scoped: %v", err)
	}
	if !reflect.DeepEqual(all, map[string]string{"a": "1"}) {
		t.Fatalf("all = %v", all)
	}

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM brain_unified_config WHERE `store` = ? AND `key` = ?")).
		WithArgs("task", "a").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := DeleteConfigInTx(ctx, tx, "a"); err != nil {
		t.Fatalf("DeleteConfigInTx scoped: %v", err)
	}

	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("scoped config routing: %v", err)
	}
}

// TestUnifiedScopedMetadataAndCustomRouting pins the metadata, local_metadata
// and derived custom-table story: same tables, one store column ahead.
func TestUnifiedScopedMetadataAndCustomRouting(t *testing.T) {
	sc := UnifiedScope{Unified: true, Store: "brain"}
	pinScope(t, sc)
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)

	mock.ExpectExec(regexp.QuoteMeta("REPLACE INTO brain_unified_metadata (`store`, `key`, value) VALUES (?, ?, ?)")).
		WithArgs("brain", "k", "v").
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := SetMetadataInTx(ctx, tx, "k", "v"); err != nil {
		t.Fatalf("SetMetadataInTx: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM brain_unified_metadata WHERE `store` = ? AND `key` = ?")).
		WithArgs("brain", "k").
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("v"))
	if v, err := GetMetadataInTx(ctx, tx, "k"); err != nil || v != "v" {
		t.Fatalf("GetMetadataInTx = %q, %v", v, err)
	}

	mock.ExpectExec(regexp.QuoteMeta("REPLACE INTO brain_unified_local_metadata (`store`, `key`, value) VALUES (?, ?, ?)")).
		WithArgs("brain", "sync_cursor", "7").
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := SetLocalMetadataInTx(ctx, tx, "sync_cursor", "7"); err != nil {
		t.Fatalf("SetLocalMetadataInTx: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM brain_unified_local_metadata WHERE `store` = ? AND `key` = ?")).
		WithArgs("brain", "sync_cursor").
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("7"))
	if v, err := GetLocalMetadataInTx(ctx, tx, "sync_cursor"); err != nil || v != "7" {
		t.Fatalf("GetLocalMetadataInTx = %q, %v", v, err)
	}

	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("scoped metadata routing: %v", err)
	}
}

// TestUnifiedScopedCustomTables pins the derived-table story: reads filter by
// namespace, syncs clear only the namespace's rows, and every inserted row
// carries its store.
func TestUnifiedScopedCustomTables(t *testing.T) {
	sc := UnifiedScope{Unified: true, Store: "assert"}
	pinScope(t, sc)
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM brain_unified_custom_statuses WHERE `store` = ?")).
		WithArgs("assert").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO brain_unified_custom_statuses (`store`, name, category) VALUES (?, ?, ?)")).
		WithArgs("assert", "review", "active").
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := SyncCustomStatusesTable(ctx, tx, "review:active"); err != nil {
		t.Fatalf("SyncCustomStatusesTable scoped: %v", err)
	}

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM brain_unified_custom_types WHERE `store` = ?")).
		WithArgs("assert").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO brain_unified_custom_types (`store`, name) VALUES (?, ?)")).
		WithArgs("assert", "gate").
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := SyncCustomTypesTable(ctx, tx, `["gate"]`); err != nil {
		t.Fatalf("SyncCustomTypesTable scoped: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT name FROM brain_unified_custom_types WHERE `store` = ? ORDER BY name")).
		WithArgs("assert").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("gate"))
	types, err := ResolveCustomTypesInTx(ctx, tx)
	if err != nil {
		t.Fatalf("ResolveCustomTypesInTx scoped: %v", err)
	}
	if !reflect.DeepEqual(types, []string{"gate"}) {
		t.Fatalf("types = %v", types)
	}

	// EnsureCustomTypeInTx resolves the namespace's types first (table hit,
	// no config fallback), finds no "convoy", and inserts it namespaced.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT name FROM brain_unified_custom_types WHERE `store` = ? ORDER BY name")).
		WithArgs("assert").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("gate"))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO brain_unified_custom_types (`store`, name) VALUES (?, ?)")).
		WithArgs("assert", "convoy").
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := EnsureCustomTypeInTx(ctx, tx, "convoy"); err != nil {
		t.Fatalf("EnsureCustomTypeInTx scoped: %v", err)
	}

	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("scoped custom tables: %v", err)
	}
}

// TestUnifiedScopedCustomTablesLegacy pins the legacy shapes: no store column,
// no namespace filter — the pre-unification statements byte for byte.
func TestUnifiedScopedCustomTablesLegacy(t *testing.T) {
	pinScope(t, UnifiedScope{})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM custom_statuses")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO custom_statuses (name, category) VALUES (?, ?)")).
		WithArgs("review", "active").
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := SyncCustomStatusesTable(ctx, tx, "review:active"); err != nil {
		t.Fatalf("SyncCustomStatusesTable legacy: %v", err)
	}

	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("legacy custom tables: %v", err)
	}
}

// TestUnifiedScopeWithoutNamespaceDegrades pins the degradation rule: unified
// database but no wrapper name means the template-seeded single-valued tables
// are addressed — the pre-cutover behaviour — rather than a guessed namespace.
func TestUnifiedScopeWithoutNamespaceDegrades(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM config WHERE `key` = ?")).
		WithArgs("issue_prefix").
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("task"))
	got, err := GetConfigInTx(ctx, tx, "issue_prefix")
	if err != nil {
		t.Fatalf("degraded read: %v", err)
	}
	if got != "task" {
		t.Fatalf("got %q", got)
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("degraded routing: %v", err)
	}
}

// TestProbeFailureDegradesToLegacy pins that a probe error (a non-Dolt or
// pre-unification database) resolves to the legacy scope, and that the probe
// answer is memoised per database so later calls issue no extra queries.
func TestProbeFailureDegradesToLegacy(t *testing.T) {
	unifiedScopeCache = sync.Map{}
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)

	mock.ExpectQuery("select database\\(\\)").WillReturnError(errors.New("not a dolt server"))
	sc := probeUnifiedScope(ctx, tx)
	if sc.Unified || sc.Store != "" {
		t.Fatalf("probe failure should degrade to legacy, got %+v", sc)
	}
	// Memoised: the second call issues no further queries.
	if sc2 := probeUnifiedScope(ctx, tx); sc2 != sc {
		t.Fatalf("memoised scope changed: %+v vs %+v", sc2, sc)
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("probe failure: %v", err)
	}
}

// TestProbeSuccessMemoises pins the probe's success path: it discovers the
// connected database, checks for the re-keyed config table, reads the
// wrapper's namespace once, and later calls reuse the cached answer.
func TestProbeSuccessMemoises(t *testing.T) {
	unifiedScopeCache = sync.Map{}
	t.Setenv("BD_NAME", "task")
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)

	mock.ExpectQuery("select database\\(\\)").
		WillReturnRows(sqlmock.NewRows([]string{"db"}).AddRow("brain_unified"))
	mock.ExpectQuery(regexp.QuoteMeta("select count(*) from information_schema.tables where table_schema = ? and table_name = 'brain_unified_config'")).
		WithArgs("brain_unified").
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	sc := probeUnifiedScope(ctx, tx)
	if want := (UnifiedScope{Unified: true, Store: "task"}); sc != want {
		t.Fatalf("probe = %+v, want %+v", sc, want)
	}
	// Memoised: the second call re-identifies the database (one cheap builtin)
	// but skips the information_schema probe.
	mock.ExpectQuery("select database\\(\\)").
		WillReturnRows(sqlmock.NewRows([]string{"db"}).AddRow("brain_unified"))
	if sc2 := probeUnifiedScope(ctx, tx); sc2 != sc {
		t.Fatalf("memoised scope changed: %+v vs %+v", sc2, sc)
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("probe success: %v", err)
	}
}

// TestScanIssueCountsScopedInTx pins statistics scoping: the namespace clauses
// are ANDed into the counts; nil prefixes keeps the unscoped single query.
func TestScanIssueCountsScopedInTx(t *testing.T) {
	ctx := context.Background()
	pinScope(t, UnifiedScope{})

	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	mock.ExpectQuery("SELECT").WillReturnRows(sqlmock.NewRows(
		[]string{"total", "open", "in_progress", "closed", "deferred", "pinned"}).
		AddRow(3, 1, 0, 2, 0, 0))
	stats := &types.Statistics{}
	if err := ScanIssueCountsScopedInTx(ctx, tx, stats, nil); err != nil {
		t.Fatalf("unscoped: %v", err)
	}
	if stats.TotalIssues != 3 {
		t.Fatalf("total = %d", stats.TotalIssues)
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unscoped statistics: %v", err)
	}
}

// TestScanIssueCountsScopedInTx_Namespaced pins the scoped variant: the
// namespace clauses are ANDed into the count query with the store prefixes.
func TestScanIssueCountsScopedInTx_Namespaced(t *testing.T) {
	ctx := context.Background()
	pinScope(t, UnifiedScope{})

	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	mock.ExpectQuery("FROM issues WHERE \\(id LIKE \\? OR id LIKE \\?\\)").
		WithArgs("task-%", "brain-%").
		WillReturnRows(sqlmock.NewRows(
			[]string{"total", "open", "in_progress", "closed", "deferred", "pinned"}).
			AddRow(1, 1, 0, 0, 0, 0))
	stats := &types.Statistics{}
	if err := ScanIssueCountsScopedInTx(ctx, tx, stats, []string{"task-", "brain"}); err != nil {
		t.Fatalf("scoped: %v", err)
	}
	if stats.TotalIssues != 1 {
		t.Fatalf("total = %d", stats.TotalIssues)
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("scoped statistics: %v", err)
	}
}
