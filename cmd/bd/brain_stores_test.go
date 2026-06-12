package main

import (
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
