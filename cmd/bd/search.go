package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/metrics"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/kvkeys"
	"github.com/steveyegge/beads/internal/storage/sqlbuild"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/utils"
	"github.com/steveyegge/beads/internal/validation"
)

var searchCmd = &cobra.Command{
	Use:     "search [query]",
	GroupID: "issues",
	Short:   "Search issues by text query",
	Long: `Search issues across title, description, comments, and ID (excludes closed issues by default).

ID-like queries (e.g., "bd-123", "hq-319") use fast exact/prefix matching.
Text queries are tokenized on whitespace and each token is matched against
title, description, and comment bodies; results are ranked by relevance unless
--sort is given (comment-only matches rank below title/description matches).
Use --no-comments to skip comment bodies, and --status all to include closed issues.

Examples:
  bd search "authentication bug"
  bd search "login" --status open
  bd search "database" --label backend --limit 10
  bd search --query "performance" --assignee alice
  bd search "bd-5q" # Search by partial ID (fast prefix match)
  bd search "security" --priority-min 0 --priority-max 2
  bd search "bug" --created-after 2025-01-01
  bd search "refactor" --status all  # Include closed issues
  bd search "bug" --sort priority
  bd search "task" --sort created --reverse
  bd search "api" --desc-contains "endpoint"
  bd search "release" --comments-contains "rollback"
  bd search "fork-origin" --no-comments  # title/description/ID only
  bd search "cleanup" --no-assignee --no-labels`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("search")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		queryFlag, _ := cmd.Flags().GetString("query")
		var query string
		if len(args) > 0 {
			query = strings.Join(args, " ")
		} else if queryFlag != "" {
			query = queryFlag
		}

		if query == "" {
			if err := cmd.Help(); err != nil {
				fmt.Fprintf(os.Stderr, "Error displaying help: %v\n", err)
			}
			return HandleError("search query is required")
		}

		// Federated mode: search the primary store, then any registered
		// secondary stores on the same Dolt server. Emits sectioned output
		// (text) or per-store arrays (JSON). Delegates entirely; the rest
		// of the standard search path does not run.
		if federated, _ := cmd.Flags().GetBool("federated"); federated {
			runFederatedSearch(cmd, query)
			return nil
		}

		// Get filter flags
		status, _ := cmd.Flags().GetString("status")
		assignee, _ := cmd.Flags().GetString("assignee")
		issueType, _ := cmd.Flags().GetString("type")
		limit, _ := cmd.Flags().GetInt("limit")
		labels, _ := cmd.Flags().GetStringSlice("label")
		labelsAny, _ := cmd.Flags().GetStringSlice("label-any")
		longFormat, _ := cmd.Flags().GetBool("long")
		sortBy, _ := cmd.Flags().GetString("sort")
		reverse, _ := cmd.Flags().GetBool("reverse")

		// Date range flags
		createdAfter, _ := cmd.Flags().GetString("created-after")
		createdBefore, _ := cmd.Flags().GetString("created-before")
		updatedAfter, _ := cmd.Flags().GetString("updated-after")
		updatedBefore, _ := cmd.Flags().GetString("updated-before")
		closedAfter, _ := cmd.Flags().GetString("closed-after")
		closedBefore, _ := cmd.Flags().GetString("closed-before")

		// Priority range flags
		priorityMinStr, _ := cmd.Flags().GetString("priority-min")
		priorityMaxStr, _ := cmd.Flags().GetString("priority-max")

		// Pattern matching flags
		descContains, _ := cmd.Flags().GetString("desc-contains")
		notesContains, _ := cmd.Flags().GetString("notes-contains")
		externalContains, _ := cmd.Flags().GetString("external-contains")
		commentsContains, _ := cmd.Flags().GetString("comments-contains")
		noComments, _ := cmd.Flags().GetBool("no-comments")

		// Empty/null check flags
		emptyDesc, _ := cmd.Flags().GetBool("empty-description")
		noAssignee, _ := cmd.Flags().GetBool("no-assignee")
		noLabels, _ := cmd.Flags().GetBool("no-labels")

		// Normalize labels
		labels = utils.NormalizeLabels(labels)
		labelsAny = utils.NormalizeLabels(labelsAny)

		// Build filter
		filter := types.IssueFilter{
			Limit: limit,
		}

		if status != "" && status != "all" {
			s := types.Status(status)
			filter.Status = &s
		} else if status != "all" {
			// Default: exclude closed issues to reduce scan scope (hq-319).
			// With 12K+ issues, ~60-70% are closed — excluding them lets the
			// query use the status index to skip the majority of rows.
			// Use --status all to search everything including closed.
			filter.ExcludeStatus = []types.Status{types.StatusClosed}
		}

		if assignee != "" {
			filter.Assignee = &assignee
		}

		if issueType != "" {
			t := types.IssueType(issueType)
			filter.IssueType = &t
		}

		if len(labels) > 0 {
			filter.Labels = labels
		}

		if len(labelsAny) > 0 {
			filter.LabelsAny = labelsAny
		}

		// Pattern matching
		if descContains != "" {
			filter.DescriptionContains = descContains
		}
		if notesContains != "" {
			filter.NotesContains = notesContains
		}
		if externalContains != "" {
			filter.ExternalRefContains = externalContains
		}
		if commentsContains != "" {
			filter.CommentsContains = commentsContains
		}
		// Comment bodies hold most of the durable content in a long-lived store,
		// so free-text search reads them by default; --no-comments restores the
		// cheaper title/description-only scan (robots-4m0m).
		filter.SearchComments = !noComments

		// Empty/null checks
		if emptyDesc {
			filter.EmptyDescription = true
		}
		if noAssignee {
			filter.NoAssignee = true
		}
		if noLabels {
			filter.NoLabels = true
		}

		// Date ranges
		if createdAfter != "" {
			t, err := parseTimeFlag(createdAfter)
			if err != nil {
				return HandleError("parsing --created-after: %v", err)
			}
			filter.CreatedAfter = &t
		}
		if createdBefore != "" {
			t, err := parseTimeFlag(createdBefore)
			if err != nil {
				return HandleError("parsing --created-before: %v", err)
			}
			filter.CreatedBefore = &t
		}
		if updatedAfter != "" {
			t, err := parseTimeFlag(updatedAfter)
			if err != nil {
				return HandleError("parsing --updated-after: %v", err)
			}
			filter.UpdatedAfter = &t
		}
		if updatedBefore != "" {
			t, err := parseTimeFlag(updatedBefore)
			if err != nil {
				return HandleError("parsing --updated-before: %v", err)
			}
			filter.UpdatedBefore = &t
		}
		if closedAfter != "" {
			t, err := parseTimeFlag(closedAfter)
			if err != nil {
				return HandleError("parsing --closed-after: %v", err)
			}
			filter.ClosedAfter = &t
		}
		if closedBefore != "" {
			t, err := parseTimeFlag(closedBefore)
			if err != nil {
				return HandleError("parsing --closed-before: %v", err)
			}
			filter.ClosedBefore = &t
		}

		if cmd.Flags().Changed("priority-min") {
			priorityMin, err := validation.ValidatePriority(priorityMinStr)
			if err != nil {
				return HandleError("parsing --priority-min: %v", err)
			}
			filter.PriorityMin = &priorityMin
		}
		if cmd.Flags().Changed("priority-max") {
			priorityMax, err := validation.ValidatePriority(priorityMaxStr)
			if err != nil {
				return HandleError("parsing --priority-max: %v", err)
			}
			filter.PriorityMax = &priorityMax
		}

		metadataFieldFlags, _ := cmd.Flags().GetStringArray("metadata-field")
		if len(metadataFieldFlags) > 0 {
			filter.MetadataFields = make(map[string]string, len(metadataFieldFlags))
			for _, mf := range metadataFieldFlags {
				k, v, ok := strings.Cut(mf, "=")
				if !ok || k == "" {
					return HandleError("invalid --metadata-field: expected key=value, got %q", mf)
				}
				if err := storage.ValidateMetadataKey(k); err != nil {
					return HandleError("invalid --metadata-field key: %v", err)
				}
				filter.MetadataFields[k] = v
			}
		}
		hasMetadataKey, _ := cmd.Flags().GetString("has-metadata-key")
		if hasMetadataKey != "" {
			if err := storage.ValidateMetadataKey(hasMetadataKey); err != nil {
				return HandleError("invalid --has-metadata-key: %v", err)
			}
			filter.HasMetadataKey = hasMetadataKey
		}

		ctx := rootCtx

		// Relevance ranking (task-4ja): for free-text queries (not ID lookups)
		// with no explicit --sort, rank results by match quality. SQL applies
		// LIMIT before Go scoring can run, so fetch the full match set
		// (Limit=0) and truncate after ranking; otherwise the LIMIT could drop
		// the best-scoring rows before they are ever scored. ID-like queries and
		// explicitly-sorted queries keep the fast, LIMIT-pushed path unchanged.
		rankResults := !sqlbuild.LooksLikeIssueID(query) && !cmd.Flags().Changed("sort")
		displayLimit := limit
		if rankResults {
			filter.Limit = 0
		}

		issues, err := store.SearchIssues(ctx, query, filter)
		if err != nil {
			return HandleError("%v", err)
		}

		if rankResults {
			rankSearchResults(issues, query)
			if displayLimit > 0 && len(issues) > displayLimit {
				issues = issues[:displayLimit]
			}
		} else {
			// Explicit --sort or ID-like query: preserve prior sort behavior.
			sortIssues(issues, sortBy, reverse)
		}

		if jsonOutput {
			// Get labels and dependency counts
			issueIDs := make([]string, len(issues))
			for i, issue := range issues {
				issueIDs[i] = issue.ID
			}
			labelsMap, err := store.GetLabelsForIssues(ctx, issueIDs)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to get labels: %v\n", err)
				labelsMap = make(map[string][]string)
			}
			depCounts, err := store.GetDependencyCounts(ctx, issueIDs)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to get dependency counts: %v\n", err)
				depCounts = make(map[string]*types.DependencyCounts)
			}
			commentCounts, err := store.GetCommentCounts(ctx, issueIDs)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to get comment counts: %v\n", err)
				commentCounts = make(map[string]int)
			}

			// Populate labels
			for _, issue := range issues {
				issue.Labels = labelsMap[issue.ID]
			}

			// Build response with counts
			issuesWithCounts := make([]*types.IssueWithCounts, len(issues))
			for i, issue := range issues {
				counts := depCounts[issue.ID]
				if counts == nil {
					counts = &types.DependencyCounts{DependencyCount: 0, DependentCount: 0}
				}
				issuesWithCounts[i] = &types.IssueWithCounts{
					Issue:           issue,
					DependencyCount: counts.DependencyCount,
					DependentCount:  counts.DependentCount,
					CommentCount:    commentCounts[issue.ID],
				}
			}
			return outputJSON(issuesWithCounts)
		}

		// Load labels for display
		issueIDs := make([]string, len(issues))
		for i, issue := range issues {
			issueIDs[i] = issue.ID
		}
		labelsMap, _ := store.GetLabelsForIssues(ctx, issueIDs)
		for _, issue := range issues {
			issue.Labels = labelsMap[issue.ID]
		}

		outputSearchResults(issues, query, longFormat)
		// `bd remember` writes to the config table, so its content is invisible
		// to issue search. A bare "No issues found" for text the user knows they
		// stored reads as data loss; point at the memory that actually holds it.
		if len(issues) == 0 {
			printMemoryMatchHint(ctx, query)
		}
		return nil
	},
}

// printMemoryMatchHint reports memories whose key or body matches query. Best
// effort: a failed config read prints nothing rather than muddying the miss.
func printMemoryMatchHint(ctx context.Context, query string) {
	allConfig, err := store.GetAllConfig(ctx)
	if err != nil {
		return
	}
	needle := strings.ToLower(query)
	var keys []string
	// kv.membead.* rows pair a memory key with the issue `remember` minted for
	// it. Collected alongside so the hint can name an ID the reader can act on.
	beads := make(map[string]string)
	for k, v := range allConfig {
		if strings.HasPrefix(k, kvkeys.MemoryBeadConfigKeyPrefix) {
			beads[strings.TrimPrefix(k, kvkeys.MemoryBeadConfigKeyPrefix)] = v
			continue
		}
		if !strings.HasPrefix(k, kvkeys.MemoryConfigKeyPrefix) {
			continue
		}
		userKey := strings.TrimPrefix(k, kvkeys.MemoryConfigKeyPrefix)
		if strings.Contains(strings.ToLower(userKey), needle) || strings.Contains(strings.ToLower(v), needle) {
			keys = append(keys, userKey)
		}
	}
	if len(keys) == 0 {
		return
	}
	sort.Strings(keys)
	tool := memoryToolName()
	fmt.Printf("\nBut %d stored memory/memories match (memories are not issues and are not searched by '%s search'):\n", len(keys), tool)
	for _, k := range keys {
		if beadID := beads[k]; beadID != "" {
			fmt.Printf("  %s  (issue %s)\n", k, beadID)
			continue
		}
		fmt.Printf("  %s\n", k)
	}
	fmt.Printf("Read them with '%s memories %s'.\n", tool, query)
}

// outputSearchResults formats and displays search results
func outputSearchResults(issues []*types.Issue, query string, longFormat bool) {
	if len(issues) == 0 {
		fmt.Printf("No issues found matching '%s'\n", query)
		return
	}

	if longFormat {
		// Long format: multi-line with details
		fmt.Printf("\nFound %d issues matching '%s':\n\n", len(issues), query)
		for _, issue := range issues {
			fmt.Printf("%s [P%d] [%s] %s\n", issue.ID, issue.Priority, issue.IssueType, issue.Status)
			fmt.Printf("  %s\n", issue.Title)
			if issue.Assignee != "" {
				fmt.Printf("  Assignee: %s\n", issue.Assignee)
			}
			if len(issue.Labels) > 0 {
				fmt.Printf("  Labels: %v\n", issue.Labels)
			}
			fmt.Println()
		}
	} else {
		// Compact format: one line per issue
		fmt.Printf("Found %d issues matching '%s':\n", len(issues), query)
		for _, issue := range issues {
			labelsStr := ""
			if len(issue.Labels) > 0 {
				labelsStr = fmt.Sprintf(" %v", issue.Labels)
			}
			assigneeStr := ""
			if issue.Assignee != "" {
				assigneeStr = fmt.Sprintf(" @%s", issue.Assignee)
			}
			fmt.Printf("%s [P%d] [%s] %s%s%s - %s\n",
				issue.ID, issue.Priority, issue.IssueType, issue.Status,
				assigneeStr, labelsStr, issue.Title)
		}
	}
}

func init() {
	searchCmd.Flags().String("query", "", "Search query (alternative to positional argument)")
	searchCmd.Flags().StringP("status", "s", "", "Filter by stored status (open, in_progress, blocked, deferred, closed, all). Default excludes closed; use 'all' to include closed. Note: dependency-blocked issues use 'bd blocked'")
	searchCmd.Flags().StringP("assignee", "a", "", "Filter by assignee")
	searchCmd.Flags().StringP("type", "t", "", "Filter by type (bug, feature, task, epic, chore, decision, merge-request, molecule, gate)")
	searchCmd.Flags().StringSliceP("label", "l", []string{}, "Filter by labels (AND: must have ALL)")
	searchCmd.Flags().StringSlice("label-any", []string{}, "Filter by labels (OR: must have AT LEAST ONE)")
	searchCmd.Flags().IntP("limit", "n", 50, "Limit results (default: 50)")
	searchCmd.Flags().Bool("long", false, "Show detailed multi-line output for each issue")
	searchCmd.Flags().String("sort", "", "Sort by field: priority, created, updated, closed, status, id, title, type, assignee")
	searchCmd.Flags().BoolP("reverse", "r", false, "Reverse sort order")

	// Date range flags
	searchCmd.Flags().String("created-after", "", "Filter issues created after date (YYYY-MM-DD or RFC3339)")
	searchCmd.Flags().String("created-before", "", "Filter issues created before date (YYYY-MM-DD or RFC3339)")
	searchCmd.Flags().String("updated-after", "", "Filter issues updated after date (YYYY-MM-DD or RFC3339)")
	searchCmd.Flags().String("updated-before", "", "Filter issues updated before date (YYYY-MM-DD or RFC3339)")
	searchCmd.Flags().String("closed-after", "", "Filter issues closed after date (YYYY-MM-DD or RFC3339)")
	searchCmd.Flags().String("closed-before", "", "Filter issues closed before date (YYYY-MM-DD or RFC3339)")

	// Priority range flags
	searchCmd.Flags().String("priority-min", "", "Filter by minimum priority (inclusive, 0-4 or P0-P4)")
	searchCmd.Flags().String("priority-max", "", "Filter by maximum priority (inclusive, 0-4 or P0-P4)")

	// Pattern matching flags
	searchCmd.Flags().String("desc-contains", "", "Filter by description substring (case-insensitive)")
	searchCmd.Flags().String("notes-contains", "", "Filter by notes substring (case-insensitive)")
	searchCmd.Flags().String("external-contains", "", "Filter by external ref substring (case-insensitive)")
	searchCmd.Flags().String("comments-contains", "", "Filter by comment-body substring (case-insensitive)")
	searchCmd.Flags().Bool("no-comments", false, "Do not match the query against comment bodies (faster)")

	// Empty/null check flags
	searchCmd.Flags().Bool("empty-description", false, "Filter issues with empty or missing description")
	searchCmd.Flags().Bool("no-assignee", false, "Filter issues with no assignee")
	searchCmd.Flags().Bool("no-labels", false, "Filter issues with no labels")

	// Metadata filtering (GH#1406)
	searchCmd.Flags().StringArray("metadata-field", nil, "Filter by metadata field (key=value, repeatable)")
	searchCmd.Flags().String("has-metadata-key", "", "Filter issues that have this metadata key set")

	rootCmd.AddCommand(searchCmd)
}
