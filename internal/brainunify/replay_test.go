package brainunify

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestChunksAndInClause(t *testing.T) {
	got := chunks([]string{"a", "b", "c", "d", "e"}, 2)
	want := [][]string{{"a", "b"}, {"c", "d"}, {"e"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chunks = %v, want %v", got, want)
	}
	if chunks(nil, 3) != nil {
		t.Error("chunks of nothing must be nothing")
	}
	clause, args := inClause("issue_id", []string{"x", "y"})
	if clause != "`issue_id` in (?,?)" || !reflect.DeepEqual(args, []any{"x", "y"}) {
		t.Errorf("inClause = %q %v", clause, args)
	}
}

// replayPlans is the bead-scoped and state tables a reconciliation widens over.
func replayPlans() []TablePlan {
	return []TablePlan{
		{Table: "issues", Scope: ScopeIssues, Target: "issues", ScopeColumn: "id"},
		{Table: "labels", Scope: ScopeIssueChild, Target: "labels", ScopeColumn: "issue_id"},
		{Table: "config", Scope: ScopeDatabaseState, Target: "brain_unified_config"},
	}
}

func newWork() *replayWork {
	return &replayWork{
		plans:         replayPlans(),
		touched:       map[string]map[string]bool{},
		changedTables: map[string]map[string]bool{},
		stateChanged:  map[string]map[string]bool{},
		changedStores: map[string]bool{},
		full:          map[string]bool{},
	}
}

func replayerWith(collisions ...Collision) *Replayer {
	return &Replayer{plan: Plan{Collisions: collisions}, opts: ReplayOptions{}}
}

func TestReconcileCollisionsLeavesUnchangedDecisionsAlone(t *testing.T) {
	c := Collision{ID: "alp-1", Winner: "alpha", Losers: []string{"gamma"}}
	w := newWork()
	err := replayerWith(c).reconcileCollisions(w, map[string]recordedCollision{
		"alp-1": {Winner: "alpha", Losers: []string{"gamma"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.collisionsToWrite) != 0 || len(w.collisionsToDelete) != 0 || len(w.touched) != 0 || len(w.wide) != 0 {
		t.Errorf("an unchanged, untouched duplicate must cost nothing: %+v", w)
	}
}

func TestReconcileCollisionsRewritesATouchedDuplicateWithoutWidening(t *testing.T) {
	c := Collision{ID: "alp-1", Winner: "alpha", Losers: []string{"gamma"}}
	w := newWork()
	w.touched["issues"] = map[string]bool{"alp-1": true}
	if err := replayerWith(c).reconcileCollisions(w, map[string]recordedCollision{
		"alp-1": {Winner: "alpha", Losers: []string{"gamma"}},
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w.collisionsToWrite, []string{"alp-1"}) {
		t.Errorf("collisionsToWrite = %v", w.collisionsToWrite)
	}
	if w.touched["labels"] != nil || len(w.wide) != 0 {
		t.Errorf("an edit that left the decision alone must not widen: touched=%v wide=%v", w.touched, w.wide)
	}
}

func TestReconcileCollisionsWidensWhenTheDecisionChanges(t *testing.T) {
	// The winner moved from gamma to alpha: both stores' rows for the id are
	// stale in every bead-scoped table, and so are their fingerprints.
	c := Collision{ID: "shr-1", Winner: "alpha", Losers: []string{"gamma"}}
	w := newWork()
	if err := replayerWith(c).reconcileCollisions(w, map[string]recordedCollision{
		"shr-1": {Winner: "gamma", Losers: []string{"alpha"}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"issues", "labels"} {
		if !w.touched[table]["shr-1"] {
			t.Errorf("shr-1 was not reconciled in %s", table)
		}
	}
	if w.touched["config"] != nil {
		t.Error("a database-state table is not reconciled per bead")
	}
	if !w.wide["alpha"] || !w.wide["gamma"] {
		t.Errorf("both stores' fingerprints are stale: wide=%v", w.wide)
	}
	if !w.losers["shr-1"]["gamma"] || w.losers["shr-1"]["alpha"] {
		t.Errorf("losers = %v", w.losers)
	}
}

func TestReconcileCollisionsRemovesRecordsThatNoLongerDescribeAnything(t *testing.T) {
	w := newWork()
	if err := replayerWith().reconcileCollisions(w, map[string]recordedCollision{
		"alp-1": {Winner: "alpha", Losers: []string{"gamma"}},
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w.collisionsToDelete, []string{"alp-1"}) || !w.touched["issues"]["alp-1"] {
		t.Errorf("delete=%v touched=%v", w.collisionsToDelete, w.touched)
	}
}

func TestReconcileCollisionsRefusesContentDisagreementUnlessAllowed(t *testing.T) {
	c := Collision{ID: "alp-2", Winner: "alpha", Losers: []string{"gamma"}, DataColumnsDiffer: true, DifferingColumns: []string{"title"}}
	err := replayerWith(c).reconcileCollisions(newWork(), nil)
	if err == nil {
		t.Fatal("a new duplicate whose copies disagree must be refused")
	}
	for _, want := range []string{"refusing to replay", "alp-2", "alpha", "gamma", "title", "table issues", "--allow-collisions"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
	r := replayerWith(c)
	r.opts.AllowCollisions = true
	if err := r.reconcileCollisions(newWork(), nil); err != nil {
		t.Errorf("--allow-collisions must let it through: %v", err)
	}
	// A disagreement the build already accepted, on an id nothing touched, is
	// not a new decision and does not block.
	if err := replayerWith(c).reconcileCollisions(newWork(), map[string]recordedCollision{
		"alp-2": {Winner: "alpha", Losers: []string{"gamma"}},
	}); err != nil {
		t.Errorf("an already-recorded, untouched duplicate must not block: %v", err)
	}
}

func TestFingerprintStale(t *testing.T) {
	w := newWork()
	w.changedTables["alpha"] = map[string]bool{"issues": true}
	w.stateChanged["beta"] = map[string]bool{"config": true}
	w.full["labels"] = true
	w.wide = map[string]bool{"gamma": true}
	issues, labels, config := replayPlans()[0], replayPlans()[1], replayPlans()[2]
	cases := []struct {
		store string
		tp    TablePlan
		want  bool
	}{
		{"alpha", issues, true},
		{"alpha", labels, true}, // unversioned: every store reloads it
		{"alpha", config, false},
		{"beta", issues, false},
		{"beta", config, true},
		{"gamma", issues, true}, // a collision it takes part in changed
		{"delta", issues, false},
	}
	for _, tc := range cases {
		if got := w.fingerprintStale(tc.store, tc.tp); got != tc.want {
			t.Errorf("fingerprintStale(%s, %s) = %v, want %v", tc.store, tc.tp.Table, got, tc.want)
		}
	}
}

func TestWriteReplayReadsGenerally(t *testing.T) {
	var buf bytes.Buffer
	err := WriteReplay(&buf, ReplayResult{
		Database: "merged", DataDir: "/data", Beads: []string{"a-1"},
		ChangedStores: []string{"alpha"},
		Tables:        []ReplayTableStats{{Table: "issues", Deleted: 1, Inserted: 2}, {Table: "wisps", Full: true}},
		Commits:       []recordedSourceCommit{{Store: "alpha", Hash: "abc"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"brain unify replay", "stores with changes in their history: alpha", "beads reconciled: 1", "reloaded whole", "next replay starts", "--reference live"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}
