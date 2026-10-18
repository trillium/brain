package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// The registry-vs-wrapper census: the registry (~/.config/brain/stores.yaml) and
// the wrappers on the host (~/.local/bin/<name>) are two lists that must
// describe the same federation. A registered store with no wrapper is a store
// nothing can reach under the name it is registered under; a generated wrapper
// with no registry entry is a CLI in PATH that resolves a store the federation
// does not know about — the same class of silent misroute the per-store checks
// exist for, seen from the other side. Counting the two lists is how a whole
// class of drift becomes a one-line fact instead of a per-store investigation.
//
// Only files this tool generated count as wrappers here (the generated headers
// are the marker). A hand-written script in ~/.local/bin that happens to share a
// store's name is not this federation's wrapper, and is never deleted or
// claimed by '--fix'.

// wrapperCensus is the registry-vs-wrapper count check for one host.
type wrapperCensus struct {
	Registered int `json:"registered"`
	// WithWrapper and MissingWrapper partition Registered; MissingWrapper
	// entries carry a parenthesised note when the file is there but unusable.
	WithWrapper    []string `json:"with_wrapper"`
	MissingWrapper []string `json:"missing_wrapper"`
	// OrphanWrappers are generated store wrappers on this host whose name is
	// not in the registry (renamed or removed store, or a stale install).
	OrphanWrappers []string `json:"orphan_wrappers"`
	// BackupWrappers are generated wrappers kept as BACKUPS inside the bin
	// directory (<store>.pre-<date>, <store>.bak). They are not orphans — the
	// store they came from is usually registered under its plain name — but
	// they sit in PATH, so the count is reported without treating it as a
	// missing or lost store.
	BackupWrappers []string `json:"backup_wrappers"`
}

// wrapperFileState reports a store's wrapper file: its path, whether a file is
// there, and whether it is executable (a wrapper that exists but cannot be
// executed is not a wrapper the store can be reached through).
func wrapperFileState(name string) (path string, exists, executable bool) {
	path, ok := wrapperPathForStore(name)
	if !ok {
		return "", false, false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return path, false, false
	}
	return path, true, info.Mode()&0o111 != 0
}

// censusWrappersAgainstRegistry counts both lists. It reads files only: nothing
// is created, renamed, or removed here.
func censusWrappersAgainstRegistry(stores map[string]storeEntry) wrapperCensus {
	c := wrapperCensus{Registered: len(stores)}
	for _, name := range sortedKeys(stores) {
		_, exists, executable := wrapperFileState(name)
		switch {
		case exists && executable:
			c.WithWrapper = append(c.WithWrapper, name)
		case exists:
			c.MissingWrapper = append(c.MissingWrapper, name+" (present but not executable)")
		default:
			c.MissingWrapper = append(c.MissingWrapper, name)
		}
	}
	c.OrphanWrappers, c.BackupWrappers = orphanStoreWrappers(stores)
	return c
}

// backupShapedWrapperName reports whether a bin entry is a BACKUP of a wrapper
// rather than a wrapper: '<store>.pre-<date>', '<store>.bak', '<store>.backup'.
func backupShapedWrapperName(name string) bool {
	return strings.Contains(name, ".pre-") || strings.Contains(name, ".bak") || strings.Contains(name, ".backup")
}

// orphanStoreWrappers returns the names of generated store wrappers in
// ~/.local/bin that the registry does not know about, and separately the names
// that are backups of wrappers kept in the bin directory. Both slices are
// sorted.
func orphanStoreWrappers(stores map[string]storeEntry) (orphans, backups []string) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, nil
	}
	ents, err := os.ReadDir(filepath.Join(home, ".local", "bin"))
	if err != nil {
		return nil, nil
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if _, registered := stores[name]; registered {
			continue
		}
		wp := filepath.Join(home, ".local", "bin", name)
		info, err := os.Lstat(wp)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// A symlink is an alias someone made by hand ('tasks -> task'
			// predates 'brain stores alias'), not a wrapper this tool
			// generated: reading through it would blame the target's content.
			continue
		}
		if !isGeneratedStoreWrapper(wp) {
			continue
		}
		if backupShapedWrapperName(name) {
			backups = append(backups, name)
			continue
		}
		// An alias wrapper's own name is never a registry key: it binds to a
		// canonical store by BD_NAME, and it is legitimate while that store is
		// registered ('brain stores alias recipes rcp').
		if canonical, ok := wrapperBoundStoreName(wp); ok {
			if _, registered := stores[canonical]; registered {
				continue
			}
		}
		orphans = append(orphans, name)
	}
	sort.Strings(orphans)
	sort.Strings(backups)
	return orphans, backups
}

// aliasBDNameRe reads the canonical store an alias wrapper pins.
var aliasBDNameRe = regexp.MustCompile(`BD_NAME="?([^"\s]+)"?`)

// wrapperBoundStoreName returns the canonical store an alias wrapper binds to,
// when the file is an alias wrapper. ok is false for a plain store wrapper,
// whose own name is the store.
func wrapperBoundStoreName(path string) (string, bool) {
	data, err := os.ReadFile(path) //nolint:gosec // inspecting our own install dir
	if err != nil || !strings.Contains(string(data), aliasWrapperHeader) {
		return "", false
	}
	m := aliasBDNameRe.FindSubmatch(data)
	if m == nil {
		return "", false
	}
	return string(m[1]), true
}

// censusTally maps the census onto the doctor's tallies: a registered store
// with no wrapper is a warning the convergence verbs clear, and an orphan
// wrapper is a failure nothing repairs automatically (removing a file is
// destructive, so an operator makes that call).
func censusTally(c wrapperCensus) (warns int, failing []string) {
	if len(c.MissingWrapper) > 0 {
		warns++
	}
	return warns, c.OrphanWrappers
}

// isGeneratedStoreWrapper reports whether a file was written by one of the
// verbs that generate wrappers ('brain stores create', 'brain stores alias',
// 'brain remote install'), whatever store it names.
func isGeneratedStoreWrapper(path string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // inspecting our own install dir
	if err != nil {
		return false
	}
	head := string(data)
	if len(head) > 512 {
		head = head[:512] // the marker sits in the header block
	}
	return strings.Contains(head, remoteWrapperMarker) ||
		strings.Contains(head, "Auto-generated by 'brain stores create ") ||
		strings.Contains(head, aliasWrapperHeader)
}

// summary is the one-line form: the counts, then only what is wrong.
func (c wrapperCensus) summary() string {
	line := fmt.Sprintf("Wrappers: %d/%d registered stores have a wrapper", len(c.WithWrapper), c.Registered)
	if len(c.MissingWrapper) > 0 {
		line += "; " + fmt.Sprintf("%d registered with NO wrapper: %s",
			len(c.MissingWrapper), strings.Join(c.MissingWrapper, " "))
	}
	if len(c.OrphanWrappers) > 0 {
		line += "; " + fmt.Sprintf("%d wrapper(s) with NO registry entry: %s",
			len(c.OrphanWrappers), strings.Join(c.OrphanWrappers, " "))
	}
	if len(c.BackupWrappers) > 0 {
		line += "; " + fmt.Sprintf("%d backup copy/copies in PATH (not wrappers): %s",
			len(c.BackupWrappers), strings.Join(c.BackupWrappers, " "))
	}
	if len(c.MissingWrapper) == 0 && len(c.OrphanWrappers) == 0 && len(c.BackupWrappers) == 0 {
		line += "; no orphans"
	}
	return line
}

// fixes names the exact command that clears each finding. Missing wrappers are
// repaired by the convergence verb; a generated wrapper with no registry entry
// is NOT deleted automatically — the operator either registers the store it
// names or removes the file, and both are one command.
//
// Which verb writes a missing wrapper depends on the machine: on a client
// (remote.yaml names a host) a store is reached by a FORWARDING wrapper written
// by 'brain remote install', and generating a local store wrapper there would
// point the command at a store this machine does not serve. The census therefore
// names the client's own install verb.
func (c wrapperCensus) fixes() []string {
	var out []string
	if len(c.MissingWrapper) > 0 {
		if host := remoteClientHost(); host != "" {
			out = append(out, fmt.Sprintf("brain remote install   # this machine is a client of %s: it needs forwarding wrappers, not store wrappers (%s)",
				host, strings.Join(c.MissingWrapper, " ")))
		} else {
			out = append(out, fmt.Sprintf("brain stores rewrite-wrappers   # generates the missing wrapper(s): %s",
				strings.Join(c.MissingWrapper, " ")))
		}
	}
	if len(c.OrphanWrappers) > 0 {
		names := make([]string, 0, len(c.OrphanWrappers))
		for _, n := range c.OrphanWrappers {
			names = append(names, filepath.Join(wrapperBinDir(), n))
		}
		out = append(out, fmt.Sprintf("brain stores add <name> <beads-dir>   # if the store still exists, register it again; else remove the stale wrapper(s): rm %s",
			strings.Join(names, " ")))
	}
	if len(c.BackupWrappers) > 0 {
		out = append(out, fmt.Sprintf("move the backup copy/copies out of PATH (they are NOT wrappers to delete in place): %s",
			filepath.Join(wrapperBinDir(), "<name>")))
	}
	return out
}

// remoteClientHost is the brain host this machine is a CLIENT of, or "" when
// this machine serves its own federation. It is read-only: nothing here changes
// the routing that 'brain remote' manages.
func remoteClientHost() string {
	cfg, err := loadRemoteConfig()
	if err != nil || cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Host)
}

// localInspectionNote explains the machine boundary when a client inspects its
// OWN wrappers. A client forwards every store command to its host by default
// (BRAIN_REMOTE=off runs locally), so a census taken here describes this
// machine's wrappers — not the federation's — and saying so prevents a reader
// from taking "55/55 wrappers" for the host's health.
func localInspectionNote() string {
	host := remoteClientHost()
	if host == "" || !strings.EqualFold(os.Getenv(envRemoteOff), "off") {
		return ""
	}
	return fmt.Sprintf("Note: this machine is a client of %s; the wrapper counts below describe THIS machine (store commands are forwarded there by default, BRAIN_REMOTE=off runs them here).", host)
}

// wrapperBinDir is ~/.local/bin, where the generated wrappers live.
func wrapperBinDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join("~", ".local", "bin")
	}
	return filepath.Join(home, ".local", "bin")
}

// brain stores orphans lists the generated wrappers whose name is not in the
// registry, the half of the census that has no per-store probe.
var brainStoresOrphansCmd = &cobra.Command{
	Use:   "orphans",
	Short: "List generated store wrappers that have no registry entry",
	Long: `List every generated store wrapper in ~/.local/bin whose name is not in the
federation registry (~/.config/brain/stores.yaml).

A wrapper with no registry entry is a CLI in PATH that resolves a store the
federation does not know about: either the store was renamed or removed and the
wrapper was left behind, or it was never registered. Only files this tool
generated count (the generated header is the marker), so a hand-written script
that happens to share a store's name is never reported — or touched.

Nothing is deleted: register the store again with 'brain stores add <name>
<beads-dir>', or remove the file yourself.`,
	Args: cobra.NoArgs,
	Run: func(_ *cobra.Command, _ []string) {
		stores, err := loadStoresRegistry()
		if err != nil {
			FatalError("loading registry: %v", err)
		}
		census := censusWrappersAgainstRegistry(stores)
		if jsonOutput {
			if err := outputJSON(census); err != nil {
				FatalError("encoding census: %v", err)
			}
		} else {
			fmt.Println(census.summary())
			for _, f := range census.fixes() {
				fmt.Printf("  fix: %s\n", f)
			}
		}
		if len(census.OrphanWrappers) > 0 {
			os.Exit(1)
		}
	},
}

func init() {
	brainStoresCmd.AddCommand(brainStoresOrphansCmd)
}
