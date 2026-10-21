// Package regtest gives tests a real, throwaway Dolt database to run the store
// registry against: an isolated sql-server under t.TempDir() on a free
// loopback port. Nothing here can reach a production server.
package regtest

import (
	"context"
	"database/sql"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/steveyegge/beads/internal/brainunify"
)

// Database is the name of the throwaway unified database.
const Database = "brain_unified"

// Server is a running throwaway Dolt server and its unified database.
type Server struct {
	srv *brainunify.IsolatedServer
	DB  *sql.DB
}

// New starts the server, or skips the test when dolt is unavailable (or in
// -short mode). The unified database carries the brain_stores and
// brain_store_prefixes tables a created store is recorded in.
func New(t *testing.T) *Server {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test starts a dolt server")
	}
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt binary not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	srv, err := brainunify.StartIsolatedServer(ctx, "dolt", filepath.Join(t.TempDir(), "dolt"))
	if err != nil {
		t.Fatalf("starting the throwaway dolt server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	root, err := srv.OpenTarget(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.ExecContext(ctx, "CREATE DATABASE "+Database); err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	db, err := srv.OpenTarget(ctx, Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{
		"CREATE TABLE brain_unified_config (`store` varchar(128) NOT NULL, `key` varchar(255) NOT NULL, value text NOT NULL, PRIMARY KEY (`store`,`key`))",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	for _, ddl := range brainunify.BrainTableDDL {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	return &Server{srv: srv, DB: db}
}

// Port is the loopback port the server listens on.
func (s *Server) Port() int { return s.srv.Port }

// Open opens one more connection to the unified database (a second "machine").
func (s *Server) Open(t *testing.T) *sql.DB {
	t.Helper()
	db, err := s.srv.OpenTarget(context.Background(), Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
