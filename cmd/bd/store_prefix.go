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
  bd store-prefix release <prefix>  free this store's own id prefix (requires
                                    --confirm; refuses while it carries beads
                                    or the build decided it)
  bd store-prefix history <prefix>  show the ownership trail — claims and
                                    releases, newest first, with the reason
                                    each act carried

Prefixes are the mechanism "which store is this bead in" rides on: a bead's
namespace is the segment of its id before the first '-'. The command refuses
anything a reader could never scope to (a '-' inside a prefix), refuses a
prefix another store already owns (the record is never rewritten silently),
and is idempotent for the claiming store's own prefixes. A claimed prefix is
usable immediately: beads mint under it ("create --prefix <prefix>"), explicit
ids carry it, and every read (narrow or wide) resolves ownership from this
same record.

Release and history are the deliberate ownership-change edge
(docs/design/brain-prefix-release.md): release deletes the row only through
an explicit, confirmed act by the owner's own namespace, and every ownership
change — a claim and a release alike — appends a row to the append-only
brain_store_prefix_events audit table in the same transaction, so ownership
is never changed silently. A release is refused while the prefix still
carries any bead (absolute refusal, no --with-beads override) and for any
prefix the build decided; a transfer is release-then-claim.

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
	Use:   "list",
	Short: "Show the unified database's prefix-ownership record",
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

var storePrefixReleaseCmd = &cobra.Command{
	Use:   "release <prefix>",
	Short: "Free this store's own id prefix (deliberate, confirmed, event-recorded)",
	Long: `Free a prefix this namespace owns on the unified database's
brain_store_prefixes record, as a deliberate act by the owning store's own
wrapper (BD_NAME): there is no --store escape on the releasing side.

The act never happens as a side effect: it refuses unless --confirm is given,
and the confirmation is recorded in the append-only event row, so "deliberate"
is stored, not asserted. On success the ownership row is deleted — the prefix
returns to exactly its pre-claim state, re-claimable by anyone through the
ordinary 'bd store-prefix add' — and a 'release' event lands in
brain_store_prefix_events inside the same transaction, naming who released,
when, and why (--reason, recorded verbatim; optional).

Refusals are loud and none is bypassable:
  - without --confirm: release is the one namespace verb whose mistake is
    subtractive, so intent must be explicit;
  - the prefix still carries beads: ABSOLUTE refusal (no --with-beads
    override) — a prefix frees only when it carries no beads; migrate the
    beads off it first, which is the explicit, auditable path;
  - the prefix is build-decided ("declared-by-store-config",
    "store-name-matches-prefix", "first-observing-source",
    "no-source-declares-or-matches"): retracting the build's mapping decision
    is a re-unification act, not this verb;
  - the act is not performed as the owner's namespace;
  - the prefix is not recorded at all — nothing to release, an error, not a
    success-shaped no-op.

To transfer a prefix to another store, release it here and claim it there.
That unclaimed window is real — the next claim wins it — so do the two acts
promptly and check 'bd store-prefix list' between them.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStorePrefixRelease(cmd, args[0])
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

var storePrefixHistoryCmd = &cobra.Command{
	Use:   "history <prefix>",
	Short: "Show a prefix's ownership trail (claims and releases, newest first)",
	Long: `Print the ownership trail of one id prefix from the append-only
brain_store_prefix_events table: every claim and every release recorded from
that table's adoption onward, newest first, with the reason each act carried.

Claims are recorded from the event table's adoption onward. Runtime rows that
existed before it have no event, and none is invented for them — the gap is
stated where you are looking, rather than filled with a fabricated row.
Every ownership change lands with its event in the same transaction, so no
path changes ownership silently.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStorePrefixHistory(cmd, args[0])
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

var (
	storePrefixTargetStore    string
	storePrefixConfirmRelease bool
	storePrefixReleaseReason  string
)

func init() {
	storePrefixCmd.AddCommand(storePrefixListCmd)
	storePrefixCmd.AddCommand(storePrefixAddCmd)
	storePrefixCmd.AddCommand(storePrefixReleaseCmd)
	storePrefixCmd.AddCommand(storePrefixHistoryCmd)
	storePrefixAddCmd.Flags().StringVar(&storePrefixTargetStore, "store", "",
		"Store whose namespace claims the prefix (default: the wrapper's BD_NAME)")
	storePrefixReleaseCmd.Flags().BoolVar(&storePrefixConfirmRelease, "confirm", false,
		"Confirm the release: the act never happens without it, and the confirmation is recorded in the event row")
	storePrefixReleaseCmd.Flags().StringVar(&storePrefixReleaseReason, "reason", "",
		"Why the prefix is being released (recorded verbatim in the event row; optional, max 64 characters)")
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

	out, err := runInStorePrefixTx(ctx, db, target, prefix, issueops.ReasonOperatorAdded)
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

// runStorePrefixRelease frees the prefix as this wrapper's own namespace —
// the owner must be the one releasing — and reports the outcome in the
// record's own vocabulary. Everything else the event refuses loudly; the
// reason the operator gives lands verbatim in the audit trail.
func runStorePrefixRelease(cmd *cobra.Command, prefix string) error {
	db, err := storePrefixDB()
	if err != nil {
		return HandleError("%v", err)
	}
	ctx := rootCtx

	sc := issueops.UnifiedScopeForTx(ctx, db)
	if !sc.Unified {
		return HandleError("%v", fmt.Errorf("this store's database is not the unified database: prefix ownership is runtime state there only (run this against 'brain_unified', e.g. through a unified wrapper)"))
	}

	// Release proves authority by namespace identity: the wrapper's own
	// BD_NAME. Deliberately unlike 'add', there is no --store flag here —
	// release is subtractive, so only the owner's namespace performs it.
	actor := strings.TrimSpace(os.Getenv("BD_NAME"))
	if actor == "" {
		return HandleError("%v", fmt.Errorf("no namespace to release as: run this under the owning store's wrapper (BD_NAME) — release proves authority the same way a claim does"))
	}

	if !storePrefixConfirmRelease {
		// Refusal before the shape check: without --confirm nothing about the
		// prefix matters, because nothing may happen.
		return HandleError("%v", fmt.Errorf("release refused: pass --confirm to release prefix %q from namespace %q — release is deliberate, never a side effect (the confirmation is recorded in the event row)", prefix, actor))
	}

	if !issueops.IsValidAddedPrefix(prefix) {
		return HandleError("%v", fmt.Errorf("invalid prefix %q: a prefix is letters, digits or underscores after an initial letter, and cannot contain '-' (an id's namespace is the segment before its first '-')", prefix))
	}

	reason := strings.TrimSpace(storePrefixReleaseReason)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return HandleError("%v", fmt.Errorf("open transaction: %w", err))
	}
	out, err := issueops.ReleaseStorePrefix(ctx, tx, prefix, issueops.ReleaseStorePrefixOpts{Actor: actor, Reason: reason})
	if err != nil {
		_ = tx.Rollback()
		return HandleError("%v", err)
	}
	if err := tx.Commit(); err != nil {
		return HandleError("%v", fmt.Errorf("commit: %w", err))
	}

	fmt.Printf("%s released by namespace %s (event %s, live beads at release: %d)\n", prefix, actor, out.EventID, out.LiveBeadCount)
	if out.AllowedPrefixesUpdated {
		fmt.Printf("allowed_prefixes for %s no longer carries %s\n", actor, prefix)
	}
	fmt.Printf("the prefix is unclaimed: re-claim it with 'bd store-prefix add %s' (the next claim wins it)\n", prefix)
	if jsonOutput {
		outputJSON(map[string]any{
			"prefix": prefix, "actor": actor, "released": true,
			"event_id": out.EventID, "owner": out.Owner,
			"live_bead_count":          out.LiveBeadCount,
			"allowed_prefixes_updated": out.AllowedPrefixesUpdated,
		})
	}
	return nil
}

// runStorePrefixHistory prints the ownership trail, newest first, and states
// the pre-adoption gap where you are looking at it rather than filling it.
func runStorePrefixHistory(cmd *cobra.Command, prefix string) error {
	db, err := storePrefixDB()
	if err != nil {
		return HandleError("%v", err)
	}
	ctx := rootCtx

	sc := issueops.UnifiedScopeForTx(ctx, db)
	if !sc.Unified {
		return HandleError("%v", fmt.Errorf("this store's database is not the unified database: prefix ownership is runtime state there only (run this against 'brain_unified', e.g. through a unified wrapper)"))
	}

	if !issueops.IsValidAddedPrefix(prefix) {
		return HandleError("%v", fmt.Errorf("invalid prefix %q: a prefix is letters, digits or underscores after an initial letter, and cannot contain '-' (an id's namespace is the segment before its first '-')", prefix))
	}

	events, err := issueops.ListStorePrefixEvents(ctx, db, prefix)
	if err != nil {
		return HandleError("%v", err)
	}

	if jsonOutput {
		outputJSON(events)
		return nil
	}
	if len(events) == 0 {
		fmt.Printf("no events recorded for prefix %s\n", prefix)
		// State the gap, don't fill it: a runtime row claimed before the
		// event table's adoption has no event row, and no synthetic one is
		// invented for it (decided 2026-10-07).
		if owner, reason, found, ferr := issueops.PrefixAlreadyRecorded(ctx, db, prefix); ferr == nil && found {
			fmt.Printf("the prefix is currently owned by store %s\n", owner)
			fmt.Printf("claims recorded before the event table's adoption have NO event row and none is invented for them — the trail starts at adoption\n")
			if reason != "" {
				fmt.Printf("the current ownership row carries its decision reason: %s\n", reason)
			}
		}
		return nil
	}
	for _, ev := range events {
		fmt.Printf("%s  %-8s actor=%-20s owner %s -> %s  reason=%s  beads=%d  event=%s\n",
			ev.EventAt, ev.EventType, ev.Actor, orNone(ev.OldStore), orNone(ev.NewStore), ev.Reason, ev.BeadCount, ev.ID)
	}
	return nil
}

// orNone renders an empty store value as the readable marker the trail uses
// for "no owner" (the events table stores empty strings there).
func orNone(s string) string {
	if s == "" {
		return "''"
	}
	return s
}

// runInStorePrefixTx runs the claim inside one transaction, so the ownership
// row and the allowed_prefixes extension land together or not at all.
func runInStorePrefixTx(ctx context.Context, db *sql.DB, target, prefix, reason string) (issueops.RecordPrefixOutcome, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return issueops.RecordPrefixOutcome{}, fmt.Errorf("open transaction: %w", err)
	}
	out, err := issueops.RecordStorePrefix(ctx, tx, target, prefix, reason)
	if err != nil {
		_ = tx.Rollback()
		return issueops.RecordPrefixOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return issueops.RecordPrefixOutcome{}, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}
