package verb_test

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/beads/internal/brain/verb"
)

// stubArgs / stubResult are minimal concrete types used to prove the
// BrainVerb generic interface compiles and round-trips data faithfully.
type stubArgs struct {
	In string
}

type stubResult struct {
	Out string
}

type stubVerb struct {
	name string
	err  error
}

func (v stubVerb) Name() string { return v.name }

func (v stubVerb) Run(_ context.Context, a stubArgs) (stubResult, error) {
	if v.err != nil {
		return stubResult{}, v.err
	}
	return stubResult{Out: a.In + "!"}, nil
}

// Compile-time proof that stubVerb satisfies BrainVerb with concrete types.
var _ verb.BrainVerb[stubArgs, stubResult] = stubVerb{}

func TestBrainVerb_Run_PassesArgsAndReturnsResult(t *testing.T) {
	t.Parallel()

	v := stubVerb{name: "stub"}
	if got := v.Name(); got != "stub" {
		t.Fatalf("Name() = %q, want %q", got, "stub")
	}

	got, err := v.Run(context.Background(), stubArgs{In: "hi"})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.Out != "hi!" {
		t.Fatalf("Run() result = %q, want %q", got.Out, "hi!")
	}
}

func TestBrainVerb_Run_PropagatesError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("boom")
	v := stubVerb{name: "stub", err: sentinel}

	_, err := v.Run(context.Background(), stubArgs{In: "ignored"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run() error = %v, want %v", err, sentinel)
	}
}

// TestBrainVerb_DifferentConcreteTypes proves the generic interface can be
// satisfied by impls with entirely different Args/Result shapes — a
// regression guard against accidentally narrowing the interface.
func TestBrainVerb_DifferentConcreteTypes(t *testing.T) {
	t.Parallel()

	type intArgs struct{ N int }
	type intResult struct{ Doubled int }

	type doublerVerb struct{}
	doublerRun := func(_ context.Context, a intArgs) (intResult, error) {
		return intResult{Doubled: a.N * 2}, nil
	}

	var d verb.BrainVerb[intArgs, intResult] = adapter[intArgs, intResult]{
		name: "doubler",
		run:  doublerRun,
	}

	if d.Name() != "doubler" {
		t.Fatalf("Name() = %q, want %q", d.Name(), "doubler")
	}
	got, err := d.Run(context.Background(), intArgs{N: 21})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got.Doubled != 42 {
		t.Fatalf("Run() Doubled = %d, want 42", got.Doubled)
	}

	_ = doublerVerb{} // keep the symbol referenced for clarity
}

// adapter is a tiny generic helper so the second-type test does not need
// its own named type. It is test-only.
type adapter[A any, R any] struct {
	name string
	run  func(context.Context, A) (R, error)
}

func (a adapter[A, R]) Name() string                            { return a.name }
func (a adapter[A, R]) Run(ctx context.Context, args A) (R, error) { return a.run(ctx, args) }
