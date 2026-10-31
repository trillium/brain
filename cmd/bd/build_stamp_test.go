package main

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestStampLine(t *testing.T) {
	if got := (buildStamp{}).stampLine(); got != "" {
		t.Fatalf("unstamped build must print no stamp line, got %q", got)
	}
	got := buildStamp{Commit: "abc123", Dirty: "yes", BuildTime: "2026-10-31T00:00:00Z"}.stampLine()
	for _, want := range []string{"commit abc123", "dirty yes", "built 2026-10-31T00:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Errorf("stamp line %q missing %q", got, want)
		}
	}
	if !strings.Contains((buildStamp{Commit: "abc"}).stampLine(), "dirty unknown") {
		t.Error("unrecorded dirty flag should read 'unknown'")
	}
}

func TestCurrentBuildStampUsesOnlyLdflagCommit(t *testing.T) {
	saved := Commit
	t.Cleanup(func() { Commit = saved })

	Commit = ""
	if st := currentBuildStamp(); st.stamped() {
		t.Fatalf("build without ldflag commit must be unstamped, got %+v", st)
	}
	if got := classifyBinary(currentBuildStamp(), "1111111111111111111111111111111111111111", nil); got.Status != "warn" {
		t.Fatalf("unstamped build must warn, got %+v", got)
	}

	Commit = "1111111111111111111111111111111111111111"
	if st := currentBuildStamp(); st.Commit != Commit {
		t.Fatalf("stamp commit = %q, want ldflag %q", st.Commit, Commit)
	}
}

func TestClassifyBinary(t *testing.T) {
	const mainSHA = "1111111111111111111111111111111111111111"
	const old = "2222222222222222222222222222222222222222"
	behind := func(string) bool { return true }
	notAncestor := func(string) bool { return false }

	cases := []struct {
		name       string
		stamp      buildStamp
		main       string
		anc        func(string) bool
		wantStatus string
		wantIn     string
	}{
		{"pre-stamp binary warns", buildStamp{}, mainSHA, nil, "warn", "no build stamp"},
		{"pre-stamp warns even without repo", buildStamp{}, "", nil, "warn", "no build stamp"},
		{"matches main", buildStamp{Commit: mainSHA, Dirty: "no"}, mainSHA, nil, "ok", "matches repo main"},
		{"behind main", buildStamp{Commit: old, Dirty: "no"}, mainSHA, behind, "warn", "behind main"},
		{"unlanded commit", buildStamp{Commit: old, Dirty: "no"}, mainSHA, notAncestor, "warn", "not on main"},
		{"dirty build of main", buildStamp{Commit: mainSHA, Dirty: "yes"}, mainSHA, nil, "warn", "uncommitted"},
		{"repo not found is informational", buildStamp{Commit: old}, "", nil, "info", "BRAIN_REPO"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyBinary(c.stamp, c.main, c.anc)
			if got.Status != c.wantStatus {
				t.Fatalf("status = %q, want %q (%s)", got.Status, c.wantStatus, got.Reason)
			}
			if !strings.Contains(got.Reason, c.wantIn) {
				t.Errorf("reason %q missing %q", got.Reason, c.wantIn)
			}
			if c.wantStatus == "warn" && got.Fix == "" {
				t.Error("a warning must carry the fix command")
			}
		})
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRepoMainAndCheckAgainstRealRepo(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "first")
	first := runGit(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "second")
	second := runGit(t, repo, "rev-parse", "HEAD")

	sha, anc := repoMain(context.Background(), repo)
	if sha != second {
		t.Fatalf("repoMain = %q, want %q", sha, second)
	}
	if got := classifyBinary(buildStamp{Commit: second, Dirty: "no"}, sha, anc); got.Status != "ok" {
		t.Errorf("current build should be ok, got %+v", got)
	}
	if got := classifyBinary(buildStamp{Commit: first, Dirty: "no"}, sha, anc); got.Status != "warn" || !strings.Contains(got.Reason, "behind main") {
		t.Errorf("older ancestor should be 'behind main', got %+v", got)
	}

	if sha, _ := repoMain(context.Background(), t.TempDir()); sha != "" {
		t.Errorf("non-repo dir must resolve no main, got %q", sha)
	}
	if sha, _ := repoMain(context.Background(), ""); sha != "" {
		t.Errorf("empty repo path must resolve no main, got %q", sha)
	}
}

func TestPrintBinaryCheck(t *testing.T) {
	var buf bytes.Buffer
	printBinaryCheck(&buf, binaryCheck{Status: "warn", Reason: "stale", Fix: "do it"})
	out := buf.String()
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "binary") || !strings.Contains(out, "fix: do it") {
		t.Errorf("unexpected doctor line: %q", out)
	}
}
