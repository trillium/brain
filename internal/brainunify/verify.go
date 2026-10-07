package brainunify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Check is one mechanical comparison between the source data and the unified
// database. A Check is a statement about two numbers or two digests, both of
// which the verifier computed by reading the databases, never assumed.
type Check struct {
	// Scope names what is being compared: a namespace (prefix), a store, or
	// the database as a whole.
	Scope string
	// Table is the table compared.
	Table string
	// Expected is the value derived from the source databases.
	Expected Fingerprint
	// Actual is the value read from the unified database.
	Actual Fingerprint
	// Note explains any difference in the two that is expected and benign,
	// such as rows removed by a recorded collision.
	Note string
	// OK reports whether the comparison passed.
	OK bool
	// Detail replaces the fingerprint difference when the check is not a
	// fingerprint comparison (a collision record, for one).
	Detail string
}

// Difference renders the mismatch, or the empty string when the check passed.
func (c Check) Difference() string {
	if c.OK {
		return ""
	}
	if c.Detail != "" {
		return c.Detail
	}
	return fmt.Sprintf("rows %d vs %d, bytes %d vs %d, hash %d vs %d",
		c.Expected.Rows, c.Actual.Rows, c.Expected.Bytes, c.Actual.Bytes, c.Expected.Hash, c.Actual.Hash)
}

// VerifyResult is the complete comparison of source against unified.
type VerifyResult struct {
	// Checks is every comparison performed, in a stable order.
	Checks []Check
	// NamespacesChecked counts the per-namespace comparisons.
	NamespacesChecked int
	// StoresChecked counts the per-store comparisons.
	StoresChecked int
	// CollisionsConfirmed counts collisions whose recorded decision was
	// re-read from the unified database and matched: identical duplicates and
	// conflicts alike.
	CollisionsConfirmed int
	// ConflictsConfirmed counts the conflicts among them: each has its conflict
	// bead, exactly the copies it names, and every copy's rows as the source
	// holds them.
	ConflictsConfirmed int
	// Reference is the side the unified database was compared against:
	// ReferenceRecorded or ReferenceLive.
	Reference string
}

// OK reports whether every check passed.
func (r VerifyResult) OK() bool {
	for _, c := range r.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// Failures returns only the failed checks.
func (r VerifyResult) Failures() []Check {
	var out []Check
	for _, c := range r.Checks {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}

// VerifyOptions configures a verification run.
type VerifyOptions struct {
	// Database is the unified database to compare against.
	Database string
	// Hosts locates the unified database's Dolt server.
	Host string
	Port int
	// Reference chooses the side a comparison is made against.
	//
	// ReferenceRecorded (the default) compares the unified database against
	// the fingerprints the build recorded as it copied. That is what makes
	// a build-time verification deterministic: sources frozen into a copy
	// can be re-read byte-identical, and the comparison measures the build,
	// not the federation's live movement.
	//
	// ReferenceLive recomputes the expected side from the sources as they
	// stand NOW, with the same fingerprints and the same collision-loser
	// exclusions. That is the acceptance test for a replay: a replayed
	// database must equal what its sources hold now, and any change a
	// replay missed (or any write a source made after the replay) makes it
	// fail, naming the table and the namespace that differ.
	Reference string
	// Logf receives a line as each table and each source is read. Verification
	// compares every row of every table on both sides, so without progress
	// output a stall is indistinguishable from a long run — and a full
	// federation takes long enough that "wait longer" is not a debugging
	// strategy.
	Logf func(format string, args ...any)
}

// The two reference modes a verification can run in.
const (
	ReferenceRecorded = "recorded"
	ReferenceLive     = "live"
)

// normalizeReference falls back to the default for an unset mode.
func normalizeReference(ref string) string {
	if ref == "" {
		return ReferenceRecorded
	}
	return ref
}

// Verifier compares the source federation against a built unified database.
//
// The comparison is mechanical and runs in both directions of the question
// that matters: for every namespace and every store, the source side and the
// unified side must agree on row count, total content size, and an
// order-independent content digest. A digest that matches cannot be produced
// by a spot check; it is recomputed from every row on both sides.
type Verifier struct {
	source  *readOnlySource
	unified *readOnlySource
	plan    Plan
	plans   []TablePlan
	opts    VerifyOptions
	log     func(format string, args ...any)
	mapper  *rowMapper
}

// NewVerifier opens the unified database for reading. The connection is
// read-only: verification must never be able to change what it measures.
func NewVerifier(ctx context.Context, source *readOnlySource, plan Plan, plans []TablePlan, opts VerifyOptions) (*Verifier, error) {
	host := opts.Host
	if host == "" {
		host = "127.0.0.1"
	}
	unified, err := OpenSource(host, opts.Port)
	if err != nil {
		return nil, err
	}
	if err := unified.PingDatabase(ctx, opts.Database); err != nil {
		_ = unified.Close()
		return nil, err
	}
	log := opts.Logf
	if log == nil {
		log = func(string, ...any) {}
	}
	return &Verifier{source: source, unified: unified, plan: plan, plans: plans, opts: opts, log: log, mapper: newRowMapper(plan)}, nil
}

// Close releases the verifier's connections.
func (v *Verifier) Close() error { return v.unified.Close() }

// Verify runs every comparison and returns the result.
//
// Both sides are read in one grouped pass per table: the source side groups by
// id prefix, the unified side does the same. Comparing group by group then
// proves both things at once — that every row arrived, and that it arrived in
// the namespace it belongs to.
func (v *Verifier) Verify(ctx context.Context) (VerifyResult, error) {
	res := VerifyResult{Reference: normalizeReference(v.opts.Reference)}

	var recorded map[string]map[string]Fingerprint
	if normalizeReference(v.opts.Reference) == ReferenceRecorded {
		// The recorded source fingerprints are the reference. Reading them is
		// also what makes a verification reproducible: the build measured the
		// sources at a point in time, and the comparison is that measurement
		// against the database it produced.
		var err error
		recorded, err = v.readRecordedFingerprints(ctx)
		if err != nil {
			return res, err
		}
		v.log("read %d recorded source fingerprint(s) from the build", countRecorded(recorded))
	} else {
		v.log("reference: live sources read directly, not the build's recorded fingerprints")
	}

	for _, tp := range v.plans {
		v.log("table %-34s scope=%s", tp.Target, tp.Scope)
		var expectedByGroup map[string]Fingerprint
		if normalizeReference(v.opts.Reference) == ReferenceLive {
			var err error
			expectedByGroup, err = v.expectedFromLiveSources(ctx, tp)
			if err != nil {
				return res, err
			}
			v.log("  live %-31s %d group(s) expected from the sources as they stand now", tp.Target, len(expectedByGroup))
		} else {
			expectedByGroup = combineRecorded(recorded[tp.Target])
		}

		// The conflict beads and their copies are left out of the aggregate on
		// this side exactly as the conflicted ids are on the source side; they are
		// verified one id at a time (confirmConflict).
		actualByGroup, err := v.unified.FingerprintByGroup(ctx, v.opts.Database, tp.Target,
			tp.ScopeColumnName(), unifiedGroupColumn(tp.Scope), v.plan.mergedExclusions())
		if err != nil {
			return res, err
		}
		v.log("  unified %-32s %d group(s)", tp.Target, len(actualByGroup))

		groups := map[string]bool{}
		for g := range expectedByGroup {
			groups[g] = true
		}
		for g := range actualByGroup {
			groups[g] = true
		}
		for _, g := range sortedKeys(groups) {
			exp := expectedByGroup[g]
			act := actualByGroup[g]
			scope := "namespace " + g
			if tp.Scope == ScopeDatabaseState {
				scope = "store " + g
			}
			res.Checks = append(res.Checks, Check{
				Scope: scope, Table: tp.Target,
				Expected: exp, Actual: act, OK: exp == act,
			})
			if tp.Scope == ScopeDatabaseState {
				res.StoresChecked++
			} else {
				res.NamespacesChecked++
			}
		}

		var want, got Fingerprint
		for _, fp := range expectedByGroup {
			want = Combine(want, fp)
		}
		for _, fp := range actualByGroup {
			got = Combine(got, fp)
		}
		res.Checks = append(res.Checks, Check{
			Scope: "all groups", Table: tp.Target,
			Expected: want, Actual: got, OK: want == got,
		})
	}

	confirmed, conflicts, failed, err := v.confirmCollisions(ctx)
	if err != nil {
		return res, err
	}
	res.CollisionsConfirmed = confirmed
	res.ConflictsConfirmed = conflicts
	res.Checks = append(res.Checks, failed...)

	sortChecks(res.Checks)
	return res, nil
}

// expectedFromLiveSources computes what the unified database must hold for
// one table, measured directly from the participating sources as they stand
// now. It is the same measurement the builder took at copy time — same
// server-side expressions, same exclusion of collision losers — shifted to
// the present. This is the side a replay is judged against.
func (v *Verifier) expectedFromLiveSources(ctx context.Context, tp TablePlan) (map[string]Fingerprint, error) {
	out := map[string]Fingerprint{}
	for _, src := range v.plan.Sources {
		if !src.Reachable {
			continue
		}
		has, err := v.source.HasTable(ctx, src.Database, tp.Table)
		if err != nil {
			return nil, fmt.Errorf("checking %s.%s: %w", src.Database, tp.Table, err)
		}
		if !has {
			continue
		}
		switch tp.Scope {
		case ScopeDatabaseState:
			fp, err := v.source.ReferenceDigest(ctx, src.Database, &tp, src.Namespace)
			if err != nil {
				return nil, fmt.Errorf("live digest of %s.%s (store %s): %w", src.Database, tp.Table, src.Namespace, err)
			}
			if fp.Rows > 0 {
				out[src.Namespace] = Combine(out[src.Namespace], fp)
			}
		default:
			byGroup, err := v.source.FingerprintByGroup(ctx, src.Database, tp.Table,
				tp.ScopeColumnName(), "", v.plan.sourceExclusions(src.Namespace))
			if err != nil {
				return nil, fmt.Errorf("live fingerprint of %s.%s (store %s): %w", src.Database, tp.Table, src.Namespace, err)
			}
			for g, fp := range byGroup {
				if fp.Rows == 0 {
					continue
				}
				out[g] = Combine(out[g], fp)
			}
		}
	}
	return out, nil
}

// readRecordedFingerprints reads what the build recorded it read, keyed by
// table and then by namespace/store.
func (v *Verifier) readRecordedFingerprints(ctx context.Context) (map[string]map[string]Fingerprint, error) {
	stmt := fmt.Sprintf(
		"select `table_name`, `group_name`, `row_count`, `byte_count`, `hash_value` from `%s`.`brain_unify_source_fingerprints`",
		v.opts.Database)
	rows, err := v.unified.query(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("reading recorded source fingerprints: %w", err)
	}
	defer rows.Close()
	out := map[string]map[string]Fingerprint{}
	for rows.Next() {
		var table, group string
		var n int64
		var b, h float64
		if err := rows.Scan(&table, &group, &n, &b, &h); err != nil {
			return nil, err
		}
		if out[table] == nil {
			out[table] = map[string]Fingerprint{}
		}
		out[table][group] = Fingerprint{Rows: n, Bytes: int64(b), Hash: int64(h)}
	}
	return out, rows.Err()
}

func combineRecorded(byGroup map[string]Fingerprint) map[string]Fingerprint {
	out := map[string]Fingerprint{}
	for g, fp := range byGroup {
		out[g] = Combine(out[g], fp)
	}
	return out
}

func countRecorded(recorded map[string]map[string]Fingerprint) int {
	var n int
	for _, m := range recorded {
		n += len(m)
	}
	return n
}

// unifiedGroupColumn is the grouping column in the unified database: the
// store column for re-keyed tables, and nothing (the id prefix) for the rest.
func unifiedGroupColumn(scope Scope) string {
	if scope == ScopeDatabaseState {
		return storeColumn
	}
	return ""
}

// confirmCollisions re-reads the recorded collision decisions out of the
// unified database and confirms each against the sources.
//
// For an identical duplicate it confirms that exactly one copy of the id
// survived, that it is the recorded winner, and that every skipped copy is on
// record. For a conflict it confirms the conflict bead, exactly the copies it
// names, and each copy's rows (confirmConflict). This is the check that would
// catch a migration which quietly dropped a relationship.
//
// A collision that does not match is a failed check in the report, not an
// aborted run: the table comparisons beside it still print, so a reader sees
// everything that differs at once. Only a read that cannot be made at all
// aborts.
func (v *Verifier) confirmCollisions(ctx context.Context) (confirmed, conflicts int, failed []Check, err error) {
	fail := func(id, detail string) {
		failed = append(failed, Check{Scope: "collision " + id, Table: "brain_unify_collisions", Detail: detail})
	}
	known := map[string]bool{}
	orphans, err := v.orphanCounts(ctx)
	if err != nil {
		return confirmed, conflicts, failed, err
	}
	for _, c := range v.plan.Collisions {
		known[c.ID] = true
		if c.Divergent {
			problems, err := v.confirmConflict(ctx, c, orphans)
			if err != nil {
				return confirmed, conflicts, failed, err
			}
			if len(problems) > 0 {
				fail(c.ID, "conflict: "+strings.Join(problems, "; "))
				continue
			}
			confirmed++
			conflicts++
			continue
		}
		copies := 0
		stmt := fmt.Sprintf("select count(*) from `%s`.`issues` where `id` = ?", v.opts.Database)
		row := v.unified.queryRow(ctx, stmt, c.ID)
		if row == nil {
			return confirmed, conflicts, failed, fmt.Errorf("refusing non-SELECT collision check for %s", c.ID)
		}
		if err := row.Scan(&copies); err != nil {
			return confirmed, conflicts, failed, fmt.Errorf("counting %s in the unified database: %w", c.ID, err)
		}
		if copies != 1 {
			fail(c.ID, fmt.Sprintf("appears %d time(s) in the unified database, want exactly 1", copies))
			continue
		}
		stmt = fmt.Sprintf("select `winner`, `losers`, `resolution` from `%s`.`brain_unify_collisions` where `id` = ?", v.opts.Database)
		r := v.unified.queryRow(ctx, stmt, c.ID)
		if r == nil {
			return confirmed, conflicts, failed, fmt.Errorf("refusing non-SELECT collision lookup for %s", c.ID)
		}
		var winner, losers, resolution string
		if err := r.Scan(&winner, &losers, &resolution); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				fail(c.ID, fmt.Sprintf("is duplicated across %s (winner %s) but the unified database records no collision for it", strings.Join(append([]string{c.Winner}, c.Losers...), ", "), c.Winner))
				continue
			}
			return confirmed, conflicts, failed, fmt.Errorf("reading recorded collision %s: %w", c.ID, err)
		}
		if resolution != ResolutionMergedIdentical {
			fail(c.ID, fmt.Sprintf("recorded as %q but the sources hold identical copies, which merge into one bead", resolution))
			continue
		}
		if winner != c.Winner {
			fail(c.ID, fmt.Sprintf("recorded winner %q but the sources' winner rule chooses %q", winner, c.Winner))
			continue
		}
		var recordedLosers []string
		if err := json.Unmarshal([]byte(losers), &recordedLosers); err != nil {
			fail(c.ID, fmt.Sprintf("recorded losers %q are unreadable: %v", losers, err))
			continue
		}
		sort.Strings(recordedLosers)
		if !equalStrings(recordedLosers, c.Losers) {
			fail(c.ID, fmt.Sprintf("recorded losers [%s] but the sources have [%s]", strings.Join(recordedLosers, ", "), strings.Join(c.Losers, ", ")))
			continue
		}
		confirmed++
	}

	// The reverse direction: a record that no duplicated id backs any more.
	stmt := fmt.Sprintf("select `id` from `%s`.`brain_unify_collisions`", v.opts.Database)
	rows, qerr := v.unified.query(ctx, stmt)
	if qerr != nil {
		return confirmed, conflicts, failed, fmt.Errorf("reading recorded collisions: %w", qerr)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return confirmed, conflicts, failed, err
		}
		if !known[id] {
			fail(id, "the unified database records a collision for an id the sources no longer duplicate")
		}
	}
	return confirmed, conflicts, failed, rows.Err()
}

// orphanCounts counts, per merged table and conflicted id, the rows that stores
// holding no copy of the id have for it. Such rows are not moved to any copy;
// they stay at the original id, beside the conflict bead's own rows.
func (v *Verifier) orphanCounts(ctx context.Context) (map[string]map[string]int64, error) {
	out := map[string]map[string]int64{}
	conflicts := v.plan.Conflicts()
	if len(conflicts) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(conflicts))
	byID := map[string]Collision{}
	for _, c := range conflicts {
		ids = append(ids, c.ID)
		byID[c.ID] = c
	}
	for _, src := range v.plan.Sources {
		for _, tp := range beadTablePlans(v.plans) {
			has, err := v.source.HasTable(ctx, src.Database, tp.Table)
			if err != nil {
				return nil, fmt.Errorf("checking store %s for table %s: %w", src.Namespace, tp.Table, err)
			}
			if !has {
				continue
			}
			clause, args := inClause(tp.ScopeColumn, ids)
			stmt := fmt.Sprintf("select `%s`, count(*) from `%s`.`%s` where %s group by `%s`", tp.ScopeColumn, src.Database, tp.Table, clause, tp.ScopeColumn)
			rows, err := v.source.query(ctx, stmt, args...)
			if err != nil {
				return nil, fmt.Errorf("counting the rows of store %s table %s that name a conflicted id: %w", src.Namespace, tp.Table, err)
			}
			for rows.Next() {
				var raw any
				var n int64
				if err := rows.Scan(&raw, &n); err != nil {
					_ = rows.Close()
					return nil, err
				}
				id := cellText(raw)
				if contains(byID[id].holders(), src.Namespace) {
					continue
				}
				if out[tp.Target] == nil {
					out[tp.Target] = map[string]int64{}
				}
				out[tp.Target][id] += n
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// confirmConflict checks one conflict against the unified database and returns
// what is wrong with it, or nothing:
//
//   - the record: it says conflict-bead and names exactly the copies the sources
//     call for, each under the minted id the sources' ids derive;
//   - the conflict bead: the original id is one open issue, whose rows (and its
//     label and its dependency to each copy) are exactly the rows the tool
//     authors, and nothing else sits at the id but rows of stores holding no
//     copy;
//   - each copy: one issue under its minted id, and with its child rows a digest
//     equal to that of the source's copy put through the build's mapper - read
//     from the sources as they stand now (--reference live) or as the build
//     recorded it (recorded). Children resolve because they are read by the
//     minted id.
//
// "Nothing extra" is the aggregate comparison beside this one: a bead the plan
// does not name, in the namespace of a minted id, is a difference there.
func (v *Verifier) confirmConflict(ctx context.Context, c Collision, orphans map[string]map[string]int64) ([]string, error) {
	var problems []string

	recRow := v.unified.queryRow(ctx, fmt.Sprintf("select `resolution`, `copy_ids`, `copy_hashes` from `%s`.`brain_unify_collisions` where `id` = ?", v.opts.Database), c.ID)
	if recRow == nil {
		return nil, fmt.Errorf("refusing non-SELECT collision lookup for %s", c.ID)
	}
	var resolution, copyIDsJSON, copyHashesJSON string
	recorded := true
	if err := recRow.Scan(&resolution, &copyIDsJSON, &copyHashesJSON); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("reading recorded collision %s: %w", c.ID, err)
		}
		recorded = false
		problems = append(problems, "the unified database records no collision for it")
	}
	recordedHashes := map[string]string{}
	if recorded {
		var gotIDs map[string]string
		if resolution != ResolutionConflict {
			problems = append(problems, fmt.Sprintf("recorded as %q, want %q", resolution, ResolutionConflict))
		}
		if err := json.Unmarshal([]byte(copyIDsJSON), &gotIDs); err != nil || !equalStringMaps(gotIDs, c.copyIDs()) {
			problems = append(problems, fmt.Sprintf("records the copies %s but the sources call for %s", copyIDsJSON, mapText(c.copyIDs())))
		}
		if err := json.Unmarshal([]byte(copyHashesJSON), &recordedHashes); err != nil {
			problems = append(problems, fmt.Sprintf("recorded copy digests %q are unreadable", copyHashesJSON))
		}
	}

	problems = append(problems, v.checkConflictBead(ctx, c, orphans)...)

	for _, cp := range c.Copies {
		src, ok := v.plan.SourceByNamespace(cp.Store)
		if !ok {
			problems = append(problems, fmt.Sprintf("the copy held by store %s has no participating source", cp.Store))
			continue
		}
		want, err := copyRowsFromSource(ctx, v.source, v.plans, v.mapper, src, c.ID)
		if err != nil {
			return nil, err
		}
		got, err := copyRowsFromMerged(ctx, v.unified, v.opts.Database, v.plans, want.Cols, cp.ID)
		if err != nil {
			return nil, err
		}
		if n := len(got.Rows["issues"]); n != 1 {
			problems = append(problems, fmt.Sprintf("the copy from store %s is %d row(s) of issues under %s, want exactly 1", cp.Store, n, cp.ID))
			continue
		}
		gotDigest := digestCopy(got.Rows)
		if normalizeReference(v.opts.Reference) == ReferenceLive {
			if wantDigest := digestCopy(want.Rows); wantDigest != gotDigest {
				problems = append(problems, fmt.Sprintf("the copy from store %s (%s) differs from the source's copy in %s", cp.Store, cp.ID, strings.Join(differingTables(want.Rows, got.Rows), ", ")))
			}
			continue
		}
		wantDigest, ok := recordedHashes[cp.Store]
		switch {
		case !ok && recorded:
			problems = append(problems, fmt.Sprintf("the record has no digest for the copy from store %s", cp.Store))
		case ok && wantDigest != gotDigest:
			problems = append(problems, fmt.Sprintf("the copy from store %s (%s) no longer matches the digest the build recorded for it (%s vs %s)", cp.Store, cp.ID, shortDigestText(wantDigest), shortDigestText(gotDigest)))
		}
	}
	return problems, nil
}

func shortDigestText(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

func mapText(m map[string]string) string {
	b, _ := json.Marshal(m)
	return string(b)
}

// differingTables names the tables in which two sets of rows differ.
func differingTables(a, b map[string][][]any) []string {
	names := map[string]bool{}
	for t := range a {
		names[t] = true
	}
	for t := range b {
		names[t] = true
	}
	var out []string
	for _, t := range sortedKeys(names) {
		if digestCopy(map[string][][]any{t: a[t]}) != digestCopy(map[string][][]any{t: b[t]}) {
			out = append(out, t)
		}
	}
	return out
}

// checkConflictBead compares what sits at the conflicted id in every
// bead-scoped table with the rows the tool authors for the conflict. The rows
// are matched on the columns the tool provides; a row that matches none of them
// must be one of a store that holds no copy.
func (v *Verifier) checkConflictBead(ctx context.Context, c Collision, orphans map[string]map[string]int64) []string {
	var problems []string
	for _, tp := range beadTablePlans(v.plans) {
		wantCols, wantRows := tableConflictRows(tp, []Collision{c})
		rows, err := v.unified.ReadRows(ctx, v.opts.Database, tp.Target, tp.Columns, "`"+tp.ScopeColumn+"` = ?", c.ID)
		if err != nil {
			problems = append(problems, fmt.Sprintf("reading %s at the conflict id: %v", tp.Target, err))
			continue
		}
		idx := make([]int, len(wantCols))
		for i, col := range wantCols {
			idx[i] = indexOf(tp.Columns, col)
		}
		project := func(row []any) string {
			var b strings.Builder
			for _, i := range idx {
				b.WriteString(cellText(row[i]))
				b.WriteByte(0x1f)
			}
			return b.String()
		}
		wantSet := map[string]int{}
		for _, row := range wantRows {
			var b strings.Builder
			for _, cell := range row {
				b.WriteString(cellText(cell))
				b.WriteByte(0x1f)
			}
			wantSet[b.String()]++
		}
		strays := int64(0)
		for _, row := range rows {
			key := project(row)
			if wantSet[key] > 0 {
				wantSet[key]--
				continue
			}
			strays++
		}
		missing := 0
		for _, n := range wantSet {
			missing += n
		}
		if missing > 0 {
			problems = append(problems, fmt.Sprintf("%s lacks %d of the %d row(s) the conflict bead should have", tp.Target, missing, len(wantRows)))
		}
		if want := orphans[tp.Target][c.ID]; strays != want {
			problems = append(problems, fmt.Sprintf("%s holds %d other row(s) at the conflict id, want %d (only rows of stores holding no copy stay there)", tp.Target, strays, want))
		}
	}
	return problems
}

// Combine merges disjoint fingerprints. Counts and sizes sum; digests combine
// by XOR, which is exactly what the server's own bit_xor aggregation does.
func Combine(a, b Fingerprint) Fingerprint {
	return Fingerprint{
		Rows:  a.Rows + b.Rows,
		Bytes: a.Bytes + b.Bytes,
		Hash:  a.Hash ^ b.Hash,
	}
}

// sortChecks orders the report so two runs of the verifier produce a diffable
// output.
func sortChecks(checks []Check) {
	sort.SliceStable(checks, func(i, j int) bool {
		if checks[i].Table != checks[j].Table {
			return checks[i].Table < checks[j].Table
		}
		return checks[i].Scope < checks[j].Scope
	})
}
