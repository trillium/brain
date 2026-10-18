package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrainStoresCmd_RegisteredOnBrain(t *testing.T) {
	found, _, err := rootCmd.Find([]string{"brain", "stores"})
	if err != nil {
		t.Fatalf("rootCmd.Find([brain stores]) error: %v", err)
	}
	if found == nil || found.Name() != "stores" {
		t.Fatal("brain stores command not registered")
	}
}

func TestBrainStoresSubcommands_Present(t *testing.T) {
	found, _, _ := rootCmd.Find([]string{"brain", "stores"})
	if found == nil {
		t.Fatal("brain stores not found")
	}
	want := []string{"add", "remove", "list", "env"}
	names := make(map[string]bool)
	for _, sub := range found.Commands() {
		names[sub.Name()] = true
	}
	for _, w := range want {
		if !names[w] {
			t.Errorf("brain stores missing subcommand %q", w)
		}
	}
}

func TestExpandPath_TildeExpansion(t *testing.T) {
	result := expandPath("~/data/knowledge/.beads")
	if result == "~/data/knowledge/.beads" {
		t.Error("expandPath did not expand ~ prefix")
	}
	if len(result) == 0 {
		t.Error("expandPath returned empty string")
	}
}

func TestExpandPath_AbsolutePassthrough(t *testing.T) {
	abs := "/tmp/test/.beads"
	if got := expandPath(abs); got != abs {
		t.Errorf("expandPath(%q) = %q, want %q", abs, got, abs)
	}
}

// ---------------------------------------------------------------------------
// Registry path healing (robots-1ypiy class: a registry carried to another
// machine describes every store by the home it was written in).
// ---------------------------------------------------------------------------

func TestResolveStorePath_HealsForeignHomePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	real := filepath.Join(home, "data", "task", ".beads")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	recorded := "/Users/someone-else/data/task/.beads"
	got, healed := resolveStorePath(recorded)
	if !healed {
		t.Fatalf("resolveStorePath(%q) did not heal to this machine's copy", recorded)
	}
	if got != real {
		t.Errorf("resolveStorePath(%q) = %q, want %q", recorded, got, real)
	}
}

func TestResolveStorePath_LeavesLiveRecordedPathAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	own := filepath.Join(home, "data", "task", ".beads")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got, healed := resolveStorePath(own)
	if healed {
		t.Errorf("a recorded path that exists must win; got healed %q", got)
	}
	if got != own {
		t.Errorf("resolveStorePath(%q) = %q, want the recorded path", own, got)
	}
}

func TestResolveStorePath_LeavesMissingStoreMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// No $HOME copy exists: the recorded path stays, so the doctor keeps
	// reporting the store as missing rather than inventing a path.
	recorded := "/Users/someone-else/data/ghost/.beads"
	got, healed := resolveStorePath(recorded)
	if healed {
		t.Fatalf("resolveStorePath(%q) healed to %q with nothing there", recorded, got)
	}
	if got != recorded {
		t.Errorf("resolveStorePath(%q) = %q, want it unchanged", recorded, got)
	}
}

func TestLoadStoresRegistry_HealsForeignHomePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	real := filepath.Join(home, "data", "task", ".beads")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dir := filepath.Join(home, ".config", "brain")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "stores:\n  task:\n    path: /Users/someone-else/data/task/.beads\n  ghost:\n    path: /Users/someone-else/data/ghost/.beads\n"
	if err := os.WriteFile(filepath.Join(dir, "stores.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write registry: %v", err)
	}

	stores, err := loadStoresRegistry()
	if err != nil {
		t.Fatalf("loadStoresRegistry: %v", err)
	}
	if stores["task"].Path != real {
		t.Errorf("task path = %q, want the healed %q", stores["task"].Path, real)
	}
	if want := "/Users/someone-else/data/ghost/.beads"; stores["ghost"].Path != want {
		t.Errorf("ghost path = %q, want it left as recorded (%q)", stores["ghost"].Path, want)
	}
}

func TestPersistHealedRegistryPaths_WritesTheHealedFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	real := filepath.Join(home, "data", "task", ".beads")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dir := filepath.Join(home, ".config", "brain")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "stores:\n  task:\n    path: /Users/someone-else/data/task/.beads\n"
	registry := filepath.Join(dir, "stores.yaml")
	if err := os.WriteFile(registry, []byte(body), 0o644); err != nil {
		t.Fatalf("write registry: %v", err)
	}

	healed, err := persistHealedRegistryPaths()
	if err != nil {
		t.Fatalf("persistHealedRegistryPaths: %v", err)
	}
	if len(healed) != 1 || healed[0] != "task" {
		t.Fatalf("healed = %v, want [task]", healed)
	}
	raw, err := os.ReadFile(registry)
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	if strings.Contains(string(raw), "/Users/someone-else/") {
		t.Errorf("registry still describes another home:\n%s", raw)
	}
	if !strings.Contains(string(raw), real) {
		t.Errorf("registry does not carry the healed path %q:\n%s", real, raw)
	}

	// Idempotent: a second run has nothing left to heal and rewrites nothing.
	again, err := persistHealedRegistryPaths()
	if err != nil {
		t.Fatalf("second persistHealedRegistryPaths: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second run healed %v, want none", again)
	}
}
