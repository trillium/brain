package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// openThrowawayStore launches a `dolt sql-server` on a fresh TempDir data dir
// and opens a migrated DoltStore on a unique database in it — no Docker
// needed, unlike the container-backed setupTestStore. The server is killed
// on test cleanup.
func openThrowawayStore(t *testing.T, ctx context.Context) *DoltStore {
	t.Helper()

	dir := t.TempDir()
	initCmd := exec.Command("dolt", "init", "--name", "test", "--email", "test@example.com")
	initCmd.Dir = dir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("dolt init throwaway dir: %v\n%s", err, out)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	serverLog, err := os.Create(filepath.Join(t.TempDir(), "dolt-server.log"))
	if err != nil {
		t.Fatalf("create server log: %v", err)
	}
	t.Cleanup(func() { _ = serverLog.Close() })
	srv := exec.Command("dolt", "sql-server", "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--data-dir", dir)
	srv.Stdout = serverLog
	srv.Stderr = serverLog
	if err := srv.Start(); err != nil {
		t.Fatalf("start dolt sql-server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Process.Kill() })

	baseCfg := mysql.Config{
		User:                 "root",
		Net:                  "tcp",
		Addr:                 "127.0.0.1:" + strconv.Itoa(port),
		ParseTime:            true,
		MultiStatements:      true,
		Timeout:              10 * time.Second,
		AllowNativePasswords: true,
		TLSConfig:            "false",
	}
	probe, err := sql.Open("mysql", baseCfg.FormatDSN())
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer probe.Close()
	deadline := time.Now().Add(30 * time.Second)
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := probe.PingContext(pingCtx)
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("throwaway server not ready: %v", err)
		}
	}

	dbName := uniqueTestDBName(t)
	if _, err := probe.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	store, err := New(ctx, &Config{
		Path:            dir,
		ServerHost:      "127.0.0.1",
		ServerPort:      port,
		Database:        dbName,
		MaxOpenConns:    1,
		CreateIfMissing: true,
	})
	if err != nil {
		t.Fatalf("open throwaway store: %v", err)
	}
	if err := store.SetConfig(ctx, "issue_prefix", "test"); err != nil {
		t.Fatalf("set issue_prefix: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func seedBatchIssues(t *testing.T, ctx context.Context, store *DoltStore, ids []string, priority int) {
	t.Helper()
	for _, id := range ids {
		iss := &types.Issue{ID: id, Title: id, Status: types.StatusOpen, Priority: priority, IssueType: types.TypeTask}
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
}

// TestBatchIssueIterMultiBatch pins R5: iteration is bounded per batch and
// correct across batch boundaries. Five issues with batch size 2 must yield
// all five exactly once, in filter order, with labels hydrated.
func TestBatchIssueIterMultiBatch(t *testing.T) {
	ctx := context.Background()
	store := openThrowawayStore(t, ctx)

	prios := map[string]int{"a": 3, "b": 1, "c": 4, "d": 0, "e": 2}
	for id, p := range prios {
		iss := &types.Issue{ID: "batch-" + id, Title: "batch " + id, Status: types.StatusOpen, Priority: p, IssueType: types.TypeTask}
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("create %s: %v", iss.ID, err)
		}
	}
	if err := store.AddLabel(ctx, "batch-a", "red", "tester"); err != nil {
		t.Fatalf("add label: %v", err)
	}
	if err := store.AddLabel(ctx, "batch-a", "blue", "tester"); err != nil {
		t.Fatalf("add label: %v", err)
	}

	it, err := store.IterIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		t.Fatalf("IterIssues: %v", err)
	}
	bi, ok := it.(*batchIssueIter)
	if !ok {
		t.Fatalf("IterIssues returned %T, want *batchIssueIter", it)
	}
	bi.size = 2
	defer it.Close()

	var got []string
	var labeled []string
	for it.Next(ctx) {
		iss := it.Value()
		if iss == nil {
			t.Fatal("nil Value during iteration")
		}
		got = append(got, iss.ID)
		if len(iss.Labels) > 0 {
			labeled = append(labeled, iss.ID)
		}
	}
	if err := it.Err(); err != nil {
		t.Fatalf("iter err: %v", err)
	}
	// Priority ASC: p0(batch-d), p1(batch-b), p2(batch-e), p3(batch-a), p4(batch-c).
	want := []string{"batch-d", "batch-b", "batch-e", "batch-a", "batch-c"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("yielded %v, want %v", got, want)
	}
	if len(labeled) != 1 || labeled[0] != "batch-a" {
		t.Fatalf("labeled = %v, want [batch-a]", labeled)
	}
}

// TestBatchIssueIterLimit verifies filter.Limit caps the total across batches
// and that early Close stops iteration.
func TestBatchIssueIterLimit(t *testing.T) {
	ctx := context.Background()
	store := openThrowawayStore(t, ctx)
	seedBatchIssues(t, ctx, store, []string{"lim-1", "lim-2", "lim-3", "lim-4"}, 1)

	it, err := store.IterIssues(ctx, "", types.IssueFilter{Limit: 3})
	if err != nil {
		t.Fatalf("IterIssues: %v", err)
	}
	it.(*batchIssueIter).size = 2
	defer it.Close()

	n := 0
	for it.Next(ctx) {
		n++
	}
	if err := it.Err(); err != nil {
		t.Fatalf("iter err: %v", err)
	}
	if n != 3 {
		t.Fatalf("yielded %d issues with Limit 3, want exactly 3", n)
	}

	it2, err := store.IterIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		t.Fatalf("IterIssues: %v", err)
	}
	if !it2.Next(ctx) {
		t.Fatal("expected first row")
	}
	if err := it2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if it2.Next(ctx) {
		t.Fatal("Next after Close should be false")
	}
	var _ storage.Iter[types.Issue] = it2
}
