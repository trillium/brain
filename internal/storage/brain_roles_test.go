package storage

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/hooksdef"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// Brain's two decorators and the beads 1.3 write roles. The CLI's writes go
// through role values taken off the store, so a decorator that only wraps the
// DoltStorage mutation methods would be bypassed by every one of them.

// hookFiringRoleAccessors lists the role accessors HookFiringStore wraps with
// one of its completion-hook role decorators, read from this package's
// source: a method on *HookFiringStore that returns &hookX{inner: inner, hooks: h}.
func hookFiringRoleAccessors(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("hook_*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 || fn.Body == nil {
				continue
			}
			star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			if id, ok := star.X.(*ast.Ident); !ok || id.Name != "HookFiringStore" {
				continue
			}
			wraps := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if id, ok := lit.Type.(*ast.Ident); ok && strings.HasPrefix(id.Name, "hook") {
					for _, el := range lit.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "hooks" {
								wraps = true
							}
						}
					}
				}
				return true
			})
			if wraps {
				out = append(out, fn.Name.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

func declaredMethods(t *testing.T, file, recv string) map[string]bool {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, d := range parsed.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		if star, ok := fn.Recv.List[0].Type.(*ast.StarExpr); ok {
			if id, ok := star.X.(*ast.Ident); ok && id.Name == recv {
				out[fn.Name.Name] = true
			}
		}
	}
	return out
}

// TestBrainExfiltrationDecoratorWrapsEveryHookFiringRole keeps the markdown
// renderer in step with upstream: every role HookFiringStore fires hooks for
// is a role the renderer must render after.
func TestBrainExfiltrationDecoratorWrapsEveryHookFiringRole(t *testing.T) {
	roles := hookFiringRoleAccessors(t)
	if len(roles) < 10 {
		t.Fatalf("found only %d hook-firing role accessors (%v); the source scan is broken", len(roles), roles)
	}
	mine := declaredMethods(t, "brain_exfiltration_roles.go", "BrainExfiltrationDecorator")
	for _, r := range roles {
		if !mine[r] {
			t.Errorf("HookFiringStore wraps %s but BrainExfiltrationDecorator does not: writes through it render no markdown (add it to brain_exfiltration_roles.go)", r)
		}
	}
}

// TestHookGuardStoreWrapsEveryHookFiringRole: every role that writes beads
// and fires hooks upstream is a role a declared guard must see.
func TestHookGuardStoreWrapsEveryHookFiringRole(t *testing.T) {
	mine := declaredMethods(t, "hook_guard_decorator.go", "HookGuardStore")
	for _, r := range hookFiringRoleAccessors(t) {
		if !mine[r] {
			t.Errorf("HookFiringStore wraps %s but HookGuardStore does not: a guard is bypassed by writes through it", r)
		}
	}
}

// ── fakes ──────────────────────────────────────────────────────────

// roleFakeStore is fakeBrainStore with a lifecycle role whose writes land in
// the same issue map, so the decorators' re-reads see them.
type roleFakeStore struct {
	*fakeBrainStore
	creates, updates, closes int
}

func (s *roleFakeStore) IssueLifecycle() (issueops.Lifecycle, error) {
	return &fakeLifecycle{s: s}, nil
}

func (s *roleFakeStore) BatchApplier() (issueops.BatchApplier, error) { return fakeApplier{}, nil }

type fakeLifecycle struct{ s *roleFakeStore }

func (l *fakeLifecycle) Create(_ context.Context, req issueops.CreateRequest) (issueops.CreateResult, error) {
	l.s.creates++
	issue := *req.Issue
	l.s.mu.Lock()
	l.s.issues[issue.ID] = &issue
	l.s.mu.Unlock()
	return issueops.CreateResult{Issue: &issue}, nil
}

func (l *fakeLifecycle) Update(_ context.Context, req issueops.UpdateRequest) (issueops.UpdateResult, error) {
	l.s.updates++
	l.s.mu.Lock()
	issue := l.s.issues[req.IssueID]
	if req.Patch.Title.Set {
		issue.Title = req.Patch.Title.Value
	}
	if req.Patch.IssueType.Set {
		issue.IssueType = types.IssueType(req.Patch.IssueType.Value)
	}
	clone := *issue
	l.s.mu.Unlock()
	return issueops.UpdateResult{Issue: &clone}, nil
}

func (l *fakeLifecycle) Close(_ context.Context, req issueops.CloseRequest) (issueops.CloseResult, error) {
	l.s.closes++
	l.s.mu.Lock()
	issue := l.s.issues[req.IssueID]
	issue.Status = types.StatusClosed
	clone := *issue
	l.s.mu.Unlock()
	return issueops.CloseResult{Issue: &clone}, nil
}

func (l *fakeLifecycle) Reopen(context.Context, issueops.ReopenRequest) (issueops.ReopenResult, error) {
	return issueops.ReopenResult{}, errors.New("not used")
}

type fakeApplier struct{}

func (fakeApplier) ApplyBatch(context.Context, issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
	return issueops.ApplyBatchResult{}, nil
}

// ── exfiltration ───────────────────────────────────────────────────

// TestBrainExfiltrationRendersLifecycleRoleWrites: a create, update and close
// through the lifecycle role each render the bead, as the DoltStorage methods
// do.
func TestBrainExfiltrationRendersLifecycleRoleWrites(t *testing.T) {
	inner := &roleFakeStore{fakeBrainStore: newFakeBrainStore()}
	exf := &recordingExf{}
	d := NewBrainExfiltrationDecorator(inner, exf)
	ctx := context.Background()

	lc, err := d.IssueLifecycle()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lc.Create(ctx, issueops.CreateRequest{Actor: "a", Issue: &types.Issue{ID: "brn-1", Title: "t", IssueType: types.TypeKnowledge}}); err != nil {
		t.Fatal(err)
	}
	if _, err := lc.Update(ctx, issueops.UpdateRequest{Actor: "a", IssueID: "brn-1", Patch: issueops.IssuePatch{Title: issueops.Field[string]{Set: true, Value: "t2"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := lc.Close(ctx, issueops.CloseRequest{Actor: "a", IssueID: "brn-1"}); err != nil {
		t.Fatal(err)
	}
	if inner.creates != 1 || inner.updates != 1 || inner.closes != 1 {
		t.Fatalf("inner lifecycle saw %d/%d/%d create/update/close, want 1 each", inner.creates, inner.updates, inner.closes)
	}
	if got := exf.renderCount(); got != 3 {
		t.Fatalf("renders = %d, want one per landed write (3)", got)
	}
	if last := exf.renders[len(exf.renders)-1]; last.Status != types.StatusClosed || last.Title != "t2" {
		t.Fatalf("last render = %q/%s, want the closed t2", last.Title, last.Status)
	}
}

// TestBrainExfiltrationWithoutExfiltratorHandsBackInnerRoles: passthrough mode
// adds no layer.
func TestBrainExfiltrationWithoutExfiltratorHandsBackInnerRoles(t *testing.T) {
	inner := &roleFakeStore{fakeBrainStore: newFakeBrainStore()}
	d := NewBrainExfiltrationDecorator(inner, nil)
	lc, err := d.IssueLifecycle()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := lc.(*fakeLifecycle); !ok {
		t.Fatalf("lifecycle = %T, want the inner store's own", lc)
	}
}

// ── guards ─────────────────────────────────────────────────────────

func guardedRoles(t *testing.T, inner DoltStorage, results map[string]hooksdef.RunResult, defs ...hooksdef.Definition) (*HookGuardStore, *[]string) {
	t.Helper()
	ran := &[]string{}
	g := NewHookGuardStore(inner, defs, HookGuardOptions{
		Command: "x", Store: "proof", Stderr: os.Stderr,
		Exec: func(_ context.Context, d hooksdef.Definition, _ hooksdef.Payload) hooksdef.RunResult {
			*ran = append(*ran, d.Name)
			return results[d.Name]
		},
	})
	return g, ran
}

func guardDef(name, policy, when string) hooksdef.Definition {
	return hooksdef.Definition{Name: name, Path: "/h/" + name + ".toml", Policy: policy, When: when,
		RefusalExit: 2, Run: "x"}
}

// TestHookGuardStoreRefusesLifecycleRoleWrites: a deny-all guard stops a
// create, an update and a close made through the lifecycle role before the
// inner store sees them.
func TestHookGuardStoreRefusesLifecycleRoleWrites(t *testing.T) {
	inner := &roleFakeStore{fakeBrainStore: newFakeBrainStore()}
	inner.issues["brn-1"] = &types.Issue{ID: "brn-1", Title: "t"}
	g, ran := guardedRoles(t, inner, map[string]hooksdef.RunResult{"deny": {ExitCode: 2, Stderr: "no"}},
		guardDef("deny", hooksdef.PolicyGuard, "*"))
	ctx := context.Background()
	lc, err := g.IssueLifecycle()
	if err != nil {
		t.Fatal(err)
	}
	refused := func(name string, err error) {
		t.Helper()
		var ref *hooksdef.Refusal
		if !errors.As(err, &ref) {
			t.Fatalf("%s: want a guard refusal, got %v", name, err)
		}
	}
	_, err = lc.Create(ctx, issueops.CreateRequest{Actor: "a", Issue: &types.Issue{ID: "brn-2", Title: "new"}})
	refused("Create", err)
	_, err = lc.Update(ctx, issueops.UpdateRequest{Actor: "a", IssueID: "brn-1", Patch: issueops.IssuePatch{Title: issueops.Field[string]{Set: true, Value: "x"}}})
	refused("Update", err)
	_, err = lc.Close(ctx, issueops.CloseRequest{Actor: "a", IssueID: "brn-1"})
	refused("Close", err)
	if inner.creates+inner.updates+inner.closes != 0 {
		t.Fatalf("refused writes reached the store: %d/%d/%d", inner.creates, inner.updates, inner.closes)
	}
	if len(*ran) != 3 {
		t.Fatalf("guard ran %d times, want once per write: %v", len(*ran), *ran)
	}
}

// TestHookGuardStoreLetsApprovedLifecycleRoleWritesThrough: a passing guard
// lets the write land, and an observer then sees it.
func TestHookGuardStoreLetsApprovedLifecycleRoleWritesThrough(t *testing.T) {
	inner := &roleFakeStore{fakeBrainStore: newFakeBrainStore()}
	g, ran := guardedRoles(t, inner, map[string]hooksdef.RunResult{},
		guardDef("ok", hooksdef.PolicyGuard, "create"), guardDef("watch", hooksdef.PolicyObserver, "create"))
	lc, err := g.IssueLifecycle()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lc.Create(context.Background(), issueops.CreateRequest{Actor: "a", Issue: &types.Issue{ID: "brn-3", Title: "fine"}}); err != nil {
		t.Fatal(err)
	}
	if inner.creates != 1 {
		t.Fatalf("approved create reached the store %d times, want 1", inner.creates)
	}
	if strings.Join(*ran, ",") != "ok,watch" {
		t.Fatalf("hooks ran %v, want the guard then the observer", *ran)
	}
}

// TestHookGuardStoreRefusesBatchApplyWhileHooksAreDeclared: the batch applier
// cannot run declared hooks per bead, so it refuses rather than write past
// them — and runs normally when nothing matches.
func TestHookGuardStoreRefusesBatchApplyWhileHooksAreDeclared(t *testing.T) {
	inner := &roleFakeStore{fakeBrainStore: newFakeBrainStore()}
	g, _ := guardedRoles(t, inner, nil, guardDef("watch", hooksdef.PolicyObserver, "close"))
	a, err := g.BatchApplier()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ApplyBatch(context.Background(), issueops.ApplyBatchRequest{Actor: "a"}); err == nil || !strings.Contains(err.Error(), "refusing to write") {
		t.Fatalf("ApplyBatch err = %v, want a refusal naming the declared hook", err)
	}
}
