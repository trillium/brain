package issueops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
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
