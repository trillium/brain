package editback

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/brain/exfiltrator"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// fakeStore records every interaction so tests can assert what the run
// wrote and never wrote.
type fakeStore struct {
	mu      sync.Mutex
	issues  map[string]*types.Issue
	updates []upCall
	adds    []labCall
	rems    []labCall
}

type upCall struct {
	ID      string
	Updates map[string]interface{}
	Actor   string
}

type labCall struct {
	ID    string
	Label string
	Actor string
}

func (f *fakeStore) GetIssue(_ context.Context, id string) (*types.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	issue, ok := f.issues[id]
	if !ok {
		return nil, fmt.Errorf("%w: issue %s", storageNotFound(), id)
	}
	return issue, nil
}

func (f *fakeStore) UpdateIssue(_ context.Context, id string, updates map[string]interface{}, actor string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	issue, ok := f.issues[id]
	if !ok {
		return fmt.Errorf("%w: issue %s", storageNotFound(), id)
	}
	copied := make(map[string]interface{}, len(updates))
	for k, v := range updates {
		copied[k] = v
	}
	f.updates = append(f.updates, upCall{ID: id, Updates: copied, Actor: actor})
	if v, ok := updates["title"].(string); ok && issue != nil {
		issue.Title = v
	}
	// More fields are only recorded, not applied — this store is a test
	// double, not the substrate. What matters is the calls it records.
	if s, ok := updates["status"].(string); ok {
		issue.Status = types.Status(s)
	}
	return nil
}

func (f *fakeStore) statusOf(issue *types.Issue) string {
	if issue == nil {
		return ""
	}
	return string(issue.Status)
}

func (f *fakeStore) AddLabel(_ context.Context, id, label, actor string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adds = append(f.adds, labCall{ID: id, Label: label, Actor: actor})
	issue, ok := f.issues[id]
	if ok {
		issue.Labels, _ = addLabelUnique(issue.Labels, label)
	}
	return nil
}

func (f *fakeStore) RemoveLabel(_ context.Context, id, label, actor string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rems = append(f.rems, labCall{ID: id, Label: label, Actor: actor})
	issue, ok := f.issues[id]
	if ok {
		issue.Labels = removeLabel(issue.Labels, label)
	}
	return nil
}

// updateField applies one scalar update to the double's row, so later
// diffs agree with a real substrate's post-write state.
func (f *fakeStore) updateField(id, field string, value interface{}) {
	issue, ok := f.issues[id]
	if !ok {
		return
	}
	switch field {
	case "title":
		issue.Title, _ = value.(string)
	case "description":
		issue.Description, _ = value.(string)
	case "status":
		issue.Status = types.Status(fmt.Sprintf("%v", value))
	case "priority":
		switch v := value.(type) {
		case int:
			issue.Priority = v
		case float64:
			issue.Priority = int(v)
		}
	}
}

func addLabelUnique(in []string, l string) ([]string, bool) {
	if slicesContains(in, l) {
		return in, false
	}
	return append(in, l), true
}

func removeLabel(in []string, l string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v != l {
			out = append(out, v)
		}
	}
	return out
}

func slicesContains(in []string, want string) bool {
	for _, v := range in {
		if v == want {
			return true
		}
	}
	return false
}

func storageNotFound() error { return storage.ErrNotFound }

func newIssue(id, title, kind string) *types.Issue {
	return &types.Issue{
		ID:        id,
		Title:     title,
		Status:    types.StatusOpen,
		Priority:  2,
		IssueType: types.IssueType(kind),
		Metadata:  []byte(fmt.Sprintf(`{"brain_slug": %q}`, testSlug(id))),
		CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
	}
}

func testSlug(id string) string { return "subject-for-" + id }

func ownedNamespace(id string) error {
	// Only ids "brain-" belong here; anything else is the refusal the
	// engine surfaces verbatim.
	if strings.HasPrefix(id, "brain-") {
		return nil
	}
	return fmt.Errorf("your own refusing authority: id %s carries a prefix this store does not own", id)
}

// renderFile writes a pre-rendered markdown file the way the exfiltrator
// does, so the tests exercise the same shape the renderer writes.
func renderFile(t *testing.T, st *fakeStore, root, id string, mutate func(got string) string) (path string) {
	t.Helper()
	issue, err := st.GetIssue(context.Background(), id)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "type: \"domain-concept\"\n")
	fmt.Fprintf(&b, "title: %s\n", yamlQuote(issue.Title))
	fmt.Fprintf(&b, "description: %s\n", yamlQuote(firstLine(issue.Description)))
	fmt.Fprintf(&b, "resource: %q\n", issue.ID)
	if len(issue.Labels) > 0 {
		items := make([]string, 0, len(issue.Labels))
		for _, l := range issue.Labels {
			items = append(items, yamlQuote(l))
		}
		fmt.Fprintf(&b, "tags: [%s]\n", strings.Join(items, ", "))
		fmt.Fprintf(&b, "labels: [%s]\n", strings.Join(items, ", "))
	}
	fmt.Fprintf(&b, "timestamp: %s\n", issue.UpdatedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "id: %s\n", issue.ID)
	fmt.Fprintf(&b, "kind: %s\n", issue.IssueType)
	fmt.Fprintf(&b, "status: %s\n", issue.Status)
	fmt.Fprintf(&b, "priority: %d\n", issue.Priority)
	fmt.Fprintf(&b, "created: %s\n", issue.CreatedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "updated: %s\n", issue.UpdatedAt.UTC().Format(time.RFC3339))
	if fmSlug := strings.TrimSpace(fmSlug(issue)); fmSlug != "" {
		fmt.Fprintf(&b, "slug: %s\n", yamlQuote(fmSlug))
	}
	b.WriteString("---\n\n")
	if issue.Title != "" {
		fmt.Fprintf(&b, "# %s\n\n", issue.Title)
	}
	b.WriteString(issue.Description)
	if !strings.HasSuffix(issue.Description, "\n") {
		b.WriteString("\n")
	}
	got := b.String()
	if mutate != nil {
		got = mutate(got)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, "entries", "knowledge", id+".md")), 0o755); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(root, "entries", "knowledge", testSlug(id)+".md")
	if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func fmSlug(issue *types.Issue) string { return testSlug(issue.ID) }

func firstLine(desc string) string {
	if desc == "" {
		return ""
	}
	return strings.Split(desc, "\n")[0]
}

func yamlQuote(s string) string {
	if s == "" {
		return `""`
	}
	return fmt.Sprintf("%q", s)
}

// seedManifest mirrors the render-phase manifest entry for a file the test
// creates by hand.
func seedManifest(t *testing.T, root, id, path string) {
	t.Helper()
	_ = os.MkdirAll(filepath.Join(root, "entries"), 0o755)
	data := fmt.Sprintf(`{
  "version": 1,
  "updated": "2026-10-09T00:00:00Z",
  "files": { %q: { "path": %q, "kind": "knowledge", "slug": %q, "rendered": "2026-10-09T00:00:00Z" } }
}`, id, path, testSlug(id))
	if err := os.WriteFile(filepath.Join(root, "entries", exfiltrator.ManifestFilename), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// outcomeMap indexes the report by kind prefix, for assertions.
func byKind(rep *Report, prefix string) []Outcome {
	var out []Outcome
	for _, oc := range rep.Outcomes {
		if strings.HasPrefix(oc.Kind, prefix) {
			out = append(out, oc)
		}
	}
	return out
}

func singleOutcome(t *testing.T, rep *Report, kind string) Outcome {
	t.Helper()
	got := byKind(rep, kind)
	if len(got) != 1 {
		t.Fatalf("expected exactly one %s outcome, got %d: %+v", kind, len(got), rep.Outcomes)
	}
	return got[0]
}
