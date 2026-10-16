package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/configfile"
)

var (
	storesRewriteJobs    int
	storesRewriteTimeout time.Duration
)

// wrapperRewrite is the outcome for one store: its wrapper was regenerated
// and then read through (the same probe 'brain stores doctor' uses).
type wrapperRewrite struct {
	Name    string `json:"name"`
	Wrapper string `json:"wrapper,omitempty"`
	Mode    string `json:"mode,omitempty"` // "server" | "embedded"
	Status  string `json:"status"`         // "ok" | "fail"
	Reason  string `json:"reason,omitempty"`
}

// rewriteStoreWrapper regenerates one store's ~/.local/bin wrapper from its
// registry entry and on-disk metadata.json. A store whose metadata.json says
// dolt_mode=server gets the shared-server wrapper; anything else gets the
// minimal BEADS_DIR/BD_NAME one. Either way BEADS_DIR is $HOME-relative.
func rewriteStoreWrapper(name string, entry storeEntry) (path, mode string, err error) {
	beadsDir := expandPath(entry.Path)
	if cfg, lerr := configfile.Load(beadsDir); lerr == nil && cfg != nil &&
		strings.EqualFold(cfg.DoltMode, configfile.DoltModeServer) {
		path, err = writeServerStoreWrapper(name, beadsDir, "", cfg.GetDoltServerHost(), cfg.GetDoltServerPort(), cfg.GetDoltDatabase())
		return path, "server", err
	}
	path, err = writeStoreWrapper(name, beadsDir, "")
	return path, "embedded", err
}

// rewriteAllStoreWrappers regenerates the wrapper of every registered store,
// then verifies each by reading through it. A store counts as ok only when its
// wrapper resolves a database; anything else is reported as a failure.
func rewriteAllStoreWrappers(self string, stores map[string]storeEntry, jobs int, timeout time.Duration) []wrapperRewrite {
	names := sortedKeys(stores)
	results := make([]wrapperRewrite, len(names))
	if jobs < 1 {
		jobs = 1
	}
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, name string) {
			defer wg.Done()
			defer func() { <-sem }()
			r := wrapperRewrite{Name: name}
			wp, mode, err := rewriteStoreWrapper(name, stores[name])
			r.Wrapper, r.Mode = wp, mode
			if err != nil {
				r.Status, r.Reason = "fail", "rewriting wrapper: "+err.Error()
				results[i] = r
				return
			}
			h := probeStore(self, name, stores[name], timeout)
			switch {
			case h.Status == "fail":
				r.Status, r.Reason = "fail", "wrapper does not resolve a database: "+h.Reason
			case h.Probe != "wrapper":
				r.Status, r.Reason = "fail", "wrapper not executable after rewrite"
			default:
				r.Status = "ok"
			}
			results[i] = r
		}(i, name)
	}
	wg.Wait()
	return results
}

func failedRewrites(rs []wrapperRewrite) []wrapperRewrite {
	var out []wrapperRewrite
	for _, r := range rs {
		if r.Status != "ok" {
			out = append(out, r)
		}
	}
	return out
}

var brainStoresRewriteWrappersCmd = &cobra.Command{
	Use:   "rewrite-wrappers",
	Short: "Regenerate every store's CLI wrapper ($HOME-relative) and verify each resolves a database",
	Long: `Rewrites ~/.local/bin/<store> for every store in the registry using the
current generator (BEADS_DIR is "$HOME/…", never an absolute home path), then
reads through each wrapper the way 'brain stores doctor' does.

Exits non-zero, naming every store that failed, if any wrapper cannot be
written or does not resolve a database. 'brain remote set' and
'brain remote install' run this on the brain host.`,
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		stores, err := loadStoresRegistry()
		if err != nil {
			FatalError("loading registry: %v", err)
		}
		self, err := os.Executable()
		if err != nil || self == "" {
			self = os.Args[0]
		}
		results := rewriteAllStoreWrappers(self, stores, storesRewriteJobs, storesRewriteTimeout)
		failed := failedRewrites(results)
		if jsonOutput {
			outputJSON(map[string]any{"total": len(results), "failed": len(failed), "stores": results})
		} else {
			for _, r := range results {
				if r.Status == "ok" {
					fmt.Printf("  ok    %-18s %s\n", r.Name, r.Wrapper)
				} else {
					fmt.Printf("  FAIL  %-18s %s\n", r.Name, r.Reason)
				}
			}
			fmt.Printf("%d/%d wrappers rewritten and verified\n", len(results)-len(failed), len(results))
		}
		if len(failed) > 0 {
			var n []string
			for _, r := range failed {
				n = append(n, r.Name)
			}
			sort.Strings(n)
			fmt.Fprintf(os.Stderr, "FAILING STORES: %s\n", strings.Join(n, " "))
			os.Exit(1)
		}
	},
}

// verifyHostWrappers runs 'brain stores rewrite-wrappers' on the host and
// returns an error listing every store whose wrapper failed. It is the gate
// behind 'brain remote set' and 'brain remote install': a host whose wrappers
// cannot resolve a database must not look successfully configured.
func verifyHostWrappers(ctx context.Context, cfg *remoteConfig) (total int, err error) {
	out, runErr := remoteCapture(ctx, cfg, "brain", "stores", "rewrite-wrappers", "--json")
	results, perr := parseRewriteResults(out)
	if perr != nil {
		if runErr != nil {
			return 0, fmt.Errorf("rewriting wrappers on %s: %w", cfg.Host, runErr)
		}
		return 0, fmt.Errorf("rewriting wrappers on %s: %w", cfg.Host, perr)
	}
	if failed := failedRewrites(results); len(failed) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "%d of %d store wrappers on %s failed to resolve a database:", len(failed), len(results), cfg.Host)
		for _, r := range failed {
			fmt.Fprintf(&b, "\n  %s: %s", r.Name, r.Reason)
		}
		return len(results), fmt.Errorf("%s", b.String())
	}
	if runErr != nil {
		return len(results), fmt.Errorf("rewriting wrappers on %s: %w", cfg.Host, runErr)
	}
	return len(results), nil
}

func parseRewriteResults(data []byte) ([]wrapperRewrite, error) {
	start := strings.Index(string(data), "{")
	if start < 0 {
		return nil, fmt.Errorf("host printed no JSON result: %q", strings.TrimSpace(string(data)))
	}
	var payload struct {
		Stores []wrapperRewrite `json:"stores"`
	}
	if err := json.Unmarshal(data[start:], &payload); err != nil {
		return nil, fmt.Errorf("parsing host result: %w", err)
	}
	return payload.Stores, nil
}

func init() {
	brainStoresRewriteWrappersCmd.Flags().IntVar(&storesRewriteJobs, "jobs", 8, "Verify this many stores concurrently")
	brainStoresRewriteWrappersCmd.Flags().DurationVar(&storesRewriteTimeout, "timeout", defaultStoreProbeTimeout, "Per-store read timeout")
	brainStoresCmd.AddCommand(brainStoresRewriteWrappersCmd)
}
