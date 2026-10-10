package brainunify

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

type recordingExecer struct{ stmts []string }

func (r *recordingExecer) ExecContext(_ context.Context, q string, _ ...any) (sql.Result, error) {
	r.stmts = append(r.stmts, q)
	return nil, nil
}

// "Keep what is already there" must not be spelled "insert ignore", which also
// swallows type errors.
func TestInsertRowsModeKeepExistingIsNotInsertIgnore(t *testing.T) {
	ex := &recordingExecer{}
	rows := [][]any{{"a-1", "x"}}
	if err := insertRowsMode(context.Background(), ex, "labels", []string{"issue_id", "label"}, rows, true); err != nil {
		t.Fatal(err)
	}
	if len(ex.stmts) != 1 {
		t.Fatalf("want one statement, got %v", ex.stmts)
	}
	got := ex.stmts[0]
	if strings.Contains(strings.ToLower(got), "insert ignore") {
		t.Fatalf("insert ignore coerces bad data instead of failing: %s", got)
	}
	if !strings.HasSuffix(got, "on duplicate key update `issue_id`=`issue_id`") {
		t.Fatalf("existing rows must be kept via a no-op duplicate-key update: %s", got)
	}

	ex.stmts = nil
	if err := insertRowsMode(context.Background(), ex, "labels", []string{"issue_id", "label"}, rows, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ex.stmts[0], "on duplicate key") {
		t.Fatalf("a plain insert must stay plain: %s", ex.stmts[0])
	}
}

// Against real Dolt: duplicates are kept as they are, and bad data errors.
func TestInsertRowsModeKeepExistingSurfacesBadData(t *testing.T) {
	f := newReplayFixture(t)
	f.exec("create database kx")
	f.execIn("kx", "create table t (k varchar(8) primary key, label varchar(8) not null)")
	f.execIn("kx", "insert into t values ('a','keep')")

	conn, err := f.src.Conn(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(f.ctx, "use kx"); err != nil {
		t.Fatal(err)
	}
	cols := []string{"k", "label"}
	if err := insertRowsMode(f.ctx, conn, "t", cols, [][]any{{"a", "other"}, {"b", "new"}}, true); err != nil {
		t.Fatalf("duplicates are not an error: %v", err)
	}
	var label string
	if err := conn.QueryRowContext(f.ctx, "select label from t where k='a'").Scan(&label); err != nil || label != "keep" {
		t.Fatalf("existing row must be left alone, got %q err=%v", label, err)
	}
	if err := insertRowsMode(f.ctx, conn, "t", cols, [][]any{{"c", nil}}, true); err == nil {
		t.Fatal("NULL in a NOT NULL column must be an error, not stored as ''")
	}
	if err := insertRowsMode(f.ctx, conn, "t", cols, [][]any{{"d", strings.Repeat("z", 20)}}, true); err == nil {
		t.Fatal("an over-length value must be an error, not truncated")
	}
}
