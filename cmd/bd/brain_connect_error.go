package main

// Store login/connect errors that say which server was tried, where that
// address came from, which host is live, and the exact fix command.
//
// Background (2026-10-18): mini0's store-host.env pointed at itself, but a
// wrapper pin made `task` connect to another machine and fail with a bare
// MySQL 1045. Nothing in the message named either server. The attribution and
// live-host lookups here are the fleet doctor's own (readStoreHostView,
// collectHostPins), not a second resolver. The original driver error text is
// always kept first; no password or credential value is ever printed.

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/go-sql-driver/mysql"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/dolt"
)

// connectFailure classes the store open errors worth explaining.
type connectFailure string

const (
	failAuth     connectFailure = "login refused (MySQL 1045 access denied)"
	failRefused  connectFailure = "connection refused"
	failTimeout  connectFailure = "connection timed out"
	failNoHost   connectFailure = "unknown host"
	failNotSense connectFailure = ""
)

// classifyConnectFailure reports which explainable failure err is, if any.
func classifyConnectFailure(err error) connectFailure {
	var me *mysql.MySQLError
	if errors.As(err, &me) && me.Number == 1045 {
		return failAuth
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return failNoHost
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return failRefused
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return failTimeout
	}
	// Errors that crossed a text boundary (wrapped with %v, driver strings).
	low := strings.ToLower(err.Error())
	switch {
	case strings.Contains(low, "error 1045") || strings.Contains(low, "access denied for user"):
		return failAuth
	case strings.Contains(low, "connection refused"):
		return failRefused
	case strings.Contains(low, "no such host"):
		return failNoHost
	case strings.Contains(low, "i/o timeout") || strings.Contains(low, "timed out") || strings.Contains(low, "deadline exceeded"):
		return failTimeout
	}
	return failNotSense
}

// connectDiagInput is everything the explanation depends on, so tests can
// supply it without touching the real home directory or process environment.
type connectDiagInput struct {
	Home     string // user home: store-host.env, ~/.local/bin wrappers
	EnvHost  string // BEADS_DOLT_SERVER_HOST as the process saw it
	EnvPort  string // BEADS_DOLT_SERVER_PORT
	BeadsDir string // store's .beads dir, for the metadata.json/config.yaml source
	Tried    hostAddr
	Failure  connectFailure
	// HasPassword reports whether any password is configured for the tried
	// address (BEADS_DOLT_PASSWORD or a credentials-file entry). Presence only.
	HasPassword bool
	CredPath    string
}

// hostSource says where the tried host came from.
type hostSource struct {
	Label string // "wrapper pin /path/to/task", "store-host.env", ...
	Kind  string // "pin" | "env-file" | "store-host" | "env" | "metadata" | "config" | "default"
}

// resolveHostSource attributes the tried host. A wrapper exports whatever it
// pins into BEADS_DOLT_SERVER_HOST, so an env value matching a pin is the pin's.
func resolveHostSource(in connectDiagInput, sh storeHostView, pins []hostPin) hostSource {
	host := in.Tried.Host
	shMatches := sh.Present && sh.Error == "" && sh.Host == host
	if in.EnvHost != "" && in.EnvHost == host {
		if !shMatches {
			for _, p := range pins {
				if p.Host == host && (p.Port == 0 || p.Port == in.Tried.Port) {
					if p.Source == "wrapper" {
						return hostSource{Kind: "pin", Label: "wrapper pin " + p.Path + ": " + p.Line}
					}
					return hostSource{Kind: "env-file", Label: "env file " + p.Path + ": " + p.Line}
				}
			}
			return hostSource{Kind: "env", Label: "environment variable BEADS_DOLT_SERVER_HOST"}
		}
		return hostSource{Kind: "store-host", Label: "store-host.env " + sh.Path}
	}
	if in.EnvHost == "" && in.BeadsDir != "" {
		if cfg, err := configfile.Load(in.BeadsDir); err == nil && cfg != nil && cfg.DoltServerHost == host {
			return hostSource{Kind: "metadata", Label: "metadata.json dolt_server_host in " + in.BeadsDir}
		}
	}
	if in.EnvHost == "" && config.GetYamlConfig("dolt.host") == host {
		return hostSource{Kind: "config", Label: "config.yaml dolt.host"}
	}
	if in.EnvHost == "" && host == configfile.DefaultDoltServerHost {
		return hostSource{Kind: "default", Label: "built-in default (no host configured)"}
	}
	return hostSource{Kind: "env", Label: "configuration (BEADS_DOLT_SERVER_HOST=" + in.EnvHost + ")"}
}

// explainStoreConnect builds the diagnosis block appended to the driver error.
func explainStoreConnect(in connectDiagInput) string {
	sh := readStoreHostView(in.Home)
	pins, _ := collectHostPins(in.Home)
	src := resolveHostSource(in, sh, pins)

	live := hostAddr{}
	liveKnown := sh.Present && sh.Error == ""
	if liveKnown {
		live = hostAddr{Host: sh.Host, Port: sh.Port}
	}
	differs := liveKnown && (live.Host != in.Tried.Host || live.Port != in.Tried.Port)

	var b strings.Builder
	fmt.Fprintf(&b, "brain: store server %s\n", in.Failure)
	fmt.Fprintf(&b, "  tried: %s  (from %s)\n", in.Tried, src.Label)
	switch {
	case differs:
		fmt.Fprintf(&b, "  live:  %s  (store-host.env %s)\n", live, sh.Path)
	case liveKnown:
		fmt.Fprintf(&b, "  live:  %s  (store-host.env %s; same as tried)\n", live, sh.Path)
	case !sh.Present:
		fmt.Fprintf(&b, "  live:  unknown, %s is missing\n", sh.Path)
	default:
		fmt.Fprintf(&b, "  live:  unknown, %s is unusable: %s\n", sh.Path, sh.Error)
	}

	fix := "brain stores doctor"
	switch {
	case differs && src.Kind == "pin":
		b.WriteString("  cause: the wrapper pins a different server than store-host.env names\n")
		fix = "brain stores rewrite-wrappers"
	case differs && src.Kind == "env":
		b.WriteString("  cause: BEADS_DOLT_SERVER_HOST in this environment overrides store-host.env\n")
		fix = "unset BEADS_DOLT_SERVER_HOST BEADS_DOLT_SERVER_PORT"
	case differs && src.Kind == "env-file":
		b.WriteString("  cause: an env file pins a different server than store-host.env names\n")
	case differs && in.Failure == failAuth && hostIsLoopback(in.Tried.Host):
		b.WriteString("  cause: this store connects to loopback while the live host is elsewhere; the loopback server rejected the login (a stale credentials entry for it is the usual reason)\n")
	case differs:
		b.WriteString("  cause: this store's own configuration points away from store-host.env\n")
	case in.Failure == failAuth && hostIsLoopback(in.Tried.Host):
		b.WriteString("  cause: a loopback address reached a server that rejected the login; the credentials entry for it is stale or that is not the store server\n")
	case in.Failure == failAuth:
		b.WriteString("  cause: the server is the expected one but rejected the login\n")
	case in.Failure == failRefused:
		b.WriteString("  cause: nothing is listening at that address\n")
	}
	if in.Failure == failAuth {
		key := in.Tried.String()
		switch {
		case in.HasPassword:
			fmt.Fprintf(&b, "  credentials: a password is configured for %s (value not shown); it was rejected\n", key)
		default:
			fmt.Fprintf(&b, "  credentials: no BEADS_DOLT_PASSWORD and no [%s] entry in %s\n", key, in.CredPath)
		}
	}
	fmt.Fprintf(&b, "  fix:   %s", fix)
	return b.String()
}

// storeConnectError is the original error plus its diagnosis.
type storeConnectError struct {
	err  error
	diag string
}

func (e *storeConnectError) Error() string { return e.err.Error() + "\n\n" + e.diag }
func (e *storeConnectError) Unwrap() error { return e.err }

// annotateStoreConnectError appends the diagnosis to a store open error. Errors
// that are not connection or login failures pass through untouched.
func annotateStoreConnectError(err error, beadsDir string) error {
	if err == nil {
		return nil
	}
	var ce *dolt.ServerConnectError
	if !errors.As(err, &ce) || ce.Socket != "" {
		return err
	}
	var already *storeConnectError
	if errors.As(err, &already) {
		return err
	}
	failure := classifyConnectFailure(err)
	if failure == failNotSense {
		return err
	}
	home, _ := os.UserHomeDir()
	credPath := os.Getenv("BEADS_CREDENTIALS_FILE")
	if credPath == "" {
		credPath = configfile.DefaultCredentialsPath()
	}
	hasPW := os.Getenv("BEADS_DOLT_PASSWORD") != "" || configfile.LookupCredentialsPassword(ce.Host, ce.Port) != ""
	in := connectDiagInput{
		Home:        home,
		EnvHost:     os.Getenv("BEADS_DOLT_SERVER_HOST"),
		EnvPort:     os.Getenv("BEADS_DOLT_SERVER_PORT"),
		BeadsDir:    beadsDir,
		Tried:       hostAddr{Host: ce.Host, Port: ce.Port},
		Failure:     failure,
		HasPassword: hasPW,
		CredPath:    credPath,
	}
	return &storeConnectError{err: err, diag: explainStoreConnect(in)}
}
