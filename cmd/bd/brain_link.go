package main

import (
	"fmt"

	"github.com/spf13/cobra"
	linkverb "github.com/steveyegge/beads/internal/brain/verb/link"
	"github.com/steveyegge/beads/internal/ui"
)

// brainLinkCmd is the Cobra wrapper for the `brain link <from> <to>` verb.
//
// All behaviour — validation, existence checks, error wording, storage write —
// lives in internal/brain/verb/link (the BrainVerb seam from Decision #5 /
// divergence/0003). This file does flag parsing (specifically, resolving the
// four mutually-exclusive edge-type flags into a single string), dependency
// wiring, and output formatting. No business logic, no validation beyond the
// mutex-flag resolution that Cobra cannot express directly.
//
// See:
//   - internal/brain/verb/link/link.go for the verb implementation.
//   - cmd/bd/brain.go for the parent command this attaches under.
//   - cmd/bd/brain_new.go for the verb-wrapper template this file mirrors.
//   - divergence/0008 for this tranche's landing notes.
//   - docs/brain/WHAT_IS_BRAIN.md § 4.2 for the behavioural spec.
var brainLinkCmd = &cobra.Command{
	Use:   "link <from> <to>",
	Short: "Link two brain docs with a typed edge",
	Long: `brain link writes one typed edge (a row in the dependencies table)
between two existing brain docs.

Exactly one edge-type flag must be set:
  --extends         the from-doc extends/revises the to-doc
  --learned-from    the from-doc captures a lesson learned from the to-doc
  --related         the two docs are related (the catch-all edge)
  --type <name>     any well-known bd dependency type (escape hatch)

The flags are mutually exclusive — setting zero or more than one is a
usage error. The wrapper resolves the chosen flag to a single edge-type
string and hands it to the verb; the verb does all validation, existence
probing, and storage I/O.

Examples:
  bd brain link B-a7b3c B-217 --learned-from
  bd brain link B-a7b3c B-552a --extends
  bd brain link B-100 B-101 --related
  bd brain link B-100 B-101 --type=blocks`,
	Args: cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		CheckReadonly("brain link")

		extends, _ := cmd.Flags().GetBool("extends")
		learnedFrom, _ := cmd.Flags().GetBool("learned-from")
		related, _ := cmd.Flags().GetBool("related")
		typeFlag, _ := cmd.Flags().GetString("type")

		// Resolve the mutex flags into a single edge-type string.
		//
		// Cobra has MarkFlagsMutuallyExclusive but it does not cover the
		// "exactly one" case cleanly across bool + string flags, and it
		// only reports the first conflict. Doing the resolution here keeps
		// the user-facing error specific (names every flag that was set
		// and every flag the user could have picked).
		var (
			resolvedType string
			chosenFlags  []string
		)
		if extends {
			resolvedType = "extends"
			chosenFlags = append(chosenFlags, "--extends")
		}
		if learnedFrom {
			resolvedType = "learned-from"
			chosenFlags = append(chosenFlags, "--learned-from")
		}
		if related {
			resolvedType = "related"
			chosenFlags = append(chosenFlags, "--related")
		}
		if typeFlag != "" {
			resolvedType = typeFlag
			chosenFlags = append(chosenFlags, "--type")
		}

		switch len(chosenFlags) {
		case 0:
			FatalErrorRespectJSON(
				"brain link: an edge-type flag is required; pass one of --extends, --learned-from, --related, or --type <name>",
			)
		case 1:
			// happy path — resolvedType is set
		default:
			FatalErrorRespectJSON(
				"brain link: edge-type flags are mutually exclusive; got %d (%v); pass exactly one of --extends, --learned-from, --related, or --type <name>",
				len(chosenFlags), chosenFlags,
			)
		}

		// Build the verb against the global store and actor.
		//
		// store (a *storage.DoltStorage) satisfies linkverb.LinkStore via the
		// embedded Storage interface's GetIssue and AddDependency methods
		// (see internal/storage/storage.go). actor is populated by
		// PersistentPreRun the same way every other write command sees it
		// (cmd/bd/main.go), matching the brain_new.go pattern.
		verb := linkverb.New(store, actor)

		result, err := verb.Run(rootCtx, linkverb.Args{
			From:     args[0],
			To:       args[1],
			EdgeType: resolvedType,
		})
		if err != nil {
			FatalErrorRespectJSON("%v", err)
		}

		// Mark the command as a writer so the auto-flush / Dolt commit
		// machinery in PersistentPostRun runs. Without this, the row sits
		// in the working set and never makes it into a Dolt commit on
		// embedded backends (matches the pattern in assign.go and
		// brain_new.go).
		commandDidWrite.Store(true)

		if jsonOutput {
			outputJSON(result)
			return
		}

		fmt.Printf("%s linked: %s —[%s]→ %s\n",
			ui.RenderPass("✓"),
			result.From,
			result.EdgeType,
			result.To)
	},
}

func init() {
	brainLinkCmd.Flags().Bool("extends", false, "Edge type: from-doc extends/revises to-doc")
	brainLinkCmd.Flags().Bool("learned-from", false, "Edge type: from-doc captures a lesson learned from to-doc")
	brainLinkCmd.Flags().Bool("related", false, "Edge type: the two docs are related")
	brainLinkCmd.Flags().String("type", "", "Edge type: any well-known bd dependency type (escape hatch)")
	brainCmd.AddCommand(brainLinkCmd)
}
