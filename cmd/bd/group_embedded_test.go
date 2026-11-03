//go:build cgo

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// bdGroupJSON runs "bd group <args> --json" and decodes the progress payload.
func bdGroupJSON(t *testing.T, bd, dir string, args ...string) groupProgress {
	t.Helper()
	fullArgs := append([]string{"group"}, args...)
	fullArgs = append(fullArgs, "--json")
	out, err := bdRunWithFlockRetry(t, bd, dir, fullArgs...)
	if err != nil {
		t.Fatalf("bd group %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	s := string(out)
	start := strings.Index(s, "{")
	if start < 0 {
		t.Fatalf("no JSON in group output: %s", s)
	}
	var p groupProgress
	if err := json.Unmarshal([]byte(s[start:]), &p); err != nil {
		t.Fatalf("parse group JSON: %v\n%s", err, s)
	}
	return p
}

// statusOf reads only the status of a bead. The shared bdShow helper cannot
// decode `show --json` for beads that carry tracks edges.
func statusOf(t *testing.T, bd, dir, id string) string {
	t.Helper()
	out, err := bdRunWithFlockRetry(t, bd, dir, "show", id, "--json")
	if err != nil {
		t.Fatalf("bd show %s: %v\n%s", id, err, out)
	}
	s := string(out)
	start := strings.IndexAny(s, "[{")
	var rows []struct {
		Status string `json:"status"`
	}
	if s[start] == '{' {
		s = "[" + s[start:] + "]"
	} else {
		s = s[start:]
	}
	if err := json.Unmarshal([]byte(s), &rows); err != nil || len(rows) == 0 {
		t.Fatalf("parse show %s: %v\n%s", id, err, out)
	}
	return rows[0].Status
}

func bdGroupFail(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bd, append([]string{"group"}, args...)...)
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected bd group %s to fail:\n%s", strings.Join(args, " "), out)
	}
	return string(out)
}

func TestEmbeddedGroup(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "gr")

	a := bdCreate(t, bd, dir, "Ship the docs", "--type", "task")
	b := bdCreate(t, bd, dir, "Ship the installer", "--type", "task")
	c := bdCreate(t, bd, dir, "Ship the announcement", "--type", "task")

	g := bdGroupJSON(t, bd, dir, "create", "Launch push", a.ID, b.ID)
	if g.Total != 2 || g.Done != 0 {
		t.Fatalf("new group: total=%d done=%d, want 2/0", g.Total, g.Done)
	}
	groupID := g.Group.ID

	t.Run("tracking_does_not_block_members", func(t *testing.T) {
		if got := statusOf(t, bd, dir, a.ID); got != "open" {
			t.Fatalf("member status = %s", got)
		}
		out, err := bdRunWithFlockRetry(t, bd, dir, "ready", "--json")
		if err != nil {
			t.Fatalf("bd ready: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), a.ID) || !strings.Contains(string(out), b.ID) {
			t.Fatalf("tracked members missing from ready work:\n%s", out)
		}
		// Members close freely even though a group tracks them.
		bdClose(t, bd, dir, a.ID)
	})

	t.Run("add_and_remove_members", func(t *testing.T) {
		p := bdGroupJSON(t, bd, dir, "add", groupID, c.ID)
		if p.Total != 3 || p.Done != 1 {
			t.Fatalf("after add: total=%d done=%d, want 3/1", p.Total, p.Done)
		}
		// Adding twice is a no-op.
		p = bdGroupJSON(t, bd, dir, "add", groupID, c.ID)
		if p.Total != 3 {
			t.Fatalf("duplicate add changed total to %d", p.Total)
		}
		p = bdGroupJSON(t, bd, dir, "remove", groupID, c.ID)
		if p.Total != 2 {
			t.Fatalf("after remove: total=%d, want 2", p.Total)
		}
		if out := bdGroupFail(t, bd, dir, "remove", groupID, c.ID); !strings.Contains(out, "not tracked") {
			t.Fatalf("unexpected error: %s", out)
		}
	})

	t.Run("show_reports_progress", func(t *testing.T) {
		out, err := bdRunWithFlockRetry(t, bd, dir, "group", "show", groupID)
		if err != nil {
			t.Fatalf("group show: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "1/2 members closed") {
			t.Fatalf("progress line missing:\n%s", out)
		}
	})

	t.Run("group_stays_open_until_last_member_closes", func(t *testing.T) {
		if got := statusOf(t, bd, dir, groupID); got != "open" {
			t.Fatalf("group closed early: %s", got)
		}
		out := bdClose(t, bd, dir, b.ID)
		if !strings.Contains(out, "Auto-closed finished group") {
			t.Fatalf("close did not report the group closing:\n%s", out)
		}
		if got := statusOf(t, bd, dir, groupID); got != "closed" {
			t.Fatalf("group status = %s, want closed", got)
		}
	})

	t.Run("removing_last_open_member_finishes_group", func(t *testing.T) {
		d := bdCreate(t, bd, dir, "Quick win", "--type", "task")
		e := bdCreate(t, bd, dir, "Dropped scope", "--type", "task")
		g2 := bdGroupJSON(t, bd, dir, "create", "Second push", d.ID, e.ID)
		bdClose(t, bd, dir, d.ID)
		bdGroupJSON(t, bd, dir, "remove", g2.Group.ID, e.ID)
		if got := statusOf(t, bd, dir, g2.Group.ID); got != "closed" {
			t.Fatalf("group status = %s, want closed", got)
		}
	})

	t.Run("empty_group_does_not_close", func(t *testing.T) {
		g3 := bdGroupJSON(t, bd, dir, "create", "Empty push")
		out, err := bdRunWithFlockRetry(t, bd, dir, "group", "sync")
		if err != nil {
			t.Fatalf("group sync: %v\n%s", err, out)
		}
		if got := statusOf(t, bd, dir, g3.Group.ID); got != "open" {
			t.Fatalf("empty group status = %s, want open", got)
		}
	})

	t.Run("plain_tracks_edge_is_not_a_group", func(t *testing.T) {
		x := bdCreate(t, bd, dir, "Old convoy", "--type", "task")
		y := bdCreate(t, bd, dir, "Convoy member", "--type", "task")
		bdDep(t, bd, dir, "add", x.ID, y.ID, "--type", "tracks")
		bdClose(t, bd, dir, y.ID)
		if got := statusOf(t, bd, dir, x.ID); got != "open" {
			t.Fatalf("unlabelled tracker closed: %s", got)
		}
		if out := bdGroupFail(t, bd, dir, "show", x.ID); !strings.Contains(out, "not a finish-line group") {
			t.Fatalf("unexpected error: %s", out)
		}
	})

	t.Run("update_status_closed_finishes_group", func(t *testing.T) {
		m := bdCreate(t, bd, dir, "Update-closed member", "--type", "task")
		g4 := bdGroupJSON(t, bd, dir, "create", "Update push", m.ID)
		bdUpdate(t, bd, dir, m.ID, "--status", "closed")
		if got := statusOf(t, bd, dir, g4.Group.ID); got != "closed" {
			t.Fatalf("group status = %s, want closed", got)
		}
	})

	t.Run("marking_duplicate_finishes_group", func(t *testing.T) {
		m := bdCreate(t, bd, dir, "Dup member", "--type", "task")
		canon := bdCreate(t, bd, dir, "Canonical", "--type", "task")
		g5 := bdGroupJSON(t, bd, dir, "create", "Dup push", m.ID)
		if out, err := bdRunWithFlockRetry(t, bd, dir, "duplicate", m.ID, "--of", canon.ID); err != nil {
			t.Fatalf("bd duplicate: %v\n%s", err, out)
		}
		if got := statusOf(t, bd, dir, g5.Group.ID); got != "closed" {
			t.Fatalf("group status = %s, want closed", got)
		}
	})

	t.Run("create_over_closed_members_closes_group", func(t *testing.T) {
		m := bdCreate(t, bd, dir, "Already done", "--type", "task")
		bdClose(t, bd, dir, m.ID)
		g6 := bdGroupJSON(t, bd, dir, "create", "Retroactive push", m.ID)
		if got := statusOf(t, bd, dir, g6.Group.ID); got != "closed" {
			t.Fatalf("group status = %s, want closed", got)
		}
	})

	t.Run("add_closed_member_closes_group", func(t *testing.T) {
		done := bdCreate(t, bd, dir, "Done one", "--type", "task")
		bdClose(t, bd, dir, done.ID)
		g7 := bdGroupJSON(t, bd, dir, "create", "Seed push")
		bdGroupJSON(t, bd, dir, "add", g7.Group.ID, done.ID)
		if got := statusOf(t, bd, dir, g7.Group.ID); got != "closed" {
			t.Fatalf("group status = %s, want closed", got)
		}
	})

}
