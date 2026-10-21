// Package storeregistry is the fleet's one list of stores: a desired-state
// registry kept in the shared (unified) database, with a version counter (the
// epoch) that goes up in the SAME transaction as every create, rename, remove,
// alias or about/edit-back change.
//
// Why it exists. The DATA of every store and the namespace record (names,
// prefixes) already live in the one shared database, so no machine ever lacks
// a store's data. What a machine can lack is only the LOCAL SHIM that points at
// it: the wrapper command ~/.local/bin/<store>, its line in stores.yaml and
// stores.env, and the ~/.pi/agent/bin link. The registry is the list those
// shims are generated from, and the epoch is how a machine learns the list
// changed. State, not events, is what gets read: a machine that slept through
// ten changes converges in one step.
//
// The package is pure data plus SQL. It writes no files and runs no commands;
// the CLI (cmd/bd) turns a registry snapshot into local shims.
package storeregistry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Entry kinds, states and wrapper kinds.
const (
	KindStore = "store"
	KindAlias = "alias"

	StateActive  = "active"
	StateRemoved = "removed"

	// WrapperServer is the shared-server wrapper form (server pins, shared
	// host). WrapperEmbedded is a store that lives in one machine's .beads
	// directory (registered with 'brain stores add'); other machines only get
	// a wrapper for it when that directory exists there.
	WrapperServer   = "server"
	WrapperEmbedded = "embedded"
)

// Operations written to the registry log.
const (
	OpCreate   = "create"
	OpRename   = "rename"
	OpRemove   = "remove"
	OpAlias    = "alias"
	OpAbout    = "about"
	OpEditBack = "edit-back"
	OpRestore  = "restore"
	OpBackfill = "backfill"
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidName reports whether s may be a wrapper/command name. The database
// supplies DATA, never code: a name that fails this check is never written to
// disk, whatever a (poisoned) row says.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// ValidRelPath checks a HOME-relative store directory: clean, under data/, no
// traversal, and none of the characters that could break out of a generated
// shell word.
func ValidRelPath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("empty path")
	case len(p) > 512:
		return fmt.Errorf("path longer than 512 bytes")
	case strings.HasPrefix(p, "/"), strings.HasPrefix(p, "~"):
		return fmt.Errorf("path %q is not HOME-relative", p)
	case strings.ContainsAny(p, "\x00\n\r\"'\\`$;&|<>*?!{} \t"):
		return fmt.Errorf("path %q has a character that is not allowed in a generated wrapper", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("path %q is not clean", p)
		}
	}
	if !strings.HasPrefix(p, "data/") {
		return fmt.Errorf("path %q is not under data/", p)
	}
	return nil
}

// Entry is one row of desired state: a command name the fleet should offer,
// or the tombstone of one it should stop offering.
type Entry struct {
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Store        string `json:"store"` // canonical store (== Name when Kind is "store")
	State        string `json:"state"`
	RelPath      string `json:"rel_path"`
	WrapperKind  string `json:"wrapper_kind"`
	About        string `json:"about"`
	EditBack     bool   `json:"edit_back"`
	CreatedEpoch int64  `json:"created_epoch"`
	ChangedEpoch int64  `json:"changed_epoch"`
}

// Validate refuses an entry that must never reach a machine's disk.
func (e Entry) Validate() error {
	if !ValidName(e.Name) {
		return fmt.Errorf("invalid name %q (want %s)", e.Name, nameRE)
	}
	if e.Kind != KindStore && e.Kind != KindAlias {
		return fmt.Errorf("%s: invalid kind %q", e.Name, e.Kind)
	}
	if e.State != StateActive && e.State != StateRemoved {
		return fmt.Errorf("%s: invalid state %q", e.Name, e.State)
	}
	if e.WrapperKind != WrapperServer && e.WrapperKind != WrapperEmbedded {
		return fmt.Errorf("%s: invalid wrapper kind %q", e.Name, e.WrapperKind)
	}
	if !ValidName(e.Store) {
		return fmt.Errorf("%s: invalid store %q", e.Name, e.Store)
	}
	if e.Kind == KindStore && e.Store != e.Name && e.State == StateActive && e.WrapperKind != WrapperServer {
		return fmt.Errorf("%s: a renamed store must use the server wrapper form", e.Name)
	}
	if err := ValidRelPath(e.RelPath); err != nil {
		return fmt.Errorf("%s: %w", e.Name, err)
	}
	return nil
}

// Line is the canonical text of one entry, the unit the digest hashes.
func (e Entry) Line() string {
	eb := "0"
	if e.EditBack {
		eb = "1"
	}
	return strings.Join([]string{e.Name, e.Kind, e.Store, e.State, e.RelPath, e.WrapperKind, strconv.Quote(e.About), eb}, "|")
}

// Digest is the sha256 over every entry's Line, sorted by name, newline
// separated. Every reader recomputes it from the rows it read, so a torn read
// or a hand edit of the table is detected rather than applied.
func Digest(entries []Entry) string {
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, e.Name+"\x00"+e.Line())
	}
	sort.Strings(lines)
	h := sha256.New()
	for _, l := range lines {
		_, line, _ := strings.Cut(l, "\x00")
		h.Write([]byte(line))
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Active returns the entries that should have a wrapper, sorted by name.
func Active(entries []Entry) []Entry { return filter(entries, StateActive) }

// Removed returns the tombstones, sorted by name.
func Removed(entries []Entry) []Entry { return filter(entries, StateRemoved) }

func filter(entries []Entry, state string) []Entry {
	var out []Entry
	for _, e := range entries {
		if e.State == state {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// State is the registry's version record: the epoch every machine polls.
type State struct {
	Epoch     int64  `json:"epoch"`
	Digest    string `json:"digest"`
	UpdatedAt string `json:"updated_at"`
	UpdatedBy string `json:"updated_by"`
}

// Machine is what one machine reports it has applied: the proof of
// convergence the fleet doctor compares against the database.
type Machine struct {
	Machine       string `json:"machine"`
	Role          string `json:"role"` // direct | client
	Relay         string `json:"relay"`
	AppliedEpoch  int64  `json:"applied_epoch"`
	AppliedDigest string `json:"applied_digest"`
	CheckedAt     string `json:"checked_at"`
	WrappersOK    int    `json:"wrappers_ok"`
	Missing       string `json:"missing"`  // JSON
	Orphans       string `json:"orphans"`  // JSON
	BdVersion     string `json:"bd_version"`
	LastError     string `json:"last_error"`
	Retired       bool   `json:"retired"`
}

// LogRow is one append-only history row.
type LogRow struct {
	Epoch   int64  `json:"epoch"`
	Seq     int    `json:"seq"`
	Op      string `json:"op"`
	Name    string `json:"name"`
	OldName string `json:"old_name"`
	Actor   string `json:"actor"`
	Machine string `json:"machine"`
	At      string `json:"at"`
	Detail  string `json:"detail"`
}
