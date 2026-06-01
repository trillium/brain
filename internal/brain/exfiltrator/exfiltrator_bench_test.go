package exfiltrator_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/brain/exfiltrator"
	"github.com/steveyegge/beads/internal/types"
)

// BenchmarkRender exercises a single Render with a ~1KB body to give a
// representative number for ISC-120 (the 500ms budget). On M-series
// Macs this is wall-clock-noisy but should stay deep under budget —
// see divergence/0012 § Decisions.
//
// We assert the per-op latency via b.Elapsed()/b.N at the end of the
// benchmark to keep the budget check honest even when the benchmark is
// run via `go test -run=BenchmarkRender -bench=.`.
func BenchmarkRender(b *testing.B) {
	b.ReportAllocs()

	root := b.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)

	body := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 24) // ~1KB
	now := time.Now().UTC()

	issue := &types.Issue{
		ID:          "B-bench01",
		Title:       "benchmark fixture",
		Description: body,
		Status:      types.StatusOpen,
		Priority:    2,
		IssueType:   types.TypeTask,
		CreatedAt:   now,
		UpdatedAt:   now,
		Labels:      []string{"alpha", "beta", "gamma"},
	}

	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := exf.Render(ctx, issue); err != nil {
			b.Fatalf("Render: %v", err)
		}
	}
	b.StopTimer()

	// Per-op budget: 500ms (ISC-120). One actual render runs in well
	// under a millisecond; we leave huge headroom for noisy CI hosts.
	if b.N > 0 {
		perOp := b.Elapsed() / time.Duration(b.N)
		if perOp > 500*time.Millisecond {
			b.Fatalf("BenchmarkRender per-op = %v, exceeds 500ms budget (ISC-120)", perOp)
		}
	}
}

// TestRenderUnder500ms is a guard rail for non-bench runs: even a
// regular `go test` confirms we are well under the ISC-120 budget.
func TestRenderUnder500ms(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)

	body := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 24)
	issue := &types.Issue{
		ID:          "B-budget01",
		Title:       "budget fixture",
		Description: body,
		Status:      types.StatusOpen,
		Priority:    2,
		IssueType:   types.TypeKnowledge,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	start := time.Now()
	if err := exf.Render(context.Background(), issue); err != nil {
		t.Fatalf("Render: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Render took %v, exceeds 500ms budget (ISC-120)", elapsed)
	}
}
