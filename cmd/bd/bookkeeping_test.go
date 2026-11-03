package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

type fakeBookkeepingConfig map[string]string

func (f fakeBookkeepingConfig) GetConfig(_ context.Context, key string) (string, error) {
	return f[key], nil
}

// fakeBookkeepingStore returns every issue it holds regardless of the filter,
// so the tests prove the selector's own guards, not just the storage filter.
type fakeBookkeepingStore struct {
	issues []*types.Issue
	err    error
}

func (f fakeBookkeepingStore) SearchIssues(context.Context, string, types.IssueFilter) ([]*types.Issue, error) {
	return f.issues, f.err
}

func TestResolveBookkeepingRetentionDays(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		name    string
		cfg     map[string]string
		flag    int
		want    int
		wantErr bool
	}{
		{name: "default when unset", want: defaultBookkeepingRetentionDays},
		{name: "config value", cfg: map[string]string{bookkeepingRetentionKey: "7"}, want: 7},
		{name: "config value is trimmed", cfg: map[string]string{bookkeepingRetentionKey: " 14 "}, want: 14},
		{name: "flag beats config", cfg: map[string]string{bookkeepingRetentionKey: "7"}, flag: 3, want: 3},
		{name: "non-numeric config is an error", cfg: map[string]string{bookkeepingRetentionKey: "soon"}, wantErr: true},
		{name: "zero config is an error", cfg: map[string]string{bookkeepingRetentionKey: "0"}, wantErr: true},
		{name: "negative config is an error", cfg: map[string]string{bookkeepingRetentionKey: "-5"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveBookkeepingRetentionDays(ctx, fakeBookkeepingConfig(tt.cfg), tt.flag)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("days = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSelectExpiredBookkeeping(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	ago := func(days int) *time.Time {
		ts := now.AddDate(0, 0, -days)
		return &ts
	}
	mk := func(id string, status types.Status, closedAgo *time.Time, pinned bool, labels ...string) *types.Issue {
		return &types.Issue{ID: id, Status: status, ClosedAt: closedAgo, Pinned: pinned, Labels: labels}
	}
	store := fakeBookkeepingStore{issues: []*types.Issue{
		mk("old-closed", types.StatusClosed, ago(40), false, "bookkeeping"),
		mk("old-closed-multi-label", types.StatusClosed, ago(31), false, "other", "bookkeeping"),
		mk("recent-closed", types.StatusClosed, ago(5), false, "bookkeeping"),
		mk("exactly-at-cutoff", types.StatusClosed, ago(30), false, "bookkeeping"),
		mk("open-old", types.StatusOpen, nil, false, "bookkeeping"),
		mk("in-progress", types.StatusInProgress, nil, false, "bookkeeping"),
		mk("hand-written", types.StatusClosed, ago(400), false, "decision"),
		mk("unlabeled", types.StatusClosed, ago(400), false),
		mk("lookalike-label", types.StatusClosed, ago(400), false, "bookkeeping-notes"),
		mk("no-close-time", types.StatusClosed, nil, false, "bookkeeping"),
		mk("pinned", types.StatusClosed, ago(90), true, "bookkeeping"),
	}}

	ids, pinned, err := selectExpiredBookkeeping(context.Background(), store, 30, now)
	if err != nil {
		t.Fatalf("selectExpiredBookkeeping: %v", err)
	}
	want := []string{"old-closed", "old-closed-multi-label"}
	if !slices.Equal(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
	if pinned != 1 {
		t.Errorf("pinned = %d, want 1", pinned)
	}
}

func TestSelectExpiredBookkeepingRetentionWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	closedAt := now.AddDate(0, 0, -10)
	store := fakeBookkeepingStore{issues: []*types.Issue{
		{ID: "ten-days", Status: types.StatusClosed, ClosedAt: &closedAt, Labels: []string{"bookkeeping"}},
	}}
	for days, wantCount := range map[int]int{7: 1, 10: 0, 11: 0, 30: 0} {
		ids, _, err := selectExpiredBookkeeping(context.Background(), store, days, now)
		if err != nil {
			t.Fatalf("retention %d: %v", days, err)
		}
		if len(ids) != wantCount {
			t.Errorf("retention %d days: got %v, want %d id(s)", days, ids, wantCount)
		}
	}
}

func TestSelectExpiredBookkeepingSearchError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	_, _, err := selectExpiredBookkeeping(context.Background(), fakeBookkeepingStore{err: boom}, 30, time.Now())
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want wrapping %v", err, boom)
	}
}
