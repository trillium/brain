package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

const (
	// bookkeepingLabel marks an issue as procedural bookkeeping (session
	// trails, guard decisions, state-change records) as opposed to a
	// deliberately written one. Only issues carrying it are ever expired.
	bookkeepingLabel = "bookkeeping"

	// bookkeepingRetentionKey is the config key holding how many days a
	// closed bookkeeping issue is kept before it may be removed.
	bookkeepingRetentionKey = "bookkeeping.retention_days"

	// defaultBookkeepingRetentionDays applies when the config key is unset.
	defaultBookkeepingRetentionDays = 30
)

// bookkeepingConfigReader is the slice of the store needed to resolve retention.
type bookkeepingConfigReader interface {
	GetConfig(ctx context.Context, key string) (string, error)
}

// bookkeepingSearcher is the slice of the store needed to find expired issues.
type bookkeepingSearcher interface {
	SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error)
}

// resolveBookkeepingRetentionDays returns the retention in days. An explicit
// flag value (>0) wins, then the config key, then the default. A configured
// value that is not a positive integer is an error rather than silently
// falling back, so a typo cannot shorten or lengthen retention unnoticed.
func resolveBookkeepingRetentionDays(ctx context.Context, cfg bookkeepingConfigReader, flagDays int) (int, error) {
	if flagDays > 0 {
		return flagDays, nil
	}
	raw, err := cfg.GetConfig(ctx, bookkeepingRetentionKey)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", bookkeepingRetentionKey, err)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultBookkeepingRetentionDays, nil
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days <= 0 {
		return 0, fmt.Errorf("%s must be a positive whole number of days, got %q", bookkeepingRetentionKey, raw)
	}
	return days, nil
}

// selectExpiredBookkeeping returns the ids of closed bookkeeping issues whose
// close time is more than retentionDays before now. Open issues, issues
// without the bookkeeping label, and pinned issues are never returned.
// pinned reports how many otherwise-expired issues were skipped for being pinned.
func selectExpiredBookkeeping(ctx context.Context, s bookkeepingSearcher, retentionDays int, now time.Time) (ids []string, pinned int, err error) {
	closed := types.StatusClosed
	cutoff := now.AddDate(0, 0, -retentionDays)
	found, err := s.SearchIssues(ctx, "", types.IssueFilter{
		Status:       &closed,
		Labels:       []string{bookkeepingLabel},
		ClosedBefore: &cutoff,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("listing bookkeeping issues: %w", err)
	}
	for _, issue := range found {
		// Re-check every guard here so the safety property does not depend
		// on the storage filter alone.
		if issue.Status != types.StatusClosed || !slices.Contains(issue.Labels, bookkeepingLabel) {
			continue
		}
		if issue.ClosedAt == nil || !issue.ClosedAt.Before(cutoff) {
			continue
		}
		if issue.Pinned {
			pinned++
			continue
		}
		ids = append(ids, issue.ID)
	}
	return ids, pinned, nil
}
