package github

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

func init() {
	tracker.Register("github", func() tracker.IssueTracker {
		return &Tracker{}
	})
}

// incrementalOverlap is how far before the last sync an incremental pull starts reading.
const incrementalOverlap = 5 * time.Minute

// issueNumberPattern matches GitHub issue URLs: .../issues/42
var issueNumberPattern = regexp.MustCompile(`/issues/(\d+)`)

// ghShorthandPattern matches the "github:{digits}" shorthand produced by BuildExternalRef
// when a full URL is unavailable.
var ghShorthandPattern = regexp.MustCompile(`^github:([1-9]\d*)$`)

// Tracker implements tracker.IssueTracker for GitHub.
type Tracker struct {
	client *Client
	config *MappingConfig
	store  storage.Storage

	// Namespace names the brain store this tracker serves. Init fills it from
	// github.namespace, then BD_NAME, then the store's issue prefix, unless the
	// caller set it first.
	Namespace string

	// Owner and Repo, when both set before Init, name the repository directly
	// and take precedence over the configured github.* values.
	Owner string
	Repo  string

	owner string
	repo  string

	// BulkLookup makes FetchIssue answer from one listing of the whole
	// repository instead of one request per issue. A sync that checks every
	// linked item (a dry run, a push) sets it: a listing is a hundred issues
	// per request, a lookup is one.
	BulkLookup bool

	// index is the repository's issues, listed once per run: by issue number,
	// and by the brain item id named in their marker.
	index *issueIndex
}

type issueIndex struct {
	byNumber map[string]*tracker.TrackerIssue
	linked   map[string]*tracker.TrackerIssue
}

func (t *Tracker) Name() string         { return "github" }
func (t *Tracker) DisplayName() string  { return "GitHub" }
func (t *Tracker) ConfigPrefix() string { return "github" }

func (t *Tracker) Init(ctx context.Context, store storage.Storage) error {
	t.store = store

	token := t.getConfig(ctx, "github.token", "GITHUB_TOKEN")
	if token == "" {
		return fmt.Errorf("GitHub token not configured (set github.token or GITHUB_TOKEN)")
	}

	owner := t.getConfig(ctx, "github.owner", "GITHUB_OWNER")
	repo := t.getConfig(ctx, "github.repo", "GITHUB_REPO")
	if t.Owner != "" && t.Repo != "" {
		owner, repo = t.Owner, t.Repo
	}

	// Try combined owner/repo format: "owner/repo"
	if owner == "" || repo == "" {
		ownerRepo := t.getConfig(ctx, "github.repository", "GITHUB_REPOSITORY")
		if ownerRepo != "" {
			parts := strings.SplitN(ownerRepo, "/", 2)
			if len(parts) == 2 {
				owner = parts[0]
				repo = parts[1]
			}
		}
	}

	if owner == "" {
		return fmt.Errorf("GitHub owner not configured (set github.owner or GITHUB_OWNER)")
	}
	if repo == "" {
		return fmt.Errorf("GitHub repo not configured (set github.repo or GITHUB_REPO)")
	}

	t.client = NewClient(token, owner, repo)
	t.owner, t.repo = owner, repo

	// Allow custom base URL for GitHub Enterprise
	baseURL := t.getConfig(ctx, "github.url", "GITHUB_API_URL")
	if baseURL != "" {
		t.client = t.client.WithBaseURL(baseURL)
	}

	if t.Namespace == "" {
		t.Namespace = firstNonEmpty(
			t.getConfig(ctx, "github.namespace", ""),
			os.Getenv("BD_NAME"),
			t.getConfig(ctx, "issue_prefix", ""),
		)
	}
	t.config = DefaultMappingConfig()
	t.config.Namespace = t.Namespace
	t.index = nil
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func (t *Tracker) Validate() error {
	if t.client == nil {
		return fmt.Errorf("GitHub tracker not initialized")
	}
	return nil
}

func (t *Tracker) Close() error { return nil }

func (t *Tracker) FetchIssues(ctx context.Context, opts tracker.FetchOptions) ([]tracker.TrackerIssue, error) {
	var issues []Issue
	var err error

	state := opts.State
	if state == "" {
		state = "all"
	}

	if opts.Since != nil {
		// Look back past the last sync. GitHub's clock is not ours, and its
		// listing can lag a write by a moment, so an issue changed just
		// before the last sync could otherwise never be seen. Re-reading an
		// unchanged issue costs nothing: pull skips it.
		issues, err = t.client.FetchIssuesSince(ctx, state, opts.Since.Add(-incrementalOverlap))
		if err == nil {
			issues = t.dropSeen(ctx, issues, *opts.Since)
		}
	} else {
		issues, err = t.client.FetchIssues(ctx, state)
	}
	if err != nil {
		return nil, err
	}

	result := make([]tracker.TrackerIssue, 0, len(issues))
	for _, gh := range issues {
		result = append(result, githubToTrackerIssue(&gh))
	}
	return result, nil
}

// dropSeen removes, from the look-back window, the issues that this store
// already tracks and that have not changed since the last sync. The look-back
// exists to find issues the last sync could not see; re-applying an old copy
// of an issue the store already has would overwrite a brain edit made while
// the last sync was running.
func (t *Tracker) dropSeen(ctx context.Context, issues []Issue, since time.Time) []Issue {
	if t.store == nil {
		return issues
	}
	kept := issues[:0]
	for i := range issues {
		gh := &issues[i]
		if gh.UpdatedAt != nil && gh.UpdatedAt.After(since) {
			kept = append(kept, *gh)
			continue
		}
		if t.trackedLocally(ctx, gh) {
			continue
		}
		kept = append(kept, *gh)
	}
	return kept
}

// trackedLocally reports whether the store already has an item for this issue,
// by the issue's URL or by the item id in its marker.
func (t *Tracker) trackedLocally(ctx context.Context, gh *Issue) bool {
	if gh.HTMLURL != "" {
		if item, err := t.store.GetIssueByExternalRef(ctx, gh.HTMLURL); err == nil && item != nil {
			return true
		}
	}
	if marker, ok := ParseMarker(gh.Body); ok && t.config != nil && marker.Store == t.config.Namespace {
		if item, err := t.store.GetIssue(ctx, marker.ID); err == nil && item != nil {
			return true
		}
	}
	return false
}

func (t *Tracker) FetchIssue(ctx context.Context, identifier string) (*tracker.TrackerIssue, error) {
	number, err := strconv.Atoi(identifier)
	if err != nil {
		return nil, fmt.Errorf("invalid GitHub issue number %q: %w", identifier, err)
	}

	if t.BulkLookup {
		idx, err := t.loadIndex(ctx)
		if err != nil {
			return nil, err
		}
		if ti, ok := idx.byNumber[identifier]; ok {
			cp := *ti
			return &cp, nil
		}
	}

	gh, err := t.client.FetchIssueByNumber(ctx, number)
	if err != nil {
		return nil, err
	}
	if gh == nil {
		return nil, nil
	}

	ti := githubToTrackerIssue(gh)
	return &ti, nil
}

func (t *Tracker) CreateIssue(ctx context.Context, issue *types.Issue) (*tracker.TrackerIssue, error) {
	fields := BeadsIssueToGitHubFields(issue, t.config)
	labels, _ := fields["labels"].([]string)

	body, _ := fields["body"].(string)
	created, err := t.client.CreateIssue(ctx, issue.Title, body, labels)
	if err != nil {
		return nil, err
	}
	// GitHub opens every new issue, so an item that is already closed gets
	// its issue closed now rather than on the next sync.
	if fields["state"] == "closed" {
		if closed, err := t.client.UpdateIssue(ctx, created.Number, map[string]interface{}{"state": "closed"}); err == nil {
			created = closed
		}
	}

	ti := githubToTrackerIssue(created)
	t.remember(created)
	return &ti, nil
}

func (t *Tracker) UpdateIssue(ctx context.Context, externalID string, issue *types.Issue) (*tracker.TrackerIssue, error) {
	number, err := strconv.Atoi(externalID)
	if err != nil {
		return nil, fmt.Errorf("invalid GitHub issue number %q: %w", externalID, err)
	}

	updates := BeadsIssueToGitHubFields(issue, t.config)
	updated, err := t.client.UpdateIssue(ctx, number, updates)
	if err != nil {
		return nil, err
	}

	ti := githubToTrackerIssue(updated)
	t.remember(updated)
	return &ti, nil
}

func (t *Tracker) FieldMapper() tracker.FieldMapper {
	return &githubFieldMapper{config: t.config}
}

// issueURLPattern captures owner, repo and number from an issue URL on any
// host (github.com or an Enterprise server).
var issueURLPattern = regexp.MustCompile(`^https?://[^/]+/([^/]+)/([^/]+)/issues/(\d+)(?:[/?#].*)?$`)

// IsExternalRef checks if a ref is a link to an issue in THIS tracker's
// repository. It recognizes full GitHub URLs and the "github:{id}" shorthand
// produced by BuildExternalRef when a URL is unavailable.
//
// A URL for a different repository is not this tracker's ref: issue numbers
// only mean something inside one repository, so a link left over from a
// previous pairing must not be read as a number in the current one.
func (t *Tracker) IsExternalRef(ref string) bool {
	if ghShorthandPattern.MatchString(ref) {
		return true
	}
	if !strings.Contains(ref, "github") || !issueNumberPattern.MatchString(ref) {
		return false
	}
	if t.owner == "" || t.repo == "" {
		return true
	}
	m := issueURLPattern.FindStringSubmatch(ref)
	if m == nil {
		return false
	}
	return strings.EqualFold(m[1], t.owner) && strings.EqualFold(m[2], t.repo)
}

// ExtractIdentifier extracts the issue number from a GitHub URL or shorthand ref.
func (t *Tracker) ExtractIdentifier(ref string) string {
	if m := ghShorthandPattern.FindStringSubmatch(ref); len(m) >= 2 {
		return m[1]
	}
	matches := issueNumberPattern.FindStringSubmatch(ref)
	if len(matches) < 2 {
		return ""
	}
	return matches[1]
}

func (t *Tracker) BuildExternalRef(issue *tracker.TrackerIssue) string {
	if issue.URL != "" {
		return issue.URL
	}
	return fmt.Sprintf("github:%s", issue.Identifier)
}

// getConfig reads a config value from storage, falling back to env var.
// For yaml-only keys (e.g. github.token), reads from config.yaml first
// to match the behavior of cmd/bd/github.go:getGitHubConfigValue().
func (t *Tracker) getConfig(ctx context.Context, key, envVar string) string {
	// Secret keys are stored in config.yaml, not the Dolt database,
	// to avoid leaking secrets when pushing to remotes.
	if config.IsYamlOnlyKey(key) {
		if val := config.GetString(key); val != "" {
			return val
		}
		if envVar != "" {
			if envVal := os.Getenv(envVar); envVal != "" {
				return envVal
			}
		}
		return ""
	}

	val, err := t.store.GetConfig(ctx, key)
	if err == nil && val != "" {
		return val
	}
	if envVar != "" {
		if envVal := os.Getenv(envVar); envVal != "" {
			return envVal
		}
	}
	return ""
}

// githubToTrackerIssue converts a github.Issue to a tracker.TrackerIssue.
func githubToTrackerIssue(gh *Issue) tracker.TrackerIssue {
	ti := tracker.TrackerIssue{
		ID:          strconv.Itoa(gh.ID),
		Identifier:  strconv.Itoa(gh.Number),
		URL:         gh.HTMLURL,
		Title:       gh.Title,
		Description: StripMarker(gh.Body),
		Labels:      gh.LabelNames(),
		Raw:         gh,
	}

	if gh.State != "" {
		ti.State = gh.State
	}

	if gh.Assignee != nil {
		ti.Assignee = gh.Assignee.Login
		ti.AssigneeID = strconv.Itoa(gh.Assignee.ID)
	}

	if gh.CreatedAt != nil {
		ti.CreatedAt = *gh.CreatedAt
	}
	if gh.UpdatedAt != nil {
		ti.UpdatedAt = *gh.UpdatedAt
	}
	if gh.ClosedAt != nil {
		ti.CompletedAt = gh.ClosedAt
	}

	return ti
}
