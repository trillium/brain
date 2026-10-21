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
	"github.com/steveyegge/beads/internal/storeregistry"
	"github.com/steveyegge/beads/internal/ui"
	"gopkg.in/yaml.v3"
)

// storesYamlPath is the registry file for brain federation stores.
const storesYamlPath = ".config/brain/stores.yaml"

// storesEnvPath is the shell-sourceable export of the registry.
const storesEnvPath = ".config/brain/stores.env"

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

func storesEnvFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, storesEnvPath)
}

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

// loadStoresRegistry reads the registry file. A missing file is an empty
// registry; a present-but-unreadable or unparseable one is an error, so a
// later save cannot persist an empty registry over unknown state.
func loadStoresRegistry() (map[string]storeEntry, error) {
	stores, _, err := readStoresFile(storesYamlFile())
	if err != nil {
		return nil, err
	}
	if stores == nil {
		stores = make(map[string]storeEntry)
	}
	return stores, nil
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
	header := "# brain federation store registry — managed by 'brain stores'\n# Do not edit manually; use 'brain stores add/remove'.\n\n"
	if err := os.WriteFile(path, append([]byte(header), data...), 0o644); err != nil { //nolint:gosec
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

var brainStoresCmd = &cobra.Command{
	Use:   "stores",
	Short: "Manage the brain federation store registry",
	Long: `Manage the registry of bd stores federated under brain.

The registry lives at ~/.config/brain/stores.yaml. Each registered store
can be searched via 'brain search', transferred to via 'brain transfer',
and synced via 'brain repo sync'.

Run 'brain stores env' to regenerate ~/.config/brain/stores.env for
shell wrapper scripts that need BRAIN_STORE_* variables.`,
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

		// Telegraph it: a registered store the fleet cannot learn of exists on
		// one machine only. Under $HOME/data it goes into the shared registry.
		epochNote := ""
		if db, adopted := registryWriteTarget(rootCtx); adopted {
			if rel, ok := homeRelativeDir(beadsDir); ok {
				e := storeregistry.Entry{Name: name, Kind: storeregistry.KindStore, Store: name, State: storeregistry.StateActive,
					RelPath: rel, WrapperKind: detectWrapperKindFor(name, filepath.Dir(beadsDir)), About: entry.About, EditBack: entry.EditBack}
				if e.Validate() == nil {
					if res, merr := storeregistry.Mutate(rootCtx, db, registryMeta(), storeregistry.PlanCreate(e)); merr != nil {
						fmt.Fprintf(os.Stderr, "warning: store registered locally only; the shared registry refused it: %v\n", merr)
					} else {
						epochNote = syncThisMachine(rootCtx, db, res.Epoch)
					}
				} else {
					fmt.Fprintf(os.Stderr, "note: %q is registered on this machine only (%v)\n", name, e.Validate())
				}
			} else {
				fmt.Fprintf(os.Stderr, "note: %q is registered on this machine only (its path is not under $HOME)\n", name)
			}
		}

		if jsonOutput {
			outputJSON(map[string]string{"name": name, "path": beadsDir, "registry": epochNote})
			return
		}
		fmt.Printf("%s Registered store %q → %s\n", ui.RenderPass("✓"), name, beadsDir)
		if epochNote != "" {
			fmt.Printf("    %s\n", epochNote)
		}
	},
}

var brainStoresRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a store from the fleet: tombstone in the shared registry, then each machine drops its generated wrapper",
	Long: `Remove a store name from the brain fleet.

When the shared registry is adopted, the name is tombstoned in the registry in
one transaction (epoch +1). This machine then syncs at once and every other
machine on its next command or within 5 minutes: each removes only its own
GENERATED wrapper, ~/.pi/agent/bin link and stores.yaml/stores.env line.
Nothing else is touched: the store's data directory, its rows in the database
and its id prefix all stay (a removed store can be created again later).
A wrapper that brain did not generate is reported, never deleted.

--local-only (or a registry that is not adopted yet) keeps the old behaviour:
the line leaves this machine's stores.yaml and stores.env is regenerated;
no other machine is told.`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		name := strings.ToLower(strings.TrimSpace(args[0]))

		if db, adopted := registryWriteTarget(rootCtx); adopted {
			snap, _, rerr := storeregistry.ReadSnapshot(rootCtx, db)
			if rerr == nil {
				for _, e := range snap.Entries {
					if e.Name == name && e.State == storeregistry.StateActive {
						res, err := storeregistry.Mutate(rootCtx, db, registryMeta(), storeregistry.PlanRemove(name))
						if err != nil {
							FatalError("removing %q from the registry: %v", name, err)
						}
						line := syncThisMachine(rootCtx, db, res.Epoch)
						if jsonOutput {
							outputJSON(map[string]any{"name": name, "epoch": res.Epoch, "registry": line})
							return
						}
						fmt.Printf("%s Removed store %q (tombstoned; data and database rows untouched)\n    %s\n", ui.RenderPass("✓"), name, line)
						return
					}
				}
			}
		}

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
		if _, err := regenerateStoresEnv(stores); err != nil {
			FatalError("regenerating stores.env: %v", err)
		}
		fmt.Printf("%s Removed store %q from this machine's registry only\n", ui.RenderPass("✓"), name)
		if wp, ok := storeWrapperPath(name); ok {
			fmt.Printf("    its wrapper %s was left in place; delete it by hand, or adopt the fleet registry ('brain stores reconcile') so removals propagate\n", wp)
		}
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

// registryApplyPlan applies a lifecycle plan through the shared registry when
// the registry is adopted and knows the name. applied is false when the verb
// should fall back to its local-only behaviour (registry not adopted, or the
// name is not in it). note is the "epoch N; k of n machines converged" line.
func registryApplyPlan(ctx context.Context, name string, plan storeregistry.PlanFunc) (note string, applied bool, err error) {
	db, adopted := registryWriteTarget(ctx)
	if !adopted {
		return "", false, nil
	}
	entries, rerr := storeregistry.ReadEntries(ctx, db)
	if rerr != nil {
		return "", false, nil
	}
	found := false
	for _, e := range entries {
		if e.Name == name && e.State == storeregistry.StateActive {
			found = true
		}
	}
	if !found {
		return "", false, nil
	}
	res, err := storeregistry.Mutate(ctx, db, registryMeta(), plan)
	if err != nil {
		return "", true, err
	}
	return syncThisMachine(ctx, db, res.Epoch), true, nil
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

		if note, applied, err := registryApplyPlan(rootCtx, name, storeregistry.PlanEditBack(name, option == "on")); applied {
			if err != nil {
				FatalError("%v", err)
			}
			if jsonOutput {
				outputJSON(map[string]any{"name": name, "edit_back": option == "on", "registry": note})
				return
			}
			fmt.Printf("%s store %s: edit-back %s\n    %s\n", ui.RenderPass("✓"), name, option, note)
			return
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
)

// createRegistry carries the shared-registry write of a create through
// provisioning: the namespace seed and the registry entry commit in one
// transaction, and Epoch is the epoch that transaction produced.
type createRegistry struct {
	DB    *sql.DB
	Entry storeregistry.Entry
	Epoch int64
}

var brainStoresCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Provision a new connected store (dolt init + entries dir + wrapper + registry)",
	Long: `Provision a new connected store end-to-end. Does, in order:

  1. Provision the Dolt store. In shared-server mode (BEADS_DOLT_SERVER_MODE=1,
     as exported by the brain wrapper) the store is created as a database in the
     running shared dolt sql-server: metadata.json (dolt_mode=server) and
     config.yaml are written first, then bd CREATEs the database and initializes
     the schema. Without the server pins it falls back to an embedded 'dolt init'
     in <path>/.beads/.
  2. Create <path>/entries/ for exfiltrated markdown.
  3. Write a CLI wrapper at ~/.local/bin/<name>. In shared-server mode the
     wrapper exports the four server pins (mirroring the review/brain wrappers);
     otherwise it pins only BEADS_DIR and BD_NAME. Either way it exec's bd.
  4. Register the store in ~/.config/brain/stores.yaml.
  5. Regenerate ~/.config/brain/stores.env.

Default path is $HOME/data/<name>. Override with --path. Skip the wrapper
with --no-wrapper if you manage shell shims another way.

If any step fails after files have been written, the verb leaves the
partial state in place and exits non-zero — re-running with the same
arguments resumes idempotently.

Examples:
  brain stores create recipes
  brain stores create recipes --path /Volumes/extra/recipes
  brain stores create recipes --bd-binary /opt/homebrew/bin/bd`,
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

	// Step 1: provision the Dolt store. In shared-server mode (the brain
	// federation default — the brain wrapper exports BEADS_DOLT_SERVER_MODE=1
	// and the server pins at create time) the store is either a namespace of
	// the unified database (when the server hosts one — the post-cutover
	// shape) or its own database in the running shared dolt sql-server (the
	// pre-cutover shape), never an embedded .beads/.dolt repo unless those
	// pins are absent. this is the one command an operator types to "make a
	// boop store": afterwards 'boop list/ready/create' address the boop
	// namespace, whatever the substrate is.
	host, port, shared := resolveSharedServerCreate()

	// Registry-adopted fleets: the store is created IN the shared registry, in
	// the same transaction as its namespace seed, so every other machine learns
	// of it. The wrapper, stores.yaml line and agent link then come from the
	// same sync every other machine runs.
	var reg *createRegistry
	if db, adopted := registryWriteTarget(rootCtx); adopted && shared {
		rel, relOK := homeRelativeDir(beadsDir)
		e := storeregistry.Entry{Name: name, Kind: storeregistry.KindStore, Store: name, State: storeregistry.StateActive,
			RelPath: rel, WrapperKind: storeregistry.WrapperServer}
		switch {
		case !relOK || e.Validate() != nil:
			FatalError("the shared registry cannot hold store %q at %s (lowercase name; directory under $HOME/data): %v; use --local-only to create it on this machine only", name, beadsDir, e.Validate())
		case createStoreNoWrap:
			FatalError("--no-wrapper cannot be combined with the shared registry (every machine generates the wrapper); use --local-only")
		case createStoreBinary != "":
			FatalError("--bd-binary cannot be combined with the shared registry (wrappers run $BD_BIN or bd); use --local-only")
		}
		reg = &createRegistry{DB: db, Entry: e}
	} else if shared && !storesLocalOnly {
		fmt.Fprintln(os.Stderr, "note: the shared store registry is not adopted in this database, so other machines will not learn of this store; run 'brain stores reconcile'")
	}
	provisioned := ""
	if shared {
		pdb, perr := provisionServerStore(context.Background(), name, beadsDir, host, port, reg)
		if perr != nil {
			FatalError("provisioning server store at %s: %v", beadsDir, perr)
		}
		provisioned = pdb
	} else {
		if err := initDoltStore(beadsDir); err != nil {
			FatalError("dolt init at %s: %v", beadsDir, err)
		}
	}

	// Step 2: entries/ sibling for exfiltration.
	if err := os.MkdirAll(entriesDir, 0o755); err != nil {
		FatalError("creating %s: %v", entriesDir, err)
	}

	if reg != nil && reg.Epoch > 0 {
		line := syncThisMachine(rootCtx, reg.DB, reg.Epoch)
		wp, _ := storeWrapperPath(name)
		if jsonOutput {
			outputJSON(map[string]any{"name": name, "path": path, "beads_dir": beadsDir, "entries_dir": entriesDir,
				"wrapper": wp, "registry": storesYamlFile(), "env_file": storesEnvFile(), "epoch": reg.Epoch, "fleet": line})
			return
		}
		fmt.Printf("%s Created store %q\n", ui.RenderPass("✓"), name)
		fmt.Printf("    beads:    %s\n", beadsDir)
		fmt.Printf("    entries:  %s\n", entriesDir)
		fmt.Printf("    wrapper:  %s\n", wp)
		fmt.Printf("    %s\n", line)
		return
	}

	// Step 3: wrapper at ~/.local/bin/<name>. Shared-server stores get the four
	// server pins so bd resolves in server mode; embedded stores get the minimal
	// BEADS_DIR/BD_NAME wrapper.
	wrapperPath := ""
	if !createStoreNoWrap {
		var wp string
		var werr error
		if shared {
			wp, werr = writeServerStoreWrapper(name, beadsDir, createStoreBinary, host, port, provisioned)
		} else {
			wp, werr = writeStoreWrapper(name, beadsDir, createStoreBinary)
		}
		if werr != nil {
			FatalError("writing wrapper: %v", werr)
		}
		wrapperPath = wp
	}

	// Step 4: register in stores.yaml.
	stores, err := loadStoresRegistry()
	if err != nil {
		FatalError("loading registry: %v", err)
	}
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

// resolveSharedServerCreate detects whether 'brain stores create' is running in
// brain shared-server mode. The brain wrapper exports BEADS_DOLT_SERVER_MODE=1 (or
// BEADS_DOLT_SHARED_SERVER=1) plus the server host/port at create time; when
// present, a new store must be provisioned as a database in the running shared
// dolt sql-server rather than as an embedded .beads/.dolt repo. Returns the
// resolved host, port, and whether shared-server mode is active.
func resolveSharedServerCreate() (host string, port int, shared bool) {
	if os.Getenv("BEADS_DOLT_SERVER_MODE") != "1" && os.Getenv("BEADS_DOLT_SHARED_SERVER") != "1" {
		return "", 0, false
	}
	host = os.Getenv("BEADS_DOLT_SERVER_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port = dolt.DefaultSQLPort
	if v := os.Getenv("BEADS_DOLT_SERVER_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			port = n
		}
	}
	return host, port, true
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
func provisionServerStore(ctx context.Context, name, beadsDir, host string, port int, reg *createRegistry) (string, error) {
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
		if reg != nil {
			if err := registerCreatedStore(ctx, reg, unifiedDatabase, name, projectID); err != nil {
				return unifiedDatabase, err
			}
		} else if err := seedNamespaceRows(ctx, host, port, unifiedDatabase, name, projectID); err != nil {
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
	if err := seedNamespaceTx(seedCtx, tx, unifiedDatabase, name, projectID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing the %q namespace seed: %w", name, err)
	}
	return nil
}

// registerCreatedStore writes a new store into the shared registry. The
// namespace seed (issue_prefix, project identity, brain_stores row, name-prefix
// claim) rides in the registry's own transaction, through the caller's
// connection, so the store and the epoch bump commit together or not at all.
func registerCreatedStore(ctx context.Context, reg *createRegistry, unifiedDatabase, name, projectID string) error {
	meta := registryMeta()
	meta.InTx = func(ctx context.Context, tx storeregistry.DBTX) error {
		return seedNamespaceTx(ctx, tx, unifiedDatabase, name, projectID)
	}
	res, err := storeregistry.Mutate(ctx, reg.DB, meta, storeregistry.PlanCreate(reg.Entry))
	if err != nil {
		return fmt.Errorf("recording %q in the shared registry: %w", name, err)
	}
	reg.Epoch = res.Epoch
	if !res.Changed {
		// Idempotent re-run of an existing store: report the current epoch.
		st, _, _ := storeregistry.ReadState(ctx, reg.DB)
		reg.Epoch = st.Epoch
	}
	return nil
}

// seedNamespaceTx is the body of the namespace seed, run inside a caller-owned
// transaction: the registry's (so the seed and the epoch bump commit together)
// or seedNamespaceRows' own.
func seedNamespaceTx(ctx context.Context, tx storeregistry.DBTX, unifiedDatabase, name, projectID string) error {
	if _, err := issueops.RecordStorePrefix(ctx, tx, name, name, reasonStoreCreated); err != nil {
		return fmt.Errorf("recording the name prefix for %q: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"REPLACE INTO brain_unified_config (`store`,`key`,value) VALUES (?, 'issue_prefix', ?)", name, name); err != nil {
		return fmt.Errorf("seeding issue_prefix for %q: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"REPLACE INTO brain_unified_metadata (`store`,`key`,value) VALUES (?, '_project_id', ?)", name, projectID); err != nil {
		return fmt.Errorf("seeding project identity for %q: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"REPLACE INTO brain_stores (`store`,`registered`,`source_database`,`beads_dir`,`project_id`,`schema_version`,`template_source`,`imported_at`) VALUES (?, 1, ?, '', ?, '', 0, ?)",
		name, unifiedDatabase, projectID, time.Now().UTC().Format("2006-01-02 15:04:05")); err != nil {
		return fmt.Errorf("recording store %q in brain_stores: %w", name, err)
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

// writeServerStoreWrapper writes a CLI wrapper at ~/.local/bin/<name> for a
// shared-server store. Unlike writeStoreWrapper, it exports the four shared-server
// pins (mirroring the review/brain wrappers) so bd resolves in server mode and
// never falls back to embedded mode or mis-discovers a sibling database. It also
// exports BRAIN_KNOWLEDGE_ROOT/BRAIN_EXFIL_FLAT to match the federation canon.
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
	wrapperPath := filepath.Join(binDir, name)
	body := fmt.Sprintf(`#!/bin/sh
# Auto-generated by 'brain stores create %s'.
# Server-mode store on the shared Dolt sql-server (%s:%d); dolt_database=%s.
# The explicit shared-server pins mirror the review/brain wrappers so bd never
# falls back to embedded mode or mis-discovers a sibling database.
export BEADS_DIR=%s
export BD_NAME=%q
export BRAIN_KNOWLEDGE_ROOT=%s
export BRAIN_EXFIL_FLAT=1
export BEADS_DOLT_SERVER_MODE=1
export BEADS_DOLT_SERVER_HOST=%s
export BEADS_DOLT_SERVER_PORT=%d
export BEADS_DOLT_SHARED_SERVER=1
exec %s "$@"
`, name, host, port, serverDatabase, shellPathExpr(beadsDir), name, shellPathExpr(knowledgeRoot), host, port, bdBinary)
	if err := os.WriteFile(wrapperPath, []byte(body), 0o755); err != nil { //nolint:gosec
		return "", fmt.Errorf("writing %s: %w", wrapperPath, err)
	}
	return wrapperPath, nil
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
	if bdBinary == "" {
		bdBinary = "bd"
	}
	wrapperPath := filepath.Join(binDir, name)
	body := fmt.Sprintf(`#!/bin/sh
# Auto-generated by 'brain stores create %s'.
# Pins this CLI variant to its own store, then delegates to bd.
exec env BEADS_DIR=%s BD_NAME=%q %s "$@"
`, name, shellPathExpr(beadsDir), name, bdBinary)
	if err := os.WriteFile(wrapperPath, []byte(body), 0o755); err != nil { //nolint:gosec
		return "", fmt.Errorf("writing %s: %w", wrapperPath, err)
	}
	return wrapperPath, nil
}

// regenerateStoresEnv writes ~/.config/brain/stores.env from the registry,
// matching the format `brain stores env` emits. It exports the canonical
// BRAIN_STORE_* / BRAIN_STORES_LIST variables. Returns the written path.
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
	sb.WriteString("# Regenerate: brain stores env\n")
	sb.WriteString("# File: ~/.config/brain/stores.env\n\n")
	for _, n := range names {
		envKey := strings.ToUpper(strings.ReplaceAll(n, "-", "_"))
		sb.WriteString(fmt.Sprintf("export BRAIN_STORE_%s=%q\n", envKey, stores[n].Path))
	}
	sb.WriteString(fmt.Sprintf("\nexport BRAIN_STORES_LIST=%q\n", strings.Join(names, ":")))

	if err := os.WriteFile(outPath, []byte(sb.String()), 0o644); err != nil { //nolint:gosec
		return "", fmt.Errorf("writing %s: %w", outPath, err)
	}
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

	if !aliasNoWrapper && aliasBdBinary == "" {
		if note, applied, err := registryApplyPlan(rootCtx, existing, storeregistry.PlanAlias(existing, alias)); applied {
			if err != nil {
				FatalError("%v", err)
			}
			if jsonOutput {
				outputJSON(map[string]string{"alias": alias, "existing": existing, "registry": note})
				return
			}
			fmt.Printf("%s Aliased %q → %q\n    %s\n", ui.RenderPass("✓"), alias, existing, note)
			return
		}
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

	// Registry path: tombstone the old name, activate the new one, record the
	// alias that keeps the old namespace resolving, bump the epoch: one
	// transaction. This machine then syncs; the fleet follows.
	if regDB, adopted := registryWriteTarget(rootCtx); adopted {
		entries, _ := storeregistry.ReadEntries(rootCtx, regDB)
		canonical, known := "", false
		for _, e := range entries {
			if e.Name == oldName && e.State == storeregistry.StateActive && e.Kind == storeregistry.KindStore {
				canonical, known = e.Store, true
			}
		}
		if known {
			rel, relOK := homeRelativeDir(newBeadsDir)
			if !relOK || storeregistry.ValidRelPath(rel) != nil || !storeregistry.ValidName(newName) {
				revertRenameMove(oldBeadsDir, newBeadsDir)
				FatalError("the registry cannot hold %q at %s (names are lowercase letters, digits, - and _; the directory must be under $HOME/data); use --local-only", newName, newBeadsDir)
			}
			res, err := registryRenameStore(rootCtx, regDB, oldName, newName, canonical, rel, renameKeepOldWrapper)
			if err != nil {
				revertRenameMove(oldBeadsDir, newBeadsDir)
				FatalError("renaming %q in the registry: %v", oldName, err)
			}
			line := syncThisMachine(rootCtx, regDB, res.Epoch)
			if jsonOutput {
				outputJSON(map[string]any{"old_name": oldName, "new_name": newName, "old_path": oldBeadsDir, "new_path": newBeadsDir, "epoch": res.Epoch, "registry": line})
				return
			}
			fmt.Printf("%s Renamed store %q → %q\n", ui.RenderPass("✓"), oldName, newName)
			fmt.Printf("    path:     %s → %s\n", oldBeadsDir, newBeadsDir)
			fmt.Printf("    %s\n", line)
			fmt.Printf("    other machines keep their old data directory untouched; they get the new name's wrapper and directory skeleton from the registry\n")
			return
		}
	}

	// Step 2: write new wrapper, remove old (unless asked to keep). A store in
	// shared-server mode keeps its server pins: the embedded form would
	// silently point the renamed store at nothing.
	var wrapperPath string
	if host, port, shared := resolveSharedServerCreate(); shared {
		wrapperPath, err = writeServerStoreWrapper(newName, newBeadsDir, "", host, port, "")
	} else {
		wrapperPath, err = writeStoreWrapper(newName, newBeadsDir, "")
	}
	if err != nil {
		FatalError("writing new wrapper: %v", err)
	}
	if !renameKeepOldWrapper {
		oldWrapper := filepath.Join(home, ".local", "bin", oldName)
		if data, err := os.ReadFile(oldWrapper); err == nil && isGeneratedWrapperBody(data) {
			_ = os.Remove(oldWrapper) // best-effort cleanup of a wrapper brain wrote
		} else if err == nil {
			fmt.Fprintf(os.Stderr, "note: %s was not generated by brain and was left in place\n", oldWrapper)
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
	fmt.Fprintln(os.Stderr, "note: the shared store registry is not adopted here, so other machines were not told; run 'brain stores reconcile'")

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

		if note, applied, err := registryApplyPlan(rootCtx, name, storeregistry.PlanAbout(name, about)); applied {
			if err != nil {
				FatalError("%v", err)
			}
			if jsonOutput {
				outputJSON(map[string]string{"name": name, "about": about, "registry": note})
				return
			}
			fmt.Printf("%s Set about for store %q\n    %s\n", ui.RenderPass("✓"), name, note)
			return
		}

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

	for _, c := range []*cobra.Command{brainStoresCreateCmd, brainStoresRenameCmd, brainStoresRemoveCmd, brainStoresAddCmd} {
		c.Flags().BoolVar(&storesLocalOnly, "local-only", false,
			"Act on this machine only; do not write the shared store registry (other machines are not told)")
	}

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

// revertRenameMove undoes the directory move of a rename that the registry then
// refused, so a failed rename leaves the machine exactly as it was.
func revertRenameMove(oldBeadsDir, newBeadsDir string) {
	if oldBeadsDir == newBeadsDir {
		return
	}
	if err := os.Rename(filepath.Dir(newBeadsDir), filepath.Dir(oldBeadsDir)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not move %s back to %s: %v\n", filepath.Dir(newBeadsDir), filepath.Dir(oldBeadsDir), err)
	}
}

// registryRenameStore tombstones the old name, activates the new one and
// records the database alias that keeps the old namespace resolving, in one
// transaction with the epoch bump.
func registryRenameStore(ctx context.Context, db *sql.DB, oldName, newName, canonical, relPath string, keepOld bool) (storeregistry.Result, error) {
	meta := registryMeta()
	if canonical != newName {
		meta.InTx = func(ctx context.Context, tx storeregistry.DBTX) error {
			if _, err := tx.ExecContext(ctx, "REPLACE INTO brain_store_aliases (alias, `store`) VALUES (?, ?)", newName, canonical); err != nil {
				return fmt.Errorf("recording the alias %s → %s: %w", newName, canonical, err)
			}
			return nil
		}
	}
	return storeregistry.Mutate(ctx, db, meta, storeregistry.PlanRename(oldName, newName, relPath, keepOld))
}
