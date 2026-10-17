// Package main — brain_remote_doctor.go
//
// Client-side diagnosis for client mode: 'brain remote doctor' answers "why
// does this client not reach the brain host, or why is it quietly talking to
// the wrong thing?" with a handful of checks that each print a pass/warn/fail
// line and a concrete fix.
//
// The checks are deliberately narrow and read-only. Nothing here writes a
// config, rewrites a wrapper, or runs a store command that changes state: the
// doctor only reads the client's own HOME/config/wrappers and runs read-only
// commands on the host ('ssh true', 'brain version', '<store> info --json').
//
// Every outside-world dependency is behind remoteDoctorHost, and every local
// fact is a field on remoteDoctor, so the whole command is unit-testable with a
// fake host and a temp HOME (see brain_remote_doctor_test.go). No test in this
// package touches a real host, the real ssh, or the real ~/.config.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// Exit codes
// ---------------------------------------------------------------------------
//
// The failing checks are reported as a bitmask, so a script can tell *what* is
// broken instead of only that something is. The bits are OR'd together when
// more than one check fails (for example 10 = ssh unreachable + wrappers
// unhealthy); the whole space is 0-63.
//
//	0   every check passed (warnings allowed)
//	1   config: no host configured, remote.yaml unreadable/invalid, or HOME
//	    sanity failed (this client is reading the wrong home or config)
//	2   ssh: the configured host is not reachable
//	4   version: the client's and the host's brain binaries differ
//	8   wrappers: a store wrapper is missing, does not carry
//	    BRAIN_REMOTE_STORE, or resolves to another store's prefix
//	16  latency: a warm call took remoteDoctorLatencyFail or more
//	32  --strict was given and at least one check warned
const (
	remoteDoctorExitConfig   = 1
	remoteDoctorExitSSH      = 2
	remoteDoctorExitVersion  = 4
	remoteDoctorExitWrappers = 8
	remoteDoctorExitLatency  = 16
	remoteDoctorExitWarnings = 32
)

const (
	// remoteDoctorLatencyWarn is the warm-call round trip above which the
	// latency check warns. The shared connection's round trip is ~70 ms on a
	// Tailscale link (docs/brain/REMOTE.md); a second is already a bad link.
	remoteDoctorLatencyWarn = 1000 * time.Millisecond
	// remoteDoctorLatencyFail is the warm-call round trip above which the
	// latency check fails: at five seconds every forwarded command feels
	// broken even though it works.
	remoteDoctorLatencyFail = 5000 * time.Millisecond
	// remoteDoctorDefaultTimeout bounds one host operation, so one wedged
	// store cannot hang the whole doctor.
	remoteDoctorDefaultTimeout = 20 * time.Second
	// remoteDoctorDefaultJobs is how many stores are probed at once.
	remoteDoctorDefaultJobs = 4
)

// Check statuses. "skip" is not a failure: it means a prerequisite check
// failed, so this one could not run and saying "fail" twice would be noise.
const (
	doctorStatusPass = "pass"
	doctorStatusWarn = "warn"
	doctorStatusFail = "fail"
	doctorStatusSkip = "skip"
)

// remoteDoctorCheck is one diagnostic line.
type remoteDoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // pass | warn | fail | skip
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
	Millis int64  `json:"ms"`
}

// doctorProblem is one finding inside a multi-finding check (wrappers).
type doctorProblem struct {
	Status string
	Text   string
	Fix    string
}

// remoteDoctorHost is the host-facing surface the client doctor needs. The
// default implementation runs ssh; tests inject a fake, so no check in the test
// suite ever reaches a real machine.
type remoteDoctorHost interface {
	// Reachable runs a trivial command over ssh and reports the round trip.
	Reachable(ctx context.Context) (time.Duration, error)
	// Run runs name and args on the host through the host's own wrapper and
	// returns its stdout.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	// Stores returns the host registry's store names and the subset of them
	// that has no wrapper in the host's bin dir.
	Stores(ctx context.Context) (names, missing []string, err error)
}

// sshDoctorHost is the real host: every method is one ssh call.
type sshDoctorHost struct{ cfg *remoteConfig }

func (h sshDoctorHost) Reachable(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	_, err := remoteShell(ctx, h.cfg, "true")
	return time.Since(start), err
}

func (h sshDoctorHost) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return remoteCapture(ctx, h.cfg, name, args...)
}

func (h sshDoctorHost) Stores(ctx context.Context) ([]string, []string, error) {
	return hostStoreNames(ctx, h.cfg)
}

// remoteDoctor is one run of the client doctor. Everything it reads about this
// machine is a field, so tests can point it at a temp HOME and a fake host.
type remoteDoctor struct {
	host         remoteDoctorHost
	cfg          *remoteConfig
	cfgErr       error
	home         string // $HOME as this process sees it
	accountHome  string // this account's home from the OS user database ("" if unknown)
	username     string
	configPath   string // the client config this process would read
	binDir       string // where this client's store wrappers live
	localVersion string // combinedVersion() of this binary
	jobs         int
	timeout      time.Duration
}

// newRemoteDoctor assembles the real client doctor: local facts from the
// process environment, host facts over ssh.
func newRemoteDoctor(cfg *remoteConfig, cfgErr error) *remoteDoctor {
	d := &remoteDoctor{
		cfg:          cfg,
		cfgErr:       cfgErr,
		home:         os.Getenv("HOME"),
		configPath:   remoteConfigFile(),
		localVersion: combinedVersion(),
		jobs:         remoteDoctorDefaultJobs,
		timeout:      remoteDoctorDefaultTimeout,
	}
	if d.home == "" {
		if home, err := os.UserHomeDir(); err == nil {
			d.home = home
		}
	}
	if u, err := user.Current(); err == nil {
		d.accountHome = u.HomeDir
		d.username = u.Username
		if i := strings.LastIndexAny(d.username, `\/`); i >= 0 {
			d.username = d.username[i+1:]
		}
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		d.binDir = filepath.Dir(exe)
	}
	if cfg != nil && cfg.Host != "" {
		d.host = sshDoctorHost{cfg: cfg}
	}
	return d
}

// run executes every check in display order. The three host-dependent checks
// are gated on the ssh check: when the host is unreachable they are reported as
// skipped, so a single cause produces one FAIL line instead of four.
func (d *remoteDoctor) run(ctx context.Context) []remoteDoctorCheck {
	config := d.checkConfig()
	ssh := d.checkSSH(ctx)
	reachable := ssh.Status == doctorStatusPass
	return []remoteDoctorCheck{
		config,
		ssh,
		d.checkVersion(ctx, reachable),
		d.checkWrappers(ctx, reachable),
		d.checkLatency(ctx, reachable),
	}
}

// hostReady reports whether the checks that need the host can run.
func (d *remoteDoctor) hostReady() bool {
	return d.host != nil && d.cfg != nil && d.cfg.Host != ""
}

// hostCtx bounds one host operation without ending the whole doctor.
func (d *remoteDoctor) hostCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if d.timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d.timeout)
}

// hostBinDir is the host's wrapper directory, for fix hints.
func (d *remoteDoctor) hostBinDir() string {
	if d.cfg == nil {
		return "~/.local/bin"
	}
	return d.cfg.binDir()
}

// ---------------------------------------------------------------------------
// config / HOME sanity (local only)
// ---------------------------------------------------------------------------

// checkConfig is the check for the class of bug where a client reads another
// user's (or the host's) home instead of its own: $HOME must be the home the OS
// user database gives this account, the config path must be inside it, and the
// file must be owned by this user. A wrong HOME silently changes which
// remote.yaml and which wrappers the whole client mode reads.
func (d *remoteDoctor) checkConfig() remoteDoctorCheck {
	if d.cfgErr != nil {
		return failedCheck("config", d.cfgErr.Error(), "fix or remove "+d.configPath)
	}
	if d.cfg == nil || strings.TrimSpace(d.cfg.Host) == "" {
		return failedCheck("config", "no brain host configured; this machine is not a client",
			"brain remote set <ssh-target>")
	}
	if err := d.cfg.validate(); err != nil {
		return failedCheck("config", err.Error(),
			"edit "+d.configPath+" or re-run 'brain remote set <ssh-target>'")
	}

	var problems []doctorProblem
	switch {
	case d.home == "":
		problems = append(problems, doctorProblem{doctorStatusFail,
			"HOME is not set, so the client config cannot be located",
			"export HOME=<your home directory>"})
	case !filepath.IsAbs(d.home):
		problems = append(problems, doctorProblem{doctorStatusFail,
			fmt.Sprintf("HOME is %q, which is not an absolute path", d.home),
			"export HOME=<your home directory>"})
	default:
		if fi, err := os.Stat(d.home); err != nil || !fi.IsDir() {
			problems = append(problems, doctorProblem{doctorStatusFail,
				fmt.Sprintf("HOME %s is not a readable directory", d.home),
				"export HOME=<your home directory>"})
		}
		if d.accountHome != "" && filepath.Clean(d.home) != filepath.Clean(d.accountHome) {
			problems = append(problems, doctorProblem{doctorStatusFail,
				fmt.Sprintf("HOME is %s but this account's home is %s — something exported the wrong HOME (a shell profile, a wrapper, or an environment copied from the host), so brain is reading the wrong .config",
					d.home, d.accountHome),
				"unset the stray HOME export (or set HOME=" + d.accountHome + ") and run brain again"})
		}
	}

	if d.configPath == "" {
		problems = append(problems, doctorProblem{doctorStatusFail,
			"cannot resolve the client config path without a HOME",
			"export HOME=<your home directory>"})
	} else {
		if d.home != "" {
			home := filepath.Clean(d.home)
			if cfgPath := filepath.Clean(d.configPath); cfgPath != home &&
				!strings.HasPrefix(cfgPath, home+string(filepath.Separator)) {
				problems = append(problems, doctorProblem{doctorStatusFail,
					fmt.Sprintf("config path %s is outside HOME %s — this client is reading a config that does not belong to this account",
						d.configPath, d.home),
					"check HOME, then 'brain remote set <ssh-target>' to write your own config"})
			}
		}
		if fi, err := os.Stat(d.configPath); err == nil {
			if uid := fileOwnerUID(fi); uid >= 0 && uid != os.Getuid() {
				problems = append(problems, doctorProblem{doctorStatusFail,
					fmt.Sprintf("%s is owned by uid %d, not by you (uid %d) — you are reading another user's config",
						d.configPath, uid, os.Getuid()),
					"remove that file and re-run 'brain remote set <ssh-target>' as yourself"})
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			problems = append(problems, doctorProblem{doctorStatusFail,
				fmt.Sprintf("cannot read %s: %v", d.configPath, err),
				"check the permissions on " + filepath.Dir(d.configPath)})
		}
	}

	// An environment override is a legitimate one-shot, but a stale one in a
	// shell profile makes the file above a lie; say so.
	if h := strings.TrimSpace(os.Getenv(envRemoteHost)); h != "" {
		problems = append(problems, doctorProblem{doctorStatusWarn,
			fmt.Sprintf("host %s comes from $%s, not from %s", h, envRemoteHost, d.configPath),
			"unset $" + envRemoteHost + " to use the config file"})
	}

	return assembleCheck("config", 0, problems,
		fmt.Sprintf("%s (HOME %s, host %s)", d.configPath, d.home, d.cfg.Host))
}

// ---------------------------------------------------------------------------
// ssh reachability
// ---------------------------------------------------------------------------

func (d *remoteDoctor) checkSSH(ctx context.Context) remoteDoctorCheck {
	if !d.hostReady() {
		return skippedCheck("ssh", "no host configured")
	}
	callCtx, cancel := d.hostCtx(ctx)
	defer cancel()
	dur, err := d.host.Reachable(callCtx)
	ms := dur.Milliseconds()
	if err != nil {
		return remoteDoctorCheck{Name: "ssh", Status: doctorStatusFail, Millis: ms,
			Detail: firstLine([]byte(err.Error())),
			Fix: fmt.Sprintf("test the link: ssh -o BatchMode=yes %s true  (host awake? Tailscale up? key login allowed?)",
				d.cfg.Host)}
	}
	return remoteDoctorCheck{Name: "ssh", Status: doctorStatusPass, Millis: ms,
		Detail: fmt.Sprintf("%s reachable (ssh true, %dms)", d.cfg.Host, ms)}
}

// ---------------------------------------------------------------------------
// version match
// ---------------------------------------------------------------------------

// checkVersion compares this binary's combined version token with the host's.
// A drifted pair is the quiet failure mode: the client parses what the host
// prints, so a mismatch shows up as confusing output much later.
func (d *remoteDoctor) checkVersion(ctx context.Context, reachable bool) remoteDoctorCheck {
	if !d.hostReady() {
		return skippedCheck("version", "no host configured")
	}
	if !reachable {
		return skippedCheck("version", "host not reachable")
	}
	callCtx, cancel := d.hostCtx(ctx)
	defer cancel()
	start := time.Now()
	out, err := d.host.Run(callCtx, "brain", "version", "--combined")
	if err != nil {
		// An older host may not know --combined; the human line carries the
		// same token.
		if out, err = d.host.Run(callCtx, "brain", "version"); err != nil {
			return remoteDoctorCheck{Name: "version", Status: doctorStatusFail,
				Millis: time.Since(start).Milliseconds(),
				Detail: fmt.Sprintf("asking the host for its version: %s", firstLine([]byte(err.Error()))),
				Fix:    fmt.Sprintf("the host needs a working brain binary: check %s/brain on %s", d.hostBinDir(), d.cfg.Host)}
		}
	}
	ms := time.Since(start).Milliseconds()
	got := parseHostVersion(out)
	switch {
	case got == "":
		return remoteDoctorCheck{Name: "version", Status: doctorStatusFail, Millis: ms,
			Detail: fmt.Sprintf("the host printed no version: %q", strings.TrimSpace(string(out))),
			Fix:    fmt.Sprintf("check %s/brain on %s ('brain --version' must work there)", d.hostBinDir(), d.cfg.Host)}
	case got == d.localVersion:
		return remoteDoctorCheck{Name: "version", Status: doctorStatusPass, Millis: ms,
			Detail: fmt.Sprintf("client and host both %s", got)}
	default:
		return remoteDoctorCheck{Name: "version", Status: doctorStatusFail, Millis: ms,
			Detail: fmt.Sprintf("client %s, host %s", d.localVersion, got),
			Fix:    fmt.Sprintf("rebuild and reinstall the older side: make build && cp bd %s/brain on %s", d.hostBinDir(), d.cfg.Host)}
	}
}

// parseHostVersion pulls the combined version token out of the host's output,
// accepting both 'version --combined' (a bare token) and 'version' (the human
// "bd version <token> (<build>)" line).
func parseHostVersion(out []byte) string {
	line := firstLine(out)
	if line == "" {
		return ""
	}
	if f := strings.Fields(line); len(f) >= 3 && f[0] == "bd" && f[1] == "version" {
		return f[2]
	}
	return line
}

// ---------------------------------------------------------------------------
// wrapper health
// ---------------------------------------------------------------------------

// clientWrapperStoreRE finds the store name a client wrapper pins. Quoted and
// bare forms are both accepted, and the value stops at whitespace or ';'.
var clientWrapperStoreRE = regexp.MustCompile(`\bBRAIN_REMOTE_STORE=('([^']*)'|"([^"]*)"|([^\s;]+))`)

// clientWrapperStore returns the BRAIN_REMOTE_STORE value a wrapper pins.
func clientWrapperStore(body string) (string, bool) {
	m := clientWrapperStoreRE.FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	for _, v := range m[2:] {
		if v != "" {
			return v, true
		}
	}
	return "", false
}

// storePrefixFromInfo extracts config.issue_prefix from 'bd info --json'. The
// command may print notices before the JSON object, so parsing starts at the
// first '{'.
func storePrefixFromInfo(data []byte) (string, bool) {
	start := bytes.IndexByte(data, '{')
	if start < 0 {
		return "", false
	}
	var payload struct {
		Config map[string]string `json:"config"`
	}
	if err := json.NewDecoder(bytes.NewReader(data[start:])).Decode(&payload); err != nil {
		return "", false
	}
	prefix := payload.Config["issue_prefix"]
	return prefix, prefix != ""
}

// checkWrappers asserts that every store the host offers has a client wrapper
// that carries its own BRAIN_REMOTE_STORE, and that each store on the host
// reports its own prefix. A wrapper pinned to the wrong store, or a store
// resolving to another store's database, is invisible until an id comes back
// with the wrong prefix — which is why it is asserted here.
func (d *remoteDoctor) checkWrappers(ctx context.Context, reachable bool) remoteDoctorCheck {
	if !d.hostReady() {
		return skippedCheck("wrappers", "no host configured")
	}
	if !reachable {
		return skippedCheck("wrappers", "host not reachable")
	}
	callCtx, cancel := d.hostCtx(ctx)
	defer cancel()
	start := time.Now()
	names, missing, err := d.host.Stores(callCtx)
	if err != nil {
		return remoteDoctorCheck{Name: "wrappers", Status: doctorStatusFail,
			Millis: time.Since(start).Milliseconds(),
			Detail: fmt.Sprintf("reading the host's store registry: %s", firstLine([]byte(err.Error()))),
			Fix:    fmt.Sprintf("on %s: brain stores list --json (the host's brain binary must work)", d.cfg.Host)}
	}
	problems := d.clientWrapperProblems(names, missing)
	problems = append(problems, d.hostPrefixProblems(ctx, names)...)
	ms := time.Since(start).Milliseconds()
	ok := fmt.Sprintf("%d store wrappers carry %s and every store reports its own prefix",
		len(names), envRemoteStore)
	return assembleCheck("wrappers", ms, problems, ok)
}

// clientWrapperProblems checks this client's wrapper files: each host store
// must have one, and it must pin its own store name.
func (d *remoteDoctor) clientWrapperProblems(names, missing []string) []doctorProblem {
	var problems []doctorProblem
	for _, name := range missing {
		problems = append(problems, doctorProblem{doctorStatusFail,
			fmt.Sprintf("the host's registry lists %q but the host has no %s wrapper — that store is unreachable from every client",
				name, name),
			fmt.Sprintf("on the host: re-create it with 'brain stores add %s <beads-dir>' (or 'brain stores create %s')",
				name, name)})
	}
	if len(names) == 0 && len(missing) == 0 {
		problems = append(problems, doctorProblem{doctorStatusWarn,
			"the host's registry has no stores, so there is nothing to forward",
			"on the host: brain stores create <name>"})
	}
	if d.binDir == "" {
		problems = append(problems, doctorProblem{doctorStatusWarn,
			"cannot locate the directory this brain binary runs from, so client wrappers were not checked",
			"re-run with --dir <wrapper-dir>"})
		return problems
	}
	for _, name := range names {
		path := filepath.Join(d.binDir, name)
		body, err := os.ReadFile(path) //nolint:gosec // the client's own wrapper directory
		if err != nil {
			problems = append(problems, doctorProblem{doctorStatusWarn,
				fmt.Sprintf("no client wrapper for store %q at %s", name, path),
				"brain remote install"})
			continue
		}
		got, ok := clientWrapperStore(string(body))
		switch {
		case !ok:
			problems = append(problems, doctorProblem{doctorStatusFail,
				fmt.Sprintf("%s does not set %s — it would run against a local database instead of the host",
					path, envRemoteStore),
				"brain remote install  (or add " + envRemoteStore + "=" + name + " to that wrapper)"})
		case got != name:
			problems = append(problems, doctorProblem{doctorStatusFail,
				fmt.Sprintf("%s sets %s=%s but is named %s — it would run store %s on the host",
					path, envRemoteStore, got, name, got),
				"brain remote install"})
		}
	}
	for _, stale := range d.staleClientWrappers(names) {
		problems = append(problems, doctorProblem{doctorStatusWarn,
			fmt.Sprintf("%s is a generated wrapper for %q, which the host no longer has",
				stale, filepath.Base(stale)),
			"brain remote install --prune"})
	}
	return problems
}

// staleClientWrappers returns generated wrappers in the client bin dir whose
// store the host no longer offers.
func (d *remoteDoctor) staleClientWrappers(names []string) []string {
	if d.binDir == "" {
		return nil
	}
	keep := make(map[string]bool, len(names))
	for _, n := range names {
		keep[n] = true
	}
	entries, err := os.ReadDir(d.binDir)
	if err != nil {
		return nil
	}
	var stale []string
	for _, e := range entries {
		if e.IsDir() || keep[e.Name()] {
			continue
		}
		path := filepath.Join(d.binDir, e.Name())
		if isRemoteWrapper(path) {
			stale = append(stale, path)
		}
	}
	return stale
}

// hostPrefixProblems probes each store through its own wrapper and checks the
// prefix it reports. Probing is concurrent: each probe opens a database on the
// host, so a wide federation would otherwise serialize into a slow doctor.
func (d *remoteDoctor) hostPrefixProblems(ctx context.Context, names []string) []doctorProblem {
	if len(names) == 0 {
		return nil
	}
	jobs := d.jobs
	if jobs < 1 {
		jobs = 1
	}
	found := make([][]doctorProblem, len(names))
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			callCtx, cancel := d.hostCtx(ctx)
			defer cancel()
			found[i] = d.hostPrefixProblem(callCtx, name, names)
		}(i, name)
	}
	wg.Wait()
	var out []doctorProblem
	for _, p := range found {
		out = append(out, p...)
	}
	return out
}

// hostPrefixProblem probes one store. A prefix equal to the store's own name is
// what 'brain stores create' seeds; a prefix belonging to a *different* store
// means the wrapper points at the wrong database (a failure), and any other
// mismatch is worth a warning rather than a verdict.
func (d *remoteDoctor) hostPrefixProblem(ctx context.Context, name string, all []string) []doctorProblem {
	out, err := d.host.Run(ctx, name, "info", "--json")
	if err != nil {
		return []doctorProblem{{doctorStatusFail,
			fmt.Sprintf("%s: the host's %s wrapper does not answer (%s)", name, name, firstLine([]byte(err.Error()))),
			fmt.Sprintf("on the host: brain stores doctor  # then fix %s", name)}}
	}
	prefix, ok := storePrefixFromInfo(out)
	if !ok {
		return []doctorProblem{{doctorStatusWarn,
			fmt.Sprintf("%s: the store reports no issue_prefix, so it cannot mint ids", name),
			fmt.Sprintf("on the host: check %s's store config ('brain stores create' seeds issue_prefix)", name)}}
	}
	if prefix == name {
		return nil
	}
	for _, other := range all {
		if other != name && prefix == other {
			return []doctorProblem{{doctorStatusFail,
				fmt.Sprintf("%s: resolves to prefix %q, which is store %q's prefix — this wrapper points at another store's database",
					name, prefix, other),
				fmt.Sprintf("on the host: check BEADS_DIR in %s/%s, then run 'brain stores rewrite-wrappers'",
					d.hostBinDir(), name)}}
		}
	}
	return []doctorProblem{{doctorStatusWarn,
		fmt.Sprintf("%s: reports prefix %q, not its own name", name, prefix),
		fmt.Sprintf("on the host: check BEADS_DIR in %s/%s if that is unexpected", d.hostBinDir(), name)}}
}

// ---------------------------------------------------------------------------
// latency
// ---------------------------------------------------------------------------

// checkLatency measures a warm call: the shared multiplexed connection means a
// forwarded command costs one round trip plus the host's own work, so this is
// the floor every store command pays.
func (d *remoteDoctor) checkLatency(ctx context.Context, reachable bool) remoteDoctorCheck {
	if !d.hostReady() {
		return skippedCheck("latency", "no host configured")
	}
	if !reachable {
		return skippedCheck("latency", "host not reachable")
	}
	callCtx, cancel := d.hostCtx(ctx)
	defer cancel()
	dur, err := d.host.Reachable(callCtx)
	ms := dur.Milliseconds()
	fix := "check the network path (Tailscale? host busy?); 'brain remote disconnect' drops a stale multiplexed connection and the next call reconnects"
	switch {
	case err != nil:
		return remoteDoctorCheck{Name: "latency", Status: doctorStatusFail, Millis: ms,
			Detail: fmt.Sprintf("the warm call failed: %s", firstLine([]byte(err.Error()))),
			Fix:    fmt.Sprintf("test the link again: ssh -o BatchMode=yes %s true", d.cfg.Host)}
	case dur >= remoteDoctorLatencyFail:
		return remoteDoctorCheck{Name: "latency", Status: doctorStatusFail, Millis: ms,
			Detail: fmt.Sprintf("a warm call took %dms (>= %s): every forwarded command pays this", ms, remoteDoctorLatencyFail),
			Fix:    fix}
	case dur >= remoteDoctorLatencyWarn:
		return remoteDoctorCheck{Name: "latency", Status: doctorStatusWarn, Millis: ms,
			Detail: fmt.Sprintf("a warm call took %dms (>= %s)", ms, remoteDoctorLatencyWarn),
			Fix:    fix}
	default:
		return remoteDoctorCheck{Name: "latency", Status: doctorStatusPass, Millis: ms,
			Detail: fmt.Sprintf("%dms per warm call on the shared ssh connection", ms)}
	}
}

// ---------------------------------------------------------------------------
// reporting
// ---------------------------------------------------------------------------

// remoteDoctorReport is the --json shape and the source of the text lines.
type remoteDoctorReport struct {
	Host     string              `json:"host,omitempty"`
	Config   string              `json:"config"`
	Home     string              `json:"home"`
	BinDir   string              `json:"bin_dir,omitempty"`
	Checks   []remoteDoctorCheck `json:"checks"`
	Passed   int                 `json:"passed"`
	Warnings int                 `json:"warnings"`
	Failed   int                 `json:"failed"`
	Skipped  int                 `json:"skipped"`
	ExitCode int                 `json:"exit_code"`
	OK       bool                `json:"ok"`
}

// remoteDoctorExitCode maps the checks to the documented bitmask.
func remoteDoctorExitCode(checks []remoteDoctorCheck, strict bool) int {
	code := 0
	for _, c := range checks {
		switch c.Status {
		case doctorStatusFail:
			switch c.Name {
			case "config":
				code |= remoteDoctorExitConfig
			case "ssh":
				code |= remoteDoctorExitSSH
			case "version":
				code |= remoteDoctorExitVersion
			case "wrappers":
				code |= remoteDoctorExitWrappers
			case "latency":
				code |= remoteDoctorExitLatency
			}
		case doctorStatusWarn:
			if strict {
				code |= remoteDoctorExitWarnings
			}
		}
	}
	return code
}

func remoteDoctorReportFor(d *remoteDoctor, checks []remoteDoctorCheck, code int) remoteDoctorReport {
	r := remoteDoctorReport{
		Config: d.configPath, Home: d.home, BinDir: d.binDir,
		Checks: checks, ExitCode: code, OK: code == 0,
	}
	if d.cfg != nil {
		r.Host = d.cfg.Host
	}
	for _, c := range checks {
		switch c.Status {
		case doctorStatusPass:
			r.Passed++
		case doctorStatusWarn:
			r.Warnings++
		case doctorStatusFail:
			r.Failed++
		case doctorStatusSkip:
			r.Skipped++
		}
	}
	return r
}

// emitRemoteDoctor writes the report as JSON or as the human pass/warn/fail
// lines. The human report goes to stdout: it is the command's output, not a
// diagnostic aside.
func emitRemoteDoctor(w io.Writer, asJSON bool, r remoteDoctorReport) {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(r)
		return
	}
	width := 0
	for _, c := range r.Checks {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	for _, c := range r.Checks {
		label := map[string]string{
			doctorStatusPass: "PASS", doctorStatusWarn: "WARN",
			doctorStatusFail: "FAIL", doctorStatusSkip: "skip",
		}[c.Status]
		line := fmt.Sprintf("%s  %-*s", label, width, c.Name)
		if c.Detail != "" {
			lines := strings.Split(c.Detail, "\n")
			line += "  " + lines[0]
			for _, l := range lines[1:] {
				line += "\n      " + l
			}
		}
		fmt.Fprintln(w, line)
		if c.Fix != "" && c.Status != doctorStatusPass && c.Status != doctorStatusSkip {
			fmt.Fprintf(w, "      fix: %s\n", c.Fix)
		}
	}
	fmt.Fprintf(w, "\nClient doctor: %d passed, %d warning(s), %d failed, %d skipped (exit %d)\n",
		r.Passed, r.Warnings, r.Failed, r.Skipped, r.ExitCode)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func failedCheck(name, detail, fix string) remoteDoctorCheck {
	return remoteDoctorCheck{Name: name, Status: doctorStatusFail, Detail: detail, Fix: fix}
}

func skippedCheck(name, detail string) remoteDoctorCheck {
	return remoteDoctorCheck{Name: name, Status: doctorStatusSkip, Detail: detail}
}

// assembleCheck folds the findings of a multi-finding check into one line: fail
// if anything failed, else warn if anything warned, else pass.
func assembleCheck(name string, ms int64, problems []doctorProblem, okDetail string) remoteDoctorCheck {
	c := remoteDoctorCheck{Name: name, Status: doctorStatusPass, Detail: okDetail, Millis: ms}
	if len(problems) == 0 {
		return c
	}
	var lines, fixes []string
	for _, p := range problems {
		switch p.Status {
		case doctorStatusFail:
			c.Status = doctorStatusFail
		case doctorStatusWarn:
			if c.Status != doctorStatusFail {
				c.Status = doctorStatusWarn
			}
		}
		lines = append(lines, p.Text)
		if p.Fix != "" && !containsString(fixes, p.Fix) {
			fixes = append(fixes, p.Fix)
		}
	}
	c.Detail = strings.Join(lines, "\n")
	c.Fix = strings.Join(fixes, "; ")
	return c
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// brain remote doctor
// ---------------------------------------------------------------------------

var (
	remoteDoctorStrict  bool
	remoteDoctorTimeout time.Duration
	remoteDoctorDir     string
	remoteDoctorJobs    int
)

var brainRemoteDoctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose this client: ssh, versions, wrappers, HOME and latency",
	Long: `Check this machine's client mode and print one pass/warn/fail line per
check, each with a concrete fix. Nothing is written: the doctor only reads this
client's HOME, config and wrappers, and runs read-only commands on the host.

Checks:
  config    the client config path, HOME sanity (HOME must be this account's
            own home, and the config must be inside it and owned by you —
            catches a client that read another user's or the host's .config)
  ssh       the configured host answers 'ssh true'
  version   the client's and the host's combined brain versions match
  wrappers  every store the host offers has a client wrapper carrying its own
            BRAIN_REMOTE_STORE, and each store reports its own prefix
  latency   a warm call on the shared ssh connection is fast enough

Exit codes (the failing checks' bits are OR'd together):
   0  every check passed (warnings allowed)
   1  config: no host, unreadable/invalid remote.yaml, or HOME sanity failed
   2  ssh: the host is not reachable
   4  version: client and host brain binaries differ
   8  wrappers: a wrapper is missing, does not carry BRAIN_REMOTE_STORE, or
      resolves to another store's prefix
  16  latency: a warm call took 5s or more
  32  --strict was given and at least one check warned

With --json the report is one JSON object (checks, tallies, exit_code, ok).`,
	Args:        cobra.NoArgs,
	Annotations: map[string]string{"brain-remote": "1"},
	// The exit code is the report; a non-zero one is not a usage error, so
	// neither cobra nor main prints anything extra for it.
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if code := runRemoteDoctorCommand(cmd); code != 0 {
			return &exitError{Code: code}
		}
		return nil
	},
}

// runRemoteDoctorCommand runs the doctor and returns the documented exit code.
// It is separate from the cobra Run so tests can exercise it without forking a
// process or exiting.
func runRemoteDoctorCommand(cmd *cobra.Command) int {
	ctx := remoteCtx(cmd)
	cfg, err := loadRemoteConfig()
	d := newRemoteDoctor(cfg, err)
	if remoteDoctorDir != "" {
		d.binDir = remoteDoctorDir
	}
	if remoteDoctorJobs > 0 {
		d.jobs = remoteDoctorJobs
	}
	// The flag default is remoteDoctorDefaultTimeout; an explicit 0 disables
	// the per-operation bound.
	d.timeout = remoteDoctorTimeout
	checks := d.run(ctx)
	code := remoteDoctorExitCode(checks, remoteDoctorStrict)
	emitRemoteDoctor(os.Stdout, jsonOutput, remoteDoctorReportFor(d, checks, code))
	return code
}

func init() {
	brainRemoteDoctorCmd.Flags().BoolVar(&remoteDoctorStrict, "strict", false,
		"Treat warnings as failures too (exit code gains bit 32)")
	brainRemoteDoctorCmd.Flags().DurationVar(&remoteDoctorTimeout, "timeout", remoteDoctorDefaultTimeout,
		"Bound one host operation (0 disables the bound)")
	brainRemoteDoctorCmd.Flags().StringVar(&remoteDoctorDir, "dir", "",
		"Directory holding this client's store wrappers (default: this binary's directory)")
	brainRemoteDoctorCmd.Flags().IntVar(&remoteDoctorJobs, "jobs", remoteDoctorDefaultJobs,
		"Probe this many stores concurrently on the host")

	brainRemoteCmd.AddCommand(brainRemoteDoctorCmd)
}
