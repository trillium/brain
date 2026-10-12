package main

import (
	"os"
	"path/filepath"
	"testing"

	transferverb "github.com/steveyegge/beads/internal/brain/verb/transfer"
)

// TestStoresRegistry_ReadableByTransfer registers a store through the
// 'brain stores' save path and reads it back through the transfer registry
// reader, so the writer's {path, about} format and the reader cannot drift.
func TestStoresRegistry_ReadableByTransfer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	storeDir := filepath.Join(home, "roundtrip-store")
	beadsDir := filepath.Join(storeDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"dolt_database":"roundtrip_db","issue_prefix":"rt"}`), 0o644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	if err := saveStoresRegistry(map[string]storeEntry{
		"roundtrip": {Path: beadsDir, About: "round-trip fixture"},
	}); err != nil {
		t.Fatalf("saveStoresRegistry: %v", err)
	}

	r, err := transferverb.Load(home)
	if err != nil {
		t.Fatalf("transfer Load: %v", err)
	}
	db, _, err := r.ResolveDest("roundtrip")
	if err != nil {
		t.Fatalf("ResolveDest(roundtrip): %v", err)
	}
	if db != "roundtrip_db" {
		t.Errorf("ResolveDest(roundtrip) db = %q, want %q", db, "roundtrip_db")
	}
}
