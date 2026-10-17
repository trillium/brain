// Package main — brain_client_registry.go
//
// Client heartbeats: which machines have connected to this brain host.
//
// A client in client mode (see brain_remote.go) names itself in the environment
// of every forwarded call: $BRAIN_REMOTE_CLIENT (user@machine), the brain host
// target it used ($BRAIN_REMOTE_CLIENT_HOST) and its version
// ($BRAIN_REMOTE_CLIENT_VERSION). The host folds each observation into one
// small record per client in ~/.config/brain/clients.yaml, next to host.yaml.
// There is no database and no new schema, and nothing to configure on either
// side: the record appears the first time a client runs a command.
//
// Recording is best-effort and throttled. A client that runs fifty commands in
// a minute costs one file write, the last-seen time is accurate to about a
// minute, and a heartbeat that cannot be written never fails the command the
// client asked for. 'brain host clients list' reads this file together with the
// registered-clients list in host.yaml (the ssh targets a promotion points at
// the new host).
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// clientRegistryRel is the heartbeat file, relative to $HOME.
	clientRegistryRel = ".config/brain/clients.yaml"

	// The environment a client sets on the host end of a forwarded call.
	envRemoteClient        = "BRAIN_REMOTE_CLIENT"
	envRemoteClientHost    = "BRAIN_REMOTE_CLIENT_HOST"
	envRemoteClientVersion = "BRAIN_REMOTE_CLIENT_VERSION"

	// clientHeartbeatInterval is how stale a record must be before another
	// forwarded call rewrites the file.
	clientHeartbeatInterval = time.Minute
	// clientRegistryMax bounds the file: beyond it the least recently seen
	// clients are dropped, so a stray or hostile client cannot grow it forever.
	clientRegistryMax = 200
	// clientFieldMax bounds one reported field, so a client cannot write a huge
	// value into the file.
	clientFieldMax = 200
)

// clientRecord is one client, as last reported by that client itself.
type clientRecord struct {
	// ID is the client's own name for itself: "user@machine".
	ID string `yaml:"id" json:"id"`
	// Host is the brain host target the client pointed at when it called. It
	// can lag behind a move, which is exactly what an operator wants to see.
	Host string `yaml:"host,omitempty" json:"host,omitempty"`
	// Version is the client's own build (e.g. "1.1.0+brain.0.5.1").
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
	// FirstSeen and LastSeen are RFC3339 UTC stamps of recorded observations.
	FirstSeen string `yaml:"first_seen,omitempty" json:"first_seen,omitempty"`
	LastSeen  string `yaml:"last_seen" json:"last_seen"`
}

// clientRegistry is ~/.config/brain/clients.yaml.
type clientRegistry struct {
	Clients []clientRecord `yaml:"clients,omitempty" json:"clients,omitempty"`
}

func clientRegistryFile(home string) string { return filepath.Join(home, clientRegistryRel) }

const clientRegistryHeader = "# brain client heartbeats — written by 'brain' on this host when a client runs a command through 'brain remote'.\n" +
	"# Read with 'brain host clients list'. Safe to delete; clients re-register themselves.\n"

func loadClientRegistry(home string) (*clientRegistry, error) {
	reg := &clientRegistry{}
	data, err := os.ReadFile(clientRegistryFile(home)) //nolint:gosec // our own config file
	if errors.Is(err, fs.ErrNotExist) {
		return reg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", clientRegistryFile(home), err)
	}
	if err := yaml.Unmarshal(data, reg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", clientRegistryFile(home), err)
	}
	return reg, nil
}

func saveClientRegistry(home string, reg *clientRegistry) error {
	data, err := yaml.Marshal(reg)
	if err != nil {
		return err
	}
	return hostWriteFile(clientRegistryFile(home), append([]byte(clientRegistryHeader), data...), 0o644) //nolint:gosec // no secrets here
}

// validClientField reports whether a value a client reported is safe to keep,
// show and embed in a shell word: printable, bounded, and never empty.
func validClientField(s string) bool {
	if s == "" || len(s) > clientFieldMax {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// clientIdentityFrom names a machine the way the host records it: the login
// name and the machine's hostname. It is what 'brain host clients list' shows,
// so it is deliberately something a person recognizes.
func clientIdentityFrom(user, host string) string {
	user, host = strings.TrimSpace(user), strings.TrimSpace(host)
	if user == "" {
		user = "unknown"
	}
	if host == "" {
		host = "unknown"
	}
	id := user + "@" + host
	if !validClientField(id) {
		return ""
	}
	return id
}

// clientIdentity names this machine to the brain host, from the environment.
func clientIdentity() string {
	user := os.Getenv("USER")
	if strings.TrimSpace(user) == "" {
		user = os.Getenv("LOGNAME")
	}
	host, err := os.Hostname()
	if err != nil {
		host = ""
	}
	return clientIdentityFrom(user, host)
}

// applyClientHeartbeat folds one observation into reg and reports whether the
// file is worth rewriting. A repeat inside clientHeartbeatInterval with the same
// host and version changes nothing, which is what keeps a chatty client cheap.
func applyClientHeartbeat(reg *clientRegistry, rec clientRecord, now time.Time) bool {
	stamp := now.UTC().Format(hostTimeFormat)
	for i := range reg.Clients {
		c := &reg.Clients[i]
		if c.ID != rec.ID {
			continue
		}
		changed := c.Host != rec.Host || c.Version != rec.Version || c.FirstSeen == ""
		if !changed {
			last, err := time.Parse(hostTimeFormat, c.LastSeen)
			changed = err != nil || now.Sub(last) >= clientHeartbeatInterval
		}
		if !changed {
			return false
		}
		c.Host, c.Version, c.LastSeen = rec.Host, rec.Version, stamp
		if c.FirstSeen == "" {
			c.FirstSeen = stamp
		}
		return true
	}
	rec.FirstSeen, rec.LastSeen = stamp, stamp
	reg.Clients = append(reg.Clients, rec)
	sort.Slice(reg.Clients, func(i, j int) bool { return reg.Clients[i].ID < reg.Clients[j].ID })
	trimClientRegistry(reg)
	return true
}

// trimClientRegistry drops the least recently seen clients beyond the cap.
func trimClientRegistry(reg *clientRegistry) {
	if len(reg.Clients) <= clientRegistryMax {
		return
	}
	byAge := append([]clientRecord(nil), reg.Clients...)
	sort.SliceStable(byAge, func(i, j int) bool { return byAge[i].LastSeen > byAge[j].LastSeen })
	keep := map[string]bool{}
	for _, c := range byAge[:clientRegistryMax] {
		keep[c.ID] = true
	}
	out := reg.Clients[:0]
	for _, c := range reg.Clients {
		if keep[c.ID] {
			out = append(out, c)
		}
	}
	reg.Clients = out
}

// recordClientHeartbeat notes that a client ran a command here. It is called
// from main before any command machinery starts, and never fails the command:
// when the file cannot be read or written the heartbeat is simply lost.
func recordClientHeartbeat() {
	if os.Getenv(envRemoteForward) != "1" || hostGateOff() {
		return
	}
	id := strings.TrimSpace(os.Getenv(envRemoteClient))
	if !validClientField(id) || !strings.Contains(id, "@") {
		return
	}
	rec := clientRecord{ID: id}
	if v := strings.TrimSpace(os.Getenv(envRemoteClientHost)); validClientField(v) {
		rec.Host = v
	}
	if v := strings.TrimSpace(os.Getenv(envRemoteClientVersion)); validClientField(v) {
		rec.Version = v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return
	}
	reg, err := loadClientRegistry(home)
	if err != nil {
		return // a damaged heartbeat file must not take every command down
	}
	if !applyClientHeartbeat(reg, rec, time.Now()) {
		return
	}
	_ = saveClientRegistry(home, reg)
}

// ---------------------------------------------------------------------------
// 'brain host clients list'
// ---------------------------------------------------------------------------

// clientRow is one line of the client list: a machine that is registered (so a
// promotion can reach it), seen connecting (a heartbeat), or both.
type clientRow struct {
	// Client is what to call the machine: its own name for itself when it has
	// connected, otherwise the ssh target it was registered under.
	Client string `json:"client"`
	// Host is the brain host the client last pointed at.
	Host string `json:"host,omitempty"`
	// Version is the client's build.
	Version string `json:"version,omitempty"`
	// FirstSeen and LastSeen are the heartbeat stamps; empty when never seen.
	FirstSeen string `json:"first_seen,omitempty"`
	LastSeen  string `json:"last_seen,omitempty"`
	// Registered is true when host.yaml lists the machine, so 'brain host
	// promote' will point it at a new host.
	Registered bool `json:"registered"`
	// Target is the registered ssh target (empty for a client seen but never
	// registered). Added is when it was registered.
	Target string `json:"ssh_target,omitempty"`
	Added  string `json:"added,omitempty"`
	// Seen is true when a heartbeat recorded this machine.
	Seen bool `json:"seen"`
}

// clientHostLabel reduces an ssh target or a client id to the machine name an
// operator would recognize: the part before the first dot, without the user.
func clientHostLabel(s string) string {
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// mergeClientRows joins the registered clients with the observed heartbeats.
// A registered machine is matched to a heartbeat by the client's own name or,
// when that is unique, by the machine name both share (registered as
// "me@mac-mini.ts.net", seen as "trillium@mac-mini"). Everything unmatched is
// still listed: a client that connected without being registered is exactly
// what an operator wants to notice.
func mergeClientRows(cfg *hostConfig, reg *clientRegistry) []clientRow {
	var registered []hostClient
	if cfg != nil {
		registered = cfg.Clients
	}
	var observed []clientRecord
	if reg != nil {
		observed = reg.Clients
	}
	labelCount := map[string]int{}
	for _, c := range registered {
		if l := clientHostLabel(c.Target); l != "" {
			labelCount[l]++
		}
	}
	used := make([]bool, len(observed))
	match := func(target string) int {
		key := strings.ToLower(strings.TrimSpace(target))
		for i, o := range observed {
			if !used[i] && strings.ToLower(o.ID) == key {
				return i
			}
		}
		label := clientHostLabel(target)
		if label == "" || labelCount[label] != 1 {
			return -1 // a machine name shared by two registered clients proves nothing
		}
		for i, o := range observed {
			if !used[i] && clientHostLabel(o.ID) == label {
				return i
			}
		}
		return -1
	}
	rows := make([]clientRow, 0, len(registered)+len(observed))
	for _, c := range registered {
		row := clientRow{Client: c.Target, Target: c.Target, Added: c.Added, Registered: true}
		if i := match(c.Target); i >= 0 {
			used[i] = true
			o := observed[i]
			row.Client, row.Host, row.Version = o.ID, o.Host, o.Version
			row.FirstSeen, row.LastSeen, row.Seen = o.FirstSeen, o.LastSeen, true
		}
		rows = append(rows, row)
	}
	for i, o := range observed {
		if used[i] {
			continue
		}
		rows = append(rows, clientRow{
			Client: o.ID, Host: o.Host, Version: o.Version,
			FirstSeen: o.FirstSeen, LastSeen: o.LastSeen, Seen: true,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if (rows[i].LastSeen != "") != (rows[j].LastSeen != "") {
			return rows[i].LastSeen != ""
		}
		if rows[i].LastSeen != rows[j].LastSeen {
			return rows[i].LastSeen > rows[j].LastSeen
		}
		return rows[i].Client < rows[j].Client
	})
	return rows
}

// clientListHost is the name of the machine the list is about.
func clientListHost() string {
	if id := clientIdentity(); id != "" {
		return id
	}
	return "this machine"
}

// humanAgo renders a heartbeat stamp the way an operator reads it: how long
// ago the client was last seen. An unparseable stamp is shown as it is.
func humanAgo(stamp string, now time.Time) string {
	t, err := time.Parse(hostTimeFormat, stamp)
	if err != nil {
		return stamp
	}
	d := now.UTC().Sub(t.UTC())
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.UTC().Format("2006-01-02")
}

// thisClientIndex finds the row that is the machine running the command: the
// client's own name for itself first, then a registered ssh target that shares
// its machine name (registered "me@laptop.ts.net", seen "trillium@laptop").
// It returns -1 when nothing matches or when more than one row would, so the
// caller never labels the wrong machine.
func thisClientIndex(rows []clientRow, you string) int {
	if you == "" {
		return -1
	}
	for i, r := range rows {
		if strings.EqualFold(r.Client, you) {
			return i
		}
	}
	label := clientHostLabel(you)
	if label == "" {
		return -1
	}
	found := -1
	for i, r := range rows {
		if clientHostLabel(r.Client) != label {
			continue
		}
		if found >= 0 {
			return -1 // two rows share the machine name: refuse to guess
		}
		found = i
	}
	return found
}

// findClientRows returns the rows an operator named on the command line, so
// 'brain host clients remove' acts on exactly the machine the list showed. A
// name is matched the way mergeClientRows joins the two halves: the client's
// own id, its registered ssh target, or — when exactly one row owns it — the
// machine name both share. An empty result means nothing matched; the caller
// decides whether that is an error.
func findClientRows(rows []clientRow, arg string) []int {
	name := strings.ToLower(strings.TrimSpace(arg))
	if name == "" {
		return nil
	}
	var exact []int
	for i, r := range rows {
		if strings.ToLower(strings.TrimSpace(r.Client)) == name ||
			(r.Target != "" && strings.ToLower(strings.TrimSpace(r.Target)) == name) {
			exact = append(exact, i)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	// Fall back to the machine name, but only when exactly one row owns it:
	// guessing between two machines is how the wrong client gets forgotten.
	label := clientHostLabel(arg)
	if label == "" {
		return nil
	}
	found := -1
	for i, r := range rows {
		if clientHostLabel(r.Client) != label {
			continue
		}
		if found >= 0 {
			return nil
		}
		found = i
	}
	if found < 0 {
		return nil
	}
	return []int{found}
}

// clientListText renders the human table for the host that owns the list.
func clientListText(host string, rows []clientRow, now time.Time) string {
	return clientListTextFor(host, rows, now, "")
}

// clientListTextFor renders the human table, including the empty state that
// says how a client gets recorded. you, when not empty, marks the row for the
// machine running the command, which is what a client sees when it asks the
// host who is connected.
func clientListTextFor(host string, rows []clientRow, now time.Time, you string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "brain host clients — %s\n", host)
	if len(rows) == 0 {
		b.WriteString(`
No clients yet. Two ways a machine shows up here:

  register it, so 'brain host promote' can point it at a new host:
      brain host clients add <ssh-target>

  or let it connect: it records itself the first time it runs any command
  through 'brain remote' — nothing to configure on the client.

A client that is only registered shows "never seen"; one that is only seen is
listed as not registered.`)
		return b.String()
	}
	fmt.Fprintf(&b, "\n  %-26s %-20s %-18s %-18s %s\n", "CLIENT", "VERSION", "LAST SEEN", "HOST", "REGISTERED")
	this := thisClientIndex(rows, you)
	for i, r := range rows {
		version := r.Version
		if version == "" {
			version = "—"
		}
		host := r.Host
		if host == "" {
			host = "—"
		}
		seen := "never seen"
		if r.Seen {
			seen = humanAgo(r.LastSeen, now)
		}
		registered := "no (seen connecting)"
		switch {
		case r.Registered && r.Target != r.Client:
			registered = r.Target
		case r.Registered:
			registered = "yes"
		}
		mark := ""
		if i == this {
			mark = "  ← this machine"
		}
		fmt.Fprintf(&b, "  %-26s %-20s %-18s %-18s %s%s\n", r.Client, version, seen, host, registered, mark)
	}
	if len(rows) > 0 {
		b.WriteString("\nRegister a machine with 'brain host clients add <ssh-target>'; forget one with 'brain host clients remove <client>'.\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// clientListPayload is the --json shape. It names the file it read, so an
// operator (or a script) can tell an empty list from an unread one.
func clientListPayload(home, host string, rows []clientRow, registered, seen int) map[string]any {
	return map[string]any{
		"host":            host,
		"config":          hostConfigFile(home),
		"clients_config":  clientRegistryFile(home),
		"clients":         rows,
		"registered":      registered,
		"seen":            seen,
		"heartbeat_after": clientHeartbeatInterval.String(),
	}
}
