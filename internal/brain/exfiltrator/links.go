package exfiltrator

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/types"
)

// This file owns the graph-edge half of the render: dependency and parent
// [[wikilinks]] plus a store tag, so Obsidian and Quartz graphs connect the
// rendered notes. Everything it emits lives in one generated block at the
// very end of the note, delimited by LinksBlockStart / LinksBlockEnd, so the
// edit-back run can strip it (StripLinksBlock) instead of reading it as a
// description edit. The store tag is deliberately NOT written into the
// frontmatter `tags`/`labels` lists: edit-back treats those as the bead's
// labels, and a render-owned tag there would round-trip into a real label.

const (
	// LinksBlockStart opens the generated links block at the end of a note.
	LinksBlockStart = "<!-- brain:links -->"
	// LinksBlockEnd closes it.
	LinksBlockEnd = "<!-- /brain:links -->"
)

// EdgeSource is the read seam the exfiltrator uses to turn a bead's
// dependency records into links. DoltStorage satisfies it directly; tests
// pass a fake. Nil leaves notes without edge links.
type EdgeSource interface {
	GetIssue(ctx context.Context, id string) (*types.Issue, error)
	GetDependencyRecords(ctx context.Context, issueID string) ([]*types.Dependency, error)
}

// WithLinks turns on edge links and the store tag. store names the store
// the notes belong to (empty omits the tag); src resolves dependency
// targets (nil omits the edge links). Returns m for chaining.
func (m *MarkdownExfiltrator) WithLinks(store string, src EdgeSource) *MarkdownExfiltrator {
	m.store = store
	m.edges = src
	return m
}

// noteLinks is everything the generated block and the `store` frontmatter
// key are rendered from. The zero value renders nothing.
type noteLinks struct {
	store   string
	parents []string
	deps    []linkTarget
}

// linkTarget is one outgoing non-parent edge: the target note's name and the
// dependency type that produced it.
type linkTarget struct {
	slug string
	typ  types.DependencyType
}

func (l noteLinks) empty() bool {
	return l.store == "" && len(l.parents) == 0 && len(l.deps) == 0
}

// nonTagRe matches characters an inline tag cannot carry.
var nonTagRe = regexp.MustCompile(`[^A-Za-z0-9_\-/]+`)

// linksFor resolves issue's outgoing edges to note names. Targets that do
// not resolve to a bead (external refs, other stores' ids, deleted rows) are
// skipped: a wikilink to no note is a dangling node in the graph, not an
// edge. Lookup failures degrade to "no links" — the next render of the bead
// converges the file, the same posture the decorator takes for render
// failures.
func (m *MarkdownExfiltrator) linksFor(ctx context.Context, issue *types.Issue) noteLinks {
	links := noteLinks{store: m.store}
	if m.edges == nil {
		return links
	}
	records, err := m.edges.GetDependencyRecords(ctx, issue.ID)
	if err != nil || len(records) == 0 {
		return links
	}

	// The manifest is the authority on which slug a bead's file actually
	// carries (it includes collision suffixes); read it once for all targets.
	m.mu.Lock()
	mf, _ := m.readManifestLocked()
	m.mu.Unlock()

	seenParent := map[string]bool{}
	seenDep := map[string]bool{}
	for _, rec := range records {
		if rec == nil || rec.IssueID != issue.ID || rec.DependsOnID == "" || rec.DependsOnID == issue.ID {
			continue
		}
		slug := m.targetSlug(ctx, mf, rec.DependsOnID)
		if slug == "" {
			continue
		}
		if rec.Type == types.DepParentChild {
			if !seenParent[slug] {
				seenParent[slug] = true
				links.parents = append(links.parents, slug)
			}
			continue
		}
		key := slug + "\x00" + string(rec.Type)
		if !seenDep[key] {
			seenDep[key] = true
			links.deps = append(links.deps, linkTarget{slug: slug, typ: rec.Type})
		}
	}
	sort.Strings(links.parents)
	sort.Slice(links.deps, func(i, j int) bool {
		if links.deps[i].slug != links.deps[j].slug {
			return links.deps[i].slug < links.deps[j].slug
		}
		return links.deps[i].typ < links.deps[j].typ
	})
	return links
}

// targetSlug returns the note name for bead id, or "" when id is not a bead
// this store can name. Order: the manifest's recorded slug (what is on
// disk), then the target's persisted brain_slug, then the slug a first render
// would derive from its title.
func (m *MarkdownExfiltrator) targetSlug(ctx context.Context, mf *ManifestFile, id string) string {
	if mf != nil {
		if e, ok := mf.Files[id]; ok && e.Slug != "" {
			return e.Slug
		}
	}
	target, err := m.edges.GetIssue(ctx, id)
	if err != nil || target == nil {
		return ""
	}
	if s := metadataSlug(target.Metadata); s != "" {
		return s
	}
	if s := kebab(target.Title); s != "" {
		return s
	}
	return kebab(target.ID)
}

// storeTag renders the store name as an Obsidian/Quartz inline tag.
func storeTag(store string) string {
	return "#store/" + nonTagRe.ReplaceAllString(store, "-")
}

// writeLinksBlock appends the generated block to b. It writes nothing when
// there is nothing to say.
func writeLinksBlock(b *strings.Builder, l noteLinks) {
	if l.empty() {
		return
	}
	b.WriteByte('\n')
	b.WriteString(LinksBlockStart)
	b.WriteByte('\n')
	if l.store != "" {
		b.WriteString(storeTag(l.store))
		b.WriteByte('\n')
	}
	if len(l.parents) > 0 || len(l.deps) > 0 {
		b.WriteByte('\n')
	}
	for _, p := range l.parents {
		fmt.Fprintf(b, "- parent: [[%s]]\n", p)
	}
	for _, d := range l.deps {
		if d.typ == "" || d.typ == types.DepBlocks {
			fmt.Fprintf(b, "- depends on: [[%s]]\n", d.slug)
		} else {
			fmt.Fprintf(b, "- depends on (%s): [[%s]]\n", d.typ, d.slug)
		}
	}
	b.WriteString(LinksBlockEnd)
	b.WriteByte('\n')
}

// StripLinksBlock removes the generated links block from the end of a
// rendered body, returning body unchanged when it carries none. Edit-back
// calls it so the block never reads as description text. Only a block that
// closes the body is stripped: a marker quoted mid-description is the
// author's text.
func StripLinksBlock(body string) string {
	trimmed := strings.TrimRight(body, "\n")
	if !strings.HasSuffix(trimmed, LinksBlockEnd) {
		return body
	}
	start := strings.LastIndex(trimmed, LinksBlockStart)
	if start < 0 {
		return body
	}
	return strings.TrimRight(trimmed[:start], "\n")
}
