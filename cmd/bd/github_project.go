package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// A project scope narrows a GitHub sync to the items that belong to one
// project record, and sends them to that project's own repository. It exists
// because a store holds far more than one project's work (and much that must
// never leave the machine), while a repository is public or shared and should
// only ever see its own project.
//
// brain keeps project records in the `projects` store. An item belongs to a
// project when either of these is true, and nothing else counts:
//
//  1. It sits under the project record in the parent-child hierarchy, at any
//     depth, in any store. The record itself is the scope, not an item.
//  2. It carries the label project:<id> for the record or for any record
//     beneath it (for example project:project-7u7w), or a label the record
//     opts in with its metadata key project_labels (for example
//     "project:brain"). Nothing is guessed from a title or a name.
//
// The target repository is read from the record, never from the store's
// pairing and never guessed. See resolveProjectRepository.

// projectLabelPrefix introduces a label that ties an item to a project.
const projectLabelPrefix = "project:"

// projectRecord is the part of a project record the scope reads.
type projectRecord struct {
	ID          string
	Title       string
	Description string
	ExternalRef string
	Labels      []string
	Metadata    map[string]any
}

// projectSource reads project records and their hierarchy. The record lives in
// the projects store, which is usually not the store being synced, so a source
// may look across stores.
type projectSource interface {
	// Record returns the project record, or an error when it does not exist.
	Record(ctx context.Context, id string) (*projectRecord, error)
	// Descendants returns the ids of every record beneath id through
	// parent-child edges, at any depth and in any store. id itself is not
	// included.
	Descendants(ctx context.Context, id string) ([]string, error)
}

// projectScope is the resolved answer to "which items belong to this project
// and where do they go".
type projectScope struct {
	Record *projectRecord
	// Owner and Repo are the target repository; RepoSource says where on the
	// record it was found.
	Owner, Repo, RepoSource string
	// Labels are the (lower-cased) labels that tie an item to the project.
	Labels []string
	// below holds the ids of every record under the project record.
	below map[string]bool
}

// Repository is the "owner/repo" form.
func (s *projectScope) Repository() string {
	if s.Owner == "" || s.Repo == "" {
		return ""
	}
	return s.Owner + "/" + s.Repo
}

// Reason says why an item belongs to the project, or returns "" when it does
// not. The reason is shown in the plan so the list can be audited.
func (s *projectScope) Reason(issue *types.Issue) string {
	if issue == nil || issue.ID == "" || issue.ID == s.Record.ID {
		return ""
	}
	if s.below[issue.ID] {
		return "under " + s.Record.ID
	}
	for _, have := range issue.Labels {
		have = strings.ToLower(strings.TrimSpace(have))
		for _, want := range s.Labels {
			if have == want {
				return "label " + want
			}
		}
	}
	return ""
}

// Select returns the issues that belong to the project, in the order given,
// with the reason for each. Ephemeral items and system-internal types are
// never selected: they never go to GitHub, scope or no scope.
func (s *projectScope) Select(issues []*types.Issue) (members []*types.Issue, reasons map[string]string) {
	reasons = map[string]string{}
	for _, issue := range issues {
		if issue == nil || issue.Ephemeral || isGitHubExcludedType(issue.IssueType) {
			continue
		}
		if reason := s.Reason(issue); reason != "" {
			members = append(members, issue)
			reasons[issue.ID] = reason
		}
	}
	return members, reasons
}

func isGitHubExcludedType(t types.IssueType) bool {
	for _, excluded := range githubExcludedTypes {
		if t == excluded {
			return true
		}
	}
	return false
}

// loadProjectScope reads the project record and its hierarchy and works out
// the repository. extraLabels are labels this run adds to the ones the record
// declares (project:<slug> style labels).
func loadProjectScope(ctx context.Context, src projectSource, projectID string, extraLabels []string) (*projectScope, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, fmt.Errorf("--project needs a project record id (for example project-73bi.1)")
	}
	rec, err := src.Record(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("project %s: %w", projectID, err)
	}
	below, err := src.Descendants(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("reading the records under %s: %w", rec.ID, err)
	}

	scope := &projectScope{Record: rec, below: map[string]bool{}}
	labels := map[string]bool{projectLabelPrefix + strings.ToLower(rec.ID): true}
	for _, id := range below {
		scope.below[id] = true
		labels[projectLabelPrefix+strings.ToLower(id)] = true
	}
	for _, l := range projectLabelsFromMetadata(rec.Metadata) {
		labels[normalizeProjectLabel(l)] = true
	}
	for _, l := range extraLabels {
		if l = strings.TrimSpace(l); l != "" {
			labels[normalizeProjectLabel(l)] = true
		}
	}
	for l := range labels {
		scope.Labels = append(scope.Labels, l)
	}
	sort.Strings(scope.Labels)

	scope.Owner, scope.Repo, scope.RepoSource, err = resolveProjectRepository(rec)
	if err != nil {
		// The scope is still usable for a dry run that names --repo.
		return scope, err
	}
	return scope, nil
}

// normalizeProjectLabel lower-cases a label and adds the project: prefix when
// it is missing, so "brain" and "project:brain" mean the same thing.
func normalizeProjectLabel(l string) string {
	l = strings.ToLower(strings.TrimSpace(l))
	if !strings.HasPrefix(l, projectLabelPrefix) {
		l = projectLabelPrefix + l
	}
	return l
}

// projectLabelsFromMetadata reads the labels a record opts in with: its
// metadata key project_labels, as a list or a comma-separated string.
func projectLabelsFromMetadata(meta map[string]any) []string {
	var out []string
	switch v := meta["project_labels"].(type) {
	case string:
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	}
	return out
}

var (
	// githubOriginRe finds the "origin github.com/owner/repo" sentence the
	// repository scan writes into a project record's description.
	githubOriginRe = regexp.MustCompile(`(?i)\borigin[:=]?\s+(?:https?://)?github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)`)
	githubURLRe    = regexp.MustCompile(`(?i)^(?:https?://)?github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)`)
)

// resolveProjectRepository finds the GitHub repository a project record names.
// It looks, in this order, and takes the first place that names one:
//
//  1. metadata key "repo" (or "github_repo"), as owner/repo or a github.com URL
//  2. a label repo:owner/name
//  3. the record's external_ref when it is a github.com URL
//  4. an "origin github.com/owner/repo" line in the description
//
// A value that is not a GitHub repository (a local checkout path, a bare name)
// is skipped, not guessed at. When the first place that names a repository
// names two different ones, that is an error rather than a coin flip. A record
// that names none is an error that says where to put one.
func resolveProjectRepository(rec *projectRecord) (owner, repo, source string, err error) {
	type candidate struct{ source, value string }
	tiers := [][]candidate{}

	var meta []candidate
	for _, key := range []string{"repo", "github_repo"} {
		if s, ok := rec.Metadata[key].(string); ok && strings.TrimSpace(s) != "" {
			meta = append(meta, candidate{"metadata " + key, s})
		}
	}
	tiers = append(tiers, meta)

	var labels []candidate
	for _, l := range rec.Labels {
		if strings.HasPrefix(strings.ToLower(l), "repo:") {
			labels = append(labels, candidate{"label " + l, l[len("repo:"):]})
		}
	}
	tiers = append(tiers, labels)

	if rec.ExternalRef != "" {
		tiers = append(tiers, []candidate{{"external_ref", rec.ExternalRef}})
	}

	var origin []candidate
	for _, m := range githubOriginRe.FindAllStringSubmatch(rec.Description, -1) {
		origin = append(origin, candidate{"description (origin line)", m[1]})
	}
	tiers = append(tiers, origin)

	var skipped []string
	for _, tier := range tiers {
		found := map[string]string{}
		for _, c := range tier {
			o, r, ok := githubRepoFromValue(c.value)
			if !ok {
				skipped = append(skipped, fmt.Sprintf("%s %q", c.source, c.value))
				continue
			}
			found[strings.ToLower(o+"/"+r)] = c.source
			owner, repo, source = o, r, c.source
		}
		switch len(found) {
		case 0:
			continue
		case 1:
			return owner, repo, source, nil
		default:
			names := make([]string, 0, len(found))
			for n := range found {
				names = append(names, n)
			}
			sort.Strings(names)
			return "", "", "", fmt.Errorf("project %s names more than one repository (%s); keep one", rec.ID, strings.Join(names, ", "))
		}
	}

	msg := fmt.Sprintf("project %s names no GitHub repository. Record one on the project, for example: projects update %s --set-metadata repo=owner/name", rec.ID, rec.ID)
	if len(skipped) > 0 {
		msg += fmt.Sprintf(" (ignored, not a GitHub repository: %s)", strings.Join(skipped, "; "))
	}
	return "", "", "", fmt.Errorf("%s", msg)
}

// githubRepoFromValue reads owner/repo from a repository value. Only a form
// that is unambiguously a GitHub repository is accepted: "owner/name", or a
// github.com URL. A filesystem path is not.
func githubRepoFromValue(v string) (owner, repo string, ok bool) {
	v = strings.TrimSpace(v)
	if v == "" || strings.HasPrefix(v, "/") || strings.HasPrefix(v, "~") || strings.HasPrefix(v, ".") {
		return "", "", false
	}
	if m := githubURLRe.FindStringSubmatch(v); m != nil {
		v = m[1]
	} else if strings.Contains(v, "://") || strings.HasPrefix(strings.ToLower(v), "git@") {
		return "", "", false
	}
	o, r, err := parseGitHubRepository(v)
	if err != nil || strings.ContainsAny(o+r, `\`) {
		return "", "", false
	}
	// "/Users/x/code/brain" style paths with a stray owner segment are not
	// repositories; GitHub names are letters, digits, '.', '-' and '_'.
	if !githubNameRe.MatchString(o) || !githubNameRe.MatchString(r) {
		return "", "", false
	}
	return o, r, true
}

var githubNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// storeProjectSource reads project records from the connected store alone. It
// is what a single-store setup (and the embedded engine) has.
type storeProjectSource struct{ st storage.DoltStorage }

func (s storeProjectSource) Record(ctx context.Context, id string) (*projectRecord, error) {
	issue, err := s.st.GetIssue(ctx, id)
	if err != nil || issue == nil {
		return nil, fmt.Errorf("no such record in this store (%v)", orNoneLabel(errString(err)))
	}
	labels, _ := s.st.GetLabels(ctx, issue.ID)
	return recordFromIssue(issue, labels), nil
}

func (s storeProjectSource) Descendants(ctx context.Context, id string) ([]string, error) {
	seen := map[string]bool{id: true}
	var out []string
	queue := []string{id}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		dependents, err := s.st.GetDependentsWithMetadata(ctx, current)
		if err != nil {
			return nil, err
		}
		for _, d := range dependents {
			if d.DependencyType == types.DepParentChild && !seen[d.ID] {
				seen[d.ID] = true
				out = append(out, d.ID)
				queue = append(queue, d.ID)
			}
		}
	}
	return out, nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func recordFromIssue(issue *types.Issue, labels []string) *projectRecord {
	rec := &projectRecord{
		ID:          issue.ID,
		Title:       issue.Title,
		Description: issue.Description,
		Labels:      labels,
	}
	if issue.ExternalRef != nil {
		rec.ExternalRef = *issue.ExternalRef
	}
	if len(issue.Metadata) > 0 {
		_ = json.Unmarshal(issue.Metadata, &rec.Metadata)
	}
	return rec
}

// sqlProjectSource reads project records straight from the shared database, so
// a record in the projects store can scope a sync of any other store. Item ids
// are unique across the database (each carries its store's prefix), so a plain
// id lookup finds the record whichever store it lives in.
type sqlProjectSource struct{ db *sql.DB }

func (s sqlProjectSource) Record(ctx context.Context, id string) (*projectRecord, error) {
	var (
		rec         = &projectRecord{ID: id}
		description sql.NullString
		externalRef sql.NullString
		metadata    sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		"SELECT id, title, description, external_ref, metadata FROM issues WHERE id = ?", id,
	).Scan(&rec.ID, &rec.Title, &description, &externalRef, &metadata)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no such record")
	}
	if err != nil {
		return nil, err
	}
	rec.Description, rec.ExternalRef = description.String, externalRef.String
	if metadata.Valid && metadata.String != "" {
		_ = json.Unmarshal([]byte(metadata.String), &rec.Metadata)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT label FROM labels WHERE issue_id = ?", id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		rec.Labels = append(rec.Labels, l)
	}
	return rec, rows.Err()
}

func (s sqlProjectSource) Descendants(ctx context.Context, id string) ([]string, error) {
	seen := map[string]bool{id: true}
	var out []string
	queue := []string{id}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		rows, err := s.db.QueryContext(ctx,
			"SELECT issue_id FROM dependencies WHERE depends_on_issue_id = ? AND type = ?", current, string(types.DepParentChild))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var child string
			if err := rows.Scan(&child); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if !seen[child] {
				seen[child] = true
				out = append(out, child)
				queue = append(queue, child)
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// newProjectSource picks the widest reader the connected store allows: the
// shared database when it can be reached directly, else the store alone.
func newProjectSource(st storage.DoltStorage) projectSource {
	if accessor, ok := storage.UnwrapStore(st).(storage.RawDBAccessor); ok {
		if db := accessor.UnderlyingDB(); db != nil {
			return sqlProjectSource{db: db}
		}
	}
	return storeProjectSource{st: st}
}
