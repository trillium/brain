package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// anchoredPattern is the exact grep the version scripts must use. It is
// duplicated here (not read from the scripts) so the behavioral assertions
// below execute a real pipeline rather than asserting on script source.
const anchoredPattern = `^[[:space:]]*Version = `

// TestVersionScriptsAnchorExactVersionLine guards the de-PAI v0.5.0 fix:
// the version scripts must extract the canonical version with a grep
// anchored to the exact `Version` declaration. The bare pattern
// `grep 'Version = '` substring-matches the fork's `BrainVersion = "..."`
// line, producing a mangled two-line version string that fails every
// package check.
func TestVersionScriptsAnchorExactVersionLine(t *testing.T) {
	repoRoot := repoRoot(t)

	// Revert guard: every version-detection script must carry the anchored
	// pattern, and the broad pattern must appear nowhere in scripts/.
	for _, script := range []string{
		"scripts/check-versions.sh",
		"scripts/update-versions.sh",
		"scripts/check-docs-version.sh",
		"scripts/gen-winres.sh",
		"scripts/upgrade-smoke-test.sh",
	} {
		body, err := os.ReadFile(filepath.Join(repoRoot, script))
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		if strings.Contains(string(body), "grep 'Version = '") {
			t.Errorf("%s still uses the broad grep that also matches BrainVersion", script)
		}
		if !strings.Contains(string(body), "grep -E '"+anchoredPattern+"'") {
			t.Errorf("%s missing the anchored Version grep", script)
		}
	}
	assertNoBroadVersionGrep(t, repoRoot)

	// Behavioral proof on a fixture containing both declarations: the
	// anchored pipeline must yield exactly the upstream Version, while the
	// old broad pipeline yields two lines (proving the fixture actually
	// discriminates and the test would catch a revert).
	fixture := "package main\n\nvar (\n\tVersion = \"9.9.9-rt.1\"\n\tBrainVersion = \"1.2.3\"\n)\n"
	dir := t.TempDir()
	fixPath := filepath.Join(dir, "version.go")
	if err := os.WriteFile(fixPath, []byte(fixture), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if got := runExtraction(t, `grep 'Version = '`, fixPath); len(got) != 2 {
		t.Fatalf("fixture check: broad grep must match 2 lines, got %q", got)
	}
	got := runExtraction(t, "grep -E '"+anchoredPattern+"'", fixPath)
	if len(got) != 1 || got[0] != "9.9.9-rt.1" {
		t.Fatalf("anchored extraction must yield exactly [9.9.9-rt.1], got %q", got)
	}

	// Same anchored pipeline against the real version.go: exactly one line,
	// the upstream Version, never the fork BrainVersion.
	real := runExtraction(t, "grep -E '"+anchoredPattern+"'", filepath.Join(repoRoot, "cmd/bd/version.go"))
	if len(real) != 1 {
		t.Fatalf("anchored grep must match exactly one line in version.go, got %q", real)
	}
	if strings.Contains(real[0], "BrainVersion") {
		t.Fatalf("anchored grep matched the BrainVersion line: %q", real[0])
	}
}

// runExtraction executes `grepPattern file | sed 's/.*"(.*)".*/\1/'` — the
// extraction pipeline the version scripts use — and returns output lines.
func runExtraction(t *testing.T, grepPattern, file string) []string {
	t.Helper()
	cmd := exec.Command("sh", "-c", grepPattern+" "+shellQuote(file)+` | sed 's/.*"\(.*\)".*/\1/'`)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("extraction %s: %v", grepPattern, err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func assertNoBroadVersionGrep(t *testing.T, repoRoot string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repoRoot, "scripts"))
	if err != nil {
		t.Fatalf("read scripts dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sh") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(repoRoot, "scripts", e.Name()))
		if err != nil {
			t.Fatalf("read scripts/%s: %v", e.Name(), err)
		}
		if strings.Contains(string(body), "grep 'Version = '") {
			t.Errorf("scripts/%s still uses the broad grep that also matches BrainVersion", e.Name())
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(filepath.Dir(file))
}
