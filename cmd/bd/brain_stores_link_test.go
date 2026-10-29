package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureAgentLink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	wrapper := filepath.Join(home, ".local", "bin", "day")

	if l, n := ensureAgentLink("day", wrapper); l != "" || n != "" {
		t.Fatalf("no link dir: got %q %q", l, n)
	}
	dir := filepath.Join(home, agentLinkDirPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "day")
	if l, n := ensureAgentLink("day", wrapper); l != link || n != "" {
		t.Fatalf("create: got %q %q", l, n)
	}
	if got, _ := os.Readlink(link); got != wrapper {
		t.Fatalf("target = %q", got)
	}
	if l, n := ensureAgentLink("day", wrapper); l != link || n != "" {
		t.Fatalf("idempotent: got %q %q", l, n)
	}
	other := filepath.Join(dir, "week")
	if err := os.WriteFile(other, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if l, n := ensureAgentLink("week", wrapper); l != "" || n == "" {
		t.Fatalf("conflict: got %q %q", l, n)
	}
	if b, _ := os.ReadFile(other); string(b) != "x" {
		t.Fatal("existing file overwritten")
	}
}
