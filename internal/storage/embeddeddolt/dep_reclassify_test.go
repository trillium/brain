//go:build cgo

package embeddeddolt_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/steveyegge/beads/internal/storage/depid"
	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// These cover the store-aware edge classification and the one-commit repair on
// a real (embedded) Dolt engine: a cross-store target this database holds is a
// real, blocking edge, and the repair moves the text rows written before that
// rule existed. The unified database is server-mode only, so the namespace
// scoping half of the fix is covered in internal/storage/dolt.

// withRawSQL opens a raw connection to the store's database, hands it to fn and
// closes it again. One handle at a time is a hard requirement here: embedded
// Dolt holds its directory, so a raw connection left open blocks the store's own
// next call (the harness helpers open and close per statement for the same
// reason).
func withRawSQL(t *testing.T, te *testEnv, fn func(db *sql.DB)) {
	t.Helper()
	ctx := t.Context()
	db, cleanup, err := embeddeddolt.OpenSQL(ctx, te.dataDir, te.database, "main")
	if err != nil {
		t.Fatalf("OpenSQL: %v", err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Fatalf("close raw SQL connection: %v", err)
		}
	}()
	fn(db)
}

// seedEmbeddedIssue creates one open task with an explicit id.
func seedEmbeddedIssue(t *testing.T, ctx context.Context, te *testEnv, id string) {
	t.Helper()
	iss := &types.Issue{ID: id, Title: id, Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	if err := te.store.CreateIssue(ctx, iss, "tester"); err != nil {
		t.Fatalf("seed issue %s: %v", id, err)
	}
}

// seedExternalDepRow writes the pre-fix shape of a cross-store edge: a text
// target in depends_on_external, exactly as the old prefix comparison produced.
// rowID is the surrogate key, so a test can give a legacy row a non-deterministic
// id the way a pre-#4259 clone would have.
func seedExternalDepRow(t *testing.T, ctx context.Context, db *sql.DB, rowID, issueID, target string, depType types.DependencyType) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO dependencies (id, issue_id, depends_on_external, type, created_at, created_by, metadata)
		VALUES (?, ?, ?, ?, NOW(), 'tester', '{}')`, rowID, issueID, target, string(depType)); err != nil {
		t.Fatalf("seed external dependency row %s (%s -> %s): %v", rowID, issueID, target, err)
	}
}

// depRow is one dependency row's typed target columns.
type depRow struct {
	issueTarget    sql.NullString
	wispTarget     sql.NullString
	externalTarget sql.NullString
}

func readDepRow(t *testing.T, ctx context.Context, db *sql.DB, issueID, target string) depRow {
	t.Helper()
	var row depRow
	if err := db.QueryRowContext(ctx, `
		SELECT depends_on_issue_id, depends_on_wisp_id, depends_on_external
		  FROM dependencies
		 WHERE issue_id = ?
		   AND COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external) = ?`,
		issueID, target).Scan(&row.issueTarget, &row.wispTarget, &row.externalTarget); err != nil {
		t.Fatalf("read dependency row %s -> %s: %v", issueID, target, err)
	}
	return row
}

// readDepRowByID reads one dependency row by its primary key, which is the only
// unambiguous way to look at a row whose pair has more than one edge (an
// external text row beside a real edge of another type).
func readDepRowByID(t *testing.T, ctx context.Context, db *sql.DB, rowID string) depRow {
	t.Helper()
	var row depRow
	if err := db.QueryRowContext(ctx, `
		SELECT depends_on_issue_id, depends_on_wisp_id, depends_on_external
		  FROM dependencies WHERE id = ?`, rowID).Scan(&row.issueTarget, &row.wispTarget, &row.externalTarget); err != nil {
		t.Fatalf("read dependency row %s: %v", rowID, err)
	}
	return row
}

func readBlocked(t *testing.T, ctx context.Context, db *sql.DB, id string) bool {
	t.Helper()
	var blocked bool
	if err := db.QueryRowContext(ctx, "SELECT is_blocked FROM issues WHERE id = ?", id).Scan(&blocked); err != nil {
		t.Fatalf("read is_blocked of %s: %v", id, err)
	}
	return blocked
}

func countDepRowsWithID(t *testing.T, ctx context.Context, db *sql.DB, rowID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dependencies WHERE id = ?", rowID).Scan(&n); err != nil {
		t.Fatalf("count dependency row %s: %v", rowID, err)
	}
	return n
}

// TestEmbeddedAddDependencyCrossNamespaceHeldTargetIsRealEdge is the
// classification fix on a real engine: the target's prefix names the store that
// owns the bead, not the database that holds it, so an edge to another store's
// bead in the same database is a real, blocking edge.
func TestEmbeddedAddDependencyCrossNamespaceHeldTargetIsRealEdge(t *testing.T) {
	te := newTestEnv(t, "cls")
	ctx := t.Context()
	seedEmbeddedIssue(t, ctx, te, "cls-source")
	seedEmbeddedIssue(t, ctx, te, "other-target")

	dep := &types.Dependency{IssueID: "cls-source", DependsOnID: "other-target", Type: types.DepBlocks}
	if err := te.store.AddDependency(ctx, dep, "tester"); err != nil {
		t.Fatalf("AddDependency across namespaces: %v", err)
	}

	withRawSQL(t, te, func(db *sql.DB) {
		row := readDepRow(t, ctx, db, "cls-source", "other-target")
		if !row.issueTarget.Valid || row.issueTarget.String != "other-target" {
			t.Errorf("depends_on_issue_id = %#v, want the real edge", row.issueTarget)
		}
		if row.wispTarget.Valid || row.externalTarget.Valid {
			t.Errorf("wisp = %#v, external = %#v: want exactly one typed target column", row.wispTarget, row.externalTarget)
		}
		if !readBlocked(t, ctx, db, "cls-source") {
			t.Error("the cross-namespace blocker did not block its source")
		}
	})
}

// TestEmbeddedAddDependencyCrossNamespaceUnheldTargetStaysExternal keeps the
// deliberate tolerance: a target no store here has minted stays text.
func TestEmbeddedAddDependencyCrossNamespaceUnheldTargetStaysExternal(t *testing.T) {
	te := newTestEnv(t, "cpe")
	ctx := t.Context()
	seedEmbeddedIssue(t, ctx, te, "cpe-source")

	dep := &types.Dependency{IssueID: "cpe-source", DependsOnID: "other-xyz", Type: types.DepBlocks}
	if err := te.store.AddDependency(ctx, dep, "tester"); err != nil {
		t.Fatalf("AddDependency to an unminted store: %v", err)
	}

	withRawSQL(t, te, func(db *sql.DB) {
		row := readDepRow(t, ctx, db, "cpe-source", "other-xyz")
		if !row.externalTarget.Valid || row.issueTarget.Valid {
			t.Errorf("issue = %#v, external = %#v: want the text reference preserved", row.issueTarget, row.externalTarget)
		}
	})
}

// TestExternalDepReclassifyDryRunWritesNothing is the backfill's safety
// property: the plan pass reads both edge tables and touches nothing.
func TestExternalDepReclassifyDryRunWritesNothing(t *testing.T) {
	te := newTestEnv(t, "dry")
	ctx := t.Context()
	seedEmbeddedIssue(t, ctx, te, "dry-source")
	seedEmbeddedIssue(t, ctx, te, "other-target")

	withRawSQL(t, te, func(db *sql.DB) {
		rowID := depid.New("dry-source", "other-target")
		seedExternalDepRow(t, ctx, db, rowID, "dry-source", "other-target", types.DepBlocks)

		plan, err := issueops.PlanExternalDepReclassify(ctx, db)
		if err != nil {
			t.Fatalf("PlanExternalDepReclassify: %v", err)
		}
		if len(plan.Reclassify) != 1 || plan.Reclassify[0].Target != "other-target" {
			t.Fatalf("plan.Reclassify = %#v, want the one cross-store text row", plan.Reclassify)
		}
		if plan.Reclassify[0].TargetKind != issueops.DepTargetIssue {
			t.Errorf("TargetKind = %v, want the issue column", plan.Reclassify[0].TargetKind)
		}
		if plan.Touched() != 1 {
			t.Errorf("Touched() = %d, want 1", plan.Touched())
		}

		// Nothing written: the row is still a text reference and the source is
		// still unblocked.
		row := readDepRow(t, ctx, db, "dry-source", "other-target")
		if !row.externalTarget.Valid || row.issueTarget.Valid {
			t.Errorf("dry run changed the row: issue = %#v, external = %#v", row.issueTarget, row.externalTarget)
		}
		if readBlocked(t, ctx, db, "dry-source") {
			t.Error("dry run changed is_blocked")
		}
	})
}

// TestExternalDepReclassifyMovesCollapsesAndBlocks is the repair end to end: the
// held target becomes a real blocking edge, a redundant text row is collapsed,
// a foreign target is left alone, and a second pass finds nothing to do.
func TestExternalDepReclassifyMovesCollapsesAndBlocks(t *testing.T) {
	te := newTestEnv(t, "rc")
	ctx := t.Context()
	seedEmbeddedIssue(t, ctx, te, "rc-source")
	seedEmbeddedIssue(t, ctx, te, "other-target")
	seedEmbeddedIssue(t, ctx, te, "rc-blocked")
	seedEmbeddedIssue(t, ctx, te, "rc-conflict")

	// 2. Real edges the pairs of the redundant and conflicting text rows below
	//    already carry. Their legacy surrogate keys are what a pre-#4259 clone
	//    would have written, which is why a text row can coexist with them.
	for _, real := range []*types.Dependency{
		{IssueID: "rc-blocked", DependsOnID: "other-target", Type: types.DepBlocks},
		{IssueID: "rc-conflict", DependsOnID: "other-target", Type: types.DepRelated},
	} {
		if err := te.store.AddDependency(ctx, real, "tester"); err != nil {
			t.Fatalf("seed real edge %s -> %s: %v", real.IssueID, real.DependsOnID, err)
		}
	}

	redundantID := "legacy-redundant-row"
	withRawSQL(t, te, func(db *sql.DB) {
		// 1. A cross-store text edge this database can make real.
		seedExternalDepRow(t, ctx, db, depid.New("rc-source", "other-target"), "rc-source", "other-target", types.DepBlocks)

		// 2. The redundant text row for the pair that already carries a real
		//    same-type edge.
		seedExternalDepRow(t, ctx, db, redundantID, "rc-blocked", "other-target", types.DepBlocks)

		// 3. A text row whose pair carries a real edge of a DIFFERENT type: a
		//    pair carries one type, so which one survives is the operator's
		//    call and the repair leaves it alone.
		conflictID := "legacy-conflict-row"
		seedExternalDepRow(t, ctx, db, conflictID, "rc-conflict", "other-target", types.DepBlocks)

		// 4. A target this database does not hold, and an explicit foreign
		//    target: both stay exactly as they are.
		seedExternalDepRow(t, ctx, db, depid.New("rc-source", "other-ghost"), "rc-source", "other-ghost", types.DepRelated)
		seedExternalDepRow(t, ctx, db, depid.New("rc-source", "external:linear/TEAM-1"), "rc-source", "external:linear/TEAM-1", types.DepRelated)

		plan := applyReclassify(t, ctx, db)
		if plan.Reclassified() != 1 {
			t.Errorf("Reclassified() = %d, want 1", plan.Reclassified())
		}
		if len(plan.Collapse) != 1 || plan.Collapse[0].DepID != redundantID {
			t.Errorf("Collapse = %#v, want the redundant legacy row", plan.Collapse)
		}
		if plan.Unheld != 1 || plan.Explicit != 1 {
			t.Errorf("Unheld = %d, Explicit = %d, want 1 each", plan.Unheld, plan.Explicit)
		}
		if len(plan.TypeConflicts) != 1 || plan.TypeConflicts[0].CoversType != types.DepRelated {
			t.Errorf("TypeConflicts = %#v, want the blocks-vs-related pair left alone", plan.TypeConflicts)
		}

		moved := readDepRow(t, ctx, db, "rc-source", "other-target")
		if !moved.issueTarget.Valid || moved.externalTarget.Valid {
			t.Errorf("moved row = (issue %#v, external %#v), want the typed column only", moved.issueTarget, moved.externalTarget)
		}
		if !readBlocked(t, ctx, db, "rc-source") {
			t.Error("the repaired edge does not block its source")
		}
		if n := countDepRowsWithID(t, ctx, db, redundantID); n != 0 {
			t.Errorf("redundant text row still present (%d row(s))", n)
		}

		if n := countDepRowsWithID(t, ctx, db, conflictID); n != 1 {
			t.Errorf("conflicting text row was removed (%d row(s))", n)
		}
		conflict := readDepRowByID(t, ctx, db, conflictID)
		if !conflict.externalTarget.Valid || conflict.externalTarget.String != "other-target" {
			t.Errorf("conflicting text row was rewritten: %#v", conflict.externalTarget)
		}
		if conflict.issueTarget.Valid {
			t.Errorf("conflicting text row gained a typed target: %#v", conflict.issueTarget)
		}

		foreign := readDepRow(t, ctx, db, "rc-source", "other-ghost")
		if !foreign.externalTarget.Valid || foreign.externalTarget.String != "other-ghost" {
			t.Errorf("unheld target was rewritten: %#v", foreign.externalTarget)
		}
		explicit := readDepRow(t, ctx, db, "rc-source", "external:linear/TEAM-1")
		if !explicit.externalTarget.Valid {
			t.Errorf("explicit external target was rewritten: %#v", explicit.externalTarget)
		}

		// Idempotent: a second pass has nothing left to do.
		second, err := issueops.PlanExternalDepReclassify(ctx, db)
		if err != nil {
			t.Fatalf("second PlanExternalDepReclassify: %v", err)
		}
		if second.Touched() != 0 {
			t.Errorf("second pass would touch %d row(s), want 0", second.Touched())
		}
	})
}

// applyReclassify runs the repair in one transaction, the way the CLI does.
func applyReclassify(t *testing.T, ctx context.Context, db *sql.DB) issueops.DepReclassifyPlan {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin repair tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	plan, err := issueops.ReclassifyExternalDepsInTx(ctx, tx)
	if err != nil {
		t.Fatalf("ReclassifyExternalDepsInTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit repair tx: %v", err)
	}
	return plan
}

// TestExternalDepReclassifyRefusesDirtyWorkingSet pins the guard that keeps the
// repair's one-commit promise honest: uncommitted edits to the graph tables are
// a refusal, not something the repair sweeps into its own commit.
func TestExternalDepReclassifyRefusesDirtyWorkingSet(t *testing.T) {
	te := newTestEnv(t, "grd")
	ctx := t.Context()
	seedEmbeddedIssue(t, ctx, te, "grd-source")
	seedEmbeddedIssue(t, ctx, te, "other-target")

	withRawSQL(t, te, func(db *sql.DB) {
		if _, err := db.ExecContext(ctx, "UPDATE issues SET title = 'uncommitted edit' WHERE id = 'grd-source'"); err != nil {
			t.Fatalf("dirty the working set: %v", err)
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer func() { _ = tx.Rollback() }()

		err = issueops.GuardDepReclassifyWorkingSet(ctx, tx)
		if err == nil {
			t.Fatal("dirty working set was accepted, want a refusal")
		}
		if !errors.Is(err, issueops.ErrDepReclassifyDirtyGraph) {
			t.Fatalf("err = %v, want ErrDepReclassifyDirtyGraph", err)
		}
	})
}
