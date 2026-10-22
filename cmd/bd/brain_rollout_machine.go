package main

// brain rollout: how a step reaches a machine. Every machine, the one running
// the command included, is driven through one interface that runs a POSIX sh
// script and returns its stdout, so the steps read the same whether the machine
// is this one, an ssh host, or a fake in a test. The scripts are plain sh on
// purpose: the machine may run a brain build that predates every verb this
// repository added, so nothing here may need the target's brain to know them.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// rolloutExec runs a script on a machine. A non-nil error means the machine
// could not be reached (ssh failed); a script that ran and failed is exit != 0.
type rolloutExec interface {
	Exec(ctx context.Context, m fleetMember, script string, stdin io.Reader) (stdout string, exit int, err error)
}

// sshRolloutExec reaches roster machines over ssh with the options the fleet
// doctor uses, and runs the script locally for the machine this process is on.
type sshRolloutExec struct{ self string }

func (e sshRolloutExec) Exec(ctx context.Context, m fleetMember, script string, stdin io.Reader) (string, int, error) {
	var c *exec.Cmd
	if m.Name == e.self {
		c = exec.CommandContext(ctx, "sh", "-c", script) //nolint:gosec // the script is built from this file's constants and validated values
	} else {
		c = exec.CommandContext(ctx, sshBinary(), "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", //nolint:gosec // roster validated by loadFleetRoster
			"-o", "ControlMaster=no", "-T", m.SSH, "--", "sh -c "+remoteQuote(script))
	}
	var stdout, stderr bytes.Buffer
	c.Stdin = stdin
	c.Stdout, c.Stderr = &stdout, &stderr
	c.WaitDelay = 2 * time.Second
	err := c.Run()
	if err == nil {
		return stdout.String(), 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if m.Name != e.self && ee.ExitCode() == sshConnectFailed {
			return stdout.String(), ee.ExitCode(), fmt.Errorf("ssh to %s failed: %s", m.SSH, strings.TrimSpace(stderr.String()))
		}
		return stdout.String() + stderrTail(stderr.String()), ee.ExitCode(), nil
	}
	return "", -1, err
}

// stderrTail keeps a failing script's message visible to the step that reports
// it, marked so it can never be parsed as a fact.
func stderrTail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > 600 {
		s = s[len(s)-600:]
	}
	return "\nstderr: " + strings.ReplaceAll(s, "\n", " | ")
}

// The environment every script starts with: where brain lives on a
// non-interactive shell, and the switch that keeps a client machine from
// forwarding the command to its host, so a version or a binary is always that
// machine's own.
const rolloutPreamble = `PATH="$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"; export PATH
`
const rolloutLocalPreamble = rolloutPreamble + `BRAIN_REMOTE=off; export BRAIN_REMOTE
`

// scriptProbe reports the machine's facts as key=value lines. Read-only.
const scriptProbe = `# rollout:probe
` + rolloutLocalPreamble + `echo "probe=1"
echo "name=$(hostname -s 2>/dev/null || hostname)"
echo "home=$HOME"
echo "uname=$(uname -s) $(uname -m)"
pin="$HOME/.local/bin/.bd-pin.sh"
if [ -r "$pin" ]; then
  echo "pin=present"
  grep -q 'satellite-config' "$pin" && echo "pin_owner=satellite-config"
  eval "$(sed -n '/^BD_BIN=/p;/^BD_WANT=/p' "$pin")"
  echo "pin_bin=${BD_BIN:-}"
  echo "pin_want=${BD_WANT:-}"
else
  echo "pin=absent"
fi
bin="${BD_BIN:-$HOME/.local/bin/bd}"
echo "bin=$bin"
if [ -x "$bin" ]; then
  if [ "$(head -c 2 "$bin")" = '#!' ]; then echo "bin_kind=script"; else echo "bin_kind=binary"; fi
  echo "bd_version=$("$bin" --version 2>/dev/null | sed -n '1s/^bd version //p')"
else
  echo "bin_kind=missing"
fi
if [ -x "$bin.real" ]; then
  echo "bin_real=$bin.real"
  echo "real_version=$("$bin.real" --version 2>/dev/null | sed -n '1s/^bd version //p')"
fi
if command -v brain >/dev/null 2>&1; then
  echo "brain_version=$(brain --version 2>/dev/null | sed -n '1s/^bd version //p')"
  brain stores --help 2>&1 | grep -qw sync && echo "has_sync=1"
fi
remote="$HOME/.config/brain/remote.yaml"
[ -s "$remote" ] && echo "relay=$(sed -n 's/^host:[[:space:]]*//p' "$remote" | head -n 1)"
[ -r "$HOME/.config/brain/registry.policy" ] && echo "policy=$(head -n 1 "$HOME/.config/brain/registry.policy")"
for f in "$(dirname "$bin")"/bd.pre-*; do [ -e "$f" ] && echo "pre=$f"; done
exit 0
`

// scriptPinCheck sources the pin the way every wrapper does, in a subshell, so
// a refusal (exit 78) is observed without ending anything. Read-only.
const scriptPinCheck = `# rollout:pincheck
` + rolloutLocalPreamble + `pin="$HOME/.local/bin/.bd-pin.sh"
[ -r "$pin" ] || { echo "no-pin"; exit 0; }
( . "$pin" ) 2>&1 && echo "pin-ok"
`

// scriptVersionOf prints the version a file on the machine reports. Read-only.
func scriptVersionOf(path string) string {
	return `# rollout:version
` + rolloutLocalPreamble + `[ -x ` + remoteQuote(path) + ` ] || { echo "missing"; exit 40; }
` + `echo "version=$(` + remoteQuote(path) + ` --version 2>/dev/null | sed -n '1s/^bd version //p')"
`
}

// scriptReport asks the machine's own brain for its registry state. It is not
// run with BRAIN_REMOTE=off: 'stores verify' is a local verb, and on a client
// it reads the registry through the relay, which is how that machine works.
const scriptReport = `# rollout:report
` + rolloutPreamble + `brain stores verify --json --probe 3
`

// scriptSync runs the machine's own 'brain stores sync' (it backs up what it
// replaces and never touches a wrapper brain did not generate).
const scriptSync = `# rollout:sync
` + rolloutPreamble + `brain stores sync
`

// The two scripts below are the client forms of scriptReport and scriptSync. A
// client's 'brain' command is a wrapper that sets BRAIN_REMOTE_STORE=brain, which
// makes the binary forward even 'stores verify' and 'stores sync' to the host: the
// answer would be the host's. Run through the bd binary itself with that variable
// cleared, the registry verbs act on this machine's own files and read the
// registry through the relay, which is how a client reaches it. BRAIN_REMOTE is
// not set to off here: off would send them looking for a database the client does
// not have.

// scriptReportClient asks a client's own binary for its registry state.
func scriptReportClient(bin string) string {
	return `# rollout:report
` + rolloutPreamble + `unset BRAIN_REMOTE_STORE
exec ` + remoteQuote(bin) + ` stores verify --json
`
}

// scriptSyncClient runs a client's own sync, not the host's.
func scriptSyncClient(bin string) string {
	return `# rollout:sync
` + rolloutPreamble + `unset BRAIN_REMOTE_STORE
exec ` + remoteQuote(bin) + ` stores sync
`
}

// scriptWrapperExists checks that the forwarding wrapper of a store is on the
// machine's PATH (the file a sync generates), before it is asked to answer.
func scriptWrapperExists(name string) string {
	return `# rollout:wrapper
` + rolloutPreamble + `command -v ` + remoteQuote(name) + ` >/dev/null 2>&1 || { echo "no wrapper named ` + name + ` on the PATH" >&2; exit 44; }
`
}

// scriptStoreRead reads through a store's wrapper by name.
func scriptStoreRead(name string) string {
	return `# rollout:storeread
` + rolloutPreamble + remoteQuote(name) + ` list --limit 1 >/dev/null
`
}

// scriptSmoke is the cheap, loop-safe check that runs after a sync wrote
// wrappers and before anything is asked to answer. It reads every wrapper brain
// generated in ~/.local/bin and checks, without running any of them, that its
// exec target is a regular executable file that is neither the wrapper itself nor
// another generated wrapper (a wrapper that execs a wrapper re-executes itself
// forever and hangs every command on the machine). Only when all of them pass
// does it run one real read, with a watchdog that kills it after a few seconds,
// so a loop it somehow missed still ends. A wrapper whose exec target is computed
// at run time ('${BD_BIN:-bd}', the direct form) is counted, not followed.
func scriptSmoke(store string, timeoutSecs int) string {
	read := ""
	if store != "" {
		read = `name=` + remoteQuote(store) + `
if ! command -v "$name" >/dev/null 2>&1; then echo "bad: no wrapper named $name on the PATH"; exit 44; fi
( "$name" list --limit 1 >/dev/null 2>&1 ) &
pid=$!
( sleep ` + fmt.Sprint(timeoutSecs) + `; kill -9 "$pid" 2>/dev/null ) >/dev/null 2>&1 &
killer=$!
wait "$pid"; rc=$?
kill "$killer" 2>/dev/null
if [ "$rc" -ne 0 ]; then
  if [ "$rc" -ge 128 ]; then echo "bad: '$name list --limit 1' did not answer within ` + fmt.Sprint(timeoutSecs) + `s (killed)"; else echo "bad: '$name list --limit 1' exited $rc"; fi
  exit 47
fi
echo "read=$name"
`
	}
	return `# rollout:smoke
` + rolloutPreamble + `unset BRAIN_REMOTE_STORE
bin="$HOME/.local/bin"
marked() { head -c 512 "$1" 2>/dev/null | grep -q -e "# Auto-generated by 'brain stores" -e "# brain-remote-wrapper:"; }
n=0; dyn=0; bad=0
for w in "$bin"/*; do
  [ -f "$w" ] || continue
  marked "$w" || continue
  n=$((n+1))
  line=$(grep -v '^[[:space:]]*#' "$w" | grep -E '(^|[[:space:]])exec[[:space:]]' | tail -n 1)
  case "$(printf '%s' "$line" | sed 's/"\$@"//g')" in
    *'$'*) dyn=$((dyn+1)); continue ;;
  esac
  t=$(printf '%s\n' "$line" | sed -n "s/.*exec '\([^']*\)'.*/\1/p")
  base=$(basename "$w")
  if [ -z "$t" ]; then echo "bad: $base: no exec target it can read"; bad=$((bad+1)); continue; fi
  if [ ! -f "$t" ] || [ ! -x "$t" ]; then echo "bad: $base: exec target $t is not an executable file"; bad=$((bad+1)); continue; fi
  if [ "$t" -ef "$w" ]; then echo "bad: $base: exec target $t is the wrapper itself (re-executes forever)"; bad=$((bad+1)); continue; fi
  if marked "$t"; then echo "bad: $base: exec target $t is itself a generated wrapper (re-executes forever)"; bad=$((bad+1)); continue; fi
done
echo "wrappers=$n dynamic=$dyn"
[ "$bad" -eq 0 ] || exit 45
` + read
}

// scriptInstall receives the candidate binary on stdin, proves the uploaded
// file runs and reports the expected version, keeps the previous binary under
// bd.pre-<sha> (never overwriting a different backup), then renames the new file
// into place. The backup is made before the rename, and the rename is atomic.
func scriptInstall(target, wantVersion, prevSHA string, size int64) string {
	return `# rollout:install
set -eu
` + rolloutLocalPreamble + `target=` + remoteQuote(target) + `
want=` + remoteQuote(wantVersion) + `
dir=$(dirname "$target")
new="$dir/.bd.new.$$"
trap 'rm -f "$new"' EXIT
cat > "$new"
chmod 755 "$new"
[ "$(wc -c < "$new" | tr -d ' ')" = '` + fmt.Sprint(size) + `' ] || { echo "uploaded file has the wrong size" >&2; exit 41; }
got=$("$new" --version 2>/dev/null | sed -n '1s/^bd version //p')
[ "$got" = "$want" ] || { echo "uploaded binary reports '$got', wanted '$want'" >&2; exit 42; }
prev=none
if [ -e "$target" ]; then
  prev="$dir/bd.pre-` + prevSHA + `"
  if [ -e "$prev" ] && ! cmp -s "$prev" "$target"; then prev="$prev.$(date +%s)"; fi
  [ -e "$prev" ] || cp -p "$target" "$prev"
fi
mv -f "$new" "$target"
trap - EXIT
echo "previous=$prev"
`
}

// scriptRestore puts a kept binary back as the installed one, keeping the one
// it replaces under bd.post-<sha>. Used by rollback and by a failed install.
func scriptRestore(target, previous, wantVersion, curSHA string) string {
	return `# rollout:restore
set -eu
` + rolloutLocalPreamble + `target=` + remoteQuote(target) + `
prev=` + remoteQuote(previous) + `
want=` + remoteQuote(wantVersion) + `
dir=$(dirname "$target")
[ -x "$prev" ] || { echo "kept binary $prev is missing" >&2; exit 40; }
got=$("$prev" --version 2>/dev/null | sed -n '1s/^bd version //p')
[ "$got" = "$want" ] || { echo "kept binary reports '$got', wanted '$want'" >&2; exit 42; }
if [ -e "$target" ]; then
  keep="$dir/bd.post-` + curSHA + `"
  if [ -e "$keep" ] && ! cmp -s "$keep" "$target"; then keep="$keep.$(date +%s)"; fi
  [ -e "$keep" ] || cp -p "$target" "$keep"
  echo "kept=$keep"
fi
tmp="$dir/.bd.restore.$$"
cp -p "$prev" "$tmp"
mv -f "$tmp" "$target"
`
}

// machineFacts is what scriptProbe printed, parsed.
type machineFacts struct {
	Name, Home, Uname           string
	PinPresent                  bool
	PinOwner, PinBin, PinWant   string
	Bin, BinKind, BinReal       string
	BdVersion, RealVersion      string
	BrainVersion                string
	HasSync                     bool
	Relay, Policy               string
	PreBackups                  []string
	hasBrain, probeSeen, hasPin bool
}

func parseMachineFacts(out string) (*machineFacts, error) {
	f := &machineFacts{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !ok {
			continue
		}
		switch k {
		case "probe":
			f.probeSeen = true
		case "name":
			f.Name = v
		case "home":
			f.Home = v
		case "uname":
			f.Uname = v
		case "pin":
			f.PinPresent = v == "present"
		case "pin_owner":
			f.PinOwner = v
		case "pin_bin":
			f.PinBin = v
		case "pin_want":
			f.PinWant = v
		case "bin":
			f.Bin = v
		case "bin_kind":
			f.BinKind = v
		case "bin_real":
			f.BinReal = v
		case "bd_version":
			f.BdVersion = v
		case "real_version":
			f.RealVersion = v
		case "brain_version":
			f.hasBrain = true
			f.BrainVersion = v
		case "has_sync":
			f.HasSync = v == "1"
		case "relay":
			f.Relay = v
		case "policy":
			f.Policy = v
		case "pre":
			f.PreBackups = append(f.PreBackups, v)
		}
	}
	if !f.probeSeen || f.Bin == "" {
		return nil, fmt.Errorf("the machine did not answer the probe (got %q)", rolloutFirstLine(out))
	}
	return f, nil
}

func rolloutFirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// installTarget is the file a new binary replaces: the pin's BD_BIN when that
// is a real binary, or the bd.real next to it when BD_BIN is a shim script (the
// layout where the shim injects credentials and execs bd.real). Anything else
// is refused rather than guessed.
func (f *machineFacts) installTarget() (path, installed string, err error) {
	switch f.BinKind {
	case "binary", "missing":
		return f.Bin, f.BdVersion, nil
	case "script":
		if f.BinReal != "" {
			return f.BinReal, f.RealVersion, nil
		}
		return "", "", fmt.Errorf("%s is a shell script and there is no %s.real next to it, so brain cannot tell which file is the binary", f.Bin, f.Bin)
	}
	return "", "", fmt.Errorf("unknown kind of %s: %q", f.Bin, f.BinKind)
}

// goPlatform maps `uname -sm` to GOOS/GOARCH.
func goPlatform(uname string) (goos, goarch string) {
	fields := strings.Fields(strings.ToLower(uname))
	if len(fields) < 2 {
		return "", ""
	}
	goos = fields[0]
	switch fields[1] {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "arm64", "aarch64":
		goarch = "arm64"
	default:
		goarch = fields[1]
	}
	return goos, goarch
}

var commitInVersionRE = regexp.MustCompile(`\(([0-9a-f]{7,40})\)`)

// shaOfVersion is the commit named in a version string, for backup file names.
func shaOfVersion(v string) string {
	if m := commitInVersionRE.FindStringSubmatch(v); m != nil {
		return m[1]
	}
	return "unknown"
}
