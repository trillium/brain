package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/brainunify"
)

// brainUnifyCmd groups the phases of the single-database migration:
// plan, build, replay, verify. They are separate verbs because they have different
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
	Short: "Plan, build, replay and verify the consolidation of brain's stores into one Dolt database",
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
          than one database and what becomes of it
  build   construct the unified database in an isolated Dolt server started
          under --data-dir, reading production but never writing to it
  replay  apply the changes the sources made since the build into the unified
          database, so it can be kept current until it becomes the reference
  verify  compare the unified database against production mechanically, by
          row count, content size and an order-independent content digest;
          --reference live compares against the sources as they stand now

Duplicated ids. An id that more than one store holds is merged into one bead
when the copies are identical. When the copies differ, nothing is picked and
nothing is discarded: each copy becomes a bead of its own under a new id
(<authoring store's prefix>-<12 hex of sha256(id NUL store)>), carrying that
copy's row and child rows, and the original id becomes an open conflict bead
that says so and lists both copies. Links other beads hold to the original id
still point at it, so they reach the conflict bead.

Production safety: this command group opens production read-only. The builder
writes only to a Dolt server it starts itself, so a build cannot modify a live
store even by accident.`,
}

// allowCollisionsRetired is the deprecation message of --allow-collisions. The
// flag used to override the refusal to build or replay when a duplicated id's
// copies disagreed on content, because keeping one copy discarded the other's
// state. No copy is discarded any more - each becomes a bead and the original id
// a conflict bead - so there is nothing left to override. The flag is still
// accepted so existing scripts keep working.
const allowCollisionsRetired = "duplicated ids whose copies differ no longer need a decision: each copy is kept as a bead and the original id becomes a conflict bead, so nothing is discarded; the flag has no effect"

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
	unifyReference string
	unifyProjectID []string
)

// projectIDMap parses repeated --project-id <store>=<uuid> values.
func projectIDMap() (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range unifyProjectID {
		i := strings.Index(kv, "=")
		if i <= 0 || i == len(kv)-1 {
			return nil, fmt.Errorf("--project-id wants <store>=<uuid>, got %q", kv)
		}
		out[strings.TrimSpace(kv[:i])] = strings.TrimSpace(kv[i+1:])
	}
	return out, nil
}

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
	unifyBuildCmd.Flags().BoolVar(&unifyAllowColl, "allow-collisions", false, "no longer has any effect")
	_ = unifyBuildCmd.Flags().MarkDeprecated("allow-collisions", allowCollisionsRetired)
	unifyBuildCmd.Flags().DurationVar(&unifyTimeout, "timeout", 4*time.Hour, "overall time budget for the build")
	unifyBuildCmd.Flags().StringSliceVar(&unifyProjectID, "project-id", nil, "project id of a store neither its database nor the registry identifies, as <store>=<uuid> (repeatable); a store left without one refuses the build")
	unifyPlanCmd.Flags().StringSliceVar(&unifyProjectID, "project-id", nil, "project id of a store neither its database nor the registry identifies, as <store>=<uuid> (repeatable)")

	unifyReplayCmd.Flags().StringVar(&unifyHost, "host", "127.0.0.1", "dolt sql-server host holding the source stores")
	unifyReplayCmd.Flags().IntVar(&unifyPort, "port", 3307, "dolt sql-server port holding the source stores")
	unifyReplayCmd.Flags().StringVar(&unifyDataDir, "data-dir", "", "directory holding the merged database built by 'unify build' (required)")
	unifyReplayCmd.Flags().StringVar(&unifyDatabase, "database", "brain_unified", "name of the merged database")
	unifyReplayCmd.Flags().StringVar(&unifyDoltBin, "dolt-bin", "dolt", "dolt binary used to start the server over the merged database")
	unifyReplayCmd.Flags().BoolVar(&unifyAllowColl, "allow-collisions", false, "no longer has any effect")
	_ = unifyReplayCmd.Flags().MarkDeprecated("allow-collisions", allowCollisionsRetired)
	unifyReplayCmd.Flags().DurationVar(&unifyTimeout, "timeout", 4*time.Hour, "overall time budget for the replay")

	unifyVerifyCmd.Flags().StringVar(&unifyReference, "reference", brainunify.ReferenceRecorded,
		"what to compare the merged database against: 'recorded' (the fingerprints the build or last replay took) or 'live' (the sources as they stand now; the acceptance test for a replay)")
	unifyVerifyCmd.Flags().StringVar(&unifyHost, "host", "127.0.0.1", "dolt sql-server host holding the production stores")
	unifyVerifyCmd.Flags().IntVar(&unifyPort, "port", 3307, "dolt sql-server port holding the production stores")
	unifyVerifyCmd.Flags().StringVar(&unifyDataDir, "data-dir", "", "directory holding the unified database built by 'unify build' (required)")
	unifyVerifyCmd.Flags().StringVar(&unifyDatabase, "database", "brain_unified", "name of the unified database")
	unifyVerifyCmd.Flags().StringVar(&unifyDoltBin, "dolt-bin", "dolt", "dolt binary used to start the server over the unified database")
	unifyVerifyCmd.Flags().StringVar(&unifyTemplate, "template", "", "store whose schema the unified database inherited")
	unifyVerifyCmd.Flags().DurationVar(&unifyTimeout, "timeout", 3*time.Hour, "overall time budget for verification")

	brainCmd.AddCommand(brainUnifyCmd)
	brainUnifyCmd.AddCommand(unifyPlanCmd, unifyBuildCmd, unifyReplayCmd, unifyVerifyCmd)
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
		ids, err := projectIDMap()
		if err != nil {
			return err
		}
		disc.ApplyProjectIDs(ids)
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
		ids, err := projectIDMap()
		if err != nil {
			return err
		}
		disc.ApplyProjectIDs(ids)
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
			DataDir:  unifyDataDir,
			Database: unifyDatabase,
			DoltBin:  unifyDoltBin,
			Host:     unifyHost,
			Port:     unifyPort,
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

var unifyReplayCmd = &cobra.Command{
	Use:   "replay",
	Short: "Apply the changes the source stores made since the merged database was built",
	Long: `A merged database is built from a moment in the past. Every bead written,
edited or closed in a source store after that moment exists only in the source.
'replay' carries those changes into the merged database, so it can be kept
current until it becomes the reference.

For every source it reads the Dolt history since the commit 'unify build'
recorded (brain_unify_source_commits), finds the beads and the database-state
tables that changed, and re-reads exactly those from the source as it stands
now. Inserts, updates and deletes are one operation: the merged database's rows
for a changed bead are made equal to what the sources hold for it. Duplicated
ids are handled by the build's own functions: identical copies stay one bead;
copies that differ become a conflict bead plus one minted bead per copy, so a
change that creates, edits or removes such a duplicate re-derives the conflict
bead and its copies. Every duplicate is recorded in brain_unify_collisions.
Tables Dolt keeps no history for (wisps) are reloaded whole. The next replay
starts where this one read.

Anything it cannot resolve confidently is a refusal that names the store and the
table, and a refused replay leaves the merged database untouched:

  - a merged database built before builds recorded their commits (rebuild it)
  - a store that joined, left or moved to another database since the build
  - a source whose history no longer contains its recorded commit
  - a source table the build imported rows from that is gone
  - a table or column the merged schema does not have
  - a merged database built before differing copies became conflict beads
  - a minted copy id that two copies derive, or that an existing bead has

Prove a replay with 'unify verify --reference live', which compares the merged
database against the sources as they stand. Sources are only ever read.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if unifyDataDir == "" {
			return fmt.Errorf("--data-dir is required: name the directory 'unify build' wrote the merged database to")
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

		started := time.Now()
		replayer := brainunify.NewReplayer(source, plan, brainunify.ReplayOptions{
			DataDir:  unifyDataDir,
			Database: unifyDatabase,
			DoltBin:  unifyDoltBin,
			Host:     unifyHost,
			Port:     unifyPort,
			Logf: func(format string, args ...any) {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[%7.1fs] %s\n",
					time.Since(started).Seconds(), fmt.Sprintf(format, args...))
			},
		})
		res, err := replayer.Replay(ctx)
		if err != nil {
			return err
		}
		return brainunify.WriteReplay(cmd.OutOrStdout(), res)
	},
}

var unifyVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Compare the unified database against production by counts, size and content digest",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if unifyReference != brainunify.ReferenceRecorded && unifyReference != brainunify.ReferenceLive {
			return fmt.Errorf("--reference must be %q or %q, got %q", brainunify.ReferenceRecorded, brainunify.ReferenceLive, unifyReference)
		}
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
			Database:  unifyDatabase,
			Host:      "127.0.0.1",
			Port:      srv.Port,
			Reference: unifyReference,
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
	Conflicts       int                  `json:"conflicts"`
	MintedCopies    int                  `json:"minted_copies"`
	Refused         string               `json:"refused,omitempty"`
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
	Resolution string            `json:"resolution"`
	Winner     string            `json:"winner,omitempty"`
	Losers     []string          `json:"losers"`
	Reason     string            `json:"reason"`
	Divergent  bool              `json:"divergent"`
	WinnerHash string            `json:"winner_hash,omitempty"`
	LoserHash  map[string]string `json:"loser_hashes,omitempty"`
	// Copies maps each copy of a conflict to the id it is minted under.
	Copies           map[string]string `json:"copies,omitempty"`
	DifferingColumns []string          `json:"differing_columns,omitempty"`
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
		Conflicts:       len(plan.Conflicts()),
		MintedCopies:    plan.MintedCopies(),
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
			Resolution: c.Resolution, Copies: copyMap(c), DifferingColumns: c.DifferingColumns,
		})
	}
	for _, e := range plan.Excluded {
		out.Excluded = append(out.Excluded, unifyExcludedJSON{Database: e.Database, Reason: e.Reason, Beads: e.Beads})
	}
	if err := plan.Refusal(); err != nil {
		out.Refused = err.Error()
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// copyMap maps each copy of a conflict to the id it is minted under.
func copyMap(c brainunify.Collision) map[string]string {
	if len(c.Copies) == 0 {
		return nil
	}
	out := make(map[string]string, len(c.Copies))
	for _, cp := range c.Copies {
		out[cp.Store] = cp.ID
	}
	return out
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
