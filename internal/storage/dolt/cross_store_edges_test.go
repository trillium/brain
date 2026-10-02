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

	// Add a dependency where an external issue depends on our issue
	externalDependent := &types.Dependency{
		IssueID:     "resume_bullets-9lp",
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
