package issueops

import (
	"context"
	"reflect"
	"regexp"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/steveyegge/beads/internal/types"
)

func pinWide(t *testing.T, wide bool) {
	t.Helper()
	prev := NamespaceReadWide()
	SetNamespaceReadWide(wide)
	t.Cleanup(func() { SetNamespaceReadWide(prev) })
}

func expectPrefixes(mock sqlmock.Sqlmock, store string, prefixes ...string) {
	rows := sqlmock.NewRows([]string{"prefix"})
	for _, p := range prefixes {
		rows.AddRow(p)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT prefix FROM brain_store_prefixes WHERE `store` = ?")).
		WithArgs(store).WillReturnRows(rows)
}

func TestScopeIssueFilterNarrowsToOwnedPrefixes(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "task"})
	pinWide(t, false)
	mock, db := newMock(t)
	expectPrefixes(mock, "task", "task", "tsk")

	got := ScopeIssueFilter(context.Background(), db, types.IssueFilter{})
	if want := []string{"task", "tsk"}; !reflect.DeepEqual(got.Namespaces, want) {
		t.Fatalf("Namespaces = %v, want %v", got.Namespaces, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestScopeFallsBackToStoreNameWithoutPrefixRows(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "task"})
	pinWide(t, false)
	mock, db := newMock(t)
	expectPrefixes(mock, "task")

	got := ScopeWorkFilter(context.Background(), db, types.WorkFilter{})
	if want := []string{"task"}; !reflect.DeepEqual(got.Namespaces, want) {
		t.Fatalf("Namespaces = %v, want %v", got.Namespaces, want)
	}
}

func TestScopeLeavesWideLegacyAndCallerScopesAlone(t *testing.T) {
	ctx := context.Background()

	t.Run("wide leaves the filter unscoped", func(t *testing.T) {
		pinScope(t, UnifiedScope{Unified: true, Store: "task"})
		pinWide(t, true)
		_, db := newMock(t) // no query is expected
		if got := ScopeIssueFilter(ctx, db, types.IssueFilter{}); got.Namespaces != nil {
			t.Fatalf("wide read was scoped to %v", got.Namespaces)
		}
		if got := StatisticNamespaces(ctx, db); got != nil {
			t.Fatalf("wide statistics were scoped to %v", got)
		}
	})

	t.Run("a legacy database is never scoped", func(t *testing.T) {
		pinScope(t, UnifiedScope{})
		pinWide(t, false)
		_, db := newMock(t)
		if got := ScopeStaleFilter(ctx, db, types.StaleFilter{}); got.Namespaces != nil {
			t.Fatalf("legacy read was scoped to %v", got.Namespaces)
		}
	})

	t.Run("a unified database with no namespace pinned is not guessed at", func(t *testing.T) {
		pinScope(t, UnifiedScope{Unified: true})
		pinWide(t, false)
		_, db := newMock(t)
		if got := ScopeIssueFilter(ctx, db, types.IssueFilter{}); got.Namespaces != nil {
			t.Fatalf("storeless read was scoped to %v", got.Namespaces)
		}
	})

	t.Run("a caller-set scope is never overridden", func(t *testing.T) {
		pinScope(t, UnifiedScope{Unified: true, Store: "task"})
		pinWide(t, false)
		_, db := newMock(t)
		got := ScopeIssueFilter(ctx, db, types.IssueFilter{Namespaces: []string{"idea"}})
		if want := []string{"idea"}; !reflect.DeepEqual(got.Namespaces, want) {
			t.Fatalf("Namespaces = %v, want %v", got.Namespaces, want)
		}
	})
}

func TestScopeWorkFilterPinsCustomStatusesInBothModes(t *testing.T) {
	for _, wide := range []bool{false, true} {
		pinScope(t, UnifiedScope{Unified: true, Store: "task"})
		pinWide(t, wide)
		mock, db := newMock(t)
		if !wide {
			expectPrefixes(mock, "task", "task")
		}
		got := ScopeWorkFilter(context.Background(), db, types.WorkFilter{})
		if got.CustomStatusStore != "task" {
			t.Errorf("wide=%v: CustomStatusStore = %q, want task", wide, got.CustomStatusStore)
		}
		if wide && got.Namespaces != nil {
			t.Errorf("wide read was scoped to %v", got.Namespaces)
		}
	}
}

func TestNamespaceAllowsIssueID(t *testing.T) {
	ctx := context.Background()

	t.Run("narrow unified reads reject foreign IDs", func(t *testing.T) {
		pinScope(t, UnifiedScope{Unified: true, Store: "task"})
		pinWide(t, false)
		mock, db := newMock(t)
		expectPrefixes(mock, "task", "task", "tsk")
		if NamespaceAllowsIssueID(ctx, db, "other-1") {
			t.Fatal("foreign issue ID was allowed")
		}
		expectPrefixes(mock, "task", "task", "tsk")
		if !NamespaceAllowsIssueID(ctx, db, "tsk-1") {
			t.Fatal("registered issue prefix was rejected")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("wide and legacy reads permit IDs", func(t *testing.T) {
		for _, sc := range []UnifiedScope{{Unified: true, Store: "task"}, {}} {
			pinScope(t, sc)
			pinWide(t, true)
			_, db := newMock(t)
			if !NamespaceAllowsIssueID(ctx, db, "other-1") {
				t.Fatal("unscoped read rejected an ID")
			}
		}
	})
}
