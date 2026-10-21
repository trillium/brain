package storeregistry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DBTX is the slice of *sql.DB / *sql.Tx the registry needs.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DDL for the four registry tables. They are created lazily with CREATE TABLE
// IF NOT EXISTS inside the transaction that first writes them (the pattern
// EnsureStorePrefixEventsTable uses), so existing databases upgrade
// additively and nothing is created until the first lifecycle write.
var DDL = []string{
	// One row; the version counter every machine polls.
	`CREATE TABLE IF NOT EXISTS brain_registry_state (
  id tinyint NOT NULL,
  epoch bigint NOT NULL,
  digest char(64) NOT NULL,
  updated_at datetime NOT NULL,
  updated_by varchar(128) NOT NULL,
  PRIMARY KEY (id)
)`,
	// Desired state: one row per command name (stores and aliases); tombstones
	// are kept so a machine that was offline still learns of a removal.
	`CREATE TABLE IF NOT EXISTS brain_registry_entries (
  name varchar(128) NOT NULL,
  kind varchar(8) NOT NULL,
  store varchar(128) NOT NULL,
  state varchar(8) NOT NULL,
  rel_path varchar(512) NOT NULL,
  wrapper_kind varchar(8) NOT NULL,
  about text NOT NULL,
  edit_back tinyint(1) NOT NULL DEFAULT 0,
  created_epoch bigint NOT NULL,
  changed_epoch bigint NOT NULL,
  PRIMARY KEY (name)
)`,
	// Append-only history, same discipline as brain_store_prefix_events.
	`CREATE TABLE IF NOT EXISTS brain_registry_log (
  epoch bigint NOT NULL,
  seq int NOT NULL,
  op varchar(16) NOT NULL,
  name varchar(128) NOT NULL,
  old_name varchar(128) NOT NULL DEFAULT '',
  actor varchar(128) NOT NULL,
  machine varchar(128) NOT NULL,
  at datetime NOT NULL,
  detail text NOT NULL,
  PRIMARY KEY (epoch, seq)
)`,
	// What each machine reports it has applied.
	`CREATE TABLE IF NOT EXISTS brain_registry_machines (
  machine varchar(128) NOT NULL,
  role varchar(8) NOT NULL,
  relay varchar(128) NOT NULL DEFAULT '',
  applied_epoch bigint NOT NULL,
  applied_digest char(64) NOT NULL,
  checked_at datetime NOT NULL,
  wrappers_ok int NOT NULL,
  missing text NOT NULL,
  orphans text NOT NULL,
  bd_version varchar(64) NOT NULL,
  last_error text NOT NULL,
  retired tinyint(1) NOT NULL DEFAULT 0,
  PRIMARY KEY (machine)
)`,
}

const timeFmt = "2006-01-02 15:04:05"

// EnsureSchema creates the registry tables if they are missing. Call it inside
// the transaction that writes, never after.
func EnsureSchema(ctx context.Context, q DBTX) error {
	for _, stmt := range DDL {
		if _, err := q.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("provisioning the store registry: %w", err)
		}
	}
	return nil
}

// isNoTable recognises the "table does not exist" errors of MySQL and Dolt.
func isNoTable(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "doesn't exist") || strings.Contains(s, "table not found") ||
		strings.Contains(s, "does not exist") || strings.Contains(s, "error 1146")
}

// ReadState reads the version record. found is false when the registry has not
// been created yet (no table, or no row): an un-adopted database, not an error.
func ReadState(ctx context.Context, q DBTX) (st State, found bool, err error) {
	var at sql.NullString
	err = q.QueryRowContext(ctx,
		"SELECT epoch, digest, CAST(updated_at AS CHAR), updated_by FROM brain_registry_state WHERE id = 1").
		Scan(&st.Epoch, &st.Digest, &at, &st.UpdatedBy)
	switch {
	case errors.Is(err, sql.ErrNoRows), isNoTable(err):
		return State{}, false, nil
	case err != nil:
		return State{}, false, fmt.Errorf("reading the registry epoch: %w", err)
	}
	st.UpdatedAt = at.String
	return st, true, nil
}

// ReadEpoch is the ride-along read: one primary-key lookup of one small row.
func ReadEpoch(ctx context.Context, q DBTX) (epoch int64, digest string, found bool, err error) {
	err = q.QueryRowContext(ctx, "SELECT epoch, digest FROM brain_registry_state WHERE id = 1").Scan(&epoch, &digest)
	switch {
	case errors.Is(err, sql.ErrNoRows), isNoTable(err):
		return 0, "", false, nil
	case err != nil:
		return 0, "", false, fmt.Errorf("reading the registry epoch: %w", err)
	}
	return epoch, digest, true, nil
}

// ReadEntries returns every entry (active and tombstones), sorted by name. A
// missing table reads as an empty registry.
func ReadEntries(ctx context.Context, q DBTX) ([]Entry, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT name, kind, store, state, rel_path, wrapper_kind, about, edit_back, created_epoch, changed_epoch FROM brain_registry_entries ORDER BY name")
	if err != nil {
		if isNoTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading registry entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Entry
	for rows.Next() {
		var e Entry
		var eb int
		if err := rows.Scan(&e.Name, &e.Kind, &e.Store, &e.State, &e.RelPath, &e.WrapperKind, &e.About, &eb, &e.CreatedEpoch, &e.ChangedEpoch); err != nil {
			return nil, fmt.Errorf("scanning registry entry: %w", err)
		}
		e.EditBack = eb != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// Snapshot is a consistent read of the registry: the version record and the
// entries, with the digest recomputed from the entries and checked against the
// record. A mismatch is a torn read or a hand edit and is refused.
type Snapshot struct {
	State   State
	Entries []Entry
}

// ErrTornRead means the entries read do not hash to the recorded digest.
var ErrTornRead = errors.New("registry entries do not match the recorded digest (torn read or hand edit)")

// ReadSnapshot reads the state, then the entries, then the state again, so a
// write that lands in between is seen as a changed epoch and retried.
func ReadSnapshot(ctx context.Context, db DBTX) (Snapshot, bool, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		st, found, err := ReadState(ctx, db)
		if err != nil || !found {
			return Snapshot{}, found, err
		}
		entries, err := ReadEntries(ctx, db)
		if err != nil {
			return Snapshot{}, true, err
		}
		again, _, err := ReadState(ctx, db)
		if err != nil {
			return Snapshot{}, true, err
		}
		if again.Epoch != st.Epoch {
			continue
		}
		if got := Digest(entries); got != st.Digest {
			lastErr = fmt.Errorf("%w: recorded %.12s, computed %.12s at epoch %d", ErrTornRead, st.Digest, got, st.Epoch)
			continue
		}
		return Snapshot{State: st, Entries: entries}, true, nil
	}
	if lastErr == nil {
		lastErr = errors.New("registry kept changing while it was read")
	}
	return Snapshot{}, true, lastErr
}

// Change is one row-level result of a plan.
type Change struct {
	Op      string
	Entry   Entry // the full resulting row
	OldName string
	Detail  string // JSON text; "" is stored as {}
}

// PlanFunc turns the current entries into changes. It must be pure: Mutate
// may call it again after losing a race on the epoch.
type PlanFunc func(cur map[string]Entry) ([]Change, error)

// Meta says who is writing, and what else must land in the same transaction.
type Meta struct {
	Actor   string
	Machine string
	// InTx runs inside the transaction, after the entries are written and
	// before the epoch moves. Whatever it writes lands with the epoch bump or
	// not at all (the namespace seed of 'stores create' goes here).
	InTx func(ctx context.Context, tx DBTX) error
	Now  func() time.Time
}

// Result is the outcome of a Mutate.
type Result struct {
	Epoch   int64
	Digest  string
	Changed bool
	Changes []Change
}

// ErrConflict is returned when the epoch kept moving under every retry.
var ErrConflict = errors.New("registry epoch kept changing; too many concurrent writers")

const maxAttempts = 8

// Mutate applies a plan atomically: the entries, the log rows, InTx's writes
// and the epoch bump commit together or not at all. The epoch is moved with a
// compare-and-swap (UPDATE ... WHERE epoch = <the epoch the plan was made
// against>); zero rows affected, or a serialization failure at commit, means
// another writer won, so the plan is re-run on the fresh state. A plan that
// yields no changes writes nothing and does not move the epoch.
func Mutate(ctx context.Context, db *sql.DB, meta Meta, plan PlanFunc) (Result, error) {
	now := meta.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return Result{}, ctx.Err()
			case <-time.After(time.Duration(attempt*25) * time.Millisecond):
			}
		}
		res, retry, err := mutateOnce(ctx, db, meta, plan, now())
		if err == nil {
			return res, nil
		}
		if !retry {
			return Result{}, err
		}
		lastErr = err
	}
	return Result{}, fmt.Errorf("%w: %v", ErrConflict, lastErr)
}

func mutateOnce(ctx context.Context, db *sql.DB, meta Meta, plan PlanFunc, at time.Time) (res Result, retry bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, false, fmt.Errorf("opening the registry transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := EnsureSchema(ctx, tx); err != nil {
		return Result{}, false, err
	}
	st, found, err := ReadState(ctx, tx)
	if err != nil {
		return Result{}, false, err
	}
	if !found {
		st = State{Epoch: 0, Digest: Digest(nil)}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO brain_registry_state (id, epoch, digest, updated_at, updated_by) VALUES (1, 0, ?, ?, ?)",
			st.Digest, at.Format(timeFmt), actorOf(meta)); err != nil {
			return Result{}, true, fmt.Errorf("initialising the registry epoch: %w", err)
		}
	}
	entries, err := ReadEntries(ctx, tx)
	if err != nil {
		return Result{}, false, err
	}
	cur := make(map[string]Entry, len(entries))
	for _, e := range entries {
		cur[e.Name] = e
	}
	changes, err := plan(cloneEntries(cur))
	if err != nil {
		return Result{}, false, err
	}
	if len(changes) == 0 {
		return Result{Epoch: st.Epoch, Digest: st.Digest}, false, nil
	}

	newEpoch := st.Epoch + 1
	for seq, c := range changes {
		e := c.Entry
		if old, ok := cur[e.Name]; ok {
			e.CreatedEpoch = old.CreatedEpoch
		} else {
			e.CreatedEpoch = newEpoch
		}
		e.ChangedEpoch = newEpoch
		if err := e.Validate(); err != nil {
			return Result{}, false, fmt.Errorf("refusing to write registry entry: %w", err)
		}
		eb := 0
		if e.EditBack {
			eb = 1
		}
		if _, err := tx.ExecContext(ctx,
			"REPLACE INTO brain_registry_entries (name, kind, store, state, rel_path, wrapper_kind, about, edit_back, created_epoch, changed_epoch) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			e.Name, e.Kind, e.Store, e.State, e.RelPath, e.WrapperKind, e.About, eb, e.CreatedEpoch, e.ChangedEpoch); err != nil {
			return Result{}, true, fmt.Errorf("writing registry entry %q: %w", e.Name, err)
		}
		cur[e.Name] = e
		changes[seq].Entry = e
		detail := c.Detail
		if detail == "" {
			detail = "{}"
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO brain_registry_log (epoch, seq, op, name, old_name, actor, machine, at, detail) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			newEpoch, seq, c.Op, e.Name, c.OldName, meta.Actor, meta.Machine, at.Format(timeFmt), detail); err != nil {
			return Result{}, true, fmt.Errorf("appending registry log: %w", err)
		}
	}
	if meta.InTx != nil {
		if err := meta.InTx(ctx, tx); err != nil {
			return Result{}, false, err
		}
	}

	all := make([]Entry, 0, len(cur))
	for _, e := range cur {
		all = append(all, e)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	digest := Digest(all)
	r, err := tx.ExecContext(ctx,
		"UPDATE brain_registry_state SET epoch = ?, digest = ?, updated_at = ?, updated_by = ? WHERE id = 1 AND epoch = ?",
		newEpoch, digest, at.Format(timeFmt), actorOf(meta), st.Epoch)
	if err != nil {
		return Result{}, true, fmt.Errorf("moving the registry epoch: %w", err)
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return Result{}, true, fmt.Errorf("lost the race for epoch %d", newEpoch)
	}
	if err := tx.Commit(); err != nil {
		committed = true // the driver finished the transaction either way
		return Result{}, true, fmt.Errorf("committing registry epoch %d: %w", newEpoch, err)
	}
	committed = true
	return Result{Epoch: newEpoch, Digest: digest, Changed: true, Changes: changes}, false, nil
}

func actorOf(m Meta) string {
	if m.Machine == "" {
		return m.Actor
	}
	return m.Machine + ":" + m.Actor
}

func cloneEntries(in map[string]Entry) map[string]Entry {
	out := make(map[string]Entry, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// ReportMachine records what a machine has applied. It never moves the epoch.
func ReportMachine(ctx context.Context, db *sql.DB, m Machine, at time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := EnsureSchema(ctx, tx); err != nil {
		return err
	}
	retired := 0
	if m.Retired {
		retired = 1
	}
	if m.Missing == "" {
		m.Missing = "{}"
	}
	if m.Orphans == "" {
		m.Orphans = "[]"
	}
	if _, err := tx.ExecContext(ctx,
		"REPLACE INTO brain_registry_machines (machine, role, relay, applied_epoch, applied_digest, checked_at, wrappers_ok, missing, orphans, bd_version, last_error, retired) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		m.Machine, m.Role, m.Relay, m.AppliedEpoch, m.AppliedDigest, at.UTC().Format(timeFmt), m.WrappersOK,
		m.Missing, m.Orphans, m.BdVersion, m.LastError, retired); err != nil {
		return fmt.Errorf("reporting machine %q: %w", m.Machine, err)
	}
	return tx.Commit()
}

// ReadMachines returns every machine's last report, sorted by name.
func ReadMachines(ctx context.Context, q DBTX) ([]Machine, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT machine, role, relay, applied_epoch, applied_digest, CAST(checked_at AS CHAR), wrappers_ok, missing, orphans, bd_version, last_error, retired FROM brain_registry_machines ORDER BY machine")
	if err != nil {
		if isNoTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading registry machines: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Machine
	for rows.Next() {
		var m Machine
		var retired int
		if err := rows.Scan(&m.Machine, &m.Role, &m.Relay, &m.AppliedEpoch, &m.AppliedDigest, &m.CheckedAt,
			&m.WrappersOK, &m.Missing, &m.Orphans, &m.BdVersion, &m.LastError, &retired); err != nil {
			return nil, err
		}
		m.Retired = retired != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// ReadLog returns the newest log rows first.
func ReadLog(ctx context.Context, q DBTX, limit int) ([]LogRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := q.QueryContext(ctx,
		"SELECT epoch, seq, op, name, old_name, actor, machine, CAST(at AS CHAR), detail FROM brain_registry_log ORDER BY epoch DESC, seq DESC LIMIT ?", limit)
	if err != nil {
		if isNoTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LogRow
	for rows.Next() {
		var r LogRow
		if err := rows.Scan(&r.Epoch, &r.Seq, &r.Op, &r.Name, &r.OldName, &r.Actor, &r.Machine, &r.At, &r.Detail); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
