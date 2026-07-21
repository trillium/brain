// Package isashow implements the pure-Go read-side primitives for the
// `bd isa-show` verb. It defines the canonical JSON document shape used by
// `bd isa-show --json`, deterministic markdown rendering for the human-facing
// output, and section-name ordering.
//
// Database access (the SELECT against issues + isa_sections, kind gating,
// not-found handling) lives in cmd/bd/isa_show.go because it needs the
// embedded-Dolt store. This package is fully pure-Go so its JSON-shape
// stability and markdown ordering can be exercised by fast unit tests without
// the cgo build tag.
package isashow

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/brain/verb/isasection"
)

// SpecOrderedSectionNames returns the twelve canonical ISA section names in
// the order locked by the ISA format spec (PAI/DOCUMENTATION/IsaFormat.md):
// problem, vision, out_of_scope, principles, constraints, goal, criteria,
// test_strategy, features, decisions, changelog, verification.
//
// This order is used both for JSON serialization (Sections field order via
// custom marshalling) and markdown rendering. It is deliberately not
// alphabetical — the spec order is part of the contract.
//
// isasection.SortedSectionNames() returns the same names alphabetized for
// error messages and help text; the two orderings coexist on purpose.
func SpecOrderedSectionNames() []string {
	return []string{
		isasection.SectionProblem,
		isasection.SectionVision,
		isasection.SectionOutOfScope,
		isasection.SectionPrinciples,
		isasection.SectionConstraints,
		isasection.SectionGoal,
		isasection.SectionCriteria,
		isasection.SectionTestStrategy,
		isasection.SectionFeatures,
		isasection.SectionDecisions,
		isasection.SectionChangelog,
		isasection.SectionVerification,
	}
}

// sectionTitles maps canonical lower_snake_case section names to the
// human-facing markdown headings used by RenderMarkdown.
var sectionTitles = map[string]string{
	isasection.SectionProblem:      "Problem",
	isasection.SectionVision:       "Vision",
	isasection.SectionOutOfScope:   "Out of Scope",
	isasection.SectionPrinciples:   "Principles",
	isasection.SectionConstraints:  "Constraints",
	isasection.SectionGoal:         "Goal",
	isasection.SectionCriteria:     "Criteria",
	isasection.SectionTestStrategy: "Test Strategy",
	isasection.SectionFeatures:     "Features",
	isasection.SectionDecisions:    "Decisions",
	isasection.SectionChangelog:    "Changelog",
	isasection.SectionVerification: "Verification",
}

// ISAProgress is the {m, n} progress pair attached to every ISA.
// It marshals as a JSON object so the wire shape is `{"m": N, "n": N}`,
// matching the ISC-16 contract.
type ISAProgress struct {
	M int
	N int
}

// MarshalJSON emits `{"m": M, "n": N}`. Using a stable key order (m before n)
// is intentional — golden-string tests in isashow_test.go rely on it.
func (p ISAProgress) MarshalJSON() ([]byte, error) {
	type progressWire struct {
		M int `json:"m"`
		N int `json:"n"`
	}
	return json.Marshal(progressWire{M: p.M, N: p.N})
}

// ISADoc is the JSON document shape for `bd isa-show --json`. The field order
// in the struct definition matches the JSON key order (Go's encoding/json
// preserves struct field order), which is part of the wire contract.
//
// *time.Time pointers serialize as `null` when nil — used for isa_started_at
// and isa_updated_at which may legitimately be unset on a fresh ISA row.
//
// Sections is a map keyed by canonical section name. Map iteration order is
// undefined in Go, but encoding/json sorts keys alphabetically when marshaling
// a map — so the JSON output for `sections` is alphabetical and stable.
// (The markdown renderer uses SpecOrderedSectionNames() to emit spec order;
// the JSON consumer is expected to look up by key, not iterate.)
type ISADoc struct {
	ID           string            `json:"id"`
	Slug         string            `json:"slug"`
	Kind         string            `json:"kind"`
	ISAPhase     string            `json:"isa_phase"`
	ISAProgress  ISAProgress       `json:"isa_progress"`
	ISAEffort    string            `json:"isa_effort"`
	ISAMode      string            `json:"isa_mode"`
	ISAStartedAt *time.Time        `json:"isa_started_at"`
	ISAUpdatedAt *time.Time        `json:"isa_updated_at"`
	Sections     map[string]string `json:"sections"`
}

// MarshalJSONIndent returns the canonical pretty-printed JSON for the doc.
// Centralizing this here lets the golden-string test pin the exact byte
// sequence (indentation: two spaces, no trailing newline).
func (d *ISADoc) MarshalJSONIndent() ([]byte, error) {
	return json.MarshalIndent(d, "", "  ")
}

// RenderMarkdown assembles the ISA as markdown for human reading. Sections
// are emitted in spec order (problem → verification); sections whose body is
// empty are skipped so the output doesn't carry blank headings.
//
// The current rendering is intentionally minimal: a `## <Title>` heading per
// section followed by a blank line and the body. The doc does not yet carry
// a title field (F1c-1/F1c-2 scope writes ISA-specific columns but the title
// belongs to the broader issues row and isn't surfaced here), so the top-level
// header is the issue id alone. A richer header (id + title + phase badge)
// can be layered on later without breaking the JSON contract.
func RenderMarkdown(doc *ISADoc) string {
	if doc == nil {
		return ""
	}

	var buf bytes.Buffer

	// Top-level header. Slug is included when present so a `cat`'d output
	// names the ISA the way humans refer to it.
	if doc.Slug != "" {
		buf.WriteString("# ")
		buf.WriteString(doc.ID)
		buf.WriteString(" — ")
		buf.WriteString(doc.Slug)
		buf.WriteString("\n\n")
	} else {
		buf.WriteString("# ")
		buf.WriteString(doc.ID)
		buf.WriteString("\n\n")
	}

	for _, name := range SpecOrderedSectionNames() {
		body, ok := doc.Sections[name]
		if !ok {
			continue
		}
		if strings.TrimSpace(body) == "" {
			continue
		}
		buf.WriteString("## ")
		buf.WriteString(sectionTitles[name])
		buf.WriteString("\n\n")
		buf.WriteString(body)
		// Ensure separation between sections — bodies sometimes lack a
		// trailing newline.
		if !strings.HasSuffix(body, "\n") {
			buf.WriteString("\n")
		}
		buf.WriteString("\n")
	}

	return buf.String()
}

// RenderSection returns the raw body for `--section=<name>` output. The body
// is returned verbatim — callers asked for the section text, not a wrapped
// "here is your section" envelope.
func RenderSection(body string) string {
	return body
}
