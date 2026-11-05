package github

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

// loadIndex lists the repository's issues once and indexes them.
func (t *Tracker) loadIndex(ctx context.Context) (*issueIndex, error) {
	if t.index != nil {
		return t.index, nil
	}
	all, err := t.client.FetchIssues(ctx, "all")
	if err != nil {
		return nil, fmt.Errorf("listing GitHub issues: %w", err)
	}
	idx := &issueIndex{
		byNumber: make(map[string]*tracker.TrackerIssue, len(all)),
		linked:   make(map[string]*tracker.TrackerIssue),
	}
	for i := range all {
		ti := githubToTrackerIssue(&all[i])
		idx.byNumber[ti.Identifier] = &ti
		idx.addLinked(&all[i], &ti, t.namespace())
	}
	t.index = idx
	return idx, nil
}

func (t *Tracker) namespace() string {
	if t.config == nil {
		return ""
	}
	return t.config.Namespace
}

// addLinked records ti under the item id its marker names, when the marker is
// this store's. Two issues naming one item: the lowest number is the original,
// later ones are copies.
func (idx *issueIndex) addLinked(gh *Issue, ti *tracker.TrackerIssue, namespace string) {
	marker, ok := ParseMarker(gh.Body)
	if !ok || namespace == "" || marker.Store != namespace {
		return
	}
	if prev, dup := idx.linked[marker.ID]; dup && gh.Number > atoiOrZero(prev.Identifier) {
		return
	}
	idx.linked[marker.ID] = ti
}

// remember keeps the index current with an issue this run just wrote.
func (t *Tracker) remember(gh *Issue) {
	if t.index == nil || gh == nil {
		return
	}
	ti := githubToTrackerIssue(gh)
	t.index.byNumber[ti.Identifier] = &ti
	t.index.addLinked(gh, &ti, t.namespace())
}

// FindLinked looks for the issue that already mirrors an item whose
// external_ref is missing: the issue whose marker names this store and this
// item id. Push calls it before creating, so a lost link is repaired instead
// of duplicated. It returns nil, nil when no issue mirrors the item.
func (t *Tracker) FindLinked(ctx context.Context, issue *types.Issue) (*tracker.TrackerIssue, error) {
	if t.config == nil || t.config.Namespace == "" || issue == nil || issue.ID == "" {
		return nil, nil
	}
	idx, err := t.loadIndex(ctx)
	if err != nil {
		return nil, err
	}
	return idx.linked[issue.ID], nil
}

func atoiOrZero(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// ContentEqual reports whether a GitHub issue already says what the item says,
// so push can skip an update that would change nothing. Without it every push
// would rewrite every issue, and each rewrite would bump the issue's
// updated_at and look like a fresh remote edit to the next pull.
func (t *Tracker) ContentEqual(local *types.Issue, remote *tracker.TrackerIssue) bool {
	if local == nil || remote == nil {
		return false
	}
	want := BeadsIssueToGitHubFields(local, t.config)
	if want["title"] != remote.Title {
		return false
	}
	if normalizeBody(StripMarker(local.Description)) != normalizeBody(remote.Description) {
		return false
	}
	state, _ := remote.State.(string)
	if state != "" && want["state"] != state {
		return false
	}
	wantLabels, _ := want["labels"].([]string)
	if !sameStringSet(wantLabels, remote.Labels) {
		return false
	}
	// A body without the item's marker is an issue that is not yet linked by
	// marker: update it once so the marker lands.
	if gh, ok := remote.Raw.(*Issue); ok && t.config != nil && t.config.Namespace != "" && local.ID != "" {
		if marker, found := ParseMarker(gh.Body); !found || marker.Store != t.config.Namespace || marker.ID != local.ID {
			return false
		}
	}
	return true
}

func normalizeBody(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.TrimRight(s, "\r\n \t")
}

func sameStringSet(a, b []string) bool {
	norm := func(in []string) []string {
		out := make([]string, 0, len(in))
		seen := map[string]bool{}
		for _, v := range in {
			v = strings.TrimSpace(v)
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
		sort.Strings(out)
		return out
	}
	an, bn := norm(a), norm(b)
	if len(an) != len(bn) {
		return false
	}
	for i := range an {
		if an[i] != bn[i] {
			return false
		}
	}
	return true
}

// ShouldImport keeps a store from importing another store's mirrors. A
// repository two stores push into holds issues whose markers name each
// store; importing them would copy one store's items into the other.
func (t *Tracker) ShouldImport(ti *tracker.TrackerIssue) bool {
	gh, ok := ti.Raw.(*Issue)
	if !ok || t.config == nil || t.config.Namespace == "" {
		return true
	}
	marker, found := ParseMarker(gh.Body)
	return !found || marker.Store == t.config.Namespace
}

// AfterConvert keeps what GitHub cannot say. It runs on every pulled issue
// that already has a brain item and carries the item's own values forward
// where the issue has none of its own, so pulling never turns an assignee, a
// pinned status or an item type GitHub has no word for into a default.
//
// It also refuses an issue that names an item already linked to a different
// issue: that is a copy (someone pasted a body into a new issue), and
// relinking the item to it would silently move the link.
func (t *Tracker) AfterConvert(_ context.Context, ti *tracker.TrackerIssue, conv *tracker.IssueConversion, _ string, existing *types.Issue, _ tracker.SyncOptions) error {
	if existing == nil || conv == nil || conv.Issue == nil {
		return nil
	}
	if existing.ExternalRef != nil {
		if ref := strings.TrimSpace(*existing.ExternalRef); ref != "" && t.IsExternalRef(ref) {
			if linked := t.ExtractIdentifier(ref); linked != "" && linked != ti.Identifier {
				return fmt.Errorf("issue #%s repeats the marker of %s, which is linked to issue #%s; leaving the link alone", ti.Identifier, existing.ID, linked)
			}
		}
	}

	out := conv.Issue
	if out.Assignee == "" {
		out.Assignee = existing.Assignee
	}
	if normalizeBody(out.Description) == normalizeBody(existing.Description) {
		out.Description = existing.Description
	}
	gh, ok := ti.Raw.(*Issue)
	if !ok {
		return nil
	}
	labels := gh.LabelNames()
	if hasLabel(labels, "type::"+string(existing.IssueType)) {
		out.IssueType = existing.IssueType
	}
	if gh.State != "closed" && existing.Status != types.StatusClosed && hasLabel(labels, "status::"+string(existing.Status)) {
		out.Status = existing.Status
	}
	return nil
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, want) {
			return true
		}
	}
	return false
}
