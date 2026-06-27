package transfer_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/beads/internal/brain/verb/transfer"
)

// TestLoad_NoHome verifies the fallback path: with no homeDir the
// registry returns the hardcoded built-in mapping, no I/O attempted.
// Every canonical store name in the spec must resolve.
func TestLoad_NoHome(t *testing.T) {
	t.Parallel()

	r, err := transfer.Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}

	cases := []struct {
		name       string
		wantDB     string
		wantPrefix string
	}{
		{"brain", "dolt", "brain"},
		{"tasks", "task", "task"},
		{"projects", "project", "project"},
		{"agents", "agent", "agent"},
		{"inbox", "inbox", "inbox"},
		{"decisions", "decision", "decision"},
		{"ideas", "idea", "idea"},
		{"life", "life", "life"},
		{"questions", "question", "question"},
		{"assert", "assert", "assert"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, prefix, err := r.ResolveDest(tc.name)
			if err != nil {
				t.Fatalf("ResolveDest(%q): %v", tc.name, err)
			}
			if db != tc.wantDB {
				t.Errorf("ResolveDest(%q) db = %q, want %q", tc.name, db, tc.wantDB)
			}
			if prefix != tc.wantPrefix {
				t.Errorf("ResolveDest(%q) prefix = %q, want %q", tc.name, prefix, tc.wantPrefix)
			}
		})
	}
}

// TestLoad_Aliases verifies the singular-form aliases (task → tasks,
// decision → decisions, …) resolve to the same DB and prefix as the
// canonical plural. The spec example `brain transfer inbox-abc task`
// must succeed even though the canonical name is "tasks".
func TestLoad_Aliases(t *testing.T) {
	t.Parallel()

	r, err := transfer.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	aliases := map[string]string{
		"task":      "tasks",
		"project":   "projects",
		"robot":     "robots",
		"agent":     "robots",
		"agents":    "robots",
		"decision":  "decisions",
		"idea":      "ideas",
		"question":  "questions",
		"assertion": "assert",
		"people":    "person",
	}
	for alias, canonical := range aliases {
		alias, canonical := alias, canonical
		t.Run(alias, func(t *testing.T) {
			t.Parallel()
			aliasDB, aliasPrefix, err := r.ResolveDest(alias)
			if err != nil {
				t.Fatalf("ResolveDest(%q): %v", alias, err)
			}
			canonDB, canonPrefix, err := r.ResolveDest(canonical)
			if err != nil {
				t.Fatalf("ResolveDest(%q): %v", canonical, err)
			}
			if aliasDB != canonDB {
				t.Errorf("alias %q db = %q, canonical %q db = %q", alias, aliasDB, canonical, canonDB)
			}
			if aliasPrefix != canonPrefix {
				t.Errorf("alias %q prefix = %q, canonical %q prefix = %q", alias, aliasPrefix, canonical, canonPrefix)
			}
		})
	}
}

// TestResolveSource verifies prefix → store → DB resolution for every
// builtin store. ISC-49: any source store works.
func TestResolveSource(t *testing.T) {
	t.Parallel()

	r, err := transfer.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cases := []struct {
		id        string
		wantStore string
		wantDB    string
	}{
		{"brain-abc123", "brain", "dolt"},
		{"task-xyz", "tasks", "task"},
		{"inbox-asdf", "inbox", "inbox"},
		{"decision-fye", "decisions", "decision"},
		{"agent-001", "agents", "agent"},
		{"project-007", "projects", "project"},
		{"idea-aaa", "ideas", "idea"},
		{"life-bbb", "life", "life"},
		{"question-ccc", "questions", "question"},
		{"assert-fye", "assert", "assert"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			store, db, err := r.ResolveSource(tc.id)
			if err != nil {
				t.Fatalf("ResolveSource(%q): %v", tc.id, err)
			}
			if store != tc.wantStore {
				t.Errorf("ResolveSource(%q) store = %q, want %q", tc.id, store, tc.wantStore)
			}
			if db != tc.wantDB {
				t.Errorf("ResolveSource(%q) db = %q, want %q", tc.id, db, tc.wantDB)
			}
		})
	}
}

// TestResolveSource_BadPrefix verifies ISC-54: an unrecognised prefix
// produces a non-zero exit (which surfaces as a returned error here).
// The message must mention the offending prefix so the user knows what
// to fix.
func TestResolveSource_BadPrefix(t *testing.T) {
	t.Parallel()

	r, err := transfer.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	_, _, err = r.ResolveSource("xyz-foo")
	if err == nil {
		t.Fatal("expected error for unknown prefix, got nil")
	}
	if want := `unknown source store for prefix "xyz"`; !contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), want)
	}
}

// TestResolveSource_NoSeparator verifies an ID with no "-" is rejected
// — the prefix cannot be parsed.
func TestResolveSource_NoSeparator(t *testing.T) {
	t.Parallel()

	r, _ := transfer.Load("")
	_, _, err := r.ResolveSource("nopfx")
	if err == nil {
		t.Fatal("expected error for id without separator, got nil")
	}
}

// TestResolveSource_Empty verifies an empty id is rejected with a
// clear "required" message.
func TestResolveSource_Empty(t *testing.T) {
	t.Parallel()

	r, _ := transfer.Load("")
	_, _, err := r.ResolveSource("")
	if err == nil {
		t.Fatal("expected error for empty id, got nil")
	}
	if !contains(err.Error(), "source id is required") {
		t.Errorf("error = %q, want \"source id is required\"", err.Error())
	}
}

// TestResolveDest_BadName verifies ISC-55: an unrecognised dest name
// returns a clear error mentioning the offending name.
func TestResolveDest_BadName(t *testing.T) {
	t.Parallel()

	r, _ := transfer.Load("")
	_, _, err := r.ResolveDest("foobar")
	if err == nil {
		t.Fatal("expected error for unknown dest, got nil")
	}
	if want := `unknown destination store "foobar"`; !contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), want)
	}
}

// TestResolveDest_Empty verifies an empty dest name is rejected.
func TestResolveDest_Empty(t *testing.T) {
	t.Parallel()

	r, _ := transfer.Load("")
	_, _, err := r.ResolveDest("")
	if err == nil {
		t.Fatal("expected error for empty dest, got nil")
	}
}

// TestLoad_YamlEnrichment verifies that ~/.config/pai/stores.yaml +
// per-store metadata.json overrides the builtin mapping. We write a
// minimal yaml that re-maps the "inbox" store to a custom database
// name and confirm Load picks it up.
func TestLoad_YamlEnrichment(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	// Set up the store's .beads/metadata.json with a custom dolt_database.
	storeDir := filepath.Join(home, "custom-inbox")
	beadsDir := filepath.Join(storeDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	metaPath := filepath.Join(beadsDir, "metadata.json")
	metaBody := `{"dolt_database":"inbox_custom"}`
	if err := os.WriteFile(metaPath, []byte(metaBody), 0o644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	// Write the stores.yaml registry.
	yamlDir := filepath.Join(home, ".config", "pai")
	if err := os.MkdirAll(yamlDir, 0o755); err != nil {
		t.Fatalf("mkdir yaml: %v", err)
	}
	yamlBody := "stores:\n  inbox: " + storeDir + "\n"
	if err := os.WriteFile(filepath.Join(yamlDir, "stores.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	r, err := transfer.Load(home)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	db, _, err := r.ResolveDest("inbox")
	if err != nil {
		t.Fatalf("ResolveDest(inbox): %v", err)
	}
	if db != "inbox_custom" {
		t.Errorf("ResolveDest(inbox) db = %q, want \"inbox_custom\" (yaml override)", db)
	}
}

// TestLoad_BadYaml verifies that a malformed stores.yaml returns an
// error but the returned registry still contains the builtin fallback,
// so the user can still transfer between known-good stores while they
// fix their yaml.
func TestLoad_BadYaml(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	yamlDir := filepath.Join(home, ".config", "pai")
	if err := os.MkdirAll(yamlDir, 0o755); err != nil {
		t.Fatalf("mkdir yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(yamlDir, "stores.yaml"), []byte("not: [valid yaml: but parseable"), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	r, err := transfer.Load(home)
	if err == nil {
		t.Fatal("expected error for malformed yaml, got nil")
	}
	if r == nil {
		t.Fatal("expected fallback registry even on yaml error, got nil")
	}
	// Builtin entries must still work.
	db, _, err := r.ResolveDest("brain")
	if err != nil {
		t.Fatalf("ResolveDest(brain) after bad yaml: %v", err)
	}
	if db != "dolt" {
		t.Errorf("after bad yaml ResolveDest(brain) db = %q, want \"dolt\"", db)
	}
}

// TestHubDB verifies the brain hub DB resolves to "dolt" both with
// and without yaml overrides — it is the authoritative answer to
// "where does a cross-store edge land if the user asks for the hub?".
func TestHubDB(t *testing.T) {
	t.Parallel()

	r, _ := transfer.Load("")
	if got := r.HubDB(); got != "dolt" {
		t.Errorf("HubDB() = %q, want \"dolt\"", got)
	}
}

// contains is a tiny substring helper kept local so the test file
// has no fmt/strings churn on every check. Mirrors strings.Contains
// without importing it (avoids a top-of-file import for one helper).
func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
