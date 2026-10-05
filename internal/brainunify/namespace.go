package brainunify

import (
	"sort"
	"strings"
)

// UnattributedNamespace is the namespace recorded for beads whose id prefix
// no source declares and no source is named after. It is a real bucket in
// production data (the feedtack database mints "fe-" ids), so it is tracked
// rather than treated as an error.
const UnattributedNamespace = "__unattributed__"

// PrefixOf returns the namespace prefix of a bead id: everything before the
// first '-'. Hierarchical ids keep their parent namespace, so
// "brain-se7t.389" has prefix "brain". An id with no '-' has no prefix and
// is returned whole.
func PrefixOf(id string) string {
	if i := strings.IndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	return id
}

// Namespace is one id prefix and the source database that owns it.
//
// Ownership is the whole of the "preserve logical separation" claim: after
// unification, "which store is this bead in" is answered by looking up the
// bead's id prefix here, so ownership must be deterministic and explicable
// even where two databases mint ids in the same prefix.
type Namespace struct {
	// Prefix is the id prefix.
	Prefix string
	// Owner is the Namespace of the source database that owns the prefix.
	Owner string
	// DeclaredBy lists sources whose config.yaml claims the prefix.
	DeclaredBy []string
	// ObservedBy lists sources that actually contain ids with the prefix.
	ObservedBy []string
	// BeadCount is the number of ids observed in the prefix across all
	// sources (duplicates counted once per source).
	BeadCount int64
	// Ambiguous reports that more than one source contributed ids to this
	// prefix. Ambiguity is normal; it only becomes a data problem when the
	// same id appears twice, which Plan reports separately.
	Ambiguous bool
	// OwnerReason explains, in one clause, why Owner won. Every namespace row
	// carries it so the mapping is auditable without re-running the tool.
	OwnerReason string
}

// Ownership reasons, recorded verbatim in the brain_store_prefixes table.
const (
	ReasonDeclared     = "declared-by-store-config"
	ReasonNameMatch    = "store-name-matches-prefix"
	ReasonUnattributed = "no-source-declares-or-matches"
	ReasonObserved     = "first-observing-source"
)

// BuildNamespaces computes prefix ownership from the observed prefixes per
// source. observed maps a source namespace to the prefixes that source
// contains, declared maps a source namespace to the prefixes the source claims
// in its config.yaml, and counts maps a prefix to the number of ids observed
// in it across all sources (counted once per source, so a duplicated id
// contributes to both of its sources).
//
// names optionally supplies, per source namespace, the other names that source
// goes by. The Dolt database name matters: the registry calls the assertions
// store "assertions" while its database — and its ids — are "assert", so name
// equality is checked against the database name as well as the namespace.
//
// The precedence, applied in order, is:
//
//  1. a source that DECLARES the prefix in its config.yaml owns it. If more
//     than one declares it, the lexicographically smallest namespace wins so
//     the outcome does not depend on map iteration order.
//  2. otherwise, a source whose own name equals the prefix owns it — a
//     database named "question" is the natural owner of "question-" ids even
//     though its config.yaml never says so.
//  3. otherwise the prefix is UNATTRIBUTED: it still migrates, still keeps
//     its ids, and is recorded as belonging to no store.
//  4. observed sources are recorded on the Namespace either way, so a prefix
//     shared by two databases is visible rather than implied.
func BuildNamespaces(observed, declared map[string][]string, counts map[string]int64, names map[string][]string) map[string]Namespace {
	all := map[string]*Namespace{}
	get := func(prefix string) *Namespace {
		ns, ok := all[prefix]
		if !ok {
			ns = &Namespace{Prefix: prefix}
			all[prefix] = ns
		}
		return ns
	}

	for _, src := range sortedKeys(observed) {
		for _, prefix := range observed[src] {
			ns := get(prefix)
			ns.ObservedBy = append(ns.ObservedBy, src)
		}
	}
	for _, src := range sortedKeys(declared) {
		for _, prefix := range declared[src] {
			ns := get(prefix)
			ns.DeclaredBy = append(ns.DeclaredBy, src)
		}
	}

	for prefix, ns := range all {
		sort.Strings(ns.DeclaredBy)
		sort.Strings(ns.ObservedBy)
		ns.Ambiguous = len(ns.ObservedBy) > 1
		ns.BeadCount = counts[prefix]
		nameMatch := hasExactNameMatch(ns.ObservedBy, prefix, names)
		switch {
		case len(ns.DeclaredBy) > 0:
			ns.Owner = ns.DeclaredBy[0]
			ns.OwnerReason = ReasonDeclared
		case nameMatch != "":
			ns.Owner = nameMatch
			ns.OwnerReason = ReasonNameMatch
		default:
			ns.Owner = UnattributedNamespace
			ns.OwnerReason = ReasonUnattributed
		}
	}
	out := make(map[string]Namespace, len(all))
	for _, ns := range all {
		out[ns.Prefix] = *ns
	}
	// A declared-but-unobserved prefix still exists: it is the namespace a
	// store will mint new ids in, and it must be present in the unified
	// database even when the store is empty today.
	return out
}

// hasExactNameMatch returns the observing source whose unified namespace or
// Dolt database name equals the prefix, or "" when none does.
func hasExactNameMatch(sources []string, prefix string, names map[string][]string) string {
	for _, s := range sources {
		if s == prefix {
			return s
		}
	}
	for _, s := range sources {
		for _, alias := range names[s] {
			if alias == prefix {
				return s
			}
		}
	}
	return ""
}

// sortedKeys returns the map's keys in sorted order so every rule that picks
// a winner is deterministic regardless of Go's map iteration order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SortedNamespaces returns namespaces ordered by prefix, for stable output.
func SortedNamespaces(ns map[string]Namespace) []Namespace {
	out := make([]Namespace, 0, len(ns))
	for _, n := range ns {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Prefix < out[j].Prefix })
	return out
}
