package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/ui"
	"gopkg.in/yaml.v3"
)

// storesYamlPath is the canonical registry file for PAI federation stores.
const storesYamlPath = ".config/pai/stores.yaml"

// storesEnvPath is the shell-sourceable export of the registry.
const storesEnvPath = ".config/pai/stores.env"

// storeEntry is the registry value for a single federated store. The Path
// points at the store's .beads directory; About is an optional human blurb
// describing what the store is for (set via 'brain stores set-about').
type storeEntry struct {
	Path  string `yaml:"path"`
	About string `yaml:"about,omitempty"`
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

// storesRegistry is the on-disk shape of ~/.config/pai/stores.yaml.
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

func loadStoresRegistry() (map[string]storeEntry, error) {
	path := storesYamlFile()
	data, err := os.ReadFile(path) //nolint:gosec
	if os.IsNotExist(err) {
		return make(map[string]storeEntry), nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var reg storesRegistry
	if err := yaml.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if reg.Stores == nil {
		return make(map[string]storeEntry), nil
	}
	return reg.Stores, nil
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
	header := "# PAI federation store registry — managed by 'brain stores'\n# Do not edit manually; use 'brain stores add/remove'.\n\n"
	return os.WriteFile(path, append([]byte(header), data...), 0o644) //nolint:gosec
}

var brainStoresCmd = &cobra.Command{
	Use:   "stores",
	Short: "Manage the brain federation store registry",
	Long: `Manage the registry of bd stores federated under brain.

The registry lives at ~/.config/pai/stores.yaml. Each registered store
can be searched via 'brain search', transferred to via 'brain transfer',
and synced via 'brain repo sync'.

Run 'brain stores env' to regenerate ~/.config/pai/stores.env for
shell wrapper scripts that need PAI_STORE_* variables.`,
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
			result := make([]map[string]string, 0, len(names))
			for _, n := range names {
				result = append(result, map[string]string{
					"name":  n,
					"path":  stores[n].Path,
					"about": stores[n].About,
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
				fmt.Printf("  %-16s %-48s %s\n", n, stores[n].Path, about)
			} else {
				fmt.Printf("  %-16s %s\n", n, stores[n].Path)
			}
		}
	},
}

var (
	createStorePath     string
	createStoreNoWrap   bool
	createStoreBinary   string
	createStoreInitName string
	createStoreInitMail string
)

var brainStoresCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Provision a new connected store (dolt init + entries dir + wrapper + registry)",
	Long: `Provision a new connected store end-to-end. Does, in order:

  1. Create <path>/.beads/ and run 'dolt init' inside it.
  2. Create <path>/entries/ for exfiltrated markdown.
  3. Write a CLI wrapper at ~/.local/bin/<name> that pins BEADS_DIR
     and BD_NAME, then exec's bd.
  4. Register the store in ~/.config/pai/stores.yaml.
  5. Regenerate ~/.config/pai/stores.env.

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

	// Step 1: dolt init the .beads dir.
	if err := initDoltStore(beadsDir); err != nil {
		FatalError("dolt init at %s: %v", beadsDir, err)
	}

	// Step 2: entries/ sibling for exfiltration.
	if err := os.MkdirAll(entriesDir, 0o755); err != nil {
		FatalError("creating %s: %v", entriesDir, err)
	}

	// Step 3: wrapper at ~/.local/bin/<name>.
	wrapperPath := ""
	if !createStoreNoWrap {
		wp, err := writeStoreWrapper(name, beadsDir, createStoreBinary)
		if err != nil {
			FatalError("writing wrapper: %v", err)
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
exec env BEADS_DIR=%q BD_NAME=%q %s "$@"
`, name, beadsDir, name, bdBinary)
	if err := os.WriteFile(wrapperPath, []byte(body), 0o755); err != nil { //nolint:gosec
		return "", fmt.Errorf("writing %s: %w", wrapperPath, err)
	}
	return wrapperPath, nil
}

// regenerateStoresEnv writes ~/.config/pai/stores.env from the registry,
// matching the format `brain stores env` emits. Returns the written path.
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
	sb.WriteString("# Source in store wrapper scripts to get PAI_STORE_* vars.\n")
	sb.WriteString("# Regenerate: brain stores env\n\n")
	for _, n := range names {
		varName := "PAI_STORE_" + strings.ToUpper(strings.ReplaceAll(n, "-", "_"))
		sb.WriteString(fmt.Sprintf("export %s=%q\n", varName, stores[n].Path))
	}
	sb.WriteString(fmt.Sprintf("\nexport PAI_STORES_LIST=%q\n", strings.Join(names, ":")))

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
	Long: `Iterate every store registered in ~/.config/pai/stores.yaml and
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
exec env BEADS_DIR=%q BD_NAME=%q %s "$@"
`, canonical, alias, alias, canonical, beadsDir, canonical, bdBinary)
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
  3. Update ~/.config/pai/stores.yaml: <old-name> → <new-name>.
  4. Regenerate ~/.config/pai/stores.env.

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
registry (~/.config/pai/stores.yaml) only. The wrapper script is left
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
	Short: "Write ~/.config/pai/stores.env from the registry (for shell wrappers)",
	Run: func(cmd *cobra.Command, args []string) {
		stores, err := loadStoresRegistry()
		if err != nil {
			FatalError("loading registry: %v", err)
		}

		home, err := os.UserHomeDir()
		if err != nil {
			FatalError("resolving home dir: %v", err)
		}
		outPath := filepath.Join(home, storesEnvPath)
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			FatalError("creating %s: %v", filepath.Dir(outPath), err)
		}

		names := sortedKeys(stores)

		var sb strings.Builder
		sb.WriteString("# Auto-generated by 'brain stores env' — do not edit manually.\n")
		sb.WriteString("# Source in store wrapper scripts to get PAI_STORE_* vars.\n")
		sb.WriteString("# Regenerate: brain stores env\n\n")
		for _, n := range names {
			varName := "PAI_STORE_" + strings.ToUpper(strings.ReplaceAll(n, "-", "_"))
			sb.WriteString(fmt.Sprintf("export %s=%q\n", varName, stores[n].Path))
		}
		sb.WriteString(fmt.Sprintf("\nexport PAI_STORES_LIST=%q\n", strings.Join(names, ":")))

		if err := os.WriteFile(outPath, []byte(sb.String()), 0o644); err != nil { //nolint:gosec
			FatalError("writing %s: %v", outPath, err)
		}

		if jsonOutput {
			outputJSON(map[string]string{"path": outPath, "stores": strings.Join(names, ",")})
			return
		}
		fmt.Printf("%s Wrote %s (%d stores)\n", ui.RenderPass("✓"), outPath, len(stores))
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
	brainStoresCmd.AddCommand(brainStoresEnvCmd)
	brainCmd.AddCommand(brainStoresCmd)
	rootCmd.AddCommand(brainStoresCmd)
}
