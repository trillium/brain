package brainunify

import (
	"strings"
	"testing"
)

func meta(cols, pk []string) (func(string) ([]string, []string, error), error) {
	return func(string) ([]string, []string, error) { return cols, pk, nil }, nil
}

func TestClassify(t *testing.T) {
	cases := []struct {
		table string
		cols  []string
		pk    []string
		want  Scope
		scope string
	}{
		{"issues", []string{"id", "title"}, []string{"id"}, ScopeIssues, "id"},
		{"labels", []string{"issue_id", "label"}, []string{"issue_id", "label"}, ScopeIssueChild, "issue_id"},
		{"comments", []string{"id", "issue_id", "text"}, []string{"id"}, ScopeIssueChild, "issue_id"},
		{"dependencies", []string{"id", "issue_id", "depends_on_issue_id"}, []string{"id"}, ScopeIssueChild, "issue_id"},
		// config has no bead reference: it describes the database, not a
		// bead, so several stores cannot share it unchanged.
		{"config", []string{"key", "value"}, []string{"key"}, ScopeDatabaseState, ""},
		{"local_metadata", []string{"key", "value"}, []string{"key"}, ScopeDatabaseState, ""},
		// child_counters references its parent through parent_id, not
		// issue_id. Classifying it as database state would move its rows into
		// a per-store table and make them unaddressable by namespace.
		{"child_counters", []string{"parent_id", "last_child"}, []string{"parent_id"}, ScopeIssueChild, "parent_id"},
		{"wisp_child_counters", []string{"parent_id", "last_child"}, []string{"parent_id"}, ScopeIssueChild, "parent_id"},
		// A wisp has no parent column at all: its own id is a hierarchical
		// bead id, so it is addressable by that id.
		{"wisps", []string{"id", "title", "description"}, []string{"id"}, ScopeIssueChild, "id"},
	}
	for _, tc := range cases {
		got, scope := Classify(tc.table, tc.cols, tc.pk)
		if got != tc.want || scope != tc.scope {
			t.Errorf("Classify(%s) = (%v, %q), want (%v, %q)", tc.table, got, scope, tc.want, tc.scope)
		}
	}
}

func TestPlanTablesTargets(t *testing.T) {
	metaOf, _ := meta(nil, nil)
	metaOf = func(t string) ([]string, []string, error) {
		switch t {
		case "issues":
			return []string{"id", "title"}, []string{"id"}, nil
		case "comments":
			return []string{"id", "issue_id", "text"}, []string{"id"}, nil
		case "config":
			return []string{"key", "value"}, []string{"key"}, nil
		case "brain_unified_config":
			return []string{"store", "key", "value"}, []string{"store", "key"}, nil
		case "brain_store_prefixes":
			return []string{"prefix", "store"}, []string{"prefix"}, nil
		}
		return nil, nil, nil
	}
	plans, err := PlanTables(
		[]string{"issues", "comments", "config", "brain_unified_config", "brain_store_prefixes"},
		metaOf)
	if err != nil {
		t.Fatalf("PlanTables: %v", err)
	}
	byName := map[string]TablePlan{}
	for _, p := range plans {
		byName[p.Table] = p
	}
	if len(byName) != 3 {
		t.Fatalf("planned %d tables, want 3 (the builder's own tables must be skipped)", len(byName))
	}
	if byName["config"].Target != "brain_unified_config" {
		t.Errorf("config target = %q, want brain_unified_config", byName["config"].Target)
	}
	if byName["comments"].Target != "comments" {
		t.Errorf("comments target = %q, want comments", byName["comments"].Target)
	}
	if got := byName["comments"].ScopeColumnName(); got != "issue_id" {
		t.Errorf("comments scope column = %q, want issue_id", got)
	}
	if got := byName["issues"].ScopeColumnName(); got != "id" {
		t.Errorf("issues scope column = %q, want id", got)
	}
}

func TestPlanTablesRejectsUnkeyableDatabaseState(t *testing.T) {
	// A database-state table with no primary key cannot be re-keyed by store,
	// and silently dropping it is not an option.
	metaOf := func(string) ([]string, []string, error) {
		return []string{"a", "b"}, nil, nil
	}
	if _, err := PlanTables([]string{"mystery"}, metaOf); err == nil {
		t.Fatal("expected an error for a database-state table with no primary key")
	}
}

func TestNamespacedDDL(t *testing.T) {
	source := "CREATE TABLE `config` (\n" +
		"  `key` varchar(255) NOT NULL,\n" +
		"  `value` text NOT NULL,\n" +
		"  PRIMARY KEY (`key`)\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"
	got, err := NamespacedDDL("brain_unified_config", source, "store", []string{"key"})
	if err != nil {
		t.Fatalf("NamespacedDDL: %v", err)
	}
	for _, want := range []string{
		"CREATE TABLE `brain_unified_config`",
		"`store` varchar(128) NOT NULL",
		"PRIMARY KEY (`store`, `key`)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("generated DDL is missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "PRIMARY KEY") != 1 {
		t.Errorf("generated DDL must declare exactly one primary key:\n%s", got)
	}
}

func TestLoadOrderPutsParentsFirst(t *testing.T) {
	refs := map[string][]string{
		"comments":     {"issues"},
		"dependencies": {"issues"},
		"labels":       {"issues"},
	}
	order := LoadOrder([]string{"comments", "dependencies", "issues", "labels"}, func(t string) []string {
		return refs[t]
	})
	if order[0] != "issues" {
		t.Errorf("load order = %v, want issues first", order)
	}
	if order[len(order)-1] == "issues" {
		t.Errorf("load order = %v, want issues last would be wrong: parents come first", order)
	}
}

func TestLoadOrderBreaksCyclesDeterministically(t *testing.T) {
	refs := map[string][]string{"a": {"b"}, "b": {"a"}}
	var first []string
	for i := 0; i < 20; i++ {
		order := LoadOrder([]string{"a", "b"}, func(t string) []string { return refs[t] })
		if first == nil {
			first = order
			continue
		}
		if len(order) != len(first) {
			t.Fatalf("cycle produced a different length: %v vs %v", order, first)
		}
		for j := range order {
			if order[j] != first[j] {
				t.Fatalf("cycle order not deterministic: %v vs %v", order, first)
			}
		}
	}
}

func TestSplitTopLevel(t *testing.T) {
	body := "`a` int,\n  `b` varchar(10) DEFAULT 'x,y',\n  KEY `k` (`a`,`b`)"
	got := splitTopLevel(body)
	if len(got) != 3 {
		t.Fatalf("split into %d parts, want 3: %q", len(got), got)
	}
	if !strings.Contains(got[1], "'x,y'") {
		t.Errorf("comma inside a string literal was treated as a separator: %q", got[1])
	}
}

func TestNamespacedDDLStripsConstraintsAndIndexes(t *testing.T) {
	source := "CREATE TABLE `child_counters` (\n" +
		"  `issue_id` varchar(255) NOT NULL,\n" +
		"  `level` int NOT NULL,\n" +
		"  `next` int NOT NULL DEFAULT '1',\n" +
		"  PRIMARY KEY (`issue_id`,`level`),\n" +
		"  KEY `idx_counter_level` (`level`),\n" +
		"  CONSTRAINT `fk_counter_parent` FOREIGN KEY (`issue_id`) REFERENCES `issues` (`id`) ON DELETE CASCADE\n" +
		") ENGINE=InnoDB"
	got, err := NamespacedDDL("brain_unified_child_counters", source, "store", []string{"issue_id", "level"})
	if err != nil {
		t.Fatalf("NamespacedDDL: %v", err)
	}
	// Keeping the constraint would collide with the original table's
	// identically named constraint in the same schema.
	for _, unwanted := range []string{"fk_counter_parent", "FOREIGN KEY", "idx_counter_level"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("generated DDL still contains %q:\n%s", unwanted, got)
		}
	}
	if !strings.Contains(got, "`next` int NOT NULL DEFAULT '1'") {
		t.Errorf("generated DDL dropped a column definition:\n%s", got)
	}
	if !strings.Contains(got, "PRIMARY KEY (`store`, `issue_id`, `level`)") {
		t.Errorf("generated DDL has the wrong primary key:\n%s", got)
	}
}
