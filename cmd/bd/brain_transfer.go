package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	transferverb "github.com/steveyegge/beads/internal/brain/verb/transfer"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/ui"
)

// brainTransferCmd is the Cobra wrapper for the `brain transfer <id> <dest>` verb.
//
// All behaviour — store-name resolution, ID generation, cross-database
// SQL, single-TX atomicity — lives in internal/brain/verb/transfer (the
// BrainVerb seam from Decision #5 / divergence/0003). This file does
// flag parsing, dependency wiring (registry load + raw-DB extraction),
// and output formatting. No business logic, no validation beyond what
// Cobra's argument parser already does.
//
// See:
//   - internal/brain/verb/transfer/transfer.go for the verb implementation.
//   - internal/brain/verb/transfer/registry.go for the store registry.
//   - cmd/bd/brain.go for the parent command this attaches under.
//   - cmd/bd/brain_link.go for the verb-wrapper template this file mirrors.
//
// # Why we adapt RawDBAccessor into transfer.TransferDB
//
// The verb works against any object exposing a *sql.Conn (so tests can
// inject an in-memory DB). Production storage exposes the raw *sql.DB
// via storage.RawDBAccessor.DB(). The adapter here closes that
// expressivity gap with a single Conn(ctx) method and nothing else,
// matching the verb's narrow-seam doctrine.
var brainTransferCmd = &cobra.Command{
	Use:   "transfer <id> <dest>",
	Short: "Atomically move a brain doc from one store to another",
	Long: `brain transfer moves an existing brain doc from its current store
to a destination store on the same Dolt SQL server, in a single
atomic transaction.

<id>    The source doc's id (e.g. "inbox-abc"). The first token before
        the "-" is treated as the store prefix; the prefix must resolve
        to a known store.

<dest>  The destination store name. Must be one of the registered store
        names — for the canonical PAI federation these are:
        brain, tasks (alias: task), projects (alias: project),
        agents (alias: agent), inbox, decisions (alias: decision),
        ideas (alias: idea), life, questions (alias: question), assert.

On success the source row is closed with a close_reason recording the
destination id, a new row is created in the destination store under
that store's prefix, and a 'supersedes' edge is written into the
source store's dependencies table (with depends_on_external set to
the new dest id) so the move is queryable from either side. All
changes commit as one transaction — a failure at any step rolls back
everything.

Examples:
  brain transfer inbox-abc brain    # move an inbox capture into the brain hub
  brain transfer inbox-abc task     # move an inbox capture into tasks
  brain transfer assert-fye brain   # move an assertion into the brain hub`,
	Args: cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		CheckReadonly("brain transfer")

		// Load the store registry. A failure here is a clean "your
		// stores.yaml is malformed" message rather than the verb's
		// generic error; we surface it before any storage I/O.
		homeDir, err := os.UserHomeDir()
		if err != nil {
			// Empty homeDir is acceptable to Load — it just skips the
			// yaml enrichment and uses the builtin fallback. We still
			// surface the resolution failure as a hint, in case the
			// builtin fallback does not cover the user's case.
			homeDir = ""
		}
		registry, err := transferverb.Load(homeDir)
		if err != nil {
			FatalErrorRespectJSON("%v", err)
		}

		// Extract a raw *sql.DB from the production storage stack. The
		// transfer verb needs cross-database SQL refs, which the
		// higher-level Storage interface methods do not expose; raw
		// access is the only path. UnwrapStore peels the decorator
		// stack so the type assertion reaches the concrete DoltStore
		// regardless of how many decorators wrap it.
		accessor, ok := storage.UnwrapStore(store).(storage.RawDBAccessor)
		if !ok {
			FatalErrorRespectJSON("brain transfer: storage does not expose raw DB access")
		}
		db := accessor.DB()
		if db == nil {
			FatalErrorRespectJSON("brain transfer: storage DB handle is nil")
		}

		verb := transferverb.New(&transferTransferDB{db: db}, registry, actor)

		result, err := verb.Run(rootCtx, transferverb.Args{
			Source: args[0],
			Dest:   args[1],
		})
		if err != nil {
			FatalErrorRespectJSON("%v", err)
		}

		// Mark the command as a writer so the auto-flush / Dolt
		// commit machinery in PersistentPostRun runs across both
		// affected databases. Without this, the rows sit in the
		// working set and never make it into a Dolt commit on
		// embedded backends (matches the pattern in brain_new.go).
		commandDidWrite.Store(true)

		SetLastTouchedID(result.Dest)

		if jsonOutput {
			outputJSON(result)
			return
		}

		fmt.Printf("%s transferred: %s → %s (supersede link written)\n",
			ui.RenderPass("✓"),
			result.Source,
			result.Dest,
		)
	},
}

// transferTransferDB adapts a production *sql.DB into the narrow
// transferverb.TransferDB interface. The adapter has no state of its
// own; it just forwards Conn(ctx) to the underlying pool. Kept here
// (rather than exporting it from the verb package) because adapting
// storage internals is a cmd/bd concern, not an engine concern.
type transferTransferDB struct {
	db *sql.DB
}

func (a *transferTransferDB) Conn(ctx context.Context) (*sql.Conn, error) {
	return a.db.Conn(ctx)
}

func init() {
	rootCmd.AddCommand(brainTransferCmd)
}
