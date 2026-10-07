package issueops

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"regexp"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// The unified database's mint boundary, at the level the create path sees it:
// NewBatchContext calls RefuseStorelessMint before anything else, so a
// storeless caller on the unified database gets the refusal instead of
// silently borrowing the template-seeded config tables.

func TestRefuseStorelessMintUnifiedWithoutNamespace(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: ""})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	err := RefuseStorelessMint(ctx, tx)
	if err == nil {
		t.Fatal("storeless mint on the unified database must refuse, got nil")
	}
	if !errors.Is(err, ErrStorelessNamespace) {
		t.Fatalf("err = %v, want ErrStorelessNamespace", err)
	}
	for _, want := range []string{"refusing to create", "BD_NAME"} {
		if !contains(err.Error(), want) {
			t.Fatalf("err message %q must carry %q", err.Error(), want)
		}
	}
}

func TestRefuseStorelessMintUnifiedWithNamespacePasses(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "task"})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	if err := RefuseStorelessMint(ctx, tx); err != nil {
		t.Fatalf("wrapper-pinned create must pass the boundary, got %v", err)
	}
}

func TestRefuseStorelessMintLegacyDatabasePasses(t *testing.T) {
	// Legacy scope: no unified tables, nothing to refuse and no behaviour change.
	pinScope(t, UnifiedScope{Unified: false, Store: ""})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	if err := RefuseStorelessMint(ctx, tx); err != nil {
		t.Fatalf("legacy database must keep pre-unification behaviour, got %v", err)
	}
}

// rowsOf is the one-shape row builder the tests below reuse.
func rowsOf(t *testing.T, cols string, rows ...[]string) *sqlmock.Rows {
	t.Helper()
	names := splitComma(t, cols)
	m := sqlmock.NewRows(names)
	for _, r := range rows {
		vals := make([]driver.Value, 0, len(r))
		for _, v := range r {
			vals = append(vals, v)
		}
		m.AddRow(vals...)
	}
	return m
}

func splitComma(t *testing.T, s string) []string {
	t.Helper()
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

// Ownership: an id whose prefix the namespace does not own refuses, with the
// way out named, and every id shape PrefixOf buckets is checked through the
// same first-'-' rule.

func TestValidateNamespaceOwnershipOwned(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "task"})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT prefix FROM brain_store_prefixes WHERE `store` = ?")).
		WithArgs("task").
		WillReturnRows(rowsOf(t, "prefix", []string{"task"}, []string{"robot"}))

	for _, id := range []string{"task-4hxq1", "task-12", "task-se7t.389", "task-wisp-z1a2b", "robot-7f3ee"} {
		if err := ValidateNamespaceOwnership(ctx, tx, "task", id); err != nil {
			t.Fatalf("id %q: want owned, got %v", id, err)
		}
	}
}

func TestValidateNamespaceOwnershipForeignPrefixRefuses(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "task"})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT prefix FROM brain_store_prefixes WHERE `store` = ?")).
		WithArgs("task").
		WillReturnRows(rowsOf(t, "prefix", []string{"task"}))

	err := ValidateNamespaceOwnership(ctx, tx, "task", "brain-9w8e7")
	if err == nil {
		t.Fatal("brain- id under the task namespace must refuse")
	}
	if !errors.Is(err, ErrPrefixOwnership) {
		t.Fatalf("err = %v, want ErrPrefixOwnership", err)
	}
	for _, want := range []string{"brain", "store-prefix add"} {
		if !contains(err.Error(), want) {
			t.Fatalf("err message %q must carry %q", err.Error(), want)
		}
	}
}

func TestValidateNamespaceOwnershipOwnNameNeedsNoRecord(t *testing.T) {
	// The namespace's own prefix is owned even when the record is empty or
	// unreadable: the wrapper's name is always the baseline.
	pinScope(t, UnifiedScope{Unified: true, Store: "task"})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	if err := ValidateNamespaceOwnership(ctx, tx, "task", "task-1a2b3c"); err != nil {
		t.Fatalf("own-prefix id must never refuse, got %v", err)
	}
}

// The runtime claim: fresh prefix records, already-owned is idempotent, and a
// foreign owner refuses without rewriting the row.

func TestRecordStorePrefixFreshClaim(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "task"})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT `store`, owner_reason FROM brain_store_prefixes WHERE prefix = ?")).
		WithArgs("proto").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO brain_store_prefixes (`prefix`,`store`,`owner_reason`,`declared_by`,`observed_by`,`bead_count`,`ambiguous`) VALUES (?, ?, 'operator-added', '', '', 0, 0)")).
		WithArgs("proto", "task").
		WillReturnResult(sqlmock.NewResult(1, 1))
	// allowed_prefixes read (no row) then scoped REPLACE for the store.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM brain_unified_config WHERE `store` = ? AND `key` = ?")).
		WithArgs("task", "allowed_prefixes").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("REPLACE INTO brain_unified_config (`store`,`key`,value) VALUES (?, ?, ?)")).
		WithArgs("task", "allowed_prefixes", "proto").
		WillReturnResult(sqlmock.NewResult(1, 1))

	out, err := RecordStorePrefix(ctx, tx, "task", "proto")
	if err != nil {
		t.Fatalf("fresh claim: %v", err)
	}
	if out.AlreadyOwned {
		t.Fatal("fresh claim cannot be AlreadyOwned")
	}
	if out.Owner != "task" || out.Reason != "operator-added" {
		t.Fatalf("owner/reason = %q/%q, want task/operator-added", out.Owner, out.Reason)
	}
	if !out.AllowedPrefixesUpdated {
		t.Fatal("allowed_prefixes must be extended so creation works immediately")
	}
}

func TestRecordStorePrefixAlreadyOwned(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "task"})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT `store`, owner_reason FROM brain_store_prefixes WHERE prefix = ?")).
		WithArgs("task").
		WillReturnRows(rowsOf(t, "store,owner_reason", []string{"task", "store-name-matches-prefix"}))

	out, err := RecordStorePrefix(ctx, tx, "task", "task")
	if err != nil {
		t.Fatalf("already-owned claim must be a silent no-op, got %v", err)
	}
	if !out.AlreadyOwned {
		t.Fatalf("out = %+v, want AlreadyOwned", out)
	}
}

func TestRecordStorePrefixForeignOwnerRefuses(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "task"})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	// brain was decided for the brain store by the build; a task claim must
	// refuse loudly, name the owner, and NOT rewrite the row: after this
	// query there are no further write expectations for this test.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `store`, owner_reason FROM brain_store_prefixes WHERE prefix = ?")).
		WithArgs("brain").
		WillReturnRows(rowsOf(t, "store,owner_reason", []string{"brain", "declared-by-store-config"}))

	_, err := RecordStorePrefix(ctx, tx, "task", "brain")
	if err == nil {
		t.Fatal("claiming another store's prefix must refuse")
	}
	if want := "owned by store \"brain\""; !contains(err.Error(), want) {
		t.Fatalf("err message %q must name the owning store", err.Error())
	}
}

func TestRecordStorePrefixRejectsBadShapes(t *testing.T) {
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	// Shape validation runs before any query, so a refused prefix issues
	// nothing — the one tx-rolling assertion guards that for every shape.
	for _, bad := range []string{"cross-store", "a-b", "has space", "", "-lead", "9digit"} {
		if _, err := RecordStorePrefix(ctx, tx, "task", bad); err == nil {
			t.Fatalf("prefix %q: want refusal, got nil", bad)
		}
	}
}

func TestIsValidAddedPrefix(t *testing.T) {
	valid := []string{"proto", "agent", "TinyKeyboard", "beads2", "under_score", "q"}
	invalid := []string{"", "a-b", "-a", "a b", "1digit", "much-too-long-because-of-lim"}
	for _, p := range valid {
		if !IsValidAddedPrefix(p) {
			t.Errorf("%q: want valid", p)
		}
	}
	for _, p := range invalid {
		if IsValidAddedPrefix(p) {
			t.Errorf("%q: want invalid", p)
		}
	}
}

func TestListStorePrefixes(t *testing.T) {
	ctx := context.Background()
	mock, db := newMock(t)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT `prefix`,`store`,owner_reason,bead_count FROM brain_store_prefixes ORDER BY `prefix` ASC")).
		WillReturnRows(rowsOf(t, "prefix,store,owner_reason,bead_count",
			[]string{"brain", "brain", "declared-by-store-config", "4312"},
			[]string{"proto", "task", "operator-added", "0"}))

	rows, err := ListStorePrefixes(ctx, db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Store != "brain" || rows[1].Reason != "operator-added" || rows[1].BeadCount != 0 {
		t.Fatalf("rows = %+v", rows)
	}
}

// contains keeps the intent of the error-message asserts readable.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
