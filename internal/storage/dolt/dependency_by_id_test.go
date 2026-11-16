package dolt

import (
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/depid"
	"github.com/steveyegge/beads/internal/types"
)

// TestGetDependencyRecordByID covers the edge-id read behind
// `bd dep remove <edge-id>`: an edge is found by the deterministic id it was
// stored under, in either dependency table, with its endpoints and ID
// populated; an unknown or empty id is nil, not an error.
func TestGetDependencyRecordByID(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	mk := func(id string, wisp bool) *types.Issue {
		i := &types.Issue{ID: id, Title: id, Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask, Ephemeral: wisp}
		if err := store.CreateIssue(ctx, i, "tester"); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		return i
	}
	target := mk("bid-target", false)
	durable := mk("bid-durable", false)
	wisp := mk("bid-wisp", true)
	for _, d := range []*types.Dependency{
		{IssueID: durable.ID, DependsOnID: target.ID, Type: types.DepBlocks},
		{IssueID: wisp.ID, DependsOnID: target.ID, Type: types.DepRelated}, // wisp source -> wisp_dependencies
	} {
		if err := store.AddDependency(ctx, d, "tester"); err != nil {
			t.Fatalf("add dep %s -> %s: %v", d.IssueID, d.DependsOnID, err)
		}
	}

	for _, tc := range []struct {
		name string
		src  string
		typ  types.DependencyType
	}{
		{"durable table", durable.ID, types.DepBlocks},
		{"wisp table", wisp.ID, types.DepRelated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := depid.New(tc.src, target.ID)
			got, err := store.GetDependencyRecordByID(ctx, id)
			if err != nil {
				t.Fatalf("GetDependencyRecordByID(%s): %v", id, err)
			}
			if got == nil {
				t.Fatalf("GetDependencyRecordByID(%s) = nil, want the %s -> %s edge", id, tc.src, target.ID)
			}
			if got.ID != id || got.IssueID != tc.src || got.DependsOnID != target.ID || got.Type != tc.typ {
				t.Errorf("GetDependencyRecordByID(%s) = {ID:%s %s -> %s %s}, want {ID:%s %s -> %s %s}",
					id, got.ID, got.IssueID, got.DependsOnID, got.Type, id, tc.src, target.ID, tc.typ)
			}
		})
	}

	// The id is directional: the reverse pair names a different, absent edge.
	for _, id := range []string{depid.New(target.ID, durable.ID), "", "not-an-edge"} {
		got, err := store.GetDependencyRecordByID(ctx, id)
		if err != nil {
			t.Fatalf("GetDependencyRecordByID(%q): %v", id, err)
		}
		if got != nil {
			t.Errorf("GetDependencyRecordByID(%q) = %+v, want nil", id, got)
		}
	}

	// The transaction role reads the same row.
	id := depid.New(durable.ID, target.ID)
	err := store.RunInTransaction(ctx, "test: read dependency by id", func(tx storage.Transaction) error {
		inTx, err := tx.GetDependencyRecordByID(ctx, id)
		if err != nil {
			return err
		}
		if inTx == nil || inTx.ID != id {
			t.Errorf("tx GetDependencyRecordByID(%s) = %+v, want the durable edge", id, inTx)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx GetDependencyRecordByID: %v", err)
	}
}
