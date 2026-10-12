// Package main — brain_host_layout.go
//
// What makes a machine "the brain host", discovered rather than configured:
// which Dolt server serves the stores, where its data directory is, which
// databases the stores live in, which wrappers and links point at them, and
// which launchd jobs belong to the role. Plus the path rebasing that lets the
// same files make sense under another account's home directory.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Rebasing: the same file, another home directory
// ---------------------------------------------------------------------------

func isPathNameByte(b byte) bool {
	return b == '_' || b == '-' || b == '.' || (b >= '0' && b <= '9') || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// rebasePath maps a path under srcHome to the same relative path under dstHome.
// ok is false for paths outside srcHome, which are returned unchanged.
func rebasePath(p, srcHome, dstHome string) (string, bool) {
	if srcHome == "" || dstHome == "" {
		return p, false
	}
	srcHome = strings.TrimRight(srcHome, "/")
	dstHome = strings.TrimRight(dstHome, "/")
	switch {
	case p == srcHome:
		return dstHome, true
	case strings.HasPrefix(p, srcHome+"/"):
		return dstHome + p[len(srcHome):], true
	}
	return p, false
}

// rebaseText rewrites every occurrence of srcHome in text as a whole path
// prefix (so /Users/me does not match /Users/mellon) to dstHome.
func rebaseText(text, srcHome, dstHome string) string {
	srcHome = strings.TrimRight(srcHome, "/")
	dstHome = strings.TrimRight(dstHome, "/")
	if srcHome == "" || srcHome == dstHome {
		return text
	}
	var b strings.Builder
	for {
		i := strings.Index(text, srcHome)
		if i < 0 {
			b.WriteString(text)
			return b.String()
		}
		end := i + len(srcHome)
		if end < len(text) && isPathNameByte(text[end]) {
			b.WriteString(text[:end])
			text = text[end:]
			continue
		}
		b.WriteString(text[:i])
		b.WriteString(dstHome)
		text = text[end:]
	}
}

var wrapperPortPinRE = regexp.MustCompile(`(BEADS_DOLT_SERVER_PORT=["']?)(\d+)`)

// rebasePortPins rewrites BEADS_DOLT_SERVER_PORT=<srcPort> to dstPort. A pin
// to some other port is left alone: it was not pointing at this server.
func rebasePortPins(text string, srcPort, dstPort int) string {
	if srcPort == dstPort || srcPort == 0 || dstPort == 0 {
		return text
	}
	return wrapperPortPinRE.ReplaceAllStringFunc(text, func(m string) string {
		sub := wrapperPortPinRE.FindStringSubmatch(m)
		if sub[2] == strconv.Itoa(srcPort) {
			return sub[1] + strconv.Itoa(dstPort)
		}
		return m
	})
}

// rebaseWrapper adapts a store wrapper for the new host: paths under the old
// home move to the new home, and port pins follow the server's new port.
func rebaseWrapper(text, srcHome, dstHome string, srcPort, dstPort int) string {
	return rebasePortPins(rebaseText(text, srcHome, dstHome), srcPort, dstPort)
}

// rebaseMetadata rewrites a store's .beads/metadata.json for the new host. Only
// the fields that name a server are touched; everything else (project id,
// database name, anything a newer version adds) passes through unchanged.
func rebaseMetadata(data []byte, srcHome, dstHome string, srcPort, dstPort int) (out []byte, notes []string, err error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, nil, fmt.Errorf("parsing metadata.json: %w", err)
	}
	if raw, ok := m["dolt_server_port"]; ok {
		var p int
		if json.Unmarshal(raw, &p) == nil && p == srcPort && dstPort != srcPort {
			m["dolt_server_port"] = json.RawMessage(strconv.Itoa(dstPort))
		} else if p != 0 && p != srcPort {
			notes = append(notes, fmt.Sprintf("dolt_server_port %d is not the server being moved (%d); left as is", p, srcPort))
		}
	}
	if raw, ok := m["dolt_server_host"]; ok {
		var h string
		if json.Unmarshal(raw, &h) == nil && h != "" && !isLoopbackHost(h) {
			notes = append(notes, fmt.Sprintf("dolt_server_host %q is not loopback; left as is", h))
		}
	}
	if raw, ok := m["dolt_server_socket"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			if r, moved := rebasePath(s, srcHome, dstHome); moved {
				enc, _ := json.Marshal(r)
				m["dolt_server_socket"] = enc
			} else {
				notes = append(notes, fmt.Sprintf("dolt_server_socket %q is outside the home directory; left as is", s))
			}
		}
	}
	if raw, ok := m["dolt_data_dir"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			if r, moved := rebasePath(s, srcHome, dstHome); moved {
				enc, _ := json.Marshal(r)
				m["dolt_data_dir"] = enc
			}
		}
	}
	// Preserve the original key order is not possible through a map; re-encode
	// with sorted keys and the indentation bd itself writes.
	var buf bytes.Buffer
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	buf.WriteString("{\n")
	for i, k := range keys {
		kb, _ := json.Marshal(k)
		var vb bytes.Buffer
		if err := json.Indent(&vb, m[k], "  ", "  "); err != nil {
			vb.Reset()
			vb.Write(m[k])
		}
		buf.WriteString("  " + string(kb) + ": " + vb.String())
		if i < len(keys)-1 {
			buf.WriteString(",")
		}
		buf.WriteString("\n")
	}
	buf.WriteString("}\n")
	return buf.Bytes(), notes, nil
}

func isLoopbackHost(h string) bool {
	return h == "127.0.0.1" || h == "localhost" || h == "::1" || h == "[::1]"
}

// ---------------------------------------------------------------------------
// Layout discovery
// ---------------------------------------------------------------------------

// layoutStore is one registered store on the host being moved.
type layoutStore struct {
	Name     string `json:"name"`
	BeadsDir string `json:"beads_dir"` // as registered
	Root     string `json:"root"`      // the store directory (parent of .beads)
	Database string `json:"database"`  // metadata.json dolt_database
	Port     int    `json:"port"`      // metadata.json dolt_server_port
	Exists   bool   `json:"exists"`    // .beads/metadata.json is present
}

// layoutFile is a small text file that travels with the role.
type layoutFile struct {
	Path    string `json:"path"`    // absolute path
	Name    string `json:"name"`    // base name
	Content string `json:"content"` // as found on this machine
	Mode    uint32 `json:"mode"`
}

// layoutLink is a symlink in an agent bin directory that points into the wrapper directory.
type layoutLink struct {
	Path   string `json:"path"`
	Target string `json:"target"`
}

// layoutService is a launchd job that might belong to the role.
type layoutService struct {
	Label    string   `json:"label"`
	Plist    string   `json:"plist"`
	Reason   string   `json:"reason"`
	Disabled bool     `json:"disabled,omitempty"`
	Files    []string `json:"files,omitempty"` // files under home its arguments name
	Declared bool     `json:"declared,omitempty"`
}

// listenerInfo describes the process holding a TCP port.
type listenerInfo struct {
	PID            int
	Args           []string
	DataDir        string
	Bind           string
	Port           int
	RemotesAPIPort int
}

// hostLayout is everything a promotion needs to know about the machine it
// moves the role away from.
type hostLayout struct {
	Home      string
	BinDir    string
	Stores    []layoutStore
	Missing   []layoutStore // registered but not on disk
	Databases []string      // distinct databases the stores live in (+ requested extras)
	Server    hostServer
	Foreign   []string // other databases on the same server (not moved)
	Wrappers  []layoutFile
	Helpers   []layoutFile
	Links     []layoutLink
	Services  []layoutService
	DoltCfg   bool
	Warnings  []string
}

func (l *hostLayout) storeNames() []string {
	out := make([]string, 0, len(l.Stores))
	for _, s := range l.Stores {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

// discoverLayout reads the registry, the stores' metadata, the running server,
// the wrappers and the launchd jobs of the machine at home.
func discoverLayout(ctx context.Context, home string, opts promoteOptions) (*hostLayout, error) {
	l := &hostLayout{Home: home, BinDir: filepath.Join(home, ".local", "bin")}

	reg, err := loadStoresRegistry()
	if err != nil {
		return nil, fmt.Errorf("reading the store registry: %w", err)
	}
	if len(reg) == 0 {
		return nil, errors.New("this machine has no stores registered (~/.config/brain/stores.yaml): there is no brain here to move")
	}
	names := make([]string, 0, len(reg))
	for n := range reg {
		names = append(names, n)
	}
	sort.Strings(names)

	portVotes := map[int]int{}
	dbSet := map[string]bool{}
	for _, n := range names {
		st := layoutStore{Name: n, BeadsDir: reg[n].Path}
		// The registry usually names the .beads directory; some entries name
		// the store directory itself, and the wrapper pins <that>/.beads.
		if filepath.Base(st.BeadsDir) != ".beads" {
			if fi, serr := os.Stat(filepath.Join(st.BeadsDir, ".beads", "metadata.json")); serr == nil && fi.Mode().IsRegular() {
				st.BeadsDir = filepath.Join(st.BeadsDir, ".beads")
			}
		}
		st.Root = filepath.Dir(st.BeadsDir)
		if filepath.Base(st.BeadsDir) != ".beads" {
			l.Warnings = append(l.Warnings, fmt.Sprintf("store %s: registered path %s is not a .beads directory and holds none; its directory is not moved", n, st.BeadsDir))
			l.Missing = append(l.Missing, st)
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(st.BeadsDir, "metadata.json")) //nolint:gosec // registered store
		if rerr != nil {
			l.Missing = append(l.Missing, st)
			continue
		}
		var md struct {
			Mode     string `json:"dolt_mode"`
			Database string `json:"dolt_database"`
			Port     int    `json:"dolt_server_port"`
		}
		if json.Unmarshal(raw, &md) != nil {
			l.Warnings = append(l.Warnings, fmt.Sprintf("store %s: unreadable metadata.json", n))
			l.Missing = append(l.Missing, st)
			continue
		}
		st.Exists = true
		st.Database, st.Port = md.Database, md.Port
		if md.Mode != "server" {
			l.Warnings = append(l.Warnings, fmt.Sprintf("store %s is not in server mode (dolt_mode=%q): it has its own embedded database, which promotion does not move", n, md.Mode))
			continue
		}
		if _, rel := rebasePath(st.Root, home, "/x"); !rel {
			l.Warnings = append(l.Warnings, fmt.Sprintf("store %s lives outside the home directory (%s)", n, st.Root))
		}
		l.Stores = append(l.Stores, st)
		portVotes[st.Port]++
		if st.Database != "" {
			dbSet[st.Database] = true
		}
	}
	if len(l.Stores) == 0 {
		return nil, errors.New("none of the registered stores is a server-mode store on disk: nothing to move")
	}

	// The server: what host.yaml recorded, else what is actually listening.
	cfg, err := loadHostConfig(home)
	if err != nil {
		return nil, err
	}
	switch {
	case cfg != nil && cfg.Server != nil && cfg.Server.Port > 0:
		l.Server = *cfg.Server
	default:
		port := 0
		for p, v := range portVotes {
			if port == 0 || v > portVotes[port] || (v == portVotes[port] && p < port) {
				port = p
			}
		}
		l.Server.Port = port
	}
	if opts.SourcePort > 0 {
		l.Server.Port = opts.SourcePort
	}
	if len(portVotes) > 1 {
		l.Warnings = append(l.Warnings, fmt.Sprintf("stores point at %d different server ports; moving the server on port %d", len(portVotes), l.Server.Port))
	}
	if li := findListener(l.Server.Port); li != nil {
		if l.Server.DataDir == "" {
			l.Server.DataDir = li.DataDir
		}
		if l.Server.Bind == "" {
			l.Server.Bind = li.Bind
		}
		if l.Server.RemotesAPIPort == 0 {
			l.Server.RemotesAPIPort = li.RemotesAPIPort
		}
		if l.Server.Service.Kind == "" {
			l.Server.Service = detectService(home, li)
		}
	}
	if opts.SourceDataDir != "" {
		l.Server.DataDir = opts.SourceDataDir
	}
	if l.Server.DataDir == "" {
		return nil, fmt.Errorf("cannot tell where the Dolt server on port %d keeps its data (is it running? on this machine?): pass --source-data-dir", l.Server.Port)
	}
	if l.Server.Bind == "" {
		l.Server.Bind = "127.0.0.1"
	}
	if l.Server.Service.Kind == "" {
		l.Server.Service = hostService{Kind: "external"}
	}

	// Databases to move.
	for d := range dbSet {
		l.Databases = append(l.Databases, d)
	}
	for _, d := range opts.ExtraDatabases {
		if !dbSet[d] {
			l.Databases = append(l.Databases, d)
			dbSet[d] = true
		}
	}
	sort.Strings(l.Databases)
	l.Server.Databases = l.Databases

	if all, lerr := listDatabases(ctx, l.Server.Port); lerr == nil {
		have := map[string]bool{}
		for _, d := range all {
			have[d] = true
		}
		for _, d := range l.Databases {
			if !have[d] {
				return nil, fmt.Errorf("database %q is used by the stores but the server on port %d does not have it", d, l.Server.Port)
			}
		}
		if opts.IncludeLegacy {
			for _, d := range all {
				if !dbSet[d] {
					l.Databases = append(l.Databases, d)
				}
			}
			sort.Strings(l.Databases)
			l.Server.Databases = l.Databases
		} else {
			for _, d := range all {
				if !dbSet[d] {
					l.Foreign = append(l.Foreign, d)
				}
			}
		}
	} else {
		return nil, fmt.Errorf("the Dolt server on port %d is not answering: %w", l.Server.Port, lerr)
	}
	if st, serr := os.Stat(filepath.Join(l.Server.DataDir, ".doltcfg")); serr == nil && st.IsDir() {
		l.DoltCfg = true
	}

	l.scanWrappers(opts)
	l.scanLinks(opts)
	l.scanServices(ctx, cfg, opts)
	return l, nil
}

var sourcedHelperRE = regexp.MustCompile(`(?m)^\s*(?:\.|source)\s+"?(?:\$HOME|\$\{HOME\}|~)/\.local/bin/([A-Za-z0-9._-]+)"?`)

func looksLikeScript(data []byte) bool { return bytes.HasPrefix(data, []byte("#!")) }

func (l *hostLayout) scanWrappers(opts promoteOptions) {
	entries, err := os.ReadDir(l.BinDir)
	if err != nil {
		return
	}
	skip := map[string]bool{}
	for _, s := range opts.SkipWrappers {
		skip[s] = true
	}
	helperNames := map[string]bool{}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || info.Size() > 64*1024 || info.Mode()&0o111 == 0 {
			continue
		}
		path := filepath.Join(l.BinDir, e.Name())
		data, rerr := os.ReadFile(path) //nolint:gosec // our own bin directory
		if rerr != nil || !looksLikeScript(data) {
			continue
		}
		if isRemoteWrapper(path) || !bytes.Contains(data, []byte("BEADS_DIR")) {
			continue
		}
		if skip[e.Name()] {
			continue
		}
		l.Wrappers = append(l.Wrappers, layoutFile{Path: path, Name: e.Name(), Content: string(data), Mode: uint32(info.Mode().Perm())})
		for _, m := range sourcedHelperRE.FindAllSubmatch(data, -1) {
			helperNames[string(m[1])] = true
		}
	}
	for name := range helperNames {
		path := filepath.Join(l.BinDir, name)
		info, ierr := os.Lstat(path)
		if ierr != nil || !info.Mode().IsRegular() || info.Size() > 64*1024 {
			continue
		}
		data, rerr := os.ReadFile(path) //nolint:gosec // our own bin directory
		if rerr != nil {
			continue
		}
		l.Helpers = append(l.Helpers, layoutFile{Path: path, Name: name, Content: string(data), Mode: uint32(info.Mode().Perm())})
	}
	sort.Slice(l.Wrappers, func(i, j int) bool { return l.Wrappers[i].Name < l.Wrappers[j].Name })
	sort.Slice(l.Helpers, func(i, j int) bool { return l.Helpers[i].Name < l.Helpers[j].Name })
}

func (l *hostLayout) scanLinks(opts promoteOptions) {
	dirs := opts.LinkDirs
	if len(dirs) == 0 {
		dirs = []string{filepath.Join(l.Home, ".pi", "agent", "bin")}
	}
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Type()&os.ModeSymlink == 0 {
				continue
			}
			path := filepath.Join(d, e.Name())
			target, rerr := os.Readlink(path)
			if rerr != nil {
				continue
			}
			abs := target
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(d, target)
			}
			if _, inBin := rebasePath(filepath.Clean(abs), l.BinDir, "/x"); !inBin {
				continue
			}
			l.Links = append(l.Links, layoutLink{Path: path, Target: target})
		}
	}
	sort.Slice(l.Links, func(i, j int) bool { return l.Links[i].Path < l.Links[j].Path })
}

// launchdPlist is the part of a LaunchAgent plist this tool reads.
type launchdPlist struct {
	Label            string            `json:"Label"`
	ProgramArguments []string          `json:"ProgramArguments"`
	Program          string            `json:"Program"`
	Env              map[string]string `json:"EnvironmentVariables"`
}

// readPlistJSON converts a plist to JSON with plutil (macOS only).
func readPlistJSON(path string) (*launchdPlist, error) {
	out, err := exec.Command("plutil", "-convert", "json", "-o", "-", path).Output() //nolint:gosec // fixed tool, path from a directory listing
	if err != nil {
		return nil, err
	}
	var p launchdPlist
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (l *hostLayout) scanServices(_ context.Context, cfg *hostConfig, opts promoteOptions) {
	if runtime.GOOS != "darwin" {
		return
	}
	declared := map[string]bool{}
	if cfg != nil {
		for _, s := range cfg.Services {
			declared[s] = true
		}
	}
	for _, s := range opts.Services {
		declared[s] = true
	}
	dir := filepath.Join(l.Home, "Library", "LaunchAgents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	disabled := launchdDisabledLabels()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".plist") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		p, perr := readPlistJSON(path)
		if perr != nil || p.Label == "" {
			continue
		}
		if p.Label == l.Server.Service.Label {
			continue // the server itself is handled as the server
		}
		reason := serviceRelevance(p, l)
		if reason == "" && !declared[p.Label] {
			continue
		}
		svc := layoutService{Label: p.Label, Plist: path, Reason: reason, Disabled: disabled[p.Label], Declared: declared[p.Label]}
		for _, a := range append(append([]string{}, p.ProgramArguments...), p.Program) {
			if rel, ok := rebasePath(a, l.Home, "/x"); ok && rel != "/x" {
				if st, serr := os.Stat(a); serr == nil && st.Mode().IsRegular() && st.Size() < 1<<20 {
					svc.Files = append(svc.Files, a)
				}
			}
		}
		l.Services = append(l.Services, svc)
	}
	sort.Slice(l.Services, func(i, j int) bool { return l.Services[i].Label < l.Services[j].Label })
}

// serviceRelevance says why a launchd job looks like it belongs to the brain
// host ("" when it does not).
func serviceRelevance(p *launchdPlist, l *hostLayout) string {
	hay := append(append([]string{p.Label, p.Program}, p.ProgramArguments...), mapValues(p.Env)...)
	joined := strings.Join(hay, " ")
	switch {
	case l.Server.DataDir != "" && strings.Contains(joined, l.Server.DataDir):
		return "names the Dolt data directory"
	case strings.Contains(joined, "-P "+strconv.Itoa(l.Server.Port)) || strings.Contains(joined, "--port "+strconv.Itoa(l.Server.Port)):
		return fmt.Sprintf("uses the server port %d", l.Server.Port)
	}
	for _, w := range l.Wrappers {
		if strings.Contains(joined, filepath.Join(".local", "bin", w.Name)) {
			return "runs the " + w.Name + " store wrapper"
		}
	}
	low := strings.ToLower(joined)
	for _, kw := range []string{"brain", "beads"} {
		if strings.Contains(low, kw) {
			return "mentions " + kw
		}
	}
	return ""
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// launchdDisabledLabels reads `launchctl print-disabled` for the user domain.
func launchdDisabledLabels() map[string]bool {
	out, err := exec.Command("launchctl", "print-disabled", "gui/"+strconv.Itoa(os.Getuid())).Output()
	if err != nil {
		return nil
	}
	res := map[string]bool{}
	re := regexp.MustCompile(`"([^"]+)"\s*=>\s*(disabled|true)`)
	for _, m := range re.FindAllStringSubmatch(string(out), -1) {
		res[m[1]] = true
	}
	return res
}

// findListener finds the process listening on a TCP port and parses the Dolt
// server arguments out of its command line. nil when nothing listens or the
// tools to ask (lsof, ps) are missing.
func findListener(port int) *listenerInfo {
	if port <= 0 {
		return nil
	}
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-t").Output()
	if err != nil {
		return nil
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return nil
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return nil
	}
	psOut, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // pid from lsof
	if err != nil {
		return nil
	}
	li := &listenerInfo{PID: pid, Port: port, Args: strings.Fields(strings.TrimSpace(string(psOut)))}
	li.parseDoltArgs()
	return li
}

func (li *listenerInfo) parseDoltArgs() {
	args := li.Args
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "--data-dir":
			li.DataDir = val()
		case strings.HasPrefix(a, "--data-dir="):
			li.DataDir = strings.TrimPrefix(a, "--data-dir=")
		case a == "-H" || a == "--host":
			li.Bind = val()
		case strings.HasPrefix(a, "--host="):
			li.Bind = strings.TrimPrefix(a, "--host=")
		case a == "--remotesapi-port":
			li.RemotesAPIPort, _ = strconv.Atoi(val())
		case strings.HasPrefix(a, "--remotesapi-port="):
			li.RemotesAPIPort, _ = strconv.Atoi(strings.TrimPrefix(a, "--remotesapi-port="))
		}
	}
}

// detectService finds the launchd job (if any) that runs the listener's server.
func detectService(home string, li *listenerInfo) hostService {
	if runtime.GOOS == "darwin" {
		dir := filepath.Join(home, "Library", "LaunchAgents")
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".plist") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			p, err := readPlistJSON(path)
			if err != nil {
				continue
			}
			joined := strings.Join(append(append([]string{}, p.ProgramArguments...), p.Program), " ")
			if strings.Contains(joined, "sql-server") && li.DataDir != "" && strings.Contains(joined, li.DataDir) {
				return hostService{Kind: "launchd", Label: p.Label, Plist: path}
			}
		}
	}
	// A server started by 'brain host' itself leaves a pid file.
	pidFile := filepath.Join(home, ".config", "brain", "dolt-server.pid")
	if pid := readPid(pidFile); pid > 0 && pid == li.PID {
		return hostService{Kind: "process", PidFile: pidFile, LogFile: filepath.Join(home, ".config", "brain", "dolt-server.log")}
	}
	return hostService{Kind: "external"}
}

// hostPathExists is a small helper for plan checks.
func hostPathExists(p string) bool {
	_, err := os.Lstat(p)
	return !errors.Is(err, fs.ErrNotExist) && err == nil
}
