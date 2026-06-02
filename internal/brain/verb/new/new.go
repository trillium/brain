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
	"strings"

	"github.com/steveyegge/beads/internal/brain/verb"
	"github.com/steveyegge/beads/internal/brain/verb/slug"
	"github.com/steveyegge/beads/internal/types"
)

// validKinds is the closed set the verb accepts for the <kind>
// positional. It is intentionally a slice (not a map) so error
// messages can list values in a deterministic order.
//
// The list mirrors the four values WHAT_IS_BRAIN.md § 6 names as
// the brain-flavoured extension of bd's `issues.issue_type` column.
// If a future tranche adds another kind, append it here AND extend
// types.IssueType.IsValid() in internal/types/types.go so storage's
// ValidateWithCustom path keeps accepting writes.
var validKinds = []string{
	string(types.TypeTask),
	string(types.TypeKnowledge),
	string(types.TypeBoth),
	string(types.TypeISA),
}

// SlugCollisionError is returned by Run when the storage layer rejects a
// slug write with a uniqueness-constraint violation. The Cobra wrapper
// type-asserts on this error with errors.As and exits with code 2 — the
// documented contract for validation/conflict failures.
//
// Detection is string-based against the storage backend's error text
// because Dolt/MySQL report uniqueness collisions as opaque messages
// containing "Duplicate" / "UNIQUE" / "unique"; we cannot rely on a
// typed driver error here without importing a Dolt-specific package
// into the verb (which would break the modularity guarantee).
type SlugCollisionError struct {
	Slug string
}

func (e *SlugCollisionError) Error() string {
	return fmt.Sprintf("slug already exists: %s", e.Slug)
}

// isSlugCollision returns true iff err's message contains a substring
// that storage backends emit for unique-index conflicts. Lower-cased
// on both sides so case differences across drivers don't slip past.
func isSlugCollision(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate") ||
		strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "unique index") ||
		strings.Contains(msg, "uniqueness violation") ||
		strings.Contains(msg, "slug_unique")
}

// Args carries the positional + flag inputs the Cobra wrapper parses.
//
// All fields are populated by cmd/bd/brain_new.go before Run is called.
// Run does not read os.Args or flag state; Args is the complete input.
type Args struct {
	// Kind is the brain doc's kind discriminator. Required. Must be
	// one of "task" | "knowledge" | "both" | "isa". An empty string or
	// any other value causes Run to return an error before any storage
	// write.
	Kind string

	// Title is the brain doc's headline. Required. An empty string
	// causes Run to return an error before any storage write.
	Title string

	// Body is the optional description / markdown body. Maps to bd's
	// existing `issues.description` column. Empty is allowed and
	// produces a doc with no body.
	Body string

	// Slug is the optional human-readable identifier persisted on the
	// issues.slug column (added in migration 0050, unique-indexed in
	// migration 0052).
	//
	// For kind=isa, slug is REQUIRED. If empty, Run auto-generates one
	// from Title via slug.Auto; if Title yields no alphanumerics the
	// auto path fails and the user must supply --slug explicitly.
	//
	// For other kinds, slug is OPTIONAL. If non-empty it is validated
	// and persisted; if empty the slug column stays NULL.
	Slug string
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

	// Slug is the final slug persisted on the row. May be empty for
	// non-isa kinds that did not supply a slug; for kind=isa, always
	// non-empty (either the user-supplied --slug or the auto-generated
	// value from Title).
	Slug string
}

// IssueCreator is the narrow seam Run needs from storage. Satisfied by
// the production *internal/storage.Storage implementations and by the
// test-only fake in new_test.go.
//
// CreateIssue mutates issue.ID in place: when the caller passes an
// empty ID, the storage layer allocates one via
// GenerateIssueIDInTable and writes it back into the struct. Run
// relies on that contract to populate Result.ID.
//
// SetSlug writes the slug column for the row identified by id. It is
// called after CreateIssue when args.Slug resolves to a non-empty value
// — separating the slug write from CreateIssue keeps the storage
// CreateIssue path unchanged (no migration to the bd-shared insert
// flow) while still letting the verb populate the new column. A
// uniqueness collision MUST surface as an error whose message contains
// one of the substrings isSlugCollision recognises so Run can wrap it
// as *SlugCollisionError.
type IssueCreator interface {
	CreateIssue(ctx context.Context, issue *types.Issue, actor string) error
	SetSlug(ctx context.Context, id, slug string) error
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
// (WHAT_IS_BRAIN.md § 4.1) and the F1d ISA-substrate verification matrix:
//
//  1. Empty kind        → "kind is required, must be one of task|knowledge|both|isa"
//  2. Invalid kind      → 'invalid kind "x", must be one of task|knowledge|both|isa'
//  3. Empty title       → "title is required"
//  4. Slug validation   → for kind=isa, resolve slug (from --slug or Auto(title));
//                         for other kinds, validate --slug only when non-empty.
//                         Slug-shape failures return *slug.ValidationError; an
//                         empty-title-derived auto-slug for kind=isa returns
//                         the helper's error directly so the wrapper can hint
//                         "supply --slug".
//
// On success, the brain doc is inserted via the storage layer with:
//   - IssueType = args.Kind
//   - Title     = args.Title
//   - Description = args.Body
//   - Status    = StatusOpen (matches bd's create default)
//   - Priority  = 2 (matches bd's create default — see cmd/bd/create.go's
//     registerPriorityFlag default of "2")
//   - IDPrefix  = "isa" (only when args.Kind == "isa"); this drives
//     issueops.CreateIssueInTxWithResult to allocate IDs of shape
//     "<ConfigPrefix>-isa-XXXXX" instead of the bare "<ConfigPrefix>-XXXXX".
//
// After CreateIssue succeeds, if the resolved slug is non-empty, Run
// calls SetSlug. A uniqueness collision (detected via isSlugCollision)
// surfaces as *SlugCollisionError; any other SetSlug error is wrapped
// with %w so the caller can unwrap.
//
// Run never writes to stdout/stderr — that is the wrapper's job.
func (v Verb) Run(ctx context.Context, args Args) (Result, error) {
	if args.Kind == "" {
		return Result{}, errors.New("kind is required, must be one of task|knowledge|both|isa")
	}
	if !isValidKind(args.Kind) {
		return Result{}, fmt.Errorf("invalid kind %q, must be one of task|knowledge|both|isa", args.Kind)
	}
	if args.Title == "" {
		return Result{}, errors.New("title is required")
	}
	if v.store == nil {
		// Constructor misuse — surfaced as a real error rather than
		// a nil deref so the wrapper can render it via FatalError.
		return Result{}, errors.New("brain new: storage is not configured")
	}

	// Resolve the slug.
	//
	// kind=isa: slug is required. Auto-derive from Title when --slug
	// was not supplied. If Title yields no alphanumerics, slug.Auto
	// returns an error directing the user to supply --slug — we pass
	// it through unwrapped so the wrapper's --help-style hint is the
	// helper's own message.
	//
	// Other kinds: slug is optional. Validate only when non-empty.
	resolvedSlug := args.Slug
	if args.Kind == string(types.TypeISA) {
		if resolvedSlug == "" {
			auto, err := slug.Auto(args.Title)
			if err != nil {
				return Result{}, err
			}
			resolvedSlug = auto
		}
		if err := slug.Validate(resolvedSlug); err != nil {
			return Result{}, err
		}
	} else if resolvedSlug != "" {
		if err := slug.Validate(resolvedSlug); err != nil {
			return Result{}, err
		}
	}

	issue := &types.Issue{
		Title:       args.Title,
		Description: args.Body,
		Status:      types.StatusOpen,
		Priority:    2, // bd create's default — matches cmd/bd/create.go registerPriorityFlag("2")
		IssueType:   types.IssueType(args.Kind),
		CreatedBy:   v.actor,
	}
	if args.Kind == string(types.TypeISA) {
		// Drives issueops.CreateIssueInTxWithResult to allocate
		// "<ConfigPrefix>-isa-XXXXX" instead of the bare
		// "<ConfigPrefix>-XXXXX". See internal/storage/issueops/create.go
		// lines 88-98 for the prefix-routing logic.
		issue.IDPrefix = "isa"
	}

	if err := v.store.CreateIssue(ctx, issue, v.actor); err != nil {
		return Result{}, fmt.Errorf("brain new: create %s doc: %w", args.Kind, err)
	}

	if resolvedSlug != "" {
		if err := v.store.SetSlug(ctx, issue.ID, resolvedSlug); err != nil {
			if isSlugCollision(err) {
				return Result{}, &SlugCollisionError{Slug: resolvedSlug}
			}
			return Result{}, fmt.Errorf("brain new: set slug %q on %s: %w", resolvedSlug, issue.ID, err)
		}
	}

	return Result{
		ID:   issue.ID,
		Kind: args.Kind,
		Slug: resolvedSlug,
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
