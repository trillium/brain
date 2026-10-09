package brainunify

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// IsolatedServer is a Dolt sql-server started under a caller-supplied data
// directory, used to hold the unified database while it is built.
//
// Isolation is the point: the unified database is never created on the
// production server, so a build cannot write to production even if every
// other guard were removed. The production server is opened read-only
// elsewhere; this server is the only thing the builder writes through.
type IsolatedServer struct {
	// DataDir is the server's data directory.
	DataDir string
	// Port is the loopback port it listens on.
	Port int
	// Bin is the dolt binary used to start it.
	Bin string

	cmd     *exec.Cmd
	logFile *os.File
	stopped bool
}

// StartIsolatedServer starts a Dolt sql-server on a free loopback port with
// its own data directory. dir must not be the production data directory; the
// caller is responsible for passing a scratch path, and nothing here reaches
// outside dir.
func StartIsolatedServer(ctx context.Context, bin, dir string) (*IsolatedServer, error) {
	if bin == "" {
		bin = "dolt"
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("creating isolated dolt data dir %s: %w", dir, err)
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	logPath := filepath.Join(dir, "unified-server.log")
	logFile, err := os.Create(logPath) //nolint:gosec // caller-supplied scratch dir
	if err != nil {
		return nil, fmt.Errorf("creating isolated server log %s: %w", logPath, err)
	}
	cmd := exec.Command(bin, "sql-server", "-H", "127.0.0.1", "-P", strconv.Itoa(port),
		"--loglevel=error", "--data-dir", dir) //nolint:gosec // bin is operator-supplied
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("starting isolated dolt server: %w", err)
	}
	srv := &IsolatedServer{DataDir: dir, Port: port, Bin: bin, cmd: cmd, logFile: logFile}

	if err := waitForServer(ctx, port, 60*time.Second, logPath); err != nil {
		_ = srv.Stop()
		return nil, err
	}
	return srv, nil
}

// DSN is the connection string for the isolated server. maxAllowedPacket=0
// lifts the driver's own client-side limit: the batches are bounded by size in
// CopyRows, and the server is the only limit that should apply.
func (s *IsolatedServer) DSN(database string) string {
	return fmt.Sprintf("root@tcp(127.0.0.1:%d)/%s?parseTime=false&multiStatements=false&maxAllowedPacket=0",
		s.Port, database)
}

// OpenTarget opens a writable connection to the isolated server. Every write
// in the migration goes through a connection created here.
func (s *IsolatedServer) OpenTarget(ctx context.Context, database string) (*sql.DB, error) {
	db, err := sql.Open("mysql", s.DSN(database))
	if err != nil {
		return nil, fmt.Errorf("opening isolated server connection: %w", err)
	}
	db.SetMaxOpenConns(4)
	pingCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging isolated server: %w", err)
	}
	return db, nil
}

// Stop shuts the server down. It is safe to call more than once.
func (s *IsolatedServer) Stop() error {
	if s.stopped {
		return nil
	}
	s.stopped = true
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_, _ = s.cmd.Process.Wait()
	}
	if s.logFile != nil {
		return s.logFile.Close()
	}
	return nil
}

// freePort asks the kernel for an unused loopback port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("finding a free port for the isolated dolt server: %w", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitForServer polls until the isolated server answers a connection or the
// deadline passes. On failure it reports the tail of the server log, because
// "did not start" without the log is a useless failure.
func waitForServer(ctx context.Context, port int, timeout time.Duration, logPath string) error {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("isolated dolt server on port %d did not start within %s (see %s)", port, timeout, logPath)
}
