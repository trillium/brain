package main

import (
	"testing"

	"github.com/steveyegge/beads/internal/storage/sqlbuild"
	"github.com/steveyegge/beads/internal/types"
)

// score is a test helper that scores an issue against a query using the same
// tokenization the SQL layer uses.
func score(title, desc, query string) int {
	issue := &types.Issue{Title: title, Description: desc}
	return scoreIssue(issue, sqlbuild.SearchTokens(query), lowerTrim(query))
}

func lowerTrim(s string) string {
	// Mirror rankSearchResults' rawQuery normalization without exporting it.
	out := make([]byte, 0, len(s))
	// TrimSpace + ToLower for ASCII; test inputs are ASCII.
	start, end := 0, len(s)
	for start < end && s[start] == ' ' {
		start++
	}
	for end > start && s[end-1] == ' ' {
		end--
	}
	for i := start; i < end; i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}

// TestScoreIssue_Ordering is the core task-4ja assertion: for the query
// "agentic agent", an entry whose title matches both tokens (and the phrase)
// outranks one that matches both tokens split across title+description, which in
// turn outranks one that matches only the single token "agent".
func TestScoreIssue_Ordering(t *testing.T) {
	t.Parallel()

	const q = "agentic agent"

	agenticBoth := score("Agentic agent framework", "agentic agent design", q)
	splitFields := score("Agent runtime", "with agentic scheduling", q)
	agentOnly := score("Agent login flow", "user auth session", q)

	if !(agenticBoth > splitFields) {
		t.Errorf("agentic-both (%d) should outrank split-fields (%d)", agenticBoth, splitFields)
	}
	if !(splitFields > agentOnly) {
		t.Errorf("split-fields (%d) should outrank agent-only (%d)", splitFields, agentOnly)
	}
	if agentOnly <= 0 {
		t.Errorf("agent-only should still score > 0 (single-token match), got %d", agentOnly)
	}
}

// TestScoreIssue_PhraseInTitleWins verifies the whole-phrase-in-title signal
// dominates scattered token matches.
func TestScoreIssue_PhraseInTitleWins(t *testing.T) {
	t.Parallel()

	const q = "login bug"
	phraseTitle := score("Login bug on submit", "", q)
	scattered := score("Bug in the login form", "", q) // both tokens, not adjacent

	if !(phraseTitle > scattered) {
		t.Errorf("phrase-in-title (%d) should outrank scattered tokens (%d)", phraseTitle, scattered)
	}
}

// TestScoreIssue_TitleBeatsDescription verifies a title match outranks a
// description-only match for the same token.
func TestScoreIssue_TitleBeatsDescription(t *testing.T) {
	t.Parallel()

	const q = "database"
	inTitle := score("Database migration", "", q)
	inDesc := score("Migration work", "touches the database", q)

	if !(inTitle > inDesc) {
		t.Errorf("title match (%d) should outrank description match (%d)", inTitle, inDesc)
	}
}

// TestWordBoundaryHit verifies whole-word/prefix detection versus internal
// substrings.
func TestWordBoundaryHit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		haystack, token string
		want            bool
	}{
		{"agent runtime", "agent", true},   // start of string
		{"the agent here", "agent", true},  // after a space
		{"reagent levels", "agent", false}, // internal substring only
		{"management", "agent", false},     // no occurrence
		{"multi-word", "word", true},       // after punctuation
	}
	for _, tt := range tests {
		if got := wordBoundaryHit(tt.haystack, tt.token); got != tt.want {
			t.Errorf("wordBoundaryHit(%q, %q) = %v, want %v", tt.haystack, tt.token, got, tt.want)
		}
	}
}

// TestRankSearchResults_AgenticAboveAgent verifies the in-place ranking sort
// places the best match first for a multi-token query.
func TestRankSearchResults_AgenticAboveAgent(t *testing.T) {
	t.Parallel()

	issues := []*types.Issue{
		{ID: "z-agent", Title: "Agent login flow", Description: "user auth"},
		{ID: "m-split", Title: "Agent runtime", Description: "with agentic scheduling"},
		{ID: "a-agentic", Title: "Agentic agent framework", Description: "agentic agent design"},
	}

	rankSearchResults(issues, "agentic agent")

	if issues[0].ID != "a-agentic" {
		t.Fatalf("expected agentic entry ranked first, got order: %s, %s, %s",
			issues[0].ID, issues[1].ID, issues[2].ID)
	}
	if issues[1].ID != "m-split" {
		t.Errorf("expected split-field entry ranked second, got %s", issues[1].ID)
	}
	if issues[2].ID != "z-agent" {
		t.Errorf("expected agent-only entry ranked last, got %s", issues[2].ID)
	}
}

// TestRankSearchResults_StableTieBreakByID verifies equal-scoring issues are
// ordered deterministically by ID so output is reproducible.
func TestRankSearchResults_StableTieBreakByID(t *testing.T) {
	t.Parallel()

	// Both titles match the single token "same" identically → equal score.
	issues := []*types.Issue{
		{ID: "bbb", Title: "same thing"},
		{ID: "aaa", Title: "same thing"},
	}
	rankSearchResults(issues, "same")

	if issues[0].ID != "aaa" || issues[1].ID != "bbb" {
		t.Errorf("expected deterministic ID tie-break aaa,bbb; got %s,%s", issues[0].ID, issues[1].ID)
	}
}
