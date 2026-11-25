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
	path := DefaultStoreHostPath()
	if body != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("writing store host file: %v", err)
		}
	}
	return path
}

func TestDefaultStoreHostPath_IsUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	want := filepath.Join(home, storeHostRelPath)
	if got := DefaultStoreHostPath(); got != want {
		t.Errorf("DefaultStoreHostPath = %q, want %q", got, want)
	}
	if got := StoreHostPathDisplay(); got != want {
		t.Errorf("StoreHostPathDisplay = %q, want %q", got, want)
	}
}

func TestStoreHostFromFile_ReadsBothKeys(t *testing.T) {
	writeStoreHost(t, "# the federation's server\nexport BEADS_DOLT_SERVER_HOST=10.1.2.3\nBEADS_DOLT_SERVER_PORT=\"3307\"\n")
	t.Setenv(StoreHostKeyHost, "127.0.0.9") // process env must be ignored here

	host, port, ok := StoreHostFromFile()
	if !ok {
		t.Fatal("StoreHostFromFile did not read the file")
	}
	if host != "10.1.2.3" || port != 3307 {
		t.Errorf("StoreHostFromFile = %s:%d, want the file's 10.1.2.3:3307", host, port)
	}
}

func TestStoreHostFromFile_MissingOrUnusableIsNotOk(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		writeStoreHost(t, "")
		if _, _, ok := StoreHostFromFile(); ok {
			t.Error("a missing file must report ok=false")
		}
	})

	t.Run("missing a key", func(t *testing.T) {
		writeStoreHost(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\n")
		if _, _, ok := StoreHostFromFile(); ok {
			t.Error("a file without a port must report ok=false")
		}
	})

	t.Run("unparseable port", func(t *testing.T) {
		writeStoreHost(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\nBEADS_DOLT_SERVER_PORT=thirty-three\n")
		if _, _, ok := StoreHostFromFile(); ok {
			t.Error("a file with an unusable port must report ok=false")
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

	t.Run("unparseable port names the file", func(t *testing.T) {
		path := writeStoreHost(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\nBEADS_DOLT_SERVER_PORT=thirty-three\n")
		err := ValidateStoreHostFile()
		if err == nil {
			t.Fatal("expected an error for an unparseable port")
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

// TestReadStoreHostDefaults is the reader a store command uses for its default
// server: it sees the file the way a shell that sources it does, key by key.
func TestReadStoreHostDefaults(t *testing.T) {
	t.Run("absent is not an error", func(t *testing.T) {
		path := writeStoreHost(t, "")
		d, found, err := ReadStoreHostDefaults()
		if found || err != nil {
			t.Fatalf("absent file: found=%v err=%v, want false and nil", found, err)
		}
		if d.Path != path || d.Host != "" || d.Port != "" || d.User != "" {
			t.Errorf("absent file defaults = %+v, want only the path", d)
		}
	})

	t.Run("reads host, port and user", func(t *testing.T) {
		writeStoreHost(t, "# the federation's server\nexport BEADS_DOLT_SERVER_HOST=10.1.2.3\nexport BEADS_DOLT_SERVER_PORT=\"3307\"\nexport BEADS_DOLT_SERVER_USER=brain_app\n")
		t.Setenv(StoreHostKeyHost, "127.0.0.9") // the file is read as written, never the env
		d, found, err := ReadStoreHostDefaults()
		if !found || err != nil {
			t.Fatalf("found=%v err=%v, want true and nil", found, err)
		}
		if d.Host != "10.1.2.3" || d.Port != "3307" || d.User != "brain_app" {
			t.Errorf("defaults = %+v, want 10.1.2.3 / 3307 / brain_app", d)
		}
	})

	t.Run("a host without a port is still a host", func(t *testing.T) {
		writeStoreHost(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\n")
		d, found, err := ReadStoreHostDefaults()
		if !found || err != nil {
			t.Fatalf("found=%v err=%v, want true and nil", found, err)
		}
		if d.Host != "10.1.2.3" || d.Port != "" || d.User != "" {
			t.Errorf("defaults = %+v, want only the host", d)
		}
	})

	t.Run("an unusable port is dropped and reported, the host is kept", func(t *testing.T) {
		path := writeStoreHost(t, "BEADS_DOLT_SERVER_HOST=10.1.2.3\nBEADS_DOLT_SERVER_PORT=thirty-three\n")
		d, found, err := ReadStoreHostDefaults()
		if !found {
			t.Fatal("a file with a bad port is still found")
		}
		if err == nil || !strings.Contains(err.Error(), path+":2") || !strings.Contains(err.Error(), "thirty-three") {
			t.Errorf("err = %v, want it to name %s:2 and the bad value", err, path)
		}
		if d.Host != "10.1.2.3" || d.Port != "" {
			t.Errorf("defaults = %+v, want the host kept and the port dropped", d)
		}
	})

	t.Run("a value only a shell can expand is dropped and reported", func(t *testing.T) {
		path := writeStoreHost(t, "export BEADS_DOLT_SERVER_HOST=\"${BEADS_DOLT_SERVER_HOST:-10.1.2.3}\"\nexport BEADS_DOLT_SERVER_PORT=3307\n")
		d, found, err := ReadStoreHostDefaults()
		if !found {
			t.Fatal("the file is still found")
		}
		if err == nil || !strings.Contains(err.Error(), path+":1") || !strings.Contains(err.Error(), "needs a shell") {
			t.Errorf("err = %v, want it to name %s:1 and the shell syntax", err, path)
		}
		if d.Host != "" || d.Port != "3307" {
			t.Errorf("defaults = %+v, want the host dropped and the port kept", d)
		}
	})

	t.Run("a later valid port replaces an earlier bad one", func(t *testing.T) {
		writeStoreHost(t, "BEADS_DOLT_SERVER_PORT=nope\nBEADS_DOLT_SERVER_PORT=3307\n")
		d, _, err := ReadStoreHostDefaults()
		if err != nil || d.Port != "3307" {
			t.Errorf("port=%q err=%v, want 3307 and nil (the last assignment wins, as in a shell)", d.Port, err)
		}
	})
}
