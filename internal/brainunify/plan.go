package brainunify

import (
	"sort"
	"strings"
)

// SourceFacts is what the planner learns about one source database without
// reading any row bodies: enough to decide inclusion and to detect
// collisions.
type SourceFacts struct {
	// Namespace is the source's name in the unified database.
	Namespace string
	// Database is the Dolt database read from.
	Database string
	// Registered reports whether a store in the registry claimed it.
	Registered bool
	// Reachable reports whether the source answered a read.
	Reachable bool
	// SkipReason explains a non-Reachable source.
	SkipReason string
	// BeadCount is the number of rows in issues.
	BeadCount int64
	// Prefixes maps each observed id prefix to its row count in this source.
	Prefixes map[string]int64
	// Tables lists the base tables the source has.
	Tables []string
	// Fingerprints maps a table name to that table's content fingerprint.
	Fingerprints map[string]Fingerprint
	// SchemaVersion is the value of the schema_migrations/config schema
	// version recorded by the source, when it can be read.
	SchemaVersion string
	// ProjectID is the source's project identity.
	ProjectID string
	// DeclaredPrefixes are the prefixes the store's config.yaml claims.
	DeclaredPrefixes []string
}

// Fingerprint is an order-independent content digest of one table, cheap
// enough to compute on a live server and strong enough that a changed row
// changes it. Rows is the row count; Bytes is the total length of the
// concatenated row text; Hash is the bitwise XOR of per-row CRC32 values,
// which is order independent so two copies of the same data in different
// insertion orders fingerprint identically.
// Fingerprint holds the three values every fingerprint is made of.
//
// Bytes and Hash are int64 here, but Dolt returns SUM() and BIT_XOR() over
// the per-row digests as a double, so they are scanned as float64 and
// converted. Every value involved is far below 2^53, where a double is exact,
// so the conversion loses nothing.
type Fingerprint struct {
	Rows  int64 `json:"rows"`
	Bytes int64 `json:"bytes"`
	Hash  int64 `json:"hash"`
}

// Collision is one id that more than one source database contains, together
// with the decision the migration took about it.
type Collision struct {
	// ID is the duplicated bead id.
	ID string
	// Prefix is the id's namespace prefix.
	Prefix string
	// Owner is the namespace that owns the prefix.
	Owner string
	// Winner is the source whose copy lands in the unified database when the
	// copies are identical. A divergent collision has no winner: every copy
	// is kept (see Copies).
	Winner string
	// Losers are the other sources holding a copy of an identical duplicate,
	// in sorted order. Their copies are the ones skipped. Empty for a
	// divergent collision.
	Losers []string
	// Reason explains the choice in one clause.
	Reason string
	// Resolution is ResolutionMergedIdentical or ResolutionConflict.
	Resolution string
	// Divergent reports whether the copies differ anywhere. A divergent
	// collision becomes a conflict bead with one new bead per copy: see
	// conflict.go.
	Divergent bool
	// Copies are the copies of a divergent collision, one per source holding
	// it, sorted by store, each with the id it is minted under. Empty for
	// identical copies.
	Copies []ConflictCopy
	// DifferingColumns names the columns whose values differ between the
	// copies, sorted. It is empty when the copies are identical.
	DifferingColumns []string
	// DataColumnsDiffer reports whether any column that carries the bead's
	// content differs, as opposed to only its bookkeeping.
	DataColumnsDiffer bool
	// WinnerHash is the content hash of the winning copy, so the report shows
	// what was compared rather than asserting a comparison. Empty for a
	// divergent collision.
	WinnerHash string
	// LoserHashes maps each losing source to its content hash. Empty for a
	// divergent collision, whose copies' hashes are on Copies.
	LoserHashes map[string]string
}

// bookkeepingColumns record when a row was last touched rather than what it
// says. A duplicated id whose copies differ only here has not lost anything
// by keeping one of them; a difference anywhere else has.
var bookkeepingColumns = map[string]bool{
	"content_hash": true,
	"updated_at":   true,
}

// Reason values of an identical duplicate, recorded in the
// brain_unify_collisions table. They say how the copy that stays was chosen;
// the copies are identical, so the choice does not change any content.
const (
	// CollisionReasonOwner: the prefix owner held a copy.
	CollisionReasonOwner = "prefix-owner-holds-copy"
	// CollisionReasonNewest: no owner held a copy, so the most recently
	// updated copy wins — divergence then resolves toward live state.
	CollisionReasonNewest = "newest-updated-copy-wins"
	// CollisionReasonFirst: total-order tiebreak after updated_at and
	// created_at, so the outcome never depends on iteration order.
	CollisionReasonFirst = "lexicographic-tiebreak"
)

// IDCopy is one source's copy of a duplicated id, as read from that source.
type IDCopy struct {
	Source    string
	UpdatedAt string
	CreatedAt string
	// ContentHash is the row's stored content_hash column. It identifies a
	// copy but is not on its own evidence of disagreement, so the full row is
	// read alongside it (see Row) whenever an id turns out to be duplicated.
	ContentHash string
	// Row is the bead read in full. It is populated only for duplicated ids,
	// which are rare enough to read whole and are exactly the rows where a
	// wrong winner would lose data.
	Row map[string]string
}

// Plan is the deterministic analysis of the current federation: which sources
// participate, which prefixes they own, and where the ids disagree.
type Plan struct {
	// Sources are the participating sources, sorted by namespace.
	Sources []SourceFacts
	// Namespaces maps prefix -> ownership.
	Namespaces map[string]Namespace
	// Collisions are the duplicated ids, sorted by id.
	Collisions []Collision
	// Excluded are databases the run refuses to migrate, with the reason.
	Excluded []ExcludedSource
	// TemplateNamespace is the source whose schema the unified database is
	// built from. Empty means the caller chose no template.
	TemplateNamespace string
	// Refusals are the reasons the plan cannot be applied (see Refusal).
	Refusals []string
}

// ExcludedSource is a database deliberately left out of the migration, and
// why. Exclusion is never silent: every entry is printed.
type ExcludedSource struct {
	Database string
	Reason   string
	Beads    int64
}

// Reasons a database can be excluded.
const (
	// ExcludeReplica: the database holds ids from several namespaces and is
	// therefore a cross-store index, not a store of its own.
	ExcludeReplica = "multi-namespace replica, not an independent store"
	// ExcludeUnreachable: the database could not be read.
	ExcludeUnreachable = "source database unreachable"
	// ExcludeNotServer: the store is embedded-mode and has no server to read.
	ExcludeNotServer = "embedded-mode store, no Dolt server to read"
)

// SourceByNamespace returns the named source's facts.
func (p Plan) SourceByNamespace(ns string) (SourceFacts, bool) {
	for _, s := range p.Sources {
		if s.Namespace == ns {
			return s, true
		}
	}
	return SourceFacts{}, false
}

// TotalBeads is the sum of source bead counts, i.e. the number of rows the
// unified database receives before collision resolution removes any.
func (p Plan) TotalBeads() int64 {
	var n int64
	for _, s := range p.Sources {
		n += s.BeadCount
	}
	return n
}

// DistinctBeads is the number of beads the unified issues table holds: the
// source rows, less the skipped copy of every identical duplicate, plus one
// conflict bead for every duplicate whose copies differ (its copies are source
// rows, so they are already counted).
func (p Plan) DistinctBeads() int64 {
	n := p.TotalBeads()
	for _, c := range p.Collisions {
		if c.Divergent {
			n++
		} else {
			n -= int64(len(c.Losers))
		}
	}
	return n
}

// DuplicateCopies is the number of source rows that will not become a row of
// their own because an identical copy of the same id already exists. Each is
// recorded in brain_unify_collisions; none is dropped silently. The copies of
// a divergent duplicate are not counted: every one of them becomes a bead.
func (p Plan) DuplicateCopies() int64 {
	var n int64
	for _, c := range p.Collisions {
		n += int64(len(c.Losers))
	}
	return n
}

// DivergentCollisions returns the collisions whose copies disagree anywhere,
// including in bookkeeping columns. They become conflict beads.
func (p Plan) DivergentCollisions() []Collision {
	return p.Conflicts()
}

// DataLossCollisions returns the divergent collisions where a column carrying
// the bead's content - not merely its bookkeeping - differs between the copies.
// Under the winner rule these were the ones where keeping a single copy
// discarded something real; they are now only reported as such, because no
// copy is discarded.
func (p Plan) DataLossCollisions() []Collision {
	var out []Collision
	for _, c := range p.Collisions {
		if c.DataColumnsDiffer {
			out = append(out, c)
		}
	}
	return out
}

// CollisionsFor returns the collisions observed in a source, keyed by id.
func (p Plan) CollisionsFor(source string) map[string]Collision {
	out := map[string]Collision{}
	for _, c := range p.Collisions {
		if contains(c.holders(), source) {
			out[c.ID] = c
		}
	}
	return out
}

// BuildPlan turns source facts into the deterministic mapping: which sources
// participate, who owns each prefix, and how every duplicated id is resolved.
//
// copies maps a duplicated id to the per-source copies read from disk. It is
// required (not optional) because choosing a winner for a divergent copy is
// exactly the decision that must not be guessed at.
func BuildPlan(facts []SourceFacts, copies map[string][]IDCopy) Plan {
	plan := Plan{TemplateNamespace: ""}

	observed := map[string][]string{}
	declared := map[string][]string{}
	names := map[string][]string{}
	counts := map[string]int64{}
	for _, f := range facts {
		if !f.Reachable {
			plan.Excluded = append(plan.Excluded, ExcludedSource{
				Database: f.Database,
				Reason:   f.SkipReason,
				Beads:    f.BeadCount,
			})
			continue
		}
		plan.Sources = append(plan.Sources, f)
		prefixes := make([]string, 0, len(f.Prefixes))
		for p, n := range f.Prefixes {
			prefixes = append(prefixes, p)
			counts[p] += n
		}
		sort.Strings(prefixes)
		observed[f.Namespace] = prefixes
		declared[f.Namespace] = append([]string(nil), f.DeclaredPrefixes...)
		if f.Database != "" && f.Database != f.Namespace {
			names[f.Namespace] = append(names[f.Namespace], f.Database)
		}
	}
	sort.Slice(plan.Sources, func(i, j int) bool {
		return plan.Sources[i].Namespace < plan.Sources[j].Namespace
	})
	plan.Namespaces = BuildNamespaces(observed, declared, counts, names)

	for id, group := range copies {
		plan.Collisions = append(plan.Collisions, resolveCollision(id, plan.Namespaces, group))
	}
	sort.Slice(plan.Collisions, func(i, j int) bool { return plan.Collisions[i].ID < plan.Collisions[j].ID })
	plan.Refusals = planRefusals(plan.Conflicts())
	return plan
}

// resolveCollision picks the winning copy of a duplicated id under a total
// order, so re-running the tool on the same data always picks the same row.
func resolveCollision(id string, namespaces map[string]Namespace, group []IDCopy) Collision {
	prefix := PrefixOf(id)
	ns := namespaces[prefix]

	sorted := append([]IDCopy(nil), group...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Source < sorted[j].Source })

	c := Collision{
		ID:          id,
		Prefix:      prefix,
		Owner:       ns.Owner,
		LoserHashes: map[string]string{},
	}

	// Rule 1: the prefix owner's copy wins. Ownership is the same rule the
	// unified database uses to answer "which store is this bead in", so the
	// winner and the namespace can never disagree.
	var winnerRow map[string]string
	if ns.Owner != "" {
		for _, cp := range sorted {
			if cp.Source == ns.Owner {
				c.Winner = cp.Source
				c.WinnerHash = cp.ContentHash
				winnerRow = cp.Row
				c.Reason = CollisionReasonOwner
			}
		}
	}

	// Rule 2: otherwise the most recently updated copy wins, so a
	// divergence resolves toward live state rather than toward whichever
	// database happened to be read first.
	if c.Winner == "" {
		best := sorted[0]
		for _, cp := range sorted[1:] {
			if beatsFreshness(cp, best) {
				best = cp
			}
		}
		c.Winner = best.Source
		c.WinnerHash = best.ContentHash
		winnerRow = best.Row
		c.Reason = CollisionReasonNewest
		if reason := tiebreakReason(sorted, best); reason != "" {
			c.Reason = reason
		}
	}

	for _, cp := range sorted {
		if cp.Source == c.Winner {
			continue
		}
		c.Losers = append(c.Losers, cp.Source)
		c.LoserHashes[cp.Source] = cp.ContentHash
		if cp.ContentHash != c.WinnerHash {
			c.Divergent = true
		}
		if cols := differingColumns(winnerRow, cp.Row); len(cols) > 0 {
			c.Divergent = true
			c.DifferingColumns = mergeSorted(c.DifferingColumns, cols)
			for _, col := range cols {
				if !bookkeepingColumns[col] {
					c.DataColumnsDiffer = true
				}
			}
		}
	}

	if !c.Divergent {
		c.Resolution = ResolutionMergedIdentical
		return c
	}

	// The copies differ: nothing wins. Every copy is kept under a minted id and
	// the original id becomes the conflict bead. The winner computed above only
	// served as the baseline the other copies were compared with; it is not a
	// choice and is not recorded as one.
	c.Resolution = ResolutionConflict
	c.Reason = CollisionReasonConflict
	c.Winner, c.WinnerHash = "", ""
	c.Losers, c.LoserHashes = nil, map[string]string{}
	for _, cp := range sorted {
		c.Copies = append(c.Copies, ConflictCopy{
			Store:       cp.Source,
			ID:          mintCopyID(namespaces, cp.Source, id),
			ContentHash: cp.ContentHash,
			Row:         cp.Row,
		})
	}
	return c
}

// differingColumns returns the columns on which two read rows disagree,
// sorted. It returns nil when either row was not read in full, because then
// nothing can honestly be claimed about the comparison.
func differingColumns(a, b map[string]string) []string {
	if a == nil || b == nil {
		return nil
	}
	var out []string
	for col, av := range a {
		bv, ok := b[col]
		if !ok {
			continue
		}
		if av != bv {
			out = append(out, col)
		}
	}
	sort.Strings(out)
	return out
}

// mergeSorted unions two sorted string slices.
func mergeSorted(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	out := append([]string(nil), a...)
	for _, v := range b {
		if !contains(out, v) {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// beatsFreshness reports whether a should replace b as the winning copy:
// later updated_at wins, then later created_at, and on an exact tie the
// lexicographically smallest source name wins so the outcome never depends on
// iteration order.
//
// Timestamps are compared as strings: both sides are read through the same
// connection settings, so the textual form is a fixed-width lexicographic
// order and avoids a parse that could disagree with the server.
func beatsFreshness(a, b IDCopy) bool {
	if c := strings.Compare(b.UpdatedAt, a.UpdatedAt); c != 0 {
		return c < 0
	}
	if c := strings.Compare(b.CreatedAt, a.CreatedAt); c != 0 {
		return c < 0
	}
	return a.Source < b.Source
}

// tiebreakReason reports whether the winner was decided purely by the
// lexicographic tiebreak (identical timestamps), which is worth surfacing
// because it means freshness could not distinguish the copies.
func tiebreakReason(sorted []IDCopy, best IDCopy) string {
	sameFreshness := true
	for _, cp := range sorted {
		if cp.Source == best.Source {
			continue
		}
		if cp.UpdatedAt != best.UpdatedAt || cp.CreatedAt != best.CreatedAt {
			sameFreshness = false
			break
		}
	}
	if sameFreshness {
		return CollisionReasonFirst
	}
	return ""
}
