// Package main — brain_remote.go
//
// Client mode: run brain, and every store command, against another machine.
//
// The brain stores live in one Dolt database on one computer (the "brain
// host"). Every other machine sets that host once ('brain remote set
// <ssh-target>'); from then on this binary does not open a database at all.
// It re-runs the exact command line on the host over ssh, through the host's
// own store wrappers, and relays stdin, stdout, stderr, the TTY and the exit
// code. Nothing about the database is exposed on the network: the only door is
// ssh, and the host keeps pinning its Dolt server to loopback.
//
//	task create "ship it"      →  ssh host  ~/.local/bin/task create 'ship it'
//	brain search roadmap       →  ssh host  ~/.local/bin/brain search roadmap
//
// 'brain remote install' writes one tiny wrapper per store in the host's
// registry (plus nothing for 'brain' itself — this binary is that command).
// Each wrapper only names the store; the forwarding logic lives here.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/beads"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

const (
	// remoteConfigPath is the client config, relative to $HOME. It sits next to
	// the store registry (stores.yaml) in brain's own configuration directory.
	remoteConfigPath = ".config/brain/remote.yaml"

	// remoteWrapperMarker is the second line of every generated client wrapper.
	// Install only ever overwrites or prunes files that carry it.
	remoteWrapperMarker = "# brain-remote-wrapper:"

	// Environment knobs. All optional.
	envRemoteOff   = "BRAIN_REMOTE"       // "off" runs locally (also set on the host side of every forward)
	envRemoteHost  = "BRAIN_REMOTE_HOST"  // overrides the configured ssh target
	envRemoteStore = "BRAIN_REMOTE_STORE" // set by a client wrapper: the command name to run on the host
	envRemoteSSH   = "BRAIN_REMOTE_SSH"   // ssh binary to use (default "ssh"; tests substitute a fake)

	defaultRemoteBinDir  = "$HOME/.local/bin"
	defaultRemoteTimeout = 5

	// sshConnectFailed is the exit status ssh uses for its own failures.
	sshConnectFailed = 255
)

// remoteConfig is the on-disk shape of ~/.config/brain/remote.yaml.
type remoteConfig struct {
	// Host is the ssh target of the brain host, e.g. "me@macbook.example.ts.net".
	Host string `yaml:"host"`
	// BinDir is where the host keeps its store wrappers (default $HOME/.local/bin).
	BinDir string `yaml:"bin_dir,omitempty"`
	// ConnectTimeout is the ssh connect timeout in seconds (default 5).
	ConnectTimeout int `yaml:"connect_timeout,omitempty"`
	// SSHOptions are extra "-o"-style options, e.g. "IdentityFile=~/.ssh/brain".
	SSHOptions []string `yaml:"ssh_options,omitempty"`
}

var (
	remoteNameRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	remoteBinDirRE = regexp.MustCompile(`^[A-Za-z0-9_./${}~-]+$`)
	remoteHostRE   = regexp.MustCompile(`^[A-Za-z0-9_.@:%\[\]-]+$`)
	remoteOptionRE = regexp.MustCompile(`^[A-Za-z0-9]+=[^\s]+$`)
)

func remoteConfigFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, remoteConfigPath)
}

func (c *remoteConfig) binDir() string {
	d := strings.TrimSpace(c.BinDir)
	if d == "" {
		return defaultRemoteBinDir
	}
	if d == "~" || strings.HasPrefix(d, "~/") {
		return "$HOME" + d[1:]
	}
	return d
}

func (c *remoteConfig) connectTimeout() int {
	if c.ConnectTimeout > 0 {
		return c.ConnectTimeout
	}
	return defaultRemoteTimeout
}

// validate rejects values that would be unsafe to hand to ssh or to embed in
// the host-side script. The config file is the user's own, so this guards
// against typos and accidents rather than an attacker.
func (c *remoteConfig) validate() error {
	if c.Host == "" {
		return errors.New("no brain host configured (run 'brain remote set <ssh-target>')")
	}
	if strings.HasPrefix(c.Host, "-") || !remoteHostRE.MatchString(c.Host) {
		return fmt.Errorf("invalid brain host %q: expected an ssh target such as user@host", c.Host)
	}
	if !remoteBinDirRE.MatchString(c.binDir()) {
		return fmt.Errorf("invalid bin_dir %q", c.BinDir)
	}
	for _, o := range c.SSHOptions {
		if !remoteOptionRE.MatchString(o) {
			return fmt.Errorf("invalid ssh option %q: expected Name=value without spaces", o)
		}
	}
	return nil
}

// loadRemoteConfig reads the client config and applies the environment
// override. A missing file is not an error: it means "not a client".
func loadRemoteConfig() (*remoteConfig, error) {
	cfg := &remoteConfig{}
	if path := remoteConfigFile(); path != "" {
		data, err := os.ReadFile(path) //nolint:gosec // user's own config file
		switch {
		case err == nil:
			if uerr := yaml.Unmarshal(data, cfg); uerr != nil {
				return nil, fmt.Errorf("reading %s: %w", path, uerr)
			}
		case !errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
	}
	if h := strings.TrimSpace(os.Getenv(envRemoteHost)); h != "" {
		cfg.Host = h
	}
	return cfg, nil
}

func saveRemoteConfig(cfg *remoteConfig) error {
	path := remoteConfigFile()
	if path == "" {
		return errors.New("cannot resolve home directory")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // same mode as stores.yaml's directory
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	header := "# brain client mode — managed by 'brain remote'. Commands run on this host over ssh.\n"
	return os.WriteFile(path, append([]byte(header), data...), 0o644) //nolint:gosec // no secrets here
}

// remoteQuote quotes s as one POSIX shell word: the result, parsed by any
// sh-compatible shell, yields exactly s. Single quotes cannot be escaped inside
// single quotes, so each is closed, escaped, and reopened.
func remoteQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// remoteScript is what the host's shell runs; the command name and arguments
// follow it as positional parameters, so none of them is ever re-parsed.
// BRAIN_REMOTE=off keeps a configured host from forwarding onward;
// BRAIN_REMOTE_FORWARDED=1 tells the host the call came from a client, so a
// host that has since moved can answer with where the brain went.
func (c *remoteConfig) remoteScript() string {
	return `d="` + c.binDir() + `"; PATH="$d:$PATH"; BRAIN_REMOTE=off; BRAIN_REMOTE_FORWARDED=1; export PATH BRAIN_REMOTE BRAIN_REMOTE_FORWARDED; exec "$d/$0" "$@"`
}

// remoteCommand builds the single string ssh sends to the host: an sh -c
// whose script is fixed and whose $0/$@ are the quoted name and arguments.
func (c *remoteConfig) remoteCommand(name string, args []string) string {
	parts := make([]string, 0, len(args)+4)
	parts = append(parts, "exec", "/bin/sh", "-c", remoteQuote(c.remoteScript()), remoteQuote(name))
	for _, a := range args {
		parts = append(parts, remoteQuote(a))
	}
	return strings.Join(parts, " ")
}

// remoteControlPath returns the ssh ControlPath template used to multiplex
// every call over one connection. Unix socket paths are short (104 bytes on
// macOS), so fall back to a per-user directory under /tmp when $HOME is deep.
func remoteControlPath() string {
	const hashLen = len("/") + 40 // "%C" expands to a 40-char hash
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dir := filepath.Join(home, ".cache", "brain", "ssh")
		if len(dir)+hashLen < 100 {
			if os.MkdirAll(dir, 0o700) == nil {
				return filepath.Join(dir, "%C")
			}
		}
	}
	dir := filepath.Join("/tmp", fmt.Sprintf("brain-ssh-%d", os.Getuid()))
	_ = os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, "%C")
}

// sshBaseArgs are the ssh options shared by every call: no prompts, bounded
// connect time, one persistent multiplexed connection.
func (c *remoteConfig) sshBaseArgs() []string {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", fmt.Sprintf("ConnectTimeout=%d", c.connectTimeout()),
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + remoteControlPath(),
		"-o", "ControlPersist=10m",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "LogLevel=ERROR",
	}
	for _, o := range c.SSHOptions {
		args = append(args, "-o", o)
	}
	return args
}

// sshArgs is the full ssh argument list for running name+args on the host.
func (c *remoteConfig) sshArgs(tty bool, name string, args []string) []string {
	a := c.sshBaseArgs()
	if tty {
		a = append(a, "-t")
	} else {
		a = append(a, "-T")
	}
	return append(a, c.Host, "--", c.remoteCommand(name, args))
}

func sshBinary() string {
	if s := os.Getenv(envRemoteSSH); s != "" {
		return s
	}
	return "ssh"
}

func fdIsTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// remoteWriteVerbs are the top-level verbs that write to whichever store
// answers them. A client wrapper names its store and forwards every verb; a
// bare 'brain'/'bd' call with no store named anywhere is refused for these.
var remoteWriteVerbs = map[string]bool{
	"create": true,
	"update": true,
	"close":  true,
	"delete": true,
	"import": true,
}

// noWorkspaceWriteMsg is the refusal for a bare client write with no store
// named by BEADS_DIR or by a .beads workspace found from the working directory.
const noWorkspaceWriteMsg = "no beads workspace here; set BEADS_DIR or BRAIN_REMOTE=off"

// remoteVerbName resolves the first command word of args to the name of the
// top-level command it invokes, so an alias ("new", "done") counts as the verb
// it stands for. A word that names no command is returned as typed.
func remoteVerbName(args []string) string {
	word, _ := commandWords(args)
	if word == "" {
		return ""
	}
	for _, c := range rootCmd.Commands() {
		if c.Name() == word || c.HasAlias(word) {
			return c.Name()
		}
	}
	return word
}

// remoteForwardingActive reports whether this invocation must be forwarded,
// and to what. name is the command to run on the host ("brain" unless a
// wrapper named a store).
func remoteForwardingActive(args []string) (cfg *remoteConfig, name string, active bool, err error) {
	if strings.EqualFold(os.Getenv(envRemoteOff), "off") {
		return nil, "", false, nil
	}
	name = os.Getenv(envRemoteStore)
	// 'brain remote …' and 'brain host …' configure or manage this machine and
	// always run here.
	if name == "" && len(args) > 0 && (args[0] == "remote" || args[0] == "host") {
		return nil, "", false, nil
	}
	// The registry verbs that manage THIS machine's shims (sync, verify, the
	// fleet doctor, the poller, the shell hook) always run here; they read the
	// registry through the relay host themselves.
	if name == "" && registryLocalVerb(args) {
		return nil, "", false, nil
	}
	// An explicit BEADS_DIR naming a local beads project wins over the client
	// config in HOME, so the store it names is opened here instead of forwarded.
	if beads.ExplicitBeadsDir() != "" {
		return nil, "", false, nil
	}
	cfg, err = loadRemoteConfig()
	if err != nil {
		return nil, "", false, err
	}
	if cfg.Host == "" {
		return nil, "", false, nil
	}
	if name == "" {
		name = "brain"
		// A bare call names no store. The host answers it through its own
		// 'brain' wrapper, so a write would land in whatever store that wrapper
		// defaults to. Reads are harmless to forward. A bare write runs here
		// against a .beads workspace found from the working directory, and is
		// refused otherwise. A BEADS_DIR reaching this point is not a local
		// project, so the host cannot honor it and it names nothing.
		if remoteWriteVerbs[remoteVerbName(args)] {
			if os.Getenv("BEADS_DIR") == "" && beads.FindBeadsDir() != "" {
				return nil, "", false, nil
			}
			return nil, "", false, errors.New(noWorkspaceWriteMsg)
		}
	}
	if !remoteNameRE.MatchString(name) {
		return nil, "", false, fmt.Errorf("invalid store command name %q in $%s", name, envRemoteStore)
	}
	if err = cfg.validate(); err != nil {
		return nil, "", false, err
	}
	return cfg, name, true, nil
}

// maybeForwardRemote runs the command on the brain host when this machine is a
// client. It reports whether it handled the invocation and the exit status.
// Called from main before any command, database or config machinery starts.
func maybeForwardRemote(args []string) (handled bool, exitCode int) {
	cfg, name, active, err := remoteForwardingActive(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "brain: %v\n", err)
		return true, 2
	}
	if !active {
		return false, 0
	}
	tty := fdIsTerminal(os.Stdin) && fdIsTerminal(os.Stdout)
	return true, forwardRemote(cfg, name, args, tty, os.Stdin, os.Stdout, os.Stderr)
}

// forwardRemote runs name+args on the host with the given streams and returns
// the exit status to use. Files are passed straight through to ssh, so a real
// TTY or pipe is relayed byte for byte.
//
// A local file named by --body-file, --description-file, or a text-file flag is
// read here and sent on stdin (see localFileFlags), because the path would
// otherwise be resolved on the brain host.
//
// If the host answers that the brain has moved (a "moved" notice, see
// brain_host_follow.go), the client verifies the move, points remote.yaml at the
// new host and runs the command once more there, provided the command's input
// can be replayed. If the move cannot be verified, nothing is changed and the
// command is not run anywhere.
func forwardRemote(cfg *remoteConfig, name string, args []string, tty bool, stdin, stdout, stderr *os.File) int {
	args, payload, err := localFileFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "brain: %v\n", err)
		return 1
	}
	if payload != nil {
		spool, err := spoolStdin(payload)
		if err != nil {
			fmt.Fprintf(stderr, "brain: %v\n", err)
			return 1
		}
		defer func() {
			_ = spool.Close()
			_ = os.Remove(spool.Name())
		}()
		stdin, tty = spool, false
	}

	startOffset := int64(-1)
	if fi, err := stdin.Stat(); err == nil && fi.Mode().IsRegular() {
		if off, serr := stdin.Seek(0, io.SeekCurrent); serr == nil {
			startOffset = off
		}
	}
	moved, code := forwardOnce(cfg, name, args, tty, stdin, stdout, stderr)
	if code != hostMovedExit || moved == nil {
		return code
	}

	oldHost := cfg.Host
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	final, hops, err := followMove(ctx, cfg, moved)
	cancel()
	if err != nil {
		fmt.Fprintf(stderr, "brain: %s says the brain host moved to %s, but that could not be verified, so nothing was changed and your command did not run: %v\n"+
			"  If you trust the move, point this client yourself:  brain remote set %s\n", oldHost, moved.Target, err, moved.Target)
		return hostMovedUnverifiedExit
	}

	persisted := os.Getenv(envRemoteHost) == ""
	if persisted {
		saved, lerr := loadRemoteConfig()
		if lerr == nil {
			saved.Host = final
			lerr = saveRemoteConfig(saved)
		}
		persisted = lerr == nil
	}
	cfg.Host = final
	plural := ""
	if len(hops) > 1 {
		plural = fmt.Sprintf(" after %d moves", len(hops))
	}
	if persisted {
		fmt.Fprintf(stderr, "brain: re-pointed this client from %s to %s (the brain host moved %s%s; verified); %s updated\n", oldHost, final, moved.At, plural, remoteConfigFile())
	} else {
		fmt.Fprintf(stderr, "brain: using %s for this command (the brain host moved from %s%s; verified); %s is overridden by $%s, so it was not rewritten\n", final, oldHost, plural, remoteConfigFile(), envRemoteHost)
	}
	if !stdinReplayable(stdin, tty, startOffset) {
		fmt.Fprintf(stderr, "brain: this command's input was already consumed and cannot be replayed; run it again to use the new host\n")
		return hostMovedExit
	}
	again, code := forwardOnce(cfg, name, args, tty, stdin, stdout, stderr)
	if code == hostMovedExit && again != nil {
		fmt.Fprintf(stderr, "brain: %s also reports a move (to %s); not following a second time for one command. Run it again.\n", final, again.Target)
	}
	return code
}

// localFileFlag reports whether flag names a local file that the command reads
// as text. --body-file and --description-file always do. --file and -f do only
// where the command reads comment text from them: create's --file is a markdown
// batch and dep's is a JSONL edge list, so those keep their paths.
func localFileFlag(flag, cmd, sub string) bool {
	switch flag {
	case "--body-file", "--description-file":
		return true
	case "--file", "-f":
		switch cmd {
		case "comment", "note":
			return flag == "--file"
		case "comments":
			return sub == "add"
		}
	}
	return false
}

// takesValue reports whether arg, a flag as typed, names a flag of cmd (its own,
// persistent, or inherited from a parent) that takes a value.
func takesValue(cmd *cobra.Command, arg string) bool {
	if long, ok := strings.CutPrefix(arg, "--"); ok {
		f := cmd.Flags().Lookup(long)
		if f == nil {
			f = cmd.PersistentFlags().Lookup(long)
		}
		if f == nil {
			f = cmd.InheritedFlags().Lookup(long)
		}
		return f != nil && f.NoOptDefVal == ""
	}
	if len(arg) != 2 {
		return false
	}
	f := cmd.Flags().ShorthandLookup(arg[1:])
	if f == nil {
		f = cmd.PersistentFlags().ShorthandLookup(arg[1:])
	}
	if f == nil {
		f = cmd.InheritedFlags().ShorthandLookup(arg[1:])
	}
	return f != nil && f.NoOptDefVal == ""
}

// positionalArgs returns the indexes of args that are positional words rather
// than flags or flag values. takes reports whether a flag word takes the next
// word as its value. Everything after "--" is positional.
func positionalArgs(args []string, takes func(string) bool) []int {
	var pos []int
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			for j := i + 1; j < len(args); j++ {
				pos = append(pos, j)
			}
			return pos
		case a == "-" || !strings.HasPrefix(a, "-"):
			pos = append(pos, i)
		case !strings.Contains(a, "=") && takes(a):
			i++
		}
	}
	return pos
}

// commandWords returns the command and subcommand words of args. Root flags
// that take a value may come before them, so their values are skipped.
func commandWords(args []string) (cmd, sub string) {
	pos := positionalArgs(args, func(a string) bool { return takesValue(rootCmd, a) })
	if len(pos) > 0 {
		cmd = args[pos[0]]
	}
	if len(pos) > 1 {
		sub = args[pos[1]]
	}
	return cmd, sub
}

// splitFlag separates a flag word into its name and inline value. The combined
// short form -f<path> splits after the f, as -f=<path> does.
func splitFlag(a string) (name, value string, inline bool) {
	name = a
	if j := strings.IndexByte(a, '='); j > 0 && strings.HasPrefix(a, "-") {
		name, value, inline = a[:j], a[j+1:], true
	}
	if strings.HasPrefix(a, "-f") && len(a) > 2 && a[2] != '=' {
		name, value, inline = "-f", a[2:], true
	}
	return name, value, inline
}

func isFileFlagWord(a string) bool {
	name, _, _ := splitFlag(a)
	return name == "--file" || name == "-f"
}

// hasCommentFile reports whether a --file or -f flag comes before the first "--",
// where the words are flags rather than literal text.
func hasCommentFile(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if isFileFlagWord(a) {
			return true
		}
	}
	return false
}

// commentArgsWithoutText drops the text words of 'comments add ID [TEXT...]' and
// any "--", because the file's text is appended after a fresh "--" and must be
// the only text the host sees.
func commentArgsWithoutText(args []string) []string {
	pos := positionalArgs(args, func(a string) bool { return takesValue(commentsAddCmd, a) })
	drop := make([]bool, len(args))
	for _, i := range pos[min(3, len(pos)):] {
		drop[i] = true
	}
	kept := make([]string, 0, len(args))
	for i, a := range args {
		if !drop[i] && a != "--" {
			kept = append(kept, a)
		}
	}
	return kept
}

// localFileFlags rewrites a forwarded command so that no local file path reaches
// the brain host. Each local file flag becomes "<flag> -", and the file's bytes
// are returned as payload for the caller to send on stdin. 'comments add' reads
// its file path itself rather than from stdin, so for it the text is appended
// as a trailing positional argument instead. A value of "-" is already stdin
// and passes through unchanged.
func localFileFlags(args []string) (rewritten []string, payload io.Reader, err error) {
	cmd, sub := commandWords(args)
	if cmd == "comments" && sub == "add" && hasCommentFile(args) {
		args = commentArgsWithoutText(args)
	}

	out := make([]string, 0, len(args))
	var tail []string
	var content []byte
	hasPayload := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			out = append(out, args[i:]...)
			break
		}
		start := i
		name, value, inline := splitFlag(a)
		if !strings.HasPrefix(a, "-") || !localFileFlag(name, cmd, sub) || (!inline && i+1 == len(args)) {
			out = append(out, a)
			continue
		}
		if !inline {
			i++
			value = args[i]
		}
		if value == "-" {
			out = append(out, args[start:i+1]...)
			continue
		}
		data, rerr := os.ReadFile(value) //nolint:gosec // the user's own file, read on this machine
		if rerr != nil {
			return nil, nil, fmt.Errorf("reading %s %s: %w", name, value, rerr)
		}
		if cmd == "comments" && sub == "add" && isFileFlagWord(a) {
			if len(tail) > 0 {
				return nil, nil, fmt.Errorf("only one file flag can be forwarded per command")
			}
			tail = append(tail, "--", string(data))
			continue
		}
		if hasPayload {
			return nil, nil, fmt.Errorf("only one file flag can be forwarded per command")
		}
		hasPayload, content = true, data
		out = append(out, name, "-")
	}
	out = append(out, tail...)
	if !hasPayload {
		return out, nil, nil
	}
	return out, bytes.NewReader(content), nil
}

// spoolStdin copies payload into a rewound temporary file. A regular file can
// be replayed if the host moves, as forwardRemote requires of its input.
func spoolStdin(payload io.Reader) (*os.File, error) {
	f, err := os.CreateTemp("", "brain-forward-*")
	if err != nil {
		return nil, fmt.Errorf("staging file contents for the brain host: %w", err)
	}
	if _, err := io.Copy(f, payload); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("staging file contents for the brain host: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("staging file contents for the brain host: %w", err)
	}
	return f, nil
}

// forwardOnce runs the command on cfg.Host. If the host answered with a moved
// notice, it is returned alongside the exit status.
func forwardOnce(cfg *remoteConfig, name string, args []string, tty bool, stdin, stdout, stderr *os.File) (*movedNotice, int) {
	cmd := exec.Command(sshBinary(), cfg.sshArgs(tty, name, args)...) //nolint:gosec // args are quoted/validated above
	// stderr is also kept (bounded) so an ssh failure can be told apart from the
	// host command exiting 255, or a closed pipe downstream ('task list | head').
	seen := &headBuffer{limit: 4096}
	filter := &movedFilter{dst: stderr}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, io.MultiWriter(filter, seen)
	code := runRelayed(cmd, cfg, stderr, seen)
	filter.flush()
	return filter.notice, code
}

// hostMovedExit is the exit status a moved host answers with (EX_TEMPFAIL).
const hostMovedExit = 75

// hostMovedMarker starts the single stderr line a host that has handed the
// brain to another machine answers forwarded commands with. The first word
// after it is the new host's ssh target (all that older clients read), then
// key=value facts the client verifies, then a sentence for people.
const hostMovedMarker = "brain-host-moved: "

// movedFilter passes stderr through, except that a first line carrying the
// moved marker is swallowed (the client reports the move in its own words).
type movedFilter struct {
	dst     io.Writer
	buf     []byte
	decided bool
	notice  *movedNotice
}

func (f *movedFilter) Write(p []byte) (int, error) {
	if f.decided {
		return f.dst.Write(p)
	}
	f.buf = append(f.buf, p...)
	if i := bytes.IndexByte(f.buf, '\n'); i >= 0 {
		f.decided = true
		if n := parseMovedNotice(f.buf[:i]); n != nil {
			f.notice = n
			f.buf = f.buf[i+1:]
		}
		f.drain()
	} else if len(f.buf) >= len(hostMovedMarker) && !bytes.HasPrefix(f.buf, []byte(hostMovedMarker)) {
		f.decided = true
		f.drain()
	}
	return len(p), nil
}

func (f *movedFilter) drain() {
	if len(f.buf) > 0 {
		_, _ = f.dst.Write(f.buf)
	}
	f.buf = nil
}

// flush releases a held first line that never turned out to be a marker.
func (f *movedFilter) flush() {
	if !f.decided {
		f.decided = true
		if n := parseMovedNotice(bytes.TrimRight(f.buf, "\n")); n != nil {
			f.notice = n
			f.buf = nil
			return
		}
		f.drain()
	}
}

// stdinReplayable reports whether a command's input can be given to a second
// attempt: a terminal (nothing was read), a character device such as /dev/null,
// or a regular file that can be rewound to where it started.
func stdinReplayable(f *os.File, tty bool, startOffset int64) bool {
	if tty {
		return true
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	switch {
	case fi.Mode().IsRegular() && startOffset >= 0:
		_, serr := f.Seek(startOffset, io.SeekStart)
		return serr == nil
	case fi.Mode()&os.ModeCharDevice != 0:
		return true
	}
	return false
}

// headBuffer keeps the first limit bytes written to it.
type headBuffer struct {
	limit int
	buf   []byte
}

func (h *headBuffer) Write(p []byte) (int, error) {
	if room := h.limit - len(h.buf); room > 0 {
		h.buf = append(h.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

var sshFailureRE = regexp.MustCompile(`(?i)\bssh: |connect to host|could not resolve|connection (refused|timed out|closed|reset)|timed out|no route to host|network is unreachable|permission denied|host key verification|kex_exchange|banner exchange|received disconnect|mux_client|control ?socket|too many authentication`)

// looksLikeSSHFailure reports whether captured stderr reads like ssh itself
// failing to connect or authenticate.
func looksLikeSSHFailure(stderr []byte) bool { return sshFailureRE.Match(stderr) }

// runRelayed runs ssh and maps its outcome to an exit status, adding a clear
// message when the connection itself failed.
func runRelayed(cmd *exec.Cmd, cfg *remoteConfig, stderr io.Writer, seen *headBuffer) int {
	err := cmd.Run()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		code := ee.ExitCode()
		if code == sshConnectFailed && looksLikeSSHFailure(seen.buf) {
			fmt.Fprint(stderr, unreachableMessage(cfg))
		}
		return code
	}
	if errors.Is(err, exec.ErrNotFound) {
		fmt.Fprintf(stderr, "brain: client mode needs the ssh command, which was not found in PATH\n")
		return 127
	}
	fmt.Fprintf(stderr, "brain: running ssh: %v\n", err)
	return 1
}

func unreachableMessage(cfg *remoteConfig) string {
	return fmt.Sprintf(`brain: cannot reach the brain host %s over ssh.
  Nothing ran on the host if the connection failed (ssh exit 255).
  - Is the host awake and online (Tailscale up on both machines)?
  - Test the link:  ssh -o BatchMode=yes %s true
  - Config: %s   (change with 'brain remote set <ssh-target>')
  - To run against a local database instead: BRAIN_REMOTE=off <command>
`, cfg.Host, cfg.Host, remoteConfigFile())
}

// remoteCapture runs a command on the host and returns its stdout. Used by the
// 'brain remote' verbs themselves (never for forwarded user commands).
func remoteCapture(ctx context.Context, cfg *remoteConfig, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, sshBinary(), cfg.sshArgs(false, name, args)...) //nolint:gosec // validated
	var stdout, stderr bytes.Buffer
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == sshConnectFailed {
			return nil, errors.New(strings.TrimSpace(unreachableMessage(cfg)) + "\n" + strings.TrimSpace(stderr.String()))
		}
		// stdout is returned with the error: callers that parse structured
		// output from a failing host command (e.g. rewrite-wrappers) need it.
		return stdout.Bytes(), fmt.Errorf("%s on %s: %w: %s", name, cfg.Host, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// remoteShell runs a fixed sh script on the host with positional arguments.
func remoteShell(ctx context.Context, cfg *remoteConfig, script string, args ...string) ([]byte, error) {
	parts := []string{"exec", "/bin/sh", "-c", remoteQuote(script), "brain-remote"}
	for _, a := range args {
		parts = append(parts, remoteQuote(a))
	}
	a := append(cfg.sshBaseArgs(), "-T", cfg.Host, "--", strings.Join(parts, " "))
	cmd := exec.CommandContext(ctx, sshBinary(), a...) //nolint:gosec // validated
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == sshConnectFailed {
			return nil, errors.New(strings.TrimSpace(unreachableMessage(cfg)) + "\n" + strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("remote script on %s: %w: %s", cfg.Host, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// ---------------------------------------------------------------------------
// Wrappers
// ---------------------------------------------------------------------------

// remoteWrapperBody is the client wrapper for one store command. It only names
// the store and hands over to the brain binary, which does the forwarding.
func remoteWrapperBody(name, brainBinary string) string {
	return "#!/bin/sh\n" +
		remoteWrapperMarker + " generated by 'brain remote install'; re-run it to refresh, do not edit.\n" +
		"# Forwards '" + name + " …' to the brain host named in ~/.config/brain/remote.yaml.\n" +
		envRemoteStore + "=" + remoteQuote(name) + " exec " + remoteQuote(brainBinary) + " \"$@\"\n"
}

func isRemoteWrapper(path string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // inspecting our own install dir
	if err != nil {
		return false
	}
	head := string(data)
	if i := strings.IndexByte(head, '\n'); i >= 0 {
		rest := head[i+1:]
		return strings.HasPrefix(rest, remoteWrapperMarker)
	}
	return false
}

// wrapperTargetRefusal says why a generated wrapper must not exec target, or ""
// when it may. A client wrapper hands over to one real binary; if that target
// is the wrapper itself, or any other generated wrapper, every command it runs
// re-executes itself forever (the wrapper for the store named 'brain' did
// exactly that). wrapperPath is the wrapper being written; the target must be
// an existing, executable, regular file that no 'brain' command generated.
func wrapperTargetRefusal(target, wrapperPath string) string {
	if target == "" || !filepath.IsAbs(target) {
		return fmt.Sprintf("exec target %q is not an absolute path", target)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return fmt.Sprintf("exec target %s cannot be resolved: %v", target, err)
	}
	st, err := os.Stat(resolved)
	switch {
	case err != nil:
		return fmt.Sprintf("exec target %s cannot be read: %v", target, err)
	case !st.Mode().IsRegular():
		return fmt.Sprintf("exec target %s is not a regular file", target)
	case st.Mode().Perm()&0o111 == 0:
		return fmt.Sprintf("exec target %s is not executable", target)
	}
	if wrapperPath != "" {
		if wst, werr := os.Stat(wrapperPath); werr == nil && os.SameFile(st, wst) {
			return fmt.Sprintf("exec target %s is the wrapper itself (%s): it would re-execute itself forever", target, wrapperPath)
		}
		if filepath.Clean(wrapperPath) == filepath.Clean(target) || filepath.Clean(wrapperPath) == resolved {
			return fmt.Sprintf("exec target %s is the wrapper itself (%s): it would re-execute itself forever", target, wrapperPath)
		}
	}
	f, err := os.Open(resolved) //nolint:gosec // inspecting the binary a wrapper would run
	if err != nil {
		return fmt.Sprintf("exec target %s cannot be read: %v", target, err)
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	if isGeneratedWrapperBody(head[:n]) {
		return fmt.Sprintf("exec target %s is itself a generated wrapper: a wrapper must exec the real bd binary, never another wrapper", target)
	}
	return ""
}

// clientBrainBinary is the binary every client wrapper hands over to: explicit
// when given, else the one running this command (symlinks resolved). 'brain
// remote install' and 'brain stores sync' must agree on it; it is never a path
// derived from a store name, because ~/.local/bin/brain is a generated wrapper.
func clientBrainBinary(explicit string) (string, error) {
	self := explicit
	if self == "" {
		var err error
		if self, err = os.Executable(); err != nil {
			return "", fmt.Errorf("locating this binary: %w", err)
		}
		if resolved, rerr := filepath.EvalSymlinks(self); rerr == nil {
			self = resolved
		}
	}
	if abs, err := filepath.Abs(self); err == nil {
		self = abs
	}
	return self, nil
}

// installAction is the outcome for one wrapper name.
type installAction struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Action string `json:"action"` // created | updated | unchanged | skipped | removed
	Note   string `json:"note,omitempty"`
}

// installRemoteWrappers writes wrappers for names into dir. It never touches a
// file it did not generate (unless force), and never writes a file called "bd"
// or the name of the brain binary itself.
func installRemoteWrappers(dir, brainBinary string, names []string, force, dryRun bool) ([]installAction, error) {
	if !dryRun {
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a bin directory
			return nil, err
		}
	}
	self := filepath.Base(brainBinary)
	var out []installAction
	for _, name := range names {
		path := filepath.Join(dir, name)
		act := installAction{Name: name, Path: path}
		switch {
		case !remoteNameRE.MatchString(name):
			act.Action, act.Note = "skipped", "not a valid command name"
		case name == "bd":
			act.Action, act.Note = "skipped", "never shadows bd"
		case name == self && filepath.Dir(brainBinary) == dir:
			act.Action, act.Note = "skipped", "this is the brain binary itself"
		case wrapperTargetRefusal(brainBinary, path) != "":
			act.Action, act.Note = "skipped", "refused: "+wrapperTargetRefusal(brainBinary, path)
		default:
			body := remoteWrapperBody(name, brainBinary)
			existing, rerr := os.ReadFile(path) //nolint:gosec // our install dir
			switch {
			case rerr == nil && string(existing) == body:
				act.Action = "unchanged"
			case rerr == nil && !isRemoteWrapper(path) && !force:
				act.Action, act.Note = "skipped", "exists and was not generated by brain remote install (use --force to replace)"
			default:
				act.Action = "created"
				if rerr == nil {
					act.Action = "updated"
				}
				if !dryRun {
					// Replace atomically and never write through a symlink.
					tmp := path + ".brain-remote.tmp"
					if err := os.WriteFile(tmp, []byte(body), 0o755); err != nil { //nolint:gosec // executable wrapper
						return out, err
					}
					if err := os.Rename(tmp, path); err != nil {
						_ = os.Remove(tmp)
						return out, err
					}
				}
			}
		}
		out = append(out, act)
	}
	return out, nil
}

// pruneRemoteWrappers removes generated wrappers in dir whose names are no
// longer in keep. Files without the marker are never touched.
func pruneRemoteWrappers(dir string, keep []string, dryRun bool) []installAction {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	want := make(map[string]bool, len(keep))
	for _, k := range keep {
		want[k] = true
	}
	var out []installAction
	for _, e := range entries {
		if e.IsDir() || want[e.Name()] {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if info, ierr := e.Info(); ierr != nil || !info.Mode().IsRegular() || !isRemoteWrapper(path) {
			continue
		}
		if !dryRun {
			if os.Remove(path) != nil {
				continue
			}
		}
		out = append(out, installAction{Name: e.Name(), Path: path, Action: "removed"})
	}
	return out
}

// shadowNotes says, for each installed name, whether another executable of the
// same name sits elsewhere on PATH (and which one wins).
func shadowNotes(dir string, names []string, pathEnv string) []string {
	var notes []string
	dirs := filepath.SplitList(pathEnv)
	clean := filepath.Clean(dir)
	for _, name := range names {
		before, after := "", ""
		seenMine := false
		for _, d := range dirs {
			if d == "" {
				continue
			}
			if filepath.Clean(d) == clean {
				seenMine = true
				continue
			}
			p := filepath.Join(d, name)
			info, err := os.Stat(p)
			if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
				continue
			}
			switch {
			case !seenMine && before == "":
				before = p
			case seenMine && after == "":
				after = p
			}
		}
		mine := filepath.Join(dir, name)
		switch {
		case before != "":
			notes = append(notes, fmt.Sprintf("%s is shadowed by %s earlier on PATH", mine, before))
		case after != "":
			notes = append(notes, fmt.Sprintf("%s shadows %s", mine, after))
		}
	}
	return notes
}

// hostStoreNames returns the commands the host offers: every store in its
// registry that has a wrapper in its bin dir.
func hostStoreNames(ctx context.Context, cfg *remoteConfig) (names, missing []string, err error) {
	data, err := remoteCapture(ctx, cfg, "brain", "stores", "list", "--json")
	if err != nil {
		return nil, nil, fmt.Errorf("reading the host's store registry: %w", err)
	}
	registry, err := parseStoreRegistryJSON(data)
	if err != nil {
		return nil, nil, err
	}
	script := `cd "` + cfg.binDir() + `" || exit 1; for n in "$@"; do [ -x "$n" ] && [ -f "$n" ] && echo "$n"; done; exit 0`
	have, err := remoteShell(ctx, cfg, script, registry...)
	if err != nil {
		return nil, nil, fmt.Errorf("checking the host's wrappers: %w", err)
	}
	present := map[string]bool{}
	for _, l := range strings.Fields(string(have)) {
		present[l] = true
	}
	for _, r := range registry {
		if present[r] {
			names = append(names, r)
		} else {
			missing = append(missing, r)
		}
	}
	return names, missing, nil
}

// parseStoreRegistryJSON extracts store names from 'brain stores list --json'.
// The host may print other lines (migration notices) before the JSON array.
func parseStoreRegistryJSON(data []byte) ([]string, error) {
	start := bytes.IndexByte(data, '[')
	if start < 0 {
		return nil, fmt.Errorf("the host's 'brain stores list --json' printed no JSON array: %q", strings.TrimSpace(string(data)))
	}
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(bytes.NewReader(data[start:])).Decode(&rows); err != nil {
		return nil, fmt.Errorf("parsing the host's store registry: %w", err)
	}
	var names []string
	for _, r := range rows {
		if remoteNameRE.MatchString(r.Name) {
			names = append(names, r.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// ---------------------------------------------------------------------------
// brain remote …
// ---------------------------------------------------------------------------

var (
	remoteInstallDir    string
	remoteInstallBin    string
	remoteInstallForce  bool
	remoteInstallDryRun bool
	remoteInstallPrune  bool
	remoteInstallExtra  []string
	remoteSetNoCheck    bool
	remoteSetBinDir     string
)

var brainRemoteCmd = &cobra.Command{
	Use:   "remote",
	Short: "Client mode: run brain and every store command on a brain host over ssh",
	Long: `Make this machine a client of another machine's brain.

  brain remote set <ssh-target>   name the brain host (e.g. me@host.ts.net)
  brain remote install            write a wrapper per store in the host's registry
  brain remote status             show the host, reachability, latency, versions
  brain remote disconnect         close the shared ssh connection
  brain remote unset              go back to using local databases

Once a host is set, 'brain …' and every installed store command ('task create',
'inbox list', …) run the same command line on the host over ssh, through the
host's own wrappers, with arguments, stdin, TTY and exit code preserved. The
Dolt server is never reached over the network. BRAIN_REMOTE=off runs a command
locally once; BRAIN_REMOTE_HOST=<target> overrides the configured host. An
exported BEADS_DIR that names a local beads project also runs locally, unforwarded.
A bare 'brain create|update|close|delete|import' runs locally against a .beads
workspace found from the working directory (BEADS_DIR unset), and is refused
rather than forwarded otherwise, so the host's default store never absorbs a
write that named no store; reads and store commands ('task create') forward as
always.

Config: ~/.config/brain/remote.yaml. See docs/brain/REMOTE.md.`,
	Annotations: map[string]string{"brain-remote": "1"},
	Run:         func(cmd *cobra.Command, _ []string) { _ = cmd.Help() },
}

var brainRemoteSetCmd = &cobra.Command{
	Use:         "set <ssh-target>",
	Short:       "Name the brain host (an ssh target such as user@host)",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{"brain-remote": "1"},
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := loadRemoteConfig()
		if err != nil {
			FatalError("%v", err)
		}
		cfg.Host = args[0]
		if remoteSetBinDir != "" {
			cfg.BinDir = remoteSetBinDir
		}
		if err := cfg.validate(); err != nil {
			FatalError("%v", err)
		}
		if !remoteSetNoCheck {
			out, err := remoteCapture(remoteCtx(cmd), cfg, "brain", "--version")
			if err != nil {
				FatalError("%v\n(config not saved; use --no-check to save anyway)", err)
			}
			fmt.Printf("host answers: %s\n", strings.TrimSpace(string(out)))
			// The host's own store wrappers are what every forwarded command runs
			// through; rewrite and verify them now, and refuse to save a host
			// whose wrappers cannot resolve a database.
			n, err := verifyHostWrappers(remoteCtx(cmd), cfg)
			if err != nil {
				FatalError("%v\n(config not saved; use --no-check to save anyway)", err)
			}
			fmt.Printf("host wrappers: %d rewritten ($HOME-relative) and verified\n", n)
		}
		// Persist only what the file holds; an env override is not a setting.
		saved := *cfg
		if err := saveRemoteConfig(&saved); err != nil {
			FatalError("saving config: %v", err)
		}
		fmt.Printf("%s Brain host set to %s (%s)\n", "✓", cfg.Host, remoteConfigFile())
		fmt.Println("  next: brain remote install     # write the store command wrappers")
	},
}

var brainRemoteUnsetCmd = &cobra.Command{
	Use:         "unset",
	Short:       "Stop forwarding; use local databases again",
	Args:        cobra.NoArgs,
	Annotations: map[string]string{"brain-remote": "1"},
	Run: func(cmd *cobra.Command, _ []string) {
		if cfg, err := loadRemoteConfig(); err == nil && cfg.Host != "" {
			_ = closeRemoteMaster(remoteCtx(cmd), cfg)
		}
		if err := os.Remove(remoteConfigFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
			FatalError("%v", err)
		}
		fmt.Println("✓ Client mode off. Installed wrappers remain; 'brain remote install' needs a host again.")
	},
}

var brainRemoteInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install or refresh the store command wrappers from the host's registry",
	Long: `Reads the host's store registry ('brain stores list' there) and writes one
small wrapper per store into --dir (default: the directory of this binary).
Re-run it whenever the host gains a store. It only ever replaces files it
generated itself, never writes a command called 'bd', and reports any wrapper
that shadows (or is shadowed by) another command on PATH.`,
	Args:        cobra.NoArgs,
	Annotations: map[string]string{"brain-remote": "1"},
	Run: func(cmd *cobra.Command, _ []string) {
		cfg, err := loadRemoteConfig()
		if err != nil {
			FatalError("%v", err)
		}
		if err := cfg.validate(); err != nil {
			FatalError("%v", err)
		}
		self, err := clientBrainBinary(remoteInstallBin)
		if err != nil {
			FatalError("%v", err)
		}
		dir := remoteInstallDir
		if dir == "" {
			dir = filepath.Dir(self)
		}
		if abs, aerr := filepath.Abs(dir); aerr == nil {
			dir = abs
		}
		if !remoteInstallDryRun {
			n, err := verifyHostWrappers(remoteCtx(cmd), cfg)
			if err != nil {
				FatalError("%v", err)
			}
			if !jsonOutput {
				fmt.Printf("host wrappers: %d rewritten ($HOME-relative) and verified on %s\n", n, cfg.Host)
			}
		}
		names, missing, err := hostStoreNames(remoteCtx(cmd), cfg)
		if err != nil {
			FatalError("%v", err)
		}
		for _, e := range remoteInstallExtra {
			if remoteNameRE.MatchString(e) {
				names = append(names, e)
			}
		}
		names = dedupeSorted(names)
		actions, err := installRemoteWrappers(dir, self, names, remoteInstallForce, remoteInstallDryRun)
		if err != nil {
			FatalError("%v", err)
		}
		if remoteInstallPrune {
			actions = append(actions, pruneRemoteWrappers(dir, names, remoteInstallDryRun)...)
		}
		notes := shadowNotes(dir, names, os.Getenv("PATH"))

		if jsonOutput {
			outputJSON(map[string]any{
				"host": cfg.Host, "dir": dir, "binary": self, "dry_run": remoteInstallDryRun,
				"actions": actions, "host_stores_without_wrapper": missing, "path_notes": notes,
			})
			return
		}
		counts := map[string]int{}
		for _, a := range actions {
			counts[a.Action]++
			if a.Action == "skipped" {
				fmt.Printf("  skipped  %-18s %s\n", a.Name, a.Note)
			}
		}
		prefix := ""
		if remoteInstallDryRun {
			prefix = "(dry run) "
		}
		fmt.Printf("%s%s Wrappers in %s for host %s: %d created, %d updated, %d unchanged, %d skipped, %d removed\n",
			prefix, "✓", dir, cfg.Host, counts["created"], counts["updated"], counts["unchanged"], counts["skipped"], counts["removed"])
		for _, m := range missing {
			fmt.Printf("  note: host registry lists %q but the host has no %s/%s wrapper\n", m, cfg.binDir(), m)
		}
		for _, n := range notes {
			fmt.Printf("  note: %s\n", n)
		}
		if !pathContains(os.Getenv("PATH"), dir) {
			fmt.Printf("  note: %s is not on PATH; add it to use the wrappers by name\n", dir)
		}
	},
}

var brainRemoteStatusCmd = &cobra.Command{
	Use:         "status",
	Short:       "Show the configured host, reachability, latency and versions",
	Args:        cobra.NoArgs,
	Annotations: map[string]string{"brain-remote": "1"},
	Run: func(cmd *cobra.Command, _ []string) {
		cfg, err := loadRemoteConfig()
		if err != nil {
			FatalError("%v", err)
		}
		fmt.Printf("config:  %s\n", remoteConfigFile())
		if cfg.Host == "" {
			fmt.Println("mode:    local (no brain host set; 'brain remote set <ssh-target>')")
			return
		}
		if err := cfg.validate(); err != nil {
			FatalError("%v", err)
		}
		fmt.Printf("mode:    client → %s (bin dir %s)\n", cfg.Host, cfg.binDir())
		start := time.Now()
		out, err := remoteCapture(remoteCtx(cmd), cfg, "brain", "--version")
		if err != nil {
			fmt.Printf("host:    UNREACHABLE\n%v\n", err)
			os.Exit(1)
		}
		fmt.Printf("host:    reachable, %s (%d ms)\n", strings.TrimSpace(string(out)), time.Since(start).Milliseconds())
		start = time.Now()
		if _, err := remoteCapture(remoteCtx(cmd), cfg, "brain", "--version"); err == nil {
			fmt.Printf("latency: %d ms per call on the shared connection\n", time.Since(start).Milliseconds())
		}
		fmt.Printf("local:   bd version %s (%s)\n", Version, Build)
	},
}

var brainRemoteDisconnectCmd = &cobra.Command{
	Use:         "disconnect",
	Short:       "Close the shared ssh connection (the next call reopens it)",
	Args:        cobra.NoArgs,
	Annotations: map[string]string{"brain-remote": "1"},
	Run: func(cmd *cobra.Command, _ []string) {
		cfg, err := loadRemoteConfig()
		if err != nil || cfg.Host == "" {
			fmt.Println("no brain host set")
			return
		}
		if err := closeRemoteMaster(remoteCtx(cmd), cfg); err != nil {
			fmt.Println("no shared connection was open")
			return
		}
		fmt.Println("✓ Shared connection closed")
	},
}

// closeRemoteMaster asks the shared connection to exit and waits (briefly) for
// it to be gone, so a call right after does not trip over the closing socket.
func closeRemoteMaster(ctx context.Context, cfg *remoteConfig) error {
	ssh := func(op string) error {
		a := append(cfg.sshBaseArgs(), "-O", op, cfg.Host)
		return exec.CommandContext(ctx, sshBinary(), a...).Run() //nolint:gosec // validated
	}
	if err := ssh("exit"); err != nil {
		return err
	}
	for i := 0; i < 20 && ssh("check") == nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

func dedupeSorted(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func pathContains(pathEnv, dir string) bool {
	for _, d := range filepath.SplitList(pathEnv) {
		if filepath.Clean(d) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}

// isBrainRemoteVerb reports whether cmd is part of 'brain remote …', which
// never opens a store (and must not be confused with 'bd dolt remote …').
func isBrainRemoteVerb(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Annotations["brain-remote"] == "1" {
			return true
		}
	}
	return false
}

func init() {
	brainRemoteSetCmd.Flags().BoolVar(&remoteSetNoCheck, "no-check", false, "Save without contacting the host")
	brainRemoteSetCmd.Flags().StringVar(&remoteSetBinDir, "bin-dir", "", "Where the host keeps its wrappers (default $HOME/.local/bin)")
	brainRemoteInstallCmd.Flags().StringVar(&remoteInstallDir, "dir", "", "Directory for the wrappers (default: this binary's directory)")
	brainRemoteInstallCmd.Flags().StringVar(&remoteInstallBin, "bin", "", "Brain binary the wrappers exec (default: this binary)")
	brainRemoteInstallCmd.Flags().BoolVar(&remoteInstallForce, "force", false, "Replace existing files that were not generated by this command")
	brainRemoteInstallCmd.Flags().BoolVar(&remoteInstallDryRun, "dry-run", false, "Show what would change without writing")
	brainRemoteInstallCmd.Flags().BoolVar(&remoteInstallPrune, "prune", false, "Remove generated wrappers for stores the host no longer has")
	brainRemoteInstallCmd.Flags().StringSliceVar(&remoteInstallExtra, "also", nil, "Extra command names to install (host wrappers that are not registry stores)")

	brainRemoteCmd.AddCommand(brainRemoteSetCmd, brainRemoteUnsetCmd, brainRemoteInstallCmd,
		brainRemoteStatusCmd, brainRemoteDisconnectCmd)
	rootCmd.AddCommand(brainRemoteCmd)
}

// remoteCtx is the command's context, or Background when run outside Execute.
func remoteCtx(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}
