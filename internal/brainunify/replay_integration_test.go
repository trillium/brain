package brainunify

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests run the whole cycle against real Dolt servers: sources on one
// scratch server, a merged database built from them on another, then writes to
// the sources, a replay, and the verifier as the judge. They need the dolt
// binary and are skipped without it.

// replayFixture is a scratch federation of sources and the merged database
// built from it.
type replayFixture struct {
	t       *testing.T
	ctx     context.Context
	srcSrv  *IsolatedServer
	src     *sql.DB
	reg     Registry
	dataDir string
}

const fixtureMergedDB = "brain_unified"

func newReplayFixture(t *testing.T) *replayFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test starts dolt servers")
	}
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt binary not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)

	srv, err := StartIsolatedServer(ctx, "dolt", filepath.Join(t.TempDir(), "sources"))
	if err != nil {
		t.Fatalf("starting the source server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	src, err := srv.OpenTarget(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })

	f := &replayFixture{t: t, ctx: ctx, srcSrv: srv, src: src, dataDir: filepath.Join(t.TempDir(), "merged")}
	for _, s := range []struct{ name, prefix string }{{"alpha", "alp"}, {"beta", "bet"}, {"gamma", "gam"}} {
		f.exec("create database `" + s.name + "`")
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
			f.execIn(s.name, stmt)
		}
		f.execIn(s.name, "replace into config values ('issue_prefix', '"+s.prefix+"')")
		f.execIn(s.name, "replace into metadata values ('_project_id', 'proj-"+s.name+"')")
		for i := 1; i <= 5; i++ {
			f.bead(s.name, fmt.Sprintf("%s-%d", s.prefix, i), fmt.Sprintf("%s bead %d", s.name, i))
		}
		f.execIn(s.name, "insert into wisps values ('"+s.prefix+"-wisp-1', 'wisp')")
		f.commit(s.name, "seed")
		f.reg.Stores = append(f.reg.Stores, Store{Namespace: s.name, Registered: true, Database: s.name, DeclaredPrefixes: []string{s.prefix}})
	}
	return f
}

func (f *replayFixture) exec(stmt string, args ...any) {
	f.t.Helper()
	if _, err := f.src.ExecContext(f.ctx, stmt, args...); err != nil {
		f.t.Fatalf("%s: %v", stmt, err)
	}
}

func (f *replayFixture) execIn(db, stmt string, args ...any) {
	f.t.Helper()
	// A pooled connection may not be the one that ran "use": pin one.
	conn, err := f.src.Conn(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(f.ctx, "use `"+db+"`"); err != nil {
		f.t.Fatal(err)
	}
	if _, err := conn.ExecContext(f.ctx, stmt, args...); err != nil {
		f.t.Fatalf("%s: %s: %v", db, stmt, err)
	}
}

func (f *replayFixture) commit(db, msg string) {
	f.t.Helper()
	conn, err := f.src.Conn(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(f.ctx, "use `"+db+"`"); err != nil {
		f.t.Fatal(err)
	}
	rows, err := conn.QueryContext(f.ctx, "call dolt_commit('-Am', ?)", msg)
	if err != nil {
		f.t.Fatalf("committing %s: %v", db, err)
	}
	for rows.Next() {
	}
	_ = rows.Close()
}

// bead adds a bead with a label and a comment.
func (f *replayFixture) bead(db, id, title string) {
	f.t.Helper()
	f.execIn(db, "insert into issues (id, title) values (?, ?)", id, title)
	f.execIn(db, "insert into labels values (?, 'tag')", id)
	f.execIn(db, "insert into comments values (uuid(), ?, 'a comment')", id)
}

// discover plans the federation as it stands now.
func (f *replayFixture) discover() (*ReadOnlySource, Plan) {
	f.t.Helper()
	source, err := OpenSource("127.0.0.1", f.srcSrv.Port)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = source.Close() })
	disc, err := Discover(f.ctx, source, f.reg)
	if err != nil {
		f.t.Fatalf("discover: %v", err)
	}
	return source, disc.Plan()
}

func (f *replayFixture) build() {
	f.t.Helper()
	source, plan := f.discover()
	b := NewBuilder(source, plan, BuildOptions{
		DataDir: f.dataDir, Database: fixtureMergedDB, DoltBin: "dolt",
		Host: "127.0.0.1", Port: f.srcSrv.Port,
	})
	if _, err := b.Build(f.ctx); err != nil {
		f.t.Fatalf("build: %v", err)
	}
}

func (f *replayFixture) replay() (ReplayResult, error) {
	f.t.Helper()
	source, plan := f.discover()
	r := NewReplayer(source, plan, ReplayOptions{
		DataDir: f.dataDir, Database: fixtureMergedDB, DoltBin: "dolt",
		Host: "127.0.0.1", Port: f.srcSrv.Port,
	})
	return r.Replay(f.ctx)
}

// verify runs the verifier against the sources as they stand now and reports
// the failing checks.
func (f *replayFixture) verify(reference string) []Check {
	f.t.Helper()
	source, plan := f.discover()
	plans, err := TablePlansFor(f.ctx, source, plan, "")
	if err != nil {
		f.t.Fatal(err)
	}
	srv, err := StartIsolatedServer(f.ctx, "dolt", f.dataDir)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = srv.Stop() }()
	v, err := NewVerifier(f.ctx, source, plan, plans, VerifyOptions{
		Database: fixtureMergedDB, Host: "127.0.0.1", Port: srv.Port, Reference: reference,
	})
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

// mergedSQL runs statements directly against the merged database.
func (f *replayFixture) mergedSQL(stmts ...string) {
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
	for _, s := range stmts {
		rows, err := db.QueryContext(f.ctx, s)
		if err != nil {
			f.t.Fatalf("%s: %v", s, err)
		}
		for rows.Next() {
		}
		_ = rows.Close()
	}
}

func failuresText(fails []Check) string {
	var b strings.Builder
	for _, c := range fails {
		fmt.Fprintf(&b, "%s %s: %s\n", c.Scope, c.Table, c.Difference())
	}
	return b.String()
}

func requireNoFailures(t *testing.T, what string, fails []Check) {
	t.Helper()
	if len(fails) > 0 {
		t.Fatalf("%s: verification failed:\n%s", what, failuresText(fails))
	}
}

func requireFailures(t *testing.T, what string, fails []Check) {
	t.Helper()
	if len(fails) == 0 {
		t.Fatalf("%s: verification passed but should have failed", what)
	}
}

// TestReplayMakesLiveVerificationPass is the acceptance scenario: verification
// against the live sources passes after a build, fails once the sources move,
// passes again after a replay, and a single further edit fails it again until
// the next replay.
func TestReplayMakesLiveVerificationPass(t *testing.T) {
	f := newReplayFixture(t)
	f.build()
	requireNoFailures(t, "right after the build", f.verify(ReferenceLive))

	// New beads (with children), an edit, a closure, a deletion, a state
	// change, a wisp change, and a new bead that duplicates an id held by
	// another store (identical content, so the copies do not disagree).
	f.bead("alpha", "alp-6", "alpha new bead")
	f.execIn("alpha", "update issues set title='alpha bead 2 edited', updated_at=now() where id='alp-2'")
	f.execIn("alpha", "update issues set status='closed', updated_at=now() where id='alp-3'")
	f.execIn("alpha", "delete from comments where issue_id='alp-4'")
	f.execIn("alpha", "delete from labels where issue_id='alp-4'")
	f.execIn("alpha", "delete from issues where id='alp-4'")
	f.execIn("alpha", "delete from wisps where id='alp-wisp-1'")
	f.execIn("alpha", "insert into wisps values ('alp-wisp-2', 'second wisp')")
	f.commit("alpha", "writes after the build")
	f.bead("beta", "bet-6", "beta new bead")
	f.execIn("beta", "replace into config values ('added_later', 'yes')") // left uncommitted
	f.execIn("gamma", "insert into issues select * from alpha.issues where id='alp-1'")
	f.commit("gamma", "a copy of alp-1")

	requireFailures(t, "before the replay", f.verify(ReferenceLive))

	res, err := f.replay()
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	for _, id := range []string{"alp-1", "alp-2", "alp-3", "alp-4", "alp-6", "bet-6"} {
		if !contains(res.Beads, id) {
			t.Errorf("replay did not reconcile %s; reconciled %v", id, res.Beads)
		}
	}
	if !contains(res.CollisionsWritten, "alp-1") {
		t.Errorf("the new duplicate alp-1 was not recorded; wrote %v", res.CollisionsWritten)
	}
	requireNoFailures(t, "after the replay (live)", f.verify(ReferenceLive))
	requireNoFailures(t, "after the replay (recorded)", f.verify(ReferenceRecorded))

	// The proof is real: one more edit in one source fails it again.
	f.execIn("beta", "update issues set title='beta bead 2 edited later', updated_at=now() where id='bet-2'")
	requireFailures(t, "after one further edit", f.verify(ReferenceLive))
	if _, err := f.replay(); err != nil {
		t.Fatalf("second replay: %v", err)
	}
	requireNoFailures(t, "after the second replay", f.verify(ReferenceLive))

	// Deleting the duplicate removes its collision record again.
	f.execIn("gamma", "delete from issues where id='alp-1'")
	f.commit("gamma", "drop the copy")
	res, err = f.replay()
	if err != nil {
		t.Fatalf("third replay: %v", err)
	}
	if !contains(res.CollisionsRemoved, "alp-1") {
		t.Errorf("collision record for alp-1 was not removed; removed %v", res.CollisionsRemoved)
	}
	requireNoFailures(t, "after the duplicate went away", f.verify(ReferenceLive))
}

func TestReplayRefusals(t *testing.T) {
	t.Run("a merged database built without recorded commits", func(t *testing.T) {
		f := newReplayFixture(t)
		f.build()
		f.mergedSQL("drop table brain_unify_source_commits")
		_, err := f.replay()
		wantRefusal(t, err, "brain_unify_source_commits", "rebuild")
	})
	t.Run("history that no longer holds the recorded commit", func(t *testing.T) {
		f := newReplayFixture(t)
		f.build()
		f.mergedSQL("update brain_unify_source_commits set commit_hash='0123456789abcdefghijklmnopqrstuv' where store='beta'")
		_, err := f.replay()
		wantRefusal(t, err, "store beta", "no longer contains its recorded commit")
	})
	t.Run("a store that joined after the build", func(t *testing.T) {
		f := newReplayFixture(t)
		f.build()
		f.mergedSQL("delete from brain_unify_source_commits where store='gamma'")
		_, err := f.replay()
		wantRefusal(t, err, "store gamma", "no recorded commit")
	})
	t.Run("a store that left", func(t *testing.T) {
		f := newReplayFixture(t)
		f.build()
		f.reg.Stores = f.reg.Stores[:2]
		f.exec("drop database `gamma`")
		_, err := f.replay()
		wantRefusal(t, err, "store gamma", "no longer participates")
	})
	t.Run("a source table the build imported rows from is gone", func(t *testing.T) {
		f := newReplayFixture(t)
		f.build()
		f.execIn("beta", "drop table labels")
		f.commit("beta", "drop labels")
		_, err := f.replay()
		wantRefusal(t, err, "store beta", "labels")
	})
	t.Run("the merged schema lacks a column", func(t *testing.T) {
		f := newReplayFixture(t)
		f.build()
		f.mergedSQL("alter table comments drop column body")
		_, err := f.replay()
		wantRefusal(t, err, "comments", "column body")
	})
	t.Run("a merged database built before differing copies became conflict beads", func(t *testing.T) {
		f := newReplayFixture(t)
		f.build()
		f.mergedSQL("alter table brain_unify_collisions drop column copy_ids")
		_, err := f.replay()
		wantRefusal(t, err, "copy_ids", "rebuild")
	})
}

func wantRefusal(t *testing.T, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal, got none")
	}
	msg := err.Error()
	if !strings.Contains(msg, "refusing to replay") {
		t.Errorf("not a refusal: %v", err)
	}
	for _, p := range parts {
		if !strings.Contains(msg, p) {
			t.Errorf("refusal does not mention %q: %v", p, err)
		}
	}
}
