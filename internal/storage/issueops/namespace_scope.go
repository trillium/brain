package issueops

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/steveyegge/beads/internal/types"
)

// This file is the namespace READ scope for the unified database, in the one
// package every read seam already imports. A wrapper's reads address either
// its own namespaces (NARROW, the default) or every store (WIDE, the operator's
// --wide); see internal/storage/dolt/unified_namespace.go for the rule's
// rationale.
//
// It lives here, below both the DoltStore and the unit-of-work provider, so the
// two read seams cannot disagree: the store scopes a filter before it reaches
// issueops, and the provider's issue repository (internal/storage/domain/db)
// scopes it through the same functions. `bd serve` answers from the provider,
// so before this file a served unified database answered for every store.

var namespaceReadWide atomic.Bool

// SetNamespaceReadWide pins the process-wide read mode. A CLI run picks it once
// at startup, before the first query.
func SetNamespaceReadWide(wide bool) { namespaceReadWide.Store(wide) }

// NamespaceReadWide reports whether reads are unscoped (every store).
func NamespaceReadWide() bool { return namespaceReadWide.Load() }

// ResolveNamespacePrefixes answers the id prefixes the wrapper's namespace owns
// on the unified database. unified=false means the connected database is not
// unified (or has no pinned namespace) and no scoping applies.
//
// It reads brain_store_prefixes on every call and caches nothing: a long-lived
// server must see 'bd store-prefix add' without a restart.
func ResolveNamespacePrefixes(ctx context.Context, q DBTX) ([]string, bool) {
	sc := UnifiedScopeForTx(ctx, q)
	if !sc.Unified || sc.Store == "" {
		return nil, false
	}
	prefixes := []string{sc.Store}
	rows, err := q.QueryContext(ctx,
		"SELECT prefix FROM brain_store_prefixes WHERE `store` = ?", sc.Store)
	if err == nil {
		defer rows.Close()
		var found []string
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				found = nil
				break
			}
			found = append(found, p)
		}
		if err := rows.Err(); err != nil {
			found = nil
		}
		if len(found) > 0 {
			prefixes = found
		}
	}
	return prefixes, true
}

// narrowPrefixes answers the prefixes a narrow read must be pinned to, or nil
// when the read stays unscoped (wide mode, a legacy database, no namespace).
func narrowPrefixes(ctx context.Context, q DBTX) []string {
	if NamespaceReadWide() {
		return nil
	}
	if prefixes, unified := ResolveNamespacePrefixes(ctx, q); unified && len(prefixes) > 0 {
		return prefixes
	}
	return nil
}

// ScopeIssueFilter pins an issue-read filter to the wrapper's namespaces. A
// no-op on a legacy database and in wide mode, and a caller-set Namespaces is
// never overridden.
func ScopeIssueFilter(ctx context.Context, q DBTX, f types.IssueFilter) types.IssueFilter {
	if f.Namespaces != nil {
		return f
	}
	if prefixes := narrowPrefixes(ctx, q); prefixes != nil {
		f.Namespaces = prefixes
	}
	return f
}

// ScopeWorkFilter is ScopeIssueFilter for ready/blocked reads. The ready-active
// custom statuses are the wrapper's own on the unified database in BOTH read
// modes: the statuses are settings, not beads.
func ScopeWorkFilter(ctx context.Context, q DBTX, f types.WorkFilter) types.WorkFilter {
	if f.CustomStatusStore == "" {
		if sc := UnifiedScopeForTx(ctx, q); sc.Enabled() {
			f.CustomStatusStore = sc.Store
		}
	}
	if f.Namespaces != nil {
		return f
	}
	if prefixes := narrowPrefixes(ctx, q); prefixes != nil {
		f.Namespaces = prefixes
	}
	return f
}

// ScopeStaleFilter is ScopeIssueFilter for stale reads.
func ScopeStaleFilter(ctx context.Context, q DBTX, f types.StaleFilter) types.StaleFilter {
	if f.Namespaces != nil {
		return f
	}
	if prefixes := narrowPrefixes(ctx, q); prefixes != nil {
		f.Namespaces = prefixes
	}
	return f
}

// StatisticNamespaces answers the prefixes statistics should count (nil on a
// legacy database and in wide mode, where the unfiltered query is the answer).
func StatisticNamespaces(ctx context.Context, q DBTX) []string {
	return narrowPrefixes(ctx, q)
}

// NamespaceAllowsIssueID reports whether id is visible to this wrapper. In a
// narrow unified store, an issue belongs to a namespace only when its ID has
// one of that store's registered prefixes followed by a dash.
func NamespaceAllowsIssueID(ctx context.Context, q DBTX, id string) bool {
	return len(NamespaceFilterIssueIDs(ctx, q, []string{id})) == 1
}

// NamespaceFilterIssueIDs returns the ids visible to this wrapper, resolving
// the namespace prefixes once for the whole batch. The input is not modified.
func NamespaceFilterIssueIDs(ctx context.Context, q DBTX, ids []string) []string {
	prefixes := narrowPrefixes(ctx, q)
	if prefixes == nil {
		return ids
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		for _, prefix := range prefixes {
			if strings.HasPrefix(id, prefix+"-") {
				out = append(out, id)
				break
			}
		}
	}
	return out
}
