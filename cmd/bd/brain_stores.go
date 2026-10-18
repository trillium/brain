package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/doltserver"
	"github.com/steveyegge/beads/internal/storage/dolt"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/ui"
	"gopkg.in/yaml.v3"
)

// storesYamlPath is the canonical registry file for brain federation stores.
// De-PAI migration (brain v0.5.0): canonical path moved from
// ~/.config/pai/stores.yaml to ~/.config/brain/stores.yaml. The legacy path
// is kept as a compat symlink (see ensureCompatSymlink) so existing stores
// keep working non-destructively.
const storesYamlPath = ".config/brain/stores.yaml"

// storesYamlLegacyPath is the pre-migration registry location. Read as a
// fallback; written only as a symlink to the canonical path.
const storesYamlLegacyPath = ".config/pai/stores.yaml"

// storesEnvPath is the shell-sourceable export of the registry.
const storesEnvPath = ".config/brain/stores.env"

// storesEnvLegacyPath is the pre-migration env export. Kept as a compat
// symlink to the canonical file.
const storesEnvLegacyPath = ".config/pai/stores.env"

// storeEntry is the registry value for a single federated store. The Path
// points at the store's .beads directory; About is an optional human blurb
// describing what the store is for (set via 'brain stores set-about').
//
// EditBack is the store's edit-back declaration (divergence/0029): when
// true, 'bd render-import' may write edits made to the store's rendered
// markdown files back into the beads they name. It is OFF by default — a
// store that does not declare edit-back keeps the one-way render behaviour,
// with the difference reported rather than applied. Set it with
// 'brain stores edit-back <name> on|off'.
type storeEntry struct {
	Path     string `yaml:"path"`
	About    string `yaml:"about,omitempty"`
	EditBack bool   `yaml:"edit_back,omitempty"`
}

// UnmarshalYAML accepts both the legacy scalar form (value is the bare path
// string) and the current mapping form ({path, about}). This keeps existing
// stores.yaml files — written before the About field existed — readable.
func (s *storeEntry) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		s.Path = value.Value
		s.About = ""
		return nil
	}
	// Alias to avoid infinite recursion into this UnmarshalYAML.
	type rawStoreEntry storeEntry
	var raw rawStoreEntry
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*s = storeEntry(raw)
	return nil
}

// storesRegistry is the on-disk shape of ~/.config/brain/stores.yaml.
type storesRegistry struct {
	Stores map[string]storeEntry `yaml:"stores"`
}

func storesYamlFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, storesYamlPath)
}

func storesYamlLegacyFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, storesYamlLegacyPath)
}

func storesEnvFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, storesEnvPath)
}

func storesEnvLegacyFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, storesEnvLegacyPath)
}

// ensureCompatSymlink points legacyPath at canonicalPath via a relative
// symlink. Non-destructive by design:
//   - legacy missing → create symlink.
//   - legacy already the correct symlink → no-op.
//   - legacy is a regular file/dir → leave untouched (a prior version's
//     data); the caller has already migrated its contents by reading the
//     legacy path first and writing the canonical path.
//
// Returns nil in all non-fatal cases so migration never blocks a store write.
//
//nolint:unparam // always-nil error keeps call sites uniform with other I/O helpers
func ensureCompatSymlink(canonicalPath, legacyPath string) error {
	if canonicalPath == "" || legacyPath == "" || canonicalPath == legacyPath {
		return nil
	}
	if _, err := os.Lstat(legacyPath); err != nil {
		if !os.IsNotExist(err) {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
			return nil
		}
		rel, err := filepath.Rel(filepath.Dir(legacyPath), canonicalPath)
		if err != nil {
			rel = canonicalPath
		}
		_ = os.Symlink(rel, legacyPath)
		return nil
	}
	// Legacy exists: if it is already a symlink, verify/fix its target.
	if fi, err := os.Lstat(legacyPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if target, err := os.Readlink(legacyPath); err == nil {
			resolved := target
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(filepath.Dir(legacyPath), target)
			}
			if filepath.Clean(resolved) == filepath.Clean(canonicalPath) {
				return nil
			}
		}
		// Stale symlink pointing elsewhere: leave it; operator intent wins.
		return nil
	}
	// Regular file/dir at legacy path: leave in place for compat reads.
	return nil
}

// migrateLegacyFileToSymlink converges the legacy path to a symlink once the
// canonical file holds the merged state. Called after each save, so the
// transition always ends at: canonical file + legacy symlink. Safe in every
// case because both callers (saveStoresRegistry, regenerateStoresEnv) write
// the canonical file from content loaded through the legacy fallback first:
// every byte of a legacy regular file is already present in the canonical
// file before the legacy path is replaced, so no data is ever lost.
// A legacy directory (never a file the migration wrote) is left untouched.
func migrateLegacyFileToSymlink(canonicalPath, legacyPath string) {
	if canonicalPath == "" || legacyPath == "" {
		return
	}
	fi, err := os.Lstat(legacyPath)
	if err != nil {
		// Legacy missing → ensure symlink for future compat reads.
		_ = ensureCompatSymlink(canonicalPath, legacyPath)
		return
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		_ = ensureCompatSymlink(canonicalPath, legacyPath)
		return
	}
	if !fi.Mode().IsRegular() {
		return
	}
	if _, err := os.Stat(canonicalPath); err != nil {
		// Canonical absent but legacy is a regular file (e.g. a save that
		// failed between load and write): seed canonical from legacy so
		// the legacy content survives, then converge below.
		data, err := os.ReadFile(legacyPath) //nolint:gosec
		if err != nil {
			return
		}
		if err := os.MkdirAll(filepath.Dir(canonicalPath), 0o755); err != nil {
			return
		}
		if err := os.WriteFile(canonicalPath, data, 0o644); err != nil { //nolint:gosec
			return
		}
	}
	// Both paths are regular files at this point. The canonical file was
	// written from merged state that already includes the legacy content
	// (loads read the legacy fallback), so replacing the stale legacy file
	// with a symlink loses nothing and keeps old-path readers current.
	// The remove+symlink is best-effort: a failure leaves the legacy file
	// readable and the canonical file authoritative.
	if err := os.Remove(legacyPath); err != nil {
		return
	}
	_ = ensureCompatSymlink(canonicalPath, legacyPath)
}

// loadStoresRegistry reads the canonical registry file union-merged with the
// legacy fallback (canonical wins on key conflict). Merging — rather than
// first-file-wins — guarantees a diverged legacy regular file can never lose
// entries silently: every load-then-save cycle persists the union, so the
// post-save symlink convergence in migrateLegacyFileToSymlink is lossless.
// readStoresFile reads one registry file. found=false means the file is
// absent (not an error). A present-but-unreadable or unparseable file
// returns found=true with the error, so callers can decide whether the
// other file's healthy state is enough to proceed.
func readStoresFile(path string) (stores map[string]storeEntry, found bool, err error) {
	if path == "" {
		return nil, false, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, fmt.Errorf("reading %s: %w", path, err)
	}
	var reg storesRegistry
	if err := yaml.Unmarshal(data, &reg); err != nil {
		return nil, true, fmt.Errorf("parsing %s: %w", path, err)
	}
	if reg.Stores == nil {
		return make(map[string]storeEntry), true, nil
	}
	return reg.Stores, true, nil
}

func loadStoresRegistry() (map[string]storeEntry, error) {
	merged, err := loadRawStoresRegistry()
	if err != nil {
		return nil, err
	}
	healRegistryPaths(merged)
	return merged, nil
}

// loadRawStoresRegistry reads the registry exactly as recorded on disk: every
// path is the literal value the last writer saved, with no machine-local
// healing. Only loadStoresRegistry (and the callers that must persist what
// they read) should use this.
func loadRawStoresRegistry() (map[string]storeEntry, error) {
	merged := make(map[string]storeEntry)
	// Legacy first so canonical entries overwrite on conflict. The legacy
	// read is best-effort: a corrupt/unreadable legacy file must never break
	// reads while the canonical file is healthy (otherwise a damaged legacy
	// copy would wedge every load-then-save command in a stuck state, since
	// load fails before save can converge it). A corrupt legacy with a
	// missing canonical stays fatal, so a later save cannot persist an
	// empty registry over unknown state.
	legacy, _, legacyErr := readStoresFile(storesYamlLegacyFile())
	canonical, canonicalFound, err := readStoresFile(storesYamlFile())
	if err != nil {
		return nil, err
	}
	if !canonicalFound && legacyErr != nil {
		return nil, legacyErr
	}
	for name, entry := range legacy {
		merged[name] = entry
	}
	for name, entry := range canonical {
		merged[name] = entry
	}
	return merged, nil
}

// healRegistryPaths resolves every entry whose recorded path describes another
// machine's home to this machine's equivalent (see resolveStorePath) and
// returns the healed names, sorted, for reporting.
//
// The registry is written by whichever machine last ran a 'brain stores' write,
// so it keeps describing the home it was written in — the live mini1 registry
// still lists /Users/<other>/data/<store>/.beads for every store while the
// wrappers (made portable by 'brain stores rewrite-wrappers') resolve this
// machine's $HOME copies. That mismatch is why a whole federation can read as
// 'registered but does not answer a read': the store answers, the registration
// points somewhere else. Healing is a resolution, never a rewrite: an entry
// whose recorded path exists is left exactly as recorded.
func healRegistryPaths(stores map[string]storeEntry) []string {
	var healed []string
	for _, name := range sortedKeys(stores) {
		entry := stores[name]
		p, ok := resolveStorePath(entry.Path)
		if !ok {
			continue
		}
		entry.Path = p
		stores[name] = entry
		healed = append(healed, name)
	}
	return healed
}

// persistHealedRegistryPaths rewrites the registry file when any entry's
// recorded path describes another machine's home while this machine's
// equivalent exists (see resolveStorePath), and returns the healed names. It is
// the write half of the healing loadStoresRegistry does in memory: a registry
// carried to another machine (or another home) keeps working immediately, and
// the file stops describing paths that do not exist here the next time a
// federation verb runs. Nothing is written when no entry heals, so a registry
// whose paths are all real is untouched byte for byte.
func persistHealedRegistryPaths() ([]string, error) {
	recorded, err := loadRawStoresRegistry()
	if err != nil {
		return nil, err
	}
	healed := healRegistryPaths(recorded)
	if len(healed) == 0 {
		return nil, nil
	}
	if err := saveStoresRegistry(recorded); err != nil {
		return nil, err
	}
	return healed, nil
}

func saveStoresRegistry(stores map[string]storeEntry) error {
	path := storesYamlFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	reg := storesRegistry{Stores: stores}
	data, err := yaml.Marshal(reg)
	if err != nil {
		return fmt.Errorf("marshaling stores: %w", err)
	}
	header := "# brain federation store registry — managed by 'brain stores'\n# Do not edit manually; use 'brain stores add/remove'.\n# Legacy path ~/.config/pai/stores.yaml is a compat symlink to this file.\n\n"
	if err := os.WriteFile(path, append([]byte(header), data...), 0o644); err != nil { //nolint:gosec
		return fmt.Errorf("writing %s: %w", path, err)
	}
	migrateLegacyFileToSymlink(path, storesYamlLegacyFile())
	return nil
}

var brainStoresCmd = &cobra.Command{
	Use:   "stores",
	Short: "Manage the brain federation store registry",
	Long: `Manage the registry of bd stores federated under brain.

The registry lives at ~/.config/brain/stores.yaml (legacy path
~/.config/pai/stores.yaml is a compat symlink kept for transition).
Each registered store
can be searched via 'brain search', transferred to via 'brain transfer',
and synced via 'brain repo sync'.

Run 'brain stores env' to regenerate ~/.config/brain/stores.env for
shell wrapper scripts that need BRAIN_STORE_* variables (deprecated
PAI_STORE_* aliases are still emitted for transition).`,
	Run: func(cmd *cobra.Command, args []string) {
		_ = cmd.Help()
	},
}

var brainStoresAddCmd = &cobra.Command{
	Use:   "add <name> <beads-dir>",
	Short: "Register a store in the brain federation registry",
	Args:  cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		name := strings.ToLower(strings.TrimSpace(args[0]))
		beadsDir := expandPath(args[1])

		if name == "" || strings.ContainsAny(name, " ./\\") {
			FatalError("store name must be a simple identifier (no spaces, slashes, or dots): %q", name)
		}

		stores, err := loadStoresRegistry()
		if err != nil {
			FatalError("loading registry: %v", err)
		}
		entry := stores[name] // preserve any existing about blurb on re-add
		entry.Path = beadsDir
		stores[name] = entry
		if err := saveStoresRegistry(stores); err != nil {
			FatalError("saving registry: %v", err)
		}

		if jsonOutput {
			outputJSON(map[string]string{"name": name, "path": beadsDir})
			return
		}
		fmt.Printf("%s Registered store %q → %s\n", ui.RenderPass("✓"), name, beadsDir)
	},
}

var brainStoresRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Unregister a store from the brain federation registry",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		name := strings.ToLower(strings.TrimSpace(args[0]))

		stores, err := loadStoresRegistry()
		if err != nil {
			FatalError("loading registry: %v", err)
		}
		if _, ok := stores[name]; !ok {
			FatalError("store %q is not registered", name)
		}
		delete(stores, name)
		if err := saveStoresRegistry(stores); err != nil {
			FatalError("saving registry: %v", err)
		}
		fmt.Printf("%s Removed store %q\n", ui.RenderPass("✓"), name)
	},
}

var storesListVerbose bool

var brainStoresListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all registered stores",
	Run: func(cmd *cobra.Command, args []string) {
		stores, err := loadStoresRegistry()
		if err != nil {
			FatalError("loading registry: %v", err)
		}
		if len(stores) == 0 {
			fmt.Println("No stores registered. Use 'brain stores add <name> <beads-dir>'.")
			return
		}

		names := sortedKeys(stores)

		if jsonOutput {
			result := make([]map[string]any, 0, len(names))
			for _, n := range names {
				result = append(result, map[string]any{
					"name":      n,
					"path":      stores[n].Path,
					"about":     stores[n].About,
					"edit_back": stores[n].EditBack,
				})
			}
			outputJSON(result)
			return
		}
		for _, n := range names {
			if storesListVerbose {
				about := stores[n].About
				if about == "" {
					about = "—"
				}
				mode := "one-way"
				if stores[n].EditBack {
					mode = "edit-back"
				}
				fmt.Printf("  %-16s %-48s %-9s %s\n", n, stores[n].Path, mode, about)
			} else if stores[n].EditBack {
				fmt.Printf("  %-16s %s (edit-back)\n", n, stores[n].Path)
			} else {
				fmt.Printf("  %-16s %s\n", n, stores[n].Path)
			}
		}
	},
}

var brainStoresEditBackCmd = &cobra.Command{
	Use:   "edit-back <name> on|off",
	Short: "Declare whether a store accepts edits from its rendered markdown files",
	Long: `Declare edit-back for a store in ~/.config/brain/stores.yaml.

Edit-back is per store and OFF by default. When a store accepts edit-back,
'bd render-import' may write an edit made to one of the store's rendered
markdown files (entries/<kind>/<slug>.md) back into the bead the file names.
When a store does not declare it, its render stays one-way: 'bd
render-import' running there reports the file-vs-row differences and writes
nothing.

The substrate stays the authority either way: a render-import run applies
only the fields the import defines (title, status, priority, labels,
description), reports every change as old → new, and deletes nothing.
Dealing with the files' own deletions is 'bd render-marks' — a rendered
file that disappears marks its bead for deletion, never removes it.

  bd stores edit-back brain on
  bd stores edit-back task off
  bd stores list --verbose`,
	Args: cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		name := strings.ToLower(strings.TrimSpace(args[0]))
		option := strings.TrimSpace(args[1])
		if option != "on" && option != "off" {
			FatalError("edit-back takes on or off, not %q", option)
		}

		stores, err := loadStoresRegistry()
		if err != nil {
			FatalError("loading registry: %v", err)
		}
		if _, ok := stores[name]; !ok {
			FatalError("store %q is not registered — 'brain stores add %s <beads-dir>' first", name, name)
		}
		entry := stores[name]
		entry.EditBack = option == "on"
		stores[name] = entry
		if err := saveStoresRegistry(stores); err != nil {
			FatalError("saving registry: %v", err)
		}

		if jsonOutput {
			outputJSON(map[string]any{"name": name, "edit_back": entry.EditBack})
			return
		}
		fmt.Printf("%s store %s: edit-back %s\n", ui.RenderPass("✓"), name, option)
	},
}

var (
	createStorePath     string
	createStoreNoWrap   bool
	createStoreBinary   string
	createStoreInitName string
	createStoreInitMail string
	createStoreEmbedded bool
)

var brainStoresCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Provision a new connected store (dolt init + entries dir + wrapper + registry)",
	Long: `Provision a new connected store end-to-end. Does, in order:

  1. Provision the Dolt store. The store is provisioned on the federation's
     shared Dolt sql-server whenever one can be found and proven to answer:
     the shared-server pins the caller's wrapper exported (BEADS_DOLT_*)
     first, else the server the registered stores' metadata.json already
     resolves to, else 127.0.0.1:3307. A candidate that does not answer is a
     loud refusal, never a store silently pinned to a host that cannot be
     read — and never a silent fallback to an embedded store. Pass --embedded
     to ask for the embedded shape explicitly; then metadata.json
     (dolt_mode=server) and config.yaml are written first, and
     'dolt init' runs in <path>/.beads.
  2. Write <path>/.beads/config.yaml (issue-prefix + BD_NAME), in either
     shape.
  3. Create <path>/entries/ for exfiltrated markdown.
  4. Write a CLI wrapper at ~/.local/bin/<name>. On a shared server the
     wrapper exports the four server pins (mirroring the review/brain
     wrappers); embedded stores get the minimal BEADS_DIR/BD_NAME wrapper.
     Either way it exec's bd.
  5. Register the store in ~/.config/brain/stores.yaml.
  6. Regenerate ~/.config/brain/stores.env.

Default path is $HOME/data/<name>. Override with --path. Skip the wrapper
with --no-wrapper if you manage shell shims another way.

If any step fails after files have been written, the verb leaves the
partial state in place and exits non-zero — re-running with the same
arguments resumes idempotently. An abandoned server-side store must be
dropped by hand on the shared server; the verb never drops a database.

Examples:
  brain stores create recipes
  brain stores create recipes --path /Volumes/extra/recipes
  brain stores create recipes --bd-binary /opt/homebrew/bin/bd
  brain stores create scratch --embedded`,
	Args: cobra.ExactArgs(1),
	Run:  runBrainStoresCreate,
}

func runBrainStoresCreate(_ *cobra.Command, args []string) {
	name := strings.ToLower(strings.TrimSpace(args[0]))
	if name == "" || strings.ContainsAny(name, " ./\\") {
		FatalError("store name must be a simple identifier (no spaces, slashes, or dots): %q", name)
	}

	path := createStorePath
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			FatalError("resolving home dir: %v", err)
		}
		path = filepath.Join(home, "data", name)
	}
	path = expandPath(path)
	beadsDir := filepath.Join(path, ".beads")
	entriesDir := filepath.Join(path, "entries")

	// Step 1: decide WHERE the store is provisioned, then provision it. A
	// store written against a Dolt server that does not answer is registered
	// but unreadable for as long as nobody notices (robots-1nri, robots-1ypiy,
	// task-d3cz, task-8m5x), so every candidate server must answer before it
	// is written into the new store's metadata.json and wrapper, and the
	// embedded (auto-starting) shape is only reachable by asking for it.
	stores, err := loadStoresRegistry()
	if err != nil {
		FatalError("loading registry: %v", err)
	}

	ctx := context.Background()
	target, terr := resolveCreateServer(ctx, probeUnifiedDatabaseOnServer)
	if terr != nil {
		FatalError("%v", terr)
	}

	embedded := target.Source == createServerEmbedded
	provisioned := ""
	if embedded {
		fmt.Fprintf(os.Stderr,
			"%s No shared Dolt sql-server answered, so %q is being provisioned as an EMBEDDED store at %s.\n"+
				"    Its reads auto-start a private local server instead of reaching the federation's.\n",
			ui.RenderWarn("⚠"), name, beadsDir)
		if err := initDoltStore(beadsDir); err != nil {
			FatalError("dolt init at %s: %v", beadsDir, err)
		}
	} else {
		pdb, perr := provisionServerStore(ctx, name, beadsDir, target.Host, target.Port)
		if perr != nil {
			FatalError("provisioning server store at %s: %v", beadsDir, perr)
		}
		provisioned = pdb
	}

	// config.yaml is part of a working store in either shape: it carries the
	// issue prefix and the store's BD_NAME, and 'brain stores create' used to
	// leave it out on the path that did not go through the Dolt sql-server, so
	// the store needed a hand-written config.yaml before any command worked
	// (task-d3cz).
	if err := writeStoreConfigYaml(beadsDir, name); err != nil {
		FatalError("writing config.yaml at %s: %v", beadsDir, err)
	}

	// Step 2: entries/ sibling for exfiltration.
	if err := os.MkdirAll(entriesDir, 0o755); err != nil {
		FatalError("creating %s: %v", entriesDir, err)
	}

	// Step 3: wrapper at ~/.local/bin/<name>. Shared-server stores get the four
	// server pins so bd resolves in server mode; embedded stores get the minimal
	// BEADS_DIR/BD_NAME wrapper.
	wrapperPath := ""
	if !createStoreNoWrap {
		var wp string
		var werr error
		if !embedded {
			wp, werr = writeServerStoreWrapper(name, beadsDir, createStoreBinary, target.Host, target.Port, provisioned)
		} else {
			wp, werr = writeStoreWrapper(name, beadsDir, createStoreBinary)
		}
		if werr != nil {
			FatalError("writing wrapper: %v", werr)
		}
		wrapperPath = wp
	}

	// Step 4: register in stores.yaml.
	entry := stores[name] // preserve any existing about blurb on re-run
	entry.Path = beadsDir
	stores[name] = entry
	if err := saveStoresRegistry(stores); err != nil {
		FatalError("saving registry: %v", err)
	}

	// Step 5: regenerate stores.env.
	envPath, err := regenerateStoresEnv(stores)
	if err != nil {
		FatalError("regenerating stores.env: %v", err)
	}

	if jsonOutput {
		payload := map[string]string{
			"name":        name,
			"path":        path,
			"beads_dir":   beadsDir,
			"entries_dir": entriesDir,
			"wrapper":     wrapperPath,
			"registry":    storesYamlFile(),
			"env_file":    envPath,
			"server":      target.describe(),
		}
		outputJSON(payload)
		return
	}

	fmt.Printf("%s Created store %q\n", ui.RenderPass("✓"), name)
	fmt.Printf("    beads:    %s\n", beadsDir)
	fmt.Printf("    entries:  %s\n", entriesDir)
	if wrapperPath != "" {
		fmt.Printf("    wrapper:  %s\n", wrapperPath)
	}
	fmt.Printf("    server:   %s\n", target.describe())
	fmt.Printf("    registry: %s\n", storesYamlFile())
	fmt.Printf("    env:      %s\n", envPath)
}

// initDoltStore creates beadsDir and runs `dolt init` inside it. Idempotent:
// if beadsDir already contains an initialized Dolt repo, this returns nil.
func initDoltStore(beadsDir string) error {
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", beadsDir, err)
	}
	if _, err := os.Stat(filepath.Join(beadsDir, ".dolt")); err == nil {
		return nil // already initialized
	}

	dolt, err := exec.LookPath("dolt")
	if err != nil {
		return fmt.Errorf("dolt binary not found in PATH (install dolt or run 'brain stores create --no-wrapper' after manual init): %w", err)
	}

	args := []string{"init"}
	if createStoreInitName != "" {
		args = append(args, "--name", createStoreInitName)
	}
	if createStoreInitMail != "" {
		args = append(args, "--email", createStoreInitMail)
	}
	cmd := exec.Command(dolt, args...)
	cmd.Dir = beadsDir
	cmd.Stdout = os.Stderr // dolt prints to stdout; route to stderr so JSON mode stdout stays clean
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("dolt init: %w", err)
	}
	return nil
}

// Where a newly provisioned store's server choice came from. Surfaced in the
// verb's output so an operator can see the federation's one config source was
// followed rather than guessed at.
const (
	createServerEnv      = "env"
	createServerFile     = "file"
	createServerEmbedded = "embedded"
)

// createServerTarget names the Dolt sql-server a new store is provisioned on,
// and where that choice came from. Source == createServerEmbedded means the
// operator asked for the embedded shape with --embedded.
type createServerTarget struct {
	Host   string
	Port   int
	Source string
}

// describe renders the target for the verb's output.
func (t createServerTarget) describe() string {
	switch t.Source {
	case createServerEmbedded:
		return "embedded (--embedded: no shared server is used)"
	case createServerEnv:
		return fmt.Sprintf("%s:%d (BEADS_DOLT_SERVER_HOST/PORT in the environment)", t.Host, t.Port)
	default:
		return fmt.Sprintf("%s:%d (%s)", t.Host, t.Port, configfile.DefaultStoreHostPath())
	}
}

// serverProbe proves a candidate server answers and reports the unified
// database it hosts. probeUnifiedDatabaseOnServer is the real one; it is a
// parameter so the choice can be tested without a live server.
type serverProbe func(ctx context.Context, host string, port int) (string, bool, error)

func probeCreateServer(ctx context.Context, probe serverProbe, host string, port int) error {
	if probe == nil {
		probe = probeUnifiedDatabaseOnServer
	}
	_, _, err := probe(ctx, host, port)
	return err
}

// resolveCreateServer decides which Dolt sql-server a new store is provisioned
// on. The host is the federation's one per-machine fact, so it comes from the
// shared store host config (~/.config/brain/store-host.env) or, overriding it,
// from BEADS_DOLT_SERVER_HOST/PORT in the process environment — never from a
// per-store default and never from the asker's imagination. A missing file is a
// loud refusal naming the file: silently provisioning against 127.0.0.1 is how
// a store ends up registered but unreadable (robots-1nri, robots-1ypiy,
// task-d3cz, task-8m5x). The server must answer before the store is written
// against it, and the embedded (auto-starting) shape is only reachable by
// asking for it with --embedded.
func resolveCreateServer(ctx context.Context, probe serverProbe) (createServerTarget, error) {
	if createStoreEmbedded {
		return createServerTarget{Source: createServerEmbedded}, nil
	}

	sh, err := configfile.Resolve()
	if err != nil {
		path := configfile.DefaultStoreHostPath()
		if path == "" {
			path = "~/.config/brain/" + configfile.StoreHostFileName
		}
		return createServerTarget{}, fmt.Errorf(
			"no shared Dolt sql-server is configured for this machine: %v\n"+
				"Write it once as\n  %s=<host>\n  %s=<port>\nin %s (or export the same two variables), then re-run. Pass --embedded only for a store that should run its own local Dolt.",
			err, configfile.StoreHostKeyHost, configfile.StoreHostKeyPort, path)
	}

	if perr := probeCreateServer(ctx, probe, sh.Host, sh.Port); perr != nil {
		return createServerTarget{}, fmt.Errorf(
			"the shared Dolt sql-server %s:%d did not answer: %v\n"+
				"Refusing to create a store whose every read would fail. Start that server or repoint %s, then re-run.",
			sh.Host, sh.Port, perr, configfile.DefaultStoreHostPath())
	}

	source := createServerFile
	if sh.Source == "env" {
		source = createServerEnv
	}
	return createServerTarget{Host: sh.Host, Port: sh.Port, Source: source}, nil
}

// buildServerStoreConfig constructs the metadata.json contents for a new
// server-mode store: dolt_mode=server pinned to the shared sql-server, the SQL
// database name equal to the store name, and a caller-supplied project identity.
// Pure (no I/O) so it can be unit-tested without a live server.
func buildServerStoreConfig(name, host string, port int, projectID string) *configfile.Config {
	cfg := configfile.DefaultConfig()
	cfg.Database = "dolt"
	cfg.Backend = configfile.BackendDolt
	cfg.DoltMode = configfile.DoltModeServer
	cfg.DoltServerHost = host
	cfg.DoltServerPort = port
	cfg.DoltDatabase = name
	cfg.GlobalDoltDatabase = doltserver.GlobalDatabaseName
	cfg.GlobalProjectID = doltserver.GlobalProjectID
	cfg.ProjectID = projectID
	return cfg
}

// provisionServerStore creates a connected store on the running shared Dolt
// sql-server, and returns the Dolt database it was provisioned into (the
// unified database's name when the server hosts one, "" for the legacy
// per-store database shape). On the unified database the new store is a
// NAMESPACE of the one database — the post-cutover shape: a separate per-store
// database would be a bead home the unified database's reads never see.
//
// Order is load-bearing:
//
//  1. Write metadata.json (server mode) + config.yaml FIRST, so the store open
//     below resolves THIS store's own .beads dir. Without a metadata.json here,
//     bd walks up and mis-resolves a parent .beads (e.g. ~/data/.beads) — the
//     ancestor-discovery bug that left every 'brain stores create' store DOA.
//  2. Open the store: with CreateIfMissing on a legacy server (bd runs CREATE
//     DATABASE on the shared server and initializes the beads schema, reusing
//     bd's own verified provisioning path); without it on a unified server
//     (the database already exists — the store pins its namespace onto it).
//  3. Persist issue_prefix and _project_id into the store's namespace (scoped
//     writes: BD_NAME is pinned for the open), so create/list/ready work and
//     the metadata.json↔namespace identity matches.
//  4. Record the store and its name prefix in the namespace record
//     (brain_stores / brain_store_prefixes, reason "store-created"), so the
//     ownership record stays the one place "which store is this bead in" is
//     answered from, and a name that collides with another store's recorded
//     prefix refuses loudly instead of slipping in silently.
//
// Idempotent: on re-run it preserves the existing project identity (minting a
// new one would break the metadata.json↔database identity check on later opens).
func provisionServerStore(ctx context.Context, name, beadsDir, host string, port int) (string, error) {
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", beadsDir, err)
	}

	// Preserve an existing project identity across idempotent re-runs.
	projectID := ""
	if existing, err := configfile.Load(beadsDir); err == nil && existing != nil {
		projectID = existing.ProjectID
	}
	if projectID == "" {
		projectID = configfile.GenerateProjectID()
	}

	// Which shape does this server host? A probe failure fails loudly rather
	// than silently picking the wrong home for the store.
	unifiedDatabase, unified, err := probeUnifiedDatabaseOnServer(ctx, host, port)
	if err != nil {
		return "", fmt.Errorf("probing the shared server for the unified database: %w", err)
	}

	cfg := buildServerStoreConfig(name, host, port, projectID)
	if unified {
		cfg.DoltDatabase = unifiedDatabase
	}
	if err := cfg.Save(beadsDir); err != nil {
		return "", fmt.Errorf("writing metadata.json: %w", err)
	}
	if err := writeStoreConfigYaml(beadsDir, name); err != nil {
		return "", fmt.Errorf("writing config.yaml: %w", err)
	}

	// The unified shape seeds the NEW store's namespace rows directly through
	// a raw connection, BEFORE the id-verified open: the open itself reads the
	// namespace identity for verification, and this process may already hold a
	// cached scope for the caller's own namespace (same database), so bd
	// handle writes here could land in the caller's namespace rather than the
	// new store's. The one seeded transaction below is deliberately
	// namespace-named (store column) and carries the issue_prefix, the project
	// identity, the brain_stores row and the name-prefix claim — a prefix
	// collision with another store's recorded prefix refuses before anything
	// is half-created.
	if unified {
		if err := seedNamespaceRows(ctx, host, port, unifiedDatabase, name, projectID); err != nil {
			return unifiedDatabase, err
		}
	}

	// BD_NAME is pinned for the open like a store wrapper would, so any probe
	// the open performs resolves to the NEW namespace — and the cached scope
	// from the caller's own open (same database, its namespace) is dropped, or
	// the id-verified open would read the caller's identity row instead of
	// the new store's.
	restoreBDName, hadBDName := os.LookupEnv("BD_NAME")
	_ = os.Setenv("BD_NAME", name)
	issueops.ResetUnifiedScopeCache()
	defer func() {
		if hadBDName {
			_ = os.Setenv("BD_NAME", restoreBDName)
		} else {
			_ = os.Unsetenv("BD_NAME")
		}
	}()

	store, err := dolt.NewFromConfigWithOptions(ctx, beadsDir, &dolt.Config{CreateIfMissing: !unified})
	if err != nil {
		return "", fmt.Errorf("opening store (provision on shared server): %w", err)
	}
	defer func() { _ = store.Close() }()

	if !unified {
		// Legacy per-store database: the plain config/metadata tables ARE this
		// store's own keyspace, so bd's own writes are right.
		if err := store.SetConfig(ctx, "issue_prefix", name); err != nil {
			return "", fmt.Errorf("setting issue_prefix in database: %w", err)
		}
		if err := store.SetMetadata(ctx, "_project_id", projectID); err != nil {
			return "", fmt.Errorf("writing project identity to database: %w", err)
		}
	}
	return unifiedDatabase, nil
}

// probeUnifiedDatabaseOnServer answers the name of the database on the shared
// server that carries brain_unified_config (the unified shape's marker table),
// and whether one exists at all. No such table = the server is not unified:
// legacy provisioning applies.
func probeUnifiedDatabaseOnServer(ctx context.Context, host string, port int) (string, bool, error) {
	if port == 0 {
		return "", false, fmt.Errorf("no shared server port resolved")
	}
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(%s:%d)/", host, port)) //nolint:gosec,noctx // shared-server convention, bounded below
	if err != nil {
		return "", false, fmt.Errorf("opening probe connection to %s:%d: %w", host, port, err)
	}
	defer func() { _ = db.Close() }()
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(probeCtx); err != nil {
		return "", false, fmt.Errorf("the shared server at %s:%d did not answer: %w", host, port, err)
	}
	var schema string
	err = db.QueryRowContext(probeCtx,
		"SELECT table_schema FROM information_schema.tables WHERE table_name = 'brain_unified_config' LIMIT 1").Scan(&schema)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("probing for the unified database: %w", err)
	}
	return schema, true, nil
}

// seedNamespaceRows writes the new store's rows on the unified database in
// one transaction, through a direct connection whose default schema is the
// unified database itself: the namespace's issue_prefix, its project identity
// in the re-keyed metadata, the brain_stores register row, and the name-prefix
// claim (reason "store-created"; a collision with another store's recorded
// prefix rolls the whole seed back loudly).
func seedNamespaceRows(ctx context.Context, host string, port int, unifiedDatabase, name, projectID string) error {
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(%s:%d)/%s", host, port, unifiedDatabase)) //nolint:gosec,noctx // shared-server convention; every statement is inside one bounded tx
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", unifiedDatabase, err)
	}
	defer func() { _ = db.Close() }()
	seedCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := db.BeginTx(seedCtx, nil)
	if err != nil {
		return fmt.Errorf("open transaction for the %s namespace seed: %w", name, err)
	}
	if _, err := issueops.RecordStorePrefix(seedCtx, tx, name, name, reasonStoreCreated); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("recording the name prefix for %q: %w", name, err)
	}
	if _, err := tx.ExecContext(seedCtx,
		"REPLACE INTO brain_unified_config (`store`,`key`,value) VALUES (?, 'issue_prefix', ?)", name, name); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("seeding issue_prefix for %q: %w", name, err)
	}
	if _, err := tx.ExecContext(seedCtx,
		"REPLACE INTO brain_unified_metadata (`store`,`key`,value) VALUES (?, '_project_id', ?)", name, projectID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("seeding project identity for %q: %w", name, err)
	}
	if _, err := tx.ExecContext(seedCtx,
		"REPLACE INTO brain_stores (`store`,`registered`,`source_database`,`beads_dir`,`project_id`,`schema_version`,`template_source`,`imported_at`) VALUES (?, 1, ?, '', ?, '', 0, ?)",
		name, unifiedDatabase, projectID, time.Now().UTC().Format("2006-01-02 15:04:05")); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("recording store %q in brain_stores: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing the %q namespace seed: %w", name, err)
	}
	return nil
}

// reasonStoreCreated is the owner_reason a store's own name prefix carries
// when the prefix came from 'brain stores create' rather than a later
// 'bd store-prefix add'.
const reasonStoreCreated = "store-created"

// writeStoreConfigYaml writes the minimal config.yaml every connected store
// carries: issue-prefix and BD_NAME, both equal to the store name (matching the
// shape of existing stores like review/tasks). Idempotent — an existing file is
// left untouched so a re-run never clobbers hand edits.
func writeStoreConfigYaml(beadsDir, name string) error {
	path := filepath.Join(beadsDir, "config.yaml")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	body := fmt.Sprintf(`# Beads configuration — connected store %q on the shared Dolt sql-server.
# Managed by 'brain stores create'; the wrapper pins server-mode env.
issue-prefix: %q
BD_NAME: %q
`, name, name, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// sharedServerHostBlock is the runtime host resolution every generated
// shared-server store wrapper carries. It deliberately does NOT bake a Dolt
// host or port: the federation's server is one fact per machine, kept in the
// shared store host config (~/.config/brain/store-host.env), and it is read
// here at run time. Repointing a machine — or moving the whole federation — is
// then one file edit, and no wrapper can disagree with another about where the
// store lives. A process env value of either key overrides the file, and a
// missing file is a loud refusal that names it rather than a silent 127.0.0.1.
//
// The literal env var and file names here must stay in sync with
// configfile.StoreHostFileEnvVar / configfile.StoreHostFileName; generated
// wrapper tests assert the agreement.
const sharedServerHostBlock = `# The federation's one source for the Dolt server; a process env value of the
# same name wins over the file, and a missing file is a loud refusal below.
_store_host_file="${BRAIN_STORE_HOST_FILE:-$HOME/.config/brain/store-host.env}"
_want_host="${BEADS_DOLT_SERVER_HOST:-}"
_want_port="${BEADS_DOLT_SERVER_PORT:-}"
if [ -z "$_want_host" ] || [ -z "$_want_port" ]; then
  if [ ! -f "$_store_host_file" ]; then
    echo "beads: missing shared store host config: $_store_host_file" >&2
    echo "beads: write BEADS_DOLT_SERVER_HOST=<host> and BEADS_DOLT_SERVER_PORT=<port> there, or export them for this shell" >&2
    exit 78
  fi
  . "$_store_host_file"
  [ -n "$_want_host" ] || _want_host="${BEADS_DOLT_SERVER_HOST:-}"
  [ -n "$_want_port" ] || _want_port="${BEADS_DOLT_SERVER_PORT:-}"
fi
if [ -z "$_want_host" ] || [ -z "$_want_port" ]; then
  echo "beads: $_store_host_file does not define BEADS_DOLT_SERVER_HOST and BEADS_DOLT_SERVER_PORT" >&2
  exit 78
fi
export BEADS_DOLT_SERVER_HOST="$_want_host"
export BEADS_DOLT_SERVER_PORT="$_want_port"
unset _store_host_file _want_host _want_port
`

// serverStoreWrapperTemplate is the canonical shared-server store wrapper:
// header, the store's own identity, server mode, the runtime host block, exec.
// serverStoreWrapperProvenanceLine is the header comment renderServerStoreWrapperBody
// replaces: host/port are provenance only, never baked into a variable.
const serverStoreWrapperProvenanceLine = "# Shared-server store, provisioned against {host}:{port}. The Dolt host and port\n# are read from the shared store host config at RUN TIME (see below), so\n# repointing this machine or the federation never rewrites this wrapper;\n"

const serverStoreWrapperTemplate = `#!/bin/sh
# Auto-generated by 'brain stores create {name}'.
` + serverStoreWrapperProvenanceLine + `# dolt_database={database} comes from .beads/metadata.json.
export BEADS_DIR={beadsdir}
export BD_NAME="{name}"
export BRAIN_KNOWLEDGE_ROOT={knowledgeroot}
export BRAIN_EXFIL_FLAT=1
export BEADS_DOLT_SERVER_MODE=1
export BEADS_DOLT_SHARED_SERVER=1
` + sharedServerHostBlock + `exec {bd} "$@"
`

// writeServerStoreWrapper writes the canonical shared-server store wrapper at
// ~/.local/bin/<name>. host/port are the server the store was provisioned
// against and appear in the header comment as provenance only: the wrapper
// resolves the server from the shared config at run time, so no host is baked
// in. It also exports BRAIN_KNOWLEDGE_ROOT/BRAIN_EXFIL_FLAT to match the
// federation canon.
// serverDatabase names the Dolt database the wrapper's metadata.json resolves
// to (the store's own database pre-cutover, the unified database after); it
// appears in the header comment only — bd reads the database from
// metadata.json, never from the wrapper.
// Returns the wrapper path.
func writeServerStoreWrapper(name, beadsDir, bdBinary, host string, port int, serverDatabase string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", binDir, err)
	}
	body := renderServerStoreWrapperBody(name, beadsDir, bdBinary, host, port, serverDatabase)
	wrapperPath := filepath.Join(binDir, name)
	if err := os.WriteFile(wrapperPath, []byte(body), 0o755); err != nil { //nolint:gosec
		return "", fmt.Errorf("writing %s: %w", wrapperPath, err)
	}
	return wrapperPath, nil
}

// renderServerStoreWrapperBody builds the canonical shared-server wrapper body.
// host/port and serverDatabase only ever appear in the header comment, as
// provenance: the executable lines resolve the server from the shared config at
// run time. host/port may be unknown (a rewrite that has no live server to
// name); the provenance line is then omitted rather than printing a fake value.
func renderServerStoreWrapperBody(name, beadsDir, bdBinary, host string, port int, serverDatabase string) string {
	if bdBinary == "" {
		bdBinary = "bd"
	}
	// The store name the render sits under comes from the namespace, not
	// the database (pre-cutover checklist item 4): once stores share the
	// unified database's BEADS_DIR, dirname(beadsDir) names the directory's
	// owner, not this store. Nest the render root under the namespace when
	// the two disagree; a store whose parent already matches renders as before.
	knowledgeRoot := filepath.Dir(beadsDir)
	if filepath.Base(knowledgeRoot) != name {
		knowledgeRoot = filepath.Join(knowledgeRoot, name)
	}
	template := serverStoreWrapperTemplate
	provenance := fmt.Sprintf("# Shared-server store, provisioned against %s:%d. The Dolt host and port\n# are read from the shared store host config at RUN TIME (see below), so\n# repointing this machine or the federation never rewrites this wrapper;\n", host, port)
	if host == "" || port <= 0 {
		provenance = "# Shared-server store. The Dolt host and port are read from the\n# shared store host config at RUN TIME (see below), so repointing this machine\n# or the federation never rewrites this wrapper;\n"
	}
	template = strings.Replace(template, serverStoreWrapperProvenanceLine, provenance, 1)
	return strings.NewReplacer(
		"{name}", name,
		"{host}", host,
		"{port}", strconv.Itoa(port),
		"{database}", serverDatabase,
		"{beadsdir}", shellPathExpr(beadsDir),
		"{knowledgeroot}", shellPathExpr(knowledgeRoot),
		"{bd}", bdBinary,
	).Replace(template)
}

// shellPathExpr renders a path as a double-quoted sh word for a generated
// wrapper. A path under the generating user's home is emitted as "$HOME/…" so
// the wrapper stays valid on any host or home directory (a wrapper copied or
// regenerated elsewhere must never carry another machine's absolute home).
// Other paths are emitted as an ordinary quoted literal.
func shellPathExpr(p string) string {
	home, err := os.UserHomeDir()
	if err == nil && home != "" && home != "/" && (p == home || strings.HasPrefix(p, home+"/")) {
		rest := p[len(home):]
		rest = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(rest)
		return `"$HOME` + rest + `"`
	}
	return fmt.Sprintf("%q", p)
}

// writeStoreWrapper writes a shell wrapper at ~/.local/bin/<name> that pins
// BEADS_DIR and BD_NAME, then exec's bd. Returns the wrapper path.
//
// bdBinary defaults to "bd" (resolved by PATH at exec time). Pass an absolute
// path to lock the wrapper to a specific binary.
func writeStoreWrapper(name, beadsDir, bdBinary string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", binDir, err)
	}
	wrapperPath := filepath.Join(binDir, name)
	body := renderStoreWrapperBody(name, beadsDir, bdBinary)
	if err := os.WriteFile(wrapperPath, []byte(body), 0o755); err != nil { //nolint:gosec
		return "", fmt.Errorf("writing %s: %w", wrapperPath, err)
	}
	return wrapperPath, nil
}

// renderStoreWrapperBody builds the minimal (embedded-store) wrapper body.
func renderStoreWrapperBody(name, beadsDir, bdBinary string) string {
	if bdBinary == "" {
		bdBinary = "bd"
	}
	return fmt.Sprintf(`#!/bin/sh
# Auto-generated by 'brain stores create %s'.
# Pins this CLI variant to its own store, then delegates to bd.
exec env BEADS_DIR=%s BD_NAME=%q %s "$@"
`, name, shellPathExpr(beadsDir), name, bdBinary)
}

// regenerateStoresEnv writes ~/.config/brain/stores.env from the registry,
// matching the format `brain stores env` emits. It exports the canonical
// BRAIN_STORE_* / BRAIN_STORES_LIST variables plus deprecated PAI_STORE_* /
// PAI_STORES_LIST aliases for transition. Returns the written path.
func regenerateStoresEnv(stores map[string]storeEntry) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	outPath := filepath.Join(home, storesEnvPath)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", filepath.Dir(outPath), err)
	}

	names := sortedKeys(stores)

	var sb strings.Builder
	sb.WriteString("# Auto-generated by 'brain stores env' — do not edit manually.\n")
	sb.WriteString("# Source in store wrapper scripts to get BRAIN_STORE_* vars.\n")
	sb.WriteString("# Deprecated PAI_STORE_* aliases are emitted for transition and will be removed.\n")
	sb.WriteString("# Regenerate: brain stores env\n")
	sb.WriteString("# Canonical file: ~/.config/brain/stores.env (legacy ~/.config/pai/stores.env is a symlink).\n\n")
	for _, n := range names {
		envKey := strings.ToUpper(strings.ReplaceAll(n, "-", "_"))
		sb.WriteString(fmt.Sprintf("export BRAIN_STORE_%s=%q\n", envKey, stores[n].Path))
	}
	sb.WriteString(fmt.Sprintf("\nexport BRAIN_STORES_LIST=%q\n", strings.Join(names, ":")))
	sb.WriteString("\n# Deprecated compat aliases (transition only).\n")
	for _, n := range names {
		envKey := strings.ToUpper(strings.ReplaceAll(n, "-", "_"))
		sb.WriteString(fmt.Sprintf("export PAI_STORE_%s=%q\n", envKey, stores[n].Path))
	}
	sb.WriteString(fmt.Sprintf("export PAI_STORES_LIST=%q\n", strings.Join(names, ":")))

	if err := os.WriteFile(outPath, []byte(sb.String()), 0o644); err != nil { //nolint:gosec
		return "", fmt.Errorf("writing %s: %w", outPath, err)
	}
	migrateLegacyFileToSymlink(outPath, filepath.Join(home, storesEnvLegacyPath))
	return outPath, nil
}

var (
	renderAllStoresJSON bool
)

var brainStoresRenderAllCmd = &cobra.Command{
	Use:   "render-all",
	Short: "Run 'bd render-all' against every store in the federation registry",
	Long: `Iterate every store registered in ~/.config/brain/stores.yaml and
trigger markdown exfiltration for each one. The current bd binary is
re-invoked once per store with BEADS_DIR pinned, so per-store summaries
land on stderr exactly as a stand-alone 'bd render-all' would.

Useful after:
  - Installing a new bd binary that changes the exfiltration contract
    (every-kind exfil, store-derived root, etc.).
  - Restoring a store from backup where the markdown sidecar drifted.
  - Adding a new store to the registry and wanting it populated.

A federation-level summary line is emitted on stderr at the end:
  Federation: <N> stores OK, <M> stores failed (rendered <R>, failed <F>)

JSON mode (--json) replaces the per-store stdout streams with one
structured object per store and a top-level summary.

Exit codes:
  0 — every store reported success
  1 — at least one store had per-bead failures or could not be opened`,
	Args: cobra.NoArgs,
	Run:  runBrainStoresRenderAll,
}

type renderAllStoreOutcome struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Rendered int    `json:"rendered"`
	Failed   int    `json:"failed"`
	Total    int    `json:"total"`
	Status   string `json:"status"`          // "ok" | "failed"
	Error    string `json:"error,omitempty"` // present when status=failed
}

func runBrainStoresRenderAll(_ *cobra.Command, _ []string) {
	stores, err := loadStoresRegistry()
	if err != nil {
		FatalError("loading registry: %v", err)
	}
	if len(stores) == 0 {
		fmt.Println("No stores registered. Use 'brain stores create <name>' or 'brain stores add <name> <beads-dir>'.")
		return
	}

	bd, err := os.Executable()
	if err != nil || bd == "" {
		// Fallback to argv[0] — fine for interactive use.
		bd = os.Args[0]
	}

	names := sortedKeys(stores)
	outcomes := make([]renderAllStoreOutcome, 0, len(names))
	storeOK := 0
	storeFailed := 0
	totalRendered := 0
	totalFailed := 0

	for _, name := range names {
		beadsDir := stores[name].Path
		out := renderAllStoreOutcome{Name: name, Path: beadsDir}

		// Subprocess: bd render-all --json with BEADS_DIR + BD_NAME pinned to
		// this store. --json gives us machine-readable per-bead counts; the
		// human-readable summary is reconstructed here at the federation level.
		sub := exec.Command(bd, "render-all", "--json")
		sub.Env = append(os.Environ(),
			"BEADS_DIR="+beadsDir,
			"BD_NAME="+name,
			// Disable auto-features the federation walk shouldn't fire per store.
			"BRAIN_NO_AUTO_FEATURE_REQUEST=1",
		)
		stdout, runErr := sub.Output()

		// Parse JSON regardless of exit code — render-all exits 1 when any bead
		// failed, but still emits the structured summary.
		var payload struct {
			Rendered int `json:"rendered"`
			Failed   int `json:"failed"`
			Total    int `json:"total"`
		}
		if jsonParseErr := json.Unmarshal(stdout, &payload); jsonParseErr == nil {
			out.Rendered = payload.Rendered
			out.Failed = payload.Failed
			out.Total = payload.Total
		}

		if runErr != nil && (out.Failed == 0 && out.Total == 0) {
			// Subprocess failed before producing a summary (e.g. bad BEADS_DIR,
			// dolt unreachable, migration error). Mark the whole store failed.
			out.Status = "failed"
			out.Error = runErr.Error()
			storeFailed++
		} else if out.Failed > 0 {
			out.Status = "failed"
			storeFailed++
		} else {
			out.Status = "ok"
			storeOK++
		}
		totalRendered += out.Rendered
		totalFailed += out.Failed
		outcomes = append(outcomes, out)

		if !renderAllStoresJSON {
			if out.Status == "ok" {
				fmt.Fprintf(os.Stderr, "[%s] %d / %d rendered (0 failed)\n",
					name, out.Rendered, out.Total)
			} else if out.Error != "" {
				fmt.Fprintf(os.Stderr, "[%s] ERROR: %s\n", name, out.Error)
			} else {
				fmt.Fprintf(os.Stderr, "[%s] %d / %d rendered (%d failed)\n",
					name, out.Rendered, out.Total, out.Failed)
			}
		}
	}

	if renderAllStoresJSON {
		summary := map[string]interface{}{
			"stores":      outcomes,
			"stores_ok":   storeOK,
			"stores_fail": storeFailed,
			"rendered":    totalRendered,
			"failed":      totalFailed,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(summary)
	} else {
		fmt.Fprintf(os.Stderr, "\nFederation: %d stores OK, %d stores failed (rendered %d, failed %d)\n",
			storeOK, storeFailed, totalRendered, totalFailed)
	}

	if storeFailed > 0 {
		os.Exit(1)
	}
}

var (
	aliasNoWrapper bool
	aliasBdBinary  string
)

var brainStoresAliasCmd = &cobra.Command{
	Use:   "alias <existing-store> <new-name>",
	Short: "Make a second CLI name resolve to an existing store",
	Long: `Create a second CLI variant ('alias') that points at an already-
registered store. Useful for singular/plural pairs (idea/ideas,
person/people) or short forms of long names.

Writes a wrapper at ~/.local/bin/<new-name> pinning the existing
store's BEADS_DIR and BD_NAME. Does NOT add a separate registry
entry — both names route to the same data because the wrapper
sets BEADS_DIR identically.

Examples:
  brain stores alias robots robot       # 'robot' command → robots store
  brain stores alias person people      # 'people' command → person store
  brain stores alias ideas idea         # 'idea' command → ideas store

To remove an alias, delete the wrapper:  rm ~/.local/bin/<new-name>`,
	Args: cobra.ExactArgs(2),
	Run:  runBrainStoresAlias,
}

func runBrainStoresAlias(_ *cobra.Command, args []string) {
	existing := strings.ToLower(strings.TrimSpace(args[0]))
	alias := strings.ToLower(strings.TrimSpace(args[1]))
	if existing == "" || alias == "" {
		FatalError("both <existing-store> and <new-name> are required")
	}
	if strings.ContainsAny(alias, " ./\\") {
		FatalError("alias name must be a simple identifier (no spaces, slashes, dots): %q", alias)
	}
	if alias == existing {
		FatalError("alias %q is the same as the existing store name", alias)
	}

	stores, err := loadStoresRegistry()
	if err != nil {
		FatalError("loading registry: %v", err)
	}
	existingEntry, ok := stores[existing]
	if !ok {
		FatalError("store %q is not registered (run 'brain stores list')", existing)
	}
	beadsDir := existingEntry.Path

	wrapperPath := ""
	if !aliasNoWrapper {
		wp, err := writeStoreWrapperForAlias(alias, beadsDir, aliasBdBinary, existing)
		if err != nil {
			FatalError("writing wrapper: %v", err)
		}
		wrapperPath = wp
	}

	if jsonOutput {
		outputJSON(map[string]string{
			"alias":    alias,
			"existing": existing,
			"path":     beadsDir,
			"wrapper":  wrapperPath,
		})
		return
	}
	fmt.Printf("%s Aliased %q → %q\n", ui.RenderPass("✓"), alias, existing)
	fmt.Printf("    target:  %s\n", beadsDir)
	if wrapperPath != "" {
		fmt.Printf("    wrapper: %s\n", wrapperPath)
	}
}

// writeStoreWrapperForAlias is like writeStoreWrapper but pins BD_NAME
// to the canonical (existing) store name, so aliased commands report
// the canonical name in version output and routing.
func writeStoreWrapperForAlias(alias, beadsDir, bdBinary, canonical string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", binDir, err)
	}
	if bdBinary == "" {
		bdBinary = "bd"
	}
	wrapperPath := filepath.Join(binDir, alias)
	body := fmt.Sprintf(`#!/bin/sh
# Auto-generated by 'brain stores alias %s %s'.
# Routes the %q CLI variant to the %q store.
exec env BEADS_DIR=%s BD_NAME=%q %s "$@"
`, canonical, alias, alias, canonical, shellPathExpr(beadsDir), canonical, bdBinary)
	if err := os.WriteFile(wrapperPath, []byte(body), 0o755); err != nil { //nolint:gosec
		return "", fmt.Errorf("writing %s: %w", wrapperPath, err)
	}
	return wrapperPath, nil
}

var (
	renameKeepOldWrapper bool
)

var brainStoresRenameCmd = &cobra.Command{
	Use:   "rename <old-name> <new-name>",
	Short: "Rename a registered store: move directory, update wrapper, registry, env",
	Long: `Rename a connected store end-to-end. Does, in order:

  1. Rename ~/data/<old-name>/  →  ~/data/<new-name>/
     (if the path follows the default ~/data/<name>/ convention).
     A custom path is left in place; only the registry key changes.
  2. Rewrite the wrapper at ~/.local/bin/<old-name> to ~/.local/bin/<new-name>
     pointing at the new path. Old wrapper is removed unless --keep-old-wrapper.
  3. Update ~/.config/brain/stores.yaml: <old-name> → <new-name>.
  4. Regenerate ~/.config/brain/stores.env.

What this does NOT do:
  - The underlying Dolt database name stays the same — existing bead IDs
    keep their original prefix (e.g. agent-XXXXX stays agent-XXXXX after
    renaming 'agents' to 'robots'). The transfer-verb's builtin alias
    table maps the old name to the new for legacy lookups.
  - The exfiltrated markdown root moves with the directory, so future
    'bd render' calls write to ~/data/<new-name>/entries/ automatically.

Examples:
  brain stores rename agents robots
  brain stores rename fishes whales
  brain stores rename agents robots --keep-old-wrapper   # leave both names working`,
	Args: cobra.ExactArgs(2),
	Run:  runBrainStoresRename,
}

func runBrainStoresRename(_ *cobra.Command, args []string) {
	oldName := strings.ToLower(strings.TrimSpace(args[0]))
	newName := strings.ToLower(strings.TrimSpace(args[1]))
	if oldName == "" || newName == "" {
		FatalError("both <old-name> and <new-name> are required")
	}
	if strings.ContainsAny(newName, " ./\\") {
		FatalError("new name must be a simple identifier (no spaces, slashes, dots): %q", newName)
	}
	if oldName == newName {
		FatalError("old and new names are the same")
	}

	stores, err := loadStoresRegistry()
	if err != nil {
		FatalError("loading registry: %v", err)
	}
	oldEntry, ok := stores[oldName]
	if !ok {
		FatalError("store %q is not registered (run 'brain stores list')", oldName)
	}
	oldBeadsDir := oldEntry.Path
	if _, ok := stores[newName]; ok {
		FatalError("store %q already exists in the registry; pick a different name", newName)
	}

	// Step 1: move the parent directory if it follows the default convention.
	newBeadsDir := oldBeadsDir
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		FatalError("resolving home dir: %v", err)
	}
	defaultOldPath := filepath.Join(home, "data", oldName)
	expectedOldBeads := filepath.Join(defaultOldPath, ".beads")
	if oldBeadsDir == expectedOldBeads {
		defaultNewPath := filepath.Join(home, "data", newName)
		if _, err := os.Stat(defaultNewPath); err == nil {
			FatalError("target path %s already exists", defaultNewPath)
		}
		if err := os.Rename(defaultOldPath, defaultNewPath); err != nil {
			FatalError("renaming %s -> %s: %v", defaultOldPath, defaultNewPath, err)
		}
		newBeadsDir = filepath.Join(defaultNewPath, ".beads")
	}

	// Step 2: write new wrapper, remove old (unless asked to keep).
	wrapperPath, err := writeStoreWrapper(newName, newBeadsDir, "")
	if err != nil {
		FatalError("writing new wrapper: %v", err)
	}
	if !renameKeepOldWrapper {
		oldWrapper := filepath.Join(home, ".local", "bin", oldName)
		if _, err := os.Stat(oldWrapper); err == nil {
			_ = os.Remove(oldWrapper) // best-effort cleanup
		}
	}

	// Step 3: update registry yaml. Carry the about blurb across the rename.
	delete(stores, oldName)
	oldEntry.Path = newBeadsDir
	stores[newName] = oldEntry
	if err := saveStoresRegistry(stores); err != nil {
		FatalError("saving registry: %v", err)
	}

	// Step 4: regenerate stores.env.
	envPath, err := regenerateStoresEnv(stores)
	if err != nil {
		FatalError("regenerating stores.env: %v", err)
	}

	if jsonOutput {
		outputJSON(map[string]string{
			"old_name":    oldName,
			"new_name":    newName,
			"old_path":    oldBeadsDir,
			"new_path":    newBeadsDir,
			"wrapper":     wrapperPath,
			"registry":    storesYamlFile(),
			"env_file":    envPath,
			"old_wrapper": pickRemovalMessage(oldName, renameKeepOldWrapper),
		})
		return
	}
	fmt.Printf("%s Renamed store %q → %q\n", ui.RenderPass("✓"), oldName, newName)
	fmt.Printf("    path:     %s → %s\n", oldBeadsDir, newBeadsDir)
	fmt.Printf("    wrapper:  %s\n", wrapperPath)
	fmt.Printf("    registry: %s\n", storesYamlFile())
	fmt.Printf("    env:      %s\n", envPath)
	if !renameKeepOldWrapper {
		fmt.Printf("    removed:  ~/.local/bin/%s\n", oldName)
	}
}

func pickRemovalMessage(oldName string, kept bool) string {
	if kept {
		return "kept"
	}
	return filepath.Join("~", ".local", "bin", oldName)
}

var brainStoresSetAboutCmd = &cobra.Command{
	Use:   "set-about <store> <blurb>",
	Short: "Set the human-readable 'about' blurb for a registered store",
	Long: `Attach a short description to a registered store, stored in the
registry (~/.config/brain/stores.yaml) only. The wrapper script is left
unchanged. View blurbs with 'brain stores list --verbose'.

Pass an empty string to clear the blurb.

Examples:
  brain stores set-about robots "agent-detected tooling defects"
  brain stores set-about ideas ""     # clear the blurb`,
	Args: cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		name := strings.ToLower(strings.TrimSpace(args[0]))
		about := strings.TrimSpace(args[1])

		stores, err := loadStoresRegistry()
		if err != nil {
			FatalError("loading registry: %v", err)
		}
		entry, ok := stores[name]
		if !ok {
			FatalError("store %q is not registered (run 'brain stores list')", name)
		}
		entry.About = about
		stores[name] = entry
		if err := saveStoresRegistry(stores); err != nil {
			FatalError("saving registry: %v", err)
		}

		if jsonOutput {
			outputJSON(map[string]string{"name": name, "path": entry.Path, "about": about})
			return
		}
		if about == "" {
			fmt.Printf("%s Cleared about blurb for store %q\n", ui.RenderPass("✓"), name)
		} else {
			fmt.Printf("%s Set about for store %q → %s\n", ui.RenderPass("✓"), name, about)
		}
	},
}

var brainStoresEnvCmd = &cobra.Command{
	Use:   "env",
	Short: "Write ~/.config/brain/stores.env from the registry (for shell wrappers)",
	Run: func(cmd *cobra.Command, args []string) {
		stores, err := loadStoresRegistry()
		if err != nil {
			FatalError("loading registry: %v", err)
		}

		envPath, err := regenerateStoresEnv(stores)
		if err != nil {
			FatalError("%v", err)
		}

		names := sortedKeys(stores)
		if jsonOutput {
			outputJSON(map[string]string{"path": envPath, "stores": strings.Join(names, ",")})
			return
		}
		fmt.Printf("%s Wrote %s (%d stores)\n", ui.RenderPass("✓"), envPath, len(stores))
	},
}

// expandPath expands ~ and cleans the path.
func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	return filepath.Clean(p)
}

// resolveStorePath resolves a registry path for the machine it is read on: a
// leading ~ is expanded, and a path recorded under another person's home is
// healed to this machine's own equivalent — but only when the recorded path
// has gone and the equivalent exists. It reports whether it healed.
//
// The healing is the registry's half of the same machine-portability fix the
// wrappers already have (homeRelativeValue / 'brain stores rewrite-wrappers'):
// a store registered as /Users/alice/data/task/.beads is still the store at
// $HOME/data/task/.beads after the store directory (or the whole registry)
// moves to another machine or home. Nothing is guessed: a recorded path that
// still exists always wins, and a path with no existing home equivalent is
// left alone so a genuinely missing store keeps reporting as missing.
func resolveStorePath(recorded string) (string, bool) {
	p := expandPath(recorded)
	if _, err := os.Stat(p); err == nil {
		return p, false
	}
	healed, ok := healForeignHomePath(p)
	if !ok {
		return p, false
	}
	return healed, true
}

// healForeignHomePath maps an absolute path under any /Users/<x> or /home/<x>
// home to this machine's home equivalent, and only when that equivalent
// exists. It never maps the current home onto itself.
func healForeignHomePath(p string) (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == "/" {
		return p, false
	}
	m := foreignHomeRe.FindStringSubmatch(p)
	if m == nil {
		return p, false
	}
	candidate := filepath.Clean(home + m[1])
	if candidate == filepath.Clean(p) {
		return p, false
	}
	if _, err := os.Stat(candidate); err != nil {
		return p, false
	}
	return candidate, true
}

func init() {
	brainStoresCreateCmd.Flags().StringVar(&createStorePath, "path", "",
		"Filesystem root for the new store (default: $HOME/data/<name>)")
	brainStoresCreateCmd.Flags().BoolVar(&createStoreNoWrap, "no-wrapper", false,
		"Skip writing the CLI wrapper at ~/.local/bin/<name>")
	brainStoresCreateCmd.Flags().StringVar(&createStoreBinary, "bd-binary", "",
		"Path to bd that the wrapper should exec (default: \"bd\", resolved by PATH)")
	brainStoresCreateCmd.Flags().StringVar(&createStoreInitName, "dolt-name", "",
		"--name arg to pass to 'dolt init' (skipped if empty)")
	brainStoresCreateCmd.Flags().StringVar(&createStoreInitMail, "dolt-email", "",
		"--email arg to pass to 'dolt init' (skipped if empty)")
	brainStoresCreateCmd.Flags().BoolVar(&createStoreEmbedded, "embedded", false,
		"Create an embedded (self-starting) store even when a shared Dolt sql-server is known")

	brainStoresRenderAllCmd.Flags().BoolVar(&renderAllStoresJSON, "json", false,
		"Emit a structured JSON object instead of per-store text summaries")

	brainStoresAliasCmd.Flags().BoolVar(&aliasNoWrapper, "no-wrapper", false,
		"Skip writing the wrapper at ~/.local/bin/<alias>")
	brainStoresAliasCmd.Flags().StringVar(&aliasBdBinary, "bd-binary", "",
		"Path to bd that the wrapper should exec (default: \"bd\")")

	brainStoresRenameCmd.Flags().BoolVar(&renameKeepOldWrapper, "keep-old-wrapper", false,
		"Leave ~/.local/bin/<old-name> in place pointing at the renamed path")

	brainStoresListCmd.Flags().BoolVarP(&storesListVerbose, "verbose", "v", false,
		"Include each store's about blurb as an extra column")

	brainStoresCmd.AddCommand(brainStoresAddCmd)
	brainStoresCmd.AddCommand(brainStoresRemoveCmd)
	brainStoresCmd.AddCommand(brainStoresListCmd)
	brainStoresCmd.AddCommand(brainStoresCreateCmd)
	brainStoresCmd.AddCommand(brainStoresAliasCmd)
	brainStoresCmd.AddCommand(brainStoresRenameCmd)
	brainStoresCmd.AddCommand(brainStoresSetAboutCmd)
	brainStoresCmd.AddCommand(brainStoresRenderAllCmd)
	brainStoresCmd.AddCommand(brainStoresEditBackCmd)
	brainStoresCmd.AddCommand(brainStoresEnvCmd)
	brainCmd.AddCommand(brainStoresCmd)
	rootCmd.AddCommand(brainStoresCmd)
}
