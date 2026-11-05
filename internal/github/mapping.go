// Package github provides client and data types for the GitHub REST API.
package github

import (
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/types"
)

// MappingConfig configures how GitHub fields map to beads fields.
type MappingConfig struct {
	PriorityMap  map[string]int    // priority label value -> beads priority (0-4)
	StateMap     map[string]string // GitHub state -> beads status
	LabelTypeMap map[string]string // type label value -> beads issue type

	// Namespace is the brain store this mapping serves. When set, pushed
	// bodies carry a marker naming the item (see marker.go) and a pulled
	// issue whose marker names this store maps back onto that item's id.
	Namespace string
}

// DefaultMappingConfig returns the default mapping configuration.
// Uses exported mapping constants from types.go as the single source of truth.
func DefaultMappingConfig() *MappingConfig {
	// Copy PriorityMapping to avoid external modification
	priorityMap := make(map[string]int, len(PriorityMapping))
	for k, v := range PriorityMapping {
		priorityMap[k] = v
	}

	// Copy typeMapping to avoid external modification
	labelTypeMap := make(map[string]string, len(typeMapping))
	for k, v := range typeMapping {
		labelTypeMap[k] = v
	}

	return &MappingConfig{
		PriorityMap: priorityMap,
		// StateMap maps GitHub states to beads statuses
		StateMap: map[string]string{
			"open":   StatusMapping["open"],
			"closed": StatusMapping["closed"],
		},
		LabelTypeMap: labelTypeMap,
	}
}

// priorityFromLabels extracts priority from GitHub labels.
// Returns default priority (2 = medium) if no priority label found.
func priorityFromLabels(labels []string, config *MappingConfig) int {
	for _, label := range labels {
		prefix, value := parseLabelPrefix(label)
		if prefix == "priority" {
			if p, ok := config.PriorityMap[strings.ToLower(value)]; ok {
				return p
			}
		}
	}
	return 2 // Default to medium
}

// statusFromLabelsAndState determines beads status from GitHub labels and state.
// GitHub's closed state takes precedence over status labels.
func statusFromLabelsAndState(labels []string, state string, config *MappingConfig) string {
	// Closed state always wins
	if state == "closed" {
		return "closed"
	}

	// Check for status label
	for _, label := range labels {
		prefix, value := parseLabelPrefix(label)
		if prefix == "status" {
			normalized := strings.ToLower(value)
			if normalized == "in_progress" {
				return "in_progress"
			}
			if normalized == "blocked" {
				return "blocked"
			}
			if normalized == "deferred" {
				return "deferred"
			}
		}
	}

	// Default: map GitHub state to beads status
	if s, ok := config.StateMap[state]; ok {
		return s
	}
	return "open"
}

// typeFromLabels extracts issue type from GitHub labels.
// Checks both scoped (type::bug) and bare (bug) labels.
// Returns "task" if no type label found.
func typeFromLabels(labels []string, config *MappingConfig) string {
	for _, label := range labels {
		prefix, value := parseLabelPrefix(label)
		if prefix == "type" {
			if t, ok := config.LabelTypeMap[strings.ToLower(value)]; ok {
				return t
			}
		}
		// Also check bare labels (no prefix)
		if prefix == "" {
			if t, ok := config.LabelTypeMap[strings.ToLower(value)]; ok {
				return t
			}
		}
	}
	return "task" // Default to task
}

// GitHubIssueToBeads converts a GitHub Issue to a beads Issue.
func GitHubIssueToBeads(gh *Issue, config *MappingConfig) *IssueConversion {
	htmlURL := gh.HTMLURL
	sourceSystem := fmt.Sprintf("github:%s:%d", gh.HTMLURL, gh.Number)
	// Use a cleaner source system format if HTMLURL is not available
	if htmlURL == "" {
		sourceSystem = fmt.Sprintf("github:%d:%d", gh.ID, gh.Number)
	}

	labelNames := gh.LabelNames()

	body := gh.Body
	var issueID string
	if marker, ok := ParseMarker(body); ok {
		body = StripMarker(body)
		if config.Namespace != "" && marker.Store == config.Namespace {
			issueID = marker.ID
		}
	}

	issue := &types.Issue{
		ID:           issueID,
		Title:        gh.Title,
		Description:  body,
		ExternalRef:  &htmlURL,
		SourceSystem: sourceSystem,
		IssueType:    types.IssueType(typeFromLabels(labelNames, config)),
		Priority:     priorityFromLabels(labelNames, config),
		Status:       types.Status(statusFromLabelsAndState(labelNames, gh.State, config)),
		Labels:       filterNonScopedLabels(labelNames),
	}

	// Set assignee from GitHub user
	if gh.Assignee != nil {
		issue.Assignee = gh.Assignee.Login
	}

	// Set timestamps
	if gh.CreatedAt != nil {
		issue.CreatedAt = *gh.CreatedAt
	}
	if gh.UpdatedAt != nil {
		issue.UpdatedAt = *gh.UpdatedAt
	}

	return &IssueConversion{
		Issue:        issue,
		Dependencies: []DependencyInfo{},
	}
}

// BeadsIssueToGitHubFields converts a beads Issue to GitHub API update fields.
func BeadsIssueToGitHubFields(issue *types.Issue, config *MappingConfig) map[string]interface{} {
	body := issue.Description
	if config != nil && config.Namespace != "" && issue.ID != "" {
		body = WithMarker(body, config.Namespace, issue.ID)
	}
	fields := map[string]interface{}{
		"title": issue.Title,
		"body":  body,
	}

	// Build labels from type, priority, and status
	var labels []string

	// Add type label
	if issue.IssueType != "" {
		labels = append(labels, "type::"+string(issue.IssueType))
	}

	// Add priority label
	priorityLabel := priorityToLabel(issue.Priority)
	if priorityLabel != "" {
		labels = append(labels, "priority::"+priorityLabel)
	}

	// Add status label (if not open or closed - those are handled by state).
	// Every other status keeps its own label, so a status GitHub has no word
	// for (pinned, hooked, a custom status) survives the round trip.
	if issue.Status != "" && issue.Status != types.StatusOpen && issue.Status != types.StatusClosed {
		labels = append(labels, "status::"+string(issue.Status))
	}

	// Add any existing non-scoped labels
	labels = append(labels, issue.Labels...)

	fields["labels"] = labels

	// Set state for closed issues
	if issue.Status == types.StatusClosed {
		fields["state"] = "closed"
	} else {
		fields["state"] = "open"
	}

	return fields
}

// priorityToLabel converts beads priority (0-4) to GitHub priority label value.
func priorityToLabel(priority int) string {
	switch priority {
	case 0:
		return "critical"
	case 1:
		return "high"
	case 2:
		return "medium"
	case 3:
		return "low"
	case 4:
		return "none"
	default:
		return "medium"
	}
}

// filterNonScopedLabels returns only labels without scoped prefixes.
// Removes priority::*, status::*, and type::* labels.
func filterNonScopedLabels(labels []string) []string {
	var filtered []string
	for _, label := range labels {
		prefix, _ := parseLabelPrefix(label)
		// Skip scoped labels that we handle specially
		if prefix == "priority" || prefix == "status" || prefix == "type" {
			continue
		}
		filtered = append(filtered, label)
	}
	return filtered
}
