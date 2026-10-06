// Package dolt — unified_namespace.go
//
// The unified database holds every store's beads in one `issues` table; a
// wrapper's day-one read paths (list, search, counts, ready, blocked,
// render-all, statistics) must therefore scope themselves to the wrapper's
// own namespace (docs/design/brain-single-database.md, pre-cutover checklist
// item 5, plus the cutover probe's Question (c) reproductions).
//
// The namespace a wrapper addresses comes from the wrapper itself (BD_NAME,
// the same source the id-prefix machinery uses), never from the database.
// The prefixes that belong to it come from the build's own
// brain_store_prefixes table — the same mapping the winner rule and the
// namespace answers agree with — falling back to the wrapper's name when
// that table is absent.
package dolt

import (
	"context"
	"sync"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// unifiedNamespaces caches the resolved namespace prefixes for the connected
// database: nil entry (absent) = not yet resolved; empty slice + unifiedOK
// false = legacy database; otherwise the namespace's prefixes.
type unifiedNamespaceCache struct {
	once     sync.Once
	prefixes []string
	unified  bool
}

// namespacePrefixes answers the id prefixes the wrapper's namespace owns on
// the unified database. unified=false means the connected database is not
// unified (or could not be probed) and no scoping should be applied.
func (s *DoltStore) namespacePrefixes(ctx context.Context) ([]string, bool) {
	s.unifiedNS.once.Do(func() {
		s.unifiedNS.prefixes, s.unifiedNS.unified = resolveNamespacePrefixes(ctx, s.db)
	})
	return s.unifiedNS.prefixes, s.unifiedNS.unified
}

func resolveNamespacePrefixes(ctx context.Context, q issueops.DBTX) ([]string, bool) {
	sc := issueops.UnifiedScopeForTx(ctx, q)
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

// scopeNamespaces pins an issue-read filter to the wrapper's namespaces on
// the unified database. No-op on a legacy database, and never overrides a
// caller that set Namespaces itself (e.g. the federated search walker).
func (s *DoltStore) scopeNamespaces(ctx context.Context, f types.IssueFilter) types.IssueFilter {
	if f.Namespaces != nil {
		return f
	}
	if prefixes, unified := s.namespacePrefixes(ctx); unified && len(prefixes) > 0 {
		f.Namespaces = prefixes
	}
	return f
}

func (s *DoltStore) scopeWorkNamespaces(ctx context.Context, f types.WorkFilter) types.WorkFilter {
	if f.Namespaces != nil {
		return f
	}
	if prefixes, unified := s.namespacePrefixes(ctx); unified && len(prefixes) > 0 {
		f.Namespaces = prefixes
	}
	return f
}

// statisticNamespaces answers the namespace prefixes statistics should count
// (empty on a legacy database).
func (s *DoltStore) statisticNamespaces(ctx context.Context) []string {
	prefixes, unified := s.namespacePrefixes(ctx)
	if !unified {
		return nil
	}
	return prefixes
}
