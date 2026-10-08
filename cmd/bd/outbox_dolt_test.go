//go:build cgo

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// ── A real dolt sql-server on a scratch directory (no docker needed) ────

// newOutboxTestDB starts a dolt sql-server on a scratch directory and returns a
// connection to a database with the outbox table created. Skips when the dolt
// binary is unavailable. The server is stopped when the test ends.
func newOutboxTestDB(t *testing.T) *sql.DB {
	t.Helper()
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt binary not on PATH")
	}
	listen, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot pick a free port: %v", err)
	}
	port := listen.Addr().(*net.TCPAddr).Port
	listen.Close()

	root := t.TempDir()
	dir := filepath.Join(root, "data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "DOLT_ROOT_PATH="+root)
	initCmd := exec.Command("dolt", "init", "--name", "outbox-test", "--email", "outbox@test.invalid")
	initCmd.Dir, initCmd.Env = dir, env
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Skipf("dolt init failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	srv := exec.Command("dolt", "sql-server", "--host", "127.0.0.1", fmt.Sprintf("--port=%d", port), "--loglevel=error")
	srv.Dir, srv.Env = dir, env
	if err := srv.Start(); err != nil {
		t.Skipf("cannot start dolt sql-server: %v", err)
	}
	t.Cleanup(func() {
		_ = srv.Process.Signal(syscall.SIGTERM)
		_ = srv.Wait()
	})

	dsn := fmt.Sprintf("root@tcp(127.0.0.1:%d)/", port)
	var admin *sql.DB
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		c, err := sql.Open("mysql", dsn)
		if err == nil {
			var one int
			if c.QueryRow("SELECT 1").Scan(&one) == nil && one == 1 {
				admin = c
				break
			}
			c.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	if admin == nil {
		t.Skip("dolt sql-server not ready in 30s")
	}
	defer admin.Close()
	if _, err := admin.Exec("CREATE DATABASE outbox_scratch"); err != nil {
		t.Skipf("cannot create scratch database: %v", err)
	}
	db, err := sql.Open("mysql", dsn+"outbox_scratch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := ensureOutboxTable(context.Background(), db); err != nil {
		t.Fatalf("ensureOutboxTable: %v", err)
	}
	return db
}

// insertTestEvent inserts one pending row directly (emission is exercised
// end-to-end by the behavioural proof on a real store with bd create).
func insertTestEvent(t *testing.T, db *sql.DB, store, id, subsJSON string) int64 {
	t.Helper()
	ctx := context.Background()
	ev := outboxEvent{TS: outboxNow(), Store: store, Command: "create", ID: id}
	payload, _ := json.Marshal(ev)
	seq, err := insertOutboxEvent(ctx, db, store, ev, string(payload), subsJSON, "{}", outboxNow())
	if err != nil {
		t.Fatalf("insertOutboxEvent: %v", err)
	}
	return seq
}

// zeroNextAttempt makes every row immediately due (simulating elapsed backoff).
func zeroNextAttempt(db *sql.DB) {
	ctx := context.Background()
	_, _ = db.ExecContext(ctx, "UPDATE event_outbox SET next_attempt_after = ''")
}

// ── Delivery: acknowledged retires; down delays; retry delivers ─────────

func TestOutboxDeliveryAckRetires(t *testing.T) {
	db := newOutboxTestDB(t)
	ctx := context.Background()

	var gotPayload string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotPayload = string(body)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	subs, err := readOutboxConfigFromSpecs([]string{fmt.Sprintf("name=pulse;url=%s", srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	seq := insertTestEvent(t, db, "task", "task-abc", `["pulse"]`)

	res := runOutboxDeliveryPass(ctx, db, "task", subs, 0, "", "")
	if res.DeliveryError != nil {
		t.Fatalf("delivery pass error: %v", res.DeliveryError)
	}
	if res.Delivered != 1 || res.Failed != 0 {
		t.Fatalf("res = %+v, want Delivered 1, Failed 0", res)
	}
	row, err := getOutboxRow(ctx, db, "task", seq)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(gotPayload), &payload); err != nil {
		t.Fatalf("subscriber got unparseable payload %q: %v", gotPayload, err)
	}
	if payload["id"] != "task-abc" || payload["seq"] != float64(seq) {
		t.Fatalf("payload = %s, want id task-abc seq %d", gotPayload, seq)
	}
	if !row.retired() {
		t.Fatalf("acknowledged event must retire: %+v", row)
	}
	if row.Acks["pulse"] == "" {
		t.Fatalf("ack timestamp missing: %+v", row)
	}
}

// A subscriber that is down delays the event: it stays pending, visible, and
// is retried on the bounded schedule once the subscriber returns.
func TestOutboxDeliverySubscriberDownDelaysAndRetryDelivers(t *testing.T) {
	db := newOutboxTestDB(t)
	ctx := context.Background()

	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			http.Error(w, "subscriber down", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	subs, err := readOutboxConfigFromSpecs([]string{fmt.Sprintf("name=pulse;url=%s", srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	subsJSON, _ := json.Marshal([]string{"pulse"})
	seq := insertTestEvent(t, db, "task", "task-1", string(subsJSON))

	// Subscriber down: the pass records the failure but must not retire, drop,
	// or otherwise disappear the event.
	res := runOutboxDeliveryPass(ctx, db, "task", subs, 0, "", "")
	if res.DeliveryError != nil {
		t.Fatalf("delivery pass error: %v", res.DeliveryError)
	}
	if res.Failed != 1 || res.Delivered != 0 {
		t.Fatalf("after down attempt: %+v", res)
	}
	row, err := getOutboxRow(ctx, db, "task", seq)
	if err != nil {
		t.Fatal(err)
	}
	if row.retired() {
		t.Fatal("a down subscriber must never retire an event")
	}
	if row.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", row.Attempts)
	}
	if !strings.Contains(row.LastError, "HTTP 503") {
		t.Fatalf("last_error = %q, want the named HTTP 503 refusal", row.LastError)
	}
	next, err := time.Parse(time.RFC3339, row.NextAttemptAfter)
	if err != nil {
		t.Fatalf("next_attempt_after %q is not RFC3339: %v", row.NextAttemptAfter, err)
	}
	if d := time.Until(next); d <= 0 || d > 3*time.Second {
		t.Fatalf("next retry in %s, want ~1s (first step of the bounded backoff)", d)
	}

	// Before the backoff elapses the event is not due: the pass leaves it alone
	// (no attempt is made, the attempt counter does not move).
	res = runOutboxDeliveryPass(ctx, db, "task", subs, 0, "", "")
	if res.Delivered != 0 || res.Failed != 0 {
		t.Fatalf("pre-due pass must not attempt, got %+v", res)
	}
	if row, _ = getOutboxRow(ctx, db, "task", seq); row.Attempts != 1 {
		t.Fatalf("attempts = %d after a not-due pass, want 1", row.Attempts)
	}

	// The subscriber returns (simulating the elapsed interval with --all-style
	// force-due): the retry delivers and the event retires.
	healthy.Store(true)
	zeroNextAttempt(db)
	res = runOutboxDeliveryPass(ctx, db, "task", subs, 0, "", "")
	if res.Delivered != 1 || res.Failed != 0 {
		t.Fatalf("after subscriber returns: %+v", res)
	}
	row, err = getOutboxRow(ctx, db, "task", seq)
	if err != nil {
		t.Fatal(err)
	}
	if !row.retired() {
		t.Fatalf("retry after recovery must retire the event: %+v", row)
	}
}

// An unacknowledged event stays visible and counts toward the bound; reaching
// the bound is a named refusal that tells the operator what to do.
func TestOutboxBoundRefusesWhenReached(t *testing.T) {
	db := newOutboxTestDB(t)
	ctx := context.Background()
	seq := insertTestEvent(t, db, "task", "task-never", `["never"]`)

	pending, err := countPendingOutbox(ctx, db, "task")
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending = %d, want 1 (the unacknowledged event stays visible)", pending)
	}
	if err := outboxBoundError(pending, 1, 2); err != nil {
		t.Fatalf("below the bound must pass: %v", err)
	}
	err = outboxBoundError(pending, 2, 2)
	if err == nil {
		t.Fatal("exceeding the bound must be refused")
	}
	for _, want := range []string{"event outbox full", "max-pending=2", "bd outbox deliver", "refusing the write"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q must mention %q", err, want)
		}
	}
	if err := outboxBoundError(1000000, 1, 0); err != nil {
		t.Fatalf("bound <= 0 disables the bound: %v", err)
	}

	// Acknowledging frees capacity: the retired event stops counting.
	if err := recordOutboxAck(ctx, db, "task", seq, "never", outboxNow()); err != nil {
		t.Fatal(err)
	}
	if pending, _ := countPendingOutbox(ctx, db, "task"); pending != 0 {
		t.Fatalf("pending after ack = %d, want 0", pending)
	}
}

// Acknowledgement recorded by hand retires; acknowledging twice is idempotent.
func TestOutboxManualAckAndIdempotence(t *testing.T) {
	db := newOutboxTestDB(t)
	ctx := context.Background()
	subsJSON, _ := json.Marshal([]string{"pulse"})
	seq := insertTestEvent(t, db, "task", "task-2", string(subsJSON))

	fixed := "2026-01-01T00:00:00Z"
	if err := recordOutboxAck(ctx, db, "task", seq, "pulse", fixed); err != nil {
		t.Fatal(err)
	}
	if err := recordOutboxAck(ctx, db, "task", seq, "pulse", "2026-02-02T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	row, err := getOutboxRow(ctx, db, "task", seq)
	if err != nil {
		t.Fatal(err)
	}
	if row.Acks["pulse"] != fixed {
		t.Fatalf("double ack overwrote the first timestamp: %+v", row)
	}
	if !row.retired() {
		t.Fatal("all-acknowledged row must be retired")
	}

	// A late extra ack must not un-retire the row (the named refusal for an
	// unfrozen subscriber lives in the verb; this layer is mechanical).
	if err := recordOutboxAck(ctx, db, "task", seq, "ghost", outboxNow()); err != nil {
		t.Fatal(err)
	}
	row, _ = getOutboxRow(ctx, db, "task", seq)
	if row.DeliveredAt == "" {
		t.Fatal("retired row must keep delivered_at")
	}
}

// Concurrent deliverers claim attempts distinctly: one CAS wins.
func TestOutboxClaimIsCompareAndSet(t *testing.T) {
	db := newOutboxTestDB(t)
	ctx := context.Background()
	subsJSON, _ := json.Marshal([]string{"pulse"})
	seq := insertTestEvent(t, db, "task", "task-3", string(subsJSON))

	now := outboxNow()
	first, err := claimOutboxAttempt(ctx, db, "task", seq, 0, now, now, "claim 1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := claimOutboxAttempt(ctx, db, "task", seq, 0, now, now, "claim 2")
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("first claim must win")
	}
	if second {
		t.Fatal("second claim on the same attempt count must lose")
	}
	row, _ := getOutboxRow(ctx, db, "task", seq)
	if row.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", row.Attempts)
	}
	if row.LastError != "claim 1" {
		t.Fatalf("last_error = %q, want the winning claim's error", row.LastError)
	}
}

// A corrupt row is surfaced by name and never blocks healthy rows from
// flowing through the same pass.
func TestOutboxCorruptRowNamedWhileHealthyFlow(t *testing.T) {
	db := newOutboxTestDB(t)
	ctx := context.Background()

	subsJSON, _ := json.Marshal([]string{"pulse"})
	_ = insertTestEvent(t, db, "task", "task-healthy", string(subsJSON))
	// Forge a corrupt row the way a botched external write would.
	if _, err := db.ExecContext(ctx, `INSERT INTO event_outbox
		(created_at, store, command, issue_id, payload, subscribers, acks, attempts, next_attempt_after, last_error, delivered_at)
		VALUES (?, ?, ?, ?, ?, 'not-json', '{}', 0, ?, '', '')`,
		outboxNow(), "task", "create", "task-bad", "{}", outboxNow()); err != nil {
		t.Fatal(err)
	}

	rows, corrupt, err := selectOutboxRows(ctx, db, "task", true, true, outboxNow(), 0)
	if err != nil {
		t.Fatalf("healthy rows must still flow: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want the healthy row only, got %d", len(rows))
	}
	if len(corrupt) != 1 {
		t.Fatalf("want exactly one named corruption, got %d", len(corrupt))
	}
	if !strings.Contains(corrupt[0].Error(), "corrupt outbox row") || !strings.Contains(corrupt[0].Error(), "subscribers column") {
		t.Fatalf("named corruption expected, got: %v", corrupt[0])
	}
	if !strings.Contains(corrupt[0].Error(), "seq=") {
		t.Fatalf("corruption must name the row's seq, got: %v", corrupt[0])
	}

	// --force repairing the corrupt row retires it.
	var direct int64
	if err := db.QueryRowContext(ctx, `SELECT seq FROM event_outbox WHERE issue_id = 'task-bad'`).Scan(&direct); err != nil {
		t.Fatal(err)
	}
	if err := forceRetireOutboxRow(ctx, db, "task", direct, outboxNow()); err != nil {
		t.Fatalf("force retire: %v", err)
	}
	again, againCorrupt, cerr := selectOutboxRows(ctx, db, "task", true, true, outboxNow(), 0)
	if cerr != nil || len(againCorrupt) != 0 {
		t.Fatalf("after force retire: err %v, corrupt %v", cerr, againCorrupt)
	}
	if len(again) != 1 || again[0].IssueID != "task-healthy" {
		t.Fatalf("after force retire, healthy rows remain: %+v %d", again, len(again))
	}
	if pending, _ := countPendingOutbox(ctx, db, "task"); pending != 1 {
		t.Fatalf("pending = %d, want 1 (only the healthy row)", pending)
	}
}

// Deferred rows (retire-after-crash) finish retiring without redelivery.
func TestOutboxRetireAfterCrash(t *testing.T) {
	db := newOutboxTestDB(t)
	ctx := context.Background()
	// Row with all acks recorded, delivered_at empty (process died between
	// ack-record and retire).
	if _, err := db.ExecContext(ctx, `INSERT INTO event_outbox
		(created_at, store, command, issue_id, payload, subscribers, acks, attempts, next_attempt_after, last_error, delivered_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, '', '')`,
		outboxNow(), "task", "create", "task-crash", "{}", `["pulse"]`, `{"pulse":"2026-01-01T00:00:00Z"}`, outboxNow()); err != nil {
		t.Fatal(err)
	}
	subs, err := readOutboxConfigFromSpecs([]string{"name=pulse;command=/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	res := runOutboxDeliveryPass(ctx, db, "task", subs, 0, "", "")
	if res.DeliveryError != nil {
		t.Fatalf("delivery error: %v", res.DeliveryError)
	}
	if res.Delivered != 1 {
		t.Fatalf("crash-recovery retire expected, got %+v", res)
	}
	if pending, _ := countPendingOutbox(ctx, db, "task"); pending != 0 {
		t.Fatalf("pending = %d, want 0", pending)
	}
}

// readOutboxConfigFromSpecs parses specs the way readOutboxConfig does without
// touching process-wide viper state (the unit tests above already cover the
// config-driven path).
func readOutboxConfigFromSpecs(specs []string) ([]*outboxSubscriber, error) {
	out := make([]*outboxSubscriber, 0, len(specs))
	seen := map[string]bool{}
	for _, spec := range specs {
		s, err := parseOutboxSubscriber(spec)
		if err != nil {
			return nil, err
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("duplicate subscriber name %q", s.Name)
		}
		seen[s.Name] = true
		out = append(out, s)
	}
	return out, nil
}
