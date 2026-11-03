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

	ids, _, _, err := selectExpiredBookkeeping(ctx, store, days, now)
	if err != nil {
		t.Fatalf("selectExpiredBookkeeping: %v", err)
	}
	if want := []string{expired}; !slices.Equal(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}

	// A shorter retention also reaches the recent one; the others stay out.
	ids, _, _, err = selectExpiredBookkeeping(ctx, store, 1, now)
	if err != nil {
		t.Fatalf("selectExpiredBookkeeping(1): %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("retention 1 day: got %d ids, want 2 (both closed bookkeeping)", len(ids))
	}
}

// TestBookkeepingExpirySkipsIssuesWithOutsideDependents checks that an expired
// bookkeeping issue something else depends on is reported and left alone,
// along with the expired issues it depends on, while the rest still expire.
func TestBookkeepingExpirySkipsIssuesWithOutsideDependents(t *testing.T) {
	t.Parallel()
	store := newTestStore(t, filepath.Join(t.TempDir(), ".beads", "beads.db"))
	ctx := context.Background()
	now := time.Now()

	create := func(title string, closed bool, labels ...string) string {
		t.Helper()
		issue := &types.Issue{Title: title, Priority: 2, IssueType: types.TypeTask, Status: types.StatusOpen}
		if closed {
			closedAt := now.AddDate(0, 0, -45)
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
	depend := func(from, on string) {
		t.Helper()
		dep := &types.Dependency{IssueID: from, DependsOnID: on, Type: types.DepBlocks}
		if err := store.AddDependency(ctx, dep, "test-user"); err != nil {
			t.Fatalf("AddDependency: %v", err)
		}
	}

	free := create("free", true, "bookkeeping")
	linked := create("linked", true, "bookkeeping")
	upstream := create("upstream of linked", true, "bookkeeping")
	chainA := create("chain a", true, "bookkeeping")
	chainB := create("chain b", true, "bookkeeping")
	handWritten := create("hand-written open", false)
	depend(handWritten, linked)
	depend(linked, upstream)
	depend(chainB, chainA)

	ids, _, skipped, err := selectExpiredBookkeeping(ctx, store, 30, now)
	if err != nil {
		t.Fatalf("selectExpiredBookkeeping: %v", err)
	}
	slices.Sort(ids)
	want := []string{chainA, chainB, free}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
	got := map[string]bool{}
	for _, sk := range skipped {
		got[sk.ID] = true
		if sk.Reason == "" {
			t.Errorf("skip %s has no reason", sk.ID)
		}
	}
	if len(got) != 2 || !got[linked] || !got[upstream] {
		t.Errorf("skipped = %v, want %s and %s", skipped, linked, upstream)
	}
}
