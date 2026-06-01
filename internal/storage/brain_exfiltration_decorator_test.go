package storage

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/steveyegge/beads/internal/brain/exfiltrator"
	"github.com/steveyegge/beads/internal/types"
)

// recordingExf is a fake exfiltrator.Exfiltrator that records calls so
// tests can verify the decorator triggered Render/Remove correctly
// without touching disk.
type recordingExf struct {
	mu        sync.Mutex
	renders   []*types.Issue
	removes   []removeCall
	renderErr error
	removeErr error
}

type removeCall struct {
	ID   string
	Kind types.IssueType
	Slug string
}

func (r *recordingExf) Render(_ context.Context, issue *types.Issue) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.renderErr != nil {
		return r.renderErr
	}
	clone := *issue
	r.renders = append(r.renders, &clone)
	return nil
}

func (r *recordingExf) Remove(_ context.Context, id string, kind types.IssueType, slug string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.removeErr != nil {
		return r.removeErr
	}
	r.removes = append(r.removes, removeCall{ID: id, Kind: kind, Slug: slug})
	return nil
}

func (r *recordingExf) renderCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.renders)
}

func (r *recordingExf) removeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.removes)
}

func (r *recordingExf) lastRender() *types.Issue {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.renders) == 0 {
		return nil
	}
	return r.renders[len(r.renders)-1]
}

// fakeBrainStore is a minimal DoltStorage stub backed by an in-memory
// map. Lifted from the hook_decorator_internal_test.go fakeHookStore
// pattern. Only the methods exercised by the decorator are implemented;
// everything else falls through to the embedded nil DoltStorage and
// will panic if called — which is the desired behavior for an
// unintended call path.
type fakeBrainStore struct {
	DoltStorage
	mu       sync.Mutex
	issues   map[string]*types.Issue
	failOn   string // method name to fail on, "" means never fail
	failWith error
}

func newFakeBrainStore() *fakeBrainStore {
	return &fakeBrainStore{issues: map[string]*types.Issue{}}
}

func (s *fakeBrainStore) shouldFail(method string) error {
	if s.failOn == method {
		return s.failWith
	}
	return nil
}

func (s *fakeBrainStore) CreateIssue(_ context.Context, issue *types.Issue, _ string) error {
	if err := s.shouldFail("CreateIssue"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[issue.ID] = cloneIssueForHook(issue)
	return nil
}

func (s *fakeBrainStore) CreateIssues(_ context.Context, issues []*types.Issue, _ string) error {
	if err := s.shouldFail("CreateIssues"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, issue := range issues {
		s.issues[issue.ID] = cloneIssueForHook(issue)
	}
	return nil
}

func (s *fakeBrainStore) GetIssue(_ context.Context, id string) (*types.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	issue, ok := s.issues[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return cloneIssueForHook(issue), nil
}

func (s *fakeBrainStore) UpdateIssue(_ context.Context, id string, updates map[string]interface{}, _ string) error {
	if err := s.shouldFail("UpdateIssue"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	issue, ok := s.issues[id]
	if !ok {
		return errors.New("not found")
	}
	if v, ok := updates["title"].(string); ok {
		issue.Title = v
	}
	if v, ok := updates["description"].(string); ok {
		issue.Description = v
	}
	if v, ok := updates["issue_type"].(string); ok {
		issue.IssueType = types.IssueType(v)
	}
	if v, ok := updates["status"].(string); ok {
		issue.Status = types.Status(v)
	}
	return nil
}

func (s *fakeBrainStore) ReopenIssue(_ context.Context, id string, _ string, _ string) error {
	if err := s.shouldFail("ReopenIssue"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if issue, ok := s.issues[id]; ok {
		issue.Status = types.StatusOpen
		return nil
	}
	return errors.New("not found")
}

func (s *fakeBrainStore) UpdateIssueType(_ context.Context, id string, issueType string, _ string) error {
	if err := s.shouldFail("UpdateIssueType"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if issue, ok := s.issues[id]; ok {
		issue.IssueType = types.IssueType(issueType)
		return nil
	}
	return errors.New("not found")
}

func (s *fakeBrainStore) CloseIssue(_ context.Context, id string, _ string, _ string, _ string) error {
	if err := s.shouldFail("CloseIssue"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if issue, ok := s.issues[id]; ok {
		issue.Status = types.StatusClosed
		return nil
	}
	return errors.New("not found")
}

func (s *fakeBrainStore) AddDependency(_ context.Context, dep *types.Dependency, _ string) error {
	if err := s.shouldFail("AddDependency"); err != nil {
		return err
	}
	return nil
}

func (s *fakeBrainStore) RemoveDependency(_ context.Context, _, _, _ string) error {
	if err := s.shouldFail("RemoveDependency"); err != nil {
		return err
	}
	return nil
}

func (s *fakeBrainStore) AddLabel(_ context.Context, id, label, _ string) error {
	if err := s.shouldFail("AddLabel"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if issue, ok := s.issues[id]; ok {
		issue.Labels = append(issue.Labels, label)
		return nil
	}
	return errors.New("not found")
}

func (s *fakeBrainStore) RemoveLabel(_ context.Context, id, label, _ string) error {
	if err := s.shouldFail("RemoveLabel"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if issue, ok := s.issues[id]; ok {
		out := issue.Labels[:0]
		for _, l := range issue.Labels {
			if l != label {
				out = append(out, l)
			}
		}
		issue.Labels = out
		return nil
	}
	return errors.New("not found")
}

func (s *fakeBrainStore) AddIssueComment(_ context.Context, id, _ string, _ string) (*types.Comment, error) {
	if err := s.shouldFail("AddIssueComment"); err != nil {
		return nil, err
	}
	return &types.Comment{ID: "c-1", Author: "tester", Text: "note"}, nil
}

func (s *fakeBrainStore) RunInTransaction(ctx context.Context, _ string, fn func(tx Transaction) error) error {
	if err := s.shouldFail("RunInTransaction"); err != nil {
		return err
	}
	return fn(&fakeBrainTransaction{store: s})
}

type fakeBrainTransaction struct {
	Transaction
	store *fakeBrainStore
}

func (t *fakeBrainTransaction) CreateIssue(ctx context.Context, issue *types.Issue, actor string) error {
	return t.store.CreateIssue(ctx, issue, actor)
}

func (t *fakeBrainTransaction) CreateIssues(ctx context.Context, issues []*types.Issue, actor string) error {
	return t.store.CreateIssues(ctx, issues, actor)
}

func (t *fakeBrainTransaction) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	return t.store.UpdateIssue(ctx, id, updates, actor)
}

func (t *fakeBrainTransaction) CloseIssue(ctx context.Context, id, reason, actor, session string) error {
	return t.store.CloseIssue(ctx, id, reason, actor, session)
}

func (t *fakeBrainTransaction) AddDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	return t.store.AddDependency(ctx, dep, actor)
}

func (t *fakeBrainTransaction) AddDependencyWithOptions(ctx context.Context, dep *types.Dependency, actor string, _ DependencyAddOptions) error {
	return t.store.AddDependency(ctx, dep, actor)
}

func (t *fakeBrainTransaction) RemoveDependency(ctx context.Context, a, b, actor string) error {
	return t.store.RemoveDependency(ctx, a, b, actor)
}

func (t *fakeBrainTransaction) AddLabel(ctx context.Context, id, label, actor string) error {
	return t.store.AddLabel(ctx, id, label, actor)
}

func (t *fakeBrainTransaction) RemoveLabel(ctx context.Context, id, label, actor string) error {
	return t.store.RemoveLabel(ctx, id, label, actor)
}

func (t *fakeBrainTransaction) AddComment(_ context.Context, _, _, _ string) error {
	return nil
}

func (t *fakeBrainTransaction) GetIssue(ctx context.Context, id string) (*types.Issue, error) {
	return t.store.GetIssue(ctx, id)
}

func (t *fakeBrainTransaction) DeleteIssue(_ context.Context, _ string) error {
	return nil
}

// newTestDecorator wires a fresh fake store + recording exfiltrator
// into a BrainExfiltrationDecorator. Returns all three so tests can
// inspect each one.
func newTestDecorator() (*BrainExfiltrationDecorator, *fakeBrainStore, *recordingExf) {
	store := newFakeBrainStore()
	exf := &recordingExf{}
	d := &BrainExfiltrationDecorator{
		DoltStorage: store,
		inner:       store,
		exf:         exf,
	}
	return d, store, exf
}

func brainIssue(id, title string, kind types.IssueType) *types.Issue {
	return &types.Issue{
		ID:        id,
		Title:     title,
		IssueType: kind,
		Status:    types.StatusOpen,
		Priority:  2,
	}
}

// ── CreateIssue ────────────────────────────────────────────────────

func TestBrainExfDecorator_CreateIssue_FiresRender(t *testing.T) {
	d, _, exf := newTestDecorator()
	if err := d.CreateIssue(context.Background(), brainIssue("B-1", "first", types.TypeTask), "tester"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if exf.renderCount() != 1 {
		t.Fatalf("render count = %d, want 1", exf.renderCount())
	}
	if got := exf.lastRender(); got == nil || got.ID != "B-1" || got.IssueType != types.TypeTask {
		t.Fatalf("last render = %+v", got)
	}
}

func TestBrainExfDecorator_CreateIssue_NonBrainStillRendersButPassthrough(t *testing.T) {
	d, _, exf := newTestDecorator()
	// Even bugs flow through Render — the exfiltrator decides whether
	// to write. We assert Render was called exactly once.
	if err := d.CreateIssue(context.Background(), brainIssue("B-bug", "bug", types.TypeBug), "tester"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if exf.renderCount() != 1 {
		t.Fatalf("render count = %d, want 1 (Render is the routing point)", exf.renderCount())
	}
}

func TestBrainExfDecorator_CreateIssue_StorageErrorSkipsRender(t *testing.T) {
	d, store, exf := newTestDecorator()
	store.failOn = "CreateIssue"
	store.failWith = errors.New("db down")

	err := d.CreateIssue(context.Background(), brainIssue("B-2", "fail", types.TypeTask), "tester")
	if err == nil {
		t.Fatalf("expected error")
	}
	if exf.renderCount() != 0 {
		t.Fatalf("render fired despite storage error: %d", exf.renderCount())
	}
}

// ── CreateIssues ───────────────────────────────────────────────────

func TestBrainExfDecorator_CreateIssues_FiresRenderForEach(t *testing.T) {
	d, _, exf := newTestDecorator()
	issues := []*types.Issue{
		brainIssue("B-a", "a", types.TypeTask),
		brainIssue("B-b", "b", types.TypeKnowledge),
		brainIssue("B-c", "c", types.TypeBoth),
	}
	if err := d.CreateIssues(context.Background(), issues, "tester"); err != nil {
		t.Fatalf("CreateIssues: %v", err)
	}
	if exf.renderCount() != 3 {
		t.Fatalf("render count = %d, want 3", exf.renderCount())
	}
}

// ── UpdateIssue ────────────────────────────────────────────────────

func TestBrainExfDecorator_UpdateIssue_FiresRender(t *testing.T) {
	d, store, exf := newTestDecorator()
	store.issues["B-up"] = brainIssue("B-up", "before", types.TypeTask)

	if err := d.UpdateIssue(context.Background(), "B-up", map[string]interface{}{"title": "after"}, "tester"); err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	if exf.renderCount() != 1 {
		t.Fatalf("render count = %d, want 1", exf.renderCount())
	}
	if got := exf.lastRender(); got.Title != "after" {
		t.Fatalf("render saw stale title %q", got.Title)
	}
}

func TestBrainExfDecorator_UpdateIssue_StorageErrorSkipsRender(t *testing.T) {
	d, store, exf := newTestDecorator()
	store.issues["B-up"] = brainIssue("B-up", "before", types.TypeTask)
	store.failOn = "UpdateIssue"
	store.failWith = errors.New("nope")

	if err := d.UpdateIssue(context.Background(), "B-up", map[string]interface{}{"title": "after"}, "tester"); err == nil {
		t.Fatalf("expected error")
	}
	if exf.renderCount() != 0 {
		t.Fatalf("render fired despite storage error")
	}
}

// ── ReopenIssue ────────────────────────────────────────────────────

func TestBrainExfDecorator_ReopenIssue_FiresRender(t *testing.T) {
	d, store, exf := newTestDecorator()
	store.issues["B-r"] = brainIssue("B-r", "r", types.TypeTask)

	if err := d.ReopenIssue(context.Background(), "B-r", "fixed wrong", "tester"); err != nil {
		t.Fatalf("ReopenIssue: %v", err)
	}
	if exf.renderCount() != 1 {
		t.Fatalf("render count = %d, want 1", exf.renderCount())
	}
}

// ── UpdateIssueType (kind transitions) ─────────────────────────────

func TestBrainExfDecorator_UpdateIssueType_BrainToBrainNoRemove(t *testing.T) {
	// We use a real MarkdownExfiltrator so the kind-transition remove
	// path actually exercises a slug. The MarkdownExfiltrator gets its
	// own tmp root and we verify the file move via filesystem.
	store := newFakeBrainStore()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)
	d := &BrainExfiltrationDecorator{
		DoltStorage: store,
		inner:       store,
		exf:         exf,
	}

	issue := brainIssue("B-tt", "transition", types.TypeKnowledge)
	if err := d.CreateIssue(context.Background(), issue, "tester"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	// Issue file should exist at entries/knowledge/transition.md
	if _, err := osStatRequire(t, root, "entries/knowledge/transition.md"); err != nil {
		t.Fatalf("expected knowledge file: %v", err)
	}

	if err := d.UpdateIssueType(context.Background(), "B-tt", string(types.TypeTask), "tester"); err != nil {
		t.Fatalf("UpdateIssueType: %v", err)
	}
	// Now: task file should exist
	if _, err := osStatRequire(t, root, "entries/task/transition.md"); err != nil {
		t.Fatalf("expected task file after transition: %v", err)
	}
	// And the old knowledge file should be removed
	if err := osStatNotExist(t, root, "entries/knowledge/transition.md"); err != nil {
		t.Fatalf("old knowledge file should be gone: %v", err)
	}
}

func TestBrainExfDecorator_UpdateIssueType_BrainToNonBrainRemovesFile(t *testing.T) {
	store := newFakeBrainStore()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)
	d := &BrainExfiltrationDecorator{
		DoltStorage: store,
		inner:       store,
		exf:         exf,
	}

	issue := brainIssue("B-out", "outbound", types.TypeKnowledge)
	if err := d.CreateIssue(context.Background(), issue, "tester"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if _, err := osStatRequire(t, root, "entries/knowledge/outbound.md"); err != nil {
		t.Fatalf("expected knowledge file: %v", err)
	}

	if err := d.UpdateIssueType(context.Background(), "B-out", string(types.TypeBug), "tester"); err != nil {
		t.Fatalf("UpdateIssueType: %v", err)
	}
	if err := osStatNotExist(t, root, "entries/knowledge/outbound.md"); err != nil {
		t.Fatalf("old knowledge file should be gone after transition out of brain: %v", err)
	}
	// No bug file should appear (bug is not a brain kind).
	if err := osStatNotExist(t, root, "entries/bug/outbound.md"); err != nil {
		t.Fatalf("bug file should not be created: %v", err)
	}
}

func TestBrainExfDecorator_UpdateIssueType_NonBrainToBrainWritesFile(t *testing.T) {
	store := newFakeBrainStore()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)
	d := &BrainExfiltrationDecorator{
		DoltStorage: store,
		inner:       store,
		exf:         exf,
	}

	// Start as a bug — no markdown.
	issue := brainIssue("B-in", "inbound", types.TypeBug)
	if err := d.CreateIssue(context.Background(), issue, "tester"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	if err := d.UpdateIssueType(context.Background(), "B-in", string(types.TypeTask), "tester"); err != nil {
		t.Fatalf("UpdateIssueType: %v", err)
	}
	if _, err := osStatRequire(t, root, "entries/task/inbound.md"); err != nil {
		t.Fatalf("expected task file after transition into brain: %v", err)
	}
}

// ── UpdateIssue with issue_type (the brain recast code path) ───────
//
// `brain recast` does NOT call UpdateIssueType — it calls UpdateIssue
// with `"issue_type": <new>` in the updates map (see
// internal/brain/verb/recast/recast.go). The decorator must therefore
// honour the same kind-transition cleanup rules on UpdateIssue that it
// does on UpdateIssueType. These tests pin that contract.

func TestBrainExfDecorator_UpdateIssue_BrainToBrainRemovesOldFile(t *testing.T) {
	store := newFakeBrainStore()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)
	d := &BrainExfiltrationDecorator{
		DoltStorage: store,
		inner:       store,
		exf:         exf,
	}

	issue := brainIssue("B-rc1", "recast-bb", types.TypeKnowledge)
	if err := d.CreateIssue(context.Background(), issue, "tester"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if _, err := osStatRequire(t, root, "entries/knowledge/recast-bb.md"); err != nil {
		t.Fatalf("expected knowledge file: %v", err)
	}

	// Simulate the exact updates map `brain recast` builds.
	updates := map[string]interface{}{"issue_type": string(types.TypeTask)}
	if err := d.UpdateIssue(context.Background(), "B-rc1", updates, "tester"); err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}

	if _, err := osStatRequire(t, root, "entries/task/recast-bb.md"); err != nil {
		t.Fatalf("expected task file after recast: %v", err)
	}
	if err := osStatNotExist(t, root, "entries/knowledge/recast-bb.md"); err != nil {
		t.Fatalf("old knowledge file should be gone after recast: %v", err)
	}
}

func TestBrainExfDecorator_UpdateIssue_BrainToNonBrainRemovesFile(t *testing.T) {
	store := newFakeBrainStore()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)
	d := &BrainExfiltrationDecorator{
		DoltStorage: store,
		inner:       store,
		exf:         exf,
	}

	issue := brainIssue("B-rc2", "recast-out", types.TypeKnowledge)
	if err := d.CreateIssue(context.Background(), issue, "tester"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if _, err := osStatRequire(t, root, "entries/knowledge/recast-out.md"); err != nil {
		t.Fatalf("expected knowledge file: %v", err)
	}

	updates := map[string]interface{}{"issue_type": string(types.TypeBug)}
	if err := d.UpdateIssue(context.Background(), "B-rc2", updates, "tester"); err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}

	if err := osStatNotExist(t, root, "entries/knowledge/recast-out.md"); err != nil {
		t.Fatalf("old knowledge file should be gone after transition out of brain: %v", err)
	}
	if err := osStatNotExist(t, root, "entries/bug/recast-out.md"); err != nil {
		t.Fatalf("bug file should not be created: %v", err)
	}
}

func TestBrainExfDecorator_UpdateIssue_TitleOnlyDoesNotTriggerRemove(t *testing.T) {
	store := newFakeBrainStore()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)
	d := &BrainExfiltrationDecorator{
		DoltStorage: store,
		inner:       store,
		exf:         exf,
	}

	issue := brainIssue("B-rc3", "edit-only", types.TypeKnowledge)
	if err := d.CreateIssue(context.Background(), issue, "tester"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if _, err := osStatRequire(t, root, "entries/knowledge/edit-only.md"); err != nil {
		t.Fatalf("expected knowledge file: %v", err)
	}

	// A title-only update must NOT cross into the kind-transition path.
	// Slug derivation is stable across title edits (persisted in
	// issues.metadata.brain_slug), so the file stays exactly where it
	// was, and no Remove should fire on the old location.
	updates := map[string]interface{}{"title": "renamed"}
	if err := d.UpdateIssue(context.Background(), "B-rc3", updates, "tester"); err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}

	if _, err := osStatRequire(t, root, "entries/knowledge/edit-only.md"); err != nil {
		t.Fatalf("file should still be present after title edit: %v", err)
	}
}

// ── Dependencies ───────────────────────────────────────────────────

func TestBrainExfDecorator_AddDependency_FiresRender(t *testing.T) {
	d, store, exf := newTestDecorator()
	store.issues["B-dep"] = brainIssue("B-dep", "dep", types.TypeTask)

	dep := &types.Dependency{IssueID: "B-dep", DependsOnID: "B-other", Type: types.DepBlocks}
	if err := d.AddDependency(context.Background(), dep, "tester"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}
	if exf.renderCount() != 1 {
		t.Fatalf("render count = %d, want 1", exf.renderCount())
	}
}

func TestBrainExfDecorator_RemoveDependency_FiresRender(t *testing.T) {
	d, store, exf := newTestDecorator()
	store.issues["B-dep"] = brainIssue("B-dep", "dep", types.TypeTask)

	if err := d.RemoveDependency(context.Background(), "B-dep", "B-other", "tester"); err != nil {
		t.Fatalf("RemoveDependency: %v", err)
	}
	if exf.renderCount() != 1 {
		t.Fatalf("render count = %d, want 1", exf.renderCount())
	}
}

// ── Labels ─────────────────────────────────────────────────────────

func TestBrainExfDecorator_AddLabel_FiresRender(t *testing.T) {
	d, store, exf := newTestDecorator()
	store.issues["B-lab"] = brainIssue("B-lab", "lab", types.TypeKnowledge)

	if err := d.AddLabel(context.Background(), "B-lab", "alpha", "tester"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	if exf.renderCount() != 1 {
		t.Fatalf("render count = %d, want 1", exf.renderCount())
	}
}

func TestBrainExfDecorator_RemoveLabel_FiresRender(t *testing.T) {
	d, store, exf := newTestDecorator()
	store.issues["B-lab"] = brainIssue("B-lab", "lab", types.TypeKnowledge)
	store.issues["B-lab"].Labels = []string{"alpha"}

	if err := d.RemoveLabel(context.Background(), "B-lab", "alpha", "tester"); err != nil {
		t.Fatalf("RemoveLabel: %v", err)
	}
	if exf.renderCount() != 1 {
		t.Fatalf("render count = %d, want 1", exf.renderCount())
	}
}

// ── Comments ───────────────────────────────────────────────────────

func TestBrainExfDecorator_AddIssueComment_FiresRender(t *testing.T) {
	d, store, exf := newTestDecorator()
	store.issues["B-c"] = brainIssue("B-c", "c", types.TypeBoth)

	c, err := d.AddIssueComment(context.Background(), "B-c", "author", "text")
	if err != nil {
		t.Fatalf("AddIssueComment: %v", err)
	}
	if c == nil {
		t.Fatalf("comment is nil")
	}
	if exf.renderCount() != 1 {
		t.Fatalf("render count = %d, want 1", exf.renderCount())
	}
}

// ── Close ──────────────────────────────────────────────────────────

func TestBrainExfDecorator_CloseIssue_FiresRender(t *testing.T) {
	d, store, exf := newTestDecorator()
	store.issues["B-cl"] = brainIssue("B-cl", "to close", types.TypeTask)

	if err := d.CloseIssue(context.Background(), "B-cl", "done", "tester", "session-1"); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}
	if exf.renderCount() != 1 {
		t.Fatalf("render count = %d, want 1", exf.renderCount())
	}
}

// ── Transaction support ────────────────────────────────────────────

func TestBrainExfDecorator_Transaction_RendersAfterCommit(t *testing.T) {
	d, _, exf := newTestDecorator()

	err := d.RunInTransaction(context.Background(), "test", func(tx Transaction) error {
		return tx.CreateIssue(context.Background(), brainIssue("B-tx", "in tx", types.TypeTask), "tester")
	})
	if err != nil {
		t.Fatalf("RunInTransaction: %v", err)
	}
	if exf.renderCount() != 1 {
		t.Fatalf("render count = %d, want 1 (after commit)", exf.renderCount())
	}
}

func TestBrainExfDecorator_Transaction_NoRendersOnError(t *testing.T) {
	d, _, exf := newTestDecorator()

	expectedErr := errors.New("rollback")
	err := d.RunInTransaction(context.Background(), "test", func(tx Transaction) error {
		_ = tx.CreateIssue(context.Background(), brainIssue("B-rb", "rolled back", types.TypeTask), "tester")
		return expectedErr
	})
	if !errors.Is(err, expectedErr) {
		t.Fatalf("err = %v, want %v", err, expectedErr)
	}
	if exf.renderCount() != 0 {
		t.Fatalf("render fired on rollback: %d", exf.renderCount())
	}
}

func TestBrainExfDecorator_Transaction_DedupRenders(t *testing.T) {
	d, _, exf := newTestDecorator()

	err := d.RunInTransaction(context.Background(), "test", func(tx Transaction) error {
		if err := tx.CreateIssue(context.Background(), brainIssue("B-dup", "dup", types.TypeTask), "tester"); err != nil {
			return err
		}
		if err := tx.UpdateIssue(context.Background(), "B-dup", map[string]interface{}{"title": "updated"}, "tester"); err != nil {
			return err
		}
		if err := tx.AddLabel(context.Background(), "B-dup", "alpha", "tester"); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunInTransaction: %v", err)
	}
	// All three mutations target B-dup — exactly one render.
	if exf.renderCount() != 1 {
		t.Fatalf("render count = %d, want 1 (dedup)", exf.renderCount())
	}
}

func TestBrainExfDecorator_Transaction_MultipleIssuesEachRender(t *testing.T) {
	d, _, exf := newTestDecorator()

	err := d.RunInTransaction(context.Background(), "test", func(tx Transaction) error {
		issues := []*types.Issue{
			brainIssue("B-x", "x", types.TypeTask),
			brainIssue("B-y", "y", types.TypeKnowledge),
		}
		return tx.CreateIssues(context.Background(), issues, "tester")
	})
	if err != nil {
		t.Fatalf("RunInTransaction: %v", err)
	}
	if exf.renderCount() != 2 {
		t.Fatalf("render count = %d, want 2", exf.renderCount())
	}
}

// ── Nil-exfiltrator passthrough ────────────────────────────────────

func TestBrainExfDecorator_NilExfiltratorIsPassthrough(t *testing.T) {
	store := newFakeBrainStore()
	d := &BrainExfiltrationDecorator{
		DoltStorage: store,
		inner:       store,
		exf:         nil,
	}
	if err := d.CreateIssue(context.Background(), brainIssue("B-x", "x", types.TypeTask), "tester"); err != nil {
		t.Fatalf("CreateIssue with nil exfiltrator should not error: %v", err)
	}
}

// ── Compile-time assertion ─────────────────────────────────────────

func TestBrainExfDecorator_CompileTimeInterface(t *testing.T) {
	var _ DoltStorage = (*BrainExfiltrationDecorator)(nil)
}

// ── Inner ──────────────────────────────────────────────────────────

func TestBrainExfDecorator_InnerReturnsRaw(t *testing.T) {
	d, store, _ := newTestDecorator()
	if d.Inner() != store {
		t.Fatalf("Inner() = %p, want %p", d.Inner(), store)
	}
}

// ── Filesystem helpers (kept here to avoid an extra file) ─────────

func osStatRequire(t *testing.T, root, rel string) (os.FileInfo, error) {
	t.Helper()
	return os.Stat(root + "/" + rel)
}

func osStatNotExist(t *testing.T, root, rel string) error {
	t.Helper()
	if _, err := os.Stat(root + "/" + rel); err == nil {
		return errors.New("file exists when it should not")
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}
