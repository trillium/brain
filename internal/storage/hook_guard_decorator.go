// Package storage — hook_guard_decorator.go
//
// HookGuardStore is the decorator that makes declared hooks (hooks.d/*.toml,
// see internal/hooksdef) part of the write path. It sits closest to the raw
// store:
//
//	rawStore → HookGuardStore → HookFiringStore → BrainExfiltrationDecorator
//
// so a refusal happens before the legacy exec hooks fire and before anything
// is rendered, and nothing above it ever sees a write that did not happen.
//
//   - A guard hook runs BEFORE the mutation and may refuse it. The refusal is
//     returned as the mutation's error, naming the hook, and is the reason the
//     bead was never created.
//   - Observer and unset-policy hooks run AFTER the mutation (after commit,
//     inside a transaction). They can never undo or block it; a failure of
//     theirs becomes a durable warning record.
//
// The store is the only writer. Hooks receive a JSON description of the
// mutation and return an exit status; nothing in this file hands a hook a
// store handle, and the warning record is written here, by the store layer,
// through the inner store's own config table.
package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/hooksdef"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// HookExecutor runs one hook against one payload. The default is
// hooksdef.Run; tests substitute their own.
type HookExecutor func(ctx context.Context, def hooksdef.Definition, p hooksdef.Payload) hooksdef.RunResult

// HookGuardOptions carries the identity facts a hook payload reports.
type HookGuardOptions struct {
	// Command is the bd command performing the mutation ("create", "close").
	Command string
	// Store is the store's logical name (never a path).
	Store string
	// Exec overrides the hook executor (tests). Nil means hooksdef.Run.
	Exec HookExecutor
	// Stderr receives the one-line notice for every recorded warning. Nil
	// means os.Stderr.
	Stderr io.Writer
	// Now overrides the clock (tests).
	Now func() time.Time
}

// hookEngine holds the policy logic shared by the top-level store and the
// transaction wrapper; only how the current row is read differs.
type hookEngine struct {
	defs    []hooksdef.Definition
	opts    HookGuardOptions
	inner   DoltStorage // warnings are written here, past the guard
	getBead func(ctx context.Context, id string) (*types.Issue, error)
}

// mutation describes one write for payload purposes.
type mutation struct {
	event   string
	subject string       // bead id when the mutation names one
	issue   *types.Issue // proposed row for creates
	updates map[string]interface{}
	reason  string
	actor   string
}

func (e *hookEngine) has(event string, guard bool) bool {
	for _, d := range e.defs {
		if d.MatchesEvent(event) && d.IsGuard() == guard {
			return true
		}
	}
	return false
}

// payload renders a mutation. For mutations on an existing bead the current
// row is read through getBead; fallback is used when it can no longer be read
// (after a delete).
func (e *hookEngine) payload(ctx context.Context, m mutation, fallback *hooksdef.PayloadIssue) hooksdef.Payload {
	p := hooksdef.Payload{
		Event: m.event, Command: e.opts.Command, Store: e.opts.Store,
		Updates: m.updates, Reason: m.reason, Actor: m.actor,
	}
	switch {
	case m.issue != nil:
		p.Issue = payloadIssue(m.issue)
	case m.subject != "":
		if cur, err := e.getBead(ctx, m.subject); err == nil && cur != nil {
			p.Issue = payloadIssue(cur)
		} else {
			p.Issue = fallback
		}
		if p.Issue == nil {
			p.Issue = &hooksdef.PayloadIssue{ID: m.subject}
		}
	}
	return p
}

func payloadIssue(i *types.Issue) *hooksdef.PayloadIssue {
	return &hooksdef.PayloadIssue{
		ID: i.ID, Title: i.Title, Description: i.Description,
		Status: string(i.Status), Priority: i.Priority, IssueType: string(i.IssueType),
		Assignee: i.Assignee, Labels: append([]string(nil), i.Labels...), CreatedBy: i.CreatedBy,
	}
}

func (e *hookEngine) exec(ctx context.Context, def hooksdef.Definition, p hooksdef.Payload) hooksdef.RunResult {
	if e.opts.Exec != nil {
		return e.opts.Exec(ctx, def, p)
	}
	return hooksdef.Run(ctx, def, p)
}

// guard runs every matching guard hook before the mutation. The first
// refusal wins and is returned as *hooksdef.Refusal.
func (e *hookEngine) guard(ctx context.Context, m mutation) (hooksdef.Payload, error) {
	if !e.has(m.event, true) {
		// A delete's observers cannot read the row afterwards; snapshot it now.
		if m.event == hooksdef.EventDelete && e.has(m.event, false) {
			return e.payload(ctx, m, nil), nil
		}
		return hooksdef.Payload{}, nil
	}
	p := e.payload(ctx, m, nil)
	for _, def := range e.defs {
		if !def.IsGuard() || !def.MatchesEvent(m.event) {
			continue
		}
		if d := hooksdef.Decide(def, m.event, e.exec(ctx, def, p)); d.Refusal != nil {
			return p, d.Refusal
		}
	}
	return p, nil
}

// observe runs every matching non-guard hook after the mutation and records a
// durable warning for each that degraded.
func (e *hookEngine) observe(ctx context.Context, p hooksdef.Payload) {
	for _, def := range e.defs {
		if def.IsGuard() || !def.MatchesEvent(p.Event) {
			continue
		}
		d := hooksdef.Decide(def, p.Event, e.exec(ctx, def, p))
		if d.Warning != nil {
			e.record(ctx, d.Warning, p)
		}
	}
}

// record writes a warning as a config-table row on the inner store and says so
// on stderr. If the record itself cannot be written the failure is announced
// loudly on stderr, with the original warning inline, because the mutation has
// already happened and returning an error would claim it had not.
func (e *hookEngine) record(ctx context.Context, w *hooksdef.Warning, p hooksdef.Payload) {
	out := e.opts.Stderr
	if out == nil {
		out = os.Stderr
	}
	now := time.Now
	if e.opts.Now != nil {
		now = e.opts.Now
	}
	w.CreatedAt = now().UTC()
	w.ID = hooksdef.NewWarningID(w.CreatedAt)
	w.Command = e.opts.Command
	if p.Issue != nil {
		w.Subject = p.Issue.ID
	}
	enc, err := w.Encode()
	if err == nil {
		err = e.inner.SetConfig(ctx, hooksdef.WarningKey(w.ID), enc)
	}
	if err != nil {
		fmt.Fprintf(out, "bd: HOOK WARNING NOT RECORDED (%v): hook %q [%s]: %s\n", err, w.Hook, w.Reason, w.Detail)
		return
	}
	fmt.Fprintf(out, "bd: hook warning %s [%s] hook %q: %s (review: bd hook warnings; acknowledge: bd hook warnings --ack %s)\n",
		w.ID, w.Reason, w.Hook, w.Detail, w.ID)
}

// HookGuardStore decorates a DoltStorage with declared guard/observer hooks.
type HookGuardStore struct {
	DoltStorage // passthrough for everything that is not a guarded mutation
	inner       DoltStorage
	eng         *hookEngine
}

// NewHookGuardStore wraps store. With no definitions it is a plain passthrough
// and callers should not wrap at all.
func NewHookGuardStore(store DoltStorage, defs []hooksdef.Definition, opts HookGuardOptions) *HookGuardStore {
	return &HookGuardStore{
		DoltStorage: store,
		inner:       store,
		eng:         &hookEngine{defs: defs, opts: opts, inner: store, getBead: store.GetIssue},
	}
}

// Inner returns the wrapped store (see UnwrapStore).
func (g *HookGuardStore) Inner() DoltStorage { return g.inner }

// Unwrap returns the underlying store, satisfying Unwrapper so UnwrapStore
// walks through this decorator to the concrete store.
func (g *HookGuardStore) Unwrap() DoltStorage { return g.inner }

// mutate is the whole top-level contract: guard, write, observe.
func (g *HookGuardStore) mutate(ctx context.Context, m mutation, op func() error) error {
	pre, err := g.eng.guard(ctx, m)
	if err != nil {
		return err
	}
	if err := op(); err != nil {
		return err
	}
	if g.eng.has(m.event, false) {
		g.eng.observe(ctx, g.eng.payload(ctx, m, pre.Issue))
	}
	return nil
}

func (g *HookGuardStore) CreateIssue(ctx context.Context, issue *types.Issue, actor string) error {
	return g.mutate(ctx, mutation{event: hooksdef.EventCreate, issue: issue, actor: actor},
		func() error { return g.inner.CreateIssue(ctx, issue, actor) })
}

// CreateIssues guards each proposed bead; the first refusal refuses the batch
// before any of it is written.
func (g *HookGuardStore) CreateIssues(ctx context.Context, issues []*types.Issue, actor string) error {
	return g.createBatch(ctx, issues, actor, func() error { return g.inner.CreateIssues(ctx, issues, actor) })
}

func (g *HookGuardStore) CreateIssuesWithFullOptions(ctx context.Context, issues []*types.Issue, actor string, opts BatchCreateOptions) error {
	return g.createBatch(ctx, issues, actor, func() error {
		return g.inner.CreateIssuesWithFullOptions(ctx, issues, actor, opts)
	})
}

func (g *HookGuardStore) createBatch(ctx context.Context, issues []*types.Issue, actor string, op func() error) error {
	for _, issue := range issues {
		if _, err := g.eng.guard(ctx, mutation{event: hooksdef.EventCreate, issue: issue, actor: actor}); err != nil {
			return err
		}
	}
	if err := op(); err != nil {
		return err
	}
	if g.eng.has(hooksdef.EventCreate, false) {
		for _, issue := range issues {
			g.eng.observe(ctx, g.eng.payload(ctx, mutation{event: hooksdef.EventCreate, issue: issue, actor: actor}, nil))
		}
	}
	return nil
}

func (g *HookGuardStore) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	return g.mutate(ctx, mutation{event: hooksdef.EventUpdate, subject: id, updates: updates, actor: actor},
		func() error { return g.inner.UpdateIssue(ctx, id, updates, actor) })
}

func (g *HookGuardStore) ReopenIssue(ctx context.Context, id string, reason string, actor string) error {
	return g.mutate(ctx, mutation{event: hooksdef.EventUpdate, subject: id, reason: reason, actor: actor,
		updates: map[string]interface{}{"reopen": true}},
		func() error { return g.inner.ReopenIssue(ctx, id, reason, actor) })
}

func (g *HookGuardStore) UpdateIssueType(ctx context.Context, id string, issueType string, actor string) error {
	return g.mutate(ctx, mutation{event: hooksdef.EventUpdate, subject: id, actor: actor,
		updates: map[string]interface{}{"issue_type": issueType}},
		func() error { return g.inner.UpdateIssueType(ctx, id, issueType, actor) })
}

func (g *HookGuardStore) CloseIssue(ctx context.Context, id string, reason string, actor string, session string) error {
	return g.mutate(ctx, mutation{event: hooksdef.EventClose, subject: id, reason: reason, actor: actor},
		func() error { return g.inner.CloseIssue(ctx, id, reason, actor, session) })
}

func (g *HookGuardStore) DeleteIssue(ctx context.Context, id string) error {
	return g.mutate(ctx, mutation{event: hooksdef.EventDelete, subject: id},
		func() error { return g.inner.DeleteIssue(ctx, id) })
}

// ClaimIssue is an update: `bd update --claim` must not be a way around an
// update guard.
func (g *HookGuardStore) ClaimIssue(ctx context.Context, id string, actor string) error {
	return g.mutate(ctx, mutation{event: hooksdef.EventUpdate, subject: id, actor: actor,
		updates: map[string]interface{}{"claim": true}},
		func() error { return g.inner.ClaimIssue(ctx, id, actor) })
}

// ClaimReadyIssue picks its bead inside the store, so the guard sees the
// request (the filter) and not yet a subject; observers see the claimed bead.
func (g *HookGuardStore) ClaimReadyIssue(ctx context.Context, filter types.WorkFilter, actor string) (*types.Issue, error) {
	m := mutation{event: hooksdef.EventUpdate, actor: actor,
		updates: map[string]interface{}{"claim": true, "ready": true}}
	if _, err := g.eng.guard(ctx, m); err != nil {
		return nil, err
	}
	claimed, err := g.inner.ClaimReadyIssue(ctx, filter, actor)
	if err != nil || claimed == nil {
		return claimed, err
	}
	if g.eng.has(m.event, false) {
		m.subject = claimed.ID
		g.eng.observe(ctx, g.eng.payload(ctx, m, nil))
	}
	return claimed, nil
}

// DeleteIssues guards each id as a delete; the first refusal refuses the batch
// before anything is removed. A dry run changes nothing and is not guarded.
func (g *HookGuardStore) DeleteIssues(ctx context.Context, ids []string, cascade bool, force bool, dryRun bool) (*types.DeleteIssuesResult, error) {
	if dryRun {
		return g.inner.DeleteIssues(ctx, ids, cascade, force, dryRun)
	}
	snapshots := make([]hooksdef.Payload, 0, len(ids))
	for _, id := range ids {
		pre, err := g.eng.guard(ctx, mutation{event: hooksdef.EventDelete, subject: id,
			updates: map[string]interface{}{"cascade": cascade}})
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, pre)
	}
	res, err := g.inner.DeleteIssues(ctx, ids, cascade, force, dryRun)
	if err != nil {
		return res, err
	}
	if g.eng.has(hooksdef.EventDelete, false) {
		for i, id := range ids {
			p := snapshots[i]
			if p.Event == "" {
				p = g.eng.payload(ctx, mutation{event: hooksdef.EventDelete, subject: id}, nil)
			}
			g.eng.observe(ctx, p)
		}
	}
	return res, nil
}

// UpdateIssueID (`bd rename`) is an update of the bead being renamed.
func (g *HookGuardStore) UpdateIssueID(ctx context.Context, oldID, newID string, issue *types.Issue, actor string) error {
	return g.mutate(ctx, mutation{event: hooksdef.EventUpdate, subject: oldID, actor: actor,
		updates: map[string]interface{}{"rename_to": newID}},
		func() error { return g.inner.UpdateIssueID(ctx, oldID, newID, issue, actor) })
}

func (g *HookGuardStore) AddDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	return g.mutate(ctx, depMutation(dep, true, actor),
		func() error { return g.inner.AddDependency(ctx, dep, actor) })
}

func (g *HookGuardStore) RemoveDependency(ctx context.Context, issueID, dependsOnID string, actor string) error {
	return g.mutate(ctx, depMutation(&types.Dependency{IssueID: issueID, DependsOnID: dependsOnID}, false, actor),
		func() error { return g.inner.RemoveDependency(ctx, issueID, dependsOnID, actor) })
}

func (g *HookGuardStore) AddLabel(ctx context.Context, issueID, label, actor string) error {
	return g.mutate(ctx, mutation{event: hooksdef.EventUpdate, subject: issueID, actor: actor,
		updates: map[string]interface{}{"add_label": label}},
		func() error { return g.inner.AddLabel(ctx, issueID, label, actor) })
}

func (g *HookGuardStore) RemoveLabel(ctx context.Context, issueID, label, actor string) error {
	return g.mutate(ctx, mutation{event: hooksdef.EventUpdate, subject: issueID, actor: actor,
		updates: map[string]interface{}{"remove_label": label}},
		func() error { return g.inner.RemoveLabel(ctx, issueID, label, actor) })
}

func (g *HookGuardStore) AddIssueComment(ctx context.Context, issueID, author, text string) (*types.Comment, error) {
	var c *types.Comment
	err := g.mutate(ctx, mutation{event: hooksdef.EventUpdate, subject: issueID, actor: author,
		updates: map[string]interface{}{"add_comment": text}},
		func() (err error) { c, err = g.inner.AddIssueComment(ctx, issueID, author, text); return err })
	if err != nil {
		return nil, err
	}
	return c, nil
}

func depMutation(dep *types.Dependency, add bool, actor string) mutation {
	key := "remove_dependency"
	if add {
		key = "add_dependency"
	}
	return mutation{event: hooksdef.EventUpdate, subject: dep.IssueID, actor: actor,
		updates: map[string]interface{}{key: map[string]interface{}{
			"depends_on_id": dep.DependsOnID, "type": string(dep.Type)}}}
}

// RunInTransaction guards every mutation inside the transaction as it is
// attempted — a refusal makes fn fail and the transaction roll back — and
// runs observers only after the transaction has committed.
func (g *HookGuardStore) RunInTransaction(ctx context.Context, commitMsg string, fn func(tx Transaction) error) error {
	var wrapped *hookGuardTx
	err := g.inner.RunInTransaction(ctx, commitMsg, func(tx Transaction) error {
		wrapped = &hookGuardTx{
			Transaction: tx,
			eng:         &hookEngine{defs: g.eng.defs, opts: g.eng.opts, inner: g.inner, getBead: tx.GetIssue},
		}
		return fn(wrapped)
	})
	if err != nil || wrapped == nil {
		return err
	}
	for _, p := range wrapped.pending {
		g.eng.observe(ctx, p)
	}
	return nil
}

// hookGuardTx guards a Transaction's mutations and queues observer payloads
// for after commit.
type hookGuardTx struct {
	Transaction
	eng     *hookEngine
	pending []hooksdef.Payload
}

func (t *hookGuardTx) mutate(ctx context.Context, m mutation, op func() error) error {
	pre, err := t.eng.guard(ctx, m)
	if err != nil {
		return err
	}
	if err := op(); err != nil {
		return err
	}
	if t.eng.has(m.event, false) {
		t.pending = append(t.pending, t.eng.payload(ctx, m, pre.Issue))
	}
	return nil
}

func (t *hookGuardTx) CreateIssue(ctx context.Context, issue *types.Issue, actor string) error {
	return t.mutate(ctx, mutation{event: hooksdef.EventCreate, issue: issue, actor: actor},
		func() error { return t.Transaction.CreateIssue(ctx, issue, actor) })
}

func (t *hookGuardTx) CreateIssues(ctx context.Context, issues []*types.Issue, actor string) error {
	for _, issue := range issues {
		if _, err := t.eng.guard(ctx, mutation{event: hooksdef.EventCreate, issue: issue, actor: actor}); err != nil {
			return err
		}
	}
	if err := t.Transaction.CreateIssues(ctx, issues, actor); err != nil {
		return err
	}
	if t.eng.has(hooksdef.EventCreate, false) {
		for _, issue := range issues {
			t.pending = append(t.pending, t.eng.payload(ctx, mutation{event: hooksdef.EventCreate, issue: issue, actor: actor}, nil))
		}
	}
	return nil
}

func (t *hookGuardTx) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	return t.mutate(ctx, mutation{event: hooksdef.EventUpdate, subject: id, updates: updates, actor: actor},
		func() error { return t.Transaction.UpdateIssue(ctx, id, updates, actor) })
}

func (t *hookGuardTx) CloseIssue(ctx context.Context, id string, reason string, actor string, session string) error {
	return t.mutate(ctx, mutation{event: hooksdef.EventClose, subject: id, reason: reason, actor: actor},
		func() error { return t.Transaction.CloseIssue(ctx, id, reason, actor, session) })
}

func (t *hookGuardTx) DeleteIssue(ctx context.Context, id string) error {
	return t.mutate(ctx, mutation{event: hooksdef.EventDelete, subject: id},
		func() error { return t.Transaction.DeleteIssue(ctx, id) })
}

func (t *hookGuardTx) AddDependency(ctx context.Context, dep *types.Dependency, actor string) error {
	return t.mutate(ctx, depMutation(dep, true, actor),
		func() error { return t.Transaction.AddDependency(ctx, dep, actor) })
}

func (t *hookGuardTx) AddDependencyWithOptions(ctx context.Context, dep *types.Dependency, actor string, opts DependencyAddOptions) error {
	return t.mutate(ctx, depMutation(dep, true, actor),
		func() error { return t.Transaction.AddDependencyWithOptions(ctx, dep, actor, opts) })
}

func (t *hookGuardTx) RemoveDependency(ctx context.Context, issueID, dependsOnID string, actor string) error {
	return t.mutate(ctx, depMutation(&types.Dependency{IssueID: issueID, DependsOnID: dependsOnID}, false, actor),
		func() error { return t.Transaction.RemoveDependency(ctx, issueID, dependsOnID, actor) })
}

func (t *hookGuardTx) AddLabel(ctx context.Context, issueID, label, actor string) error {
	return t.mutate(ctx, mutation{event: hooksdef.EventUpdate, subject: issueID, actor: actor,
		updates: map[string]interface{}{"add_label": label}},
		func() error { return t.Transaction.AddLabel(ctx, issueID, label, actor) })
}

func (t *hookGuardTx) RemoveLabel(ctx context.Context, issueID, label, actor string) error {
	return t.mutate(ctx, mutation{event: hooksdef.EventUpdate, subject: issueID, actor: actor,
		updates: map[string]interface{}{"remove_label": label}},
		func() error { return t.Transaction.RemoveLabel(ctx, issueID, label, actor) })
}

func (t *hookGuardTx) AddComment(ctx context.Context, issueID, actor, comment string) error {
	return t.mutate(ctx, mutation{event: hooksdef.EventUpdate, subject: issueID, actor: actor,
		updates: map[string]interface{}{"add_comment": comment}},
		func() error { return t.Transaction.AddComment(ctx, issueID, actor, comment) })
}

func (t *hookGuardTx) ImportIssueComment(ctx context.Context, issueID, author, text string, createdAt time.Time) (*types.Comment, error) {
	var c *types.Comment
	err := t.mutate(ctx, mutation{event: hooksdef.EventUpdate, subject: issueID, actor: author,
		updates: map[string]interface{}{"add_comment": text}},
		func() (err error) {
			c, err = t.Transaction.ImportIssueComment(ctx, issueID, author, text, createdAt)
			return err
		})
	if err != nil {
		return nil, err
	}
	return c, nil
}

var (
	_ DoltStorage = (*HookGuardStore)(nil)
	_ Transaction = (*hookGuardTx)(nil)
)

// ── Mutation methods beads 1.3 added to DoltStorage ─────────────────

func (g *HookGuardStore) UpdateIssueChecked(ctx context.Context, id string, updates map[string]interface{}, actor string, opts UpdateIssueOptions) error {
	return g.mutate(ctx, mutation{event: hooksdef.EventUpdate, subject: id, updates: updates, actor: actor},
		func() error { return g.inner.UpdateIssueChecked(ctx, id, updates, actor, opts) })
}

func (g *HookGuardStore) CloseIssueChecked(ctx context.Context, id string, actor string, opts CloseIssueOptions) (CloseIssueResult, error) {
	var res CloseIssueResult
	err := g.mutate(ctx, mutation{event: hooksdef.EventClose, subject: id, actor: actor},
		func() (err error) { res, err = g.inner.CloseIssueChecked(ctx, id, actor, opts); return err })
	return res, err
}

// UnclaimIssue is an update, like ClaimIssue.
func (g *HookGuardStore) UnclaimIssue(ctx context.Context, id string, actor string, force bool) error {
	return g.mutate(ctx, mutation{event: hooksdef.EventUpdate, subject: id, actor: actor,
		updates: map[string]interface{}{"unclaim": true}},
		func() error { return g.inner.UnclaimIssue(ctx, id, actor, force) })
}

func (g *HookGuardStore) UnclaimIssueIfAssignee(ctx context.Context, id string, actor string, expectedAssignee string) error {
	return g.mutate(ctx, mutation{event: hooksdef.EventUpdate, subject: id, actor: actor,
		updates: map[string]interface{}{"unclaim": true}},
		func() error { return g.inner.UnclaimIssueIfAssignee(ctx, id, actor, expectedAssignee) })
}

func (g *HookGuardStore) MergeMetadata(ctx context.Context, issueID, key string, value json.RawMessage, actor string) error {
	return g.mutate(ctx, mutation{event: hooksdef.EventUpdate, subject: issueID, actor: actor,
		updates: map[string]interface{}{"metadata_key": key}},
		func() error { return g.inner.MergeMetadata(ctx, issueID, key, value, actor) })
}

func (g *HookGuardStore) AddDependencyWithOptions(ctx context.Context, dep *types.Dependency, actor string, opts DependencyAddOptions) error {
	return g.mutate(ctx, depMutation(dep, true, actor),
		func() error { return g.inner.AddDependencyWithOptions(ctx, dep, actor, opts) })
}

func (g *HookGuardStore) RemoveDependencyWithOptions(ctx context.Context, issueID, dependsOnID string, actor string, opts DependencyRemoveOptions) error {
	return g.mutate(ctx, depMutation(&types.Dependency{IssueID: issueID, DependsOnID: dependsOnID}, false, actor),
		func() error { return g.inner.RemoveDependencyWithOptions(ctx, issueID, dependsOnID, actor, opts) })
}

// RunInIssueLifecycleTransaction guards the transaction's mutations the way
// RunInTransaction does, plus its reopens.
func (g *HookGuardStore) RunInIssueLifecycleTransaction(ctx context.Context, commitMsg string, fn func(tx IssueLifecycleTransaction) error) error {
	var wrapped *hookGuardLifecycleTx
	err := g.inner.RunInIssueLifecycleTransaction(ctx, commitMsg, func(tx IssueLifecycleTransaction) error {
		wrapped = &hookGuardLifecycleTx{
			hookGuardTx: &hookGuardTx{
				Transaction: tx,
				eng:         &hookEngine{defs: g.eng.defs, opts: g.eng.opts, inner: g.inner, getBead: tx.GetIssue},
			},
			reopener: tx,
		}
		return fn(wrapped)
	})
	if err != nil || wrapped == nil {
		return err
	}
	for _, p := range wrapped.pending {
		g.eng.observe(ctx, p)
	}
	return nil
}

type hookGuardLifecycleTx struct {
	*hookGuardTx
	reopener IssueLifecycleTransaction
}

func (t *hookGuardLifecycleTx) ReopenIssueWithResult(ctx context.Context, id string, reason string, actor string) (bool, error) {
	var reopened bool
	err := t.mutate(ctx, mutation{event: hooksdef.EventUpdate, subject: id, reason: reason, actor: actor,
		updates: map[string]interface{}{"reopen": true}},
		func() (err error) {
			reopened, err = t.reopener.ReopenIssueWithResult(ctx, id, reason, actor)
			return err
		})
	return reopened, err
}

// ── Write roles (beads 1.3) ─────────────────────────────────────────
//
// Upstream 1.3 routes the CLI's writes through role values taken off the
// store (IssueLifecycle, IssueClaimer, BatchCloser, …) instead of the
// DoltStorage mutation methods above. A role taken through the embedded
// DoltStorage would be the inner store's, unguarded, so every write role is
// wrapped here: each request is guarded the way the matching DoltStorage
// method is, and observers run after it lands. The two roles whose requests
// cannot be mapped to per-bead events (BatchApplier, Sweeper) refuse to run
// while a hook is declared for an event they could perform, rather than
// write past it.

// IssueLifecycle guards create, update, close and reopen.
func (g *HookGuardStore) IssueLifecycle() (issueops.Lifecycle, error) {
	inner, err := g.inner.IssueLifecycle()
	if err != nil {
		return nil, err
	}
	return &guardLifecycle{inner: inner, g: g}, nil
}

// IssueClaimer guards a claim as an update, like ClaimIssue.
func (g *HookGuardStore) IssueClaimer() (issueops.Claimer, error) {
	inner, err := g.inner.IssueClaimer()
	if err != nil {
		return nil, err
	}
	return &guardClaimer{inner: inner, g: g}, nil
}

// ReadyClaimer guards a claim-next as an update, like ClaimReadyIssue.
func (g *HookGuardStore) ReadyClaimer() (issueops.ReadyClaimer, error) {
	inner, err := g.inner.ReadyClaimer()
	if err != nil {
		return nil, err
	}
	return &guardReadyClaimer{inner: inner, g: g}, nil
}

// Commenter guards a comment as an update, like AddIssueComment.
func (g *HookGuardStore) Commenter() (issueops.Commenter, error) {
	inner, err := g.inner.Commenter()
	if err != nil {
		return nil, err
	}
	return &guardCommenter{inner: inner, g: g}, nil
}

// DependencyEditor guards each edge as an update of its source, like
// AddDependency and RemoveDependency.
func (g *HookGuardStore) DependencyEditor() (issueops.DependencyEditor, error) {
	inner, err := g.inner.DependencyEditor()
	if err != nil {
		return nil, err
	}
	return &guardDependencyEditor{inner: inner, g: g}, nil
}

// BatchCloser guards each item as a close; the first refusal refuses the batch.
func (g *HookGuardStore) BatchCloser() (issueops.BatchCloser, error) {
	inner, err := g.inner.BatchCloser()
	if err != nil {
		return nil, err
	}
	return &guardBatchCloser{inner: inner, g: g}, nil
}

// BatchCreator guards each item as a create; the first refusal refuses the batch.
func (g *HookGuardStore) BatchCreator() (issueops.BatchCreator, error) {
	inner, err := g.inner.BatchCreator()
	if err != nil {
		return nil, err
	}
	return &guardBatchCreator{inner: inner, g: g}, nil
}

// MetadataCAS guards a compare-and-set as an update of its bead.
func (g *HookGuardStore) MetadataCAS() (issueops.MetadataCAS, error) {
	inner, err := g.inner.MetadataCAS()
	if err != nil {
		return nil, err
	}
	return &guardMetadataCAS{inner: inner, g: g}, nil
}

// Releaser guards a release as an update of its bead.
func (g *HookGuardStore) Releaser() (issueops.Releaser, error) {
	inner, err := g.inner.Releaser()
	if err != nil {
		return nil, err
	}
	return &guardReleaser{inner: inner, g: g}, nil
}

// Deleter guards each id as a delete, like DeleteIssues.
func (g *HookGuardStore) Deleter() (issueops.Deleter, error) {
	inner, err := g.inner.Deleter()
	if err != nil {
		return nil, err
	}
	return &guardDeleter{inner: inner, g: g}, nil
}

// BatchApplier performs creates, updates, closes and edge edits in one
// request, so it refuses while any hook is declared for those events.
func (g *HookGuardStore) BatchApplier() (issueops.BatchApplier, error) {
	inner, err := g.inner.BatchApplier()
	if err != nil {
		return nil, err
	}
	return &guardBatchApplier{inner: inner, g: g}, nil
}

// Sweeper deletes the beads a tier selects, so it refuses while any hook is
// declared for deletes.
func (g *HookGuardStore) Sweeper() (issueops.Sweeper, error) {
	inner, err := g.inner.Sweeper()
	if err != nil {
		return nil, err
	}
	return &guardSweeper{inner: inner, g: g}, nil
}

// refuseUnguardable is the fail-closed answer of a role that cannot run
// declared hooks per bead.
func (g *HookGuardStore) refuseUnguardable(role string, events ...string) error {
	for _, ev := range events {
		if g.eng.has(ev, true) || g.eng.has(ev, false) {
			return fmt.Errorf("refusing to write: %s cannot run declared hooks bead by bead, and a hook is declared for %q events (hooks.d); run the equivalent single-bead commands instead", role, ev)
		}
	}
	return nil
}

// patchUpdates renders the fields an IssuePatch sets, for the hook payload's
// updates map. Values are the patch's own; the map is descriptive only.
func patchUpdates(p issueops.IssuePatch) map[string]interface{} {
	out := map[string]interface{}{}
	v := reflect.ValueOf(p)
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := v.Field(i)
		name := snakeCase(t.Field(i).Name)
		switch val := f.Interface().(type) {
		case issueops.LabelPatch:
			if len(val.Add) > 0 {
				out["add_labels"] = val.Add
			}
			if len(val.Remove) > 0 {
				out["remove_labels"] = val.Remove
			}
			if val.Replace.Set {
				out["labels"] = val.Replace.Value
			}
		case issueops.MetadataPatch:
			if !reflect.ValueOf(val).IsZero() {
				out["metadata"] = true
			}
		default:
			if f.Kind() != reflect.Struct {
				continue
			}
			set := f.FieldByName("Set")
			value := f.FieldByName("Value")
			if set.IsValid() && value.IsValid() && set.Kind() == reflect.Bool && set.Bool() {
				out[name] = value.Interface()
			}
		}
	}
	return out
}

func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

type guardLifecycle struct {
	inner issueops.Lifecycle
	g     *HookGuardStore
}

func (l *guardLifecycle) Create(ctx context.Context, req issueops.CreateRequest) (res issueops.CreateResult, err error) {
	m := mutation{event: hooksdef.EventCreate, issue: req.Issue, actor: req.Actor}
	err = l.g.mutate(ctx, m, func() (e error) { res, e = l.inner.Create(ctx, req); return e })
	return res, err
}

func (l *guardLifecycle) Update(ctx context.Context, req issueops.UpdateRequest) (res issueops.UpdateResult, err error) {
	updates := patchUpdates(req.Patch)
	if req.Claim {
		updates["claim"] = true
	}
	m := mutation{event: hooksdef.EventUpdate, subject: req.IssueID, updates: updates, actor: req.Actor}
	err = l.g.mutate(ctx, m, func() (e error) { res, e = l.inner.Update(ctx, req); return e })
	return res, err
}

func (l *guardLifecycle) Close(ctx context.Context, req issueops.CloseRequest) (res issueops.CloseResult, err error) {
	m := mutation{event: hooksdef.EventClose, subject: req.IssueID, reason: req.Reason, actor: req.Actor}
	err = l.g.mutate(ctx, m, func() (e error) { res, e = l.inner.Close(ctx, req); return e })
	return res, err
}

func (l *guardLifecycle) Reopen(ctx context.Context, req issueops.ReopenRequest) (res issueops.ReopenResult, err error) {
	m := mutation{event: hooksdef.EventUpdate, subject: req.IssueID, reason: req.Reason, actor: req.Actor,
		updates: map[string]interface{}{"reopen": true}}
	err = l.g.mutate(ctx, m, func() (e error) { res, e = l.inner.Reopen(ctx, req); return e })
	return res, err
}

type guardClaimer struct {
	inner issueops.Claimer
	g     *HookGuardStore
}

func (c *guardClaimer) Claim(ctx context.Context, req issueops.ClaimRequest) (res issueops.ClaimResult, err error) {
	m := mutation{event: hooksdef.EventUpdate, subject: req.IssueID, actor: req.Actor,
		updates: map[string]interface{}{"claim": true}}
	err = c.g.mutate(ctx, m, func() (e error) { res, e = c.inner.Claim(ctx, req); return e })
	return res, err
}

type guardReadyClaimer struct {
	inner issueops.ReadyClaimer
	g     *HookGuardStore
}

// ClaimNext picks its bead inside the store, so the guard sees the request and
// not yet a subject; observers see the claimed bead.
func (c *guardReadyClaimer) ClaimNext(ctx context.Context, req issueops.ClaimNextRequest) (issueops.ClaimNextResult, error) {
	m := mutation{event: hooksdef.EventUpdate, actor: req.Actor,
		updates: map[string]interface{}{"claim": true, "ready": true}}
	if _, err := c.g.eng.guard(ctx, m); err != nil {
		return issueops.ClaimNextResult{}, err
	}
	res, err := c.inner.ClaimNext(ctx, req)
	if err != nil || res.Claimed == nil {
		return res, err
	}
	if c.g.eng.has(m.event, false) {
		m.subject = res.Claimed.ID
		c.g.eng.observe(ctx, c.g.eng.payload(ctx, m, nil))
	}
	return res, nil
}

type guardCommenter struct {
	inner issueops.Commenter
	g     *HookGuardStore
}

func (c *guardCommenter) AddComment(ctx context.Context, req issueops.AddCommentRequest) (res issueops.AddCommentResult, err error) {
	m := mutation{event: hooksdef.EventUpdate, subject: req.IssueID, actor: req.Author,
		updates: map[string]interface{}{"add_comment": req.Text}}
	err = c.g.mutate(ctx, m, func() (e error) { res, e = c.inner.AddComment(ctx, req); return e })
	return res, err
}

type guardDependencyEditor struct {
	inner issueops.DependencyEditor
	g     *HookGuardStore
}

// AddDependencies guards every edge before any is written; observers run per
// edge after the request lands.
func (d *guardDependencyEditor) AddDependencies(ctx context.Context, req issueops.AddDependenciesRequest) (issueops.AddDependenciesResult, error) {
	muts := make([]mutation, 0, len(req.Edges))
	for _, e := range req.Edges {
		m := depMutation(&types.Dependency{IssueID: e.IssueID, DependsOnID: e.DependsOnID, Type: e.Type}, true, req.Actor)
		if _, err := d.g.eng.guard(ctx, m); err != nil {
			return issueops.AddDependenciesResult{}, err
		}
		muts = append(muts, m)
	}
	res, err := d.inner.AddDependencies(ctx, req)
	if err != nil {
		return res, err
	}
	for _, m := range muts {
		if d.g.eng.has(m.event, false) {
			d.g.eng.observe(ctx, d.g.eng.payload(ctx, m, nil))
		}
	}
	return res, nil
}

func (d *guardDependencyEditor) RemoveDependency(ctx context.Context, req issueops.RemoveDependencyRequest) (res issueops.RemoveDependencyResult, err error) {
	m := depMutation(&types.Dependency{IssueID: req.IssueID, DependsOnID: req.DependsOnID}, false, req.Actor)
	err = d.g.mutate(ctx, m, func() (e error) { res, e = d.inner.RemoveDependency(ctx, req); return e })
	return res, err
}

type guardBatchCloser struct {
	inner issueops.BatchCloser
	g     *HookGuardStore
}

func (c *guardBatchCloser) CloseBatch(ctx context.Context, req issueops.CloseBatchRequest) (issueops.CloseBatchResult, error) {
	muts := make([]mutation, 0, len(req.Items))
	for _, it := range req.Items {
		m := mutation{event: hooksdef.EventClose, subject: it.IssueID, reason: it.Reason, actor: req.Actor}
		if _, err := c.g.eng.guard(ctx, m); err != nil {
			return issueops.CloseBatchResult{}, err
		}
		muts = append(muts, m)
	}
	res, err := c.inner.CloseBatch(ctx, req)
	if err != nil {
		return res, err
	}
	if c.g.eng.has(hooksdef.EventClose, false) {
		for _, m := range muts {
			c.g.eng.observe(ctx, c.g.eng.payload(ctx, m, nil))
		}
	}
	return res, nil
}

type guardBatchCreator struct {
	inner issueops.BatchCreator
	g     *HookGuardStore
}

func (c *guardBatchCreator) CreateBatch(ctx context.Context, req issueops.CreateBatchRequest) (issueops.CreateBatchResult, error) {
	for _, it := range req.Items {
		if _, err := c.g.eng.guard(ctx, mutation{event: hooksdef.EventCreate, issue: it.Issue, actor: req.Actor}); err != nil {
			return issueops.CreateBatchResult{}, err
		}
	}
	res, err := c.inner.CreateBatch(ctx, req)
	if err != nil {
		return res, err
	}
	if c.g.eng.has(hooksdef.EventCreate, false) {
		for _, it := range req.Items {
			c.g.eng.observe(ctx, c.g.eng.payload(ctx, mutation{event: hooksdef.EventCreate, issue: it.Issue, actor: req.Actor}, nil))
		}
	}
	return res, nil
}

type guardMetadataCAS struct {
	inner issueops.MetadataCAS
	g     *HookGuardStore
}

func (c *guardMetadataCAS) CompareAndSetKey(ctx context.Context, req issueops.CompareAndSetKeyRequest) (res issueops.CompareAndSetKeyResult, err error) {
	m := mutation{event: hooksdef.EventUpdate, subject: req.IssueID, actor: req.Actor,
		updates: map[string]interface{}{"metadata_key": req.Key}}
	err = c.g.mutate(ctx, m, func() (e error) { res, e = c.inner.CompareAndSetKey(ctx, req); return e })
	return res, err
}

type guardReleaser struct {
	inner issueops.Releaser
	g     *HookGuardStore
}

func (r *guardReleaser) Release(ctx context.Context, req issueops.ReleaseRequest) (res issueops.ReleaseResult, err error) {
	m := mutation{event: hooksdef.EventUpdate, subject: req.IssueID, actor: req.Actor,
		updates: map[string]interface{}{"release": true}}
	err = r.g.mutate(ctx, m, func() (e error) { res, e = r.inner.Release(ctx, req); return e })
	return res, err
}

type guardDeleter struct {
	inner issueops.Deleter
	g     *HookGuardStore
}

// Delete guards each id before anything is removed. Observers see the
// snapshots taken before the delete, since the rows are gone afterwards.
func (d *guardDeleter) Delete(ctx context.Context, req issueops.DeleteRequest) (issueops.DeleteResult, error) {
	snapshots := make([]hooksdef.Payload, 0, len(req.IDs))
	for _, id := range req.IDs {
		pre, err := d.g.eng.guard(ctx, mutation{event: hooksdef.EventDelete, subject: id, actor: req.Actor})
		if err != nil {
			return issueops.DeleteResult{}, err
		}
		snapshots = append(snapshots, pre)
	}
	res, err := d.inner.Delete(ctx, req)
	if err != nil || res.DryRun {
		return res, err
	}
	if d.g.eng.has(hooksdef.EventDelete, false) {
		for i, id := range req.IDs {
			d.g.eng.observe(ctx, d.g.eng.payload(ctx, mutation{event: hooksdef.EventDelete, subject: id, actor: req.Actor}, snapshots[i].Issue))
		}
	}
	return res, nil
}

type guardBatchApplier struct {
	inner issueops.BatchApplier
	g     *HookGuardStore
}

func (a *guardBatchApplier) ApplyBatch(ctx context.Context, req issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
	if err := a.g.refuseUnguardable("batch apply", hooksdef.EventCreate, hooksdef.EventUpdate, hooksdef.EventClose, hooksdef.EventDelete); err != nil {
		return issueops.ApplyBatchResult{}, err
	}
	return a.inner.ApplyBatch(ctx, req)
}

type guardSweeper struct {
	inner issueops.Sweeper
	g     *HookGuardStore
}

func (s *guardSweeper) Sweep(ctx context.Context, req issueops.SweepRequest) (issueops.SweepResult, error) {
	if err := s.g.refuseUnguardable("sweep", hooksdef.EventDelete); err != nil {
		return issueops.SweepResult{}, err
	}
	return s.inner.Sweep(ctx, req)
}
