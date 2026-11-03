package exfiltrator

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// This file owns the comment half of the render: a bead's comment trail,
// oldest first, in one generated block between the description and the links
// block. Comments are part of the durable record (bd show prints them), so
// the greppable copy carries them too. The block is delimited by
// CommentsBlockStart / CommentsBlockEnd so edit-back can strip it
// (StripCommentsBlock) instead of reading it as description text, and it is
// emitted only when the bead has comments, so comment-less files stay
// byte-identical to what they were before this block existed.

const (
	// CommentsBlockStart opens the generated comments block.
	CommentsBlockStart = "<!-- brain:comments -->"
	// CommentsBlockEnd closes it.
	CommentsBlockEnd = "<!-- /brain:comments -->"
)

// CommentSource is the optional read seam for a bead's comments. The
// EdgeSource passed to WithLinks is asked for it by type assertion, so
// DoltStorage satisfies it with no extra wiring. Without it (and without
// Issue.Comments already hydrated) the render carries no comments block.
type CommentSource interface {
	GetIssueComments(ctx context.Context, issueID string) ([]*types.Comment, error)
}

// commentsFor returns the comments to render for issue, oldest first.
// Hydrated Issue.Comments win; otherwise the comment source is read. A
// lookup failure degrades to "no comments" — the next render of the bead
// converges the file, the same posture linksFor takes.
func (m *MarkdownExfiltrator) commentsFor(ctx context.Context, issue *types.Issue) []*types.Comment {
	comments := issue.Comments
	if len(comments) == 0 {
		src, ok := m.edges.(CommentSource)
		if !ok || src == nil {
			return nil
		}
		var err error
		comments, err = src.GetIssueComments(ctx, issue.ID)
		if err != nil {
			return nil
		}
	}
	out := make([]*types.Comment, 0, len(comments))
	for _, c := range comments {
		if c != nil {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// markerEscaper keeps generated-block markers quoted inside comment text
// from reading as real block boundaries.
var markerEscaper = strings.NewReplacer(
	CommentsBlockStart, "&lt;!-- brain:comments -->",
	CommentsBlockEnd, "&lt;!-- /brain:comments -->",
	LinksBlockStart, "&lt;!-- brain:links -->",
	LinksBlockEnd, "&lt;!-- /brain:links -->",
)

// writeCommentsBlock appends the generated block to b. It writes nothing
// when there are no comments.
func writeCommentsBlock(b *strings.Builder, comments []*types.Comment) {
	if len(comments) == 0 {
		return
	}
	b.WriteByte('\n')
	b.WriteString(CommentsBlockStart)
	b.WriteString("\n## Comments\n")
	for _, c := range comments {
		author := strings.TrimSpace(c.Author)
		if author == "" {
			author = "unknown"
		}
		fmt.Fprintf(b, "\n### %s", markerEscaper.Replace(author))
		if !c.CreatedAt.IsZero() {
			fmt.Fprintf(b, " · %s", c.CreatedAt.UTC().Format(time.RFC3339))
		}
		b.WriteString("\n\n")
		text := markerEscaper.Replace(strings.TrimRight(c.Text, "\n"))
		b.WriteString(text)
		b.WriteByte('\n')
	}
	b.WriteString(CommentsBlockEnd)
	b.WriteByte('\n')
}

// StripCommentsBlock removes the generated comments block from the end of a
// body that has already had its links block stripped, returning body
// unchanged when it carries none. Edit-back calls it so comments never read
// as description text. Only a block that closes the body is stripped.
func StripCommentsBlock(body string) string {
	trimmed := strings.TrimRight(body, "\n")
	if !strings.HasSuffix(trimmed, CommentsBlockEnd) {
		return body
	}
	start := strings.LastIndex(trimmed, CommentsBlockStart)
	if start < 0 {
		return body
	}
	return strings.TrimRight(trimmed[:start], "\n")
}
