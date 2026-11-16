//go:build cgo

package embeddeddolt_test

import (
	"testing"

	"github.com/steveyegge/beads/internal/storage/depid"
	"github.com/steveyegge/beads/internal/types"
)

// TestGetDependencyRecordByIDEmbedded mirrors the dolt-suite edge-id read on
// the embedded backend: found in either dependency table with ID populated,
// nil for an unknown or empty id.
func TestGetDependencyRecordByIDEmbedded(t *testing.T) {
	skipUnlessEmbeddedDolt(t)

	te := newTestEnv(t, "bid")
	ctx := t.Context()

	for _, issue := range []*types.Issue{
		{ID: "bid-target", Title: "target", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask},
		{ID: "bid-durable", Title: "durable", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask},
		{ID: "bid-wisp", Title: "wisp", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask, Ephemeral: true},
	} {
		if err := te.store.CreateIssue(ctx, issue, "tester"); err != nil {
			t.Fatalf("CreateIssue %s: %v", issue.ID, err)
		}
	}
	for _, dep := range []*types.Dependency{
		{IssueID: "bid-durable", DependsOnID: "bid-target", Type: types.DepBlocks},
		{IssueID: "bid-wisp", DependsOnID: "bid-target", Type: types.DepRelated}, // wisp source -> wisp_dependencies
	} {
		if err := te.store.AddDependency(ctx, dep, "tester"); err != nil {
			t.Fatalf("AddDependency %s->%s: %v", dep.IssueID, dep.DependsOnID, err)
		}
	}

	for _, tc := range []struct {
		src string
		typ types.DependencyType
	}{
		{"bid-durable", types.DepBlocks},
		{"bid-wisp", types.DepRelated},
	} {
		id := depid.New(tc.src, "bid-target")
		got, err := te.store.GetDependencyRecordByID(ctx, id)
		if err != nil {
			t.Fatalf("GetDependencyRecordByID(%s): %v", id, err)
		}
		if got == nil || got.ID != id || got.IssueID != tc.src || got.DependsOnID != "bid-target" || got.Type != tc.typ {
			t.Errorf("GetDependencyRecordByID(%s) = %+v, want {ID:%s %s -> bid-target %s}", id, got, id, tc.src, tc.typ)
		}
	}

	for _, id := range []string{depid.New("bid-target", "bid-durable"), "", "not-an-edge"} {
		got, err := te.store.GetDependencyRecordByID(ctx, id)
		if err != nil {
			t.Fatalf("GetDependencyRecordByID(%q): %v", id, err)
		}
		if got != nil {
			t.Errorf("GetDependencyRecordByID(%q) = %+v, want nil", id, got)
		}
	}
}
