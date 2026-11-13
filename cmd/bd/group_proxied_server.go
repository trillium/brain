package main

import (
	"context"
	"fmt"
	"os"

	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/ui"
)

type uowGroupReader struct{ uw uow.UnitOfWork }

func (r uowGroupReader) trackingGroups(ctx context.Context, id string) ([]*types.Issue, error) {
	dependents, err := r.uw.DependencyUseCase().ListWithIssueMetadata(ctx, id, domain.DepListFilter{
		Types:     []types.DependencyType{types.DepTracks},
		Direction: domain.DepDirectionIn,
	})
	if err != nil {
		return nil, err
	}
	var groups []*types.Issue
	for _, d := range dependents {
		if d.DependencyType != types.DepTracks || d.Status == types.StatusClosed {
			continue
		}
		labels, err := r.uw.LabelUseCase().GetLabels(ctx, d.ID)
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

func (r uowGroupReader) members(ctx context.Context, groupID string) ([]groupMember, error) {
	deps, err := r.uw.DependencyUseCase().ListWithIssueMetadata(ctx, groupID, domain.DepListFilter{
		Types:     []types.DependencyType{types.DepTracks},
		Direction: domain.DepDirectionOut,
	})
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

// autoCloseProxiedCompletedGroups is the proxied-server twin of
// autoCloseCompletedGroups. It returns the ids of the groups it closed, so a
// caller that names its own commit message can name them too.
func autoCloseProxiedCompletedGroups(ctx context.Context, uw uow.UnitOfWork, closedID, actorName, session string, jsonOut bool) []string {
	ready, err := groupsReadyToClose(ctx, uowGroupReader{uw}, closedID)
	if err != nil {
		return nil
	}
	var closed []string
	for _, p := range ready {
		params := domain.CloseIssueParams{Reason: groupCloseReason, Session: session}
		if _, err := uw.IssueUseCase().CloseIssue(ctx, p.Group.ID, params, actorName); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not auto-close finished group %s: %v\n", p.Group.ID, err)
			continue
		}
		closed = append(closed, p.Group.ID)
		if !jsonOut {
			fmt.Printf("%s Auto-closed finished group %s\n", ui.RenderPass("✓"), formatFeedbackID(p.Group.ID, p.Group.Title))
		}
		closed = append(closed, autoCloseProxiedCompletedGroups(ctx, uw, p.Group.ID, actorName, session, jsonOut)...)
	}
	return closed
}
