// Package storage — brain_exfiltration_roles.go
//
// Upstream 1.3 routes the CLI's writes through role values taken off the
// store (IssueLifecycle, IssueClaimer, Commenter, …). A role taken through the
// decorator's embedded DoltStorage would be the inner store's, and every write
// through it would skip the markdown render this decorator exists for. So the
// decorator hands back the same role decorators HookFiringStore uses
// (hook_issue_operations.go and its siblings), wired to a renderer instead of
// the hook runner: one completion surface, two consumers. A new hook-firing
// role upstream adds is therefore one accessor here away from rendering, and
// TestBrainExfiltrationDecoratorWrapsEveryHookFiringRole keeps the two lists
// in step.
package storage

import (
	"context"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// exfilCompletion renders the brain docs a landed role write touched. It
// satisfies issueOperationHooks, the completion surface the role decorators
// call after each committed operation.
type exfilCompletion struct{ d *BrainExfiltrationDecorator }

var _ issueOperationHooks = exfilCompletion{}

func (c exfilCompletion) CompleteIssueOperationCreate(ctx context.Context, issue *types.Issue, dependencies []*types.Dependency) {
	if issue == nil {
		return
	}
	c.d.renderByID(ctx, issue.ID)
	// A reverse edge changes the other end's dependency list, which its own
	// markdown shows.
	for _, dep := range dependencies {
		if dep != nil && dep.IssueID != issue.ID {
			c.d.renderByID(ctx, dep.IssueID)
		}
	}
}

func (c exfilCompletion) CompleteIssueOperationUpdate(issue *types.Issue) {
	if issue != nil {
		c.d.renderByID(context.Background(), issue.ID)
	}
}

func (c exfilCompletion) CompleteIssueOperationClose(issue *types.Issue) {
	if issue != nil {
		c.d.renderByID(context.Background(), issue.ID)
	}
}

func (c exfilCompletion) CompleteIssueOperationDependency(ctx context.Context, issueID string) {
	c.d.renderByID(ctx, issueID)
}

func (c exfilCompletion) CompleteIssueOperationComment(ctx context.Context, issueID string) {
	c.d.renderByID(ctx, issueID)
}

func (c exfilCompletion) CompleteIssueOperationMetadata(ctx context.Context, issueID string) {
	c.d.renderByID(ctx, issueID)
}

func (c exfilCompletion) CompleteIssueOperationRelease(ctx context.Context, issueID string) {
	c.d.renderByID(ctx, issueID)
}

// IssueLifecycle renders after create, update, close and reopen. An update
// that changes the issue type also removes the markdown filed under the old
// kind, as UpdateIssue does.
func (d *BrainExfiltrationDecorator) IssueLifecycle() (issueops.Lifecycle, error) {
	inner, err := d.inner.IssueLifecycle()
	if err != nil || d.exf == nil {
		return inner, err
	}
	return &exfilLifecycle{hookIssueOperations: &hookIssueOperations{inner: inner, hooks: exfilCompletion{d}}, d: d}, nil
}

type exfilLifecycle struct {
	*hookIssueOperations
	d *BrainExfiltrationDecorator
}

func (l *exfilLifecycle) Update(ctx context.Context, req issueops.UpdateRequest) (issueops.UpdateResult, error) {
	if !req.Patch.IssueType.Set {
		return l.hookIssueOperations.Update(ctx, req)
	}
	oldKind, oldSlug := l.d.snapshotKindAndSlug(ctx, req.IssueID)
	res, err := l.hookIssueOperations.Update(ctx, req)
	if err == nil {
		l.d.applyKindTransitionCleanup(ctx, req.IssueID, oldKind, oldSlug, types.IssueType(req.Patch.IssueType.Value))
		// The cleanup can remove the file the completion just wrote when
		// the slug did not change, so render once more.
		l.d.renderByID(ctx, req.IssueID)
	}
	return res, err
}

// IssueClaimer renders the claimed bead.
func (d *BrainExfiltrationDecorator) IssueClaimer() (issueops.Claimer, error) {
	inner, err := d.inner.IssueClaimer()
	if err != nil || d.exf == nil {
		return inner, err
	}
	return &hookIssueClaimer{inner: inner, hooks: exfilCompletion{d}}, nil
}

// ReadyClaimer renders the bead a claim-next picked.
func (d *BrainExfiltrationDecorator) ReadyClaimer() (issueops.ReadyClaimer, error) {
	inner, err := d.inner.ReadyClaimer()
	if err != nil || d.exf == nil {
		return inner, err
	}
	return &hookReadyClaimer{inner: inner, hooks: exfilCompletion{d}}, nil
}

// Commenter renders the commented bead.
func (d *BrainExfiltrationDecorator) Commenter() (issueops.Commenter, error) {
	inner, err := d.inner.Commenter()
	if err != nil || d.exf == nil {
		return inner, err
	}
	return &hookCommenter{inner: inner, hooks: exfilCompletion{d}}, nil
}

// DependencyEditor renders each edge's source bead.
func (d *BrainExfiltrationDecorator) DependencyEditor() (issueops.DependencyEditor, error) {
	inner, err := d.inner.DependencyEditor()
	if err != nil || d.exf == nil {
		return inner, err
	}
	return &hookDependencyEditor{inner: inner, hooks: exfilCompletion{d}}, nil
}

// BatchCloser renders every bead the batch closed.
func (d *BrainExfiltrationDecorator) BatchCloser() (issueops.BatchCloser, error) {
	inner, err := d.inner.BatchCloser()
	if err != nil || d.exf == nil {
		return inner, err
	}
	return &hookBatchCloser{inner: inner, hooks: exfilCompletion{d}}, nil
}

// BatchCreator renders every bead the batch created.
func (d *BrainExfiltrationDecorator) BatchCreator() (issueops.BatchCreator, error) {
	inner, err := d.inner.BatchCreator()
	if err != nil || d.exf == nil {
		return inner, err
	}
	return &hookBatchCreator{inner: inner, hooks: exfilCompletion{d}}, nil
}

// BatchApplier renders every bead an applied batch landed.
func (d *BrainExfiltrationDecorator) BatchApplier() (issueops.BatchApplier, error) {
	inner, err := d.inner.BatchApplier()
	if err != nil || d.exf == nil {
		return inner, err
	}
	return &hookBatchApplier{inner: inner, hooks: exfilCompletion{d}}, nil
}

// MetadataCAS renders a bead whose metadata a compare-and-set moved.
func (d *BrainExfiltrationDecorator) MetadataCAS() (issueops.MetadataCAS, error) {
	inner, err := d.inner.MetadataCAS()
	if err != nil || d.exf == nil {
		return inner, err
	}
	return &hookMetadataCAS{inner: inner, hooks: exfilCompletion{d}}, nil
}

// Releaser renders a released bead.
func (d *BrainExfiltrationDecorator) Releaser() (issueops.Releaser, error) {
	inner, err := d.inner.Releaser()
	if err != nil || d.exf == nil {
		return inner, err
	}
	return &hookReleaser{inner: inner, hooks: exfilCompletion{d}}, nil
}
