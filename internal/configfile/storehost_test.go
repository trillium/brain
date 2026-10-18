package configfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeStoreHost points BRAIN_STORE_HOST_FILE at a temp file with the given
// body and returns its path.
func writeStoreHost(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), StoreHostFileName)
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("writing store host file: %v", err)
		}
	}
	t.Setenv(StoreHostFileEnvVar, path)
	t.Setenv(StoreHostKeyHost, "")
	t.Setenv(StoreHostKeyPort, "")
	return path
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

// TestGetDoltServerHost_SharedFileBeatsPerStoreMetadata is the contract behind
// 'one config source': the federation's server is one fact per machine, so a
// store that still carries a stale dolt_server_host must not drag reads back to
// the old host.
func TestGetDoltServerHost_SharedFileBeatsPerStoreMetadata(t *testing.T) {
	writeStoreHost(t, "BEADS_DOLT_SERVER_HOST=10.9.9.9\nBEADS_DOLT_SERVER_PORT=3311\n")
	cfg := &Config{Backend: BackendDolt, DoltMode: DoltModeServer, DoltServerHost: "10.0.0.1", DoltServerPort: 3300}

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

// TestGetDoltServerHost_FallsBackToMetadataWithoutTheFile keeps every store
// that predates the shared config working: no file means the per-store pins are
// still the answer.
func TestGetDoltServerHost_FallsBackToMetadataWithoutTheFile(t *testing.T) {
	writeStoreHost(t, "")
	cfg := &Config{Backend: BackendDolt, DoltMode: DoltModeServer, DoltServerHost: "10.0.0.1", DoltServerPort: 3300}

	if got := cfg.GetDoltServerHost(); got != "10.0.0.1" {
		t.Errorf("GetDoltServerHost = %q, want the metadata pin 10.0.0.1", got)
	}
	if got := cfg.GetDoltServerPort(); got != 3300 {
		t.Errorf("GetDoltServerPort = %d, want the metadata pin 3300", got)
	}
}
