package dolt

import (
	"context"
	"errors"
	"testing"
)

// tripBreaker opens the breaker the way five connection failures would.
func tripBreaker(t *testing.T, cb *circuitBreaker) {
	t.Helper()
	for i := 0; i < circuitFailureThreshold; i++ {
		cb.RecordFailure()
	}
	if cb.State() != circuitOpen {
		t.Fatalf("breaker state = %q, want open", cb.State())
	}
}

func TestWithWriteRetry_BreakerRejectionIsReplayedAfterRecovery(t *testing.T) {
	t.Setenv("BEADS_TEST_MODE", "") // need real breaker behavior
	cb := newTestCircuitBreaker(t)
	tripBreaker(t, cb)
	store := &DoltStore{breaker: cb}

	var attempts []int
	SetBreakerRejectRetry(func(_ context.Context, attempt int, err error) bool {
		attempts = append(attempts, attempt)
		if !errors.Is(err, ErrCircuitOpen) {
			t.Errorf("policy got %v, want ErrCircuitOpen", err)
		}
		if attempt == 2 {
			cb.Reset() // the server came back during the wait
		}
		return true
	})
	t.Cleanup(func() { SetBreakerRejectRetry(nil) })

	ran := 0
	err := store.withWriteRetry(context.Background(), func() error { ran++; return nil })
	if err != nil {
		t.Fatalf("err = %v, want nil after recovery", err)
	}
	if ran != 1 {
		t.Fatalf("op ran %d times, want exactly once (never replayed, never run while rejected)", ran)
	}
	if len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 2 {
		t.Fatalf("policy attempts = %v, want [1 2]", attempts)
	}
}

func TestWithWriteRetry_FinalRejectionReturnsErrCircuitOpenUnchanged(t *testing.T) {
	t.Setenv("BEADS_TEST_MODE", "")
	cb := newTestCircuitBreaker(t)
	tripBreaker(t, cb)
	store := &DoltStore{breaker: cb}

	calls := 0
	SetBreakerRejectRetry(func(_ context.Context, attempt int, _ error) bool {
		calls++
		return attempt <= 3 // give up after three retries
	})
	t.Cleanup(func() { SetBreakerRejectRetry(nil) })

	ran := 0
	err := store.withWriteRetry(context.Background(), func() error { ran++; return nil })
	if err != ErrCircuitOpen {
		t.Fatalf("err = %v, want the unchanged ErrCircuitOpen", err)
	}
	if ran != 0 {
		t.Fatalf("op ran %d times while the breaker rejected, want 0", ran)
	}
	if calls != 4 {
		t.Fatalf("policy consulted %d times, want 4 (3 retries then give up)", calls)
	}
}

func TestWithWriteRetry_NoPolicyFailsImmediately(t *testing.T) {
	t.Setenv("BEADS_TEST_MODE", "")
	SetBreakerRejectRetry(nil)
	cb := newTestCircuitBreaker(t)
	tripBreaker(t, cb)
	store := &DoltStore{breaker: cb}

	if err := store.withWriteRetry(context.Background(), func() error { return nil }); err != ErrCircuitOpen {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
}

func TestWithWriteRetry_FailureAfterOpStartedIsNeverReplayed(t *testing.T) {
	t.Setenv("BEADS_TEST_MODE", "")
	cb := newTestCircuitBreaker(t)
	store := &DoltStore{breaker: cb}

	consulted := 0
	SetBreakerRejectRetry(func(context.Context, int, error) bool { consulted++; return true })
	t.Cleanup(func() { SetBreakerRejectRetry(nil) })

	// Connection failures trip the breaker mid-statement: the statement may
	// have reached the server, so it must not be offered for a replay.
	ran := 0
	err := store.withWriteRetry(context.Background(), func() error {
		ran++
		return errors.New("write: broken pipe")
	})
	if err == nil {
		t.Fatal("failure was swallowed")
	}
	if consulted != 0 {
		t.Fatalf("policy consulted %d times for a failure after op started, want 0", consulted)
	}
	if errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("post-start failure must not look like a pre-send rejection: %v", err)
	}

	// A plain non-transient error is also returned as is, once.
	ran = 0
	cb.Reset()
	boom := errors.New("syntax error in SQL")
	if err := store.withWriteRetry(context.Background(), func() error { ran++; return boom }); err != boom || ran != 1 {
		t.Fatalf("err=%v ran=%d, want the same error after one run", err, ran)
	}
}

func TestWithRetry_ReadRejectionNeverConsultsWritePolicy(t *testing.T) {
	t.Setenv("BEADS_TEST_MODE", "")
	cb := newTestCircuitBreaker(t)
	tripBreaker(t, cb)
	store := &DoltStore{breaker: cb}

	consulted := 0
	SetBreakerRejectRetry(func(context.Context, int, error) bool { consulted++; return true })
	t.Cleanup(func() { SetBreakerRejectRetry(nil) })

	if err := store.withRetry(context.Background(), func() error { return nil }); err != ErrCircuitOpen {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
	if consulted != 0 {
		t.Fatalf("read path consulted the write policy %d times", consulted)
	}
}
