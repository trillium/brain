package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/metrics"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/ui"
	"github.com/steveyegge/beads/internal/utils"
)

// groupLabel marks a bead as a finish-line group. Only labeled beads close
// themselves when their members finish, so older beads that happen to carry
// "tracks" edges keep their current behavior.
const groupLabel = "finish-line-group"

// groupMember is one tracked bead as seen from its group.
type groupMember struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	// Unresolved marks an edge whose target is not in this store.
	Unresolved bool `json:"unresolved,omitempty"`
}

// groupProgress summarizes a group's tracked members.
type groupProgress struct {
	Group   *types.Issue  `json:"group"`
	Members []groupMember `json:"members"`
	Total   int           `json:"total"`
	Done    int           `json:"done"`
}

// Complete reports whether every tracked member is closed. An empty group is
// never complete, and an unresolved member counts as not done.
func (p *groupProgress) Complete() bool {
	return p.Total > 0 && p.Done == p.Total
}

// groupReader is the read surface the group logic needs. The direct store
// satisfies it, and the proxied close path supplies an adapter.
type groupReader interface {
	// trackingGroups returns the open finish-line groups that track id.
	trackingGroups(ctx context.Context, id string) ([]*types.Issue, error)
	// members returns the beads the group tracks.
	members(ctx context.Context, groupID string) ([]groupMember, error)
}

type storeGroupReader struct{ s storage.DoltStorage }

func (r storeGroupReader) trackingGroups(ctx context.Context, id string) ([]*types.Issue, error) {
	dependents, err := r.s.GetDependentsWithMetadata(ctx, id)
	if err != nil {
		return nil, err
	}
	var groups []*types.Issue
	for _, d := range dependents {
		if d.DependencyType != types.DepTracks || d.Status == types.StatusClosed {
			continue
		}
		labels, err := r.s.GetLabels(ctx, d.ID)
		if err != nil {
			return nil, err
		}
		if labelsInclude(labels, groupLabel) {
			issue := d.Issue
			groups = append(groups, &issue)
		}
	}
	return groups, nil
}

func (r storeGroupReader) members(ctx context.Context, groupID string) ([]groupMember, error) {
	deps, err := r.s.GetDependenciesWithMetadata(ctx, groupID)
	if err != nil {
		return nil, err
	}
	var out []groupMember
	for _, d := range deps {
		if d.DependencyType != types.DepTracks {
			continue
		}
		out = append(out, groupMember{ID: d.ID, Title: d.Title, Status: string(d.Status), Unresolved: d.Unresolved})
	}
	return out, nil
}

func labelsInclude(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// computeGroupProgress tallies a group's members.
func computeGroupProgress(ctx context.Context, r groupReader, group *types.Issue) (*groupProgress, error) {
	members, err := r.members(ctx, group.ID)
	if err != nil {
		return nil, err
	}
	p := &groupProgress{Group: group, Members: members, Total: len(members)}
	for _, m := range members {
		if !m.Unresolved && m.Status == string(types.StatusClosed) {
			p.Done++
		}
	}
	return p, nil
}

// groupsReadyToClose returns the open groups that tracked closedID and now
// have every member closed.
func groupsReadyToClose(ctx context.Context, r groupReader, closedID string) ([]*groupProgress, error) {
	groups, err := r.trackingGroups(ctx, closedID)
	if err != nil {
		return nil, err
	}
	var ready []*groupProgress
	for _, g := range groups {
		p, err := computeGroupProgress(ctx, r, g)
		if err != nil {
			return nil, err
		}
		if p.Complete() {
			ready = append(ready, p)
		}
	}
	return ready, nil
}

const groupCloseReason = "all tracked members closed"

// autoCloseCompletedGroups closes every finish-line group whose last open
// member was just closed. Best effort: a failure here never fails the close.
func autoCloseCompletedGroups(ctx context.Context, s storage.DoltStorage, closedID, actorName, session string) {
	ready, err := groupsReadyToClose(ctx, storeGroupReader{s}, closedID)
	if err != nil {
		return
	}
	for _, p := range ready {
		if err := s.CloseIssue(ctx, p.Group.ID, groupCloseReason, actorName, session); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not auto-close finished group %s: %v\n", p.Group.ID, err)
			continue
		}
		if !jsonOutput {
			fmt.Printf("%s Auto-closed finished group %s\n", ui.RenderPass("✓"), formatFeedbackID(p.Group.ID, p.Group.Title))
		}
		autoCloseCompletedGroups(ctx, s, p.Group.ID, actorName, session)
	}
}

var groupCmd = &cobra.Command{
	Use:     "group",
	GroupID: "deps",
	Short:   "Finish-line groups: one bead that tracks a push and closes itself when it is done",
	Long: `A group is a bead that tracks other beads with non-blocking "tracks"
edges. Members are never blocked by the group and tracking does not change
ready-work calculation. When every tracked member is closed, the group
closes itself.

Use a group for a push with a finish line. Keep long-lived project beads
for durable knowledge rather than as a to-do list.

Members must live in the same store as the group.`,
}

func requireDirectGroupStore() error {
	if usesProxiedServer() {
		return fmt.Errorf("bd group verbs are not yet supported with the proxied server; closing a member still closes its groups")
	}
	return nil
}

var groupCreateCmd = &cobra.Command{
	Use:           "create <title> [member-id...]",
	Short:         "Create a finish-line group, optionally tracking members",
	Args:          cobra.MinimumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("group-create")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()
		CheckReadonly("group create")
		if err := requireDirectGroupStore(); err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		ctx := rootCtx
		memberIDs, err := resolveGroupMembers(ctx, args[1:])
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		desc, _ := cmd.Flags().GetString("description")
		priority, _ := cmd.Flags().GetInt("priority")
		group := &types.Issue{
			Title:       args[0],
			Description: desc,
			Status:      types.StatusOpen,
			Priority:    priority,
			IssueType:   types.TypeTask,
			CreatedBy:   getFilingActor(),
		}
		if err := store.CreateIssue(ctx, group, actor); err != nil {
			return HandleErrorRespectJSON("creating group: %v", err)
		}
		commandDidWrite.Store(true)
		if err := store.AddLabel(ctx, group.ID, groupLabel, actor); err != nil {
			return HandleErrorRespectJSON("labeling group %s: %v", group.ID, err)
		}
		for _, id := range memberIDs {
			if err := trackMember(ctx, group.ID, id); err != nil {
				return HandleErrorRespectJSON("%v", err)
			}
		}
		if err := closeGroupIfComplete(ctx, group.ID); err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		SetLastTouchedID(group.ID)
		if jsonOutput {
			return printGroupJSON(ctx, group.ID)
		}
		fmt.Printf("%s Created group %s tracking %d member(s)\n", ui.RenderPass("✓"), formatFeedbackID(group.ID, group.Title), len(memberIDs))
		return nil
	},
}

var groupAddCmd = &cobra.Command{
	Use:           "add <group-id> <member-id>...",
	Short:         "Track more beads in a group",
	Args:          cobra.MinimumNArgs(2),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		CheckReadonly("group add")
		if err := requireDirectGroupStore(); err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		ctx := rootCtx
		groupID, err := resolveGroupID(ctx, args[0])
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		memberIDs, err := resolveGroupMembers(ctx, args[1:])
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		for _, id := range memberIDs {
			if err := trackMember(ctx, groupID, id); err != nil {
				return HandleErrorRespectJSON("%v", err)
			}
		}
		if err := closeGroupIfComplete(ctx, groupID); err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		if jsonOutput {
			return printGroupJSON(ctx, groupID)
		}
		fmt.Printf("%s Group %s now tracks %d more member(s)\n", ui.RenderPass("✓"), groupID, len(memberIDs))
		return nil
	},
}

var groupRemoveCmd = &cobra.Command{
	Use:           "remove <group-id> <member-id>...",
	Short:         "Stop tracking beads in a group",
	Args:          cobra.MinimumNArgs(2),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		CheckReadonly("group remove")
		if err := requireDirectGroupStore(); err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		ctx := rootCtx
		groupID, err := resolveGroupID(ctx, args[0])
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		memberIDs, err := resolveGroupMembers(ctx, args[1:])
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		tracked, err := storeGroupReader{store}.members(ctx, groupID)
		if err != nil {
			return HandleErrorRespectJSON("reading group members: %v", err)
		}
		for _, id := range memberIDs {
			if !groupTracks(tracked, id) {
				return HandleErrorRespectJSON("%s is not tracked by group %s", id, groupID)
			}
			if err := store.RemoveDependency(ctx, groupID, id, actor); err != nil {
				return HandleErrorRespectJSON("removing %s from group %s: %v", id, groupID, err)
			}
			commandDidWrite.Store(true)
		}
		// Removing the last open member can finish the group.
		if err := closeGroupIfComplete(ctx, groupID); err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		if jsonOutput {
			return printGroupJSON(ctx, groupID)
		}
		fmt.Printf("%s Group %s no longer tracks %d member(s)\n", ui.RenderPass("✓"), groupID, len(memberIDs))
		return nil
	},
}

var groupShowCmd = &cobra.Command{
	Use:           "show <group-id>",
	Short:         "Show a group's members and progress",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := requireDirectGroupStore(); err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		ctx := rootCtx
		groupID, err := resolveGroupID(ctx, args[0])
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		if jsonOutput {
			return printGroupJSON(ctx, groupID)
		}
		group, err := store.GetIssue(ctx, groupID)
		if err != nil || group == nil {
			return HandleErrorRespectJSON("loading group %s: %v", groupID, err)
		}
		p, err := computeGroupProgress(ctx, storeGroupReader{store}, group)
		if err != nil {
			return HandleErrorRespectJSON("reading group members: %v", err)
		}
		fmt.Printf("%s [%s]\n", formatFeedbackID(group.ID, group.Title), group.Status)
		fmt.Printf("   Progress: %d/%d members closed\n", p.Done, p.Total)
		for _, m := range p.Members {
			mark := "○"
			if !m.Unresolved && m.Status == string(types.StatusClosed) {
				mark = ui.RenderPass("✓")
			}
			status := m.Status
			if m.Unresolved {
				status = "not in this store"
			}
			fmt.Printf("   %s %s (%s)\n", mark, formatFeedbackID(m.ID, m.Title), status)
		}
		return nil
	},
}

var groupSyncCmd = &cobra.Command{
	Use:           "sync",
	Short:         "Close every open group whose members are all closed",
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		CheckReadonly("group sync")
		if err := requireDirectGroupStore(); err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		ctx := rootCtx
		open := types.StatusOpen
		groups, err := store.SearchIssues(ctx, "", types.IssueFilter{Labels: []string{groupLabel}, Status: &open})
		if err != nil {
			return HandleErrorRespectJSON("listing groups: %v", err)
		}
		var closed []string
		for _, g := range groups {
			if err := closeGroupIfComplete(ctx, g.ID); err != nil {
				return HandleErrorRespectJSON("%v", err)
			}
			if after, _ := store.GetIssue(ctx, g.ID); after != nil && after.Status == types.StatusClosed {
				closed = append(closed, g.ID)
			}
		}
		if jsonOutput {
			if closed == nil {
				closed = []string{}
			}
			return outputJSON(map[string]interface{}{"closed": closed, "count": len(closed)})
		}
		fmt.Printf("%s Closed %d finished group(s)\n", ui.RenderPass("✓"), len(closed))
		return nil
	},
}

func resolveGroupMembers(ctx context.Context, inputs []string) ([]string, error) {
	ids := make([]string, 0, len(inputs))
	for _, in := range inputs {
		id, err := utils.ResolvePartialID(ctx, store, in)
		if err != nil {
			return nil, fmt.Errorf("member %s: %v", in, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// resolveGroupID resolves input and checks it is a finish-line group.
func resolveGroupID(ctx context.Context, input string) (string, error) {
	id, err := utils.ResolvePartialID(ctx, store, input)
	if err != nil {
		return "", fmt.Errorf("group %s: %v", input, err)
	}
	labels, err := store.GetLabels(ctx, id)
	if err != nil {
		return "", fmt.Errorf("reading labels of %s: %v", id, err)
	}
	if !labelsInclude(labels, groupLabel) {
		return "", fmt.Errorf("%s is not a finish-line group (create one with `bd group create`)", id)
	}
	return id, nil
}

func groupTracks(members []groupMember, id string) bool {
	for _, m := range members {
		if m.ID == id {
			return true
		}
	}
	return false
}

func trackMember(ctx context.Context, groupID, memberID string) error {
	if groupID == memberID {
		return fmt.Errorf("a group cannot track itself")
	}
	tracked, err := storeGroupReader{store}.members(ctx, groupID)
	if err != nil {
		return fmt.Errorf("reading group members: %v", err)
	}
	if groupTracks(tracked, memberID) {
		return nil
	}
	dep := &types.Dependency{IssueID: groupID, DependsOnID: memberID, Type: types.DepTracks}
	if err := store.AddDependency(ctx, dep, actor); err != nil {
		return fmt.Errorf("tracking %s in group %s: %v", memberID, groupID, err)
	}
	commandDidWrite.Store(true)
	return nil
}

// closeGroupIfComplete closes an open group whose members are all closed.
func closeGroupIfComplete(ctx context.Context, groupID string) error {
	group, err := store.GetIssue(ctx, groupID)
	if err != nil || group == nil {
		return fmt.Errorf("loading group %s: %v", groupID, err)
	}
	if group.Status == types.StatusClosed {
		return nil
	}
	p, err := computeGroupProgress(ctx, storeGroupReader{store}, group)
	if err != nil {
		return fmt.Errorf("reading group members: %v", err)
	}
	if !p.Complete() {
		return nil
	}
	if err := store.CloseIssue(ctx, groupID, groupCloseReason, actor, ""); err != nil {
		return fmt.Errorf("closing group %s: %v", groupID, err)
	}
	commandDidWrite.Store(true)
	if !jsonOutput {
		fmt.Printf("%s Closed finished group %s\n", ui.RenderPass("✓"), formatFeedbackID(group.ID, group.Title))
	}
	autoCloseCompletedGroups(ctx, store, groupID, actor, "")
	return nil
}

func printGroupJSON(ctx context.Context, groupID string) error {
	group, err := store.GetIssue(ctx, groupID)
	if err != nil || group == nil {
		return HandleErrorRespectJSON("loading group %s: %v", groupID, err)
	}
	p, err := computeGroupProgress(ctx, storeGroupReader{store}, group)
	if err != nil {
		return HandleErrorRespectJSON("reading group members: %v", err)
	}
	if p.Members == nil {
		p.Members = []groupMember{}
	}
	return outputJSON(p)
}

func init() {
	groupCreateCmd.Flags().StringP("description", "d", "", "Group description")
	groupCreateCmd.Flags().IntP("priority", "p", 2, "Group priority (0-4)")
	groupCmd.AddCommand(groupCreateCmd, groupAddCmd, groupRemoveCmd, groupShowCmd, groupSyncCmd)
	rootCmd.AddCommand(groupCmd)
}
