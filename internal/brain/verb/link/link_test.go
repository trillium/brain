package link_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/brain/verb"
	"github.com/steveyegge/beads/internal/brain/verb/link"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// recorderStore is a hand-rolled fake that implements link.LinkStore. It
// records every dependency passed to AddDependency and consults `existing`
// for GetIssue so the verb's existence-check path is exercisable without
// bringing up Dolt.
//
// existing maps issue ID → existence (true = present, missing/false =
// returns storage.ErrNotFound). addErr lets a test inject a storage write
// failure to verify the verb wraps it correctly. getErr lets a test
// inject a non-ErrNotFound storage read failure to verify the verb
// distinguishes "missing" from "transport failed".
type recorderStore struct {
	existing map[string]bool
	added    []*types.Dependency
	actors   []string
	addErr   error
	getErr   error
}

func (r *recorderStore) GetIssue(_ context.Context, id string) (*types.Issue, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	if r.existing[id] {
		return &types.Issue{ID: id}, nil
	}
	// Mirror the wrapping wording bd's storage layer uses so callers can
	// errors.Is(err, storage.ErrNotFound) the same way they would in prod.
	return nil, fmt.Errorf("%w: issue %s", storage.ErrNotFound, id)
}

func (r *recorderStore) AddDependency(_ context.Context, dep *types.Dependency, actor string) error {
	if r.addErr != nil {
		return r.addErr
	}
	// Take a shallow copy so a later mutation by the caller can't change
	// what the recorder remembers.
	clone := *dep
	r.added = append(r.added, &clone)
	r.actors = append(r.actors, actor)
	return nil
}

// Compile-time proof that the test-only recorder satisfies the same
// LinkStore seam production storage does. If the seam changes shape,
// this assertion catches it at build time, not in CI.
var _ link.LinkStore = (*recorderStore)(nil)

// Compile-time proof that link.Verb satisfies BrainVerb[Args, Result].
// Duplicated here (the engine file has the same assertion) so a test-only
// rename doesn't silently break the contract.
var _ verb.BrainVerb[link.Args, link.Result] = link.Verb{}

func newRecorder(existingIDs ...string) *recorderStore {
	m := make(map[string]bool, len(existingIDs))
	for _, id := range existingIDs {
		m[id] = true
	}
	return &recorderStore{existing: m}
}

// --- Happy paths --------------------------------------------------------

// TestRun_HappyPath_LearnedFrom exercises WHAT_IS_BRAIN.md § 4.2 scenario
// "link a new insight to the work that produced it":
// from-doc and to-doc exist; --learned-from resolves to EdgeType "learned-from";
// the dependency is inserted with type=learned-from and the from/to ids.
func TestRun_HappyPath_LearnedFrom(t *testing.T) {
	t.Parallel()

	rec := newRecorder("B-a7b3c", "B-217")
	v := link.New(rec, "tester")

	got, err := v.Run(context.Background(), link.Args{
		From:     "B-a7b3c",
		To:       "B-217",
		EdgeType: string(types.DepLearnedFrom),
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.From != "B-a7b3c" || got.To != "B-217" {
		t.Errorf("Result endpoints = (%q, %q), want (B-a7b3c, B-217)", got.From, got.To)
	}
	if got.EdgeType != string(types.DepLearnedFrom) {
		t.Errorf("Result.EdgeType = %q, want %q", got.EdgeType, types.DepLearnedFrom)
	}

	if len(rec.added) != 1 {
		t.Fatalf("recorder saw %d dependencies, want 1", len(rec.added))
	}
	dep := rec.added[0]
	if dep.IssueID != "B-a7b3c" {
		t.Errorf("dep.IssueID = %q, want %q", dep.IssueID, "B-a7b3c")
	}
	if dep.DependsOnID != "B-217" {
		t.Errorf("dep.DependsOnID = %q, want %q", dep.DependsOnID, "B-217")
	}
	if dep.Type != types.DepLearnedFrom {
		t.Errorf("dep.Type = %q, want %q", dep.Type, types.DepLearnedFrom)
	}
	if rec.actors[0] != "tester" {
		t.Errorf("AddDependency actor = %q, want %q", rec.actors[0], "tester")
	}
}

// TestRun_HappyPath_Extends exercises WHAT_IS_BRAIN.md § 4.2 scenario
// "extend a prior knowledge doc": same shape with EdgeType "extends".
func TestRun_HappyPath_Extends(t *testing.T) {
	t.Parallel()

	rec := newRecorder("B-a7b3c", "B-552a")
	v := link.New(rec, "tester")

	got, err := v.Run(context.Background(), link.Args{
		From:     "B-a7b3c",
		To:       "B-552a",
		EdgeType: string(types.DepExtends),
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.EdgeType != string(types.DepExtends) {
		t.Errorf("Result.EdgeType = %q, want %q", got.EdgeType, types.DepExtends)
	}
	if len(rec.added) != 1 {
		t.Fatalf("recorder saw %d dependencies, want 1", len(rec.added))
	}
	if rec.added[0].Type != types.DepExtends {
		t.Errorf("dep.Type = %q, want %q (the brain-only edge type from types.go)",
			rec.added[0].Type, types.DepExtends)
	}
}

// TestRun_HappyPath_FallthroughType exercises WHAT_IS_BRAIN.md § 4.2 scenario
// "link using bd's edge types from the brain surface": EdgeType "blocks" is
// not a brain-only flag but the --type <name> fallthrough must accept any
// well-known bd dependency type. The resulting row uses type=blocks.
func TestRun_HappyPath_FallthroughType(t *testing.T) {
	t.Parallel()

	rec := newRecorder("B-218", "B-219")
	v := link.New(rec, "tester")

	got, err := v.Run(context.Background(), link.Args{
		From:     "B-218",
		To:       "B-219",
		EdgeType: string(types.DepBlocks),
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.EdgeType != string(types.DepBlocks) {
		t.Errorf("Result.EdgeType = %q, want %q", got.EdgeType, types.DepBlocks)
	}
	if len(rec.added) != 1 {
		t.Fatalf("recorder saw %d dependencies, want 1", len(rec.added))
	}
	if rec.added[0].Type != types.DepBlocks {
		t.Errorf("dep.Type = %q, want %q", rec.added[0].Type, types.DepBlocks)
	}
}

// TestRun_HappyPath_Related exercises the third brain-flagged edge type —
// `--related` falls through to bd's existing `related` edge type, with no
// new schema. Locks in that nothing about routing the brain flag changes
// the row that lands.
func TestRun_HappyPath_Related(t *testing.T) {
	t.Parallel()

	rec := newRecorder("B-a7b3c", "B-100")
	v := link.New(rec, "tester")

	if _, err := v.Run(context.Background(), link.Args{
		From:     "B-a7b3c",
		To:       "B-100",
		EdgeType: string(types.DepRelated),
	}); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(rec.added) != 1 {
		t.Fatalf("recorder saw %d dependencies, want 1", len(rec.added))
	}
	if rec.added[0].Type != types.DepRelated {
		t.Errorf("dep.Type = %q, want %q (bd's existing related edge type)",
			rec.added[0].Type, types.DepRelated)
	}
}

// TestRun_HappyPath_SelfLink documents the deliberate decision: brain link
// permits self-edges (from == to) because bd's storage layer does not
// reject them and brain has no independent knowledge-graph reason to.
// `extends`/`learned-from` on a doc pointing at itself is a meaningful
// construct (e.g. an updated revision of an idea referencing its own
// prior state).
//
// If bd later adds a self-edge guard at the storage layer, this test
// flips to expecting an error — and the failure points at the storage
// change as the root cause, not at the brain verb's policy.
func TestRun_HappyPath_SelfLink(t *testing.T) {
	t.Parallel()

	rec := newRecorder("B-self")
	v := link.New(rec, "tester")

	if _, err := v.Run(context.Background(), link.Args{
		From:     "B-self",
		To:       "B-self",
		EdgeType: string(types.DepExtends),
	}); err != nil {
		t.Fatalf("Run() error = %v, want nil (brain link permits self-edges; bd storage does not reject them)", err)
	}
	if len(rec.added) != 1 {
		t.Fatalf("recorder saw %d dependencies, want 1", len(rec.added))
	}
	if rec.added[0].IssueID != rec.added[0].DependsOnID {
		t.Errorf("self-edge endpoints diverged: %s → %s", rec.added[0].IssueID, rec.added[0].DependsOnID)
	}
}

// --- Validation failures -----------------------------------------------

// TestRun_MissingTarget exercises WHAT_IS_BRAIN.md § 4.2 scenario
// "linking nonexistent ID" — target side. From-doc exists, to-doc does
// not. The error must say "target brain doc <id> not found" (distinct from
// the source-missing wording) and NO row may be inserted.
func TestRun_MissingTarget(t *testing.T) {
	t.Parallel()

	rec := newRecorder("B-a7b3c") // target intentionally absent
	v := link.New(rec, "tester")

	_, err := v.Run(context.Background(), link.Args{
		From:     "B-a7b3c",
		To:       "B-DOESNT",
		EdgeType: string(types.DepRelated),
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a target-not-found error")
	}
	want := "target brain doc B-DOESNT not found"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q must contain %q (the exact substring the spec requires)", err.Error(), want)
	}
	if len(rec.added) != 0 {
		t.Fatalf("recorder saw %d dependencies, want 0 (existence check must precede storage write)",
			len(rec.added))
	}
}

// TestRun_MissingSource is the source-side mirror of TestRun_MissingTarget.
// The spec requires distinct wording for the two sides — bd's storage
// layer says "issue X not found" for both, so the verb's existence
// probe is the only place this distinction is enforced.
func TestRun_MissingSource(t *testing.T) {
	t.Parallel()

	rec := newRecorder("B-target") // source intentionally absent
	v := link.New(rec, "tester")

	_, err := v.Run(context.Background(), link.Args{
		From:     "B-GONE",
		To:       "B-target",
		EdgeType: string(types.DepRelated),
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a from-not-found error")
	}
	want := "from brain doc B-GONE not found"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q must contain %q (the source-side wording the spec requires)", err.Error(), want)
	}
	if len(rec.added) != 0 {
		t.Fatalf("recorder saw %d dependencies, want 0", len(rec.added))
	}
}

// TestRun_EmptyEdgeType is an orthogonal guard: the Cobra wrapper resolves
// the mutex flags (--extends, --learned-from, --related, --type <name>)
// into a single EdgeType string. If a caller constructs Args by hand
// without setting EdgeType, the verb must refuse before touching storage
// — and the error must name the recovery flags so the user can fix it
// without reading source.
func TestRun_EmptyEdgeType(t *testing.T) {
	t.Parallel()

	rec := newRecorder("B-from", "B-to")
	v := link.New(rec, "tester")

	_, err := v.Run(context.Background(), link.Args{
		From:     "B-from",
		To:       "B-to",
		EdgeType: "",
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a required-edge-type error")
	}
	msg := err.Error()
	for _, needle := range []string{"--extends", "--learned-from", "--related", "--type"} {
		if !strings.Contains(msg, needle) {
			t.Errorf("error %q missing %q — the recovery hint must list the four mutex flags",
				msg, needle)
		}
	}
	if len(rec.added) != 0 {
		t.Fatalf("recorder saw %d dependencies, want 0", len(rec.added))
	}
}

// TestRun_EmptyFrom is an orthogonal Cobra-bypass guard: the wrapper uses
// ExactArgs(2) so this is normally caught upstream, but a caller that
// constructs Args by hand can still produce an empty From. The verb's
// own guard refuses to write a doc-less edge.
func TestRun_EmptyFrom(t *testing.T) {
	t.Parallel()

	rec := newRecorder("B-to")
	v := link.New(rec, "tester")

	_, err := v.Run(context.Background(), link.Args{
		From:     "",
		To:       "B-to",
		EdgeType: string(types.DepRelated),
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a required-from error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "from") {
		t.Errorf("error %q must mention 'from' to point at the missing field", err.Error())
	}
	if len(rec.added) != 0 {
		t.Fatalf("recorder saw %d dependencies, want 0", len(rec.added))
	}
}

// TestRun_EmptyTo is the target-side mirror of TestRun_EmptyFrom.
func TestRun_EmptyTo(t *testing.T) {
	t.Parallel()

	rec := newRecorder("B-from")
	v := link.New(rec, "tester")

	_, err := v.Run(context.Background(), link.Args{
		From:     "B-from",
		To:       "",
		EdgeType: string(types.DepRelated),
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a required-target error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "target") {
		t.Errorf("error %q must mention 'target' to point at the missing field", err.Error())
	}
	if len(rec.added) != 0 {
		t.Fatalf("recorder saw %d dependencies, want 0", len(rec.added))
	}
}

// TestRun_InvalidEdgeType drives an edge type that exceeds the 50-character
// cap in types.DependencyType.IsValid(). The wrapper would normally not
// produce this via the mutex flags, but `--type <name>` is fallthrough
// and accepts arbitrary input; the verb's guard catches it before the
// existence probes so no GetIssue / AddDependency calls happen.
func TestRun_InvalidEdgeType(t *testing.T) {
	t.Parallel()

	rec := newRecorder("B-from", "B-to")
	v := link.New(rec, "tester")

	bogus := strings.Repeat("x", 51) // 51 chars > 50-char cap → IsValid false
	_, err := v.Run(context.Background(), link.Args{
		From:     "B-from",
		To:       "B-to",
		EdgeType: bogus,
	})
	if err == nil {
		t.Fatal("Run() error = nil, want an invalid-edge-type error")
	}
	// The error should name well-known types so the user can recover.
	// "extends" and "learned-from" are the brain-flavoured ones and are
	// guaranteed to be in the list (added in types.go for v0.3).
	for _, needle := range []string{"extends", "learned-from"} {
		if !strings.Contains(err.Error(), needle) {
			t.Errorf("error %q missing %q — recovery hint must list well-known types",
				err.Error(), needle)
		}
	}
	if len(rec.added) != 0 {
		t.Fatalf("recorder saw %d dependencies, want 0 (edge-type validation must precede storage write)",
			len(rec.added))
	}
}

// --- Plumbing guards ---------------------------------------------------

// TestRun_NilStoreReturnsError protects callers who forget to wire a store.
// Without this guard the verb would nil-deref on GetIssue — surfacing the
// misconfiguration as a real error keeps the Cobra wrapper's error
// rendering uniform.
func TestRun_NilStoreReturnsError(t *testing.T) {
	t.Parallel()

	v := link.New(nil, "tester")
	_, err := v.Run(context.Background(), link.Args{
		From:     "B-from",
		To:       "B-to",
		EdgeType: string(types.DepRelated),
	})
	if err == nil {
		t.Fatal("Run() error = nil, want an unconfigured-storage error")
	}
	if !strings.Contains(err.Error(), "storage") {
		t.Errorf("error %q must mention 'storage' to point at the misconfiguration", err.Error())
	}
}

// TestRun_StorageErrorIsWrapped ensures a storage write failure surfaces
// with context (edge type + the original error) rather than as a bare
// "add failed". The %w wrap is the contract — bulk callers must be able
// to errors.Is the underlying sentinel.
func TestRun_StorageErrorIsWrapped(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("dolt write failed: connection refused")
	rec := newRecorder("B-from", "B-to")
	rec.addErr = sentinel
	v := link.New(rec, "tester")

	_, err := v.Run(context.Background(), link.Args{
		From:     "B-from",
		To:       "B-to",
		EdgeType: string(types.DepLearnedFrom),
	})
	if err == nil {
		t.Fatal("Run() error = nil, want the wrapped storage error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("errors.Is(err, sentinel) = false; verb must wrap with %%w so callers can unwrap")
	}
	if !strings.Contains(err.Error(), "learned-from") {
		t.Errorf("error %q must mention edge type %q for caller diagnosis", err.Error(), "learned-from")
	}
}

// TestRun_GetIssueTransportErrorIsWrapped ensures the existence-probe
// distinguishes "missing" (storage.ErrNotFound, surfaced as the brain
// spec's wording) from "lookup failed" (any other error, surfaced
// wrapped so callers can diagnose transport issues without losing the
// originating error).
func TestRun_GetIssueTransportErrorIsWrapped(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("dolt: connection lost mid-query")
	rec := newRecorder("B-from", "B-to")
	rec.getErr = sentinel
	v := link.New(rec, "tester")

	_, err := v.Run(context.Background(), link.Args{
		From:     "B-from",
		To:       "B-to",
		EdgeType: string(types.DepRelated),
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a wrapped lookup error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("errors.Is(err, sentinel) = false; verb must wrap GetIssue failures with %%w")
	}
	// Must NOT be misclassified as the brain-spec "not found" wording —
	// that is reserved for storage.ErrNotFound only.
	if strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q misclassifies transport failure as not-found", err.Error())
	}
	if len(rec.added) != 0 {
		t.Fatalf("recorder saw %d dependencies, want 0 (existence-probe failure must abort)",
			len(rec.added))
	}
}

// TestVerbName pins the seam contract: Name() must match the first token
// of the Cobra `Use:` field in cmd/bd/brain_link.go. If someone renames
// either side without the other, this test catches it.
func TestVerbName(t *testing.T) {
	t.Parallel()
	v := link.Verb{}
	if got := v.Name(); got != "link" {
		t.Fatalf("Verb.Name() = %q, want %q", got, "link")
	}
}
