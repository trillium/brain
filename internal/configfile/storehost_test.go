package configfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeStoreHost points HOME at a temp dir, writes the machine's shared
// store-host file there with the given body (nothing when body is empty), and
// returns the file's path.
func writeStoreHost(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
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

func TestResolve_ReadsTheSharedFile(t *testing.T) {
	writeStoreHost(t, "# the federation's server\nexport BEADS_DOLT_SERVER_HOST=10.1.2.3\nBEADS_DOLT_SERVER_PORT=\"3307\"\n")

	got, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Host != "10.1.2.3" || got.Port != 3307 || got.Source != "file" {
		t.Errorf("Resolve = %+v, want 10.1.2.3:3307 from the file", got)
	}
	if got.HostSource != "file" || got.PortSource != "file" {
		t.Errorf("provenance = %q/%q, want file/file", got.HostSource, got.PortSource)
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
	if got.HostSource != "env" || got.PortSource != "env" {
		t.Errorf("provenance = %q/%q, want env/env", got.HostSource, got.PortSource)
	}
}

// TestResolve_MixedLayersAreLabelledMixed: the host and the port may come from
// different layers, and the label must say which key came from where rather
// than claiming the file supplied both.
func TestResolve_MixedLayersAreLabelledMixed(t *testing.T) {
	t.Run("host from env, port from file", func(t *testing.T) {
		writeStoreHost(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\nBEADS_DOLT_SERVER_PORT=3307\n")
		t.Setenv(StoreHostKeyHost, "127.0.0.9")

		got, err := Resolve()
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Host != "127.0.0.9" || got.Port != 3307 {
			t.Fatalf("Resolve = %s:%d, want 127.0.0.9:3307", got.Host, got.Port)
		}
		if got.Source != "mixed" || got.HostSource != "env" || got.PortSource != "file" {
			t.Errorf("provenance = %q (%s/%s), want mixed (env/file)", got.Source, got.HostSource, got.PortSource)
		}
	})

	t.Run("port from env, host from file", func(t *testing.T) {
		writeStoreHost(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\nBEADS_DOLT_SERVER_PORT=3307\n")
		t.Setenv(StoreHostKeyPort, "3399")

		got, err := Resolve()
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Host != "10.1.2.3" || got.Port != 3399 {
			t.Fatalf("Resolve = %s:%d, want 10.1.2.3:3399", got.Host, got.Port)
		}
		if got.Source != "mixed" || got.HostSource != "file" || got.PortSource != "env" {
			t.Errorf("provenance = %q (%s/%s), want mixed (file/env)", got.Source, got.HostSource, got.PortSource)
		}
	})
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

// TestValidateStoreHostFile is the contract the brain write verbs enforce: a
// shared file that exists but cannot be used is a loud error naming the file,
// while an absent file is not an error.
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
