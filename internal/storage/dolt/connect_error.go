package dolt

import (
	"net"
	"strconv"
)

// ServerConnectError marks a failure to reach or log in to a dolt sql-server,
// carrying the address the store actually tried. Error() is the wrapped
// error's text followed by Hint, so wrapping never changes what the driver
// said; the CLI uses the tried address to explain it (which host, where it
// came from, which host is live) and replaces Hint with its own fix command.
type ServerConnectError struct {
	Host   string // empty when Socket is set
	Port   int
	Socket string // unix socket path, when the connection was socket-based
	Err    error
	Hint   string // generic next step, shown when nothing better is known
}

func (e *ServerConnectError) Error() string {
	if e.Hint == "" {
		return e.Err.Error()
	}
	return e.Err.Error() + "\n\n" + e.Hint
}
func (e *ServerConnectError) Unwrap() error { return e.Err }

// Addr is the host:port (or socket path) that was tried.
func (e *ServerConnectError) Addr() string {
	if e.Socket != "" {
		return e.Socket
	}
	return net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
}

// connectErr tags err with the address cfg points at. A nil err stays nil.
func connectErr(cfg *Config, err error, hint string) error {
	if err == nil {
		return nil
	}
	return &ServerConnectError{Host: cfg.ServerHost, Port: cfg.ServerPort, Socket: cfg.ServerSocket, Err: err, Hint: hint}
}
