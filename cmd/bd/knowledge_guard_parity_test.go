//go:build cgo

package main

import (
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// The knowledge-kind guard (divergence/0049) on the direct route, end to end
// through the real RunE functions against a real store: close, claim and a
// blocks edge refuse a knowledge bead before any write, --force lets each
// through, and a `both` bead is work and takes every verb.

func seedKnowledge(env *parityEnv, id, title string) *types.Issue {
	return env.seed(id, title, func(i *types.Issue) { i.IssueType = types.TypeKnowledge })
}

func TestParityCloseRefusesKnowledgeBead(t *testing.T) {
	env := newParityEnv(t)
	seedKnowledge(env, "test-kg1", "A fact")

	env.setFlags(closeCmd, nil)
	res := env.run(closeCmd, "test-kg1")
	if res.exitCode != 1 {
		t.Fatalf("exit = %d, want 1; stdout=%q stderr=%q", res.exitCode, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stderr, "cannot close test-kg1: it is a knowledge bead") {
		t.Errorf("stderr = %q, want the knowledge refusal", res.stderr)
	}
	if got := env.store.mutations(); len(got) != 0 {
		t.Errorf("store mutations = %v, want none (refused pre-write)", got)
	}
	if got := env.get("test-kg1"); got.Status != types.StatusOpen {
		t.Errorf("status = %s, want open", got.Status)
	}
}

func TestParityCloseForceClosesKnowledgeBead(t *testing.T) {
	env := newParityEnv(t)
	seedKnowledge(env, "test-kg2", "A fact that was wrong")

	env.setFlags(closeCmd, map[string]string{"force": "true", "reason": "superseded"})
	res := env.run(closeCmd, "test-kg2")
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", res.exitCode, res.stderr)
	}
	if got := env.get("test-kg2"); got.Status != types.StatusClosed {
		t.Errorf("status = %s, want closed under --force", got.Status)
	}
}

func TestParityCloseTakesBothKind(t *testing.T) {
	env := newParityEnv(t)
	env.seed("test-kg3", "Work that is also a fact", func(i *types.Issue) { i.IssueType = types.TypeBoth })

	env.setFlags(closeCmd, nil)
	res := env.run(closeCmd, "test-kg3")
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", res.exitCode, res.stderr)
	}
	if got := env.get("test-kg3"); got.Status != types.StatusClosed {
		t.Errorf("status = %s, want closed", got.Status)
	}
}

func TestParityClaimRefusesKnowledgeBead(t *testing.T) {
	env := newParityEnv(t)
	seedKnowledge(env, "test-kg4", "A fact")

	env.setFlags(updateCmd, map[string]string{"claim": "true"})
	res := env.run(updateCmd, "test-kg4")
	if res.exitCode != 1 {
		t.Fatalf("exit = %d, want 1 (a policy refusal, not a guard mismatch); stderr=%q", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stderr, "cannot claim test-kg4: it is a knowledge bead") {
		t.Errorf("stderr = %q, want the knowledge refusal", res.stderr)
	}
	if got := env.store.mutations(); len(got) != 0 {
		t.Errorf("store mutations = %v, want none (refused pre-write)", got)
	}
	got := env.get("test-kg4")
	if got.Status != types.StatusOpen || got.Assignee != "" {
		t.Errorf("issue = status %s assignee %q, want open and unassigned", got.Status, got.Assignee)
	}
}

func TestParityClaimForceTakesKnowledgeBead(t *testing.T) {
	env := newParityEnv(t)
	seedKnowledge(env, "test-kg5", "A fact filed as knowledge by mistake")

	env.setFlags(updateCmd, map[string]string{"claim": "true", "force": "true"})
	res := env.run(updateCmd, "test-kg5")
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", res.exitCode, res.stderr)
	}
	got := env.get("test-kg5")
	if got.Status != types.StatusInProgress || got.Assignee != actor {
		t.Errorf("issue = status %s assignee %q, want in_progress by %s", got.Status, got.Assignee, actor)
	}
}

func TestParityClaimRefusesRequestedKnowledgeKind(t *testing.T) {
	env := newParityEnv(t)
	env.seed("test-kg5a", "Work becoming a fact", nil)

	env.setFlags(updateCmd, map[string]string{"claim": "true", "type": "knowledge"})
	res := env.run(updateCmd, "test-kg5a")
	if res.exitCode != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stderr, "cannot claim test-kg5a: it is a knowledge bead") {
		t.Errorf("stderr = %q, want the knowledge refusal", res.stderr)
	}
	got := env.get("test-kg5a")
	if got.IssueType != types.TypeTask || got.Status != types.StatusOpen || got.Assignee != "" {
		t.Errorf("issue = %+v, want unchanged task", got)
	}
}

// Plain field edits on a knowledge bead are not work verbs and pass without
// --force: the guard reads only --claim.
func TestParityUpdateWithoutClaimEditsKnowledgeBead(t *testing.T) {
	env := newParityEnv(t)
	seedKnowledge(env, "test-kg6", "A fact")

	env.setFlags(updateCmd, map[string]string{"title": "A sharper fact"})
	res := env.run(updateCmd, "test-kg6")
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", res.exitCode, res.stderr)
	}
	if got := env.get("test-kg6"); got.Title != "A sharper fact" {
		t.Errorf("title = %q, want the edit applied", got.Title)
	}
}

func depIDs(t *testing.T, env *parityEnv, id string) []string {
	t.Helper()
	deps, err := env.store.inner.GetDependencies(rootCtx, id)
	if err != nil {
		t.Fatalf("GetDependencies(%s): %v", id, err)
	}
	ids := make([]string, 0, len(deps))
	for _, d := range deps {
		ids = append(ids, d.ID)
	}
	return ids
}

func TestParityDepAddRefusesBlocksOnKnowledgeBead(t *testing.T) {
	env := newParityEnv(t)
	seedKnowledge(env, "test-kg7", "A fact")
	env.seed("test-kg8", "A task", nil)

	// The task would be blocked by the fact.
	env.setFlags(depAddCmd, nil)
	res := env.run(depAddCmd, "test-kg8", "test-kg7")
	if res.exitCode != 1 {
		t.Fatalf("exit = %d, want 1; stdout=%q stderr=%q", res.exitCode, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stderr, "cannot block on test-kg7: it is a knowledge bead") {
		t.Errorf("stderr = %q, want the knowledge refusal", res.stderr)
	}
	if got := depIDs(t, env, "test-kg8"); len(got) != 0 {
		t.Errorf("dependencies of test-kg8 = %v, want none", got)
	}

	// The fact would be blocked by the task.
	env.setFlags(depAddCmd, nil)
	res = env.run(depAddCmd, "test-kg7", "test-kg8")
	if res.exitCode != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stderr, "cannot block test-kg7: it is a knowledge bead") {
		t.Errorf("stderr = %q, want the knowledge refusal", res.stderr)
	}
	if got := depIDs(t, env, "test-kg7"); len(got) != 0 {
		t.Errorf("dependencies of test-kg7 = %v, want none", got)
	}
}

func TestParityDepAddForceAndOtherTypesReachKnowledgeBead(t *testing.T) {
	env := newParityEnv(t)
	seedKnowledge(env, "test-kg9", "A fact")
	env.seed("test-kg10", "A task", nil)
	env.seed("test-kg11", "Another task", nil)

	// A related edge is a link knowledge may carry (ISC-116): no --force needed.
	env.setFlags(depAddCmd, map[string]string{"type": "related"})
	res := env.run(depAddCmd, "test-kg10", "test-kg9")
	if res.exitCode != 0 {
		t.Fatalf("related edge: exit = %d, want 0; stderr=%q", res.exitCode, res.stderr)
	}
	if got := depIDs(t, env, "test-kg10"); len(got) != 1 || got[0] != "test-kg9" {
		t.Errorf("dependencies of test-kg10 = %v, want [test-kg9]", got)
	}

	// --force lets a blocks edge through.
	env.setFlags(depAddCmd, map[string]string{"force": "true"})
	res = env.run(depAddCmd, "test-kg11", "test-kg9")
	if res.exitCode != 0 {
		t.Fatalf("forced blocks edge: exit = %d, want 0; stderr=%q", res.exitCode, res.stderr)
	}
	if got := depIDs(t, env, "test-kg11"); len(got) != 1 || got[0] != "test-kg9" {
		t.Errorf("dependencies of test-kg11 = %v, want [test-kg9]", got)
	}
}

// `bd dep --blocks` is the other direct entry for a blocks edge and reads the
// same guard.
func TestParityDepBlocksRefusesKnowledgeBead(t *testing.T) {
	env := newParityEnv(t)
	seedKnowledge(env, "test-kg12", "A fact")
	env.seed("test-kg13", "A task", nil)

	// "the fact blocks the task": the fact is the blocker.
	env.setFlags(depCmd, map[string]string{"blocks": "test-kg13"})
	res := env.run(depCmd, "test-kg12")
	if res.exitCode != 1 {
		t.Fatalf("exit = %d, want 1; stdout=%q stderr=%q", res.exitCode, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stderr, "cannot block on test-kg12: it is a knowledge bead") {
		t.Errorf("stderr = %q, want the knowledge refusal", res.stderr)
	}
	if got := depIDs(t, env, "test-kg13"); len(got) != 0 {
		t.Errorf("dependencies of test-kg13 = %v, want none", got)
	}

	env.setFlags(depCmd, map[string]string{"blocks": "test-kg13", "force": "true"})
	res = env.run(depCmd, "test-kg12")
	if res.exitCode != 0 {
		t.Fatalf("forced: exit = %d, want 0; stderr=%q", res.exitCode, res.stderr)
	}
	if got := depIDs(t, env, "test-kg13"); len(got) != 1 || got[0] != "test-kg12" {
		t.Errorf("dependencies of test-kg13 = %v, want [test-kg12]", got)
	}
}
