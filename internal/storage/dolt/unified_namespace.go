// Package dolt — unified_namespace.go
//
// The unified database holds every store's beads in one `issues` table; a
// wrapper's read paths (list, search, counts, ready, blocked, render-all,
// statistics) address namespaces in one of two deliberate modes
// (docs/design/brain-single-database.md, pre-cutover checklist item 5, plus
// the cutover probe's Question (c) reproductions):
//
//   - NARROW (default): the wrapper's own namespace only. The namespace a
//     wrapper addresses comes from the wrapper itself (BD_NAME, the same
//     source the id-prefix machinery uses), never from the database, and the
//     prefixes that belong to it come from the build's own
//     brain_store_prefixes table — the same mapping the winner rule and the
//     namespace answers agree with — falling back to the wrapper's name when
//     that table is absent.
//   - WIDE (--wide): every store. The merged table unscoped IS the all-stores
//     view, including beads whose prefix no source ever claimed. The
//     federated walk (`bd search --federated`) is the wide view's precedent:
//     on the unified database it reads the same unscoped rows and buckets
//     them per owning store, so --wide is the flat form of one mechanism,
//     not a second one.
//
// The operator picks the mode per command with a flag; nothing silently
// substitutes one mode for another. The mode is process-wide because a CLI
// run picks it once at startup, before the first query.
package dolt

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// NamespaceReadScope is the deliberate read mode for a wrapper's read paths
// on the unified database.
type NamespaceReadScope int

const (
	// NamespaceReadNarrow is the default: read paths scope themselves to the
	// wrapper's own namespaces.
	NamespaceReadNarrow NamespaceReadScope = iota
	// NamespaceReadWide leaves reads unscoped: on the unified database the
	// merged table without namespace restriction is the every-store view.
	NamespaceReadWide
)

// namespaceReadScope holds the process-wide read mode. Set once at command
// startup from the --wide flag; tests may pin it around individual reads.
var namespaceReadScope atomic.Int32

// SetNamespaceReadScope pins the process-wide read mode. Calling it before
// the first read is the contract; mid-run flips remain supported for tests.
func SetNamespaceReadScope(m NamespaceReadScope) {
	namespaceReadScope.Store(int32(m))
}

// namespaceReadScopeNow answers the active read mode.
func namespaceReadScopeNow() NamespaceReadScope {
	switch NamespaceReadScope(namespaceReadScope.Load()) {
	case NamespaceReadWide:
		return NamespaceReadWide
	default:
		return NamespaceReadNarrow
	}
}

// readScopeWide reports whether the active mode is wide.
func readScopeWide() bool {
	return namespaceReadScopeNow() == NamespaceReadWide
}

// resetNamespaceReadScopeForTest answers the active mode and resets it to
// narrow, so a test that pinned wide cannot leak into other tests.
func resetNamespaceReadScopeForTest() NamespaceReadScope {
	m := namespaceReadScopeNow()
	SetNamespaceReadScope(NamespaceReadNarrow)
	return m
}

// unifiedNamespaces caches the resolved namespace prefixes for the connected
// database: prefixes=nil + unified=false means a legacy database (no
// scoping); otherwise prefixes are the namespace's own.
type unifiedNamespaceCache struct {
	once     sync.Once
	prefixes []string
	unified  bool
}

// namespacePrefixesAnswers answers the narrow prefix set, unless the active
// mode is wide — wide reads do not narrow-scope, so the resolution never runs.
func (s *DoltStore) namespacePrefixes(ctx context.Context) ([]string, bool) {
	if readScopeWide() {
		return nil, false
	}
	s.unifiedNS.once.Do(func() {
		s.unifiedNS.prefixes, s.unifiedNS.unified = resolveNamespacePrefixes(ctx, s.db)
	})
	return s.unifiedNS.prefixes, s.unifiedNS.unified
}

// narrowPrefixes is the unguarded resolution used by callers that must resolve
// the namespaces regardless of the read mode (none today; kept so the probe
// has exactly one non-cached seam to mock in tests).
func (s *DoltStore) narrowPrefixes(ctx context.Context) ([]string, bool) {
	s.unifiedNS.once.Do(func() {
		s.unifiedNS.prefixes, s.unifiedNS.unified = resolveNamespacePrefixes(ctx, s.db)
	})
	return s.unifiedNS.prefixes, s.unifiedNS.unified
}

// resolveNamespacePrefixes answers the id prefixes the wrapper's namespace
// owns on the unified database. unified=false means the connected database is
// not unified (or could not be probed) and no scoping should be applied.
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
// caller that set Namespaces itself (e.g. the federated search walker). In
// wide mode the filter is returned untouched: unscoped on the unified
// database means "every store".
func (s *DoltStore) scopeNamespaces(ctx context.Context, f types.IssueFilter) types.IssueFilter {
	if f.Namespaces != nil || readScopeWide() {
		return f
	}
	if prefixes, unified := s.namespacePrefixes(ctx); unified && len(prefixes) > 0 {
		f.Namespaces = prefixes
	}
	return f
}

func (s *DoltStore) scopeWorkNamespaces(ctx context.Context, f types.WorkFilter) types.WorkFilter {
	if f.Namespaces != nil || readScopeWide() {
		return f
	}
	if prefixes, unified := s.namespacePrefixes(ctx); unified && len(prefixes) > 0 {
		f.Namespaces = prefixes
	}
	return f
}

// statisticNamespaces answers the namespace prefixes statistics should count
// (empty on a legacy database). In wide mode it answers empty — the wide
// view counts every store's beads, which is exactly the unfiltered query.
func (s *DoltStore) statisticNamespaces(ctx context.Context) []string {
	prefixes, unified := s.namespacePrefixes(ctx)
	if !unified {
		return nil
	}
	return prefixes
}
