package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	newverb "github.com/steveyegge/beads/internal/brain/verb/new"
	"github.com/steveyegge/beads/internal/brain/verb/slug"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/ui"
)

// newCmd is the Cobra wrapper for the top-level `new <kind> <title>` verb.
//
// Hoisted from the prior `brain new` subtree per divergence/0006: the brain
// verbs (new, related, recast) live at the top level of the bd binary
// directly. With BD_NAME=brain the binary surfaces them as `brain new ...`;
// without it they surface as `bd new ...`. Either way no `brain` parent
// command exists.
//
// All behaviour lives in internal/brain/verb/new (the BrainVerb seam from
// Decision #5 / divergence/0003). This file does flag parsing, dependency
// wiring, and output formatting — no business logic, no validation. If the
// verb's contract changes, this wrapper does not move; if the rendering
// changes, the verb does not move. That is the modularity guarantee.
//
// See:
//   - internal/brain/verb/new/new.go for the verb implementation.
//   - divergence/0006 for the brain-IS-bd reframe that motivates the hoist.
//   - divergence/0007 for the initial landing notes.
//   - docs/brain/WHAT_IS_BRAIN.md § 4.1 for the behavioural spec.
var newCmd = &cobra.Command{
	Use:   "new <kind> <title>",
	Short: "Create a new brain doc (kind = task | knowledge | both | isa)",
	Long: `new creates a brain doc of the given kind.

<kind> must be one of:
  task       — work to be done; participates in ready/blocked queues
  knowledge  — a note or learning; reference-only, never "ready"
  both       — task-shaped work whose body is also the lesson (defaults open)
  isa        — an Ideal State Artifact; allocates IDs of shape <prefix>-isa-XXXXX
               and REQUIRES a slug (auto-generated from <title> when --slug is
               omitted; supply --slug explicitly when the title yields no
               alphanumerics).

The kind value rides on the existing issues.issue_type column. For kind=isa
the verb additionally sets issue.IDPrefix="isa" so the storage layer allocates
"<config-prefix>-isa-XXXXX" IDs (see migrations 0050/0051/0052 for the
substrate).

--slug is optional for non-isa kinds: when non-empty it is validated against
the slug regex (^[a-z0-9][a-z0-9-]{0,63}$) and written to the issues.slug
column; when empty the column stays NULL. Slug values must be globally unique
across all kinds — collisions exit with code 2.

Examples:
  brain new task "ship the FTS5 indexer"
  brain new knowledge "Dolt FK constraints are lazy until commit"
  brain new both "Friday cache bug + postmortem" --body "details..."
  brain new isa "Brain as ISA Substrate"
  brain new isa "Custom ISA" --slug=my-custom-slug`,
	Args: cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		CheckReadonly("new")

		kind := args[0]
		title := args[1]
		body, _ := cmd.Flags().GetString("body")
		slugFlag, _ := cmd.Flags().GetString("slug")

		// Build the verb against an adapter that pairs the global store
		// (for CreateIssue) with a raw-SQL SetSlug implementation. The
		// adapter keeps the verb's IssueCreator seam narrow while letting
		// production storage write the slug column without growing a new
		// method on every storage backend.
		creator := &brainNewStoreAdapter{ctx: rootCtx, inner: store}
		verb := newverb.New(creator, actor)

		result, err := verb.Run(rootCtx, newverb.Args{
			Kind:  kind,
			Title: title,
			Body:  body,
			Slug:  slugFlag,
		})
		if err != nil {
			// Validation and conflict failures exit 2 — mirror the contract
			// patch.go / isa_section.go use for *ValidationError so callers
			// can distinguish "you gave me garbage" (2) from "I broke" (1).
			var vErr *slug.ValidationError
			var cErr *newverb.SlugCollisionError
			if errors.As(err, &vErr) || errors.As(err, &cErr) {
				exitValidation(err)
			}
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

		if result.Slug != "" {
			fmt.Printf("%s Created brain %s: %s (slug=%s)\n",
				ui.RenderPass("✓"),
				result.Kind,
				formatFeedbackID(result.ID, title),
				result.Slug)
		} else {
			fmt.Printf("%s Created brain %s: %s\n",
				ui.RenderPass("✓"),
				result.Kind,
				formatFeedbackID(result.ID, title))
		}
	},
}

// brainNewStoreAdapter satisfies newverb.IssueCreator by delegating
// CreateIssue to the global storage.DoltStorage and implementing SetSlug
// via raw SQL through the RawDBAccessor seam. Mirrors the openWriteDB
// pattern in cmd/bd/patch.go so the slug write touches the same UPDATE
// path that `bd patch --field slug` uses.
type brainNewStoreAdapter struct {
	ctx   context.Context
	inner storage.DoltStorage
}

func (a *brainNewStoreAdapter) CreateIssue(ctx context.Context, issue *types.Issue, actor string) error {
	return a.inner.CreateIssue(ctx, issue, actor)
}

func (a *brainNewStoreAdapter) SetSlug(ctx context.Context, id, value string) error {
	accessor, ok := storage.UnwrapStore(a.inner).(storage.RawDBAccessor)
	if !ok {
		return fmt.Errorf("store does not expose raw DB access; cannot write slug")
	}
	db := accessor.DB()
	if db == nil {
		return fmt.Errorf("store DB is nil; cannot write slug")
	}
	_, err := db.ExecContext(ctx,
		"UPDATE issues SET slug = ? WHERE id = ?",
		value, id,
	)
	return err
}

func init() {
	newCmd.Flags().String("body", "", "Optional markdown body (maps to the existing description column)")
	newCmd.Flags().String("slug", "", "Optional slug (required for kind=isa; auto-generated from title when omitted)")
	// Help text lists the closed set explicitly so users don't need to read
	// source to discover the kind vocabulary. Pulled from newverb.ValidKinds
	// so the help string can never drift from the verb's actual guard.
	newCmd.Long += "\n\nValid kinds: " + strings.Join(newverb.ValidKinds(), " | ")
	rootCmd.AddCommand(newCmd)
}
