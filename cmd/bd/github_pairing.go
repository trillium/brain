package main

import (
	"fmt"
	"strings"
)

// A pairing ties one brain store to one GitHub repository and names the
// direction changes travel. It lives in the store's own config (the same
// github.* keys the rest of the integration already reads), so it belongs to
// that store alone: in the shared database each store has its own config rows,
// and pairing one store touches no other.
//
//	github.repository   owner/repo        (the existing key)
//	github.direction    two-way | brain-to-github | github-to-brain
//	github.conflict     prefer-newer | prefer-local | prefer-github  (two-way only)
//	github.namespace    the store name written into every mirrored issue
const (
	githubKeyRepository = "github.repository"
	githubKeyDirection  = "github.direction"
	githubKeyConflict   = "github.conflict"
	githubKeyNamespace  = "github.namespace"
)

// GitHubDirection is the way changes travel between a store and its repository.
type GitHubDirection string

const (
	// GitHubTwoWay carries changes both ways: new and changed brain items go to
	// GitHub, new and changed issues come into brain, and an item changed on
	// both sides is settled by the conflict policy.
	GitHubTwoWay GitHubDirection = "two-way"
	// GitHubBrainToGitHub makes brain the source and GitHub a mirror. Nothing is
	// ever pulled; an edit made on GitHub is overwritten by the next sync.
	GitHubBrainToGitHub GitHubDirection = "brain-to-github"
	// GitHubGitHubToBrain makes GitHub the source and brain a mirror. Nothing is
	// ever pushed; an edit made in brain to a mirrored item is overwritten by the
	// next sync.
	GitHubGitHubToBrain GitHubDirection = "github-to-brain"
)

// defaultGitHubDirection is what a store with a repository but no recorded
// direction does, and what an unpaired ad hoc run does: the behaviour
// 'brain github sync' has always had.
const defaultGitHubDirection = GitHubTwoWay

// defaultGitHubConflict settles an item changed on both sides of a two-way
// pairing: the side edited most recently wins. Pick prefer-local to make brain
// always win, prefer-github to make GitHub always win.
const defaultGitHubConflict = GitHubConflictPreferNewer

// parseGitHubDirection reads a direction name. The empty string is the default.
func parseGitHubDirection(s string) (GitHubDirection, error) {
	switch d := GitHubDirection(strings.ToLower(strings.TrimSpace(s))); d {
	case "":
		return defaultGitHubDirection, nil
	case GitHubTwoWay, GitHubBrainToGitHub, GitHubGitHubToBrain:
		return d, nil
	default:
		return "", fmt.Errorf("unknown direction %q (use %s, %s or %s)", s, GitHubTwoWay, GitHubBrainToGitHub, GitHubGitHubToBrain)
	}
}

// parseGitHubConflict reads a conflict policy name. The empty string is the default.
func parseGitHubConflict(s string) (GitHubConflictStrategy, error) {
	switch c := GitHubConflictStrategy(strings.ToLower(strings.TrimSpace(s))); c {
	case "":
		return defaultGitHubConflict, nil
	case GitHubConflictPreferNewer, GitHubConflictPreferLocal, GitHubConflictPreferGitHub:
		return c, nil
	default:
		return "", fmt.Errorf("unknown conflict policy %q (use %s, %s or %s)", s, GitHubConflictPreferNewer, GitHubConflictPreferLocal, GitHubConflictPreferGitHub)
	}
}

// parseGitHubRepository splits "owner/repo". A full https URL is accepted and
// reduced, so a pasted repository link works.
func parseGitHubRepository(s string) (owner, repo string, err error) {
	s = strings.TrimSpace(s)
	for _, prefix := range []string{"https://github.com/", "http://github.com/", "github.com/"} {
		s = strings.TrimPrefix(s, prefix)
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(s, " \t?#") {
		return "", "", fmt.Errorf("repository %q is not owner/repo", s)
	}
	return parts[0], parts[1], nil
}

// GitHubPairing is one store-to-repository pairing.
type GitHubPairing struct {
	Store     string                 `json:"store"`
	Owner     string                 `json:"owner"`
	Repo      string                 `json:"repo"`
	Direction GitHubDirection        `json:"direction"`
	Conflict  GitHubConflictStrategy `json:"conflict"`
	// Recorded is true when the pairing was saved with 'github pair'; false
	// for an ad hoc run configured only by flags and environment.
	Recorded bool `json:"recorded"`
}

// Repository is the "owner/repo" form.
func (p GitHubPairing) Repository() string {
	if p.Owner == "" || p.Repo == "" {
		return ""
	}
	return p.Owner + "/" + p.Repo
}

// loadGitHubPairing reads the pairing from config values. get returns "" for a
// key that is not set. A recorded direction or conflict policy that does not
// parse is an error, never silently replaced by a default: a typo must not
// turn a one-way mirror into a two-way sync.
func loadGitHubPairing(get func(key string) string, store string) (GitHubPairing, error) {
	p := GitHubPairing{Store: store}
	if repository := get(githubKeyRepository); repository != "" {
		owner, repo, err := parseGitHubRepository(repository)
		if err != nil {
			return p, fmt.Errorf("%s: %w", githubKeyRepository, err)
		}
		p.Owner, p.Repo = owner, repo
	}
	if p.Repository() == "" {
		p.Owner, p.Repo = get("github.owner"), get("github.repo")
	}

	recorded := get(githubKeyDirection)
	direction, err := parseGitHubDirection(recorded)
	if err != nil {
		return p, fmt.Errorf("%s: %w", githubKeyDirection, err)
	}
	p.Direction = direction

	conflict, err := parseGitHubConflict(get(githubKeyConflict))
	if err != nil {
		return p, fmt.Errorf("%s: %w", githubKeyConflict, err)
	}
	p.Conflict = conflict

	p.Recorded = recorded != "" && p.Repository() != ""
	return p, nil
}

// githubRunPlan is what one sync run does, after the pairing and the flags
// have been reconciled.
type githubRunPlan struct {
	Pull, Push bool
	// SourceWins is set for a one-way pairing: the source side is the truth,
	// so an edit made to a mirrored item on the mirror side is overwritten.
	SourceWins bool
	// PullOverwrite lets a pull replace an item edited locally since the last
	// sync (github-to-brain only).
	PullOverwrite bool
	Conflict      GitHubConflictStrategy
}

// planGitHubRun reconciles the pairing direction with --pull-only/--push-only.
// A flag may narrow a two-way pairing to one of its halves for a single run.
// It may never reach past the pairing: a brain-to-github pairing refuses
// --pull-only, because pulling is exactly what the pairing promises never
// happens.
func planGitHubRun(p GitHubPairing, pullOnly, pushOnly bool) (githubRunPlan, error) {
	if pullOnly && pushOnly {
		return githubRunPlan{}, fmt.Errorf("cannot use both --pull-only and --push-only")
	}
	plan := githubRunPlan{Conflict: p.Conflict}
	switch p.Direction {
	case GitHubBrainToGitHub:
		if pullOnly {
			return plan, fmt.Errorf("this pairing is %s: nothing is pulled from GitHub, so --pull-only has nothing to do (change the pairing with 'github pair --direction')", p.Direction)
		}
		plan.Push, plan.SourceWins = true, true
	case GitHubGitHubToBrain:
		if pushOnly {
			return plan, fmt.Errorf("this pairing is %s: nothing is pushed to GitHub, so --push-only has nothing to do (change the pairing with 'github pair --direction')", p.Direction)
		}
		plan.Pull, plan.SourceWins, plan.PullOverwrite = true, true, true
	default:
		plan.Pull, plan.Push = !pushOnly, !pullOnly
	}
	return plan, nil
}
