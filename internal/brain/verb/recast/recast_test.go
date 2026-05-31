package recast_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/brain/verb"
	"github.com/steveyegge/beads/internal/brain/verb/recast"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// recorderStore is a hand-rolled fake that implements recast.RecastStore.
// It satisfies the three methods the verb needs:
//
//   - GetIssue: returns the *types.Issue for the seeded ID, or wrapped
//     storage.ErrNotFound if absent.
//   - GetDependenciesWithMetadata: returns the outgoing edges seeded for
//     this issueID. nil/empty seeds return (nil, nil) so the verb sees
//     the same "no neighbours" shape it would see against real storage.
//   - UpdateIssue: records the updates map and the actor for assertion.
//     Returns updErr if set so the wrap-on-failure test can pass a
//     sentinel through.
//
// The fake is intentionally hand-rolled (not generated) so the test file
// stays self-contained and the seam can drift in either direction
// without breaking generation tooling. A compile-time assertion below
// catches any seam-shape change at build time.
//
// updateCalls is the recording surface. Each UpdateIssue invocation
// appends a snapshot of the (id, updates, actor) trio so the no-op test
// can assert zero entries and the mutating tests can assert exactly one.
type recorderStore struct {
	issues      map[string]*types.Issue
	edges       map[string][]*types.IssueWithDependencyMetadata
	getErr      error
	edgeErr     error
	updErr      error
	updateCalls []updateCall
}

type updateCall struct {
	id      string
	updates map[string]interface{}
	actor   string
}

func (r *recorderStore) GetIssue(_ context.Context, id string) (*types.Issue, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	if iss, ok := r.issues[id]; ok {
		return iss, nil
	}
	// Mirror the wrapping wording bd's storage layer uses so callers can
	// errors.Is(err, storage.ErrNotFound) the same way they would in prod.
	return nil, fmt.Errorf("%w: issue %s", storage.ErrNotFound, id)
}

func (r *recorderStore) GetDependenciesWithMetadata(_ context.Context, issueID string) ([]*types.IssueWithDependencyMetadata, error) {
	if r.edgeErr != nil {
		return nil, r.edgeErr
	}
	src := r.edges[issueID]
	if len(src) == 0 {
		return nil, nil
	}
	out := make([]*types.IssueWithDependencyMetadata, len(src))
	copy(out, src)
	return out, nil
}

func (r *recorderStore) UpdateIssue(_ context.Context, id string, updates map[string]interface{}, actor string) error {
	if r.updErr != nil {
		return r.updErr
	}
	// Copy the updates map so a later mutation by the test cannot
	// retroactively change the recorded call.
	cp := make(map[string]interface{}, len(updates))
	for k, v := range updates {
		cp[k] = v
	}
	r.updateCalls = append(r.updateCalls, updateCall{id: id, updates: cp, actor: actor})
	return nil
}

// Compile-time proof that the test-only recorder satisfies the same
// RecastStore seam production storage does. If the seam changes shape,
// this assertion catches it at build time, not in CI.
var _ recast.RecastStore = (*recorderStore)(nil)

// Compile-time proof that recast.Verb satisfies BrainVerb[Args, Result].
// Duplicated here (the engine file has the same assertion) so a test-only
// rename doesn't silently break the contract.
var _ verb.BrainVerb[recast.Args, recast.Result] = recast.Verb{}

// mkIssue builds a *types.Issue with the fields the verb actually reads
// (ID, Title, IssueType, Status). Other fields default-zero; the verb
// is documented to ignore them.
func mkIssue(id, title string, kind types.IssueType, status types.Status) *types.Issue {
	return &types.Issue{
		ID:        id,
		Title:     title,
		IssueType: kind,
		Status:    status,
	}
}

// mkEdge builds an outgoing edge from the seeded perspective: the
// neighbour issue + the edge type that points TO it from the parent.
// The verb only reads IssueWithDependencyMetadata.Issue.ID for the
// EdgesPreserved list — we still seed Title/Kind/Status so the row
// shape matches what storage returns in prod.
func mkEdge(neighbour *types.Issue, dep types.DependencyType) *types.IssueWithDependencyMetadata {
	return &types.IssueWithDependencyMetadata{
		Issue:          *neighbour,
		DependencyType: dep,
	}
}

func newRecorder() *recorderStore {
	return &recorderStore{
		issues: make(map[string]*types.Issue),
		edges:  make(map[string][]*types.IssueWithDependencyMetadata),
	}
}

// --- Spec scenario 1: knowledge → task, three edges preserved -----------

// TestRun_KnowledgeToTask_DefaultsOpen exercises WHAT_IS_BRAIN.md § 4.4
// scenario "knowledge → task (let's turn that into a task)": center is
// kind=knowledge with three outgoing edges; after recast --to=task the
// row's issue_type is 'task', the status defaults to 'open' (the
// knowledge source had no meaningful task status before), and all three
// edges are reported as preserved in sorted order.
func TestRun_KnowledgeToTask_DefaultsOpen(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	center := mkIssue("B-a7b3c", "Dolt FK constraints are lazy until commit",
		types.TypeKnowledge, types.StatusOpen)
	rec.issues[center.ID] = center

	// Three outgoing edges. Seeded in NON-sorted order so the test
	// verifies the verb sorts before reporting.
	e1 := mkIssue("B-552a", "edge 1", types.TypeTask, types.StatusOpen)
	e2 := mkIssue("B-217", "edge 2", types.TypeKnowledge, types.StatusOpen)
	e3 := mkIssue("B-100", "edge 3", types.TypeBoth, types.StatusOpen)
	rec.edges[center.ID] = []*types.IssueWithDependencyMetadata{
		mkEdge(e1, types.DepExtends),
		mkEdge(e2, types.DepLearnedFrom),
		mkEdge(e3, types.DepRelated),
	}

	v := recast.New(rec, "test-actor")
	got, err := v.Run(context.Background(), recast.Args{ID: "B-a7b3c", ToKind: "task"})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.NoOp {
		t.Error("Result.NoOp = true, want false on a kind-changing recast")
	}
	if got.OldKind != string(types.TypeKnowledge) {
		t.Errorf("OldKind = %q, want %q", got.OldKind, types.TypeKnowledge)
	}
	if got.NewKind != string(types.TypeTask) {
		t.Errorf("NewKind = %q, want %q", got.NewKind, types.TypeTask)
	}
	if got.OldStatus != string(types.StatusOpen) {
		t.Errorf("OldStatus = %q, want %q", got.OldStatus, types.StatusOpen)
	}
	if got.NewStatus != string(types.StatusOpen) {
		t.Errorf("NewStatus = %q, want %q (defaulted on knowledge→task)", got.NewStatus, types.StatusOpen)
	}

	// EdgesPreserved sorted by neighbour ID alphabetically.
	wantEdges := []string{"B-100", "B-217", "B-552a"}
	if len(got.EdgesPreserved) != len(wantEdges) {
		t.Fatalf("EdgesPreserved len = %d, want %d", len(got.EdgesPreserved), len(wantEdges))
	}
	for i, want := range wantEdges {
		if got.EdgesPreserved[i] != want {
			t.Errorf("EdgesPreserved[%d] = %q, want %q (sorted)", i, got.EdgesPreserved[i], want)
		}
	}

	// Exactly one UpdateIssue call with issue_type AND status set.
	if len(rec.updateCalls) != 1 {
		t.Fatalf("UpdateIssue called %d times, want 1", len(rec.updateCalls))
	}
	call := rec.updateCalls[0]
	if call.id != "B-a7b3c" {
		t.Errorf("UpdateIssue id = %q, want %q", call.id, "B-a7b3c")
	}
	if call.actor != "test-actor" {
		t.Errorf("UpdateIssue actor = %q, want %q (audit trail must carry it)", call.actor, "test-actor")
	}
	if call.updates["issue_type"] != "task" {
		t.Errorf("updates[issue_type] = %v, want %q", call.updates["issue_type"], "task")
	}
	// status was 'open' before AND defaulted to 'open' — same string,
	// so the verb should NOT include "status" in the updates map (the
	// transition didn't change status, only kind). This keeps the audit
	// trail clean.
	if _, ok := call.updates["status"]; ok {
		t.Errorf("updates[status] present (%v), want absent when newStatus equals oldStatus",
			call.updates["status"])
	}
}

// --- Spec scenario 2: task → knowledge (status preserved, both polarities)

// TestRun_TaskToKnowledge_PreservesStatusOpen pins WHAT_IS_BRAIN.md § 4.4
// scenario "misclassification recovery": a task doc is reclassified as
// knowledge; the status column is preserved verbatim. The verb does NOT
// change status; only kind flips.
func TestRun_TaskToKnowledge_PreservesStatusOpen(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	rec.issues["B-9c12a"] = mkIssue("B-9c12a", "task → knowledge", types.TypeTask, types.StatusOpen)

	v := recast.New(rec, "test-actor")
	got, err := v.Run(context.Background(), recast.Args{ID: "B-9c12a", ToKind: "knowledge"})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.OldKind != string(types.TypeTask) || got.NewKind != string(types.TypeKnowledge) {
		t.Errorf("kind transition = %q → %q, want task → knowledge", got.OldKind, got.NewKind)
	}
	if got.OldStatus != string(types.StatusOpen) || got.NewStatus != string(types.StatusOpen) {
		t.Errorf("status = %q → %q, want open → open (preserved)", got.OldStatus, got.NewStatus)
	}
	if len(rec.updateCalls) != 1 {
		t.Fatalf("UpdateIssue called %d times, want 1", len(rec.updateCalls))
	}
	if _, ok := rec.updateCalls[0].updates["status"]; ok {
		t.Error("updates[status] present, want absent when status unchanged")
	}
}

// TestRun_TaskToKnowledge_PreservesStatusClosed is the closed polarity of
// the same scenario: the spec specifically calls out that "the status
// column is preserved but no longer participates in `brain ready`". A
// closed task that becomes knowledge stays closed.
func TestRun_TaskToKnowledge_PreservesStatusClosed(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	rec.issues["B-9c12a"] = mkIssue("B-9c12a", "closed task", types.TypeTask, types.StatusClosed)

	v := recast.New(rec, "test-actor")
	got, err := v.Run(context.Background(), recast.Args{ID: "B-9c12a", ToKind: "knowledge"})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.NewStatus != string(types.StatusClosed) {
		t.Errorf("NewStatus = %q, want %q (closed preserved through kind flip)", got.NewStatus, types.StatusClosed)
	}
}

// --- Spec scenario 3: knowledge → both (defaults open) -----------------

// TestRun_KnowledgeToBoth_DefaultsOpen exercises WHAT_IS_BRAIN.md § 4.4
// scenario "knowledge → both (active investigation)": a knowledge doc
// becomes a both-kind doc; status defaults to open per the same rule
// that applies to knowledge → task.
func TestRun_KnowledgeToBoth_DefaultsOpen(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	rec.issues["B-a7b3c"] = mkIssue("B-a7b3c", "active inv", types.TypeKnowledge, types.StatusOpen)

	v := recast.New(rec, "test-actor")
	got, err := v.Run(context.Background(), recast.Args{ID: "B-a7b3c", ToKind: "both"})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.OldKind != string(types.TypeKnowledge) || got.NewKind != string(types.TypeBoth) {
		t.Errorf("kind transition = %q → %q, want knowledge → both", got.OldKind, got.NewKind)
	}
	if got.NewStatus != string(types.StatusOpen) {
		t.Errorf("NewStatus = %q, want %q (defaulted on knowledge→both)", got.NewStatus, types.StatusOpen)
	}
}

// --- Spec scenario 4: task → both (status preserved) -------------------

// TestRun_TaskToBoth_PreservesStatus verifies the task → both transition:
// kind flips, status preserved (no defaulting). A task in progress
// becoming a both-kind doc keeps its in-progress status. We use
// StatusOpen here for simplicity; the defaulting rule only fires on
// knowledge sources.
func TestRun_TaskToBoth_PreservesStatus(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	rec.issues["B-t1"] = mkIssue("B-t1", "task→both", types.TypeTask, types.StatusOpen)

	v := recast.New(rec, "test-actor")
	got, err := v.Run(context.Background(), recast.Args{ID: "B-t1", ToKind: "both"})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.OldKind != string(types.TypeTask) || got.NewKind != string(types.TypeBoth) {
		t.Errorf("kind transition = %q → %q, want task → both", got.OldKind, got.NewKind)
	}
	if got.NewStatus != string(types.StatusOpen) {
		t.Errorf("NewStatus = %q, want %q (preserved)", got.NewStatus, types.StatusOpen)
	}
}

// --- Spec scenario 5: both → task (status preserved) -------------------

// TestRun_BothToTask_PreservesStatus verifies the both → task transition:
// kind flips, status preserved. A closed both-kind doc that collapses to
// task stays closed — no re-opening, no defaulting.
func TestRun_BothToTask_PreservesStatus(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	rec.issues["B-b1"] = mkIssue("B-b1", "both→task", types.TypeBoth, types.StatusClosed)

	v := recast.New(rec, "test-actor")
	got, err := v.Run(context.Background(), recast.Args{ID: "B-b1", ToKind: "task"})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.OldKind != string(types.TypeBoth) || got.NewKind != string(types.TypeTask) {
		t.Errorf("kind transition = %q → %q, want both → task", got.OldKind, got.NewKind)
	}
	if got.NewStatus != string(types.StatusClosed) {
		t.Errorf("NewStatus = %q, want %q (preserved through both→task)", got.NewStatus, types.StatusClosed)
	}
}

// --- Spec scenario 6: both → knowledge (status preserved) --------------

// TestRun_BothToKnowledge_PreservesStatus verifies the both → knowledge
// transition: kind flips, status preserved verbatim. The spec wording
// applies the same way here as for task → knowledge.
func TestRun_BothToKnowledge_PreservesStatus(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	rec.issues["B-b2"] = mkIssue("B-b2", "both→knowledge", types.TypeBoth, types.StatusOpen)

	v := recast.New(rec, "test-actor")
	got, err := v.Run(context.Background(), recast.Args{ID: "B-b2", ToKind: "knowledge"})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.OldKind != string(types.TypeBoth) || got.NewKind != string(types.TypeKnowledge) {
		t.Errorf("kind transition = %q → %q, want both → knowledge", got.OldKind, got.NewKind)
	}
	if got.NewStatus != string(types.StatusOpen) {
		t.Errorf("NewStatus = %q, want %q (preserved through both→knowledge)", got.NewStatus, types.StatusOpen)
	}
}

// --- Spec scenario 7: knowledge with closed status → task --------------

// TestRun_KnowledgeClosedToTask_PreservesClosed pins the defaulting
// rule's exception: a knowledge doc that was explicitly closed STAYS
// closed when recast to task. The rule is "default open ONLY if not
// already closed" — the user closed it deliberately and brain does not
// re-open it.
func TestRun_KnowledgeClosedToTask_PreservesClosed(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	rec.issues["B-kc"] = mkIssue("B-kc", "closed knowledge", types.TypeKnowledge, types.StatusClosed)

	v := recast.New(rec, "test-actor")
	got, err := v.Run(context.Background(), recast.Args{ID: "B-kc", ToKind: "task"})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.NewStatus != string(types.StatusClosed) {
		t.Errorf("NewStatus = %q, want %q (closed knowledge stays closed through →task)", got.NewStatus, types.StatusClosed)
	}
	// Since old==new on status, updates map should NOT contain status.
	if len(rec.updateCalls) != 1 {
		t.Fatalf("UpdateIssue called %d times, want 1", len(rec.updateCalls))
	}
	if _, ok := rec.updateCalls[0].updates["status"]; ok {
		t.Error("updates[status] present, want absent when newStatus equals oldStatus")
	}
}

// --- Spec scenario 8: no-op (current kind == target) -------------------

// TestRun_NoOp_KindAlreadyMatches exercises WHAT_IS_BRAIN.md § 4.4
// scenario "idempotent recast": current kind already equals --to=. The
// verb must exit 0 with NoOp=true, write NOTHING to storage, and report
// OldKind == NewKind. The wrapper renders `no-op: <id> already
// kind=<kind>`.
func TestRun_NoOp_KindAlreadyMatches(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	rec.issues["B-tk"] = mkIssue("B-tk", "already a task", types.TypeTask, types.StatusOpen)
	// Seed one edge so the no-op still enumerates EdgesPreserved.
	e1 := mkIssue("B-e1", "edge", types.TypeTask, types.StatusOpen)
	rec.edges["B-tk"] = []*types.IssueWithDependencyMetadata{mkEdge(e1, types.DepExtends)}

	v := recast.New(rec, "test-actor")
	got, err := v.Run(context.Background(), recast.Args{ID: "B-tk", ToKind: "task"})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if !got.NoOp {
		t.Error("NoOp = false, want true (kind already matches)")
	}
	if got.OldKind != got.NewKind {
		t.Errorf("OldKind=%q, NewKind=%q, want equal on no-op", got.OldKind, got.NewKind)
	}
	if got.NewKind != string(types.TypeTask) {
		t.Errorf("NewKind = %q, want %q", got.NewKind, types.TypeTask)
	}
	// No-op MUST NOT touch storage's write path.
	if len(rec.updateCalls) != 0 {
		t.Errorf("UpdateIssue called %d times on no-op, want 0", len(rec.updateCalls))
	}
	// EdgesPreserved should still be populated and sorted.
	if len(got.EdgesPreserved) != 1 || got.EdgesPreserved[0] != "B-e1" {
		t.Errorf("EdgesPreserved = %v, want [B-e1]", got.EdgesPreserved)
	}
	// Status fields are empty on no-op (the wrapper elides the line).
	if got.OldStatus != "" || got.NewStatus != "" {
		t.Errorf("Status fields = (%q, %q), want both empty on no-op", got.OldStatus, got.NewStatus)
	}
}

// --- Spec scenario 9: invalid target kind ------------------------------

// TestRun_InvalidTargetKind exercises WHAT_IS_BRAIN.md § 4.4 scenario
// "invalid target kind": the user passed something that isn't task |
// knowledge | both. The error must name the offending value AND list
// the three valid kinds so the user can recover without reading code.
func TestRun_InvalidTargetKind(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	rec.issues["B-x"] = mkIssue("B-x", "x", types.TypeKnowledge, types.StatusOpen)

	v := recast.New(rec, "test-actor")
	_, err := v.Run(context.Background(), recast.Args{ID: "B-x", ToKind: "archived"})
	if err == nil {
		t.Fatal("Run() error = nil, want an invalid-kind error")
	}
	got := err.Error()
	if !strings.Contains(got, "invalid target kind") {
		t.Errorf("error %q must contain %q (spec-mandated wording)", got, "invalid target kind")
	}
	for _, want := range []string{"task", "knowledge", "both", "archived"} {
		if !strings.Contains(got, want) {
			t.Errorf("error %q must mention %q", got, want)
		}
	}
	// Validation runs BEFORE any storage read.
	if len(rec.updateCalls) != 0 {
		t.Errorf("UpdateIssue called %d times on invalid kind, want 0", len(rec.updateCalls))
	}
}

// --- Spec scenario 10: nonexistent ID ----------------------------------

// TestRun_NonexistentID exercises the missing-doc path: the row does
// not exist. The error must be the spec-required wording, and NO
// UpdateIssue call may happen.
func TestRun_NonexistentID(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	// No issues seeded.

	v := recast.New(rec, "test-actor")
	_, err := v.Run(context.Background(), recast.Args{ID: "B-NOPE", ToKind: "task"})
	if err == nil {
		t.Fatal("Run() error = nil, want a not-found error")
	}
	want := "brain doc B-NOPE not found"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q must contain %q (the spec-mandated wording)", err.Error(), want)
	}
	if len(rec.updateCalls) != 0 {
		t.Errorf("UpdateIssue called %d times on missing doc, want 0", len(rec.updateCalls))
	}
}

// --- Spec scenario 11: edges enumerated deterministically --------------

// TestRun_EdgesPreserved_DeterministicOrder pins the contract that
// EdgesPreserved is sorted by neighbour ID alphabetically. Two recast
// calls against the same seeded storage must return byte-identical
// slices. The seed inserts edges in REVERSE alphabetical order so a
// verb that simply walked the input slice would produce different
// output than one that sorts.
func TestRun_EdgesPreserved_DeterministicOrder(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	rec.issues["B-ctr"] = mkIssue("B-ctr", "center", types.TypeKnowledge, types.StatusOpen)

	z := mkIssue("B-z", "z", types.TypeTask, types.StatusOpen)
	m := mkIssue("B-m", "m", types.TypeTask, types.StatusOpen)
	a := mkIssue("B-a", "a", types.TypeTask, types.StatusOpen)
	rec.edges["B-ctr"] = []*types.IssueWithDependencyMetadata{
		mkEdge(z, types.DepRelated),
		mkEdge(m, types.DepExtends),
		mkEdge(a, types.DepLearnedFrom),
	}

	v := recast.New(rec, "test-actor")
	got, err := v.Run(context.Background(), recast.Args{ID: "B-ctr", ToKind: "task"})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	want := []string{"B-a", "B-m", "B-z"}
	if len(got.EdgesPreserved) != len(want) {
		t.Fatalf("EdgesPreserved len = %d, want %d", len(got.EdgesPreserved), len(want))
	}
	for i, w := range want {
		if got.EdgesPreserved[i] != w {
			t.Errorf("EdgesPreserved[%d] = %q, want %q (sorted)", i, got.EdgesPreserved[i], w)
		}
	}
}

// --- Spec scenario 12: no edges (empty slice not nil) ------------------

// TestRun_EdgesPreserved_EmptyNotNil pins the contract that
// EdgesPreserved is an empty slice (not nil) when the doc has no
// outgoing edges. This is the JSON contract: --json must NEVER emit
// `"edges_preserved": null` for a doc with zero edges; it must emit
// `"edges_preserved": []`.
func TestRun_EdgesPreserved_EmptyNotNil(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	rec.issues["B-orph"] = mkIssue("B-orph", "lonely", types.TypeKnowledge, types.StatusOpen)
	// No edges seeded.

	v := recast.New(rec, "test-actor")
	got, err := v.Run(context.Background(), recast.Args{ID: "B-orph", ToKind: "task"})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (orphan recast is normal)", err)
	}
	if got.EdgesPreserved == nil {
		t.Error("EdgesPreserved is nil; want empty slice (json contract)")
	}
	if len(got.EdgesPreserved) != 0 {
		t.Errorf("EdgesPreserved len = %d, want 0", len(got.EdgesPreserved))
	}
}

// --- Validation guards --------------------------------------------------

// TestRun_EmptyID is an orthogonal Cobra-bypass guard. The wrapper's
// ExactArgs(1) normally catches this, but a hand-constructed Args can
// still produce an empty ID. The verb's own guard refuses to do a
// storage lookup against "".
func TestRun_EmptyID(t *testing.T) {
	t.Parallel()
	v := recast.New(newRecorder(), "test-actor")
	_, err := v.Run(context.Background(), recast.Args{ID: "", ToKind: "task"})
	if err == nil {
		t.Fatal("Run() error = nil, want a required-id error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "required") {
		t.Errorf("error %q must mention 'required'", err.Error())
	}
}

// TestRun_EmptyToKind is the orthogonal guard for a missing --to= value.
// Cobra MarkFlagRequired normally catches this; the verb's own guard is
// the modularity guarantee for hand-constructed Args.
func TestRun_EmptyToKind(t *testing.T) {
	t.Parallel()
	v := recast.New(newRecorder(), "test-actor")
	_, err := v.Run(context.Background(), recast.Args{ID: "B-A", ToKind: ""})
	if err == nil {
		t.Fatal("Run() error = nil, want a required-kind error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "required") {
		t.Errorf("error %q must mention 'required'", err.Error())
	}
}

// TestRun_NilStoreReturnsError protects callers who forget to wire a
// store. Without this guard the verb would nil-deref on GetIssue.
func TestRun_NilStoreReturnsError(t *testing.T) {
	t.Parallel()
	v := recast.New(nil, "test-actor")
	_, err := v.Run(context.Background(), recast.Args{ID: "B-A", ToKind: "task"})
	if err == nil {
		t.Fatal("Run() error = nil, want an unconfigured-storage error")
	}
	if !strings.Contains(err.Error(), "storage") {
		t.Errorf("error %q must mention 'storage'", err.Error())
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
	rec := newRecorder()
	rec.getErr = sentinel

	v := recast.New(rec, "test-actor")
	_, err := v.Run(context.Background(), recast.Args{ID: "B-A", ToKind: "task"})
	if err == nil {
		t.Fatal("Run() error = nil, want a wrapped lookup error")
	}
	if !errors.Is(err, sentinel) {
		t.Error("errors.Is(err, sentinel) = false; verb must wrap GetIssue failures with %w")
	}
	if strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q misclassifies transport failure as not-found", err.Error())
	}
}

// TestRun_GetEdgesErrorIsWrapped ensures a failure during edge
// enumeration surfaces wrapped with the offending node ID so callers
// can diagnose which step failed.
func TestRun_GetEdgesErrorIsWrapped(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("dolt: deadlock detected")
	rec := newRecorder()
	rec.issues["B-A"] = mkIssue("B-A", "x", types.TypeKnowledge, types.StatusOpen)
	rec.edgeErr = sentinel

	v := recast.New(rec, "test-actor")
	_, err := v.Run(context.Background(), recast.Args{ID: "B-A", ToKind: "task"})
	if err == nil {
		t.Fatal("Run() error = nil, want a wrapped edge-lookup error")
	}
	if !errors.Is(err, sentinel) {
		t.Error("errors.Is(err, sentinel) = false; verb must wrap edge lookups with %w")
	}
	if !strings.Contains(err.Error(), "B-A") {
		t.Errorf("error %q must name the node whose edge load failed", err.Error())
	}
}

// TestRun_UpdateIssueErrorIsWrapped ensures a failure during the
// UpdateIssue write surfaces wrapped with the offending ID so callers
// can diagnose. The verb must NOT swallow the underlying error.
func TestRun_UpdateIssueErrorIsWrapped(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("dolt: foreign key violation")
	rec := newRecorder()
	rec.issues["B-A"] = mkIssue("B-A", "x", types.TypeKnowledge, types.StatusOpen)
	rec.updErr = sentinel

	v := recast.New(rec, "test-actor")
	_, err := v.Run(context.Background(), recast.Args{ID: "B-A", ToKind: "task"})
	if err == nil {
		t.Fatal("Run() error = nil, want a wrapped update error")
	}
	if !errors.Is(err, sentinel) {
		t.Error("errors.Is(err, sentinel) = false; verb must wrap UpdateIssue failures with %w")
	}
	if !strings.Contains(err.Error(), "B-A") {
		t.Errorf("error %q must name the doc whose update failed", err.Error())
	}
}

// TestVerbName pins the seam contract: Name() must match the first
// token of the Cobra `Use:` field in cmd/bd/brain_recast.go.
func TestVerbName(t *testing.T) {
	t.Parallel()
	v := recast.Verb{}
	if got := v.Name(); got != "recast" {
		t.Fatalf("Verb.Name() = %q, want %q", got, "recast")
	}
}
