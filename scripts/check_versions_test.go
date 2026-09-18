package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestVersionScriptsAnchorExactVersionLine guards the de-PAI v0.5.0 fix:
// scripts/check-versions.sh and scripts/update-versions.sh must extract the
// canonical version with a grep anchored to the exact `Version` declaration.
// The bare pattern `grep 'Version = '` substring-matches the fork's
// `BrainVersion = "..."` line, producing a mangled two-line version string
// that fails every package check.
func TestVersionScriptsAnchorExactVersionLine(t *testing.T) {
	repoRoot := repoRoot(t)

	anchored := []string{
		"scripts/check-versions.sh",
		"scripts/update-versions.sh",
		"scripts/check-docs-version.sh",
		"scripts/gen-winres.sh",
		"scripts/upgrade-smoke-test.sh",
	}
	for _, script := range anchored {
		body, err := os.ReadFile(filepath.Join(repoRoot, script))
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		if strings.Contains(string(body), "grep 'Version = '") {
			t.Errorf("%s still uses the broad grep that also matches BrainVersion", script)
		}
		if !strings.Contains(string(body), "grep -E '^[[:space:]]*Version = '") {
			t.Errorf("%s missing the anchored Version grep", script)
		}
	}

	// The anchored extraction against the real version.go must yield exactly
	// one line: the upstream Version, never the fork BrainVersion.
	cmd := exec.Command("grep", "-E", "^[[:space:]]*Version = ", "cmd/bd/version.go")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("anchored grep: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 1 {
		t.Fatalf("anchored grep must match exactly one line, got %d: %q", len(lines), out)
	}
	if strings.Contains(lines[0], "BrainVersion") {
		t.Fatalf("anchored grep matched the BrainVersion line: %q", lines[0])
	}
	if !strings.Contains(lines[0], "Version = \"") {
		t.Fatalf("anchored grep matched an unexpected line: %q", lines[0])
	}

	// No shell script may use the broad pattern: it substring-matches the
	// fork's BrainVersion line. check-beads-upstream.sh already used the
	// anchored form before this migration (repo precedent).
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
