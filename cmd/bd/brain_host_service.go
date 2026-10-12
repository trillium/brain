// Package main — brain_host_service.go
//
// Starting, stopping and un-starting the Dolt server (and the other jobs that
// belong to the host role). Three ways a server can be kept running:
//
//	launchd    a macOS LaunchAgent: survives logout and reboot. The default on macOS.
//	process    a detached dolt the tool started itself, with a pid file. Not
//	           restarted at login; the plan says so.
//	external   something else starts it. The tool never starts or stops it; it
//	           only checks that it answers.
//
// Every action is journaled before it is taken, so a rollback can reverse it.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// serviceController reverses journaled service actions.
type serviceController interface {
	undo(e journalEntry) (step string, err error)
}

// serverSpec is how to launch a Dolt SQL server.
type serverSpec struct {
	DoltBin        string
	DataDir        string
	Bind           string
	Port           int
	RemotesAPIPort int
}

func (s serverSpec) args() []string {
	a := []string{"sql-server", "-H", s.Bind, "-P", strconv.Itoa(s.Port), "--data-dir", s.DataDir, "--loglevel", "warning"}
	if s.RemotesAPIPort > 0 {
		a = append(a, "--remotesapi-port", strconv.Itoa(s.RemotesAPIPort))
	}
	return a
}

// serviceManager runs service actions. run is replaceable so tests can record
// launchctl calls instead of making them.
type serviceManager struct {
	home string
	run  func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func newServiceManager(home string) *serviceManager {
	return &serviceManager{home: home, run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // fixed tool names, validated labels
	}}
}

// defaultServiceKind chooses how to keep a server running on this machine.
func defaultServiceKind() string {
	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("launchctl"); err == nil {
			return "launchd"
		}
	}
	return "process"
}

// newServerService fills in the files a service of this kind uses.
func (m *serviceManager) newServerService(kind, label string) hostService {
	svc := hostService{Kind: kind}
	switch kind {
	case "launchd":
		if label == "" {
			label = "com.brain.dolt-server"
		}
		svc.Label = label
		svc.Plist = filepath.Join(m.home, "Library", "LaunchAgents", label+".plist")
		svc.LogFile = filepath.Join(m.home, "Library", "Logs", label+".log")
	case "process":
		svc.PidFile = filepath.Join(m.home, ".config", "brain", "dolt-server.pid")
		svc.LogFile = filepath.Join(m.home, ".config", "brain", "dolt-server.log")
	}
	return svc
}

func validLaunchdLabel(l string) bool {
	if l == "" || len(l) > 128 {
		return false
	}
	for _, r := range l {
		switch {
		case r >= '0' && r <= '9', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func (m *serviceManager) domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

// start brings the server up under svc and waits until it answers.
func (m *serviceManager) start(ctx context.Context, svc hostService, spec serverSpec, j *journal) error {
	switch svc.Kind {
	case "external":
		return waitServerReady(ctx, spec.Port, 20*time.Second)
	case "process":
		return m.startProcess(ctx, svc, spec, j)
	case "launchd":
		return m.startLaunchd(ctx, svc, spec, j)
	default:
		return fmt.Errorf("unknown service kind %q", svc.Kind)
	}
}

func (m *serviceManager) startProcess(ctx context.Context, svc hostService, spec serverSpec, j *journal) error {
	if portListening(spec.Port) {
		if pid := readPid(svc.PidFile); pid > 0 && pidAlive(pid) {
			return waitServerReady(ctx, spec.Port, 30*time.Second) // ours, already up
		}
		return fmt.Errorf("something else is already listening on port %d", spec.Port)
	}
	if err := os.MkdirAll(filepath.Dir(svc.LogFile), 0o755); err != nil { //nolint:gosec // config directory
		return err
	}
	ensureDoltIdentityIfMissing(spec.DoltBin)
	if j != nil {
		// The pid file is journaled first so that, undoing newest-first, the
		// server is stopped (which needs the pid file) before the file is removed.
		if err := j.saveFile(svc.PidFile); err != nil {
			return err
		}
		if err := j.recordService("started", svc); err != nil {
			return err
		}
	}
	return m.startArgv(ctx, svc, append([]string{spec.DoltBin}, spec.args()...), spec.DataDir, spec.Port)
}

// startArgv launches a detached server process and records its pid.
func (m *serviceManager) startArgv(ctx context.Context, svc hostService, argv []string, dir string, port int) error {
	if err := os.MkdirAll(filepath.Dir(svc.LogFile), 0o755); err != nil { //nolint:gosec // config directory
		return err
	}
	logf, err := os.OpenFile(svc.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // our own log
	if err != nil {
		return err
	}
	defer func() { _ = logf.Close() }()
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // dolt found by LookPath, args built from validated values
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = detachedProcAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting dolt: %w", err)
	}
	pid := cmd.Process.Pid
	if err := hostWriteFile(svc.PidFile, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	go func() { _ = cmd.Wait() }() // reap if it exits while we are still alive
	if err := waitServerReady(ctx, port, 60*time.Second); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("%w (see %s)", err, svc.LogFile)
	}
	return nil
}

func (m *serviceManager) startLaunchd(ctx context.Context, svc hostService, spec serverSpec, j *journal) error {
	if !validLaunchdLabel(svc.Label) {
		return fmt.Errorf("invalid launchd label %q", svc.Label)
	}
	body := launchdServerPlist(svc, spec)
	if j != nil {
		if err := j.writeFile(svc.Plist, []byte(body), 0o644); err != nil { //nolint:gosec // LaunchAgent plists are world-readable by convention
			return err
		}
		if err := j.recordService("loaded", svc); err != nil {
			return err
		}
	} else if err := hostWriteFile(svc.Plist, []byte(body), 0o644); err != nil { //nolint:gosec // as above
		return err
	}
	if err := os.MkdirAll(filepath.Dir(svc.LogFile), 0o755); err != nil { //nolint:gosec // log directory
		return err
	}
	ensureDoltIdentityIfMissing(spec.DoltBin)
	if out, err := m.run(ctx, "launchctl", "bootstrap", m.domain(), svc.Plist); err != nil {
		// Already loaded is fine: kick it so it picks up the plist.
		if _, kerr := m.run(ctx, "launchctl", "kickstart", "-k", m.domain()+"/"+svc.Label); kerr != nil {
			return fmt.Errorf("launchctl bootstrap %s: %w: %s", svc.Label, err, strings.TrimSpace(string(out)))
		}
	}
	return waitServerReady(ctx, spec.Port, 60*time.Second)
}

// stop shuts the server down. With disable, a launchd job is also disabled so
// it does not come back at the next login.
func (m *serviceManager) stop(ctx context.Context, svc hostService, port int, disable bool, spec *serverSpec, j *journal) error {
	switch svc.Kind {
	case "external", "":
		return errors.New("the server is managed outside brain; stop it yourself, then re-run")
	case "process":
		pid := readPid(svc.PidFile)
		if pid <= 0 || !pidAlive(pid) {
			if portListening(port) {
				return fmt.Errorf("port %d is in use by a process brain did not start (no live pid in %s)", port, svc.PidFile)
			}
			return nil
		}
		if j != nil {
			var argv []string
			dir := ""
			if spec != nil {
				argv = append([]string{spec.DoltBin}, spec.args()...)
				dir = spec.DataDir
			}
			if err := j.recordServiceStop(svc, argv, dir, port); err != nil {
				return err
			}
		}
		return terminatePid(pid, port)
	case "launchd":
		if !validLaunchdLabel(svc.Label) {
			return fmt.Errorf("invalid launchd label %q", svc.Label)
		}
		if j != nil {
			if err := j.recordService("unloaded", svc); err != nil {
				return err
			}
			if disable {
				if err := j.recordService("disabled", svc); err != nil {
					return err
				}
			}
		}
		if disable {
			if out, err := m.run(ctx, "launchctl", "disable", m.domain()+"/"+svc.Label); err != nil {
				return fmt.Errorf("launchctl disable %s: %w: %s", svc.Label, err, strings.TrimSpace(string(out)))
			}
		}
		if out, err := m.run(ctx, "launchctl", "bootout", m.domain()+"/"+svc.Label); err != nil {
			if !strings.Contains(string(out), "No such process") && !strings.Contains(string(out), "Could not find") && !strings.Contains(strings.ToLower(string(out)), "not found") {
				return fmt.Errorf("launchctl bootout %s: %w: %s", svc.Label, err, strings.TrimSpace(string(out)))
			}
		}
		return waitPortClosed(ctx, port, 30*time.Second)
	}
	return fmt.Errorf("unknown service kind %q", svc.Kind)
}

// undo reverses one journaled service action.
func (m *serviceManager) undo(e journalEntry) (string, error) {
	svc := e.Service
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	switch svc.Kind {
	case "process":
		switch e.Action {
		case "started":
			pid := readPid(svc.PidFile)
			if pid > 0 && pidAlive(pid) {
				if err := terminatePid(pid, 0); err != nil {
					return "", err
				}
				return fmt.Sprintf("stopped the dolt server started by this promotion (pid %d)", pid), nil
			}
		case "stopped":
			if len(e.Argv) == 0 {
				return "", errors.New("the server brain stopped must be started again by hand: no start command was recorded")
			}
			if portListening(e.Port) {
				return "", nil
			}
			if err := m.startArgv(ctx, svc, e.Argv, e.Dir, e.Port); err != nil {
				return "", err
			}
			return fmt.Sprintf("started the dolt server again on port %d", e.Port), nil
		}
	case "launchd":
		if !validLaunchdLabel(svc.Label) {
			return "", nil
		}
		target := m.domain() + "/" + svc.Label
		switch e.Action {
		case "loaded":
			if _, err := m.run(ctx, "launchctl", "bootout", target); err == nil {
				return "unloaded " + svc.Label, nil
			}
		case "unloaded":
			if svc.Plist == "" {
				return "", nil
			}
			if _, err := m.run(ctx, "launchctl", "bootstrap", m.domain(), svc.Plist); err != nil {
				return "", fmt.Errorf("launchctl bootstrap %s: %w", svc.Label, err)
			}
			return "loaded " + svc.Label, nil
		case "disabled":
			if _, err := m.run(ctx, "launchctl", "enable", target); err != nil {
				return "", fmt.Errorf("launchctl enable %s: %w", svc.Label, err)
			}
			return "enabled " + svc.Label, nil
		}
	}
	return "", nil
}

func readPid(file string) int {
	if file == "" {
		return 0
	}
	data, err := os.ReadFile(file) //nolint:gosec // our own pid file
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return n
}

func waitPortClosed(ctx context.Context, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for portListening(port) {
		if time.Now().After(deadline) {
			return fmt.Errorf("port %d is still open after %s", port, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil
}

// launchdServerPlist renders the LaunchAgent for a brain Dolt server.
func launchdServerPlist(svc hostService, spec serverSpec) string {
	var args strings.Builder
	args.WriteString("    <string>" + xmlEscape(spec.DoltBin) + "</string>\n")
	for _, a := range spec.args() {
		args.WriteString("    <string>" + xmlEscape(a) + "</string>\n")
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Written by 'brain host promote': the Dolt server that holds the brain database. -->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + xmlEscape(svc.Label) + `</string>
  <key>ProgramArguments</key>
  <array>
` + args.String() + `  </array>
  <key>WorkingDirectory</key>
  <string>` + xmlEscape(spec.DataDir) + `</string>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>` + xmlEscape(svc.LogFile) + `</string>
  <key>StandardErrorPath</key>
  <string>` + xmlEscape(svc.LogFile) + `</string>
</dict>
</plist>
`
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// ensureDoltIdentityIfMissing sets a global Dolt author identity when none is
// configured: a server cannot commit without one, and a fresh account has none.
// An identity the user already set is never touched.
func ensureDoltIdentityIfMissing(doltBin string) {
	set := func(key, value string) {
		out, err := exec.Command(doltBin, "config", "--global", "--get", key).Output() //nolint:gosec // dolt found by LookPath
		if err == nil && strings.TrimSpace(string(out)) != "" {
			return
		}
		_ = exec.Command(doltBin, "config", "--global", "--add", key, value).Run() //nolint:gosec // as above
	}
	set("user.name", "brain")
	set("user.email", "brain@localhost")
}
