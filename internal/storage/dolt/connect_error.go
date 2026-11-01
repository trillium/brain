package dolt

import (
	"net"
	"strconv"
)

// ServerConnectError marks a failure to reach or log in to a dolt sql-server,
// carrying the address the store actually tried. Error() is exactly the
// wrapped error's text, so wrapping never changes what the driver said; the
// CLI uses errors.As to find the tried address and explain it (which host,
// where it came from, which host is live, the fix command).
type ServerConnectError struct {
	Host   string // empty when Socket is set
	Port   int
	Socket string // unix socket path, when the connection was socket-based
	Err    error
}

func (e *ServerConnectError) Error() string { return e.Err.Error() }
func (e *ServerConnectError) Unwrap() error { return e.Err }

// Addr is the host:port (or socket path) that was tried.
func (e *ServerConnectError) Addr() string {
	if e.Socket != "" {
		return e.Socket
	}
	return net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
}

// connectErr tags err with the address cfg points at. A nil err stays nil.
func connectErr(cfg *Config, err error) error {
	if err == nil {
		return nil
	}
	return &ServerConnectError{Host: cfg.ServerHost, Port: cfg.ServerPort, Socket: cfg.ServerSocket, Err: err}
}
