package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/hooksdef"
)

func TestBrainServeGateRefusesDeclaredHooks(t *testing.T) {
	beadsDir := t.TempDir()
	ctx := context.Background()
	if err := brainServeGate(ctx, beadsDir, nil, false); err != nil {
		t.Fatalf("a store with no hooks and no database to probe must be served: %v", err)
	}

	dir := hooksdef.HooksDir(beadsDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	guard := "run = \"true\"\npolicy = \"guard\"\nwhen = \"close\"\ntimeout = \"3s\"\nrefusal_exit = 9\n"
	if err := os.WriteFile(filepath.Join(dir, "g.toml"), []byte(guard), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, wide := range []bool{false, true} {
		err := brainServeGate(ctx, beadsDir, nil, wide)
		if err == nil || !strings.Contains(err.Error(), "could not be guarded") {
			t.Errorf("wide=%v: a declared guard must refuse the server, got %v", wide, err)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "bad.toml"), []byte("run = = ="), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := brainServeGate(ctx, beadsDir, nil, true); err == nil || !strings.Contains(err.Error(), "unusable") {
		t.Errorf("unusable hook definitions must refuse the server, got %v", err)
	}
}
