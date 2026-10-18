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
// every store wrapper. Repointing a host, or moving a whole federation, is then
// one file edit and no wrapper rewrite.
//
// The file is KEY=VALUE, one per line, deliberately shell-sourceable so the
// generated store wrappers can `. "$file"` it at run time:
//
//	BEADS_DOLT_SERVER_HOST=127.0.0.1
//	BEADS_DOLT_SERVER_PORT=3307
//
// Only the brain verbs read it: 'brain stores create' and
// 'brain stores rewrite-wrappers' refuse up front when it is absent, and the
// generated store wrappers read it at run time and exit non-zero when it is
// missing or unusable. Core bd resolution is untouched: the wrappers export the
// two variables, which bd already honours. Credentials never live here:
// passwords stay in the environment or in ~/.config/beads/credentials.
const (
	// StoreHostFileName is the basename of the shared store-host config.
	StoreHostFileName = "store-host.env"

	// StoreHostKeyHost and StoreHostKeyPort are the only keys this file
	// defines. Other lines are ignored so the file stays usable as a shell
	// fragment.
	StoreHostKeyHost = "BEADS_DOLT_SERVER_HOST"
	StoreHostKeyPort = "BEADS_DOLT_SERVER_PORT"
)

// ErrStoreHostMissing reports that the shared store-host config is not there.
// The brain verbs turn this into a loud refusal naming the path.
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
	// HostSource and PortSource name where each value came from: "env" or
	// "file".
	HostSource string
	PortSource string
	// Source is "env" when both values came from the process environment,
	// "file" when both came from the shared file, and "mixed" when they came
	// from different layers.
	Source string
}

// Resolve returns the store host/port for this process: a process env value
// wins per key, the shared file supplies what the environment does not, and a
// missing file is ErrStoreHostMissing (never a silent 127.0.0.1). A file that
// exists but does not define both keys, or defines an unusable port, is an
// error naming the file — a half-written config is a defect to fix, not a
// fallback.
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
		out.HostSource = "file"
	} else {
		out.HostSource = "env"
	}
	if envPort != "" {
		p, err := strconv.Atoi(envPort)
		if err != nil || p <= 0 {
			return StoreHost{}, fmt.Errorf("%s=%q is not a valid port", StoreHostKeyPort, envPort)
		}
		out.Port = p
		out.PortSource = "env"
	} else {
		out.Port = filePort
		out.PortSource = "file"
	}

	switch {
	case out.HostSource == "env" && out.PortSource == "env":
		out.Source = "env"
	case out.HostSource == "file" && out.PortSource == "file":
		out.Source = "file"
	default:
		out.Source = "mixed"
	}
	if out.Host == "" || out.Port <= 0 {
		return StoreHost{}, fmt.Errorf("%s does not define both %s and %s", path, StoreHostKeyHost, StoreHostKeyPort)
	}
	return out, nil
}

// ValidateStoreHostFile reports a shared store-host config that exists but
// cannot be used: a missing key, an unparseable port, or an unreadable file.
// The brain verbs call it before they touch a wrapper or create a store, so
// such a file is a loud failure naming the file. An absent file is not an
// error.
func ValidateStoreHostFile() error {
	_, _, err := loadStoreHostFile(DefaultStoreHostPath())
	if err == nil || errors.Is(err, ErrStoreHostMissing) {
		return nil
	}
	return err
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
