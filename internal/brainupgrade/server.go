package brainupgrade

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// Server is a Dolt sql-server this package starts over a scratch data
// directory to check a backup or rehearse the migration. It listens on a free
// loopback port and runs only until Stop. Nothing it does reaches outside its
// data directory.
type Server struct {
	DataDir string
	Bin     string
	Port    int

	cmd *exec.Cmd
	log *os.File
}

// StartServer starts a Dolt sql-server on dataDir and waits for it to answer.
func StartServer(ctx context.Context, bin, dataDir string) (*Server, error) {
	if bin == "" {
		bin = "dolt"
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("finding a free port: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	logPath := filepath.Join(filepath.Dir(dataDir), filepath.Base(dataDir)+"-server.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // scratch dir
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, "sql-server", "-H", "127.0.0.1", "-P", strconv.Itoa(port), //nolint:gosec // operator-supplied dolt binary
		"--loglevel=warning", "--data-dir", dataDir)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("starting dolt sql-server: %w", err)
	}
	s := &Server{DataDir: dataDir, Bin: bin, Port: port, cmd: cmd, log: logFile}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			_ = c.Close()
			return s, nil
		}
		if time.Now().After(deadline) {
			_ = s.Stop()
			return nil, fmt.Errorf("dolt sql-server on %s did not answer within 2m (see %s)", dataDir, logPath)
		}
		select {
		case <-ctx.Done():
			_ = s.Stop()
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Endpoint is the server's root login.
func (s *Server) Endpoint() Endpoint {
	return Endpoint{Host: "127.0.0.1", Port: s.Port, User: "root"}
}

// Stop shuts the server down the way launchd stops one (SIGTERM, so Dolt
// flushes), killing it if it has not exited within a minute. It is safe to
// call more than once.
func (s *Server) Stop() error {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(time.Minute):
		_ = s.cmd.Process.Kill()
		<-done
	}
	s.cmd = nil
	return s.log.Close()
}
