package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// brainPromoteCmd is a UX-only redirector that catches the natural-
// language verb `brain promote <id>` and points the user at
// `brain recast <id> --to=<kind>`.
//
// Rationale (WHAT_IS_BRAIN.md § 4.4 "Why recast and not promote"):
//
//	bd already ships `bd promote` (and `bd mol wisp promote`) for the
//	wisp → bead graduation — a different, narrower operation. To stay
//	out of that namespace collision, brain's kind-shift verb is named
//	`recast`. But `promote` is the natural-language candidate, and a
//	user reaching for it out of habit deserves a helpful redirect
//	rather than a silent fall-through to bd's wisp promotion.
//
// This subcommand is registered DIRECTLY on `brainCmd` (not under the
// recast verb package) because it has no business logic, no storage
// access, and no actor — it is pure UX. It accepts any number of
// arguments (Args: ArbitraryArgs) so a user typing
// `brain promote B-a7b3c task` or `brain promote --to=task B-a7b3c`
// still hits the hint and not Cobra's "unknown command" message.
//
// The hint is printed to stderr (matching FatalError's destination)
// and the command returns a non-nil error so the process exit code
// reflects the redirection — this is a usage error, not a successful
// no-op.
//
// See:
//   - cmd/bd/brain_recast.go for the verb this redirects to.
//   - docs/brain/WHAT_IS_BRAIN.md § 4.4 for the spec rationale.
//   - divergence/0010 for this tranche's landing notes.
var brainPromoteCmd = &cobra.Command{
	Use:   "promote",
	Short: "Redirect: did you mean `brain recast <id> --to=<kind>`?",
	Long: `brain promote is NOT a brain verb. It is a redirector for the natural-
language verb that often comes to mind when someone wants to shift a
brain doc's kind (knowledge → task, for example).

The brain verb for kind-shift is "recast":

  brain recast <id> --to=<kind>

The name "promote" was avoided because bd already uses it for wisp →
bead graduation (a different, narrower operation). Namespacing matters
more than reading-naturalness for a verb that appears hundreds of
times in shell history.`,
	Args:          cobra.ArbitraryArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Stderr destination matches FatalError; non-nil return matches
		// `os.Exit(1)` semantics through Cobra's error path. The two-line
		// hint matches the wording in WHAT_IS_BRAIN.md § 4.4.
		fmt.Fprintln(os.Stderr, "error: did you mean `brain recast <id> --to=<kind>`?")
		fmt.Fprintln(os.Stderr, "       `bd promote` graduates wisps to beads; `brain recast` shifts kind.")
		return errors.New("brain promote is not a brain verb; use brain recast")
	},
}

func init() {
	brainCmd.AddCommand(brainPromoteCmd)
}
