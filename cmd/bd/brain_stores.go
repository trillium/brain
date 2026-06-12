package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
	brainStoresCmd.AddCommand(brainStoresAddCmd)
	brainStoresCmd.AddCommand(brainStoresRemoveCmd)
	brainStoresCmd.AddCommand(brainStoresListCmd)
	brainStoresCmd.AddCommand(brainStoresEnvCmd)
	brainCmd.AddCommand(brainStoresCmd)
	rootCmd.AddCommand(brainStoresCmd)
}
