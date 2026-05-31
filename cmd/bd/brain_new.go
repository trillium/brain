package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	newverb "github.com/steveyegge/beads/internal/brain/verb/new"
	"github.com/steveyegge/beads/internal/ui"
)

// brainNewCmd is the Cobra wrapper for the `brain new <kind> <title>` verb.
//
// All behaviour lives in internal/brain/verb/new (the BrainVerb seam from
// Decision #5 / divergence/0003). This file does flag parsing, dependency
// wiring, and output formatting — no business logic, no validation. If the
// verb's contract changes, this wrapper does not move; if the rendering
// changes, the verb does not move. That is the modularity guarantee.
//
// See:
//   - internal/brain/verb/new/new.go for the verb implementation.
//   - cmd/bd/brain.go for the parent command this attaches under.
//   - divergence/0007 for this tranche's landing notes.
//   - docs/brain/WHAT_IS_BRAIN.md § 4.1 for the behavioural spec.
var brainNewCmd = &cobra.Command{
	Use:   "new <kind> <title>",
	Short: "Create a new brain doc (kind = task | knowledge | both)",
	Long: `brain new creates a brain doc of the given kind.

<kind> must be one of:
  task       — work to be done; participates in ready/blocked queues
  knowledge  — a note or learning; reference-only, never "ready"
  both       — task-shaped work whose body is also the lesson (defaults open)

The kind value rides on the existing issues.issue_type column — no schema
migration is involved. See ISA.md §"Decisions" → "Decision 5
(modularity-first architecture)" for the seam this command plugs into and
docs/brain/WHAT_IS_BRAIN.md § 4.1 for the behavioural spec.

Examples:
  bd brain new task "ship the FTS5 indexer"
  bd brain new knowledge "Dolt FK constraints are lazy until commit"
  bd brain new both "Friday cache bug + postmortem" --body "details..."`,
	Args: cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		CheckReadonly("brain new")

		kind := args[0]
		title := args[1]
		body, _ := cmd.Flags().GetString("body")

		// Build the verb against the global store and actor.
		//
		// store satisfies newverb.IssueCreator via storage.Storage's
		// CreateIssue method (see internal/storage/storage.go). actor is
		// populated by PersistentPreRun the same way every other write
		// command sees it (cmd/bd/main.go).
		verb := newverb.New(store, actor)

		result, err := verb.Run(rootCtx, newverb.Args{
			Kind:  kind,
			Title: title,
			Body:  body,
		})
		if err != nil {
			FatalErrorRespectJSON("%v", err)
		}

		// Mark the command as a writer so the auto-flush / Dolt commit
		// machinery in PersistentPostRun runs. Without this, the row sits
		// in the working set and never makes it into a Dolt commit on
		// embedded backends (matches the pattern in assign.go).
		commandDidWrite.Store(true)

		SetLastTouchedID(result.ID)

		if jsonOutput {
			outputJSON(result)
			return
		}

		fmt.Printf("%s Created brain %s: %s\n",
			ui.RenderPass("✓"),
			result.Kind,
			formatFeedbackID(result.ID, title))
	},
}

func init() {
	brainNewCmd.Flags().String("body", "", "Optional markdown body (maps to the existing description column)")
	// Help text lists the closed set explicitly so users don't need to read
	// source to discover the kind vocabulary. Pulled from newverb.ValidKinds
	// so the help string can never drift from the verb's actual guard.
	brainNewCmd.Long += "\n\nValid kinds: " + strings.Join(newverb.ValidKinds(), " | ")
	brainCmd.AddCommand(brainNewCmd)
}
