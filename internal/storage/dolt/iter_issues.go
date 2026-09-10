// Package dolt — iter_issues.go
//
// Iterator over the issues table, hydrated in BOUNDED batches: each batch is
// read inside a SINGLE read transaction (one *sql.Conn) — scan the rows,
// close the cursor, batch-fetch labels with WHERE issue_id IN (...) — then
// the connection is released before the caller walks that batch.
//
// Why not a streaming cursor with per-row label hydration: server-mode Dolt
// pins each store to MaxOpenConns=1 because branch isolation is session-level
// (DOLT_CHECKOUT applies to the connection, see dolt_test.go setupTestStore).
// A streaming cursor holds that one connection for *sql.Rows, so hydrating
// labels per row needs a SECOND connection that can never be granted — the
// classic pool deadlock (mybd-2pcb). R5: the previous shape buffered the
// ENTIRE result set before returning; batching bounds memory to one batch
// (iterIssuesBatchSize) at the cost of cross-batch snapshot consistency —
// batches run in separate read transactions, so a concurrently-written row
// may appear twice or be skipped across a batch boundary. CLI iteration
// tolerates that; transactional readers must not use this iterator.
//
// This iterator queries only the `issues` table — wisp routing happens in
// the slice-returning SearchIssues which merges wisps and issues. Callers that
// need wisps stream IterWisps separately.
package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// IterIssues returns issues matching the filter from the `issues` table.
//
// The path queries only the issues table (wisps are returned separately via
// IterWisps). The slice path SearchIssues merges both for backward
// compatibility — that merge needs a seen-set keyed by ID across the full
// issues result set, so it stays separate from this issues-only iterator.
func (s *DoltStore) IterIssues(ctx context.Context, query string, filter types.IssueFilter) (storage.Iter[types.Issue], error) {
	if s.closed.Load() {
		return nil, ErrStoreClosed
	}
	whereClauses, args, err := issueops.BuildIssueFilterClauses(query, filter, issueops.IssuesFilterTables)
	if err != nil {
		return nil, fmt.Errorf("iter issues: build filter: %w", err)
	}
	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	return &batchIssueIter{
		store:    s,
		whereSQL: whereSQL,
		args:     args,
		limit:    filter.Limit,
		size:     iterIssuesBatchSize,
		idx:      -1,
	}, nil
}

// iterIssuesBatchSize bounds how many issues (plus their labels) one batch
// holds in memory. Batches run in separate read transactions; see the package
// comment on cross-batch snapshot consistency.
const iterIssuesBatchSize = 200

// batchIssueIter is a storage.Iter[types.Issue] that hydrates the issues
// table in bounded batches. Fetch errors are lazy: the first failing batch
// ends iteration and surfaces through Err. Filter-build errors stay eager
// (returned by IterIssues itself).
type batchIssueIter struct {
	store     *DoltStore
	whereSQL  string
	args      []any
	limit     int // filter.Limit; <=0 means unbounded
	size      int // rows per batch; IterIssues sets iterIssuesBatchSize, tests may shrink it
	offset    int
	yielded   int
	batch     []*types.Issue
	idx       int
	err       error
	closed    bool
	exhausted bool
}

func (it *batchIssueIter) Next(ctx context.Context) bool {
	if it.closed || it.err != nil || it.exhausted {
		return false
	}
	it.idx++
	for it.idx >= len(it.batch) {
		if !it.fetchBatch(ctx) {
			return false
		}
	}
	return true
}

func (it *batchIssueIter) Value() *types.Issue {
	if it.idx < 0 || it.idx >= len(it.batch) {
		return nil
	}
	return it.batch[it.idx]
}

func (it *batchIssueIter) Err() error { return it.err }

func (it *batchIssueIter) Close() error {
	it.closed = true
	it.batch = nil
	return nil
}

// fetchBatch loads the next bounded page (same single-tx scan-then-hydrate
// shape the unbounded implementation used) and resets the cursor onto it.
// It reports whether a non-empty page was loaded.
func (it *batchIssueIter) fetchBatch(ctx context.Context) bool {
	size := it.size
	if size <= 0 {
		size = iterIssuesBatchSize
	}
	if it.limit > 0 {
		if remaining := it.limit - it.yielded; remaining <= 0 {
			it.exhausted = true
			return false
		} else if remaining < size {
			size = remaining
		}
	}

	//nolint:gosec // G201: whereSQL contains column comparisons with ?, sizes are safe integers
	q := fmt.Sprintf(`SELECT %s FROM issues %s ORDER BY priority ASC, created_at DESC, id ASC LIMIT %d OFFSET %d`,
		issueops.IssueSelectColumns, it.whereSQL, size, it.offset)

	var issues []*types.Issue
	txErr := it.store.withReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, q, it.args...)
		if err != nil {
			return fmt.Errorf("iter issues: query: %w", err)
		}
		defer func() { _ = rows.Close() }()
		ids := make([]string, 0, size)
		for rows.Next() {
			iss, scanErr := issueops.ScanIssueFrom(rows)
			if scanErr != nil {
				return fmt.Errorf("iter issues: scan: %w", scanErr)
			}
			issues = append(issues, iss)
			ids = append(ids, iss.ID)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iter issues: rows: %w", err)
		}
		// A *sql.Tx is bound to one connection, so the cursor must be
		// closed before the label query can run on it.
		_ = rows.Close()
		labelMap, err := issueops.GetLabelsForIssuesFromTableInTx(ctx, tx, "labels", ids)
		if err != nil {
			return fmt.Errorf("iter issues: hydrate labels: %w", err)
		}
		for _, iss := range issues {
			if labels, ok := labelMap[iss.ID]; ok {
				iss.Labels = labels
			}
		}
		return nil
	})
	if txErr != nil {
		it.err = txErr
		return false
	}
	if len(issues) == 0 {
		it.exhausted = true
		return false
	}
	it.batch = issues
	it.idx = 0
	it.offset += len(issues)
	it.yielded += len(issues)
	return true
}
