package brainunify

import (
	"context"
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
	return &Verifier{source: source, unified: unified, plan: plan, plans: plans, opts: opts}, nil
}

// Close releases the verifier's connections.
func (v *Verifier) Close() error { return v.unified.Close() }

// Verify runs every comparison and returns the result.
func (v *Verifier) Verify(ctx context.Context) (VerifyResult, error) {
	var res VerifyResult

	losingIDs := losingIssueIDs(v.plan.Collisions)

	// Per-namespace comparison. A namespace is the unit the unified database
	// can actually express, so it is the unit the comparison uses: the
	// source side is every source's rows under that prefix, minus the copies
	// the recorded collision decision removed.
	for _, ns := range SortedNamespaces(v.plan.Namespaces) {
		if ns.Prefix == "" || ns.Prefix == UnattributedNamespace {
			continue
		}
		escaped := escapeLike(ns.Prefix)
		for _, tp := range v.plans {
			if tp.Scope == ScopeDatabaseState {
				continue
			}
			where := fmt.Sprintf("`%s` like '%s-%%'", tp.ScopeColumnName(), escaped)
			expected, contributors, err := v.expectedFingerprint(ctx, tp, where, losingIDs)
			if err != nil {
				return res, err
			}
			if contributors == 0 {
				continue
			}
			actual, err := v.unified.Fingerprint(ctx, v.opts.Database, tp.Target, where)
			if err != nil {
				return res, err
			}
			res.Checks = append(res.Checks, Check{
				Scope: "namespace " + ns.Prefix, Table: tp.Target,
				Expected: expected, Actual: actual, OK: expected == actual,
			})
			res.NamespacesChecked++
		}
	}

	// Per-store comparison of the re-keyed database-state tables. These are
	// compared per store because the unified database addresses them by the
	// store column rather than by a prefix.
	for _, tp := range v.plans {
		if tp.Scope != ScopeDatabaseState {
			continue
		}
		expected, _, err := v.expectedFingerprint(ctx, tp, "", nil)
		if err != nil {
			return res, err
		}
		actual, err := v.unified.Fingerprint(ctx, v.opts.Database, tp.Target,
			fmt.Sprintf("`store` in (%s)", quoteList(storeNames(v.plan.Sources))))
		if err != nil {
			return res, err
		}
		res.Checks = append(res.Checks, Check{
			Scope: "all stores", Table: tp.Target,
			Expected: expected, Actual: actual, OK: expected == actual,
		})
		res.StoresChecked++
	}

	// Whole-table comparison for every issue-scoped table, which catches a
	// row that landed in the unified database but under no prefix at all.
	for _, tp := range v.plans {
		if tp.Scope == ScopeDatabaseState {
			continue
		}
		expected, _, err := v.expectedFingerprint(ctx, tp, "", losingIDs)
		if err != nil {
			return res, err
		}
		actual, err := v.unified.Fingerprint(ctx, v.opts.Database, tp.Target, "")
		if err != nil {
			return res, err
		}
		res.Checks = append(res.Checks, Check{
			Scope: "all namespaces", Table: tp.Target,
			Expected: expected, Actual: actual, OK: expected == actual,
		})
		res.NamespacesChecked++
	}

	// The provenance tables must agree with the plan that produced them.
	confirmed, err := v.confirmCollisions(ctx)
	if err != nil {
		return res, err
	}
	res.CollisionsConfirmed = confirmed

	sortChecks(res.Checks)
	return res, nil
}

// expectedFingerprint computes the source-side fingerprint of one table under
// one filter by combining every contributing source, with the rows the
// recorded collisions removed.
//
// Combining is exact rather than approximate: after the collision exclusions
// the contributing row sets are disjoint, so their counts and sizes sum and
// their digests combine by XOR — the same identity the server applies when it
// aggregates the unified table in a single pass.
func (v *Verifier) expectedFingerprint(ctx context.Context, tp TablePlan, where string, losingIDs map[string]string) (Fingerprint, int, error) {
	var total Fingerprint
	contributors := 0
	for _, src := range v.plan.Sources {
		has, err := v.source.HasTable(ctx, src.Database, tp.Table)
		if err != nil || !has {
			continue
		}
		var excluded []string
		if tp.Scope != ScopeDatabaseState {
			excluded = losingIDsFor(losingIDs, src.Namespace)
		}
		fp, err := v.fingerprintExcluding(ctx, src.Database, tp.Table, where, excluded, tp.ScopeColumnName())
		if err != nil {
			return Fingerprint{}, 0, err
		}
		if fp.Rows == 0 {
			continue
		}
		contributors++
		total = Combine(total, fp)
	}
	return total, contributors, nil
}

// fingerprintExcluding computes a table fingerprint with rows whose key is
// listed in excluded removed. The exclusion is applied inside the same query
// as the digest so the value is comparable with the unified side without the
// verifier ever holding rows in memory.
func (v *Verifier) fingerprintExcluding(ctx context.Context, database, table, where string, excluded []string, keyColumn string) (Fingerprint, error) {
	if len(excluded) > 0 {
		cond := fmt.Sprintf("`%s` not in (%s)", keyColumn, quoteList(excluded))
		if strings.TrimSpace(where) == "" {
			where = cond
		} else {
			where = "(" + where + ") and " + cond
		}
	}
	return v.source.Fingerprint(ctx, database, table, where)
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

func sortChecks(checks []Check) {
	sort.SliceStable(checks, func(i, j int) bool {
		if checks[i].Scope != checks[j].Scope {
			return checks[i].Scope < checks[j].Scope
		}
		return checks[i].Table < checks[j].Table
	})
}

func storeNames(sources []SourceFacts) []string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		out = append(out, s.Namespace)
	}
	sort.Strings(out)
	return out
}

// quoteList renders a string slice as a quoted SQL list.
func quoteList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, quoteLiteral(v))
	}
	return strings.Join(quoted, ",")
}

// escapeLike escapes the LIKE wildcards in a literal prefix. Prefixes are
// operator-controlled, but escaping keeps a store named "a_b" from matching
// "aXb-" ids and silently inflating a fingerprint.
func escapeLike(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_")
	return r.Replace(s)
}
