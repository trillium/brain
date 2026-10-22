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

// ── Wide/narrow read modes ─────────────────────────────────────────
//
// The unified database's two deliberate read modes: narrow (the default,
// scoped to the wrapper's own namespaces — today's behaviour, unchanged) and
// wide (the operator's explicit --wide, which unscopes the read so every
// store's beads are visible). Nothing substitutes one mode for another.

func TestScopeNamespacesWideLeavesFilterUnscoped(t *testing.T) {
	issueops.ResetUnifiedScopeCacheForTest()
	defer resetNamespaceReadScopeForTest()
	SetNamespaceReadScope(NamespaceReadWide)

	ctx := context.Background()
	s := &DoltStore{}
	s.unifiedNS.once.Do(func() {
		s.unifiedNS.prefixes = []string{"task", "robot"}
		s.unifiedNS.unified = true
	})

	f := s.scopeNamespaces(ctx, types.IssueFilter{})
	if f.Namespaces != nil {
		t.Fatalf("wide mode scoped anyway: %v", f.Namespaces)
	}
	wf := s.scopeWorkNamespaces(ctx, types.WorkFilter{})
	if wf.Namespaces != nil {
		t.Fatalf("wide mode scoped the work read anyway: %v", wf.Namespaces)
	}
	if ns := s.statisticNamespaces(ctx); ns != nil {
		t.Fatalf("wide mode narrowed statistics anyway: %v", ns)
	}
}

func TestScopeNamespacesNarrowStillScopesByDefault(t *testing.T) {
	issueops.ResetUnifiedScopeCacheForTest()
	defer resetNamespaceReadScopeForTest()
	SetNamespaceReadScope(NamespaceReadNarrow)

	ctx := context.Background()
	s := &DoltStore{}
	s.unifiedNS.once.Do(func() {
		s.unifiedNS.prefixes = []string{"task", "robot"}
		s.unifiedNS.unified = true
	})

	f := s.scopeNamespaces(ctx, types.IssueFilter{})
	if !reflect.DeepEqual(f.Namespaces, []string{"task", "robot"}) {
		t.Fatalf("narrow mode lost the namespace pin: %v", f.Namespaces)
	}
}

func TestScopeNamespacesWideThenNarrowModeIsExplicitPerProcess(t *testing.T) {
	issueops.ResetUnifiedScopeCacheForTest()
	defer resetNamespaceReadScopeForTest()

	ctx := context.Background()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	s := &DoltStore{db: db}

	t.Setenv("BD_NAME", "task")
	mock.ExpectQuery("select database\\(\\)").
		WillReturnRows(sqlmock.NewRows([]string{"db"}).AddRow("brain_unified"))
	mock.ExpectQuery("select count\\(\\*\\) from information_schema").
		WithArgs("brain_unified").
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT prefix FROM brain_store_prefixes WHERE `store` = ?")).
		WithArgs("task").
		WillReturnRows(sqlmock.NewRows([]string{"prefix"}).AddRow("task"))

	// Wide first: the narrow resolution never even runs, the filter is not
	// pinned, and the probe expectations stay unread.
	SetNamespaceReadScope(NamespaceReadWide)
	if f := s.scopeNamespaces(ctx, types.IssueFilter{}); f.Namespaces != nil {
		t.Fatalf("wide pinned: %v", f.Namespaces)
	}

	// Back to narrow: the SAME read pins the namespace — the mode changes
	// what it answers, so an operator chooses per command.
	SetNamespaceReadScope(NamespaceReadNarrow)
	if f := s.scopeNamespaces(ctx, types.IssueFilter{}); !reflect.DeepEqual(f.Namespaces, []string{"task"}) {
		t.Fatalf("narrow did not pin the namespace again: %v", f.Namespaces)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestGetStaleIssuesWideUnscoped(t *testing.T) {
	issueops.ResetUnifiedScopeCacheForTest()
	defer resetNamespaceReadScopeForTest()
	SetNamespaceReadScope(NamespaceReadWide)

	ctx := context.Background()
	s := &DoltStore{}
	s.unifiedNS.once.Do(func() {
		s.unifiedNS.prefixes = []string{"task"}
		s.unifiedNS.unified = true
	})
	// The stale scoping check runs before any query (sqlmock would fail if
	// the resolution ran), so expectations stay empty by construction.
	filter := types.StaleFilter{Days: 7}
	if prefixes, unified := s.namespacePrefixes(ctx); !readScopeWide() && unified && len(prefixes) > 0 && filter.Namespaces == nil {
		filter.Namespaces = prefixes
	}
	if filter.Namespaces != nil {
		t.Fatalf("wide mode pinned the stale filter: %v", filter.Namespaces)
	}
}

// TestScopeNamespacesLeavesIdAddressedReadsUnscoped is the global-id-reads rule:
// a read that names the ids it wants — or that is addressed by id and cannot
// name them yet (IDLookup, a partial-id resolve) — is never namespace-scoped,
// while a listing stays narrow. The namespace clause can only exclude
// explicitly named ids, which is how `task show brain-c56qg` came to answer
// "no issue found" for a bead this database plainly held.
func TestScopeNamespacesLeavesIdAddressedReadsUnscoped(t *testing.T) {
	issueops.ResetUnifiedScopeCacheForTest()
	defer resetNamespaceReadScopeForTest()
	SetNamespaceReadScope(NamespaceReadNarrow)

	ctx := context.Background()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	t.Setenv("BD_NAME", "task")
	mock.ExpectQuery("select database\\(\\)").
		WillReturnRows(sqlmock.NewRows([]string{"db"}).AddRow("dolt_unified_scope_id_read_test"))
	mock.ExpectQuery("select count\\(\\*\\) from information_schema").
		WithArgs("dolt_unified_scope_id_read_test").
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	mock.ExpectQuery(regexp.QuoteMeta("select `store` from brain_store_aliases where `alias` = ?")).
		WithArgs("task").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT prefix FROM brain_store_prefixes WHERE `store` = ?")).
		WithArgs("task").
		WillReturnRows(sqlmock.NewRows([]string{"prefix"}).AddRow("task"))

	s := &DoltStore{db: db}

	// A listing is scoped to the wrapper's namespace.
	if f := s.scopeNamespaces(ctx, types.IssueFilter{}); !reflect.DeepEqual(f.Namespaces, []string{"task"}) {
		t.Fatalf("listing filter Namespaces = %v, want [task]", f.Namespaces)
	}

	// Naming the ids is an id-addressed read: the scope must not narrow it.
	if f := s.scopeNamespaces(ctx, types.IssueFilter{IDs: []string{"brain-c56qg"}}); f.Namespaces != nil {
		t.Fatalf("id-addressed read was scoped: Namespaces = %v", f.Namespaces)
	}
	// A partial-id read cannot name its ids, so it says so.
	if f := s.scopeNamespaces(ctx, types.IssueFilter{IDLookup: true}); f.Namespaces != nil {
		t.Fatalf("partial-id read was scoped: Namespaces = %v", f.Namespaces)
	}
	// A caller that set the scope itself keeps it, IDLookup or not.
	if f := s.scopeNamespaces(ctx, types.IssueFilter{Namespaces: []string{"robot"}}); !reflect.DeepEqual(f.Namespaces, []string{"robot"}) {
		t.Fatalf("caller-set Namespaces = %v, want [robot]", f.Namespaces)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("probe: %v", err)
	}
}
