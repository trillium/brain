package main

import (
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

// storesRegistry is the on-disk shape of ~/.config/pai/stores.yaml.
type storesRegistry struct {
	Stores map[string]string `yaml:"stores"`
}

func storesYamlFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, storesYamlPath)
}

func loadStoresRegistry() (map[string]string, error) {
	path := storesYamlFile()
	data, err := os.ReadFile(path) //nolint:gosec
	if os.IsNotExist(err) {
		return make(map[string]string), nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var reg storesRegistry
	if err := yaml.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if reg.Stores == nil {
		return make(map[string]string), nil
	}
	return reg.Stores, nil
}

func saveStoresRegistry(stores map[string]string) error {
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
		stores[name] = beadsDir
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
				result = append(result, map[string]string{"name": n, "path": stores[n]})
			}
			outputJSON(result)
			return
		}
		for _, n := range names {
			fmt.Printf("  %-16s %s\n", n, stores[n])
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
	stores[name] = beadsDir
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
func regenerateStoresEnv(stores map[string]string) (string, error) {
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
		sb.WriteString(fmt.Sprintf("export %s=%q\n", varName, stores[n]))
	}
	sb.WriteString(fmt.Sprintf("\nexport PAI_STORES_LIST=%q\n", strings.Join(names, ":")))

	if err := os.WriteFile(outPath, []byte(sb.String()), 0o644); err != nil { //nolint:gosec
		return "", fmt.Errorf("writing %s: %w", outPath, err)
	}
	return outPath, nil
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
			sb.WriteString(fmt.Sprintf("export %s=%q\n", varName, stores[n]))
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

	brainStoresCmd.AddCommand(brainStoresAddCmd)
	brainStoresCmd.AddCommand(brainStoresRemoveCmd)
	brainStoresCmd.AddCommand(brainStoresListCmd)
	brainStoresCmd.AddCommand(brainStoresCreateCmd)
	brainStoresCmd.AddCommand(brainStoresEnvCmd)
	brainCmd.AddCommand(brainStoresCmd)
	rootCmd.AddCommand(brainStoresCmd)
}
