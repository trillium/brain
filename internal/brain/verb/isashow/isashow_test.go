package isashow

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/brain/verb/isasection"
)

// TestSpecOrderedSectionNames pins the ordering. If the spec order ever
// drifts, this test fails loudly so consumers don't silently see a different
// markdown render order.
func TestSpecOrderedSectionNames(t *testing.T) {
	got := SpecOrderedSectionNames()
	want := []string{
		"problem", "vision", "out_of_scope", "principles",
		"constraints", "goal", "criteria", "test_strategy",
		"features", "decisions", "changelog", "verification",
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d sections, got %d", len(want), len(got))
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("position %d: expected %q, got %q", i, name, got[i])
		}
	}
}

// TestSpecOrderCoversAllCanonicalNames guarantees SpecOrderedSectionNames is
// in lockstep with isasection.ValidSections — adding a new canonical section
// must add it to both places or this test fails.
func TestSpecOrderCoversAllCanonicalNames(t *testing.T) {
	ordered := SpecOrderedSectionNames()
	if len(ordered) != len(isasection.ValidSections) {
		t.Fatalf("SpecOrderedSectionNames has %d entries but isasection.ValidSections has %d — they must match",
			len(ordered), len(isasection.ValidSections))
	}
	for _, name := range ordered {
		if !isasection.IsValidSection(name) {
			t.Errorf("spec order includes %q which isasection.IsValidSection rejects", name)
		}
	}
}

// TestISAProgressMarshalJSON pins the {"m": M, "n": N} object shape. This is
// part of the ISC-16 wire contract: it must not regress to two top-level
// `isa_progress_m` / `isa_progress_n` fields or a tuple.
func TestISAProgressMarshalJSON(t *testing.T) {
	p := ISAProgress{M: 3, N: 7}
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"m":3,"n":7}`
	if string(out) != want {
		t.Errorf("expected %s, got %s", want, string(out))
	}
}

// TestISADocJSONShape is the golden-string test. The exact pretty-printed
// bytes are pinned so any drift in field order, indentation, or progress
// shape lights up immediately. This is the ISC-16 contract.
func TestISADocJSONShape(t *testing.T) {
	started := time.Date(2026, 6, 2, 10, 30, 0, 0, time.UTC)
	updated := time.Date(2026, 6, 2, 11, 45, 0, 0, time.UTC)
	doc := &ISADoc{
		ID:           "isa-001",
		Slug:         "ship-the-thing",
		Kind:         "isa",
		ISAPhase:     "BUILD",
		ISAProgress:  ISAProgress{M: 4, N: 12},
		ISAEffort:    "E3",
		ISAMode:      "algorithm",
		ISAStartedAt: &started,
		ISAUpdatedAt: &updated,
		Sections: map[string]string{
			"problem": "we lack a substrate",
			"vision":  "we have a substrate",
		},
	}

	out, err := doc.MarshalJSONIndent()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	want := `{
  "id": "isa-001",
  "slug": "ship-the-thing",
  "kind": "isa",
  "isa_phase": "BUILD",
  "isa_progress": {
    "m": 4,
    "n": 12
  },
  "isa_effort": "E3",
  "isa_mode": "algorithm",
  "isa_started_at": "2026-06-02T10:30:00Z",
  "isa_updated_at": "2026-06-02T11:45:00Z",
  "sections": {
    "problem": "we lack a substrate",
    "vision": "we have a substrate"
  }
}`
	if string(out) != want {
		t.Errorf("JSON shape drift.\nwant:\n%s\n\ngot:\n%s", want, string(out))
	}
}

// TestISADocNullTimestamps verifies that a doc with nil time pointers
// serializes the timestamp fields as JSON null, not as the zero time.
func TestISADocNullTimestamps(t *testing.T) {
	doc := &ISADoc{
		ID:          "isa-002",
		Kind:        "isa",
		ISAProgress: ISAProgress{M: 0, N: 0},
		Sections:    map[string]string{},
	}

	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"isa_started_at":null`) {
		t.Errorf("expected isa_started_at to serialize as null, got: %s", s)
	}
	if !strings.Contains(s, `"isa_updated_at":null`) {
		t.Errorf("expected isa_updated_at to serialize as null, got: %s", s)
	}
}

// TestRenderMarkdownSpecOrder verifies sections are emitted in spec order
// regardless of the order they were inserted into the Sections map.
// We insert in reverse-spec order; the output must still be problem → vision.
func TestRenderMarkdownSpecOrder(t *testing.T) {
	doc := &ISADoc{
		ID: "isa-003",
		Sections: map[string]string{
			"verification": "verify body",
			"changelog":    "changelog body",
			"problem":      "problem body",
			"vision":       "vision body",
		},
	}
	md := RenderMarkdown(doc)

	// Find positions of each section heading. Spec order: problem before
	// vision before changelog before verification.
	idxProblem := strings.Index(md, "## Problem")
	idxVision := strings.Index(md, "## Vision")
	idxChangelog := strings.Index(md, "## Changelog")
	idxVerification := strings.Index(md, "## Verification")

	for name, idx := range map[string]int{
		"## Problem":      idxProblem,
		"## Vision":       idxVision,
		"## Changelog":    idxChangelog,
		"## Verification": idxVerification,
	} {
		if idx < 0 {
			t.Fatalf("expected %q heading in output, got:\n%s", name, md)
		}
	}
	if !(idxProblem < idxVision && idxVision < idxChangelog && idxChangelog < idxVerification) {
		t.Errorf("expected spec order Problem<Vision<Changelog<Verification, got positions %d, %d, %d, %d",
			idxProblem, idxVision, idxChangelog, idxVerification)
	}
}

// TestRenderMarkdownSkipsEmptySections verifies sections with empty or
// whitespace-only bodies are omitted entirely — no blank headings in output.
func TestRenderMarkdownSkipsEmptySections(t *testing.T) {
	doc := &ISADoc{
		ID: "isa-004",
		Sections: map[string]string{
			"problem":    "real body",
			"vision":     "",
			"goal":       "   \n\t\n",
			"principles": "another real body",
		},
	}
	md := RenderMarkdown(doc)

	if !strings.Contains(md, "## Problem") {
		t.Error("expected Problem heading to be rendered")
	}
	if !strings.Contains(md, "## Principles") {
		t.Error("expected Principles heading to be rendered")
	}
	if strings.Contains(md, "## Vision") {
		t.Errorf("did not expect empty Vision heading in output:\n%s", md)
	}
	if strings.Contains(md, "## Goal") {
		t.Errorf("did not expect whitespace-only Goal heading in output:\n%s", md)
	}
}

// TestRenderMarkdownEmptyDoc returns "" on a nil doc so callers can pipe the
// output without nil-check ceremony.
func TestRenderMarkdownEmptyDoc(t *testing.T) {
	if got := RenderMarkdown(nil); got != "" {
		t.Errorf("expected empty string for nil doc, got %q", got)
	}
}

// TestRenderMarkdownHeader checks the top-level header includes id and slug
// when slug is present, and just id otherwise.
func TestRenderMarkdownHeader(t *testing.T) {
	withSlug := &ISADoc{
		ID:       "isa-005",
		Slug:     "my-isa-slug",
		Sections: map[string]string{"problem": "x"},
	}
	if md := RenderMarkdown(withSlug); !strings.HasPrefix(md, "# isa-005 — my-isa-slug\n") {
		t.Errorf("expected header to combine id and slug, got: %s", md)
	}

	withoutSlug := &ISADoc{
		ID:       "isa-006",
		Sections: map[string]string{"problem": "x"},
	}
	if md := RenderMarkdown(withoutSlug); !strings.HasPrefix(md, "# isa-006\n") {
		t.Errorf("expected header to be just id when slug empty, got: %s", md)
	}
}

// TestRenderSectionPassthrough verifies RenderSection is a no-op on the body.
func TestRenderSectionPassthrough(t *testing.T) {
	body := "raw\nbody\nwith\nnewlines\n"
	if got := RenderSection(body); got != body {
		t.Errorf("RenderSection should be identity, got %q want %q", got, body)
	}
}
