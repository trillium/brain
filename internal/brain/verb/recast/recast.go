// Package recast implements the `brain recast <id> --to=<kind>` verb.
//
// `brain recast` is the fourth and final of the verbs brain adds on top of
// the bd-renamed-as-brain binary (alongside `new`, `link`, and `related`).
// Unlike the other three — which create rows, write edges, or walk the graph
// — recast is the only verb that MUTATES an existing brain doc's kind. The
// same ID stays; every edge survives; every comment survives; the body
// survives; only `issue_type` (and sometimes `status`) changes.
//
// See docs/brain/WHAT_IS_BRAIN.md § 4.4 for the behavioural spec (including
// the five Given/When/Then scenarios this package's tests trace back to and
// the rationale for the name `recast` over `promote`). See
// internal/brain/verb/verb.go for the seam (Decision #5, divergence/0003)
// this package plugs into. See divergence/0010 for this tranche's landing
// notes. See internal/brain/verb/related/related.go for the verb-package
// template this file mirrors (modulo the read-only/write polarity flip:
// related is read-only, recast writes one row).
//
// # Shape
//
// The package exports the verb's Args / Result, the narrow storage seam,
// the Verb impl, and the constructor:
//
//   - Args      — `{ID, ToKind}`; positional + flag-resolved input the
//     Cobra wrapper at cmd/bd/brain_recast.go parses.
//   - Result    — `{ID, OldKind, NewKind, OldStatus, NewStatus,
//     EdgesPreserved, NoOp}`. The wrapper renders the human form and
//     marshals the same struct for --json.
//   - RecastStore — narrow interface (`GetIssue`, `GetDependenciesWithMetadata`,
//     and `UpdateIssue`) so the verb is testable against a 3-method fake.
//   - Verb      — implements verb.BrainVerb[Args, Result].
//   - New(store, actor) — constructor.
//
// # Kind transitions
//
// The verb's body is the kind-transition table. The valid targets are
// {task, knowledge, both}; any other value is rejected with the spec
// wording from § 4.4 "invalid target kind". The transitions are:
//
//	current  → task                : issue_type='task';   status preserved if 'closed', else defaulted to 'open' (knowledge sources had no meaningful task status before)
//	current  → knowledge           : issue_type='knowledge'; status PRESERVED but no longer participates in `brain ready` (the spec calls this "inert for ready-queue purposes")
//	current  → both                : issue_type='both';   status preserved if 'closed', else defaulted to 'open'
//	current == target              : no-op; no write; reports OldKind == NewKind, NoOp=true
//
// In code: only `knowledge → task` and `knowledge → both` trigger the
// status default. Every task-sourced or both-sourced transition preserves
// status. The defaulting rule respects existing closure: a knowledge doc
// whose status was explicitly `closed` stays closed on transition to task
// (the user closed it deliberately; brain does not re-open it).
//
// # No-op detection
//
// A no-op is defined as `currentKind == args.ToKind`. The verb does NOT
// short-circuit on "status would also default to the same value" — that
// case cannot arise because status defaults only apply on knowledge-source
// transitions, which by definition change the kind. The no-op path writes
// nothing to storage (verified by the fake recorder in the test suite),
// exits with NoOp=true, NewKind==OldKind, and an empty status field on the
// Result so the wrapper renders `no-op: <id> already kind=<kind>`.
//
// # Markdown relocation is out of scope
//
// The spec mentions that an `entries/knowledge/<slug>.md` file should
// relocate to `entries/task/<slug>.md` (or vice versa) on a kind change.
// That relocation is the EXFILTRATOR'S job on its next idempotent sync —
// the verb does NOT touch the markdown surface. The verb's contract is
// row-only: change `issue_type` (and possibly `status`) on the Dolt row;
// the next exfiltrator run will rebuild the markdown tree from the
// updated rows. Documenting this here so the next reader does not look
// for missing file-move code.
//
// # Edge preservation
//
// Edges live in the `dependencies` table; the verb never reads from or
// writes to that table. All edge-preservation is automatic: by leaving
// dependencies untouched, every (issue_id, depends_on_id, type) row
// pointing at or away from `<id>` survives the mutation. The verb DOES
// enumerate the outgoing edges via `GetDependenciesWithMetadata` so the
// Result's `EdgesPreserved` list can confirm the count to the user; that
// read happens BEFORE the UpdateIssue call so a transport failure on the
// read does not leave the row mutated without a corresponding count.
//
// # Why a narrow RecastStore interface (not storage.Storage)
//
// The verb needs exactly three operations: fetch one issue by ID (the
// existence probe + current-kind/status read), fetch the outgoing edges
// for that issue (so EdgesPreserved can be reported), and update the
// issue's issue_type (and optionally status) field. Depending on the full
// `storage.Storage` interface (~60 methods) would over-constrain the seam.
// `RecastStore` is the smallest surface that lets `storage.Storage` be
// passed in production and a 3-method fake be passed in tests. This
// matches the modularity-first principle from Decision #5 and mirrors
// `link.LinkStore`, `related.RelatedStore`, and `newverb.IssueCreator`.
package recast

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/steveyegge/beads/internal/brain/verb"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// validTargetKinds is the closed set the wrapper resolves --to= against.
// The spec (§ 4.4 "invalid target kind" scenario) requires the error
// listing all three so the user can recover without reading source.
//
// Declared as a slice (not a map) so iteration is deterministic — the
// error message lists them in spec order: task, knowledge, both.
var validTargetKinds = []types.IssueType{types.TypeTask, types.TypeKnowledge, types.TypeBoth}

// Args carries the positional + flag-resolved inputs the Cobra wrapper
// parses. All fields are populated before Run is called; Run does not
// read flag state.
type Args struct {
	// ID is the brain doc to recast. Required. Maps to issues.id in storage.
	ID string

	// ToKind is the target kind the row's issue_type column will be set
	// to. Required. Must be one of "task", "knowledge", or "both"; any
	// other value (including the empty string) is rejected with the
	// spec-mandated wording.
	ToKind string
}

// Result is what the verb returns on success. The Cobra wrapper formats
// it for stdout / --json. For the no-op path, NoOp=true and the OldKind/
// NewKind fields are equal; OldStatus/NewStatus are empty (no status
// change happens on a no-op).
type Result struct {
	// ID is the brain doc that was recast. Always echoed so the wrapper
	// can format the confirmation line without re-reading Args.
	ID string `json:"id"`

	// OldKind is the issue_type the row carried BEFORE the recast.
	// Always populated, including on the no-op path (where it equals
	// NewKind).
	OldKind string `json:"old_kind"`

	// NewKind is the issue_type the row carries AFTER the recast.
	// Equals args.ToKind on the success path; equals OldKind on the
	// no-op path.
	NewKind string `json:"new_kind"`

	// OldStatus is the status the row carried BEFORE the recast. Empty
	// on the no-op path (the wrapper elides the status line when
	// OldStatus is empty). Populated on every mutating transition so
	// the wrapper can render `status: <old> → <new>` when it changed
	// or `status: <s> (preserved)` when it did not.
	OldStatus string `json:"old_status,omitempty"`

	// NewStatus is the status the row carries AFTER the recast. Empty
	// on the no-op path. For task-sourced and both-sourced transitions,
	// equals OldStatus (preserved). For knowledge-sourced transitions
	// to task or both, equals "closed" iff OldStatus was "closed",
	// otherwise "open" (defaulted).
	NewStatus string `json:"new_status,omitempty"`

	// EdgesPreserved is the deterministically-ordered list of outgoing
	// edge target IDs that survived the mutation. Always a non-nil slice
	// — empty when the doc has no outgoing edges, so --json never emits
	// `null` here. Ordering is by neighbour ID (alphabetical), matching
	// the `related` verb's sibling-sort discipline so two recast calls
	// against the same data produce byte-identical output.
	EdgesPreserved []string `json:"edges_preserved"`

	// NoOp is true iff the current kind already equals args.ToKind. On
	// that branch the verb writes NOTHING to storage; the row is
	// untouched.
	NoOp bool `json:"noop"`
}

// RecastStore is the narrow seam Run needs from storage. Satisfied by
// the production *internal/storage.Storage implementations (which have
// all three methods on the embedded Storage interface) and by the
// test-only fake in recast_test.go.
//
// GetIssue is used to probe existence and read the current kind/status.
// GetDependenciesWithMetadata is used to enumerate outgoing edges for
// the EdgesPreserved field (one storage call; not part of the mutation).
// UpdateIssue is the single write call: it carries the updates map with
// "issue_type" and optionally "status" fields, mirroring the same
// storage path `bd update --type` uses.
type RecastStore interface {
	GetIssue(ctx context.Context, id string) (*types.Issue, error)
	GetDependenciesWithMetadata(ctx context.Context, issueID string) ([]*types.IssueWithDependencyMetadata, error)
	UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error
}

// Verb implements verb.BrainVerb[Args, Result].
type Verb struct {
	store RecastStore
	actor string
}

// New constructs a Verb bound to the given storage and actor.
//
// store is the RecastStore the verb reads existence/edges through and
// writes the kind/status mutation through. In cmd/bd/, this is the
// global `store` (typed *storage.DoltStorage but used through the
// narrower RecastStore — DoltStorage embeds the core Storage interface
// which has all three required methods). actor is the audit-trail
// actor string — in cmd/bd/ it comes from PersistentPreRun the same
// way every other write command sees it, and it is passed through to
// UpdateIssue so the audit log attributes the mutation correctly.
func New(store RecastStore, actor string) Verb {
	return Verb{store: store, actor: actor}
}

// Compile-time proof that Verb satisfies BrainVerb with the concrete
// types declared in this package. If a refactor breaks this assertion,
// the seam contract has been violated.
var _ verb.BrainVerb[Args, Result] = Verb{}

// Name returns the verb word as it appears on the CLI ("recast"). The
// Cobra wrapper must use this as the first whitespace-delimited token
// of its Use field.
func (Verb) Name() string { return "recast" }

// Run is the entire behaviour of `brain recast <id> --to=<kind>`.
//
// Validation order matches the spec's Given/When/Then scenarios
// (WHAT_IS_BRAIN.md § 4.4):
//
//  1. Empty ID         → "brain doc id is required"
//  2. Empty ToKind     → "target kind is required, pass --to=task|knowledge|both"
//  3. Invalid ToKind   → 'invalid target kind "x", must be one of task|knowledge|both'
//  4. Storage unwired  → "brain recast: storage is not configured"
//  5. ID missing       → "brain doc <id> not found"
//
// On valid input, Run:
//
//  1. Reads the current row (existence + current kind + current status).
//  2. If currentKind == ToKind, returns NoOp=true with both kind fields
//     equal and no status fields set; storage is NOT called for an update.
//  3. Otherwise, enumerates outgoing edges (for EdgesPreserved), computes
//     the new status per the table in the package comment, and calls
//     UpdateIssue with `issue_type` and (when applicable) `status`.
//
// Run never writes to stdout/stderr — that is the wrapper's job.
func (v Verb) Run(ctx context.Context, args Args) (Result, error) {
	if args.ID == "" {
		return Result{}, errors.New("brain doc id is required")
	}
	if args.ToKind == "" {
		return Result{}, errors.New(
			"target kind is required, pass --to=task|knowledge|both",
		)
	}
	if !isValidTargetKind(args.ToKind) {
		// Spec-mandated wording from § 4.4 "invalid target kind". The
		// list is intentionally spelled out (not just "well-known") so
		// the user can recover without reading code. Matches the link
		// verb's well-known-list discipline.
		return Result{}, fmt.Errorf(
			"invalid target kind %q, must be one of %s",
			args.ToKind, formatValidKinds(),
		)
	}

	if v.store == nil {
		// Constructor misuse — surfaced as a real error rather than a
		// nil deref so the wrapper can render it via FatalError. Matches
		// the link / related guards.
		return Result{}, errors.New("brain recast: storage is not configured")
	}

	// Existence probe. We do this BEFORE any other work so a missing
	// row fails fast with the spec-mandated wording. bd's storage wraps
	// "not found" with storage.ErrNotFound; we unwrap with errors.Is to
	// produce the brain-spec error and pass any other error through
	// wrapped so transport failures stay diagnosable.
	current, err := v.store.GetIssue(ctx, args.ID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return Result{}, fmt.Errorf("brain doc %s not found", args.ID)
		}
		return Result{}, fmt.Errorf("brain recast: lookup %s: %w", args.ID, err)
	}

	oldKind := string(current.IssueType)
	oldStatus := string(current.Status)

	// No-op branch: current kind already equals the target. Per spec
	// § 4.4 "idempotent recast" scenario, this is exit 0 with no write,
	// no markdown churn (the wrapper won't even touch SetLastTouchedID).
	// We still enumerate edges so the JSON output has a complete picture
	// of the doc's neighbourhood — the read is cheap and the symmetry
	// helps downstream tooling that diffs recast JSON before/after.
	if oldKind == args.ToKind {
		edges, edgeErr := v.listEdges(ctx, args.ID)
		if edgeErr != nil {
			return Result{}, edgeErr
		}
		return Result{
			ID:             args.ID,
			OldKind:        oldKind,
			NewKind:        oldKind,
			EdgesPreserved: edges,
			NoOp:           true,
		}, nil
	}

	// Mutating branch. Enumerate edges first so we have the preserved
	// list to report. Doing this BEFORE the UpdateIssue call means a
	// transport failure on the edge read does not leave the row mutated
	// without a corresponding count — bd's UpdateIssue is not atomic
	// with the edge read, but the ordering at least guarantees we
	// either report the full transition or report nothing.
	edges, edgeErr := v.listEdges(ctx, args.ID)
	if edgeErr != nil {
		return Result{}, edgeErr
	}

	// Compute the new status per the table in the package comment.
	// Only knowledge-sourced transitions trigger the defaulting rule;
	// every other transition preserves status verbatim.
	newStatus := computeNewStatus(current.IssueType, args.ToKind, current.Status)

	updates := map[string]interface{}{
		"issue_type": args.ToKind,
	}
	if newStatus != oldStatus {
		// Only include the status field when it actually changes. This
		// keeps the audit trail clean (UpdateIssue's audit log records
		// every field present in the updates map, regardless of whether
		// the value actually changed at the storage layer).
		updates["status"] = newStatus
	}

	if err := v.store.UpdateIssue(ctx, args.ID, updates, v.actor); err != nil {
		return Result{}, fmt.Errorf("brain recast: update %s: %w", args.ID, err)
	}

	return Result{
		ID:             args.ID,
		OldKind:        oldKind,
		NewKind:        args.ToKind,
		OldStatus:      oldStatus,
		NewStatus:      newStatus,
		EdgesPreserved: edges,
		NoOp:           false,
	}, nil
}

// listEdges fetches outgoing edges for `id` and returns the
// deterministically-ordered list of neighbour IDs. Always returns a
// non-nil slice (empty when the doc has no outgoing edges) so the
// Result's EdgesPreserved field never marshals as `null`.
//
// Ordering is by neighbour ID (alphabetical), matching the `related`
// verb's sibling-sort discipline. We do NOT include the edge type or
// any other metadata here; EdgesPreserved is a flat list of IDs because
// the spec's confirmation line ("edges: 3 preserved (B-552a, B-217,
// B-100)") shows only IDs.
func (v Verb) listEdges(ctx context.Context, id string) ([]string, error) {
	rows, err := v.store.GetDependenciesWithMetadata(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("brain recast: load edges from %s: %w", id, err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	sort.Strings(out)
	return out, nil
}

// computeNewStatus implements the kind-transition status-defaulting rule
// documented in the package comment:
//
//   - knowledge → task / both : preserve "closed" verbatim, else default
//     "open". A knowledge doc has no meaningful task-workflow status
//     before the recast, so we pick "open" as the sensible default —
//     unless the user explicitly closed it (preserve the closure).
//   - task → knowledge / both       : status preserved.
//   - both → task / knowledge       : status preserved.
//
// The function takes the OLD kind and the NEW kind (string, not
// IssueType, because the wrapper hands us a raw string from --to=) and
// the OLD status, and returns the status the row should carry after
// the mutation.
func computeNewStatus(oldKind types.IssueType, newKind string, oldStatus types.Status) string {
	// Only knowledge-sourced transitions trigger the defaulting rule.
	if oldKind != types.TypeKnowledge {
		return string(oldStatus)
	}
	// knowledge → task or knowledge → both: defaulting applies.
	if oldStatus == types.StatusClosed {
		// User explicitly closed the knowledge doc; preserve that
		// closure even though we're flipping kind. The spec scenario
		// "knowledge with closed status → task" pins this rule.
		return string(types.StatusClosed)
	}
	return string(types.StatusOpen)
}

// isValidTargetKind returns true iff s is one of the closed set of
// target kinds {task, knowledge, both}. Used by the input validation
// step; the wrapper does NOT pre-filter, so this is the single point
// of truth for "what counts as a valid --to= value".
func isValidTargetKind(s string) bool {
	for _, k := range validTargetKinds {
		if s == string(k) {
			return true
		}
	}
	return false
}

// formatValidKinds renders the valid kinds as a pipe-separated list
// for the error message. Used only on the failure path, so the
// allocation cost is incurred only when input is invalid.
func formatValidKinds() string {
	out := string(validTargetKinds[0])
	for _, k := range validTargetKinds[1:] {
		out += "|" + string(k)
	}
	return out
}
