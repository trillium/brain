package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/brain/exfiltrator"
	"github.com/steveyegge/beads/internal/types"
)

var renderCmd = &cobra.Command{
	Use:     "render <id>",
	GroupID: "issues",
	Short:   "Re-render an issue's markdown to the store's exfiltration root",
	Long: `Render the markdown for a single issue and write it to disk under
<exfil-root>/entries/<kind>/<slug>.md.

Exfil root resolves in this order: BRAIN_KNOWLEDGE_ROOT env, dirname($BEADS_DIR),
~/data/brain. Every kind renders — task, knowledge, both, bug, feature, epic,
etc. — there is no kind gate.

Use this when the on-disk markdown has been deleted, corrupted, or written by
an older version of bd. The substrate row is authoritative; the markdown is a
derived view.

The path of the rendered file is printed to stdout. Exit 0 on success.

Examples:
  bd render brain-k00042
  BRAIN_KNOWLEDGE_ROOT=/tmp/work bd render brain-k00042`,
	Args: cobra.ExactArgs(1),
	Run:  runRender,
}

var renderAllCmd = &cobra.Command{
	Use:     "render-all",
	GroupID: "issues",
	Short:   "Re-render every issue's markdown (useful after corruption or root change)",
	Long: `Walk every issue in the substrate and render its markdown to the
configured exfil root.

For each issue, one tab-separated line is printed to stdout:
  <id>\t<path>\t<status>

where status is "rendered" or "failed: <reason>". A per-issue failure does not
stop the run; the exit code is 0 only if every render succeeded. Otherwise
exit 1 (and individual failure lines on stdout describe what went wrong).`,
	Args: cobra.NoArgs,
	Run:  runRenderAll,
}

func init() {
	renderCmd.ValidArgsFunction = issueIDCompletion
	rootCmd.AddCommand(renderCmd)
	rootCmd.AddCommand(renderAllCmd)
}

func runRender(_ *cobra.Command, args []string) {
	id := args[0]
	ctx := rootCtx

	if store == nil {
		FatalErrorRespectJSON("no active store")
	}

	exf := newBrainExfiltrator(store)
	if exf == nil {
		FatalErrorRespectJSON("exfiltration is disabled (cannot resolve root)")
	}

	issue, err := store.GetIssue(ctx, id)
	if err != nil {
		FatalErrorRespectJSON("%v", err)
	}
	if issue == nil {
		FatalErrorRespectJSON("issue %s not found", id)
	}

	if err := exf.Render(ctx, issue); err != nil {
		FatalErrorRespectJSON("%v", err)
	}

	path := renderTargetPath(exf, issue)
	emitRenderPath(id, path)
}

func runRenderAll(_ *cobra.Command, _ []string) {
	ctx := rootCtx

	if store == nil {
		FatalErrorRespectJSON("no active store")
	}

	exf := newBrainExfiltrator(store)
	if exf == nil {
		FatalErrorRespectJSON("exfiltration is disabled (cannot resolve root)")
	}

	it, err := store.IterIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		FatalErrorRespectJSON("iterating issues: %v", err)
	}
	defer it.Close()

	anyFailed := false
	for it.Next(ctx) {
		issue := it.Value()
		if issue == nil {
			continue
		}
		if err := exf.Render(ctx, issue); err != nil {
			anyFailed = true
			fmt.Printf("%s\t\tfailed: %v\n", issue.ID, err)
			continue
		}
		fmt.Printf("%s\t%s\trendered\n", issue.ID, renderTargetPath(exf, issue))
	}
	if err := it.Err(); err != nil {
		FatalErrorRespectJSON("iterating issues: %v", err)
	}

	if anyFailed {
		os.Exit(1)
	}
}

// renderTargetPath returns the on-disk path the exfiltrator wrote for issue.
// Falls back to "<exfil-root>/entries/<kind>/" when slug resolution fails, so
// the stdout line is never empty.
func renderTargetPath(exf exfiltrator.Exfiltrator, issue *types.Issue) string {
	mx, ok := exf.(*exfiltrator.MarkdownExfiltrator)
	if !ok {
		return ""
	}
	slug, _, err := mx.SlugFor(issue)
	if err != nil || slug == "" {
		return mx.PathFor(issue.IssueType, "")
	}
	return mx.PathFor(issue.IssueType, slug)
}

func emitRenderPath(id, path string) {
	if jsonOutput {
		payload := map[string]interface{}{"id": id, "path": path}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(payload)
		return
	}
	fmt.Println(path)
}

