package main

import (
	"strings"
	"testing"
)

// TestBrainCmd_RegisteredOnRoot asserts the `brain` parent command is
// attached to rootCmd via init() — without this, every brain_*.go child
// init() that calls brainCmd.AddCommand(...) is unreachable from the CLI.
func TestBrainCmd_RegisteredOnRoot(t *testing.T) {
	found, _, err := rootCmd.Find([]string{"brain"})
	if err != nil {
		t.Fatalf("rootCmd.Find([brain]) error: %v", err)
	}
	if found == nil {
		t.Fatal("rootCmd.Find([brain]) returned nil; brain parent command not registered")
	}
	if found.Name() != "brain" {
		t.Fatalf("found.Name() = %q, want %q", found.Name(), "brain")
	}
}

// TestBrainCmd_HelpMentionsSubcommands verifies the parent's long help
// references the seam (BrainVerb / Decision #5) so a future agent reading
// `brain --help` learns where new verbs go.
func TestBrainCmd_HelpMentionsSubcommands(t *testing.T) {
	if brainCmd.Long == "" {
		t.Fatal("brainCmd.Long is empty; help text is the verb-discovery surface")
	}
	for _, needle := range []string{"new", "show", "list", "link", "related"} {
		if !strings.Contains(brainCmd.Long, needle) {
			t.Errorf("brainCmd.Long missing reference to verb %q", needle)
		}
	}
	if !strings.Contains(brainCmd.Long, "ISA.md") {
		t.Errorf("brainCmd.Long missing pointer to ISA.md; verb-discovery loses its anchor")
	}
}
