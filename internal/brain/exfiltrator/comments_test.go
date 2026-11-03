package exfiltrator_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/brain/exfiltrator"
	"github.com/steveyegge/beads/internal/types"
)

// commentEdges is an EdgeSource that is also a CommentSource.
type commentEdges struct {
	fakeEdges
	comments map[string][]*types.Comment
	err      error
}

func (c *commentEdges) GetIssueComments(_ context.Context, id string) ([]*types.Comment, error) {
	return c.comments[id], c.err
}

func at(h int) time.Time { return time.Date(2026, 6, 1, h, 0, 0, 0, time.UTC) }

func renderWith(t *testing.T, issue *types.Issue, src exfiltrator.EdgeSource) string {
	t.Helper()
	return renderLinked(t, issue, src, "")
}

func TestRender_CommentsBlock_OldestFirst(t *testing.T) {
	issue := mustBrainIssue(t, "brain-c1", "Has Comments", types.TypeTask)
	src := &commentEdges{comments: map[string][]*types.Comment{"brain-c1": {
		{Author: "bo", Text: "second", CreatedAt: at(14)},
		{Author: "al", Text: "first\nline two", CreatedAt: at(9)},
	}}}
	got := renderWith(t, issue, src)

	for _, want := range []string{
		exfiltrator.CommentsBlockStart, "## Comments",
		"### al · 2026-06-01T09:00:00Z", "first\nline two",
		"### bo · 2026-06-01T14:00:00Z", "second",
		exfiltrator.CommentsBlockEnd,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q\n---\n%s", want, got)
		}
	}
	if strings.Index(got, "### al") > strings.Index(got, "### bo") {
		t.Errorf("comments not oldest first\n---\n%s", got)
	}
	if strings.Index(got, "body for brain-c1") > strings.Index(got, exfiltrator.CommentsBlockStart) {
		t.Errorf("comments block should follow the description\n---\n%s", got)
	}
}

func TestRender_NoComments_ByteIdentical(t *testing.T) {
	issue := mustBrainIssue(t, "brain-c2", "No Comments", types.TypeTask)
	plain := renderWith(t, issue, nil)
	withSource := renderWith(t, issue, &commentEdges{})
	if plain != withSource {
		t.Errorf("a comment-less bead must render identically\nplain=%q\nwith=%q", plain, withSource)
	}
	if strings.Contains(withSource, "brain:comments") || strings.Contains(withSource, "## Comments") {
		t.Errorf("comment-less render carries a comments section:\n%s", withSource)
	}
}

func TestRender_AddingCommentChangesFile(t *testing.T) {
	issue := mustBrainIssue(t, "brain-c3", "Grows", types.TypeTask)
	src := &commentEdges{comments: map[string][]*types.Comment{}}
	exf := exfiltrator.NewMarkdownExfiltrator(t.TempDir(), nil).WithLinks("", src)
	read := func() string {
		t.Helper()
		if err := exf.Render(context.Background(), issue); err != nil {
			t.Fatalf("Render: %v", err)
		}
		slug, _, _ := exf.SlugFor(issue)
		b, err := os.ReadFile(exf.PathFor(issue.IssueType, slug))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	before := read()
	src.comments["brain-c3"] = []*types.Comment{{Author: "al", Text: "new note", CreatedAt: at(9)}}
	after := read()
	if before == after || !strings.Contains(after, "new note") {
		t.Errorf("render must change when a comment is added\nbefore=%q\nafter=%q", before, after)
	}
}

func TestRender_HydratedCommentsWithoutSource(t *testing.T) {
	issue := mustBrainIssue(t, "brain-c4", "Hydrated", types.TypeTask)
	issue.Comments = []*types.Comment{{Author: "al", Text: "carried on the issue", CreatedAt: at(9)}}
	got := renderWith(t, issue, nil)
	if !strings.Contains(got, "carried on the issue") {
		t.Errorf("hydrated comments not rendered:\n%s", got)
	}
}

func TestRender_CommentLookupFailureRendersWithout(t *testing.T) {
	issue := mustBrainIssue(t, "brain-c5", "Flaky", types.TypeTask)
	src := &commentEdges{err: errors.New("down"), comments: map[string][]*types.Comment{
		"brain-c5": {{Author: "al", Text: "x", CreatedAt: at(9)}},
	}}
	got := renderWith(t, issue, src)
	if strings.Contains(got, "brain:comments") {
		t.Errorf("lookup failure should render without comments:\n%s", got)
	}
}

func TestStripCommentsBlock_WithLinksBlock(t *testing.T) {
	child, base := linkedFixture(t)
	src := &commentEdges{fakeEdges: *base, comments: map[string][]*types.Comment{
		child.ID: {{Author: "al", Text: "note", CreatedAt: at(9)}},
	}}
	got := renderLinked(t, child, src, "brain")
	if !strings.Contains(got, exfiltrator.CommentsBlockStart) || !strings.Contains(got, "[[") {
		t.Fatalf("fixture should carry both blocks:\n%s", got)
	}
	stripped := exfiltrator.StripCommentsBlock(exfiltrator.StripLinksBlock(got))
	if strings.Contains(stripped, "note") || strings.Contains(stripped, "brain:") {
		t.Errorf("generated blocks not stripped:\n%s", stripped)
	}
	if !strings.HasSuffix(stripped, "body for brain-child") {
		t.Errorf("description should be what remains: %q", stripped)
	}
	mid := "intro <!-- brain:comments --> quoted\n\nmore text\n"
	if exfiltrator.StripCommentsBlock(mid) != mid {
		t.Errorf("a marker quoted mid-description must be left alone")
	}
}

func TestRender_CommentQuotingMarkerCannotForgeBlock(t *testing.T) {
	issue := mustBrainIssue(t, "brain-c6", "Quoter", types.TypeTask)
	src := &commentEdges{comments: map[string][]*types.Comment{"brain-c6": {
		{Author: "al", Text: "see " + exfiltrator.CommentsBlockStart + " here", CreatedAt: at(9)},
	}}}
	got := renderWith(t, issue, src)
	if strings.Count(got, exfiltrator.CommentsBlockStart) != 1 {
		t.Errorf("quoted marker must be neutralised:\n%s", got)
	}
	if s := exfiltrator.StripCommentsBlock(got); strings.Contains(s, "here") {
		t.Errorf("strip must remove the whole block:\n%s", s)
	}
}
