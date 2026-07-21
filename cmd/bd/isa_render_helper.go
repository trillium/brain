package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
)

// renderISAByID is the shared post-write render path used by:
//
//   - `bd isa-render <id>` (the user-facing verb)
//   - `bd patch <id> --field=...` (auto-render after a successful ISA-field patch)
//   - `bd isa-section <id> <section> ...` (auto-render after a section UPSERT)
//
// It loads the joined ISA row + sections via the existing loadRenderRow path,
// then calls renderRowToDisk to perform resolve-path → render → atomic-write.
// Returns the resolved on-disk target path on success, or a non-nil error on
// any failure.
//
// Errors are returned untyped — callers decide whether a failure is fatal
// (the explicit `bd isa-render` verb) or warning-only (post-commit hooks on
// `bd patch` and `bd isa-section`, where the brain row is canonical and the
// markdown shadow is best-effort per ISC-40).
//
// st is the storage.DoltStorage the caller is holding; we unwrap it for raw
// *sql.DB access via the same RawDBAccessor path patch.go uses. Keeping the
// helper store-aware (rather than db-aware) means the hook call sites don't
// need to thread an *sql.DB around themselves.
func renderISAByID(ctx context.Context, st storage.DoltStorage, id string) (renderOutcome, error) {
	if st == nil {
		return renderOutcome{}, errors.New("renderISAByID: nil store")
	}
	accessor, ok := storage.UnwrapStore(st).(storage.RawDBAccessor)
	if !ok {
		return renderOutcome{}, errors.New("renderISAByID: store does not expose raw DB access")
	}
	db := accessor.DB()
	if db == nil {
		return renderOutcome{}, errors.New("renderISAByID: store DB is nil")
	}
	return renderISAByIDWithDB(ctx, db, id)
}

// renderISAByIDWithDB is the *sql.DB-flavored helper used by both
// renderISAByID and the cobra command runISARender (which opens its own
// read DB via openReadDB). Splitting the two lets us share the load+render
// pipeline without forcing the verb to thread the global store through.
func renderISAByIDWithDB(ctx context.Context, db *sql.DB, id string) (renderOutcome, error) {
	if db == nil {
		return renderOutcome{}, errors.New("renderISAByIDWithDB: nil db")
	}
	row, err := loadRenderRow(ctx, db, id)
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
