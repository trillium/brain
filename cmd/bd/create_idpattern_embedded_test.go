//go:build cgo

package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/idpattern"
)

// TestEmbeddedCreateIDPattern covers a store with create.id-pattern set: one
// bead per period, a date-derived id, and a refusal naming the existing bead.
func TestEmbeddedCreateIDPattern(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt create tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	loc, err := time.LoadLocation(idpattern.Location)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(loc)

	kinds := []struct {
		kind       string
		explicit   string // a date to pass with --date
		wantExpl   string // id that date maps to
		nonMatchID string
	}{
		{"day", "2026-03-04", "day-2026-03-04", "day-2026-03"},
		{"week", "2026-03-04", "week-2026-03-01", "week-2026-03"}, // Wed -> Sunday
		{"month", "2026-03-04", "month-2026-03", "month-2026-03-04"},
		{"quarter", "2026-08-04", "quarter-2026-q3", "quarter-2026-Q3"},
		{"year", "2026-03-04", "year-2026", "year-2026-03"},
	}
	for _, k := range kinds {
		t.Run(k.kind, func(t *testing.T) {
			dir, _, _ := bdInit(t, bd, "--prefix", k.kind)
			bdConfig(t, bd, dir, "set", idpattern.ConfigKey, k.kind)

			r, err := idpattern.New(idpattern.Kind(k.kind), k.kind, now)
			if err != nil {
				t.Fatal(err)
			}
			today := r.Today()

			if got := bdCreateSilent(t, bd, dir, "today"); got != today {
				t.Fatalf("default id = %q, want %q", got, today)
			}

			out := bdCreateFail(t, bd, dir, "again")
			if !strings.Contains(out, today+" already exists") {
				t.Errorf("duplicate refusal should name %s, got:\n%s", today, out)
			}
			// Same period through every other door is refused too.
			if out := bdCreateFail(t, bd, dir, "again", "--id", today); !strings.Contains(out, "already exists") {
				t.Errorf("duplicate --id not refused:\n%s", out)
			}
			if out := bdCreateFail(t, bd, dir, "again", "--date", now.Format("2006-01-02")); !strings.Contains(out, "already exists") {
				t.Errorf("duplicate --date not refused:\n%s", out)
			}

			if got := bdCreateSilent(t, bd, dir, "dated", "--date", k.explicit); got != k.wantExpl {
				t.Errorf("--date %s id = %q, want %q", k.explicit, got, k.wantExpl)
			}
			if out := bdCreateFail(t, bd, dir, "dup", "--date", k.explicit); !strings.Contains(out, k.wantExpl+" already exists") {
				t.Errorf("duplicate explicit date should name %s, got:\n%s", k.wantExpl, out)
			}

			for _, bad := range []string{k.nonMatchID, k.kind + "-random", "other-2026-03-04"} {
				if out := bdCreateFail(t, bd, dir, "bad", "--id", bad); !strings.Contains(out, "does not match") && !strings.Contains(out, "prefix mismatch") {
					t.Errorf("--id %s should be rejected, got:\n%s", bad, out)
				}
			}
			if out := bdCreateFail(t, bd, dir, "bad", "--date", "2026-02-30"); !strings.Contains(out, "invalid date") {
				t.Errorf("malformed --date should be rejected, got:\n%s", out)
			}

			// Bulk routes mint their own ids, so they are refused outright.
			plan := writeGraphCreatePlan(t, dir)
			if out := bdCreateFail(t, bd, dir, "--graph", plan); !strings.Contains(out, idpattern.ConfigKey) {
				t.Errorf("--graph should be refused, got:\n%s", out)
			}
			// A child of the period bead is not a second bead for the period.
			if got := bdCreateSilent(t, bd, dir, "child", "--parent", today); got != today+".1" {
				t.Errorf("child id = %q, want %s.1", got, today)
			}
		})
	}

	t.Run("no_pattern_date_rejected", func(t *testing.T) {
		dir, _, _ := bdInit(t, bd, "--prefix", "np")
		if out := bdCreateFail(t, bd, dir, "x", "--date", "2026-03-04"); !strings.Contains(out, idpattern.ConfigKey) {
			t.Errorf("--date without a pattern should explain, got:\n%s", out)
		}
		if id := bdCreateSilent(t, bd, dir, "plain"); !strings.HasPrefix(id, "np-") {
			t.Errorf("unpatterned store should mint a normal id, got %q", id)
		}
	})
}
