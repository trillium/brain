package brainunify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Conflict beads.
//
// When more than one store holds the same bead id and the copies differ in any
// column, keeping one of them would discard the other's state. Nothing is
// discarded instead: every copy becomes a bead of its own, under a new id
// minted deterministically from the original id and the store that authored
// the copy, carrying that copy's row and all of its child rows. The original id
// stays a real bead - the conflict bead - which is open, says it is an
// unresolved duplicate, names both copies, and links to each of them. Links
// other beads hold to the original id are not touched, so they now reach the
// conflict bead.
//
// Copies that are identical are still merged into one bead, as before.
//
// Everything that decides what a source row becomes in the merged database -
// which rows move to a copy, what their new ids are, which rows are skipped,
// what the conflict bead is made of - lives in this file and is used by the
// build, the replay and the verifier alike, so the three cannot disagree.

// Resolution values, recorded in brain_unify_collisions.resolution.
const (
	// ResolutionMergedIdentical: the copies were identical; one bead remains.
	ResolutionMergedIdentical = "merged-identical"
	// ResolutionConflict: the copies differ; each is a bead of its own and the
	// original id is a conflict bead linking to them.
	ResolutionConflict = "conflict-bead"
)

// CollisionReasonConflict is the Reason of a conflict: nothing was chosen.
const CollisionReasonConflict = "copies-differ-each-kept"

const (
	// ConflictDependencyType is the dependency type from a conflict bead to each
	// of its copies. `tracks` is bd's non-blocking "this bead follows those"
	// edge (a convoy tracking its issues): it is outside AffectsReadyWork, so a
	// conflict neither blocks the copies nor is hidden from ready work by them,
	// and it points from the conflict bead to the copy, which is the direction
	// "this tracks that". `blocks` would make an unresolved conflict wait on its
	// own copies; `duplicates` says the source is a duplicate OF the target,
	// which a conflict bead is not; `related` is symmetric and says nothing about
	// which side is the conflict.
	ConflictDependencyType = "tracks"
	// ConflictLabel marks every conflict bead, so they can be listed together.
	ConflictLabel = "unify-conflict"
	// ConflictActor is the created_by of everything the unify tools author.
	ConflictActor = "brain-unify"
	// conflictStore is the pseudo-store the import log records the rows the tool
	// itself authored under (they come from no source).
	conflictStore = "(conflict beads)"
)

// mintedSuffixLen is the number of hex characters of the digest a minted id
// carries: 48 bits, so two distinct (id, store) pairs colliding is a refusal
// that does not happen at the federation's size.
const mintedSuffixLen = 12

// ConflictCopy is one store's copy of a conflicted id.
type ConflictCopy struct {
	// Store is the store that authored the copy.
	Store string
	// ID is the id the copy has in the merged database.
	ID string
	// ContentHash is the copy's stored content hash, as the source held it.
	ContentHash string
	// Row is the copy's issues row as the source held it.
	Row map[string]string
}

// copyIDs maps each copy's store to its minted id.
func (c Collision) copyIDs() map[string]string {
	out := make(map[string]string, len(c.Copies))
	for _, cp := range c.Copies {
		out[cp.Store] = cp.ID
	}
	return out
}

// copyFor returns the copy a store authored.
func (c Collision) copyFor(store string) (ConflictCopy, bool) {
	for _, cp := range c.Copies {
		if cp.Store == store {
			return cp, true
		}
	}
	return ConflictCopy{}, false
}

// holders returns every store that holds a copy of the id, sorted, whichever
// way the collision was resolved.
func (c Collision) holders() []string {
	if c.Divergent {
		out := make([]string, 0, len(c.Copies))
		for _, cp := range c.Copies {
			out = append(out, cp.Store)
		}
		return out
	}
	out := append([]string{c.Winner}, c.Losers...)
	sort.Strings(out)
	return out
}

// mintCopyID derives the id of one store's copy of an original id:
//
//	<authoring store's prefix> "-" first 12 hex of sha256(original id 0x00 store)
//
// Both inputs are read from the sources, so two builds over the same sources
// derive the same ids with no clock, counter or read order involved. The NUL
// separator keeps ("a-1", "b") and ("a-", "1b") from folding together.
func mintCopyID(namespaces map[string]Namespace, store, original string) string {
	sum := sha256.Sum256([]byte(original + "\x00" + store))
	return copyPrefix(namespaces, store, PrefixOf(original)) + "-" + hex.EncodeToString(sum[:])[:mintedSuffixLen]
}

// copyPrefix is the prefix a store's copies carry: the store's own, so the copy
// lands in the namespace of the store that wrote it. A store may own several
// prefixes; the original id's prefix wins when the store owns it, then a prefix
// named like the store, then the smallest. A store that owns no prefix (an
// unregistered database) keeps the original's, so the copy stays where the id
// was. Only prefixes without a hyphen are candidates: the namespace of an id is
// everything before its first hyphen (see PrefixOf), so a minted id must not
// read as a different namespace than the one chosen.
func copyPrefix(namespaces map[string]Namespace, store, original string) string {
	var owned []string
	for p, ns := range namespaces {
		if ns.Owner == store && p != "" && PrefixOf(p) == p && !strings.Contains(p, "-") {
			owned = append(owned, p)
		}
	}
	sort.Strings(owned)
	switch {
	case contains(owned, original):
		return original
	case contains(owned, store):
		return store
	case len(owned) > 0:
		return owned[0]
	default:
		return original
	}
}

// ---------------------------------------------------------------------------
// the plan's view of conflicts
// ---------------------------------------------------------------------------

// Conflicts returns the collisions whose copies differ: the ones that become a
// conflict bead and one bead per copy.
func (p Plan) Conflicts() []Collision {
	var out []Collision
	for _, c := range p.Collisions {
		if c.Divergent {
			out = append(out, c)
		}
	}
	return out
}

// MintedCopies is the number of beads minted for conflicts.
func (p Plan) MintedCopies() int {
	n := 0
	for _, c := range p.Conflicts() {
		n += len(c.Copies)
	}
	return n
}

// Refusal returns the reason the plan cannot be applied, or nil. A plan the
// tool cannot apply safely is refused rather than approximated:
//
//   - a minted id that two copies derive, or that an existing bead already has,
//     would make the merged database's primary key ambiguous;
//   - two copies of one id carrying the same non-empty slug, which the merged
//     database holds unique.
func (p Plan) Refusal() error {
	if len(p.Refusals) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(p.Refusals, "; "))
}

// sourceExclusions are the ids whose rows a source's aggregate fingerprint
// leaves out, because they are not copied as-is: the rows of an identical
// duplicate's skipped copy, and every conflicted id (its rows move to the
// minted copies, or are verified one id at a time).
func (p Plan) sourceExclusions(store string) []string {
	var out []string
	for _, c := range p.Collisions {
		if c.Divergent {
			out = append(out, c.ID)
			continue
		}
		for _, l := range c.Losers {
			if l == store {
				out = append(out, c.ID)
			}
		}
	}
	sort.Strings(out)
	return out
}

// mergedExclusions are the ids the merged database's aggregate fingerprint
// leaves out, matching sourceExclusions: the conflict beads and their copies,
// all of which are verified one id at a time.
func (p Plan) mergedExclusions() []string {
	var out []string
	for _, c := range p.Conflicts() {
		out = append(out, c.ID)
		for _, cp := range c.Copies {
			out = append(out, cp.ID)
		}
	}
	sort.Strings(out)
	return out
}

// planRefusals finds the minted-id clashes and slug clashes of a plan's
// conflicts.
func planRefusals(conflicts []Collision) []string {
	var out []string
	owner := map[string]string{}
	for _, c := range conflicts {
		slugs := map[string]string{}
		for _, cp := range c.Copies {
			key := c.ID + " in store " + cp.Store
			if prev, ok := owner[cp.ID]; ok && prev != key {
				out = append(out, fmt.Sprintf("minted id %s would be derived for both %s and %s", cp.ID, prev, key))
			}
			owner[cp.ID] = key
			if slug := cp.Row["slug"]; slug != "" {
				if prev, ok := slugs[slug]; ok {
					out = append(out, fmt.Sprintf("the copies of %s in stores %s and %s both carry slug %q, which the merged database holds unique", c.ID, prev, cp.Store, slug))
				}
				slugs[slug] = cp.Store
			}
		}
	}
	return out
}

// mintedClashes reports minted ids that an existing bead already has.
func mintedClashes(conflicts []Collision, existing map[string]bool) []string {
	var out []string
	for _, c := range conflicts {
		for _, cp := range c.Copies {
			if existing[cp.ID] {
				out = append(out, fmt.Sprintf("minted id %s (copy of %s from store %s) is already the id of an existing bead", cp.ID, c.ID, cp.Store))
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// moving rows: what a source row becomes in the merged database
// ---------------------------------------------------------------------------

// rowMapper decides what each row of a bead-scoped source table becomes in the
// merged database. The build, the replay and the verifier all map through it.
type rowMapper struct {
	// skip: store -> id -> the store's copy is the skipped copy of an
	// identical duplicate.
	skip map[string]map[string]bool
	// moved: store -> original id -> the copy that store's rows move to.
	moved map[string]map[string]ConflictCopy
	// conflicts: every conflicted id.
	conflicts map[string]Collision
}

// newRowMapper builds the mapper for a plan.
func newRowMapper(p Plan) *rowMapper {
	m := &rowMapper{
		skip:      map[string]map[string]bool{},
		moved:     map[string]map[string]ConflictCopy{},
		conflicts: map[string]Collision{},
	}
	for _, c := range p.Collisions {
		if c.Divergent {
			m.conflicts[c.ID] = c
			for _, cp := range c.Copies {
				if m.moved[cp.Store] == nil {
					m.moved[cp.Store] = map[string]ConflictCopy{}
				}
				m.moved[cp.Store][c.ID] = cp
			}
			continue
		}
		for _, l := range c.Losers {
			if m.skip[l] == nil {
				m.skip[l] = map[string]bool{}
			}
			m.skip[l][c.ID] = true
		}
	}
	return m
}

// selfReferencingKeys names the tables whose rows point at each other by the
// same key space they are re-keyed in, and the column that does the pointing:
// an interaction's parent is another interaction of the same bead.
var selfReferencingKeys = map[string]string{"interactions": "parent_id"}

// rekeyColumn reports which column of a bead-scoped table has to be re-keyed
// when a copy's rows move. A table whose primary key contains the scope column
// needs nothing: moving the rows changes the key. A table keyed by a single
// column of its own (comments, events, dependencies - each keyed by a uuid
// that the two copies of a bead share, because one was copied from the other)
// would see the second copy's rows collide with the first's, so that column
// gets a deterministic new value per copy. A table keyed any other way cannot
// be re-keyed without guessing, and is refused.
func rekeyColumn(tp TablePlan) (col string, need bool, err error) {
	if tp.Scope == ScopeDatabaseState || len(tp.PrimaryKey) == 0 || contains(tp.PrimaryKey, tp.ScopeColumn) {
		return "", false, nil
	}
	if len(tp.PrimaryKey) == 1 {
		return tp.PrimaryKey[0], true, nil
	}
	return "", false, fmt.Errorf("table %s is keyed by (%s), which holds neither its %s column nor a single column of its own, so the rows of a conflict's copies cannot be given distinct keys",
		tp.Table, strings.Join(tp.PrimaryKey, ", "), tp.ScopeColumn)
}

// deriveChildKey is the new key of a child row moved to a copy. It derives from
// the table, the store, the original bead id and the row's old key, so it is the
// same on every build and replay and different for every copy. A uuid-shaped
// key stays uuid-shaped (an RFC 4122 name-based id); any other key stays the
// same length.
func deriveChildKey(table, store, original, old string) string {
	sum := sha256.Sum256([]byte(table + "\x00" + store + "\x00" + original + "\x00" + old))
	if looksLikeUUID(old) {
		return uuidFromDigest(sum)
	}
	h := hex.EncodeToString(sum[:])
	n := len(old)
	if n < 8 {
		n = 8
	}
	if n > len(h) {
		n = len(h)
	}
	return h[:n]
}

// uuidFromDigest shapes a digest as an RFC 4122 name-based (version 5) uuid.
func uuidFromDigest(sum [sha256.Size]byte) string {
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if r != '-' {
				return false
			}
		case (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F'):
		default:
			return false
		}
	}
	return true
}

// cellText renders a driver value as the text the mapper compares and keys on.
func cellText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(t)
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

// mapRows maps a batch of one store's rows of a bead-scoped table: rows of an
// identical duplicate's skipped copy are dropped (and counted), rows of a
// conflicted id the store holds a copy of move to that copy's minted id, and
// every other row is untouched. cols names the columns of each row.
func (m *rowMapper) mapRows(tp TablePlan, store string, cols []string, rows [][]any) (kept [][]any, skipped int64, err error) {
	scopeIdx := indexOf(cols, tp.ScopeColumn)
	if scopeIdx < 0 {
		return nil, 0, fmt.Errorf("table %s: the scope column %s is not among the columns read", tp.Table, tp.ScopeColumn)
	}
	rekey, need, err := rekeyColumn(tp)
	if err != nil {
		return nil, 0, err
	}
	rekeyIdx := -1
	if need {
		if rekeyIdx = indexOf(cols, rekey); rekeyIdx < 0 {
			return nil, 0, fmt.Errorf("table %s: the key column %s is not among the columns read", tp.Table, rekey)
		}
	}
	selfIdx := -1
	if col, ok := selfReferencingKeys[tp.Table]; ok && need && col != rekey {
		selfIdx = indexOf(cols, col)
	}

	skip, moved := m.skip[store], m.moved[store]
	if len(skip) == 0 && len(moved) == 0 {
		return rows, 0, nil
	}
	kept = make([][]any, 0, len(rows))
	for _, row := range rows {
		id := cellText(row[scopeIdx])
		if skip[id] {
			skipped++
			continue
		}
		cp, isMoved := moved[id]
		if !isMoved {
			kept = append(kept, row)
			continue
		}
		next := append([]any(nil), row...)
		next[scopeIdx] = cp.ID
		if rekeyIdx >= 0 {
			next[rekeyIdx] = deriveChildKey(tp.Table, store, id, cellText(row[rekeyIdx]))
		}
		if selfIdx >= 0 && row[selfIdx] != nil && cellText(row[selfIdx]) != "" {
			next[selfIdx] = deriveChildKey(tp.Table, store, id, cellText(row[selfIdx]))
		}
		kept = append(kept, next)
	}
	return kept, skipped, nil
}

// validateRekeying refuses, before anything is written, a table whose rows a
// conflict's copies could not be given distinct keys in.
func validateRekeying(plans []TablePlan) error {
	for _, tp := range plans {
		if _, _, err := rekeyColumn(tp); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// the conflict bead
// ---------------------------------------------------------------------------

// conflictTitle is the title of a conflict bead.
func conflictTitle(c Collision) string {
	return fmt.Sprintf("Unresolved duplicate-id conflict: %s (%d differing copies)", c.ID, len(c.Copies))
}

// conflictDescription is the description of a conflict bead: what happened,
// which copies exist and where each came from, what differs, and what the
// links to this id now mean.
func conflictDescription(c Collision) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The id %s existed in %d stores with different content. `bd brain unify` did not pick one: it kept every copy as a bead of its own, and this bead is what is left at the original id. Nothing was discarded. This conflict is unresolved.\n\nCopies:\n", c.ID, len(c.Copies))
	for _, cp := range c.Copies {
		fmt.Fprintf(&b, "- %s - from store %s", cp.ID, cp.Store)
		var facts []string
		if s := cp.Row["status"]; s != "" {
			facts = append(facts, "status "+s)
		}
		if s := cp.Row["updated_at"]; s != "" {
			facts = append(facts, "updated "+s)
		}
		if len(facts) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(facts, ", "))
		}
		if t := oneLine(cp.Row["title"], 100); t != "" {
			fmt.Fprintf(&b, " %q", t)
		}
		b.WriteByte('\n')
	}
	if len(c.DifferingColumns) > 0 {
		fmt.Fprintf(&b, "\nColumns that differ between the copies: %s.\n", strings.Join(c.DifferingColumns, ", "))
		if !c.DataColumnsDiffer {
			b.WriteString("Only bookkeeping columns differ (content_hash, updated_at); the content is the same.\n")
		}
	}
	fmt.Fprintf(&b, "\nThis bead %s each copy (dependency type %q, which blocks nothing). Links other beads hold to %s still point here, so they reach this conflict rather than either copy. Review the copies, keep what is true in one of them, close the other, then close this bead; `bd brain unify` has no command that does this for you. The merge record is brain_unify_collisions, row %s.\n",
		ConflictDependencyType, ConflictDependencyType, c.ID, c.ID)
	return b.String()
}

// oneLine collapses whitespace and bounds the length of a text for a listing.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}

// conflictTimes are the created_at and updated_at of a conflict bead: the
// earliest creation and the latest update of its copies, so they are a function
// of the sources and a rebuild writes the same row.
func conflictTimes(c Collision) (created, updated string) {
	const fallback = "2000-01-01 00:00:00"
	for _, cp := range c.Copies {
		if t := cp.Row["created_at"]; t != "" && (created == "" || t < created) {
			created = t
		}
		if t := cp.Row["updated_at"]; t != "" && t > updated {
			updated = t
		}
	}
	if created == "" {
		created = fallback
	}
	if updated == "" {
		updated = created
	}
	return created, updated
}

// conflictRows are the rows the tool authors for a conflict, by merged table:
// the conflict bead itself, one dependency from it to each copy, and its label.
// Only the columns that carry meaning are given; the table defaults the rest.
func conflictRows(c Collision) map[string][]map[string]any {
	created, updated := conflictTimes(c)
	out := map[string][]map[string]any{
		"issues": {{
			"id":                  c.ID,
			"title":               conflictTitle(c),
			"description":         conflictDescription(c),
			"design":              "",
			"acceptance_criteria": "",
			"notes":               "",
			"status":              "open",
			"priority":            1,
			"issue_type":          "task",
			"created_at":          created,
			"created_by":          ConflictActor,
			"updated_at":          updated,
		}},
		"labels": {{"issue_id": c.ID, "label": ConflictLabel}},
	}
	for _, cp := range c.Copies {
		out["dependencies"] = append(out["dependencies"], map[string]any{
			"id":                  uuidFromDigest(sha256.Sum256([]byte("conflict-dependency\x00" + c.ID + "\x00" + cp.ID))),
			"issue_id":            c.ID,
			"type":                ConflictDependencyType,
			"created_at":          created,
			"created_by":          ConflictActor,
			"thread_id":           "",
			"depends_on_issue_id": cp.ID,
		})
	}
	return out
}

// conflictColumns lists the columns each conflict table has to provide.
var conflictColumns = map[string][]string{
	"issues":       {"id", "title", "description", "status", "priority", "issue_type", "created_at", "created_by", "updated_at"},
	"dependencies": {"id", "issue_id", "type", "created_at", "created_by", "depends_on_issue_id"},
	"labels":       {"issue_id", "label"},
}

// validateConflictTables refuses a merged schema that cannot hold a conflict
// bead: it needs issues, dependencies and labels tables with the columns above.
func validateConflictTables(plans []TablePlan) error {
	for _, table := range []string{"issues", "dependencies", "labels"} {
		var tp *TablePlan
		for i := range plans {
			if plans[i].Table == table {
				tp = &plans[i]
			}
		}
		if tp == nil {
			return fmt.Errorf("the schema has no %s table, which a conflict bead needs", table)
		}
		for _, col := range conflictColumns[table] {
			if !contains(tp.Columns, col) {
				return fmt.Errorf("table %s has no column %s, which a conflict bead needs", table, col)
			}
		}
	}
	return nil
}

// tableConflictRows renders the rows the tool authors for conflicts in one
// merged table: the columns they provide, in the table's column order, and one
// value slice per row. A table the tool authors nothing in yields none.
func tableConflictRows(tp TablePlan, conflicts []Collision) (cols []string, rows [][]any) {
	var maps []map[string]any
	for _, c := range conflicts {
		maps = append(maps, conflictRows(c)[tp.Table]...)
	}
	if len(maps) == 0 {
		return nil, nil
	}
	for _, col := range tp.Columns {
		for _, m := range maps {
			if _, ok := m[col]; ok {
				cols = append(cols, col)
				break
			}
		}
	}
	for _, m := range maps {
		row := make([]any, len(cols))
		for i, col := range cols {
			row[i] = m[col]
		}
		rows = append(rows, row)
	}
	return cols, rows
}

// ---------------------------------------------------------------------------
// reading a copy: what its rows should be, and what they are
// ---------------------------------------------------------------------------

// copyRead is the rows of one copy of a conflicted id, by merged table, and the
// columns each was read with.
type copyRead struct {
	Rows map[string][][]any
	Cols map[string][]string
}

// sharedSourceColumns returns the columns of a merged table that the source's
// table also has: the columns a copy of the table's rows can supply.
func sharedSourceColumns(ctx context.Context, source *readOnlySource, database string, tp TablePlan) ([]string, error) {
	have, err := source.Columns(ctx, database, tp.Table)
	if err != nil {
		return nil, err
	}
	var common []string
	for _, c := range tp.Columns {
		if contains(have, c) {
			common = append(common, c)
		}
	}
	return common, nil
}

// beadTablePlans are the plans of the bead-scoped tables.
func beadTablePlans(plans []TablePlan) []TablePlan {
	var out []TablePlan
	for _, tp := range plans {
		if tp.Scope != ScopeDatabaseState {
			out = append(out, tp)
		}
	}
	return out
}

// copyRowsFromSource reads what one store's copy of a conflicted id becomes in
// the merged database: its rows in every bead-scoped table the source has, put
// through the same row mapper the build uses, so they carry the minted id and
// the new child keys. The read is a handful of indexed lookups per table.
func copyRowsFromSource(ctx context.Context, source *readOnlySource, plans []TablePlan, mapper *rowMapper, src SourceFacts, id string) (copyRead, error) {
	out := copyRead{Rows: map[string][][]any{}, Cols: map[string][]string{}}
	for _, tp := range beadTablePlans(plans) {
		has, err := source.HasTable(ctx, src.Database, tp.Table)
		if err != nil {
			return out, fmt.Errorf("checking store %s for table %s: %w", src.Namespace, tp.Table, err)
		}
		if !has {
			continue
		}
		cols, err := sharedSourceColumns(ctx, source, src.Database, tp)
		if err != nil {
			return out, fmt.Errorf("listing the columns of %s.%s: %w", src.Database, tp.Table, err)
		}
		if indexOf(cols, tp.ScopeColumn) < 0 {
			return out, fmt.Errorf("store %s table %s has no %s column", src.Namespace, tp.Table, tp.ScopeColumn)
		}
		out.Cols[tp.Target] = cols
		rows, err := source.ReadRows(ctx, src.Database, tp.Table, cols, "`"+tp.ScopeColumn+"` = ?", id)
		if err != nil {
			return out, fmt.Errorf("reading store %s table %s for %s: %w", src.Namespace, tp.Table, id, err)
		}
		kept, _, err := mapper.mapRows(tp, src.Namespace, cols, rows)
		if err != nil {
			return out, err
		}
		if len(kept) > 0 {
			out.Rows[tp.Target] = kept
		}
	}
	return out, nil
}

// copyRowsFromMerged reads a bead's rows out of the merged database, with the
// columns a source copy was read with (every column of a table the source does
// not have).
func copyRowsFromMerged(ctx context.Context, merged *readOnlySource, database string, plans []TablePlan, cols map[string][]string, id string) (copyRead, error) {
	out := copyRead{Rows: map[string][][]any{}, Cols: cols}
	for _, tp := range beadTablePlans(plans) {
		c := cols[tp.Target]
		if len(c) == 0 {
			c = tp.Columns
		}
		rows, err := merged.ReadRows(ctx, database, tp.Target, c, "`"+tp.ScopeColumn+"` = ?", id)
		if err != nil {
			return out, fmt.Errorf("reading merged table %s for %s: %w", tp.Target, id, err)
		}
		if len(rows) > 0 {
			out.Rows[tp.Target] = rows
		}
	}
	return out, nil
}

// digestCopy is an order-independent digest of one copy's rows: every row of
// every table, each cell tagged so NULL and an empty string differ. It is
// computed in Go from rows read the same way on both sides, and it is the only
// digest of a copy there is.
func digestCopy(rows map[string][][]any) string {
	h := sha256.New()
	for _, table := range sortedKeys(rows) {
		lines := make([]string, 0, len(rows[table]))
		for _, row := range rows[table] {
			var b strings.Builder
			for i, v := range row {
				if i > 0 {
					b.WriteByte(0x1f)
				}
				if v == nil {
					b.WriteByte(0x01)
					continue
				}
				b.WriteByte(0x02)
				b.WriteString(cellText(v))
			}
			lines = append(lines, b.String())
		}
		sort.Strings(lines)
		h.Write([]byte(table))
		h.Write([]byte{0x00})
		for _, l := range lines {
			h.Write([]byte(l))
			h.Write([]byte{0x0a})
		}
		h.Write([]byte{0x00})
	}
	return hex.EncodeToString(h.Sum(nil))
}
