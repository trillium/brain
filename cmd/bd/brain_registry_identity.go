package main

// Project identity of registered stores. A store another machine can create a
// skeleton for needs a `_project_id` row in brain_unified_metadata; without it
// `brain stores sync` cannot write the store's metadata.json, so the store is
// registered but unsyncable there (incident 2026-11-24). This file owns:
//
//   - ensureProjectIDTx: the in-transaction, never-overwrite backfill that
//     'stores add' and 'stores reconcile' ride in the registry's transaction
//     ('stores create' seeds the row itself in seedNamespaceTx);
//   - identityProblems: the doctor's read of which registered stores lack the row;
//   - repairIdentity: 'brain stores doctor --repair-identity'.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storeregistry"
)

// doctorRepairIdentity is the --repair-identity flag of 'brain stores doctor'.
var doctorRepairIdentity bool

// identityRepairCommand is the one command that repairs a missing identity row.
const identityRepairCommand = "brain stores doctor --repair-identity"

// missingIdentityError is a store that is registered but has no `_project_id`
// row in the shared database. Its message is the repair hint.
type missingIdentityError struct{ Store string }

func (e *missingIdentityError) Error() string {
	return fmt.Sprintf("the database has no project identity for %q. Repair: on a machine that has %s's .beads/metadata.json, run '%s' (it backfills the _project_id row from that file), then re-run 'brain stores sync'",
		e.Store, e.Store, identityRepairCommand)
}

// localProjectID returns the project_id in dir/metadata.json ("" when this
// machine has no such file or it carries no id).
func localProjectID(beadsDir string) string {
	cfg, err := configfile.Load(beadsDir)
	if err != nil || cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.ProjectID)
}

func missingLocalProjectIDError(store, beadsDir string) error {
	return fmt.Errorf("cannot register store %q: %s/metadata.json has no project_id; add the store's project_id to that file and retry", store, beadsDir)
}

func requireLocalProjectID(store, beadsDir string) error {
	if localProjectID(beadsDir) == "" {
		return missingLocalProjectIDError(store, beadsDir)
	}
	return nil
}

func requireLocalProjectIDs(entries []storeregistry.Entry) error {
	home, _ := os.UserHomeDir()
	for _, e := range entries {
		if e.Kind != storeregistry.KindStore || e.State != storeregistry.StateActive {
			continue
		}
		if err := requireLocalProjectID(e.Store, filepath.Join(home, filepath.FromSlash(e.RelPath))); err != nil {
			return err
		}
	}
	return nil
}

// ensureProjectIDTx writes the `_project_id` row for store inside the caller's
// transaction, only when no row exists. An existing row with the same value is
// a no-op; one with a different value is reported as conflict (the existing
// row is never overwritten).
func ensureProjectIDTx(ctx context.Context, tx storeregistry.DBTX, store, projectID string) (inserted bool, conflict string, err error) {
	if projectID == "" {
		return false, "", nil
	}
	var cur string
	qerr := tx.QueryRowContext(ctx,
		"SELECT value FROM brain_unified_metadata WHERE `store` = ? AND `key` = '_project_id'", store).Scan(&cur)
	switch {
	case qerr == nil:
		if cur != "" && cur != projectID {
			return false, cur, nil
		}
		if cur == projectID {
			return false, "", nil
		}
	case !errors.Is(qerr, sql.ErrNoRows):
		return false, "", fmt.Errorf("reading the project identity of %q: %w", store, qerr)
	}
	if _, err := tx.ExecContext(ctx,
		"REPLACE INTO brain_unified_metadata (`store`,`key`,value) VALUES (?, '_project_id', ?)", store, projectID); err != nil {
		return false, "", fmt.Errorf("writing the project identity of %q: %w", store, err)
	}
	return true, "", nil
}

// identityBackfillInTx returns an InTx hook that backfills, for every store
// entry given, the identity row from this machine's metadata.json. Conflicts
// are left for 'doctor --repair-identity' to report; they never fail the
// registry write.
func identityBackfillInTx(entries []storeregistry.Entry) func(context.Context, storeregistry.DBTX) error {
	home, _ := os.UserHomeDir()
	return func(ctx context.Context, tx storeregistry.DBTX) error {
		for _, e := range entries {
			if e.Kind != storeregistry.KindStore || e.State != storeregistry.StateActive {
				continue
			}
			pid := localProjectID(filepath.Join(home, filepath.FromSlash(e.RelPath)))
			if pid == "" {
				return missingLocalProjectIDError(e.Store, filepath.Join(home, filepath.FromSlash(e.RelPath)))
			}
			if _, _, err := ensureProjectIDTx(ctx, tx, e.Store, pid); err != nil {
				return err
			}
		}
		return nil
	}
}

// chainInTx runs a then b inside one registry transaction (either may be nil).
func chainInTx(a, b func(context.Context, storeregistry.DBTX) error) func(context.Context, storeregistry.DBTX) error {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return func(ctx context.Context, tx storeregistry.DBTX) error {
		if err := a(ctx, tx); err != nil {
			return err
		}
		return b(ctx, tx)
	}
}

// identityProblem is one registered store with no identity row.
type identityProblem struct {
	Store   string `json:"store"`
	LocalID bool   `json:"local_metadata_has_id"` // this machine could repair it
	Repair  string `json:"repair"`
}

func (p identityProblem) String() string {
	return fmt.Sprintf("store %q has no _project_id row in brain_unified_metadata, so no other machine can create it (repair, on a machine that has %s/.beads/metadata.json: %s)",
		p.Store, p.Store, p.Repair)
}

// identityProblems lists the active registry stores that have no `_project_id`
// row. A database without the metadata table (legacy) yields none.
func identityProblems(ctx context.Context, q storeregistry.DBTX, entries []storeregistry.Entry) []identityProblem {
	rows, err := q.QueryContext(ctx, "SELECT `store` FROM brain_unified_metadata WHERE `key` = '_project_id' AND value <> ''")
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	have := map[string]bool{}
	for rows.Next() {
		var s string
		if rows.Scan(&s) == nil {
			have[s] = true
		}
	}
	if rows.Err() != nil {
		return nil
	}
	home, _ := os.UserHomeDir()
	seen := map[string]bool{}
	var out []identityProblem
	for _, e := range storeregistry.Active(entries) {
		if e.Kind != storeregistry.KindStore || have[e.Store] || seen[e.Store] {
			continue
		}
		seen[e.Store] = true
		out = append(out, identityProblem{
			Store:   e.Store,
			LocalID: localProjectID(filepath.Join(home, filepath.FromSlash(e.RelPath))) != "",
			Repair:  identityRepairCommand,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Store < out[j].Store })
	return out
}

// identityRepairResult is the outcome for one store of --repair-identity.
type identityRepairResult struct {
	Store  string `json:"store"`
	Status string `json:"status"` // repaired | conflict | no-local-id
	Detail string `json:"detail"`
}

// repairIdentity backfills the missing identity rows from this machine's
// metadata.json files, in one transaction. It never overwrites an existing row:
// a store whose row exists with a different value than this machine's
// metadata.json is reported as a conflict and left alone.
func repairIdentity(ctx context.Context, db *sql.DB, entries []storeregistry.Entry) ([]identityRepairResult, error) {
	missing := map[string]bool{}
	for _, p := range identityProblems(ctx, db, entries) {
		missing[p.Store] = true
	}
	home, _ := os.UserHomeDir()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	var res []identityRepairResult
	seen := map[string]bool{}
	for _, e := range storeregistry.Active(entries) {
		if e.Kind != storeregistry.KindStore || seen[e.Store] {
			continue
		}
		seen[e.Store] = true
		pid := localProjectID(filepath.Join(home, filepath.FromSlash(e.RelPath)))
		if pid == "" {
			if missing[e.Store] {
				res = append(res, identityRepairResult{Store: e.Store, Status: "no-local-id",
					Detail: fmt.Sprintf("this machine has no %s/.beads/metadata.json with a project_id; run '%s' on a machine that has it", e.Store, identityRepairCommand)})
			}
			continue
		}
		inserted, conflict, err := ensureProjectIDTx(ctx, tx, e.Store, pid)
		switch {
		case err != nil:
			_ = tx.Rollback()
			return nil, err
		case conflict != "":
			res = append(res, identityRepairResult{Store: e.Store, Status: "conflict",
				Detail: fmt.Sprintf("the database already has project id %q but this machine's metadata.json says %q; refusing to overwrite", conflict, pid)})
		case inserted:
			res = append(res, identityRepairResult{Store: e.Store, Status: "repaired", Detail: "wrote _project_id " + pid})
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}

// printIdentityRepair renders the results and reports whether anything is left unrepaired.
func printIdentityRepair(w io.Writer, res []identityRepairResult) (ok bool) {
	if len(res) == 0 {
		fmt.Fprintln(w, "identity: every registered store has a _project_id row; nothing to repair")
		return true
	}
	ok = true
	for _, r := range res {
		fmt.Fprintf(w, "%-12s %-9s %s\n", r.Store, strings.ToUpper(r.Status), r.Detail)
		if r.Status != "repaired" {
			ok = false
		}
	}
	return ok
}

// runDoctorRepairIdentity is 'brain stores doctor --repair-identity'.
func runDoctorRepairIdentity(ctx context.Context) {
	db, err := registryDBFunc()
	if err != nil || db == nil {
		fmt.Fprintf(os.Stderr, "Error: cannot reach the shared database: %v\n", err)
		os.Exit(2)
	}
	snap, found, err := storeregistry.ReadSnapshot(ctx, db)
	if err != nil || !found {
		fmt.Fprintf(os.Stderr, "Error: the registry is not readable or not adopted here: %v\n", err)
		os.Exit(2)
	}
	res, err := repairIdentity(ctx, db, snap.Entries)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}
	if storesDoctorJSON || jsonOutput {
		_ = outputJSON(res)
	} else {
		printIdentityRepair(os.Stdout, res)
	}
	for _, r := range res {
		if r.Status != "repaired" {
			os.Exit(1)
		}
	}
}

// identityAudit collects the shared database identity check for a doctor run.
type identityAudit struct {
	Problems   []identityProblem
	Unverified bool
}

func identityAuditFor(ctx context.Context, db storeregistry.DBTX, entries []storeregistry.Entry, relayClient bool) identityAudit {
	if relayClient {
		return identityAudit{Unverified: true}
	}
	if db == nil {
		return identityAudit{}
	}
	return identityAudit{Problems: identityProblems(ctx, db, entries)}
}

func (a identityAudit) apply(results []storeHealth) []storeHealth {
	if a.Unverified {
		return append(results, storeHealth{Name: "__identity__", Probe: "registry", Status: "fail", Reason: "identity unverified (relay client)"})
	}
	for _, p := range a.Problems {
		reason := fmt.Sprintf("registered but has no _project_id row in the shared database (no other machine can create it); repair: %s", p.Repair)
		found := false
		for i := range results {
			if results[i].Name == p.Store {
				results[i].Status = "fail"
				if results[i].Reason != "" {
					reason = results[i].Reason + "; " + reason
				}
				results[i].Reason = reason
				found = true
			}
		}
		if !found {
			results = append(results, storeHealth{Name: p.Store, Probe: "registry", Status: "fail", Reason: reason})
		}
	}
	return results
}

func (a identityAudit) fleetFindings() []string {
	if a.Unverified {
		return []string{"FAIL identity unverified (relay client)"}
	}
	findings := make([]string, 0, len(a.Problems))
	for _, p := range a.Problems {
		findings = append(findings, "FAIL "+p.String())
	}
	return findings
}

// localIdentityAudit reads this machine's view of the shared registry.
func localIdentityAudit(ctx context.Context) identityAudit {
	if _, isClient := clientSourceIfConfigured(); isClient {
		return identityAuditFor(ctx, nil, nil, true)
	}
	db, err := registryDBFunc()
	if err != nil || db == nil {
		return identityAudit{}
	}
	snap, found, err := storeregistry.ReadSnapshot(ctx, db)
	if err != nil || !found {
		return identityAudit{}
	}
	return identityAuditFor(ctx, db, snap.Entries, false)
}
