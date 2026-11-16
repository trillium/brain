package main

import (
	"errors"

	"github.com/steveyegge/beads/internal/storage"
)

// errNoRawSQLTx is what the brain and ISA verbs report when the store behind
// them has no raw-SQL transaction seam. Every in-tree store has one
// (storage.SQLTxRunner on the embedded and server stores, RawDBAccessor
// adapted for anything else), so reaching it means a store was wired up
// without either.
var errNoRawSQLTx = errors.New("store does not expose raw SQL transactions")

// sqlTxRunnerFor returns the raw-SQL transaction seam of the store behind st,
// unwrapping the hook decorators the CLI stacks on it. It replaces the
// RawDBAccessor assertion the ISA verbs used to make, which only the
// server-mode store satisfied and which failed every fresh embedded store
// with "store does not expose raw DB access".
func sqlTxRunnerFor(st storage.DoltStorage) (storage.SQLTxRunner, error) {
	runner, ok := storage.SQLTxRunnerFor(st)
	if !ok {
		return nil, errNoRawSQLTx
	}
	return runner, nil
}
