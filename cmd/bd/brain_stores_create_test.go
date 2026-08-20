package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/doltserver"
	"github.com/steveyegge/beads/internal/storage/dolt"
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

	stores := map[string]storeEntry{
		"recipes":     {Path: "/data/recipes/.beads"},
		"ideas":       {Path: "/data/ideas/.beads"},
		"side-quests": {Path: "/data/side-quests/.beads"}, // hyphen → underscore in env var
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

func TestResolveSharedServerCreate(t *testing.T) {
	t.Run("off when no server env", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_MODE", "")
		t.Setenv("BEADS_DOLT_SHARED_SERVER", "")
		t.Setenv("BEADS_DOLT_SERVER_HOST", "")
		t.Setenv("BEADS_DOLT_SERVER_PORT", "")
		if _, _, shared := resolveSharedServerCreate(); shared {
			t.Fatal("expected shared=false when no server env is set")
		}
	})

	t.Run("on via SERVER_MODE with explicit host/port", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_MODE", "1")
		t.Setenv("BEADS_DOLT_SHARED_SERVER", "")
		t.Setenv("BEADS_DOLT_SERVER_HOST", "10.0.0.5")
		t.Setenv("BEADS_DOLT_SERVER_PORT", "3399")
		host, port, shared := resolveSharedServerCreate()
		if !shared {
			t.Fatal("expected shared=true when BEADS_DOLT_SERVER_MODE=1")
		}
		if host != "10.0.0.5" {
			t.Errorf("host = %q, want 10.0.0.5", host)
		}
		if port != 3399 {
			t.Errorf("port = %d, want 3399", port)
		}
	})

	t.Run("on via SHARED_SERVER with default host/port", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_MODE", "")
		t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")
		t.Setenv("BEADS_DOLT_SERVER_HOST", "")
		t.Setenv("BEADS_DOLT_SERVER_PORT", "")
		host, port, shared := resolveSharedServerCreate()
		if !shared {
			t.Fatal("expected shared=true when BEADS_DOLT_SHARED_SERVER=1")
		}
		if host != "127.0.0.1" {
			t.Errorf("host = %q, want 127.0.0.1 default", host)
		}
		if port != dolt.DefaultSQLPort {
			t.Errorf("port = %d, want default %d", port, dolt.DefaultSQLPort)
		}
	})

	t.Run("bad port falls back to default", func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_MODE", "1")
		t.Setenv("BEADS_DOLT_SERVER_HOST", "")
		t.Setenv("BEADS_DOLT_SERVER_PORT", "not-a-number")
		_, port, shared := resolveSharedServerCreate()
		if !shared {
			t.Fatal("expected shared=true")
		}
		if port != dolt.DefaultSQLPort {
			t.Errorf("port = %d, want default %d on unparseable port", port, dolt.DefaultSQLPort)
		}
	})
}

func TestBuildServerStoreConfig(t *testing.T) {
	cfg := buildServerStoreConfig("friction", "127.0.0.1", 3307, "abc-123")
	if !cfg.IsDoltServerMode() {
		t.Errorf("config should be server mode; dolt_mode = %q", cfg.DoltMode)
	}
	if cfg.GetDoltDatabase() != "friction" {
		t.Errorf("dolt_database = %q, want friction", cfg.GetDoltDatabase())
	}
	if cfg.DoltServerHost != "127.0.0.1" || cfg.DoltServerPort != 3307 {
		t.Errorf("server host/port = %s:%d, want 127.0.0.1:3307", cfg.DoltServerHost, cfg.DoltServerPort)
	}
	if cfg.ProjectID != "abc-123" {
		t.Errorf("project_id = %q, want abc-123 (must not be regenerated)", cfg.ProjectID)
	}
	if cfg.Database != "dolt" || cfg.Backend != configfile.BackendDolt {
		t.Errorf("database/backend = %q/%q, want dolt/%s", cfg.Database, cfg.Backend, configfile.BackendDolt)
	}
	if cfg.GlobalDoltDatabase != doltserver.GlobalDatabaseName || cfg.GlobalProjectID != doltserver.GlobalProjectID {
		t.Errorf("global fields = %q/%q, want %q/%q",
			cfg.GlobalDoltDatabase, cfg.GlobalProjectID,
			doltserver.GlobalDatabaseName, doltserver.GlobalProjectID)
	}
}

func TestBuildServerStoreConfig_RoundTripsThroughMetadataJSON(t *testing.T) {
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := buildServerStoreConfig("chores", "127.0.0.1", 3307, "id-xyz")
	if err := cfg.Save(beadsDir); err != nil {
		t.Fatalf("save metadata.json: %v", err)
	}
	loaded, err := configfile.Load(beadsDir)
	if err != nil || loaded == nil {
		t.Fatalf("reload metadata.json: %v", err)
	}
	if !loaded.IsDoltServerMode() {
		t.Errorf("reloaded config not server mode: %q", loaded.DoltMode)
	}
	if loaded.GetDoltDatabase() != "chores" {
		t.Errorf("reloaded dolt_database = %q, want chores", loaded.GetDoltDatabase())
	}
	if loaded.ProjectID != "id-xyz" {
		t.Errorf("reloaded project_id = %q, want id-xyz", loaded.ProjectID)
	}
}

func TestWriteServerStoreWrapper_EmitsServerPins(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	wp, err := writeServerStoreWrapper("friction", "/data/friction/.beads", "", "127.0.0.1", 3307)
	if err != nil {
		t.Fatalf("writeServerStoreWrapper: %v", err)
	}

	home, _ := os.UserHomeDir()
	if want := filepath.Join(home, ".local", "bin", "friction"); wp != want {
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
		`export BEADS_DIR="/data/friction/.beads"`,
		`export BD_NAME="friction"`,
		`export BRAIN_KNOWLEDGE_ROOT="/data/friction"`,
		"export BRAIN_EXFIL_FLAT=1",
		"export BEADS_DOLT_SERVER_MODE=1",
		"export BEADS_DOLT_SERVER_HOST=127.0.0.1",
		"export BEADS_DOLT_SERVER_PORT=3307",
		"export BEADS_DOLT_SHARED_SERVER=1",
		`exec bd "$@"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("wrapper missing %q. Got:\n%s", want, got)
		}
	}
}

func TestWriteServerStoreWrapper_UsesCustomBdBinary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	wp, err := writeServerStoreWrapper("chores", "/data/chores/.beads", "/opt/homebrew/bin/beads", "127.0.0.1", 3307)
	if err != nil {
		t.Fatalf("writeServerStoreWrapper: %v", err)
	}
	body, err := os.ReadFile(wp)
	if err != nil {
		t.Fatalf("read wrapper: %v", err)
	}
	if !strings.Contains(string(body), `exec /opt/homebrew/bin/beads "$@"`) {
		t.Errorf("wrapper missing custom binary exec. Got:\n%s", string(body))
	}
}

func TestWriteStoreConfigYaml(t *testing.T) {
	beadsDir := t.TempDir()

	if err := writeStoreConfigYaml(beadsDir, "friction"); err != nil {
		t.Fatalf("writeStoreConfigYaml: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(beadsDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		`issue-prefix: "friction"`,
		`BD_NAME: "friction"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config.yaml missing %q. Got:\n%s", want, got)
		}
	}

	// Idempotent: a second call must not clobber an edited file.
	edited := "issue-prefix: \"friction\"\nBD_NAME: \"friction\"\n# hand edit\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(edited), 0o600); err != nil {
		t.Fatalf("seed edited config: %v", err)
	}
	if err := writeStoreConfigYaml(beadsDir, "friction"); err != nil {
		t.Fatalf("second writeStoreConfigYaml: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(beadsDir, "config.yaml"))
	if err != nil {
		t.Fatalf("re-read config.yaml: %v", err)
	}
	if string(after) != edited {
		t.Errorf("writeStoreConfigYaml clobbered existing file.\nwant:\n%s\ngot:\n%s", edited, string(after))
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
