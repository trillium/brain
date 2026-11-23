package db

import (
	"context"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/steveyegge/beads/internal/config"
)

// TestGetConfigIssuePrefixPinnedStoreWinsOverDBWide pins the m1studio1 shape on
// the domain create path: a wrapper-pinned store (BD_NAME) over a database whose
// plain config.issue_prefix is the template store's seed ("lifespan") resolves
// its own config.yaml prefix, because the scoped row cannot be consulted.
func TestGetConfigIssuePrefixPinnedStoreWinsOverDBWide(t *testing.T) {
	t.Setenv("BD_NAME", "task")
	t.Setenv("BEADS_TEST_IGNORE_REPO_CONFIG", "1")
	if err := config.Initialize(); err != nil {
		t.Fatalf("config.Initialize: %v", err)
	}
	t.Cleanup(config.ResetForTesting)
	config.Set("issue-prefix", "task")

	sdb, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	mock.MatchExpectationsInOrder(false)
	// Not a unified database (no brain_unified_config), however often it is probed.
	for i := 0; i < 3; i++ {
		mock.ExpectQuery("select database\\(\\)").WillReturnRows(sqlmock.NewRows([]string{"d"}).AddRow("shared_db_mint_prefix_test"))
	}
	mock.ExpectQuery("information_schema.tables").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectQuery("SELECT value FROM config").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("lifespan"))

	got, err := NewConfigSQLRepository(sdb).GetConfig(context.Background(), "issue_prefix")
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if got != "task" {
		t.Fatalf("GetConfig(issue_prefix) = %q, want %q (db-wide lifespan must not win)", got, "task")
	}
}
