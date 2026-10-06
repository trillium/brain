package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/brainunify"
)

// brainUnifyCmd groups the three phases of the single-database migration:
// plan, build, verify. They are separate verbs because they have different
// safety properties and should be run at different times: plan is a read of
// production, build writes only to an isolated scratch server, and verify
// reads both sides and reports.
//
// The commands never write to a production database. `plan` and `verify`
// open the production server through a connection that can only issue reads,
// and `build` writes exclusively to a Dolt server it starts itself under
// --data-dir.
var brainUnifyCmd = &cobra.Command{
	Use:   "unify",
	Short: "Plan, build and verify the consolidation of brain's stores into one Dolt database",
	Long: `brain stores each keep their own Dolt database on a shared server. 'unify'
consolidates them into ONE authoritative database while preserving the logical
separation between stores.

The mechanism is prefixes. A bead id is already self-describing
("brain-se7t.389" is in the "brain" namespace), so store identity is already
encoded in the primary key and unification needs no schema change, no id
rewrite and no query rewrite. What the separate databases carried implicitly —
which physical database a row came from, which store declares which prefix —
is written into brain_stores and brain_store_prefixes.

Phases:

  plan    read production and print the deterministic mapping: participating
          databases, namespace ownership, and every id that exists in more
          than one database
  build   construct the unified database in an isolated Dolt server started
          under --data-dir, reading production but never writing to it
  verify  compare the unified database against production mechanically, by
          row count, content size and an order-independent content digest

Production safety: this command group opens production read-only. The builder
writes only to a Dolt server it starts itself, so a build cannot modify a live
store even by accident.`,
}

var (
	unifyHost      string
	unifyPort      int
	unifyDataDir   string
	unifyDatabase  string
	unifyDoltBin   string
	unifyTemplate  string
	unifyAllowColl bool
	unifyJSON      bool
	unifyTimeout   time.Duration
)

func init() {
	unifyPlanCmd.Flags().StringVar(&unifyHost, "host", "127.0.0.1", "dolt sql-server host holding the production stores")
	unifyPlanCmd.Flags().IntVar(&unifyPort, "port", 3307, "dolt sql-server port holding the production stores")
	unifyPlanCmd.Flags().StringVar(&unifyTemplate, "template", "", "store whose schema the unified database inherits (default: the store with the most beads)")
	unifyPlanCmd.Flags().BoolVar(&unifyJSON, "json", false, "emit the plan as JSON")
	unifyPlanCmd.Flags().DurationVar(&unifyTimeout, "timeout", 20*time.Minute, "overall time budget for the plan")

	unifyBuildCmd.Flags().StringVar(&unifyHost, "host", "127.0.0.1", "dolt sql-server host holding the production stores")
	unifyBuildCmd.Flags().IntVar(&unifyPort, "port", 3307, "dolt sql-server port holding the production stores")
	unifyBuildCmd.Flags().StringVar(&unifyDataDir, "data-dir", "", "scratch directory for the isolated dolt server holding the unified database (required)")
	unifyBuildCmd.Flags().StringVar(&unifyDatabase, "database", "brain_unified", "name of the unified database inside the isolated server")
	unifyBuildCmd.Flags().StringVar(&unifyDoltBin, "dolt-bin", "dolt", "dolt binary used to start the isolated server")
	unifyBuildCmd.Flags().StringVar(&unifyTemplate, "template", "", "store whose schema the unified database inherits (default: the store with the most beads)")
	unifyBuildCmd.Flags().BoolVar(&unifyAllowColl, "allow-collisions", false, "proceed even when a duplicated id has copies that disagree on content")
	unifyBuildCmd.Flags().DurationVar(&unifyTimeout, "timeout", 4*time.Hour, "overall time budget for the build")

	unifyVerifyCmd.Flags().StringVar(&unifyHost, "host", "127.0.0.1", "dolt sql-server host holding the production stores")
	unifyVerifyCmd.Flags().IntVar(&unifyPort, "port", 3307, "dolt sql-server port holding the production stores")
	unifyVerifyCmd.Flags().StringVar(&unifyDataDir, "data-dir", "", "directory holding the unified database built by 'unify build' (required)")
	unifyVerifyCmd.Flags().StringVar(&unifyDatabase, "database", "brain_unified", "name of the unified database")
	unifyVerifyCmd.Flags().StringVar(&unifyDoltBin, "dolt-bin", "dolt", "dolt binary used to start the server over the unified database")
	unifyVerifyCmd.Flags().StringVar(&unifyTemplate, "template", "", "store whose schema the unified database inherited")
	unifyVerifyCmd.Flags().DurationVar(&unifyTimeout, "timeout", 3*time.Hour, "overall time budget for verification")

	brainCmd.AddCommand(brainUnifyCmd)
	brainUnifyCmd.AddCommand(unifyPlanCmd, unifyBuildCmd, unifyVerifyCmd)
}

var unifyPlanCmd = &cobra.Command{
	Use:   "plan",
	Short: "Print the deterministic mapping from every current store into the unified database",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), unifyTimeout)
		defer cancel()

		reg, source, err := openUnifySources(ctx, unifyHost, unifyPort)
		if err != nil {
			return err
		}
		defer func() { _ = source.Close() }()

		disc, err := brainunify.Discover(ctx, source, reg)
		if err != nil {
			return err
		}
		plan := disc.Plan()
		plan.TemplateNamespace = unifyTemplate

		if unifyJSON {
			return writeUnifyJSON(cmd, plan, disc)
		}
		return brainunify.WritePlan(cmd.OutOrStdout(), plan, disc)
	},
}

var unifyBuildCmd = &cobra.Command{
	Use:   "build",
	Short: "Construct the unified database in an isolated Dolt server from live store data",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if unifyDataDir == "" {
			return fmt.Errorf("--data-dir is required: the unified database is never written into a production dolt data directory")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), unifyTimeout)
		defer cancel()

		reg, source, err := openUnifySources(ctx, unifyHost, unifyPort)
		if err != nil {
			return err
		}
		defer func() { _ = source.Close() }()

		disc, err := brainunify.Discover(ctx, source, reg)
		if err != nil {
			return err
		}
		plan := disc.Plan()
		plan.TemplateNamespace = unifyTemplate

		// The plan is printed before the build so the mapping that is about to
		// be applied is on the record even when the build later fails.
		if err := brainunify.WritePlan(cmd.OutOrStdout(), plan, disc); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "\n--- building ---"); err != nil {
			return err
		}

		builder := brainunify.NewBuilder(source, plan, brainunify.BuildOptions{
			DataDir:         unifyDataDir,
			Database:        unifyDatabase,
			DoltBin:         unifyDoltBin,
			AllowCollisions: unifyAllowColl,
			Logf: func(format string, args ...any) {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", args...)
			},
		})
		res, err := builder.Build(ctx)
		if err != nil {
			return err
		}
		return brainunify.WriteBuild(cmd.OutOrStdout(), res)
	},
}

var unifyVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Compare the unified database against production by counts, size and content digest",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if unifyDataDir == "" {
			return fmt.Errorf("--data-dir is required: name the directory 'unify build' wrote the unified database to")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), unifyTimeout)
		defer cancel()

		reg, source, err := openUnifySources(ctx, unifyHost, unifyPort)
		if err != nil {
			return err
		}
		defer func() { _ = source.Close() }()

		disc, err := brainunify.Discover(ctx, source, reg)
		if err != nil {
			return err
		}
		plan := disc.Plan()
		plan.TemplateNamespace = unifyTemplate

		// The unified database is verified through its own server, started
		// over the data dir the build wrote. Starting a second server over
		// the same directory is safe because the build's server is stopped
		// when the build returns.
		srv, err := brainunify.StartIsolatedServer(ctx, unifyDoltBin, unifyDataDir)
		if err != nil {
			return err
		}
		defer func() { _ = srv.Stop() }()

		started := time.Now()

		plans, err := brainunify.TablePlansFor(ctx, source, plan, unifyTemplate)
		if err != nil {
			return err
		}
		verifier, err := brainunify.NewVerifier(ctx, source, plan, plans, brainunify.VerifyOptions{
			Database: unifyDatabase,
			Host:     "127.0.0.1",
			Port:     srv.Port,
			// Progress goes to stderr, flushed per line and stamped with the
			// elapsed time. Verification reads every row of every table on
			// both sides, so a stall must be visible while it happens rather
			// than inferred an hour later from an empty log.
			Logf: func(format string, args ...any) {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[%7.1fs] %s\n",
					time.Since(started).Seconds(), fmt.Sprintf(format, args...))
			},
		})
		if err != nil {
			return err
		}
		defer func() { _ = verifier.Close() }()

		res, err := verifier.Verify(ctx)
		if err != nil {
			return err
		}
		if err := brainunify.WriteVerify(cmd.OutOrStdout(), res); err != nil {
			return err
		}
		if !res.OK() {
			return fmt.Errorf("verification failed: %d check(s) did not match", len(res.Failures()))
		}
		return nil
	},
}

// unifyPlanJSON is the machine-readable form of the plan. It carries the same
// facts as the text report so a reviewer can diff two runs without scraping
// the prose.
type unifyPlanJSON struct {
	Sources         []unifySourceJSON    `json:"sources"`
	Namespaces      []unifyNamespaceJSON `json:"namespaces"`
	Collisions      []unifyCollisionJSON `json:"collisions"`
	Excluded        []unifyExcludedJSON  `json:"excluded"`
	Replicas        []string             `json:"replicas"`
	Unregistered    []string             `json:"unregistered"`
	Template        string               `json:"template"`
	TotalBeads      int64                `json:"total_beads"`
	DistinctBeads   int64                `json:"distinct_beads"`
	DuplicateCopies int64                `json:"duplicate_copies"`
	Blocked         string               `json:"blocked,omitempty"`
}

type unifySourceJSON struct {
	Store            string   `json:"store"`
	Database         string   `json:"source_database"`
	Registered       bool     `json:"registered"`
	BeadCount        int64    `json:"bead_count"`
	DeclaredPrefixes []string `json:"declared_prefixes"`
	ProjectID        string   `json:"project_id,omitempty"`
}

type unifyNamespaceJSON struct {
	Prefix      string   `json:"prefix"`
	Owner       string   `json:"owner"`
	OwnerReason string   `json:"owner_reason"`
	DeclaredBy  []string `json:"declared_by"`
	ObservedBy  []string `json:"observed_by"`
	BeadCount   int64    `json:"bead_count"`
	Ambiguous   bool     `json:"ambiguous"`
}

type unifyCollisionJSON struct {
	ID         string            `json:"id"`
	Prefix     string            `json:"prefix"`
	Owner      string            `json:"owner"`
	Winner     string            `json:"winner"`
	Losers     []string          `json:"losers"`
	Reason     string            `json:"reason"`
	Divergent  bool              `json:"divergent"`
	WinnerHash string            `json:"winner_hash,omitempty"`
	LoserHash  map[string]string `json:"loser_hashes,omitempty"`
}

type unifyExcludedJSON struct {
	Database string `json:"database"`
	Reason   string `json:"reason"`
	Beads    int64  `json:"bead_count"`
}

// writeUnifyJSON emits the plan as JSON for machine comparison.
func writeUnifyJSON(cmd *cobra.Command, plan brainunify.Plan, disc brainunify.Discovery) error {
	out := unifyPlanJSON{
		Namespaces:      []unifyNamespaceJSON{},
		Collisions:      []unifyCollisionJSON{},
		Excluded:        []unifyExcludedJSON{},
		Replicas:        disc.Replicas,
		Unregistered:    disc.Unregistered,
		Template:        plan.TemplateNamespace,
		TotalBeads:      plan.TotalBeads(),
		DistinctBeads:   plan.DistinctBeads(),
		DuplicateCopies: plan.DuplicateCopies(),
	}
	for _, s := range plan.Sources {
		out.Sources = append(out.Sources, unifySourceJSON{
			Store: s.Namespace, Database: s.Database, Registered: s.Registered,
			BeadCount: s.BeadCount, DeclaredPrefixes: s.DeclaredPrefixes, ProjectID: s.ProjectID,
		})
	}
	for _, ns := range brainunify.SortedNamespaces(plan.Namespaces) {
		out.Namespaces = append(out.Namespaces, unifyNamespaceJSON{
			Prefix: ns.Prefix, Owner: ns.Owner, OwnerReason: ns.OwnerReason,
			DeclaredBy: ns.DeclaredBy, ObservedBy: ns.ObservedBy,
			BeadCount: ns.BeadCount, Ambiguous: ns.Ambiguous,
		})
	}
	for _, c := range plan.Collisions {
		out.Collisions = append(out.Collisions, unifyCollisionJSON{
			ID: c.ID, Prefix: c.Prefix, Owner: c.Owner, Winner: c.Winner,
			Losers: c.Losers, Reason: c.Reason, Divergent: c.Divergent,
			WinnerHash: c.WinnerHash, LoserHash: c.LoserHashes,
		})
	}
	for _, e := range plan.Excluded {
		out.Excluded = append(out.Excluded, unifyExcludedJSON{Database: e.Database, Reason: e.Reason, Beads: e.Beads})
	}
	if reason, blocked := plan.Blocks(); blocked {
		out.Blocked = reason
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// openUnifySources loads the store registry and opens production read-only.
func openUnifySources(ctx context.Context, host string, port int) (brainunify.Registry, *brainunify.ReadOnlySource, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return brainunify.Registry{}, nil, fmt.Errorf("resolving home directory: %w", err)
	}
	reg, err := brainunify.LoadRegistry(home)
	if err != nil {
		return brainunify.Registry{}, nil, err
	}
	source, err := brainunify.OpenReadOnlySource(ctx, host, port)
	if err != nil {
		return brainunify.Registry{}, nil, err
	}
	return reg, source, nil
}
