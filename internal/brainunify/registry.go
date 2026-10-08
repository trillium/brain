// Package brainunify consolidates brain's federated per-store Dolt databases
// into ONE authoritative Dolt database, preserving the logical separation
// between stores as id-prefix namespaces.
//
// The mechanism is prefixes. A bead id is already self-describing
// ("brain-se7t.389" -> prefix "brain"), so the store a bead belongs to is
// already encoded in the primary key. Unification therefore needs no change
// to the issues schema, no rewrite of any id, and no change to any query that
// filters by id or prefix. What the separate databases carried that a single
// database cannot express directly — which physical database a row came from,
// which store declares which prefix — is recorded explicitly in the
// brain_stores / brain_store_prefixes tables the builder creates.
//
// The package is deliberately split so that everything except the SQL access
// is pure and unit-testable:
//
//   - registry.go   store registry -> (store, Dolt database) resolution
//   - namespace.go  id prefix parsing and namespace ownership rules
//   - plan.go       inventory + collision analysis (pure)
//   - dbsource.go   read-only SQL access to the source Dolt server
//   - schema.go     unified schema: classification of tables, brain_* DDL
//   - build.go      construction of the unified database
//   - verify.go     mechanical source-vs-unified comparison (pure decision,
//     thin SQL shell)
//   - report.go     deterministic text rendering of plan and verify results
//
// Production safety. Nothing in this package writes to a source database.
// dbsource is constructed with a read-only intent and every statement it can
// issue is a SELECT; the builder opens a second, separate connection to an
// isolated Dolt server that it starts under a caller-supplied data directory.
package brainunify

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Registry entry shapes. These mirror ~/.config/brain/stores.yaml, the file
// `brain stores` maintains. The registry is the authority for which stores
// exist; the store's metadata.json is the authority for which Dolt database
// backs it.
const (
	registryCanonical = ".config/brain/stores.yaml"
	registryLegacy    = ".config/pai/stores.yaml"
)

// Store is one federated brain store: the registry name, the .beads directory
// on disk, and the Dolt database on the shared server that actually holds its
// rows. Store name and Dolt database name are NOT the same thing in practice
// (registry "decisions" -> database "decision", "robots" -> "agent",
// "assertions" -> "assert", "brain" -> "dolt"), so both are carried.
type Store struct {
	// Namespace is the name this source has in the unified database. For a
	// registered store it is the registry key ("inbox"). For a database
	// discovered on the server but claimed by no store it is "db:<name>".
	Namespace string
	// Registered reports whether the registry claimed this store.
	Registered bool
	// BeadsDir is the store's .beads directory on disk.
	BeadsDir string
	// Database is the Dolt SQL database name on the server.
	Database string
	// DeclaredPrefixes are the prefixes the store claims via config.yaml
	// (issue-prefix, then BD_NAME). Empty when the store declares none.
	DeclaredPrefixes []string
	// ProjectID is the store's project identity from metadata.json. Retained
	// so provenance survives into the unified database.
	ProjectID string
	// Host and Port locate the source Dolt sql-server.
	Host string
	Port int
	// ServerMode reports whether the store is server-backed. Embedded stores
	// have no server coordinates and are rejected by the builder.
	ServerMode bool
	// About is the registry's human description, carried through for reports.
	About string
}

// Prefix returns the store's primary declared prefix, or "" if it declares
// none. It is used only for display and for the affinity tiebreak in
// namespace ownership; the full DeclaredPrefixes list is what the plan uses.
func (s Store) Prefix() string {
	if len(s.DeclaredPrefixes) == 0 {
		return ""
	}
	return s.DeclaredPrefixes[0]
}

// DSN builds a go-sql-driver DSN for this store's database. The source
// connection is only ever used for SELECTs.
func (s Store) DSN() string {
	host := s.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := s.Port
	if port == 0 {
		port = 3307
	}
	return fmt.Sprintf("root@tcp(%s:%d)/%s?parseTime=false&multiStatements=false",
		host, port, s.Database)
}

// metadataJSON is the subset of a store's .beads/metadata.json that matters
// for unification. Deliberately a local struct rather than a dependency on
// the configfile package: the builder must keep working when metadata.json
// predates or postdates the current schema.
type metadataJSON struct {
	DoltMode           string `json:"dolt_mode"`
	DoltServerHost     string `json:"dolt_server_host"`
	DoltServerPort     int    `json:"dolt_server_port"`
	DoltDatabase       string `json:"dolt_database"`
	ProjectID          string `json:"project_id"`
	GlobalDoltDatabase string `json:"global_dolt_database"`
}

// registryFile is the on-disk shape of stores.yaml.
type registryFile struct {
	Stores map[string]registryEntry `yaml:"stores"`
}

// registryEntry accepts both the legacy scalar form (a bare path) and the
// current mapping form, matching what `brain stores` writes.
type registryEntry struct {
	Path  string `yaml:"path"`
	About string `yaml:"about,omitempty"`
}

// UnmarshalYAML accepts the scalar (legacy) form as well as the mapping form.
func (r *registryEntry) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		r.Path = value.Value
		return nil
	}
	type raw registryEntry
	var out raw
	if err := value.Decode(&out); err != nil {
		return err
	}
	*r = registryEntry(out)
	return nil
}

// Registry is the resolved store set for a unification run.
type Registry struct {
	// Stores is every source database the run will consider, sorted by
	// Namespace. Registration and discovery are merged here.
	Stores []Store
	// RegisteredNames are the registry keys, sorted.
	RegisteredNames []string
	// UnclaimedNames are server databases that no store claims and that are
	// therefore included under a "db:<name>" namespace. They are reported
	// explicitly because they may be abandoned data.
	UnclaimedNames []string
	// ReplicaNames are server databases excluded because they hold more than
	// one namespace and are therefore a cross-store index rather than a
	// store. Excluding a database that is really a store would lose data, so
	// the plan prints these with their counts.
	ReplicaNames []string
	// RegistryPath is the file the registry was read from.
	RegistryPath string
	// IncludeDatabases names unregistered databases that hold several prefixes
	// but are stores, not cross-store replicas (a project with two prefixes).
	// Each participates as an ordinary "db:<name>" store instead of being
	// excluded. The operator says so; the tool cannot tell the two apart.
	IncludeDatabases []string
	// RescueOrphansFrom names replica databases (a cross-store index such as
	// beads_global) that stay excluded as stores but from which every bead held
	// by no participating store is brought in, as the store "db:<name>". The
	// replica's other beads are copies of beads that live in their own stores
	// and are not read.
	RescueOrphansFrom []string
}

// ByDatabase returns the stores that read from the named Dolt database.
func (r Registry) ByDatabase(name string) []Store {
	var out []Store
	for _, s := range r.Stores {
		if s.Database == name {
			out = append(out, s)
		}
	}
	return out
}

// LoadRegistry reads the store registry and resolves each store to its Dolt
// database. home is the user's home directory (normally os.UserHomeDir);
// passing it explicitly keeps the resolution testable.
func LoadRegistry(home string) (Registry, error) {
	canonical := filepath.Join(home, registryCanonical)
	legacy := filepath.Join(home, registryLegacy)
	path := canonical
	if _, err := os.Stat(canonical); err != nil && !os.IsNotExist(err) {
		return Registry{}, fmt.Errorf("reading store registry %s: %w", canonical, err)
	} else if os.IsNotExist(err) {
		if _, lerr := os.Stat(legacy); lerr != nil {
			return Registry{}, fmt.Errorf("no brain store registry at %s (legacy %s also absent): %w", canonical, legacy, lerr)
		}
		path = legacy
	}
	raw, err := os.ReadFile(path) //nolint:gosec // operator-owned registry, fixed path
	if err != nil {
		return Registry{}, fmt.Errorf("reading store registry %s: %w", path, err)
	}
	var reg registryFile
	if err := yaml.Unmarshal(raw, &reg); err != nil {
		return Registry{}, fmt.Errorf("parsing store registry %s: %w", path, err)
	}
	names := make([]string, 0, len(reg.Stores))
	for name := range reg.Stores {
		names = append(names, name)
	}
	sort.Strings(names)

	reg2 := Registry{RegistryPath: path}
	for _, name := range names {
		entry := reg.Stores[name]
		beadsDir := resolveBeadsDir(entry.Path)
		store := Store{
			Namespace:  name,
			Registered: true,
			BeadsDir:   beadsDir,
			About:      entry.About,
		}
		applyMetadata(&store, beadsDir)
		store.DeclaredPrefixes = readDeclaredPrefixes(beadsDir)
		reg2.Stores = append(reg2.Stores, store)
	}
	sort.Slice(reg2.Stores, func(i, j int) bool { return reg2.Stores[i].Namespace < reg2.Stores[j].Namespace })
	reg2.RegisteredNames = names
	return reg2, nil
}

// resolveBeadsDir accepts either a path already pointing at the .beads
// directory or a path pointing at the store root that contains one. The
// registry historically wrote both forms (e.g. "resumes" was registered
// without its .beads suffix), so both are resolved.
func resolveBeadsDir(path string) string {
	if path == "" {
		return ""
	}
	expanded := expandHome(path)
	if filepath.Base(expanded) == ".beads" {
		return expanded
	}
	if _, err := os.Stat(filepath.Join(expanded, "metadata.json")); err == nil {
		return expanded
	}
	return filepath.Join(expanded, ".beads")
}

// expandHome expands a leading "~" so registry paths need not be pre-resolved.
func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}

// applyMetadata fills the server coordinates from the store's metadata.json.
// A missing or unreadable file leaves the store with zero-valued coordinates;
// the builder reports those as unusable rather than guessing.
func applyMetadata(store *Store, beadsDir string) {
	store.Host = "127.0.0.1"
	store.Port = 3307
	if beadsDir == "" {
		return
	}
	raw, err := os.ReadFile(filepath.Join(beadsDir, "metadata.json")) //nolint:gosec // operator-owned path
	if err != nil {
		store.Database = defaultDatabaseName(beadsDir)
		return
	}
	var md metadataJSON
	if err := json.Unmarshal(raw, &md); err != nil {
		store.Database = defaultDatabaseName(beadsDir)
		return
	}
	if md.DoltMode == "server" {
		store.ServerMode = true
	}
	if md.DoltServerHost != "" {
		store.Host = md.DoltServerHost
	}
	if md.DoltServerPort != 0 {
		store.Port = md.DoltServerPort
	}
	store.ProjectID = md.ProjectID
	if md.DoltDatabase != "" {
		store.Database = md.DoltDatabase
	} else {
		store.Database = defaultDatabaseName(beadsDir)
	}
}

// defaultDatabaseName is the fallback when metadata.json names no database:
// the directory that contains the .beads directory. That is the layout
// ~/.data/<name>/.beads -> database <name>.
func defaultDatabaseName(beadsDir string) string {
	parent := filepath.Dir(beadsDir)
	return filepath.Base(parent)
}

// readDeclaredPrefixes returns the prefixes a store claims in config.yaml,
// in priority order: issue-prefix first, then BD_NAME. A store may declare
// both and legitimately own both namespaces (the robots store declares
// issue-prefix "robots" while its BD_NAME, and older ids, are "agent").
func readDeclaredPrefixes(beadsDir string) []string {
	if beadsDir == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(beadsDir, "config.yaml")) //nolint:gosec // operator-owned path
	if err != nil {
		return nil
	}
	var doc struct {
		IssuePrefix string `yaml:"issue-prefix"`
		BDName      string `yaml:"BD_NAME"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range []string{doc.IssuePrefix, doc.BDName} {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
