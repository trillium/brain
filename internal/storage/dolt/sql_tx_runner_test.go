package dolt

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// TestServerStoreRunsRawSQLTransactions pins the server-mode half of the
// seam the brain and ISA verbs use (see embeddeddolt's twin). The server
// store still exposes RawDBAccessor for the other raw-SQL callers, but the
// ISA verbs must reach the SQLTxRunner it implements itself, so both stores
// run the same callback code.
func TestServerStoreRunsRawSQLTransactions(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	runner, ok := storage.SQLTxRunnerFor(store)
	if !ok {
		t.Fatal("SQLTxRunnerFor(server store) = false, want true")
	}
	if _, native := runner.(*DoltStore); !native {
		t.Fatalf("SQLTxRunnerFor(server store) = %T, want the store itself rather than the RawDBAccessor fallback", runner)
	}

	issue := &types.Issue{ID: "sqltx-isa-1", Title: "Slug round-trip", IssueType: types.IssueType("isa"), Priority: 2, Status: types.StatusOpen}
	if err := store.CreateIssue(ctx, issue, "test"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	err := runner.RunWriteSQLTx(ctx, func(ctx context.Context, tx storage.SQLTx) error {
		_, err := tx.ExecContext(ctx, "UPDATE issues SET slug = ? WHERE id = ?", "slug-round-trip", issue.ID)
		return err
	})
	if err != nil {
		t.Fatalf("RunWriteSQLTx: %v", err)
	}

	readSlug := func() string {
		t.Helper()
		var slug sql.NullString
		err := runner.RunReadSQLTx(ctx, func(ctx context.Context, tx storage.SQLTx) error {
			return tx.QueryRowContext(ctx, "SELECT slug FROM issues WHERE id = ?", issue.ID).Scan(&slug)
		})
		if err != nil {
			t.Fatalf("RunReadSQLTx: %v", err)
		}
		return slug.String
	}
	if got := readSlug(); got != "slug-round-trip" {
		t.Fatalf("slug after RunWriteSQLTx = %q, want %q", got, "slug-round-trip")
	}

	sentinel := errors.New("abort this write")
	err = runner.RunWriteSQLTx(ctx, func(ctx context.Context, tx storage.SQLTx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE issues SET slug = ? WHERE id = ?", "never-committed", issue.ID); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("RunWriteSQLTx with failing callback = %v, want an error wrapping the callback's", err)
	}
	if got := readSlug(); got != "slug-round-trip" {
		t.Fatalf("slug after rolled-back write = %q, want %q", got, "slug-round-trip")
	}

	err = runner.RunReadSQLTx(ctx, func(ctx context.Context, tx storage.SQLTx) error {
		var id string
		return tx.QueryRowContext(ctx, "SELECT id FROM issues WHERE id = ?", "no-such-row").Scan(&id)
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("RunReadSQLTx scanning a missing row = %v, want sql.ErrNoRows", err)
	}

	err = runner.RunReadSQLTx(ctx, func(ctx context.Context, tx storage.SQLTx) error {
		_, err := tx.ExecContext(ctx, "UPDATE issues SET slug = ? WHERE id = ?", "read-tx-leak", issue.ID)
		return err
	})
	if err != nil {
		t.Fatalf("RunReadSQLTx with a write: %v", err)
	}
	if got := readSlug(); got != "slug-round-trip" {
		t.Fatalf("slug after a write inside RunReadSQLTx = %q, want %q (read transactions roll back)", got, "slug-round-trip")
	}
}
