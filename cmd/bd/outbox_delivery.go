// Package main — outbox_delivery.go
//
// Delivery half of the event outbox: subscriber definitions, per-subscriber
// transports, and the delivery pass with its bounded backoff.
//
// Acknowledgement contract (normative; mirrored in docs/brain/event-outbox.md):
//
//   - An event is delivered to every subscriber frozen on its row at emission
//     time. Subscribers configured later do not retroactively subscribe to
//     already-emitted events.
//   - An acknowledgement is, per subscriber: an HTTP 2xx response within the
//     delivery timeout (change-events.outbox.timeout), a command subscriber
//     exiting 0 within that timeout, or an operator recording the
//     acknowledgement with 'bd outbox ack'.
//   - An acknowledgement arriving after our timeout does not count for the
//     attempt that timed out: the event is retried and the subscriber receives
//     the event again. Delivery is therefore at-least-once and subscribers
//     must be idempotent. A duplicate (second) acknowledgement is idempotent:
//     the first acknowledgement timestamp wins.
//   - A subscriber that never acknowledges keeps the event visible in
//     'bd outbox list' forever; the retry delay caps at one hour; nothing is
//     ever dropped or marked delivered without an acknowledgement.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/config"
)

// outboxBackoffBase and outboxBackoffMax bound the retry schedule. After the
// n-th failed attempt, the next attempt is delayed by
// min(outboxBackoffBase << (n-1), outboxBackoffMax): 1s, 2s, 4s, 8s, ... with
// the delay capping at one hour. The schedule is bounded in delay but
// unbounded in attempts: a subscriber that never acknowledges delays its
// events forever rather than losing them.
const (
	outboxBackoffBase = time.Second
	outboxBackoffMax  = time.Hour

	// outboxDefaultTimeout is the per-attempt acknowledgement window when the
	// config key change-events.outbox.timeout is unset or nonsensical.
	outboxDefaultTimeout = 10 * time.Second
)

// outboxAttemptDelay answers the delay after the given number of failed
// attempts (attempts >= 1).
func outboxAttemptDelay(attempts int) time.Duration {
	if attempts < 1 {
		return outboxBackoffBase
	}
	d := outboxBackoffBase
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= outboxBackoffMax {
			return outboxBackoffMax
		}
	}
	return d
}

// ── Subscriber definitions ───────────────────────────────────────────────

// outboxSubscriber is one parsed subscriber definition.
type outboxSubscriber struct {
	Name    string
	URL     string // http(s) endpoint; ack = 2xx within timeout
	Command string // executable path; event JSON on stdin; ack = exit 0
}

// outboxSpecPattern rejects subscriber names with characters that would make
// row bookkeeping ambiguous (the names live in a JSON array).
var outboxSpecPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// parseOutboxSubscriber parses one subscriber definition from config.
// Definitions are strings of the form name=…;url=… or name=…;command=…,
// semicolon-separated so URLs may contain commas. Unknown fields, duplicate
// fields, missing names, or both/neither of url and command are named
// failures — a subscriber whose definition cannot be understood must never be
// guessed at.
func parseOutboxSubscriber(spec string) (*outboxSubscriber, error) {
	s := &outboxSubscriber{}
	fieldSeen := map[string]bool{}
	for _, part := range strings.Split(spec, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.Index(part, "=")
		if eq <= 0 {
			return nil, fmt.Errorf("unusable subscriber definition %q: field %q has no =value (want name=<n>;url=<u> or name=<n>;command=<path>)", spec, part)
		}
		key := strings.TrimSpace(part[:eq])
		value := strings.TrimSpace(part[eq+1:])
		if fieldSeen[key] {
			return nil, fmt.Errorf("unusable subscriber definition %q: duplicate field %q", spec, key)
		}
		fieldSeen[key] = true
		switch key {
		case "name":
			s.Name = value
		case "url":
			s.URL = value
		case "command":
			s.Command = value
		default:
			return nil, fmt.Errorf("unusable subscriber definition %q: unknown field %q (want name, url, command)", spec, key)
		}
	}
	if s.Name == "" {
		return nil, fmt.Errorf("unusable subscriber definition %q: missing name=<n>", spec)
	}
	if !outboxSpecPattern.MatchString(s.Name) {
		return nil, fmt.Errorf("unusable subscriber definition %q: name %q may only contain letters, digits, '.', '_', '-'", spec, s.Name)
	}
	if (s.URL == "") == (s.Command == "") {
		return nil, fmt.Errorf("unusable subscriber definition %q: give exactly one of url=<u> or command=<path>", spec)
	}
	return s, nil
}

// readOutboxConfig loads and validates all subscriber definitions.
func readOutboxConfig() ([]*outboxSubscriber, error) {
	specs := config.GetStringSlice("change-events.outbox.subscribers")
	out := make([]*outboxSubscriber, 0, len(specs))
	seen := map[string]bool{}
	for _, spec := range specs {
		s, err := parseOutboxSubscriber(spec)
		if err != nil {
			return nil, fmt.Errorf("event outbox: change-events.outbox.subscribers: %w", err)
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("event outbox: change-events.outbox.subscribers: duplicate subscriber name %q", s.Name)
		}
		seen[s.Name] = true
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ── Delivery pass ────────────────────────────────────────────────────────

// outboxDeliveryResult summarizes one delivery pass.
type outboxDeliveryResult struct {
	Delivered     int     // events retired (every frozen subscriber acknowledged)
	Failed        int     // attempts that failed (events stay pending with backoff)
	Skipped       int     // events left for a later pass (budget exhausted)
	Corrupt       []error // named per-row corruption refusals
	DeliveryError error   // pass-level failure (query failure, claim write failure)
}

// runOutboxDeliveryPass attempts delivery of every due pending event within a
// wall-clock budget. It never deletes or retires anything itself: only an
// acknowledgement (or an empty subscriber snapshot) retires an event.
//
// budget <= 0 means unbounded ('bd outbox deliver'). nowOverride, when
// non-empty, replaces the due-comparison clock ('deliver --all' uses a lexical
// maximum so every pending row is due). warnHeader, when non-empty, is
// prefixed to per-attempt stderr warnings so the opportunistic pass and the
// operator verb are distinguishable in output.
func runOutboxDeliveryPass(ctx context.Context, db *sql.DB, store string, cfg []*outboxSubscriber, budget time.Duration, nowOverride, warnHeader string) *outboxDeliveryResult {
	res := &outboxDeliveryResult{Corrupt: []error{}}
	byName := make(map[string]*outboxSubscriber, len(cfg))
	for _, s := range cfg {
		byName[s.Name] = s
	}

	now := outboxNow()
	due := now
	if nowOverride != "" {
		due = nowOverride
	}
	rows, corrupt, err := selectOutboxRows(ctx, db, store, true, true, due, 0)
	for _, c := range corrupt {
		res.Corrupt = append(res.Corrupt, c)
	}
	if err != nil {
		res.DeliveryError = err
		return res
	}

	timeout := config.GetDuration("change-events.outbox.timeout")
	if timeout <= 0 {
		timeout = outboxDefaultTimeout
	}
	started := time.Now()
	for _, row := range rows {
		if budget > 0 && time.Since(started) > budget {
			res.Skipped++
			continue
		}
		if row.retired() {
			// Every frozen subscriber already acknowledged but delivered_at
			// was never set (a crash between ack-record and retire). Finish
			// retiring; no redelivery happens for this row.
			if err := markOutboxDelivered(ctx, db, store, row.Seq, outboxNow()); err != nil {
				res.DeliveryError = fmt.Errorf("outbox row seq=%d: retire after crash: %w", row.Seq, err)
				return res
			}
			res.Delivered++
			continue
		}
		payload, perr := outboxDeliveryPayload(row)
		if perr != nil {
			// A payload that cannot be rebuilt is a corrupt row: named, never
			// delivered, never dropped.
			res.Corrupt = append(res.Corrupt, perr)
			continue
		}
		attemptOK := false
		var attemptErrs []string
		for _, name := range row.awaiting() {
			s, ok := byName[name]
			if !ok {
				// A subscriber frozen on the row at emission time is no longer
				// defined. Never guess: name it, treat it as a failed attempt
				// (backoff applies), let the operator resolve it.
				attemptErrs = append(attemptErrs,
					fmt.Sprintf("subscriber \"%s\" frozen on this event is not defined in change-events.outbox.subscribers; re-add it or run 'bd outbox ack %d --subscriber %s'", name, row.Seq, name))
				continue
			}
			if err := deliverToSubscriber(ctx, s, payload, timeout); err != nil {
				attemptErrs = append(attemptErrs, err.Error())
				continue
			}
			if err := recordOutboxAck(ctx, db, store, row.Seq, name, outboxNow()); err != nil {
				attemptErrs = append(attemptErrs, err.Error())
			}
			attemptOK = true
		}
		if len(attemptErrs) > 0 {
			msg := strings.Join(attemptErrs, "; ")
			delay := outboxAttemptDelay(row.Attempts + 1)
			nextAfter := time.Now().UTC().Add(delay).Format(time.RFC3339)
			if _, cerr := claimOutboxAttempt(ctx, db, store, row.Seq, row.Attempts, now, nextAfter, msg); cerr != nil {
				res.DeliveryError = cerr
				return res
			}
			res.Failed++
			fmt.Fprintf(os.Stderr, "%s event outbox: %s (seq=%d, retry %d in %s)\n", warnHeader, truncateOutboxErr(msg), row.Seq, row.Attempts+1, delay)
			continue
		}
		if attemptOK {
			res.Delivered++
		}
	}
	return res
}

// outboxDeliveryPayload is the JSON a subscriber receives: the stored event
// with the row's sequence number filled in. seq is the stable identity of the
// event — delivery is at-least-once, so subscribers dedupe on (store, seq).
func outboxDeliveryPayload(row *outboxRow) ([]byte, error) {
	var ev outboxEvent
	if err := json.Unmarshal([]byte(row.Payload), &ev); err != nil {
		return nil, fmt.Errorf("corrupt outbox row seq=%d: payload column: %w", row.Seq, err)
	}
	ev.Seq = row.Seq
	return json.Marshal(ev)
}

// deliverToSubscriber performs one delivery attempt to one subscriber.
// Returns nil on acknowledgement (2xx / exit 0).
func deliverToSubscriber(ctx context.Context, s *outboxSubscriber, payload []byte, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch {
	case s.URL != "":
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("subscriber %q: cannot build request to %s: %w", s.Name, s.URL, err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("subscriber %q: POST %s failed: %w", s.Name, s.URL, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return fmt.Errorf("subscriber %q: POST %s answered HTTP %d (acknowledgement requires 2xx within %s)", s.Name, s.URL, resp.StatusCode, timeout)
		}
		return nil
	default:
		execCmd := exec.CommandContext(ctx, s.Command)
		execCmd.Stdin = bytes.NewReader(append(append([]byte{}, payload...), '\n'))
		execCmd.Stderr = nil
		if out, err := execCmd.CombinedOutput(); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("subscriber %q: command %s timed out after %s (no acknowledgement)", s.Name, s.Command, timeout)
			}
			return fmt.Errorf("subscriber %q: command %s exited non-zero: %v (output: %s)", s.Name, s.Command, err, truncateOutboxErr(string(out)))
		}
		return nil
	}
}

func truncateOutboxErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:197] + "..."
	}
	return s
}
