package issueops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/types"
)

// This file is the data half of store-aware edge classification: it finds the
// dependency rows stored as text in depends_on_external whose target this
// database actually holds, and turns them into real edges (see ProbeDepTarget
// for the classification rule and why the prefix comparison got it wrong).
//
// The rows exist because every cross-store edge used to be classified as
// foreign: on the unified database task-dena0's "blocks on brain-c56qg" carries
// prefix task, so it landed in depends_on_external, where nothing traverses it,
// nothing blocks on it, and `bd ready` happily lists a blocked bead as ready
// (docs/design/brain-single-database.md §Gaps, brain-e5tlj step 4). Fixing the
// classifier fixes new writes; these rows are the ones already written.
//
// Nothing here runs on its own: it is a deliberate one-time repair, planned by
// PlanExternalDepReclassify (read-only, the dry run) and applied by
// ReclassifyExternalDepsInTx inside one transaction. The store layer commits
// it as one Dolt commit so the whole repair is one `dolt revert` away.

// ErrDepReclassifyDirtyGraph reports that the external-target repair was asked
// to run while the tables its commit stages already carried uncommitted
// working-set changes.
var ErrDepReclassifyDirtyGraph = errors.New("external-target reclassification needs a clean working set")

// GuardDepReclassifyWorkingSet refuses the repair when its committable tables
// (issues, dependencies) already carry uncommitted working-set changes. The
// repair is deliberately ONE Dolt commit — that is what makes the whole change
// one `dolt revert` away — and staging edits that are not its own would break
// that promise (and roll a stranger's work into the revert). Run it as the
// first statement inside the repair's own transaction.
func GuardDepReclassifyWorkingSet(ctx context.Context, tx DBTX) error {
	dirty, err := dirtyGraphTables(ctx, tx)
	if err != nil {
		return fmt.Errorf("check working set before external-target reclassification: %w", err)
	}
	if len(dirty) == 0 {
		return nil
	}
	return fmt.Errorf("%w: commit or discard pending changes to %s first",
		ErrDepReclassifyDirtyGraph, strings.Join(dirty, ", "))
}

// DepReclassifyRow is one depends_on_external row the repair has an opinion
// about: which edge table it lives in, which row it is, and — for a row the
// repair touches — which typed column its target belongs in.
type DepReclassifyRow struct {
	Table      string // "dependencies" or "wisp_dependencies"
	DepID      string // the row's primary key (depid.New(issue_id, target))
	IssueID    string // the edge's source
	Target     string // the text target, as stored
	DepType    types.DependencyType
	TargetKind DepTargetKind // DepTargetIssue or DepTargetWisp
	// CoversType is the type of the real edge that already covers this pair,
	// for a row the repair collapses or leaves alone on a type conflict.
	CoversType types.DependencyType
}

// DepReclassifyPlan is one read-only pass over both edge tables: what the
// repair would make real, what it would collapse, and what it leaves alone.
type DepReclassifyPlan struct {
	// Reclassify are text rows whose target this database holds and whose pair
	// carries no real edge yet: the repair moves the target into its typed
	// column, which makes the edge traversable and blocking.
	Reclassify []DepReclassifyRow
	// Collapse are text rows whose pair already carries a real edge of the
	// same type in the same table: pure redundancy, so the repair removes the
	// text row rather than leave two ids for one edge (the 214-rows-into-213
	// collapse of brain-e5tlj step 4).
	Collapse []DepReclassifyRow
	// TypeConflicts are text rows whose pair already carries a real edge of a
	// DIFFERENT type. A pair carries one type (AddDependencyInTx refuses a
	// second), so moving the text target into the typed column would collide
	// with the edge that is already there. They are left exactly as they are
	// and reported, because choosing which type survives is the operator's
	// call, not the repair's.
	TypeConflicts []DepReclassifyRow
	// Explicit counts rows whose target carries the "external:" prefix: foreign
	// by construction, never touched.
	Explicit int
	// Unheld counts rows whose bare target this database holds nowhere.
	Unheld int
	// Scanned counts every external-target row read, including the ones above.
	Scanned int
}

// Touched reports how many rows the repair would write (moves plus collapses).
func (p DepReclassifyPlan) Touched() int { return len(p.Reclassify) + len(p.Collapse) }

// Reclassified counts the rows that become real edges.
func (p DepReclassifyPlan) Reclassified() int { return len(p.Reclassify) }

// edgeTableTargetColumn answers the three-column shape of one edge table, and
// whether the table exists at all (an older or partial schema may lack the wisp
// tables).
func edgeTableTargetColumn(ctx context.Context, q DBTX, table string) (bool, error) {
	var one int
	//nolint:gosec // G201: table is a hardcoded constant, never user input.
	err := q.QueryRowContext(ctx, "SELECT 1 FROM "+table+" LIMIT 1").Scan(&one)
	switch {
	case err == nil, errors.Is(err, sql.ErrNoRows):
		return true, nil
	case isTableNotExistError(err):
		return false, nil
	default:
		return false, fmt.Errorf("probing %s: %w", table, err)
	}
}

// PlanExternalDepReclassify reads both edge tables and answers, for every row
// whose target lives in depends_on_external, what the repair would do with it.
// It writes nothing, so it is both the dry run and the first half of the apply
// pass.
func PlanExternalDepReclassify(ctx context.Context, q DBTX) (DepReclassifyPlan, error) {
	var plan DepReclassifyPlan
	for _, table := range []string{"dependencies", "wisp_dependencies"} {
		if err := planExternalDepReclassifyTable(ctx, q, table, &plan); err != nil {
			return DepReclassifyPlan{}, err
		}
	}
	sortPlanRows(plan.Reclassify)
	sortPlanRows(plan.Collapse)
	sortPlanRows(plan.TypeConflicts)
	return plan, nil
}

// sortPlanRows orders rows deterministically (table, then primary key) so a
// dry run and its apply pass report the same sequence.
func sortPlanRows(rows []DepReclassifyRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Table != rows[j].Table {
			return rows[i].Table < rows[j].Table
		}
		return rows[i].DepID < rows[j].DepID
	})
}

func planExternalDepReclassifyTable(ctx context.Context, q DBTX, table string, plan *DepReclassifyPlan) error {
	exists, err := edgeTableTargetColumn(ctx, q, table)
	if err != nil || !exists {
		return err
	}

	//nolint:gosec // G201: table is a hardcoded constant, never user input.
	rows, err := q.QueryContext(ctx, `
		SELECT id, issue_id, depends_on_external, type
		FROM `+table+`
		WHERE depends_on_external IS NOT NULL AND depends_on_external <> ''
		ORDER BY id`)
	if err != nil {
		return fmt.Errorf("reading external dependency targets from %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	type rawRow struct {
		depID, issueID, target string
		depType                string
	}
	var raw []rawRow
	for rows.Next() {
		var r rawRow
		if err := rows.Scan(&r.depID, &r.issueID, &r.target, &r.depType); err != nil {
			return fmt.Errorf("scanning external dependency target in %s: %w", table, err)
		}
		raw = append(raw, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating external dependency targets in %s: %w", table, err)
	}
	_ = rows.Close()

	for _, r := range raw {
		plan.Scanned++
		row := DepReclassifyRow{
			Table:   table,
			DepID:   r.depID,
			IssueID: r.issueID,
			Target:  r.target,
			DepType: types.DependencyType(r.depType),
		}

		probe, err := ProbeDepTarget(ctx, q, r.issueID, r.target)
		if err != nil {
			return err
		}
		if !probe.Hold {
			// The "external:" form is foreign by construction; a bare id this
			// database holds nowhere is a target that is not here to link to.
			if strings.HasPrefix(r.target, "external:") {
				plan.Explicit++
			} else {
				plan.Unheld++
			}
			continue
		}
		row.TargetKind = probe.Kind

		// The pair may already carry a real edge in this table, written by the
		// same name-and-type add that also left this text row behind. Whether
		// the covering edge's type matches decides if the text row adds
		// anything: the same type is pure redundancy (collapse it), a different
		// type is a conflict the operator has to settle (leave it alone).
		covered, err := existingDepType(ctx, q, table, r.issueID, r.target)
		if err != nil {
			return err
		}
		if covered != "" {
			row.CoversType = covered
			if covered == row.DepType {
				plan.Collapse = append(plan.Collapse, row)
			} else {
				plan.TypeConflicts = append(plan.TypeConflicts, row)
			}
			continue
		}

		plan.Reclassify = append(plan.Reclassify, row)
	}
	return nil
}

// existingDepType answers the dependency type of the real edge covering
// (issueID, target) in table, if there is one.
func existingDepType(ctx context.Context, q DBTX, table, issueID, target string) (types.DependencyType, error) {
	var depType string
	//nolint:gosec // G201: table is a hardcoded constant, never user input.
	err := q.QueryRowContext(ctx,
		"SELECT type FROM "+table+" WHERE issue_id = ? AND COALESCE(depends_on_issue_id, depends_on_wisp_id) = ? ORDER BY type LIMIT 1",
		issueID, target).Scan(&depType)
	switch {
	case err == nil:
		return types.DependencyType(depType), nil
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	default:
		return "", fmt.Errorf("reading the covering edge type in %s: %w", table, err)
	}
}

// ReclassifyExternalDepsInTx applies the repair inside the caller's
// transaction: every row PlanExternalDepReclassify would reclassify is moved
// into its typed target column, every redundant text row is removed, and the
// is_blocked flags of everything the newly real edges can block are recomputed.
// It returns the plan it applied, so the caller can report exactly what changed.
//
// The caller owns the transaction and, on Dolt, the single commit (and the
// staging of the tables it touched: dependencies and issues). The function is
// idempotent — a second pass finds nothing to reclassify — so a retried
// transaction re-derives the same answer instead of double-applying.
func ReclassifyExternalDepsInTx(ctx context.Context, tx *sql.Tx) (DepReclassifyPlan, error) {
	plan, err := PlanExternalDepReclassify(ctx, tx)
	if err != nil {
		return DepReclassifyPlan{}, err
	}
	if plan.Touched() == 0 {
		return plan, nil
	}

	// Collect the graph rows the new edges can affect, so one recompute pass
	// repairs every source (and its descendants) the backfill newly blocks.
	var affectedIssues, affectedWisps []string
	collect := func(row DepReclassifyRow) error {
		if row.Table == "wisp_dependencies" {
			issues, wisps, err := AffectedByDepChangeForWispInTx(ctx, tx, row.IssueID, row.Target, row.DepType)
			if err != nil {
				return fmt.Errorf("affected set for %s -> %s: %w", row.IssueID, row.Target, err)
			}
			affectedIssues = append(affectedIssues, issues...)
			affectedWisps = append(affectedWisps, wisps...)
			return nil
		}
		issues, wisps, err := AffectedByDepChangeInTx(ctx, tx, row.IssueID, row.Target, row.DepType)
		if err != nil {
			return fmt.Errorf("affected set for %s -> %s: %w", row.IssueID, row.Target, err)
		}
		affectedIssues = append(affectedIssues, issues...)
		affectedWisps = append(affectedWisps, wisps...)
		return nil
	}

	for _, row := range plan.Reclassify {
		// Exactly one of the three target columns may be non-null
		// (ck_dep_one_target): the update moves the target in one statement so
		// the row never passes through a two-target state.
		//nolint:gosec // G201: table is a hardcoded constant; column comes from DepTargetKind.Column()
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf("UPDATE %s SET %s = ?, depends_on_external = NULL WHERE id = ?",
				row.Table, row.TargetKind.Column()),
			row.Target, row.DepID); err != nil {
			return DepReclassifyPlan{}, fmt.Errorf("reclassifying %s row %s (%s -> %s): %w",
				row.Table, row.DepID, row.IssueID, row.Target, err)
		}
		if err := collect(row); err != nil {
			return DepReclassifyPlan{}, err
		}
	}

	for _, row := range plan.Collapse {
		// A text row whose pair already carries a real edge of the same type:
		// removing it leaves the edge itself exactly as it was.
		//nolint:gosec // G201: table is a hardcoded constant, never user input.
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+row.Table+" WHERE id = ?", row.DepID); err != nil {
			return DepReclassifyPlan{}, fmt.Errorf("collapsing redundant %s row %s (%s -> %s): %w",
				row.Table, row.DepID, row.IssueID, row.Target, err)
		}
	}

	if err := RecomputeIsBlockedInTx(ctx, tx, dedupeIDs(affectedIssues), dedupeIDs(affectedWisps)); err != nil {
		return DepReclassifyPlan{}, fmt.Errorf("recompute is_blocked after reclassifying external targets: %w", err)
	}
	return plan, nil
}

// dedupeIDs removes duplicates while keeping the first-seen order, so the
// recompute pass carries each id once however many edges touched it.
func dedupeIDs(ids []string) []string {
	if len(ids) == 0 {
		return ids
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
