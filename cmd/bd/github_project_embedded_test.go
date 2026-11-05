//go:build cgo

package main

import (
	"strings"
	"testing"
)

// projectStore builds a store with one project record and a mix of items: some
// belong to the project, some do not.
func projectStore(t *testing.T, gh *fakeGitHub, recordMetadata string) (s *ghStore, project string, members []string, others []string) {
	t.Helper()
	s = newGHStore(t, "pj", gh)
	project = s.create("Widgets", "-t", "epic", "--metadata", recordMetadata)
	child := s.create("Child of the project", "--parent", project)
	labelled := s.create("Tied by a label", "-l", "project:"+project)
	members = []string{child, labelled}
	others = []string{
		s.create("Private client matter", "-l", "client-project"),
		s.create("Unrelated chore"),
		s.create("Mentions " + project + " only in the title"),
	}
	return s, project, members, others
}

func TestGitHubSyncProjectSendsOnlyTheProjectsItems(t *testing.T) {
	gh := newFakeGitHub(t)
	s, project, members, others := projectStore(t, gh, `{"repo":"acme/widgets"}`)

	// The dry run lists exactly the project's items and changes nothing.
	plan := s.sync("--project", project, "--dry-run")
	if !plan.DryRun || plan.Project == nil || plan.Project.InScope != 2 || plan.GitHub.Create != 2 {
		t.Fatalf("plan = %+v, project = %+v; want 2 items in scope, 2 creates", plan, plan.Project)
	}
	if plan.Project.Repo != "acme/widgets" || plan.Project.RepoSource != "metadata repo" || plan.Project.Visibility != "private" {
		t.Fatalf("project summary = %+v", plan.Project)
	}
	listed := map[string]string{}
	for _, it := range plan.Project.Items {
		listed[it.ID] = it.Reason
	}
	if len(listed) != 2 || listed[members[0]] != "under "+project || listed[members[1]] != "label project:"+project {
		t.Fatalf("listed items = %v; want the child and the labelled item with their reasons", listed)
	}
	if n, posts, _ := gh.counts(); n != 0 || posts != 0 {
		t.Fatalf("dry run touched GitHub: %d issues, %d posts", n, posts)
	}

	// A real run publishes those two and nothing else.
	if got := s.sync("--project", project); got.GitHub.Create != 2 || got.Errors != 0 {
		t.Fatalf("sync = %+v", got)
	}
	if n, posts, _ := gh.counts(); n != 2 || posts != 2 {
		t.Fatalf("GitHub has %d issues after %d posts; want exactly the 2 project items", n, posts)
	}
	gh.numberOf(t, "Child of the project")
	gh.numberOf(t, "Tied by a label")
	for _, id := range others {
		if ref := s.show(id).ExternalRef; ref != nil && *ref != "" {
			t.Fatalf("item %s is not the project's but was linked to %s", id, *ref)
		}
	}
	if ref := s.show(project).ExternalRef; ref != nil && *ref != "" {
		t.Fatalf("the project record itself was published: %s", *ref)
	}

	// Running again changes nothing.
	again := s.sync("--project", project)
	if again.GitHub.Create+again.GitHub.Update+again.GitHub.Link != 0 || again.Skipped != 2 {
		t.Fatalf("repeat sync = %+v", again)
	}
	if n, posts, patches := gh.counts(); n != 2 || posts != 2 || patches != 0 {
		t.Fatalf("repeat sync wrote to GitHub: %d issues, %d posts, %d patches", n, posts, patches)
	}

	// A project run does not claim the store is in step: it leaves the
	// recorded sync position alone.
	if out := s.must("github", "status"); strings.Contains(out, "Last sync:") {
		t.Fatalf("a project run recorded a sync position:\n%s", out)
	}
}

func TestGitHubSyncProjectNeedsARepositoryOnTheRecord(t *testing.T) {
	gh := newFakeGitHub(t)
	s, project, _, _ := projectStore(t, gh, `{"repo":"/Users/someone/code/widgets"}`)

	out := s.mustFail("github", "sync", "--project", project, "--dry-run")
	if !strings.Contains(out, "names no GitHub repository") || !strings.Contains(out, "ignored, not a GitHub repository") {
		t.Fatalf("expected a clear refusal, got:\n%s", out)
	}
	// --repo may preview, never publish.
	plan := s.sync("--project", project, "--repo", "acme/widgets", "--dry-run")
	if plan.Project == nil || plan.Project.InScope != 2 || !strings.Contains(plan.Project.RepoSource, "--repo") {
		t.Fatalf("preview with --repo = %+v", plan.Project)
	}
	out = s.mustFail("github", "sync", "--project", project, "--repo", "acme/widgets")
	if !strings.Contains(out, "may only stand in for it on a --dry-run") {
		t.Fatalf("a real run with --repo should be refused, got:\n%s", out)
	}
	if n, posts, _ := gh.counts(); n != 0 || posts != 0 {
		t.Fatalf("GitHub was written to: %d issues, %d posts", n, posts)
	}
}

func TestGitHubSyncProjectRefusesAPublicRepositoryUntilAllowed(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.public = true
	s, project, _, _ := projectStore(t, gh, `{"repo":"acme/widgets"}`)

	plan := s.sync("--project", project, "--dry-run")
	if plan.Project.Visibility != "public" {
		t.Fatalf("the plan should say the repository is public: %+v", plan.Project)
	}
	out := s.mustFail("github", "sync", "--project", project)
	if !strings.Contains(out, "public repository") || !strings.Contains(out, "--allow-public") {
		t.Fatalf("expected a public-repository refusal, got:\n%s", out)
	}
	if n, posts, _ := gh.counts(); n != 0 || posts != 0 {
		t.Fatalf("a refused run wrote to GitHub: %d issues, %d posts", n, posts)
	}
	if got := s.sync("--project", project, "--allow-public"); got.GitHub.Create != 2 {
		t.Fatalf("with --allow-public: %+v", got)
	}
}

func TestGitHubSyncProjectIsAlwaysOneWay(t *testing.T) {
	gh := newFakeGitHub(t)
	s, project, _, _ := projectStore(t, gh, `{"repo":"acme/widgets"}`)
	gh.seed("Written on GitHub", "by a person")

	for _, args := range [][]string{
		{"--pull-only"},
		{"--direction", "two-way"},
		{"--prefer-local"},
	} {
		out := s.mustFail(append([]string{"github", "sync", "--project", project, "--dry-run"}, args...)...)
		if out == "" {
			t.Fatalf("%v: expected a refusal", args)
		}
	}
	s.sync("--project", project)
	for _, title := range s.titles() {
		if title == "Written on GitHub" {
			t.Fatalf("a project run pulled an issue into the store")
		}
	}
}

// A project run never records a sync position, so the first two-way run after
// it has no baseline. It must not replace a brain edit with GitHub's older copy.
func TestGitHubSyncTwoWayAfterProjectRunKeepsBrainEdit(t *testing.T) {
	gh := newFakeGitHub(t)
	s, project, members, _ := projectStore(t, gh, `{"repo":"acme/widgets"}`)
	s.sync("--project", project)
	settle()

	s.must("update", members[1], "--title", "Edited in brain")
	got := s.sync("--repo", "acme/widgets", "--direction", "two-way")
	if got.Brain.Update != 0 {
		t.Fatalf("the pull rewrote an item that was only edited in brain: %+v", got)
	}
	if title := s.show(members[1]).Title; title != "Edited in brain" {
		t.Fatalf("title = %q; the brain edit was lost", title)
	}
	for i := 1; i <= 2; i++ {
		if gh.get(i).Title == "Edited in brain" {
			return
		}
	}
	t.Fatalf("the brain edit never reached GitHub")
}

func TestGitHubSyncProjectLabelsAreOptIn(t *testing.T) {
	gh := newFakeGitHub(t)
	s, project, _, _ := projectStore(t, gh, `{"repo":"acme/widgets","project_labels":"project:widgets-slug"}`)
	s.create("Tied by the record's slug label", "-l", "project:widgets-slug")
	s.create("Tied by a run-only label", "-l", "project:extra")

	plan := s.sync("--project", project, "--dry-run")
	if plan.Project.InScope != 3 {
		t.Fatalf("with the record's own alias: %d in scope, want 3", plan.Project.InScope)
	}
	plan = s.sync("--project", project, "--project-label", "extra", "--dry-run")
	if plan.Project.InScope != 4 {
		t.Fatalf("with --project-label: %d in scope, want 4", plan.Project.InScope)
	}
	out := s.mustFail("github", "sync", "--project-label", "extra", "--dry-run")
	if !strings.Contains(out, "only apply together with --project") {
		t.Fatalf("--project-label without --project should be refused, got:\n%s", out)
	}
}
