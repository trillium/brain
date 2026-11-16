package dolt

import (
	"context"
	"database/sql"

	"github.com/steveyegge/beads/internal/storage"
)

var _ storage.SQLTxRunner = (*DoltStore)(nil)

// RunReadSQLTx runs fn in a read transaction that is always rolled back. It
// goes through withReadTx so the store's closed check, read lock, and
// transient-connection retry apply; fn may therefore run more than once.
func (s *DoltStore) RunReadSQLTx(ctx context.Context, fn func(ctx context.Context, tx storage.SQLTx) error) error {
	return s.withReadTx(ctx, func(tx *sql.Tx) error {
		return fn(ctx, tx)
	})
}

// RunWriteSQLTx runs fn in one write transaction and commits it when fn
// returns nil. Like RunEventsMaintenanceTx it uses withWriteTx rather than
// withRetryTx: the autocommit statements this replaces had no conflict retry
// either, and a single run keeps the callback contract simple.
func (s *DoltStore) RunWriteSQLTx(ctx context.Context, fn func(ctx context.Context, tx storage.SQLTx) error) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		return fn(ctx, tx)
	})
}
