// Package verb defines the BrainVerb seam — the load-bearing interface that
// every brain CLI verb implements.
//
// Decision #5 (modularity-first architecture, 2026-05-31) names BrainVerb as
// the first of five swappable seams that quarantine v0.3's eager defaults
// behind hours-to-swap interfaces rather than days-to-swap refactors.
//
// # Shape
//
// BrainVerb is a Go 1.18+ generic interface parameterised on the verb's
// concrete Args and Result types. The Cobra wrapper at
// cmd/bd/brain_<verb>.go knows the concrete types at compile time and gets
// type-safe calls into the engine package without runtime any-cast.
//
//	type myVerb struct{ store storage.DoltStorage }
//	func (myVerb) Name() string { return "my-verb" }
//	func (v myVerb) Run(ctx context.Context, a Args) (Result, error) { ... }
//
//	var _ verb.BrainVerb[Args, Result] = myVerb{}
//
// # Why generic
//
// An any-typed interface (`Run(any) (any, error)`) was the alternative.
// Generic won because:
//
//   - Every call site (the Cobra wrapper) already knows the verb's Args and
//     Result types at compile time. Erasing them to `any` just to widen the
//     interface adds a cast at every call with zero callers benefiting.
//   - There is no need today for a slice of heterogeneous verbs
//     (`[]BrainVerb`). The wrapper file knows its one verb.
//   - If a future tranche needs a heterogeneous registry, it can introduce
//     a non-generic `Verb` facet (Name() string + Run(any) (any, error))
//     alongside this generic one without breaking existing impls.
//
// # Trade-off accepted
//
// Generics force the wrapper to spell out Args/Result type parameters when
// constructing the verb. That is one extra line per wrapper; it pays for
// itself the first time someone refactors a Result struct and the compiler
// catches every call site.
package verb

import "context"

// BrainVerb is the contract every brain CLI verb implements.
//
// The Cobra wrapper in cmd/bd/brain_<verb>.go parses flags, builds the
// concrete Args value, calls Run, and formats the concrete Result for
// stdout / JSON. Swapping a verb's behaviour means replacing one Run impl;
// the Cobra wiring does not move.
type BrainVerb[Args any, Result any] interface {
	// Name is the verb word as it appears on the CLI ("new", "show",
	// "list", "link", "related", ...). It must match the Cobra command's
	// Use field's first whitespace-delimited token.
	Name() string

	// Run executes the verb. The context carries cancellation; Args
	// carries flags and positional input already parsed by the wrapper;
	// Result carries the structured output the wrapper formats.
	//
	// Implementations must not write to stdout/stderr directly — all
	// output formatting belongs in the Cobra wrapper. This keeps the
	// engine package testable without stdio capture.
	Run(ctx context.Context, args Args) (Result, error)
}
