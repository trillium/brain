package scripts_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// paiNeedles are the strings of the retired PAI system: its config
// directory, its home under ~/.claude, and its environment-variable prefix.
// The captain removed PAI from brain for good; these must not come back as
// compatibility shims, fallbacks, aliases, or doc references.
var paiNeedles = []string{"PAI_", ".config/pai", ".claude/PAI"}

// paiAllowed lists the tracked paths that may still name PAI. Keep it short:
// they are frozen historical records, not live behavior.
var paiAllowed = []string{
	"scripts/no_pai_test.go", // this guard
	"divergence/",            // append-only design trail, written while PAI existed
	"docs/brain/archive/",    // retired ISA, kept as history
}

// TestNoPAIReferences fails when a tracked file outside the allow-list
// mentions PAI_*, ~/.config/pai, or ~/.claude/PAI.
func TestNoPAIReferences(t *testing.T) {
	root := repoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Skipf("git ls-files unavailable: %v", err)
	}
	var offenders []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || allowedPAIPath(rel) {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || strings.IndexByte(string(body), 0) >= 0 {
			continue // deleted in the worktree, or binary
		}
		for i, line := range strings.Split(string(body), "\n") {
			for _, needle := range paiNeedles {
				if strings.Contains(line, needle) {
					offenders = append(offenders, fmt.Sprintf("%s:%d: %s", rel, i+1, strings.TrimSpace(line)))
				}
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("PAI was deleted from brain on purpose (captain decision, bead task-y7vvn):\n"+
			"no compatibility shims, legacy readers, PAI_* aliases, or ~/.config/pai / ~/.claude/PAI references.\n"+
			"Remove these, or (only for frozen history) extend paiAllowed in scripts/no_pai_test.go:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func allowedPAIPath(rel string) bool {
	for _, a := range paiAllowed {
		if rel == a || (strings.HasSuffix(a, "/") && strings.HasPrefix(rel, a)) {
			return true
		}
	}
	return false
}
