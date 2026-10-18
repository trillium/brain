package configfile

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The brain federation keeps the Dolt sql-server it talks to in ONE shared file
// per machine — ~/.config/brain/store-host.env — instead of baking a host into
// every store wrapper (and into every store's metadata.json). Repointing a
// host, or moving a whole federation, is then one file edit and no wrapper
// rewrite, and no tool that reads the file can disagree with another about
// which server the federation lives on.
//
// The file is KEY=VALUE, one per line, deliberately shell-sourceable so the
// generated store wrappers can `. "$file"` it at run time:
//
//	BEADS_DOLT_SERVER_HOST=127.0.0.1
//	BEADS_DOLT_SERVER_PORT=3307
//
// The process environment of the same name overrides the file, so a one-off
// `BEADS_DOLT_SERVER_HOST=other.host store list` keeps working. Credentials
// never live here: passwords stay in the environment or in
// ~/.config/beads/credentials (see GetDoltServerPasswordForPort).
const (
	// StoreHostFileName is the basename of the shared store-host config.
	StoreHostFileName = "store-host.env"

	// StoreHostFileEnvVar overrides the shared file's path. It exists so the
	// generated wrappers and bd always agree on which file is authoritative
	// (a test or a staged migration can point both at the same file).
	StoreHostFileEnvVar = "BRAIN_STORE_HOST_FILE"

	// StoreHostKeyHost and StoreHostKeyPort name the server; the other two keys
	// are the CREDENTIAL REFERENCE. The record never holds a secret: it names
	// the MySQL user and the path of the password file, and bd resolves the
	// password from that file at run time (see LookupCredentialsPassword).
	// Unrelated lines are ignored so the file stays usable as a shell fragment.
	StoreHostKeyHost            = "BEADS_DOLT_SERVER_HOST"
	StoreHostKeyPort            = "BEADS_DOLT_SERVER_PORT"
	StoreHostKeyUser            = "BEADS_DOLT_SERVER_USER"
	StoreHostKeyCredentialsFile = "BEADS_CREDENTIALS_FILE"
)

// ErrStoreHostMissing reports that the shared store-host config is not there.
// Callers that must not silently fall back to a default server (the generated
// store wrappers, 'brain stores create') turn this into a loud refusal naming
// the path; callers resolving a host incidentally treat it as "no value".
var ErrStoreHostMissing = errors.New("shared store host config missing")

// DefaultStoreHostPath returns the shared store-host config path for this
// user: $BRAIN_STORE_HOST_FILE when set, else ~/.config/brain/store-host.env.
func DefaultStoreHostPath() string {
	if p := strings.TrimSpace(os.Getenv(StoreHostFileEnvVar)); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "brain", StoreHostFileName)
}

// StoreHost is the resolved Dolt sql-server the federation lives on.
type StoreHost struct {
	Host string
	Port int
	// Source names where each value came from: "env", "file", or "" when unset.
	Source string
}

// Resolve returns the store host/port for this process: a process env value
// wins, the shared file supplies what the environment does not, and a missing
// file is ErrStoreHostMissing (never a silent 127.0.0.1). A file that exists but
// does not define both keys, or defines an unusable port, is an error naming
// the file — a half-written config is a defect to fix, not a fallback.
func Resolve() (StoreHost, error) {
	envHost := strings.TrimSpace(os.Getenv(StoreHostKeyHost))
	envPort := strings.TrimSpace(os.Getenv(StoreHostKeyPort))

	path := DefaultStoreHostPath()
	fileHost, filePort := "", 0
	if envHost == "" || envPort == "" {
		h, p, err := loadStoreHostFile(path)
		if err != nil {
			return StoreHost{}, err
		}
		fileHost, filePort = h, p
	}

	out := StoreHost{Host: envHost, Port: 0}
	if out.Host == "" {
		out.Host = fileHost
	}
	if envPort != "" {
		p, err := strconv.Atoi(envPort)
		if err != nil || p <= 0 {
			return StoreHost{}, fmt.Errorf("%s=%q is not a valid port", StoreHostKeyPort, envPort)
		}
		out.Port = p
	} else {
		out.Port = filePort
	}

	switch {
	case envHost != "" && envPort != "":
		out.Source = "env"
	case fileHost != "" && filePort > 0:
		out.Source = "file"
	case out.Host != "" && out.Port > 0:
		out.Source = "mixed"
	}
	if out.Host == "" || out.Port <= 0 {
		return StoreHost{}, fmt.Errorf("%s does not define both %s and %s", path, StoreHostKeyHost, StoreHostKeyPort)
	}
	return out, nil
}

// StoreHostValue returns one raw value from the shared store-host file,
// ignoring the process environment. ok is false when the file is absent or
// does not define the key, so callers keep their own fallback chain. It is the
// accessor behind the record's credential reference (the MySQL user and the
// password file's path) — never a password.
func StoreHostValue(key string) (string, bool) {
	values, err := ReadStoreHostValues(DefaultStoreHostPath())
	if err != nil {
		return "", false
	}
	v, ok := values[key]
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// CredentialsRef is the credential REFERENCE the federation's store-host record
// carries: which MySQL user to connect as and which file holds the password.
// The password itself is never in the record, never printed, and never in git.
// An empty field means the record names nothing and the caller falls back to
// bd's own defaults (user "root", ~/.config/beads/credentials).
//
// FileExists reports whether CredentialsFile is present, so a caller can tell
// "no reference configured" from "the referenced file is missing" without ever
// reading a password.
type CredentialsRef struct {
	User            string
	CredentialsFile string
	FileExists      bool
}

// ResolveCredentialsRef returns the credential reference for this process: a
// process env value wins per key, the shared file supplies what the environment
// does not, and path existence is reported for the credentials file. It never
// reads or returns a password.
func ResolveCredentialsRef() CredentialsRef {
	ref := CredentialsRef{
		User:            strings.TrimSpace(os.Getenv(StoreHostKeyUser)),
		CredentialsFile: strings.TrimSpace(os.Getenv(StoreHostKeyCredentialsFile)),
	}
	values, err := ReadStoreHostValues(DefaultStoreHostPath())
	if err == nil {
		if ref.User == "" {
			ref.User = values[StoreHostKeyUser]
		}
		if ref.CredentialsFile == "" {
			ref.CredentialsFile = values[StoreHostKeyCredentialsFile]
		}
	}
	if ref.CredentialsFile != "" {
		if _, statErr := os.Stat(ref.CredentialsFile); statErr == nil {
			ref.FileExists = true
		}
	}
	return ref
}

// StoreHostFromFile reports the host/port the shared file defines, ignoring the
// process environment. ok is false when the file is absent (or when
// DefaultStoreHostPath cannot be resolved), so this is safe to call from
// host-resolution paths that have already consulted the environment.
func StoreHostFromFile() (host string, port int, ok bool) {
	h, p, err := loadStoreHostFile(DefaultStoreHostPath())
	if err != nil {
		return "", 0, false
	}
	return h, p, true
}

// ReadStoreHostValues parses one store-host file into its keys, skipping lines
// the format does not define. A missing file (or an unresolvable path) returns
// ErrStoreHostMissing so callers can name the path that should exist.
func ReadStoreHostValues(path string) (map[string]string, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: %s", ErrStoreHostMissing, StoreHostFileName)
	}
	f, err := os.Open(path) //nolint:gosec // operator-owned config path
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrStoreHostMissing, path)
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	values := make(map[string]string)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, value, ok := ParseStoreHostLine(sc.Text())
		if !ok {
			continue
		}
		values[key] = value
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return values, nil
}

// loadStoreHostFile reads one store-host file. A missing file (or an
// unresolvable path) returns ErrStoreHostMissing so callers can name the path
// that should exist.
func loadStoreHostFile(path string) (string, int, error) {
	if path == "" {
		return "", 0, fmt.Errorf("%w: %s", ErrStoreHostMissing, StoreHostFileName)
	}
	f, err := os.Open(path) //nolint:gosec // operator-owned config path
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, fmt.Errorf("%w: %s", ErrStoreHostMissing, path)
		}
		return "", 0, fmt.Errorf("reading %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	host := ""
	port := 0
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		key, value, ok := ParseStoreHostLine(sc.Text())
		if !ok {
			continue
		}
		switch key {
		case StoreHostKeyHost:
			host = value
		case StoreHostKeyPort:
			p, err := strconv.Atoi(value)
			if err != nil || p <= 0 {
				return "", 0, fmt.Errorf("%s:%d: %s=%q is not a valid port", path, lineNo, key, value)
			}
			port = p
		}
	}
	if err := sc.Err(); err != nil {
		return "", 0, fmt.Errorf("reading %s: %w", path, err)
	}
	if host == "" || port <= 0 {
		return "", 0, fmt.Errorf("%s does not define both %s and %s", path, StoreHostKeyHost, StoreHostKeyPort)
	}
	return host, port, nil
}

// ParseStoreHostLine parses one KEY=VALUE line, accepting an optional leading
// `export`, surrounding quotes, and `#` comments. Blank lines and anything
// without '=' are skipped so the file stays a valid shell fragment.
func ParseStoreHostLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")
	line = strings.TrimSpace(line)
	if idx := strings.Index(line, "#"); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	key, value, found := strings.Cut(line, "=")
	if !found {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
	}
	if key == "" {
		return "", "", false
	}
	return key, strings.TrimSpace(value), true
}
