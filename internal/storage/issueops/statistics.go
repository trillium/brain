package issueops

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/types"
)

// ScanIssueCountsInTx populates the count fields (TotalIssues, OpenIssues,
// InProgressIssues, ClosedIssues, DeferredIssues, PinnedIssues) of stats from
// the issues table. It does NOT compute BlockedIssues or ReadyIssues — callers
// fill those in using their own blocked-ID computation strategy.
func ScanIssueCountsInTx(ctx context.Context, tx DBTX, stats *types.Statistics) error {
	if err := tx.QueryRowContext(ctx, `
		SELECT
			COUNT(*) AS total,
			COALESCE(SUM(CASE WHEN status = 'open' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'in_progress' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'closed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'deferred' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN pinned = 1 THEN 1 ELSE 0 END), 0)
		FROM issues
	`).Scan(
		&stats.TotalIssues,
		&stats.OpenIssues,
		&stats.InProgressIssues,
		&stats.ClosedIssues,
		&stats.DeferredIssues,
		&stats.PinnedIssues,
	); err != nil {
		return fmt.Errorf("scan issue counts: %w", err)
	}
	return nil
}

// ScanIssueCountsScopedInTx is ScanIssueCountsInTx with optional namespace
// scoping for the unified database: the listed namespaces' `id LIKE
// '<prefix>-%'` clauses are ANDed into the count. Nil/empty prefixes means
// unscoped (the legacy single-namespace behaviour).
func ScanIssueCountsScopedInTx(ctx context.Context, tx DBTX, stats *types.Statistics, namespaces []string) error {
	where := ""
	var args []any
	if len(namespaces) > 0 {
		ors := make([]string, 0, len(namespaces))
		for _, ns := range namespaces {
			ors = append(ors, "id LIKE ?")
			args = append(args, strings.TrimSuffix(ns, "-")+"-%")
		}
		where = " WHERE (" + strings.Join(ors, " OR ") + ")"
	}
	//nolint:gosec // G201: fixed fragments and ? placeholders only
	query := `
		SELECT
			COUNT(*) AS total,
			COALESCE(SUM(CASE WHEN status = 'open' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'in_progress' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'closed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'deferred' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN pinned = 1 THEN 1 ELSE 0 END), 0)
		FROM issues` + where
	if err := tx.QueryRowContext(ctx, query, args...).Scan(
		&stats.TotalIssues,
		&stats.OpenIssues,
		&stats.InProgressIssues,
		&stats.ClosedIssues,
		&stats.DeferredIssues,
		&stats.PinnedIssues,
	); err != nil {
		return fmt.Errorf("scan issue counts: %w", err)
	}
	return nil
}
