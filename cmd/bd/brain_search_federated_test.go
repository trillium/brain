package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSearchFederatedFlag_Registered verifies that --federated is wired onto
// the existing root search command (not a separate subcommand). This is
// ISC-41 — federation is an opt-in flag on `bd search`.
func TestSearchFederatedFlag_Registered(t *testing.T) {
	flag := searchCmd.Flags().Lookup("federated")
	if flag == nil {
		t.Fatal("--federated flag not registered on searchCmd")
	}
	if flag.DefValue != "false" {
		t.Errorf("--federated default = %q, want %q", flag.DefValue, "false")
	}
}

func TestIsSafeIdentifier(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"lowercase", "task", true},
		{"uppercase", "TASK", true},
		{"mixed", "Task", true},
		{"digits", "task1", true},
		{"underscore", "my_task", true},
		{"leading_digit", "1task", true}, // permitted; Dolt allows it
		{"only_underscore", "_", true},
		{"hyphen_rejected", "my-task", false},
		{"dot_rejected", "my.task", false},
		{"space_rejected", "my task", false},
		{"semicolon_rejected", "task;", false},
		{"quote_rejected", "task'", false},
		{"backtick_rejected", "task`", false},
		{"backslash_rejected", `task\`, false},
		{"slash_rejected", "task/db", false},
		{"unicode_rejected", "tâsk", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isSafeIdentifier(c.in); got != c.want {
				t.Errorf("isSafeIdentifier(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestResolveDoltDatabase_PrefersDoltDatabaseField(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Both fields present — dolt_database wins.
	content := `{"database": "fallback", "dolt_database": "task"}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}

	got, err := resolveDoltDatabase(beadsDir)
	if err != nil {
		t.Fatalf("resolveDoltDatabase: %v", err)
	}
	if got != "task" {
		t.Errorf("resolveDoltDatabase = %q, want %q", got, "task")
	}
}

func TestResolveDoltDatabase_FallsBackToDatabase(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Only `database` present — used as fallback when dolt_database is empty.
	content := `{"database": "legacy_db"}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}

	got, err := resolveDoltDatabase(beadsDir)
	if err != nil {
		t.Fatalf("resolveDoltDatabase: %v", err)
	}
	if got != "legacy_db" {
		t.Errorf("resolveDoltDatabase fallback = %q, want %q", got, "legacy_db")
	}
}

func TestResolveDoltDatabase_MissingFile(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// No metadata.json — caller is expected to skip the store silently
	// (ISC-43). Verify we return an error rather than panicking or returning
	// a misleading empty string with no error.
	_, err := resolveDoltDatabase(beadsDir)
	if err == nil {
		t.Fatal("expected error for missing metadata.json, got nil")
	}
}

func TestResolveDoltDatabase_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
	if _, err := resolveDoltDatabase(beadsDir); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestResolveDoltDatabase_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Empty file → invalid JSON → caller skips. We must NOT treat this as
	// a valid empty-string database, which would lead to a SQL injection
	// attempt or worse a query against the connection's default db.
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(""), 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
	if _, err := resolveDoltDatabase(beadsDir); err == nil {
		t.Fatal("expected error for empty metadata.json, got nil")
	}
}
