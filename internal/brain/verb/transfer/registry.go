// Package transfer's registry.go owns the store-name ↔ Dolt-database-name
// mapping the verb consults to route cross-store transfers.
//
// # Why a registry lives in this package
//
// `brain transfer <id> <dest>` has to translate three things into raw SQL:
//
//  1. An ID prefix (e.g. "inbox") inferred from the source ID → which store
//     holds the source row.
//  2. A user-supplied dest store name (e.g. "brain", "task") → which Dolt
//     database on the same SQL server holds the destination rows.
//  3. A store name → the prefix it allocates new IDs under.
//
// All three resolutions point at the same in-memory `Registry`. Sticking
// the registry in the transfer package (rather than reaching into
// cmd/bd/brain_stores.go) keeps the verb engine free of any cmd/bd import,
// matching the modularity guarantee for the other brain verbs.
//
// # Sources of truth (in priority order)
//
//  1. The optional `~/.config/pai/stores.yaml` registry + each store's
//     `.beads/metadata.json#dolt_database`. When present, this gives the
//     authoritative store-name → DB-name and store-name → prefix mappings
//     for that user's actual setup.
//  2. The hardcoded built-in fallback below. Used when stores.yaml is
//     missing, unreadable, or does not list a name the user asked for.
//     This matches the canonical 10-store PAI federation documented in
//     the task spec.
//
// The hardcoded fallback is intentionally present even when the yaml
// load succeeds, so that built-in names (e.g. "brain", "task") always
// resolve even if a partial yaml only registers a subset.
//
// # Why the prefix → store map matters
//
// Source-side resolution happens in two steps inside Run: split the ID
// on its first `-` to get the prefix, then look the prefix up in
// `prefixToStore` to find the store name. The store name is what the
// user-visible error wording and result struct use; the DB name is what
// the SQL routes through. Keeping prefix and store-name distinct lets
// the user say `brain transfer inbox-abc task` rather than having to
// know the underlying database name.
package transfer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Registry is the resolved set of stores the verb knows about.
//
// All three maps are populated together — looking up by store name,
// prefix, or just listing all names hits the same in-memory state. A
// `*Registry` is safe for concurrent reads after Load returns (the verb
// never mutates it after construction).
type Registry struct {
	// nameToDB maps a user-visible store name ("brain", "task", "inbox")
	// to the Dolt SQL database name that holds that store's rows
	// ("dolt", "task", "inbox"). The database name is what gets
	// backtick-quoted into `<db>`.`<table>` SQL refs by the verb.
	nameToDB map[string]string

	// prefixToStore maps an ID prefix (the part before the first "-" in
	// any issue ID) to the store name it belongs to. For example,
	// "inbox" → "inbox", "brain" → "brain", "decision" → "decisions".
	// The verb uses this to figure out which database to read the
	// source row from given only the source ID.
	prefixToStore map[string]string

	// nameToPrefix is the inverse view used when generating destination
	// IDs: given the dest store name the user typed, what ID prefix
	// should the new row get? For example, dest "brain" allocates IDs
	// shaped "brain-XXXXX", dest "task" allocates "task-XXXXX".
	nameToPrefix map[string]string
}

// builtinRegistry is the canonical 10-store PAI federation mapping
// documented in the task spec. Used as both the seed for Load and the
// fallback when stores.yaml is missing or partial. The mapping is
// intentionally hardcoded — the federation shape changes rarely, and a
// broken yaml should never lock a user out of transferring between the
// well-known stores.
//
// Each entry has three keys: the store name (what the user types as
// <dest>), the Dolt database name (what goes into the SQL ref), and the
// ID prefix that store allocates (used to route source IDs back to a
// store and to mint destination IDs).
//
//nolint:gochecknoglobals // package-private read-only seed table
var builtinRegistry = []struct {
	Name   string
	DB     string
	Prefix string
}{
	// store "brain" is the knowledge hub. Its Dolt DB happens to be
	// "dolt" for legacy reasons (the first store ever set up was named
	// after the substrate); the prefix matches the store name so
	// destination IDs look like "brain-XXXXX".
	{Name: "brain", DB: "dolt", Prefix: "brain"},
	{Name: "tasks", DB: "task", Prefix: "task"},
	{Name: "projects", DB: "project", Prefix: "project"},
	// "robots" was renamed from "agents" in brain v0.4.0. The Dolt DB
	// and ID prefix stay "agent" so existing bead IDs (agent-XXXXX)
	// keep resolving cleanly. Aliases below preserve the old names.
	{Name: "robots", DB: "agent", Prefix: "agent"},
	{Name: "inbox", DB: "inbox", Prefix: "inbox"},
	{Name: "decisions", DB: "decision", Prefix: "decision"},
	{Name: "ideas", DB: "idea", Prefix: "idea"},
	{Name: "life", DB: "life", Prefix: "life"},
	{Name: "questions", DB: "question", Prefix: "question"},
	{Name: "assert", DB: "assert", Prefix: "assert"},
	{Name: "person", DB: "person", Prefix: "person"},
}

// builtinAliases lets the user type either the canonical store name
// ("tasks", "decisions") or the short / DB-aligned form ("task",
// "decision"). Aliases resolve to the same store. This is purely a
// usability concession — `brain transfer inbox-abc task` should not
// fail just because the canonical name is plural.
//
//nolint:gochecknoglobals // package-private read-only alias table
var builtinAliases = map[string]string{
	"task":      "tasks",
	"project":   "projects",
	"robot":     "robots",
	"agent":     "robots", // legacy: prior name "agents", kept for compat
	"agents":    "robots", // legacy: prior name "agents", kept for compat
	"decision":  "decisions",
	"idea":      "ideas",
	"question":  "questions",
	"assertion": "assert",
	"people":    "person",
}

// validDBName matches a safe Dolt database name. Mirrors the pattern in
// internal/storage/dolt/history.go (validDatabasePattern) so that any
// name we splice into `<db>.<table>` SQL refs has the same guarantees
// as one the production storage layer would accept. Letters, digits,
// underscore, hyphen; must not start with a digit.
//
//nolint:gochecknoglobals // compiled-once regex
var validDBName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_\-]*$`)

// validPrefix matches a safe issue-prefix shape. Prefixes appear in IDs
// like "<prefix>-<hash>", so they must be non-empty and free of "-"
// (the separator). Lowercased alphanumerics + underscore covers every
// real prefix in the builtin registry; we apply this to whatever the
// stores.yaml metadata yields so a malformed prefix can't corrupt ID
// generation later.
//
//nolint:gochecknoglobals // compiled-once regex
var validPrefix = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// storesYAML mirrors the on-disk shape of ~/.config/pai/stores.yaml.
// Only the `stores: name → path` map is consumed; any other keys are
// ignored so a yaml format extension upstream does not break us.
type storesYAML struct {
	Stores map[string]string `yaml:"stores"`
}

// metadataJSON mirrors the subset of .beads/metadata.json we need.
// Other fields (project_id, backend, etc.) are read elsewhere by bd;
// here we only care about the SQL database name and the issue prefix
// (the latter is sometimes available in metadata, more often only in
// the live config table — we treat metadata as optional enrichment).
type metadataJSON struct {
	DoltDatabase string `json:"dolt_database"`
	IssuePrefix  string `json:"issue_prefix,omitempty"`
}

// Load builds a Registry from the hardcoded fallback + any per-user
// overrides discovered on disk.
//
// homeDir is the absolute path to the user's home directory. Pass
// os.UserHomeDir() in production; pass a temp dir in tests. When
// homeDir is empty the yaml/metadata enrichment is skipped entirely
// and the returned Registry is the hardcoded fallback — Load never
// fails for "no yaml present", which is the common case on fresh
// installs.
//
// All yaml/metadata enrichment is best-effort: if stores.yaml is
// unparseable, or a per-store metadata.json is missing or corrupt,
// Load logs nothing (it's a verb path, not a service) and keeps the
// fallback for that entry. A partial registry with the builtin
// fallback intact is more useful than a hard failure on a typo in
// one entry.
func Load(homeDir string) (*Registry, error) {
	r := &Registry{
		nameToDB:      make(map[string]string, len(builtinRegistry)),
		prefixToStore: make(map[string]string, len(builtinRegistry)),
		nameToPrefix:  make(map[string]string, len(builtinRegistry)),
	}

	// Seed the fallback first so even if every later step fails the
	// builtin mapping is intact. Validation of the hardcoded entries
	// is a programming-error guard, not a runtime guard, so the panic
	// rather than a returned error is intentional: a bad entry here
	// means the binary was built wrong.
	for _, e := range builtinRegistry {
		if !validDBName.MatchString(e.DB) {
			return nil, fmt.Errorf("brain transfer: builtin registry has invalid DB name %q (build bug)", e.DB)
		}
		if !validPrefix.MatchString(e.Prefix) {
			return nil, fmt.Errorf("brain transfer: builtin registry has invalid prefix %q (build bug)", e.Prefix)
		}
		r.nameToDB[e.Name] = e.DB
		r.prefixToStore[e.Prefix] = e.Name
		r.nameToPrefix[e.Name] = e.Prefix
	}
	// Aliases share the canonical store's DB / prefix entries. Adding
	// them in a second pass keeps the canonical name authoritative for
	// reverse lookups (DB → name).
	for alias, canonical := range builtinAliases {
		if db, ok := r.nameToDB[canonical]; ok {
			r.nameToDB[alias] = db
		}
		if prefix, ok := r.nameToPrefix[canonical]; ok {
			r.nameToPrefix[alias] = prefix
		}
	}

	if homeDir == "" {
		return r, nil
	}

	// Try the optional ~/.config/pai/stores.yaml registry. Absence is
	// the common case (fresh install); we silently keep the fallback.
	yamlPath := filepath.Join(homeDir, ".config", "pai", "stores.yaml")
	data, err := os.ReadFile(yamlPath) //nolint:gosec // path is constructed from the caller-supplied home dir
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		// A real I/O error reading the yaml: keep the fallback but
		// surface a clear message so the caller can decide whether
		// the partial registry is acceptable for the user's command.
		return r, fmt.Errorf("brain transfer: reading %s: %w", yamlPath, err)
	}
	var doc storesYAML
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return r, fmt.Errorf("brain transfer: parsing %s: %w", yamlPath, err)
	}

	// For each yaml entry, look up the store's .beads/metadata.json
	// to learn its dolt_database name. Empty / unreadable / missing
	// files leave that entry on the fallback row (if any).
	for name, beadsPath := range doc.Stores {
		name = strings.ToLower(strings.TrimSpace(name))
		beadsPath = strings.TrimSpace(beadsPath)
		if name == "" || beadsPath == "" {
			continue
		}
		if !validPrefix.MatchString(name) {
			// Yaml registered a store with a name we cannot use as a
			// prefix shape; skip it but keep the fallback intact.
			continue
		}
		metaPath := filepath.Join(expandHome(beadsPath, homeDir), ".beads", "metadata.json")
		raw, err := os.ReadFile(metaPath) //nolint:gosec // path is constructed from caller-supplied registry
		if err != nil {
			continue
		}
		var meta metadataJSON
		if err := json.Unmarshal(raw, &meta); err != nil {
			continue
		}
		db := strings.TrimSpace(meta.DoltDatabase)
		if db != "" && validDBName.MatchString(db) {
			r.nameToDB[name] = db
		}
		prefix := strings.TrimSpace(meta.IssuePrefix)
		if prefix != "" && validPrefix.MatchString(prefix) {
			r.nameToPrefix[name] = prefix
			r.prefixToStore[prefix] = name
		}
	}

	return r, nil
}

// expandHome resolves a leading "~/" in p against homeDir. Any path
// that does not start with "~/" is returned unchanged. This lets the
// stores.yaml file use "~/data/knowledge" without forcing every reader
// to do their own tilde expansion.
func expandHome(p, homeDir string) string {
	if homeDir == "" {
		return p
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(homeDir, p[2:])
	}
	return p
}

// ResolveSource returns the store name + Dolt DB name for the source
// ID. The source ID's first "-" splits prefix from the rest; the
// prefix is looked up in prefixToStore to find the store name, which
// in turn maps to the DB.
//
// Returns ("", "", error) for any of:
//   - empty id
//   - id with no "-" (no prefix to extract)
//   - prefix not registered (neither in fallback nor yaml)
//
// The error wording matches the spec's required user-visible message
// for ISC-54.
func (r *Registry) ResolveSource(id string) (storeName, dbName string, err error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", fmt.Errorf("brain transfer: source id is required")
	}
	idx := strings.Index(id, "-")
	if idx <= 0 {
		return "", "", fmt.Errorf("brain transfer: source id %q has no prefix (expected <prefix>-<suffix>)", id)
	}
	prefix := id[:idx]
	name, ok := r.prefixToStore[prefix]
	if !ok {
		return "", "", fmt.Errorf("brain transfer: unknown source store for prefix %q-", prefix)
	}
	db, ok := r.nameToDB[name]
	if !ok {
		// Programming error path — prefixToStore had an entry the
		// nameToDB table didn't. Should never happen in production,
		// but surface clearly if it does so it's not silently wrong.
		return "", "", fmt.Errorf("brain transfer: store %q has no database mapping", name)
	}
	return name, db, nil
}

// ResolveDest returns the Dolt DB name + ID prefix for the dest store
// name the user typed. Aliases (e.g. "task" → "tasks") resolve
// transparently.
//
// Returns ("", "", error) when the name is not registered. The error
// wording matches the spec's required user-visible message for ISC-55.
func (r *Registry) ResolveDest(name string) (dbName, prefix string, err error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "", "", fmt.Errorf("brain transfer: destination store name is required")
	}
	db, ok := r.nameToDB[name]
	if !ok {
		return "", "", fmt.Errorf("brain transfer: unknown destination store %q", name)
	}
	prefix, ok = r.nameToPrefix[name]
	if !ok {
		// Same defensive guard as ResolveSource — every entry in
		// nameToDB should have a matching nameToPrefix entry.
		return "", "", fmt.Errorf("brain transfer: store %q has no prefix mapping", name)
	}
	return db, prefix, nil
}

// HubDB returns the Dolt database name for the canonical brain hub
// store. Used by the verb to find the database where supersede edges
// could optionally be mirrored. Always falls back to the builtin
// brain entry ("dolt") even if the yaml didn't list "brain".
//
// Currently the verb writes its supersede edge to the *source* store
// (where the FK fk_dep_issue is satisfiable), not the hub — see the
// package doc on transfer.go for the rationale. HubDB is kept on the
// Registry surface for callers that want to query the hub later.
func (r *Registry) HubDB() string {
	if db, ok := r.nameToDB["brain"]; ok {
		return db
	}
	return "dolt"
}
