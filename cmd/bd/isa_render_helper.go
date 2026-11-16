package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
)

// renderISAByID is the shared render path used by:
//
//   - `bd isa-render <id>` and `bd isa-render-all` (the user-facing verbs)
//   - `bd patch <id> --field=...` (auto-render after a successful ISA-field patch)
//   - `bd isa-section <id> <section> ...` (auto-render after a section UPSERT)
//
// It loads the joined ISA row + sections via loadRenderRow inside one of the
// store's read transactions, then calls renderRowToDisk to perform
// resolve-path → render → atomic-write. The disk write happens after the
// transaction has been released: on the embedded store a transaction holds
// the engine, and the server store may re-run a read callback, so neither
// should wrap a filesystem side effect. Returns the resolved on-disk target
// path on success, or a non-nil error on any failure.
//
// Errors are returned untyped — callers decide whether a failure is fatal
// (the explicit `bd isa-render` verb) or warning-only (post-commit hooks on
// `bd patch` and `bd isa-section`, where the brain row is canonical and the
// markdown shadow is best-effort per ISC-40).
//
// st is the storage.DoltStorage the caller is holding; we reach its raw-SQL
// transaction seam through the same sqlTxRunnerFor path patch.go uses, so
// the embedded store (which exposes no *sql.DB) works like the server store.
func renderISAByID(ctx context.Context, st storage.DoltStorage, id string) (renderOutcome, error) {
	if st == nil {
		return renderOutcome{}, errors.New("renderISAByID: nil store")
	}
	runner, err := sqlTxRunnerFor(st)
	if err != nil {
		return renderOutcome{}, fmt.Errorf("renderISAByID: %w", err)
	}
	var row *renderRow
	err = runner.RunReadSQLTx(ctx, func(ctx context.Context, tx storage.SQLTx) error {
		var loadErr error
		row, loadErr = loadRenderRow(ctx, tx, id)
		return loadErr
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return renderOutcome{}, fmt.Errorf("isa not found: %s", id)
		}
		var wk *wrongKindError
		if errors.As(err, &wk) {
			return renderOutcome{}, fmt.Errorf("%s is not an ISA (kind=%s)", wk.id, wk.kind)
		}
		return renderOutcome{}, fmt.Errorf("reading isa: %w", err)
	}
	outcome, err := renderRowToDisk(row)
	if err != nil {
		return renderOutcome{}, fmt.Errorf("rendering %s: %w", id, err)
	}
	return outcome, nil
}
