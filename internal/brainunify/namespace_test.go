package brainunify

import (
	"reflect"
	"testing"
)

func TestPrefixOf(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"brain-se7t", "brain"},
		// A hierarchical child id stays in its parent's namespace. Getting
		// this wrong would scatter child beads into a namespace of their own.
		{"brain-se7t.389", "brain"},
		{"brain-se7t.389.2", "brain"},
		{"agent-identity-29u", "agent"},
		{"nightshift-4kx2", "nightshift"},
		{"external_llm_tasks-9", "external_llm_tasks"},
		// Underscores are part of a prefix; only '-' separates.
		{"resume_bullets-2a1", "resume_bullets"},
		// No separator: the whole id is its own prefix rather than empty.
		{"TK1", "TK1"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := PrefixOf(tc.id); got != tc.want {
			t.Errorf("PrefixOf(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestBuildNamespacesDeclaredWins(t *testing.T) {
	// The real shape: the brain store (database "dolt") holds brain- beads
	// and also stale copies of project- beads that belong to the project
	// store. The project store declares the prefix, so it owns it.
	observed := map[string][]string{
		"brain":    {"brain", "project"},
		"projects": {"project"},
	}
	declared := map[string][]string{
		"brain":    {"brain"},
		"projects": {"project"},
	}
	counts := map[string]int64{"brain": 1910, "project": 36 + 476}

	got := BuildNamespaces(observed, declared, counts, nil)
	if got["project"].Owner != "projects" {
		t.Errorf("project prefix owner = %q, want %q", got["project"].Owner, "projects")
	}
	if got["project"].OwnerReason != ReasonDeclared {
		t.Errorf("project owner reason = %q, want %q", got["project"].OwnerReason, ReasonDeclared)
	}
	if !got["project"].Ambiguous {
		t.Error("project prefix should be flagged ambiguous: two databases contribute to it")
	}
	if got["brain"].Owner != "brain" {
		t.Errorf("brain prefix owner = %q, want brain", got["brain"].Owner)
	}
	if got["brain"].Ambiguous {
		t.Error("brain prefix is only in one source and should not be ambiguous")
	}
}

func TestBuildNamespacesNameMatch(t *testing.T) {
	// The assertions store declares no prefix at all, but its database is
	// named "assert" and its beads are "assert-*". Name equality is what
	// resolves ownership there.
	observed := map[string][]string{
		"assertions": {"assert"},
		"db:dolt":    {"assert", "brain"},
	}
	// The assertions store's unified namespace is "assertions" but its Dolt
	// database — and its ids — are "assert".
	names := map[string][]string{"assertions": {"assert"}}
	got := BuildNamespaces(observed, nil, map[string]int64{"assert": 61, "brain": 10}, names)
	if got["assert"].Owner != "assertions" {
		t.Errorf("assert prefix owner = %q, want assertions", got["assert"].Owner)
	}
	if got["assert"].OwnerReason != ReasonNameMatch {
		t.Errorf("assert owner reason = %q, want %q", got["assert"].OwnerReason, ReasonNameMatch)
	}
}

func TestBuildNamespacesSoleObserverOfAnUnregisteredDatabaseOwnsItsPrefix(t *testing.T) {
	// The "fe-" prefix of the feedtack database, and "commitment-" of the
	// commitments one: no store declares them and the database name does not
	// equal the prefix, but exactly one unregistered database holds them. A
	// wrapper pinned to that database must still see its beads, so it owns them.
	got := BuildNamespaces(
		map[string][]string{"db:feedtack": {"fe"}, "db:commitments": {"commitment"}},
		nil,
		map[string]int64{"fe": 24, "commitment": 14},
		map[string][]string{"db:commitments": {"commitments"}},
	)
	for prefix, owner := range map[string]string{"fe": "db:feedtack", "commitment": "db:commitments"} {
		if got[prefix].Owner != owner || got[prefix].OwnerReason != ReasonSoleObserver {
			t.Errorf("%s: owner %q reason %q, want %q / %q", prefix, got[prefix].Owner, got[prefix].OwnerReason, owner, ReasonSoleObserver)
		}
	}
}

func TestBuildNamespacesUnattributed(t *testing.T) {
	// A prefix no store claims, no source is named after, and more than one
	// source holds stays with no owner; so does one held only by a registered
	// store that does not declare it.
	got := BuildNamespaces(
		map[string][]string{"db:one": {"zz"}, "db:two": {"zz"}, "projects": {"yy"}},
		nil,
		map[string]int64{"zz": 3, "yy": 1},
		nil,
	)
	for _, prefix := range []string{"zz", "yy"} {
		if got[prefix].Owner != UnattributedNamespace || got[prefix].OwnerReason != ReasonUnattributed {
			t.Errorf("%s: owner %q reason %q, want unattributed", prefix, got[prefix].Owner, got[prefix].OwnerReason)
		}
	}
}

func TestBuildNamespacesDeclaredButEmptyStore(t *testing.T) {
	// A store with no beads still owns its namespace: that is the namespace
	// it will mint the next id in.
	got := BuildNamespaces(
		map[string][]string{},
		map[string][]string{"chores": {"chores"}},
		map[string]int64{},
		nil,
	)
	if got["chores"].Owner != "chores" {
		t.Errorf("chores owner = %q, want chores", got["chores"].Owner)
	}
	if got["chores"].BeadCount != 0 {
		t.Errorf("chores bead count = %d, want 0", got["chores"].BeadCount)
	}
}

func TestBuildNamespacesDeterministicAcrossDeclaringStores(t *testing.T) {
	// Two stores declare the same prefix. The winner must not depend on map
	// iteration order, so run the same input repeatedly.
	observed := map[string][]string{"a": {"dup-"}, "b": {"dup-"}}
	declared := map[string][]string{"a": {"dup"}, "b": {"dup"}}
	want := BuildNamespaces(observed, declared, nil, nil)["dup"].Owner
	for i := 0; i < 50; i++ {
		if got := BuildNamespaces(observed, declared, nil, nil)["dup"].Owner; got != want {
			t.Fatalf("owner changed between runs: %q then %q", want, got)
		}
	}
	if want != "a" {
		t.Errorf("owner = %q, want the lexicographically smallest declaring store", want)
	}
}

func TestSortedNamespaces(t *testing.T) {
	got := SortedNamespaces(map[string]Namespace{
		"task":   {Prefix: "task"},
		"brain":  {Prefix: "brain"},
		"assert": {Prefix: "assert"},
	})
	var prefixes []string
	for _, ns := range got {
		prefixes = append(prefixes, ns.Prefix)
	}
	if !reflect.DeepEqual(prefixes, []string{"assert", "brain", "task"}) {
		t.Errorf("prefixes not sorted: %v", prefixes)
	}
}
