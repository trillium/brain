package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Build stamp: which repo commit this binary was built from, and whether the
// tree was dirty. Set only via -ldflags by `make build` (and so by
// scripts/install-from-repo.sh and scripts/deploy-local.sh); the vcs.revision
// Go embeds in a plain `go build` does not count. `brain stores doctor` flags
// an unstamped build of this code. A pre-stamp build (e.g. 1.3.1) has no
// `stores` command to run that check: there the signal is `bd version` showing
// no stamp line, or `stores` missing entirely.
var (
	// Dirty is "yes" when the working tree had uncommitted changes at build
	// time, "no" when clean, "" when the build did not record it.
	Dirty = ""
	// BuildTime is the UTC RFC3339 build timestamp ("" when unrecorded).
	BuildTime = ""
	// SourceRepo is the absolute path of the checkout the binary was built
	// from. The doctor uses it to find "the repo" without configuration;
	// BRAIN_REPO overrides it.
	SourceRepo = ""
)

// buildStamp is the comparable identity of an installed build.
type buildStamp struct {
	Commit    string `json:"commit,omitempty"`
	Dirty     string `json:"dirty,omitempty"` // "yes" | "no" | ""
	BuildTime string `json:"build_time,omitempty"`
}

// stamped reports whether the build recorded the commit it came from.
func (s buildStamp) stamped() bool { return s.Commit != "" }

// currentBuildStamp reads the ldflags-set stamp of the running binary.
func currentBuildStamp() buildStamp {
	return buildStamp{Commit: Commit, Dirty: Dirty, BuildTime: BuildTime}
}

// stampLine renders the stamp for `bd version`; "" when unstamped.
func (s buildStamp) stampLine() string {
	if !s.stamped() {
		return ""
	}
	dirty := s.Dirty
	if dirty == "" {
		dirty = "unknown"
	}
	built := s.BuildTime
	if built == "" {
		built = "unknown"
	}
	return fmt.Sprintf("commit %s  dirty %s  built %s", s.Commit, dirty, built)
}

// binaryCheck is the doctor's verdict on the installed binary.
type binaryCheck struct {
	Status string `json:"status"` // "ok" | "warn" | "info"
	Reason string `json:"reason"`
	Fix    string `json:"fix,omitempty"`
}

const binaryFixHint = "rebuild from the repo: scripts/install-from-repo.sh"

// classifyBinary compares a build stamp to the repo's current main commit.
// mainSHA is "" when the repo or its main could not be resolved; isAncestor
// reports whether the stamped commit is an ancestor of mainSHA (only called
// when the commits differ).
func classifyBinary(s buildStamp, mainSHA string, isAncestor func(commit string) bool) binaryCheck {
	if !s.stamped() {
		return binaryCheck{Status: "warn", Reason: "binary carries no build stamp (built before stamping, or by plain 'go build')", Fix: binaryFixHint}
	}
	short := shortCommit(s.Commit)
	if mainSHA == "" {
		return binaryCheck{Status: "info", Reason: fmt.Sprintf("binary built from %s, but the brain repo's main could not be located to compare (set BRAIN_REPO)", short)}
	}
	if s.Commit != mainSHA && !strings.HasPrefix(mainSHA, s.Commit) && !strings.HasPrefix(s.Commit, mainSHA) {
		how := "not on main"
		if isAncestor != nil && isAncestor(s.Commit) {
			how = "behind main"
		}
		return binaryCheck{Status: "warn", Reason: fmt.Sprintf("binary built from %s, %s (main is %s)", short, how, shortCommit(mainSHA)), Fix: binaryFixHint}
	}
	if s.Dirty == "yes" {
		return binaryCheck{Status: "warn", Reason: fmt.Sprintf("binary built from main %s with uncommitted changes", short), Fix: binaryFixHint}
	}
	return binaryCheck{Status: "ok", Reason: fmt.Sprintf("binary matches repo main %s", short)}
}

// repoMain resolves the brain repo's current main commit from its
// remote-tracking ref (falling back to local main). Read-only: it never
// fetches, so the answer is as fresh as the clone's last fetch.
func repoMain(ctx context.Context, repo string) (string, func(string) bool) {
	if repo == "" {
		return "", nil
	}
	if st, err := os.Stat(repo); err != nil || !st.IsDir() {
		return "", nil
	}
	rev := func(ref string) string {
		out, err := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "--verify", "-q", ref+"^{commit}").Output() //nolint:gosec // repo is operator config / build stamp
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	sha := rev("origin/main")
	if sha == "" {
		sha = rev("main")
	}
	if sha == "" {
		return "", nil
	}
	return sha, func(commit string) bool {
		return exec.CommandContext(ctx, "git", "-C", repo, "merge-base", "--is-ancestor", commit, sha).Run() == nil //nolint:gosec // see above
	}
}

// checkInstalledBinary runs the binary-identity check for the running bd.
func checkInstalledBinary() binaryCheck {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repo := os.Getenv("BRAIN_REPO")
	if repo == "" {
		repo = SourceRepo
	}
	mainSHA, anc := repoMain(ctx, repo)
	return classifyBinary(currentBuildStamp(), mainSHA, anc)
}

// printBinaryCheck writes the check as a doctor line to w.
func printBinaryCheck(w interface{ Write([]byte) (int, error) }, c binaryCheck) {
	label := map[string]string{"ok": "ok  ", "info": "info", "warn": "WARN"}[c.Status]
	fmt.Fprintf(w, "%s  binary  %s\n", label, c.Reason)
	if c.Fix != "" {
		fmt.Fprintf(w, "      fix: %s\n", c.Fix)
	}
}
