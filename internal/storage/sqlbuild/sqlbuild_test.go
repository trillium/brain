package sqlbuild

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

func TestOrderByKnownKeys(t *testing.T) {
	t.Parallel()

	cases := []struct {
		sortBy   string
		sortDesc bool
		table    string
		want     string
	}{
		{"", false, "", "ORDER BY priority ASC, created_at DESC, id ASC"},
		{"priority", true, "", "ORDER BY priority DESC, created_at DESC, id ASC"},
		{"created", false, "", "ORDER BY created_at DESC, id ASC"},
		{"created", true, "", "ORDER BY created_at ASC, id ASC"},
		{"title", false, "i", "ORDER BY LOWER(i.title) ASC, i.id ASC"},
		{"updated", false, "i", "ORDER BY i.updated_at DESC, i.id ASC"},
		{"bogus-key", false, "", "ORDER BY priority ASC, created_at DESC, id ASC"},
		{"id", false, "", ""}, // Go-side sort
	}
	for _, tc := range cases {
		if got := OrderBy(tc.sortBy, tc.sortDesc, tc.table); got != tc.want {
			t.Errorf("OrderBy(%q, %v, %q) = %q, want %q", tc.sortBy, tc.sortDesc, tc.table, got, tc.want)
		}
	}
}

// TestUnionSortColumnsCoverSortDefs pins that every SQL-side sort key has a
// sort_* alias in UnionSortColumnsSQL, so UNION consumers can order by any
// key OrderByForColumns may emit.
func TestUnionSortColumnsCoverSortDefs(t *testing.T) {
	t.Parallel()

	for key := range SortDefs {
		alias := "sort_" + key
		if key == "" {
			alias = "sort_priority"
		}
		if !strings.Contains(UnionSortColumnsSQL, alias) {
			t.Errorf("UnionSortColumnsSQL missing alias %q for sort key %q", alias, key)
		}
	}
}

// TestLessMirrorsOrderBy spot-checks that the Go-side comparator agrees with
// the SQL default ordering on the documented tie-break chain: priority ASC,
// then created_at DESC, then id ASC.
func TestLessMirrorsOrderBy(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	older := now.Add(-time.Hour)
	a := &types.Issue{ID: "a", Priority: 1, CreatedAt: now}
	b := &types.Issue{ID: "b", Priority: 2, CreatedAt: now}
	if !Less(a, b, "", false) || Less(b, a, "", false) {
		t.Error("default sort must order priority 1 before priority 2")
	}
	c := &types.Issue{ID: "c", Priority: 1, CreatedAt: older}
	if !Less(a, c, "", false) {
		t.Error("equal priority must order newer created_at first (created_at DESC)")
	}
	d := &types.Issue{ID: "d", Priority: 1, CreatedAt: now}
	if !Less(a, d, "", false) || Less(d, a, "", false) {
		t.Error("full tie must break by id ASC")
	}
}

func TestReadyWorkExcludeTypes(t *testing.T) {
	t.Parallel()

	base := ReadyWorkExcludeTypes(nil)
	seen := make(map[types.IssueType]bool, len(base))
	for _, typ := range base {
		if seen[typ] {
			t.Errorf("duplicate type %q in default exclude list", typ)
		}
		seen[typ] = true
	}
	for _, want := range []types.IssueType{"merge-request", types.TypeGate, types.TypeMolecule, "agent", "rig", "role", "message"} {
		if !seen[want] {
			t.Errorf("default exclude list missing %q", want)
		}
	}

	extended := ReadyWorkExcludeTypes([]types.IssueType{"custom", "", types.TypeGate})
	if got, want := len(extended), len(base)+1; got != want {
		t.Errorf("extras must dedupe and drop empties: len = %d, want %d", got, want)
	}
}

func TestBuildReadyWorkWhereBatchesIDSets(t *testing.T) {
	t.Parallel()

	ids := make([]string, QueryBatchSize+1)
	for i := range ids {
		ids[i] = "x-" + strings.Repeat("a", 3)
	}
	where, args, err := BuildReadyWorkWhere(types.WorkFilter{}, IssuesFilterTables, ReadyWorkWhereInputs{DeferredChildIDs: ids})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.Count(where, "id NOT IN ("); got != 2 {
		t.Errorf("expected 2 batched NOT IN clauses for %d IDs, got %d", len(ids), got)
	}
	wantArgs := len(ids) + len(ReadyWorkExcludeTypes(nil))
	if len(args) != wantArgs {
		t.Errorf("args = %d, want %d", len(args), wantArgs)
	}
}

func TestSearchCountsSQLShape(t *testing.T) {
	t.Parallel()

	sql := SearchCountsSQL(WispsFilterTables, "WHERE x = ?", "ORDER BY y", "LIMIT 5", true, false)
	for _, want := range []string{
		"FROM wisps i",
		"FROM wisp_dependencies",
		"FROM wisp_comments",
		"FROM wisp_labels",
		"UNION ALL", // wisp reverse deps included
		"WHERE x = ?",
		"ORDER BY y",
		"LIMIT 5",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("counts SQL missing %q", want)
		}
	}

	noWispDeps := SearchCountsSQL(IssuesFilterTables, "", "", "", false, true)
	if strings.Contains(noWispDeps, "UNION ALL") {
		t.Error("counts SQL must not union wisp reverse deps when probe says absent")
	}
	if strings.Contains(noWispDeps, "JSON_ARRAYAGG(label)") {
		t.Error("counts SQL must skip the labels join when skipLabels is set")
	}
	if !strings.Contains(noWispDeps, "NULL AS labels_json") {
		t.Error("counts SQL must project NULL labels_json when skipLabels is set")
	}
}

// TestCommentMatchClause pins the correlated-EXISTS shape and the disabled
// cases, so callers can rely on "ok == false means append nothing" (robots-4m0m).
func TestCommentMatchClause(t *testing.T) {
	t.Parallel()

	clause, ok := CommentMatchClause(true, IssuesFilterTables)
	if !ok {
		t.Fatal("CommentMatchClause(true, issues) must be enabled")
	}
	want := "EXISTS (SELECT 1 FROM comments bd_cmt WHERE bd_cmt.issue_id = issues.id AND LOWER(bd_cmt.text) LIKE ?)"
	if clause != want {
		t.Errorf("clause = %q, want %q", clause, want)
	}
	if got := strings.Count(clause, "?"); got != 1 {
		t.Errorf("clause must carry exactly one placeholder, got %d", got)
	}

	wispClause, ok := CommentMatchClause(true, WispsFilterTables)
	if !ok {
		t.Fatal("CommentMatchClause(true, wisps) must be enabled")
	}
	if !strings.Contains(wispClause, "FROM wisp_comments") || !strings.Contains(wispClause, "wisps.id") {
		t.Errorf("wisp clause must target the wisp table family, got %q", wispClause)
	}

	if _, ok := CommentMatchClause(false, IssuesFilterTables); ok {
		t.Error("CommentMatchClause(false, ...) must report disabled")
	}
	if _, ok := CommentMatchClause(true, FilterTables{Main: "issues"}); ok {
		t.Error("a table family with no comments table must report disabled")
	}
}

// TestFreeTextSearchIncludesComments covers the reported defect: a free-text
// query must be able to match comment bodies, and must not when opted out.
func TestFreeTextSearchIncludesComments(t *testing.T) {
	t.Parallel()

	base, baseArgs, err := BuildIssueFilterClauses("fork-origin", types.IssueFilter{}, IssuesFilterTables)
	if err != nil {
		t.Fatalf("build (comments off): %v", err)
	}
	if strings.Contains(strings.Join(base, " "), "bd_cmt") {
		t.Errorf("comment search is opt-in; got %v", base)
	}

	withComments, withArgs, err := BuildIssueFilterClauses("fork-origin", types.IssueFilter{SearchComments: true}, IssuesFilterTables)
	if err != nil {
		t.Fatalf("build (comments on): %v", err)
	}
	joined := strings.Join(withComments, " ")
	if !strings.Contains(joined, "LOWER(bd_cmt.text) LIKE ?") {
		t.Fatalf("expected a comment predicate in %v", withComments)
	}
	// The comment predicate must be OR'd into the free-text group, not AND'd as
	// a separate clause — otherwise it narrows results instead of widening them.
	if len(withComments) != len(base) {
		t.Errorf("comment search must not add a top-level AND clause: %v vs %v", withComments, base)
	}
	if len(withArgs) != len(baseArgs)+1 {
		t.Errorf("expected exactly one extra arg per token, got %d vs %d", len(withArgs), len(baseArgs))
	}
	if strings.Count(joined, "?") != len(withArgs) {
		t.Errorf("placeholder/arg mismatch: %d placeholders, %d args", strings.Count(joined, "?"), len(withArgs))
	}
}

// TestFreeTextSearchCommentsPerToken pins that every whitespace token gets its
// own comment probe, matching the per-token title/description handling.
func TestFreeTextSearchCommentsPerToken(t *testing.T) {
	t.Parallel()

	clauses, args, err := BuildIssueFilterClauses("agentic agent", types.IssueFilter{SearchComments: true}, IssuesFilterTables)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	joined := strings.Join(clauses, " ")
	if got := strings.Count(joined, "LOWER(bd_cmt.text) LIKE ?"); got != 2 {
		t.Errorf("expected one comment probe per token (2), got %d in %q", got, joined)
	}
	if strings.Count(joined, "?") != len(args) {
		t.Errorf("placeholder/arg mismatch: %d placeholders, %d args", strings.Count(joined, "?"), len(args))
	}
}

// TestIDLikeSearchIncludesComments covers the ID-shaped query branch, which
// takes a different path through the builder.
func TestIDLikeSearchIncludesComments(t *testing.T) {
	t.Parallel()

	clauses, args, err := BuildIssueFilterClauses("robots-4m0m", types.IssueFilter{SearchComments: true}, IssuesFilterTables)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	joined := strings.Join(clauses, " ")
	if !strings.Contains(joined, "LOWER(bd_cmt.text) LIKE ?") {
		t.Fatalf("ID-like query must also probe comments, got %v", clauses)
	}
	if strings.Count(joined, "?") != len(args) {
		t.Errorf("placeholder/arg mismatch: %d placeholders, %d args", strings.Count(joined, "?"), len(args))
	}
}

// TestCommentsContainsIsAndFilter pins that --comments-contains narrows results
// (AND) regardless of SearchComments, mirroring DescriptionContains.
func TestCommentsContainsIsAndFilter(t *testing.T) {
	t.Parallel()

	clauses, args, err := BuildIssueFilterClauses("", types.IssueFilter{CommentsContains: "RollBack"}, IssuesFilterTables)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(clauses) != 1 || !strings.Contains(clauses[0], "LOWER(bd_cmt.text) LIKE ?") {
		t.Fatalf("expected a single comment clause, got %v", clauses)
	}
	if len(args) != 1 || args[0] != "%rollback%" {
		t.Errorf("expected lowercased substring pattern, got %v", args)
	}
}
