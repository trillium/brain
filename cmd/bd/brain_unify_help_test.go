package main

import (
	"strings"
	"testing"
)

// The build warns about a store with no project id (plan.Unidentified feeds a
// warning only); its help text must not promise a refusal.
func TestUnifyBuildProjectIDHelpMatchesWarnOnlyBehaviour(t *testing.T) {
	f := unifyBuildCmd.Flags().Lookup("project-id")
	if f == nil {
		t.Fatal("unify build has no --project-id flag")
	}
	if strings.Contains(f.Usage, "refuse") {
		t.Fatalf("--project-id help claims a refusal the build does not make: %q", f.Usage)
	}
	if !strings.Contains(f.Usage, "warning") {
		t.Fatalf("--project-id help should say a missing id draws a warning: %q", f.Usage)
	}
}
