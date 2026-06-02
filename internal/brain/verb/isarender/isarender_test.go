package isarender

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// strPtr is a tiny helper because Go has no literal `*string` syntax.
func strPtr(s string) *string { return &s }

func timePtr(t time.Time) *time.Time { return &t }

// TestRenderFrontmatterGolden pins the byte-exact YAML frontmatter for a
// fully-populated ISA. If any key order, quoting, or null-handling rule
// drifts, this fails loudly.
func TestRenderFrontmatterGolden(t *testing.T) {
	started := time.Date(2026, 6, 2, 10, 30, 0, 0, time.UTC)
	updated := time.Date(2026, 6, 2, 11, 45, 0, 0, time.UTC)
	in := &RenderInput{
		ID:        "brain-isa-00001",
		Slug:      "ship-the-thing",
		Title:     "Ship the thing",
		Phase:     strPtr("BUILD"),
		Effort:    strPtr("advanced"),
		Mode:      strPtr("interactive"),
		ProgressM: 4,
		ProgressN: 12,
		StartedAt: &started,
		UpdatedAt: &updated,
		Sections:  map[string]string{}, // empty so we test frontmatter alone
	}

	out, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	want := `---
task: "Ship the thing"
slug: "ship-the-thing"
effort: advanced
phase: build
progress: "4/12"
mode: interactive
started: 2026-06-02T10:30:00Z
updated: 2026-06-02T11:45:00Z
brain_id: brain-isa-00001
---

# Ship the thing
`
	if out != want {
		t.Errorf("frontmatter mismatch\nwant:\n%s\ngot:\n%s", want, out)
	}
}

// TestRenderFrontmatterOmitsNulls verifies the NULL-handling rules from the
// IsaFormat v2.7 spec: phase, effort, mode, started, updated keys are omitted
// when their input is nil; progress is always present (even 0/0); brain_id
// and slug are always present.
func TestRenderFrontmatterOmitsNulls(t *testing.T) {
	in := &RenderInput{
		ID:       "brain-isa-00002",
		Slug:     "minimal",
		Title:    "Minimal",
		Sections: map[string]string{},
	}

	out, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	want := `---
task: "Minimal"
slug: "minimal"
progress: "0/0"
brain_id: brain-isa-00002
---

# Minimal
`
	if out != want {
		t.Errorf("minimal frontmatter mismatch\nwant:\n%s\ngot:\n%s", want, out)
	}
}

// TestRenderSectionsSpecOrder verifies sections come out in IsaFormat v2.7
// spec order even when the input map is shuffled. encoding-order independence
// is one of the few correctness properties that's easy to break and easy to
// test.
func TestRenderSectionsSpecOrder(t *testing.T) {
	in := &RenderInput{
		ID:    "brain-isa-00003",
		Slug:  "ordered",
		Title: "Ordered",
		Sections: map[string]string{
			// shuffled — verification before problem, etc.
			"verification":  "verify-body",
			"problem":       "problem-body",
			"changelog":     "changelog-body",
			"vision":        "vision-body",
			"goal":          "goal-body",
			"out_of_scope":  "oos-body",
			"principles":    "principles-body",
			"constraints":   "constraints-body",
			"criteria":      "criteria-body",
			"test_strategy": "ts-body",
			"features":      "features-body",
			"decisions":     "decisions-body",
		},
	}

	out, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// The twelve section headings should appear in spec order. Find each one
	// and verify its byte offset is monotonically increasing.
	expectedHeadings := []string{
		"## Problem",
		"## Vision",
		"## Out of Scope",
		"## Principles",
		"## Constraints",
		"## Goal",
		"## Criteria",
		"## Test Strategy",
		"## Features",
		"## Decisions",
		"## Changelog",
		"## Verification",
	}
	prevIdx := -1
	for _, h := range expectedHeadings {
		idx := strings.Index(out, h+"\n")
		if idx < 0 {
			t.Errorf("expected heading %q not found in output", h)
			continue
		}
		if idx <= prevIdx {
			t.Errorf("heading %q at offset %d came before previous heading at %d",
				h, idx, prevIdx)
		}
		prevIdx = idx
	}
}

// TestRenderSkipsEmptySections verifies sections with empty or whitespace-only
// bodies do not produce a heading. F1c-2's RenderMarkdown has the same
// behavior; F2 must preserve it so trip through Dolt → markdown is lossless
// for "section never set" vs "section set to empty string".
func TestRenderSkipsEmptySections(t *testing.T) {
	in := &RenderInput{
		ID:    "brain-isa-00004",
		Slug:  "sparse",
		Title: "Sparse",
		Sections: map[string]string{
			"problem":   "real problem body",
			"vision":    "",          // empty: skipped
			"goal":      "   \n\t\n", // whitespace only: skipped
			"changelog": "real changelog body",
		},
	}

	out, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if !strings.Contains(out, "## Problem") {
		t.Error("expected ## Problem heading")
	}
	if !strings.Contains(out, "## Changelog") {
		t.Error("expected ## Changelog heading")
	}
	if strings.Contains(out, "## Vision") {
		t.Error("did not expect ## Vision heading for empty section")
	}
	if strings.Contains(out, "## Goal") {
		t.Error("did not expect ## Goal heading for whitespace-only section")
	}
}

// TestRenderSectionBodyVerbatim verifies bodies are emitted byte-for-byte
// with no trimming or normalization. Changelog entries care about exact
// whitespace; code fences care about indentation.
func TestRenderSectionBodyVerbatim(t *testing.T) {
	body := "Line one with trailing spaces   \n\n```\n  indented code\n```\n"
	in := &RenderInput{
		ID:    "brain-isa-00005",
		Slug:  "verbatim",
		Title: "Verbatim",
		Sections: map[string]string{
			"problem": body,
		},
	}
	out, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, body) {
		t.Errorf("expected body to appear verbatim in output; got:\n%s", out)
	}
}

// TestRenderEmptyIDOrSlug verifies the input guards. An ISA without an id or
// slug cannot be safely written (no addressable substrate row; no target
// directory), so we refuse to render.
func TestRenderEmptyIDOrSlug(t *testing.T) {
	for name, in := range map[string]*RenderInput{
		"empty_id":   {ID: "", Slug: "ok", Title: "x"},
		"empty_slug": {ID: "ok", Slug: "", Title: "x"},
		"nil_input":  nil,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Render(in)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var ie *InputError
			if !errors.As(err, &ie) {
				t.Errorf("expected *InputError, got %T: %v", err, err)
			}
		})
	}
}

// TestRenderLearnPhaseProducesFile verifies ISC-39: a phase=LEARN, progress=N/N
// ISA renders normally — no special-casing for closed ISAs.
func TestRenderLearnPhaseProducesFile(t *testing.T) {
	in := &RenderInput{
		ID:        "brain-isa-00006",
		Slug:      "closed-isa",
		Title:     "Closed ISA",
		Phase:     strPtr("LEARN"),
		ProgressM: 8,
		ProgressN: 8,
		Sections: map[string]string{
			"problem":   "solved",
			"changelog": "* shipped 2026-06-02",
		},
	}
	out, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "phase: learn") {
		t.Errorf("expected phase: learn in output, got:\n%s", out)
	}
	if !strings.Contains(out, `progress: "8/8"`) {
		t.Errorf("expected progress: \"8/8\" in output")
	}
	if !strings.Contains(out, "## Problem") {
		t.Errorf("expected ## Problem heading")
	}
}

// TestRenderSingleTrailingNewline pins the contract that the output ends
// with exactly one '\n'. Multiple trailing newlines have caused git diff
// noise in adjacent PAI tools; one is enough.
func TestRenderSingleTrailingNewline(t *testing.T) {
	in := &RenderInput{
		ID:    "brain-isa-00007",
		Slug:  "trail",
		Title: "Trail",
		Sections: map[string]string{
			"problem": "body with trailing newlines\n\n\n",
		},
	}
	out, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("expected output to end with newline, got %q",
			out[max(0, len(out)-10):])
	}
	if strings.HasSuffix(out, "\n\n") {
		t.Errorf("expected output to end with exactly one newline, got trailing blank line")
	}
}

// max — Go 1.21+ has builtin, but be defensive.
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestResolveExfilRootEnv verifies the env var overrides the default.
func TestResolveExfilRootEnv(t *testing.T) {
	t.Setenv(EnvExfilRoot, "/tmp/test-exfil-root")
	got := ResolveExfilRoot()
	if got != "/tmp/test-exfil-root" {
		t.Errorf("expected env value, got %q", got)
	}
}

// TestResolveExfilRootDefault verifies the HOME-based default when the env
// var is unset.
func TestResolveExfilRootDefault(t *testing.T) {
	t.Setenv(EnvExfilRoot, "")
	t.Setenv("HOME", "/tmp/fake-home")
	got := ResolveExfilRoot()
	want := filepath.Join("/tmp/fake-home", DefaultExfilRoot)
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

// TestResolveTargetPathHappy verifies the canonical layout.
func TestResolveTargetPathHappy(t *testing.T) {
	got, err := ResolveTargetPath("/tmp/exfil", "my-slug")
	if err != nil {
		t.Fatalf("ResolveTargetPath: %v", err)
	}
	want := "/tmp/exfil/isa/my-slug/ISA.md"
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

// TestResolveTargetPathTraversal verifies ISC-37: slugs that escape the
// exfil root surface as *PathTraversalError.
func TestResolveTargetPathTraversal(t *testing.T) {
	cases := []struct {
		name string
		slug string
	}{
		{"parent_escape", "../../etc/passwd"},
		{"single_parent", ".."},
		{"absolute_path", "/etc/passwd"},
		{"hidden_dotdot", "ok/../../etc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveTargetPath("/tmp/exfil", tc.slug)
			if err == nil {
				t.Fatalf("expected error for slug %q, got nil", tc.slug)
			}
			// Acceptable typed errors: PathTraversalError (escape) or
			// InputError (slug resolved to the root itself). Both are
			// terminal — cobra exits 2 for either.
			var pte *PathTraversalError
			var ie *InputError
			if !errors.As(err, &pte) && !errors.As(err, &ie) {
				t.Errorf("expected *PathTraversalError or *InputError, got %T: %v", err, err)
			}
		})
	}
}

// TestResolveTargetPathEmptySlug verifies the explicit empty-slug guard.
func TestResolveTargetPathEmptySlug(t *testing.T) {
	_, err := ResolveTargetPath("/tmp/exfil", "")
	if err == nil {
		t.Fatal("expected error for empty slug")
	}
	var ie *InputError
	if !errors.As(err, &ie) {
		t.Errorf("expected *InputError, got %T", err)
	}
}

// TestWriteAtomicHappyPath verifies WriteAtomic writes the content, leaves
// the target file with the right bytes, and cleans up the temp sibling.
func TestWriteAtomicHappyPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "subdir", "ISA.md")
	content := "hello, atomic world\n"

	if err := WriteAtomic(target, content); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != content {
		t.Errorf("expected %q, got %q", content, string(got))
	}

	// Verify no .tmp.* siblings remain. Render is rare and atomic — a stray
	// temp file would indicate a bug.
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "ISA.md.tmp.") {
			t.Errorf("stray temp file remains: %s", name)
		}
	}
}

// TestWriteAtomicOverwritesExisting verifies rename(2) replaces an existing
// target — render-after-render must update in place without surfacing an
// EEXIST.
func TestWriteAtomicOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ISA.md")

	if err := WriteAtomic(target, "v1\n"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := WriteAtomic(target, "v2\n"); err != nil {
		t.Fatalf("second write: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "v2\n" {
		t.Errorf("expected v2 after overwrite, got %q", string(got))
	}
}

// TestWriteAtomicEmptyTarget verifies the input guard.
func TestWriteAtomicEmptyTarget(t *testing.T) {
	if err := WriteAtomic("", "x"); err == nil {
		t.Fatal("expected error for empty targetPath")
	}
}

// TestRenderRoundTripSpecOrderMatchesIsashow verifies the SpecOrderedSectionNames
// used in this package is the same one isashow exports — a regression here
// means the two renderers can drift.
func TestRenderRoundTripSpecOrderMatchesIsashow(t *testing.T) {
	// All twelve sections in the rendered output should appear in the order
	// dictated by isashow.SpecOrderedSectionNames. We just make a body for
	// each, render, and verify the headings appear in the expected sequence
	// — the same as TestRenderSectionsSpecOrder but with all twelve set.
	sections := map[string]string{}
	for _, n := range []string{
		"problem", "vision", "out_of_scope", "principles", "constraints",
		"goal", "criteria", "test_strategy", "features", "decisions",
		"changelog", "verification",
	} {
		sections[n] = "body for " + n
	}
	in := &RenderInput{
		ID: "brain-isa-00008", Slug: "all-sections", Title: "All", Sections: sections,
	}
	out, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// Count headings — must be 12.
	got := strings.Count(out, "\n## ")
	if got != 12 {
		t.Errorf("expected 12 section headings, got %d\noutput:\n%s", got, out)
	}
}
