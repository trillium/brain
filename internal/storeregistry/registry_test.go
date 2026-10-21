package storeregistry_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/beads/internal/storeregistry"
	"github.com/steveyegge/beads/internal/storeregistry/regtest"
)

var meta = storeregistry.Meta{Actor: "test", Machine: "mini0"}

func TestValidNameAndPath(t *testing.T) {
	for _, n := range []string{"task", "side-quests", "a_b", "x1"} {
		if !storeregistry.ValidName(n) {
			t.Errorf("%q should be valid", n)
		}
	}
	for _, n := range []string{"", "-a", "A", "a b", "a/b", "a.b", "a;rm", "$(x)", strings.Repeat("a", 65), "../x"} {
		if storeregistry.ValidName(n) {
			t.Errorf("%q should be invalid", n)
		}
	}
	for _, p := range []string{"data/task/.beads", "data/a-b/.beads"} {
		if err := storeregistry.ValidRelPath(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	for _, p := range []string{"", "/etc/passwd", "~/x", "data/../x", "data//x", "data/x y", "data/$(id)", "data/`id`", "other/x", "data/a\"b", "data/a;b", "./data/x"} {
		if storeregistry.ValidRelPath(p) == nil {
			t.Errorf("%q should be refused", p)
		}
	}
}

func TestDigestStableAndSensitive(t *testing.T) {
	a, b := storeregistry.New("a"), storeregistry.New("b")
	d1 := storeregistry.Digest([]storeregistry.Entry{a, b})
	d2 := storeregistry.Digest([]storeregistry.Entry{b, a})
	if d1 != d2 || len(d1) != 64 {
		t.Fatalf("digest must be order independent and 64 hex: %s %s", d1, d2)
	}
	b2 := b
	b2.About = "line1\nline2|x"
	if storeregistry.Digest([]storeregistry.Entry{a, b2}) == d1 {
		t.Fatal("about must change the digest")
	}
	b3 := b
	b3.State = storeregistry.StateRemoved
	if storeregistry.Digest([]storeregistry.Entry{a, b3}) == d1 {
		t.Fatal("a tombstone must change the digest")
	}
	// Epoch bookkeeping fields are not part of the desired state.
	b4 := b
	b4.ChangedEpoch = 99
	if storeregistry.Digest([]storeregistry.Entry{a, b4}) != d1 {
		t.Fatal("changed_epoch must not change the digest")
	}
}

func TestEntryValidateRefusesPoisonedRows(t *testing.T) {
	e := storeregistry.New("ok")
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*storeregistry.Entry){
		"name":    func(e *storeregistry.Entry) { e.Name = "a;b" },
		"store":   func(e *storeregistry.Entry) { e.Store = "$(x)" },
		"path":    func(e *storeregistry.Entry) { e.RelPath = "data/../../x" },
		"kind":    func(e *storeregistry.Entry) { e.Kind = "evil" },
		"state":   func(e *storeregistry.Entry) { e.State = "" },
		"wrapper": func(e *storeregistry.Entry) { e.WrapperKind = "sh" },
	} {
		c := e
		mut(&c)
		if c.Validate() == nil {
			t.Errorf("%s: poisoned entry accepted", name)
		}
	}
}

func TestMutateLifecycleOnDolt(t *testing.T) {
	s := regtest.New(t)
	ctx := context.Background()

	if _, found, err := storeregistry.ReadState(ctx, s.DB); err != nil || found {
		t.Fatalf("fresh database: found=%v err=%v", found, err)
	}
	if es, err := storeregistry.ReadEntries(ctx, s.DB); err != nil || len(es) != 0 {
		t.Fatalf("fresh entries: %v %v", es, err)
	}

	res, err := storeregistry.Mutate(ctx, s.DB, meta, storeregistry.PlanCreate(storeregistry.New("recipes")))
	if err != nil || res.Epoch != 1 || !res.Changed {
		t.Fatalf("create: %+v %v", res, err)
	}
	// Idempotent re-create: no new epoch.
	res, err = storeregistry.Mutate(ctx, s.DB, meta, storeregistry.PlanCreate(storeregistry.New("recipes")))
	if err != nil || res.Epoch != 1 || res.Changed {
		t.Fatalf("re-create must not move the epoch: %+v %v", res, err)
	}
	if _, err := storeregistry.Mutate(ctx, s.DB, meta, storeregistry.PlanRename("recipes", "meals", "data/meals/.beads", false)); err != nil {
		t.Fatal(err)
	}
	snap, found, err := storeregistry.ReadSnapshot(ctx, s.DB)
	if err != nil || !found {
		t.Fatalf("snapshot: %v %v", found, err)
	}
	if snap.State.Epoch != 2 {
		t.Fatalf("epoch = %d, want 2", snap.State.Epoch)
	}
	by := map[string]storeregistry.Entry{}
	for _, e := range snap.Entries {
		by[e.Name] = e
	}
	if by["recipes"].State != storeregistry.StateRemoved || by["meals"].State != storeregistry.StateActive {
		t.Fatalf("rename: %+v", by)
	}
	if by["meals"].Store != "recipes" {
		t.Fatalf("a renamed store keeps its canonical namespace: %+v", by["meals"])
	}
	if by["meals"].CreatedEpoch != 2 || by["recipes"].CreatedEpoch != 1 || by["recipes"].ChangedEpoch != 2 {
		t.Fatalf("epoch bookkeeping: %+v", by)
	}

	if _, err := storeregistry.Mutate(ctx, s.DB, meta, storeregistry.PlanAlias("meals", "food")); err != nil {
		t.Fatal(err)
	}
	if _, err := storeregistry.Mutate(ctx, s.DB, meta, storeregistry.PlanAbout("meals", "what we eat")); err != nil {
		t.Fatal(err)
	}
	if _, err := storeregistry.Mutate(ctx, s.DB, meta, storeregistry.PlanEditBack("meals", true)); err != nil {
		t.Fatal(err)
	}
	// Remove cascades to aliases and leaves tombstones.
	if _, err := storeregistry.Mutate(ctx, s.DB, meta, storeregistry.PlanRemove("meals")); err != nil {
		t.Fatal(err)
	}
	snap, _, err = storeregistry.ReadSnapshot(ctx, s.DB)
	if err != nil {
		t.Fatal(err)
	}
	if snap.State.Epoch != 6 || len(storeregistry.Active(snap.Entries)) != 0 || len(storeregistry.Removed(snap.Entries)) != 3 {
		t.Fatalf("after remove: epoch=%d entries=%+v", snap.State.Epoch, snap.Entries)
	}
	// Removing twice is refused, not silently accepted.
	if _, err := storeregistry.Mutate(ctx, s.DB, meta, storeregistry.PlanRemove("meals")); err == nil {
		t.Fatal("second remove must be refused")
	}

	log, err := storeregistry.ReadLog(ctx, s.DB, 50)
	if err != nil || len(log) < 6 {
		t.Fatalf("log: %v %v", log, err)
	}
	if log[len(log)-1].Epoch != 1 || log[len(log)-1].Op != storeregistry.OpCreate || log[len(log)-1].Machine != "mini0" {
		t.Fatalf("oldest log row: %+v", log[len(log)-1])
	}
}

func TestMutateRollsBackEverythingWhenInTxFails(t *testing.T) {
	s := regtest.New(t)
	ctx := context.Background()
	if _, err := storeregistry.Mutate(ctx, s.DB, meta, storeregistry.PlanCreate(storeregistry.New("keep"))); err != nil {
		t.Fatal(err)
	}
	m := meta
	m.InTx = func(ctx context.Context, tx storeregistry.DBTX) error {
		if _, err := tx.ExecContext(ctx, "REPLACE INTO brain_unified_config (`store`,`key`,value) VALUES ('doomed','issue_prefix','doomed')"); err != nil {
			return err
		}
		return errors.New("namespace seed refused")
	}
	if _, err := storeregistry.Mutate(ctx, s.DB, m, storeregistry.PlanCreate(storeregistry.New("doomed"))); err == nil {
		t.Fatal("expected the in-transaction failure")
	}
	st, _, _ := storeregistry.ReadState(ctx, s.DB)
	if st.Epoch != 1 {
		t.Fatalf("a failed create must not move the epoch, got %d", st.Epoch)
	}
	es, _ := storeregistry.ReadEntries(ctx, s.DB)
	if len(es) != 1 || es[0].Name != "keep" {
		t.Fatalf("entries: %+v", es)
	}
	var n int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM brain_unified_config WHERE `store`='doomed'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("the namespace seed must roll back with the registry write: n=%d err=%v", n, err)
	}
}

func TestConcurrentCreatorsEachGetOneEpochNoLostUpdate(t *testing.T) {
	s := regtest.New(t)
	ctx := context.Background()
	if _, err := storeregistry.Mutate(ctx, s.DB, meta, storeregistry.PlanCreate(storeregistry.New("seed"))); err != nil {
		t.Fatal(err)
	}
	const writers = 6
	var wg sync.WaitGroup
	epochs := make([]int64, writers)
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db := s.Open(t)
			r, err := storeregistry.Mutate(ctx, db, storeregistry.Meta{Actor: "w", Machine: fmt.Sprintf("m%d", i)},
				storeregistry.PlanCreate(storeregistry.New(fmt.Sprintf("store%d", i))))
			epochs[i], errs[i] = r.Epoch, err
		}(i)
	}
	wg.Wait()
	seen := map[int64]bool{}
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("writer %d: %v", i, errs[i])
		}
		if seen[epochs[i]] {
			t.Fatalf("two writers got epoch %d: %v", epochs[i], epochs)
		}
		seen[epochs[i]] = true
	}
	snap, _, err := storeregistry.ReadSnapshot(ctx, s.DB)
	if err != nil {
		t.Fatal(err)
	}
	if snap.State.Epoch != writers+1 || len(snap.Entries) != writers+1 {
		t.Fatalf("lost update: epoch=%d entries=%d", snap.State.Epoch, len(snap.Entries))
	}
	log, _ := storeregistry.ReadLog(ctx, s.DB, 100)
	if len(log) != writers+1 {
		t.Fatalf("log rows = %d, want %d", len(log), writers+1)
	}
}

func TestSnapshotRefusesTornRead(t *testing.T) {
	s := regtest.New(t)
	ctx := context.Background()
	if _, err := storeregistry.Mutate(ctx, s.DB, meta, storeregistry.PlanCreate(storeregistry.New("a"))); err != nil {
		t.Fatal(err)
	}
	// A hand edit of the entries without the epoch moving.
	if _, err := s.DB.Exec("UPDATE brain_registry_entries SET about = 'sneaky' WHERE name = 'a'"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storeregistry.ReadSnapshot(ctx, s.DB); !errors.Is(err, storeregistry.ErrTornRead) {
		t.Fatalf("want ErrTornRead, got %v", err)
	}
}

func TestBackfillIsIdempotentAndLeavesExistingRows(t *testing.T) {
	s := regtest.New(t)
	ctx := context.Background()
	a := storeregistry.New("a")
	a.About = "mine"
	if _, err := storeregistry.Mutate(ctx, s.DB, meta, storeregistry.PlanCreate(a)); err != nil {
		t.Fatal(err)
	}
	other := storeregistry.New("a")
	other.About = "theirs"
	b := storeregistry.New("b")
	r, err := storeregistry.Mutate(ctx, s.DB, meta, storeregistry.PlanBackfill([]storeregistry.Entry{other, b}))
	if err != nil || r.Epoch != 2 || len(r.Changes) != 1 {
		t.Fatalf("backfill: %+v %v", r, err)
	}
	r, err = storeregistry.Mutate(ctx, s.DB, meta, storeregistry.PlanBackfill([]storeregistry.Entry{other, b}))
	if err != nil || r.Changed || r.Epoch != 2 {
		t.Fatalf("second backfill must be a no-op: %+v %v", r, err)
	}
	es, _ := storeregistry.ReadEntries(ctx, s.DB)
	if es[0].About != "mine" {
		t.Fatalf("backfill overwrote an existing row: %+v", es[0])
	}
}

func TestReportMachineDoesNotMoveEpoch(t *testing.T) {
	s := regtest.New(t)
	ctx := context.Background()
	if _, err := storeregistry.Mutate(ctx, s.DB, meta, storeregistry.PlanCreate(storeregistry.New("a"))); err != nil {
		t.Fatal(err)
	}
	st, _, _ := storeregistry.ReadState(ctx, s.DB)
	m := storeregistry.Machine{Machine: "mini1", Role: "direct", AppliedEpoch: 1, AppliedDigest: st.Digest, WrappersOK: 1}
	for i := 0; i < 2; i++ { // upsert
		if err := storeregistry.ReportMachine(ctx, s.DB, m, testNow()); err != nil {
			t.Fatal(err)
		}
	}
	ms, err := storeregistry.ReadMachines(ctx, s.DB)
	if err != nil || len(ms) != 1 || ms[0].Machine != "mini1" || ms[0].AppliedEpoch != 1 {
		t.Fatalf("machines: %+v %v", ms, err)
	}
	after, _, _ := storeregistry.ReadState(ctx, s.DB)
	if after.Epoch != st.Epoch {
		t.Fatalf("a machine report moved the epoch: %d -> %d", st.Epoch, after.Epoch)
	}
}

func TestEpochReadIsCheap(t *testing.T) {
	s := regtest.New(t)
	ctx := context.Background()
	if _, err := storeregistry.Mutate(ctx, s.DB, meta, storeregistry.PlanCreate(storeregistry.New("a"))); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := storeregistry.ReadEpoch(ctx, s.DB); err != nil { // warm the connection
		t.Fatal(err)
	}
	r := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, _, _, err := storeregistry.ReadEpoch(ctx, s.DB); err != nil {
				b.Fatal(err)
			}
		}
	})
	per := r.NsPerOp()
	t.Logf("ride-along epoch read: %.2f ms/op over %d iterations", float64(per)/1e6, r.N)
	if per > 10_000_000 { // 10ms, generous for a loaded CI box; measured ~0.3ms
		t.Fatalf("the epoch read costs %d ns/op, want well under 10ms", per)
	}
}
