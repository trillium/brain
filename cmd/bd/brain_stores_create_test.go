package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteStoreWrapper_WritesExecutableWrapper(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	wp, err := writeStoreWrapper("recipes", "/data/recipes/.beads", "")
	if err != nil {
		t.Fatalf("writeStoreWrapper: %v", err)
	}

	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".local", "bin", "recipes")
	if wp != want {
		t.Fatalf("wrapper path = %q, want %q", wp, want)
	}

	info, err := os.Stat(wp)
	if err != nil {
		t.Fatalf("stat wrapper: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("wrapper not executable: mode %v", info.Mode())
	}

	body, err := os.ReadFile(wp)
	if err != nil {
		t.Fatalf("read wrapper: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"#!/bin/sh",
		`BEADS_DIR="/data/recipes/.beads"`,
		`BD_NAME="recipes"`,
		` bd "$@"`, // default bd binary
	} {
		if !strings.Contains(got, want) {
			t.Errorf("wrapper missing %q. Got:\n%s", want, got)
		}
	}
}

func TestWriteStoreWrapper_UsesCustomBdBinary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	wp, err := writeStoreWrapper("ideas", "/data/ideas/.beads", "/opt/homebrew/bin/bd")
	if err != nil {
		t.Fatalf("writeStoreWrapper: %v", err)
	}
	body, err := os.ReadFile(wp)
	if err != nil {
		t.Fatalf("read wrapper: %v", err)
	}
	if !strings.Contains(string(body), `/opt/homebrew/bin/bd "$@"`) {
		t.Errorf("wrapper missing custom binary exec. Got:\n%s", string(body))
	}
}

func TestRegenerateStoresEnv_WritesShellExports(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	stores := map[string]string{
		"recipes":     "/data/recipes/.beads",
		"ideas":       "/data/ideas/.beads",
		"side-quests": "/data/side-quests/.beads", // hyphen → underscore in env var
	}
	envPath, err := regenerateStoresEnv(stores)
	if err != nil {
		t.Fatalf("regenerateStoresEnv: %v", err)
	}

	body, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		`export PAI_STORE_RECIPES="/data/recipes/.beads"`,
		`export PAI_STORE_IDEAS="/data/ideas/.beads"`,
		`export PAI_STORE_SIDE_QUESTS="/data/side-quests/.beads"`,
		`export PAI_STORES_LIST="ideas:recipes:side-quests"`, // sorted, colon-joined
	} {
		if !strings.Contains(got, want) {
			t.Errorf("env file missing line %q. Got:\n%s", want, got)
		}
	}
}

func TestInitDoltStore_IdempotentWhenAlreadyInitialized(t *testing.T) {
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	// Pre-create a sentinel .dolt so initDoltStore treats it as already-initialized
	// and short-circuits before attempting to exec dolt.
	if err := os.MkdirAll(filepath.Join(beadsDir, ".dolt"), 0o755); err != nil {
		t.Fatalf("seed .dolt: %v", err)
	}

	if err := initDoltStore(beadsDir); err != nil {
		t.Fatalf("initDoltStore on already-initialized dir should be a no-op, got: %v", err)
	}
}

func TestInitDoltStore_ErrorsWhenDoltMissing(t *testing.T) {
	// Empty PATH guarantees `dolt` cannot be resolved.
	t.Setenv("PATH", "")

	beadsDir := filepath.Join(t.TempDir(), ".beads")
	err := initDoltStore(beadsDir)
	if err == nil {
		t.Fatal("expected error when dolt is unavailable, got nil")
	}
	if !strings.Contains(err.Error(), "dolt binary not found") {
		t.Errorf("error should name missing dolt; got: %v", err)
	}
}
