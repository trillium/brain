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

// TestGraphVerbsRegisteredOnRoot locks the documented surface:
// `brain related` and `brain recast` are root verbs. A registration that
// only calls brainCmd.AddCommand makes them `brain brain <verb>`, which
// the installed binary reports as an unknown command.
func TestGraphVerbsRegisteredOnRoot(t *testing.T) {
	for _, name := range []string{"related", "recast"} {
		found, _, err := rootCmd.Find([]string{name})
		if err != nil {
			t.Fatalf("rootCmd.Find([%s]) error: %v", name, err)
		}
		if found == nil || found.Name() != name {
			t.Fatalf("root command %q not registered", name)
		}
	}
}

// TestGraphVerbsStillUnderBrain keeps `bd brain related` / `bd brain recast`
// / `bd brain promote` working after the root registration.
func TestGraphVerbsStillUnderBrain(t *testing.T) {
	for _, name := range []string{"related", "recast", "promote"} {
		found, _, err := rootCmd.Find([]string{"brain", name})
		if err != nil {
			t.Fatalf("rootCmd.Find([brain %s]) error: %v", name, err)
		}
		if found == nil || found.Name() != name {
			t.Fatalf("brain %s not registered", name)
		}
	}
}
