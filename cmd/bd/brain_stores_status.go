package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
)

var (
	storesStatusJSON     bool
	storesStatusJobs     int
	storesStatusTimeout  time.Duration
	storesStatusRecent   time.Duration
	storesStatusWatch    bool
	storesStatusInterval time.Duration
	storesStatusPolls    int
)

// storeSnapshot is one store's point-in-time state: what the monitor
// dashboard shows per row and what --watch diffs between polls.
type storeSnapshot struct {
	Name      string         `json:"name"`
	Reachable bool           `json:"reachable"`
	Reason    string         `json:"reason,omitempty"`
	Total     int            `json:"total"`
	ByStatus  map[string]int `json:"by_status,omitempty"`
	Open      int            `json:"open"`
	Ready     int            `json:"ready"`
	Updated   int            `json:"updated"` // beads updated since the window start
	Millis    int64          `json:"ms"`
	// ProbedAt is when this probe began; the next watch poll counts updates
	// since then.
	ProbedAt time.Time `json:"probed_at"`
}

// storeRunner runs one bd verb against a registered store and returns its
// stdout. It is a seam so the snapshot and diff logic is testable without
// a live federation.
type storeRunner func(ctx context.Context, name string, entry storeEntry, args ...string) ([]byte, error)

// execStoreRunner reaches the store the way an agent would: through its CLI
// wrapper, or — for a store registered with --no-wrapper — through this binary
// with BEADS_DIR pinned.
func execStoreRunner(ctx context.Context, name string, entry storeEntry, args ...string) ([]byte, error) {
	var sub *exec.Cmd
	if wrapper, ok := storeWrapperPath(name); ok {
		sub = exec.CommandContext(ctx, wrapper, args...)
		sub.Env = os.Environ()
	} else {
		self, err := os.Executable()
		if err != nil || self == "" {
			self = os.Args[0]
		}
		sub = exec.CommandContext(ctx, self, args...) //nolint:gosec // G702: self is os.Executable()
		sub.Env = append(os.Environ(), "BEADS_DIR="+expandPath(entry.Path), "BD_NAME="+name)
	}
	sub.Env = append(sub.Env, "BRAIN_NO_AUTO_FEATURE_REQUEST=1")
	sub.WaitDelay = probeWaitDelay
	out, err := sub.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out, fmt.Errorf("%s", failureReason(out, ee.Stderr))
		}
	}
	return out, err
}

// failureReason picks the line that says why a verb failed: bd prints advisory
// "Warning:" lines first (permissions, schema skew) and, in --json mode, the
// error as a JSON object on stdout, so neither the first stderr line nor the
// exit status alone names the cause.
func failureReason(stdout, stderr []byte) string {
	var je struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(stdout, &je) == nil && je.Error != "" {
		return doctorFirstLine([]byte(je.Error))
	}
	for _, line := range strings.Split(string(stderr), "\n") {
		if s := strings.TrimSpace(line); s != "" && !strings.HasPrefix(s, "Warning:") {
			return s
		}
	}
	return doctorFirstLine(stderr)
}

// snapshotStore reads a store's counts: by status, ready, and beads updated
// since `since`. Any failing read marks the whole store unreachable, so a
// half-read store never reports misleading zeros.
func snapshotStore(run storeRunner, name string, entry storeEntry, since time.Time, timeout time.Duration) storeSnapshot {
	snap := storeSnapshot{Name: name, ProbedAt: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	fail := func(what string, err error) storeSnapshot {
		snap.Millis = time.Since(snap.ProbedAt).Milliseconds()
		snap.Reason = what + ": " + err.Error()
		if ctx.Err() != nil {
			snap.Reason = fmt.Sprintf("%s timed out after %s", what, timeout)
		}
		return snap
	}

	out, err := run(ctx, name, entry, "count", "--by-status", "--json")
	if err != nil {
		return fail("count", err)
	}
	var byStatus struct {
		Groups []struct {
			Count int    `json:"count"`
			Group string `json:"group"`
		} `json:"groups"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(out, &byStatus); err != nil {
		return fail("count", fmt.Errorf("unparseable output: %w", err))
	}
	snap.ByStatus = map[string]int{}
	for _, g := range byStatus.Groups {
		snap.ByStatus[g.Group] = g.Count
		snap.Total += g.Count
	}
	if byStatus.Total > 0 {
		snap.Total = byStatus.Total
	}
	snap.Open = snap.ByStatus["open"]

	out, err = run(ctx, name, entry, "ready", "--brief", "--json", "--limit", "0")
	if err != nil {
		return fail("ready", err)
	}
	var ready []json.RawMessage
	if err := json.Unmarshal(out, &ready); err != nil {
		return fail("ready", fmt.Errorf("unparseable output: %w", err))
	}
	snap.Ready = len(ready)

	out, err = run(ctx, name, entry, "count", "--updated-after", since.UTC().Format(time.RFC3339), "--json")
	if err != nil {
		return fail("count --updated-after", err)
	}
	var updated struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(out, &updated); err != nil {
		return fail("count --updated-after", fmt.Errorf("unparseable output: %w", err))
	}
	snap.Updated = updated.Count

	snap.Reachable = true
	snap.Millis = time.Since(snap.ProbedAt).Milliseconds()
	return snap
}

// snapshotFleet snapshots every store in names concurrently. since returns the
// window start for a store, so watch mode can count from each store's last
// probe.
func snapshotFleet(run storeRunner, stores map[string]storeEntry, names []string, since func(string) time.Time, jobs int, timeout time.Duration) []storeSnapshot {
	if jobs < 1 {
		jobs = 1
	}
	results := make([]storeSnapshot, len(names))
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = snapshotStore(run, name, stores[name], since(name), timeout)
		}(i, name)
	}
	wg.Wait()
	return results
}

// diffStoreSnapshots returns one line per observable change between two polls of
// the same store: reachability flips, count movement, and edits that left the
// counts alone. Nothing changed → no lines (a silent poll).
func diffStoreSnapshots(prev, cur storeSnapshot) []string {
	var lines []string
	switch {
	case prev.Reachable && !cur.Reachable:
		return []string{fmt.Sprintf("%s down: %s", cur.Name, cur.Reason)}
	case !prev.Reachable && cur.Reachable:
		lines = append(lines, fmt.Sprintf("%s up: %d beads (%d open, %d ready)", cur.Name, cur.Total, cur.Open, cur.Ready))
		return lines
	case !cur.Reachable:
		return nil // still down; already reported
	}
	var parts []string
	for _, f := range []struct {
		label    string
		old, now int
	}{
		{"total", prev.Total, cur.Total},
		{"open", prev.Open, cur.Open},
		{"ready", prev.Ready, cur.Ready},
	} {
		if f.old != f.now {
			parts = append(parts, fmt.Sprintf("%s %d→%d", f.label, f.old, f.now))
		}
	}
	if len(parts) == 0 && cur.Updated == 0 {
		return nil
	}
	if cur.Updated > 0 {
		parts = append(parts, fmt.Sprintf("%d updated", cur.Updated))
	}
	return append(lines, fmt.Sprintf("%s changed: %s", cur.Name, strings.Join(parts, ", ")))
}

var brainStoresStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Live per-store counts across the federation, with an optional change feed",
	Long: `Show every store in the registry at a glance: total, open and ready beads
and how many were updated within --recent, plus a fleet roll-up. Stores are
read through their own CLI wrappers (the same path 'stores doctor' probes), so
the registry decides what is monitored and a store missing from the shared
Dolt server shows as down rather than being silently dropped.

With --watch it keeps polling every --interval and prints one line per change
(counts moved, beads edited, a store going down or coming back), nothing when
nothing changed — the shape the Monitor tool consumes. --json emits one JSON
object per poll (a snapshot, then per-change events in watch mode).

Exit codes (without --watch): 0 every store answered, 1 at least one is down
(with a 'FAILING STORES: <names>' line).`,
	Args: cobra.NoArgs,
	Run:  runBrainStoresStatus,
}

func runBrainStoresStatus(cmd *cobra.Command, _ []string) {
	stores, err := loadStoresRegistry()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: loading registry: %v\n", err)
		fmt.Fprintf(os.Stderr, "FAILING STORES: %s\n", doctorRegistrySentinel)
		os.Exit(1)
	}
	if len(stores) == 0 {
		fmt.Println("No stores registered. Use 'brain stores create <name>' or 'brain stores add <name> <beads-dir>'.")
		return
	}
	timeout := storesStatusTimeout
	if timeout <= 0 {
		timeout = defaultStoreProbeTimeout
	}
	names := sortedKeys(stores)
	failed := runStoresStatus(os.Stdout, execStoreRunner, stores, names, statusOpts{
		JSON: storesStatusJSON, Jobs: storesStatusJobs, Timeout: timeout,
		Recent: storesStatusRecent, Watch: storesStatusWatch,
		Interval: storesStatusInterval, Polls: storesStatusPolls,
	})
	if len(failed) > 0 && !storesStatusWatch {
		fmt.Fprintf(os.Stderr, "FAILING STORES: %s\n", strings.Join(failed, " "))
		os.Exit(1)
	}
}

type statusOpts struct {
	JSON     bool
	Jobs     int
	Timeout  time.Duration
	Recent   time.Duration
	Watch    bool
	Interval time.Duration
	Polls    int // watch: stop after this many polls (0 = until interrupted)
}

// runStoresStatus prints the snapshot (and, in watch mode, the change feed)
// and returns the names of stores that were down at the last poll.
func runStoresStatus(w io.Writer, run storeRunner, stores map[string]storeEntry, names []string, o statusOpts) []string {
	windowStart := time.Now().Add(-o.Recent)
	snaps := snapshotFleet(run, stores, names, func(string) time.Time { return windowStart }, o.Jobs, o.Timeout)
	printSnapshot(w, snaps, o)
	if !o.Watch {
		return downStores(snaps)
	}

	prev := map[string]storeSnapshot{}
	for _, s := range snaps {
		prev[s.Name] = s
	}
	interval := o.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	for poll := 1; o.Polls == 0 || poll <= o.Polls; poll++ {
		time.Sleep(interval)
		cur := snapshotFleet(run, stores, names, func(n string) time.Time { return prev[n].ProbedAt }, o.Jobs, o.Timeout)
		for _, s := range cur {
			for _, line := range diffStoreSnapshots(prev[s.Name], s) {
				if o.JSON {
					b, _ := json.Marshal(map[string]interface{}{
						"event": line, "store": s.Name, "ts": time.Now().UTC().Format(time.RFC3339), "snapshot": s,
					})
					fmt.Fprintln(w, string(b))
				} else {
					fmt.Fprintf(w, "%s %s\n", time.Now().Format("15:04:05"), line)
				}
			}
			prev[s.Name] = s
		}
		snaps = cur
	}
	return downStores(snaps)
}

func downStores(snaps []storeSnapshot) []string {
	var down []string
	for _, s := range snaps {
		if !s.Reachable {
			down = append(down, s.Name)
		}
	}
	return down
}

func printSnapshot(w io.Writer, snaps []storeSnapshot, o statusOpts) {
	var total, open, ready, updated, up int
	for _, s := range snaps {
		if s.Reachable {
			up++
			total += s.Total
			open += s.Open
			ready += s.Ready
			updated += s.Updated
		}
	}
	if o.JSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]interface{}{
			"stores": snaps, "reachable": up, "total_stores": len(snaps),
			"beads": total, "open": open, "ready": ready, "updated": updated,
			"recent": o.Recent.String(),
		})
		return
	}
	sorted := append([]storeSnapshot(nil), snaps...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	width := len("STORE")
	for _, s := range sorted {
		if len(s.Name) > width {
			width = len(s.Name)
		}
	}
	fmt.Fprintf(w, "%-*s  %7s  %6s  %6s  %8s\n", width, "STORE", "TOTAL", "OPEN", "READY", "UPDATED")
	for _, s := range sorted {
		if !s.Reachable {
			fmt.Fprintf(w, "%-*s  DOWN  %s\n", width, s.Name, s.Reason)
			continue
		}
		fmt.Fprintf(w, "%-*s  %7d  %6d  %6d  %8d\n", width, s.Name, s.Total, s.Open, s.Ready, s.Updated)
	}
	fmt.Fprintf(w, "%-*s  %7d  %6d  %6d  %8d\n", width, "FLEET", total, open, ready, updated)
	fmt.Fprintf(w, "%d/%d stores reachable; updated = changed in the last %s\n", up, len(snaps), o.Recent)
}

func init() {
	f := brainStoresStatusCmd.Flags()
	f.BoolVar(&storesStatusJSON, "json", false, "Emit JSON (a snapshot object; one event object per change with --watch)")
	f.IntVar(&storesStatusJobs, "jobs", 8, "Probe this many stores concurrently")
	f.DurationVar(&storesStatusTimeout, "timeout", defaultStoreProbeTimeout, "Per-store read timeout")
	f.DurationVar(&storesStatusRecent, "recent", 24*time.Hour, "Window for the 'updated' column")
	f.BoolVar(&storesStatusWatch, "watch", false, "Keep polling and print one line per change")
	f.DurationVar(&storesStatusInterval, "interval", 30*time.Second, "Poll interval for --watch")
	f.IntVar(&storesStatusPolls, "polls", 0, "With --watch, stop after this many polls (0 = until interrupted)")
	brainStoresCmd.AddCommand(brainStoresStatusCmd)
}
