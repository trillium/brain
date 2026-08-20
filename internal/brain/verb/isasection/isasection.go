// Package isasection implements pure-Go validation for the `bd isa-section`
// verb.
//
// `bd isa-section <id> <section-name>` writes one of the twelve canonical ISA
// document sections (Problem, Vision, Out of Scope, …) to the isa_sections
// table. The body is supplied either via --value-from-file (avoiding shell
// arg-length limits) or via --value-stdin (for pipeline use).
//
// This package owns the kind-agnostic concerns: the canonical 12-name set,
// validation entry, and a typed ValidationError so the cobra command in
// cmd/bd/isa_section.go can format and exit consistently with the patch verb.
// Database work (transaction, UPSERT, isa_updated_at touch, wrong-kind /
// not-found gating) lives in the cobra layer because it depends on the
// embedded-Dolt store.
package isasection

import (
	"fmt"
	"sort"
	"strings"
)

// Canonical section name constants. The twelve names are locked to the ISA
// format spec (PAI/DOCUMENTATION/IsaFormat.md) and the body order documented
// there.
const (
	SectionProblem      = "problem"
	SectionVision       = "vision"
	SectionOutOfScope   = "out_of_scope"
	SectionPrinciples   = "principles"
	SectionConstraints  = "constraints"
	SectionGoal         = "goal"
	SectionCriteria     = "criteria"
	SectionTestStrategy = "test_strategy"
	SectionFeatures     = "features"
	SectionDecisions    = "decisions"
	SectionChangelog    = "changelog"
	SectionVerification = "verification"
)

// ValidSections is the canonical lower_snake_case set. ISA section names are
// case-sensitive (lower_snake_case only). Anything else is rejected.
var ValidSections = map[string]struct{}{
	SectionProblem:      {},
	SectionVision:       {},
	SectionOutOfScope:   {},
	SectionPrinciples:   {},
	SectionConstraints:  {},
	SectionGoal:         {},
	SectionCriteria:     {},
	SectionTestStrategy: {},
	SectionFeatures:     {},
	SectionDecisions:    {},
	SectionChangelog:    {},
	SectionVerification: {},
}

// ValidationError is the typed error this verb raises for input shape
// problems (unknown section name, empty name). The cobra layer asserts on
// this type and exits with code 2.
type ValidationError struct {
	Msg string
}

func (e *ValidationError) Error() string { return e.Msg }

// IsValidSection returns true when name is one of the twelve canonical
// section names. It is case-sensitive on purpose: ISA section names are a
// fixed lower_snake_case alphabet, not a free-form label.
func IsValidSection(name string) bool {
	_, ok := ValidSections[name]
	return ok
}

// ValidateSectionName returns a *ValidationError if name is empty or not a
// member of the canonical set. The error message enumerates every valid
// section name in sorted order so the user can copy/paste a correction.
func ValidateSectionName(name string) error {
	if name == "" {
		return &ValidationError{Msg: "section name is required"}
	}
	if !IsValidSection(name) {
		return &ValidationError{
			Msg: fmt.Sprintf(
				"unknown ISA section %q; valid sections: %s",
				name, strings.Join(SortedSectionNames(), ", "),
			),
		}
	}
	return nil
}

// SortedSectionNames returns the twelve valid section names in alphabetical
// order. Stable output is useful for error messages and help text.
func SortedSectionNames() []string {
	names := make([]string, 0, len(ValidSections))
	for k := range ValidSections {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
