package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheck_ConformantBundle asserts a well-formed bundle (two typed
// entries + a frontmatter-free index.md + a reserved log.md) passes with
// zero violations.
func TestCheck_ConformantBundle(t *testing.T) {
	rep, err := Check([]string{"testdata/conformant"})
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if rep.Failed != 0 {
		t.Fatalf("expected 0 failures, got %d: %+v", rep.Failed, rep.Violations)
	}
	// alpha.md, beta.md, index.md, log.md — all four are checked, none fail.
	if rep.Checked != 4 {
		t.Errorf("expected 4 files checked, got %d", rep.Checked)
	}
	if rep.Passed != 4 {
		t.Errorf("expected 4 files passed, got %d", rep.Passed)
	}
	if len(rep.Violations) != 0 {
		t.Errorf("expected no violations, got %+v", rep.Violations)
	}
}

// TestCheck_NonConformantBundle asserts a bundle with one type-less file
// and one unparseable-frontmatter file fails, names both exact files, and
// attributes the correct OKF requirement to each.
func TestCheck_NonConformantBundle(t *testing.T) {
	rep, err := Check([]string{"testdata/nonconformant"})
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if rep.Failed != 2 {
		t.Fatalf("expected 2 failures, got %d: %+v", rep.Failed, rep.Violations)
	}

	byBase := map[string]Violation{}
	for _, v := range rep.Violations {
		byBase[filepath.Base(v.File)] = v
	}

	missing, ok := byBase["missing-type.md"]
	if !ok {
		t.Fatalf("expected a violation for missing-type.md; got %+v", rep.Violations)
	}
	if !strings.Contains(missing.Reason, "OKF #2") {
		t.Errorf("missing-type.md should fail OKF #2, got reason: %q", missing.Reason)
	}

	bad, ok := byBase["bad-yaml.md"]
	if !ok {
		t.Fatalf("expected a violation for bad-yaml.md; got %+v", rep.Violations)
	}
	if !strings.Contains(bad.Reason, "OKF #1") {
		t.Errorf("bad-yaml.md should fail OKF #1, got reason: %q", bad.Reason)
	}
}

// TestCheck_SingleFile confirms a single .md file path is accepted and
// checked directly (not just directories).
func TestCheck_SingleFile(t *testing.T) {
	conformant := filepath.Join("testdata", "conformant", "entries", "knowledge", "alpha.md")
	rep, err := Check([]string{conformant})
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if rep.Checked != 1 || rep.Failed != 0 {
		t.Errorf("expected 1 checked / 0 failed for a conformant single file, got checked=%d failed=%d", rep.Checked, rep.Failed)
	}

	typeless := filepath.Join("testdata", "nonconformant", "entries", "knowledge", "missing-type.md")
	rep, err = Check([]string{typeless})
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if rep.Checked != 1 || rep.Failed != 1 {
		t.Errorf("expected 1 checked / 1 failed for a type-less single file, got checked=%d failed=%d", rep.Checked, rep.Failed)
	}
}

// TestCheck_MissingPath surfaces a non-existent path as an error (not a
// silent zero-violation pass — that would let CI green on a typo'd path).
func TestCheck_MissingPath(t *testing.T) {
	if _, err := Check([]string{"testdata/does-not-exist"}); err == nil {
		t.Fatal("expected an error for a non-existent path, got nil")
	}
}

// TestCheck_Dedup confirms a file reachable via two overlapping input
// paths (store root + its own entries/ dir) is checked exactly once.
func TestCheck_Dedup(t *testing.T) {
	rep, err := Check([]string{
		"testdata/conformant",
		"testdata/conformant/entries",
	})
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if rep.Checked != 4 {
		t.Errorf("expected 4 unique files checked after dedup, got %d", rep.Checked)
	}
}

// TestExtractFrontmatter exercises the fence-parsing helper directly for
// the shape edge cases the walk relies on.
func TestExtractFrontmatter(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantOK  bool
		wantHas string // substring that must appear in the extracted block
	}{
		{"well-formed", "---\ntype: knowledge\n---\n\nbody", true, "type: knowledge"},
		{"no-fence", "# just a heading\n", false, ""},
		{"opening-no-closing", "---\ntype: knowledge\nbody with no close", false, ""},
		{"empty-file", "", false, ""},
		{"bom-prefixed", "\xEF\xBB\xBF---\ntype: x\n---\n", true, "type: x"},
		{"crlf-fences", "---\r\ntype: x\r\n---\r\n", true, "type: x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fm, ok := extractFrontmatter([]byte(tc.in))
			if ok != tc.wantOK {
				t.Fatalf("extractFrontmatter ok = %v, want %v", ok, tc.wantOK)
			}
			if tc.wantOK && !strings.Contains(string(fm), tc.wantHas) {
				t.Errorf("extracted block %q does not contain %q", string(fm), tc.wantHas)
			}
		})
	}
}

// TestHasNonEmptyType covers the type-value validation rules: present +
// non-empty string passes; absent, empty, whitespace, nil, and non-string
// values all fail.
func TestHasNonEmptyType(t *testing.T) {
	cases := []struct {
		name string
		fm   map[string]any
		want bool
	}{
		{"present", map[string]any{"type": "knowledge"}, true},
		{"absent", map[string]any{"kind": "knowledge"}, false},
		{"empty", map[string]any{"type": ""}, false},
		{"whitespace", map[string]any{"type": "   "}, false},
		{"nil", map[string]any{"type": nil}, false},
		{"non-string", map[string]any{"type": []any{"a", "b"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasNonEmptyType(tc.fm); got != tc.want {
				t.Errorf("hasNonEmptyType(%v) = %v, want %v", tc.fm, got, tc.want)
			}
		})
	}
}

// TestCheck_RootIndexWithOKFVersion asserts the bundle-root index.md may
// carry `okf_version` frontmatter (isa-6zq ISC-9) while the nested
// knowledge/index.md (frontmatter-free) and the typed entry both pass —
// the whole bundle is conformant.
func TestCheck_RootIndexWithOKFVersion(t *testing.T) {
	rep, err := Check([]string{"testdata/rootindex"})
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if rep.Failed != 0 {
		t.Fatalf("expected 0 failures for a root index carrying okf_version, got %d: %+v",
			rep.Failed, rep.Violations)
	}
	// entries/index.md, knowledge/index.md, knowledge/alpha.md
	if rep.Checked != 3 {
		t.Errorf("expected 3 files checked, got %d", rep.Checked)
	}
}

// TestCheck_NestedIndexWithFrontmatterFails asserts that a NESTED index.md
// carrying frontmatter is still rejected (#3) even though the bundle-root
// index.md is allowed to carry okf_version. The typed entry still passes.
func TestCheck_NestedIndexWithFrontmatterFails(t *testing.T) {
	rep, err := Check([]string{"testdata/nested-index-fm"})
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if rep.Failed != 1 {
		t.Fatalf("expected exactly 1 failure (the nested index), got %d: %+v",
			rep.Failed, rep.Violations)
	}
	v := rep.Violations[0]
	if filepath.Base(filepath.Dir(v.File)) != "knowledge" {
		t.Errorf("expected the nested knowledge/index.md to fail, got %q", v.File)
	}
	if filepath.Base(v.File) != "index.md" {
		t.Errorf("expected the failing file to be an index.md, got %q", v.File)
	}
	if !strings.Contains(v.Reason, "OKF #3") || !strings.Contains(v.Reason, "nested") {
		t.Errorf("nested index.md should fail OKF #3 as a nested index, got reason: %q", v.Reason)
	}
}

// TestCheck_RootIndexDisallowedKey asserts the root-index exemption is narrow:
// frontmatter with a disallowed key (e.g. a stray `type`) is rejected even at
// the bundle root.
func TestCheck_RootIndexDisallowedKey(t *testing.T) {
	dir := t.TempDir()
	entries := filepath.Join(dir, "entries")
	if err := os.MkdirAll(entries, 0o755); err != nil {
		t.Fatal(err)
	}
	// Root index with a disallowed key alongside okf_version.
	rootIdx := "---\nokf_version: \"0.1\"\nstatus: open\n---\n\n# Entries\n"
	if err := os.WriteFile(filepath.Join(entries, "index.md"), []byte(rootIdx), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := Check([]string{dir})
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if rep.Failed != 1 {
		t.Fatalf("expected 1 failure for a disallowed root-index key, got %d: %+v",
			rep.Failed, rep.Violations)
	}
	if !strings.Contains(rep.Violations[0].Reason, "OKF #3") {
		t.Errorf("expected an OKF #3 reason for the disallowed key, got %q", rep.Violations[0].Reason)
	}
}

// TestCheck_MissingTypeStillFails is a regression guard: after adding the
// root-index exemption, a non-reserved entry missing `type` must STILL fail.
func TestCheck_MissingTypeStillFails(t *testing.T) {
	rep, err := Check([]string{"testdata/nonconformant"})
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	var sawMissingType bool
	for _, v := range rep.Violations {
		if filepath.Base(v.File) == "missing-type.md" && strings.Contains(v.Reason, "OKF #2") {
			sawMissingType = true
		}
	}
	if !sawMissingType {
		t.Errorf("expected missing-type.md to still fail OKF #2 after the root-index exemption; violations: %+v", rep.Violations)
	}
}

// TestReport_WriteJSON confirms the machine-readable schema is stable:
// {checked, passed, failed, violations:[{file, reason}]}.
func TestReport_WriteJSON(t *testing.T) {
	rep, err := Check([]string{"testdata/nonconformant"})
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	var buf bytes.Buffer
	if err := rep.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var decoded struct {
		Checked    int `json:"checked"`
		Passed     int `json:"passed"`
		Failed     int `json:"failed"`
		Violations []struct {
			File   string `json:"file"`
			Reason string `json:"reason"`
		} `json:"violations"`
	}
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("emitted JSON does not decode against the documented schema: %v", err)
	}
	if decoded.Failed != 2 || len(decoded.Violations) != 2 {
		t.Errorf("JSON report mismatch: failed=%d violations=%d", decoded.Failed, len(decoded.Violations))
	}
	for _, v := range decoded.Violations {
		if v.File == "" || v.Reason == "" {
			t.Errorf("violation entry missing file/reason: %+v", v)
		}
	}
}
