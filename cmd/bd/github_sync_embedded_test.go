//go:build cgo

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/github"
	"github.com/steveyegge/beads/internal/types"
)

// These tests drive the real bd binary against a real embedded store and a fake
// GitHub server, so they cover the whole path a person's command takes: the
// pairing in config, the direction, the sync engine, and the links between
// items and issues. They need no Docker; gate and pattern follow the other
// *_embedded_test.go files (BEADS_TEST_EMBEDDED_DOLT=1).

// fakeGitHub is a minimal GitHub issues API for one repository, acme/widgets.
type fakeGitHub struct {
	mu      sync.Mutex
	issues  map[int]*github.Issue
	next    int
	posts   int
	patches int
	// public makes the repository report itself public; the default is private.
	public bool
	server *httptest.Server
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{issues: map[int]*github.Issue{}, next: 1}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets")
	switch {
	case path == "" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(map[string]any{"full_name": "acme/widgets", "name": "widgets", "private": !f.public})
	case path == "/issues" && r.Method == http.MethodGet:
		state := r.URL.Query().Get("state")
		var since time.Time
		if s := r.URL.Query().Get("since"); s != "" {
			since, _ = time.Parse(time.RFC3339, s)
		}
		out := []*github.Issue{}
		for n := 1; n < f.next; n++ {
			gh := f.issues[n]
			if gh == nil || (state != "all" && state != "" && gh.State != state) {
				continue
			}
			if gh.UpdatedAt != nil && gh.UpdatedAt.Before(since) {
				continue
			}
			out = append(out, gh)
		}
		_ = json.NewEncoder(w).Encode(out)
	case path == "/issues" && r.Method == http.MethodPost:
		var req struct {
			Title  string   `json:"title"`
			Body   string   `json:"body"`
			Labels []string `json:"labels"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.posts++
		gh := f.put(&github.Issue{Title: req.Title, Body: req.Body, State: "open"}, req.Labels)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(gh)
	case strings.HasPrefix(path, "/issues/"):
		n, _ := strconv.Atoi(strings.TrimPrefix(path, "/issues/"))
		gh := f.issues[n]
		if gh == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPatch {
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.patches++
			if v, ok := req["title"].(string); ok {
				gh.Title = v
			}
			if v, ok := req["body"].(string); ok {
				gh.Body = v
			}
			if v, ok := req["state"].(string); ok {
				gh.State = v
			}
			if v, ok := req["labels"].([]any); ok {
				gh.Labels = nil
				for _, l := range v {
					gh.Labels = append(gh.Labels, github.Label{Name: l.(string)})
				}
			}
			now := time.Now().UTC().Truncate(time.Second)
			gh.UpdatedAt = &now
		}
		_ = json.NewEncoder(w).Encode(gh)
	default:
		http.NotFound(w, r)
	}
}

// put stores an issue as GitHub would have created it just now.
func (f *fakeGitHub) put(gh *github.Issue, labels []string) *github.Issue {
	gh.Number = f.next
	gh.ID = 9000 + f.next
	// The fake is a GitHub Enterprise host: its issues live on the host that
	// serves its API, as RefScope requires of a ref in the paired repository.
	gh.HTMLURL = fmt.Sprintf("%s/acme/widgets/issues/%d", f.server.URL, f.next)
	for _, l := range labels {
		gh.Labels = append(gh.Labels, github.Label{Name: l})
	}
	now := time.Now().UTC().Truncate(time.Second)
	gh.CreatedAt, gh.UpdatedAt = &now, &now
	f.issues[f.next] = gh
	f.next++
	return gh
}

// seed adds an issue made by a person on GitHub.
func (f *fakeGitHub) seed(title, body string, labels ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.put(&github.Issue{Title: title, Body: body, State: "open"}, labels).Number
}

// edit changes an issue the way a person on GitHub would.
func (f *fakeGitHub) edit(n int, fn func(*github.Issue)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.issues[n])
	now := time.Now().UTC().Truncate(time.Second)
	f.issues[n].UpdatedAt = &now
}

// numberOf finds an issue by title; sync does not promise an order.
func (f *fakeGitHub) numberOf(t *testing.T, title string) int {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for n, gh := range f.issues {
		if gh.Title == title {
			return n
		}
	}
	t.Fatalf("no issue titled %q", title)
	return 0
}

func (f *fakeGitHub) get(n int) github.Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.issues[n]
}

func (f *fakeGitHub) counts() (issues, posts, patches int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.issues), f.posts, f.patches
}

// ghStore is one embedded store plus the helpers to drive its github commands.
type ghStore struct {
	t   *testing.T
	bd  string
	dir string
	gh  *fakeGitHub
}

func newGHStore(t *testing.T, prefix string, gh *fakeGitHub) *ghStore {
	t.Helper()
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", prefix)
	return &ghStore{t: t, bd: bd, dir: dir, gh: gh}
}

// run executes bd and returns its output and error.
func (s *ghStore) run(args ...string) (string, error) {
	s.t.Helper()
	for attempt := 0; attempt < 10; attempt++ {
		cmd := exec.Command(s.bd, args...)
		cmd.Dir = s.dir
		cmd.Env = append(bdEnv(s.dir), "GITHUB_TOKEN=test-token", "GITHUB_API_URL="+s.gh.server.URL)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		out := stdout.String() + stderr.String()
		if err != nil && isEmbeddedLockOutput(out) {
			time.Sleep(time.Duration(500*(attempt+1)) * time.Millisecond)
			continue
		}
		return out, err
	}
	return "", fmt.Errorf("bd %v: embedded lock never released", args)
}

func (s *ghStore) must(args ...string) string {
	s.t.Helper()
	out, err := s.run(args...)
	if err != nil {
		s.t.Fatalf("bd %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (s *ghStore) mustFail(args ...string) string {
	s.t.Helper()
	out, err := s.run(args...)
	if err == nil {
		s.t.Fatalf("bd %s should have failed:\n%s", strings.Join(args, " "), out)
	}
	return out
}

// sync runs 'github sync --json' and returns the summary.
func (s *ghStore) sync(args ...string) githubPlanSummary {
	s.t.Helper()
	out := s.must(append([]string{"github", "sync", "--json"}, args...)...)
	start, end := strings.Index(out, "{"), strings.LastIndex(out, "}")
	if start < 0 || end < start {
		s.t.Fatalf("no JSON summary in:\n%s", out)
	}
	var sum githubPlanSummary
	if err := json.Unmarshal([]byte(out[start:end+1]), &sum); err != nil {
		s.t.Fatalf("parse summary: %v\n%s", err, out)
	}
	return sum
}

func (s *ghStore) create(title string, args ...string) string {
	s.t.Helper()
	return bdCreateSilent(s.t, s.bd, s.dir, append([]string{title}, args...)...)
}

func (s *ghStore) show(id string) *types.Issue { return bdShow(s.t, s.bd, s.dir, id) }

func (s *ghStore) titles() []string {
	s.t.Helper()
	out := s.must("list", "--all", "--json", "--limit", "0")
	start := strings.Index(out, "[")
	var items []types.Issue
	if err := json.Unmarshal([]byte(out[start:]), &items); err != nil {
		s.t.Fatalf("parse list: %v\n%s", err, out)
	}
	var titles []string
	for _, it := range items {
		titles = append(titles, it.Title)
	}
	return titles
}

func (s *ghStore) pair(args ...string) {
	s.t.Helper()
	s.must(append([]string{"github", "pair", "acme/widgets", "--no-verify"}, args...)...)
}

// settle waits out the one-second rounding the engine applies to its own
// last-sync stamp, so the next edit is unmistakably after the last sync.
func settle() { time.Sleep(2100 * time.Millisecond) }

func TestGitHubSyncTwoWayIsRepeatable(t *testing.T) {
	gh := newFakeGitHub(t)
	s := newGHStore(t, "tw", gh)
	a := s.create("Alpha", "-t", "bug", "-p", "1", "-d", "first item")
	b := s.create("Beta")
	s.pair()

	// The plan names what would happen and changes nothing.
	plan := s.sync("--dry-run")
	if !plan.DryRun || plan.GitHub.Create != 2 || plan.Brain.Create != 0 {
		t.Fatalf("plan = %+v, want 2 creates on GitHub", plan)
	}
	if n, posts, _ := gh.counts(); n != 0 || posts != 0 {
		t.Fatalf("dry run touched GitHub: %d issues, %d posts", n, posts)
	}

	// First sync creates one issue per item and links them.
	if got := s.sync(); got.GitHub.Create != 2 || got.Errors != 0 {
		t.Fatalf("first sync = %+v", got)
	}
	if n, posts, _ := gh.counts(); n != 2 || posts != 2 {
		t.Fatalf("after first sync: %d issues, %d posts; want 2 and 2", n, posts)
	}
	linkA := s.show(a).ExternalRef
	if linkA == nil || !strings.HasPrefix(*linkA, gh.server.URL+"/acme/widgets/issues/") {
		t.Fatalf("item %s is not linked to its issue: %v", a, linkA)
	}
	if body := gh.get(gh.numberOf(t, "Alpha")).Body; !strings.Contains(body, "first item") || !strings.Contains(body, github.BuildMarker("tw", a)) {
		t.Fatalf("issue body should carry the text and the hidden marker, got %q", body)
	}

	// Running again changes nothing, on either side.
	before := *s.show(b)
	for i := 0; i < 2; i++ {
		got := s.sync()
		if got.GitHub.Create+got.GitHub.Update+got.GitHub.Link+got.Brain.Create+got.Brain.Update != 0 {
			t.Fatalf("repeat sync %d changed something: %+v", i+1, got)
		}
	}
	if n, posts, patches := gh.counts(); n != 2 || posts != 2 || patches != 0 {
		t.Fatalf("repeat syncs wrote to GitHub: %d issues, %d posts, %d patches", n, posts, patches)
	}
	if after := s.show(b); after.Title != before.Title || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("repeat syncs rewrote item %s", b)
	}
}

func TestGitHubSyncCarriesClosedAndReopened(t *testing.T) {
	gh := newFakeGitHub(t)
	s := newGHStore(t, "cr", gh)
	a := s.create("Close me")
	b := s.create("Close me on GitHub")
	s.pair()
	s.sync()
	settle()

	s.must("close", a, "--reason", "done")
	closed := s.sync()
	na, nb := gh.numberOf(t, "Close me"), gh.numberOf(t, "Close me on GitHub")
	if got := gh.get(na).State; got != "closed" {
		t.Fatalf("closing in brain left the issue %s (sync: %+v; item: %s)", got, closed, s.show(a).Status)
	}
	settle()
	s.must("reopen", a)
	s.sync()
	if got := gh.get(na).State; got != "open" {
		t.Fatalf("reopening in brain left the issue %s", got)
	}

	settle()
	gh.edit(nb, func(i *github.Issue) { i.State = "closed" })
	s.sync()
	if got := s.show(b).Status; got != types.StatusClosed {
		t.Fatalf("closing on GitHub left the item %s", got)
	}
	settle()
	gh.edit(nb, func(i *github.Issue) { i.State = "open" })
	s.sync()
	if got := s.show(b).Status; got != types.StatusOpen {
		t.Fatalf("reopening on GitHub left the item %s", got)
	}
}

func TestGitHubSyncLostLinkDoesNotDuplicate(t *testing.T) {
	gh := newFakeGitHub(t)
	s := newGHStore(t, "ll", gh)
	a := s.create("Keep one copy")
	s.pair()
	s.sync()

	// The item forgets its issue (restored backup, interrupted run, ...).
	s.must("update", a, "--external-ref", "")
	if ref := s.show(a).ExternalRef; ref != nil && *ref != "" {
		t.Fatalf("setup: link still there: %v", *ref)
	}

	plan := s.sync("--dry-run")
	if plan.GitHub.Create != 0 || plan.GitHub.Link != 1 {
		t.Fatalf("plan = %+v; the existing issue should be linked, not recreated", plan)
	}
	if got := s.sync(); got.GitHub.Create != 0 || got.GitHub.Link != 1 {
		t.Fatalf("sync = %+v", got)
	}
	if n, posts, _ := gh.counts(); n != 1 || posts != 1 {
		t.Fatalf("a second issue appeared: %d issues, %d posts", n, posts)
	}
	if ref := s.show(a).ExternalRef; ref == nil || !strings.HasSuffix(*ref, "/issues/1") {
		t.Fatalf("item not relinked to issue 1: %v", ref)
	}
	if titles := s.titles(); len(titles) != 1 {
		t.Fatalf("a second item appeared: %v", titles)
	}
}

func TestGitHubSyncBrainEditsAreNotOverwrittenByThePull(t *testing.T) {
	gh := newFakeGitHub(t)
	s := newGHStore(t, "ed", gh)
	a := s.create("Original title")
	s.pair()
	s.sync()
	settle()

	// An edit made in brain after the last sync must reach GitHub, not be
	// replaced by GitHub's older copy.
	s.must("update", a, "--title", "Edited in brain")
	got := s.sync()
	if got.Brain.Update != 0 {
		t.Fatalf("the pull rewrote an item that was only edited locally: %+v", got)
	}
	if title := s.show(a).Title; title != "Edited in brain" {
		t.Fatalf("title = %q; the local edit was lost", title)
	}
	if title := gh.get(1).Title; title != "Edited in brain" {
		t.Fatalf("GitHub title = %q; the local edit was not pushed", title)
	}
}

func TestGitHubSyncConflictPolicies(t *testing.T) {
	for _, tc := range []struct {
		flag      string
		wantBrain string
		wantGH    string
	}{
		{"--prefer-local", "from brain", "from brain"},
		{"--prefer-github", "from GitHub", "from GitHub"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			gh := newFakeGitHub(t)
			s := newGHStore(t, "cf", gh)
			a := s.create("Contested")
			s.pair()
			s.sync()
			settle()

			s.must("update", a, "--title", "from brain")
			gh.edit(1, func(i *github.Issue) { i.Title = "from GitHub" })
			got := s.sync(tc.flag)
			if got.Conflicts != 1 {
				t.Fatalf("conflicts = %d, want 1: %+v", got.Conflicts, got)
			}
			if title := s.show(a).Title; title != tc.wantBrain {
				t.Errorf("brain title = %q, want %q", title, tc.wantBrain)
			}
			if title := gh.get(1).Title; title != tc.wantGH {
				t.Errorf("GitHub title = %q, want %q", title, tc.wantGH)
			}
		})
	}
}

func TestGitHubSyncPullsNativeIssuesAndKeepsWhatGitHubCannotSay(t *testing.T) {
	gh := newFakeGitHub(t)
	s := newGHStore(t, "pn", gh)
	assigned := s.create("Has an assignee", "-a", "alice")
	s.pair()
	gh.seed("Opened on GitHub", "from a person", "bug")
	got := s.sync()
	if got.Brain.Create != 1 || got.GitHub.Create != 1 {
		t.Fatalf("sync = %+v; want the native issue imported and the item pushed", got)
	}
	// An issue pulled from GitHub gets the hidden marker on the next sync (a
	// single quiet update), after which sync is quiet.
	settle()
	if got = s.sync(); got.GitHub.Create != 0 || got.Brain.Create != 0 || got.GitHub.Update > 1 {
		t.Fatalf("second sync = %+v", got)
	}
	settle()
	got = s.sync()
	if got.GitHub.Create+got.GitHub.Update+got.Brain.Create+got.Brain.Update != 0 {
		t.Fatalf("third sync not quiet: %+v", got)
	}
	if a := s.show(assigned).Assignee; a != "alice" {
		t.Fatalf("assignee = %q; GitHub has none for it, the item's own must stay", a)
	}
	if titles := strings.Join(s.titles(), "|"); !strings.Contains(titles, "Opened on GitHub") {
		t.Fatalf("native issue not imported: %s", titles)
	}
}

func TestGitHubSyncBrainToGitHubNeverPulls(t *testing.T) {
	gh := newFakeGitHub(t)
	s := newGHStore(t, "bg", gh)
	s.create("Mirrored item")
	gh.seed("Someone else's issue", "native")
	s.pair("--direction", "brain-to-github")

	if out := s.mustFail("github", "sync", "--pull-only"); !strings.Contains(out, "nothing is pulled") {
		t.Fatalf("--pull-only on a brain-to-github pairing should be refused clearly, got:\n%s", out)
	}
	got := s.sync()
	if got.Brain.Create+got.Brain.Update != 0 || got.GitHub.Create != 1 {
		t.Fatalf("sync = %+v", got)
	}
	if titles := s.titles(); len(titles) != 1 {
		t.Fatalf("brain-to-github imported something: %v", titles)
	}

	// GitHub is a mirror: an edit there is replaced by brain's version.
	settle()
	gh.edit(2, func(i *github.Issue) { i.Title = "Vandalised" })
	if got := s.sync(); got.GitHub.Update != 1 {
		t.Fatalf("sync = %+v; the mirror should be restored", got)
	}
	if title := gh.get(2).Title; title != "Mirrored item" {
		t.Fatalf("mirror title = %q, want it restored", title)
	}
}

func TestGitHubSyncGitHubToBrainNeverPushes(t *testing.T) {
	gh := newFakeGitHub(t)
	s := newGHStore(t, "gb", gh)
	own := s.create("A brain-only item")
	gh.seed("Native one", "x", "bug")
	gh.seed("Native two", "y")
	// Another store's mirror lives in this repository too.
	gh.seed("Other store's mirror", github.WithMarker("body", "other", "other-1"))
	s.pair("--direction", "github-to-brain")

	if out := s.mustFail("github", "sync", "--push-only"); !strings.Contains(out, "nothing is pushed") {
		t.Fatalf("--push-only on a github-to-brain pairing should be refused clearly, got:\n%s", out)
	}
	got := s.sync()
	if got.Brain.Create != 2 || got.GitHub.Create+got.GitHub.Update+got.GitHub.Link != 0 {
		t.Fatalf("sync = %+v; want the two native issues imported and nothing sent", got)
	}
	if _, posts, patches := gh.counts(); posts != 0 || patches != 0 {
		t.Fatalf("github-to-brain wrote to GitHub: %d posts, %d patches", posts, patches)
	}
	titles := strings.Join(s.titles(), "|")
	if strings.Contains(titles, "Other store's mirror") {
		t.Fatalf("another store's mirror was imported: %s", titles)
	}
	if ref := s.show(own).ExternalRef; ref != nil && *ref != "" {
		t.Fatalf("a brain-only item was linked to an issue: %s", *ref)
	}

	// Brain is the mirror: a local edit to an imported item is replaced on
	// the next sync even though the issue itself did not change.
	settle()
	imported := ""
	for _, it := range strings.Split(s.must("list", "--all", "--json", "--limit", "0"), `"id": "`)[1:] {
		id := it[:strings.Index(it, `"`)]
		if id != own {
			imported = id
			break
		}
	}
	want := s.show(imported).Title
	s.must("update", imported, "--title", "edited in brain")
	if got := s.sync(); got.Brain.Update != 1 {
		t.Fatalf("sync = %+v; the edited mirror should be restored", got)
	}
	if title := s.show(imported).Title; title != want {
		t.Fatalf("a github-to-brain mirror kept a local edit: %q (want %q)", title, want)
	}
	if got := s.sync(); got.Brain.Create+got.Brain.Update != 0 {
		t.Fatalf("a quiet mirror sync changed something: %+v", got)
	}
}

func TestGitHubPairingCommands(t *testing.T) {
	gh := newFakeGitHub(t)
	s := newGHStore(t, "pc", gh)

	if out := s.mustFail("github", "pair", "acme/widgets", "--direction", "sideways"); !strings.Contains(out, "unknown direction") {
		t.Fatalf("bad direction: %s", out)
	}
	if out := s.mustFail("github", "pair", "acme/widgets", "--direction", "brain-to-github", "--conflict", "prefer-local"); !strings.Contains(out, "two-way") {
		t.Fatalf("--conflict on a one-way pairing should be refused: %s", out)
	}
	if out := s.mustFail("github", "pair", "not-a-repo"); !strings.Contains(out, "owner/repo") {
		t.Fatalf("bad repository: %s", out)
	}

	s.must("github", "pair", "acme/widgets", "--direction", "github-to-brain")
	status := s.must("github", "status")
	for _, want := range []string{"Direction:  github-to-brain", "Store:      pc"} {
		if !strings.Contains(status, want) {
			t.Errorf("status missing %q:\n%s", want, status)
		}
	}
	if strings.Contains(status, "test-token") {
		t.Fatalf("status printed the token:\n%s", status)
	}

	if out := s.mustFail("github", "pair", "acme/other"); !strings.Contains(out, "already paired") {
		t.Fatalf("repointing without --force should be refused: %s", out)
	}
	if out := s.mustFail("github", "sync", "--dry-run", "--repo", "acme/other", "--direction", "two-way"); !strings.Contains(out, "paired") {
		t.Fatalf("an ad hoc direction must not override a recorded pairing: %s", out)
	}
	s.must("github", "pair", "acme/other", "--force", "--no-verify")
	if status := s.must("github", "status"); !strings.Contains(status, "Repository: other") {
		t.Fatalf("repoint not recorded:\n%s", status)
	}

	s.must("github", "unpair")
	if status := s.must("github", "status"); strings.Contains(status, "Direction:  github-to-brain") || !strings.Contains(status, "no pairing recorded") {
		t.Fatalf("unpair left a pairing:\n%s", status)
	}
}
