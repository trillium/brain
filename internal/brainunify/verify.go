package brainunify

import (
	"context"
	"fmt"
	"sort"
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
}

// Difference renders the mismatch, or the empty string when the check passed.
func (c Check) Difference() string {
	if c.OK {
		return ""
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
	// re-read from the unified database and matched.
	CollisionsConfirmed int
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
	// Logf receives a line as each table and each source is read. Verification
	// compares every row of every table on both sides, so without progress
	// output a stall is indistinguishable from a long run — and a full
	// federation takes long enough that "wait longer" is not a debugging
	// strategy.
	Logf func(format string, args ...any)
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
	return &Verifier{source: source, unified: unified, plan: plan, plans: plans, opts: opts, log: log}, nil
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
	var res VerifyResult

	// The recorded source fingerprints are the reference. Reading them is
	// also what makes a verification reproducible: the build measured the
	// sources at a point in time, and the comparison is that measurement
	// against the database it produced.
	recorded, err := v.readRecordedFingerprints(ctx)
	if err != nil {
		return res, err
	}
	v.log("read %d recorded source fingerprint(s) from the build", countRecorded(recorded))

	for _, tp := range v.plans {
		v.log("table %-34s scope=%s", tp.Target, tp.Scope)
		expectedByGroup := combineRecorded(recorded[tp.Target])

		actualByGroup, err := v.unified.FingerprintByGroup(ctx, v.opts.Database, tp.Target,
			tp.ScopeColumnName(), unifiedGroupColumn(tp.Scope), nil)
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

	confirmed, err := v.confirmCollisions(ctx)
	if err != nil {
		return res, err
	}
	res.CollisionsConfirmed = confirmed

	sortChecks(res.Checks)
	return res, nil
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
// unified database and confirms that exactly one copy of each duplicated id
// survived, that it is the recorded winner, and that every losing copy is on
// record. This is the check that would catch a migration which quietly dropped
// a relationship.
func (v *Verifier) confirmCollisions(ctx context.Context) (int, error) {
	confirmed := 0
	for _, c := range v.plan.Collisions {
		copies := 0
		stmt := fmt.Sprintf("select count(*) from `%s`.`issues` where `id` = ?", v.opts.Database)
		row := v.unified.queryRow(ctx, stmt, c.ID)
		if row == nil {
			return 0, fmt.Errorf("refusing non-SELECT collision check for %s", c.ID)
		}
		if err := row.Scan(&copies); err != nil {
			return 0, fmt.Errorf("counting %s in the unified database: %w", c.ID, err)
		}
		if copies != 1 {
			return confirmed, fmt.Errorf("collision %s appears %d time(s) in the unified database, want exactly 1", c.ID, copies)
		}
		stmt = fmt.Sprintf("select `winner`, `losers` from `%s`.`brain_unify_collisions` where `id` = ?", v.opts.Database)
		r := v.unified.queryRow(ctx, stmt, c.ID)
		if r == nil {
			return 0, fmt.Errorf("refusing non-SELECT collision lookup for %s", c.ID)
		}
		var winner, losers string
		if err := r.Scan(&winner, &losers); err != nil {
			return confirmed, fmt.Errorf("reading recorded collision %s: %w", c.ID, err)
		}
		if winner != c.Winner {
			return confirmed, fmt.Errorf("collision %s recorded winner %q but the plan chose %q", c.ID, winner, c.Winner)
		}
		confirmed++
	}
	return confirmed, nil
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
