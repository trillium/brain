package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeIdentityFile lays an agent identity.md under a stubbed $HOME.
func writeIdentityFile(t *testing.T, home, id, body string) {
	t.Helper()
	dir := filepath.Join(home, ".parlay", "agents", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "identity.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write identity.md: %v", err)
	}
}

// TestAgentIdentityActor covers the fleet agent identity lookup that feeds the
// actor/created_by stamp (robots-0x8).
func TestAgentIdentityActor(t *testing.T) {
	const identityDoc = `---
id: volt
name: "Volt"
color: "#EA580C"
model: opus
---
# Identity — volt

You are **volt**.
`

	tests := []struct {
		name      string
		agentName string
		agentID   string
		identity  string // identity.md body, "" for no file
		want      string
	}{
		{
			name:      "display name from the environment wins",
			agentName: "Volt",
			agentID:   "volt",
			identity:  identityDoc,
			want:      "Volt",
		},
		{
			name:      "display name is trimmed",
			agentName: "  Volt  ",
			agentID:   "volt",
			want:      "Volt",
		},
		{
			name:     "name resolved from identity.md when only the id is set",
			agentID:  "volt",
			identity: identityDoc,
			want:     "Volt",
		},
		{
			name:     "unquoted identity.md name is accepted",
			agentID:  "volt",
			identity: "---\nid: volt\nname: Volt\n---\n",
			want:     "Volt",
		},
		{
			name:     "identity.md without a name falls back to the id",
			agentID:  "volt",
			identity: "---\nid: volt\ncolor: \"#EA580C\"\n---\n",
			want:     "volt",
		},
		{
			name:     "process with no identity.md falls back to the id",
			agentID:  "volt",
			identity: "",
			want:     "volt",
		},
		{
			name:      "no fleet agent identity at all",
			agentName: "",
			agentID:   "",
			want:      "",
		},
		{
			name:     "an id parlay would not mint is not an identity",
			agentID:  "../../etc/passwd",
			identity: "",
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv(agentNameEnvVar, tt.agentName)
			t.Setenv(agentIDEnvVar, tt.agentID)
			if tt.identity != "" {
				writeIdentityFile(t, home, tt.agentID, tt.identity)
			}

			if got := agentIdentityActor(); got != tt.want {
				t.Errorf("agentIdentityActor() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAgentIdentityFileNameIgnoresTraversal guards the identity.md path against
// an id that would name a file outside the agents directory.
func TestAgentIdentityFileNameIgnoresTraversal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// The file a `../` id resolves to, readable so the test proves the id is
	// refused rather than merely missing.
	if err := os.MkdirAll(filepath.Join(home, ".parlay"), 0o755); err != nil {
		t.Fatalf("mkdir .parlay: %v", err)
	}
	secret := filepath.Join(home, ".parlay", "secret.md")
	if err := os.WriteFile(secret, []byte("---\nname: Stolen\n---\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", secret, err)
	}
	if got := frontmatterName(string(mustReadFile(t, secret))); got != "Stolen" {
		t.Fatalf("fixture name = %q, want %q", got, "Stolen")
	}

	for _, id := range []string{"../secret", "..", ".", "volt/../../secret", "volt junior"} {
		if got := agentIdentityFileName(id); got != "" {
			t.Errorf("agentIdentityFileName(%q) = %q, want it ignored", id, got)
		}
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// TestFrontmatterName covers the identity.md frontmatter reader.
func TestFrontmatterName(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "quoted name",
			doc:  "---\nid: grandma-2\nname: \"Grandma Danny II\"\n---\n# Identity\n",
			want: "Grandma Danny II",
		},
		{
			name: "single-quoted name",
			doc:  "---\nname: 'Fable'\n---\n",
			want: "Fable",
		},
		{
			name: "unquoted name",
			doc:  "---\nid: danny\nname: Danny\ncolor: \"#EA580C\"\n---\n",
			want: "Danny",
		},
		{
			name: "name after the frontmatter is not the identity",
			doc:  "---\nid: danny\n---\nname: Not This One\n",
			want: "",
		},
		{
			name: "no frontmatter",
			doc:  "# Identity — danny\n\nname: Not This One\n",
			want: "",
		},
		{
			name: "nested name is not the identity",
			doc:  "---\nid: danny\nmeta:\n  name: nested\n---\n",
			want: "",
		},
		{
			name: "frontmatter without a name",
			doc:  "---\nid: danny\ncolor: \"#EA580C\"\n---\n",
			want: "",
		},
		{
			name: "empty document",
			doc:  "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := frontmatterName(tt.doc); got != tt.want {
				t.Errorf("frontmatterName(%q) = %q, want %q", tt.doc, got, tt.want)
			}
		})
	}
}
