package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	recastverb "github.com/steveyegge/beads/internal/brain/verb/recast"
	"github.com/steveyegge/beads/internal/ui"
)

// brainRecastCmd is the Cobra wrapper for `brain recast <id> --to=<kind>`.
//
// All behaviour — validation, kind-transition table, status-defaulting,
// no-op detection, edge enumeration, storage write — lives in
// internal/brain/verb/recast (the BrainVerb seam from Decision #5 /
// divergence/0003). This file does flag parsing, dependency wiring, and
// output formatting. No business logic.
//
// See:
//   - internal/brain/verb/recast/recast.go for the verb implementation.
//   - cmd/bd/brain.go for the parent command this attaches under.
//   - cmd/bd/brain_link.go for the verb-wrapper template this file
//     mirrors (recast is a writer like link, so it calls CheckReadonly,
//     sets commandDidWrite, and passes actor through).
//   - cmd/bd/brain_promote.go for the namespace-collision-avoidance hint.
//   - divergence/0010 for this tranche's landing notes.
//   - docs/brain/WHAT_IS_BRAIN.md § 4.4 for the behavioural spec.
var brainRecastCmd = &cobra.Command{
	Use:   "recast <id>",
	Short: "Change the kind of an existing brain doc in place (edges/comments preserved)",
	Long: `brain recast shifts the kind of an existing brain doc. Every edge
survives. Every comment survives. The body survives. The ID survives.
Only the issue_type column changes — and, on certain transitions, the
status column.

--to=<kind> is REQUIRED. Valid values:
  task        — work to be done; participates in ready/blocked queues
  knowledge   — a note or learning; reference-only, never "ready"
  both        — task-shaped work whose body is also the lesson

Status rules:
  knowledge → task / both : status defaults to 'open' unless the row
                            was explicitly 'closed' (then preserved)
  task → knowledge / both : status preserved
  both → task / knowledge : status preserved

If the current kind already equals --to, recast is a no-op (exit 0,
no write, no markdown churn).

Markdown relocation (entries/knowledge/<slug>.md → entries/task/<slug>.md)
is OUT OF SCOPE for this verb — the exfiltrator handles it on its next
idempotent sync.

Examples:
  brain recast B-a7b3c --to=task
  brain recast B-a7b3c --to=knowledge
  brain recast B-a7b3c --to=both
  brain recast B-a7b3c --to=task --json`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		CheckReadonly("brain recast")

		toKind, _ := cmd.Flags().GetString("to")

		// Build the verb against the global store and actor.
		//
		// store (a *storage.DoltStorage) satisfies recastverb.RecastStore
		// via the embedded Storage interface's GetIssue,
		// GetDependenciesWithMetadata, and UpdateIssue methods (see
		// internal/storage/storage.go). actor is populated by
		// PersistentPreRun the same way every other write command sees
		// it (cmd/bd/main.go), matching the brain_link.go pattern.
		v := recastverb.New(store, actor)

		result, err := v.Run(rootCtx, recastverb.Args{
			ID:     args[0],
			ToKind: toKind,
		})
		if err != nil {
			FatalErrorRespectJSON("%v", err)
		}

		// Mark the command as a writer so the auto-flush / Dolt commit
		// machinery in PersistentPostRun runs — but ONLY when we actually
		// wrote. The no-op path performs zero writes; flagging it as a
		// write would force a needless Dolt commit cycle.
		if !result.NoOp {
			commandDidWrite.Store(true)
			SetLastTouchedID(result.ID)
		}

		if jsonOutput {
			outputJSON(result)
			return
		}

		// Human render. Three possible lines:
		//   1. no-op:    `no-op: <id> already kind=<kind>`
		//   2. success:  `recast: <id>  <old> → <new>` + status line +
		//                edges line (per spec § 4.4 sample output).
		if result.NoOp {
			fmt.Printf("%s no-op: %s already kind=%s\n",
				ui.RenderPass("✓"),
				result.ID,
				result.NewKind)
			return
		}

		fmt.Printf("%s recast: %s  %s → %s\n",
			ui.RenderPass("✓"),
			result.ID,
			result.OldKind,
			result.NewKind)

		// Status line is conditional: only print when status actually
		// changed (NewStatus != OldStatus). When unchanged, the line
		// is omitted per the spec sample output.
		if result.OldStatus != result.NewStatus {
			fmt.Printf("status: %s → %s\n", result.OldStatus, result.NewStatus)
		}

		// Edges line: always print on the mutating path, even with 0
		// edges, so the user sees the count is real.
		fmt.Printf("edges:  %d preserved%s\n",
			len(result.EdgesPreserved),
			formatEdgeList(result.EdgesPreserved))
	},
}

// formatEdgeList renders the list of preserved edge IDs as
// ` (id1, id2, id3)` for the human output. Returns the empty string
// when the slice is empty so the edges line reads "edges:  0 preserved"
// without a trailing empty parenthesis.
func formatEdgeList(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return " (" + strings.Join(ids, ", ") + ")"
}

func init() {
	brainRecastCmd.Flags().String("to", "",
		"Target kind: one of task | knowledge | both (required)")
	// MarkFlagRequired turns a missing --to= into a Cobra usage error
	// with exit code 1. The verb's own guard catches it again for the
	// hand-constructed-Args case (modularity guarantee).
	_ = brainRecastCmd.MarkFlagRequired("to")
	brainCmd.AddCommand(brainRecastCmd)
}
