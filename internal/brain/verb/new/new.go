// Package newverb implements the `brain new <kind> <title>` verb.
//
// The directory is `new/` to match the verb word; the package is
// named `newverb` to avoid shadowing Go's built-in `new()` function
// inside this package's body. Importers refer to it as `newverb`.
//
// `brain new` is the first of the four verbs brain adds on top of the
// bd-renamed-as-brain binary (the others — link, related, recast —
// land in subsequent tranches). It writes a new brain doc through the
// same internal/storage path that bd's `create` uses, with the kind
// argument landing in the existing `issues.issue_type` TEXT column.
// No schema migration; kind is just a tag on a column bd already had.
//
// See docs/brain/WHAT_IS_BRAIN.md § 4.1 for the behavioural spec and
// the Given/When/Then scenarios this package's tests trace back to.
// See internal/brain/verb/verb.go for the seam (Decision #5,
// divergence/0003) this package plugs into. See divergence/0007 for
// this tranche's landing notes.
//
// # Shape
//
// The package exports three types and one constructor:
//
//   - Args     — `{Kind, Title, Body}`; positional inputs the Cobra
//     wrapper at cmd/bd/brain_new.go parses.
//   - Result   — `{ID, Kind}`; the new brain doc's allocated ID and the
//     kind it was created with. The wrapper formats this
//     for stdout / --json.
//   - Verb     — implements verb.BrainVerb[Args, Result]. Holds the
//     narrow IssueCreator dependency so the verb is unit-
//     testable against a fake without bringing up Dolt.
//   - New(store, actor) — constructor that returns Verb.
//
// # Why a narrow IssueCreator interface (not storage.Storage)
//
// The verb only needs to insert one row. Depending on the full
// storage.Storage interface (~60 methods) would over-constrain the
// seam and force tests to construct a 60-method fake to exercise a
// 1-method call. IssueCreator is the smallest surface that lets
// storage.Storage be passed in production and a 1-method recorder be
// passed in tests. This matches the modularity-first principle from
// Decision #5: keep seams as narrow as the verb actually needs.
package newverb

import (
	"context"
	"errors"
	"fmt"

	"github.com/steveyegge/beads/internal/brain/verb"
	"github.com/steveyegge/beads/internal/types"
)

// validKinds is the closed set the verb accepts for the <kind>
// positional. It is intentionally a slice (not a map) so error
// messages can list values in a deterministic order.
//
// The list mirrors the three values WHAT_IS_BRAIN.md § 6 names as
// the brain-flavoured extension of bd's `issues.issue_type` column.
// If a future tranche adds a fourth kind, append it here AND extend
// types.IssueType.IsValid() in internal/types/types.go so storage's
// ValidateWithCustom path keeps accepting writes.
var validKinds = []string{
	string(types.TypeTask),
	string(types.TypeKnowledge),
	string(types.TypeBoth),
}

// Args carries the positional + flag inputs the Cobra wrapper parses.
//
// All fields are populated by cmd/bd/brain_new.go before Run is called.
// Run does not read os.Args or flag state; Args is the complete input.
type Args struct {
	// Kind is the brain doc's kind discriminator. Required. Must be
	// one of "task" | "knowledge" | "both". An empty string or any
	// other value causes Run to return an error before any storage
	// write.
	Kind string

	// Title is the brain doc's headline. Required. An empty string
	// causes Run to return an error before any storage write.
	Title string

	// Body is the optional description / markdown body. Maps to bd's
	// existing `issues.description` column. Empty is allowed and
	// produces a doc with no body.
	Body string
}

// Result is what the verb returns on success. The Cobra wrapper formats
// it for stdout / --json.
type Result struct {
	// ID is the brain doc ID the storage layer allocated. Non-empty
	// on a successful Run; the wrapper prints this as
	// "created: <id>".
	ID string

	// Kind is the kind value the doc was created with — echoed back
	// so the wrapper can print the confirmation line without a
	// second round-trip through Args.
	Kind string
}

// IssueCreator is the narrow seam Run needs from storage. Satisfied by
// the production *internal/storage.Storage implementations and by the
// test-only fake in new_test.go.
//
// CreateIssue mutates issue.ID in place: when the caller passes an
// empty ID, the storage layer allocates one via
// GenerateIssueIDInTable and writes it back into the struct. Run
// relies on that contract to populate Result.ID.
type IssueCreator interface {
	CreateIssue(ctx context.Context, issue *types.Issue, actor string) error
}

// Verb implements verb.BrainVerb[Args, Result].
type Verb struct {
	store IssueCreator
	actor string
}

// New constructs a Verb bound to the given storage and actor.
//
// store is the IssueCreator the verb writes through. In cmd/bd/, this
// is the global `store` (typed *storage.DoltStorage but used through
// the narrower IssueCreator). actor is the audit-trail actor string —
// in cmd/bd/ it comes from getActorWithGit().
func New(store IssueCreator, actor string) Verb {
	return Verb{store: store, actor: actor}
}

// Compile-time proof that Verb satisfies BrainVerb with the concrete
// types declared in this package. If a refactor breaks this assertion,
// the seam contract has been violated.
var _ verb.BrainVerb[Args, Result] = Verb{}

// Name returns the verb word as it appears on the CLI ("new"). The
// Cobra wrapper must use this as the first whitespace-delimited token
// of its Use field.
func (Verb) Name() string { return "new" }

// Run is the entire behaviour of `brain new <kind> <title>`.
//
// Validation order matches the spec's Given/When/Then scenarios
// (WHAT_IS_BRAIN.md § 4.1):
//
//  1. Empty kind        → "kind is required, must be one of task|knowledge|both"
//     (scenario "missing kind argument")
//  2. Invalid kind      → 'invalid kind "x", must be one of task|knowledge|both'
//     (scenario "invalid kind value")
//  3. Empty title       → "title is required"
//     (orthogonal guard — Issue.Validate would catch it later, but
//     we catch it before allocating an ID for a clearer error)
//
// On success, the brain doc is inserted via the storage layer with:
//   - IssueType = args.Kind
//   - Title     = args.Title
//   - Description = args.Body
//   - Status    = StatusOpen (matches bd's create default)
//   - Priority  = 2 (matches bd's create default — see cmd/bd/create.go's
//     registerPriorityFlag default of "2")
//
// The storage layer auto-generates the ID; Run reads it back from the
// mutated issue pointer for Result.ID.
//
// Run never writes to stdout/stderr — that is the wrapper's job.
func (v Verb) Run(ctx context.Context, args Args) (Result, error) {
	if args.Kind == "" {
		return Result{}, errors.New("kind is required, must be one of task|knowledge|both")
	}
	if !isValidKind(args.Kind) {
		return Result{}, fmt.Errorf("invalid kind %q, must be one of task|knowledge|both", args.Kind)
	}
	if args.Title == "" {
		return Result{}, errors.New("title is required")
	}
	if v.store == nil {
		// Constructor misuse — surfaced as a real error rather than
		// a nil deref so the wrapper can render it via FatalError.
		return Result{}, errors.New("brain new: storage is not configured")
	}

	issue := &types.Issue{
		Title:       args.Title,
		Description: args.Body,
		Status:      types.StatusOpen,
		Priority:    2, // bd create's default — matches cmd/bd/create.go registerPriorityFlag("2")
		IssueType:   types.IssueType(args.Kind),
		CreatedBy:   v.actor,
	}

	if err := v.store.CreateIssue(ctx, issue, v.actor); err != nil {
		return Result{}, fmt.Errorf("brain new: create %s doc: %w", args.Kind, err)
	}

	return Result{
		ID:   issue.ID,
		Kind: args.Kind,
	}, nil
}

// isValidKind returns true iff k is one of the closed set the verb
// accepts. Kept as a helper (rather than inlined) so the same check
// can be shared with cmd/bd/brain_new.go without re-exporting the
// validKinds slice.
func isValidKind(k string) bool {
	for _, v := range validKinds {
		if k == v {
			return true
		}
	}
	return false
}

// ValidKinds returns a copy of the accepted kind values, in display
// order. Exported for cmd/bd/brain_new.go to use in --help text and
// shell completions without reaching into package internals.
func ValidKinds() []string {
	out := make([]string, len(validKinds))
	copy(out, validKinds)
	return out
}
