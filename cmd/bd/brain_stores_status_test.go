package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBrainStoresStatusCmd_Registered(t *testing.T) {
	found, _, err := rootCmd.Find([]string{"brain", "stores", "status"})
	if err != nil || found == nil || found.Name() != "status" {
		t.Fatalf("brain stores status not registered: %v", err)
	}
	if !storesCommandCanRunWithoutStore(found) {
		t.Error("stores status must run from a directory with no store")
	}
}

// fakeStatusFleet answers the three verbs snapshotStore issues from per-store tables.
type fakeStatusFleet struct {
	status  map[string]string // store -> by-status JSON
	ready   map[string]string
	updated map[string]string
	down    map[string]string // store -> error
}

func (f *fakeStatusFleet) run(_ context.Context, name string, _ storeEntry, args ...string) ([]byte, error) {
	if msg, ok := f.down[name]; ok {
		return nil, errors.New(msg)
	}
	switch {
	case args[0] == "ready":
		return []byte(f.ready[name]), nil
	case len(args) > 1 && args[1] == "--updated-after":
		return []byte(f.updated[name]), nil
	}
	return []byte(f.status[name]), nil
}

func newFakeStatusFleet() *fakeStatusFleet {
	return &fakeStatusFleet{
		status: map[string]string{
			"alpha": `{"groups":[{"count":3,"group":"open"},{"count":5,"group":"closed"}],"total":8}`,
			"beta":  `{"groups":[{"count":1,"group":"open"}],"total":1}`,
		},
		ready:   map[string]string{"alpha": `[{"id":"a-1"},{"id":"a-2"}]`, "beta": `[]`},
		updated: map[string]string{"alpha": `{"count":4}`, "beta": `{"count":0}`},
		down:    map[string]string{},
	}
}

func TestSnapshotStore(t *testing.T) {
	f := newFakeStatusFleet()
	s := snapshotStore(f.run, "alpha", storeEntry{}, time.Now(), time.Second)
	if !s.Reachable || s.Total != 8 || s.Open != 3 || s.Ready != 2 || s.Updated != 4 || s.ByStatus["closed"] != 5 {
		t.Fatalf("unexpected snapshot: %+v", s)
	}

	f.down["alpha"] = "boom"
	s = snapshotStore(f.run, "alpha", storeEntry{}, time.Now(), time.Second)
	if s.Reachable || !strings.Contains(s.Reason, "boom") || s.Total != 0 {
		t.Fatalf("failed store must be unreachable with zero counts: %+v", s)
	}

	f.down = map[string]string{}
	f.ready["alpha"] = `not json`
	s = snapshotStore(f.run, "alpha", storeEntry{}, time.Now(), time.Second)
	if s.Reachable || !strings.Contains(s.Reason, "unparseable") {
		t.Fatalf("garbage output must mark the store unreachable: %+v", s)
	}
}

func TestDiffStoreSnapshots(t *testing.T) {
	base := storeSnapshot{Name: "alpha", Reachable: true, Total: 8, Open: 3, Ready: 2}
	if got := diffStoreSnapshots(base, base); len(got) != 0 {
		t.Errorf("unchanged store must be silent, got %v", got)
	}
	moved := base
	moved.Total, moved.Open = 9, 4
	got := diffStoreSnapshots(base, moved)
	if len(got) != 1 || !strings.Contains(got[0], "total 8→9") || !strings.Contains(got[0], "open 3→4") {
		t.Errorf("count movement not reported: %v", got)
	}
	edited := base
	edited.Updated = 2
	got = diffStoreSnapshots(base, edited)
	if len(got) != 1 || !strings.Contains(got[0], "2 updated") {
		t.Errorf("an edit that leaves counts alone must still be reported: %v", got)
	}
	down := storeSnapshot{Name: "alpha", Reason: "gone"}
	if got := diffStoreSnapshots(base, down); len(got) != 1 || !strings.Contains(got[0], "alpha down: gone") {
		t.Errorf("going down not reported: %v", got)
	}
	if got := diffStoreSnapshots(down, down); len(got) != 0 {
		t.Errorf("a store that stays down must not repeat: %v", got)
	}
	if got := diffStoreSnapshots(down, base); len(got) != 1 || !strings.Contains(got[0], "alpha up") {
		t.Errorf("recovery not reported: %v", got)
	}
}

func TestRunStoresStatus_Snapshot(t *testing.T) {
	f := newFakeStatusFleet()
	f.down["beta"] = "database not found"
	stores := map[string]storeEntry{"alpha": {}, "beta": {}}
	var out bytes.Buffer
	failed := runStoresStatus(&out, f.run, stores, []string{"alpha", "beta"}, statusOpts{Jobs: 2, Timeout: time.Second, Recent: time.Hour})
	if len(failed) != 1 || failed[0] != "beta" {
		t.Fatalf("failed = %v, want [beta]", failed)
	}
	text := out.String()
	for _, want := range []string{"alpha", "DOWN  count: database not found", "FLEET", "1/2 stores reachable"} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q:\n%s", want, text)
		}
	}
}

func TestRunStoresStatus_WatchEmitsOnlyChanges(t *testing.T) {
	f := newFakeStatusFleet()
	stores := map[string]storeEntry{"alpha": {}, "beta": {}}
	polls := 0
	run := func(ctx context.Context, name string, e storeEntry, args ...string) ([]byte, error) {
		// Baseline (poll 0) and poll 1 see the same state; alpha gains a bead
		// from poll 2 on, so only that poll's alpha row reports it.
		if args[0] == "count" && args[1] == "--by-status" && name == "alpha" {
			polls++
			if polls >= 3 {
				return []byte(`{"groups":[{"count":4,"group":"open"},{"count":5,"group":"closed"}],"total":9}`), nil
			}
		}
		return f.run(ctx, name, e, args...)
	}
	f.updated["alpha"] = `{"count":0}`
	var out bytes.Buffer
	runStoresStatus(&out, run, stores, []string{"alpha", "beta"}, statusOpts{
		Jobs: 1, Timeout: time.Second, Recent: time.Hour, Watch: true, Interval: time.Millisecond, Polls: 3,
	})
	lines := strings.Split(out.String(), "\n")
	changed := 0
	for _, l := range lines {
		if strings.Contains(l, "alpha changed: total 8→9, open 3→4") {
			changed++
		}
		if strings.Contains(l, "beta changed") {
			t.Errorf("beta never changed, got %q", l)
		}
	}
	if changed != 1 {
		t.Errorf("want exactly one alpha change line, got %d:\n%s", changed, out.String())
	}
}

func TestFailureReason(t *testing.T) {
	if got := failureReason([]byte(`{"error":"db missing\nmore"}`), []byte("Warning: perms\n")); got != "db missing" {
		t.Errorf("JSON error should win over a warning line, got %q", got)
	}
	if got := failureReason(nil, []byte("Warning: perms\nError: nope\n")); got != "Error: nope" {
		t.Errorf("warning lines must be skipped, got %q", got)
	}
	if got := failureReason(nil, []byte("Warning: only\n")); got != "Warning: only" {
		t.Errorf("fall back to the first line, got %q", got)
	}
}
