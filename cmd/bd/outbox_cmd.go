// Package main — outbox_cmd.go
//
// 'bd outbox' — operator verbs for the durable event outbox.
//
//	bd outbox list                     the backlog: pending rows, attempts, errors, bound
//	bd outbox deliver                  one delivery pass: due events, acknowledged or backed off
//	bd outbox ack <seq> --subscriber   record an acknowledgement by hand (operator escape hatch)
//	bd outbox purge --before           delete retired events older than a timestamp
//
// See docs/brain/event-outbox.md for the full delivery contract.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/config"
)

// outboxRescueNow is a lexical maximum RFC3339 string: due-ness is compared
// lexically against next_attempt_after, so it makes every pending row due
// ('bd outbox deliver --all').
const outboxRescueNow = "9999-12-31T23:59:59Z"

var outboxCmd = &cobra.Command{
	Use:     "outbox",
	GroupID: "sync",
	Short:   "Durable event outbox: delivery state, retries, backlog",
	Long: `Operator verbs for the durable event outbox.

Every write command records one event per mutated issue in the event_outbox
table inside the store's own database — the same Dolt commit as the mutation —
and delivery retries each event with bounded backoff (1s, 2s, 4s, ... capping
at 1h) until every configured subscriber acknowledges it. A subscriber being
down delays an event; it never loses one.

  bd outbox list                      the backlog: pending rows, attempts, errors, bound
  bd outbox deliver                   one delivery pass over due events
  bd outbox ack <seq> --subscriber X  record an acknowledgement by hand
  bd outbox purge --before <RFC3339>  delete retired events older than this

Configuration (config.yaml):
  change-events.outbox.enabled: true
  change-events.outbox.subscribers:
    - "name=pulse;url=http://127.0.0.1:8099/events"
    - "name=archive;command=/usr/local/bin/archive-event"

An acknowledgement is an HTTP 2xx (within change-events.outbox.timeout) for a
url subscriber, exit 0 for a command subscriber, or 'bd outbox ack'. Only an
acknowledgement retires an event; an unacknowledged event stays visible in
'bd outbox list' forever.`,
}

// outboxOpen resolves the raw database for outbox verbs.
func outboxOpen() (context.Context, *sql.DB, string, error) {
	db, err := outboxDB()
	if err != nil {
		return nil, nil, "", err
	}
	ctx := rootCtx
	scope, err := outboxScopeKey(ctx, db)
	if err != nil {
		return nil, nil, "", fmt.Errorf("event outbox: scope probe failed: %w", err)
	}
	if err := ensureOutboxTable(ctx, db); err != nil {
		return nil, nil, "", fmt.Errorf("event outbox: %w", err)
	}
	return ctx, db, scope, nil
}

// commitOutboxBookkeeping commits the working set through the open store when
// embedded auto-commit applies, so ack/attempt bookkeeping written by these
// verbs is part of store history. Server-mode stores own their own commit
// lifecycle; there it no-ops.
func commitOutboxBookkeeping(ctx context.Context, what string) error {
	return commitPendingIfEmbedded(ctx, store, getActor(), doltAutoCommitParams{
		Command:         "outbox",
		MessageOverride: fmt.Sprintf("event outbox: %s", what),
	})
}

// ── outbox list ──────────────────────────────────────────────────────────

var outboxListPendingOnly bool

var outboxListCmd = &cobra.Command{
	Use:   "list",
	Short: "Show outbox events: the backlog and its bound",
	Long: `List outbox events with the delivery state of each.

The summary line shows what the backoff-and-bound story is anchored on: events
not yet retired (pending) against change-events.outbox.max-pending. Reaching
the bound refuses new writes loudly; see docs/brain/event-outbox.md.`,
	RunE: runOutboxList,
}

type outboxEventJSON struct {
	Seq              int64             `json:"seq"`
	CreatedAt        string            `json:"created_at"`
	Store            string            `json:"store"`
	Command          string            `json:"command"`
	IssueID          string            `json:"issue_id"`
	Payload          json.RawMessage   `json:"payload"`
	Subscribers      []string          `json:"subscribers"`
	Acks             map[string]string `json:"acks,omitempty"`
	Awaiting         []string          `json:"awaiting,omitempty"`
	Attempts         int               `json:"attempts"`
	NextAttemptAfter string            `json:"next_attempt_after,omitempty"`
	LastError        string            `json:"last_error,omitempty"`
	DeliveredAt      string            `json:"delivered_at,omitempty"`
}

func outboxRowToJSON(r *outboxRow) outboxEventJSON {
	out := outboxEventJSON{
		Seq:         r.Seq,
		CreatedAt:   r.CreatedAt,
		Store:       r.Store,
		Command:     r.Command,
		IssueID:     r.IssueID,
		Payload:     json.RawMessage(r.Payload),
		Subscribers: r.Subscribers,
		Acks:        r.Acks,
		Attempts:    r.Attempts,
		LastError:   r.LastError,
		DeliveredAt: r.DeliveredAt,
	}
	if !r.retired() {
		out.Awaiting = r.awaiting()
		out.NextAttemptAfter = r.NextAttemptAfter
	}
	return out
}

func runOutboxList(cmd *cobra.Command, args []string) error {
	ctx, db, scope, err := outboxOpen()
	if err != nil {
		return HandleError("%v", err)
	}
	list, corrupt, err := selectOutboxRows(ctx, db, scope, outboxListPendingOnly, false, outboxNow(), 0)
	if err != nil {
		return HandleError("event outbox: %v", err)
	}
	pending, err := countPendingOutbox(ctx, db, scope)
	if err != nil {
		return HandleError("event outbox: %v", err)
	}
	bound := config.GetInt("change-events.outbox.max-pending")

	if jsonOutput {
		rows := make([]outboxEventJSON, 0, len(list))
		for _, r := range list {
			rows = append(rows, outboxRowToJSON(r))
		}
		out := map[string]interface{}{
			"pending": pending,
			"bound":   bound,
			"store":   changeEventStoreName(beads.FindBeadsDir()),
			"events":  rows,
			"corrupt": errStrings(corrupt),
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return HandleError("event outbox: encode: %v", err)
		}
		return nil
	}

	fmt.Printf("event outbox (%s): pending %d of bound %d, listed %d, corrupt %d\n",
		changeEventStoreName(beads.FindBeadsDir()), pending, bound, len(list), len(corrupt))
	if bound > 0 && pending >= bound {
		fmt.Printf("BACKLOG AT BOUND: writes are refused until events are delivered, 'bd outbox ack'-ed, or the bound is raised\n")
	}
	for _, c := range corrupt {
		fmt.Fprintf(os.Stderr, "%v\n", c)
	}
	if len(list) == 0 {
		return nil
	}
	fmt.Printf("%-8s %-21s %-12s %-9s %-4s %-21s %-21s\n", "seq", "created", "issue", "command", "try", "next_attempt", "status")
	for _, r := range list {
		status := "delivered " + r.DeliveredAt
		if !r.retired() {
			awaiting := r.awaiting()
			status = "awaiting " + strings.Join(awaiting, ",")
			if r.LastError != "" {
				status += " err: " + truncateOutboxErr(r.LastError)
			}
		}
		fmt.Printf("%-8d %-21s %-12s %-9s %-4d %-21s %s\n",
			r.Seq, r.CreatedAt, r.IssueID, r.Command, r.Attempts, r.NextAttemptAfter, status)
	}
	return nil
}

// ── outbox deliver ───────────────────────────────────────────────────────

var outboxDeliverAll bool

var outboxDeliverCmd = &cobra.Command{
	Use:   "deliver",
	Short: "Run one delivery pass: attempt every due pending event",
	Long: `Deliver due outbox events to their configured subscribers.

Each pending event whose next-attempt time has arrived is attempted, once per
subscriber. Success (HTTP 2xx / exit 0) records the acknowledgement; an event
whose every frozen subscriber has acknowledged retires. Failure leaves the
event pending with its next retry scheduled by the bounded backoff (1s, 2s,
4s, ... capping at 1h) and the attempt's error recorded on the row. Nothing is
ever dropped: run again (loop, cron, or the automatic pass after each write)
until the subscriber comes back.`,
	RunE: runOutboxDeliver,
}

func runOutboxDeliver(cmd *cobra.Command, args []string) error {
	ctx, db, scope, err := outboxOpen()
	if err != nil {
		return HandleError("%v", err)
	}
	cfg, err := readOutboxConfig()
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	now := outboxNow()
	if outboxDeliverAll {
		// --all ignores the backoff schedule: every pending row is due.
		now = outboxRescueNow
	}
	res := runOutboxDeliveryPass(ctx, db, scope, cfg, 0, now, "")
	if res.DeliveryError != nil {
		return HandleError("event outbox: %v", res.DeliveryError)
	}
	for _, c := range res.Corrupt {
		fmt.Fprintf(os.Stderr, "%v\n", c)
	}
	if cerr := commitOutboxBookkeeping(ctx, "deliver"); cerr != nil {
		return HandleError("event outbox: commit delivery state: %v", cerr)
	}
	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]interface{}{
			"delivered": res.Delivered,
			"failed":    res.Failed,
			"skipped":   res.Skipped,
			"corrupt":   errStrings(res.Corrupt),
		}); err != nil {
			return HandleError("event outbox: encode: %v", err)
		}
	} else {
		fmt.Printf("delivered %d, failed %d, skipped %d, corrupt %d\n",
			res.Delivered, res.Failed, res.Skipped, len(res.Corrupt))
	}
	return nil
}

// ── outbox ack ───────────────────────────────────────────────────────────

var (
	outboxAckSubscriber string
	outboxAckForce      bool
)

var outboxAckCmd = &cobra.Command{
	Use:   "ack <seq> --subscriber <name>",
	Short: "Record an acknowledgement by hand",
	Long: `Record an acknowledgement for one outbox event by hand.

This is the operator escape hatch for a subscriber that will never acknowledge
(decommissioned, or replaced by a renamed one) and for repairing a corrupt
row. The event retires when every subscriber frozen on it has acknowledged;
pass a name not on the row with --force to record it anyway.`,
	Args: cobra.ExactArgs(1),
	RunE: runOutboxAck,
}

func runOutboxAck(cmd *cobra.Command, args []string) error {
	ctx, db, scope, err := outboxOpen()
	if err != nil {
		return HandleError("%v", err)
	}
	seq, err := parseOutboxSeq(args[0])
	if err != nil {
		return HandleErrorRespectJSON("event outbox: %v", err)
	}
	if outboxAckSubscriber == "" {
		return HandleErrorRespectJSON("event outbox: ack requires --subscriber <name> (who acknowledged?): missing --subscriber")
	}
	row, err := getOutboxRow(ctx, db, scope, seq)
	if err != nil {
		// A corrupt row can still be retired by hand: --force refuses to guess
		// silently, but the operator has named the decision.
		if outboxAckForce {
			now := outboxNow()
			if err := forceRetireOutboxRow(ctx, db, scope, seq, now); err != nil {
				return HandleError("event outbox: %v", err)
			}
			if cerr := commitOutboxBookkeeping(ctx, fmt.Sprintf("ack %d (force)", seq)); cerr != nil {
				return HandleError("event outbox: commit ack: %v", cerr)
			}
			fmt.Printf("acknowledged event seq=%d with --force (row could not be read normally: %v)\n", seq, err)
			return nil
		}
		return HandleError("event outbox: %v", err)
	}
	known := len(row.Subscribers) == 0
	for _, n := range row.Subscribers {
		if n == outboxAckSubscriber {
			known = true
			break
		}
	}
	if !known && !outboxAckForce {
		return HandleErrorRespectJSON(
			"event outbox: subscriber %q is not frozen on event seq=%d (frozen: [%s]); pass --force to record it anyway",
			outboxAckSubscriber, seq, strings.Join(row.Subscribers, ", "))
	}
	if err := recordOutboxAck(ctx, db, scope, seq, outboxAckSubscriber, outboxNow()); err != nil {
		return HandleError("event outbox: %v", err)
	}
	if cerr := commitOutboxBookkeeping(ctx, fmt.Sprintf("ack %d (subscriber %s)", seq, outboxAckSubscriber)); cerr != nil {
		return HandleError("event outbox: commit ack: %v", cerr)
	}
	fmt.Printf("acknowledged event seq=%d for subscriber %s\n", seq, outboxAckSubscriber)
	return nil
}

func parseOutboxSeq(s string) (int64, error) {
	var seq int64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &seq); err != nil || seq <= 0 {
		return 0, fmt.Errorf("outbox event seq must be a positive integer, got %q", s)
	}
	return seq, nil
}

// ── outbox purge ─────────────────────────────────────────────────────────

var outboxPurgeBefore string

var outboxPurgeCmd = &cobra.Command{
	Use:   "purge --before <RFC3339>",
	Short: "Delete retired events older than a timestamp",
	Long: `Delete retired outbox events (delivered and acknowledged) whose
delivered_at is older than --before. Pending events are never touched: the
outbox only shrinks by acknowledgement, and only the operator deletes record.`,
	RunE: runOutboxPurge,
}

func runOutboxPurge(cmd *cobra.Command, args []string) error {
	if strings.TrimSpace(outboxPurgeBefore) == "" {
		return HandleErrorRespectJSON(
			"event outbox: purge requires --before <RFC3339> (refusing to guess the retention window): missing --before")
	}
	before, err := time.Parse(time.RFC3339, outboxPurgeBefore)
	if err != nil {
		return HandleErrorRespectJSON("event outbox: --before must be RFC3339 (e.g. 2026-01-01T00:00:00Z): %v", err)
	}
	ctx, db, scope, err := outboxOpen()
	if err != nil {
		return HandleError("%v", err)
	}
	n, err := deleteRetiredOutboxBefore(ctx, db, scope, before.UTC().Format(time.RFC3339))
	if err != nil {
		return HandleError("%v", err)
	}
	if cerr := commitOutboxBookkeeping(ctx, fmt.Sprintf("purge before %s (%d rows)", before.UTC().Format(time.RFC3339), n)); cerr != nil {
		return HandleError("event outbox: commit purge: %v", cerr)
	}
	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]interface{}{"purged": n})
	} else {
		fmt.Printf("purged %d retired event(s) delivered before %s\n", n, before.UTC().Format(time.RFC3339))
	}
	return nil
}

func errStrings(errs []error) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Error()
	}
	return out
}

func init() {
	outboxCmd.AddCommand(outboxListCmd, outboxDeliverCmd, outboxAckCmd, outboxPurgeCmd)
	outboxListCmd.Flags().BoolVar(&outboxListPendingOnly, "pending-only", false, "Only unretired events")
	outboxDeliverCmd.Flags().BoolVar(&outboxDeliverAll, "all", false, "Attempt every pending event, ignoring the backoff schedule")
	outboxAckCmd.Flags().StringVar(&outboxAckSubscriber, "subscriber", "", "Subscriber that acknowledged (required)")
	outboxAckCmd.Flags().BoolVar(&outboxAckForce, "force", false, "Acknowledge even when the subscriber is not frozen on the row, or the row is corrupt")
	outboxPurgeCmd.Flags().StringVar(&outboxPurgeBefore, "before", "", "Delete retired events delivered before this RFC3339 timestamp (required)")
	rootCmd.AddCommand(outboxCmd)
}
