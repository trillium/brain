package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

func newPairedTracker(namespace string) *Tracker {
	tr := &Tracker{Namespace: namespace, client: NewClient("", "acme", "widgets")}
	tr.config = DefaultMappingConfig()
	tr.config.Namespace = namespace
	return tr
}

func ghIssue(number int, title, body, state string, labels ...string) *Issue {
	gh := &Issue{ID: number + 1000, Number: number, Title: title, Body: body, State: state,
		HTMLURL: fmt.Sprintf("https://github.com/acme/widgets/issues/%d", number)}
	for _, l := range labels {
		gh.Labels = append(gh.Labels, Label{Name: l})
	}
	return gh
}

// A link into another repository is a GitHub ref with no number here: the
// sync skips its item instead of reading the number in the paired repository.
func TestExtractIdentifierIsRepositoryScoped(t *testing.T) {
	tr := newPairedTracker("brain")
	cases := []struct {
		ref  string
		want string
	}{
		{"https://github.com/acme/widgets/issues/7", "7"},
		{"https://github.com/ACME/Widgets/issues/7", "7"},
		{"https://github.com/acme/other/issues/7", ""},
		{"https://github.com/someone/widgets/issues/7", ""},
		{"github:7", "7"},
	}
	// On a GitHub Enterprise host the configured repository's refs count too.
	ghe := &Tracker{client: NewClient("", "acme", "widgets").WithBaseURL("https://ghe.example.com/api/v3")}
	if !ghe.IsExternalRef("https://ghe.example.com/acme/widgets/issues/7") || ghe.ExtractIdentifier("https://ghe.example.com/acme/widgets/issues/7") != "7" {
		t.Error("a ref into the configured repository on its Enterprise host must be this tracker's")
	}
	for _, c := range cases {
		if !tr.IsExternalRef(c.ref) {
			t.Errorf("IsExternalRef(%q) = false, want true", c.ref)
		}
		if got := tr.ExtractIdentifier(c.ref); got != c.want {
			t.Errorf("ExtractIdentifier(%q) = %q, want %q", c.ref, got, c.want)
		}
	}
}

func TestContentEqual(t *testing.T) {
	tr := newPairedTracker("brain")
	local := &types.Issue{ID: "brain-1", Title: "T", Description: "body", Priority: 1, IssueType: types.TypeBug, Status: types.StatusOpen}
	remote := func(mod func(*Issue)) *tracker.TrackerIssue {
		gh := ghIssue(1, "T", WithMarker("body", "brain", "brain-1"), "open", "type::bug", "priority::high")
		if mod != nil {
			mod(gh)
		}
		ti := githubToTrackerIssue(gh)
		return &ti
	}

	if !tr.ContentEqual(local, remote(nil)) {
		t.Fatal("identical item and issue must compare equal")
	}
	if !tr.ContentEqual(local, remote(func(g *Issue) { g.Body = strings.ReplaceAll(g.Body, "\n", "\r\n") })) {
		t.Error("CRLF line endings GitHub may store must not count as a change")
	}
	for name, mod := range map[string]func(*Issue){
		"title":      func(g *Issue) { g.Title = "other" },
		"body":       func(g *Issue) { g.Body = WithMarker("changed", "brain", "brain-1") },
		"state":      func(g *Issue) { g.State = "closed" },
		"labels":     func(g *Issue) { g.Labels = append(g.Labels, Label{Name: "extra"}) },
		"no marker":  func(g *Issue) { g.Body = "body" },
		"other item": func(g *Issue) { g.Body = WithMarker("body", "brain", "brain-9") },
	} {
		if tr.ContentEqual(local, remote(mod)) {
			t.Errorf("a difference in %s must not compare equal", name)
		}
	}
}

func TestStatusLabelsSurviveAnyStatus(t *testing.T) {
	cfg := DefaultMappingConfig()
	for _, status := range []types.Status{types.StatusInProgress, types.StatusBlocked, types.StatusDeferred, types.StatusPinned, types.StatusHooked, "done"} {
		fields := BeadsIssueToGitHubFields(&types.Issue{ID: "x-1", Status: status}, cfg)
		labels := fields["labels"].([]string)
		if !hasLabel(labels, "status::"+string(status)) {
			t.Errorf("status %q lost: labels %v", status, labels)
		}
		if fields["state"] != "open" {
			t.Errorf("status %q should leave the issue open, got %v", status, fields["state"])
		}
	}
	for _, status := range []types.Status{types.StatusOpen, types.StatusClosed} {
		labels := BeadsIssueToGitHubFields(&types.Issue{ID: "x-1", Status: status}, cfg)["labels"].([]string)
		for _, l := range labels {
			if strings.HasPrefix(l, "status::") {
				t.Errorf("status %q should not get a label, got %v", status, labels)
			}
		}
	}
}

func TestPullMapsMarkerBackToItsItem(t *testing.T) {
	cfg := DefaultMappingConfig()
	cfg.Namespace = "brain"
	own := GitHubIssueToBeads(ghIssue(3, "t", WithMarker("hello", "brain", "brain-3"), "open"), cfg)
	if own.Issue.ID != "brain-3" || own.Issue.Description != "hello" {
		t.Fatalf("own marker: id=%q desc=%q", own.Issue.ID, own.Issue.Description)
	}
	foreign := GitHubIssueToBeads(ghIssue(4, "t", WithMarker("hello", "other", "other-1"), "open"), cfg)
	if foreign.Issue.ID != "" {
		t.Fatalf("another store's marker must not name an item here, got id %q", foreign.Issue.ID)
	}
	native := GitHubIssueToBeads(ghIssue(5, "t", "plain", "open"), cfg)
	if native.Issue.ID != "" || native.Issue.Description != "plain" {
		t.Fatalf("native issue: id=%q desc=%q", native.Issue.ID, native.Issue.Description)
	}
}

func TestShouldImportRefusesAnotherStoresMirror(t *testing.T) {
	tr := newPairedTracker("brain")
	imp := func(body string) bool {
		ti := githubToTrackerIssue(ghIssue(1, "t", body, "open"))
		return tr.ShouldImport(&ti)
	}
	if !imp("a native issue") {
		t.Error("native issues are imported")
	}
	if !imp(WithMarker("x", "brain", "brain-1")) {
		t.Error("this store's own mirrors are matched, not skipped")
	}
	if imp(WithMarker("x", "task", "task-1")) {
		t.Error("another store's mirror must not be imported")
	}
}

func TestAfterConvertKeepsWhatGitHubCannotSay(t *testing.T) {
	tr := newPairedTracker("brain")
	existing := &types.Issue{ID: "brain-1", Assignee: "alice", IssueType: "knowledge", Status: types.StatusPinned, Description: "line\n"}
	gh := ghIssue(1, "t", WithMarker("line", "brain", "brain-1"), "open", "type::knowledge", "status::pinned")
	ti := githubToTrackerIssue(gh)
	conv := tr.FieldMapper().IssueToBeads(&ti)

	if err := tr.AfterConvert(context.Background(), &ti, conv, "", existing, tracker.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	got := conv.Issue
	if got.Assignee != "alice" {
		t.Errorf("assignee = %q, GitHub has none so the item's must stay", got.Assignee)
	}
	if got.IssueType != "knowledge" {
		t.Errorf("type = %q, want knowledge", got.IssueType)
	}
	if got.Status != types.StatusPinned {
		t.Errorf("status = %q, want pinned", got.Status)
	}
	if got.Description != "line\n" {
		t.Errorf("description = %q, trailing newline must not count as a change", got.Description)
	}
}

func TestAfterConvertLetsGitHubChangeWhatItCanSay(t *testing.T) {
	tr := newPairedTracker("brain")
	existing := &types.Issue{ID: "brain-1", IssueType: types.TypeTask, Status: types.StatusInProgress}
	// Someone changed the type label and closed the issue on GitHub.
	gh := ghIssue(1, "t", WithMarker("", "brain", "brain-1"), "closed", "type::bug", "status::in_progress")
	ti := githubToTrackerIssue(gh)
	conv := tr.FieldMapper().IssueToBeads(&ti)
	if err := tr.AfterConvert(context.Background(), &ti, conv, "", existing, tracker.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if conv.Issue.IssueType != types.TypeBug {
		t.Errorf("type = %q, GitHub's change should win", conv.Issue.IssueType)
	}
	if conv.Issue.Status != types.StatusClosed {
		t.Errorf("status = %q, a closed issue closes the item", conv.Issue.Status)
	}
}

func TestAfterConvertRefusesACopyOfALinkedItem(t *testing.T) {
	tr := newPairedTracker("brain")
	ref := "https://github.com/acme/widgets/issues/3"
	existing := &types.Issue{ID: "brain-1", ExternalRef: &ref}
	// Issue 9 repeats brain-1's marker (a pasted body) but brain-1 is linked to issue 3.
	gh := ghIssue(9, "copy", WithMarker("x", "brain", "brain-1"), "open")
	ti := githubToTrackerIssue(gh)
	conv := tr.FieldMapper().IssueToBeads(&ti)
	if err := tr.AfterConvert(context.Background(), &ti, conv, "", existing, tracker.SyncOptions{}); err == nil {
		t.Fatal("a copy must not take over the link")
	}
}

// fakeGitHub serves the list endpoint and counts requests.
type fakeGitHub struct {
	mu     sync.Mutex
	issues []*Issue
	lists  int
	gets   int
}

func (f *fakeGitHub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets")
		switch {
		case path == "/issues" && r.Method == http.MethodGet:
			f.lists++
			_ = json.NewEncoder(w).Encode(f.issues)
		case strings.HasPrefix(path, "/issues/") && r.Method == http.MethodGet:
			f.gets++
			n, _ := strconv.Atoi(strings.TrimPrefix(path, "/issues/"))
			for _, gh := range f.issues {
				if gh.Number == n {
					_ = json.NewEncoder(w).Encode(gh)
					return
				}
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func trackerAgainst(t *testing.T, f *fakeGitHub, namespace string) *Tracker {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	tr := newPairedTracker(namespace)
	tr.client = NewClient("token", "acme", "widgets").WithBaseURL(srv.URL)
	return tr
}

func TestFindLinkedAdoptsTheIssueAMarkerNames(t *testing.T) {
	f := &fakeGitHub{issues: []*Issue{
		ghIssue(1, "native", "no marker", "open"),
		ghIssue(2, "mirror", WithMarker("x", "brain", "brain-7"), "open"),
		ghIssue(3, "other store", WithMarker("x", "task", "brain-8"), "open"),
		// A later copy of brain-7's issue: the original (lowest number) wins.
		ghIssue(4, "copy", WithMarker("x", "brain", "brain-7"), "open"),
	}}
	tr := trackerAgainst(t, f, "brain")
	ctx := context.Background()

	got, err := tr.FindLinked(ctx, &types.Issue{ID: "brain-7"})
	if err != nil || got == nil || got.Identifier != "2" {
		t.Fatalf("FindLinked(brain-7) = %+v, %v; want issue 2", got, err)
	}
	if got, _ := tr.FindLinked(ctx, &types.Issue{ID: "brain-8"}); got != nil {
		t.Errorf("brain-8's marker names another store, got issue %s", got.Identifier)
	}
	if got, _ := tr.FindLinked(ctx, &types.Issue{ID: "brain-99"}); got != nil {
		t.Errorf("brain-99 has no issue, got %s", got.Identifier)
	}
	if f.lists != 1 {
		t.Errorf("the repository should be listed once for all lookups, listed %d times", f.lists)
	}
}

func TestFindLinkedNeedsANamespace(t *testing.T) {
	f := &fakeGitHub{issues: []*Issue{ghIssue(1, "m", WithMarker("x", "", "brain-1"), "open")}}
	tr := trackerAgainst(t, f, "")
	if got, err := tr.FindLinked(context.Background(), &types.Issue{ID: "brain-1"}); got != nil || err != nil {
		t.Fatalf("without a namespace nothing can be matched, got %+v, %v", got, err)
	}
}

func TestBulkLookupAnswersFromOneListing(t *testing.T) {
	f := &fakeGitHub{issues: []*Issue{ghIssue(1, "a", "", "open"), ghIssue(2, "b", "", "closed")}}
	tr := trackerAgainst(t, f, "brain")
	tr.BulkLookup = true
	ctx := context.Background()
	for _, id := range []string{"1", "2", "1", "2"} {
		if ti, err := tr.FetchIssue(ctx, id); err != nil || ti == nil || ti.Identifier != id {
			t.Fatalf("FetchIssue(%s) = %+v, %v", id, ti, err)
		}
	}
	if f.lists != 1 || f.gets != 0 {
		t.Fatalf("lists=%d gets=%d, want one listing and no single lookups", f.lists, f.gets)
	}
	// An issue the listing does not hold (newer than it) falls back to a lookup.
	f.issues = append(f.issues, ghIssue(3, "c", "", "open"))
	if ti, err := tr.FetchIssue(ctx, "3"); err != nil || ti == nil || f.gets != 1 {
		t.Fatalf("fallback lookup: %+v, %v, gets=%d", ti, err, f.gets)
	}
}

func TestIncrementalFetchLooksBackButSkipsTrackedIssues(t *testing.T) {
	since := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	before := since.Add(-2 * time.Minute)
	after := since.Add(time.Minute)
	tracked := ghIssue(1, "tracked", WithMarker("x", "brain", "brain-1"), "open")
	tracked.UpdatedAt = &before
	unseen := ghIssue(2, "created just before the last sync", "native", "open")
	unseen.UpdatedAt = &before
	changed := ghIssue(3, "tracked and changed", WithMarker("x", "brain", "brain-3"), "open")
	changed.UpdatedAt = &after

	var gotSince string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSince = r.URL.Query().Get("since")
		_ = json.NewEncoder(w).Encode([]*Issue{tracked, unseen, changed})
	}))
	defer srv.Close()
	tr := newPairedTracker("brain")
	tr.client = NewClient("token", "acme", "widgets").WithBaseURL(srv.URL)
	tr.store = trackedStore{ids: map[string]bool{"brain-1": true, "brain-3": true}}

	issues, err := tr.FetchIssues(context.Background(), tracker.FetchOptions{Since: &since})
	if err != nil {
		t.Fatal(err)
	}
	if want := since.Add(-incrementalOverlap).Format(time.RFC3339); gotSince != want {
		t.Errorf("since = %q, want %q (the look-back)", gotSince, want)
	}
	var numbers []string
	for _, ti := range issues {
		numbers = append(numbers, ti.Identifier)
	}
	if strings.Join(numbers, ",") != "2,3" {
		t.Fatalf("issues = %v; want 2 (unseen, found by the look-back) and 3 (changed), not 1 (tracked, unchanged)", numbers)
	}
}

func TestCreateIssueClosesAnItemThatIsAlreadyClosed(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method)
		state := "open"
		if r.Method == http.MethodPatch {
			state = "closed"
		}
		_ = json.NewEncoder(w).Encode(&Issue{Number: 7, Title: "done", State: state, HTMLURL: "https://github.com/acme/widgets/issues/7"})
	}))
	defer srv.Close()
	tr := newPairedTracker("brain")
	tr.client = NewClient("token", "acme", "widgets").WithBaseURL(srv.URL)

	got, err := tr.CreateIssue(context.Background(), &types.Issue{ID: "brain-1", Title: "done", Status: types.StatusClosed})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "POST,PATCH" {
		t.Errorf("calls = %v, want a create then a close", calls)
	}
	if state, _ := got.State.(string); state != "closed" {
		t.Errorf("state = %v, want closed", got.State)
	}
}
