package main

// Retry for the store connection of write commands.
//
// Background (robots-8e8dk): during a long run of sequential write calls
// (comment, label, close) the Dolt sql-server stopped answering for about 40s.
// Each call is its own bd process, so each one failed on its first connection
// attempt with "Dolt server unreachable ... i/o timeout" or "dolt circuit
// breaker is open", returned non-zero, and was re-run by hand afterwards.
//
// What is retried, and why that is safe:
//
//   - Only the open of the store connection, and only for write verbs. A
//     failed open has sent no write: the dial never connected, or the circuit
//     breaker rejected the call before any socket was used. Re-running it
//     cannot double-apply anything.
//   - Only transient causes: an i/o timeout or refused connection to the
//     server, or the circuit breaker being open. Login refusals, unknown hosts,
//     schema errors and every other open error are logical or configuration
//     failures and fail on the first attempt, as before.
//   - Nothing after the store is open is retried here. A write that already
//     reached the server and then lost the connection may have committed, so
//     replaying it could apply it twice; the store's own retry layer already
//     decides that case (it refuses to replay an ambiguous commit).
//   - The circuit breaker is left alone. Each attempt asks it again, so a wait
//     only ever lets its own cooldown (5s) elapse and its active probe run.
//
// The last attempt's error is returned unchanged, so the exit status stays
// non-zero exactly as it was.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/storage/dolt"
)

// envWriteRetrySchedule overrides the wait before each retry: a comma-separated
// list of durations ("1s,3s,9s"; a bare number is seconds). The list length is
// the number of retries after the first attempt. "off", "none" or "0" disables
// retrying.
const envWriteRetrySchedule = "BD_WRITE_RETRY_SCHEDULE"

// defaultWriteRetrySchedule is 3 retries after the first attempt (4 attempts
// in all), 13s of waiting in total: long enough to span the breaker's 5s
// cooldown plus a probe, short enough that a real outage still fails promptly.
var defaultWriteRetrySchedule = []time.Duration{1 * time.Second, 3 * time.Second, 9 * time.Second}

// writeRetryActive is set per command: true when the running command is a
// write verb whose store open may be retried.
var writeRetryActive atomic.Bool

// writeRetrySleep waits between attempts; tests replace it to run instantly.
var writeRetrySleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// writeRetryLog receives the one-line notice printed before each retry.
var writeRetryLog = func(msg string) { fmt.Fprintln(os.Stderr, msg) }

// writeVerbTop lists top-level commands that mutate the store.
var writeVerbTop = map[string]bool{
	"create": true, "q": true, "update": true, "close": true, "reopen": true,
	"comment": true, "note": true, "patch": true, "link": true, "delete": true,
	"tag": true, "priority": true, "assign": true, "defer": true, "undefer": true,
	"supersede": true, "duplicate": true, "set-state": true, "promote": true,
}

// writeVerbSub lists the mutating subcommands of command groups whose other
// subcommands (list, show, ...) only read.
var writeVerbSub = map[string]map[string]bool{
	"label":    {"add": true, "remove": true, "propagate": true},
	"dep":      {"add": true, "remove": true},
	"comments": {"add": true},
	"kv":       {"set": true, "clear": true},
}

// isWriteVerb reports whether cmd is a command whose store open may be retried.
// The root command name is skipped, so BD_NAME renames do not matter.
func isWriteVerb(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	words := strings.Fields(cmd.CommandPath())
	if len(words) < 2 {
		return false
	}
	words = words[1:]
	if writeVerbTop[words[0]] {
		return true
	}
	if subs, ok := writeVerbSub[words[0]]; ok && len(words) > 1 {
		return subs[words[1]]
	}
	return false
}

// writeRetrySchedule resolves the retry waits from the environment.
func writeRetrySchedule() []time.Duration {
	raw := strings.TrimSpace(os.Getenv(envWriteRetrySchedule))
	if raw == "" {
		return defaultWriteRetrySchedule
	}
	switch strings.ToLower(raw) {
	case "off", "none", "0":
		return nil
	}
	var out []time.Duration
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		d, err := time.ParseDuration(part)
		if err != nil {
			secs, serr := strconv.ParseFloat(part, 64)
			if serr != nil || secs < 0 {
				fmt.Fprintf(os.Stderr, "warning: ignoring %s=%q (want durations like 1s,3s,9s); using the default\n", envWriteRetrySchedule, raw)
				return defaultWriteRetrySchedule
			}
			d = time.Duration(secs * float64(time.Second))
		}
		if d < 0 {
			fmt.Fprintf(os.Stderr, "warning: ignoring %s=%q (negative wait); using the default\n", envWriteRetrySchedule, raw)
			return defaultWriteRetrySchedule
		}
		out = append(out, d)
	}
	return out
}

// isTransientOpenError reports whether a store open error is one of the
// transient server conditions worth waiting out: the circuit breaker rejecting
// the call, or the server not answering (i/o timeout, refused connection).
func isTransientOpenError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, dolt.ErrCircuitOpen) {
		return true
	}
	var ce *dolt.ServerConnectError
	if !errors.As(err, &ce) {
		return false
	}
	switch classifyConnectFailure(err) {
	case failTimeout, failRefused:
		return true
	}
	return false
}

// retryWriteOpen runs open and, for a write verb, retries it on a transient
// connect error following the configured schedule. Read commands and every
// non-transient error return after the first attempt.
func retryWriteOpen[T any](ctx context.Context, open func() (T, error)) (T, error) {
	return retryTransient(ctx, writeRetryActive.Load(), writeRetrySchedule(), isTransientOpenError, open)
}

// retryWriteOpenUnlessReadOnly is retryWriteOpen for a store opened from a
// config that may ask for read-only access, which never retries.
func retryWriteOpenUnlessReadOnly[T any](ctx context.Context, readOnly bool, open func() (T, error)) (T, error) {
	if readOnly {
		return open()
	}
	return retryWriteOpen(ctx, open)
}

func retryTransient[T any](ctx context.Context, enabled bool, schedule []time.Duration, transient func(error) bool, open func() (T, error)) (T, error) {
	val, err := open()
	if !enabled {
		return val, err
	}
	for i := 0; err != nil && transient(err) && i < len(schedule); i++ {
		if ctx.Err() != nil {
			break
		}
		writeRetryLog(fmt.Sprintf("bd: store unavailable (%s); retry %d/%d in %s",
			retryFirstLine(err.Error()), i+1, len(schedule), schedule[i]))
		if sleepErr := writeRetrySleep(ctx, schedule[i]); sleepErr != nil {
			break
		}
		val, err = open()
	}
	return val, err
}

func retryFirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
