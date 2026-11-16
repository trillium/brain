package storage

import (
	"context"
	"database/sql"
)

// SQLTx is the slice of *sql.Tx a raw-SQL caller needs. It is the same shape
// as issueops.DBTX, restated here because issueops imports this package.
type SQLTx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// SQLTxRunner runs raw SQL inside one of the store's own transactions. It is
// the seam for the brain and ISA verbs (brain new, patch, isa-section,
// isa-show, isa-list, isa-by-slug, isa-render*) that write and read columns
// the typed DoltStorage surface does not model (issues.slug, the isa_*
// columns, isa_sections).
//
// Unlike RawDBAccessor — which hands out a long-lived *sql.DB that only the
// server-mode store has — this works on the embedded store too, which opens
// one connection per transaction and exposes no stable *sql.DB. Every store
// that wants these verbs implements this interface; callers reach it through
// SQLTxRunnerFor, which unwraps the hook decorators and falls back to a
// RawDBAccessor-only store, so the two seams never diverge in behavior.
//
// RunReadSQLTx always rolls the transaction back. The server-mode store may
// re-run fn on a transient connection error, so fn must assign its results,
// never accumulate them.
//
// RunWriteSQLTx commits when fn returns nil and rolls back otherwise. It is
// one SQL transaction with no conflict retry, matching the autocommit
// statements it replaces; the CLI's post-run Dolt commit is unchanged.
type SQLTxRunner interface {
	RunReadSQLTx(ctx context.Context, fn func(ctx context.Context, tx SQLTx) error) error
	RunWriteSQLTx(ctx context.Context, fn func(ctx context.Context, tx SQLTx) error) error
}

// SQLTxRunnerFor returns the raw-SQL transaction seam of the concrete store
// behind s, reaching through any Unwrapper decorator chain. A store that
// implements SQLTxRunner is preferred; a store that only exposes
// RawDBAccessor is adapted so a registered backend that predates this seam
// keeps working. The second result is false when s is nil or exposes neither.
func SQLTxRunnerFor(s DoltStorage) (SQLTxRunner, bool) {
	if s == nil {
		return nil, false
	}
	inner := UnwrapStore(s)
	if inner == nil {
		return nil, false
	}
	if runner, ok := inner.(SQLTxRunner); ok {
		return runner, true
	}
	if accessor, ok := inner.(RawDBAccessor); ok {
		if db := accessor.DB(); db != nil {
			return rawDBSQLTxRunner{db: db}, true
		}
	}
	return nil, false
}

// rawDBSQLTxRunner adapts a RawDBAccessor's *sql.DB to SQLTxRunner with plain
// BeginTx/Commit/Rollback.
type rawDBSQLTxRunner struct {
	db *sql.DB
}

func (r rawDBSQLTxRunner) RunReadSQLTx(ctx context.Context, fn func(ctx context.Context, tx SQLTx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return fn(ctx, tx)
}

func (r rawDBSQLTxRunner) RunWriteSQLTx(ctx context.Context, fn func(ctx context.Context, tx SQLTx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(ctx, tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
