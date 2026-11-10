package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/storage/dolt"
)

func timeoutConnectErr() error {
	return &dolt.ServerConnectError{
		Host: "100.84.99.99", Port: 3307,
		Err:  fmt.Errorf("Dolt server unreachable at 100.84.99.99:3307: dial tcp 100.84.99.99:3307: i/o timeout"),
		Hint: "The Dolt server may not be running. Try:\n  bd dolt start",
	}
}

// stubRetryEnv makes retries instant and captures the stderr notices.
func stubRetryEnv(t *testing.T) (waits *[]time.Duration, notices *[]string) {
	t.Helper()
	oldSleep, oldLog := writeRetrySleep, writeRetryLog
	var w []time.Duration
	var n []string
	writeRetrySleep = func(_ context.Context, d time.Duration) error { w = append(w, d); return nil }
	writeRetryLog = func(m string) { n = append(n, m) }
	t.Cleanup(func() { writeRetrySleep, writeRetryLog = oldSleep, oldLog })
	return &w, &n
}

func TestIsTransientOpenError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"circuit open", dolt.ErrCircuitOpen, true},
		{"circuit open wrapped", fmt.Errorf("open: %w", dolt.ErrCircuitOpen), true},
		{"i/o timeout", timeoutConnectErr(), true},
		{"annotated i/o timeout", annotateStoreConnectError(timeoutConnectErr(), ""), true},
		{"refused", &dolt.ServerConnectError{Host: "h", Port: 1, Err: fmt.Errorf("dial tcp h:1: %w", syscall.ECONNREFUSED)}, true},
		{"login refused", &dolt.ServerConnectError{Host: "h", Port: 1, Err: &mysql.MySQLError{Number: 1045, Message: "Access denied for user"}}, false},
		{"unknown host", &dolt.ServerConnectError{Host: "h", Port: 1, Err: fmt.Errorf("dial tcp: lookup h: no such host")}, false},
		{"plain i/o timeout text outside a connect error", errors.New("query failed: i/o timeout"), false},
		{"validation", errors.New("issue not found"), false},
	}
	for _, c := range cases {
		if got := isTransientOpenError(c.err); got != c.want {
			t.Errorf("%s: isTransientOpenError = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRetryTransientRecoversAfterTransientFailures(t *testing.T) {
	waits, notices := stubRetryEnv(t)
	calls := 0
	got, err := retryTransient(context.Background(), true, defaultWriteRetrySchedule, isTransientOpenError, func() (string, error) {
		calls++
		switch calls {
		case 1:
			return "", timeoutConnectErr()
		case 2:
			return "", dolt.ErrCircuitOpen
		}
		return "store", nil
	})
	if err != nil || got != "store" {
		t.Fatalf("got (%q, %v), want (store, nil)", got, err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	if want := []time.Duration{time.Second, 3 * time.Second}; fmt.Sprint(*waits) != fmt.Sprint(want) {
		t.Fatalf("waits = %v, want %v", *waits, want)
	}
	if len(*notices) != 2 || !strings.Contains((*notices)[0], "retry 1/3 in 1s") || !strings.Contains((*notices)[1], "retry 2/3 in 3s") {
		t.Fatalf("notices = %q", *notices)
	}
}

func TestRetryTransientFinalFailureKeepsOriginalError(t *testing.T) {
	waits, notices := stubRetryEnv(t)
	calls := 0
	orig := timeoutConnectErr()
	_, err := retryTransient(context.Background(), true, defaultWriteRetrySchedule, isTransientOpenError, func() (int, error) {
		calls++
		return 0, orig
	})
	if err == nil {
		t.Fatal("final failure was swallowed: err is nil")
	}
	if err != orig {
		t.Fatalf("err = %v, want the unchanged original error", err)
	}
	if calls != 4 { // first attempt + 3 retries
		t.Fatalf("calls = %d, want 4", calls)
	}
	if want := []time.Duration{time.Second, 3 * time.Second, 9 * time.Second}; fmt.Sprint(*waits) != fmt.Sprint(want) {
		t.Fatalf("waits = %v, want %v", *waits, want)
	}
	if len(*notices) != 3 {
		t.Fatalf("want one stderr notice per retry, got %q", *notices)
	}
	// A returned error is what makes main exit non-zero.
	if code, ok := exitCodeFromError(err); ok && code == 0 {
		t.Fatalf("final failure maps to exit code 0")
	}
}

func TestRetryTransientNeverRetriesNonTransientOrReads(t *testing.T) {
	stubRetryEnv(t)
	for name, tc := range map[string]struct {
		enabled bool
		err     error
	}{
		"login refused on a write": {true, &dolt.ServerConnectError{Host: "h", Port: 1, Err: &mysql.MySQLError{Number: 1045, Message: "Access denied for user"}}},
		"validation on a write":    {true, errors.New("invalid issue")},
		"i/o timeout on a read":    {false, timeoutConnectErr()},
		"breaker open on a read":   {false, dolt.ErrCircuitOpen},
	} {
		calls := 0
		_, err := retryTransient(context.Background(), tc.enabled, defaultWriteRetrySchedule, isTransientOpenError, func() (int, error) {
			calls++
			return 0, tc.err
		})
		if calls != 1 || err != tc.err {
			t.Errorf("%s: calls=%d err=%v, want exactly one attempt and the same error", name, calls, err)
		}
	}
}

func TestRetryTransientStopsWhenContextCancelled(t *testing.T) {
	stubRetryEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	writeRetrySleep = func(context.Context, time.Duration) error { cancel(); return ctx.Err() }
	_, err := retryTransient(ctx, true, defaultWriteRetrySchedule, isTransientOpenError, func() (int, error) {
		calls++
		return 0, dolt.ErrCircuitOpen
	})
	if calls != 1 || !errors.Is(err, dolt.ErrCircuitOpen) {
		t.Fatalf("calls=%d err=%v, want one attempt and the open error", calls, err)
	}
}

func TestWriteRetryScheduleEnv(t *testing.T) {
	cases := []struct {
		env  string
		want []time.Duration
	}{
		{"", defaultWriteRetrySchedule},
		{"off", nil},
		{"0", nil},
		{"10ms,20ms", []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}},
		{"1,2.5", []time.Duration{time.Second, 2500 * time.Millisecond}},
		{"banana", defaultWriteRetrySchedule},
		{"1s,-2s", defaultWriteRetrySchedule},
	}
	for _, c := range cases {
		t.Setenv(envWriteRetrySchedule, c.env)
		if got := writeRetrySchedule(); fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%q: schedule = %v, want %v", c.env, got, c.want)
		}
	}
}

func TestIsWriteVerb(t *testing.T) {
	root := &cobra.Command{Use: "bd"}
	add := func(parent *cobra.Command, use string) *cobra.Command {
		c := &cobra.Command{Use: use}
		parent.AddCommand(c)
		return c
	}
	comment := add(root, "comment")
	closeCmd := add(root, "close")
	list := add(root, "list")
	show := add(root, "show")
	label := add(root, "label")
	labelAdd, labelList := add(label, "add"), add(label, "list")
	comments := add(root, "comments")
	commentsAdd := add(comments, "add")

	cases := map[*cobra.Command]bool{
		root: false, comment: true, closeCmd: true, list: false, show: false,
		label: false, labelAdd: true, labelList: false, comments: false, commentsAdd: true,
	}
	for _, use := range []string{"edit", "rename", "rename-prefix", "batch", "import", "reopen", "defer", "undefer", "set-state"} {
		cases[add(root, use)] = true
	}
	for _, use := range []string{"ready", "state", "search", "count"} {
		cases[add(root, use)] = false
	}
	for group, subs := range map[string]map[string]bool{
		"epic":  {"close-eligible": true, "status": false},
		"gate":  {"create": true, "resolve": true, "check": true, "list": false, "show": false},
		"mol":   {"bond": true, "burn": true, "squash": true, "pour": true, "show": false, "stale": false},
		"swarm": {"create": true, "status": false, "validate": false},
		"todo":  {"add": true, "done": true, "list": false},
		"group": {"create": true, "add": true, "remove": true, "sync": true, "show": false},
	} {
		g := add(root, group)
		cases[g] = false
		for use, want := range subs {
			cases[add(g, use)] = want
		}
	}

	var mol *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "mol" {
			mol = c
		}
	}
	wisp := add(mol, "wisp")
	cases[wisp] = false
	cases[add(wisp, "create")] = true
	cases[add(wisp, "gc")] = true
	cases[add(wisp, "list")] = false

	restoreRead := add(root, "restore")
	restoreRead.Flags().Bool("apply", false, "")
	cases[restoreRead] = false
	restoreRoot := &cobra.Command{Use: "bd"}
	restoreApply := add(restoreRoot, "restore")
	restoreApply.Flags().Bool("apply", false, "")
	if err := restoreApply.Flags().Set("apply", "true"); err != nil {
		t.Fatal(err)
	}
	cases[restoreApply] = true

	for cmd, want := range cases {
		if got := isWriteVerb(cmd); got != want {
			t.Errorf("isWriteVerb(%q) = %v, want %v", cmd.CommandPath(), got, want)
		}
	}
	if isWriteVerb(nil) {
		t.Error("nil command must not be a write verb")
	}
}

func TestRetryWriteOpenFollowsActiveFlag(t *testing.T) {
	stubRetryEnv(t)
	t.Setenv(envWriteRetrySchedule, "1ms,1ms")
	defer writeRetryActive.Store(false)

	for _, active := range []bool{true, false} {
		writeRetryActive.Store(active)
		calls := 0
		_, err := retryWriteOpen(context.Background(), func() (int, error) { calls++; return 0, dolt.ErrCircuitOpen })
		want := 1
		if active {
			want = 3
		}
		if calls != want || err == nil {
			t.Errorf("active=%v: calls=%d err=%v, want %d calls and a final error", active, calls, err, want)
		}
	}

	writeRetryActive.Store(true)
	calls := 0
	_, err := retryWriteOpenUnlessReadOnly(context.Background(), true, func() (int, error) { calls++; return 0, dolt.ErrCircuitOpen })
	if calls != 1 || err == nil {
		t.Errorf("read-only config retried: calls=%d err=%v", calls, err)
	}
}

func TestBreakerRejectRetryPolicy(t *testing.T) {
	waits, notices := stubRetryEnv(t)
	defer writeRetryActive.Store(false)

	// Read verb: never waits.
	writeRetryActive.Store(false)
	if breakerRejectRetryPolicy(context.Background(), 1, dolt.ErrCircuitOpen) {
		t.Fatal("a read command retried a breaker rejection")
	}

	// Write verb: waits 1s, 3s, 9s for attempts 1..3, then gives up.
	writeRetryActive.Store(true)
	t.Setenv(envWriteRetrySchedule, "")
	for attempt := 1; attempt <= 3; attempt++ {
		if !breakerRejectRetryPolicy(context.Background(), attempt, dolt.ErrCircuitOpen) {
			t.Fatalf("attempt %d: want retry", attempt)
		}
	}
	if breakerRejectRetryPolicy(context.Background(), 4, dolt.ErrCircuitOpen) {
		t.Fatal("attempt 4: retried past the schedule")
	}
	if want := []time.Duration{time.Second, 3 * time.Second, 9 * time.Second}; fmt.Sprint(*waits) != fmt.Sprint(want) {
		t.Fatalf("waits = %v, want %v", *waits, want)
	}
	if len(*notices) != 3 || !strings.Contains((*notices)[2], "retry 3/3 in 9s") {
		t.Fatalf("notices = %q", *notices)
	}

	// The override applies here too, and "off" disables it.
	t.Setenv(envWriteRetrySchedule, "off")
	if breakerRejectRetryPolicy(context.Background(), 1, dolt.ErrCircuitOpen) {
		t.Fatal("schedule off still retried")
	}

	// A cancelled context stops the wait.
	t.Setenv(envWriteRetrySchedule, "1s")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if breakerRejectRetryPolicy(ctx, 1, dolt.ErrCircuitOpen) {
		t.Fatal("retried on a cancelled context")
	}
}
