package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	relatedverb "github.com/steveyegge/beads/internal/brain/verb/related"
	"github.com/steveyegge/beads/internal/ui"
)

// brainRelatedCmd is the Cobra wrapper for `brain related <id> [--depth=N]`.
//
// All TRAVERSAL behaviour — validation, existence check, BFS, cycle
// detection, deterministic ordering — lives in internal/brain/verb/related
// (the BrainVerb seam from Decision #5 / divergence/0003). This file does
// flag parsing, dependency wiring, and OUTPUT FORMATTING (the indented
// box-drawing tree and the --json marshal).
//
// The verb returning a tree (rather than pre-rendered text) is a
// deliberate split: the verb decides WHAT to traverse, the wrapper decides
// HOW to render it. This is a small exception to "no business logic in the
// wrapper" (rendering is presentation, not business); see
// internal/brain/verb/related/related.go § "Why the verb returns a tree"
// for the rationale.
//
// See:
//   - internal/brain/verb/related/related.go for the verb implementation.
//   - cmd/bd/brain.go for the parent command this attaches under.
//   - cmd/bd/brain_link.go for the verb-wrapper template this file
//     mirrors (minus the writer-side mark — related is read-only).
//   - divergence/0009 for this tranche's landing notes.
//   - docs/brain/WHAT_IS_BRAIN.md § 4.3 for the behavioural spec
//     (including the rendered sample tree this wrapper reproduces).
var brainRelatedCmd = &cobra.Command{
	Use:   "related <id>",
	Short: "Walk the graph from a center brain doc and print the subgraph as a tree",
	Long: `brain related performs a breadth-first walk of outgoing edges from
the given center brain doc and prints the reachable subgraph as an
indented tree.

The walk is bounded by --depth (default 2). --depth=0 prints just the
center; --depth=1 prints the center and direct neighbours; higher
values BFS further out. Each printed node carries its kind tag
([kind=task], [kind=knowledge], or [kind=both]) and — for closed tasks
— a ", closed" annotation. On a cycle, the second appearance of a node
is annotated "(already visited)" and the BFS does not recurse through
it again.

Edges are followed in the outgoing (from → to) direction only — the
same direction "bd dep list" prints by default and the same direction
"brain link <a> <b>" creates. This keeps the rendered tree directional
and prevents an explosion at common hub nodes; a future
"--bidirectional" flag is conceivable but not in scope.

Examples:
  bd brain related B-a7b3c
  bd brain related B-a7b3c --depth=3
  bd brain related B-a7b3c --depth=0      # print the center alone
  bd brain related B-a7b3c --json         # machine-readable tree`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		// brain related is read-only. CheckReadonly is intentionally NOT
		// called: the verb performs no writes, so there is no readonly
		// mode to enforce. This is the only structural difference from
		// brain_new.go / brain_link.go.

		depth, _ := cmd.Flags().GetInt("depth")

		// Build the verb against the global store.
		//
		// store (a *storage.DoltStorage) satisfies relatedverb.RelatedStore
		// via the embedded Storage interface's GetIssue and
		// GetDependenciesWithMetadata methods (see
		// internal/storage/storage.go). No actor — the verb performs no
		// writes, so there is no audit trail to attribute.
		v := relatedverb.New(store)

		result, err := v.Run(rootCtx, relatedverb.Args{
			ID:    args[0],
			Depth: depth,
		})
		if err != nil {
			FatalErrorRespectJSON("%v", err)
		}

		if jsonOutput {
			outputJSON(result)
			return
		}

		// Human render: the indented box-drawing tree from
		// WHAT_IS_BRAIN.md § 4.3. The center prints with no edge tag.
		// Each descendant prints with its incoming edge type as
		// `├─[<edge>]→ <id> · <title>          [kind=<kind>...]` (or
		// `└─[<edge>]→ ...` for the last child of its parent).
		renderRelatedTree(result.Center)
	},
}

// renderRelatedTree prints the result tree to stdout. The center is the root;
// each descendant carries its EdgeFromParent and is rendered with the
// box-drawing tee or elbow depending on whether it's the last sibling.
//
// The function is intentionally local to this file (not in the verb
// package) because rendering is a presentation concern; see the
// "Why the verb returns a tree" rationale in
// internal/brain/verb/related/related.go.
func renderRelatedTree(center *relatedverb.Node) {
	if center == nil {
		return
	}

	// Center line: no edge prefix. Style the ID by closed-vs-open so
	// the eye can scan quickly, matching bd dep list's idStr pattern.
	fmt.Printf("%s · %s          %s\n",
		styleID(center.ID, center.Closed),
		center.Title,
		formatKindTag(center.Kind, center.Closed))

	if len(center.Children) == 0 {
		// Orphan: explicit "(no neighbours)" line per the spec scenario.
		// "no neighbors" in the spec text matches US spelling; we keep
		// it to-the-letter so downstream tooling can grep for it.
		if !center.AlreadyVisited {
			fmt.Println("(no neighbors)")
		}
		return
	}

	// Print a blank vertical-bar line under the center for visual
	// separation, matching the spec sample tree.
	fmt.Println("│")
	for i, child := range center.Children {
		isLast := i == len(center.Children)-1
		renderSubtree(child, "", isLast)
	}
}

// renderSubtree prints one descendant node and recurses into its
// children. prefix is the indentation string carrying the box-drawing
// vertical bars from ancestor branches. isLast indicates whether this
// node is the last child of its parent (so we draw └─ vs ├─).
func renderSubtree(n *relatedverb.Node, prefix string, isLast bool) {
	var branch, childPrefix string
	if isLast {
		branch = "└─"
		childPrefix = prefix + "   "
	} else {
		branch = "├─"
		childPrefix = prefix + "│  "
	}

	// Line shape: `<prefix><branch>[<edge>]→ <id> · <title>          [kind=...]`
	// followed by an optional " (already visited)" marker.
	line := fmt.Sprintf("%s%s[%s]→ %s · %s          %s",
		prefix, branch,
		n.EdgeFromParent,
		styleID(n.ID, n.Closed),
		n.Title,
		formatKindTag(n.Kind, n.Closed))
	if n.AlreadyVisited {
		line += " (already visited)"
	}
	fmt.Println(line)

	if n.AlreadyVisited {
		// Cycle prune: do not descend, do not print a continuation bar.
		return
	}

	if len(n.Children) == 0 {
		// Leaf — no continuation needed.
		return
	}

	// Continuation bar for visual separation between this node's line
	// and its first child, matching the spec sample tree.
	fmt.Println(childPrefix + "│")
	for i, c := range n.Children {
		renderSubtree(c, childPrefix, i == len(n.Children)-1)
	}
}

// styleID applies bd's status-aware ID styling so closed nodes visually
// dim and open nodes stand out. Mirrors the dep list helper.
func styleID(id string, closed bool) string {
	if closed {
		return ui.StatusClosedStyle.Render(id)
	}
	return ui.StatusOpenStyle.Render(id)
}

// formatKindTag formats the `[kind=<kind>]` (or `[kind=<kind>, closed]`)
// suffix that appears at the end of every printed node line. Closed is
// only meaningful for tasks (knowledge docs don't have a workflow
// status) but the tag is uniform — letting the wrapper render the
// status verbatim keeps the rule "if status=closed, say so" simple.
func formatKindTag(kind string, closed bool) string {
	if kind == "" {
		// Defensive: a doc with no IssueType (shouldn't happen on
		// brain-created docs, but bd's own docs can elide it) just
		// gets a bare "[kind=]" rather than dropping the tag, so the
		// renderer always produces a parseable line shape.
		kind = "(unknown)"
	}
	parts := []string{"kind=" + kind}
	if closed {
		parts = append(parts, "closed")
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func init() {
	brainRelatedCmd.Flags().Int("depth", relatedverb.DefaultDepth,
		"BFS depth cap (0 prints the center alone; higher walks further)")
	brainCmd.AddCommand(brainRelatedCmd)
}
