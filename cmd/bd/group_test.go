package main

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// fakeGroupReader serves canned groups and members.
type fakeGroupReader struct {
	byMember map[string][]*types.Issue // member id -> groups tracking it
	byGroup  map[string][]groupMember  // group id -> tracked members
}

func (f fakeGroupReader) trackingGroups(_ context.Context, id string) ([]*types.Issue, error) {
	return f.byMember[id], nil
}

func (f fakeGroupReader) members(_ context.Context, groupID string) ([]groupMember, error) {
	return f.byGroup[groupID], nil
}

func TestGroupsReadyToClose(t *testing.T) {
	g := &types.Issue{ID: "g-1", Title: "launch"}
	closed := string(types.StatusClosed)
	open := string(types.StatusOpen)

	cases := []struct {
		name    string
		members []groupMember
		want    bool
	}{
		{"all closed", []groupMember{{ID: "a", Status: closed}, {ID: "b", Status: closed}}, true},
		{"one still open", []groupMember{{ID: "a", Status: closed}, {ID: "b", Status: open}}, false},
		{"empty group never closes", nil, false},
		{"unresolved member counts as not done", []groupMember{{ID: "a", Status: closed}, {ID: "x", Status: closed, Unresolved: true}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := fakeGroupReader{
				byMember: map[string][]*types.Issue{"a": {g}},
				byGroup:  map[string][]groupMember{"g-1": tc.members},
			}
			ready, err := groupsReadyToClose(context.Background(), r, "a")
			if err != nil {
				t.Fatal(err)
			}
			if got := len(ready) == 1; got != tc.want {
				t.Fatalf("ready = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGroupProgressCounts(t *testing.T) {
	r := fakeGroupReader{byGroup: map[string][]groupMember{"g-1": {
		{ID: "a", Status: "closed"}, {ID: "b", Status: "open"}, {ID: "c", Status: "in_progress"},
	}}}
	p, err := computeGroupProgress(context.Background(), r, &types.Issue{ID: "g-1"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 3 || p.Done != 1 || p.Complete() {
		t.Fatalf("got total=%d done=%d complete=%v", p.Total, p.Done, p.Complete())
	}
}
