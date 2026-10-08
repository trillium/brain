package issueops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// This file implements the unified database's namespace rules for minting
// beads (docs/design/brain-single-database.md, §"The mechanism: prefixes"). On
// the unified database the bedrock invariant the cutover relies on is that a
// bead cannot exist outside a store namespace:
//
//  1. nothing mints a bead on this database without a store namespace pinned
//     (BD_NAME): the storeless caller cannot borrow the template-seeded
//     config/metadata tables those tables exist to support opening the
//     database at all — a refusal with the reason is the only outcome;
//  2. nothing mints or accepts a bead whose id prefix the pinned namespace
//     does not own in brain_store_prefixes; the prefixes a store owns are
//     runtimeeditable, so the operator answers such a refusal with
//     'bd store-prefix add <prefix>' and retries.
//
// Neither rule changes any behaviour on a legacy (per-store) database: there
// the scope is never unified and every check here vanishes.

// ErrStorelessNamespace is the sentinel a mint refusal carries when the
// connected database is the unified one and no store namespace is pinned.
// Callers may errors.Is it to distinguish refusal from ordinary config errors.
var ErrStorelessNamespace = errors.New("refusing to create: unified database with no store namespace pinned")

// ErrPrefixOwnership is the sentinel an id-prefix ownership refusal carries
// on the unified database. The prefix is not among the pinned namespace's
// prefixes recorded in brain_store_prefixes.
var ErrPrefixOwnership = errors.New("refusing to create: id prefix is not owned by this store's namespace")

// ReasonOperatorAdded is the owner_reason a prefix claim from `bd store-prefix
// add` carries — an operator decision, recorded so the audit trail is intact.
const ReasonOperatorAdded = "operator-added"

// Prefix ownership event types (brain_store_prefix_events.event_type). The
// vocabulary is fixed now so the future transfer verb (
// docs/design/brain-prefix-release.md §Question c, the named fast-follow)
// reuses it without a schema change.
const (
	// PrefixEventClaim is appended inside the transaction that inserts a new
	// runtime claim into brain_store_prefixes. Claims are recorded from the
	// event table's adoption onward; pre-adoption runtime rows are NOT
	// backfilled (decided 2026-10-07: an honest boundary beats fabricated
	// history).
	PrefixEventClaim = "claim"
	// PrefixEventRelease is appended inside the transaction that deletes the
	// ownership row, naming who released, when, and why.
	PrefixEventRelease = "release"
	// PrefixEventTransfer is reserved for the deliberate fast-follow atomic
	// move verb; it is not today's event vocabulary.
	PrefixEventTransfer = "transfer"
)

// ReasonReleasedReserved is the default reason a release carries when the
// operator gives none of their own wording.
const ReasonReleasedReserved = "released"

// buildDecidedOwnerReasons is the reason vocabulary the unify build writes.
// A row carrying one of these is the build's mapping decision, and releasing
// it through the runtime verb is a re-unification act (refused —
// docs/design/brain-prefix-release.md §Question e, refusal 3). The list is
// enumerated, not compared by prefix, because a future build-side reason must
// be added here consciously rather than silently becoming releasable.
var buildDecidedOwnerReasons = []string{
	"declared-by-store-config",
	"store-name-matches-prefix",
	"first-observing-source",
	"no-source-declares-or-matches",
}

// isBuildDecidedOwnerReason answers whether owner_reason is one the unify
// build wrote (as opposed to a runtime claim the owner may release).
func isBuildDecidedOwnerReason(reason string) bool {
	for _, r := range buildDecidedOwnerReasons {
		if r == reason {
			return true
		}
	}
	return false
}

// storePrefixEventsDDL is the events table's schema (internal/brainunify
// BrainTableDDL, §Question d). New unified builds create it from there;
// existing unified databases get it here, additively, inside the first
// transaction that needs it — CREATE TABLE IF NOT EXISTS, so provisioning is
// idempotent and never rewrites an existing table.
const storePrefixEventsDDL = `CREATE TABLE IF NOT EXISTS brain_store_prefix_events (
  id varchar(64) NOT NULL,
  event_type varchar(32) NOT NULL,
  prefix varchar(255) NOT NULL,
  actor varchar(128) NOT NULL,
  old_store varchar(128) NOT NULL,
  new_store varchar(128) NOT NULL,
  reason varchar(64) NOT NULL,
  bead_count bigint NOT NULL DEFAULT 0,
  event_at datetime NOT NULL,
  PRIMARY KEY (id),
  KEY idx_brain_prefix_events (prefix)
)`

// EnsureStorePrefixEventsTable provisions the append-only event table on a
// unified database that predates it. It is called inside the same transaction
// that changes ownership, so on Dolt — where schema changes are transactional
// — the table exists exactly when the first event lands, and nothing is
// provisioned on a legacy (per-store) database where no ownership record
// exists. Callers that open their own transaction should call it inside that
// transaction, never after: an event row must never land without the table
// existing, and this is what makes claim events complete from adoption onward.
func EnsureStorePrefixEventsTable(ctx context.Context, q DBTX) error {
	sc := UnifiedScopeForTx(ctx, q)
	if !sc.Unified {
		return nil // no namespace record on a legacy database → no events either
	}
	if _, err := q.ExecContext(ctx, storePrefixEventsDDL); err != nil {
		return fmt.Errorf("provisioning brain_store_prefix_events: %w", err)
	}
	return nil
}

// appendStorePrefixEvent writes one audit row into brain_store_prefix_events.
// The caller owns the transaction: this MUST run inside the same transaction
// that changes brain_store_prefixes, never after it and never instead of it —
// the two land together or not at all (docs/design/brain-prefix-release.md
// §Question d). The table is appended to only: no UPDATE, no DELETE, no
// compaction; brain_store_prefixes stays current state and the events table
// is its complete history from adoption onward.
func appendStorePrefixEvent(ctx context.Context, q DBTX, eventType, prefix, actor, oldStore, newStore, reason string, beadCount int64) (string, error) {
	id := NewEventID()
	if len(reason) > 64 {
		return "", fmt.Errorf("event reason %q exceeds brain_store_prefix_events.reason's 64 characters", reason)
	}
	_, err := q.ExecContext(ctx,
		"INSERT INTO brain_store_prefix_events (id, event_type, prefix, actor, old_store, new_store, reason, bead_count, event_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		id, eventType, prefix, actor, oldStore, newStore, reason, beadCount, time.Now().UTC())
	if err != nil {
		return "", fmt.Errorf("appending %s event for prefix %q: %w", eventType, prefix, err)
	}
	return id, nil
}

// liveStorePrefixBeadCount counts the beads that carry prefix right now, from
// the merged issues table — never from brain_store_prefixes' stale
// bead_count, which is a claim-time observation (docs/design/
// brain-prefix-release.md §Question e, refusal 4). The prefix is bound, never
// interpolated, so LIKE wildcards in it ('_' is a legal prefix character)
// cannot distort the bucket; and CONCAT instead of a quoted literal keeps the
// pattern expressible without string building in Go. All beads under the
// prefix are in the bucket: beadIDPrefix buckets on the first '-', so both
// top-level ("p-1a2b3c"), counter ("p-12"), hierarchical ("p-se7t.9") and
// wisp ("p-wisp-..."), plus the id exactly equal to the prefix itself, are
// exactly the ids the ownership check would also bucket under prefix.
func liveStorePrefixBeadCount(ctx context.Context, q DBTX, prefix string) (int64, error) {
	var n int64
	if err := q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM issues WHERE id = ? OR id LIKE CONCAT(?, '-%')",
		prefix, prefix).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting live beads under prefix %q: %w", prefix, err)
	}
	return n, nil
}

// RefuseStorelessMint checks the unified scope's create boundary and returns
// the storeless refusal error when the connected database is the unified one
// and no namespace is pinned. Nil on everything else — a legacy database, or
// a unified database reached under a wrapper — keeps legacy callers untouched.
// The error wraps ErrStorelessNamespace so callers can errors.Is it, and names
// the mechanism (BD_NAME) and the way out, so the refusal teaches rather than
// stonewalls.
func RefuseStorelessMint(ctx context.Context, q DBTX) error {
	sc := UnifiedScopeForTx(ctx, q)
	if sc.Unified && sc.Store == "" {
		return fmt.Errorf("%w: this is the unified database — every bead must belong to a store — "+
			"and no store namespace is pinned (BD_NAME is unset), so there is no namespace this bead could fit. "+
			"Run this command under a store wrapper that exports BD_NAME.", ErrStorelessNamespace)
	}
	return nil
}

// beadIDPrefix answers the id prefix of a bead id: everything before the
// first '-'. Hierarchical ids keep their parent's prefix, so "task-se7t.389"
// is "task". Keep in step with internal/brainunify.PrefixOf (same rule) —
// duplicated here rather than imported so the issueops layer stays free of
// the brainunify build's dependency graph.
func beadIDPrefix(id string) string {
	if i := strings.IndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	return id
}

// namespaceOwnsPrefix answers whether the pinned namespace's record in
// brain_store_prefixes claims prefix. The record is read inside the caller's
// transaction, so a prefix added in the same process lifetime (or the same
// transaction) is visible immediately. When the record itself cannot be read
// (probe failure, table absent), the wrapper's name is the answer: the
// namespace's own prefix always belongs to it.
func namespaceOwnsPrefix(ctx context.Context, q DBTX, store, prefix string) (bool, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT prefix FROM brain_store_prefixes WHERE `store` = ?", store)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return prefix == store, nil
		}
		// An unreadable record would make every create fail; degrade to the
		// wrapper's own name rather than to an unscoped grant.
		return prefix == store, nil
	}
	defer func() { _ = rows.Close() }()
	var owned bool
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return prefix == store, nil
		}
		if p == prefix {
			owned = true
		}
	}
	if err := rows.Err(); err != nil {
		return prefix == store, nil
	}
	return owned, nil
}

// ValidateNamespaceOwnership refuses, on the unified database under a pinned
// namespace, an id whose prefix the namespace does not own. The check runs
// after the id exists (minted or explicit), so it sees every shape the mint
// can produce: top-level ("task-1a2b3c"), counter/"task-12", hierarchical
// ("task-se7t.9"), wisp ("task-wisp-b4e2f"), and prefix overrides ("proto-x0y").
// Refusing — rather than trusting the caller's --force — is the point: on the
// unified database a bead minted under the wrong namespace is a bead no
// store's narrow view will ever own, and the owner record is the thing that is
// true about the data.
func ValidateNamespaceOwnership(ctx context.Context, q DBTX, store, id string) error {
	prefix := beadIDPrefix(id)
	if prefix == store {
		return nil
	}
	owned, _ := namespaceOwnsPrefix(ctx, q, store, prefix)
	if owned {
		return nil
	}
	return fmt.Errorf("%w: id %q carries prefix %q, which namespace %q does not own (brain_store_prefixes has no such row for it); "+
		"to create under this prefix anyway, first claim it for this namespace with 'bd store-prefix add %s'",
		ErrPrefixOwnership, id, prefix, store, prefix)
}

// validAddedPrefixPhrase is the shape a runtime-claimable prefix may take.
// A prefix this strict always answers beadIDPrefix(id) == prefix for ids
// minted under it: no '-' inside the prefix, so a claim can never be shadowed
// by a longer prefix the first '-' would swallow.
var validAddedPrefixPhrase = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,31}$`)

// IsValidAddedPrefix validates an operator-requested runtime prefix. The
// shape rule is deliberate: a '-' inside a prefix would disagree with the
// first-'-' bucket every reader already uses, so the shape refuses it instead
// of letting a claim record something no read can ever scope to.
func IsValidAddedPrefix(prefix string) bool {
	return validAddedPrefixPhrase.MatchString(prefix)
}

// PrefixAlreadyRecorded answers the brain_store_prefixes row for prefix, when
// one exists. (found=false means the prefix is unclaimed on this database.)
func PrefixAlreadyRecorded(ctx context.Context, q DBTX, prefix string) (owner, reason string, found bool, err error) {
	var o, r string
	err = q.QueryRowContext(ctx,
		"SELECT `store`, owner_reason FROM brain_store_prefixes WHERE prefix = ?", prefix).
		Scan(&o, &r)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("reading brain_store_prefixes for %q: %w", prefix, err)
	}
	return o, r, true, nil
}

// RecordStorePrefix claims prefix for store in the connected database's
// brain_store_prefixes record, as a runtime decision:
//
//   - unclaimed → recorded now with the caller's owner_reason (default
//     ReasonOperatorAdded; 'brain stores create' claims its name prefix
//     under a "store-created" reason);
//   - recorded for the same store → no write, AlreadyOwned=true;
//   - recorded for a DIFFERENT store → refused: the row is not rewritten or
//     deleted, the caller is told whose prefix it is and that the claiming
//     store can only have it after that owner releases it. Two stores
//     claiming the same new prefix therefore converge on exactly one owner —
//     the first to record it — and every later claimant gets a loud refusal,
//     never a silent takeover.
//
// The claiming store's allowed_prefixes config gains the prefix (when the
// namespace's scoped config is addressable), so creation under the prefix is
// usable in the same command that recorded it.
//
// Every NEW claim appends a 'claim' event row in the SAME transaction that
// inserts the ownership row (docs/design/brain-prefix-release.md §Question d):
// the two land together or not at all. An already-owned no-op appends no
// event — nothing changed, and the events table is append-only, never
// rewritten. Pre-adoption runtime rows have no claim event and are not
// backfilled (decided 2026-10-07, an honest boundary).
type RecordPrefixOutcome struct {
	AlreadyOwned bool
	Owner        string
	Reason       string
	// AllowedPrefixesUpdated is true when the store's allowed_prefixes was
	// extended (or already carried the prefix).
	AllowedPrefixesUpdated bool
}

func RecordStorePrefix(ctx context.Context, q DBTX, store, prefix, reason string) (RecordPrefixOutcome, error) {
	var out RecordPrefixOutcome
	if !IsValidAddedPrefix(prefix) {
		return out, fmt.Errorf("invalid prefix %q: a prefix is letters, digits or underscores after an initial letter, and cannot contain '-' (an id's namespace is the segment before its first '-')", prefix)
	}
	if reason == "" {
		reason = ReasonOperatorAdded
	}
	if len(reason) > 64 {
		return out, fmt.Errorf("owner_reason %q exceeds brain_store_prefixes.owner_reason's 64 characters", reason)
	}

	rowOwner, rowReason, found, err := PrefixAlreadyRecorded(ctx, q, prefix)
	if err != nil {
		return out, err
	}
	if found {
		out.Owner, out.Reason = rowOwner, rowReason
		if rowOwner == store {
			out.AlreadyOwned = true
			if err := recordStorePrefixAllowedAdd(ctx, q, store, prefix); err == nil {
				out.AllowedPrefixesUpdated = true
			}
			return out, nil
		}
		return out, fmt.Errorf("prefix %q is already owned by store %q (decided by %q) — "+
			"brain_store_prefixes is never rewritten silently: store %q can only take it after %q releases it",
			prefix, rowOwner, rowReason, store, rowOwner)
	}

	if _, err := q.ExecContext(ctx,
		"INSERT INTO brain_store_prefixes (`prefix`,`store`,`owner_reason`,`declared_by`,`observed_by`,`bead_count`,`ambiguous`) "+
			"VALUES (?, ?, ?, '', '', 0, 0)", prefix, store, reason); err != nil {
		return out, fmt.Errorf("recording prefix %q for store %q: %w", prefix, store, err)
	}
	out.Owner, out.Reason = store, reason

	// The event table is provisioned (idempotent) inside this same
	// transaction, so an existing unified database predating the table gets
	// it exactly when its first event lands — and the event lands or the
	// whole claim rolls back, never one without the other.
	if err := EnsureStorePrefixEventsTable(ctx, q); err != nil {
		return out, err
	}
	liveCount, err := liveStorePrefixBeadCount(ctx, q, prefix)
	if err != nil {
		return out, err
	}
	if _, err := appendStorePrefixEvent(ctx, q, PrefixEventClaim, prefix, store, "", store, reason, liveCount); err != nil {
		return out, err
	}

	if err := recordStorePrefixAllowedAdd(ctx, q, store, prefix); err == nil {
		out.AllowedPrefixesUpdated = true
	}
	return out, nil
}

// recordStorePrefixAllowedAdd extends store's allowed_prefixes config row with
// prefix (a scoped write into the re-keyed table on the unified database; and
// a plain config write on everything else — where the claim was never
// meaningful anyway, the failure is ignored by the callers, who report it
// without failing the claim). Best-effort: the ownership record is the
// authority; allowed_prefixes only smooths the create path.
func recordStorePrefixAllowedAdd(ctx context.Context, q DBTX, store, prefix string) error {
	sc := UnifiedScopeForTx(ctx, q)
	table := "config"
	query := "SELECT value FROM config WHERE `key` = ?"
	args := []any{"allowed_prefixes"}
	if sc.Enabled() {
		table = sc.name(utConfig)
		query = "SELECT value FROM " + table + " WHERE `store` = ? AND `key` = ?"
		args = []any{store, "allowed_prefixes"}
	}
	var current string
	err := q.QueryRowContext(ctx, query, args...).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading allowed_prefixes for %q: %w", store, err)
	}
	for _, part := range strings.Split(current, ",") {
		if strings.TrimSpace(part) == prefix {
			return nil // already allowed
		}
	}
	value := strings.TrimSpace(current)
	if value == "" {
		value = prefix
	} else {
		value += "," + prefix
	}
	if sc.Enabled() {
		_, err = q.ExecContext(ctx, "REPLACE INTO "+table+" (`store`,`key`,value) VALUES (?, ?, ?)", store, "allowed_prefixes", value)
	} else {
		_, err = q.ExecContext(ctx, "REPLACE INTO config (`key`, value) VALUES (?, ?)", "allowed_prefixes", value)
	}
	if err != nil {
		return fmt.Errorf("writing allowed_prefixes for %q: %w", store, err)
	}
	return nil
}

// ListStorePrefixes answers the full prefix ownership record, oldest meaning
// intact: (prefix, owning store, reason it was decided, how many beads sat in
// it at claim time).
func ListStorePrefixes(ctx context.Context, q DBTX) ([]RecordStorePrefixRow, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT `prefix`,`store`,owner_reason,bead_count FROM brain_store_prefixes ORDER BY `prefix` ASC")
	if err != nil {
		return nil, fmt.Errorf("reading brain_store_prefixes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []RecordStorePrefixRow
	for rows.Next() {
		var r RecordStorePrefixRow
		if err := rows.Scan(&r.Prefix, &r.Store, &r.Reason, &r.BeadCount); err != nil {
			return nil, fmt.Errorf("scanning brain_store_prefixes: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading brain_store_prefixes: %w", err)
	}
	return out, nil
}

// RecordStorePrefixRow is one mindprint of the namespace record.
type RecordStorePrefixRow struct {
	Prefix    string
	Store     string
	Reason    string
	BeadCount int64
}

// ReleaseStorePrefixOutcome reports what a release did, in the same
// reporting style the claim path uses.
type ReleaseStorePrefixOutcome struct {
	Released bool
	// Owner is the store that held the prefix (before release).
	Owner string
	// EventID is the release event's own id — the pointer refusal messages
	// and history readers carry ("released by <owner> at <time>, event
	// <id>, reason: <why>").
	EventID string
	// LiveBeadCount is the bead count observed at event time.
	LiveBeadCount int64
	// AllowedPrefixesUpdated is true when the owner's allowed_prefixes no
	// longer carries the prefix.
	AllowedPrefixesUpdated bool
}

// ReleaseStorePrefixOpts is what the caller asserts when it releases:
// Actor is the namespace performing the act (the wrapper's BD_NAME, proven
// the same way a claim proves authority), Reason is the operator's why (a
// default is supplied when empty) — every release event records both.
type ReleaseStorePrefixOpts struct {
	Actor  string
	Reason string
}

// ReleaseStorePrefix frees a claimed prefix, as a deliberate namespace-
// maintenance act (docs/design/brain-prefix-release.md):
//
//   - delete removes the brain_store_prefixes row. b-row-1 was the decided
//     shape: the prefix returns to exactly its pre-claim state — no row,
//     no owner, re-claimable by anyone via the ordinary claim path.
//   - the release event row is appended in the SAME transaction, so no
//     ownership change is ever silent; the owner's allowed_prefixes loses
//     the prefix in the same transaction too.
//
// Refusal semantics (§Question e), all loud, none bypassable:
//
//  1. the prefix is not recorded at all — "nothing to release": a no-op
//     success would let an operator believe a release happened;
//  2. the act is not performed as the owner (actor != row's store) — no
//     --store escape exists on the releasing side;
//  3. the prefix is build-decided (declared-by-store-config,
//     store-name-matches-prefix, first-observing-source,
//     no-source-declares-or-matches) — re-homing the build's mapping
//     decision is a re-unification question, not this verb;
//  4. the prefix still carries live beads — ABSOLUTE refusal (decided
//     2026-10-07): there is no --with-beads override; a prefix frees only
//     when it carries no beads, and migrating beads off it first is the
//     explicit, auditable path;
//  6. a defensive assertion verifies the structural own-name fallback
//     (the create path always honors prefix <store>) so no release can
//     strand a store's own-name minting.
//
// Racing acts (a release racing a release, or a claim) serialise the same
// way the claim path's are: the row is read inside the caller's
// transaction, so the loser finds the row gone or changed and refuses
// "already released" rather than double-deleting.
func ReleaseStorePrefix(ctx context.Context, q DBTX, prefix string, opts ReleaseStorePrefixOpts) (ReleaseStorePrefixOutcome, error) {
	var out ReleaseStorePrefixOutcome
	if opts.Actor == "" {
		return out, fmt.Errorf("no namespace to release as: the actor (the wrapper's BD_NAME) is required — release proves authority the same way a claim does")
	}
	reason := strings.TrimSpace(opts.Reason)
	if reason == "" {
		reason = ReasonReleasedReserved
	}

	// Refusal 8: the shape rule applies identically to release.
	if !IsValidAddedPrefix(prefix) {
		return out, fmt.Errorf("invalid prefix %q: a prefix is letters, digits or underscores after an initial letter, and cannot contain '-' (an id's namespace is the segment before its first '-')", prefix)
	}

	// Refusal 1/2/3/4 read the row inside the caller's transaction — the
	// read-then-write idiom the claim path uses, so racing acts serialise.
	rowOwner, rowReason, found, err := PrefixAlreadyRecorded(ctx, q, prefix)
	if err != nil {
		return out, err
	}
	if !found {
		return out, fmt.Errorf("prefix %q is not recorded in brain_store_prefixes — nothing to release: a release of a nonexistent claim must be an error, not a success-shaped no-op, so an operator never believes a release happened when it did not", prefix)
	}
	out.Owner = rowOwner

	// Refusal 2: authority by namespace identity. No --store escape exists
	// on the releasing side; the owner's wrapper is the one that releases.
	if rowOwner != opts.Actor {
		return out, fmt.Errorf("prefix %q is owned by store %q, not by %q — release must be performed as the owner's namespace (run it under %q's wrapper); there is no --store escape on the releasing side", prefix, rowOwner, opts.Actor, rowOwner)
	}

	// Refusal 3: build-decided rows never release through this verb.
	if isBuildDecidedOwnerReason(rowReason) {
		return out, fmt.Errorf("prefix %q is build-decided (reason %q) — this verb does not retract the unify build's mapping decision: re-run the build/mapping with corrected inputs, or perform the change as a deliberate administrative act outside this verb", prefix, rowReason)
	}

	// Refusal 4, absolute: live beads refuse release outright. Counted from
	// the merged issues table now, not from the row's stale bead_count.
	liveCount, err := liveStorePrefixBeadCount(ctx, q, prefix)
	if err != nil {
		return out, err
	}
	out.LiveBeadCount = liveCount
	if liveCount > 0 {
		return out, fmt.Errorf("prefix %q still carries %d live bead(s) — ABSOLUTE refusal: a prefix that still carries beads cannot be released, there is no --with-beads override. Migrate the beads off the prefix first (that path is explicit and auditable); releasing now would let a second store claim the prefix and make ownership of the existing beads ambiguous", prefix, liveCount)
	}

	// The ownership change and its event land together or not at all: the
	// events table is provisioned idempotently inside this transaction, the
	// event row is appended, then the ownership row is deleted.
	if err := EnsureStorePrefixEventsTable(ctx, q); err != nil {
		return out, err
	}
	eventID, err := appendStorePrefixEvent(ctx, q, PrefixEventRelease, prefix, opts.Actor, rowOwner, "", reason, liveCount)
	if err != nil {
		return out, err
	}
	if _, err := q.ExecContext(ctx,
		"DELETE FROM brain_store_prefixes WHERE `prefix` = ?", prefix); err != nil {
		return out, fmt.Errorf("releasing prefix %q: %w", prefix, err)
	}
	out.Released = true
	out.EventID = eventID

	// Defensive assertion (refusal 6): the structural own-name fallback —
	// the create path short-circuits on prefix == store before it reads any
	// row — must hold for the released row's owner even after it loses this
	// row. Verified through the same function the create path uses, not
	// trusted: under it, no release can strand a store's minting, and this
	// assertion makes the structural claim checkable rather than asserted.
	if err := ValidateNamespaceOwnership(ctx, q, rowOwner, rowOwner+"-release-assert"); err != nil {
		return out, fmt.Errorf("release of %q failed the own-name mint assertion for namespace %q: %v — rolled back, the row is unchanged", prefix, rowOwner, err)
	}

	if err := recordStorePrefixAllowedRemove(ctx, q, rowOwner, prefix); err == nil {
		out.AllowedPrefixesUpdated = true
	}
	return out, nil
}

// recordStorePrefixAllowedRemove removes prefix from store's
// allowed_prefixes config row — the undo of recordStorePrefixAllowedAdd, so
// the scoped config row never names a claim the record no longer carries.
// Best-effort for the same reason the add is: the ownership record is the
// authority; allowed_prefixes only smooths the create path.
func recordStorePrefixAllowedRemove(ctx context.Context, q DBTX, store, prefix string) error {
	sc := UnifiedScopeForTx(ctx, q)
	table := "config"
	query := "SELECT value FROM config WHERE `key` = ?"
	args := []any{"allowed_prefixes"}
	if sc.Enabled() {
		table = sc.name(utConfig)
		query = "SELECT value FROM " + table + " WHERE `store` = ? AND `key` = ?"
		args = []any{store, "allowed_prefixes"}
	}
	var current string
	err := q.QueryRowContext(ctx, query, args...).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // nothing recorded: nothing to remove
	}
	if err != nil {
		return fmt.Errorf("reading allowed_prefixes for %q: %w", store, err)
	}
	var kept []string
	for _, part := range strings.Split(current, ",") {
		p := strings.TrimSpace(part)
		if p == prefix || p == "" {
			continue
		}
		kept = append(kept, p)
	}
	value := strings.Join(kept, ",")
	if sc.Enabled() {
		_, err = q.ExecContext(ctx, "REPLACE INTO "+table+" (`store`,`key`,value) VALUES (?, ?, ?)", store, "allowed_prefixes", value)
	} else {
		_, err = q.ExecContext(ctx, "REPLACE INTO config (`key`, value) VALUES (?, ?)", "allowed_prefixes", value)
	}
	if err != nil {
		return fmt.Errorf("writing allowed_prefixes for %q: %w", store, err)
	}
	return nil
}

// storePrefixEventsAt is the event_at rendering helper. The Dolt driver may
// answer a datetime column as time.Time or as its string form (DSN-dependent),
// so the scan is typed loosely and normalized here — the trail's own words,
// never a reshaping of the row.
func storePrefixEventsAt(v any) (string, error) {
	switch t := v.(type) {
	case time.Time:
		if t.IsZero() {
			return "", fmt.Errorf("brain_store_prefix_events.event_at is zero")
		}
		return t.UTC().Format(time.RFC3339), nil
	case string:
		t = strings.TrimSpace(t)
		if t == "" {
			return "", fmt.Errorf("brain_store_prefix_events.event_at is empty")
		}
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05Z07:00", "2006-01-02"} {
			if tt, err := time.Parse(layout, t); err == nil {
				return tt.UTC().Format(time.RFC3339), nil
			}
		}
		return "", fmt.Errorf("brain_store_prefix_events.event_at %q is not a readable datetime", t)
	case []byte:
		return storePrefixEventsAt(string(t))
	default:
		return "", fmt.Errorf("brain_store_prefix_events.event_at has unexpected type %T", v)
	}
}

// ListStorePrefixEvents answers the ownership trail for one prefix, newest
// first. The events table is the record the release writes and the history
// verb reads. Returns an empty slice when no events exist: on a
// pre-adoption runtime claim that is the honest gap the history states
// rather than fills — the existing row has no event and none is invented
// (decided 2026-10-07).
func ListStorePrefixEvents(ctx context.Context, q DBTX, prefix string) ([]StorePrefixEvent, error) {
	if prefix != "" && !IsValidAddedPrefix(prefix) {
		return nil, fmt.Errorf("invalid prefix %q: a prefix is letters, digits or underscores after an initial letter, and cannot contain '-' (an id's namespace is the segment before its first '-')", prefix)
	}
	query := "SELECT id, event_type, prefix, actor, old_store, new_store, reason, bead_count, event_at FROM brain_store_prefix_events"
	var args []any
	if prefix != "" {
		query += " WHERE prefix = ?"
		args = []any{prefix}
	}
	query += " ORDER BY event_at DESC, id DESC"
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		// A unified database that predates the table has no events yet: the
		// table is provisioned inside the first transaction that changes
		// ownership, so its absence is the honest pre-adoption gap, not a
		// failure — and reading must not create it (history is read-only).
		if isMissingTableError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading brain_store_prefix_events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []StorePrefixEvent
	for rows.Next() {
		ev := StorePrefixEvent{}
		var eventAt any
		if err := rows.Scan(&ev.ID, &ev.EventType, &ev.Prefix, &ev.Actor, &ev.OldStore, &ev.NewStore, &ev.Reason, &ev.BeadCount, &eventAt); err != nil {
			return nil, fmt.Errorf("scanning brain_store_prefix_events: %w", err)
		}
		at, err := storePrefixEventsAt(eventAt)
		if err != nil {
			return nil, err
		}
		ev.EventAt = at
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading brain_store_prefix_events: %w", err)
	}
	return out, nil
}

// isMissingTableError recognises the "table does not exist" answer from Dolt
// ("table not found", MySQL 1146) and MySQL-compatible servers.
func isMissingTableError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "table not found") ||
		strings.Contains(msg, "doesn't exist") ||
		strings.Contains(msg, "error 1146")
}

// StorePrefixEvent is one row of the prefix-ownership audit trail — who did
// what to which prefix, when, and why. Append-only: these rows are never
// updated or deleted.
type StorePrefixEvent struct {
	ID        string
	EventType string
	Prefix    string
	Actor     string
	OldStore  string
	NewStore  string
	Reason    string
	BeadCount int64
	EventAt   string // RFC3339 UTC, from the event's event_at
}
