// Package link implements the `brain link <from> <to> [--edge-type]` verb.
//
// `brain link` is the second of the four verbs brain adds on top of the
// bd-renamed-as-brain binary (alongside `new`, and the still-to-land `related`
// and `recast`). It writes a new row into the existing `dependencies` table
// that bd's own `bd dep add` writes to — the only thing new on the brain
// surface is the flag spelling. Where `bd dep add --type=learned-from` reads
// awkwardly for knowledge-graph work, `brain link --learned-from` reads the
// way Trillium actually says it out loud.
//
// See docs/brain/WHAT_IS_BRAIN.md § 4.2 for the behavioural spec and the
// Given/When/Then scenarios this package's tests trace back to. See
// internal/brain/verb/verb.go for the seam (Decision #5, divergence/0003)
// this package plugs into. See divergence/0008 for this tranche's landing
// notes. See internal/brain/verb/new/new.go for the verb-package template
// this file mirrors.
//
// # Shape
//
// The package exports three types and one constructor:
//
//   - Args        — `{From, To, EdgeType}`; positional inputs + flag-resolved
//     edge type the Cobra wrapper at cmd/bd/brain_link.go parses.
//   - Result      — `{From, To, EdgeType}`; echoes the trio for stdout / --json.
//   - Verb        — implements verb.BrainVerb[Args, Result]. Holds the narrow
//     LinkStore dependency so the verb is unit-testable against a
//     fake without bringing up Dolt.
//   - New(store, actor) — constructor that returns Verb.
//
// # Why a narrow LinkStore interface (not storage.Storage)
//
// The verb needs exactly two operations: check that the source and target
// brain docs exist, and insert one edge row. Depending on the full
// `storage.Storage` interface (~60 methods) would over-constrain the seam
// and force tests to construct a 60-method fake to exercise two calls.
// `LinkStore` is the smallest surface that lets `storage.Storage` be passed
// in production and a 2-method recorder be passed in tests. This matches
// the modularity-first principle from Decision #5: keep seams as narrow as
// the verb actually needs. The pattern mirrors `newverb.IssueCreator`.
//
// # Why the verb does existence checks instead of letting storage error
//
// `storage.AddDependency` returns `"issue %s not found"` for BOTH a missing
// source and a missing target — the same wording, with only the offending
// ID telling them apart. The brain spec (WHAT_IS_BRAIN.md § 4.2) requires
// distinct messages: `from brain doc <id> not found` vs
// `target brain doc <id> not found`. The verb does the existence probes
// itself with `GetIssue` so the error wording can name which side is
// missing. The probes are cheap (single primary-key lookups), they happen
// before any write, and they keep brain's user-facing error vocabulary
// consistent regardless of which storage backend is wired in.
package link

import (
	"context"
	"errors"
	"fmt"

	"github.com/steveyegge/beads/internal/brain/verb"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// Args carries the positional + flag-resolved inputs the Cobra wrapper parses.
//
// All fields are populated by cmd/bd/brain_link.go before Run is called.
// In particular, the wrapper resolves the mutually-exclusive flags
// (--extends, --learned-from, --related, --type <name>) into a single
// EdgeType string before calling Run. Run does not read flag state; Args
// is the complete input.
type Args struct {
	// From is the source brain doc ID — the doc the edge points FROM.
	// Required. Maps to dependencies.issue_id in storage.
	From string

	// To is the target brain doc ID — the doc the edge points TO.
	// Required. Maps to dependencies.depends_on_id in storage.
	To string

	// EdgeType is the dependency type the edge is written with. Required.
	// The wrapper resolves --extends → "extends", --learned-from →
	// "learned-from", --related → "related", and --type <name> → <name>.
	// Run validates this against types.DependencyType.IsValid() (which
	// accepts any non-empty string up to 50 chars; see types.go for
	// the canonical well-known set).
	EdgeType string
}

// Result is what the verb returns on success. The Cobra wrapper formats
// it for stdout / --json. The three fields echo Args; the wrapper uses
// them to render the confirmation line without re-reading Args, matching
// the new/Result shape.
type Result struct {
	// From is the source ID the edge was written with — echoed from Args
	// so the wrapper can format `linked: <from> —[<type>]→ <to>` without
	// re-reading Args.
	From string

	// To is the target ID the edge was written with — echoed from Args.
	To string

	// EdgeType is the dependency type the edge was written with — echoed
	// from Args.
	EdgeType string
}

// LinkStore is the narrow seam Run needs from storage. Satisfied by the
// production *internal/storage.Storage implementations (which have both
// methods on the embedded Storage interface) and by the test-only fake
// in link_test.go.
//
// GetIssue is used twice — once for the source, once for the target —
// to produce side-specific error wording ("from brain doc ... not found"
// vs "target brain doc ... not found") that bd's storage cannot produce
// on its own. AddDependency is the single write call.
type LinkStore interface {
	GetIssue(ctx context.Context, id string) (*types.Issue, error)
	AddDependency(ctx context.Context, dep *types.Dependency, actor string) error
}

// Verb implements verb.BrainVerb[Args, Result].
type Verb struct {
	store LinkStore
	actor string
}

// New constructs a Verb bound to the given storage and actor.
//
// store is the LinkStore the verb reads existence and writes edges through.
// In cmd/bd/, this is the global `store` (typed *storage.DoltStorage but
// used through the narrower LinkStore — DoltStorage embeds the core Storage
// interface which has both GetIssue and AddDependency). actor is the
// audit-trail actor string — in cmd/bd/ it comes from PersistentPreRun the
// same way every other write command sees it.
func New(store LinkStore, actor string) Verb {
	return Verb{store: store, actor: actor}
}

// Compile-time proof that Verb satisfies BrainVerb with the concrete
// types declared in this package. If a refactor breaks this assertion,
// the seam contract has been violated.
var _ verb.BrainVerb[Args, Result] = Verb{}

// Name returns the verb word as it appears on the CLI ("link"). The
// Cobra wrapper must use this as the first whitespace-delimited token
// of its Use field.
func (Verb) Name() string { return "link" }

// Run is the entire behaviour of `brain link <from> <to> [--edge-type]`.
//
// Validation order matches the spec's Given/When/Then scenarios
// (WHAT_IS_BRAIN.md § 4.2):
//
//  1. Empty From         → "from brain doc id is required"
//     (orthogonal guard — Cobra's ExactArgs(2) would normally catch this,
//     but the verb's own guard is the modularity guarantee.)
//  2. Empty To           → "target brain doc id is required"
//  3. Empty EdgeType     → "edge type is required, pass one of --extends, --learned-from, --related, or --type <name>"
//     (the wrapper resolves mutex flags into a single string; this is
//     the catch-net for callers that construct Args by hand.)
//  4. Invalid EdgeType   → 'invalid edge type "x"' with a list of well-known
//     types from types.WellKnownDependencyTypes() so the user can
//     recover without reading code. types.DependencyType.IsValid()
//     itself only checks length (1..50) — the well-known list is the
//     stronger guard.
//  5. From doc missing   → "from brain doc <id> not found"
//     (scenario "linking nonexistent ID" — source side)
//  6. To doc missing     → "target brain doc <id> not found"
//     (scenario "linking nonexistent ID" — target side)
//
// On success, the edge is inserted via storage.AddDependency with:
//   - IssueID     = args.From
//   - DependsOnID = args.To
//   - Type        = args.EdgeType
//
// Storage's own idempotency rules apply (matching bd dep add):
//   - exact (from, to, type) duplicate → no error; metadata refreshed.
//   - (from, to) exists with a DIFFERENT type → error from storage about
//     the type conflict; surfaced to the caller wrapped with %w.
//
// Self-link (from == to) is permitted: bd's storage layer does not reject
// it (no SELF_BLOCK constraint in dependencies.go), and brain has no
// independent reason to reject — `extends` and `learned-from` on a doc
// pointing at itself are valid knowledge-graph constructs (e.g. a doc
// extending its own earlier revision). If bd later adds a self-link
// guard, brain link picks it up automatically through the storage error.
//
// Run never writes to stdout/stderr — that is the wrapper's job.
func (v Verb) Run(ctx context.Context, args Args) (Result, error) {
	if args.From == "" {
		return Result{}, errors.New("from brain doc id is required")
	}
	if args.To == "" {
		return Result{}, errors.New("target brain doc id is required")
	}
	if args.EdgeType == "" {
		return Result{}, errors.New(
			"edge type is required, pass one of --extends, --learned-from, --related, or --type <name>",
		)
	}

	dt := types.DependencyType(args.EdgeType)
	if !dt.IsValid() {
		// types.DependencyType.IsValid only checks length (1..50). If we
		// got here with !IsValid, the caller fed us something pathological
		// (empty after trim, or >50 chars). Either way, the recovery hint
		// names the well-known set so the user can pick a valid one.
		return Result{}, fmt.Errorf(
			"invalid edge type %q (must be non-empty, ≤50 chars); well-known types: %s",
			args.EdgeType, formatWellKnown(),
		)
	}

	if v.store == nil {
		// Constructor misuse — surfaced as a real error rather than a nil
		// deref so the wrapper can render it via FatalError. Matches the
		// new-verb guard.
		return Result{}, errors.New("brain link: storage is not configured")
	}

	// Existence probes. We do these in the verb (rather than letting
	// storage.AddDependency produce the error) because storage uses the
	// same "issue %s not found" wording for both sides — and the spec
	// requires distinct messages so the user knows which doc id to fix.
	if _, err := v.store.GetIssue(ctx, args.From); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return Result{}, fmt.Errorf("from brain doc %s not found", args.From)
		}
		return Result{}, fmt.Errorf("brain link: lookup from doc %s: %w", args.From, err)
	}
	if _, err := v.store.GetIssue(ctx, args.To); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return Result{}, fmt.Errorf("target brain doc %s not found", args.To)
		}
		return Result{}, fmt.Errorf("brain link: lookup target doc %s: %w", args.To, err)
	}

	dep := &types.Dependency{
		IssueID:     args.From,
		DependsOnID: args.To,
		Type:        dt,
	}
	if err := v.store.AddDependency(ctx, dep, v.actor); err != nil {
		return Result{}, fmt.Errorf("brain link: add %s edge: %w", args.EdgeType, err)
	}

	return Result{
		From:     args.From,
		To:       args.To,
		EdgeType: args.EdgeType,
	}, nil
}

// formatWellKnown renders the well-known dependency types as a
// comma-separated list, in the order types.WellKnownDependencyTypes
// returns them. Used only in error messages, so the cost of formatting
// is incurred only on the failure path.
func formatWellKnown() string {
	wk := types.WellKnownDependencyTypes()
	if len(wk) == 0 {
		return ""
	}
	out := string(wk[0])
	for _, t := range wk[1:] {
		out += ", " + string(t)
	}
	return out
}
