package dolt

import (
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

func TestGetDependenciesWithMetadata_ExternalEdges(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	// Create an issue
	issue := &types.Issue{
		ID:        "bd-main",
		Title:     "Main Issue",
		Status:    types.StatusOpen,
		Priority:  1,
		IssueType: types.TypeTask,
	}
	if err := store.CreateIssue(ctx, issue, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}

	// Add a related edge to an ID that does not exist in this store. This
	// mirrors `dep add` across named stores, which persists its target in
	// depends_on_external rather than creating a local issue row.
	const externalID = "crossstore-reader-target-9lp"
	externalDep := &types.Dependency{
		IssueID:     issue.ID,
		DependsOnID: externalID,
		Type:        "related",
	}
	if err := store.AddDependency(ctx, externalDep, "tester"); err != nil {
		t.Fatalf("failed to add external dependency: %v", err)
	}

	// Get dependencies with metadata - should include the external dependency
	deps, err := store.GetDependenciesWithMetadata(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetDependenciesWithMetadata failed: %v", err)
	}

	// Should have 1 dependency (the external one)
	if len(deps) != 1 {
		t.Errorf("expected 1 dependency, got %d", len(deps))
	}

	// Verify the external dependency is present with correct ID
	if len(deps) > 0 {
		if deps[0].ID != externalID {
			t.Errorf("expected dependency ID %q, got %q", externalID, deps[0].ID)
		}
		if deps[0].DependencyType != "related" {
			t.Errorf("expected dependency type 'related', got %q", deps[0].DependencyType)
		}
		if !deps[0].Unresolved || deps[0].Title != types.UnresolvedDependencyTitle {
			t.Errorf("expected unresolved target titled %q, got unresolved=%v title=%q",
				types.UnresolvedDependencyTitle, deps[0].Unresolved, deps[0].Title)
		}
	}
}

// Legacy cross-store rows store "external:<store>:<id>". With one shared
// issues table the bare <id> is a real bead, so the reader must return it.
func TestGetDependenciesWithMetadata_LegacyExternalForm(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	for _, iss := range []*types.Issue{
		{ID: "bd-src", Title: "Source", Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask},
		{ID: "bd-real", Title: "Real Target", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask},
	} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("failed to create issue %s: %v", iss.ID, err)
		}
	}
	for _, target := range []string{"external:otherstore:bd-real", "external:otherstore:bd-gone"} {
		dep := &types.Dependency{IssueID: "bd-src", DependsOnID: target, Type: "related"}
		if err := store.AddDependency(ctx, dep, "tester"); err != nil {
			t.Fatalf("failed to add dependency %s: %v", target, err)
		}
	}

	deps, err := store.GetDependenciesWithMetadata(ctx, "bd-src")
	if err != nil {
		t.Fatalf("GetDependenciesWithMetadata failed: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("expected 2 dependencies, got %d", len(deps))
	}
	byID := map[string]*types.IssueWithDependencyMetadata{}
	for _, d := range deps {
		byID[d.ID] = d
	}
	real := byID["bd-real"]
	if real == nil || real.Unresolved || real.Title != "Real Target" || real.Priority != 2 {
		t.Errorf("external:otherstore:bd-real should resolve to the real bead, got %+v", real)
	}
	gone := byID["external:otherstore:bd-gone"]
	if gone == nil || !gone.Unresolved || gone.Title != types.UnresolvedDependencyTitle {
		t.Errorf("missing target should stay visible as unresolved, got %+v", gone)
	}
}

func TestGetDependentsWithMetadata_ExternalEdges(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	// Create an issue
	issue := &types.Issue{
		ID:        "bd-target",
		Title:     "Target Issue",
		Status:    types.StatusOpen,
		Priority:  1,
		IssueType: types.TypeTask,
	}
	if err := store.CreateIssue(ctx, issue, "tester"); err != nil {
		t.Fatalf("failed to create issue: %v", err)
	}

	// AddDependency requires the source issue to exist (every store shares one
	// issues table), so a dependent from another store is an ordinary row.
	other := &types.Issue{
		ID:        "resume_bullets-9lp",
		Title:     "Dependent From Another Store",
		Status:    types.StatusOpen,
		Priority:  1,
		IssueType: types.TypeTask,
	}
	if err := store.CreateIssue(ctx, other, "tester"); err != nil {
		t.Fatalf("failed to create dependent issue: %v", err)
	}
	externalDependent := &types.Dependency{
		IssueID:     other.ID,
		DependsOnID: issue.ID,
		Type:        "blocks",
	}
	if err := store.AddDependency(ctx, externalDependent, "tester"); err != nil {
		t.Fatalf("failed to add external dependent: %v", err)
	}

	// Get dependents with metadata - should include the external dependent
	dependents, err := store.GetDependentsWithMetadata(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetDependentsWithMetadata failed: %v", err)
	}

	// Should have 1 dependent (the external one)
	if len(dependents) != 1 {
		t.Errorf("expected 1 dependent, got %d", len(dependents))
	}

	// Verify the external dependent is present with correct ID
	if len(dependents) > 0 {
		if dependents[0].ID != "resume_bullets-9lp" {
			t.Errorf("expected dependent ID 'resume_bullets-9lp', got %q", dependents[0].ID)
		}
		if dependents[0].DependencyType != "blocks" {
			t.Errorf("expected dependency type 'blocks', got %q", dependents[0].DependencyType)
		}
	}
}
