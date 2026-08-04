package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMatchRouteForPrefix covers canonical matches, alias matches (with ID
// rewrite to the canonical prefix), and non-matches. The alias case is what
// lets a plural prefix like "ideas-" resolve to the "idea-" store.
func TestMatchRouteForPrefix(t *testing.T) {
	routes := []prefixRoute{
		{Prefix: "idea-", Path: "../ideas", Aliases: []string{"ideas-"}},
		{Prefix: "task-", Path: "../tasks"},
	}

	cases := []struct {
		name        string
		prefix      string
		id          string
		wantMatched bool
		wantPath    string
		wantID      string
	}{
		{"canonical match, id unchanged", "idea-", "idea-6f4", true, "../ideas", "idea-6f4"},
		{"alias match, id rewritten to canonical", "ideas-", "ideas-6f4", true, "../ideas", "idea-6f4"},
		{"canonical match on second route", "task-", "task-abc", true, "../tasks", "task-abc"},
		{"unaliased plural does not match", "tasks-", "tasks-abc", false, "", "tasks-abc"},
		{"unknown prefix does not match", "nope-", "nope-1", false, "", "nope-1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, gotID := matchRouteForPrefix(routes, tc.prefix, tc.id)
			if (got != nil) != tc.wantMatched {
				t.Fatalf("matched=%v, want %v", got != nil, tc.wantMatched)
			}
			if got != nil && got.Path != tc.wantPath {
				t.Errorf("path=%q, want %q", got.Path, tc.wantPath)
			}
			if gotID != tc.wantID {
				t.Errorf("id=%q, want %q", gotID, tc.wantID)
			}
		})
	}
}

// TestLoadPrefixRoutesParsesAliases ensures the aliases field round-trips
// through routes.jsonl and that alias-only fields do not break loading of
// alias-free routes.
func TestLoadPrefixRoutesParsesAliases(t *testing.T) {
	dir := t.TempDir()
	content := `{"prefix":"idea-","path":"../ideas","aliases":["ideas-"]}
{"prefix":"task-","path":"../tasks"}
`
	if err := os.WriteFile(filepath.Join(dir, "routes.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	routes, err := loadPrefixRoutes(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 {
		t.Fatalf("got %d routes, want 2", len(routes))
	}
	if len(routes[0].Aliases) != 1 || routes[0].Aliases[0] != "ideas-" {
		t.Errorf("route[0].Aliases=%v, want [ideas-]", routes[0].Aliases)
	}
	if len(routes[1].Aliases) != 0 {
		t.Errorf("route[1].Aliases=%v, want empty", routes[1].Aliases)
	}
}
