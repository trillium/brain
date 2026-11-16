//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"

	"github.com/steveyegge/beads/internal/storage"
)

var _ storage.SQLTxRunner = (*EmbeddedDoltStore)(nil)

// RunReadSQLTx runs fn in a short-lived read transaction and rolls it back.
// This is how the brain and ISA verbs read raw columns on an embedded store:
// the store opens one engine handle per transaction, so there is no stable
// *sql.DB to hand out and RawDBAccessor is deliberately not implemented here
// (a second handle opened while one is live blocks on the database lock).
func (s *EmbeddedDoltStore) RunReadSQLTx(ctx context.Context, fn func(ctx context.Context, tx storage.SQLTx) error) error {
	return s.withConn(ctx, false, func(tx *sql.Tx) error {
		return fn(ctx, tx)
	})
}

// RunWriteSQLTx runs fn in a short-lived write transaction and commits it
// when fn returns nil. A read-only store refuses with ErrReadOnly before any
// handle is opened, the same as every other write.
func (s *EmbeddedDoltStore) RunWriteSQLTx(ctx context.Context, fn func(ctx context.Context, tx storage.SQLTx) error) error {
	return s.withConn(ctx, true, func(tx *sql.Tx) error {
		return fn(ctx, tx)
	})
}
