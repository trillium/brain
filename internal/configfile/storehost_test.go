package configfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeStoreHost points HOME at a fresh temp dir, then writes the machine's
// shared store-host file there with the given body (nothing when body is
// empty), and returns the file's path.
func writeStoreHost(t *testing.T, body string) string {
	t.Helper()
	return writeStoreHostAt(t, t.TempDir(), body)
}

// writeStoreHostAt points HOME at home and writes the shared store-host file
// there with the given body (nothing when body is empty).
func writeStoreHostAt(t *testing.T, home, body string) string {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path := filepath.Join(home, ".config", "brain", StoreHostFileName)
	if body != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("writing store host file: %v", err)
		}
	}
	t.Setenv(StoreHostKeyHost, "")
	t.Setenv(StoreHostKeyPort, "")
	return path
}

// writeBrainRegistry names each store in the brain registry under home, with
// the recorded path given.
func writeBrainRegistry(t *testing.T, home string, stores map[string]string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "brain")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var b strings.Builder
	b.WriteString("stores:\n")
	for name, path := range stores {
		fmt.Fprintf(&b, "  %s:\n    path: %q\n", name, path)
	}
	if err := os.WriteFile(filepath.Join(dir, "stores.yaml"), []byte(b.String()), 0o644); err != nil {
		t.Fatalf("writing registry: %v", err)
	}
}

// newStoreConfig writes metadata.json for a store at beadsDir and returns the
// config Load reads back for it, so the config carries the directory the
// federation predicate needs.
func newStoreConfig(t *testing.T, beadsDir string, cfg *Config) *Config {
	t.Helper()
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := cfg.Save(beadsDir); err != nil {
		t.Fatalf("writing metadata.json: %v", err)
	}
	loaded, err := Load(beadsDir)
	if err != nil || loaded == nil {
		t.Fatalf("Load(%s): %v", beadsDir, err)
	}
	return loaded
}

func TestResolve_ReadsTheSharedFile(t *testing.T) {
	writeStoreHost(t, "# the federation's server\nexport BEADS_DOLT_SERVER_HOST=10.1.2.3\nBEADS_DOLT_SERVER_PORT=\"3307\"\n")

	got, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Host != "10.1.2.3" || got.Port != 3307 || got.Source != "file" {
		t.Errorf("Resolve = %+v, want 10.1.2.3:3307 from the file", got)
	}
}

func TestResolve_ProcessEnvOverridesTheFile(t *testing.T) {
	writeStoreHost(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\nBEADS_DOLT_SERVER_PORT=3307\n")
	t.Setenv(StoreHostKeyHost, "127.0.0.9")
	t.Setenv(StoreHostKeyPort, "3399")

	got, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Host != "127.0.0.9" || got.Port != 3399 || got.Source != "env" {
		t.Errorf("Resolve = %+v, want the process env to win", got)
	}
}

func TestResolve_MissingFileIsAnErrorNamingThePath(t *testing.T) {
	path := writeStoreHost(t, "") // no file written

	_, err := Resolve()
	if err == nil {
		t.Fatal("expected an error when the shared file is missing")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error must name the missing file %q; got: %v", path, err)
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error must say the file is missing; got: %v", err)
	}
}

func TestResolve_EnvOnlyWorksWithoutTheFile(t *testing.T) {
	writeStoreHost(t, "")
	t.Setenv(StoreHostKeyHost, "127.0.0.9")
	t.Setenv(StoreHostKeyPort, "3399")

	got, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Source != "env" {
		t.Errorf("source = %q, want env", got.Source)
	}
}

func TestResolve_RefusesHalfWrittenAndMalformedFiles(t *testing.T) {
	t.Run("host only", func(t *testing.T) {
		writeStoreHost(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\n")
		if _, err := Resolve(); err == nil {
			t.Fatal("expected an error for a file without a port")
		}
	})

	t.Run("unparseable port", func(t *testing.T) {
		path := writeStoreHost(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\nBEADS_DOLT_SERVER_PORT=thirty-three\n")
		_, err := Resolve()
		if err == nil {
			t.Fatal("expected an error for an unparseable port")
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error must name the file; got: %v", err)
		}
	})

	t.Run("a half-written file refuses even with a full env override", func(t *testing.T) {
		writeStoreHost(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\n")
		t.Setenv(StoreHostKeyHost, "127.0.0.9")
		t.Setenv(StoreHostKeyPort, "3399")
		if _, err := Resolve(); err == nil {
			t.Fatal("expected an error for a present-but-unusable shared file")
		}
	})
}

// TestValidateStoreHostFile is the contract 'bd' enforces before any command
// runs: a shared file that exists but cannot be used stops bd with an error
// naming the file, while an absent file leaves per-store resolution intact.
func TestValidateStoreHostFile(t *testing.T) {
	t.Run("absent file is not an error", func(t *testing.T) {
		writeStoreHost(t, "")
		if err := ValidateStoreHostFile(); err != nil {
			t.Fatalf("ValidateStoreHostFile: %v", err)
		}
	})

	t.Run("unusable file names the file", func(t *testing.T) {
		path := writeStoreHost(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\n")
		err := ValidateStoreHostFile()
		if err == nil {
			t.Fatal("expected an error for a file without a port")
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error must name the file; got: %v", err)
		}
	})

	t.Run("valid file is not an error", func(t *testing.T) {
		writeStoreHost(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\nBEADS_DOLT_SERVER_PORT=3307\n")
		if err := ValidateStoreHostFile(); err != nil {
			t.Fatalf("ValidateStoreHostFile: %v", err)
		}
	})
}

func TestStoreHostFromFile_IgnoresProcessEnv(t *testing.T) {
	writeStoreHost(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\nBEADS_DOLT_SERVER_PORT=3307\n")
	t.Setenv(StoreHostKeyHost, "127.0.0.9")

	host, port, ok := StoreHostFromFile()
	if !ok {
		t.Fatal("StoreHostFromFile did not read the file")
	}
	if host != "10.1.2.3" || port != 3307 {
		t.Errorf("StoreHostFromFile = %s:%d, want the file's 10.1.2.3:3307", host, port)
	}
}

func TestStoreHostFromFile_MissingFileIsNotAnError(t *testing.T) {
	writeStoreHost(t, "")
	if _, _, ok := StoreHostFromFile(); ok {
		t.Error("a missing file must report ok=false, not a value")
	}
}

// TestGetDoltServerHost_FederationStoreUsesTheSharedFile is the contract behind
// 'one config source': the federation's server is one fact per machine, so a
// federation store that still carries a stale dolt_server_host must not drag
// reads back to the old host.
func TestGetDoltServerHost_FederationStoreUsesTheSharedFile(t *testing.T) {
	home := t.TempDir()
	writeStoreHostAt(t, home, "BEADS_DOLT_SERVER_HOST=10.9.9.9\nBEADS_DOLT_SERVER_PORT=3311\n")
	beadsDir := filepath.Join(home, "data", "task", ".beads")
	writeBrainRegistry(t, home, map[string]string{"task": filepath.Join(home, "data", "task")})
	cfg := newStoreConfig(t, beadsDir, &Config{
		Backend: BackendDolt, DoltMode: DoltModeServer,
		DoltServerHost: "10.0.0.1", DoltServerPort: 3300,
	})

	if got := cfg.GetDoltServerHost(); got != "10.9.9.9" {
		t.Errorf("GetDoltServerHost = %q, want the shared file's 10.9.9.9", got)
	}
	if got := cfg.GetDoltServerPort(); got != 3311 {
		t.Errorf("GetDoltServerPort = %d, want the shared file's 3311", got)
	}

	t.Setenv(StoreHostKeyHost, "127.0.0.9")
	t.Setenv(StoreHostKeyPort, "3399")
	if got := cfg.GetDoltServerHost(); got != "127.0.0.9" {
		t.Errorf("environment must win over the file; got %q", got)
	}
	if got := cfg.GetDoltServerPort(); got != 3399 {
		t.Errorf("environment must win over the file; got %d", got)
	}
}

// TestGetDoltServerHost_UpstreamSharedServerKeepsItsOwnPins: an upstream
// 'bd init --shared-server' project writes the same shared-server markers the
// brain federation uses, so the brain file must not govern it.
func TestGetDoltServerHost_UpstreamSharedServerKeepsItsOwnPins(t *testing.T) {
	home := t.TempDir()
	writeStoreHostAt(t, home, "BEADS_DOLT_SERVER_HOST=10.9.9.9\nBEADS_DOLT_SERVER_PORT=3311\n")
	beadsDir := filepath.Join(home, "projects", "unrelated", ".beads")
	cfg := newStoreConfig(t, beadsDir, &Config{
		Backend: BackendDolt, DoltMode: DoltModeServer,
		DoltServerHost: "10.0.0.5", DoltServerPort: 14567,
		GlobalDoltDatabase: "beads_global",
		GlobalProjectID:    "00000000-0000-0000-0000-000000000000",
	})

	if got := cfg.GetDoltServerHost(); got != "10.0.0.5" {
		t.Errorf("GetDoltServerHost = %q, want the project's own 10.0.0.5", got)
	}
	if got := cfg.GetDoltServerPort(); got != 14567 {
		t.Errorf("GetDoltServerPort = %d, want the project's own 14567", got)
	}
}

// TestGetDoltServerHost_UnusableFederationFileResolvesNoHost: a federation
// store with a present-but-unusable shared file must not silently fall through
// to its own pins.
func TestGetDoltServerHost_UnusableFederationFileResolvesNoHost(t *testing.T) {
	home := t.TempDir()
	writeStoreHostAt(t, home, "BEADS_DOLT_SERVER_HOST=10.1.2.3\n") // no port
	beadsDir := filepath.Join(home, "data", "task", ".beads")
	writeBrainRegistry(t, home, map[string]string{"task": filepath.Join(home, "data", "task")})
	cfg := newStoreConfig(t, beadsDir, &Config{
		Backend: BackendDolt, DoltMode: DoltModeServer,
		DoltServerHost: "10.0.0.1", DoltServerPort: 3300,
	})

	if got := cfg.GetDoltServerHost(); got == "10.0.0.1" || got == DefaultDoltServerHost {
		t.Errorf("GetDoltServerHost = %q, must not fall through to a per-store host", got)
	}
	if got := cfg.GetDoltServerPort(); got != 0 {
		t.Errorf("GetDoltServerPort = %d, must not fall through to a per-store port", got)
	}
}

// TestGetDoltServerHost_UnusableFileLeavesUnrelatedProjectAlone: the shared
// file is read only by federation stores, so a defect in it must not change an
// unrelated project's resolution.
func TestGetDoltServerHost_UnusableFileLeavesUnrelatedProjectAlone(t *testing.T) {
	home := t.TempDir()
	writeStoreHostAt(t, home, "BEADS_DOLT_SERVER_HOST=10.1.2.3\n")
	beadsDir := filepath.Join(home, "projects", "unrelated", ".beads")
	cfg := newStoreConfig(t, beadsDir, &Config{
		Backend: BackendDolt, DoltMode: DoltModeServer,
		DoltServerHost: "10.0.0.5", DoltServerPort: 14567,
		GlobalDoltDatabase: "beads_global",
	})

	if got := cfg.GetDoltServerHost(); got != "10.0.0.5" {
		t.Errorf("GetDoltServerHost = %q, want the project's own 10.0.0.5", got)
	}
	if got := cfg.GetDoltServerPort(); got != 14567 {
		t.Errorf("GetDoltServerPort = %d, want the project's own 14567", got)
	}
}

// TestIsFederationStore_RegistryMembership: federation membership comes from
// the brain registry, by name or by a recorded path that resolves to this
// store, never from upstream shared-server markers.
func TestIsFederationStore_RegistryMembership(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	registered := filepath.Join(home, "data", "tasks", ".beads")
	writeBrainRegistry(t, home, map[string]string{"notes": "/Users/someone-else/data/tasks/.beads"})
	if !IsFederationStore(registered) {
		t.Error("a recorded path under another machine's home must resolve to this machine's copy")
	}

	unrelated := filepath.Join(home, "projects", "p", ".beads")
	newStoreConfig(t, unrelated, &Config{
		GlobalDoltDatabase: "beads_global",
		GlobalProjectID:    "00000000-0000-0000-0000-000000000000",
	})
	if IsFederationStore(unrelated) {
		t.Error("shared-server markers alone must not make a store a federation store")
	}

	if IsFederationStore(filepath.Join(home, "data", "ghost", ".beads")) {
		t.Error("a store the registry does not name must not be a federation store")
	}
}

// TestResolve_MixedLayersAreLabelledMixed: the host and the port may come from
// different layers, and the label must say so rather than claim the file.
func TestResolve_MixedLayersAreLabelledMixed(t *testing.T) {
	writeStoreHost(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\nBEADS_DOLT_SERVER_PORT=3307\n")

	t.Setenv(StoreHostKeyHost, "127.0.0.9")
	got, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Host != "127.0.0.9" || got.Port != 3307 {
		t.Fatalf("Resolve = %s:%d, want 127.0.0.9:3307", got.Host, got.Port)
	}
	if got.Source != "mixed" {
		t.Errorf("Source = %q, want mixed for a host from the environment and a port from the file", got.Source)
	}
}

// TestGetDoltServerHost_FallsBackToMetadataWithoutTheFile keeps every store
// that predates the shared config working: no file means the per-store pins are
// still the answer, even for a federation store.
func TestGetDoltServerHost_FallsBackToMetadataWithoutTheFile(t *testing.T) {
	home := t.TempDir()
	writeStoreHostAt(t, home, "")
	beadsDir := filepath.Join(home, "data", "task", ".beads")
	writeBrainRegistry(t, home, map[string]string{"task": filepath.Join(home, "data", "task")})
	cfg := newStoreConfig(t, beadsDir, &Config{
		Backend: BackendDolt, DoltMode: DoltModeServer,
		DoltServerHost: "10.0.0.1", DoltServerPort: 3300,
	})

	if got := cfg.GetDoltServerHost(); got != "10.0.0.1" {
		t.Errorf("GetDoltServerHost = %q, want the metadata pin 10.0.0.1", got)
	}
	if got := cfg.GetDoltServerPort(); got != 3300 {
		t.Errorf("GetDoltServerPort = %d, want the metadata pin 3300", got)
	}
}
