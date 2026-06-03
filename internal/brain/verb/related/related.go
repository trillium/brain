// Package related implements the `related <id> [--depth=N]` verb (surfaced
// as `brain related ...` when the binary is installed as `brain`).
//
// `related` is one of the verbs brain adds on top of the bd-renamed-as-brain
// binary (alongside `new` and `recast`). Unlike `new` — which translates
// brain vocabulary to an existing bd write path — `related` has no bd
// analogue. `bd dep list` prints a flat one-hop table; `related` does a
// depth-bounded BFS over the same `dependencies` table and returns the
// subgraph as a tree.
//
// See docs/brain/WHAT_IS_BRAIN.md § 4.3 for the behavioural spec (including
// the rendered sample tree) and the Given/When/Then scenarios this
// package's tests trace back to. See internal/brain/verb/verb.go for the
// seam (Decision #5, divergence/0003) this package plugs into. See
// divergence/0006 for the brain-IS-bd reframe that motivates the top-level
// hoist of brain verbs, and divergence/0009 for the initial landing notes.
//
// # Shape
//
// The package exports a tree node type, the verb's Args / Result, the
// narrow storage seam, the Verb impl, and the constructor:
//
//   - Node      — `{ID, Title, Kind, Closed, EdgeFromParent, Children,
//     AlreadyVisited}`; nodes in the result tree. The wrapper at
//     cmd/bd/related.go renders these into the indented box-drawing tree;
//     --json marshals the tree directly.
//   - Args      — `{ID, Depth}`; positional + flag-resolved input.
//   - Result    — `{Center *Node}`; the BFS root. Always non-nil on
//     success.
//   - RelatedStore — narrow interface (`GetIssue` + `GetDependenciesWithMetadata`)
//     so the verb is testable against a 2-method fake.
//   - Verb      — implements verb.BrainVerb[Args, Result].
//   - New(store) — constructor.
//
// # BFS, depth cap, deterministic ordering
//
// Run performs a true breadth-first search starting from args.ID. It
// processes the queue in FIFO order; each dequeued node has its OUTGOING
// neighbours fetched in one storage call. Children appear under their
// parent in deterministic order — sorted first by edge type (alphabetical),
// then by neighbour ID. Determinism matters because the rendered tree is
// human-readable output that downstream tooling (and Trillium's own muscle
// memory) compares across runs; non-deterministic ordering would noise up
// every diff.
//
// # Why outgoing edges only (not bidirectional)
//
// `dependencies.issue_id` is the "from" side; `dependencies.depends_on_id`
// is the "to" side. Run follows the from→to direction only (the same
// direction `bd dep list` prints by default and the same direction
// `brain link a b --learned-from` creates). This is the choice the spec
// hints at — the sample tree in WHAT_IS_BRAIN.md § 4.3 reads as outgoing
// — and it keeps the rendered subgraph small enough to fit on a phone
// screen, which is the explicit "phone-driven exploration" scenario.
// Bidirectional traversal would explode at common hub nodes (e.g. a popular
// `extends`-target that 30 other docs revise) and would make the tree
// unreadable on small screens. A future `--bidirectional` flag is
// conceivable but out of scope for this tranche.
//
// # Why cycle detection by visited-set (not depth-cap alone)
//
// A pure depth cap would let a cycle re-emit the same nodes at every
// remaining level — `A → B → A → B → A` at depth=4. The visited-set
// guarantees each node prints once in the tree; on the second appearance
// of a node, Run records it as a leaf with `AlreadyVisited=true` and does
// NOT recurse. This matches WHAT_IS_BRAIN.md § 4.3 scenario "cycle in the
// graph". The Cobra wrapper renders `(already visited)` after the
// edge-and-node line; --json sets `"already_visited": true` and emits an
// empty `children` array.
//
// # Why the verb returns a tree, not pre-rendered text
//
// The split is deliberate. The verb's job is the TRAVERSAL — what nodes
// are reachable, in what order, with what edges. The wrapper's job is
// the RENDERING — how the tree appears on stdout (indented box-drawing)
// versus --json (the recursive Node struct). This is a slight exception
// to "no business logic in the wrapper" (rendering is presentation, not
// business), and it lets the verb stay pure-Go testable while the wrapper
// owns the visual contract. The Node struct is the API surface between
// the two; both sides see the same shape.
//
// # Why a narrow RelatedStore interface (not storage.Storage)
//
// The verb needs exactly two operations: fetch one issue by ID (the
// existence probe at the center and to enrich each visited node with
// Title/Kind/Status) and fetch outgoing edges for one issue with their
// edge-type metadata. Depending on the full `storage.Storage` interface
// (~60 methods) would over-constrain the seam. `RelatedStore` is the
// smallest surface that lets `storage.Storage` be passed in production
// and a 2-method fake be passed in tests. This matches the
// modularity-first principle from Decision #5 and mirrors `link.LinkStore`
// and `newverb.IssueCreator`.
package related

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/steveyegge/beads/internal/brain/verb"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// DefaultDepth is the depth used when the Cobra wrapper does not pass a
// `--depth` flag. Run itself does NOT apply this default; it honours
// whatever Depth value Args carries (including 0, which is a documented
// scenario: print the center only). The wrapper at cmd/bd/related.go is
// the single source of the default so that callers constructing Args by
// hand (e.g. tests) can pin any value they like.
const DefaultDepth = 2

// Args carries the positional + flag-resolved inputs the Cobra wrapper
// parses. All fields are populated before Run is called; Run does not
// read flag state.
type Args struct {
	// ID is the center brain doc ID — the BFS root. Required.
	ID string

	// Depth is the BFS depth cap. 0 prints just the center. 1 prints the
	// center + direct outgoing neighbours. Higher values BFS further out.
	// Negative values are rejected.
	Depth int
}

// Node is a node in the result tree. The same struct represents both the
// center (root) and every descendant; the wrapper distinguishes them by
// whether EdgeFromParent is empty (center) or set (descendant).
type Node struct {
	// ID is the brain doc ID.
	ID string `json:"id"`

	// Title is the brain doc's headline. Populated from the issue row at
	// visit time so the wrapper does not need to re-fetch.
	Title string `json:"title"`

	// Kind is the brain doc's kind — one of "task" | "knowledge" | "both"
	// — read from `issues.issue_type`. The wrapper prints this as
	// `[kind=<value>]` after the title.
	Kind string `json:"kind"`

	// Closed is true iff the brain doc's Status is types.StatusClosed.
	// The wrapper prints `, closed` inside the `[kind=...]` bracket when
	// true. Knowledge docs are not "closed" in any workflow sense but
	// still surface their Status here for uniformity.
	Closed bool `json:"closed"`

	// EdgeFromParent is the dependency type that linked this node to its
	// parent in the BFS tree. Empty on the center (root) node. For
	// descendants it is one of the 18 well-known dependency types from
	// types.WellKnownDependencyTypes().
	EdgeFromParent string `json:"edge_from_parent,omitempty"`

	// Children are the next-depth-level neighbours, ordered by edge type
	// (alphabetical) then by neighbour ID. Empty slice — not nil — on
	// orphan / leaf nodes so --json output never elides the field.
	Children []*Node `json:"children"`

	// AlreadyVisited is true iff this node was reached via an edge whose
	// target was already visited at a shallower depth (or the same
	// depth, on a different path). When true, Children is empty and the
	// wrapper renders `(already visited)`. This is the cycle-pruning
	// signal documented in WHAT_IS_BRAIN.md § 4.3 scenario "cycle in
	// the graph".
	AlreadyVisited bool `json:"already_visited,omitempty"`
}

// Result is what the verb returns on success. Center is always non-nil
// for a successful Run (a missing center is reported as an error, not as
// a nil Center).
type Result struct {
	// Center is the BFS root — the node corresponding to args.ID.
	Center *Node `json:"center"`
}

// RelatedStore is the narrow seam Run needs from storage. Satisfied by
// the production *internal/storage.Storage implementations (which have
// both methods on the embedded Storage interface) and by the test-only
// fake in related_test.go.
//
// GetIssue is used to enrich each visited node with Title / Kind /
// Status. GetDependenciesWithMetadata is the BFS-expansion call — it
// returns each outgoing neighbour together with the edge type that links
// them, in one round trip.
type RelatedStore interface {
	GetIssue(ctx context.Context, id string) (*types.Issue, error)
	GetDependenciesWithMetadata(ctx context.Context, issueID string) ([]*types.IssueWithDependencyMetadata, error)
}

// Verb implements verb.BrainVerb[Args, Result].
type Verb struct {
	store RelatedStore
}

// New constructs a Verb bound to the given storage.
//
// store is the RelatedStore the verb reads existence + edges through.
// In cmd/bd/, this is the global `store` (typed *storage.DoltStorage
// but used through the narrower RelatedStore — DoltStorage embeds the
// core Storage interface which has both required methods). Unlike `new`
// and `link`, this verb does not take an actor: it performs no writes,
// so there is no audit trail to attribute.
func New(store RelatedStore) Verb {
	return Verb{store: store}
}

// Compile-time proof that Verb satisfies BrainVerb with the concrete
// types declared in this package. If a refactor breaks this assertion,
// the seam contract has been violated.
var _ verb.BrainVerb[Args, Result] = Verb{}

// Name returns the verb word as it appears on the CLI ("related"). The
// Cobra wrapper must use this as the first whitespace-delimited token
// of its Use field.
func (Verb) Name() string { return "related" }

// Run is the entire behaviour of `brain related <id> [--depth=N]`.
//
// Validation order matches the spec's Given/When/Then scenarios
// (WHAT_IS_BRAIN.md § 4.3):
//
//  1. Empty ID          → "brain doc id is required"
//  2. Negative Depth    → 'invalid depth: must be >= 0, got <n>'
//  3. Storage unwired   → "brain related: storage is not configured"
//  4. Center missing    → "brain doc <id> not found"
//     (scenario "nonexistent center" — uses storage.ErrNotFound
//     check to distinguish missing-doc from transport failure)
//
// On success, Run performs a BFS from args.ID following only the
// outgoing direction of the `dependencies` table, capped at args.Depth
// hops, with a visited-set to prune cycles. Children at each level are
// sorted by (edge type, neighbour ID) so output is byte-identical across
// runs against the same data.
//
// Run never writes to stdout/stderr — that is the wrapper's job.
func (v Verb) Run(ctx context.Context, args Args) (Result, error) {
	if args.ID == "" {
		return Result{}, errors.New("brain doc id is required")
	}
	if args.Depth < 0 {
		return Result{}, fmt.Errorf("invalid depth: must be >= 0, got %d", args.Depth)
	}
	if v.store == nil {
		// Constructor misuse — surfaced as a real error rather than a
		// nil deref so the wrapper can render it via FatalError. Matches
		// the new + link verb guard.
		return Result{}, errors.New("brain related: storage is not configured")
	}

	// Existence probe + center enrichment. We do this BEFORE seeding the
	// BFS so a missing center fails fast with the spec-mandated wording.
	// bd's storage wraps the underlying "not found" with storage.ErrNotFound;
	// we unwrap with errors.Is to produce the brain-spec error and pass
	// any other error through wrapped so transport failures stay
	// diagnosable.
	centerIssue, err := v.store.GetIssue(ctx, args.ID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return Result{}, fmt.Errorf("brain doc %s not found", args.ID)
		}
		return Result{}, fmt.Errorf("brain related: lookup center %s: %w", args.ID, err)
	}

	center := nodeFromIssue(centerIssue, "" /* center has no parent edge */)

	// visited tracks every ID we've already placed in the tree, so a
	// cycle re-discovers that ID as `AlreadyVisited`. The center itself
	// is the first entry — re-visiting it (e.g. via A → B → A) prunes
	// to a leaf, matching scenario "cycle in the graph".
	visited := map[string]bool{center.ID: true}

	// queueItem ties a tree node to the BFS distance from the center.
	// We compare distance against args.Depth before each expansion: at
	// distance == args.Depth, no further expansion happens (the node is
	// a leaf, ending the BFS branch). At distance < args.Depth, the
	// node's outgoing edges are fetched and appended as children.
	type queueItem struct {
		node     *Node
		distance int
	}

	queue := []queueItem{{node: center, distance: 0}}

	for len(queue) > 0 {
		head := queue[0]
		queue = queue[1:]

		if head.distance >= args.Depth {
			// Reached the depth cap — do not expand further. The node
			// stays in the tree as a leaf with its existing (empty)
			// Children slice. This is also the only branch reached
			// when args.Depth == 0 (center prints alone).
			continue
		}

		// Fetch outgoing edges for this node, with each neighbour's
		// edge type attached. One call per BFS step is the same
		// per-step cost shape `bd dep list` would use repeatedly.
		neighbours, err := v.store.GetDependenciesWithMetadata(ctx, head.node.ID)
		if err != nil {
			return Result{}, fmt.Errorf("brain related: load edges from %s: %w", head.node.ID, err)
		}

		// Deterministic ordering: sort children by edge type (alpha)
		// then by neighbour ID. This is the contract the rendered tree
		// depends on. We sort the SOURCE slice before walking it so the
		// child order in the tree matches the spec-required output
		// shape regardless of how storage returned the rows.
		sort.SliceStable(neighbours, func(i, j int) bool {
			if neighbours[i].DependencyType != neighbours[j].DependencyType {
				return string(neighbours[i].DependencyType) < string(neighbours[j].DependencyType)
			}
			return neighbours[i].ID < neighbours[j].ID
		})

		for _, n := range neighbours {
			edgeType := string(n.DependencyType)

			if visited[n.ID] {
				// Cycle prune: a node already in the tree re-appears
				// as an AlreadyVisited leaf with NO further expansion.
				// We keep its Title/Kind/Status so the wrapper can
				// still print the contextual line — the "(already
				// visited)" marker tells the reader they're seeing
				// the cycle rather than the doc's first visit.
				leaf := &Node{
					ID:             n.ID,
					Title:          n.Title,
					Kind:           string(n.IssueType),
					Closed:         n.Status == types.StatusClosed,
					EdgeFromParent: edgeType,
					Children:       []*Node{},
					AlreadyVisited: true,
				}
				head.node.Children = append(head.node.Children, leaf)
				continue
			}

			// First visit: enrich from the metadata row (which embeds
			// types.Issue, so Title/Kind/Status are already populated;
			// no second GetIssue round trip needed), mark visited,
			// enqueue for further expansion at distance+1.
			child := &Node{
				ID:             n.ID,
				Title:          n.Title,
				Kind:           string(n.IssueType),
				Closed:         n.Status == types.StatusClosed,
				EdgeFromParent: edgeType,
				Children:       []*Node{},
			}
			visited[n.ID] = true
			head.node.Children = append(head.node.Children, child)
			queue = append(queue, queueItem{node: child, distance: head.distance + 1})
		}
	}

	return Result{Center: center}, nil
}

// nodeFromIssue projects a *types.Issue into a tree Node with an empty
// Children slice (so --json never elides the field on a leaf). edgeFrom
// is the dependency type that linked this node to its parent; pass ""
// for the BFS root.
func nodeFromIssue(iss *types.Issue, edgeFrom string) *Node {
	return &Node{
		ID:             iss.ID,
		Title:          iss.Title,
		Kind:           string(iss.IssueType),
		Closed:         iss.Status == types.StatusClosed,
		EdgeFromParent: edgeFrom,
		Children:       []*Node{},
	}
}
