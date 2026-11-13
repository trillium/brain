package github

import (
	"context"
	"errors"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// trackedStore is a storage.Storage that knows only which item ids exist. Every
// other method panics through the nil embedded interface, which is the point:
// the tracker code under test must not reach for anything else.
type trackedStore struct {
	storage.Storage
	ids map[string]bool
}

func (trackedStore) GetIssueByExternalRef(context.Context, string) (*types.Issue, error) {
	return nil, errors.New("not found")
}

func (s trackedStore) GetIssue(_ context.Context, id string) (*types.Issue, error) {
	if s.ids[id] {
		return &types.Issue{ID: id}, nil
	}
	return nil, errors.New("not found")
}

// SearchIssues answers only an id lookup, the one search the tracker makes
// to ask whether an item exists.
func (s trackedStore) SearchIssues(_ context.Context, _ string, filter types.IssueFilter) ([]*types.Issue, error) {
	var out []*types.Issue
	for _, id := range filter.IDs {
		if s.ids[id] {
			out = append(out, &types.Issue{ID: id})
		}
	}
	return out, nil
}
