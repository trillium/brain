// Package storage — brain_exfiltration_decorator.go
//
// BrainExfiltrationDecorator is a storage decorator that renders brain
// docs (issues whose IssueType is one of {task, knowledge, both}) to
// markdown after every successful mutation. It stacks above the
// existing HookFiringStore so the decorator chain becomes:
//
//	rawStore → HookFiringStore → BrainExfiltrationDecorator → store
//
// Non-brain-kind mutations passthrough untouched — bd's own behavior
// does not change.
//
// See:
//   - divergence/0012-exfiltration-decorator.md — landing notes,
//     documented sensible-default decisions.
//   - docs/brain/WHAT_IS_BRAIN.md § 8 — architecture diagram, the
//     "Exfiltration is a decorator on bd's existing HookFiringStore,
//     not a parallel write path" line.
//   - internal/brain/exfiltrator/exfiltrator.go — Render / Remove
//     implementation; this file holds only the decorator wiring.
package storage

import (
	"context"

	"github.com/steveyegge/beads/internal/brain/exfiltrator"
	"github.com/steveyegge/beads/internal/types"
)

// BrainExfiltrationDecorator wraps a DoltStorage and writes a markdown
// render after every successful mutation that produces a brain-kind
// issue. Non-mutation methods pass through via the embedded interface.
type BrainExfiltrationDecorator struct {
	DoltStorage // embedded for passthrough of non-overridden methods
	inner       DoltStorage
	exf         exfiltrator.Exfiltrator
}

// NewBrainExfiltrationDecorator wraps store so brain-kind mutations
// trigger a markdown render via exf. If exf is nil, the decorator is
// a passthrough (no rendering happens).
func NewBrainExfiltrationDecorator(store DoltStorage, exf exfiltrator.Exfiltrator) *BrainExfiltrationDecorator {
	return &BrainExfiltrationDecorator{
		DoltStorage: store,
		inner:       store,
		exf:         exf,
	}
}

// Inner returns the underlying store, useful for type assertions
// (e.g., StoreLocator, RawDBAccessor). Mirrors HookFiringStore.Inner.
func (d *BrainExfiltrationDecorator) Inner() DoltStorage { return d.inner }

// ── Issue mutations ────────────────────────────────────────────────

// CreateIssue creates an issue and renders the markdown for brain-kind
// docs after the storage write succeeds.
func (d *BrainExfiltrationDecorator) CreateIssue(ctx context.Context, issue *types.Issue, actor string) error {
	if err := d.inner.CreateIssue(ctx, issue, actor); err != nil {
		return err
	}
	d.renderByID(ctx, issue.ID)
	return nil
}

// CreateIssues creates multiple issues and renders the markdown for
// each brain-kind doc after the storage write succeeds.
func (d *BrainExfiltrationDecorator) CreateIssues(ctx context.Context, issues []*types.Issue, actor string) error {
	if err := d.inner.CreateIssues(ctx, issues, actor); err != nil {
		return err
	}
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		d.renderByID(ctx, issue.ID)
	}
	return nil
}

// UpdateIssue updates an issue and re-renders the markdown if the
// updated issue is a brain-kind doc.
//
// When the updates map carries an "issue_type" field, the call is
// effectively a kind transition (this is the path `brain recast` takes;
// see internal/brain/verb/recast/recast.go). The decorator snapshots
// the (oldKind, oldSlug) BEFORE the storage write so it can remove the
// stale file from `entries/<oldKind>/` afterwards — mirroring the
// dedicated UpdateIssueType path and honouring divergence/0012's
// "kind transitions move the file" decision.
func (d *BrainExfiltrationDecorator) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	var (
		hasKindChange bool
		oldKind       types.IssueType
		oldSlug       string
		newKind       types.IssueType
	)
	if raw, ok := updates["issue_type"]; ok {
		if s, ok := raw.(string); ok && s != "" {
			oldKind, oldSlug = d.snapshotKindAndSlug(ctx, id)
			newKind = types.IssueType(s)
			hasKindChange = true
		}
	}

	if err := d.inner.UpdateIssue(ctx, id, updates, actor); err != nil {
		return err
	}

	if hasKindChange {
		d.applyKindTransitionCleanup(ctx, id, oldKind, oldSlug, newKind)
	}

	d.renderByID(ctx, id)
	return nil
}

// ReopenIssue reopens an issue and re-renders.
func (d *BrainExfiltrationDecorator) ReopenIssue(ctx context.Context, id string, reason string, actor string) error {
	if err := d.inner.ReopenIssue(ctx, id, reason, actor); err != nil {
		return err
	}
	d.renderByID(ctx, id)
	return nil
}

// UpdateIssueType changes an issue's kind. If the transition crosses
// the brain set boundary (e.g. knowledge → bug, or bug → task), the
// stale file is removed and/or a new file is written so the on-disk
// view matches the new kind.
func (d *BrainExfiltrationDecorator) UpdateIssueType(ctx context.Context, id string, issueType string, actor string) error {
	// Snapshot the old kind + slug before the update so we can clean
	// up if the transition crosses out of the brain set.
	oldKind, oldSlug := d.snapshotKindAndSlug(ctx, id)

	if err := d.inner.UpdateIssueType(ctx, id, issueType, actor); err != nil {
		return err
	}

	newKind := types.IssueType(issueType)
	d.applyKindTransitionCleanup(ctx, id, oldKind, oldSlug, newKind)

	if exfiltrator.IsBrainKind(newKind) {
		d.renderByID(ctx, id)
	}
	return nil
}

// applyKindTransitionCleanup removes the stale `entries/<oldKind>/<oldSlug>.md`
// file when an issue's kind has just changed. Caller is responsible for
// snapshotting (oldKind, oldSlug) BEFORE the storage write — this helper
// only consumes the snapshot.
//
// No-ops when:
//   - oldKind is not a brain kind (no markdown was ever written),
//   - oldKind == newKind (e.g. a status-only update that happened to
//     pass through this path),
//   - oldSlug is empty (the snapshot didn't resolve a slug), or
//   - the exfiltrator is nil (passthrough mode).
//
// Remove failures are intentionally swallowed: the storage mutation
// already succeeded and the reconciler is the documented safety net for
// any orphan file (divergence/0012 § Decisions #5).
func (d *BrainExfiltrationDecorator) applyKindTransitionCleanup(ctx context.Context, id string, oldKind types.IssueType, oldSlug string, newKind types.IssueType) {
	if d.exf == nil || oldSlug == "" {
		return
	}
	if !exfiltrator.IsBrainKind(oldKind) || oldKind == newKind {
		return
	}
	_ = d.exf.Remove(ctx, id, oldKind, oldSlug)
}

// CloseIssue closes an issue and re-renders.
func (d *BrainExfiltrationDecorator) CloseIssue(ctx context.Context, id string, reason string, actor string, session string) error {
	if err := d.inner.CloseIssue(ctx, id, reason, actor, session); err != nil {
		return err
	}
	d.renderByID(ctx, id)
	return nil
}

// ── Dependency mutations ───────────────────────────────────────────

// AddDependency adds a dependency and re-renders the affected issue.
func (d *BrainExfiltrationDecorator) AddDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	if err := d.inner.AddDependency(ctx, dep, actor); err != nil {
		return err
	}
	if dep != nil {
		d.renderByID(ctx, dep.IssueID)
	}
	return nil
}

// RemoveDependency removes a dependency and re-renders.
func (d *BrainExfiltrationDecorator) RemoveDependency(ctx context.Context, issueID, dependsOnID string, actor string) error {
	if err := d.inner.RemoveDependency(ctx, issueID, dependsOnID, actor); err != nil {
		return err
	}
	d.renderByID(ctx, issueID)
	return nil
}

// ── Label mutations ────────────────────────────────────────────────

// AddLabel adds a label and re-renders.
func (d *BrainExfiltrationDecorator) AddLabel(ctx context.Context, issueID, label, actor string) error {
	if err := d.inner.AddLabel(ctx, issueID, label, actor); err != nil {
		return err
	}
	d.renderByID(ctx, issueID)
	return nil
}

// RemoveLabel removes a label and re-renders.
func (d *BrainExfiltrationDecorator) RemoveLabel(ctx context.Context, issueID, label, actor string) error {
	if err := d.inner.RemoveLabel(ctx, issueID, label, actor); err != nil {
		return err
	}
	d.renderByID(ctx, issueID)
	return nil
}

// ── Comment mutations ──────────────────────────────────────────────

// AddIssueComment adds a comment and re-renders the parent issue.
func (d *BrainExfiltrationDecorator) AddIssueComment(ctx context.Context, issueID, author, text string) (*types.Comment, error) {
	comment, err := d.inner.AddIssueComment(ctx, issueID, author, text)
	if err != nil {
		return nil, err
	}
	d.renderByID(ctx, issueID)
	return comment, nil
}

// ── Transaction support ────────────────────────────────────────────

// RunInTransaction wraps the callback's transaction with render tracking.
// Mutations inside the transaction accumulate a set of issue IDs to
// re-render; renders fire only after the transaction commits successfully.
// On rollback or error, no renders fire.
func (d *BrainExfiltrationDecorator) RunInTransaction(ctx context.Context, commitMsg string, fn func(tx Transaction) error) error {
	var tracked *exfiltrationTrackingTransaction
	err := d.inner.RunInTransaction(ctx, commitMsg, func(tx Transaction) error {
		tracked = &exfiltrationTrackingTransaction{Transaction: tx}
		return fn(tracked)
	})
	if err != nil || tracked == nil {
		return err
	}
	// Transaction committed — render every dirty ID once.
	for _, id := range tracked.dirty() {
		d.renderByID(ctx, id)
	}
	return nil
}

// ── Internals ──────────────────────────────────────────────────────

// renderByID re-fetches the issue and (if it is a brain kind) hands
// it to the exfiltrator. Re-fetch failures are swallowed — the storage
// mutation already succeeded, and a transient read failure must not
// surface as a write failure to the caller. The reconciler is the
// safety net for any rows that missed their render.
func (d *BrainExfiltrationDecorator) renderByID(ctx context.Context, id string) {
	if d.exf == nil || id == "" {
		return
	}
	issue, err := d.inner.GetIssue(ctx, id)
	if err != nil {
		return
	}
	_ = d.exf.Render(ctx, issue)
}

// snapshotKindAndSlug returns the current (kind, slug) for id from the
// inner store. Used before UpdateIssueType so the decorator can remove
// the stale file if the kind transition leaves the brain set.
//
// Returns ("", "") when the issue is missing or its metadata has no
// slug.
func (d *BrainExfiltrationDecorator) snapshotKindAndSlug(ctx context.Context, id string) (types.IssueType, string) {
	issue, err := d.inner.GetIssue(ctx, id)
	if err != nil || issue == nil {
		return "", ""
	}
	if mx, ok := d.exf.(*exfiltrator.MarkdownExfiltrator); ok && exfiltrator.IsBrainKind(issue.IssueType) {
		slug, _, err := mx.SlugFor(issue)
		if err != nil {
			return issue.IssueType, ""
		}
		return issue.IssueType, slug
	}
	return issue.IssueType, ""
}

// ── Tracking transaction ───────────────────────────────────────────

// exfiltrationTrackingTransaction wraps a Transaction, recording the
// IDs of mutated issues so renders fire after commit.
//
// Dedup happens at commit time via a small inline set so an issue
// mutated multiple times inside the transaction renders only once.
type exfiltrationTrackingTransaction struct {
	Transaction
	pendingIDs []string
	seen       map[string]struct{}
}

func (t *exfiltrationTrackingTransaction) mark(id string) {
	if id == "" {
		return
	}
	if t.seen == nil {
		t.seen = make(map[string]struct{})
	}
	if _, ok := t.seen[id]; ok {
		return
	}
	t.seen[id] = struct{}{}
	t.pendingIDs = append(t.pendingIDs, id)
}

func (t *exfiltrationTrackingTransaction) dirty() []string {
	out := make([]string, len(t.pendingIDs))
	copy(out, t.pendingIDs)
	return out
}

func (t *exfiltrationTrackingTransaction) CreateIssue(ctx context.Context, issue *types.Issue, actor string) error {
	if err := t.Transaction.CreateIssue(ctx, issue, actor); err != nil {
		return err
	}
	if issue != nil {
		t.mark(issue.ID)
	}
	return nil
}

func (t *exfiltrationTrackingTransaction) CreateIssues(ctx context.Context, issues []*types.Issue, actor string) error {
	if err := t.Transaction.CreateIssues(ctx, issues, actor); err != nil {
		return err
	}
	for _, issue := range issues {
		if issue != nil {
			t.mark(issue.ID)
		}
	}
	return nil
}

func (t *exfiltrationTrackingTransaction) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	if err := t.Transaction.UpdateIssue(ctx, id, updates, actor); err != nil {
		return err
	}
	t.mark(id)
	return nil
}

func (t *exfiltrationTrackingTransaction) CloseIssue(ctx context.Context, id string, reason string, actor string, session string) error {
	if err := t.Transaction.CloseIssue(ctx, id, reason, actor, session); err != nil {
		return err
	}
	t.mark(id)
	return nil
}

func (t *exfiltrationTrackingTransaction) AddDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	return t.AddDependencyWithOptions(ctx, dep, actor, DependencyAddOptions{})
}

func (t *exfiltrationTrackingTransaction) AddDependencyWithOptions(ctx context.Context, dep *types.Dependency, actor string, opts DependencyAddOptions) error {
	if err := t.Transaction.AddDependencyWithOptions(ctx, dep, actor, opts); err != nil {
		return err
	}
	if dep != nil {
		t.mark(dep.IssueID)
	}
	return nil
}

func (t *exfiltrationTrackingTransaction) RemoveDependency(ctx context.Context, issueID, dependsOnID string, actor string) error {
	if err := t.Transaction.RemoveDependency(ctx, issueID, dependsOnID, actor); err != nil {
		return err
	}
	t.mark(issueID)
	return nil
}

func (t *exfiltrationTrackingTransaction) AddLabel(ctx context.Context, issueID, label, actor string) error {
	if err := t.Transaction.AddLabel(ctx, issueID, label, actor); err != nil {
		return err
	}
	t.mark(issueID)
	return nil
}

func (t *exfiltrationTrackingTransaction) RemoveLabel(ctx context.Context, issueID, label, actor string) error {
	if err := t.Transaction.RemoveLabel(ctx, issueID, label, actor); err != nil {
		return err
	}
	t.mark(issueID)
	return nil
}

func (t *exfiltrationTrackingTransaction) AddComment(ctx context.Context, issueID, actor, comment string) error {
	if err := t.Transaction.AddComment(ctx, issueID, actor, comment); err != nil {
		return err
	}
	t.mark(issueID)
	return nil
}

// DeleteIssue is a passthrough — when a brain doc is deleted via
// transaction, the markdown file is intentionally left for the
// reconciler to sweep. We do not have the (kind, slug) pair after
// the row is gone, and deferred reconciliation is the documented
// safety net (see divergence/0012 § Decisions).
func (t *exfiltrationTrackingTransaction) DeleteIssue(ctx context.Context, id string) error {
	return t.Transaction.DeleteIssue(ctx, id)
}

// ── Compile-time assertions ────────────────────────────────────────

var _ DoltStorage = (*BrainExfiltrationDecorator)(nil)
var _ Transaction = (*exfiltrationTrackingTransaction)(nil)

// Ensure BrainExfiltrationDecorator's mutation methods are used (not
// the embedded passthrough). Mirrors HookFiringStore's compile-time
// check pattern.
var (
	_ interface {
		CreateIssue(context.Context, *types.Issue, string) error
	} = (*BrainExfiltrationDecorator)(nil)
	_ interface {
		UpdateIssue(context.Context, string, map[string]interface{}, string) error
	} = (*BrainExfiltrationDecorator)(nil)
	_ interface {
		UpdateIssueType(context.Context, string, string, string) error
	} = (*BrainExfiltrationDecorator)(nil)
	_ interface {
		CloseIssue(context.Context, string, string, string, string) error
	} = (*BrainExfiltrationDecorator)(nil)
	_ interface {
		RunInTransaction(context.Context, string, func(Transaction) error) error
	} = (*BrainExfiltrationDecorator)(nil)
	_ interface {
		AddDependency(context.Context, *types.Dependency, string) error
	} = (*BrainExfiltrationDecorator)(nil)
	_ interface {
		AddLabel(context.Context, string, string, string) error
	} = (*BrainExfiltrationDecorator)(nil)
	_ interface {
		AddIssueComment(context.Context, string, string, string) (*types.Comment, error)
	} = (*BrainExfiltrationDecorator)(nil)
	_ interface {
		ReopenIssue(context.Context, string, string, string) error
	} = (*BrainExfiltrationDecorator)(nil)
)
