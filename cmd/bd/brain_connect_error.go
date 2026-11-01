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
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/go-sql-driver/mysql"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/doltserver"
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
	// PasswordFrom says which configured password the login used: "env"
	// (BEADS_DOLT_PASSWORD), "file" (a credentials-file entry) or "". Presence only.
	PasswordFrom string
	CredPath     string
	// AutoStartLog is the log of a server bd auto-started that still did not answer.
	AutoStartLog string
}

// hostSource says where the tried host (or port) came from.
type hostSource struct {
	Label string // "wrapper pin /path/to/task", "store-host.env", ...
	Kind  string // "pin" | "env-file" | "store-host" | "env" | "metadata" | "config" | "port-file" | "default" | ""
	Path  string // file holding the assignment
	Field string // variable or key holding the assignment
}

const (
	envHostVar = "BEADS_DOLT_SERVER_HOST"
	envPortVar = "BEADS_DOLT_SERVER_PORT"
)

// pinSource attributes an env value to the wrapper or env file that exports it.
func pinSource(pins []hostPin, match func(hostPin) bool, field string) (hostSource, bool) {
	for _, p := range pins {
		if match(p) {
			if p.Source == "wrapper" {
				return hostSource{Kind: "pin", Label: "wrapper pin " + p.Path + ": " + p.Line, Path: p.Path, Field: field}, true
			}
			return hostSource{Kind: "env-file", Label: "env file " + p.Path + ": " + p.Line, Path: p.Path, Field: field}, true
		}
	}
	return hostSource{}, false
}

// resolveHostSource attributes the tried host. A wrapper exports whatever it
// pins into BEADS_DOLT_SERVER_HOST, so an env value matching a pin is the pin's.
func resolveHostSource(in connectDiagInput, sh storeHostView, pins []hostPin) hostSource {
	host := in.Tried.Host
	shMatches := sh.Present && sh.Error == "" && sh.Host == host
	if in.EnvHost != "" && in.EnvHost == host {
		if !shMatches {
			if src, ok := pinSource(pins, func(p hostPin) bool {
				return p.Host == host && (p.Port == 0 || p.Port == in.Tried.Port)
			}, envHostVar); ok {
				return src
			}
			return hostSource{Kind: "env", Label: "environment variable " + envHostVar, Field: envHostVar}
		}
		return hostSource{Kind: "store-host", Label: "store-host.env " + sh.Path}
	}
	if in.EnvHost == "" && in.BeadsDir != "" {
		if cfg, err := configfile.Load(in.BeadsDir); err == nil && cfg != nil && cfg.DoltServerHost == host {
			path := filepath.Join(in.BeadsDir, configfile.ConfigFileName)
			return hostSource{Kind: "metadata", Label: "metadata.json dolt_server_host in " + path, Path: path, Field: "dolt_server_host"}
		}
	}
	if in.EnvHost == "" && config.GetYamlConfig("dolt.host") == host {
		path := yamlConfigPath()
		return hostSource{Kind: "config", Label: "config.yaml dolt.host in " + path, Path: path, Field: "dolt.host"}
	}
	if in.EnvHost == "" && host == configfile.DefaultDoltServerHost {
		return hostSource{Kind: "default", Label: "built-in default (no host configured)"}
	}
	return hostSource{Label: "configuration (" + envHostVar + "=" + in.EnvHost + ")"}
}

// resolvePortSource attributes the tried port, in the order the store resolves
// it: env, the .beads port file, config.yaml dolt.port, metadata.json.
func resolvePortSource(in connectDiagInput, pins []hostPin) hostSource {
	port := in.Tried.Port
	if in.EnvPort != "" {
		if in.EnvPort == strconv.Itoa(port) {
			if src, ok := pinSource(pins, func(p hostPin) bool {
				return p.Port == port && (p.Host == "" || p.Host == in.Tried.Host)
			}, envPortVar); ok {
				return src
			}
			return hostSource{Kind: "env", Label: "environment variable " + envPortVar, Field: envPortVar}
		}
		return hostSource{Label: "configuration (" + envPortVar + "=" + in.EnvPort + ")"}
	}
	if in.BeadsDir != "" {
		path := filepath.Join(in.BeadsDir, doltserver.PortFileName)
		// #nosec G304 -- fixed file name inside the store's .beads dir
		if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) == strconv.Itoa(port) {
			return hostSource{Kind: "port-file", Label: "port file " + path, Path: path}
		}
	}
	if config.GetYamlConfig("dolt.port") == strconv.Itoa(port) {
		path := yamlConfigPath()
		return hostSource{Kind: "config", Label: "config.yaml dolt.port in " + path, Path: path, Field: "dolt.port"}
	}
	if in.BeadsDir != "" {
		if cfg, err := configfile.Load(in.BeadsDir); err == nil && cfg != nil && cfg.DoltServerPort == port {
			path := filepath.Join(in.BeadsDir, configfile.ConfigFileName)
			return hostSource{Kind: "metadata", Label: "metadata.json dolt_server_port in " + path, Path: path, Field: "dolt_server_port"}
		}
	}
	return hostSource{Kind: "default", Label: "derived port (no port configured)"}
}

func yamlConfigPath() string {
	if p := config.ConfigFileUsed(); p != "" {
		return p
	}
	return "config.yaml"
}

// sourceFix is the single action for a source pointing away from
// store-host.env. It never writes a host or port anywhere: store-host.env stays
// the one record of the live server, and only the wrappers read it.
func sourceFix(src hostSource) string {
	switch src.Kind {
	case "pin":
		return "brain stores rewrite-wrappers"
	case "env":
		return "unset " + src.Field
	case "env-file":
		return "remove the " + src.Field + " line from " + src.Path
	case "metadata", "config", "port-file", "default":
		return "run bd through the store wrapper (brain store <name> ...); if the wrapper is missing or stale, brain stores rewrite-wrappers"
	}
	return "brain stores doctor"
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
	if differs && live.Host == in.Tried.Host {
		src = resolvePortSource(in, pins)
	}

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

	key := in.Tried.String()
	fix := "brain stores doctor"
	switch {
	case differs:
		switch src.Kind {
		case "pin":
			b.WriteString("  cause: the wrapper pins a different server than store-host.env names\n")
		case "env":
			fmt.Fprintf(&b, "  cause: %s in this environment overrides store-host.env\n", src.Field)
		case "env-file":
			b.WriteString("  cause: an env file pins a different server than store-host.env names\n")
		case "metadata", "config", "port-file", "default":
			b.WriteString("  cause: bd ran without the store wrapper, so store-host.env never applied and the address came from the source above\n")
		default:
			b.WriteString("  cause: the tried address does not match store-host.env\n")
		}
		if in.Failure == failAuth && hostIsLoopback(in.Tried.Host) {
			b.WriteString("  note:  the loopback server rejected the login (a stale credentials entry for it is the usual reason)\n")
		}
		fix = sourceFix(src)
	case in.Failure == failAuth && hostIsLoopback(in.Tried.Host):
		b.WriteString("  cause: a loopback address reached a server that rejected the login; the credentials entry for it is stale or that is not the store server\n")
		fix = credentialFix(in, key)
	case in.Failure == failAuth:
		b.WriteString("  cause: the server is the expected one but rejected the login\n")
		fix = credentialFix(in, key)
	case hostIsLoopback(in.Tried.Host) && in.AutoStartLog != "":
		b.WriteString("  cause: bd auto-started a local server but it still does not answer\n")
		fix = "tail -n 50 " + in.AutoStartLog
	case hostIsLoopback(in.Tried.Host):
		b.WriteString("  cause: no local store server is answering at that address\n")
		fix = "bd dolt start"
	case in.Failure == failRefused:
		b.WriteString("  cause: nothing is listening at that address\n")
	}
	if in.Failure == failAuth {
		switch in.PasswordFrom {
		case "env":
			fmt.Fprintf(&b, "  credentials: BEADS_DOLT_PASSWORD is set (value not shown); it was rejected by %s\n", key)
		case "file":
			fmt.Fprintf(&b, "  credentials: a password is configured for %s in %s (value not shown); it was rejected\n", key, in.CredPath)
		default:
			fmt.Fprintf(&b, "  credentials: no BEADS_DOLT_PASSWORD and no [%s] entry in %s\n", key, in.CredPath)
		}
	}
	fmt.Fprintf(&b, "  fix:   %s", fix)
	return b.String()
}

// credentialFix names the edit that replaces the rejected (or missing) password.
func credentialFix(in connectDiagInput, key string) string {
	switch in.PasswordFrom {
	case "env":
		return "set BEADS_DOLT_PASSWORD to the password " + key + " accepts, or unset it to use [" + key + "] in " + in.CredPath
	case "file":
		return "update or remove the [" + key + "] section in " + in.CredPath
	default:
		return "add a [" + key + "] section with password=... to " + in.CredPath
	}
}

// storeConnectError is the original error plus its diagnosis, which replaces
// the store's generic hint with the fix that matches the cause.
type storeConnectError struct {
	err  *dolt.ServerConnectError
	diag string
}

func (e *storeConnectError) Error() string { return e.err.Err.Error() + "\n\n" + e.diag }
func (e *storeConnectError) Unwrap() error { return e.err }

// annotateStoreConnectError appends the diagnosis to a store open error. Errors
// that are not connection or login failures pass through untouched.
func annotateStoreConnectError(err error, beadsDir string) error {
	ce, ok := err.(*dolt.ServerConnectError)
	if !ok || ce.Socket != "" {
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
	pwFrom := ""
	switch {
	case os.Getenv("BEADS_DOLT_PASSWORD") != "":
		pwFrom = "env"
	case configfile.LookupCredentialsPassword(ce.Host, ce.Port) != "":
		pwFrom = "file"
	}
	in := connectDiagInput{
		Home:         home,
		EnvHost:      os.Getenv("BEADS_DOLT_SERVER_HOST"),
		EnvPort:      os.Getenv("BEADS_DOLT_SERVER_PORT"),
		BeadsDir:     beadsDir,
		Tried:        hostAddr{Host: ce.Host, Port: ce.Port},
		Failure:      failure,
		PasswordFrom: pwFrom,
		CredPath:     credPath,
		AutoStartLog: ce.LogPath,
	}
	return &storeConnectError{err: ce, diag: explainStoreConnect(in)}
}
