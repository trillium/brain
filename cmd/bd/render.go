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
exit 1 (and individual failure lines on stdout describe what went wrong).

A summary line is emitted on stderr at the end:
  Exfiltrated <ok> / <total> beads to <root>/entries/ (<failed> failed)

With --json, stdout is a single JSON object instead of per-line text:
  { "rendered": N, "failed": N, "total": N, "root": "<path>",
    "results": [ { "id", "path", "status", "error" }, ... ] }`,
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

type renderAllResult struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
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

	var (
		results []renderAllResult
		ok      int
		failed  int
	)
	for it.Next(ctx) {
		issue := it.Value()
		if issue == nil {
			continue
		}
		path := renderTargetPath(exf, issue)
		if err := exf.Render(ctx, issue); err != nil {
			failed++
			results = append(results, renderAllResult{
				ID:     issue.ID,
				Path:   path,
				Status: "failed",
				Error:  err.Error(),
			})
			if !jsonOutput {
				fmt.Printf("%s\t\tfailed: %v\n", issue.ID, err)
			}
			continue
		}
		ok++
		results = append(results, renderAllResult{
			ID:     issue.ID,
			Path:   path,
			Status: "rendered",
		})
		if !jsonOutput {
			fmt.Printf("%s\t%s\trendered\n", issue.ID, path)
		}
	}
	if err := it.Err(); err != nil {
		FatalErrorRespectJSON("iterating issues: %v", err)
	}

	total := ok + failed
	root := renderRoot(exf)

	if jsonOutput {
		payload := map[string]interface{}{
			"rendered": ok,
			"failed":   failed,
			"total":    total,
			"root":     root,
			"results":  results,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(payload)
	} else {
		fmt.Fprintln(os.Stderr, formatRenderAllSummary(ok, total, failed, root))
	}

	if failed > 0 {
		os.Exit(1)
	}
}

func renderRoot(exf exfiltrator.Exfiltrator) string {
	if mx, ok := exf.(*exfiltrator.MarkdownExfiltrator); ok {
		return mx.Root()
	}
	return ""
}

// formatRenderAllSummary builds the one-line confirmation `bd render-all`
// emits on stderr. Extracted so it can be unit-tested without running the
// full verb.
func formatRenderAllSummary(ok, total, failed int, root string) string {
	return fmt.Sprintf("Exfiltrated %d / %d beads to %s/entries/ (%d failed)",
		ok, total, root, failed)
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
		payload := map[string]interface{}{
			"id":     id,
			"path":   path,
			"status": "rendered",
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(payload)
		return
	}
	fmt.Println(path)
}
