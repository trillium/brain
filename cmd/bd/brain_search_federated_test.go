package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/steveyegge/beads/internal/storage/issueops"
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

// ── Unified-walk federation (pre-cutover checklist item 5) ───────────

// TestCollectUnifiedSections_BucketsByPrefixOwner pins the re-pointed walk:
// one query over the connected unified database, rows bucketed per owning
// store from brain_store_prefixes, the primary's own rows skipped, unknown
// prefixes skipped silently.
func TestCollectUnifiedSections_BucketsByPrefixOwner(t *testing.T) {
	t.Setenv("BD_NAME", "brain")
	issueops.ResetUnifiedScopeCacheForTest()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("select database\\(\\)").
		WillReturnRows(sqlmock.NewRows([]string{"db"}).AddRow("brain_unified"))
	mock.ExpectQuery("select count\\(\\*\\) from information_schema").
		WithArgs("brain_unified").
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	mock.ExpectQuery("SELECT prefix, store FROM brain_store_prefixes").
		WillReturnRows(sqlmock.NewRows([]string{"prefix", "store"}).
			AddRow("brain", "brain").AddRow("task", "task"))
	mock.ExpectQuery("SELECT id, title, status, issue_type, priority FROM issues").
		WithArgs("%isolation%").
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "status", "issue_type", "priority"}).
			AddRow("task-ns1", "namespace isolation test bead", "open", "task", 2).
			AddRow("brain-777", "primary bead about isolation", "open", "knowledge", 2).
			AddRow("weird99", "unknown prefix", "open", "task", 1))

	sections := collectUnifiedSections(context.Background(), db, "isolation")
	if sections == nil {
		t.Fatal("collectUnifiedSections = nil, want sections")
	}
	if len(sections) != 1 || sections[0].Store != "task" {
		t.Fatalf("sections = %+v, want one task section", sections)
	}
	if len(sections[0].Issues) != 1 || sections[0].Issues[0].ID != "task-ns1" {
		t.Fatalf("task section = %+v", sections[0].Issues)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unified walk: %v", err)
	}
}

// TestCollectUnifiedSections_NilOnLegacy pins the fallback: a non-unified
// database answers nil so the registry walk applies.
func TestCollectUnifiedSections_NilOnLegacy(t *testing.T) {
	t.Setenv("BD_NAME", "brain")
	issueops.ResetUnifiedScopeCacheForTest()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("select database\\(\\)").
		WillReturnRows(sqlmock.NewRows([]string{"db"}).AddRow("knowledge"))
	mock.ExpectQuery("select count\\(\\*\\) from information_schema").
		WithArgs("knowledge").
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))

	if sections := collectUnifiedSections(context.Background(), db, "q"); sections != nil {
		t.Fatalf("legacy database produced unified sections: %+v", sections)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

// TestCollectUnifiedSections_EmptyIsRealAnswer pins that a successful unified
// walk with no secondary results is an empty answer (not a registry fallback)
// — after cutover the registry's old databases are stale or gone.
func TestCollectUnifiedSections_EmptyIsRealAnswer(t *testing.T) {
	t.Setenv("BD_NAME", "brain")
	issueops.ResetUnifiedScopeCacheForTest()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("select database\\(\\)").
		WillReturnRows(sqlmock.NewRows([]string{"db"}).AddRow("brain_unified"))
	mock.ExpectQuery("select count\\(\\*\\) from information_schema").
		WithArgs("brain_unified").
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	mock.ExpectQuery("SELECT prefix, store FROM brain_store_prefixes").
		WillReturnRows(sqlmock.NewRows([]string{"prefix", "store"}).AddRow("task", "task"))
	mock.ExpectQuery("SELECT id, title, status, issue_type, priority FROM issues").
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "status", "issue_type", "priority"}).
			AddRow("brain-1", "primary result", "open", "knowledge", 2))

	sections := collectUnifiedSections(context.Background(), db, "result")
	if sections == nil {
		t.Fatal("empty unified walk answered nil; want empty sections (real answer)")
	}
	if len(sections) != 0 {
		t.Fatalf("sections = %+v, want empty", sections)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unified walk: %v", err)
	}
}

// ── Markdown render root derivation (item 4) ─────────────────────────

func TestBrainKnowledgeRoot_NamespaceOverridesSharedDir(t *testing.T) {
	// The unified database's shared BEADS_DIR: the store name the render
	// sits under must come from the namespace (BD_NAME), not the .beads
	// parent directory's owner.
	t.Setenv("BEADS_DIR", "/home/u/data/brain/.beads")
	t.Setenv("BD_NAME", "task")
	if got := brainKnowledgeRoot(); got != filepath.Join("/home/u/data/brain", "task") {
		t.Fatalf("brainKnowledgeRoot = %q", got)
	}
}

func TestBrainKnowledgeRoot_LegacyStoreUnchanged(t *testing.T) {
	// A wrapper whose .beads parent already matches its namespace renders
	// exactly as before.
	t.Setenv("BEADS_DIR", "/home/u/data/brain/.beads")
	t.Setenv("BD_NAME", "brain")
	if got := brainKnowledgeRoot(); got != "/home/u/data/brain" {
		t.Fatalf("brainKnowledgeRoot = %q", got)
	}
}

func TestBrainKnowledgeRoot_ExplicitRootWins(t *testing.T) {
	t.Setenv("BRAIN_KNOWLEDGE_ROOT", "/custom/root")
	t.Setenv("BEADS_DIR", "/home/u/data/brain/.beads")
	t.Setenv("BD_NAME", "task")
	if got := brainKnowledgeRoot(); got != "/custom/root" {
		t.Fatalf("brainKnowledgeRoot = %q", got)
	}
}

func TestBrainKnowledgeRoot_NoNamespaceKeepsLegacy(t *testing.T) {
	t.Setenv("BEADS_DIR", "/home/u/data/tasks/.beads")
	t.Setenv("BD_NAME", "")
	if got := brainKnowledgeRoot(); got != "/home/u/data/tasks" {
		t.Fatalf("brainKnowledgeRoot = %q", got)
	}
}
