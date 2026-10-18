package configfile

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/steveyegge/beads/internal/config"
)

// The brain federation keeps the Dolt sql-server it talks to in ONE shared file
// per machine — ~/.config/brain/store-host.env — instead of baking a host into
// every store wrapper (and into every store's metadata.json). Repointing a
// host, or moving a whole federation, is then one file edit and no wrapper
// rewrite, and no tool that reads the file can disagree with another about
// which server the federation lives on.
//
// The file governs brain federation stores only (see IsFederationStore). An
// unrelated bd project keeps its own per-project resolution: port file, then
// config.yaml, then metadata.json.
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

	// StoreHostKeyHost and StoreHostKeyPort are the only keys this file
	// defines. Other lines are ignored so the file stays usable as a shell
	// fragment.
	StoreHostKeyHost = "BEADS_DOLT_SERVER_HOST"
	StoreHostKeyPort = "BEADS_DOLT_SERVER_PORT"

	// SharedServerEnvVar selects bd's shared-server mode for this process — the
	// mode every generated brain store wrapper selects.
	SharedServerEnvVar = "BEADS_DOLT_SHARED_SERVER"
)

// ErrStoreHostMissing reports that the shared store-host config is not there.
// Callers that must not silently fall back to a default server (the generated
// store wrappers, 'brain stores create') turn this into a loud refusal naming
// the path; callers resolving a host incidentally treat it as "no value".
var ErrStoreHostMissing = errors.New("shared store host config missing")

// DefaultStoreHostPath returns the shared store-host config path for this
// user: ~/.config/brain/store-host.env, one file per machine.
func DefaultStoreHostPath() string {
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
	// Source names where the values came from: "env", "file", or "mixed" when
	// the host and the port come from different layers.
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
	h, p, err := loadStoreHostFile(path)
	if err != nil && !errors.Is(err, ErrStoreHostMissing) {
		return StoreHost{}, err
	}
	if errors.Is(err, ErrStoreHostMissing) && (envHost == "" || envPort == "") {
		return StoreHost{}, err
	}
	fileHost, filePort := h, p

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
	case envHost != "" || envPort != "":
		out.Source = "mixed"
	case fileHost != "" && filePort > 0:
		out.Source = "file"
	}
	if out.Host == "" || out.Port <= 0 {
		return StoreHost{}, fmt.Errorf("%s does not define both %s and %s", path, StoreHostKeyHost, StoreHostKeyPort)
	}
	return out, nil
}

// SharedServerSelected reports whether this process runs in bd's shared-server
// mode (the BEADS_DOLT_SHARED_SERVER env var or config.yaml's
// dolt.shared-server) — the mode every generated brain store wrapper selects.
func SharedServerSelected() bool {
	if v := os.Getenv(SharedServerEnvVar); v == "1" || strings.EqualFold(v, "true") {
		return true
	}
	return config.GetBool("dolt.shared-server")
}

// IsFederationStore reports whether the store at beadsDir is a brain federation
// store: one in bd's shared-server mode, which is what the generated store
// wrappers select and what 'brain stores create' provisions (metadata.json's
// global_dolt_database / global_project_id, the shared-server env, or
// config.yaml's dolt.shared-server).
//
// The shared store-host config governs only these stores. An unrelated bd
// project keeps its own resolution (port file, then config.yaml, then
// metadata.json) exactly as upstream.
func IsFederationStore(beadsDir string) bool {
	if SharedServerSelected() {
		return true
	}
	if beadsDir == "" {
		return false
	}
	if _, err := os.Stat(ConfigPath(beadsDir)); err != nil {
		return false
	}
	cfg, err := Load(beadsDir)
	if err != nil || cfg == nil {
		return false
	}
	return cfg.IsFederationStore()
}

// IsFederationStore reports whether this config describes a brain federation
// store (see IsFederationStore(beadsDir)).
func (c *Config) IsFederationStore() bool {
	if SharedServerSelected() {
		return true
	}
	if c == nil {
		return false
	}
	return c.GlobalDoltDatabase != "" || c.GlobalProjectID != ""
}

// StoreHostFromFile reports the host/port the shared file defines, ignoring the
// process environment. ok is false when the file is absent, unreadable, or does
// not define both keys, so this is safe to call from host-resolution paths that
// have already consulted the environment; bd validates a present-but-unusable
// file before any command runs (ValidateStoreHostFile).
func StoreHostFromFile() (host string, port int, ok bool) {
	h, p, err := loadStoreHostFile(DefaultStoreHostPath())
	if err != nil {
		return "", 0, false
	}
	return h, p, true
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
		key, value, ok := parseStoreHostLine(sc.Text())
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

// ValidateStoreHostFile reports a shared store-host config that exists but
// cannot be used: a missing key, an unparseable port, or an unreadable file.
// bd calls it before any command so such a file is a loud failure naming the
// file instead of a silent fallback to a per-store host. An absent file is not
// an error — that path keeps its per-store behaviour.
func ValidateStoreHostFile() error {
	_, _, err := loadStoreHostFile(DefaultStoreHostPath())
	if err == nil || errors.Is(err, ErrStoreHostMissing) {
		return nil
	}
	return err
}

// parseStoreHostLine parses one KEY=VALUE line, accepting an optional leading
// `export`, surrounding quotes, and `#` comments. Blank lines and anything
// without '=' are skipped so the file stays a valid shell fragment.
func parseStoreHostLine(line string) (key, value string, ok bool) {
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
