package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// The knowledge-kind guard (ISC-112, ISC-113; divergence/0048).
//
// brain stores facts and work on one issues table, told apart by issue_type:
// `knowledge` is a fact, `task` and bd's other work types are work, `both` is
// work that is also a fact. The work verbs — claim, close, and a `blocks`
// edge — mean nothing on a fact, and letting them through is how 172 closed
// and 2 blocked knowledge beads accumulated before this guard existed. These
// helpers are the one place the refusal is spelled; every route (direct and
// proxied) of `bd update --claim`, `bd close` and `bd dep` calls them, so the
// refusal reads the same wherever it fires.
//
// The guard is a command-layer policy, like the template and pinned fences
// beside it, not an engine rule: `bd batch`, the GitHub sync and the molecule
// auto-close keep writing through the store untouched, and --force waives it
// for triage (reopening a bead that was filed as knowledge by mistake, or
// closing a fact that was wrong).

// errKnowledgeKind is the sentinel every knowledge refusal matches with
// errors.Is, so a caller can tell this policy refusal from the others
// without reading its message.
var errKnowledgeKind = errors.New("knowledge beads take no work verbs")

// knowledgeKindError is one refusal: the verb that was attempted and the
// knowledge bead it was attempted on.
type knowledgeKindError struct {
	verb string
	id   string
}

func (e *knowledgeKindError) Error() string {
	return fmt.Sprintf("cannot %s %s: it is a knowledge bead (issue_type=knowledge), and knowledge is never claimed, closed or put on the blocking graph; pass --force to do it anyway", e.verb, e.id)
}

func (e *knowledgeKindError) Is(target error) bool { return target == errKnowledgeKind }

// refuseKnowledgeKind returns the refusal for verb on issue, or nil when the
// issue is not a knowledge bead, is unknown (a nil issue is not a verdict —
// the write that follows owns the existence check), or force waives the
// guard. verb is the imperative the message starts with: "claim", "close",
// "block", "block on".
func refuseKnowledgeKind(verb, id string, issue *types.Issue, force bool) error {
	if force || issue == nil || !issue.IssueType.IsKnowledge() {
		return nil
	}
	return &knowledgeKindError{verb: verb, id: id}
}

// refuseKnowledgeBlocksEdge applies the guard to one dependency edge. Only a
// `blocks` edge is work: every other type (related, parent-child,
// discovered-from, brain's own extends/learned-from) is a link knowledge may
// carry (ISC-116). Both ends are checked — a fact can neither be blocked
// nor block — through lookup, which answers nil for an id this process
// cannot read (an external ref, a wisp, a cross-store id); nil is not a
// verdict and the edge passes to the write.
func refuseKnowledgeBlocksEdge(edge issueops.DependencyEdge, lookup func(id string) *types.Issue, force bool) error {
	if force || edge.Type != types.DepBlocks {
		return nil
	}
	if err := refuseKnowledgeKind("block", edge.IssueID, lookup(edge.IssueID), force); err != nil {
		return err
	}
	return refuseKnowledgeKind("block on", edge.DependsOnID, lookup(edge.DependsOnID), force)
}

// hasBlocksEdge reports whether any edge is one the guard would read, so a
// route that must open a read to apply it can skip the read otherwise.
func hasBlocksEdge(edges []issueops.DependencyEdge) bool {
	for _, edge := range edges {
		if edge.Type == types.DepBlocks {
			return true
		}
	}
	return false
}

// storeIssueLookup is the direct route's lookup: one store, read by id. An
// id the store does not hold (or cannot read as an issue) answers nil.
func storeIssueLookup(ctx context.Context, st storage.DoltStorage) func(id string) *types.Issue {
	return func(id string) *types.Issue {
		if st == nil || IsExternalRef(id) {
			return nil
		}
		issue, err := st.GetIssue(ctx, id)
		if err != nil {
			return nil
		}
		return issue
	}
}

// endpointIssueLookup is the direct route's lookup for a two-store edge: the
// dependent is read from its own (write-intent) store and the blocker from
// the store its routing resolved, which may be nil when the id was carried
// across without resolving (a foreign prefix the fallback accepted).
func endpointIssueLookup(ctx context.Context, fromID string, fromStore storage.DoltStorage, toID string, toStore storage.DoltStorage) func(id string) *types.Issue {
	from, to := storeIssueLookup(ctx, fromStore), storeIssueLookup(ctx, toStore)
	return func(id string) *types.Issue {
		switch id {
		case fromID:
			return from(id)
		case toID:
			return to(id)
		}
		return nil
	}
}

// proxiedIssueLookup is the proxied route's lookup: one read-only unit of
// work for every id the guard asks about. release closes it; a provider that
// cannot open one yields a lookup that answers nil, so the guard stands
// aside and the write reports the provider's real error.
func proxiedIssueLookup(ctx context.Context) (lookup func(id string) *types.Issue, release func()) {
	if uowProvider == nil {
		return func(string) *types.Issue { return nil }, func() {}
	}
	uw, err := uowProvider.NewUOW(ctx)
	if err != nil {
		return func(string) *types.Issue { return nil }, func() {}
	}
	return func(id string) *types.Issue {
		if IsExternalRef(id) {
			return nil
		}
		issue, err := uw.IssueUseCase().GetIssue(ctx, id)
		if err != nil {
			return nil
		}
		return issue
	}, func() { uw.Close(ctx) }
}

// refuseKnowledgeBlocksEdgesProxied applies the guard to every edge of one
// proxied dep write, opening the read only when there is a blocks edge to
// read for.
func refuseKnowledgeBlocksEdgesProxied(ctx context.Context, edges []issueops.DependencyEdge, force bool) error {
	if force || !hasBlocksEdge(edges) {
		return nil
	}
	lookup, release := proxiedIssueLookup(ctx)
	defer release()
	for _, edge := range edges {
		if err := refuseKnowledgeBlocksEdge(edge, lookup, force); err != nil {
			return err
		}
	}
	return nil
}
