package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/doltserver"
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
		`export BRAIN_STORE_RECIPES="/data/recipes/.beads"`,
		`export BRAIN_STORE_IDEAS="/data/ideas/.beads"`,
		`export BRAIN_STORE_SIDE_QUESTS="/data/side-quests/.beads"`,
		`export BRAIN_STORES_LIST="ideas:recipes:side-quests"`, // sorted, colon-joined
		// Deprecated compat aliases (transition only).
		`export PAI_STORE_RECIPES="/data/recipes/.beads"`,
		`export PAI_STORE_IDEAS="/data/ideas/.beads"`,
		`export PAI_STORE_SIDE_QUESTS="/data/side-quests/.beads"`,
		`export PAI_STORES_LIST="ideas:recipes:side-quests"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("env file missing line %q. Got:\n%s", want, got)
		}
	}
}

func TestResolveCreateServer(t *testing.T) {
	// A store-host file in a private HOME. 'brain stores create' provisions
	// from this file: the wrapper it writes reads the file at run time, so a
	// process env value may only agree with it.
	writeStoreHostFile := func(t *testing.T, body string) string {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		path := configfile.DefaultStoreHostPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write store-host.env: %v", err)
		}
		return path
	}
	clearEnv := func(t *testing.T) {
		t.Setenv("BEADS_DOLT_SERVER_HOST", "")
		t.Setenv("BEADS_DOLT_SERVER_PORT", "")
	}
	alwaysAnswers := func(_ context.Context, host string, port int) (string, bool, error) {
		if host == "" || port == 0 {
			t.Errorf("probe called with empty target %q:%d", host, port)
		}
		return "brain_unified", true, nil
	}

	t.Run("missing shared config is a loud refusal naming the file", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		clearEnv(t)
		old := createStoreEmbedded
		createStoreEmbedded = false
		defer func() { createStoreEmbedded = old }()

		_, err := resolveCreateServer(context.Background(), alwaysAnswers)
		if err == nil {
			t.Fatal("expected an error when no shared store host config exists")
		}
		want := configfile.DefaultStoreHostPath()
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name the missing file %q; got: %v", want, err)
		}
		if !strings.Contains(err.Error(), "BEADS_DOLT_SERVER_HOST") {
			t.Errorf("error must name the key to write; got: %v", err)
		}
	})

	t.Run("a present-but-unusable shared file is refused", func(t *testing.T) {
		writeStoreHostFile(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\n")
		clearEnv(t)
		old := createStoreEmbedded
		createStoreEmbedded = false
		defer func() { createStoreEmbedded = old }()

		if _, err := resolveCreateServer(context.Background(), alwaysAnswers); err == nil {
			t.Fatal("expected a refusal for a file that does not define both keys")
		}
	})

	t.Run("the shared file supplies host and port", func(t *testing.T) {
		writeStoreHostFile(t, "# the federation's server\nBEADS_DOLT_SERVER_HOST=10.1.2.3\nBEADS_DOLT_SERVER_PORT=3307\n")
		clearEnv(t)
		old := createStoreEmbedded
		createStoreEmbedded = false
		defer func() { createStoreEmbedded = old }()

		got, err := resolveCreateServer(context.Background(), alwaysAnswers)
		if err != nil {
			t.Fatalf("resolveCreateServer: %v", err)
		}
		if got.Host != "10.1.2.3" || got.Port != 3307 || got.Source != createServerFile {
			t.Errorf("target = %+v, want 10.1.2.3:3307 from the file", got)
		}
	})

	t.Run("an env host differing from the file is refused, naming both", func(t *testing.T) {
		path := writeStoreHostFile(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\nBEADS_DOLT_SERVER_PORT=3307\n")
		t.Setenv("BEADS_DOLT_SERVER_HOST", "127.0.0.1")
		t.Setenv("BEADS_DOLT_SERVER_PORT", "3307")
		old := createStoreEmbedded
		createStoreEmbedded = false
		defer func() { createStoreEmbedded = old }()

		_, err := resolveCreateServer(context.Background(), alwaysAnswers)
		if err == nil {
			t.Fatal("expected a refusal when the env host differs from the shared file")
		}
		for _, want := range []string{"127.0.0.1", "10.1.2.3", path, configfile.StoreHostKeyHost} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error must name %q; got: %v", want, err)
			}
		}
	})

	t.Run("an env port differing from the file is refused, naming both", func(t *testing.T) {
		path := writeStoreHostFile(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\nBEADS_DOLT_SERVER_PORT=3307\n")
		t.Setenv("BEADS_DOLT_SERVER_HOST", "")
		t.Setenv("BEADS_DOLT_SERVER_PORT", "3399")
		old := createStoreEmbedded
		createStoreEmbedded = false
		defer func() { createStoreEmbedded = old }()

		_, err := resolveCreateServer(context.Background(), alwaysAnswers)
		if err == nil {
			t.Fatal("expected a refusal when the env port differs from the shared file")
		}
		for _, want := range []string{"3399", "3307", path, configfile.StoreHostKeyPort} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error must name %q; got: %v", want, err)
			}
		}
	})

	t.Run("env values matching the file are accepted", func(t *testing.T) {
		writeStoreHostFile(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\nBEADS_DOLT_SERVER_PORT=3307\n")
		t.Setenv("BEADS_DOLT_SERVER_HOST", "10.1.2.3")
		t.Setenv("BEADS_DOLT_SERVER_PORT", "3307")
		old := createStoreEmbedded
		createStoreEmbedded = false
		defer func() { createStoreEmbedded = old }()

		got, err := resolveCreateServer(context.Background(), alwaysAnswers)
		if err != nil {
			t.Fatalf("resolveCreateServer: %v", err)
		}
		if got.Host != "10.1.2.3" || got.Port != 3307 || got.Source != createServerFile {
			t.Errorf("target = %+v, want the file's 10.1.2.3:3307", got)
		}
	})

	t.Run("an env-only config without the shared file is refused", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		t.Setenv("BEADS_DOLT_SERVER_HOST", "127.0.0.1")
		t.Setenv("BEADS_DOLT_SERVER_PORT", "3399")
		old := createStoreEmbedded
		createStoreEmbedded = false
		defer func() { createStoreEmbedded = old }()

		_, err := resolveCreateServer(context.Background(), alwaysAnswers)
		if err == nil {
			t.Fatal("expected a refusal: the generated wrapper resolves its host from the shared file, which does not exist")
		}
		want := configfile.DefaultStoreHostPath()
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name the file %q; got: %v", want, err)
		}
		if !strings.Contains(err.Error(), "BEADS_DOLT_SERVER_HOST") || !strings.Contains(err.Error(), "BEADS_DOLT_SERVER_PORT") {
			t.Errorf("error must name both keys to write; got: %v", err)
		}
	})

	t.Run("a server that does not answer is refused, never baked", func(t *testing.T) {
		writeStoreHostFile(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\nBEADS_DOLT_SERVER_PORT=3307\n")
		clearEnv(t)
		old := createStoreEmbedded
		createStoreEmbedded = false
		defer func() { createStoreEmbedded = old }()

		dead := func(context.Context, string, int) (string, bool, error) {
			return "", false, errors.New("connection refused")
		}
		_, err := resolveCreateServer(context.Background(), dead)
		if err == nil {
			t.Fatal("expected a refusal when the configured server does not answer")
		}
		if !strings.Contains(err.Error(), "10.1.2.3:3307") {
			t.Errorf("error must name the host that did not answer; got: %v", err)
		}
	})

	t.Run("--embedded provisions locally without touching the shared server", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		clearEnv(t)
		old := createStoreEmbedded
		createStoreEmbedded = true
		defer func() { createStoreEmbedded = old }()

		probed := false
		probe := func(context.Context, string, int) (string, bool, error) {
			probed = true
			return "", false, errors.New("should not be probed")
		}
		got, err := resolveCreateServer(context.Background(), probe)
		if err != nil {
			t.Fatalf("resolveCreateServer with --embedded: %v", err)
		}
		if got.Source != createServerEmbedded {
			t.Errorf("source = %q, want %q", got.Source, createServerEmbedded)
		}
		if probed {
			t.Error("--embedded must not probe a shared server")
		}
	})

	t.Run("a half-written shared file is an error, not a default", func(t *testing.T) {
		writeStoreHostFile(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\n")
		clearEnv(t)
		old := createStoreEmbedded
		createStoreEmbedded = false
		defer func() { createStoreEmbedded = old }()

		if _, err := resolveCreateServer(context.Background(), alwaysAnswers); err == nil {
			t.Fatal("expected an error for a file that defines only the host")
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

func TestWriteServerStoreWrapper_ResolvesHostAtRunTime(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	wp, err := writeServerStoreWrapper("friction", "/data/friction/.beads", "", "127.0.0.1", 3307, "friction")
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
		"export BEADS_DOLT_SHARED_SERVER=1",
		`exec bd "$@"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("wrapper missing %q. Got:\n%s", want, got)
		}
	}
}

// TestServerStoreWrapper_RuntimeHostResolution runs the generated wrapper for
// real: with no shared config file it must refuse loudly, with the file it must
// hand the two variables to bd, and a process env value must win over the file.
// This is the executable contract behind 'no silent 127.0.0.1 default'.
func TestServerStoreWrapper_RuntimeHostResolution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("wrapper convention is POSIX-only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	wp, err := writeServerStoreWrapper("friction", filepath.Join(home, "data", "friction", ".beads"), "", "127.0.0.1", 3307, "brain_unified")
	if err != nil {
		t.Fatalf("writeServerStoreWrapper: %v", err)
	}
	// A stub bd that echoes the host/port it was handed.
	binDir := t.TempDir()
	bdStub := filepath.Join(binDir, "bd-stub")
	if err := os.WriteFile(bdStub, []byte("#!/bin/sh\necho \"host=$BEADS_DOLT_SERVER_HOST port=$BEADS_DOLT_SERVER_PORT mode=$BEADS_DOLT_SERVER_MODE\"\n"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("write bd stub: %v", err)
	}
	if _, err := writeServerStoreWrapper("friction", filepath.Join(home, "data", "friction", ".beads"), bdStub, "127.0.0.1", 3307, "brain_unified"); err != nil {
		t.Fatalf("rewrite wrapper with stub: %v", err)
	}

	run := func(env ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(wp, "list") //nolint:gosec // the wrapper we just wrote
		cmd.Env = append([]string{"HOME=" + home, "PATH=" + binDir + ":" + os.Getenv("PATH")}, env...)
		out, err := cmd.CombinedOutput()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("running wrapper: %v", err)
		}
		return string(out), code
	}

	out, code := run()
	if code == 0 {
		t.Fatalf("a missing shared store host config must refuse; wrapper said: %s", out)
	}
	if !strings.Contains(out, "store-host.env") {
		t.Errorf("refusal must name the missing file; got: %s", out)
	}

	hostFile := configfile.DefaultStoreHostPath()
	if err := os.MkdirAll(filepath.Dir(hostFile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(hostFile, []byte("BEADS_DOLT_SERVER_HOST=10.9.9.9\nBEADS_DOLT_SERVER_PORT=3311\n"), 0o600); err != nil {
		t.Fatalf("write shared config: %v", err)
	}

	out, code = run()
	if code != 0 {
		t.Fatalf("wrapper failed with a shared config present (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "host=10.9.9.9 port=3311 mode=1") {
		t.Errorf("wrapper must read host/port from the shared config; got: %s", out)
	}

	out, code = run("BEADS_DOLT_SERVER_HOST=127.0.0.9", "BEADS_DOLT_SERVER_PORT=3399")
	if code != 0 {
		t.Fatalf("wrapper failed with an env override (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "host=127.0.0.9 port=3399") {
		t.Errorf("a process env value must win over the shared config; got: %s", out)
	}

	// A canonical wrapper that already carries the runtime block must survive
	// the repoint pass: the block's own exports are the resolution, so they are
	// never stripped and the child still receives the file's host and port.
	repointed, err := rewriteWrapperInPlace(wp, true)
	if err != nil {
		t.Fatalf("rewriteWrapperInPlace: %v", err)
	}
	if repointed {
		t.Error("canonical wrapper reported as repointed; the runtime block was rewritten")
	}
	out, code = run()
	if code != 0 {
		t.Fatalf("wrapper failed after the repoint pass (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "host=10.9.9.9 port=3311 mode=1") {
		t.Errorf("the repoint pass must leave the runtime host block intact; got: %s", out)
	}
}

// A shared file whose port is not a number is unusable: the generated wrapper
// must refuse and name the key rather than hand a bad value to bd.
func TestServerStoreWrapper_RefusesUnusablePort(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("wrapper convention is POSIX-only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	wp, err := writeServerStoreWrapper("friction", filepath.Join(home, "data", "friction", ".beads"), "/bin/true", "127.0.0.1", 3307, "brain_unified")
	if err != nil {
		t.Fatalf("writeServerStoreWrapper: %v", err)
	}
	hostFile := configfile.DefaultStoreHostPath()
	if err := os.MkdirAll(filepath.Dir(hostFile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(hostFile, []byte("BEADS_DOLT_SERVER_HOST=10.9.9.9\nBEADS_DOLT_SERVER_PORT=thirty-three\n"), 0o600); err != nil {
		t.Fatalf("write shared config: %v", err)
	}

	cmd := exec.Command(wp) //nolint:gosec // the wrapper we just wrote
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("an unusable shared port must refuse; wrapper said: %s", out)
	}
	if !strings.Contains(string(out), "BEADS_DOLT_SERVER_PORT") {
		t.Errorf("refusal must name the unusable key; got: %s", out)
	}
}

func TestWriteServerStoreWrapper_UsesCustomBdBinary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	wp, err := writeServerStoreWrapper("chores", "/data/chores/.beads", "/opt/homebrew/bin/beads", "127.0.0.1", 3307, "chores")
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

func TestLoadStoresRegistry_LegacyFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	home, _ := os.UserHomeDir()
	legacyDir := filepath.Join(home, ".config", "pai")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	legacyBody := "stores:\n  task:\n    path: /data/tasks/.beads\n"
	if err := os.WriteFile(filepath.Join(legacyDir, "stores.yaml"), []byte(legacyBody), 0o644); err != nil {
		t.Fatalf("write legacy yaml: %v", err)
	}

	stores, err := loadStoresRegistry()
	if err != nil {
		t.Fatalf("loadStoresRegistry: %v", err)
	}
	if stores["task"].Path != "/data/tasks/.beads" {
		t.Errorf("legacy fallback not read; got %+v", stores)
	}
}

func TestSaveStoresRegistry_ConvergesLegacySymlink(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	stores := map[string]storeEntry{"task": {Path: "/data/tasks/.beads"}}
	if err := saveStoresRegistry(stores); err != nil {
		t.Fatalf("saveStoresRegistry: %v", err)
	}

	home, _ := os.UserHomeDir()
	canonical := filepath.Join(home, ".config", "brain", "stores.yaml")
	legacy := filepath.Join(home, ".config", "pai", "stores.yaml")
	if _, err := os.Stat(canonical); err != nil {
		t.Fatalf("canonical registry missing: %v", err)
	}
	fi, err := os.Lstat(legacy)
	if err != nil {
		t.Fatalf("legacy compat path missing: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("legacy path should be a symlink, mode %v", fi.Mode())
	}
	// Round-trip: load reads the canonical file back.
	back, err := loadStoresRegistry()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if back["task"].Path != "/data/tasks/.beads" {
		t.Errorf("round-trip mismatch; got %+v", back)
	}
}

func TestSaveStoresRegistry_ConvergesLegacyRegularFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	home, _ := os.UserHomeDir()
	legacyDir := filepath.Join(home, ".config", "pai")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	// Pre-migration machine: only a legacy regular file exists.
	legacyBody := "stores:\n  task:\n    path: /data/tasks/.beads\n"
	if err := os.WriteFile(filepath.Join(legacyDir, "stores.yaml"), []byte(legacyBody), 0o644); err != nil {
		t.Fatalf("write legacy yaml: %v", err)
	}

	// First post-migration save merges legacy content and must converge
	// the legacy path to a symlink — never leave it stale.
	stores, err := loadStoresRegistry()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	stores["recipes"] = storeEntry{Path: "/data/recipes/.beads"}
	if err := saveStoresRegistry(stores); err != nil {
		t.Fatalf("save: %v", err)
	}

	legacy := filepath.Join(legacyDir, "stores.yaml")
	fi, err := os.Lstat(legacy)
	if err != nil {
		t.Fatalf("legacy path missing after save: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("legacy path should be a symlink after first save, mode %v", fi.Mode())
	}
	back, err := loadStoresRegistry()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if back["task"].Path != "/data/tasks/.beads" || back["recipes"].Path != "/data/recipes/.beads" {
		t.Errorf("merged state lost; got %+v", back)
	}
}

func TestLoadStoresRegistry_MergesDivergedFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	home, _ := os.UserHomeDir()
	for dir, body := range map[string]string{
		// Legacy holds a store the canonical file never saw.
		filepath.Join(home, ".config", "pai"): "stores:\n  task:\n    path: /data/tasks/.beads\n  shared:\n    path: /legacy/shared/.beads\n",
		// Canonical wins on the conflicting key.
		filepath.Join(home, ".config", "brain"): "stores:\n  recipes:\n    path: /data/recipes/.beads\n  shared:\n    path: /canonical/shared/.beads\n",
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stores.yaml"), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", dir, err)
		}
	}

	stores, err := loadStoresRegistry()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if stores["task"].Path != "/data/tasks/.beads" {
		t.Errorf("legacy-only entry lost; got %+v", stores)
	}
	if stores["recipes"].Path != "/data/recipes/.beads" {
		t.Errorf("canonical-only entry lost; got %+v", stores)
	}
	if stores["shared"].Path != "/canonical/shared/.beads" {
		t.Errorf("canonical should win on conflict; got %+v", stores)
	}
}

func TestLoadStoresRegistry_CorruptLegacyHealthyCanonical(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	home, _ := os.UserHomeDir()
	for dir, body := range map[string]string{
		// Corrupt legacy must never wedge reads while canonical is healthy.
		filepath.Join(home, ".config", "pai"):   "not: [valid yaml: but parseable",
		filepath.Join(home, ".config", "brain"): "stores:\n  task:\n    path: /data/tasks/.beads\n",
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stores.yaml"), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", dir, err)
		}
	}

	stores, err := loadStoresRegistry()
	if err != nil {
		t.Fatalf("corrupt legacy must not fail healthy-canonical load: %v", err)
	}
	if stores["task"].Path != "/data/tasks/.beads" {
		t.Errorf("canonical content lost; got %+v", stores)
	}
}

func TestLoadStoresRegistry_CorruptLegacyMissingCanonical(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	home, _ := os.UserHomeDir()
	legacyDir := filepath.Join(home, ".config", "pai")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "stores.yaml"), []byte("not: [valid yaml: but parseable"), 0o644); err != nil {
		t.Fatalf("write legacy yaml: %v", err)
	}

	// Corrupt legacy with no canonical stays fatal: a later save must not
	// persist an empty registry over unknown state.
	if _, err := loadStoresRegistry(); err == nil {
		t.Fatal("expected error for corrupt legacy with missing canonical, got nil")
	}
}
