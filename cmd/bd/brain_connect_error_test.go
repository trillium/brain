package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/steveyegge/beads/internal/storage/dolt"
)

const (
	mini0Self  = "100.84.99.99"
	mini1Addr  = "100.102.238.78"
	storeHost  = "BEADS_DOLT_SERVER_HOST=%s\nBEADS_DOLT_SERVER_PORT=%d\n"
	wrapperPin = "#!/bin/sh\nexport BEADS_DOLT_SERVER_HOST=%s\nexport BEADS_DOLT_SERVER_PORT=3307\nexec bd \"$@\"\n"
)

func access1045(host string) error {
	return fmt.Errorf("failed to check if database %q exists on server %s:3307: %w", "beads_task", host,
		&mysql.MySQLError{Number: 1045, SQLState: [5]byte{'2', '8', '0', '0', '0'}, Message: "Access denied for user 'root'@'100.84.99.99' (using password: YES)"})
}

func connectErrFor(host string, err error) error {
	return &dolt.ServerConnectError{Host: host, Port: 3307, Err: err}
}

func mini0Home(t *testing.T, liveHost string) string {
	t.Helper()
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, ".config/brain/store-host.env"), fmt.Sprintf(storeHost, liveHost, 3307), 0o644)
	return home
}

func diagFor(t *testing.T, home, envHost string, tried string, f connectFailure) string {
	t.Helper()
	return explainStoreConnect(connectDiagInput{
		Home: home, EnvHost: envHost, Tried: hostAddr{Host: tried, Port: 3307},
		Failure: f, CredPath: filepath.Join(home, ".config/beads/credentials"),
	})
}

func wantAll(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("diagnosis missing %q:\n%s", w, got)
		}
	}
}

// The 2026-10-18 mini0 picture: store-host.env names mini0 itself, a wrapper
// pin makes `task` connect to mini1, and the server answers 1045.
func TestExplainPinOverridesStoreHost(t *testing.T) {
	home := mini0Home(t, mini0Self)
	mustWrite(t, filepath.Join(home, ".local/bin/task"), fmt.Sprintf(wrapperPin, mini1Addr), 0o755)

	got := diagFor(t, home, mini1Addr, mini1Addr, failAuth)

	wantAll(t, got,
		"tried: "+mini1Addr+":3307",
		"wrapper pin "+filepath.Join(home, ".local/bin/task"),
		"live:  "+mini0Self+":3307",
		"store-host.env "+filepath.Join(home, ".config/brain/store-host.env"),
		"fix:   brain stores rewrite-wrappers",
	)
}

// Loopback on mini0 got 1045 from a stale credentials entry while the live
// host is the machine's own address.
func TestExplainLoopbackStaleCredential(t *testing.T) {
	home := mini0Home(t, mini0Self)
	credPath := filepath.Join(home, ".config/beads/credentials")
	got := explainStoreConnect(connectDiagInput{
		Home: home, EnvHost: "", Tried: hostAddr{Host: "127.0.0.1", Port: 3307},
		Failure: failAuth, PasswordFrom: "file", CredPath: credPath,
	})
	wantAll(t, got,
		"tried: 127.0.0.1:3307",
		"built-in default",
		"live:  "+mini0Self+":3307",
		"a password is configured for 127.0.0.1:3307 in "+credPath+" (value not shown); it was rejected",
		"fix:   bd dolt set host "+mini0Self+" && bd dolt set port 3307",
	)

	// Same loopback address, and it IS the live host: the stale entry is the cause.
	home = mini0Home(t, "127.0.0.1")
	got = explainStoreConnect(connectDiagInput{
		Home: home, Tried: hostAddr{Host: "127.0.0.1", Port: 3307},
		Failure: failAuth, PasswordFrom: "file", CredPath: "/x/credentials",
	})
	wantAll(t, got, "same as tried", "credentials entry for it is stale",
		"fix:   update or remove the [127.0.0.1:3307] section in /x/credentials")
}

func TestExplainAuthFixNamesPasswordSource(t *testing.T) {
	home := mini0Home(t, mini0Self)
	for _, c := range []struct{ from, fix string }{
		{"env", "fix:   set BEADS_DOLT_PASSWORD to the password " + mini0Self + ":3307 accepts, or unset it to use [" + mini0Self + ":3307] in /x/credentials"},
		{"", "fix:   add a [" + mini0Self + ":3307] section with password=... to /x/credentials"},
	} {
		got := explainStoreConnect(connectDiagInput{
			Home: home, EnvHost: mini0Self, Tried: hostAddr{Host: mini0Self, Port: 3307},
			Failure: failAuth, PasswordFrom: c.from, CredPath: "/x/credentials",
		})
		wantAll(t, got, "rejected the login", c.fix)
	}
}

func TestExplainEnvFilePinNamesFile(t *testing.T) {
	home := mini0Home(t, mini0Self)
	envFile := filepath.Join(home, ".config/beads/task.env")
	mustWrite(t, envFile, "BEADS_DOLT_SERVER_HOST="+mini1Addr+"\n", 0o644)
	pins := []hostPin{{Source: "env-file", Path: envFile, Host: mini1Addr, Port: 3307, Line: "BEADS_DOLT_SERVER_HOST=" + mini1Addr}}
	in := connectDiagInput{Home: home, EnvHost: mini1Addr, Tried: hostAddr{Host: mini1Addr, Port: 3307}, Failure: failAuth}
	src := resolveHostSource(in, readStoreHostView(home), pins)
	if src.Kind != "env-file" || src.Path != envFile {
		t.Fatalf("env file pin not attributed: %+v", src)
	}
}

func TestExplainConnectionRefused(t *testing.T) {
	home := mini0Home(t, mini0Self)
	got := diagFor(t, home, mini0Self, mini0Self, failRefused)
	wantAll(t, got,
		"connection refused",
		"tried: "+mini0Self+":3307",
		"from store-host.env",
		"same as tried",
		"nothing is listening at that address",
		"fix:   brain stores doctor",
	)

	// A loopback server that is down is started locally.
	home = mini0Home(t, "127.0.0.1")
	got = diagFor(t, home, "", "127.0.0.1", failRefused)
	wantAll(t, got, "no local store server is answering", "fix:   bd dolt start")
}

func TestExplainEnvOverrideSuggestsUnset(t *testing.T) {
	home := mini0Home(t, mini0Self)
	got := diagFor(t, home, "10.9.9.9", "10.9.9.9", failTimeout)
	wantAll(t, got, "environment variable BEADS_DOLT_SERVER_HOST", "overrides store-host.env", "fix:   unset BEADS_DOLT_SERVER_HOST")
}

func TestExplainMissingStoreHostFile(t *testing.T) {
	got := diagFor(t, t.TempDir(), "", "10.9.9.9", failRefused)
	wantAll(t, got, "live:  unknown", "is missing", "fix:   brain stores doctor")
}

// The store's own "bd dolt start" hint is replaced by the diagnosis, so the
// message carries exactly one fix.
func TestAnnotateShowsExactlyOneFix(t *testing.T) {
	home := mini0Home(t, mini0Self)
	t.Setenv("HOME", home)
	t.Setenv("BEADS_DOLT_SERVER_HOST", mini0Self)
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	orig := &dolt.ServerConnectError{Host: mini0Self, Port: 3307,
		Err:  fmt.Errorf("Dolt server unreachable at %s:3307: %w", mini0Self, refused),
		Hint: "The Dolt server may not be running. Try:\n  bd dolt start"}
	if !strings.Contains(orig.Error(), "bd dolt start") {
		t.Fatalf("unannotated error must keep the store hint: %s", orig.Error())
	}
	msg := annotateStoreConnectError(orig, "").Error()
	if strings.Contains(msg, "bd dolt start") || strings.Count(msg, "fix:") != 1 {
		t.Fatalf("want exactly one fix line:\n%s", msg)
	}
	wantAll(t, msg, "Dolt server unreachable at "+mini0Self+":3307", "fix:   brain stores doctor")
}

func TestExplainNeverPrintsSecrets(t *testing.T) {
	home := mini0Home(t, mini0Self)
	t.Setenv("BEADS_DOLT_PASSWORD", "hunter2-secret")
	mustWrite(t, filepath.Join(home, ".config/beads/credentials"), "["+mini1Addr+":3307]\npassword=hunter2-secret\n", 0o600)
	mustWrite(t, filepath.Join(home, ".local/bin/task"), fmt.Sprintf(wrapperPin, mini1Addr), 0o755)
	got := explainStoreConnect(connectDiagInput{
		Home: home, EnvHost: mini1Addr, Tried: hostAddr{Host: mini1Addr, Port: 3307},
		Failure: failAuth, PasswordFrom: "env", CredPath: filepath.Join(home, ".config/beads/credentials"),
	})
	if strings.Contains(got, "hunter2") {
		t.Fatalf("diagnosis leaked a secret:\n%s", got)
	}
}

func TestAnnotateKeepsDriverTextAndAppendsDiagnosis(t *testing.T) {
	home := mini0Home(t, mini0Self)
	mustWrite(t, filepath.Join(home, ".local/bin/task"), fmt.Sprintf(wrapperPin, mini1Addr), 0o755)
	t.Setenv("HOME", home)
	t.Setenv("BEADS_DOLT_SERVER_HOST", mini1Addr)

	orig := connectErrFor(mini1Addr, access1045(mini1Addr))
	err := annotateStoreConnectError(orig, "")
	msg := err.Error()
	if !strings.HasPrefix(msg, orig.Error()) {
		t.Fatalf("driver text must come first and unchanged:\n%s", msg)
	}
	wantAll(t, msg, "Error 1045", "Access denied for user", "fix:   brain stores rewrite-wrappers")
	var me *mysql.MySQLError
	if !errors.As(err, &me) || me.Number != 1045 {
		t.Fatalf("annotated error must still unwrap to the MySQL error: %v", err)
	}
	// Idempotent.
	if again := annotateStoreConnectError(err, ""); again.Error() != msg {
		t.Fatalf("annotating twice changed the message")
	}
}

func TestAnnotatePassesThroughUnrelatedErrors(t *testing.T) {
	plain := errors.New("disk full")
	if got := annotateStoreConnectError(plain, ""); got != plain {
		t.Fatalf("plain error was altered: %v", got)
	}
	notFound := connectErrFor("127.0.0.1", errors.New(`database "x" not found on Dolt server`))
	if got := annotateStoreConnectError(notFound, ""); got != notFound {
		t.Fatalf("non-connection server error was altered: %v", got)
	}
	if annotateStoreConnectError(nil, "") != nil {
		t.Fatal("nil must stay nil")
	}
}

func TestClassifyConnectFailure(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	cases := []struct {
		name string
		err  error
		want connectFailure
	}{
		{"1045 typed", access1045("h"), failAuth},
		{"1045 text", errors.New("Error 1045 (28000): Access denied for user 'root'"), failAuth},
		{"refused typed", refused, failRefused},
		{"refused text", errors.New("dial tcp 1.2.3.4:3307: connect: connection refused"), failRefused},
		{"timeout text", errors.New("dial tcp 1.2.3.4:3307: i/o timeout"), failTimeout},
		{"dns", &net.DNSError{Err: "no such host", Name: "nope"}, failNoHost},
		{"other", errors.New("syntax error"), failNotSense},
	}
	for _, c := range cases {
		if got := classifyConnectFailure(c.err); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// TestExplainTranscript logs the full before/after message for the three
// cases the task names; run with -v to read it.
func TestExplainTranscript(t *testing.T) {
	home := mini0Home(t, mini0Self)
	mustWrite(t, filepath.Join(home, ".local/bin/task"), fmt.Sprintf(wrapperPin, mini1Addr), 0o755)
	t.Setenv("HOME", home)
	t.Setenv("BEADS_CREDENTIALS_FILE", filepath.Join(home, ".config/beads/credentials"))
	t.Setenv("BEADS_DOLT_PASSWORD", "")

	show := func(title, envHost string, orig error) {
		t.Setenv("BEADS_DOLT_SERVER_HOST", envHost)
		after := annotateStoreConnectError(orig, "")
		t.Logf("\n=== %s\n--- before:\n%s\n--- after:\n%s\n", title, orig.Error(), after.Error())
	}
	show("pin overrides store-host.env (mini0 -> mini1)", mini1Addr, connectErrFor(mini1Addr, access1045(mini1Addr)))
	show("loopback 1045, stale credentials", "", connectErrFor("127.0.0.1", access1045("127.0.0.1")))
	show("connection refused", mini0Self, &dolt.ServerConnectError{Host: mini0Self, Port: 3307,
		Err:  fmt.Errorf("Dolt server unreachable at %s:3307: %w", mini0Self, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}),
		Hint: "The Dolt server may not be running. Try:\n  bd dolt start"})
}
