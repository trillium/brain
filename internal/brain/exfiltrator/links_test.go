package exfiltrator_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/brain/exfiltrator"
	"github.com/steveyegge/beads/internal/types"
)

// fakeEdges is an in-memory EdgeSource.
type fakeEdges struct {
	issues map[string]*types.Issue
	deps   map[string][]*types.Dependency
	depErr error
}

func (f *fakeEdges) GetIssue(_ context.Context, id string) (*types.Issue, error) {
	if i, ok := f.issues[id]; ok {
		return i, nil
	}
	return nil, errors.New("not found")
}

func (f *fakeEdges) GetDependencyRecords(_ context.Context, id string) ([]*types.Dependency, error) {
	return f.deps[id], f.depErr
}

func linkedFixture(t *testing.T) (*types.Issue, *fakeEdges) {
	child := mustBrainIssue(t, "brain-child", "Child Task", types.TypeTask)
	parent := mustBrainIssue(t, "brain-epic", "The Epic", types.TypeEpic)
	dep := mustBrainIssue(t, "brain-dep", "Blocking Work", types.TypeTask)
	rel := mustBrainIssue(t, "brain-rel", "Related Thing", types.TypeTask)
	return child, &fakeEdges{
		issues: map[string]*types.Issue{"brain-epic": parent, "brain-dep": dep, "brain-rel": rel},
		deps: map[string][]*types.Dependency{
			"brain-child": {
				{IssueID: "brain-child", DependsOnID: "brain-epic", Type: types.DepParentChild},
				{IssueID: "brain-child", DependsOnID: "brain-dep", Type: types.DepBlocks},
				{IssueID: "brain-child", DependsOnID: "brain-rel", Type: types.DepRelated},
				{IssueID: "brain-child", DependsOnID: "external:other:thing", Type: types.DepBlocks},
			},
		},
	}
}

func renderLinked(t *testing.T, issue *types.Issue, src exfiltrator.EdgeSource, store string) string {
	t.Helper()
	exf := exfiltrator.NewMarkdownExfiltrator(t.TempDir(), nil).WithLinks(store, src)
	if err := exf.Render(context.Background(), issue); err != nil {
		t.Fatalf("Render: %v", err)
	}
	slug, _, _ := exf.SlugFor(issue)
	got, err := os.ReadFile(exf.PathFor(issue.IssueType, slug))
	if err != nil {
		t.Fatalf("read render: %v", err)
	}
	return string(got)
}

func TestRender_EmitsParentDependencyWikilinksAndStoreTag(t *testing.T) {
	child, src := linkedFixture(t)
	got := renderLinked(t, child, src, "brain")

	for _, want := range []string{
		"store: \"brain\"\n",
		"#store/brain\n",
		"- parent: [[the-epic]]\n",
		"- depends on: [[blocking-work]]\n",
		"- depends on (related): [[related-thing]]\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "external:") || strings.Contains(got, "other") {
		t.Errorf("unresolvable external dependency must not become a link\n---\n%s", got)
	}
	if !strings.HasSuffix(got, exfiltrator.LinksBlockEnd+"\n") {
		t.Errorf("links block must close the note\n---\n%s", got)
	}
}

func TestRender_LinkTargetsUseManifestSlug(t *testing.T) {
	// A target rendered with a collision-suffixed slug must be linked by the
	// slug its file actually carries, not the title-derived one.
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)
	parent := mustBrainIssue(t, "brain-epic", "The Epic", types.TypeEpic)
	parent.Metadata = []byte(`{"brain_slug":"the-epic-custom"}`)
	if err := exf.Render(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	child, src := linkedFixture(t)
	src.issues["brain-epic"] = parent
	exf.WithLinks("brain", src)
	if err := exf.Render(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	slug, _, _ := exf.SlugFor(child)
	got, _ := os.ReadFile(exf.PathFor(child.IssueType, slug))
	if !strings.Contains(string(got), "- parent: [[the-epic-custom]]\n") {
		t.Errorf("parent link should use the rendered slug\n---\n%s", got)
	}
}

func TestRender_NoLinksConfiguredIsUnchanged(t *testing.T) {
	child, _ := linkedFixture(t)
	plain := renderLinked(t, child, nil, "")
	if strings.Contains(plain, "store:") || strings.Contains(plain, exfiltrator.LinksBlockStart) {
		t.Errorf("render without links config must be unchanged\n---\n%s", plain)
	}
}

func TestRender_StoreTagWithoutEdges(t *testing.T) {
	child, _ := linkedFixture(t)
	got := renderLinked(t, child, &fakeEdges{}, "ideas")
	if !strings.Contains(got, "#store/ideas") || strings.Contains(got, "- parent:") || strings.Contains(got, "depends on") {
		t.Errorf("want store tag only\n---\n%s", got)
	}
}

func TestRender_LinksAreDeterministic(t *testing.T) {
	child, src := linkedFixture(t)
	a := renderLinked(t, child, src, "brain")
	src.deps["brain-child"][1], src.deps["brain-child"][2] = src.deps["brain-child"][2], src.deps["brain-child"][1]
	b := renderLinked(t, child, src, "brain")
	if a != b {
		t.Errorf("render must not depend on dependency record order\n%s\n---\n%s", a, b)
	}
}

func TestRender_DependencyLookupFailureDegradesToStoreTag(t *testing.T) {
	child, src := linkedFixture(t)
	src.depErr = errors.New("db down")
	got := renderLinked(t, child, src, "brain")
	if strings.Contains(got, "[[") || !strings.Contains(got, "#store/brain") {
		t.Errorf("lookup failure should render without edges\n---\n%s", got)
	}
}

func TestStripLinksBlock(t *testing.T) {
	child, src := linkedFixture(t)
	got := renderLinked(t, child, src, "brain")
	plain := renderLinked(t, child, nil, "")
	stripped := exfiltrator.StripLinksBlock(got)
	// The generated block is the only difference besides the store key.
	if strings.Contains(stripped, "[[") || strings.Contains(stripped, exfiltrator.LinksBlockStart) {
		t.Errorf("block not stripped:\n%s", stripped)
	}
	if !strings.HasSuffix(strings.TrimRight(plain, "\n"), strings.TrimRight(stripped[strings.Index(stripped, "# Child Task"):], "\n")) {
		t.Errorf("stripped body should equal the link-free body\nstripped=%q\nplain=%q", stripped, plain)
	}
	mid := "intro <!-- brain:links --> quoted\n\nmore text\n"
	if exfiltrator.StripLinksBlock(mid) != mid {
		t.Errorf("a marker quoted mid-description must be left alone")
	}
}
