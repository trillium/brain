package main

import (
	"os"
	"path/filepath"
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

// A store name reaches generated shells (the wrapper pins BD_NAME), so names
// that shell would expand or that break its quoting are refused before any
// file is written.
func TestValidateStoreName_RejectsShellSignificantNames(t *testing.T) {
	for _, bad := range []string{"", "a b", "a/b", "a.b", `a"b`, "a`b", "a$(b)", "a;b", "a\nb", "a\\b"} {
		if err := validateStoreName("store name", bad); err == nil {
			t.Errorf("validateStoreName(%q) accepted a name that is not shell-safe", bad)
		}
	}
	for _, good := range []string{"inbox", "side-quests", "resume_bullets", "task2"} {
		if err := validateStoreName("store name", good); err != nil {
			t.Errorf("validateStoreName(%q) = %v, want nil", good, err)
		}
	}
}

func TestExpandPath_AbsolutePassthrough(t *testing.T) {
	abs := "/tmp/test/.beads"
	if got := expandPath(abs); got != abs {
		t.Errorf("expandPath(%q) = %q, want %q", abs, got, abs)
	}
}

// ---------------------------------------------------------------------------
// A registry carried to another machine describes stores by the home it was
// written in; the load never guesses or rewrites that (robots-1ypiy class).
// ---------------------------------------------------------------------------

func TestLoadStoresRegistry_KeepsForeignHomePathAsRecorded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// A coincidental $HOME copy exists, but the registry entry is identity, not
	// a folder-existence guess: the recorded path is kept exactly.
	real := filepath.Join(home, "data", "task", ".beads")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dir := filepath.Join(home, ".config", "brain")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	recorded := "/Users/someone-else/data/task/.beads"
	body := "stores:\n  task:\n    path: " + recorded + "\n"
	if err := os.WriteFile(filepath.Join(dir, "stores.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write registry: %v", err)
	}

	stores, err := loadStoresRegistry()
	if err != nil {
		t.Fatalf("loadStoresRegistry: %v", err)
	}
	if stores["task"].Path != recorded {
		t.Errorf("task path = %q, want the recorded %q", stores["task"].Path, recorded)
	}
}
