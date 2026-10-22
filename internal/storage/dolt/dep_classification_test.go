// Package dolt: store-aware edge classification and global id reads.
//
// These cover the two halves of the fix for cross-store edges on the unified
// database: a cross-store target this database holds is a real edge (not text
// in depends_on_external), and a read addressed by id resolves any store's bead
// while a listing stays narrow. Both are behavior the pre-fix code got wrong:
// AddDependencyInTx classified by comparing id prefixes, and every read path
// applied the wrapper's namespace scope — including the by-id fast path in
// ResolvePartialID, which is why `task show brain-c56qg` answered "no issue
// found" on a database that held the bead.
package dolt

import (
	"context"
	"database/sql"
	"testing"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// seedIssue creates one open task with an explicit id.
func seedIssue(ctx context.Context, t *testing.T, store *DoltStore, id string) {
	t.Helper()
	iss := &types.Issue{ID: id, Title: id, Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
		t.Fatalf("seed issue %s: %v", id, err)
	}
}

// depTargetColumns answers which typed target column a dependency row uses.
func depTargetColumns(ctx context.Context, t *testing.T, db *sql.DB, issueID, target string) (issueCol, wispCol, externalCol sql.NullString) {
	t.Helper()
	err := db.QueryRowContext(ctx,
		`SELECT depends_on_issue_id, depends_on_wisp_id, depends_on_external
		   FROM dependencies WHERE issue_id = ? AND COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external) = ?`,
		issueID, target).Scan(&issueCol, &wispCol, &externalCol)
	if err != nil {
		t.Fatalf("read target columns for %s -> %s: %v", issueID, target, err)
	}
	return issueCol, wispCol, externalCol
}

// TestAddDependencyCrossNamespaceHeldTargetIsRealEdge is the classification
// fix: under namespace model A the target's prefix says which store owns the
// bead, not which database holds it, so a target this database holds must land
// in depends_on_issue_id. Before the fix the prefix comparison filed every
// cross-store edge as foreign text.
func TestAddDependencyCrossNamespaceHeldTargetIsRealEdge(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	seedIssue(ctx, t, store, "task-source")
	seedIssue(ctx, t, store, "brain-target")

	dep := &types.Dependency{IssueID: "task-source", DependsOnID: "brain-target", Type: types.DepBlocks}
	if err := store.AddDependency(ctx, dep, "tester"); err != nil {
		t.Fatalf("AddDependency across stores: %v", err)
	}

	issueCol, wispCol, externalCol := depTargetColumns(ctx, t, store.db, "task-source", "brain-target")
	if !issueCol.Valid || issueCol.String != "brain-target" {
		t.Errorf("depends_on_issue_id = %#v, want the real edge (a cross-store target this database holds)", issueCol)
	}
	if wispCol.Valid || externalCol.Valid {
		t.Errorf("depends_on_wisp_id = %#v, depends_on_external = %#v: want exactly one typed target column", wispCol, externalCol)
	}

	// A real edge blocks; a text reference never did.
	if !isBlocked(ctx, t, store.db, "task-source") {
		t.Error("task-source is not blocked by its cross-store blocker, so the edge is still not traversed")
	}

	deps, err := store.GetDependenciesWithMetadata(ctx, "task-source")
	if err != nil {
		t.Fatalf("GetDependenciesWithMetadata: %v", err)
	}
	if len(deps) != 1 || deps[0].ID != "brain-target" {
		t.Fatalf("dependencies = %#v, want the resolved cross-store target", deps)
	}
}

// TestAddDependencyCrossNamespaceUnheldTargetStaysExternal is the tolerance the
// fix deliberately keeps: a target no store has minted in this database stays
// the text reference depends_on_external is for, instead of becoming a refusal.
func TestAddDependencyCrossNamespaceUnheldTargetStaysExternal(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	seedIssue(ctx, t, store, "task-source")

	dep := &types.Dependency{IssueID: "task-source", DependsOnID: "other-xyz", Type: types.DepBlocks}
	if err := store.AddDependency(ctx, dep, "tester"); err != nil {
		t.Fatalf("AddDependency to a not-yet-minted store: %v", err)
	}

	issueCol, _, externalCol := depTargetColumns(ctx, t, store.db, "task-source", "other-xyz")
	if !externalCol.Valid || externalCol.String != "other-xyz" || issueCol.Valid {
		t.Errorf("columns = (issue %#v, external %#v), want the text reference preserved", issueCol, externalCol)
	}
	if isBlocked(ctx, t, store.db, "task-source") {
		t.Error("an unheld target blocked its source: a text reference carries no local status to derive from")
	}
}

// TestAddDependencyOwnNamespaceMissingTargetRefused pins the other half of the
// rule: a name in the source's own store namespace that this database does not
// hold is an error, not a silent text edge.
func TestAddDependencyOwnNamespaceMissingTargetRefused(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	seedIssue(ctx, t, store, "task-source")

	dep := &types.Dependency{IssueID: "task-source", DependsOnID: "task-ghost", Type: types.DepBlocks}
	err := store.AddDependency(ctx, dep, "tester")
	if err == nil {
		t.Fatal("AddDependency to a missing same-store id succeeded, want a not-found refusal")
	}
}

// TestProbeDepTargetMatrix pins the classification answer itself, so the rule
// is visible in one place: what the database holds decides the column, the
// prefix decides only what an unheld target means.
func TestProbeDepTargetMatrix(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	seedIssue(ctx, t, store, "task-source")
	seedIssue(ctx, t, store, "task-peer")
	seedIssue(ctx, t, store, "brain-target")

	cases := []struct {
		name          string
		target        string
		wantKind      issueops.DepTargetKind
		wantHold      bool
		wantSameNS    bool
		wantTable     string
		wantIssueType bool
	}{
		{"held issue in another namespace", "brain-target", issueops.DepTargetIssue, true, false, "issues", true},
		{"held issue in the same namespace", "task-peer", issueops.DepTargetIssue, true, false, "issues", true},
		{"unheld id in another namespace", "brain-ghost", issueops.DepTargetExternal, false, false, "", false},
		{"unheld id in the same namespace", "task-ghost", issueops.DepTargetIssue, false, true, "", false},
		{"explicit external form", "external:linear/TEAM-1", issueops.DepTargetExternal, false, false, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe, err := issueops.ProbeDepTarget(ctx, store.db, "task-source", tc.target)
			if err != nil {
				t.Fatalf("ProbeDepTarget: %v", err)
			}
			if probe.Kind != tc.wantKind {
				t.Errorf("Kind = %v, want %v", probe.Kind, tc.wantKind)
			}
			if probe.Hold != tc.wantHold {
				t.Errorf("Hold = %v, want %v", probe.Hold, tc.wantHold)
			}
			if probe.SameNamespace != tc.wantSameNS {
				t.Errorf("SameNamespace = %v, want %v", probe.SameNamespace, tc.wantSameNS)
			}
			if probe.Table != tc.wantTable {
				t.Errorf("Table = %q, want %q", probe.Table, tc.wantTable)
			}
			if (probe.IssueType != "") != tc.wantIssueType {
				t.Errorf("IssueType = %q, want non-empty = %v", probe.IssueType, tc.wantIssueType)
			}
		})
	}
}

// unifiedNamespaceFixture makes a throwaway database answer the unified-database
// probe the way the real one does: a brain_unified_config table (the probe's
// marker) plus a brain_store_prefixes record for the wrapper's namespace. No
// live store is touched — setupConcurrentTestStore's database is local, fresh
// and uniquely named.
func unifiedNamespaceFixture(ctx context.Context, t *testing.T, store *DoltStore, storeName string, prefixes ...string) {
	t.Helper()
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS brain_unified_config (k varchar(64) NOT NULL, v text, PRIMARY KEY (k))`,
		`CREATE TABLE IF NOT EXISTS brain_store_prefixes (prefix varchar(255) NOT NULL, store varchar(128) NOT NULL, PRIMARY KEY (prefix))`,
	}
	for _, stmt := range stmts {
		if _, err := store.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("provision unified fixture: %v", err)
		}
	}
	for _, prefix := range prefixes {
		if _, err := store.db.ExecContext(ctx,
			`INSERT INTO brain_store_prefixes (prefix, store) VALUES (?, ?)`, prefix, storeName); err != nil {
			t.Fatalf("record prefix %s: %v", prefix, err)
		}
	}
}

// TestIdAddressedReadResolvesForeignNamespace is the global-id-reads fix: an
// id-addressed read resolves any store's bead, while a listing stays narrow.
// Before the fix the by-id fast path in ResolvePartialID (SearchIssues with an
// IDs filter) was namespace-scoped like a listing, so it reported "no issue
// found" for a bead the database plainly held.
func TestIdAddressedReadResolvesForeignNamespace(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	seedIssue(ctx, t, store, "task-aaa")
	seedIssue(ctx, t, store, "brain-bbb")
	unifiedNamespaceFixture(ctx, t, store, "task", "task")

	t.Setenv("BD_NAME", "task")
	issueops.ResetUnifiedScopeCacheForTest()
	SetNamespaceReadScope(NamespaceReadNarrow)
	t.Cleanup(func() {
		resetNamespaceReadScopeForTest()
		issueops.ResetUnifiedScopeCacheForTest()
	})

	// The listing stays narrow: the wrapper sees its own namespace only.
	listed, err := store.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		t.Fatalf("SearchIssues (listing): %v", err)
	}
	for _, iss := range listed {
		if iss.ID == "brain-bbb" {
			t.Fatalf("a narrow listing returned %s, which the task namespace does not own", iss.ID)
		}
	}
	if len(listed) != 1 || listed[0].ID != "task-aaa" {
		t.Fatalf("narrow listing = %#v, want exactly task-aaa", listed)
	}

	// The id-addressed read resolves the foreign bead (this is what
	// ResolvePartialID's exact-id fast path does).
	byID, err := store.SearchIssues(ctx, "", types.IssueFilter{IDs: []string{"brain-bbb"}})
	if err != nil {
		t.Fatalf("SearchIssues (by id): %v", err)
	}
	if len(byID) != 1 || byID[0].ID != "brain-bbb" {
		t.Fatalf("id-addressed read = %#v, want brain-bbb: an id-addressed read resolves any id", byID)
	}

	// A partial-id search is id-addressed too, and cannot name the id up front,
	// so it carries IDLookup.
	partial, err := store.SearchIssues(ctx, "bbb", types.IssueFilter{IDLookup: true})
	if err != nil {
		t.Fatalf("SearchIssues (partial id): %v", err)
	}
	if len(partial) != 1 || partial[0].ID != "brain-bbb" {
		t.Fatalf("partial-id read = %#v, want brain-bbb", partial)
	}
}
