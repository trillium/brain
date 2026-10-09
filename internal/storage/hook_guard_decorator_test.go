package storage_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/hooksdef"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// guardFakeStore is a DoltStorage with only what the guard decorator touches.
// Any other method panics on the nil embedded interface, which is the point:
// the decorator must not reach for anything else.
type guardFakeStore struct {
	storage.DoltStorage
	issues  map[string]*types.Issue
	config  map[string]string
	created int
	closed  []string
	cfgErr  error
}

func newGuardFake() *guardFakeStore {
	return &guardFakeStore{issues: map[string]*types.Issue{}, config: map[string]string{}}
}

func (f *guardFakeStore) CreateIssue(_ context.Context, i *types.Issue, _ string) error {
	f.created++
	i.ID = "fk-" + string(rune('a'+f.created))
	f.issues[i.ID] = i
	return nil
}
func (f *guardFakeStore) GetIssue(_ context.Context, id string) (*types.Issue, error) {
	if i, ok := f.issues[id]; ok {
		return i, nil
	}
	return nil, errors.New("not found")
}
func (f *guardFakeStore) CloseIssue(_ context.Context, id, _, _, _ string) error {
	f.closed = append(f.closed, id)
	return nil
}
func (f *guardFakeStore) SetConfig(_ context.Context, k, v string) error {
	if f.cfgErr != nil {
		return f.cfgErr
	}
	f.config[k] = v
	return nil
}
func (f *guardFakeStore) RunInTransaction(_ context.Context, _ string, fn func(storage.Transaction) error) error {
	return fn(&guardFakeTx{f: f})
}

type guardFakeTx struct {
	storage.Transaction
	f *guardFakeStore
}

func (t *guardFakeTx) CreateIssue(ctx context.Context, i *types.Issue, a string) error {
	return t.f.CreateIssue(ctx, i, a)
}
func (t *guardFakeTx) GetIssue(ctx context.Context, id string) (*types.Issue, error) {
	return t.f.GetIssue(ctx, id)
}

// scripted maps a hook name to the result it "runs" with, and records payloads.
type scripted struct {
	results  map[string]hooksdef.RunResult
	payloads []hooksdef.Payload
	ran      []string
}

func (s *scripted) exec(_ context.Context, d hooksdef.Definition, p hooksdef.Payload) hooksdef.RunResult {
	s.ran = append(s.ran, d.Name)
	s.payloads = append(s.payloads, p)
	return s.results[d.Name]
}

func def(name, policy, when string) hooksdef.Definition {
	return hooksdef.Definition{Name: name, Path: "/h/" + name + ".toml", Policy: policy, When: when,
		RefusalExit: 2, Timeout: time.Second, Run: "x"}
}

func guarded(t *testing.T, f *guardFakeStore, s *scripted, defs ...hooksdef.Definition) (*storage.HookGuardStore, *bytes.Buffer) {
	t.Helper()
	var errBuf bytes.Buffer
	return storage.NewHookGuardStore(f, defs, storage.HookGuardOptions{
		Command: "create", Store: "proof", Exec: s.exec, Stderr: &errBuf,
		Now: func() time.Time { return time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC) },
	}), &errBuf
}

func warningsIn(t *testing.T, f *guardFakeStore) []hooksdef.Warning {
	t.Helper()
	ws, errs := hooksdef.WarningsFromConfig(f.config)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	return ws
}

func TestGuardRefusalMeansNothingIsWritten(t *testing.T) {
	f := newGuardFake()
	s := &scripted{results: map[string]hooksdef.RunResult{"no-secrets": {ExitCode: 2, Stderr: "secret in title"}}}
	g, _ := guarded(t, f, s, def("no-secrets", hooksdef.PolicyGuard, "create"))

	err := g.CreateIssue(context.Background(), &types.Issue{Title: "AKIA..."}, "me")
	var ref *hooksdef.Refusal
	if !errors.As(err, &ref) || ref.Hook != "no-secrets" || ref.Cause != hooksdef.CauseRefused {
		t.Fatalf("want a Refusal naming the hook, got %v", err)
	}
	if f.created != 0 || len(f.issues) != 0 {
		t.Fatalf("guard refused but the store was written: created=%d", f.created)
	}
	if len(f.config) != 0 {
		t.Fatalf("a refusal is not a warning; nothing should be recorded: %v", f.config)
	}
	if got := s.payloads[0]; got.Event != "create" || got.Issue == nil || got.Issue.Title != "AKIA..." || got.Store != "proof" || got.Actor != "me" {
		t.Fatalf("payload %+v", got)
	}
}

func TestGuardFailureFailsClosedAndObserversDoNotRunOnRefusal(t *testing.T) {
	f := newGuardFake()
	s := &scripted{results: map[string]hooksdef.RunResult{"crash": {ExitCode: 7, Stderr: "boom"}, "obs": {}}}
	g, _ := guarded(t, f, s, def("crash", hooksdef.PolicyGuard, "*"), def("obs", hooksdef.PolicyObserver, "*"))
	err := g.CreateIssue(context.Background(), &types.Issue{Title: "x"}, "me")
	var ref *hooksdef.Refusal
	if !errors.As(err, &ref) || ref.Cause != hooksdef.CauseFailed || !strings.Contains(err.Error(), `"crash"`) {
		t.Fatalf("got %v", err)
	}
	if f.created != 0 || len(s.ran) != 1 {
		t.Fatalf("created=%d ran=%v; observers must not run for a write that did not happen", f.created, s.ran)
	}
}

func TestObserverFailureFailsOpenAndRecordsRoutableWarning(t *testing.T) {
	f := newGuardFake()
	s := &scripted{results: map[string]hooksdef.RunResult{"mirror": {ExitCode: 3, Stderr: "host down"}}}
	g, errBuf := guarded(t, f, s, def("mirror", hooksdef.PolicyObserver, "create"))

	if err := g.CreateIssue(context.Background(), &types.Issue{Title: "ok"}, "me"); err != nil {
		t.Fatalf("observer failure must not block: %v", err)
	}
	if f.created != 1 {
		t.Fatal("write did not happen")
	}
	ws := warningsIn(t, f)
	if len(ws) != 1 {
		t.Fatalf("want exactly one warning, got %v", ws)
	}
	w := ws[0]
	if w.Hook != "mirror" || w.Reason != hooksdef.ReasonFailure || w.Status != hooksdef.StatusOpen ||
		w.Subject != "fk-b" || w.Command != "create" || w.Event != "create" ||
		!strings.Contains(w.Detail, "host down") || !strings.HasPrefix(w.ID, "hw-20261008T090000Z-") {
		t.Fatalf("warning %+v", w)
	}
	if !strings.Contains(errBuf.String(), w.ID) || !strings.Contains(errBuf.String(), "hook warnings") {
		t.Fatalf("stderr should carry the id and where to look: %q", errBuf.String())
	}
}

func TestObserverSeesPostWritePayloadWithID(t *testing.T) {
	f := newGuardFake()
	s := &scripted{results: map[string]hooksdef.RunResult{"obs": {}}}
	g, _ := guarded(t, f, s, def("obs", hooksdef.PolicyObserver, "create"))
	if err := g.CreateIssue(context.Background(), &types.Issue{Title: "t"}, "me"); err != nil {
		t.Fatal(err)
	}
	if got := s.payloads[0].Issue.ID; got != "fk-b" {
		t.Fatalf("observer should see the created id, got %q", got)
	}
	if len(f.config) != 0 {
		t.Fatalf("a passing observer records nothing: %v", f.config)
	}
}

func TestUnsetPolicyWarnsButNeverBlocks(t *testing.T) {
	f := newGuardFake()
	s := &scripted{results: map[string]hooksdef.RunResult{"u": {}}}
	g, _ := guarded(t, f, s, def("u", hooksdef.PolicyUnset, "create"))
	if err := g.CreateIssue(context.Background(), &types.Issue{Title: "t"}, "me"); err != nil {
		t.Fatal(err)
	}
	ws := warningsIn(t, f)
	if f.created != 1 || len(ws) != 1 || ws[0].Reason != hooksdef.ReasonUnsetPolicy || ws[0].Hook != "u" {
		t.Fatalf("created=%d warnings=%v", f.created, ws)
	}
}

func TestWhenSelectsEvents(t *testing.T) {
	f := newGuardFake()
	s := &scripted{results: map[string]hooksdef.RunResult{"closer": {ExitCode: 2}}}
	g, _ := guarded(t, f, s, def("closer", hooksdef.PolicyGuard, "close"))
	if err := g.CreateIssue(context.Background(), &types.Issue{Title: "t"}, "me"); err != nil {
		t.Fatalf("a close guard must not fire on create: %v", err)
	}
	err := g.CloseIssue(context.Background(), "fk-b", "done", "me", "")
	if err == nil || len(f.closed) != 0 {
		t.Fatalf("close should be refused before happening: err=%v closed=%v", err, f.closed)
	}
	if strings.Contains(err.Error(), "bead not created") {
		t.Fatalf("close refusal must not claim a create: %v", err)
	}
	if p := s.payloads[len(s.payloads)-1]; p.Event != "close" || p.Reason != "done" || p.Issue == nil || p.Issue.ID != "fk-b" {
		t.Fatalf("close payload %+v", p)
	}
}

func TestTransactionGuardsRefuseAndObserversWaitForCommit(t *testing.T) {
	f := newGuardFake()
	s := &scripted{results: map[string]hooksdef.RunResult{"g": {ExitCode: 2, Stderr: "no"}, "o": {ExitCode: 1}}}
	g, _ := guarded(t, f, s, def("g", hooksdef.PolicyGuard, "create"))
	err := g.RunInTransaction(context.Background(), "msg", func(tx storage.Transaction) error {
		return tx.CreateIssue(context.Background(), &types.Issue{Title: "t"}, "me")
	})
	var ref *hooksdef.Refusal
	if !errors.As(err, &ref) || f.created != 0 {
		t.Fatalf("tx create should be refused: err=%v created=%d", err, f.created)
	}

	f2 := newGuardFake()
	s2 := &scripted{results: map[string]hooksdef.RunResult{"o": {ExitCode: 1}}}
	g2, _ := guarded(t, f2, s2, def("o", hooksdef.PolicyObserver, "create"))
	err = g2.RunInTransaction(context.Background(), "msg", func(tx storage.Transaction) error {
		if err := tx.CreateIssue(context.Background(), &types.Issue{Title: "t"}, "me"); err != nil {
			return err
		}
		if len(s2.ran) != 0 {
			t.Error("observer ran before the transaction committed")
		}
		return nil
	})
	if err != nil || f2.created != 1 || len(warningsIn(t, f2)) != 1 {
		t.Fatalf("err=%v created=%d warnings=%v", err, f2.created, f2.config)
	}

	// A rolled-back transaction fires no observers.
	f3 := newGuardFake()
	s3 := &scripted{results: map[string]hooksdef.RunResult{"o": {ExitCode: 1}}}
	g3, _ := guarded(t, f3, s3, def("o", hooksdef.PolicyObserver, "create"))
	_ = g3.RunInTransaction(context.Background(), "msg", func(tx storage.Transaction) error {
		_ = tx.CreateIssue(context.Background(), &types.Issue{Title: "t"}, "me")
		return errors.New("rollback")
	})
	if len(s3.ran) != 0 || len(f3.config) != 0 {
		t.Fatalf("rolled-back tx must run no observers: ran=%v", s3.ran)
	}
}

func TestUnrecordableWarningIsAnnouncedNotSwallowed(t *testing.T) {
	f := newGuardFake()
	f.cfgErr = errors.New("disk full")
	s := &scripted{results: map[string]hooksdef.RunResult{"o": {ExitCode: 1, Stderr: "x"}}}
	g, errBuf := guarded(t, f, s, def("o", hooksdef.PolicyObserver, "create"))
	if err := g.CreateIssue(context.Background(), &types.Issue{Title: "t"}, "me"); err != nil {
		t.Fatalf("the write already happened and must not be reported as failed: %v", err)
	}
	if !strings.Contains(errBuf.String(), "NOT RECORDED") || !strings.Contains(errBuf.String(), "disk full") || !strings.Contains(errBuf.String(), `"o"`) {
		t.Fatalf("stderr %q", errBuf.String())
	}
}

func TestGuardStoreIsUnwrappable(t *testing.T) {
	f := newGuardFake()
	g := storage.NewHookGuardStore(f, nil, storage.HookGuardOptions{})
	if storage.UnwrapStore(g) != storage.DoltStorage(f) {
		t.Fatal("UnwrapStore must peel the guard decorator")
	}
}
