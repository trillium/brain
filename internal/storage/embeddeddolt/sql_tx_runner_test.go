//go:build cgo

package embeddeddolt_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
	"github.com/steveyegge/beads/internal/types"
)

// TestEmbeddedStoreRunsRawSQLTransactions pins the seam the brain and ISA
// verbs (brain new, patch --field slug, isa-section, isa-show) reach a fresh
// embedded store through. Before it existed those verbs asserted
// storage.RawDBAccessor, which only the server-mode store satisfies, and
// failed every embedded store with "store does not expose raw DB access"
// (task-t57uw).
func TestEmbeddedStoreRunsRawSQLTransactions(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt tests")
	}

	ctx := t.Context()
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	store, err := embeddeddolt.Open(ctx, beadsDir, "sqltxdb", "main")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	runner, ok := storage.SQLTxRunnerFor(store)
	if !ok {
		t.Fatal("SQLTxRunnerFor(embedded store) = false, want the store's own SQLTxRunner")
	}
	if _, isRaw := storage.UnwrapStore(store).(storage.RawDBAccessor); isRaw {
		t.Fatal("embedded store now implements RawDBAccessor; this test assumes the SQLTxRunner seam is the only raw-SQL path")
	}

	if err := store.SetConfig(ctx, "issue_prefix", "sqltxdb"); err != nil {
		t.Fatalf("SetConfig issue_prefix: %v", err)
	}
	issue := &types.Issue{ID: "sqltxdb-isa-1", Title: "Slug round-trip", IssueType: types.IssueType("isa"), Priority: 2, Status: types.StatusOpen}
	if err := store.CreateIssue(ctx, issue, "test"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	// The write the verbs make: a column the typed surface does not model.
	err = runner.RunWriteSQLTx(ctx, func(ctx context.Context, tx storage.SQLTx) error {
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

	// A callback error rolls the write back, and the error reaches the caller
	// in a form errors.Is still recognizes (the verbs map sql.ErrNoRows to
	// "not found").
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

	// A read transaction is rolled back even when the callback writes.
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

	// The typed surface sees the raw write: the same connection path serves
	// both, so there is no second engine handle to go stale.
	got, err := store.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if got.Title != issue.Title {
		t.Fatalf("GetIssue title = %q, want %q", got.Title, issue.Title)
	}
}

// TestEmbeddedPreviewStoreRefusesRawSQLWrite keeps the read-only preview
// store's contract on the new seam: reads work, writes refuse with
// ErrReadOnly before any engine handle is opened.
func TestEmbeddedPreviewStoreRefusesRawSQLWrite(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt tests")
	}

	ctx := t.Context()
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	store, err := embeddeddolt.Open(ctx, beadsDir, "sqltxro", "main")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	preview, err := embeddeddolt.OpenForPreviewCommand(ctx, beadsDir, "sqltxro", "main")
	if err != nil {
		t.Fatalf("OpenForPreviewCommand: %v", err)
	}
	defer func() { _ = preview.Close() }()

	runner, ok := storage.SQLTxRunnerFor(preview)
	if !ok {
		t.Fatal("SQLTxRunnerFor(preview store) = false, want true")
	}
	var n int
	if err := runner.RunReadSQLTx(ctx, func(ctx context.Context, tx storage.SQLTx) error {
		return tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM issues").Scan(&n)
	}); err != nil {
		t.Fatalf("RunReadSQLTx on preview store: %v", err)
	}
	ran := false
	err = runner.RunWriteSQLTx(ctx, func(ctx context.Context, tx storage.SQLTx) error {
		ran = true
		return nil
	})
	if !errors.Is(err, embeddeddolt.ErrReadOnly) {
		t.Fatalf("RunWriteSQLTx on preview store = %v, want embeddeddolt.ErrReadOnly", err)
	}
	if ran {
		t.Fatal("RunWriteSQLTx on preview store ran the callback before refusing")
	}
}
