package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/issueops"
)

// `bd store-prefix` — the runtime edge of the unified database's namespace
// record (brain_store_prefixes, docs/design/brain-single-database.md §The
// mechanism).
//
// Prefix ownership on the unified database was build-time only: the unify run
// wrote the record once, and a store could never claim a prefix afterwards.
// Runtime prefixes are the missing arbitrary surface: whenever a store needs
// a new id prefix, the operator claims it here, the decision lands in the same
// record (with owner_reason "operator-added", so the audit trail never has a
// gap), and the prefix is usable immediately — narrower reads scope to it, the
// wide view already includes it, and beads mint under/explicitly carry it
// because the create path validates ownership against this same record.
//
// The conflict rule is deliberate and non-silent: brain_store_prefixes is
// never rewritten. An unclaimed prefix goes to the claiming store; a prefix
// already recorded for the same store is a no-op; a prefix recorded for a
// DIFFERENT store refuses with the owner and the reason — to transfer it, the
// owner releases it first. Two stores claiming the same fresh prefix converge
// on exactly one owner (first record wins) and every later claimant is
// refused, never silently re-homed.

var storePrefixCmd = &cobra.Command{
	Use:     "store-prefix",
	GroupID: "setup",
	Short:   "Show or extend the unified database's namespace record (brain_store_prefixes)",
	Long: `Manage the unified database's prefix-ownership record at runtime.

  bd store-prefix list              show every prefix, its owning store, and
                                    the reason the ownership was decided
  bd store-prefix add <prefix>      claim a new id prefix for this store
                                    (defaults to the wrapper's BD_NAME;
                                    claim for another store with --store)

Prefixes are the mechanism "which store is this bead in" rides on: a bead's
namespace is the segment of its id before the first '-'. The command refuses
anything a reader could never scope to (a '-' inside a prefix), refuses a
prefix another store already owns (the record is never rewritten silently),
and is idempotent for the claiming store's own prefixes. A claimed prefix is
usable immediately: beads mint under it ("create --prefix <prefix>"), explicit
ids carry it, and every read (narrow or wide) resolves ownership from this
same record.

On a legacy (per-store) database there is no namespace record to extend; the
command refuses instead of pretending.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

var storePrefixListCmd = &cobra.Command{
	Use: "list",
	Short:   "Show the unified database's prefix-ownership record",
	Long: `List every id prefix the unified database's brain_store_prefixes record
knows: the prefix, the store that owns it, the reason the ownership was
decided (build-time "declared-by-store-config"/"store-name-matches-prefix" or
runtime "operator-added"), and the bead count observed at claim time.`,
	Args: cobra.ExactArgs(0),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStorePrefixList(cmd)
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

var storePrefixAddCmd = &cobra.Command{
	Use:   "add <prefix>",
	Short: "Claim a new id prefix for a store's namespace",
	Long: `Claim an id prefix for the named store in the unified database's
brain_store_prefixes record, so beads can mint under it immediately.

The prefix defaults to the wrapper invoking this command (BD_NAME); claim for
a different store with --store. The ownership decision is recorded with its
reason ("operator-added"), and when this namespace's scoped config is
addressable the prefix is appended to the store's allowed_prefixes so the
create path accepts it in the same breath.

Refusals are loud and non-silent:
  - a '-' inside a prefix would disagree with the first-'-' bucket every
    reader uses, so the shape refuses it;
  - a prefix another store already owns refuses with the owner's name and the
    reason it was decided — transfer it only after that store releases it;
  - claiming a prefix this store already owns is a no-op (nothing changes).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStorePrefixAdd(cmd, args[0])
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

var storePrefixTargetStore string

func init() {
	storePrefixCmd.AddCommand(storePrefixListCmd)
	storePrefixCmd.AddCommand(storePrefixAddCmd)
	storePrefixAddCmd.Flags().StringVar(&storePrefixTargetStore, "store", "",
		"Store whose namespace claims the prefix (default: the wrapper's BD_NAME)")
	rootCmd.AddCommand(storePrefixCmd)
}

// storePrefixDB resolves the connected store's raw Dolt connection — the same
// seam the federated walk uses — because the namespace record is a property of
// the connected database, not of one wrapper's storage handle.
func storePrefixDB() (*sql.DB, error) {
	if store == nil {
		return nil, fmt.Errorf("no database connection available (%s)", diagHint())
	}
	accessor, ok := storage.UnwrapStore(store).(storage.RawDBAccessor)
	if !ok {
		return nil, fmt.Errorf("the connected store does not expose a raw Dolt connection; open the unified database through a server-mode wrapper (BEADS_DOLT_SERVER_MODE=1)")
	}
	db := accessor.UnderlyingDB()
	if db == nil {
		return nil, fmt.Errorf("the connected store's raw Dolt connection is unavailable")
	}
	return db, nil
}

// resolveStorePrefixTarget answers the store whose namespace the command acts
// on: --store when given, else the wrapper's pinned namespace (BD_NAME).
func resolveStorePrefixTarget() (string, error) {
	name := strings.TrimSpace(storePrefixTargetStore)
	if name == "" {
		name = strings.TrimSpace(os.Getenv("BD_NAME"))
	}
	if name == "" {
		return "", fmt.Errorf("no store to act for: pass --store <name>, or run this under a store wrapper that exports BD_NAME")
	}
	return name, nil
}

// runStorePrefixList prints the namespace record, human-first, JSON-first when
// --json is set. Ownership never leaves the record's own words.
func runStorePrefixList(cmd *cobra.Command) error {
	db, err := storePrefixDB()
	if err != nil {
		return HandleError("%v", err)
	}
	ctx := rootCtx

	sc := issueops.UnifiedScopeForTx(ctx, db)
	if !sc.Unified {
		return HandleError("%v", fmt.Errorf("%s is not the unified database: there is no namespace record to list", "this store"))
	}

	rows, err := issueops.ListStorePrefixes(ctx, db)
	if err != nil {
		return HandleError("%v", err)
	}

	if jsonOutput {
		outputJSON(rows)
		return nil
	}
	if len(rows) == 0 {
		fmt.Println("no prefixes recorded")
		return nil
	}
	sorted := rows
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Prefix < sorted[j].Prefix })
	for _, r := range sorted {
		fmt.Printf("%-16s store=%-20s reason=%s beads=%d\n", r.Prefix, r.Store, r.Reason, r.BeadCount)
	}
	return nil
}

// runStorePrefixAdd claims prefix for the target store and reports the
// outcome in the record's own vocabulary.
func runStorePrefixAdd(cmd *cobra.Command, prefix string) error {
	db, err := storePrefixDB()
	if err != nil {
		return HandleError("%v", err)
	}
	ctx := rootCtx

	sc := issueops.UnifiedScopeForTx(ctx, db)
	if !sc.Unified {
		return HandleError("%v", fmt.Errorf("this store's database is not the unified database: prefix ownership is runtime state there only (run this against 'brain_unified', e.g. through a unified wrapper)"))
	}

	target, err := resolveStorePrefixTarget()
	if err != nil {
		return HandleError("%v", err)
	}

	if !issueops.IsValidAddedPrefix(prefix) {
		return HandleError("%v", fmt.Errorf("invalid prefix %q: a prefix is letters, digits or underscores after an initial letter, and cannot contain '-' (an id's namespace is the segment before its first '-')", prefix))
	}

	out, err := runInStorePrefixTx(ctx, db, target, prefix)
	if err != nil {
		return HandleError("%v", err)
	}

	if out.AlreadyOwned {
		fmt.Printf("%s already owned by store %s (decided by %s); nothing changed\n",
			prefix, out.Owner, out.Reason)
		if jsonOutput {
			outputJSON(map[string]any{
				"prefix": prefix, "store": out.Owner, "reason": out.Reason,
				"already_owned": true, "allowed_prefixes_updated": out.AllowedPrefixesUpdated,
			})
		}
		return nil
	}

	fmt.Printf("%s added for store %s (recording: operator-added)\n", prefix, target)
	if out.AllowedPrefixesUpdated {
		fmt.Printf("allowed_prefixes for %s now carries %s — creation under this prefix works now:\n", target, prefix)
		fmt.Printf("  %s  create --prefix %s \"...\"     # mint ids under it\n", os.Getenv("BD_NAME"), prefix)
		fmt.Printf("  %s  create --id %s-<suffix> ...  # explicit ids under it\n", os.Getenv("BD_NAME"), prefix)
	}
	if jsonOutput {
		outputJSON(map[string]any{
			"prefix": prefix, "store": out.Owner, "reason": out.Reason,
			"already_owned": false, "allowed_prefixes_updated": out.AllowedPrefixesUpdated,
		})
	}
	return nil
}

// runInStorePrefixTx runs the claim inside one transaction, so the ownership
// row and the allowed_prefixes extension land together or not at all.
func runInStorePrefixTx(ctx context.Context, db *sql.DB, target, prefix string) (issueops.RecordPrefixOutcome, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return issueops.RecordPrefixOutcome{}, fmt.Errorf("open transaction: %w", err)
	}
	out, err := issueops.RecordStorePrefix(ctx, tx, target, prefix)
	if err != nil {
		_ = tx.Rollback()
		return issueops.RecordPrefixOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return issueops.RecordPrefixOutcome{}, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}
