package storage_test

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"testing"

	"github.com/steveyegge/beads/internal/hooksdef"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// writeFake records which of the claim/delete/rename writes reached the store.
type writeFake struct {
	*guardFakeStore
	reached []string
}

func (w *writeFake) ClaimIssue(_ context.Context, id, _ string) error {
	w.reached = append(w.reached, "claim:"+id)
	return nil
}
func (w *writeFake) ClaimReadyIssue(_ context.Context, _ types.WorkFilter, _ string) (*types.Issue, error) {
	w.reached = append(w.reached, "claim-ready")
	return &types.Issue{ID: "fk-ready"}, nil
}
func (w *writeFake) DeleteIssues(_ context.Context, ids []string, _, _, dryRun bool) (*types.DeleteIssuesResult, error) {
	if !dryRun {
		w.reached = append(w.reached, "delete")
	}
	return &types.DeleteIssuesResult{DeletedCount: len(ids)}, nil
}
func (w *writeFake) UpdateIssueID(_ context.Context, oldID, newID string, _ *types.Issue, _ string) error {
	w.reached = append(w.reached, "rename:"+oldID+">"+newID)
	return nil
}

// A deny-all guard must stop every write path, including the ones that do not
// go through CreateIssue/UpdateIssue/CloseIssue.
func TestDenyAllGuardStopsClaimBatchDeleteAndRename(t *testing.T) {
	f := &writeFake{guardFakeStore: newGuardFake()}
	f.issues["fk-a"] = &types.Issue{ID: "fk-a", Title: "a"}
	f.issues["fk-b"] = &types.Issue{ID: "fk-b", Title: "b"}
	s := &scripted{results: map[string]hooksdef.RunResult{"deny": {ExitCode: 2, Stderr: "no"}}}
	errBuf := new(bytes.Buffer)
	g := storage.NewHookGuardStore(f, []hooksdef.Definition{def("deny", hooksdef.PolicyGuard, "*")},
		storage.HookGuardOptions{Command: "x", Store: "proof", Exec: s.exec, Stderr: errBuf})
	ctx := context.Background()

	refused := func(name string, err error) {
		t.Helper()
		var ref *hooksdef.Refusal
		if !errors.As(err, &ref) {
			t.Fatalf("%s: want a guard refusal, got %v", name, err)
		}
	}
	refused("ClaimIssue", g.ClaimIssue(ctx, "fk-a", "me"))
	_, err := g.ClaimReadyIssue(ctx, types.WorkFilter{}, "me")
	refused("ClaimReadyIssue", err)
	_, err = g.DeleteIssues(ctx, []string{"fk-a", "fk-b"}, true, true, false)
	refused("DeleteIssues", err)
	refused("UpdateIssueID", g.UpdateIssueID(ctx, "fk-a", "fk-z", &types.Issue{ID: "fk-z"}, "me"))
	if len(f.reached) != 0 {
		t.Fatalf("refused writes reached the store: %v", f.reached)
	}

	// A dry run changes nothing, so it is not a write to guard.
	if _, err := g.DeleteIssues(ctx, []string{"fk-a"}, false, false, true); err != nil {
		t.Fatalf("dry-run delete must not be guarded: %v", err)
	}
}

func TestPassingGuardLetsClaimBatchDeleteAndRenameThrough(t *testing.T) {
	f := &writeFake{guardFakeStore: newGuardFake()}
	s := &scripted{results: map[string]hooksdef.RunResult{}}
	g := storage.NewHookGuardStore(f, []hooksdef.Definition{def("ok", hooksdef.PolicyGuard, "*")},
		storage.HookGuardOptions{Command: "x", Store: "proof", Exec: s.exec, Stderr: new(bytes.Buffer)})
	ctx := context.Background()
	if err := g.ClaimIssue(ctx, "fk-a", "me"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.ClaimReadyIssue(ctx, types.WorkFilter{}, "me"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.DeleteIssues(ctx, []string{"fk-a", "fk-b"}, false, true, false); err != nil {
		t.Fatal(err)
	}
	if err := g.UpdateIssueID(ctx, "fk-a", "fk-z", &types.Issue{ID: "fk-z"}, "me"); err != nil {
		t.Fatal(err)
	}
	if len(f.reached) != 4 {
		t.Fatalf("every approved write should reach the store, got %v", f.reached)
	}
	// Two deletes + claim + claim-ready + rename each ran the guard.
	if len(s.ran) != 5 {
		t.Fatalf("guard should run once per ClaimIssue/ClaimReadyIssue/delete id/rename, ran %d: %v", len(s.ran), s.ran)
	}
}

// guardAllowlist names every DoltStorage method HookGuardStore deliberately
// passes straight through. A method that is on neither this list nor wrapped
// fails TestEveryStorageMethodIsGuardedOrAllowlisted, so a new mutating
// method cannot silently skip declared guards.
var guardAllowlist = map[string]string{}

func init() {
	add := func(why string, names ...string) {
		for _, n := range names {
			guardAllowlist[n] = why
		}
	}
	add("read-only",
		"AsOf", "CheckEligibility", "CommitExists", "CountDependencies", "CountDependents", "CountDependentsByStatus",
		"CountEvents", "CountIssueComments", "CountIssues", "CountIssuesByGroup", "CurrentBranch", "DetectCycles", "Diff",
		"FindWispDependentsRecursive", "GetAllConfig", "GetAllDependencyRecords", "GetAllEventsSince", "GetBlockedIssues",
		"GetBlockingInfoForIssues", "GetCommentCounts", "GetCommentsForIssues", "GetCompactionSnapshot", "GetConfig",
		"GetConflicts", "GetCurrentCommit", "GetCustomStatuses", "GetCustomStatusesDetailed", "GetCustomTypes",
		"GetDependencies", "GetDependenciesWithMetadata", "GetDependencyCounts", "GetDependencyRecords",
		"GetDependencyRecordsForIssues", "GetDependencyTree", "GetDependents", "GetDependentsWithMetadata",
		"GetEpicsEligibleForClosure", "GetEvents", "GetFederationPeer", "GetInfraTypes", "GetIssue", "GetIssueByExternalRef",
		"GetIssueComments", "GetIssuesByIDs", "GetIssuesByLabel", "GetLabels", "GetLabelsForIssues", "GetLocalMetadata",
		"GetMetadata", "GetMoleculeLastActivity", "GetMoleculeProgress", "GetNewlyUnblockedByClose", "GetNextChildID",
		"GetReadyWork", "GetReadyWorkWithCounts", "GetRepoMtime", "GetStaleIssues", "GetStatistics", "GetTier1Candidates",
		"GetTier2Candidates", "HasRemote", "History", "IsBlocked", "IsInfraTypeCtx", "IterAllDependencyRecords",
		"IterAllEventsSince", "IterBlockedIssues", "IterDependenciesWithMetadata", "IterDependentsWithMetadata",
		"IterEvents", "IterIssueComments", "IterIssues", "IterReadyWork", "IterWisps", "ListBranches",
		"ListFederationPeers", "ListRemotes", "ListWisps", "Log", "MergeSlotCheck", "SearchIssues",
		"SearchIssuesWithCounts", "SlotGet", "SnapshotIssue", "Status", "SyncStatus")
	add("version control and remote administration; not bead mutations",
		"AddFederationPeer", "AddRemote", "Branch", "Checkout", "Close", "Commit", "CommitMergeResolution",
		"CommitPending", "CommitWithConfig", "DeleteBranch", "Fetch", "ForcePush", "Merge", "Pull", "PullFrom",
		"PullRemote", "Push", "PushRemote", "PushTo", "RemoveFederationPeer", "RemoveRemote", "ResolveConflicts",
		"Sync")
	add("store configuration and bookkeeping; not bead content",
		"ClearRepoMtime", "DeleteConfig", "SetConfig", "SetLocalMetadata", "SetMetadata", "SetRepoMtime")
	add("KNOWN GAP: mutates beads but is not guarded yet (see docs/brain/HOOKS.md 'not covered')",
		"AddComment", "ApplyCompaction", "DeleteIssuesBySourceRepo", "ImportIssueComment", "MergeSlotAcquire",
		"MergeSlotCreate", "MergeSlotRelease", "PromoteFromEphemeral", "RestoreFromSnapshot", "SlotClear", "SlotSet")
}

// guardedMethods returns the method names HookGuardStore declares itself,
// read from the decorator's source: a promoted method has no declaration.
func guardedMethods(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "hook_guard_decorator.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		if star, ok := fn.Recv.List[0].Type.(*ast.StarExpr); ok {
			if id, ok := star.X.(*ast.Ident); ok && id.Name == "HookGuardStore" {
				out[fn.Name.Name] = true
			}
		}
	}
	return out
}

func TestEveryStorageMethodIsGuardedOrAllowlisted(t *testing.T) {
	wrapped := guardedMethods(t)
	ty := reflect.TypeOf((*storage.DoltStorage)(nil)).Elem()
	var unreviewed, stale []string
	seen := map[string]bool{}
	for i := 0; i < ty.NumMethod(); i++ {
		name := ty.Method(i).Name
		seen[name] = true
		_, allowed := guardAllowlist[name]
		switch {
		case !wrapped[name] && !allowed:
			unreviewed = append(unreviewed, name)
		case wrapped[name] && allowed:
			stale = append(stale, name)
		}
	}
	for name := range guardAllowlist {
		if !seen[name] {
			stale = append(stale, name+" (no longer a DoltStorage method)")
		}
	}
	sort.Strings(unreviewed)
	sort.Strings(stale)
	if len(unreviewed) > 0 {
		t.Errorf("DoltStorage methods that HookGuardStore neither wraps nor allowlists (a mutating method here bypasses declared guards; wrap it in hook_guard_decorator.go or list it in guardAllowlist): %v", unreviewed)
	}
	if len(stale) > 0 {
		t.Errorf("guardAllowlist entries that are wrapped now or no longer exist; remove them: %v", stale)
	}
	for _, must := range []string{"ClaimIssue", "ClaimReadyIssue", "DeleteIssues", "UpdateIssueID"} {
		if !wrapped[must] {
			t.Errorf("%s must be wrapped by HookGuardStore", must)
		}
	}
}
