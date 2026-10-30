//go:build cgo

package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// TestAgentFiledBeadKeepsHumanClaimIdentity runs an agent-filed `bd todo add`
// against a real store: created_by names the agent, while the claim identity
// (assignee, claim actor) stays the human's git identity, so a claim the human
// identity already holds is still re-claimable idempotently (robots-0x8).
func TestAgentFiledBeadKeepsHumanClaimIdentity(t *testing.T) {
	ensureCleanGlobalState(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	humanGitIdentity(t, home, "Human Owner")
	t.Setenv("BEADS_ACTOR", "")
	t.Setenv("BD_ACTOR", "")
	t.Setenv(agentNameEnvVar, "Volt")
	t.Setenv(agentIDEnvVar, "volt")

	s := newTestStore(t, filepath.Join(t.TempDir(), ".beads", "beads.db"))
	ctx := context.Background()

	origStore, origActor, origCtx, origJSON := store, actor, rootCtx, jsonOutput
	t.Cleanup(func() {
		store, actor, rootCtx, jsonOutput = origStore, origActor, origCtx, origJSON
	})
	setStore(s)
	rootCtx = ctx
	jsonOutput = false
	actor = ""
	actor = getActorWithGit()

	// A claim taken under the human identity before agent stamping existed.
	prior := &types.Issue{Title: "claimed before", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	if err := s.CreateIssue(ctx, prior, "Human Owner"); err != nil {
		t.Fatalf("create prior: %v", err)
	}
	if err := s.ClaimIssue(ctx, prior.ID, "Human Owner"); err != nil {
		t.Fatalf("claim prior: %v", err)
	}

	captureStdout(t, func() error { return addTodoCmd.RunE(addTodoCmd, []string{"filed by agent"}) })

	issues, err := s.SearchIssues(ctx, "filed by agent", types.IssueFilter{})
	if err != nil || len(issues) != 1 {
		t.Fatalf("SearchIssues = %d issues, err %v; want 1", len(issues), err)
	}
	filed := issues[0]
	if filed.CreatedBy != "Volt" {
		t.Errorf("created_by = %q, want agent %q", filed.CreatedBy, "Volt")
	}
	if filed.Assignee != "Human Owner" {
		t.Errorf("assignee = %q, want human %q", filed.Assignee, "Human Owner")
	}

	if err := s.ClaimIssue(ctx, filed.ID, getActorWithGit()); err != nil {
		t.Fatalf("claim filed: %v", err)
	}
	if err := s.ClaimIssue(ctx, prior.ID, getActorWithGit()); err != nil {
		t.Errorf("re-claim of the human's existing claim = %v, want idempotent success", err)
	}
	got, err := s.GetIssue(ctx, prior.ID)
	if err != nil {
		t.Fatalf("get prior: %v", err)
	}
	if got.Assignee != "Human Owner" || got.Status != types.StatusInProgress {
		t.Errorf("prior claim = %q/%s, want %q/in_progress", got.Assignee, got.Status, "Human Owner")
	}
}
