package dolt

import (
	"context"
	"database/sql"
	"reflect"
	"regexp"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// namespaceProbe stubs the unified-database probe answers at the dolt layer.
// resolveNamespacePrefixes is the seam: it takes any DBTX-shaped queryer, so
// sqlmock works without a live server.
func TestResolveNamespacePrefixesUnified(t *testing.T) {
	issueops.ResetUnifiedScopeCacheForTest()
	ctx := context.Background()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	t.Setenv("BD_NAME", "task")
	mock.ExpectQuery("select database\\(\\)").
		WillReturnRows(sqlmock.NewRows([]string{"db"}).AddRow("brain_unified"))
	mock.ExpectQuery("select count\\(\\*\\) from information_schema").
		WithArgs("brain_unified").
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT prefix FROM brain_store_prefixes WHERE `store` = ?")).
		WithArgs("task").
		WillReturnRows(sqlmock.NewRows([]string{"prefix"}).AddRow("task").AddRow("robot"))

	prefixes, unified := resolveNamespacePrefixes(ctx, db)
	if !unified {
		t.Fatal("unified = false, want true")
	}
	if !reflect.DeepEqual(prefixes, []string{"task", "robot"}) {
		t.Fatalf("prefixes = %v", prefixes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestResolveNamespacePrefixesLegacyDatabase(t *testing.T) {
	issueops.ResetUnifiedScopeCacheForTest()
	ctx := context.Background()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	t.Setenv("BD_NAME", "task")
	mock.ExpectQuery("select database\\(\\)").
		WillReturnRows(sqlmock.NewRows([]string{"db"}).AddRow("tasks"))
	mock.ExpectQuery("select count\\(\\*\\) from information_schema").
		WithArgs("tasks").
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))

	prefixes, unified := resolveNamespacePrefixes(ctx, db)
	if unified {
		t.Fatal("unified = true, want false (legacy database)")
	}
	if prefixes != nil {
		t.Fatalf("prefixes = %v, want nil", prefixes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestResolveNamespacePrefixesFallsBackToWrapperName(t *testing.T) {
	issueops.ResetUnifiedScopeCacheForTest()
	ctx := context.Background()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	t.Setenv("BD_NAME", "task")
	mock.ExpectQuery("select database\\(\\)").
		WillReturnRows(sqlmock.NewRows([]string{"db"}).AddRow("brain_unified"))
	mock.ExpectQuery("select count\\(\\*\\) from information_schema").
		WithArgs("brain_unified").
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	// brain_store_prefixes unreadable → fall back to the wrapper's name.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT prefix FROM brain_store_prefixes WHERE `store` = ?")).
		WithArgs("task").
		WillReturnError(sql.ErrConnDone)

	prefixes, unified := resolveNamespacePrefixes(ctx, db)
	if !unified {
		t.Fatal("unified = false, want true")
	}
	if !reflect.DeepEqual(prefixes, []string{"task"}) {
		t.Fatalf("prefixes = %v, want [task]", prefixes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestScopeNamespacesNoopWhenCallerSet(t *testing.T) {
	ctx := context.Background()
	s := &DoltStore{}
	f := s.scopeNamespaces(ctx, types.IssueFilter{Namespaces: []string{"explicit"}})
	if !reflect.DeepEqual(f.Namespaces, []string{"explicit"}) {
		t.Fatalf("caller-set Namespaces clobbered: %v", f.Namespaces)
	}
}

func TestScopeNamespacesNoopOnLegacy(t *testing.T) {
	ctx := context.Background()
	s := &DoltStore{}
	s.unifiedNS.once.Do(func() {
		s.unifiedNS.prefixes = nil
		s.unifiedNS.unified = false
	})
	f := s.scopeNamespaces(ctx, types.IssueFilter{})
	if f.Namespaces != nil {
		t.Fatalf("legacy database scoped anyway: %v", f.Namespaces)
	}
}
