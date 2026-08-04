package main

import (
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/storage/sqlbuild"
	"github.com/steveyegge/beads/internal/types"
)

// Relevance score weights. Kept as named constants so the ranking behavior is
// auditable and the unit tests can reason about relative ordering rather than
// magic numbers (task-4ja).
const (
	scorePhraseInTitle       = 1000 // exact full query phrase appears in the title
	scorePhraseInDescription = 400  // exact full query phrase appears in the description
	scoreAllTokensInTitle    = 300  // every token present in the title
	scoreAllTokensInCombined = 150  // every token present across title+description
	scoreTokenInTitle        = 40   // per-token, token present in the title
	scoreTokenInDescription  = 10   // per-token, token present in the description
	scoreBoundaryTitle       = 5    // per-token word-boundary/prefix bonus in the title
	scoreBoundaryDescription = 2    // per-token word-boundary/prefix bonus in the description
)

// scoreIssue computes a relevance score for a single issue against a free-text
// query. tokens are the lowercased, de-duplicated query tokens (see
// sqlbuild.SearchTokens) and rawQuery is the lowercased, trimmed full query
// used for whole-phrase matching. Higher is more relevant. The score is a pure
// function of the issue's title/description and the query, so ranking is
// deterministic.
func scoreIssue(issue *types.Issue, tokens []string, rawQuery string) int {
	if issue == nil {
		return 0
	}
	title := strings.ToLower(issue.Title)
	desc := strings.ToLower(issue.Description)
	// A single combined haystack for "all tokens present anywhere" checks. The
	// separator prevents a token from spanning the title/description boundary.
	combined := title + "\n" + desc

	score := 0

	// Whole-phrase matches are the strongest signal. Title and description are
	// scored independently so a phrase present in both outranks one present in
	// only one (monotonic: more matches never lowers the score).
	if rawQuery != "" {
		if strings.Contains(title, rawQuery) {
			score += scorePhraseInTitle
		}
		if strings.Contains(desc, rawQuery) {
			score += scorePhraseInDescription
		}
	}

	// All-tokens-present bonuses reward multi-token queries whose every term is
	// covered, without requiring adjacency.
	if len(tokens) > 0 {
		allInTitle := true
		allInCombined := true
		for _, tok := range tokens {
			if !strings.Contains(title, tok) {
				allInTitle = false
			}
			if !strings.Contains(combined, tok) {
				allInCombined = false
			}
		}
		if allInTitle {
			score += scoreAllTokensInTitle
		} else if allInCombined {
			score += scoreAllTokensInCombined
		}
	}

	// Per-token contributions plus a small word-boundary/prefix bonus so that a
	// token matching a whole word ("agent") ranks above one matching only as a
	// substring ("management").
	for _, tok := range tokens {
		if strings.Contains(title, tok) {
			score += scoreTokenInTitle
			if wordBoundaryHit(title, tok) {
				score += scoreBoundaryTitle
			}
		}
		if strings.Contains(desc, tok) {
			score += scoreTokenInDescription
			if wordBoundaryHit(desc, tok) {
				score += scoreBoundaryDescription
			}
		}
	}

	return score
}

// wordBoundaryHit reports whether token occurs in haystack at a word boundary —
// at the start of the string or immediately after a non-alphanumeric rune. This
// captures whole-word and prefix matches ("agent" in "agent runtime") while
// excluding purely internal substrings ("agent" in "management").
func wordBoundaryHit(haystack, token string) bool {
	if token == "" {
		return false
	}
	from := 0
	for {
		i := strings.Index(haystack[from:], token)
		if i < 0 {
			return false
		}
		pos := from + i
		if pos == 0 || !isWordByte(haystack[pos-1]) {
			return true
		}
		from = pos + 1
		if from >= len(haystack) {
			return false
		}
	}
}

// isWordByte reports whether b is an ASCII alphanumeric byte. Query tokens are
// lowercased ASCII in practice; a non-word preceding byte (space, punctuation,
// newline separator) marks a boundary.
func isWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// rankSearchResults sorts issues in place by descending relevance to query,
// with a stable, deterministic tie-break on issue ID. Scores are precomputed
// once per issue to avoid recomputing inside the comparison function.
func rankSearchResults(issues []*types.Issue, query string) {
	if len(issues) < 2 {
		return
	}
	tokens := sqlbuild.SearchTokens(query)
	raw := strings.ToLower(strings.TrimSpace(query))

	scores := make(map[string]int, len(issues))
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		scores[issue.ID] = scoreIssue(issue, tokens, raw)
	}

	sort.SliceStable(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		if a == nil || b == nil {
			return a != nil && b == nil
		}
		si, sj := scores[a.ID], scores[b.ID]
		if si != sj {
			return si > sj
		}
		return a.ID < b.ID
	})
}
