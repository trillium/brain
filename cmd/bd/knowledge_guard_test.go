package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// The knowledge-kind guard (divergence/0049) is one helper every work verb
// calls, so its verdict is pinned once here: a knowledge bead refuses, a
// `both` bead and every work kind pass, an unknown issue is not a verdict,
// and --force waives it.

func TestRefuseKnowledgeKind(t *testing.T) {
	knowledge := &types.Issue{ID: "brain-k1", IssueType: types.TypeKnowledge}
	tests := []struct {
		name   string
		issue  *types.Issue
		force  bool
		refuse bool
	}{
		{"knowledge refuses", knowledge, false, true},
		{"knowledge passes with --force", knowledge, true, false},
		{"both is work and passes", &types.Issue{ID: "brain-b1", IssueType: types.TypeBoth}, false, false},
		{"isa passes", &types.Issue{ID: "brain-i1", IssueType: types.TypeISA}, false, false},
		{"task passes", &types.Issue{ID: "bd-t1", IssueType: types.TypeTask}, false, false},
		{"untyped passes", &types.Issue{ID: "bd-u1"}, false, false},
		{"unknown issue is not a verdict", nil, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := refuseKnowledgeKind("close", "x-1", tt.issue, tt.force)
			if (err != nil) != tt.refuse {
				t.Fatalf("refuseKnowledgeKind = %v, want refusal=%v", err, tt.refuse)
			}
			if err != nil && !errors.Is(err, errKnowledgeKind) {
				t.Errorf("refusal does not match errKnowledgeKind: %v", err)
			}
		})
	}
}

// The message names the verb, the id, the reason and the escape, so an agent
// reading it knows what was refused and what to do about it.
func TestKnowledgeKindErrorMessage(t *testing.T) {
	err := refuseKnowledgeKind("claim", "brain-k1", &types.Issue{ID: "brain-k1", IssueType: types.TypeKnowledge}, false)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	const want = "cannot claim brain-k1: it is a knowledge bead (issue_type=knowledge), and knowledge is never claimed, closed or put on the blocking graph; pass --force to do it anyway"
	if err.Error() != want {
		t.Errorf("message = %q\nwant      %q", err.Error(), want)
	}
}

// Only a blocks edge is work (ISC-113); every other edge type is a link
// knowledge may carry (ISC-116). Both ends are read; an unresolved endpoint
// fails closed, while an intentional external reference passes.
func TestRefuseKnowledgeBlocksEdge(t *testing.T) {
	issues := map[string]*types.Issue{
		"brain-k1": {ID: "brain-k1", IssueType: types.TypeKnowledge},
		"bd-t1":    {ID: "bd-t1", IssueType: types.TypeTask},
		"bd-t2":    {ID: "bd-t2", IssueType: types.TypeTask},
	}
	lookup := func(id string) (*types.Issue, error) {
		if IsExternalRef(id) {
			return nil, nil
		}
		issue, ok := issues[id]
		if !ok {
			return nil, errors.New("issue store is unavailable")
		}
		return issue, nil
	}

	tests := []struct {
		name  string
		edge  issueops.DependencyEdge
		force bool
		want  string // "" for pass, else a prefix of the refusal
	}{
		{"task blocked by task passes", issueops.DependencyEdge{IssueID: "bd-t1", DependsOnID: "bd-t2", Type: types.DepBlocks}, false, ""},
		{"knowledge as the blocked end refuses", issueops.DependencyEdge{IssueID: "brain-k1", DependsOnID: "bd-t1", Type: types.DepBlocks}, false, "cannot block brain-k1:"},
		{"knowledge as the blocker refuses", issueops.DependencyEdge{IssueID: "bd-t1", DependsOnID: "brain-k1", Type: types.DepBlocks}, false, "cannot block on brain-k1:"},
		{"--force waives the blocks guard", issueops.DependencyEdge{IssueID: "bd-t1", DependsOnID: "brain-k1", Type: types.DepBlocks}, true, ""},
		{"related to knowledge passes", issueops.DependencyEdge{IssueID: "bd-t1", DependsOnID: "brain-k1", Type: types.DepRelated}, false, ""},
		{"parent-child with knowledge passes", issueops.DependencyEdge{IssueID: "brain-k1", DependsOnID: "bd-t1", Type: types.DepParentChild}, false, ""},
		{"unreadable blocker refuses", issueops.DependencyEdge{IssueID: "bd-t1", DependsOnID: "elsewhere-9", Type: types.DepBlocks}, false, "cannot block on elsewhere-9: cannot verify whether it is a knowledge bead:"},
		{"external ref passes", issueops.DependencyEdge{IssueID: "bd-t1", DependsOnID: "external:gh-1", Type: types.DepBlocks}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := refuseKnowledgeBlocksEdge(tt.edge, lookup, tt.force)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("edge refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("edge passed, want refusal starting %q", tt.want)
			}
			if !strings.HasPrefix(err.Error(), tt.want) {
				t.Errorf("refusal = %q, want prefix %q", err.Error(), tt.want)
			}
		})
	}
}

func TestProxiedBlocksRefusesUnreadableEndpoint(t *testing.T) {
	claimRoleFixture(t, &types.Issue{ID: "bd-t1"}, issueops.UpdateResult{}, nil)

	err := refuseKnowledgeBlocksEdgesProxied(context.Background(), []issueops.DependencyEdge{{
		IssueID: "bd-t1", DependsOnID: "foreign-k1", Type: types.DepBlocks,
	}}, false)
	if err == nil || !strings.HasPrefix(err.Error(), "cannot block bd-t1: cannot verify whether it is a knowledge bead:") {
		t.Fatalf("refusal = %v, want unreadable-endpoint refusal", err)
	}
}

// The proxied route reads the guard from the same pre-read its other fences
// use: a knowledge claim is a per-id failure and never reaches the lifecycle.
func TestProxiedClaimRefusesKnowledgeBead(t *testing.T) {
	before := &types.Issue{ID: "brain-k1", Status: types.StatusOpen, IssueType: types.TypeKnowledge}
	p := claimRoleFixture(t, before, issueops.UpdateResult{}, nil)

	got, fail, err := applyUpdateProxiedOne(context.Background(), "brain-k1", &updateInput{claim: true, fields: map[string]any{}})
	if err != nil {
		t.Fatalf("applyUpdateProxiedOne aborted the batch: %v", err)
	}
	if got != nil || fail == nil {
		t.Fatalf("issue = %+v, fail = %+v; want a per-id failure and no issue", got, fail)
	}
	if !strings.HasPrefix(fail.Error, "cannot claim brain-k1:") {
		t.Errorf("failure = %q, want the knowledge refusal", fail.Error)
	}
	if p.lifecycle.request.IssueID != "" {
		t.Errorf("the refused claim reached the lifecycle: %+v", p.lifecycle.request)
	}
}

// --force on the proxied route passes the claim through to the lifecycle.
func TestProxiedClaimForceTakesKnowledgeBead(t *testing.T) {
	before := &types.Issue{ID: "brain-k1", Status: types.StatusOpen, IssueType: types.TypeKnowledge}
	claimed := &types.Issue{ID: "brain-k1", Status: types.StatusInProgress, Assignee: "agent", IssueType: types.TypeKnowledge}
	p := claimRoleFixture(t, before, issueops.UpdateResult{Issue: claimed, Changed: true}, nil)

	oldActor := actor
	actor = "agent"
	t.Cleanup(func() { actor = oldActor })

	got, fail, err := applyUpdateProxiedOne(context.Background(), "brain-k1", &updateInput{claim: true, force: true, fields: map[string]any{}})
	if err != nil || fail != nil {
		t.Fatalf("applyUpdateProxiedOne: err = %v, fail = %+v", err, fail)
	}
	if got == nil || got.Assignee != "agent" {
		t.Fatalf("updated issue = %+v, want the claimed row", got)
	}
	if !p.lifecycle.request.Claim || p.lifecycle.request.IssueID != "brain-k1" {
		t.Errorf("request = %+v, want the forced claim of brain-k1", p.lifecycle.request)
	}
}

func TestProxiedClaimRefusesRequestedKnowledgeKind(t *testing.T) {
	before := &types.Issue{ID: "bd-t1", Status: types.StatusOpen, IssueType: types.TypeTask}
	p := claimRoleFixture(t, before, issueops.UpdateResult{}, nil)

	got, fail, err := applyUpdateProxiedOne(context.Background(), "bd-t1", &updateInput{
		claim:  true,
		fields: map[string]any{"issue_type": string(types.TypeKnowledge)},
	})
	if err != nil || got != nil || fail == nil {
		t.Fatalf("issue = %+v, fail = %+v, err = %v; want knowledge refusal", got, fail, err)
	}
	if !strings.HasPrefix(fail.Error, "cannot claim bd-t1:") {
		t.Errorf("failure = %q, want the knowledge refusal", fail.Error)
	}
	if p.lifecycle.request.IssueID != "" {
		t.Errorf("the refused claim reached the lifecycle: %+v", p.lifecycle.request)
	}
}

// The proxied close reads the same helper inside the same not-yet-closed
// branch as the direct route; the direct branch is exercised end to end in
// knowledge_guard_parity_test.go, and this pins the shared verdict both
// routes spell.
func TestKnowledgeCloseRefusalSpelling(t *testing.T) {
	issue := &types.Issue{ID: "brain-k1", Status: types.StatusOpen, IssueType: types.TypeKnowledge}
	direct := closeDirectCheckOne("brain-k1", issue, false)
	helper := refuseKnowledgeKind("close", "brain-k1", issue, false)
	if helper == nil || direct != helper.Error() {
		t.Errorf("direct close refusal = %q, helper = %v; want the same sentence", direct, helper)
	}
	if forced := closeDirectCheckOne("brain-k1", issue, true); forced != "" {
		t.Errorf("--force close refused: %q", forced)
	}
	closed := &types.Issue{ID: "brain-k2", Status: types.StatusClosed, IssueType: types.TypeKnowledge}
	if again := closeDirectCheckOne("brain-k2", closed, false); again != "" {
		t.Errorf("re-close of an already closed knowledge bead refused: %q", again)
	}
}
