package newverb_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/brain/verb"
	newverb "github.com/steveyegge/beads/internal/brain/verb/new"
	"github.com/steveyegge/beads/internal/brain/verb/slug"
	"github.com/steveyegge/beads/internal/types"
)

// recorderStore is a hand-rolled fake that implements newverb.IssueCreator.
// It records every issue passed to CreateIssue and assigns a deterministic
// fake ID so the verb's "read back issue.ID for Result.ID" contract is
// exercisable without bringing up Dolt.
//
// nextID is a counter — each CreateIssue call assigns "B-<n>" where <n>
// starts at 1. createErr lets a test inject a storage failure to verify
// the verb wraps it correctly.
//
// slugs records every (id, slug) pair the verb writes via SetSlug.
// setSlugErr lets a test inject a SetSlug failure (e.g. to simulate a
// Dolt uniqueness collision).
type recorderStore struct {
	created    []*types.Issue
	actors     []string
	nextID     int
	createErr  error
	slugs      map[string]string
	setSlugErr error
}

func (r *recorderStore) CreateIssue(_ context.Context, issue *types.Issue, actor string) error {
	if r.createErr != nil {
		return r.createErr
	}
	r.nextID++
	if issue.ID == "" {
		// Honour the IDPrefix the verb sets for kind=isa so tests can
		// assert on the "B-isa-..." shape without bringing up storage.
		if issue.IDPrefix != "" {
			issue.ID = "B-" + issue.IDPrefix + "-" + idSuffix(r.nextID)
		} else {
			issue.ID = idFor(r.nextID)
		}
	}
	// Take a shallow copy so a later mutation by the caller can't change
	// what the recorder remembers.
	clone := *issue
	r.created = append(r.created, &clone)
	r.actors = append(r.actors, actor)
	return nil
}

func (r *recorderStore) SetSlug(_ context.Context, id, slug string) error {
	if r.setSlugErr != nil {
		return r.setSlugErr
	}
	if r.slugs == nil {
		r.slugs = map[string]string{}
	}
	r.slugs[id] = slug
	return nil
}

func idSuffix(n int) string {
	const hex = "abcdef0123456789"
	return string([]byte{
		hex[n%16],
		hex[(n>>4)%16],
		hex[(n>>8)%16],
		hex[(n>>12)%16],
		hex[(n>>16)%16],
	})
}

func idFor(n int) string {
	// Tests assert on the prefix "B-", not the exact suffix, so any
	// deterministic non-empty suffix works. A short hex-ish stamp keeps
	// the look close to real brain IDs ("B-a7b3c") without pulling in
	// the hash generator.
	const hex = "abcdef0123456789"
	suffix := []byte{
		hex[n%16],
		hex[(n>>4)%16],
		hex[(n>>8)%16],
		hex[(n>>12)%16],
		hex[(n>>16)%16],
	}
	return "B-" + string(suffix)
}

// Compile-time proof that the test-only recorder satisfies the same
// IssueCreator seam production storage does. If the seam changes shape,
// this assertion catches it at build time, not in CI.
var _ newverb.IssueCreator = (*recorderStore)(nil)

// Compile-time proof that newverb.Verb satisfies BrainVerb[Args, Result].
// Duplicated here (the engine file has the same assertion) so a test-only
// rename doesn't silently break the contract.
var _ verb.BrainVerb[newverb.Args, newverb.Result] = newverb.Verb{}

// --- Happy paths --------------------------------------------------------

// TestRun_HappyPath_Task exercises WHAT_IS_BRAIN.md § 4.1 scenario
// "create a task from a phone": kind=task, status=open, ID allocated.
func TestRun_HappyPath_Task(t *testing.T) {
	t.Parallel()

	rec := &recorderStore{}
	v := newverb.New(rec, "tester")

	got, err := v.Run(context.Background(), newverb.Args{
		Kind:  string(types.TypeTask),
		Title: "ship the FTS5 indexer",
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.ID == "" {
		t.Fatal("Result.ID is empty; storage was supposed to allocate one")
	}
	if got.Kind != string(types.TypeTask) {
		t.Errorf("Result.Kind = %q, want %q", got.Kind, types.TypeTask)
	}

	if len(rec.created) != 1 {
		t.Fatalf("recorder saw %d issues, want 1", len(rec.created))
	}
	issue := rec.created[0]
	if issue.IssueType != types.TypeTask {
		t.Errorf("issue.IssueType = %q, want %q", issue.IssueType, types.TypeTask)
	}
	if issue.Title != "ship the FTS5 indexer" {
		t.Errorf("issue.Title = %q, want %q", issue.Title, "ship the FTS5 indexer")
	}
	if issue.Status != types.StatusOpen {
		t.Errorf("issue.Status = %q, want %q (task brain docs default to open per § 4.1)",
			issue.Status, types.StatusOpen)
	}
	if rec.actors[0] != "tester" {
		t.Errorf("CreateIssue actor = %q, want %q", rec.actors[0], "tester")
	}
}

// TestRun_HappyPath_Knowledge exercises WHAT_IS_BRAIN.md § 4.1 scenario
// "create a new knowledge doc mid-conversation".
func TestRun_HappyPath_Knowledge(t *testing.T) {
	t.Parallel()

	rec := &recorderStore{}
	v := newverb.New(rec, "tester")

	got, err := v.Run(context.Background(), newverb.Args{
		Kind:  string(types.TypeKnowledge),
		Title: "Dolt FK constraints are lazy until commit",
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.Kind != string(types.TypeKnowledge) {
		t.Errorf("Result.Kind = %q, want %q", got.Kind, types.TypeKnowledge)
	}
	if len(rec.created) != 1 {
		t.Fatalf("recorder saw %d issues, want 1", len(rec.created))
	}
	if rec.created[0].IssueType != types.TypeKnowledge {
		t.Errorf("issue.IssueType = %q, want %q (kind tag rides on the existing issue_type column)",
			rec.created[0].IssueType, types.TypeKnowledge)
	}
}

// TestRun_HappyPath_Both exercises WHAT_IS_BRAIN.md § 4.1 scenario
// 'create a "both" because the work and the lesson are inseparable'.
func TestRun_HappyPath_Both(t *testing.T) {
	t.Parallel()

	rec := &recorderStore{}
	v := newverb.New(rec, "tester")

	got, err := v.Run(context.Background(), newverb.Args{
		Kind:  string(types.TypeBoth),
		Title: "Friday cache bug + postmortem",
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.Kind != string(types.TypeBoth) {
		t.Errorf("Result.Kind = %q, want %q", got.Kind, types.TypeBoth)
	}
	if rec.created[0].IssueType != types.TypeBoth {
		t.Errorf("issue.IssueType = %q, want %q", rec.created[0].IssueType, types.TypeBoth)
	}
	// "both" is task-shaped for ready queues, per § 4.1 — so it must
	// default to open just like task does.
	if rec.created[0].Status != types.StatusOpen {
		t.Errorf("issue.Status = %q, want %q", rec.created[0].Status, types.StatusOpen)
	}
}

// --- Validation failures -----------------------------------------------

// TestRun_InvalidKind exercises WHAT_IS_BRAIN.md § 4.1 scenario
// "invalid kind value": the typed-enum guard rejects "note" / "junk".
// The error must mention all three accepted values so the user can
// recover without reading code.
func TestRun_InvalidKind(t *testing.T) {
	t.Parallel()

	rec := &recorderStore{}
	v := newverb.New(rec, "tester")

	_, err := v.Run(context.Background(), newverb.Args{
		Kind:  "junk",
		Title: "should never be written",
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a kind-validation error")
	}
	msg := err.Error()
	for _, needle := range []string{"task", "knowledge", "both"} {
		if !strings.Contains(msg, needle) {
			t.Errorf("error %q missing %q — the recovery hint must list all three valid kinds",
				msg, needle)
		}
	}
	if len(rec.created) != 0 {
		t.Fatalf("recorder saw %d issues, want 0 (validation must precede the storage write)",
			len(rec.created))
	}
}

// TestRun_EmptyKind exercises WHAT_IS_BRAIN.md § 4.1 scenario
// "missing kind argument": empty Kind is rejected with a hint that
// kind is required. The Cobra layer would normally catch this via
// cobra.ExactArgs(2), but the verb's own guard is the modularity
// guarantee — if someone constructs Args by hand, the verb still
// refuses to write a doc with no kind.
func TestRun_EmptyKind(t *testing.T) {
	t.Parallel()

	rec := &recorderStore{}
	v := newverb.New(rec, "tester")

	_, err := v.Run(context.Background(), newverb.Args{
		Kind:  "",
		Title: "title is fine but kind is empty",
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a required-kind error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "kind") {
		t.Errorf("error %q must mention 'kind'", err.Error())
	}
	if !strings.Contains(strings.ToLower(err.Error()), "required") {
		t.Errorf("error %q must mention 'required' so the user knows the field is mandatory",
			err.Error())
	}
	if len(rec.created) != 0 {
		t.Fatalf("recorder saw %d issues, want 0", len(rec.created))
	}
}

// TestRun_EmptyTitle is an orthogonal-but-necessary guard: bd's storage
// layer would eventually reject an empty title via Issue.Validate
// ("title is required"), but the verb catches it before any storage
// call so the error path is fast and clear and so dry-run-style
// callers don't allocate an ID for a doc that can never be written.
func TestRun_EmptyTitle(t *testing.T) {
	t.Parallel()

	rec := &recorderStore{}
	v := newverb.New(rec, "tester")

	_, err := v.Run(context.Background(), newverb.Args{
		Kind:  string(types.TypeTask),
		Title: "",
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a required-title error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "title") {
		t.Errorf("error %q must mention 'title'", err.Error())
	}
	if len(rec.created) != 0 {
		t.Fatalf("recorder saw %d issues, want 0 (title validation must precede storage write)",
			len(rec.created))
	}
}

// --- Body passthrough --------------------------------------------------

// TestRun_BodyPassthrough proves the optional --body flag lands in the
// existing description column. The exfiltrator (ISC-117-121, not yet
// built) will eventually mirror this body into entries/{kind}/{id}.md;
// this test guards the upstream end of that pipe.
func TestRun_BodyPassthrough(t *testing.T) {
	t.Parallel()

	rec := &recorderStore{}
	v := newverb.New(rec, "tester")

	_, err := v.Run(context.Background(), newverb.Args{
		Kind:  string(types.TypeKnowledge),
		Title: "Dolt FK constraints are lazy until commit",
		Body:  "hello — the constraint is checked at commit time, not insert time.",
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(rec.created) != 1 {
		t.Fatalf("recorder saw %d issues, want 1", len(rec.created))
	}
	if !strings.Contains(rec.created[0].Description, "hello") {
		t.Errorf("issue.Description = %q, want it to contain %q", rec.created[0].Description, "hello")
	}
}

// --- Plumbing guards ---------------------------------------------------

// TestRun_NilStoreReturnsError protects callers who forget to wire a
// store. Without this guard the verb would nil-deref on the first
// CreateIssue call — surfacing the misconfiguration as a real error
// keeps the Cobra wrapper's error rendering uniform.
func TestRun_NilStoreReturnsError(t *testing.T) {
	t.Parallel()

	v := newverb.New(nil, "tester")
	_, err := v.Run(context.Background(), newverb.Args{
		Kind:  string(types.TypeTask),
		Title: "irrelevant",
	})
	if err == nil {
		t.Fatal("Run() error = nil, want an unconfigured-storage error")
	}
	if !strings.Contains(err.Error(), "storage") {
		t.Errorf("error %q must mention 'storage' to point at the misconfiguration", err.Error())
	}
}

// TestRun_StorageErrorIsWrapped ensures a storage failure surfaces
// with context (kind + the original error) rather than as a bare
// "create failed". The %w wrap is the contract.
func TestRun_StorageErrorIsWrapped(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("dolt write failed: connection refused")
	rec := &recorderStore{createErr: sentinel}
	v := newverb.New(rec, "tester")

	_, err := v.Run(context.Background(), newverb.Args{
		Kind:  string(types.TypeTask),
		Title: "doesn't matter",
	})
	if err == nil {
		t.Fatal("Run() error = nil, want the wrapped storage error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("errors.Is(err, sentinel) = false; verb must wrap with %%w so callers can unwrap")
	}
	if !strings.Contains(err.Error(), "task") {
		t.Errorf("error %q must mention kind %q for caller diagnosis", err.Error(), "task")
	}
}

// TestVerbName pins the seam contract: Name() must match the first
// token of the Cobra `Use:` field in cmd/bd/brain_new.go. If someone
// renames either side without the other, this test catches it.
func TestVerbName(t *testing.T) {
	t.Parallel()
	v := newverb.Verb{}
	if got := v.Name(); got != "new" {
		t.Fatalf("Verb.Name() = %q, want %q", got, "new")
	}
}

// TestValidKindsContents is a guard against silent additions/removals
// of the accepted kind set. If a future tranche extends the set, this
// test must be updated explicitly — that prevents drift between the
// verb, types.IsValid(), and the spec.
func TestValidKindsContents(t *testing.T) {
	t.Parallel()

	got := newverb.ValidKinds()
	want := []string{"task", "knowledge", "both", "isa"}
	if len(got) != len(want) {
		t.Fatalf("ValidKinds() len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("ValidKinds()[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// --- ISA kind + slug ---------------------------------------------------

// TestRun_ISA_ExplicitSlug exercises ISC-1 + ISC-3: kind=isa with a
// caller-supplied --slug. The verb must set IDPrefix=isa on the issue
// (so storage allocates "<prefix>-isa-XXXXX") and call SetSlug with
// the supplied slug.
func TestRun_ISA_ExplicitSlug(t *testing.T) {
	t.Parallel()

	rec := &recorderStore{}
	v := newverb.New(rec, "tester")

	got, err := v.Run(context.Background(), newverb.Args{
		Kind:  string(types.TypeISA),
		Title: "Brain as ISA Substrate",
		Slug:  "brain-as-isa-substrate",
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.Kind != string(types.TypeISA) {
		t.Errorf("Result.Kind = %q, want %q", got.Kind, types.TypeISA)
	}
	if got.Slug != "brain-as-isa-substrate" {
		t.Errorf("Result.Slug = %q, want %q", got.Slug, "brain-as-isa-substrate")
	}
	if len(rec.created) != 1 {
		t.Fatalf("recorder saw %d issues, want 1", len(rec.created))
	}
	if rec.created[0].IDPrefix != "isa" {
		t.Errorf("issue.IDPrefix = %q, want %q (kind=isa must set IDPrefix=isa for ISC-2)",
			rec.created[0].IDPrefix, "isa")
	}
	if rec.created[0].IssueType != types.TypeISA {
		t.Errorf("issue.IssueType = %q, want %q", rec.created[0].IssueType, types.TypeISA)
	}
	if rec.slugs[got.ID] != "brain-as-isa-substrate" {
		t.Errorf("SetSlug recorded %q, want %q", rec.slugs[got.ID], "brain-as-isa-substrate")
	}
}

// TestRun_ISA_AutoSlugFromTitle exercises ISC-5: kind=isa with no
// --slug — the verb must auto-generate one via slug.Auto.
func TestRun_ISA_AutoSlugFromTitle(t *testing.T) {
	t.Parallel()

	rec := &recorderStore{}
	v := newverb.New(rec, "tester")

	got, err := v.Run(context.Background(), newverb.Args{
		Kind:  string(types.TypeISA),
		Title: "Foo Bar Baz",
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.Slug != "foo-bar-baz" {
		t.Errorf("Result.Slug = %q, want %q (auto-slug from title)", got.Slug, "foo-bar-baz")
	}
	if rec.slugs[got.ID] != "foo-bar-baz" {
		t.Errorf("SetSlug recorded %q, want %q", rec.slugs[got.ID], "foo-bar-baz")
	}
}

// TestRun_ISA_TitleWithNoAlphanumericsFailsWithHint exercises ISC-5
// fallback: a title that yields no alphanumerics must fail with an
// error pointing the user at --slug.
func TestRun_ISA_TitleWithNoAlphanumericsFailsWithHint(t *testing.T) {
	t.Parallel()

	rec := &recorderStore{}
	v := newverb.New(rec, "tester")

	_, err := v.Run(context.Background(), newverb.Args{
		Kind:  string(types.TypeISA),
		Title: "!!!",
	})
	if err == nil {
		t.Fatal("Run() error = nil, want an auto-slug failure")
	}
	if !strings.Contains(err.Error(), "--slug") {
		t.Errorf("error %q must mention '--slug' so the user knows the recovery", err.Error())
	}
	if len(rec.created) != 0 {
		t.Fatalf("recorder saw %d issues, want 0 (slug-failure must precede storage write)",
			len(rec.created))
	}
}

// TestRun_ISA_InvalidSlugFailsWithValidationError exercises ISC-3
// validation: an invalid --slug must return *slug.ValidationError
// (the wrapper checks for that type to exit with code 2).
func TestRun_ISA_InvalidSlugFailsWithValidationError(t *testing.T) {
	t.Parallel()

	rec := &recorderStore{}
	v := newverb.New(rec, "tester")

	_, err := v.Run(context.Background(), newverb.Args{
		Kind:  string(types.TypeISA),
		Title: "Whatever",
		Slug:  "BadSlug",
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a ValidationError")
	}
	var vErr *slug.ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("err = %v (type %T), want *slug.ValidationError", err, err)
	}
	if !strings.Contains(err.Error(), "BadSlug") {
		t.Errorf("error %q must include the offending slug", err.Error())
	}
	if len(rec.created) != 0 {
		t.Fatalf("recorder saw %d issues, want 0", len(rec.created))
	}
}

// TestRun_ISA_SlugCollisionReturnsTypedError exercises ISC-4: when
// SetSlug returns a uniqueness-collision error from the storage
// backend, the verb must wrap it as *SlugCollisionError so the wrapper
// can exit 2 with a clear message.
func TestRun_ISA_SlugCollisionReturnsTypedError(t *testing.T) {
	t.Parallel()

	rec := &recorderStore{
		setSlugErr: errors.New("Duplicate entry 'foo-bar' for key 'slug_unique'"),
	}
	v := newverb.New(rec, "tester")

	_, err := v.Run(context.Background(), newverb.Args{
		Kind:  string(types.TypeISA),
		Title: "Whatever",
		Slug:  "foo-bar",
	})
	if err == nil {
		t.Fatal("Run() error = nil, want a SlugCollisionError")
	}
	var cErr *newverb.SlugCollisionError
	if !errors.As(err, &cErr) {
		t.Fatalf("err = %v (type %T), want *SlugCollisionError", err, err)
	}
	if cErr.Slug != "foo-bar" {
		t.Errorf("SlugCollisionError.Slug = %q, want %q", cErr.Slug, "foo-bar")
	}
	if !strings.Contains(err.Error(), "foo-bar") {
		t.Errorf("error %q must include the conflicting slug", err.Error())
	}
}

// TestRun_Task_WithSlug exercises symmetry: kind=task with --slug
// should persist the slug too (slug is not isa-exclusive at the
// storage layer per migration 0050).
func TestRun_Task_WithSlug(t *testing.T) {
	t.Parallel()

	rec := &recorderStore{}
	v := newverb.New(rec, "tester")

	got, err := v.Run(context.Background(), newverb.Args{
		Kind:  string(types.TypeTask),
		Title: "ship the FTS5 indexer",
		Slug:  "ship-fts5",
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.Slug != "ship-fts5" {
		t.Errorf("Result.Slug = %q, want %q", got.Slug, "ship-fts5")
	}
	if rec.created[0].IDPrefix != "" {
		t.Errorf("issue.IDPrefix = %q, want \"\" (non-isa kinds must not set isa IDPrefix)",
			rec.created[0].IDPrefix)
	}
	if rec.slugs[got.ID] != "ship-fts5" {
		t.Errorf("SetSlug recorded %q, want %q", rec.slugs[got.ID], "ship-fts5")
	}
}

// TestRun_Task_NoSlug confirms backwards compatibility: kind=task with
// no --slug must not call SetSlug at all and must leave Result.Slug
// empty.
func TestRun_Task_NoSlug(t *testing.T) {
	t.Parallel()

	rec := &recorderStore{}
	v := newverb.New(rec, "tester")

	got, err := v.Run(context.Background(), newverb.Args{
		Kind:  string(types.TypeTask),
		Title: "no slug here",
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.Slug != "" {
		t.Errorf("Result.Slug = %q, want \"\" (no --slug means no slug write)", got.Slug)
	}
	if len(rec.slugs) != 0 {
		t.Errorf("SetSlug was called for non-isa kind with empty --slug: %v", rec.slugs)
	}
}

// TestRun_Knowledge_EmptySlug confirms that an explicitly-empty slug
// for a non-isa kind is a no-op (no SetSlug call, no validation).
func TestRun_Knowledge_EmptySlug(t *testing.T) {
	t.Parallel()

	rec := &recorderStore{}
	v := newverb.New(rec, "tester")

	_, err := v.Run(context.Background(), newverb.Args{
		Kind:  string(types.TypeKnowledge),
		Title: "no slug",
		Slug:  "",
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(rec.slugs) != 0 {
		t.Errorf("SetSlug was called with empty slug for non-isa kind: %v", rec.slugs)
	}
}

// TestValidKindsReturnsCopy proves ValidKinds defends against caller
// mutation. Without a copy, a misbehaving caller could mutate the
// package-level slice and corrupt every subsequent call.
func TestValidKindsReturnsCopy(t *testing.T) {
	t.Parallel()

	a := newverb.ValidKinds()
	a[0] = "MUTATED"
	b := newverb.ValidKinds()
	if b[0] == "MUTATED" {
		t.Fatal("ValidKinds() returns the package slice itself; callers can corrupt internal state")
	}
}
