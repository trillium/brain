// Package main provides the bd CLI commands.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/github"
	"github.com/steveyegge/beads/internal/metrics"
	"github.com/steveyegge/beads/internal/tracker"
	"github.com/steveyegge/beads/internal/types"
)

// GitHubConfig holds GitHub connection configuration.
type GitHubConfig struct {
	Token      string // Personal access token
	Owner      string // Repository owner (user or organization)
	Repo       string // Repository name
	Repository string // Combined "owner/repo" format
	URL        string // Custom API URL (for GitHub Enterprise)
}

// githubCmd is the root command for GitHub operations.
var githubCmd = &cobra.Command{
	Use:   "github",
	Short: "GitHub integration commands",
	Long: `Commands for syncing a brain store with the issues of a GitHub repository.

A store is paired with one repository, and the pairing names the direction
changes travel (see 'github pair'):

  two-way          items and issues are kept in step both ways (default)
  brain-to-github  brain is the source; GitHub issues mirror it
  github-to-brain  GitHub is the source; brain items mirror it

Configuration can be set via 'bd config' or environment variables:
  github.token / GITHUB_TOKEN           - Personal access token
  github.owner / GITHUB_OWNER           - Repository owner
  github.repo / GITHUB_REPO             - Repository name
  github.repository / GITHUB_REPOSITORY - Combined "owner/repo" format
  github.url / GITHUB_API_URL           - Custom API URL (GitHub Enterprise)

With no token configured, 'gh auth token' (the GitHub CLI's login) is used.`,
}

// githubSyncCmd synchronizes issues between beads and GitHub.
var githubSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync this store's items with GitHub issues",
	Long: `Synchronize this store's items with the issues of its paired repository.

The direction comes from the store's pairing ('github pair'). A store with no
recorded direction syncs two-way:
- Pulls new/updated issues from GitHub into brain
- Pushes brain items to GitHub

An item and its issue stay linked: the item remembers the issue's URL, and the
issue carries a hidden marker naming the item, so running sync again never
creates a second copy of either. Closing or reopening on one side is carried
to the other. Comments are not synced.

On a two-way pairing, --pull-only or --push-only narrows a single run to one
half. On a one-way pairing the flag for the opposite half is refused.

If an item and its issue were both changed since the last sync, the conflict
policy decides: --prefer-newer (default) keeps whichever side was edited last,
--prefer-local keeps brain's version, --prefer-github keeps GitHub's. The
policy can be recorded on the pairing with 'github pair --conflict'.

Use --dry-run first: it prints what each side would create, update or link,
with counts, and changes nothing.

With --project <project-id> the run is limited to the items that belong to one
project record (kept in the projects store) and goes one way, brain to GitHub,
into the repository that record names. An item belongs when it is under the
record or carries the label project:<id>; nothing else is sent. The plan lists
every item with the reason it is included. A real run into a public repository
needs --allow-public.

With --watch the command stays running and syncs every --interval.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runGitHubSync,
}

// githubStatusCmd displays GitHub configuration and sync status.
var githubStatusCmd = &cobra.Command{
	Use:           "status",
	Short:         "Show GitHub sync status",
	Long:          `Display current GitHub configuration and sync status.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runGitHubStatus,
}

// githubReposCmd lists accessible GitHub repositories.
var githubReposCmd = &cobra.Command{
	Use:           "repos",
	Short:         "List accessible GitHub repositories",
	Long:          `List GitHub repositories that the configured token has access to.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runGitHubRepos,
}

var (
	githubSyncDryRun   bool
	githubSyncPullOnly bool
	githubSyncPushOnly bool
	githubPreferLocal  bool
	githubPreferGitHub bool
	githubPreferNewer  bool

	githubSyncWatch    bool
	githubSyncInterval time.Duration
	githubSyncRepo     string
	githubSyncDir      string

	githubSyncProject      string
	githubSyncProjectLabel []string
	githubSyncAllowPublic  bool

	githubPairDirection string
	githubPairConflict  string
	githubPairForce     bool
	githubPairNoVerify  bool
)

// GitHubConflictStrategy defines how to resolve conflicts between local and GitHub versions.
type GitHubConflictStrategy string

const (
	// GitHubConflictPreferNewer uses the most recently updated version (default).
	GitHubConflictPreferNewer GitHubConflictStrategy = "prefer-newer"
	// GitHubConflictPreferLocal always keeps the local beads version.
	GitHubConflictPreferLocal GitHubConflictStrategy = "prefer-local"
	// GitHubConflictPreferGitHub always uses the GitHub version.
	GitHubConflictPreferGitHub GitHubConflictStrategy = "prefer-github"
)

// getGitHubConflictStrategy determines the conflict strategy from flag values.
// Returns error if multiple conflicting flags are set.
func getGitHubConflictStrategy(preferLocal, preferGitHub, preferNewer bool) (GitHubConflictStrategy, error) {
	flagsSet := 0
	if preferLocal {
		flagsSet++
	}
	if preferGitHub {
		flagsSet++
	}
	if preferNewer {
		flagsSet++
	}
	if flagsSet > 1 {
		return "", fmt.Errorf("cannot use multiple conflict resolution flags")
	}

	if preferLocal {
		return GitHubConflictPreferLocal, nil
	}
	if preferGitHub {
		return GitHubConflictPreferGitHub, nil
	}
	return GitHubConflictPreferNewer, nil
}

// parseGitHubSourceSystem parses a source system string like "github:https://...:42"
// Returns the issue number and ok (whether it's a valid GitHub source).
func parseGitHubSourceSystem(sourceSystem string) (number int, ok bool) {
	if !strings.HasPrefix(sourceSystem, "github:") {
		return 0, false
	}

	// Find last ":" and parse the number after it
	lastColon := strings.LastIndex(sourceSystem, ":")
	if lastColon == -1 || lastColon == len(sourceSystem)-1 {
		return 0, false
	}

	var err error
	number, err = strconv.Atoi(sourceSystem[lastColon+1:])
	if err != nil {
		return 0, false
	}

	return number, true
}

func init() {
	// Add subcommands to github
	githubCmd.AddCommand(githubSyncCmd)
	githubCmd.AddCommand(githubStatusCmd)
	githubCmd.AddCommand(githubReposCmd)
	githubCmd.AddCommand(githubPairCmd)
	githubCmd.AddCommand(githubUnpairCmd)
	githubPairCmd.Flags().StringVar(&githubPairDirection, "direction", "", "Direction: two-way (default), brain-to-github or github-to-brain")
	githubPairCmd.Flags().StringVar(&githubPairConflict, "conflict", "", "Two-way conflict policy: prefer-newer (default), prefer-local or prefer-github")
	githubPairCmd.Flags().BoolVar(&githubPairForce, "force", false, "Repoint a store that is already paired with a different repository")
	githubPairCmd.Flags().BoolVar(&githubPairNoVerify, "no-verify", false, "Do not check that the repository exists and is reachable")

	// Add flags to sync command
	githubSyncCmd.Flags().BoolVar(&githubSyncDryRun, "dry-run", false, "Show what would be synced without making changes")
	githubSyncCmd.Flags().BoolVar(&githubSyncPullOnly, "pull-only", false, "Only pull GitHub issues into brain")
	githubSyncCmd.Flags().BoolVar(&githubSyncPushOnly, "push-only", false, "Only push brain items to GitHub")
	githubSyncCmd.Flags().BoolVar(&githubSyncWatch, "watch", false, "Keep running and sync every --interval (off by default; see 'github schedule' for a background job)")
	githubSyncCmd.Flags().DurationVar(&githubSyncInterval, "interval", 5*time.Minute, "Time between syncs with --watch (minimum 30s)")
	githubSyncCmd.Flags().StringVar(&githubSyncRepo, "repo", "", "Run against this owner/repo for this run only, without saving a pairing")
	githubSyncCmd.Flags().StringVar(&githubSyncDir, "direction", "", "Direction for this run only when the store has no recorded pairing (two-way, brain-to-github, github-to-brain)")

	githubSyncCmd.Flags().StringVar(&githubSyncProject, "project", "", "Sync only the items that belong to this project record (for example project-73bi.1), one way, brain to GitHub, into the repository the record names")
	githubSyncCmd.Flags().StringSliceVar(&githubSyncProjectLabel, "project-label", nil, "With --project: also count items carrying this label (for example project:brain) as the project's; repeatable")
	githubSyncCmd.Flags().BoolVar(&githubSyncAllowPublic, "allow-public", false, "With --project: allow a real run to publish into a public repository (a dry run never needs it)")

	// Conflict resolution flags (mutually exclusive)
	githubSyncCmd.Flags().BoolVar(&githubPreferLocal, "prefer-local", false, "On conflict, keep local beads version")
	githubSyncCmd.Flags().BoolVar(&githubPreferGitHub, "prefer-github", false, "On conflict, use GitHub version")
	githubSyncCmd.Flags().BoolVar(&githubPreferNewer, "prefer-newer", false, "On conflict, use most recent version (default)")
	registerSelectiveSyncFlags(githubSyncCmd)

	// Register github command with root
	rootCmd.AddCommand(githubCmd)
}

// getGitHubConfig returns GitHub configuration from bd config or environment.
func getGitHubConfig() GitHubConfig {
	ctx := context.Background()
	config := GitHubConfig{}

	config.Token = getGitHubConfigValue(ctx, "github.token")
	config.Owner = getGitHubConfigValue(ctx, "github.owner")
	config.Repo = getGitHubConfigValue(ctx, "github.repo")
	config.Repository = getGitHubConfigValue(ctx, "github.repository")
	config.URL = getGitHubConfigValue(ctx, "github.url")

	// Parse combined owner/repo format if individual fields are empty
	if (config.Owner == "" || config.Repo == "") && config.Repository != "" {
		parts := strings.SplitN(config.Repository, "/", 2)
		if len(parts) == 2 {
			if config.Owner == "" {
				config.Owner = parts[0]
			}
			if config.Repo == "" {
				config.Repo = parts[1]
			}
		}
	}

	return config
}

// getGitHubConfigValue reads a GitHub configuration value from store or environment.
func getGitHubConfigValue(ctx context.Context, key string) string {
	// Secret keys (e.g. github.token) are stored in config.yaml, not the
	// Dolt database, to avoid leaking secrets when pushing to remotes.
	if config.IsYamlOnlyKey(key) {
		if value := config.GetString(key); value != "" {
			return value
		}
		// Fall back to environment variable
		envKey := githubConfigToEnvVar(key)
		if envKey != "" {
			if value := os.Getenv(envKey); value != "" {
				return value
			}
		}
		return ""
	}

	// Try to read from store (works in direct mode)
	if store != nil {
		value, _ := store.GetConfig(ctx, key)
		if value != "" {
			return value
		}
	} else if dbPath != "" {
		tempStore, err := openReadOnlyStoreForDBPath(ctx, dbPath)
		if err == nil {
			defer func() { _ = tempStore.Close() }()
			value, _ := tempStore.GetConfig(ctx, key)
			if value != "" {
				return value
			}
		}
	}

	// Fall back to environment variable
	envKey := githubConfigToEnvVar(key)
	if envKey != "" {
		if value := os.Getenv(envKey); value != "" {
			return value
		}
	}

	return ""
}

// githubConfigToEnvVar maps GitHub config keys to their environment variable names.
func githubConfigToEnvVar(key string) string {
	switch key {
	case "github.token":
		return "GITHUB_TOKEN"
	case "github.owner":
		return "GITHUB_OWNER"
	case "github.repo":
		return "GITHUB_REPO"
	case "github.repository":
		return "GITHUB_REPOSITORY"
	case "github.url":
		return "GITHUB_API_URL"
	default:
		return ""
	}
}

// validateGitHubConfig checks that required configuration is present.
func validateGitHubConfig(config GitHubConfig) error {
	if config.Token == "" {
		return fmt.Errorf("github.token is not configured. Set via 'bd config set github.token <token>' or GITHUB_TOKEN environment variable")
	}
	if config.Owner == "" {
		return fmt.Errorf("github.owner is not configured. Set via 'bd config set github.owner <owner>' or GITHUB_OWNER environment variable")
	}
	if config.Repo == "" {
		return fmt.Errorf("github.repo is not configured. Set via 'bd config set github.repo <repo>' or GITHUB_REPO environment variable")
	}
	return nil
}

// maskGitHubToken masks a token for safe display.
// Shows only the first 4 characters to aid identification without
// revealing enough to reduce brute-force entropy.
func maskGitHubToken(token string) string {
	if token == "" {
		return "(not set)"
	}
	if len(token) <= 4 {
		return "****"
	}
	return token[:4] + "****"
}

// getGitHubClient creates a GitHub client from the current configuration.
func getGitHubClient(config GitHubConfig) *github.Client {
	client := github.NewClient(config.Token, config.Owner, config.Repo)
	if config.URL != "" {
		client = client.WithBaseURL(config.URL)
	}
	return client
}

// runGitHubStatus implements the github status command.
func runGitHubStatus(cmd *cobra.Command, args []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("github status is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("github-status")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	config := getGitHubConfig()
	ctx := context.Background()
	pairing, pairErr := loadGitHubPairing(func(key string) string { return getGitHubConfigValue(ctx, key) }, currentStoreName())

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, "GitHub Pairing")
	_, _ = fmt.Fprintln(out, "==============")
	_, _ = fmt.Fprintf(out, "Store:      %s\n", orNoneLabel(pairing.Store))
	token := maskGitHubToken(config.Token)
	if config.Token == "" && ghTokenFromCLI() != "" {
		token = "(from the gh CLI login)"
		config.Token = "gh"
	}
	_, _ = fmt.Fprintf(out, "Token:      %s\n", token)
	_, _ = fmt.Fprintf(out, "Owner:      %s\n", config.Owner)
	_, _ = fmt.Fprintf(out, "Repository: %s\n", config.Repo)
	if config.URL != "" {
		_, _ = fmt.Fprintf(out, "API URL:    %s\n", config.URL)
	}
	if pairErr != nil {
		_, _ = fmt.Fprintf(out, "\nStatus: ❌ Pairing is not valid\nError: %v\n", pairErr)
		return nil
	}
	if pairing.Recorded {
		_, _ = fmt.Fprintf(out, "Direction:  %s\n", pairing.Direction)
		if pairing.Direction == GitHubTwoWay {
			_, _ = fmt.Fprintf(out, "Conflicts:  %s\n", pairing.Conflict)
		}
	} else {
		_, _ = fmt.Fprintf(out, "Direction:  %s (no pairing recorded; 'brain github pair' saves one)\n", pairing.Direction)
	}
	if store != nil {
		if last, err := store.GetLocalMetadata(ctx, "github.last_sync"); err == nil && last != "" {
			_, _ = fmt.Fprintf(out, "Last sync:  %s\n", last)
		}
	}

	// Validate configuration
	if err := validateGitHubConfig(config); err != nil {
		_, _ = fmt.Fprintf(out, "\nStatus: ❌ Not configured\n")
		_, _ = fmt.Fprintf(out, "Error: %v\n", err)
		return nil
	}

	_, _ = fmt.Fprintf(out, "\nStatus: ✓ Configured\n")
	return nil
}

func orNoneLabel(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// currentStoreName is the store this run addresses: the wrapper's name, or the
// store's issue prefix when run without a wrapper.
func currentStoreName() string {
	if name := strings.TrimSpace(os.Getenv("BD_NAME")); name != "" {
		return name
	}
	if p := config.GetString("issue-prefix"); p != "" {
		return p
	}
	if store != nil {
		if p, err := store.GetConfig(context.Background(), "issue_prefix"); err == nil {
			return strings.TrimSpace(p)
		}
	}
	return ""
}

// runGitHubRepos implements the github repos command.
func runGitHubRepos(cmd *cobra.Command, args []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("github repos is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("github-repos")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	config := getGitHubConfig()
	if config.Token == "" {
		return HandleError("github.token is not configured. Set via 'bd config set github.token <token>' or GITHUB_TOKEN environment variable")
	}

	out := cmd.OutOrStdout()
	client := getGitHubClient(config)
	ctx := context.Background()

	repos, err := client.ListRepositories(ctx)
	if err != nil {
		return HandleError("failed to fetch repositories: %v", err)
	}

	_, _ = fmt.Fprintln(out, "Accessible GitHub Repositories")
	_, _ = fmt.Fprintln(out, "==============================")
	for _, r := range repos {
		_, _ = fmt.Fprintf(out, "  %s\n", r.FullName)
		if r.Description != "" {
			_, _ = fmt.Fprintf(out, "    %s\n", r.Description)
		}
		_, _ = fmt.Fprintf(out, "    %s\n", r.HTMLURL)
		_, _ = fmt.Fprintln(out)
	}

	if len(repos) == 0 {
		_, _ = fmt.Fprintln(out, "No repositories found")
	}

	return nil
}

// ghTokenFromCLI asks the GitHub CLI for the token it is logged in with. The
// token goes straight into this process's environment for the tracker to read;
// it is never printed or stored.
func ghTokenFromCLI() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// githubSetup is everything one sync run needs: where to sync, in which
// direction, and with what conflict policy.
type githubSetup struct {
	Config  GitHubConfig
	Pairing GitHubPairing
	Plan    githubRunPlan
	AdHoc   bool
	// Project is set when the run is limited to one project's items.
	Project *projectScope
}

// resolveGitHubSetup merges the store's pairing with this run's flags.
func resolveGitHubSetup(cmd *cobra.Command) (*githubSetup, error) {
	ctx := context.Background()
	config := getGitHubConfig()

	pairing, err := loadGitHubPairing(func(key string) string { return getGitHubConfigValue(ctx, key) }, currentStoreName())
	if err != nil {
		return nil, err
	}

	adHoc := false
	var project *projectScope
	if githubSyncProject != "" {
		project, err = resolveGitHubProject(ctx, &config, &pairing)
		if err != nil {
			return nil, err
		}
		adHoc = true
	} else if len(githubSyncProjectLabel) > 0 || githubSyncAllowPublic {
		return nil, fmt.Errorf("--project-label and --allow-public only apply together with --project")
	}
	if githubSyncRepo != "" && project == nil {
		owner, repo, err := parseGitHubRepository(githubSyncRepo)
		if err != nil {
			return nil, err
		}
		if pairing.Recorded && !strings.EqualFold(pairing.Repository(), owner+"/"+repo) && !githubSyncDryRun {
			return nil, fmt.Errorf("this store is paired with %s; --repo %s/%s may only be used with --dry-run (repoint with 'github pair --force')", pairing.Repository(), owner, repo)
		}
		config.Owner, config.Repo = owner, repo
		pairing.Owner, pairing.Repo = owner, repo
		adHoc = true
	}
	if githubSyncDir != "" && project != nil {
		direction, err := parseGitHubDirection(githubSyncDir)
		if err != nil {
			return nil, err
		}
		if direction != GitHubBrainToGitHub {
			return nil, fmt.Errorf("--project syncs one way, brain to GitHub; --direction %s contradicts it", direction)
		}
	} else if githubSyncDir != "" {
		direction, err := parseGitHubDirection(githubSyncDir)
		if err != nil {
			return nil, err
		}
		if pairing.Recorded && direction != pairing.Direction {
			return nil, fmt.Errorf("this store is paired %s; --direction %s would override its pairing (change it with 'github pair --force --direction')", pairing.Direction, direction)
		}
		pairing.Direction = direction
		adHoc = true
	}

	flagStrategy, err := getGitHubConflictStrategy(githubPreferLocal, githubPreferGitHub, githubPreferNewer)
	if err != nil {
		return nil, fmt.Errorf("%v (--prefer-local, --prefer-github, --prefer-newer)", err)
	}
	conflictFlagGiven := githubPreferLocal || githubPreferGitHub || githubPreferNewer
	if conflictFlagGiven {
		if pairing.Direction != GitHubTwoWay {
			return nil, fmt.Errorf("conflict flags only apply to a two-way pairing; %s always takes the source side", pairing.Direction)
		}
		pairing.Conflict = flagStrategy
	}

	plan, err := planGitHubRun(pairing, githubSyncPullOnly, githubSyncPushOnly)
	if err != nil {
		return nil, err
	}
	return &githubSetup{Config: config, Pairing: pairing, Plan: plan, AdHoc: adHoc, Project: project}, nil
}

// runGitHubSync implements the github sync command.
// Uses the tracker.Engine for all sync operations.
func runGitHubSync(cmd *cobra.Command, args []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("github sync is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("github-sync")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	if !githubSyncDryRun {
		CheckReadonly("github sync")
	}
	if err := ensureStoreActive(); err != nil {
		return HandleError("database not available: %v", err)
	}

	setup, err := resolveGitHubSetup(cmd)
	if err != nil {
		return HandleError("%v", err)
	}
	if setup.Config.Token == "" {
		if token := ghTokenFromCLI(); token != "" {
			setup.Config.Token = token
			_ = os.Setenv("GITHUB_TOKEN", token)
		}
	}
	if err := validateGitHubConfig(setup.Config); err != nil {
		return HandleError("%v", err)
	}

	if !githubSyncWatch {
		_, err := runGitHubSyncOnce(cmd, setup)
		return err
	}

	if githubSyncInterval < 30*time.Second {
		return HandleError("--interval must be at least 30s")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Watching %s: syncing every %s (Ctrl-C to stop)\n", setup.Pairing.Repository(), githubSyncInterval)
	for {
		_, _ = fmt.Fprintf(out, "[%s] sync\n", time.Now().Format(time.RFC3339))
		if _, err := runGitHubSyncOnce(cmd, setup); err != nil {
			// A failed run (network, rate limit) must not end the watch.
			_, _ = fmt.Fprintf(os.Stderr, "sync failed, will retry: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(githubSyncInterval):
		}
	}
}

// githubPlanSummary is the per-side tally of one run. In a dry run it is the
// plan; after a real run it is what happened.
type githubPlanSummary struct {
	Store     string          `json:"store"`
	Repo      string          `json:"repo"`
	Direction GitHubDirection `json:"direction"`
	DryRun    bool            `json:"dry_run"`
	GitHub    githubSideTally `json:"github"`
	Brain     githubSideTally `json:"brain"`
	Conflicts int             `json:"conflicts"`
	Skipped   int             `json:"skipped"`
	Errors    int             `json:"errors"`
	// Project is set when the run was limited to one project's items.
	Project *githubProjectSummary `json:"project,omitempty"`
}

type githubSideTally struct {
	Create int `json:"create"`
	Update int `json:"update"`
	Link   int `json:"link,omitempty"`
}

func summarizeGitHubRun(setup *githubSetup, result *tracker.SyncResult, dryRun bool) githubPlanSummary {
	return githubPlanSummary{
		Store:     setup.Pairing.Store,
		Repo:      setup.Pairing.Repository(),
		Direction: setup.Pairing.Direction,
		DryRun:    dryRun,
		GitHub:    githubSideTally{Create: result.PushStats.Created, Update: result.PushStats.Updated, Link: result.PushStats.Linked},
		Brain:     githubSideTally{Create: result.PullStats.Created, Update: result.PullStats.Updated},
		Conflicts: result.Stats.Conflicts,
		Skipped:   result.Stats.Skipped,
		Errors:    result.Stats.Errors,
	}
}

func printGitHubSummary(out interface{ Write([]byte) (int, error) }, sum githubPlanSummary) {
	verb := map[bool]string{true: "would", false: "did"}[sum.DryRun]
	title := "Result"
	if sum.DryRun {
		title = "Plan (dry run, nothing changed)"
	}
	_, _ = fmt.Fprintf(out, "\n%s: %s <-> %s (%s)\n", title, orNoneLabel(sum.Store), sum.Repo, sum.Direction)
	_, _ = fmt.Fprintf(out, "  GitHub %s: create %d, update %d, link %d existing\n", verb, sum.GitHub.Create, sum.GitHub.Update, sum.GitHub.Link)
	_, _ = fmt.Fprintf(out, "  brain  %s: create %d, update %d\n", verb, sum.Brain.Create, sum.Brain.Update)
	if sum.Project != nil {
		_, _ = fmt.Fprintf(out, "  project %s: %d in scope, the rest of the store (%d) is left alone\n", sum.Project.ID, sum.Project.InScope, sum.Project.StoreItems-sum.Project.InScope)
		_, _ = fmt.Fprintf(out, "  unchanged %d, errors %d\n", sum.Skipped, sum.Errors)
		return
	}
	_, _ = fmt.Fprintf(out, "  unchanged/skipped %d, conflicts %d, errors %d\n", sum.Skipped, sum.Conflicts, sum.Errors)
}

// githubExcludedTypes are the system-internal item types that never go to
// GitHub: they are brain's own plumbing, not work a person would triage.
var githubExcludedTypes = []types.IssueType{types.TypeMessage, types.TypeMolecule, types.TypeGate, types.TypeEvent}

// newGitHubTargetedTracker prepares the tracker for 'github push' and 'github
// pull', which name the items to move. They obey the store's pairing: a
// brain-to-github pairing never pulls, a github-to-brain pairing never pushes.
func newGitHubTargetedTracker(ctx context.Context, push bool) (*github.Tracker, error) {
	config := getGitHubConfig()
	pairing, err := loadGitHubPairing(func(key string) string { return getGitHubConfigValue(ctx, key) }, currentStoreName())
	if err != nil {
		return nil, err
	}
	if push && pairing.Direction == GitHubGitHubToBrain {
		return nil, fmt.Errorf("this store is paired %s: nothing is pushed to GitHub", pairing.Direction)
	}
	if !push && pairing.Direction == GitHubBrainToGitHub {
		return nil, fmt.Errorf("this store is paired %s: nothing is pulled from GitHub", pairing.Direction)
	}
	if config.Token == "" {
		if token := ghTokenFromCLI(); token != "" {
			config.Token = token
			_ = os.Setenv("GITHUB_TOKEN", token)
		}
	}
	if err := validateGitHubConfig(config); err != nil {
		return nil, err
	}
	gt := &github.Tracker{Namespace: getGitHubConfigValue(ctx, githubKeyNamespace)}
	if err := gt.Init(ctx, store); err != nil {
		return nil, fmt.Errorf("initializing GitHub tracker: %w", err)
	}
	return gt, nil
}

// runGitHubSyncOnce performs one sync pass.
func runGitHubSyncOnce(cmd *cobra.Command, setup *githubSetup) (*tracker.SyncResult, error) {
	out := cmd.OutOrStdout()
	ctx := context.Background()

	gt := &github.Tracker{
		Namespace:  getGitHubConfigValue(ctx, githubKeyNamespace),
		Owner:      setup.Config.Owner,
		Repo:       setup.Config.Repo,
		BulkLookup: true,
	}
	if err := gt.Init(ctx, store); err != nil {
		return nil, HandleError("initializing GitHub tracker: %v", err)
	}

	// Create the sync engine
	engine := tracker.NewEngine(gt, store, actor)
	engine.OnMessage = func(msg string) { _, _ = fmt.Fprintln(out, "  "+msg) }
	if jsonOutput {
		// Keep stdout a single JSON document.
		engine.OnMessage = func(msg string) { _, _ = fmt.Fprintln(os.Stderr, "  "+msg) }
	}
	engine.OnWarning = func(msg string) { _, _ = fmt.Fprintf(os.Stderr, "Warning: %s\n", msg) }

	// GitHub-specific hooks: pulled issues keep what GitHub cannot say and
	// never copy another store's mirrors; pushes skip issues that already say
	// the same thing.
	engine.PullHooks = &tracker.PullHooks{
		ShouldImport: gt.ShouldImport,
		AfterConvert: gt.AfterConvert,
	}
	engine.PushHooks = buildGitHubPushHooks(gt)
	if setup.Plan.SourceWins && setup.Plan.Push {
		// A brain-to-github mirror must notice an edit made on GitHub. The
		// push cache only knows the local copy is unchanged since the last
		// push, so it would skip the issue; without it every linked issue is
		// compared with the bulk listing (BulkLookup), one request per
		// hundred issues.
		engine.PushHooks.ContentHash = nil
	}

	opts := tracker.SyncOptions{
		Pull:               setup.Plan.Pull,
		Push:               setup.Plan.Push,
		DryRun:             githubSyncDryRun,
		ExcludeEphemeral:   true,
		ExcludeTypes:       githubExcludedTypes,
		PullOverwriteLocal: setup.Plan.PullOverwrite,
		KeepSyncPosition:   setup.Project != nil,
	}
	if err := applySelectiveSyncFlags(cmd, &opts, setup.Plan.Push); err != nil {
		return nil, HandleError("%v", err)
	}

	switch setup.Plan.Conflict {
	case GitHubConflictPreferLocal:
		opts.ConflictResolution = tracker.ConflictLocal
	case GitHubConflictPreferGitHub:
		opts.ConflictResolution = tracker.ConflictExternal
	default:
		opts.ConflictResolution = tracker.ConflictTimestamp
	}

	if !jsonOutput {
		if githubSyncDryRun {
			_, _ = fmt.Fprintln(out, "Dry run mode - no changes will be made")
			_, _ = fmt.Fprintln(out)
		}
		if setup.AdHoc && setup.Project == nil {
			_, _ = fmt.Fprintf(out, "Ad hoc run against %s (%s); no pairing is saved\n", setup.Pairing.Repository(), setup.Pairing.Direction)
		}
	}

	var scoped *githubProjectPlan
	if setup.Project != nil {
		var err error
		scoped, err = planGitHubProject(ctx, setup, out)
		if err != nil {
			return nil, HandleError("%v", err)
		}
		engine.PushHooks.ShouldPush = scoped.ShouldPush
	}

	result, err := engine.Sync(ctx, opts)
	if err != nil {
		return result, HandleError("%v", err)
	}
	if err := commitSyncBeforeLastSync(ctx, gt, result); err != nil {
		return result, HandleError("%v", err)
	}

	sum := summarizeGitHubRun(setup, result, githubSyncDryRun)
	if scoped != nil {
		scoped.Annotate(&sum, result)
	}
	if jsonOutput {
		_ = outputJSON(sum)
		return result, nil
	}
	printGitHubSummary(out, sum)
	if githubSyncDryRun {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, "Run without --dry-run to apply changes")
	}
	return result, nil
}

// commitSyncBeforeLastSync makes the links this run wrote part of the history
// the next run reads. The next pull decides whether an item was linked since
// the last sync from external_ref as of last_sync in Dolt history (upstream's
// pre-linked hydrate), and an item it thinks was linked since may be
// overwritten from GitHub. In embedded mode the run's writes are committed
// only when the command ends, after the render and the outbox, which can be
// later than the last_sync the engine recorded; the link then looks new and
// the next pull overwrites a brain edit. So the run is committed here and
// last_sync recorded again after the commit. In server mode every write is
// already committed, and this records the same position again.
func commitSyncBeforeLastSync(ctx context.Context, gt *github.Tracker, result *tracker.SyncResult) error {
	if result == nil || result.LastSync == "" {
		return nil
	}
	if err := commitPendingIfEmbedded(ctx, store, actor, doltAutoCommitParams{Command: "github sync"}); err != nil {
		return fmt.Errorf("committing the sync: %w", err)
	}
	lastSync := time.Now().UTC().Truncate(time.Second).Add(time.Second).Format(time.RFC3339Nano)
	if err := store.SetLocalMetadata(ctx, gt.ConfigPrefix()+".last_sync", lastSync); err != nil {
		return fmt.Errorf("recording last_sync: %w", err)
	}
	result.LastSync = lastSync
	return nil
}

// githubPairCmd records the pairing between this store and a repository.
var githubPairCmd = &cobra.Command{
	Use:   "pair <owner/repo>",
	Short: "Pair this store with a GitHub repository and pick the sync direction",
	Long: `Record which GitHub repository this store syncs with, and which way changes travel.

  brain github pair trillium/brain                                  two-way (default)
  brain github pair trillium/brain --direction brain-to-github      brain is the source
  brain github pair trillium/brain --direction github-to-brain      GitHub is the source
  brain github pair trillium/brain --conflict prefer-local          brain wins two-way conflicts

The pairing is saved in this store's own config and touches no other store.
Nothing is synced by pairing; run 'brain github sync --dry-run' to see what the
first sync would do.

Pair one repository with one store. Two stores two-way paired with the same
repository would each import the other's native issues.`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runGitHubPair,
}

// githubUnpairCmd removes the pairing.
var githubUnpairCmd = &cobra.Command{
	Use:   "unpair",
	Short: "Remove this store's GitHub pairing",
	Long: `Forget the repository, direction and conflict policy for this store.

Items keep the link they remember to their issues and the issues keep their
markers, so pairing the same repository again picks up where it left off. No
item and no issue is deleted.`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runGitHubUnpair,
}

func runGitHubPair(cmd *cobra.Command, args []string) error {
	CheckReadonly("github pair")
	if err := ensureStoreActive(); err != nil {
		return HandleError("database not available: %v", err)
	}
	ctx := context.Background()

	owner, repo, err := parseGitHubRepository(args[0])
	if err != nil {
		return HandleError("%v", err)
	}
	direction, err := parseGitHubDirection(githubPairDirection)
	if err != nil {
		return HandleError("%v", err)
	}
	conflict, err := parseGitHubConflict(githubPairConflict)
	if err != nil {
		return HandleError("%v", err)
	}
	if githubPairConflict != "" && direction != GitHubTwoWay {
		return HandleError("--conflict only applies to a two-way pairing; %s always takes the source side", direction)
	}

	existing, err := loadGitHubPairing(func(key string) string { return getGitHubConfigValue(ctx, key) }, currentStoreName())
	if err != nil {
		return HandleError("existing pairing is not valid: %v", err)
	}
	repointing := existing.Repository() != "" && !strings.EqualFold(existing.Repository(), owner+"/"+repo)
	if repointing && !githubPairForce {
		return HandleError("this store is already paired with %s; pass --force to repoint it to %s/%s", existing.Repository(), owner, repo)
	}

	if !githubPairNoVerify {
		config := getGitHubConfig()
		if config.Token == "" {
			config.Token = ghTokenFromCLI()
		}
		if config.Token != "" {
			config.Owner, config.Repo = owner, repo
			if _, err := getGitHubClient(config).GetRepository(ctx); err != nil {
				return HandleError("cannot reach %s/%s with the available GitHub login: %v (use --no-verify to pair anyway)", owner, repo, err)
			}
		}
	}

	namespace := existing.Store
	if namespace == "" {
		namespace = currentStoreName()
	}
	if namespace == "" {
		return HandleError("cannot tell which store this is: run through the store's command (for example 'brain github pair ...') so BD_NAME is set")
	}

	values := map[string]string{
		githubKeyRepository: owner + "/" + repo,
		"github.owner":      owner,
		"github.repo":       repo,
		githubKeyDirection:  string(direction),
		githubKeyConflict:   string(conflict),
		githubKeyNamespace:  namespace,
	}
	for _, key := range []string{githubKeyRepository, "github.owner", "github.repo", githubKeyDirection, githubKeyConflict, githubKeyNamespace} {
		if err := store.SetConfig(ctx, key, values[key]); err != nil {
			return HandleError("saving %s: %v", key, err)
		}
	}
	if repointing {
		// Links and the sync position belong to the old repository.
		if err := store.SetLocalMetadata(ctx, "github.last_sync", ""); err != nil {
			return HandleError("resetting sync position: %v", err)
		}
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Paired %s with %s/%s (%s", namespace, owner, repo, direction)
	if direction == GitHubTwoWay {
		_, _ = fmt.Fprintf(out, ", conflicts: %s", conflict)
	}
	_, _ = fmt.Fprintln(out, ")")
	_, _ = fmt.Fprintln(out, "Nothing was synced. Preview the first sync with: brain github sync --dry-run")
	return nil
}

// buildGitHubPushHooks creates PushHooks for GitHub-specific push behavior.
// The ContentEqual hook lets the engine skip issues whose pushable fields
// already match GitHub, so repeated `github sync --push-only` / `github push`
// runs don't re-PATCH unchanged issues (gastownhall/beads#4214). It is the
// tracker's own comparison (brain): it also requires the item marker, so an
// issue not yet linked by marker is updated once to receive it.
func buildGitHubPushHooks(gt *github.Tracker) *tracker.PushHooks {
	config := gt.MappingConfig()
	if config == nil {
		config = github.DefaultMappingConfig()
	}
	return &tracker.PushHooks{
		ContentEqual: gt.ContentEqual,
		// ContentHash lets the engine skip the per-issue GitHub fetch entirely
		// when an issue is unchanged since its last push, so a no-op
		// `github sync --push-only` makes ~zero REST calls instead of one GET
		// per linked issue (gastownhall/beads#4214).
		ContentHash: func(local *types.Issue) string {
			return github.PushContentHash(local, config)
		},
		// TargetScope supplies the host and repository omitted by shorthand refs
		// such as github:42, so changing GitHub target configuration invalidates
		// the local no-op cache.
		TargetScope: gt.PushTargetScope,
	}
}

func runGitHubUnpair(cmd *cobra.Command, args []string) error {
	CheckReadonly("github unpair")
	if err := ensureStoreActive(); err != nil {
		return HandleError("database not available: %v", err)
	}
	ctx := context.Background()
	for _, key := range []string{githubKeyRepository, "github.owner", "github.repo", githubKeyDirection, githubKeyConflict, githubKeyNamespace} {
		if err := store.DeleteConfig(ctx, key); err != nil {
			return HandleError("clearing %s: %v", key, err)
		}
	}
	if err := store.SetLocalMetadata(ctx, "github.last_sync", ""); err != nil {
		return HandleError("resetting sync position: %v", err)
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Pairing removed. No item or issue was changed.")
	return nil
}
