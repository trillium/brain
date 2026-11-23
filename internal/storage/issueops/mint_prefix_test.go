package issueops

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/storage"
)

// initYAMLPrefix puts a config.yaml issue-prefix in effect for one test. Not
// parallel-safe: config is process-global.
func initYAMLPrefix(t *testing.T, prefix string) {
	t.Helper()
	t.Setenv("BEADS_TEST_IGNORE_REPO_CONFIG", "1")
	if err := config.Initialize(); err != nil {
		t.Fatalf("config.Initialize: %v", err)
	}
	t.Cleanup(config.ResetForTesting)
	if prefix != "" {
		config.Set("issue-prefix", prefix)
	}
}

func TestResolveMintPrefix(t *testing.T) {
	cases := []struct {
		name     string
		sc       UnifiedScope
		yaml     string
		dbPrefix string
		want     string
		wantOK   bool
	}{
		// The m1studio1 shape: a stock-like scope (store pinned, scoped row
		// cannot be consulted) over a database whose plain issue_prefix is the
		// template store's seed. The store's own prefix must win.
		{"pinned store, non-unified db: yaml wins over db-wide", UnifiedScope{Store: "task"}, "task", "lifespan", "task", true},
		{"pinned store, probe failed: yaml wins over db-wide", UnifiedScope{Store: "robots"}, "agent", "lifespan", "agent", true},
		{"pinned store, no yaml: db value stays", UnifiedScope{Store: "task"}, "", "lifespan", "lifespan", true},
		{"pinned store, nothing anywhere: no prefix", UnifiedScope{Store: "task"}, "", "", "", false},
		{"pinned store, no db row, yaml present: yaml mints", UnifiedScope{Store: "task"}, "task", "", "task", true},
		{"pinned store, yaml suffix is normalized", UnifiedScope{Store: "task"}, "task-", "lifespan", "task", true},
		// Scoped routing owns the answer: the row is the store's own.
		{"enabled scope: scoped row wins over yaml", UnifiedScope{Unified: true, Store: "ideas"}, "stale", "idea", "idea", true},
		{"enabled scope: scoped suffix is normalized", UnifiedScope{Unified: true, Store: "ideas"}, "stale", "idea-", "idea", true},
		{"enabled scope, row missing: refuse, never yaml", UnifiedScope{Unified: true, Store: "ideas"}, "idea", "", "", false},
		// No store pinned: the legacy behaviour is untouched.
		{"no store pinned: db value used", UnifiedScope{}, "other", "gt", "gt", true},
		{"no store pinned: db suffix is normalized", UnifiedScope{}, "other", "gt-", "gt", true},
		{"no store pinned, no row: no prefix", UnifiedScope{}, "other", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			initYAMLPrefix(t, tc.yaml)
			got, ok := ResolveMintPrefix(tc.sc, tc.dbPrefix)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("ResolveMintPrefix(%+v, %q) = (%q, %v), want (%q, %v)", tc.sc, tc.dbPrefix, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestReadConfigPrefixPinnedStoreBeatsDBWide is the m1studio1 regression: a
// wrapper-pinned store on a database whose db-wide issue_prefix is lifespan
// mints under its own prefix, not lifespan.
func TestReadConfigPrefixPinnedStoreBeatsDBWide(t *testing.T) {
	initYAMLPrefix(t, "task")
	pinScope(t, UnifiedScope{Store: "task"}) // store pinned, scoped routing unavailable
	mock, db := newMock(t)
	ctx := context.Background()
	tx := txOf(ctx, t, mock, db)
	mock.ExpectQuery("SELECT value FROM config WHERE `key` = \\?").
		WithArgs("issue_prefix").
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("lifespan"))

	got, err := ReadConfigPrefix(ctx, tx)
	if err != nil {
		t.Fatalf("ReadConfigPrefix: %v", err)
	}
	if got != "task" {
		t.Fatalf("ReadConfigPrefix() = %q, want %q (the db-wide lifespan must not win over the pinned store)", got, "task")
	}
}

func TestReadConfigPrefixScopedRowStillWins(t *testing.T) {
	initYAMLPrefix(t, "stale")
	pinScope(t, UnifiedScope{Unified: true, Store: "ideas"})
	mock, db := newMock(t)
	ctx := context.Background()
	tx := txOf(ctx, t, mock, db)
	mock.ExpectQuery("SELECT value FROM brain_unified_config WHERE `store` = \\? AND `key` = \\?").
		WithArgs("ideas", "issue_prefix").
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("idea-"))

	got, err := ReadConfigPrefix(ctx, tx)
	if err != nil {
		t.Fatalf("ReadConfigPrefix: %v", err)
	}
	if got != "idea" {
		t.Fatalf("ReadConfigPrefix() = %q, want %q", got, "idea")
	}
}

func TestReadConfigPrefixUnifiedMissingRowRefuses(t *testing.T) {
	initYAMLPrefix(t, "idea")
	pinScope(t, UnifiedScope{Unified: true, Store: "ideas"})
	mock, db := newMock(t)
	ctx := context.Background()
	tx := txOf(ctx, t, mock, db)
	mock.ExpectQuery("SELECT value FROM brain_unified_config WHERE `store` = \\? AND `key` = \\?").
		WithArgs("ideas", "issue_prefix").
		WillReturnRows(sqlmock.NewRows([]string{"value"}))

	if _, err := ReadConfigPrefix(ctx, tx); !errors.Is(err, storage.ErrNotInitialized) {
		t.Fatalf("ReadConfigPrefix error = %v, want ErrNotInitialized", err)
	}
}

// TestProbeUnifiedScopeFailureIsNotCached: a failed shape probe must not pin
// "not unified" for the life of the process.
func TestProbeUnifiedScopeFailureIsNotCached(t *testing.T) {
	t.Setenv("BD_NAME", "task")
	const dbname = "brain_probe_test"
	unifiedScopeCache.Delete(dbname)
	t.Cleanup(func() { unifiedScopeCache.Delete(dbname) })
	mock, db := newMock(t)
	ctx := context.Background()

	mock.ExpectQuery("select database\\(\\)").WillReturnRows(sqlmock.NewRows([]string{"d"}).AddRow(dbname))
	mock.ExpectQuery("information_schema.tables").WillReturnError(errors.New("transient"))
	sc := probeUnifiedScope(ctx, db)
	if sc.Unified || sc.Store != "task" {
		t.Fatalf("failed probe scope = %+v, want not unified with the pinned store kept", sc)
	}
	if _, cached := unifiedScopeCache.Load(dbname); cached {
		t.Fatal("a failed probe was cached")
	}

	mock.ExpectQuery("select database\\(\\)").WillReturnRows(sqlmock.NewRows([]string{"d"}).AddRow(dbname))
	mock.ExpectQuery("information_schema.tables").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	mock.ExpectQuery("brain_store_aliases").WillReturnRows(sqlmock.NewRows([]string{"store"}))
	sc = probeUnifiedScope(ctx, db)
	if !sc.Unified || !sc.Enabled() {
		t.Fatalf("retry scope = %+v, want unified and enabled", sc)
	}
}

func TestProbeMintPrefix(t *testing.T) {
	ctx := context.Background()

	t.Run("pinned store on a non-unified db reports its own prefix beside the db-wide one", func(t *testing.T) {
		initYAMLPrefix(t, "task")
		pinScope(t, UnifiedScope{Store: "task"})
		mock, db := newMock(t)
		tx := txOf(ctx, t, mock, db)
		mock.MatchExpectationsInOrder(false)
		// The db-wide read, then the create path's own read: both see lifespan.
		mock.ExpectQuery("SELECT value FROM config WHERE `key` = \\?").WithArgs("issue_prefix").
			WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("lifespan"))
		mock.ExpectQuery("SELECT value FROM config WHERE `key` = \\?").WithArgs("issue_prefix").
			WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("lifespan"))

		got, err := ProbeMintPrefix(ctx, tx)
		if err != nil {
			t.Fatalf("ProbeMintPrefix: %v", err)
		}
		if got.MintPrefix != "task" || got.DBIssuePrefix != "lifespan" || got.SampleID != "task-probe" {
			t.Fatalf("probe = %+v, want mint task / db lifespan / sample task-probe", got)
		}
		if got.Error != "" {
			t.Fatalf("probe carried an error: %s", got.Error)
		}
	})

	t.Run("probe only reads: it never writes", func(t *testing.T) {
		initYAMLPrefix(t, "task")
		pinScope(t, UnifiedScope{Store: "task"})
		mock, db := newMock(t)
		tx := txOf(ctx, t, mock, db)
		mock.ExpectQuery("SELECT value FROM config").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("lifespan"))
		mock.ExpectQuery("SELECT value FROM config").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("lifespan"))
		if _, err := ProbeMintPrefix(ctx, tx); err != nil {
			t.Fatalf("ProbeMintPrefix: %v", err)
		}
		// sqlmock fails any statement that was not expected (no INSERT/UPDATE
		// was); the two reads above are the whole script.
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("unified storeless caller is refused, as a create would be", func(t *testing.T) {
		initYAMLPrefix(t, "")
		pinScope(t, UnifiedScope{Unified: true})
		mock, db := newMock(t)
		tx := txOf(ctx, t, mock, db)
		mock.ExpectQuery("SELECT value FROM config").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("lifespan"))

		got, err := ProbeMintPrefix(ctx, tx)
		if err != nil {
			t.Fatalf("ProbeMintPrefix: %v", err)
		}
		if got.Error == "" || got.MintPrefix != "" {
			t.Fatalf("probe = %+v, want a refusal and no mint prefix", got)
		}
		if got.DBIssuePrefix != "lifespan" {
			t.Fatalf("db prefix = %q, want lifespan", got.DBIssuePrefix)
		}
	})

	t.Run("unified routed store reports ownership", func(t *testing.T) {
		initYAMLPrefix(t, "")
		pinScope(t, UnifiedScope{Unified: true, Store: "ideas"})
		mock, db := newMock(t)
		tx := txOf(ctx, t, mock, db)
		mock.ExpectQuery("SELECT value FROM config WHERE `key` = \\?").WithArgs("issue_prefix").
			WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("lifespan"))
		mock.ExpectQuery("SELECT value FROM brain_unified_config WHERE `store` = \\? AND `key` = \\?").WithArgs("ideas", "issue_prefix").
			WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("idea"))
		mock.ExpectQuery("SELECT prefix FROM brain_store_prefixes").WithArgs("ideas").
			WillReturnRows(sqlmock.NewRows([]string{"prefix"}).AddRow("idea"))

		got, err := ProbeMintPrefix(ctx, tx)
		if err != nil {
			t.Fatalf("ProbeMintPrefix: %v", err)
		}
		if got.MintPrefix != "idea" || got.Owned == nil || !*got.Owned || !got.Routed {
			t.Fatalf("probe = %+v, want routed, mint idea, owned", got)
		}
	})
}
