package main

import (
	"github.com/spf13/cobra"
)

// brainCmd is the parent command for the brain v0.3 verb namespace.
//
// brain is a Go superset of bd. Where bd's verbs (create, show, list, dep
// add, dep list) speak the bd vocabulary, brain's verbs (new, show, list,
// link, related) speak the knowledge-graph vocabulary documented in
// ../../ISA.md. Each child verb is a thin Cobra wrapper at
// cmd/bd/brain_<verb>.go that delegates through the BrainVerb seam at
// internal/brain/verb/verb.go (Decision #5, modularity-first).
//
// Adding a verb is three steps:
//
//  1. Implement BrainVerb in internal/brain/verb/<verb>/<verb>.go.
//  2. Create cmd/bd/brain_<verb>.go that parses flags, invokes the verb,
//     and formats the result.
//  3. Call brainCmd.AddCommand(brain<Verb>Cmd) in the child file's init().
//
// brainCmd itself takes no arguments. Without a subcommand, it prints help.
var brainCmd = &cobra.Command{
	Use:   "brain",
	Short: "brain — knowledge graph + task tool on top of bd",
	Long: `brain v0.3 absorbs bd into a single tool that unifies tasks and
knowledge under one substrate (Dolt) with markdown as the exfiltrated
render artifact.

The 'brain' verbs (new, show, list, link, related) speak the knowledge-graph
vocabulary documented in ISA.md. They are thin aliases over bd's storage
layer, gated by the kind discriminator (task | knowledge | both).

For the bd vocabulary (create, dep, list, show, etc.) see 'bd --help'.

See ISA.md §"Decisions" → "First-Tranche Decisions" → "Decision 5
(modularity-first architecture)" for the seam this command tree implements.`,
	Run: func(cmd *cobra.Command, args []string) {
		// No subcommand → print help. Help() always returns nil for cobra.
		_ = cmd.Help()
	},
}

func init() {
	rootCmd.AddCommand(brainCmd)
}
