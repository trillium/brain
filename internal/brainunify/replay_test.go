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

func identical(id, winner string, losers ...string) Collision {
	return Collision{ID: id, Winner: winner, Losers: losers, Resolution: ResolutionMergedIdentical}
}

func conflict(id string, copies map[string]string, stores ...string) Collision {
	c := Collision{ID: id, Divergent: true, Resolution: ResolutionConflict}
	for _, s := range stores {
		c.Copies = append(c.Copies, ConflictCopy{Store: s, ID: copies[s]})
	}
	return c
}

func recordedIdentical(winner string, losers ...string) recordedCollision {
	return recordedCollision{Resolution: ResolutionMergedIdentical, Winner: winner, Losers: losers}
}

func TestReconcileCollisionsLeavesUnchangedDecisionsAlone(t *testing.T) {
	c := identical("alp-1", "alpha", "gamma")
	w := newWork()
	err := replayerWith(c).reconcileCollisions(w, map[string]recordedCollision{
		"alp-1": recordedIdentical("alpha", "gamma"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.collisionsToWrite) != 0 || len(w.collisionsToDelete) != 0 || len(w.touched) != 0 || len(w.wide) != 0 {
		t.Errorf("an unchanged, untouched duplicate must cost nothing: %+v", w)
	}
}

func TestReconcileCollisionsRewritesATouchedDuplicateWithoutWidening(t *testing.T) {
	c := identical("alp-1", "alpha", "gamma")
	w := newWork()
	w.touched["issues"] = map[string]bool{"alp-1": true}
	if err := replayerWith(c).reconcileCollisions(w, map[string]recordedCollision{
		"alp-1": recordedIdentical("alpha", "gamma"),
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
	c := identical("shr-1", "alpha", "gamma")
	w := newWork()
	if err := replayerWith(c).reconcileCollisions(w, map[string]recordedCollision{
		"shr-1": recordedIdentical("gamma", "alpha"),
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
}

func TestReconcileCollisionsRemovesRecordsThatNoLongerDescribeAnything(t *testing.T) {
	w := newWork()
	if err := replayerWith().reconcileCollisions(w, map[string]recordedCollision{
		"alp-1": recordedIdentical("alpha", "gamma"),
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w.collisionsToDelete, []string{"alp-1"}) || !w.touched["issues"]["alp-1"] {
		t.Errorf("delete=%v touched=%v", w.collisionsToDelete, w.touched)
	}
}

func TestReconcileCollisionsNeverRefusesACopyThatDiffers(t *testing.T) {
	// A copy that differs used to need --allow-collisions because keeping one
	// copy discarded the other's state. Both are kept now, so a new duplicate
	// whose copies disagree is just reconciled.
	c := conflict("alp-2", map[string]string{"alpha": "alp-aaaaaaaaaaaa", "gamma": "gam-bbbbbbbbbbbb"}, "alpha", "gamma")
	c.DataColumnsDiffer, c.DifferingColumns = true, []string{"title"}
	w := newWork()
	if err := replayerWith(c).reconcileCollisions(w, nil); err != nil {
		t.Fatalf("a conflict must not be refused: %v", err)
	}
	if !reflect.DeepEqual(w.collisionsToWrite, []string{"alp-2"}) || !w.touched["issues"]["alp-2"] {
		t.Errorf("the new conflict was not reconciled: write=%v touched=%v", w.collisionsToWrite, w.touched)
	}
}

func TestReconcileCollisionsWidensWhenIdenticalCopiesBecomeAConflict(t *testing.T) {
	c := conflict("alp-2", map[string]string{"alpha": "alp-aaaaaaaaaaaa", "gamma": "gam-bbbbbbbbbbbb"}, "alpha", "gamma")
	w := newWork()
	if err := replayerWith(c).reconcileCollisions(w, map[string]recordedCollision{
		"alp-2": recordedIdentical("alpha", "gamma"),
	}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"issues", "labels"} {
		if !w.touched[table]["alp-2"] {
			t.Errorf("alp-2 was not reconciled in %s", table)
		}
	}
	if !w.wide["alpha"] || !w.wide["gamma"] {
		t.Errorf("both holders' fingerprints are stale: wide=%v", w.wide)
	}
}

func TestReconcileCollisionsRereadsConflictsWhenATableIsReloadedWhole(t *testing.T) {
	// A table the sources keep no history for (wisps) is reloaded whole, and a
	// change there to a conflict's copy appears in no diff: the record's digest
	// of the copy is taken again on every replay that reloads such a table.
	c := conflict("alp-2", map[string]string{"alpha": "alp-aaaaaaaaaaaa", "gamma": "gam-bbbbbbbbbbbb"}, "alpha", "gamma")
	rec := map[string]recordedCollision{"alp-2": {Resolution: ResolutionConflict, CopyIDs: c.copyIDs()}}
	w := newWork()
	if err := replayerWith(c).reconcileCollisions(w, rec); err != nil || len(w.collisionsToWrite) != 0 {
		t.Fatalf("with every table versioned an untouched, unchanged conflict costs nothing: %v %v", err, w.collisionsToWrite)
	}
	w = newWork()
	w.full["wisps"] = true
	if err := replayerWith(c).reconcileCollisions(w, rec); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w.collisionsToWrite, []string{"alp-2"}) {
		t.Errorf("collisionsToWrite = %v", w.collisionsToWrite)
	}
	if len(w.wide) != 0 {
		t.Errorf("the decision did not change, so no fingerprint is stale: %v", w.wide)
	}
}

func TestMergedIDsCoverEveryIDTheOldAndNewResolutionUsed(t *testing.T) {
	// The id used to be a conflict whose copies were minted for stores alpha and
	// gamma; now it is one for alpha and beta. Replaying it must delete the
	// conflict bead, the minted ids of the old decision and those of the new.
	live := conflict("alp-2", map[string]string{"alpha": "alp-aaaaaaaaaaaa", "beta": "bet-cccccccccccc"}, "alpha", "beta")
	w := newWork()
	w.live = map[string]Collision{"alp-2": live}
	w.recorded = map[string]recordedCollision{
		"alp-2": {Resolution: ResolutionConflict, CopyIDs: map[string]string{"alpha": "alp-aaaaaaaaaaaa", "gamma": "gam-bbbbbbbbbbbb"}},
	}
	got := w.mergedIDs([]string{"alp-2", "alp-3"})
	want := []string{"alp-2", "alp-3", "alp-aaaaaaaaaaaa", "bet-cccccccccccc", "gam-bbbbbbbbbbbb"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergedIDs = %v, want %v", got, want)
	}
}

func TestRecordedCollisionMatches(t *testing.T) {
	ids := map[string]string{"alpha": "alp-aaaaaaaaaaaa", "gamma": "gam-bbbbbbbbbbbb"}
	c := conflict("x-1", ids, "alpha", "gamma")
	if !(recordedCollision{Resolution: ResolutionConflict, CopyIDs: ids}).matches(c) {
		t.Error("a record naming the same copies matches")
	}
	if (recordedCollision{Resolution: ResolutionConflict, CopyIDs: map[string]string{"alpha": "alp-aaaaaaaaaaaa"}}).matches(c) {
		t.Error("a record naming fewer copies does not match")
	}
	if recordedIdentical("alpha", "gamma").matches(c) {
		t.Error("a record that merged the copies does not match a conflict")
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
