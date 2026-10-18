package configfile

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/steveyegge/beads/internal/config"
	"gopkg.in/yaml.v3"
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
)

// Brain store registry locations: the canonical file and the pre-migration one.
// The registry is the authority for which stores the federation has.
const (
	brainRegistryRelPath       = ".config/brain/stores.yaml"
	brainRegistryLegacyRelPath = ".config/pai/stores.yaml"
)

// foreignHomeRe matches an absolute path under another user's home
// (/Users/<x>/… or /home/<x>/…) so a registry written on another machine still
// names this machine's copy of a store.
var foreignHomeRe = regexp.MustCompile(`^/(?:Users|home)/[^/]+(/.*)?$`)

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

// brainStoreRegistry is the on-disk shape of the brain store registry
// (~/.config/brain/stores.yaml). Only the recorded path is needed here.
type brainStoreRegistry struct {
	Stores map[string]brainStoreRecord `yaml:"stores"`
}

type brainStoreRecord struct {
	Path string `yaml:"path"`
}

// UnmarshalYAML accepts the legacy scalar form (a bare path) as well as the
// current mapping form.
func (r *brainStoreRecord) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		r.Path = value.Value
		return nil
	}
	type raw brainStoreRecord
	var out raw
	if err := value.Decode(&out); err != nil {
		return err
	}
	*r = brainStoreRecord(out)
	return nil
}

// IsFederationStore reports whether beadsDir is a brain federation store: one
// the brain store registry (~/.config/brain/stores.yaml, legacy
// ~/.config/pai/stores.yaml) names, by store name or by a recorded path that
// resolves to this store. The registry is written by whichever machine last ran
// a 'brain stores' write, so a recorded path is compared with any user's home
// prefix reduced to $HOME.
//
// The shared store-host config governs only these stores. An unrelated bd
// project — including an upstream 'bd init --shared-server' project, whose
// shared-server markers the registry does not carry — keeps its own resolution
// (port file, then config.yaml, then metadata.json).
func IsFederationStore(beadsDir string) bool {
	if beadsDir == "" {
		return false
	}
	local, err := filepath.Abs(beadsDir)
	if err != nil {
		local = filepath.Clean(beadsDir)
	}
	names := federationStoreNames(local)
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	for _, rel := range []string{brainRegistryRelPath, brainRegistryLegacyRelPath} {
		reg, ok := readBrainStoreRegistry(filepath.Join(home, rel))
		if !ok {
			continue
		}
		for name, record := range reg.Stores {
			if names[name] {
				return true
			}
			if recordedPathResolvesTo(record.Path, local) {
				return true
			}
		}
	}
	return false
}

// IsFederationStore reports whether the store this config was loaded from is a
// brain federation store (see IsFederationStore(beadsDir)). A config built
// without a store directory (DefaultConfig) is never a federation store.
func (c *Config) IsFederationStore() bool {
	if c == nil {
		return false
	}
	return IsFederationStore(c.beadsDir)
}

// federationStoreNames returns the names that can identify the store at
// beadsDir: the store directory's name, and the name the store's own
// config.yaml declares.
func federationStoreNames(beadsDir string) map[string]bool {
	names := map[string]bool{}
	base := filepath.Base(beadsDir)
	if base == ".beads" {
		base = filepath.Base(filepath.Dir(beadsDir))
	}
	if base != "" && base != "." && base != string(filepath.Separator) {
		names[base] = true
	}
	if beadsDir == "" {
		return names
	}
	for _, key := range []string{"BD_NAME", "issue-prefix"} {
		if v := strings.TrimSpace(config.GetStringFromDir(beadsDir, key)); v != "" {
			names[v] = true
		}
	}
	return names
}

// recordedPathResolvesTo reports whether a registry-recorded store path names
// beadsDir. A recorded path may point at the .beads directory or at the store
// root that contains one, and it may name another machine's home.
func recordedPathResolvesTo(recorded, beadsDir string) bool {
	recorded = strings.TrimSpace(recorded)
	if recorded == "" {
		return false
	}
	recorded = expandRecordedHome(recorded)
	for _, candidate := range []string{recorded, filepath.Join(recorded, ".beads")} {
		if filepath.Clean(candidate) == filepath.Clean(beadsDir) {
			return true
		}
	}
	recordedRel, recordedOK := homeRelativePath(recorded)
	localRel, localOK := homeRelativePath(beadsDir)
	if !recordedOK || !localOK {
		return false
	}
	return recordedRel == localRel ||
		filepath.Join(recordedRel, ".beads") == localRel ||
		filepath.Join(localRel, ".beads") == recordedRel
}

// expandRecordedHome expands a leading ~ / $HOME / ${HOME} a registry path may
// use, so a registry need not be pre-resolved.
func expandRecordedHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	p = strings.ReplaceAll(p, "${HOME}", home)
	p = strings.ReplaceAll(p, "$HOME", home)
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// homeRelativePath reduces a path under a user's home — this machine's home or
// any /Users/<x> or /home/<x> home — to its home-relative form.
func homeRelativePath(p string) (string, bool) {
	p = filepath.Clean(p)
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" &&
		(p == home || strings.HasPrefix(p, home+"/")) {
		return p[len(home):], true
	}
	if m := foreignHomeRe.FindStringSubmatch(p); m != nil {
		return m[1], true
	}
	return "", false
}

// readBrainStoreRegistry reads one registry file; ok is false when it is
// absent or unreadable, which leaves the store unrecognized rather than an
// error.
func readBrainStoreRegistry(path string) (brainStoreRegistry, bool) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-owned registry, fixed path
	if err != nil {
		return brainStoreRegistry{}, false
	}
	var reg brainStoreRegistry
	if err := yaml.Unmarshal(data, &reg); err != nil {
		return brainStoreRegistry{}, false
	}
	return reg, true
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
