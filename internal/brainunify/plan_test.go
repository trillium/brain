package brainunify

import (
	"reflect"
	"strings"
	"testing"
)

// realFederation reproduces the shape measured on the live brain federation:
// two stores that both mint ids in the "task" prefix, and a knowledge store
// that holds stale copies of ids belonging to other stores.
func realFederation() []SourceFacts {
	return []SourceFacts{
		{
			Namespace: "brain", Database: "dolt", Registered: true, Reachable: true,
			BeadCount:        2033,
			DeclaredPrefixes: []string{"brain"},
			Prefixes:         map[string]int64{"brain": 1910, "project": 36, "assert": 21, "task": 17},
		},
		{
			Namespace: "projects", Database: "project", Registered: true, Reachable: true,
			BeadCount:        476,
			DeclaredPrefixes: []string{"project"},
			Prefixes:         map[string]int64{"project": 476},
		},
		{
			Namespace: "assertions", Database: "assert", Registered: true, Reachable: true,
			BeadCount: 61,
			Prefixes:  map[string]int64{"assert": 61},
		},
		{
			Namespace: "task", Database: "tasks", Registered: true, Reachable: true,
			BeadCount:        5661,
			DeclaredPrefixes: []string{"task"},
			Prefixes:         map[string]int64{"task": 5661},
		},
		{
			// Unclaimed database, single namespace: still migrated.
			Namespace: "db:task", Database: "task", Registered: false, Reachable: true,
			BeadCount: 21,
			Prefixes:  map[string]int64{"task": 21},
		},
	}
}

func TestBuildPlanBookkeepingOnlyDifferenceIsStillAConflict(t *testing.T) {
	// The three assert-* ids that look divergent on their stored
	// content_hash turn out to differ only in bookkeeping: same title, same
	// description, same status. "Differ in any column" is the test, so they are
	// conflicts too - but the plan says the difference is bookkeeping only.
	row := func(hash, updated string) map[string]string {
		return map[string]string{
			"id": "assert-7lt", "title": "barterboo.com", "description": "same text",
			"status": "closed", "content_hash": hash, "updated_at": updated,
		}
	}
	copies := map[string][]IDCopy{
		"assert-7lt": {
			{Source: "brain", ContentHash: "aaa", UpdatedAt: "2026-06-12 18:35:17", Row: row("aaa", "2026-06-12 18:35:17")},
			{Source: "assertions", ContentHash: "bbb", UpdatedAt: "2026-06-12 18:35:18", Row: row("bbb", "2026-06-12 18:35:18")},
		},
	}
	facts := []SourceFacts{
		{Namespace: "brain", Database: "dolt", Reachable: true, BeadCount: 21, DeclaredPrefixes: []string{"brain"}, Prefixes: map[string]int64{"assert": 21}},
		{Namespace: "assertions", Database: "assert", Reachable: true, BeadCount: 61, DeclaredPrefixes: []string{"assert"}, Prefixes: map[string]int64{"assert": 61}},
	}
	plan := BuildPlan(facts, copies)
	c := plan.Collisions[0]
	if !c.Divergent || c.Resolution != ResolutionConflict {
		t.Errorf("a bookkeeping difference is a difference: divergent=%v resolution=%q", c.Divergent, c.Resolution)
	}
	if c.DataColumnsDiffer {
		t.Errorf("only bookkeeping columns differ, and the plan must say so: %v", c.DifferingColumns)
	}
	if err := plan.Refusal(); err != nil {
		t.Errorf("nothing about a conflict refuses the plan: %v", err)
	}
}

func TestBuildPlanCounts(t *testing.T) {
	copies := map[string][]IDCopy{
		"project-2a6": {
			{Source: "brain", UpdatedAt: "2026-06-13 16:22:20", CreatedAt: "2026-06-13 16:21:58", ContentHash: "aaa"},
			{Source: "projects", UpdatedAt: "2026-06-13 16:22:20", CreatedAt: "2026-06-13 16:21:58", ContentHash: "aaa"},
		},
		"assert-2qo": {
			{Source: "brain", UpdatedAt: "2026-06-12 21:25:03", CreatedAt: "2026-06-12 21:24:22", ContentHash: "bbb"},
			{Source: "assertions", UpdatedAt: "2026-06-12 21:25:03", CreatedAt: "2026-06-12 21:24:22", ContentHash: "bbb"},
		},
	}
	plan := BuildPlan(realFederation(), copies)

	if got := plan.TotalBeads(); got != 2033+476+61+5661+21 {
		t.Errorf("TotalBeads = %d", got)
	}
	if got := plan.DistinctBeads(); got != plan.TotalBeads()-2 {
		t.Errorf("DistinctBeads = %d, want total minus one per duplicated id", got)
	}
	if got := plan.DuplicateCopies(); got != 2 {
		t.Errorf("DuplicateCopies = %d, want 2", got)
	}
	if len(plan.Collisions) != 2 {
		t.Fatalf("got %d collisions, want 2", len(plan.Collisions))
	}
}

func TestBuildPlanOwnerWinsCollision(t *testing.T) {
	copies := map[string][]IDCopy{
		"project-2a6": {
			{Source: "brain", UpdatedAt: "2026-06-13 16:22:20", CreatedAt: "2026-06-13 16:21:58", ContentHash: "aaa"},
			{Source: "projects", UpdatedAt: "2026-06-13 16:22:20", CreatedAt: "2026-06-13 16:21:58", ContentHash: "aaa"},
		},
	}
	plan := BuildPlan(realFederation(), copies)
	c := plan.Collisions[0]
	if c.Winner != "projects" {
		t.Errorf("winner = %q, want projects (the prefix owner)", c.Winner)
	}
	if c.Reason != CollisionReasonOwner {
		t.Errorf("reason = %q, want %q", c.Reason, CollisionReasonOwner)
	}
	if !reflectDeepEqual(c.Losers, []string{"brain"}) {
		t.Errorf("losers = %v, want [brain]", c.Losers)
	}
	if c.Divergent {
		t.Error("identical copies must not be reported as divergent")
	}
}

func TestBuildPlanDivergentCollisionBecomesAConflict(t *testing.T) {
	// The real agent-0bq case: the same id in two databases with different
	// status and updated_at. Nothing is chosen and nothing is lost: each copy is
	// kept under a minted id of its authoring store's prefix, and the id is a
	// conflict.
	copies := map[string][]IDCopy{
		"agent-0bq": {
			{Source: "brain", UpdatedAt: "2026-06-14 17:01:24", CreatedAt: "2026-06-14 17:01:24", ContentHash: "stale",
				Row: map[string]string{"id": "agent-0bq", "status": "open", "title": "Orchestration mode guard", "updated_at": "2026-06-14 17:01:24"}},
			{Source: "robots", UpdatedAt: "2026-07-29 16:29:46", CreatedAt: "2026-06-14 17:01:24", ContentHash: "closed",
				Row: map[string]string{"id": "agent-0bq", "status": "closed", "title": "Orchestration mode guard", "updated_at": "2026-07-29 16:29:46"}},
		},
	}
	facts := []SourceFacts{
		{Namespace: "brain", Database: "dolt", Registered: true, Reachable: true, BeadCount: 162,
			DeclaredPrefixes: []string{"brain"}, Prefixes: map[string]int64{"agent": 162}},
		{Namespace: "robots", Database: "agent", Registered: true, Reachable: true, BeadCount: 1021,
			DeclaredPrefixes: []string{"robots", "agent"}, Prefixes: map[string]int64{"agent": 1021}},
	}
	plan := BuildPlan(facts, copies)
	c := plan.Collisions[0]
	if !c.Divergent || c.Resolution != ResolutionConflict {
		t.Fatalf("differing rows must be a conflict: %+v", c)
	}
	if !c.DataColumnsDiffer || !reflectDeepEqual(c.DifferingColumns, []string{"status", "updated_at"}) {
		t.Errorf("differing columns = %v (data differs: %v), want [status updated_at]", c.DifferingColumns, c.DataColumnsDiffer)
	}
	if c.Winner != "" || len(c.Losers) != 0 {
		t.Errorf("a conflict has no winner and no loser: winner=%q losers=%v", c.Winner, c.Losers)
	}
	if len(c.Copies) != 2 || c.Copies[0].Store != "brain" || c.Copies[1].Store != "robots" {
		t.Fatalf("copies = %+v, want one per store, sorted by store", c.Copies)
	}
	// brain authored one copy, so its copy lives in brain's namespace; robots
	// owns the agent prefix, so its copy keeps it.
	if got := c.Copies[0].ID; !strings.HasPrefix(got, "brain-") || len(got) != len("brain-")+12 {
		t.Errorf("brain's copy = %q, want brain-<12 hex>", got)
	}
	if got := c.Copies[1].ID; !strings.HasPrefix(got, "agent-") || len(got) != len("agent-")+12 {
		t.Errorf("robots' copy = %q, want agent-<12 hex>", got)
	}
	if err := plan.Refusal(); err != nil {
		t.Errorf("a conflict must not stop a build: %v", err)
	}
	if got := plan.DistinctBeads(); got != plan.TotalBeads()+1 {
		t.Errorf("DistinctBeads = %d, want the source rows plus the one conflict bead", got)
	}
	if plan.DuplicateCopies() != 0 || len(plan.Conflicts()) != 1 || plan.MintedCopies() != 2 {
		t.Errorf("duplicate copies %d, conflicts %d, minted %d", plan.DuplicateCopies(), len(plan.Conflicts()), plan.MintedCopies())
	}
	if got := plan.CollisionsFor("brain"); len(got) != 1 {
		t.Errorf("a conflict belongs to every store holding a copy: %v", got)
	}

	// The plan is deterministic: a second plan derives the same ids.
	again := BuildPlan(facts, copies).Collisions[0]
	if !reflect.DeepEqual(again.copyIDs(), c.copyIDs()) {
		t.Errorf("minted ids changed between plans: %v vs %v", again.copyIDs(), c.copyIDs())
	}
}

func TestBuildPlanNewestWinsWhenNoOwner(t *testing.T) {
	// No source declares or matches "fe-", so freshness decides.
	copies := map[string][]IDCopy{
		"fe-1a2b": {
			{Source: "db:feedtack", UpdatedAt: "2026-01-01 00:00:00", CreatedAt: "2026-01-01 00:00:00", ContentHash: "same"},
			{Source: "db:other", UpdatedAt: "2026-05-05 00:00:00", CreatedAt: "2026-01-01 00:00:00", ContentHash: "same"},
		},
	}
	facts := []SourceFacts{
		{Namespace: "db:feedtack", Database: "feedtack", Reachable: true, BeadCount: 1, Prefixes: map[string]int64{"fe": 1}},
		{Namespace: "db:other", Database: "other", Reachable: true, BeadCount: 1, Prefixes: map[string]int64{"fe": 1}},
	}
	plan := BuildPlan(facts, copies)
	c := plan.Collisions[0]
	if c.Winner != "db:other" {
		t.Errorf("winner = %q, want db:other (the more recently updated copy)", c.Winner)
	}
	if c.Reason != CollisionReasonNewest {
		t.Errorf("reason = %q, want %q", c.Reason, CollisionReasonNewest)
	}
}

func TestBuildPlanIdenticalTimestampsUseLexicographicTiebreak(t *testing.T) {
	copies := map[string][]IDCopy{
		"zz-1": {
			{Source: "db:b", UpdatedAt: "2026-01-01 00:00:00", CreatedAt: "2026-01-01 00:00:00", ContentHash: "same"},
			{Source: "db:a", UpdatedAt: "2026-01-01 00:00:00", CreatedAt: "2026-01-01 00:00:00", ContentHash: "same"},
		},
	}
	facts := []SourceFacts{
		{Namespace: "db:a", Database: "a", Reachable: true, BeadCount: 1, Prefixes: map[string]int64{"zz": 1}},
		{Namespace: "db:b", Database: "b", Reachable: true, BeadCount: 1, Prefixes: map[string]int64{"zz": 1}},
	}
	plan := BuildPlan(facts, copies)
	c := plan.Collisions[0]
	if c.Winner != "db:a" {
		t.Errorf("winner = %q, want db:a", c.Winner)
	}
	if c.Reason != CollisionReasonFirst {
		t.Errorf("reason = %q, want %q", c.Reason, CollisionReasonFirst)
	}
	if c.Divergent {
		t.Error("identical copies are not divergent")
	}

	// The same two copies with different hashes are a conflict, whatever their
	// timestamps: no tiebreak is needed because nothing is chosen.
	copies["zz-1"][0].ContentHash = "one"
	copies["zz-1"][1].ContentHash = "two"
	c = BuildPlan(facts, copies).Collisions[0]
	if !c.Divergent || c.Winner != "" || len(c.Copies) != 2 {
		t.Errorf("different hashes must be a conflict with both copies kept: %+v", c)
	}
}

func TestBuildPlanExcludesUnreachableSources(t *testing.T) {
	facts := []SourceFacts{
		{Namespace: "brain", Database: "dolt", Registered: true, Reachable: true, BeadCount: 10,
			Prefixes: map[string]int64{"brain": 10}},
		{Namespace: "beads_global", Database: "beads_global", Registered: false, Reachable: false,
			SkipReason: ExcludeReplica, BeadCount: 2330},
	}
	plan := BuildPlan(facts, nil)
	if len(plan.Sources) != 1 {
		t.Fatalf("participating sources = %d, want 1", len(plan.Sources))
	}
	if plan.TotalBeads() != 10 {
		t.Errorf("TotalBeads = %d, want 10 (the replica must not be counted)", plan.TotalBeads())
	}
	if len(plan.Excluded) != 1 || plan.Excluded[0].Database != "beads_global" {
		t.Errorf("excluded = %v, want beads_global", plan.Excluded)
	}
	if plan.Excluded[0].Beads != 2330 {
		t.Errorf("excluded bead count = %d, want 2330 so the exclusion is visible", plan.Excluded[0].Beads)
	}
}

func TestCollisionsFor(t *testing.T) {
	copies := map[string][]IDCopy{
		"task-04m": {
			{Source: "brain", ContentHash: "a"},
			{Source: "task", ContentHash: "a"},
		},
	}
	plan := BuildPlan(realFederation(), copies)
	got := plan.CollisionsFor("brain")
	if len(got) != 1 {
		t.Fatalf("CollisionsFor(brain) = %d entries, want 1", len(got))
	}
	if _, ok := got["task-04m"]; !ok {
		t.Error("the losing copy's store must see the collision too")
	}
}

func reflectDeepEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
