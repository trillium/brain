package related_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/brain/verb"
	"github.com/steveyegge/beads/internal/brain/verb/related"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// recorderStore is a hand-rolled fake that implements related.RelatedStore.
// It satisfies the two methods the verb needs:
//
//   - GetIssue: returns the *types.Issue for the seeded ID, or
//     storage.ErrNotFound (wrapped, to mirror dolt's wrapping) if absent.
//   - GetDependenciesWithMetadata: returns the outgoing edges seeded for
//     this issueID. The slice may be empty (orphan) or absent (no edges
//     for that node) — both cases return (nil, nil) so the verb sees the
//     same "no neighbours" shape it would see against real storage.
//
// The fake is intentionally hand-rolled (not generated) so the test file
// stays self-contained and the seam can drift in either direction
// without breaking generation tooling. A compile-time assertion below
// catches any seam-shape change at build time.
type recorderStore struct {
	issues  map[string]*types.Issue
	edges   map[string][]*types.IssueWithDependencyMetadata
	getErr  error
	edgeErr error
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
	// Return a fresh copy so a test cannot mutate the seeded slice and
	// poison a later assertion.
	src := r.edges[issueID]
	if len(src) == 0 {
		return nil, nil
	}
	out := make([]*types.IssueWithDependencyMetadata, len(src))
	copy(out, src)
	return out, nil
}

// Compile-time proof that the test-only recorder satisfies the same
// RelatedStore seam production storage does. If the seam changes shape,
// this assertion catches it at build time, not in CI.
var _ related.RelatedStore = (*recorderStore)(nil)

// Compile-time proof that related.Verb satisfies BrainVerb[Args, Result].
// Duplicated here (the engine file has the same assertion) so a test-only
// rename doesn't silently break the contract.
var _ verb.BrainVerb[related.Args, related.Result] = related.Verb{}

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
// The verb reads IssueWithDependencyMetadata.Issue.{ID, Title, IssueType,
// Status} and .DependencyType — the same set GetDependenciesWithMetadata
// populates in prod.
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

// --- Spec scenario 1: depth=2, no cycle ---------------------------------

// TestRun_HappyPath_Depth2 exercises WHAT_IS_BRAIN.md § 4.3 sample tree:
// center has 2 outgoing children at depth=1, each with 1 grandchild at
// depth=2. The verb must:
//
//   - Place the center as Result.Center.
//   - Attach exactly 2 children under the center.
//   - Attach exactly 1 grandchild under each child.
//   - Populate each node's ID, Title, Kind, Closed, EdgeFromParent
//     correctly.
//   - Sort siblings by (edge type, neighbour ID).
func TestRun_HappyPath_Depth2(t *testing.T) {
	t.Parallel()

	rec := newRecorder()

	// Seed the same shape as the WHAT_IS_BRAIN.md sample:
	//   A --extends--> B --extends--> D
	//   A --learned-from--> C --caused-by--> E
	a := mkIssue("B-A", "Center doc", types.TypeKnowledge, types.StatusOpen)
	b := mkIssue("B-B", "Child via extends", types.TypeKnowledge, types.StatusOpen)
	c := mkIssue("B-C", "Child via learned-from", types.TypeTask, types.StatusClosed)
	d := mkIssue("B-D", "Grandchild of B", types.TypeKnowledge, types.StatusOpen)
	e := mkIssue("B-E", "Grandchild of C", types.TypeTask, types.StatusClosed)

	for _, iss := range []*types.Issue{a, b, c, d, e} {
		rec.issues[iss.ID] = iss
	}

	rec.edges["B-A"] = []*types.IssueWithDependencyMetadata{
		mkEdge(b, types.DepExtends),
		mkEdge(c, types.DepLearnedFrom),
	}
	rec.edges["B-B"] = []*types.IssueWithDependencyMetadata{
		mkEdge(d, types.DepExtends),
	}
	rec.edges["B-C"] = []*types.IssueWithDependencyMetadata{
		mkEdge(e, types.DepCausedBy),
	}

	v := related.New(rec)
	got, err := v.Run(context.Background(), related.Args{ID: "B-A", Depth: 2})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.Center == nil {
		t.Fatal("Result.Center is nil; want the center node")
	}
	if got.Center.ID != "B-A" {
		t.Errorf("Center.ID = %q, want %q", got.Center.ID, "B-A")
	}
	if got.Center.EdgeFromParent != "" {
		t.Errorf("Center.EdgeFromParent = %q, want \"\" (center has no parent edge)", got.Center.EdgeFromParent)
	}
	if got.Center.AlreadyVisited {
		t.Error("Center.AlreadyVisited = true, want false on first visit")
	}

	// Children of the center: must be sorted by edge type (alpha) — that
	// means "extends" (B-B) BEFORE "learned-from" (B-C).
	if len(got.Center.Children) != 2 {
		t.Fatalf("Center has %d children, want 2", len(got.Center.Children))
	}
	if got.Center.Children[0].EdgeFromParent != string(types.DepExtends) {
		t.Errorf("Children[0].EdgeFromParent = %q, want %q (extends sorts before learned-from)",
			got.Center.Children[0].EdgeFromParent, types.DepExtends)
	}
	if got.Center.Children[0].ID != "B-B" {
		t.Errorf("Children[0].ID = %q, want %q", got.Center.Children[0].ID, "B-B")
	}
	if got.Center.Children[1].EdgeFromParent != string(types.DepLearnedFrom) {
		t.Errorf("Children[1].EdgeFromParent = %q, want %q",
			got.Center.Children[1].EdgeFromParent, types.DepLearnedFrom)
	}
	if got.Center.Children[1].ID != "B-C" {
		t.Errorf("Children[1].ID = %q, want %q", got.Center.Children[1].ID, "B-C")
	}

	// Grandchildren — one each under B-B and B-C.
	bChild := got.Center.Children[0]
	if len(bChild.Children) != 1 {
		t.Fatalf("B-B has %d children, want 1", len(bChild.Children))
	}
	if bChild.Children[0].ID != "B-D" {
		t.Errorf("B-B's child = %q, want %q", bChild.Children[0].ID, "B-D")
	}
	if bChild.Children[0].EdgeFromParent != string(types.DepExtends) {
		t.Errorf("B-D EdgeFromParent = %q, want %q", bChild.Children[0].EdgeFromParent, types.DepExtends)
	}

	cChild := got.Center.Children[1]
	if len(cChild.Children) != 1 {
		t.Fatalf("B-C has %d children, want 1", len(cChild.Children))
	}
	if cChild.Children[0].ID != "B-E" {
		t.Errorf("B-C's child = %q, want %q", cChild.Children[0].ID, "B-E")
	}

	// Kind + Closed surfaces correctly. B-C is a closed task; the
	// wrapper renders this as `[kind=task, closed]`.
	if cChild.Kind != string(types.TypeTask) {
		t.Errorf("B-C.Kind = %q, want %q", cChild.Kind, types.TypeTask)
	}
	if !cChild.Closed {
		t.Error("B-C.Closed = false, want true (status=closed)")
	}
	if bChild.Closed {
		t.Error("B-B.Closed = true, want false (status=open)")
	}
}

// --- Spec scenario 2: depth=0 ------------------------------------------

// TestRun_Depth0 exercises the documented depth=0 behaviour: the result
// is the center alone, with no children. This matches the spec wording
// "--depth=0 shows just the center node" in WHAT_IS_BRAIN.md § 4.3.
func TestRun_Depth0(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	a := mkIssue("B-A", "Only-the-center", types.TypeKnowledge, types.StatusOpen)
	b := mkIssue("B-B", "Would-be-child", types.TypeKnowledge, types.StatusOpen)
	rec.issues["B-A"] = a
	rec.issues["B-B"] = b
	rec.edges["B-A"] = []*types.IssueWithDependencyMetadata{mkEdge(b, types.DepExtends)}

	v := related.New(rec)
	got, err := v.Run(context.Background(), related.Args{ID: "B-A", Depth: 0})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.Center == nil {
		t.Fatal("Center is nil; want the center node even at depth=0")
	}
	if got.Center.ID != "B-A" {
		t.Errorf("Center.ID = %q, want %q", got.Center.ID, "B-A")
	}
	if len(got.Center.Children) != 0 {
		t.Errorf("Center.Children len = %d, want 0 (depth=0 means no expansion)", len(got.Center.Children))
	}
	if got.Center.Children == nil {
		t.Error("Center.Children is nil, want empty slice (json contract: never elide the field)")
	}
}

// --- Spec scenario 3: depth=1 ------------------------------------------

// TestRun_Depth1 exercises the documented depth=1 behaviour: the result
// is the center plus direct outgoing neighbours, no grandchildren. The
// spec calls this "equivalent to bd dep list <id> with brain's rendering".
func TestRun_Depth1(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	a := mkIssue("B-A", "center", types.TypeKnowledge, types.StatusOpen)
	b := mkIssue("B-B", "direct neighbour", types.TypeKnowledge, types.StatusOpen)
	d := mkIssue("B-D", "would-be-grandchild", types.TypeKnowledge, types.StatusOpen)
	rec.issues["B-A"] = a
	rec.issues["B-B"] = b
	rec.issues["B-D"] = d
	rec.edges["B-A"] = []*types.IssueWithDependencyMetadata{mkEdge(b, types.DepExtends)}
	rec.edges["B-B"] = []*types.IssueWithDependencyMetadata{mkEdge(d, types.DepExtends)}

	v := related.New(rec)
	got, err := v.Run(context.Background(), related.Args{ID: "B-A", Depth: 1})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(got.Center.Children) != 1 {
		t.Fatalf("Center.Children len = %d, want 1", len(got.Center.Children))
	}
	child := got.Center.Children[0]
	if child.ID != "B-B" {
		t.Errorf("Child.ID = %q, want %q", child.ID, "B-B")
	}
	// At depth=1, the direct neighbour is reached but its OWN children
	// are not expanded — it remains a leaf in the result tree even
	// though storage HAS an outgoing edge for it.
	if len(child.Children) != 0 {
		t.Errorf("Child.Children len = %d, want 0 (depth cap reached at distance 1)", len(child.Children))
	}
}

// --- Spec scenario 4: orphan -------------------------------------------

// TestRun_Orphan exercises WHAT_IS_BRAIN.md § 4.3 scenario "orphan node":
// the center exists but has no outgoing edges. The result must be the
// center with an empty Children slice and no error. The wrapper renders
// this as "(no neighbours)".
func TestRun_Orphan(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	rec.issues["B-orph"] = mkIssue("B-orph", "lonely", types.TypeKnowledge, types.StatusOpen)
	// No edges seeded.

	v := related.New(rec)
	got, err := v.Run(context.Background(), related.Args{ID: "B-orph", Depth: 5})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (orphan is a normal result)", err)
	}
	if got.Center == nil {
		t.Fatal("Center is nil; want the orphan node")
	}
	if got.Center.ID != "B-orph" {
		t.Errorf("Center.ID = %q, want %q", got.Center.ID, "B-orph")
	}
	if got.Center.Children == nil {
		t.Error("Center.Children is nil, want empty slice")
	}
	if len(got.Center.Children) != 0 {
		t.Errorf("Center.Children len = %d, want 0", len(got.Center.Children))
	}
}

// --- Spec scenario 5: cycle --------------------------------------------

// TestRun_Cycle exercises WHAT_IS_BRAIN.md § 4.3 scenario "cycle in the
// graph": A --extends--> B --extends--> A. With depth=10 (deliberately
// generous), the BFS must NOT loop forever. A appears once at the root,
// B appears as A's child, and A re-appears under B as an AlreadyVisited
// leaf with no children of its own.
func TestRun_Cycle(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	a := mkIssue("B-A", "A", types.TypeKnowledge, types.StatusOpen)
	b := mkIssue("B-B", "B", types.TypeKnowledge, types.StatusOpen)
	rec.issues["B-A"] = a
	rec.issues["B-B"] = b
	rec.edges["B-A"] = []*types.IssueWithDependencyMetadata{mkEdge(b, types.DepExtends)}
	rec.edges["B-B"] = []*types.IssueWithDependencyMetadata{mkEdge(a, types.DepExtends)}

	v := related.New(rec)
	got, err := v.Run(context.Background(), related.Args{ID: "B-A", Depth: 10})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.Center == nil || got.Center.ID != "B-A" {
		t.Fatalf("Center.ID = %v, want B-A", got.Center)
	}
	if len(got.Center.Children) != 1 || got.Center.Children[0].ID != "B-B" {
		t.Fatalf("Center.Children = %+v, want one child B-B", got.Center.Children)
	}
	bNode := got.Center.Children[0]
	if bNode.AlreadyVisited {
		t.Error("B-B.AlreadyVisited = true on first visit, want false")
	}
	if len(bNode.Children) != 1 {
		t.Fatalf("B-B.Children len = %d, want 1 (the cycle's second appearance of A)", len(bNode.Children))
	}
	cycleLeaf := bNode.Children[0]
	if cycleLeaf.ID != "B-A" {
		t.Errorf("cycle leaf ID = %q, want %q", cycleLeaf.ID, "B-A")
	}
	if !cycleLeaf.AlreadyVisited {
		t.Error("cycle leaf AlreadyVisited = false, want true (second appearance must prune)")
	}
	if len(cycleLeaf.Children) != 0 {
		t.Errorf("cycle leaf has %d children, want 0 (AlreadyVisited must NOT recurse)", len(cycleLeaf.Children))
	}
	if cycleLeaf.EdgeFromParent != string(types.DepExtends) {
		t.Errorf("cycle leaf EdgeFromParent = %q, want %q", cycleLeaf.EdgeFromParent, types.DepExtends)
	}
}

// --- Spec scenario 6: nonexistent center -------------------------------

// TestRun_NonexistentCenter exercises WHAT_IS_BRAIN.md § 4.3 scenario
// "nonexistent center": the center ID is not in storage. The error must
// be the spec-required wording, and NO edge calls may happen (the
// existence probe gates BFS).
func TestRun_NonexistentCenter(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	// No issues seeded.

	v := related.New(rec)
	_, err := v.Run(context.Background(), related.Args{ID: "B-NOPE", Depth: 2})
	if err == nil {
		t.Fatal("Run() error = nil, want a not-found error")
	}
	want := "brain doc B-NOPE not found"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q must contain %q (the spec-mandated wording)", err.Error(), want)
	}
}

// --- Spec scenario 7: deterministic ordering ---------------------------

// TestRun_DeterministicOrdering pins the contract that two Run calls
// against the same seeded storage produce byte-identical tree shapes.
// This matters because the rendered tree is human-readable output that
// gets diffed across runs; ordering jitter would noise up every diff.
//
// The seeded edges are inserted in REVERSE of the expected sort order
// (learned-from before extends; B-Z before B-A under the same edge
// type), so a verb that simply walked the input slice would produce
// different output than one that sorts. The expected output is sorted
// by (edge type alpha, then neighbour ID).
func TestRun_DeterministicOrdering(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	center := mkIssue("B-CTR", "center", types.TypeKnowledge, types.StatusOpen)
	a := mkIssue("B-A", "A via learned-from", types.TypeKnowledge, types.StatusOpen)
	z := mkIssue("B-Z", "Z via extends", types.TypeKnowledge, types.StatusOpen)
	m := mkIssue("B-M", "M via extends", types.TypeKnowledge, types.StatusOpen)
	for _, iss := range []*types.Issue{center, a, z, m} {
		rec.issues[iss.ID] = iss
	}
	// Insertion order is intentionally NOT the expected sorted order:
	// learned-from before extends; Z before M within extends.
	rec.edges["B-CTR"] = []*types.IssueWithDependencyMetadata{
		mkEdge(a, types.DepLearnedFrom),
		mkEdge(z, types.DepExtends),
		mkEdge(m, types.DepExtends),
	}

	expected := []struct {
		edge string
		id   string
	}{
		{string(types.DepExtends), "B-M"},     // extends < learned-from; M < Z
		{string(types.DepExtends), "B-Z"},     // extends, second
		{string(types.DepLearnedFrom), "B-A"}, // learned-from, after both extends
	}

	v := related.New(rec)
	for run := 0; run < 3; run++ {
		got, err := v.Run(context.Background(), related.Args{ID: "B-CTR", Depth: 1})
		if err != nil {
			t.Fatalf("Run #%d error = %v", run, err)
		}
		if len(got.Center.Children) != len(expected) {
			t.Fatalf("Run #%d: %d children, want %d", run, len(got.Center.Children), len(expected))
		}
		for i, exp := range expected {
			child := got.Center.Children[i]
			if child.EdgeFromParent != exp.edge || child.ID != exp.id {
				t.Errorf("Run #%d Children[%d] = (edge=%q, id=%q), want (edge=%q, id=%q)",
					run, i, child.EdgeFromParent, child.ID, exp.edge, exp.id)
			}
		}
	}
}

// --- Spec scenario 8: mixed kinds reachable ----------------------------

// TestRun_MixedKinds confirms the "one bag of brain docs" invariant from
// WHAT_IS_BRAIN.md § 2: the BFS traverses edges without regard to kind,
// so task + knowledge + both kinds all surface in the same tree when
// they're connected. The verb just reports each node's kind; the
// wrapper renders the `[kind=...]` tag.
func TestRun_MixedKinds(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	center := mkIssue("B-C", "center", types.TypeKnowledge, types.StatusOpen)
	taskChild := mkIssue("B-T", "task child", types.TypeTask, types.StatusOpen)
	bothChild := mkIssue("B-X", "both child", types.TypeBoth, types.StatusOpen)
	knowGrand := mkIssue("B-K", "knowledge grandchild", types.TypeKnowledge, types.StatusOpen)
	for _, iss := range []*types.Issue{center, taskChild, bothChild, knowGrand} {
		rec.issues[iss.ID] = iss
	}
	rec.edges["B-C"] = []*types.IssueWithDependencyMetadata{
		mkEdge(taskChild, types.DepRelated),
		mkEdge(bothChild, types.DepExtends),
	}
	rec.edges["B-T"] = []*types.IssueWithDependencyMetadata{
		mkEdge(knowGrand, types.DepLearnedFrom),
	}

	v := related.New(rec)
	got, err := v.Run(context.Background(), related.Args{ID: "B-C", Depth: 2})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// Verify every kind value made it into the tree.
	seenKinds := map[string]bool{}
	var walk func(n *related.Node)
	walk = func(n *related.Node) {
		seenKinds[n.Kind] = true
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(got.Center)
	for _, want := range []string{string(types.TypeKnowledge), string(types.TypeTask), string(types.TypeBoth)} {
		if !seenKinds[want] {
			t.Errorf("kind %q missing from tree; saw %v", want, seenKinds)
		}
	}
}

// --- Validation guards --------------------------------------------------

// TestRun_EmptyID is an orthogonal Cobra-bypass guard. The wrapper's
// ExactArgs(1) normally catches this, but a hand-constructed Args can
// still produce an empty ID. The verb's own guard refuses to do a
// storage lookup against "".
func TestRun_EmptyID(t *testing.T) {
	t.Parallel()
	v := related.New(newRecorder())
	_, err := v.Run(context.Background(), related.Args{ID: "", Depth: 2})
	if err == nil {
		t.Fatal("Run() error = nil, want a required-id error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "required") {
		t.Errorf("error %q must mention 'required'", err.Error())
	}
}

// TestRun_NegativeDepth pins the contract that a negative depth is a
// caller bug, not a "go forever" instruction. The error must name the
// offending value so the caller can diagnose without reading source.
func TestRun_NegativeDepth(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	rec.issues["B-A"] = mkIssue("B-A", "x", types.TypeKnowledge, types.StatusOpen)
	v := related.New(rec)
	_, err := v.Run(context.Background(), related.Args{ID: "B-A", Depth: -1})
	if err == nil {
		t.Fatal("Run() error = nil, want an invalid-depth error")
	}
	if !strings.Contains(err.Error(), "-1") {
		t.Errorf("error %q must name the offending depth", err.Error())
	}
}

// TestRun_NilStoreReturnsError protects callers who forget to wire a
// store. Without this guard the verb would nil-deref on GetIssue.
func TestRun_NilStoreReturnsError(t *testing.T) {
	t.Parallel()
	v := related.New(nil)
	_, err := v.Run(context.Background(), related.Args{ID: "B-A", Depth: 1})
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

	v := related.New(rec)
	_, err := v.Run(context.Background(), related.Args{ID: "B-A", Depth: 1})
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

// TestRun_GetEdgesErrorIsWrapped ensures a failure during BFS expansion
// surfaces wrapped with the offending node ID so callers can diagnose
// which step of the walk failed.
func TestRun_GetEdgesErrorIsWrapped(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("dolt: deadlock detected")
	rec := newRecorder()
	rec.issues["B-A"] = mkIssue("B-A", "x", types.TypeKnowledge, types.StatusOpen)
	rec.edgeErr = sentinel

	v := related.New(rec)
	_, err := v.Run(context.Background(), related.Args{ID: "B-A", Depth: 2})
	if err == nil {
		t.Fatal("Run() error = nil, want a wrapped edge-lookup error")
	}
	if !errors.Is(err, sentinel) {
		t.Error("errors.Is(err, sentinel) = false; verb must wrap edge lookups with %w")
	}
	if !strings.Contains(err.Error(), "B-A") {
		t.Errorf("error %q must name the node whose expansion failed", err.Error())
	}
}

// TestVerbName pins the seam contract: Name() must match the first
// token of the Cobra `Use:` field in cmd/bd/brain_related.go. If
// someone renames either side without the other, this test catches it.
func TestVerbName(t *testing.T) {
	t.Parallel()
	v := related.Verb{}
	if got := v.Name(); got != "related" {
		t.Fatalf("Verb.Name() = %q, want %q", got, "related")
	}
}

// TestDefaultDepth pins the spec contract that the default depth is 2.
// The wrapper at cmd/bd/brain_related.go uses this constant for the
// flag default, so a drift between spec and code is caught here.
func TestDefaultDepth(t *testing.T) {
	t.Parallel()
	if related.DefaultDepth != 2 {
		t.Fatalf("DefaultDepth = %d, want 2 (WHAT_IS_BRAIN.md § 4.3)", related.DefaultDepth)
	}
}
