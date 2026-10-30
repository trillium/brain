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
// two variables, which bd already honors. Credentials never live here:
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

// storeHostRelPath is the shared store-host config path relative to a user's
// home directory. Every path this package reports is built from it.
var storeHostRelPath = filepath.Join(".config", "brain", StoreHostFileName)

// DefaultStoreHostPath returns the shared store-host config path for this
// user: ~/.config/brain/store-host.env, one file per machine.
func DefaultStoreHostPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, storeHostRelPath)
}

// StoreHostHomeRelativePath returns the shared store-host config path in the
// "$HOME/…" form a generated wrapper uses, so the wrapper stays valid on any
// machine.
func StoreHostHomeRelativePath() string {
	return "$HOME/" + filepath.ToSlash(storeHostRelPath)
}

// StoreHostPathDisplay returns the shared store-host config path for messages:
// the resolved absolute path, or its "~"-relative form when the home directory
// cannot be resolved.
func StoreHostPathDisplay() string {
	if path := DefaultStoreHostPath(); path != "" {
		return path
	}
	return "~/" + filepath.ToSlash(storeHostRelPath)
}

// StoreHostFromFile reports the host/port the shared file defines, ignoring the
// process environment. ok is false when the file is absent or unusable;
// ValidateStoreHostFile distinguishes those cases for callers that must refuse.
func StoreHostFromFile() (host string, port int, ok bool) {
	h, p, err := loadStoreHostFile(DefaultStoreHostPath())
	if err != nil {
		return "", 0, false
	}
	return h, p, true
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
