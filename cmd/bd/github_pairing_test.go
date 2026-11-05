package main

import (
	"strings"
	"testing"
)

func cfg(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseGitHubDirection(t *testing.T) {
	cases := map[string]GitHubDirection{
		"":                GitHubTwoWay,
		"two-way":         GitHubTwoWay,
		" Two-Way ":       GitHubTwoWay,
		"brain-to-github": GitHubBrainToGitHub,
		"github-to-brain": GitHubGitHubToBrain,
	}
	for in, want := range cases {
		got, err := parseGitHubDirection(in)
		if err != nil || got != want {
			t.Errorf("parseGitHubDirection(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"both", "push", "one-way", "github-to-beads"} {
		if _, err := parseGitHubDirection(bad); err == nil {
			t.Errorf("parseGitHubDirection(%q) should fail", bad)
		}
	}
}

func TestParseGitHubConflict(t *testing.T) {
	if got, err := parseGitHubConflict(""); err != nil || got != GitHubConflictPreferNewer {
		t.Fatalf("default conflict policy = %q, %v; want prefer-newer", got, err)
	}
	for _, in := range []string{"prefer-local", "prefer-github", "prefer-newer"} {
		if got, err := parseGitHubConflict(in); err != nil || string(got) != in {
			t.Errorf("parseGitHubConflict(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := parseGitHubConflict("prefer-brain"); err == nil {
		t.Error("an unknown policy must fail")
	}
}

func TestParseGitHubRepository(t *testing.T) {
	good := map[string][2]string{
		"trillium/brain":                     {"trillium", "brain"},
		"https://github.com/trillium/brain":  {"trillium", "brain"},
		"https://github.com/trillium/brain/": {"trillium", "brain"},
		"github.com/trillium/brain.git":      {"trillium", "brain"},
		"  trillium/brain  ":                 {"trillium", "brain"},
	}
	for in, want := range good {
		o, r, err := parseGitHubRepository(in)
		if err != nil || o != want[0] || r != want[1] {
			t.Errorf("parseGitHubRepository(%q) = %q, %q, %v", in, o, r, err)
		}
	}
	for _, bad := range []string{"", "brain", "a/b/c", "/brain", "trillium/", "tri llium/brain", "trillium/brain?x=1"} {
		if _, _, err := parseGitHubRepository(bad); err == nil {
			t.Errorf("parseGitHubRepository(%q) should fail", bad)
		}
	}
}

func TestLoadGitHubPairing(t *testing.T) {
	t.Run("recorded pairing", func(t *testing.T) {
		p, err := loadGitHubPairing(cfg(map[string]string{
			"github.repository": "trillium/brain",
			"github.direction":  "brain-to-github",
		}), "brain")
		if err != nil {
			t.Fatal(err)
		}
		if p.Repository() != "trillium/brain" || p.Direction != GitHubBrainToGitHub || !p.Recorded || p.Store != "brain" {
			t.Fatalf("pairing = %+v", p)
		}
	})
	t.Run("repository without a direction is two-way and not recorded", func(t *testing.T) {
		p, err := loadGitHubPairing(cfg(map[string]string{"github.repository": "trillium/brain"}), "brain")
		if err != nil {
			t.Fatal(err)
		}
		if p.Direction != GitHubTwoWay || p.Conflict != GitHubConflictPreferNewer || p.Recorded {
			t.Fatalf("pairing = %+v", p)
		}
	})
	t.Run("owner and repo keys still work", func(t *testing.T) {
		p, err := loadGitHubPairing(cfg(map[string]string{"github.owner": "o", "github.repo": "r"}), "s")
		if err != nil || p.Repository() != "o/r" {
			t.Fatalf("pairing = %+v, %v", p, err)
		}
	})
	t.Run("a mistyped direction is an error, never a default", func(t *testing.T) {
		_, err := loadGitHubPairing(cfg(map[string]string{
			"github.repository": "trillium/brain",
			"github.direction":  "brain-to-gihub",
		}), "brain")
		if err == nil || !strings.Contains(err.Error(), githubKeyDirection) {
			t.Fatalf("err = %v; a typo must not silently turn a mirror into a two-way sync", err)
		}
	})
	t.Run("a bad conflict policy is an error", func(t *testing.T) {
		if _, err := loadGitHubPairing(cfg(map[string]string{"github.conflict": "x"}), "brain"); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("pairings of different stores do not share values", func(t *testing.T) {
		a, _ := loadGitHubPairing(cfg(map[string]string{"github.repository": "o/a", "github.direction": "github-to-brain"}), "a")
		b, _ := loadGitHubPairing(cfg(map[string]string{"github.repository": "o/b"}), "b")
		if a.Repository() == b.Repository() || a.Direction == b.Direction {
			t.Fatalf("a=%+v b=%+v", a, b)
		}
	})
}

func TestPlanGitHubRun(t *testing.T) {
	type want struct{ pull, push, sourceWins, overwrite bool }
	two := GitHubPairing{Direction: GitHubTwoWay, Conflict: GitHubConflictPreferLocal}
	toGH := GitHubPairing{Direction: GitHubBrainToGitHub, Conflict: GitHubConflictPreferNewer}
	toBrain := GitHubPairing{Direction: GitHubGitHubToBrain, Conflict: GitHubConflictPreferNewer}

	cases := []struct {
		name               string
		p                  GitHubPairing
		pullOnly, pushOnly bool
		want               *want
	}{
		{"two-way", two, false, false, &want{true, true, false, false}},
		{"two-way narrowed to pull", two, true, false, &want{true, false, false, false}},
		{"two-way narrowed to push", two, false, true, &want{false, true, false, false}},
		{"brain-to-github", toGH, false, false, &want{false, true, true, false}},
		{"brain-to-github, --push-only is fine", toGH, false, true, &want{false, true, true, false}},
		{"brain-to-github refuses --pull-only", toGH, true, false, nil},
		{"github-to-brain", toBrain, false, false, &want{true, false, true, true}},
		{"github-to-brain, --pull-only is fine", toBrain, true, false, &want{true, false, true, true}},
		{"github-to-brain refuses --push-only", toBrain, false, true, nil},
		{"both flags", two, true, true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan, err := planGitHubRun(c.p, c.pullOnly, c.pushOnly)
			if c.want == nil {
				if err == nil {
					t.Fatalf("expected an error, got %+v", plan)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := want{plan.Pull, plan.Push, plan.SourceWins, plan.PullOverwrite}
			if got != *c.want {
				t.Fatalf("plan = %+v, want %+v", got, *c.want)
			}
			if plan.Conflict != c.p.Conflict {
				t.Fatalf("conflict = %q, want %q", plan.Conflict, c.p.Conflict)
			}
		})
	}
}

func TestOneWayRefusalsNameTheDirection(t *testing.T) {
	_, err := planGitHubRun(GitHubPairing{Direction: GitHubBrainToGitHub}, true, false)
	if err == nil || !strings.Contains(err.Error(), "brain-to-github") || !strings.Contains(err.Error(), "nothing is pulled") {
		t.Fatalf("err = %v", err)
	}
	_, err = planGitHubRun(GitHubPairing{Direction: GitHubGitHubToBrain}, false, true)
	if err == nil || !strings.Contains(err.Error(), "github-to-brain") || !strings.Contains(err.Error(), "nothing is pushed") {
		t.Fatalf("err = %v", err)
	}
}

func TestGitHubExcludedTypesAreSystemInternal(t *testing.T) {
	got := map[string]bool{}
	for _, ty := range githubExcludedTypes {
		got[string(ty)] = true
	}
	for _, internal := range []string{"message", "molecule", "gate", "event"} {
		if !got[internal] {
			t.Errorf("%s is brain's own plumbing and must not go to GitHub", internal)
		}
	}
	for _, work := range []string{"task", "bug", "feature", "epic", "decision", "knowledge"} {
		if got[work] {
			t.Errorf("%s is work a person would triage and must be synced", work)
		}
	}
}
