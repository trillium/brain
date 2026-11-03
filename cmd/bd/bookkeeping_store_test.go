//go:build cgo

package main

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// TestBookkeepingExpiryAgainstStore runs the selector against a real store so
// the label, status and close-time filters are exercised end to end.
func TestBookkeepingExpiryAgainstStore(t *testing.T) {
	t.Parallel()
	store := newTestStore(t, filepath.Join(t.TempDir(), ".beads", "beads.db"))
	ctx := context.Background()
	now := time.Now()

	create := func(title string, closedDaysAgo int, labels ...string) string {
		t.Helper()
		issue := &types.Issue{Title: title, Priority: 2, IssueType: types.TypeTask, Status: types.StatusOpen}
		if closedDaysAgo > 0 {
			closedAt := now.AddDate(0, 0, -closedDaysAgo)
			issue.Status = types.StatusClosed
			issue.ClosedAt = &closedAt
		}
		if err := store.CreateIssue(ctx, issue, "test-user"); err != nil {
			t.Fatalf("create %q: %v", title, err)
		}
		for _, l := range labels {
			if err := store.AddLabel(ctx, issue.ID, l, "test-user"); err != nil {
				t.Fatalf("label %q: %v", title, err)
			}
		}
		return issue.ID
	}

	expired := create("old bookkeeping", 45, "bookkeeping")
	create("recent bookkeeping", 3, "bookkeeping")
	create("open bookkeeping", 0, "bookkeeping")
	create("old hand-written", 45, "decision")
	create("old unlabeled", 45)

	if err := store.SetConfig(ctx, bookkeepingRetentionKey, "30"); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	days, err := resolveBookkeepingRetentionDays(ctx, store, 0)
	if err != nil || days != 30 {
		t.Fatalf("retention = %d, %v; want 30", days, err)
	}

	ids, _, err := selectExpiredBookkeeping(ctx, store, days, now)
	if err != nil {
		t.Fatalf("selectExpiredBookkeeping: %v", err)
	}
	if want := []string{expired}; !slices.Equal(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}

	// A shorter retention also reaches the recent one; the others stay out.
	ids, _, err = selectExpiredBookkeeping(ctx, store, 1, now)
	if err != nil {
		t.Fatalf("selectExpiredBookkeeping(1): %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("retention 1 day: got %d ids, want 2 (both closed bookkeeping)", len(ids))
	}
}
