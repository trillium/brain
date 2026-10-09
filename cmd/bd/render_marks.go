// render_marks.go — `bd render-marks`.
//
// The operator surface for the deletion mark. When a rendered file
// disappears, the render-import pass marks its bead with
// exfiltrator.DeletionMarkLabel ("marked-for-deletion") — the bead is never
// removed (the captain's verdict: "I don't want users accidentally deleting
// data"), and while the mark stands every render of the bead is skipped, so
// nothing resurrects the deleted file without a human.
//
// These verbs are who can see the mark and how it is cleared:
//
//	bd render-marks list          every bead carrying the mark
//	bd render-marks clear <id>…   the ONLY way a mark leaves a bead
//
// Clearing is an explicit, named act: RemoveLabel through the normal store
// surface, after which the post-write render path re-creates the file. No
// render path ever clears the mark by itself, and a re-render of a marked
// bead does not silently hide it.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/brain/exfiltrator"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

var renderMarksCmd = &cobra.Command{
	Use:     "render-marks",
	GroupID: "issues",
	Short:   "Show or clear the \"marked-for-deletion\" label beads carry when their rendered file was deleted",
	Long: `Dealing with deletion marks.

When a rendered markdown file (entries/<kind>/<slug>.md) disappears — an
operator intended it, or an accident in the synced folder — 'bd
render-import' marks the bead with the "marked-for-deletion" label instead
of deleting anything. The bead stays; the mark is visible on every read
('bd show', 'bd list'); and, while the mark stands, renders skip the bead
(the file is not silently re-created), so the deletion intent gets a
human's review rather than a background undo.

  bd render-marks list          every bead carrying the mark
  bd render-marks clear <id>…   acknowledge the review, clear the mark

Clearing the mark is an explicit act and the only way a mark leaves a bead:
it removes the label through the normal store surface, after which the
post-write render path re-creates the file. Deleting the SUBSTRATE bead
itself, if the deletion intent is confirmed after all, is deliberately a
separate decision ('bd delete <id>') and never automatic.`,
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		_ = cmd.Help()
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

var renderMarksListCmd = &cobra.Command{
	Use:   "list",
	Short: "List every bead carrying the deletion mark",
	Long: `List the beads whose rendered file was deleted and whose deletion has not
been reviewed yet. Each bead carries the "marked-for-deletion" label.

With --json, stdout is a JSON array of {id, title, status} objects.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := rootCtx
		if store == nil {
			FatalErrorRespectJSON("no active store")
		}
		return runRenderMarksList(ctx)
	},
}

var renderMarksClearCmd = &cobra.Command{
	Use:   "clear <id>...",
	Short: "Clear the deletion mark on the named beads (renders resume; the files are re-created)",
	Long: `Clear the "marked-for-deletion" label on the named beads. This is the ONLY
way a mark leaves a bead — no render path clears it by itself.

After a clear, the normal render path re-creates each bead's rendered file
(the label change itself triggers the post-write render). To acknowledge
the deletion intent by actually deleting the substrate bead, use
'bd delete <id>' separately; this verb only unmarks, it never deletes.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := rootCtx
		if store == nil {
			FatalErrorRespectJSON("no active store")
		}
		return runRenderMarksClear(ctx, args)
	},
}

func init() {
	renderMarksCmd.AddCommand(renderMarksListCmd)
	renderMarksCmd.AddCommand(renderMarksClearCmd)
	rootCmd.AddCommand(renderMarksCmd)
}

func runRenderMarksList(ctx context.Context) error {
	it, err := store.IterIssues(ctx, "", types.IssueFilter{Labels: []string{exfiltrator.DeletionMarkLabel}})
	if err != nil {
		FatalErrorRespectJSON("iterating marked beads: %v", err)
	}
	defer func() { _ = it.Close() }()

	type row struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Status string `json:"status"`
	}
	var rows []row
	for it.Next(ctx) {
		issue := it.Value()
		if issue == nil {
			continue
		}
		rows = append(rows, row{ID: issue.ID, Title: issue.Title, Status: string(issue.Status)})
		if !jsonOutput {
			fmt.Printf("%s\t%s\t%s\n", issue.ID, issue.Status, issue.Title)
		}
	}
	if err := it.Err(); err != nil {
		FatalErrorRespectJSON("iterating marked beads: %v", err)
	}
	if jsonOutput {
		if rows == nil {
			rows = []row{}
		}
		outputJSON(rows)
		return nil
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "render-marks: no bead carries the mark")
	}
	return nil
}

func runRenderMarksClear(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if _, err := store.GetIssue(ctx, id); err != nil {
			FatalErrorRespectJSON("cannot find bead %s: %v", id, err)
		}
		labels, err := store.GetLabels(ctx, id)
		if err != nil {
			FatalErrorRespectJSON("reading labels for %s: %v", id, err)
		}
		marked := false
		for _, l := range labels {
			if l == exfiltrator.DeletionMarkLabel {
				marked = true
				break
			}
		}
		if !marked {
			fmt.Printf("unmarked %s: does not carry the mark, nothing to clear\n", id)
			continue
		}
		if err := store.RemoveLabel(ctx, id, exfiltrator.DeletionMarkLabel, brainEditBackActor); err != nil {
			FatalErrorRespectJSON("clearing mark on %s: %v", id, err)
		}
		fmt.Printf("cleared  %s: deletion mark removed; the post-write render re-creates the file\n", id)
	}
	return nil
}

// ensure storage loop-up stays straight: the decorated store's
// RemoveLabel triggers the post-write render, which is what re-creates
// the file — hence "renders resume" in the help text above.
var _ = storage.ErrNotFound
