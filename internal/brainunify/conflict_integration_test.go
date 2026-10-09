package brainunify

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// These tests run the conflict-bead model against real Dolt servers, with the
// verifier as the judge: build a merged database from sources with identical
// and differing duplicates, change the sources, replay, verify.

// mergedQuery runs a SELECT against the merged database and returns the rows as
// text.
func (f *replayFixture) mergedQuery(query string, args ...any) [][]string {
	f.t.Helper()
	srv, err := StartIsolatedServer(f.ctx, "dolt", f.dataDir)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = srv.Stop() }()
	db, err := srv.OpenTarget(f.ctx, fixtureMergedDB)
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(f.ctx, query, args...)
	if err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out [][]string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			f.t.Fatal(err)
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			switch t := v.(type) {
			case nil:
				row[i] = "NULL"
			case []byte:
				row[i] = string(t)
			default:
				row[i] = fmt.Sprint(t)
			}
		}
		out = append(out, row)
	}
	return out
}

func (f *replayFixture) scalar(query string, args ...any) string {
	f.t.Helper()
	rows := f.mergedQuery(query, args...)
	if len(rows) != 1 || len(rows[0]) != 1 {
		f.t.Fatalf("%s: want one value, got %v", query, rows)
	}
	return rows[0][0]
}

// conflictOf returns the planned conflict for an id.
func (f *replayFixture) conflictOf(id string) Collision {
	f.t.Helper()
	_, plan := f.discover()
	for _, c := range plan.Collisions {
		if c.ID == id {
			return c
		}
	}
	f.t.Fatalf("%s is not a duplicated id", id)
	return Collision{}
}

// seedDuplicates gives alpha and beta two ids in common: shr-1, whose rows are
// identical, and shr-2, whose copies differ in status and notes and carry
// children that share their keys (the two stores' comments and dependency were
// copied from one another, so they have the same uuids).
func (f *replayFixture) seedDuplicates() {
	f.t.Helper()
	f.execIn("alpha", "insert into issues (id, title, status, created_at, updated_at) values ('shr-1', 'identical', 'open', '2026-01-01 00:00:00', '2026-01-02 00:00:00')")
	f.execIn("alpha", "insert into labels values ('shr-1', 'only-alpha-keeps-this')")
	f.execIn("beta", "insert into issues select * from alpha.issues where id = 'shr-1'")

	f.execIn("alpha", "insert into issues (id, title, status, notes, created_at, updated_at) values ('shr-2', 'differs', 'open', '', '2026-01-01 00:00:00', '2026-01-02 00:00:00')")
	f.execIn("beta", "insert into issues (id, title, status, notes, created_at, updated_at) values ('shr-2', 'differs', 'closed', 'verified fixed', '2026-01-01 00:00:00', '2026-03-04 05:06:07')")
	for _, db := range []string{"alpha", "beta"} {
		f.execIn(db, "insert into labels values ('shr-2', 'both')")
		f.execIn(db, "insert into comments values ('11111111-1111-4111-8111-111111111111', 'shr-2', 'a shared comment')")
		f.execIn(db, "insert into dependencies (id, issue_id, type, created_by, depends_on_issue_id) values ('22222222-2222-4222-8222-222222222222', 'shr-2', 'blocks', 'tester', ?)", db[:3]+"-1")
	}
	f.execIn("alpha", "insert into labels values ('shr-2', 'only-alpha')")
	f.execIn("beta", "insert into comments values (uuid(), 'shr-2', 'only beta wrote this')")

	// A bead in a third store that depends on the conflicted id: the link keeps
	// pointing at the original id.
	f.execIn("gamma", "insert into dependencies (id, issue_id, type, created_by, depends_on_issue_id) values ('33333333-3333-4333-8333-333333333333', 'gam-1', 'blocks', 'tester', 'shr-2')")
	f.commit("alpha", "duplicates")
	f.commit("beta", "duplicates")
	f.commit("gamma", "a link to the conflicted id")
}

// TestConflictBeadsAreBuiltAndVerified is the build half of the behaviour: the
// shape the captain asked for, checked row by row in the merged database, and
// the verifier passing against both references.
func TestConflictBeadsAreBuiltAndVerified(t *testing.T) {
	f := newReplayFixture(t)
	f.seedDuplicates()
	f.build()

	c := f.conflictOf("shr-2")
	if len(c.Copies) != 2 {
		t.Fatalf("conflict = %+v", c)
	}
	alphaID, betaID := c.copyIDs()["alpha"], c.copyIDs()["beta"]
	if !strings.HasPrefix(alphaID, "alp-") || !strings.HasPrefix(betaID, "bet-") {
		t.Fatalf("each copy is minted under its authoring store's prefix: %v", c.copyIDs())
	}

	// The conflict bead: the original id, open, saying what it is and naming both
	// copies and where each came from.
	if got := f.mergedQuery("select status, issue_type, created_by from issues where id='shr-2'"); fmt.Sprint(got) != "[[open task brain-unify]]" {
		t.Errorf("conflict bead row = %v", got)
	}
	title := f.scalar("select title from issues where id='shr-2'")
	desc := f.scalar("select description from issues where id='shr-2'")
	if !strings.Contains(title, "conflict") {
		t.Errorf("the title does not say it is a conflict: %q", title)
	}
	for _, want := range []string{alphaID, betaID, "store alpha", "store beta", "unresolved", "status, updated_at"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the description lacks %q:\n%s", want, desc)
		}
	}
	if got := f.scalar("select count(*) from labels where issue_id='shr-2' and label=?", ConflictLabel); got != "1" {
		t.Errorf("the conflict bead carries the %s label: %s", ConflictLabel, got)
	}
	// A dependency of type tracks from the conflict bead to each copy.
	deps := f.mergedQuery("select type, depends_on_issue_id from dependencies where issue_id='shr-2' order by depends_on_issue_id")
	if fmt.Sprint(deps) != fmt.Sprint([][]string{{"tracks", alphaID}, {"tracks", betaID}}) {
		t.Errorf("conflict dependencies = %v", deps)
	}
	// Nothing of the copies stayed at the original id.
	for _, table := range []string{"comments"} {
		if got := f.scalar("select count(*) from " + table + " where issue_id='shr-2'"); got != "0" {
			t.Errorf("%s still holds %s row(s) at the original id", table, got)
		}
	}
	// A link another bead held to the original id still points at it.
	if got := f.scalar("select count(*) from dependencies where issue_id='gam-1' and depends_on_issue_id='shr-2'"); got != "1" {
		t.Errorf("the link from gam-1 to shr-2 = %s row(s), want it to keep pointing at the conflict bead", got)
	}

	// Each copy is a bead of its own, with its own row and children.
	if got := f.mergedQuery("select id, title, status, notes from issues where id in (?, ?) order by id", alphaID, betaID); fmt.Sprint(got) != fmt.Sprint([][]string{
		{alphaID, "differs", "open", ""}, {betaID, "differs", "closed", "verified fixed"},
	}) {
		t.Errorf("copies = %v", got)
	}
	if got := f.scalar("select count(*) from labels where issue_id=?", alphaID); got != "2" {
		t.Errorf("alpha's copy has %s labels, want 2", got)
	}
	if got := f.scalar("select count(*) from labels where issue_id=?", betaID); got != "1" {
		t.Errorf("beta's copy has %s labels, want 1", got)
	}
	// The comment both stores held under one uuid exists once per copy, under two
	// different keys; beta's own comment is only on beta's copy.
	if got := f.scalar("select count(*) from comments where issue_id=?", alphaID); got != "1" {
		t.Errorf("alpha's copy has %s comments, want 1", got)
	}
	if got := f.scalar("select count(*) from comments where issue_id=?", betaID); got != "2" {
		t.Errorf("beta's copy has %s comments, want 2", got)
	}
	if got := f.scalar("select count(distinct id) from comments where body='a shared comment'"); got != "2" {
		t.Errorf("the shared comment should now have two keys, has %s", got)
	}
	if got := f.scalar("select count(*) from dependencies where issue_id in (?, ?) and type='blocks'", alphaID, betaID); got != "2" {
		t.Errorf("each copy keeps the dependency it authored: %s", got)
	}

	// The identical duplicate is one bead, as before.
	if got := f.scalar("select count(*) from issues where id='shr-1'"); got != "1" {
		t.Errorf("shr-1 = %s rows", got)
	}
	if got := f.scalar("select resolution from brain_unify_collisions where id='shr-1'"); got != ResolutionMergedIdentical {
		t.Errorf("shr-1 resolution = %s", got)
	}

	// The record names the conflict and its copies.
	rec := f.mergedQuery("select resolution, winner, copy_ids from brain_unify_collisions where id='shr-2'")
	if len(rec) != 1 || rec[0][0] != ResolutionConflict || rec[0][1] != "" ||
		!strings.Contains(rec[0][2], alphaID) || !strings.Contains(rec[0][2], betaID) {
		t.Errorf("collision record = %v", rec)
	}
	// 2 identical source rows merged to 1; 2 conflicting rows became 2 copies and
	// a conflict bead.
	// 15 beads of their own + shr-1 and shr-2 in two stores each = 19 source rows.
	if got := f.scalar("select count(*) from issues"); got != "19" {
		t.Errorf("issues = %s, want 19", got)
	}

	requireNoFailures(t, "verify (live) after the build", f.verify(ReferenceLive))
	requireNoFailures(t, "verify (recorded) after the build", f.verify(ReferenceRecorded))
}

// TestVerifierCatchesABrokenConflict corrupts the merged database in the ways a
// wrong build could, one at a time, and requires verification to fail in both
// modes.
func TestVerifierCatchesABrokenConflict(t *testing.T) {
	f := newReplayFixture(t)
	f.seedDuplicates()
	f.build()
	c := f.conflictOf("shr-2")
	alphaID, betaID := c.copyIDs()["alpha"], c.copyIDs()["beta"]
	requireNoFailures(t, "baseline", f.verify(ReferenceLive))

	breakages := []struct {
		name string
		sql  []string
		undo []string
	}{
		{"a copy's title edited",
			[]string{"update issues set title='tampered' where id='" + alphaID + "'"},
			[]string{"update issues set title='differs' where id='" + alphaID + "'"}},
		{"a copy's child row dropped",
			[]string{"delete from labels where issue_id='" + alphaID + "' and label='only-alpha'"},
			[]string{"insert into labels values ('" + alphaID + "', 'only-alpha')"}},
		{"a copy missing",
			[]string{"delete from issues where id='" + betaID + "'"},
			nil},
	}
	for _, b := range breakages {
		f.mergedSQL(b.sql...)
		requireFailures(t, b.name+" (live)", f.verify(ReferenceLive))
		requireFailures(t, b.name+" (recorded)", f.verify(ReferenceRecorded))
		if b.undo == nil {
			break
		}
		f.mergedSQL(b.undo...)
		requireNoFailures(t, "after undoing: "+b.name, f.verify(ReferenceLive))
	}
}

// TestConflictBeadIsCheckedToo breaks the conflict bead side: its status, its
// link to a copy, an extra row at the id, and an extra bead in a copy's
// namespace.
func TestConflictBeadIsCheckedToo(t *testing.T) {
	f := newReplayFixture(t)
	f.seedDuplicates()
	f.build()
	c := f.conflictOf("shr-2")
	alphaID := c.copyIDs()["alpha"]

	for _, b := range []struct {
		name      string
		sql, undo string
	}{
		{"the conflict bead closed", "update issues set status='closed' where id='shr-2'", "update issues set status='open' where id='shr-2'"},
		{"a link to a copy removed", "delete from dependencies where issue_id='shr-2' and depends_on_issue_id='" + alphaID + "'", ""},
		{"a stray row left at the original id", "insert into comments values (uuid(), 'shr-2', 'stray')", "delete from comments where issue_id='shr-2'"},
		{"an extra bead in a copy's namespace", "insert into issues (id, title) values ('alp-extra', 'not in any source')", "delete from issues where id='alp-extra'"},
	} {
		f.mergedSQL(b.sql)
		requireFailures(t, b.name+" (live)", f.verify(ReferenceLive))
		requireFailures(t, b.name+" (recorded)", f.verify(ReferenceRecorded))
		if b.undo == "" {
			break
		}
		f.mergedSQL(b.undo)
		requireNoFailures(t, "after undoing: "+b.name, f.verify(ReferenceLive))
	}
}

// TestReplayCreatesEditsAndRemovesConflicts is the replay half: every way a
// source change can create, edit or remove a divergent duplicate, with
// verification against the live sources failing before each replay and passing
// after it.
func TestReplayCreatesEditsAndRemovesConflicts(t *testing.T) {
	f := newReplayFixture(t)
	f.seedDuplicates()
	f.build()
	requireNoFailures(t, "right after the build", f.verify(ReferenceLive))

	// 1. An identical duplicate stops being identical: it becomes a conflict.
	f.execIn("beta", "update issues set status='closed', notes='done', updated_at='2026-05-05 05:05:05' where id='shr-1'")
	f.commit("beta", "beta closes its copy of shr-1")
	requireFailures(t, "a duplicate newly diverged, before the replay", f.verify(ReferenceLive))
	res, err := f.replay()
	if err != nil {
		t.Fatalf("replay 1: %v", err)
	}
	if !contains(res.CollisionsWritten, "shr-1") {
		t.Errorf("replay 1 did not rewrite the record of shr-1: %v", res.CollisionsWritten)
	}
	requireNoFailures(t, "after replay 1 (live)", f.verify(ReferenceLive))
	requireNoFailures(t, "after replay 1 (recorded)", f.verify(ReferenceRecorded))
	c1 := f.conflictOf("shr-1")
	if c1.Resolution != ResolutionConflict || len(c1.Copies) != 2 {
		t.Fatalf("shr-1 is now %+v", c1)
	}
	if got := f.scalar("select status from issues where id='shr-1'"); got != "open" {
		t.Errorf("the conflict bead at shr-1 has status %s", got)
	}
	if got := f.scalar("select count(*) from issues where id in (?, ?)", c1.copyIDs()["alpha"], c1.copyIDs()["beta"]); got != "2" {
		t.Errorf("shr-1's minted copies = %s rows", got)
	}
	// The one label alpha kept travelled with alpha's copy.
	if got := f.scalar("select count(*) from labels where issue_id=? and label='only-alpha-keeps-this'", c1.copyIDs()["alpha"]); got != "1" {
		t.Errorf("alpha's label did not follow its copy: %s", got)
	}

	// 2. A copy of an existing conflict is edited and gains a child.
	f.execIn("alpha", "update issues set title='differs, edited', updated_at='2026-06-06 06:06:06' where id='shr-2'")
	f.execIn("alpha", "insert into labels values ('shr-2', 'added-later')")
	f.commit("alpha", "edit a copy")
	requireFailures(t, "a copy edited, before the replay", f.verify(ReferenceLive))
	if _, err := f.replay(); err != nil {
		t.Fatalf("replay 2: %v", err)
	}
	requireNoFailures(t, "after replay 2 (live)", f.verify(ReferenceLive))
	requireNoFailures(t, "after replay 2 (recorded)", f.verify(ReferenceRecorded))
	alpha2 := f.conflictOf("shr-2").copyIDs()["alpha"]
	if got := f.scalar("select title from issues where id=?", alpha2); got != "differs, edited" {
		t.Errorf("alpha's copy has title %q", got)
	}
	if got := f.scalar("select count(*) from labels where issue_id=? and label='added-later'", alpha2); got != "1" {
		t.Errorf("the new label is not on alpha's copy: %s", got)
	}

	// 3. A bead that exists only in alpha is copied into gamma with different
	// content: a brand new conflict.
	f.execIn("gamma", "insert into issues (id, title, status) values ('alp-2', 'gamma disagrees', 'closed')")
	f.commit("gamma", "a differing copy of alp-2")
	requireFailures(t, "a new conflict, before the replay", f.verify(ReferenceLive))
	if _, err := f.replay(); err != nil {
		t.Fatalf("replay 3: %v", err)
	}
	requireNoFailures(t, "after replay 3 (live)", f.verify(ReferenceLive))
	requireNoFailures(t, "after replay 3 (recorded)", f.verify(ReferenceRecorded))
	c3 := f.conflictOf("alp-2")
	if got := f.scalar("select count(*) from issues where id in (?, ?)", c3.copyIDs()["alpha"], c3.copyIDs()["gamma"]); got != "2" {
		t.Errorf("alp-2's minted copies = %s rows", got)
	}
	if got := f.scalar("select count(*) from labels where issue_id=?", c3.copyIDs()["alpha"]); got != "1" {
		t.Errorf("alpha's copy of alp-2 keeps its label: %s", got)
	}

	// 4. The other copy of a conflict is deleted: the survivor is a plain bead
	// again and every minted id is gone.
	gone := f.conflictOf("shr-2").copyIDs()
	f.execIn("beta", "delete from labels where issue_id='shr-2'")
	f.execIn("beta", "delete from comments where issue_id='shr-2'")
	f.execIn("beta", "delete from dependencies where issue_id='shr-2'")
	f.execIn("beta", "delete from issues where id='shr-2'")
	f.commit("beta", "beta drops shr-2")
	requireFailures(t, "a conflict resolved by deletion, before the replay", f.verify(ReferenceLive))
	res, err = f.replay()
	if err != nil {
		t.Fatalf("replay 4: %v", err)
	}
	if !contains(res.CollisionsRemoved, "shr-2") {
		t.Errorf("replay 4 did not remove the record of shr-2: %v", res.CollisionsRemoved)
	}
	requireNoFailures(t, "after replay 4 (live)", f.verify(ReferenceLive))
	requireNoFailures(t, "after replay 4 (recorded)", f.verify(ReferenceRecorded))
	if got := f.scalar("select count(*) from issues where id in (?, ?)", gone["alpha"], gone["beta"]); got != "0" {
		t.Errorf("the minted copies of shr-2 survived the end of the conflict: %s", got)
	}
	if got := f.mergedQuery("select title, status from issues where id='shr-2'"); fmt.Sprint(got) != "[[differs, edited open]]" {
		t.Errorf("shr-2 is alpha's bead again: %v", got)
	}
	if got := f.scalar("select count(*) from dependencies where issue_id='shr-2' and type='tracks'"); got != "0" {
		t.Errorf("the conflict links outlived the conflict: %s", got)
	}
	if got := f.scalar("select count(*) from brain_unify_collisions where id='shr-2'"); got != "0" {
		t.Errorf("shr-2 is still recorded as a collision: %s", got)
	}

	// 5. Two differing copies are made identical again: they merge into one bead.
	f.execIn("beta", "delete from issues where id='shr-1'")
	f.execIn("beta", "insert into issues select * from alpha.issues where id = 'shr-1'")
	f.commit("beta", "beta's copy equals alpha's again")
	requireFailures(t, "a conflict made identical, before the replay", f.verify(ReferenceLive))
	if _, err := f.replay(); err != nil {
		t.Fatalf("replay 5: %v", err)
	}
	requireNoFailures(t, "after replay 5 (live)", f.verify(ReferenceLive))
	requireNoFailures(t, "after replay 5 (recorded)", f.verify(ReferenceRecorded))
	if got := f.scalar("select resolution from brain_unify_collisions where id='shr-1'"); got != ResolutionMergedIdentical {
		t.Errorf("shr-1 resolution = %s", got)
	}
	if got := f.scalar("select count(*) from issues where id in (?, ?)", c1.copyIDs()["alpha"], c1.copyIDs()["beta"]); got != "0" {
		t.Errorf("shr-1's minted copies survived: %s", got)
	}

	// One further edit fails the live verification again.
	f.execIn("gamma", "update issues set title='gamma edit' where id='alp-2'")
	f.commit("gamma", "one more edit")
	requireFailures(t, "after one further edit", f.verify(ReferenceLive))
	if _, err := f.replay(); err != nil {
		t.Fatalf("replay 6: %v", err)
	}
	requireNoFailures(t, "after the last replay", f.verify(ReferenceLive))
}

// TestBuildRefusesAMintedIDThatAlreadyExists derives a copy's id and plants a
// bead with that id in another store: the primary key would be ambiguous, so
// the build refuses and writes nothing.
func TestBuildRefusesAMintedIDThatAlreadyExists(t *testing.T) {
	f := newReplayFixture(t)
	f.seedDuplicates()
	minted := f.conflictOf("shr-2").copyIDs()["alpha"]
	f.execIn("gamma", "insert into issues (id, title) values (?, 'squatter')", minted)
	f.commit("gamma", "plant the id")

	source, plan := f.discover()
	_, err := NewBuilder(source, plan, BuildOptions{
		DataDir: f.dataDir, Database: fixtureMergedDB, DoltBin: "dolt", Host: "127.0.0.1", Port: f.srcSrv.Port,
	}).Build(f.ctx)
	if err == nil || !strings.Contains(err.Error(), "refusing to build") || !strings.Contains(err.Error(), minted) {
		t.Fatalf("want a refusal naming %s, got %v", minted, err)
	}
}

// makeDB gives the scratch source server a database with the fixture schema.
func (f *replayFixture) makeDB(name, prefix string) {
	f.t.Helper()
	f.exec("create database `" + name + "`")
	for _, stmt := range []string{
		"create table issues (id varchar(255) primary key, title varchar(500) not null, description varchar(2000) not null default '', notes varchar(2000) not null default '', status varchar(32) not null default 'open', priority int not null default 2, issue_type varchar(32) not null default 'task', content_hash varchar(64), created_at datetime not null default current_timestamp, created_by varchar(255) default '', updated_at datetime not null default current_timestamp)",
		"create table labels (issue_id varchar(255) not null, label varchar(255) not null, primary key (issue_id, label))",
		"create table comments (id char(36) primary key, issue_id varchar(255) not null, body text not null)",
		"create table dependencies (id char(36) primary key, issue_id varchar(255) not null, type varchar(32) not null default 'blocks', created_at datetime not null default current_timestamp, created_by varchar(255) not null default '', thread_id varchar(255) default '', depends_on_issue_id varchar(255))",
		"create table config (`key` varchar(255) primary key, value text not null)",
		"create table metadata (`key` varchar(255) primary key, value text not null)",
		"replace into dolt_ignore values ('wisps', true)",
		"create table wisps (id varchar(255) primary key, title varchar(500) not null)",
	} {
		f.execIn(name, stmt)
	}
	f.execIn(name, "replace into config values ('issue_prefix', '"+prefix+"')")
	f.execIn(name, "replace into metadata values ('_project_id', 'proj-"+name+"')")
}

// TestIncludedDatabaseAndRescuedOrphans: an unregistered database with two
// prefixes is a store when the operator says so, and a replica contributes only
// the beads no store holds. Everything verifies, and the replay keeps both
// current.
func TestIncludedDatabaseAndRescuedOrphans(t *testing.T) {
	f := newReplayFixture(t)
	f.makeDB("multi", "xa")
	f.execIn("multi", "insert into issues (id, title) values ('xa-1', 'first prefix'), ('xb-1', 'second prefix')")
	f.execIn("multi", "insert into labels values ('xb-1', 'child')")
	f.commit("multi", "seed")

	f.makeDB("repl", "alp")
	// copies of two beads that live in alpha, plus two that live nowhere else
	f.execIn("repl", "insert into issues select * from alpha.issues where id in ('alp-1', 'alp-2')")
	f.execIn("repl", "insert into labels select * from alpha.labels where issue_id in ('alp-1', 'alp-2')")
	f.execIn("repl", "insert into issues (id, title) values ('orp-1', 'orphan one'), ('orp-2', 'orphan two')")
	f.execIn("repl", "insert into labels values ('orp-1', 'only-here')")
	// a child row of a bead the replica holds no issue row for (it lives in alpha)
	f.execIn("repl", "insert into labels values ('alp-3', 'stray-in-replica')")
	f.commit("repl", "seed")

	// Without the flags both databases are replicas and nothing of them is carried.
	f.build()
	if got := f.scalar("select count(*) from issues where id in ('xa-1','xb-1','orp-1')"); got != "0" {
		t.Fatalf("without the flags the replicas must stay out, found %s", got)
	}

	f.reg.IncludeDatabases = []string{"multi"}
	f.reg.RescueOrphansFrom = []string{"repl"}
	f.dataDir += "-2"
	f.build()
	for _, id := range []string{"xa-1", "xb-1", "orp-1", "orp-2"} {
		if got := f.scalar("select count(*) from issues where id=?", id); got != "1" {
			t.Errorf("%s: %s rows in the merged database, want 1", id, got)
		}
	}
	if got := f.scalar("select count(*) from issues where id='alp-1'"); got != "1" {
		t.Errorf("alp-1 lives in alpha and the replica copy must not add a second: %s", got)
	}
	if got := f.scalar("select count(*) from labels where issue_id='orp-1' and label='only-here'"); got != "1" {
		t.Errorf("an orphan's children come with it: %s", got)
	}
	if got := f.scalar("select count(*) from labels where issue_id='alp-3' and label='stray-in-replica'"); got != "0" {
		t.Errorf("a child row the replica holds for a bead that lives in alpha must not be carried: %s", got)
	}
	if got := f.scalar("select count(*) from labels where issue_id='alp-1'"); got != "1" {
		t.Errorf("the replica's copy of alp-1's label must not be added to alpha's: %s", got)
	}
	if got := f.scalar("select owner_reason from brain_store_prefixes where prefix='orp'"); got != ReasonSoleObserver {
		t.Errorf("the orphans' prefix is owned by the replica's store as sole observer: %s", got)
	}
	requireNoFailures(t, "verify live", f.verify(ReferenceLive))
	requireNoFailures(t, "verify recorded", f.verify(ReferenceRecorded))

	// The replica gains an orphan and one is edited; the included store gains a bead.
	f.execIn("repl", "insert into issues (id, title) values ('orp-3', 'orphan three')")
	f.execIn("repl", "update issues set title='orphan one, edited' where id='orp-1'")
	f.commit("repl", "changes")
	f.execIn("multi", "insert into issues (id, title) values ('xb-2', 'new in multi')")
	f.commit("multi", "new bead")
	requireFailures(t, "before the replay", f.verify(ReferenceLive))
	if _, err := f.replay(); err != nil {
		t.Fatalf("replay: %v", err)
	}
	requireNoFailures(t, "after the replay (live)", f.verify(ReferenceLive))
	requireNoFailures(t, "after the replay (recorded)", f.verify(ReferenceRecorded))
	if got := f.scalar("select title from issues where id='orp-1'"); got != "orphan one, edited" {
		t.Errorf("orp-1 = %q", got)
	}
	if got := f.scalar("select count(*) from issues where id in ('orp-3','xb-2')"); got != "2" {
		t.Errorf("new beads in the rescued/included databases = %s, want 2", got)
	}
}

// TestRecordedVerifyAfterReplayWhenStoresShareAPrefix: two stores each hold beads
// of one prefix that nobody declares. The recorded fingerprints are per store, the
// verifier's reference is their combination, and a replay that changes one
// store's share must leave the recorded verification passing.
func TestRecordedVerifyAfterReplayWhenStoresShareAPrefix(t *testing.T) {
	f := newReplayFixture(t)
	f.execIn("alpha", "insert into issues (id, title) values ('zzz-1', 'alpha holds zzz-1'), ('zzz-2', 'alpha holds zzz-2')")
	f.execIn("beta", "insert into issues (id, title) values ('zzz-3', 'beta holds zzz-3')")
	f.commit("alpha", "zzz")
	f.commit("beta", "zzz")
	f.build()
	requireNoFailures(t, "recorded after the build", f.verify(ReferenceRecorded))
	f.execIn("alpha", "update issues set title='alpha edited zzz-1' where id='zzz-1'")
	f.commit("alpha", "edit")
	if _, err := f.replay(); err != nil {
		t.Fatalf("replay: %v", err)
	}
	requireNoFailures(t, "recorded after the replay", f.verify(ReferenceRecorded))
	requireNoFailures(t, "live after the replay", f.verify(ReferenceLive))
	f.execIn("beta", "insert into issues (id, title) values ('zzz-4', 'beta adds zzz-4')")
	f.commit("beta", "add")
	if _, err := f.replay(); err != nil {
		t.Fatalf("second replay: %v", err)
	}
	requireNoFailures(t, "recorded after the second replay", f.verify(ReferenceRecorded))
}

// TestReplayWithNothingNewTouchesNothing: a replay over unchanged sources writes
// no table, and a change in a table Dolt does not version (wisps) is found by its
// fingerprint, reloads that table alone, and leaves verification passing.
func TestReplayWithNothingNewTouchesNothing(t *testing.T) {
	f := newReplayFixture(t)
	f.build()
	res, err := f.replay()
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(res.Tables) != 0 || len(res.Beads) != 0 || len(res.ChangedStores) != 0 {
		t.Errorf("a replay over unchanged sources must touch nothing: tables %v beads %v stores %v", res.Tables, res.Beads, res.ChangedStores)
	}
	requireNoFailures(t, "live after a no-change replay", f.verify(ReferenceLive))
	requireNoFailures(t, "recorded after a no-change replay", f.verify(ReferenceRecorded))

	// only an unversioned table changes: no commit, nothing in dolt_diff
	f.execIn("beta", "insert into wisps values ('bet-wisp-9', 'a new wisp')")
	requireFailures(t, "an unversioned change, before the replay", f.verify(ReferenceLive))
	res, err = f.replay()
	if err != nil {
		t.Fatalf("replay 2: %v", err)
	}
	reloaded := map[string]bool{}
	for _, tb := range res.Tables {
		reloaded[tb.Table] = true
	}
	if !reloaded["wisps"] || reloaded["issues"] || reloaded["labels"] {
		t.Errorf("only the wisps table should have been reloaded: %v", res.Tables)
	}
	requireNoFailures(t, "live after the unversioned change", f.verify(ReferenceLive))
	requireNoFailures(t, "recorded after the unversioned change", f.verify(ReferenceRecorded))
	// and the next replay is a no-op again
	if res, err = f.replay(); err != nil || len(res.Tables) != 0 {
		t.Errorf("the next replay must be a no-op again: %v %v", res.Tables, err)
	}
}

// hostedReplayer runs a replay against a merged database a RUNNING server hosts.
func (f *replayFixture) hostedReplay(port int, opts ReplayOptions) (ReplayResult, error) {
	f.t.Helper()
	source, plan := f.discover()
	opts.Database, opts.Host, opts.Port = fixtureMergedDB, "127.0.0.1", f.srcSrv.Port
	opts.MergedHost, opts.MergedPort = "127.0.0.1", port
	return NewReplayer(source, plan, opts).Replay(f.ctx)
}

func (f *replayFixture) hostedVerify(port int, reference string) []Check {
	f.t.Helper()
	source, plan := f.discover()
	plans, err := TablePlansFor(f.ctx, source, plan, "")
	if err != nil {
		f.t.Fatal(err)
	}
	v, err := NewVerifier(f.ctx, source, plan, plans, VerifyOptions{Database: fixtureMergedDB, Host: "127.0.0.1", Port: port, Reference: reference})
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = v.Close() }()
	res, err := v.Verify(f.ctx)
	if err != nil {
		f.t.Fatalf("verify: %v", err)
	}
	return res.Failures()
}

// TestReplayUpdatesAHostedMergedDatabaseInPlace is the no-downtime shape: the
// merged database stays on a running server, the sources keep changing, the
// replay (and the readiness check, a dry-run replay) work against that server,
// and a replay run after the stores were repointed does not undo what the merged
// database changed since.
func TestReplayUpdatesAHostedMergedDatabaseInPlace(t *testing.T) {
	f := newReplayFixture(t)
	f.build()
	srv, err := StartIsolatedServer(f.ctx, "dolt", f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Stop() }()

	// ready: nothing differs yet
	res, err := f.hostedReplay(srv.Port, ReplayOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if res.Pending() != 0 || !res.DryRun {
		t.Fatalf("a fresh build must be ready: %+v", res)
	}
	if !strings.Contains(readinessVerdict(res), "READY") {
		t.Errorf("verdict = %q", readinessVerdict(res))
	}

	// the sources move while the server stays up
	f.execIn("alpha", "update issues set title='alpha 2 edited', updated_at=now() where id='alp-2'")
	f.execIn("alpha", "update issues set title='alpha 3 edited', updated_at=now() where id='alp-3'")
	f.bead("beta", "bet-7", "beta new")
	f.commit("alpha", "edits")
	f.commit("beta", "new bead")
	res, err = f.hostedReplay(srv.Port, ReplayOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry run 2: %v", err)
	}
	if res.Pending() == 0 || len(res.Beads) < 3 || !strings.Contains(readinessVerdict(res), "NOT YET") {
		t.Fatalf("the readiness check must list the differences: %+v", res)
	}
	if got := f.hostedVerifyTitle(srv.Port, "alp-2"); got == "alpha 2 edited" {
		t.Fatal("a dry run must apply nothing")
	}

	res, err = f.hostedReplay(srv.Port, ReplayOptions{})
	if err != nil {
		t.Fatalf("hosted replay: %v", err)
	}
	if !res.Committed {
		t.Errorf("a hosted replay commits unless asked not to")
	}
	requireNoFailures(t, "live after the hosted replay", f.hostedVerify(srv.Port, ReferenceLive))
	requireNoFailures(t, "recorded after the hosted replay", f.hostedVerify(srv.Port, ReferenceRecorded))
	res, err = f.hostedReplay(srv.Port, ReplayOptions{DryRun: true})
	if err != nil || res.Pending() != 0 {
		t.Fatalf("after the replay the readiness check must say READY: %v %+v", err, res)
	}

	// the stores are repointed: from here the merged database is written to.
	time.Sleep(1200 * time.Millisecond)
	repointed := time.Now().UTC().Format("2006-01-02 15:04:05")
	time.Sleep(1200 * time.Millisecond)
	db, err := srv.OpenTarget(f.ctx, fixtureMergedDB)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"update issues set title='written in the merged database', updated_at=utc_timestamp() where id='alp-2'",
		"insert into comments values (uuid(), 'alp-4', 'a comment written after the repoint')",
		"update issues set updated_at=utc_timestamp() where id='alp-4'",
		// a bead whose merged row is untouched but gained a comment: an older
		// write to the old database must still reach the row without losing it
		"insert into comments values (uuid(), 'alp-5', 'merged-only comment on alp-5')",
	} {
		if _, err := db.ExecContext(f.ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// stragglers: the old databases change after the repoint too
	f.execIn("alpha", "update issues set title='straggler wrote alp-2', updated_at=now() where id='alp-2'")
	f.execIn("alpha", "update issues set title='straggler wrote alp-3', updated_at=now() where id='alp-3'")
	f.execIn("alpha", "update issues set title='straggler wrote alp-4', updated_at=now() where id='alp-4'")
	f.execIn("alpha", "update issues set title='straggler wrote alp-5', updated_at=now() where id='alp-5'")
	f.execIn("alpha", "insert into labels values ('alp-3', 'straggler-label')")
	f.commit("alpha", "stragglers")
	res, err = f.hostedReplay(srv.Port, ReplayOptions{NoCommit: true, ProtectAfter: repointed})
	if err != nil {
		t.Fatalf("post-repoint replay: %v", err)
	}
	if !reflect.DeepEqual(res.KeptMerged, []string{"alp-2", "alp-4"}) {
		t.Errorf("KeptMerged = %v, want alp-2 and alp-4 (changed in the merged database after the repoint)", res.KeptMerged)
	}
	if res.Committed {
		t.Error("--no-commit must not commit")
	}
	if got := f.hostedVerifyTitle(srv.Port, "alp-2"); got != "written in the merged database" {
		t.Errorf("alp-2 = %q: the merged write was undone", got)
	}
	if got := f.hostedVerifyTitle(srv.Port, "alp-3"); got != "straggler wrote alp-3" {
		t.Errorf("alp-3 = %q: the straggler write to the old database was not carried over", got)
	}
	if got := f.hostedVerifyTitle(srv.Port, "alp-5"); got != "straggler wrote alp-5" {
		t.Errorf("alp-5 = %q: the older write to the old database was not carried over", got)
	}
	var n int
	if err := db.QueryRowContext(f.ctx, "select count(*) from comments where issue_id='alp-5' and body like 'merged-only%'").Scan(&n); err != nil || n != 1 {
		t.Errorf("the merged-only comment on alp-5 = %d rows (%v): a catch-up deleted what the merged database wrote", n, err)
	}
	if err := db.QueryRowContext(f.ctx, "select count(*) from labels where issue_id='alp-3' and label='straggler-label'").Scan(&n); err != nil || n != 1 {
		t.Errorf("the straggler's new label on alp-3 = %d rows (%v), want it added", n, err)
	}
	if err := db.QueryRowContext(f.ctx, "select count(*) from comments where issue_id='alp-4' and body like 'a comment written after%'").Scan(&n); err != nil || n != 1 {
		t.Errorf("the post-repoint comment on alp-4 = %d rows (%v), want 1", n, err)
	}
}

func (f *replayFixture) hostedVerifyTitle(port int, id string) string {
	f.t.Helper()
	db, err := OpenSource("127.0.0.1", port)
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.query(f.ctx, "select title from `"+fixtureMergedDB+"`.issues where id = ?", id)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var title string
	if rows.Next() {
		_ = rows.Scan(&title)
	}
	return title
}

// TestTwoReplaysDoNotRunAtOnce: a second replay of a hosted merged database
// refuses while the first holds the lock.
func TestTwoReplaysDoNotRunAtOnce(t *testing.T) {
	f := newReplayFixture(t)
	f.build()
	srv, err := StartIsolatedServer(f.ctx, "dolt", f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Stop() }()
	db, err := srv.OpenTarget(f.ctx, fixtureMergedDB)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var got int
	if err := conn.QueryRowContext(f.ctx, "select get_lock('brain_unify_replay', 0)").Scan(&got); err != nil || got != 1 {
		t.Fatalf("taking the lock: %v %d", err, got)
	}
	if _, err := f.hostedReplay(srv.Port, ReplayOptions{}); err == nil || !strings.Contains(err.Error(), "another replay") {
		t.Fatalf("a second replay must refuse: %v", err)
	}
	if _, err := f.hostedReplay(srv.Port, ReplayOptions{DryRun: true}); err != nil {
		t.Fatalf("a dry run (the readiness check) does not need the lock: %v", err)
	}
	_, _ = conn.ExecContext(f.ctx, "select release_lock('brain_unify_replay')")
	if _, err := f.hostedReplay(srv.Port, ReplayOptions{}); err != nil {
		t.Fatalf("after the lock is released a replay runs: %v", err)
	}
}

// TestPostRepointCatchUpReadsTheOldDatabases: once the stores are repointed, every
// store's metadata names the merged database, so the registry no longer says where
// a store's own database is. The catch-up takes that from the merged database's
// own record (brain_stores.source_database), never treats the merged database as a
// source, and carries a write made to an old database after the last sync.
func TestPostRepointCatchUpReadsTheOldDatabases(t *testing.T) {
	f := newReplayFixture(t)
	f.build()
	srv, err := StartIsolatedServer(f.ctx, "dolt", f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Stop() }()
	if _, err := f.hostedReplay(srv.Port, ReplayOptions{}); err != nil {
		t.Fatalf("pre-repoint sync: %v", err)
	}

	// the repoint: every store's metadata now names the merged database
	for i := range f.reg.Stores {
		f.reg.Stores[i].Database = fixtureMergedDB
	}
	time.Sleep(1200 * time.Millisecond)
	repointed := time.Now().UTC().Format("2006-01-02 15:04:05")
	time.Sleep(1200 * time.Millisecond)

	// a write reaches an OLD database after the last pre-repoint sync
	f.execIn("beta", "update issues set title='written to the old database after the last sync', updated_at=now() where id='bet-2'")
	f.bead("gamma", "gam-9", "created in an old database after the last sync")
	f.commit("beta", "straggler")
	f.commit("gamma", "straggler")

	// the bug: with the registry as the metadata now describes it, the stores are
	// "built from X but now read from brain_unified" and the replay refuses
	if _, err := f.hostedReplay(srv.Port, ReplayOptions{NoCommit: true, ProtectAfter: repointed}); err == nil || !strings.Contains(err.Error(), "refusing to replay") {
		t.Fatalf("without the merged database's record the repointed registry must be refused as before: %v", err)
	}

	mapping, err := LoadMergedMapping(f.ctx, "127.0.0.1", srv.Port, fixtureMergedDB)
	if err != nil || mapping["beta"] != "beta" {
		t.Fatalf("the merged database records where each store came from: %v %v", mapping, err)
	}
	f.reg.ApplyMergedMapping(fixtureMergedDB, mapping)
	res, err := f.hostedReplay(srv.Port, ReplayOptions{NoCommit: true, ProtectAfter: repointed})
	if err != nil {
		t.Fatalf("post-repoint catch-up: %v", err)
	}
	if got := f.hostedVerifyTitle(srv.Port, "bet-2"); got != "written to the old database after the last sync" {
		t.Errorf("bet-2 = %q: the write to the old database was not carried over (kept %v)", got, res.KeptMerged)
	}
	if got := f.hostedVerifyTitle(srv.Port, "gam-9"); got != "created in an old database after the last sync" {
		t.Errorf("gam-9 = %q: the bead created in an old database was not carried over", got)
	}
	// and the next one is quiet, with the pins advanced
	res, err = f.hostedReplay(srv.Port, ReplayOptions{DryRun: true})
	if err != nil || res.Pending() != 0 {
		t.Errorf("after the catch-up nothing is pending: %v %+v", err, res)
	}
}
