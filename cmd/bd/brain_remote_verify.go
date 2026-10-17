package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
)

var (
	remoteVerifyDir     string
	remoteVerifyTimeout time.Duration
)

// verifyRow is the verdict for one store command.
type verifyRow struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | empty | fail
	Prefix string `json:"prefix,omitempty"`
	Note   string `json:"note,omitempty"`
}

// storeListFunc runs '<wrapper> list --limit 1' and returns the id prefix of
// the first bead ("" for an empty store).
type storeListFunc func(path string) (prefix string, err error)

// idPrefix is everything before the last '-' of a bead id ("task-oxsbt" →
// "task", "tech_talks-1f.2" → "tech_talks"). The suffix never contains '-'.
func idPrefix(id string) string {
	if i := strings.LastIndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	return ""
}

// wrapperPinsStore reports whether the wrapper at path hands over to the host
// as store name, i.e. it sets BRAIN_REMOTE_STORE='<name>'.
func wrapperPinsStore(path, name string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // inspecting our own install dir
	if err != nil {
		return false
	}
	return strings.Contains(string(data), envRemoteStore+"="+remoteQuote(name))
}

// verifyClientWrappers checks every store command in names under dir:
//  1. the wrapper exists and, in client mode (remote), pins BRAIN_REMOTE_STORE
//     to its own name rather than being an old local wrapper;
//  2. '<name> list --limit 1' answers with that store's own id prefix: the
//     name itself, or at least not another store's name, and not a prefix
//     already claimed by a different store (the misroute that sends 'status'
//     into the brain store).
//
// The brain binary itself (the 'brain' store's command) is not a wrapper, so
// only check 2 applies to it. In client mode legacy wrappers in dir that no
// host store accounts for are flagged too.
func verifyClientWrappers(dir, brainBinary string, names []string, remote bool, list storeListFunc) []verifyRow {
	known := make(map[string]bool, len(names))
	for _, n := range names {
		known[n] = true
	}
	rows := make([]verifyRow, len(names))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			row := verifyRow{Name: name, Status: "fail"}
			path := filepath.Join(dir, name)
			isBinary := name == filepath.Base(brainBinary) && filepath.Dir(brainBinary) == dir
			_, statErr := os.Stat(path)
			switch {
			case statErr != nil:
				row.Note = "no wrapper in " + dir
			case remote && !isBinary && isLegacyStoreWrapper(path):
				row.Note = "legacy local wrapper: not switched to the host (run brain remote install)"
			case remote && !isBinary && !wrapperPinsStore(path, name):
				row.Note = "wrapper lacks " + envRemoteStore + "='" + name + "'"
			default:
				prefix, err := list(path)
				row.Prefix = prefix
				switch {
				case err != nil:
					row.Note = err.Error()
				case prefix == "":
					row.Status, row.Note = "empty", "no beads, prefix not confirmed"
				default:
					row.Status = "ok"
				}
			}
			rows[i] = row
		}()
	}
	wg.Wait()

	// Cross-check prefixes: a store may answer with its own name or an alias
	// (robots → agent) but never with another store's name or a shared alias.
	claimed := map[string]string{}
	for i := range rows {
		r := &rows[i]
		if r.Status != "ok" || r.Prefix == r.Name {
			continue
		}
		switch {
		case known[r.Prefix]:
			r.Status, r.Note = "fail", fmt.Sprintf("returns %s- ids: routed into the %s store", r.Prefix, r.Prefix)
		case claimed[r.Prefix] != "":
			r.Status, r.Note = "fail", fmt.Sprintf("shares prefix %s- with %s", r.Prefix, claimed[r.Prefix])
		default:
			claimed[r.Prefix] = r.Name
			r.Note = "alias prefix " + r.Prefix + "-"
		}
	}

	if remote {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			// Backups such as status.pre-x.bak are not commands (names have no dots).
			if known[e.Name()] || e.IsDir() || !remoteNameRE.MatchString(e.Name()) || !isLegacyStoreWrapper(filepath.Join(dir, e.Name())) {
				continue
			}
			rows = append(rows, verifyRow{Name: e.Name(), Status: "fail", Note: "legacy local wrapper still in " + dir + ", not in the host registry (brain remote install --also " + e.Name() + ")"})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

// runStoreList executes a wrapper as '<wrapper> list --limit 1 --json' and
// returns the first bead's id prefix.
func runStoreList(ctx context.Context, timeout time.Duration) storeListFunc {
	once := runStoreListOnce(ctx, timeout)
	return func(path string) (string, error) {
		// One retry: the host's shared Dolt server can refuse a connection
		// under the parallel load this check creates.
		prefix, err := once(path)
		if err != nil {
			time.Sleep(time.Second)
			prefix, err = once(path)
		}
		return prefix, err
	}
}

func runStoreListOnce(ctx context.Context, timeout time.Duration) storeListFunc {
	return func(path string) (string, error) {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(cctx, path, "list", "--limit", "1", "--json") //nolint:gosec // our own wrapper
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(stderr.String())
			if len(msg) > 160 {
				msg = msg[:160]
			}
			return "", fmt.Errorf("list failed: %v: %s", err, msg)
		}
		data := stdout.Bytes()
		start := bytes.IndexByte(data, '[')
		if start < 0 {
			return "", fmt.Errorf("list printed no JSON array: %q", strings.TrimSpace(string(data)))
		}
		var beads []struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(bytes.NewReader(data[start:])).Decode(&beads); err != nil {
			return "", fmt.Errorf("parsing list output: %w", err)
		}
		if len(beads) == 0 {
			return "", nil
		}
		return idPrefix(beads[0].ID), nil
	}
}

// localStoreNames lists the registry stores on this machine (host mode).
func localStoreNames(ctx context.Context, self string) ([]string, error) {
	cmd := exec.CommandContext(ctx, self, "stores", "list", "--json") //nolint:gosec // this binary
	cmd.Env = append(os.Environ(), envRemoteOff+"=off")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("reading this machine's store registry: %w", err)
	}
	return parseStoreRegistryJSON(out)
}

var brainRemoteVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Check every store command routes to its own store (exit 1 if not)",
	Long: `Checks the wrappers in --dir (default: this binary's directory) for every
store in the registry:

  client mode  every wrapper pins BRAIN_REMOTE_STORE='<store>' (no old local
               wrapper left), and no legacy wrapper lingers in the directory;
  every mode   '<store> list --limit 1' returns that store's own id prefix, not
               another store's (e.g. 'status' answering with brain- ids).

On a client the store names come from the host's registry; on the host (no
brain host set) from the local registry, and the pin check is skipped.
Empty stores are reported as 'empty' (prefix unconfirmed), not as failures.`,
	Args:        cobra.NoArgs,
	Annotations: map[string]string{"brain-remote": "1"},
	Run: func(cmd *cobra.Command, _ []string) {
		cfg, err := loadRemoteConfig()
		if err != nil {
			FatalError("%v", err)
		}
		self, err := os.Executable()
		if err != nil {
			FatalError("locating this binary: %v", err)
		}
		if resolved, rerr := filepath.EvalSymlinks(self); rerr == nil {
			self = resolved
		}
		dir := remoteVerifyDir
		if dir == "" {
			dir = filepath.Dir(self)
		}
		if abs, aerr := filepath.Abs(dir); aerr == nil {
			dir = abs
		}
		remote := cfg.Host != ""
		var names []string
		if remote {
			if err := cfg.validate(); err != nil {
				FatalError("%v", err)
			}
			var missing []string
			if names, missing, err = hostStoreNames(remoteCtx(cmd), cfg); err != nil {
				FatalError("%v", err)
			}
			// A host store with no wrapper on the host has nothing to verify.
			_ = missing
		} else if names, err = localStoreNames(remoteCtx(cmd), self); err != nil {
			FatalError("%v", err)
		}
		rows := verifyClientWrappers(dir, self, names, remote, runStoreList(remoteCtx(cmd), remoteVerifyTimeout))

		counts := map[string]int{}
		for _, r := range rows {
			counts[r.Status]++
		}
		mode := "host (local stores)"
		if remote {
			mode = "client of " + cfg.Host
		}
		if jsonOutput {
			outputJSON(map[string]any{"mode": mode, "dir": dir, "ok": counts["fail"] == 0, "counts": counts, "stores": rows})
		} else {
			for _, r := range rows {
				fmt.Printf("  %-5s %-18s %-14s %s\n", r.Status, r.Name, r.Prefix, r.Note)
			}
			fmt.Printf("%d ok, %d empty, %d fail — %s, wrappers in %s\n", counts["ok"], counts["empty"], counts["fail"], mode, dir)
		}
		if counts["fail"] > 0 {
			os.Exit(1)
		}
	},
}

func init() {
	brainRemoteVerifyCmd.Flags().StringVar(&remoteVerifyDir, "dir", "", "Directory holding the wrappers (default: this binary's directory)")
	brainRemoteVerifyCmd.Flags().DurationVar(&remoteVerifyTimeout, "timeout", 60*time.Second, "Per-store timeout for 'list'")
	brainRemoteCmd.AddCommand(brainRemoteVerifyCmd)
}
