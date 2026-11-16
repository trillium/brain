package main

import (
	"strings"
	"testing"
)

// The keep pattern is the one string the loader hands the server to decide
// which words reach the ranker, so its shape is an owned contract: every
// Like term must be present, verbatim and regex-safe, inside the negative
// lookahead that spares words beginning with a query term.
func TestRecallKeepPatternSparesEachQueryTerm(t *testing.T) {
	got := recallKeepPattern([]string{"imsg", "launchd", "c++"})
	for _, want := range []string{"imsg", "launchd", `c\+\+`} {
		if !strings.Contains(got, want) {
			t.Errorf("pattern %q lacks term %q", got, want)
		}
	}
	if !strings.HasPrefix(got, "(?i)") {
		t.Errorf("pattern %q is not case-insensitive", got)
	}
	if !strings.Contains(got, "(?!(?:imsg|launchd|c\\+\\+))") {
		t.Errorf("pattern %q does not spare the terms as one alternation", got)
	}
}
