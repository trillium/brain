// Package main — brain_host_cmd.go
//
// The 'brain host' command family: promote, status, verify, rollback, plus the
// small registries a promotion reads (clients, role-bound services) and the
// hidden 'peer' op the other machine calls over ssh.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

var hostPromoteOpts promoteOptions
var (
	hostRollbackDiscard bool
	hostRollbackForce   bool
	hostStatusCheck     bool
)

func hostAnno() map[string]string { return map[string]string{"brain-host": "1"} }

// isBrainHostVerb reports whether cmd is part of 'brain host …', which manages
// this machine's role and never opens a store.
func isBrainHostVerb(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Annotations["brain-host"] == "1" {
			return true
		}
	}
	return false
}

var brainHostCmd = &cobra.Command{
	Use:   "host",
	Short: "Move the brain host role between machines (promote, verify, rollback)",
	Long: `The brain lives on one machine, the brain host: its Dolt server holds the
database, and every other machine is a client (see 'brain remote'). 'brain host'
moves that role.

  brain host promote <ssh-target>   move the host role from this machine to another
  brain host promote <t> --dry-run  show the plan and every reason it would refuse
  brain host status                 what role this machine has, and any promotion in progress
  brain host verify                 check the host (or, on the old host, that the move holds)
  brain host rollback               put both machines back as they were
  brain host clients …              the machines known to follow this host
  brain host service …              launchd jobs that belong to the host role

A promotion copies the database with Dolt's own backup (proved by hash), moves
the store directories, registry, wrappers and links, starts the Dolt server on
the new host, turns this machine into a client of it, and points the known
clients there. It can be killed at any point and resumed by running it again;
the old host's data is never modified or deleted. See docs/brain/HOST_PROMOTE.md.`,
	Annotations: hostAnno(),
	Run:         func(cmd *cobra.Command, _ []string) { _ = cmd.Help() },
}

var brainHostPromoteCmd = &cobra.Command{
	Use:   "promote <ssh-target>",
	Short: "Move the brain host role from this machine to the target",
	Args:  cobra.ExactArgs(1),
	Long: `Move the brain host role from this machine to <ssh-target> (user@host).

Run it on the current host. The target needs ssh key login from here, this brain
build, dolt and rsync. Start with --dry-run: it prints the plan and every reason
the promotion would refuse, and changes nothing.

Phases (each repeatable; a killed promotion resumes by running this again):
  preflight seed files freeze delta restore serve parity fence flip clients verify

--stop-after <phase> runs up to that phase and stops, so the slow copy can be
done ahead and the short pause and switch later.`,
	Annotations: hostAnno(),
	RunE: func(cmd *cobra.Command, args []string) error {
		hostPromoteOpts.Target = args[0]
		return runHostPromote(cmd, hostPromoteOpts)
	},
}

var brainHostStatusCmd = &cobra.Command{
	Use:         "status",
	Short:       "Show this machine's role and any promotion in progress",
	Args:        cobra.NoArgs,
	Annotations: hostAnno(),
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runHostStatus(cmd)
	},
}

var brainHostVerifyCmd = &cobra.Command{
	Use:         "verify",
	Short:       "Check that the host role is where it should be and working",
	Args:        cobra.NoArgs,
	Annotations: hostAnno(),
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runHostVerify(cmd)
	},
}

var brainHostRollbackCmd = &cobra.Command{
	Use:   "rollback [promotion-id]",
	Short: "Undo a promotion (finished or not): both machines go back as they were",
	Args:  cobra.MaximumNArgs(1),
	Long: `Undo a promotion. Everything either machine changed was journaled first, so
this restores it: this machine serves the brain again, the new host stops.

If the new host has been written to since the switch, those writes would be
lost, so rollback refuses; move the host role back with 'brain host promote'
from the new host instead (it carries the writes), or pass --discard-new-writes
to drop them (the database is kept aside on the new host, not deleted).`,
	Annotations: hostAnno(),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := ""
		if len(args) == 1 {
			id = args[0]
		}
		return runHostRollback(cmd, id)
	},
}

var brainHostClientsCmd = &cobra.Command{
	Use:         "clients",
	Short:       "The machines known to follow this host (they are updated by a promotion)",
	Annotations: hostAnno(),
	Run:         func(cmd *cobra.Command, _ []string) { _ = cmd.Help() },
}

var brainHostClientsListCmd = &cobra.Command{
	Use: "list", Short: "List known clients", Args: cobra.NoArgs, Annotations: hostAnno(),
	RunE: func(cmd *cobra.Command, _ []string) error {
		home, _ := os.UserHomeDir()
		cfg, err := loadHostConfig(home)
		if err != nil {
			return err
		}
		if cfg == nil || len(cfg.Clients) == 0 {
			fmt.Println("no known clients")
		} else {
			for _, c := range cfg.Clients {
				fmt.Printf("%s  (added %s)\n", c.Target, c.Added)
			}
		}
		// What the last 'brain rollout' runs recorded for each machine of the fleet:
		// the binary it runs, its pin state and when it was last proved.
		if rows := hostClientsRollouts(cmd.Context()); len(rows) > 0 {
			fmt.Println("\nrollout record (brain rollout status):")
			printRolloutRecords(os.Stdout, rows)
		}
		return nil
	},
}

var brainHostClientsAddCmd = &cobra.Command{
	Use: "add <ssh-target>", Short: "Record a machine that follows this host", Args: cobra.ExactArgs(1), Annotations: hostAnno(),
	RunE: func(cmd *cobra.Command, args []string) error {
		return editHostConfig(func(cfg *hostConfig) error {
			rc := &remoteConfig{Host: args[0]}
			if err := rc.validate(); err != nil {
				return err
			}
			for _, c := range cfg.Clients {
				if c.Target == args[0] {
					return nil
				}
			}
			cfg.Clients = append(cfg.Clients, hostClient{Target: args[0], Added: nowStamp()})
			fmt.Printf("✓ %s will be pointed at the new host by a promotion\n", args[0])
			return nil
		})
	},
}

var brainHostClientsRemoveCmd = &cobra.Command{
	Use: "remove <ssh-target>", Short: "Forget a client", Args: cobra.ExactArgs(1), Annotations: hostAnno(),
	RunE: func(cmd *cobra.Command, args []string) error {
		return editHostConfig(func(cfg *hostConfig) error {
			out := cfg.Clients[:0]
			for _, c := range cfg.Clients {
				if c.Target != args[0] {
					out = append(out, c)
				}
			}
			cfg.Clients = out
			fmt.Printf("✓ %s forgotten\n", args[0])
			return nil
		})
	},
}

var brainHostServiceCmd = &cobra.Command{
	Use:         "service",
	Short:       "launchd jobs that belong to the host role and move with it",
	Annotations: hostAnno(),
	Run:         func(cmd *cobra.Command, _ []string) { _ = cmd.Help() },
}

var brainHostServiceAddCmd = &cobra.Command{
	Use: "add <launchd-label>", Short: "Declare a job that moves with the host role", Args: cobra.ExactArgs(1), Annotations: hostAnno(),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !validLaunchdLabel(args[0]) {
			return fmt.Errorf("invalid launchd label %q", args[0])
		}
		return editHostConfig(func(cfg *hostConfig) error {
			if !containsStr(cfg.Services, args[0]) {
				cfg.Services = append(cfg.Services, args[0])
				sort.Strings(cfg.Services)
			}
			fmt.Printf("✓ job %s moves with the host role\n", args[0])
			return nil
		})
	},
}

var brainHostServiceRemoveCmd = &cobra.Command{
	Use: "remove <launchd-label>", Short: "Stop treating a job as part of the host role", Args: cobra.ExactArgs(1), Annotations: hostAnno(),
	RunE: func(cmd *cobra.Command, args []string) error {
		return editHostConfig(func(cfg *hostConfig) error {
			cfg.Services = removeString(cfg.Services, args[0])
			fmt.Printf("✓ job %s no longer moves with the host role\n", args[0])
			return nil
		})
	},
}

var brainHostPeerCmd = &cobra.Command{
	Use:    "peer <op>",
	Short:  "Internal: run one step of a promotion on this machine (called over ssh)",
	Args:   cobra.ExactArgs(1),
	Hidden: true, Annotations: hostAnno(),
	Run: func(cmd *cobra.Command, args []string) {
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		code := runPeerOp(ctx, args[0], os.Stdin, os.Stdout)
		stop()
		os.Exit(code)
	},
}

// editHostConfig loads (or starts) host.yaml, lets fn change it, and saves it.
func editHostConfig(fn func(*hostConfig) error) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cfg, err := loadHostConfig(home)
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = &hostConfig{Role: roleHost, Generation: 1, Since: nowStamp()}
	}
	if cfg.Role == roleMoved {
		return fmt.Errorf("this machine is no longer the host (it moved to %s); run this on the current host", cfg.MovedTo)
	}
	if err := fn(cfg); err != nil {
		return err
	}
	return saveHostConfig(home, cfg)
}

func init() {
	f := brainHostPromoteCmd.Flags()
	o := &hostPromoteOpts
	f.BoolVar(&o.DryRun, "dry-run", false, "Show the plan and every reason it would refuse; change nothing")
	f.BoolVarP(&o.Yes, "yes", "y", false, "Do not ask for confirmation")
	f.StringVar(&o.StopAfter, "stop-after", "", "Run up to and including this phase, then stop ("+strings.Join(promotePhases, ", ")+")")
	f.StringArrayVar(&o.SSHOptions, "ssh-option", nil, "Extra ssh -o option for reaching the target, Name=value (repeatable)")
	f.StringVar(&o.PeerBin, "peer-bin", "", "The brain command on the target (default: brain, then bd, on its PATH)")
	f.StringVar(&o.Bind, "bind", "", "Address the new Dolt server listens on (default 127.0.0.1)")
	f.IntVar(&o.Port, "port", 0, "Port the new Dolt server listens on (default: same as this host's)")
	f.IntVar(&o.RemotesAPIPort, "remotesapi-port", 0, "Also serve Dolt's remotesapi on this port on the new host (off by default)")
	f.StringVar(&o.ServiceKind, "service", "auto", "How the new server is kept running: auto, launchd, process, external")
	f.StringVar(&o.ServiceLabel, "service-label", "", "launchd label for the new server (default com.brain.dolt-server)")
	f.StringArrayVar(&o.ExtraDatabases, "database", nil, "Also move this database (repeatable)")
	f.BoolVar(&o.IncludeLegacy, "include-legacy", false, "Also move every other database on the old server (the pre-cutover per-store databases)")
	f.StringArrayVar(&o.Services, "service-job", nil, "launchd job that belongs to the host role and moves with it (repeatable; see 'brain host service')")
	f.StringArrayVar(&o.LinkDirs, "link-dir", nil, "Directory of agent links to recreate on the target (default ~/.pi/agent/bin)")
	f.StringArrayVar(&o.SkipWrappers, "skip-wrapper", nil, "Leave this wrapper out (repeatable)")
	f.BoolVar(&o.NoDoltcfg, "no-doltcfg", false, "Do not copy the Dolt user and privilege files")
	f.BoolVar(&o.StopSourceServer, "stop-source-server", false, "Stop this machine's Dolt server after the switch (default: leave it running, refusing brain writes)")
	f.StringArrayVar(&o.Clients, "client", nil, "A machine that follows this host and should be pointed at the new one (repeatable)")
	f.BoolVar(&o.MoveAside, "move-aside", false, "Rename store directories that already exist on the target out of the way (never deletes)")
	f.BoolVar(&o.ReplaceStale, "replace-stale", false, "The target is a former host: rename its old copy aside and replace it")
	f.BoolVar(&o.ForceWrappers, "force-wrappers", false, "Replace wrappers and links on the target that brain did not generate (originals are journaled)")
	f.BoolVar(&o.AllowOlderDolt, "allow-older-dolt", false, "Allow a target whose dolt is older than this machine's")
	f.BoolVar(&o.NoSeed, "no-seed", false, "Skip the live pre-copy; do the whole copy while writes are paused")
	f.BoolVar(&o.NoSmoke, "no-smoke", false, "Skip the per-store `count` checks (the database hash comparison still runs)")
	f.IntVar(&o.SourcePort, "source-port", 0, "This host's Dolt port, when it cannot be told from the stores")
	f.StringVar(&o.SourceDataDir, "source-data-dir", "", "This host's Dolt data directory, when it cannot be discovered")

	brainHostRollbackCmd.Flags().BoolVar(&hostRollbackDiscard, "discard-new-writes", false, "Roll back even though the new host has been written to since the switch")
	brainHostRollbackCmd.Flags().BoolVar(&hostRollbackForce, "force", false, "Undo this machine even if the target cannot be reached (run rollback again later to finish the target)")
	brainHostStatusCmd.Flags().BoolVar(&hostStatusCheck, "check", false, "Also contact the server and hash its databases")

	brainHostClientsCmd.AddCommand(brainHostClientsListCmd, brainHostClientsAddCmd, brainHostClientsRemoveCmd)
	brainHostServiceCmd.AddCommand(brainHostServiceAddCmd, brainHostServiceRemoveCmd)
	brainHostCmd.AddCommand(brainHostPromoteCmd, brainHostStatusCmd, brainHostVerifyCmd, brainHostRollbackCmd,
		brainHostClientsCmd, brainHostServiceCmd, brainHostPeerCmd)
	rootCmd.AddCommand(brainHostCmd)
}

// ---------------------------------------------------------------------------
// promote
// ---------------------------------------------------------------------------

func hostContext(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(remoteCtx(cmd), os.Interrupt, syscall.SIGTERM)
}

func runHostPromote(cmd *cobra.Command, opts promoteOptions) error {
	cmd.SilenceUsage = true
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return errors.New("cannot resolve the home directory")
	}
	if opts.StopAfter != "" && !containsStr(promotePhases, opts.StopAfter) {
		return fmt.Errorf("--stop-after: unknown phase %q (phases: %s)", opts.StopAfter, strings.Join(promotePhases, ", "))
	}
	switch opts.ServiceKind {
	case "", "auto", "launchd", "process", "external":
	default:
		return fmt.Errorf("--service: %q is not one of auto, launchd, process, external", opts.ServiceKind)
	}
	if err := os.MkdirAll(filepath.Join(home, promoteRootRel), 0o700); err != nil {
		return err
	}
	unlock, err := lockPromotion(filepath.Join(home, promoteRootRel, "lock"))
	if err != nil {
		return err
	}
	defer unlock()
	ctx, stop := hostContext(cmd)
	defer stop()

	host, err := loadHostConfig(home)
	if err != nil {
		return err
	}
	// Already done? Say so, change nothing: promotion is idempotent.
	if cur := currentPromoteID(home); cur == "" && host != nil && host.Role == roleMoved {
		if host.MovedTo == opts.Target {
			fmt.Printf("✓ Already done: the brain host role moved to %s on %s (promotion %s). Nothing to do.\n", host.MovedTo, shortDate(host.MovedAt), host.PromoteID)
			return nil
		}
		return fmt.Errorf("this machine is no longer the brain host: it moved to %s on %s. Promote from the current host", host.MovedTo, shortDate(host.MovedAt))
	}

	var st *promoteState
	if cur := currentPromoteID(home); cur != "" {
		st, err = loadPromoteState(home, cur)
		if err != nil {
			return fmt.Errorf("reading the promotion in progress (%s): %w", cur, err)
		}
		if st.Target != opts.Target {
			return fmt.Errorf("promotion %s to %s is in progress on this machine. Resume it with the same target, or run 'brain host rollback' to undo it", st.ID, st.Target)
		}
		stored := st.Options
		stored.DryRun, stored.Yes, stored.StopAfter = opts.DryRun, opts.Yes, opts.StopAfter
		if !sameOptions(stored, opts) && !opts.DryRun {
			fmt.Fprintln(os.Stderr, "note: resuming with the options the promotion started with; flags given now are ignored except --yes and --stop-after")
		}
		opts = stored
	} else {
		st = &promoteState{
			Version: 1, ID: newPromoteID(), Target: opts.Target, Options: opts, Outcome: outcomeRunning,
			Done: map[string]string{}, Started: nowStamp(), SourceHome: home,
		}
		if !opts.DryRun {
			if err := os.MkdirAll(promoteDir(home, st.ID), 0o700); err != nil {
				return err
			}
			if err := st.save(home); err != nil {
				return err
			}
			if err := setCurrentPromote(home, st.ID); err != nil {
				return err
			}
		}
	}
	p, err := newPromoter(home, opts, st, os.Stdout, os.Stdin)
	if err != nil {
		return err
	}
	if opts.DryRun {
		if err := p.preflight(ctx); err != nil {
			return err
		}
		if jsonOutput {
			writeJSON(os.Stdout, p.plan.summary(opts))
		} else {
			p.plan.print(os.Stdout, opts)
		}
		if len(p.plan.Blockers) > 0 {
			return fmt.Errorf("dry run: the promotion would refuse (%d reason(s))", len(p.plan.Blockers))
		}
		if !jsonOutput {
			fmt.Println("\nDry run: nothing was changed. Run again without --dry-run to promote.")
		}
		return nil
	}
	if err := p.run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "\n%v\n", err)
		fmt.Fprintf(os.Stderr, "\nThe promotion (%s) is saved. Fix the cause and run the same command again to resume,\nor run 'brain host rollback' to put everything back.\n", st.ID)
		return errors.New("promotion did not finish")
	}
	if p.st.Outcome == outcomeDone {
		p.say("\n✓ Promotion %s complete: %s is the brain host (generation %d).", st.ID, opts.Target, st.Generation)
		p.say("  This machine is a client of it; its old data is untouched. To undo: brain host rollback")
	}
	return nil
}

func sameOptions(a, b promoteOptions) bool {
	a.DryRun, a.Yes, a.StopAfter = false, false, ""
	b.DryRun, b.Yes, b.StopAfter = false, false, ""
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// summary is the plan without file contents, for --json.
func (pl *promotePlan) summary(opts promoteOptions) map[string]any {
	stores := make([]string, 0, len(pl.Stores))
	for _, s := range pl.Stores {
		stores = append(stores, s.Name)
	}
	jobs := []string{}
	for _, s := range pl.Services {
		jobs = append(jobs, s.Label)
	}
	return map[string]any{
		"id": pl.ID, "target": opts.Target, "target_home": pl.TargetHome, "generation": pl.Generation,
		"databases": pl.DBBytes, "stores": stores, "wrappers": len(pl.Wrappers), "links": len(pl.Links),
		"server": map[string]any{"data_dir": pl.DataDir, "bind": pl.Bind, "port": pl.Port, "service": pl.Service},
		"jobs":   jobs, "clients": pl.Clients,
		"warnings": pl.Warnings, "notes": pl.Notes, "blockers": pl.Blockers, "would_proceed": len(pl.Blockers) == 0,
	}
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func runHostStatus(cmd *cobra.Command) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cfg, err := loadHostConfig(home)
	if err != nil {
		return err
	}
	rc, _ := loadRemoteConfig()
	var relay relayInfo
	if cfg == nil {
		relay = detectRelay(home, portListening)
	}
	cur := currentPromoteID(home)
	last := lastPromoteID(home)
	var st *promoteState
	if id := cur; id != "" {
		st, _ = loadPromoteState(home, id)
	} else if last != "" {
		st, _ = loadPromoteState(home, last)
	}

	if jsonOutput {
		out := map[string]any{"home": home, "host": cfg, "client_of": rc.Host, "promotion_in_progress": cur}
		if cfg == nil && relay.Role != relayNone {
			out["relay"] = map[string]any{"role": string(relay.Role), "target": relay.Target, "reason": relay.Reason}
		}
		if st != nil {
			out["promotion"] = map[string]any{"id": st.ID, "target": st.Target, "outcome": st.Outcome, "done": st.Done, "generation": st.Generation}
		}
		return outputJSON(out)
	}
	fmt.Printf("machine:   %s\n", home)
	switch {
	case cfg == nil && relay.Role != relayNone:
		fmt.Printf("role:      %s\n", relay.describe())
	case cfg == nil && rc.Host != "":
		fmt.Printf("role:      client of %s (no host.yaml)\n", rc.Host)
	case cfg == nil:
		fmt.Println("role:      host (implied: no host.yaml; one is written the first time a promotion starts)")
	case cfg.Role == roleHost:
		fmt.Printf("role:      host, generation %d (since %s)\n", cfg.Generation, cfg.Since)
	case cfg.Role == roleFrozen:
		fmt.Printf("role:      host, FROZEN for promotion %s since %s — writes are refused, reads work\n", cfg.PromoteID, cfg.FrozenSince)
	case cfg.Role == roleMoved:
		fmt.Printf("role:      moved — the brain host is %s (since %s, promotion %s)\n", cfg.MovedTo, cfg.MovedAt, cfg.PromoteID)
	}
	if rc.Host != "" {
		fmt.Printf("client of: %s\n", rc.Host)
	}
	if cfg != nil && cfg.Server != nil {
		s := cfg.Server
		state := "not answering"
		if portListening(s.Port) {
			state = "answering"
		}
		fmt.Printf("server:    %s:%d, data %s, %s (%s)\n", s.Bind, s.Port, s.DataDir, s.Service.Kind, state)
		if len(s.Databases) > 0 {
			fmt.Printf("databases: %s\n", strings.Join(s.Databases, ", "))
		}
		if hostStatusCheck && portListening(s.Port) {
			ctx, cancel := context.WithTimeout(remoteCtx(cmd), time.Minute)
			defer cancel()
			for _, db := range s.Databases {
				if h, herr := databaseHash(ctx, s.Port, db); herr == nil {
					fmt.Printf("hash:      %s = %s\n", db, h)
				}
			}
		}
	}
	if cfg != nil && len(cfg.Clients) > 0 {
		names := make([]string, 0, len(cfg.Clients))
		for _, c := range cfg.Clients {
			names = append(names, c.Target)
		}
		fmt.Printf("clients:   %s\n", strings.Join(names, ", "))
	}
	if cfg != nil && len(cfg.Services) > 0 {
		fmt.Printf("jobs:      %s (move with the role)\n", strings.Join(cfg.Services, ", "))
	}
	if st != nil {
		inProgress := ""
		if cur != "" {
			inProgress = " — IN PROGRESS"
		}
		fmt.Printf("promotion: %s to %s, %s%s\n", st.ID, st.Target, st.Outcome, inProgress)
		var done []string
		for _, ph := range append(append([]string{}, promotePhases...), "flip.activate", "flip.demote", "flip.stopsrc") {
			if _, ok := st.Done[ph]; ok {
				done = append(done, ph)
			}
		}
		fmt.Printf("  done:    %s\n", strings.Join(done, " "))
		if cur != "" {
			fmt.Printf("  next:    run 'brain host promote %s' to resume, or 'brain host rollback' to undo\n", st.Target)
		}
		if len(st.Failed) > 0 {
			fmt.Printf("  clients not yet updated: %s\n", strings.Join(st.Failed, "; "))
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// verify
// ---------------------------------------------------------------------------

func runHostVerify(cmd *cobra.Command) error {
	cmd.SilenceUsage = true
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cfg, err := loadHostConfig(home)
	if err != nil {
		return err
	}
	ctx, stop := hostContext(cmd)
	defer stop()
	var problems []string
	ok := func(format string, a ...any) { fmt.Printf("✓ "+format+"\n", a...) }
	bad := func(format string, a ...any) {
		problems = append(problems, fmt.Sprintf(format, a...))
		fmt.Printf("✗ "+format+"\n", a...)
	}

	switch {
	case cfg != nil && cfg.Role == roleMoved:
		ok("this machine recorded the move to %s (promotion %s)", cfg.MovedTo, cfg.PromoteID)
		rc, rerr := loadRemoteConfig()
		if rerr != nil || rc.Host != cfg.MovedTo {
			bad("this machine is not configured as a client of %s (brain remote status)", cfg.MovedTo)
			break
		}
		conn, cerr := newPeerConn(rc.Host, rc.SSHOptions)
		if cerr != nil {
			bad("%v", cerr)
			break
		}
		hello, herr := conn.hello(ctx, "", nil)
		if herr != nil {
			bad("the new host does not answer: %v", herr)
			break
		}
		var status peerStatusResp
		if err := conn.call(ctx, "status", peerStatusReq{Check: true}, &status); err != nil {
			bad("asking the new host for its status: %v", err)
			break
		}
		switch {
		case status.Host == nil || status.Host.Role != roleHost:
			bad("%s does not say it is the brain host", rc.Host)
		case status.Host.Generation <= cfg.Generation:
			bad("%s is generation %d, not newer than this machine's %d", rc.Host, status.Host.Generation, cfg.Generation)
		default:
			ok("%s is the brain host, generation %d (%s)", rc.Host, status.Host.Generation, hello.Version)
		}
		if id := lastPromoteID(home); id != "" {
			if st, lerr := loadPromoteState(home, id); lerr == nil && st.FlipHeads != nil && status.Host != nil && status.Host.Server != nil {
				var hist peerHistoryResp
				if err := conn.call(ctx, "history", peerHistoryReq{Port: status.Host.Server.Port, Commits: st.FlipHeads}, &hist); err == nil {
					for db, c := range st.FlipHeads {
						if hist.Contains[db] {
							ok("%s on the new host contains this machine's last commit (%s): nothing was lost in the move", db, shortHash(c))
						} else {
							bad("%s on the new host does not contain this machine's last commit %s", db, shortHash(c))
						}
					}
				}
			}
		}
		reg, _ := loadStoresRegistry()
		checked := false
		for _, name := range sortedKeysStr(reg) {
			if fi, serr := os.Stat(filepath.Join(home, ".local", "bin", name)); serr != nil || !fi.Mode().IsRegular() {
				continue
			}
			if cerr := checkClientPath(ctx, rc, name); cerr != nil {
				bad("running '%s' on the new host from here: %v", name, cerr)
			} else {
				ok("this machine, as a client, runs '%s' on the new host over ssh", name)
			}
			checked = true
			break
		}
		if !checked {
			ok("(no store wrapper here to exercise the client path with)")
		}
	default:
		var port int
		var dbs []string
		if cfg != nil && cfg.Server != nil {
			port, dbs = cfg.Server.Port, cfg.Server.Databases
		}
		if port == 0 {
			return errors.New("this machine has no recorded server (host.yaml); run 'brain host promote --dry-run <target>' to have it discovered")
		}
		if !portListening(port) {
			bad("no Dolt server answers on port %d", port)
			break
		}
		ok("the Dolt server answers on port %d", port)
		for _, db := range dbs {
			if h, herr := databaseHash(ctx, port, db); herr != nil {
				bad("database %s: %v", db, herr)
			} else {
				ok("database %s hashes to %s", db, shortHash(h))
			}
		}
		reg, rerr := loadStoresRegistry()
		if rerr != nil {
			bad("store registry: %v", rerr)
			break
		}
		env := &peerEnv{home: home, svc: newServiceManager(home)}
		req := peerSmokeReq{Port: port}
		for _, name := range sortedKeysStr(reg) {
			w := filepath.Join(home, ".local", "bin", name)
			if fi, serr := os.Stat(w); serr == nil && fi.Mode().IsRegular() {
				req.Stores = append(req.Stores, peerSmokeStore{Name: name, Wrapper: w, BeadsDir: reg[name].Path})
			}
		}
		resp, serr := env.smoke(ctx, req)
		if serr != nil {
			bad("%v", serr)
			break
		}
		failed := 0
		for _, r := range resp.Results {
			if r.Error != "" {
				failed++
				bad("%s: %s", r.Name, r.Error)
			}
		}
		if failed == 0 {
			ok("all %d store commands answer", len(resp.Results))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("verification found %d problem(s)", len(problems))
	}
	return nil
}

// ---------------------------------------------------------------------------
// rollback
// ---------------------------------------------------------------------------

func runHostRollback(cmd *cobra.Command, id string) error {
	cmd.SilenceUsage = true
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(home, promoteRootRel), 0o700); err != nil {
		return err
	}
	unlock, err := lockPromotion(filepath.Join(home, promoteRootRel, "lock"))
	if err != nil {
		return err
	}
	defer unlock()
	ctx, stop := hostContext(cmd)
	defer stop()

	if id == "" {
		if id = currentPromoteID(home); id == "" {
			id = lastPromoteID(home)
		}
	}
	if id == "" {
		return errors.New("there is no promotion on this machine to roll back")
	}
	st, err := loadPromoteState(home, id)
	if err != nil {
		return fmt.Errorf("reading promotion %s: %w", id, err)
	}
	if st.Outcome == outcomeRolledBack || st.Outcome == outcomeAborted {
		fmt.Printf("✓ Promotion %s was already %s. Nothing to do.\n", id, st.Outcome)
		return nil
	}
	p, err := newPromoter(home, st.Options, st, os.Stdout, os.Stdin)
	if err != nil {
		return err
	}
	flipped := st.phaseDone("flip.activate")

	// 1. The target first: it stops serving before this machine starts again,
	// so there is never a moment two machines both serve the brain.
	reachable := false
	targetMachine := ""
	if st.Done["preflight"] != "" {
		if hello, herr := p.peer.hello(ctx, st.Options.PeerBin, nil); herr == nil {
			reachable = true
			targetMachine = hello.MachineID
		} else {
			fmt.Printf("! the target %s cannot be reached: %v\n", st.Target, herr)
		}
	}
	if flipped && reachable && !hostRollbackDiscard {
		dbs := make([]string, 0, len(st.Databases))
		for _, d := range st.Databases {
			dbs = append(dbs, d.Name)
		}
		var hs peerHashesResp
		if herr := p.peer.call(ctx, "hashes", peerHashesReq{Port: st.Summary.TargetServer.Port, Databases: dbs}, &hs); herr != nil {
			return fmt.Errorf("could not read the new host's databases to check for writes since the switch: %w\n(use --discard-new-writes to roll back regardless)", herr)
		}
		for _, d := range st.Databases {
			if hs.Hashes[d.Name] != st.FlipHashes[d.Name] {
				return fmt.Errorf("the new host has been written to since the switch (database %s changed): rolling back would lose those writes.\n"+
					"  Move the host role back instead — it carries them:  (on the new host)  brain host promote <this machine> --replace-stale\n"+
					"  Or drop them on purpose:  brain host rollback --discard-new-writes   (the database is kept aside on the new host, not deleted)", d.Name)
			}
		}
		fmt.Println("✓ the new host's databases are unchanged since the switch: nothing is lost by rolling back")
	}
	if reachable {
		var ur peerUndoResp
		req := peerUndoReq{ID: id}
		if flipped {
			req.ForwardID, req.ForwardGeneration = id+"-undo", st.Generation+1
		}
		if err := p.peer.call(ctx, "undo", req, &ur); err != nil {
			if !hostRollbackForce {
				return fmt.Errorf("undoing the target: %w\n(fix that and run rollback again, or --force to undo this machine only)", err)
			}
			fmt.Printf("! target not fully undone: %v\n", err)
		}
		for _, s := range summarizeUndo(ur.Steps) {
			fmt.Printf("  target: %s\n", s)
		}
		fmt.Printf("✓ %s no longer serves the brain\n", st.Target)
		if ur.ForwardTo != "" {
			fmt.Printf("✓ %s forwards to %s now: clients that already followed the move there follow it back\n", st.Target, ur.ForwardTo)
		}
	} else if st.Done["preflight"] != "" {
		if !hostRollbackForce {
			return fmt.Errorf("the target %s cannot be reached, so it cannot be told to stop serving. Run rollback again when it is reachable, or --force to undo this machine anyway (the target would still think it is the host)", st.Target)
		}
		fmt.Println("! undoing this machine only (--force); run 'brain host rollback' again once the target is reachable")
	}

	// 2. This machine.
	steps, uerr := p.j.undo(p.svc)
	for _, s := range summarizeUndo(steps) {
		fmt.Printf("  here: %s\n", s)
	}
	if uerr != nil {
		return fmt.Errorf("undoing this machine: %w", uerr)
	}
	if host, _ := loadHostConfig(home); host != nil && host.Role != roleHost {
		return fmt.Errorf("this machine's host.yaml still says %q after the undo; look at %s", host.Role, hostConfigFile(home))
	}
	if flipped && targetMachine != "" {
		// Rolling back a finished move is itself a move back: record it, one
		// generation on, so a client that followed the first move can verify the return.
		cfg, _ := loadHostConfig(home)
		if cfg == nil {
			srv := st.Summary.SourceServer
			cfg = &hostConfig{Role: roleHost, Since: nowStamp(), Server: &srv}
		}
		cfg.Generation, cfg.PromoteID = st.Generation+1, id+"-undo"
		cfg.Lineage = append(cfg.Lineage,
			lineageEntry{PromoteID: id, Generation: st.Generation, From: machineID(home), To: targetMachine, At: st.Started},
			lineageEntry{PromoteID: id + "-undo", Generation: st.Generation + 1, From: targetMachine, To: machineID(home), At: nowStamp()})
		if err := saveHostConfig(home, cfg); err != nil {
			return err
		}
	}
	if flipped || st.Frozen {
		if srv := st.Summary.SourceServer; srv.Port > 0 {
			if err := waitServerReady(ctx, srv.Port, 60*time.Second); err != nil {
				fmt.Printf("! this machine's Dolt server is not answering on port %d: %v\n", srv.Port, err)
			} else if !hostRollbackDiscard {
				for _, d := range st.Databases {
					if h, herr := databaseHash(ctx, srv.Port, d.Name); herr == nil && h == d.SourceHash {
						fmt.Printf("✓ %s on this machine hashes to the frozen %s again\n", d.Name, shortHash(h))
					}
				}
			}
		}
	}
	if flipped {
		st.Outcome = outcomeRolledBack
	} else {
		st.Outcome = outcomeAborted
	}
	st.logf("rolled back")
	if err := st.save(home); err != nil {
		return err
	}
	clearCurrentPromote(home)
	fmt.Printf("✓ Promotion %s %s. This machine is the brain host again; writes work.\n", id, st.Outcome)
	fmt.Printf("  The copy made on %s is left in place (%s); remove it yourself when you are sure.\n", st.Target, st.Summary.StagingDir)
	return nil
}

// summarizeUndo folds the per-file undo steps into one line per kind, so a
// rollback that touches a hundred wrappers reads as a few lines, and leaves the
// steps that are not file operations (stopping a server, loading a job) as they are.
func summarizeUndo(steps []string) []string {
	counts := map[string]int{}
	var order []string
	var other []string
	for _, s := range steps {
		verb, rest, _ := strings.Cut(s, " ")
		switch verb {
		case "removed", "restored":
			if strings.HasPrefix(rest, "link ") {
				verb += " link"
			} else {
				verb += " file"
			}
		case "moved":
			verb = "moved directory back"
		default:
			other = append(other, s)
			continue
		}
		if counts[verb] == 0 {
			order = append(order, verb)
		}
		counts[verb]++
	}
	var out []string
	for _, v := range order {
		n := counts[v]
		suffix := ""
		if n != 1 {
			suffix = "s"
		}
		if strings.HasSuffix(v, "back") {
			noun := "directory"
			if n != 1 {
				noun = "directories"
			}
			out = append(out, fmt.Sprintf("moved %d %s back", n, noun))
			continue
		}
		out = append(out, fmt.Sprintf("%s %d%s", strings.Fields(v)[0], n, " "+strings.Fields(v)[1]+suffix))
	}
	return append(out, other...)
}
