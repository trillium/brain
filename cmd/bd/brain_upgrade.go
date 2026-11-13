package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/brainupgrade"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/schema"
)

// brainUpgradeCmd moves brain's Dolt databases from the beads 1.1 schema to
// the schema this binary carries, behind a verified backup. Every step talks
// to the Dolt server directly; none opens the caller's store, because opening
// a store with a newer binary is itself a migration attempt.
var brainUpgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Migrate brain's Dolt databases to this binary's schema behind a verified backup",
	Long: `Moves every brain database on a Dolt server (brain_unified and any per-store
database) from the beads 1.1 schema to the schema this binary carries.

Steps, in order:

  plan      read each database and report what the migration would do (a dry
            run: nothing is written)
  backup    take a Dolt-native backup of each database into --backup-dir,
            open it as a database on a Dolt server of its own and compare it
            with the database by dolt_hashof_db, HEAD, the length of its
            history, and every table's row count and whole-row digest; only a
            backup that reproduces the database is marked verified
  rehearse  lay each backup out as a copy on a Dolt server of its own,
            migrate the copy, verify it, and roll it back the offline way;
            the live databases are not touched
  apply     migrate each database whose verified backup still matches it
            exactly, then verify it
  verify    compare a migrated database with the state its backup holds:
            every schema series at latest, every table and column kept, every
            row count and digest unchanged outside the columns the migration
            rewrites on purpose
  rollback  put each backup back in place of its database and prove the
            result is the backed-up state (discards everything written since
            the backup); see 'rollback --help' for the online and offline ways

Stop every writer before 'backup' and keep them stopped until 'verify' passes
and every client runs the new binary: 'apply' refuses a database that changed
after its backup, because restoring that backup would lose the change.

The steps need a login that may run DOLT_BACKUP and DDL (the server's root
account): --user, BEADS_DOLT_SERVER_USER, then root; the password from
BEADS_DOLT_PASSWORD, else the [host:port] entry of the beads credentials file.
A file:// backup is written by the server, so run 'backup', 'rehearse' and
'rollback' on the server's own machine.`,
}

var (
	upgradeHost      string
	upgradePort      int
	upgradeUser      string
	upgradeDatabases []string
	upgradeBackupDir string
	upgradeScratch   string
	upgradeDoltBin   string
	upgradeKeep      bool
	upgradeYes       bool
	upgradeRemotesOK bool
	upgradeOffline   bool
	upgradeCheck     bool
	upgradeDataDir   string
	upgradeJSON      bool
	upgradeTimeout   time.Duration
)

func init() {
	all := []*cobra.Command{upgradePlanCmd, upgradeBackupCmd, upgradeRehearseCmd, upgradeApplyCmd, upgradeVerifyCmd, upgradeRollbackCmd}
	for _, c := range all {
		c.Flags().StringVar(&upgradeHost, "host", envOr("BEADS_DOLT_SERVER_HOST", "127.0.0.1"), "Dolt sql-server host")
		c.Flags().IntVar(&upgradePort, "port", envIntOr("BEADS_DOLT_SERVER_PORT", 3307), "Dolt sql-server port")
		c.Flags().StringVar(&upgradeUser, "user", "", "login on the server (default BEADS_DOLT_SERVER_USER, then root)")
		c.Flags().StringSliceVar(&upgradeDatabases, "database", nil, "database to act on (repeatable; default: every database on the server that holds beads, or every one in the manifest)")
		c.Flags().BoolVar(&upgradeJSON, "json", false, "print the result as JSON")
		c.Flags().DurationVar(&upgradeTimeout, "timeout", 6*time.Hour, "overall time budget")
	}
	for _, c := range []*cobra.Command{upgradeBackupCmd, upgradeRehearseCmd, upgradeApplyCmd, upgradeVerifyCmd, upgradeRollbackCmd} {
		c.Flags().StringVar(&upgradeBackupDir, "backup-dir", "", "directory holding the backups and their manifest (required)")
	}
	upgradePlanCmd.Flags().StringVar(&upgradeBackupDir, "backup-dir", "", "also report whether each database still matches its verified backup here")
	for _, c := range []*cobra.Command{upgradeBackupCmd, upgradeRehearseCmd} {
		c.Flags().StringVar(&upgradeScratch, "scratch-dir", "", "directory a backup is opened in on a Dolt server of its own; same filesystem as --backup-dir so it can be cloned, not copied (default: <backup-dir>/.scratch, removed afterwards)")
		c.Flags().StringVar(&upgradeDoltBin, "dolt-bin", "dolt", "dolt binary for the isolated server")
		c.Flags().BoolVar(&upgradeKeep, "keep-scratch", false, "keep the isolated server's data directory afterwards")
	}
	upgradeApplyCmd.Flags().BoolVar(&upgradeRemotesOK, "allow-remote-backed", false, "migrate a database that has Dolt remotes; every clone of it must then be migrated by this binary too, and nothing may push the old schema back")
	upgradeRollbackCmd.Flags().BoolVar(&upgradeOffline, "offline", false, "with the Dolt server stopped, lay each backup out in place of its database directory (minutes, not hours)")
	upgradeRollbackCmd.Flags().StringVar(&upgradeDataDir, "data-dir", "", "the stopped Dolt server's data directory (with --offline)")
	upgradeRollbackCmd.Flags().BoolVar(&upgradeCheck, "check", false, "only compare each database with the state its backup holds")
	upgradeRollbackCmd.Flags().BoolVar(&upgradeYes, "yes", false, "confirm: replace each database with its backup, discarding everything written since")

	brainCmd.AddCommand(brainUpgradeCmd)
	brainUpgradeCmd.AddCommand(all...)
}

// upgradeCommandCanRunWithoutStore reports whether cmd is a 'brain upgrade'
// step. They talk to the Dolt server and must never open the caller's store:
// a newer binary opening a store on an older schema would try to migrate it.
func upgradeCommandCanRunWithoutStore(cmd *cobra.Command) bool {
	if cmd == nil || cmd.Parent() == nil || cmd.Parent() != brainUpgradeCmd {
		return false
	}
	return true
}

func envIntOr(key string, fallback int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil && v > 0 {
		return v
	}
	return fallback
}

func upgradeEndpoint() brainupgrade.Endpoint {
	user := upgradeUser
	if user == "" {
		user = envOr("BEADS_DOLT_SERVER_USER", "root")
	}
	password := os.Getenv("BEADS_DOLT_PASSWORD")
	if password == "" {
		password = configfile.LookupCredentialsPassword(upgradeHost, upgradePort)
	}
	return brainupgrade.Endpoint{Host: upgradeHost, Port: upgradePort, User: user, Password: password}
}

// upgradeTargets resolves the databases a step acts on: --database, else every
// database in the manifest (m non-nil), else every beads database on the
// server.
func upgradeTargets(ctx context.Context, ep brainupgrade.Endpoint, m *brainupgrade.Manifest) ([]string, error) {
	if len(upgradeDatabases) > 0 {
		return upgradeDatabases, nil
	}
	if m != nil {
		var names []string
		for _, b := range m.Backups {
			names = append(names, b.Database)
		}
		if len(names) == 0 {
			return nil, fmt.Errorf("the manifest in %s records no backups", upgradeBackupDir)
		}
		return names, nil
	}
	db, err := ep.Open(ctx, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	names, err := brainupgrade.BeadsDatabases(ctx, db)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no database on %s holds beads", ep)
	}
	return names, nil
}

func requireBackupDir() error {
	if upgradeBackupDir == "" {
		return errors.New("--backup-dir is required")
	}
	return nil
}

func loadUpgradeManifest() (*brainupgrade.Manifest, error) {
	if err := requireBackupDir(); err != nil {
		return nil, err
	}
	m, err := brainupgrade.LoadManifest(upgradeBackupDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no backup manifest in %s: run 'brain upgrade backup' first", upgradeBackupDir)
	}
	return m, err
}

func manifestBackup(m *brainupgrade.Manifest, database string) (brainupgrade.Backup, error) {
	b, ok := m.Get(database)
	if !ok {
		return b, fmt.Errorf("the manifest in %s has no backup of %s", upgradeBackupDir, database)
	}
	return b, nil
}

// writeUpgradeReport records a step's result beside the backups, so every
// step leaves evidence whatever the terminal kept.
func writeUpgradeReport(step, database string, v any) (string, error) {
	if upgradeBackupDir == "" {
		return "", nil
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(upgradeBackupDir, "reports", fmt.Sprintf("%s-%s-%s.json", time.Now().UTC().Format("20060102T150405Z"), step, database))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, append(raw, '\n'), 0o600)
}

func emitUpgradeJSON(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// upgradeScratchDir is a fresh directory, under --scratch-dir or
// <backup-dir>/.scratch, for the Dolt server a backup is opened on; done
// removes it unless --keep-scratch.
func upgradeScratchDir(purpose string) (dir string, done func()) {
	base := upgradeScratch
	if base == "" {
		base = filepath.Join(upgradeBackupDir, ".scratch")
	}
	dir = filepath.Join(base, fmt.Sprintf("%s-%d", purpose, time.Now().UnixNano()))
	return dir, func() {
		if !upgradeKeep {
			_ = os.RemoveAll(dir)
		}
	}
}

// upgradePlanEntry is one database in the plan.
type upgradePlanEntry struct {
	Database string                     `json:"database"`
	DBHash   string                     `json:"db_hash"`
	Plan     schema.UpgradePlan         `json:"schema"`
	Pending  bool                       `json:"pending"`
	Remotes  []string                   `json:"remotes,omitempty"`
	Backup   *upgradePlanBackupStanding `json:"backup,omitempty"`
}

type upgradePlanBackupStanding struct {
	Verified bool   `json:"verified"`
	Matches  bool   `json:"matches"`
	At       string `json:"taken_at"`
}

var upgradePlanCmd = &cobra.Command{
	Use:   "plan",
	Short: "Report what the migration would do to each database (reads only)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), upgradeTimeout)
		defer cancel()
		ep := upgradeEndpoint()
		var m *brainupgrade.Manifest
		if upgradeBackupDir != "" {
			loaded, err := brainupgrade.LoadManifest(upgradeBackupDir)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			m = loaded
		}
		targets, err := upgradeTargets(ctx, ep, nil)
		if err != nil {
			return err
		}
		var entries []upgradePlanEntry
		for _, name := range targets {
			c, err := ep.Conn(ctx, name)
			if err != nil {
				return err
			}
			_, dbHash, err := brainupgrade.Identity(ctx, c.Conn)
			if err != nil {
				_ = c.Close()
				return err
			}
			st, err := schema.PlanUpgrade(ctx, c.Conn)
			if err != nil {
				_ = c.Close()
				return err
			}
			remotes, err := brainupgrade.Remotes(ctx, c.Conn)
			_ = c.Close()
			if err != nil {
				return err
			}
			entry := upgradePlanEntry{Database: name, DBHash: dbHash, Plan: st, Pending: st.Pending(), Remotes: remotes}
			if m != nil {
				if b, ok := m.Get(name); ok {
					entry.Backup = &upgradePlanBackupStanding{Verified: b.Verified, Matches: b.State.DBHash == dbHash, At: b.State.TakenAt.Format(time.RFC3339)}
				}
			}
			entries = append(entries, entry)
		}
		if upgradeJSON {
			return emitUpgradeJSON(cmd, map[string]any{"server": ep.String(), "databases": entries})
		}
		out := cmd.OutOrStdout()
		for _, e := range entries {
			writeUpgradePlan(out, ep, e)
		}
		return nil
	},
}

func writeUpgradePlan(out io.Writer, ep brainupgrade.Endpoint, e upgradePlanEntry) {
	p := e.Plan
	_, _ = fmt.Fprintf(out, "%s on %s (dolt_hashof_db %s)\n", e.Database, ep, e.DBHash)
	if len(p.BrainRowsInMain) > 0 {
		var at []string
		for _, r := range p.BrainRowsInMain {
			at = append(at, strconv.Itoa(r.MainVersion))
		}
		_, _ = fmt.Fprintf(out, "  schema_migrations   records brain migrations at %s: they move to brain_schema_migrations, not re-run\n", strings.Join(at, ", "))
	}
	if p.HealRefusal != "" {
		_, _ = fmt.Fprintf(out, "  REFUSED             %s\n", p.HealRefusal)
	}
	_, _ = fmt.Fprintf(out, "  main series         %d -> %d: %s\n", p.MainEffective, p.MainLatest, pendingText(p.PendingMain))
	_, _ = fmt.Fprintf(out, "  ignored series      %d -> %d: %s\n", p.IgnoredCurrent, p.IgnoredLatest, pendingText(p.PendingIgnored))
	_, _ = fmt.Fprintf(out, "  brain series        %d -> %d: %s\n", p.BrainCurrent, p.BrainLatest, pendingText(p.PendingBrain))
	if len(e.Remotes) > 0 {
		_, _ = fmt.Fprintf(out, "  dolt remotes        %s (apply needs --allow-remote-backed)\n", strings.Join(e.Remotes, ", "))
	}
	switch {
	case e.Backup == nil:
		_, _ = fmt.Fprintln(out, "  backup              none recorded")
	case !e.Backup.Verified:
		_, _ = fmt.Fprintf(out, "  backup              taken %s, NOT verified\n", e.Backup.At)
	case !e.Backup.Matches:
		_, _ = fmt.Fprintf(out, "  backup              verified (taken %s), but the database CHANGED since: back up again\n", e.Backup.At)
	default:
		_, _ = fmt.Fprintf(out, "  backup              verified (taken %s) and still matches\n", e.Backup.At)
	}
}

func pendingText(names []string) string {
	if len(names) == 0 {
		return "up to date"
	}
	return fmt.Sprintf("%d to run (%s)", len(names), strings.Join(names, ", "))
}

var upgradeBackupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Take a Dolt-native backup of each database and verify it against the database",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if err := requireBackupDir(); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), upgradeTimeout)
		defer cancel()
		if err := os.MkdirAll(upgradeBackupDir, 0o700); err != nil {
			return err
		}
		ep := upgradeEndpoint()
		m, err := brainupgrade.LoadManifest(upgradeBackupDir)
		if errors.Is(err, os.ErrNotExist) {
			m, err = &brainupgrade.Manifest{}, nil
		}
		if err != nil {
			return err
		}
		m.Server, m.BD = ep.String(), Version
		targets, err := upgradeTargets(ctx, ep, nil)
		if err != nil {
			return err
		}
		out := cmd.ErrOrStderr()
		var results []brainupgrade.Backup
		failed := false
		for _, name := range targets {
			url, err := brainupgrade.BackupURL(upgradeBackupDir, name)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "%s: backing up to %s\n", name, url)
			c, err := ep.Conn(ctx, name)
			if err != nil {
				return err
			}
			st, err := brainupgrade.TakeBackup(ctx, c.Conn, url)
			_ = c.Close()
			if err != nil {
				return err
			}
			// The server wrote the backup; seeing it here is what proves this
			// command runs where the backup can be opened and rolled back from.
			if _, err := os.Stat(filepath.Join(upgradeBackupDir, name)); err != nil {
				return fmt.Errorf("the server reports a backup of %s at %s, but it is not visible here (%w): run this on the server's machine", name, url, err)
			}
			b := brainupgrade.Backup{Database: name, URL: url, State: st}
			_, _ = fmt.Fprintf(out, "%s: verifying the backup: opening it on a Dolt server of its own and comparing\n", name)
			dir, done := upgradeScratchDir("verify-" + name)
			diffs, err := brainupgrade.VerifyBackup(ctx, upgradeDoltBin, dir, b)
			done()
			if err != nil {
				return err
			}
			b.Verified, b.VerifyDiffs = len(diffs) == 0, diffs
			if b.Verified {
				b.VerifiedAt = time.Now().UTC()
			} else {
				failed = true
			}
			m.Put(b)
			if err := m.Save(upgradeBackupDir); err != nil {
				return err
			}
			if _, err := writeUpgradeReport("backup", name, b); err != nil {
				return err
			}
			results = append(results, b)
		}
		if upgradeJSON {
			if err := emitUpgradeJSON(cmd, results); err != nil {
				return err
			}
		} else {
			for _, b := range results {
				writeBackupResult(cmd.OutOrStdout(), b)
			}
		}
		if failed {
			return errors.New("a backup did not reproduce its database; nothing may be migrated until a backup verifies")
		}
		return nil
	},
}

func writeBackupResult(out io.Writer, b brainupgrade.Backup) {
	verdict := "VERIFIED"
	if !b.Verified {
		verdict = "NOT VERIFIED"
	}
	_, _ = fmt.Fprintf(out, "%s: %s  backup %s\n", b.Database, verdict, b.URL)
	_, _ = fmt.Fprintf(out, "  dolt_hashof_db %s  HEAD %s  %d tables\n", b.State.DBHash, b.State.Head, len(b.State.Tables))
	for _, d := range b.VerifyDiffs {
		_, _ = fmt.Fprintf(out, "  differs: %s\n", d)
	}
}

var upgradeRehearseCmd = &cobra.Command{
	Use:   "rehearse",
	Short: "Migrate, verify and roll back a copy of each backup, on a Dolt server of its own",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		m, err := loadUpgradeManifest()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), upgradeTimeout)
		defer cancel()
		targets, err := upgradeTargets(ctx, brainupgrade.Endpoint{}, m)
		if err != nil {
			return err
		}
		var results []brainupgrade.Rehearsal
		failed := false
		for _, name := range targets {
			b, err := manifestBackup(m, name)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: rehearsing on a copy of %s on a Dolt server of its own\n", name, b.URL)
			dir, done := upgradeScratchDir("rehearse-" + name)
			reh, err := brainupgrade.Rehearse(ctx, upgradeDoltBin, dir, b)
			done()
			if err != nil {
				return err
			}
			if !reh.OK() {
				failed = true
			}
			if _, err := writeUpgradeReport("rehearse", name, reh); err != nil {
				return err
			}
			results = append(results, reh)
		}
		if upgradeJSON {
			if err := emitUpgradeJSON(cmd, results); err != nil {
				return err
			}
		} else {
			for _, r := range results {
				writeRehearsal(cmd.OutOrStdout(), r)
			}
		}
		if failed {
			return errors.New("the rehearsal did not pass")
		}
		return nil
	},
}

func writeRehearsal(out io.Writer, r brainupgrade.Rehearsal) {
	verdict := "PASS"
	if !r.OK() {
		verdict = "FAIL"
	}
	_, _ = fmt.Fprintf(out, "%s: rehearsal %s\n", r.Database, verdict)
	for _, d := range r.CopyDiffs {
		_, _ = fmt.Fprintf(out, "  copy differs from the backup: %s\n", d)
	}
	if len(r.CopyDiffs) > 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "  migration: %d migrations in %.1fs\n", r.Apply.Applied, r.Apply.Seconds)
	writeMigrationReport(out, r.Apply.Report)
	_, _ = fmt.Fprintf(out, "  rollback (%s: stop, put the backup in place, start): %.1fs", r.Rollback.Mode, r.Rollback.Seconds)
	if r.Rollback.OK() {
		_, _ = fmt.Fprintf(out, ", dolt_hashof_db %s: the backed-up state\n", r.Apply.Report.DBHashBefore)
	} else {
		_, _ = fmt.Fprintln(out)
		for _, d := range r.Rollback.Diffs {
			_, _ = fmt.Fprintf(out, "    differs: %s\n", d)
		}
	}
}

func writeMigrationReport(out io.Writer, rep brainupgrade.MigrationReport) {
	verdict := "PASS"
	if !rep.OK() {
		verdict = "FAIL"
	}
	s := rep.Schema
	_, _ = fmt.Fprintf(out, "  verify %s: main %d/%d, ignored %d/%d, brain %d/%d; dolt_hashof_db %s -> %s\n", verdict,
		s.MainEffective, s.MainLatest, s.IgnoredCurrent, s.IgnoredLatest, s.BrainCurrent, s.BrainLatest, rep.DBHashBefore, rep.DBHashAfter)
	for _, t := range rep.Tables {
		line := fmt.Sprintf("    %-28s %-10s rows %d -> %d", t.Table, t.Status, t.RowsBefore, t.RowsAfter)
		if len(t.Rewritten) > 0 {
			line += "  rewritten: " + strings.Join(t.Rewritten, ", ")
		}
		if len(t.AddedColumns) > 0 {
			line += "  added: " + strings.Join(t.AddedColumns, ", ")
		}
		if t.Detail != "" {
			line += "  (" + t.Detail + ")"
		}
		_, _ = fmt.Fprintln(out, line)
	}
	if len(rep.NewTables) > 0 {
		_, _ = fmt.Fprintf(out, "    new tables: %s\n", strings.Join(rep.NewTables, ", "))
	}
	for _, f := range rep.Failures {
		_, _ = fmt.Fprintf(out, "    FAIL: %s\n", f)
	}
}

var upgradeApplyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Migrate each database whose verified backup still matches it, then verify it",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		m, err := loadUpgradeManifest()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), upgradeTimeout)
		defer cancel()
		ep := upgradeEndpoint()
		targets, err := upgradeTargets(ctx, ep, m)
		if err != nil {
			return err
		}
		// Every database is checked against its backup before any is
		// migrated, so a refusal leaves the whole server as it was.
		var backups []brainupgrade.Backup
		for _, name := range targets {
			b, err := manifestBackup(m, name)
			if err != nil {
				return err
			}
			if !b.Verified {
				return fmt.Errorf("the backup of %s was never verified; run 'brain upgrade backup' again", name)
			}
			c, err := ep.Conn(ctx, name)
			if err != nil {
				return err
			}
			_, dbHash, err := brainupgrade.Identity(ctx, c.Conn)
			if err != nil {
				_ = c.Close()
				return err
			}
			remotes, err := brainupgrade.Remotes(ctx, c.Conn)
			_ = c.Close()
			if err != nil {
				return err
			}
			// A clone that still runs the old schema would merge or push it
			// back over the migrated one; upstream gates the same case.
			if len(remotes) > 0 && !upgradeRemotesOK {
				return fmt.Errorf("%s has Dolt remotes (%s): migrating it forks its schema from every clone of it. "+
					"Migrate or retire those clones too, then pass --allow-remote-backed", name, strings.Join(remotes, ", "))
			}
			if dbHash != b.State.DBHash {
				return fmt.Errorf("%w: %s is at %s, its backup holds %s; stop every writer and run 'brain upgrade backup' again", brainupgrade.ErrChanged, name, dbHash, b.State.DBHash)
			}
			backups = append(backups, b)
		}
		var results []brainupgrade.ApplyResult
		failed := false
		for _, b := range backups {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: migrating\n", b.Database)
			c, err := ep.Conn(ctx, b.Database)
			if err != nil {
				return err
			}
			res, err := brainupgrade.Apply(ctx, c.Conn, b)
			_ = c.Close()
			if err != nil {
				return fmt.Errorf("%w\nRoll back with: bd brain upgrade rollback --backup-dir %s --database %s --yes", err, upgradeBackupDir, b.Database)
			}
			if !res.Report.OK() {
				failed = true
			}
			if _, err := writeUpgradeReport("apply", b.Database, res); err != nil {
				return err
			}
			results = append(results, res)
		}
		if upgradeJSON {
			if err := emitUpgradeJSON(cmd, results); err != nil {
				return err
			}
		} else {
			for _, r := range results {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: %d migrations in %.1fs\n", r.Database, r.Applied, r.Seconds)
				writeMigrationReport(cmd.OutOrStdout(), r.Report)
			}
		}
		if failed {
			return fmt.Errorf("a migrated database did not verify; roll back with: bd brain upgrade rollback --backup-dir %s --yes", upgradeBackupDir)
		}
		return nil
	},
}

var upgradeVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Compare each migrated database with the state its backup holds",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		m, err := loadUpgradeManifest()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), upgradeTimeout)
		defer cancel()
		ep := upgradeEndpoint()
		targets, err := upgradeTargets(ctx, ep, m)
		if err != nil {
			return err
		}
		var reports []brainupgrade.MigrationReport
		failed := false
		for _, name := range targets {
			b, err := manifestBackup(m, name)
			if err != nil {
				return err
			}
			c, err := ep.Conn(ctx, name)
			if err != nil {
				return err
			}
			rep, err := brainupgrade.VerifyMigrated(ctx, c.Conn, b.State)
			_ = c.Close()
			if err != nil {
				return err
			}
			if !rep.OK() {
				failed = true
			}
			if _, err := writeUpgradeReport("verify", name, rep); err != nil {
				return err
			}
			reports = append(reports, rep)
		}
		if upgradeJSON {
			if err := emitUpgradeJSON(cmd, reports); err != nil {
				return err
			}
		} else {
			for _, r := range reports {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s:\n", r.Database)
				writeMigrationReport(cmd.OutOrStdout(), r)
			}
		}
		if failed {
			return errors.New("verification failed")
		}
		return nil
	},
}

var upgradeRollbackCmd = &cobra.Command{
	Use:   "rollback",
	Short: "Put each database back to its backup and prove it is the backed-up state",
	Long: `Puts each database back to the state its verified backup holds, discarding
everything written since the backup.

  rollback --yes                 online: DOLT_BACKUP('restore', '--force') on the
                                 running server, then the remotes and backup
                                 registrations the backup recorded are added back.
                                 No restart, but slow on a large database: Dolt
                                 checks every chunk's references across the whole
                                 history first.
  rollback --yes --offline --data-dir DIR
                                 with the Dolt server STOPPED: each database
                                 directory under DIR is moved aside (kept as
                                 <db>.migrated-<stamp>) and the backup is laid out
                                 in its place, keeping the repository config.
                                 Minutes. Start the server, then run --check.
  rollback --check               compare each database with the state its backup
                                 holds (reads only).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		m, err := loadUpgradeManifest()
		if err != nil {
			return err
		}
		if !upgradeYes && !upgradeCheck {
			return errors.New("rollback replaces each database with its backup and discards everything written since; pass --yes to confirm (or --check to only compare)")
		}
		if upgradeOffline && upgradeDataDir == "" {
			return errors.New("--offline needs --data-dir, the stopped server's data directory")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), upgradeTimeout)
		defer cancel()
		ep := upgradeEndpoint()
		targets, err := upgradeTargets(ctx, brainupgrade.Endpoint{}, m)
		if err != nil {
			return err
		}
		var backups []brainupgrade.Backup
		for _, name := range targets {
			b, err := manifestBackup(m, name)
			if err != nil {
				return err
			}
			if !b.Verified {
				return fmt.Errorf("the backup of %s was never verified; refusing to restore it", name)
			}
			backups = append(backups, b)
		}
		if upgradeOffline {
			// Swapping the directory under a running server would leave it
			// serving the old files; the server must be down.
			if db, err := ep.Open(ctx, ""); err == nil {
				_ = db.Close()
				return fmt.Errorf("the Dolt server on %s is running; stop it before an offline rollback (or roll back online without --offline)", ep)
			}
		}
		var results []brainupgrade.RollbackResult
		failed := false
		for _, b := range backups {
			var res brainupgrade.RollbackResult
			switch {
			case upgradeCheck:
				res = brainupgrade.RollbackResult{Database: b.Database, Mode: "check"}
				res.Diffs, err = brainupgrade.CheckRestored(ctx, ep, b)
			case upgradeOffline:
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: laying out %s in place of %s\n", b.Database, b.URL, filepath.Join(upgradeDataDir, b.Database))
				start := time.Now()
				res = brainupgrade.RollbackResult{Database: b.Database, Mode: "offline"}
				res.Aside, err = brainupgrade.OfflineRollback(upgradeDataDir, b)
				res.Seconds = time.Since(start).Seconds()
			default:
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: restoring from %s on the running server\n", b.Database, b.URL)
				res, err = brainupgrade.Rollback(ctx, ep, b)
			}
			if err != nil {
				return err
			}
			if !res.OK() {
				failed = true
			}
			if _, err := writeUpgradeReport("rollback-"+res.Mode, b.Database, res); err != nil {
				return err
			}
			results = append(results, res)
		}
		if upgradeJSON {
			if err := emitUpgradeJSON(cmd, results); err != nil {
				return err
			}
		} else {
			for _, r := range results {
				switch {
				case r.Mode == "offline":
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: backup laid out in %.1fs; the migrated database is kept at %s\n", r.Database, r.Seconds, r.Aside)
				case r.OK():
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: the backed-up state (%s)\n", r.Database, r.Mode)
				default:
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: DIFFERS from the backed-up state (%s)\n", r.Database, r.Mode)
					for _, d := range r.Diffs {
						_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  differs: %s\n", d)
					}
				}
			}
			if upgradeOffline {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Start the Dolt server, then prove the result with: bd brain upgrade rollback --check --backup-dir "+upgradeBackupDir)
			}
		}
		if failed {
			return errors.New("a database is not its backed-up state")
		}
		return nil
	},
}
