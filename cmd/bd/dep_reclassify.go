// Package main: `bd dep reclassify` — the deliberate, one-commit repair that
// turns dependency rows stored as text in depends_on_external into real edges
// when the database actually holds their target.
//
// The repair exists because edge classification used to compare id prefixes:
// on the unified database a cross-store target (task-dena0 -> brain-c56qg)
// looked foreign, so the edge landed in depends_on_external, where nothing
// traverses it and nothing blocks (docs/design/brain-single-database.md). The
// classifier is fixed at the source (issueops.ProbeDepTarget); this command
// repairs the rows written before that fix, and is a no-op afterwards.
//
// It never runs as a side effect: without --apply it is read-only (the dry
// run), and with --apply the whole repair lands as ONE Dolt commit that
// stages only the edge table and `issues`, so the change is one
// `dolt revert` away. It refuses when those tables already carry uncommitted
// work, so the revert cannot take a stranger's edits with it.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/ui"
)

var depReclassifyCmd = &cobra.Command{
	Use:   "reclassify",
	Short: "Turn external text dependency links into real edges where the target lives here",
	Long: `Find dependency rows whose target is stored as text in depends_on_external
and make the ones this database actually holds into real edges.

An edge whose target this database holds belongs in depends_on_issue_id (or
depends_on_wisp_id). A text row is invisible to every graph reader: nothing
traverses it, nothing blocks on it, and 'bd ready' lists a blocked bead as
ready. On the unified database every store's beads share one issues table, so
most text rows are this case — a cross-store edge filed as foreign because the
two ids carried different prefixes (see docs/design/brain-single-database.md,
and issueops.ProbeDepTarget for the rule that replaced the prefix comparison).

What this command does with each text row:
  - target this database holds  → move it into its typed column (a real edge);
  - target of a pair that already carries a real edge of the same type
                               → remove the redundant text row (one edge, one id);
  - target of a pair that already carries a real edge of a DIFFERENT type
                               → left alone and reported: a pair carries one
                                 type, so which one survives is your call;
  - target this database holds nowhere, and the "external:..." form
                               → left exactly as it is: depends_on_external is
                                 for genuinely foreign targets.

Without --apply nothing is written: the command prints the plan, every row it
would touch, and the exact command that would apply it. With --apply the whole
repair is one Dolt commit (staging only the edge table and issues), so it is
one 'dolt revert' away.`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		apply, _ := cmd.Flags().GetBool("apply")
		return runDepReclassify(apply)
	},
}

func runDepReclassify(apply bool) error {
	ctx := rootCtx
	db, err := depReclassifyDB()
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	if !apply {
		plan, err := depReclassifyScan(ctx, db)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		if jsonOutput {
			outputJSON(depReclassifyJSON(plan, false, ""))
			return nil
		}
		reportDepReclassifyPlan(plan, false)
		return nil
	}

	plan, commit, err := depReclassifyApply(ctx, db)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	if jsonOutput {
		outputJSON(depReclassifyJSON(plan, true, commit))
		return nil
	}
	reportDepReclassifyPlan(plan, true)
	if plan.Touched() == 0 {
		return nil
	}
	fmt.Printf("\n%s %d row(s) repaired in one Dolt commit%s\n",
		ui.RenderPass("✓"), plan.Touched(), commitSuffix(commit))
	if commit != "" {
		fmt.Printf("  revert it with:  bd dolt revert %s\n", shortCommit(commit))
	}
	return nil
}

// commitSuffix renders the commit hash the repair landed as, when there is
// one (the hash is reporting only: the repair is committed either way).
func commitSuffix(commit string) string {
	if commit == "" {
		return ""
	}
	return " (" + shortCommit(commit) + ")"
}

// depReclassifyDB answers the raw Dolt connection behind the connected store,
// exactly as the other namespace/maintenance verbs do (bd sql, bd
// store-prefix): one accessor seam, so the repair follows the store wherever
// its database lives and reuses the credentials bd already resolved. Nothing
// here reads or writes a hardcoded host.
func depReclassifyDB() (*sql.DB, error) {
	if store == nil {
		return nil, fmt.Errorf("no database connection available (%s)", diagHint())
	}
	accessor, ok := storage.UnwrapStore(store).(storage.RawDBAccessor)
	if !ok {
		return nil, fmt.Errorf("the connected store does not expose a raw Dolt connection; open the database through a server-mode wrapper (BEADS_DOLT_SERVER_MODE=1)")
	}
	db := accessor.UnderlyingDB()
	if db == nil {
		return nil, fmt.Errorf("the connected store's raw Dolt connection is unavailable")
	}
	return db, nil
}

// depReclassifyScan is the dry run: one read-only pass over both edge tables.
func depReclassifyScan(ctx context.Context, db *sql.DB) (issueops.DepReclassifyPlan, error) {
	return issueops.PlanExternalDepReclassify(ctx, db)
}

// depReclassifyApply performs the repair as one commit. The transaction, the
// staging and the commit share one session on purpose: DOLT_ADD and
// DOLT_COMMIT are session state, so splitting them would stage one connection's
// work and commit another's.
func depReclassifyApply(ctx context.Context, db *sql.DB) (issueops.DepReclassifyPlan, string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return issueops.DepReclassifyPlan{}, "", fmt.Errorf("begin repair transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := issueops.GuardDepReclassifyWorkingSet(ctx, tx); err != nil {
		return issueops.DepReclassifyPlan{}, "", err
	}

	plan, err := issueops.ReclassifyExternalDepsInTx(ctx, tx)
	if err != nil {
		return issueops.DepReclassifyPlan{}, "", err
	}
	if plan.Touched() == 0 {
		return plan, "", nil
	}

	// Stage only what the repair changed: the edge table carrying the moved
	// rows, and issues for the is_blocked recompute. The wisp edge table is
	// dolt-ignored and never staged.
	for _, table := range []string{"dependencies", "issues"} {
		if _, err := tx.ExecContext(ctx, "CALL DOLT_ADD(?)", table); err != nil {
			return issueops.DepReclassifyPlan{}, "", fmt.Errorf("dolt add %s: %w", table, err)
		}
	}
	message := fmt.Sprintf("bd: reclassify %d external dependency target(s) into real edges", plan.Reclassified())
	author := fmt.Sprintf("%s <%s@beads.local>", getActor(), "dep-reclassify")
	if _, err := tx.ExecContext(ctx, "CALL DOLT_COMMIT('-m', ?, '--author', ?)", message, author); err != nil && !issueops.IsNothingToCommitError(err) {
		// Nothing to commit is benign: a repair confined to wisp_dependencies
		// (a dolt-ignored table) has no versioned change of its own, and a
		// blocked-state recompute can legitimately flip nothing.
		return issueops.DepReclassifyPlan{}, "", fmt.Errorf("dolt commit: %w", err)
	}

	// The revert point: HEAD after the repair's own commit. Reporting only —
	// the repair is committed either way, so a failure here is not an error.
	var commit string
	if err := tx.QueryRowContext(ctx, "SELECT HASHOF('HEAD')").Scan(&commit); err != nil {
		commit = ""
	}
	if err := tx.Commit(); err != nil {
		return issueops.DepReclassifyPlan{}, "", fmt.Errorf("commit repair transaction: %w", err)
	}
	return plan, commit, nil
}

// reportDepReclassifyPlan prints the plan. applied distinguishes the dry run
// from a repair that has already happened, because the same words would
// otherwise read as a promise the second time.
func reportDepReclassifyPlan(plan issueops.DepReclassifyPlan, applied bool) {
	verb := "would reclassify"
	if applied {
		verb = "reclassified"
	} else {
		fmt.Printf("%d row(s) store their dependency target as text in depends_on_external.\n\n", plan.Scanned)
	}
	issues, wisps := 0, 0
	for _, row := range plan.Reclassify {
		if row.TargetKind == issueops.DepTargetWisp {
			wisps++
		} else {
			issues++
		}
	}
	fmt.Printf("  %5d  %s into real edges (%d issue target(s), %d wisp target(s))\n",
		len(plan.Reclassify), verb, issues, wisps)
	fmt.Printf("  %5d  %s (the pair already carries a same-type edge of its own)\n",
		len(plan.Collapse), collapseWord(applied))
	fmt.Printf("  %5d  left alone (the pair's real edge carries a different type)\n", len(plan.TypeConflicts))
	fmt.Printf("  %5d  left alone (no such bead in this database; %d explicit \"external:\" target(s))\n",
		plan.Unheld, plan.Explicit)

	for _, row := range plan.Reclassify {
		fmt.Printf("      %s: %s -> %s (%s)\n", row.Table, row.IssueID, row.Target, row.DepType)
	}
	for _, row := range plan.Collapse {
		fmt.Printf("      %s: %s -> %s (redundant %s, real edge carries %s)\n",
			row.Table, row.IssueID, row.Target, row.DepType, row.CoversType)
	}
	for _, row := range plan.TypeConflicts {
		fmt.Printf("      %s: %s -> %s (text row says %s, real edge says %s — resolve by hand)\n",
			row.Table, row.IssueID, row.Target, row.DepType, row.CoversType)
	}

	if plan.Touched() == 0 {
		if !applied {
			fmt.Println("\nNothing to repair: every text target this database holds is already a real edge.")
		}
		return
	}
	if !applied {
		fmt.Printf("\nNothing was written — this was the dry run. Apply the repair with:\n  bd dep reclassify --apply\n")
	}
}

func collapseWord(applied bool) string {
	if applied {
		return "collapsed (redundant text row removed)"
	}
	return "would collapse"
}

// depReclassifyJSON is the machine-readable plan, the same shape for the dry
// run and the applied repair.
func depReclassifyJSON(plan issueops.DepReclassifyPlan, applied bool, commit string) map[string]any {
	rows := func(in []issueops.DepReclassifyRow) []map[string]any {
		out := make([]map[string]any, 0, len(in))
		for _, row := range in {
			entry := map[string]any{
				"table":       row.Table,
				"dep_id":      row.DepID,
				"issue_id":    row.IssueID,
				"target":      row.Target,
				"type":        string(row.DepType),
				"target_kind": strings.ToLower(strings.TrimPrefix(row.TargetKind.Column(), "depends_on_")),
			}
			if row.CoversType != "" {
				entry["covers_type"] = string(row.CoversType)
			}
			out = append(out, entry)
		}
		return out
	}
	out := map[string]any{
		"scanned":        plan.Scanned,
		"applied":        applied,
		"reclassify":     rows(plan.Reclassify),
		"collapse":       rows(plan.Collapse),
		"type_conflicts": rows(plan.TypeConflicts),
		"unheld":         plan.Unheld,
		"explicit":       plan.Explicit,
		"touched":        plan.Touched(),
	}
	if commit != "" {
		out["commit"] = commit
	}
	return out
}

func init() {
	depReclassifyCmd.Flags().Bool("apply", false, "Apply the repair (default: dry run, nothing written)")
	depCmd.AddCommand(depReclassifyCmd)
}
