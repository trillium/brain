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
	"fmt"
	"io"
	"os"
	"time"

	"github.com/steveyegge/beads/internal/hooksdef"
	"github.com/steveyegge/beads/internal/types"
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
