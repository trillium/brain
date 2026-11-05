package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/ui"
)

// resolveGitHubProject reads the project record named by --project and settles
// where its items go. A project run is always one way, brain to GitHub: it
// never reads the repository back into the store. The repository comes from the
// record; --repo may stand in for it only on a dry run, so a preview can name a
// repository the record does not name yet, but nothing is ever published
// anywhere the record does not say.
func resolveGitHubProject(ctx context.Context, config *GitHubConfig, pairing *GitHubPairing) (*projectScope, error) {
	if store == nil {
		return nil, fmt.Errorf("--project needs an open store")
	}
	scope, repoErr := loadProjectScope(ctx, newProjectSource(store), githubSyncProject, githubSyncProjectLabel)
	if scope == nil {
		return nil, repoErr
	}

	if githubSyncRepo != "" {
		owner, repo, err := parseGitHubRepository(githubSyncRepo)
		if err != nil {
			return nil, err
		}
		if repoErr != nil || !strings.EqualFold(scope.Repository(), owner+"/"+repo) {
			if !githubSyncDryRun {
				if repoErr != nil {
					return nil, fmt.Errorf("%v; --repo may only stand in for it on a --dry-run", repoErr)
				}
				return nil, fmt.Errorf("project %s names %s; --repo %s/%s may only be used with --dry-run (fix the record to send its items elsewhere)", scope.Record.ID, scope.Repository(), owner, repo)
			}
			scope.Owner, scope.Repo, scope.RepoSource = owner, repo, "--repo (not the record's own)"
		}
	} else if repoErr != nil {
		return nil, repoErr
	}

	if pairing.Recorded && !strings.EqualFold(pairing.Repository(), scope.Repository()) && !githubSyncDryRun {
		return nil, fmt.Errorf("this store is paired with %s; a project run into %s may only be a --dry-run (a store's items belong in one repository: 'github unpair' first)", pairing.Repository(), scope.Repository())
	}

	config.Owner, config.Repo = scope.Owner, scope.Repo
	pairing.Owner, pairing.Repo = scope.Owner, scope.Repo
	pairing.Direction = GitHubBrainToGitHub
	return scope, nil
}

// githubProjectPlan is the project scope applied to the connected store: which
// of its items belong to the project, and why.
type githubProjectPlan struct {
	Scope      *projectScope
	Members    []*types.Issue
	Reasons    map[string]string
	Visibility string // "public", "private" or "unknown"
	inScope    map[string]bool
	total      int
}

// ShouldPush is the push hook: only the project's items go.
func (p *githubProjectPlan) ShouldPush(issue *types.Issue) bool { return p.inScope[issue.ID] }

// planGitHubProject selects the project's items from the store, checks who can
// read the target repository, prints the list, and refuses a real run into a
// public repository unless --allow-public was given.
func planGitHubProject(ctx context.Context, setup *githubSetup, out io.Writer) (*githubProjectPlan, error) {
	issues, err := store.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return nil, fmt.Errorf("searching local issues: %w", err)
	}
	scope := setup.Project
	members, reasons := scope.Select(issues)
	plan := &githubProjectPlan{Scope: scope, Members: members, Reasons: reasons, Visibility: "unknown", inScope: map[string]bool{}, total: len(issues)}
	for _, m := range members {
		plan.inScope[m.ID] = true
	}

	if repo, err := getGitHubClient(setup.Config).GetRepository(ctx); err == nil {
		plan.Visibility = "private"
		if !repo.Private {
			plan.Visibility = "public"
		}
	}

	if !githubSyncDryRun && len(members) > 0 {
		switch {
		case plan.Visibility == "unknown":
			return nil, fmt.Errorf("cannot tell whether %s is public; refusing to publish %d items to it (run --dry-run to see the list)", scope.Repository(), len(members))
		case plan.Visibility == "public" && !githubSyncAllowPublic:
			return nil, fmt.Errorf("%s is a public repository: refusing to publish %d items from %s to it. Review the list with --dry-run, then pass --allow-public to publish", scope.Repository(), len(members), scope.Record.ID)
		}
	}

	if !jsonOutput {
		printGitHubProjectScope(out, plan)
	}
	return plan, nil
}

func printGitHubProjectScope(out io.Writer, plan *githubProjectPlan) {
	scope := plan.Scope
	_, _ = fmt.Fprintf(out, "Project scope: %s  %s\n", scope.Record.ID, ui.SanitizeForTerminal(scope.Record.Title))
	_, _ = fmt.Fprintf(out, "  repository: %s (from %s), %s\n", scope.Repository(), scope.RepoSource, plan.Visibility)
	_, _ = fmt.Fprintf(out, "  one way, brain to GitHub; an item belongs when it is under the record or carries one of: %s\n", strings.Join(scope.Labels, ", "))
	_, _ = fmt.Fprintf(out, "  %d of %d in this store belong to the project:\n", len(plan.Members), plan.total)
	for _, m := range plan.Members {
		_, _ = fmt.Fprintf(out, "    %s  [%s]  %s  (%s)\n", m.ID, m.Status, ui.SanitizeForTerminal(m.Title), plan.Reasons[m.ID])
	}
	_, _ = fmt.Fprintln(out)
}

// githubProjectSummary is the project part of the run summary.
type githubProjectSummary struct {
	ID         string               `json:"id"`
	Title      string               `json:"title"`
	Repo       string               `json:"repo"`
	RepoSource string               `json:"repo_source"`
	Visibility string               `json:"visibility"`
	Labels     []string             `json:"labels"`
	InScope    int                  `json:"in_scope"`
	StoreItems int                  `json:"store_items"`
	Items      []githubProjectEntry `json:"items"`
}

type githubProjectEntry struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// Annotate adds the project to the run summary. Everything outside the project
// is out of scope rather than skipped, so "unchanged" counts only its items.
func (p *githubProjectPlan) Annotate(sum *githubPlanSummary, result *tracker.SyncResult) {
	ps := &githubProjectSummary{
		ID:         p.Scope.Record.ID,
		Title:      p.Scope.Record.Title,
		Repo:       p.Scope.Repository(),
		RepoSource: p.Scope.RepoSource,
		Visibility: p.Visibility,
		Labels:     p.Scope.Labels,
		InScope:    len(p.Members),
		StoreItems: p.total,
		Items:      []githubProjectEntry{},
	}
	for _, m := range p.Members {
		ps.Items = append(ps.Items, githubProjectEntry{ID: m.ID, Title: m.Title, Status: string(m.Status), Reason: p.Reasons[m.ID]})
	}
	sum.Project = ps
	unchanged := len(p.Members) - result.PushStats.Created - result.PushStats.Updated - result.PushStats.Linked - result.PushStats.Errors
	if unchanged < 0 {
		unchanged = 0
	}
	sum.Skipped = unchanged
}
