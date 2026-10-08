// Package main — outbox.go
//
// Durable event outbox. When change-events.outbox.enabled is set, every write
// command records one event per mutated issue in the event_outbox table inside
// the store's own database, and delivery retries each event with bounded backoff until every configured
// subscriber acknowledges it.
//
// The outbox is the event log and it is part of the record: it lives in the
// store's database with the data it describes, next to the events table that
// feeds it, so it can be backed up, pushed, and diffed with the data instead
// of drifting from it in a side file. change-events.jsonl (if
// change-events.enabled is also set) remains a convenience tail target; the
// store is authoritative. See docs/brain/event-outbox.md.
//
// Delivery contract (full semantics in docs/brain/event-outbox.md):
//   - An event is retired only when acknowledgement is recorded.
//   - Acknowledgement for a subscriber X is: an HTTP 2xx response within the
//     delivery timeout, or a command subscriber exiting 0 within the timeout,
//     or an operator `bd outbox ack` recording the acknowledgement manually.
//   - An unacknowledged event stays visible in `bd outbox list` forever; the
//     backlog is bounded by change-events.outbox.max-pending and reaching the
//     bound refuses new writes loudly rather than dropping events.
//
// Atomicity, stated exactly (see docs/brain/event-outbox.md "What is atomic"):
//   - All events of one command are inserted in ONE database transaction: a
//     command's events exist together or not at all.
//   - A failure to record them fails the command loudly (non-zero exit, named
//     error) — never a silent eventless mutation.
//   - The insert happens in the command's post-run, a second transaction after
//     the store's own mutation transaction. A process killed in the window
//     between the two leaves a committed mutation without an event. That window
//     is not closed by this feature; it is documented, not hidden.
//   - The preflight (subscriber definitions readable, backlog under the bound,
//     table present) runs BEFORE the mutation, so every foreseeable refusal
//     happens while nothing has been written.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/issueops"
)

// outboxTable is the outbox table inside the store's database. It always carries
// a `store` column: empty on a per-store database (the whole database is the
// store; the store label travels in the payload), the namespace on the unified
// database, where scoped queries filter on it (same pattern as local_metadata).
const outboxTable = "event_outbox"

// outboxEvent is the durable twin of changeEvent (change_events.go): one event
// per mutated issue per write command. The JSON payload stored in `payload`
// (and delivered to subscribers) has exactly those four fields plus the
// outbox sequence number the event was recorded under.
type outboxEvent struct {
	TS      string `json:"ts"`      // RFC3339 UTC timestamp of the write
	Store   string `json:"store"`   // logical store name (BD_NAME / store dir)
	Command string `json:"command"` // cobra command name that wrote
	ID      string `json:"id"`      // mutated issue id
	Seq     int64  `json:"seq"`     // assigned at insert; 0 in the emission draft
}

// outboxRow is one event_outbox row as read back from the database.
type outboxRow struct {
	Seq              int64
	CreatedAt        string
	Store            string
	Command          string
	IssueID          string
	Payload          string
	Subscribers      []string          // parsed from SubscribersRaw
	SubscribersRaw   string            // JSON array of subscriber names, frozen at emission
	Acks             map[string]string // subscriber name -> RFC3339 UTC ack time
	AcksRaw          string            // JSON object
	Attempts         int
	NextAttemptAfter string
	LastError        string
	DeliveredAt      string // non-empty when retired
}

// retired reports whether every subscriber frozen on the row has acknowledged.
// A row with an empty subscriber snapshot retires at emission: there is nobody
// to deliver to, so there is nothing to wait for.
func (r *outboxRow) retired() bool {
	if r.DeliveredAt != "" {
		return true
	}
	if len(r.Subscribers) == 0 {
		return true
	}
	return len(r.acksFor(r.Subscribers)) == len(r.Subscribers)
}

// acksFor filters r.Acks to the given names.
func (r *outboxRow) acksFor(names []string) map[string]string {
	out := make(map[string]string, len(names))
	for _, n := range names {
		if ts, ok := r.Acks[n]; ok {
			out[n] = ts
		}
	}
	return out
}

// awaiting returns the subscriber names that have not acknowledged.
func (r *outboxRow) awaiting() []string {
	var out []string
	for _, n := range r.Subscribers {
		if _, ok := r.Acks[n]; !ok {
			out = append(out, n)
		}
	}
	return out
}

// corrupt reports why the row cannot be trusted. Empty string means the row is
// well-formed. Corruption is never repaired silently or dropped: it is a named
// failure surfaced by every outbox verb.
func (r *outboxRow) corruptReason() string {
	if r.SubscribersRaw == "" {
		return "empty subscribers column"
	}
	if r.AcksRaw == "" {
		return "empty acks column"
	}
	return ""
}

// parseOutboxJSON decodes the two JSON columns into outboxRow.
func (r *outboxRow) parseOutboxJSON() error {
	if reason := r.corruptReason(); reason != "" {
		return fmt.Errorf("corrupt outbox row seq=%d: %s", r.Seq, reason)
	}
	if err := json.Unmarshal([]byte(r.SubscribersRaw), &r.Subscribers); err != nil {
		return fmt.Errorf("corrupt outbox row seq=%d: subscribers column: %w", r.Seq, err)
	}
	sort.Strings(r.Subscribers)
	if err := json.Unmarshal([]byte(r.AcksRaw), &r.Acks); err != nil {
		return fmt.Errorf("corrupt outbox row seq=%d: acks column: %w", r.Seq, err)
	}
	return nil
}

// outboxNow is the clock used for every outbox timestamp: RFC3339 UTC, so the
// string columns sort lexically and the stored values survive any Dolt or host
// timezone setting.
func outboxNow() string { return time.Now().UTC().Format(time.RFC3339) }

// outboxDefaultEffort is the opportunistic delivery budget when the config
// key is unset or nonsensical (see docs/brain/event-outbox.md).
const outboxDefaultEffort = 5 * time.Second

// ── SQL layer ────────────────────────────────────────────────────────────

// outboxDDL is the lazy-create DDL for the outbox table. The table is created
// on first emission, not by a schema migration: stores that never enable the
// feature never gain the table.
var outboxDDL = `CREATE TABLE IF NOT EXISTS event_outbox (
    seq BIGINT NOT NULL AUTO_INCREMENT,
    created_at VARCHAR(40) NOT NULL,
    store VARCHAR(255) NOT NULL,
    command VARCHAR(255) NOT NULL,
    issue_id VARCHAR(255) NOT NULL,
    payload TEXT NOT NULL,
    subscribers TEXT NOT NULL,
    acks TEXT NOT NULL,
    attempts INT NOT NULL DEFAULT 0,
    next_attempt_after VARCHAR(40) NOT NULL,
    last_error TEXT NOT NULL,
    delivered_at VARCHAR(40) NOT NULL DEFAULT '',
    PRIMARY KEY (seq)
)`

// ensureOutboxTable creates the outbox table if absent.
func ensureOutboxTable(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, outboxDDL); err != nil {
		return fmt.Errorf("create %s: %w", outboxTable, err)
	}
	return nil
}

// outboxScopeKey returns the store filter for scoped queries. On a unified
// database the namespace (BD_NAME) selects the wrapper's own rows; elsewhere
// the empty string means no filter. Mirrors issueops.SetLocalMetadataInTx.
func outboxScopeKey(ctx context.Context, db *sql.DB) (string, error) {
	sc := issueops.UnifiedScopeForTx(ctx, db)
	if !sc.Enabled() {
		return "", nil
	}
	return sc.Store, nil
}

// outboxWhere builds a WHERE clause from conditions that carry their own
// arguments, appending the scoped-store filter last (empty store = no filter).
func outboxWhere(store string, conds []string, args []interface{}) (string, []interface{}) {
	if store != "" {
		conds = append(append([]string{}, conds...), "store = ?")
		args = append(append([]interface{}{}, args...), store)
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// suffixStore is the scoped-store clause for single-row updates:
// " AND store = ?" on the unified database, empty elsewhere. The matching
// argument goes last (see outboxStoreArgs).
func suffixStore(store string) string {
	if store != "" {
		return " AND store = ?"
	}
	return ""
}

// outboxStoreArgs appends the scoped-store argument after positional args.
func outboxStoreArgs(store string, args ...interface{}) []interface{} {
	if store != "" {
		return append(args, store)
	}
	return args
}

// outboxExecer is the write surface shared by *sql.DB and *sql.Tx.
type outboxExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// insertOutboxEvent inserts one event and returns its sequence number.
// deliveredAt non-empty inserts the row already retired (no subscribers).
func insertOutboxEvent(ctx context.Context, ex outboxExecer, store string, ev outboxEvent, payload, subscribersJSON, acksJSON, now, deliveredAt string) (int64, error) {
	res, err := ex.ExecContext(ctx, `INSERT INTO `+outboxTable+`
		(created_at, store, command, issue_id, payload, subscribers, acks, attempts, next_attempt_after, last_error, delivered_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, '', ?)`,
		now, store, ev.Command, ev.ID, payload, subscribersJSON, acksJSON, now, deliveredAt)
	if err != nil {
		return 0, fmt.Errorf("insert outbox event: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert outbox event: last insert id: %w", err)
	}
	return seq, nil
}

// countPendingOutbox counts rows still awaiting delivery or acknowledgement —
// the operator-visible backlog the bound applies to.
func countPendingOutbox(ctx context.Context, db *sql.DB, store string) (int, error) {
	where, args := outboxWhere(store, []string{"delivered_at = ''"}, nil)
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+outboxTable+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count pending outbox: %w", err)
	}
	return n, nil
}

const outboxRowColumns = `seq, created_at, store, command, issue_id, payload, subscribers, acks, attempts, next_attempt_after, last_error, delivered_at`

// selectOutboxRows returns rows ordered by sequence. pending=true filters to
// unretired rows; dueFor limits to rows whose next attempt time has arrived.
// A corrupt row is never silently skipped: it is returned in corrupt as a
// named failure while the healthy rows flow normally.
func selectOutboxRows(ctx context.Context, db *sql.DB, store string, pending, dueFor bool, now string, limit int) (out []*outboxRow, corrupt []error, err error) {
	var conds []string
	var args []interface{}
	if pending || dueFor {
		conds = append(conds, "delivered_at = ''")
	}
	if dueFor {
		conds = append(conds, "next_attempt_after <= ?")
		args = append(args, now)
	}
	where, args := outboxWhere(store, conds, args)
	q := `SELECT ` + outboxRowColumns + ` FROM ` + outboxTable + where + " ORDER BY seq ASC"
	if limit > 0 {
		q += " LIMIT " + strconv.Itoa(limit)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("select outbox rows: %w", err)
	}
	defer rows.Close()
	out = []*outboxRow{}
	for rows.Next() {
		r, scanErr := scanOutboxRow(rows)
		if scanErr != nil {
			corrupt = append(corrupt, scanErr)
			continue
		}
		out = append(out, r)
	}
	return out, corrupt, rows.Err()
}

// scanOutboxRow scans the columns then parses the JSON payload
// columns, surfacing corruption as a named error instead of aborting the
// whole result set.
func scanOutboxRow(rows *sql.Rows) (*outboxRow, error) {
	raw := struct {
		Seq            int64
		CreatedAt      string
		Store          string
		Command        string
		IssueID        string
		Payload        string
		SubscribersRaw string
		AcksRaw        string
		Attempts       int
		NextAttempt    string
		LastError      string
		DeliveredAt    string
	}{}
	if err := rows.Scan(&raw.Seq, &raw.CreatedAt, &raw.Store, &raw.Command, &raw.IssueID, &raw.Payload,
		&raw.SubscribersRaw, &raw.AcksRaw, &raw.Attempts, &raw.NextAttempt, &raw.LastError, &raw.DeliveredAt); err != nil {
		return nil, fmt.Errorf("scan outbox row: %w", err)
	}
	r := &outboxRow{
		Seq: raw.Seq, CreatedAt: raw.CreatedAt, Store: raw.Store, Command: raw.Command,
		IssueID: raw.IssueID, Payload: raw.Payload,
		SubscribersRaw: raw.SubscribersRaw, AcksRaw: raw.AcksRaw,
		Attempts: raw.Attempts, NextAttemptAfter: raw.NextAttempt,
		LastError: raw.LastError, DeliveredAt: raw.DeliveredAt,
	}
	if scanErr := r.parseOutboxJSON(); scanErr != nil {
		return nil, scanErr
	}
	return r, nil
}

// claimOutboxAttempt is a compare-and-set: the row's attempt counter is
// advanced (and its next-attempt time and error updated) only if the caller
// still sees the expected attempt count, so two concurrent delivering
// processes cannot both claim the same attempt. False = someone else claimed
// it; the row's own later pass will handle it.
func claimOutboxAttempt(ctx context.Context, db *sql.DB, store string, seq int64, expectedAttempts int, now, nextAfter, attemptErr string) (bool, error) {
	q := `UPDATE ` + outboxTable + ` SET attempts = attempts + 1, next_attempt_after = ?, last_error = ?
		WHERE seq = ? AND attempts = ?` + suffixStore(store)
	res, err := db.ExecContext(ctx, q, outboxStoreArgs(store, nextAfter, attemptErr, seq, expectedAttempts)...)
	if err != nil {
		return false, fmt.Errorf("update outbox row seq=%d: %w", seq, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update outbox row seq=%d: rows affected: %w", seq, err)
	}
	return n > 0, nil
}

// recordOutboxAck merges one acknowledgement into the row and retires the row
// when every frozen subscriber has acknowledged. Acknowledging twice is
// idempotent: the first acknowledgement timestamp wins.
func recordOutboxAck(ctx context.Context, db *sql.DB, store string, seq int64, subscriber, ackAt string) error {
	row, err := getOutboxRow(ctx, db, store, seq)
	if err != nil {
		return err
	}
	if _, dup := row.Acks[subscriber]; !dup {
		row.Acks[subscriber] = ackAt
	}
	acksJSON, err := json.Marshal(row.Acks)
	if err != nil {
		return fmt.Errorf("outbox row seq=%d: marshal acks: %w", seq, err)
	}
	deliveredAt := row.DeliveredAt
	if deliveredAt == "" && row.retired() {
		deliveredAt = ackAt
	}
	q := `UPDATE ` + outboxTable + ` SET acks = ?, delivered_at = ? WHERE seq = ?` + suffixStore(store)
	if _, err := db.ExecContext(ctx, q, outboxStoreArgs(store, string(acksJSON), deliveredAt, seq)...); err != nil {
		return fmt.Errorf("update outbox row seq=%d: %w", seq, err)
	}
	return nil
}

// getOutboxRow reads one row by sequence.
func getOutboxRow(ctx context.Context, db *sql.DB, store string, seq int64) (*outboxRow, error) {
	q := `SELECT ` + outboxRowColumns + ` FROM ` + outboxTable + ` WHERE seq = ?` + suffixStore(store)
	rows, err := db.QueryContext(ctx, q, outboxStoreArgs(store, seq)...)
	if err != nil {
		return nil, fmt.Errorf("select outbox row seq=%d: %w", seq, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("select outbox row seq=%d: %w", seq, err)
		}
		return nil, fmt.Errorf("outbox event seq=%d not found", seq)
	}
	r, err := scanOutboxRow(rows)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// deleteRetiredOutboxBefore deletes retired events (delivered_at non-empty)
// with delivered_at < before. Retired events remain part of the record until
// the operator explicitly purges them; nothing is ever deleted automatically.
func deleteRetiredOutboxBefore(ctx context.Context, db *sql.DB, store string, before string) (int64, error) {
	q := `DELETE FROM ` + outboxTable + ` WHERE delivered_at <> '' AND delivered_at < ?` + suffixStore(store)
	res, err := db.ExecContext(ctx, q, outboxStoreArgs(store, before)...)
	if err != nil {
		return 0, fmt.Errorf("purge outbox: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purge outbox: rows affected: %w", err)
	}
	return n, nil
}

// ── Emission ─────────────────────────────────────────────────────────────

// outboxDB resolves the raw database handle for the open store. The outbox
// writes through the store's own connection, so an outbox row can never exist
// in a place the store's record cannot reach.
func outboxDB() (*sql.DB, error) {
	if store == nil {
		return nil, fmt.Errorf("event outbox: no store is open")
	}
	acc, ok := storage.UnwrapStore(store).(storage.RawDBAccessor)
	if !ok {
		return nil, fmt.Errorf("event outbox: the open store does not expose raw database access (embedded mode); the durable outbox requires a dolt sql-server store, so no event can be recorded — refusing rather than emitting an event that is not part of the record")
	}
	return acc.DB(), nil
}

// outboxBoundError is the bound decision: adding `adding` events to `pending`
// unretired ones must not exceed `bound` (<= 0 disables the bound). Reaching
// it is a named refusal — never a silent drop or a silent throttle.
func outboxBoundError(pending, adding, bound int) error {
	if bound > 0 && pending+adding > bound {
		return fmt.Errorf("event outbox full: %d pending events (%d more queued by this command) exceed the bound change-events.outbox.max-pending=%d — run 'bd outbox deliver', fix or 'bd outbox ack' the subscriber, or raise the bound; refusing the command before it writes rather than recording an event that may never be delivered",
			pending, adding, bound)
	}
	return nil
}

// outboxExemptCommands never trip the preflight: the verbs an operator needs to
// drain or reconfigure a full outbox, and commands that never mutate issues.
var outboxExemptCommands = map[string]bool{
	"outbox": true, "config": true, "vc": true, "dolt": true, "doctor": true,
	"help": true, "version": true, "prime": true, "init": true, "bootstrap": true,
	"completion": true, "where": true, "info": true, "status": true, "query": true,
}

// outboxPreflight runs before every command once the store is open. For a
// command that may write (not read-only, not exempt) with the outbox enabled it
// refuses — loudly, before any mutation — when the subscriber definitions are
// unreadable or the unretired backlog has reached
// change-events.outbox.max-pending. Exempt: the 'outbox' verbs and 'config'
// (so an operator can always drain or reconfigure), plus read-only commands.
func outboxPreflight(ctx context.Context, cmd *cobra.Command) error {
	if !config.GetBool("change-events.outbox.enabled") {
		return nil
	}
	if isReadOnlyCommand(cmd.Name()) || outboxExemptCommands[cmd.Name()] {
		return nil
	}
	for c := cmd; c != nil; c = c.Parent() {
		if outboxExemptCommands[c.Name()] {
			return nil
		}
	}
	if _, err := readOutboxConfig(); err != nil {
		return err
	}
	db, err := outboxDB()
	if err != nil {
		return err
	}
	if err := ensureOutboxTable(ctx, db); err != nil {
		return fmt.Errorf("event outbox: %w", err)
	}
	scope, err := outboxScopeKey(ctx, db)
	if err != nil {
		return fmt.Errorf("event outbox: scope probe failed: %w", err)
	}
	pending, err := countPendingOutbox(ctx, db, scope)
	if err != nil {
		return fmt.Errorf("event outbox: %w", err)
	}
	return outboxBoundError(pending, 1, config.GetInt("change-events.outbox.max-pending"))
}

// maybeRecordOutboxEvents records the current command's mutated issues as
// durable outbox events in one transaction. Called from PersistentPostRunE only
// when a real write happened.
//
// Failure modes are loud, never silent: a missing table, an unreachable
// database, a refused insert, or a bound breach all fail the command with a
// named error — a mutation whose event could not be recorded must not be
// wave-nodded through, because an eventless mutation is exactly the silent
// failure the outbox exists to prevent.
//
// Returns the number of events recorded (0 when the outbox is disabled).
func maybeRecordOutboxEvents(ctx context.Context, commandName string) (int, error) {
	if !config.GetBool("change-events.outbox.enabled") {
		return 0, nil
	}
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		return 0, fmt.Errorf("event outbox: no .beads directory found; refusing to emit an event that is not part of the record")
	}

	ids := commandChangedIDs
	if len(ids) == 0 {
		ids = []string{GetLastTouchedID()}
	}
	eventStore := changeEventStoreName(beadsDir)
	now := outboxNow()

	db, err := outboxDB()
	if err != nil {
		return 0, err
	}
	if err := ensureOutboxTable(ctx, db); err != nil {
		return 0, fmt.Errorf("event outbox: %w", err)
	}
	scope, err := outboxScopeKey(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("event outbox: scope probe failed: %w", err)
	}

	// The bound is enforced by outboxPreflight BEFORE the command mutates; the
	// mutation has already happened here, so the event is always recorded —
	// a command that mutates several issues may overshoot the bound by its own
	// mutation count, and an event is never dropped to enforce it.

	// An unreadable subscriber definition is a named refusal, never a guess:
	// emitting under it would freeze an empty snapshot and retire the event
	// with nobody to deliver to.
	cfg, err := readOutboxConfig()
	if err != nil {
		return 0, err
	}
	subs := make([]string, len(cfg))
	for i, sub := range cfg {
		subs[i] = sub.Name
	}
	subsJSON, err := json.Marshal(subs)
	if err != nil {
		return 0, fmt.Errorf("event outbox: marshal subscribers: %w", err)
	}
	// No subscribers configured: nothing to deliver, nothing to wait for.
	// The event retires at emission with delivered_at = created_at; the
	// record of what was emitted still exists.
	acksJSON := "{}"
	deliveredAt := ""
	if len(subs) == 0 {
		deliveredAt = now
	}

	// One transaction for every event of this command: all or nothing.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("event outbox: begin: %w", err)
	}
	for _, id := range ids {
		ev := outboxEvent{TS: now, Store: eventStore, Command: commandName, ID: id}
		payload, err := json.Marshal(ev)
		if err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("event outbox: marshal event for %s: %w", id, err)
		}
		if _, err := insertOutboxEvent(ctx, tx, scope, ev, string(payload), string(subsJSON), acksJSON, now, deliveredAt); err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("event outbox: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("event outbox: commit: %w", err)
	}
	outboxDirty = true
	return len(ids), nil
}

// outboxDirty is set when this process changed outbox rows; commitOutboxWork
// turns it into one Dolt commit.
var outboxDirty bool

// commitOutboxWork makes this process's outbox changes (new events plus
// delivery bookkeeping) part of Dolt history with one commit that stages only
// the event_outbox table. The rows are already durable in the working set the
// moment they are inserted; this commit is what puts them in history (and so in
// push/pull/backup). A no-op when nothing changed or the outbox is off.
func commitOutboxWork(ctx context.Context, message string) error {
	if !outboxDirty || !config.GetBool("change-events.outbox.enabled") {
		return nil
	}
	db, err := outboxDB()
	if err != nil {
		return err
	}
	if err := commitOutboxTable(ctx, db, message); err != nil {
		return err
	}
	outboxDirty = false
	return nil
}

// commitOutboxTable stages only event_outbox and commits it on one pinned
// connection (stage and commit must share a Dolt session). "Nothing to commit"
// is benign.
func commitOutboxTable(ctx context.Context, db *sql.DB, message string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "CALL DOLT_ADD(?)", outboxTable); err != nil {
		return fmt.Errorf("dolt add %s: %w", outboxTable, err)
	}
	author := fmt.Sprintf("%s <%s@beads.local>", getActor(), "outbox")
	if _, err := conn.ExecContext(ctx, "CALL DOLT_COMMIT('-m', ?, '--author', ?)", message, author); err != nil && !issueops.IsNothingToCommitError(err) {
		return fmt.Errorf("dolt commit: %w", err)
	}
	return nil
}

// forceRetireOutboxRow marks a row delivered without reading its JSON columns:
// the operator-ack --force path for a corrupt row. It is a named, deliberate
// operator decision ('bd outbox ack <seq> --force'), never an automatic one.
func forceRetireOutboxRow(ctx context.Context, db *sql.DB, store string, seq int64, at string) error {
	q := `UPDATE ` + outboxTable + ` SET delivered_at = ? WHERE seq = ? AND delivered_at = ''` + suffixStore(store)
	res, err := db.ExecContext(ctx, q, outboxStoreArgs(store, at, seq)...)
	if err != nil {
		return fmt.Errorf("update outbox row seq=%d: %w", seq, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("outbox event seq=%d not found or already retired", seq)
	}
	return nil
}

// markOutboxDelivered sets delivered_at without touching acks (used to finish
// retiring a row whose acknowledgements were all recorded before a crash).
func markOutboxDelivered(ctx context.Context, db *sql.DB, store string, seq int64, at string) error {
	q := `UPDATE ` + outboxTable + ` SET delivered_at = ? WHERE seq = ?` + suffixStore(store)
	if _, err := db.ExecContext(ctx, q, outboxStoreArgs(store, at, seq)...); err != nil {
		return fmt.Errorf("update outbox row seq=%d: %w", seq, err)
	}
	return nil
}

// maybeDeliverOutboxDue is the opportunistic delivery pass run after a write
// command's event commit. Delivery failure is normal (a down subscriber is a
// delay, not a defect) so it never fails the command: it attempts due events
// within the effort budget and leaves whatever could not be delivered exactly
// where it is, visibly, for the next pass or `bd outbox deliver`.
func maybeDeliverOutboxDue(ctx context.Context) {
	if !config.GetBool("change-events.outbox.enabled") {
		return
	}
	cfg, err := readOutboxConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: event outbox: %v\n", err)
		return
	}
	db, err := outboxDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
		return
	}
	scope, err := outboxScopeKey(ctx, db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: event outbox: scope probe failed: %v\n", err)
		return
	}
	budget := config.GetDuration("change-events.outbox.delivery-budget")
	if budget <= 0 {
		budget = outboxDefaultEffort
	}
	res := runOutboxDeliveryPass(ctx, db, scope, cfg, budget, "", "")
	if res.Delivered > 0 || res.Failed > 0 {
		outboxDirty = true
	}
	if res.Delivered > 0 {
		fmt.Fprintf(os.Stderr, "Event outbox: delivered %d event(s) this pass (%d failed attempts remain pending)\n", res.Delivered, res.Failed)
	}
	for _, c := range res.Corrupt {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", c)
	}
	if res.DeliveryError != nil {
		fmt.Fprintf(os.Stderr, "Warning: event outbox delivery pass stopped early: %v\n", res.DeliveryError)
	}
}
