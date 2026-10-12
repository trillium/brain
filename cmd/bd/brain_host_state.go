// Package main — brain_host_state.go
//
// The host role is machine state, not database state. Each machine keeps a
// small file, ~/.config/brain/host.yaml, that says what role it plays right
// now, and each promotion keeps a state file and an undo journal. Nothing here
// touches a database; everything is plain files written atomically, so a crash
// at any instant leaves either the old file or the new one.
//
//	role: host     this machine serves the brain (the default for a machine
//	               that runs the Dolt server and has no host.yaml at all)
//	role: frozen   a promotion has paused writes here; reads still work
//	role: moved    the brain lives elsewhere now (moved_to); this machine is
//	               a client of the new host and redirects stragglers
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	// hostConfigRel is the per-machine role file, relative to $HOME.
	hostConfigRel = ".config/brain/host.yaml"
	// promoteRootRel holds one directory per promotion on the machine that
	// started it (the old host): state.json, journal.jsonl, staging output.
	promoteRootRel = ".config/brain/promote"
	// incomingRootRel holds one directory per promotion on the receiving
	// machine (the new host): incoming.json and its own journal.
	incomingRootRel = ".config/brain/promote-incoming"

	// hostProtocol is the version of the source/target peer protocol. Both ends
	// must speak the same one; the binary version may differ.
	hostProtocol = 1

	hostTimeFormat = time.RFC3339
)

type hostRole string

const (
	roleHost   hostRole = "host"
	roleFrozen hostRole = "frozen"
	roleMoved  hostRole = "moved"
)

// hostService says how the Dolt server on this machine is kept running.
type hostService struct {
	// Kind is "launchd" (macOS job), "process" (a detached dolt the tool
	// started; not restarted at login), or "external" (something else starts
	// it; the tool only checks that it answers).
	Kind    string `yaml:"kind" json:"kind"`
	Label   string `yaml:"label,omitempty" json:"label,omitempty"`
	Plist   string `yaml:"plist,omitempty" json:"plist,omitempty"`
	PidFile string `yaml:"pid_file,omitempty" json:"pid_file,omitempty"`
	LogFile string `yaml:"log_file,omitempty" json:"log_file,omitempty"`
}

// hostServer is the Dolt server this machine serves the brain from.
type hostServer struct {
	DataDir        string      `yaml:"data_dir" json:"data_dir"`
	Bind           string      `yaml:"bind" json:"bind"`
	Port           int         `yaml:"port" json:"port"`
	RemotesAPIPort int         `yaml:"remotesapi_port,omitempty" json:"remotesapi_port,omitempty"`
	Databases      []string    `yaml:"databases,omitempty" json:"databases,omitempty"`
	Service        hostService `yaml:"service" json:"service"`
}

// hostClient is a machine known to follow this host.
type hostClient struct {
	Target string `yaml:"target" json:"target"`
	Added  string `yaml:"added,omitempty" json:"added,omitempty"`
}

// hostConfig is ~/.config/brain/host.yaml.
type hostConfig struct {
	Role hostRole `yaml:"role" json:"role"`
	// Generation counts how many times the host role has moved. A new host's
	// generation is its predecessor's plus one, so two machines that both
	// claim to be the host are told apart by which number is higher.
	Generation  int          `yaml:"generation" json:"generation"`
	PromoteID   string       `yaml:"promote_id,omitempty" json:"promote_id,omitempty"`
	Since       string       `yaml:"since,omitempty" json:"since,omitempty"`
	FrozenSince string       `yaml:"frozen_since,omitempty" json:"frozen_since,omitempty"`
	MovedTo     string       `yaml:"moved_to,omitempty" json:"moved_to,omitempty"`
	MovedAt     string       `yaml:"moved_at,omitempty" json:"moved_at,omitempty"`
	Server      *hostServer  `yaml:"server,omitempty" json:"server,omitempty"`
	Clients     []hostClient `yaml:"clients,omitempty" json:"clients,omitempty"`
	// Services are launchd labels of jobs that belong to the host role (the
	// backup job, a bridge server). A promotion moves these with the role.
	Services []string `yaml:"services,omitempty" json:"services,omitempty"`
	// Lineage is the chain of promotions that made this machine the host (or,
	// on a machine that has moved on, that it took part in). Clients use it to
	// verify a move before following it.
	Lineage []lineageEntry `yaml:"lineage,omitempty" json:"lineage,omitempty"`
}

func hostConfigFile(home string) string { return filepath.Join(home, hostConfigRel) }

// loadHostConfig reads host.yaml. A missing file is (nil, nil): the machine has
// not been given a role explicitly.
func loadHostConfig(home string) (*hostConfig, error) {
	data, err := os.ReadFile(hostConfigFile(home)) //nolint:gosec // our own config file
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", hostConfigFile(home), err)
	}
	var cfg hostConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", hostConfigFile(home), err)
	}
	switch cfg.Role {
	case roleHost, roleFrozen, roleMoved:
	case "":
		cfg.Role = roleHost
	default:
		return nil, fmt.Errorf("%s: unknown role %q", hostConfigFile(home), cfg.Role)
	}
	return &cfg, nil
}

const hostConfigHeader = "# brain host role — managed by 'brain host'. Do not edit while a promotion is running.\n"

func marshalHostConfig(cfg *hostConfig) ([]byte, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return append([]byte(hostConfigHeader), data...), nil
}

func saveHostConfig(home string, cfg *hostConfig) error {
	data, err := marshalHostConfig(cfg)
	if err != nil {
		return err
	}
	return hostWriteFile(hostConfigFile(home), data, 0o644) //nolint:gosec // no secrets here
}

func nowStamp() string { return time.Now().UTC().Format(hostTimeFormat) }

// hostWriteFile writes data to path through a temp file in the same
// directory and a rename, so a reader (or a crash) sees the old file or the
// new one, never half of it. A symlink at path is replaced, not written through.
func hostWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // config / bin directories
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// newPromoteID makes a sortable, collision-resistant id: UTC time plus four
// random bytes, e.g. 20261012T031500Z-9f3a1c2e.
func newPromoteID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

func validPromoteID(id string) bool {
	if len(id) < 10 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// machineID is a stable name for "this computer's account": it is how a
// promotion recognises that the target is the machine it is running on.
func machineID(home string) string {
	host, _ := os.Hostname()
	real := home
	if r, err := filepath.EvalSymlinks(home); err == nil {
		real = r
	}
	sum := sha256.Sum256([]byte(host + "\x00" + real))
	return hex.EncodeToString(sum[:8])
}

// ---------------------------------------------------------------------------
// Undo journal
// ---------------------------------------------------------------------------

// journalEntry is one reversible step. Entries are written BEFORE the step
// they describe (write-ahead), so undo of a step that never happened is a
// harmless no-op rather than a missing record.
type journalEntry struct {
	Op string `json:"op"` // file | rename | mkdir | service
	At string `json:"at"`

	// file: the state of Path before the first change this promotion made.
	Path       string `json:"path,omitempty"`
	Existed    bool   `json:"existed,omitempty"`
	IsSymlink  bool   `json:"is_symlink,omitempty"`
	LinkTarget string `json:"link_target,omitempty"`
	Mode       uint32 `json:"mode,omitempty"`
	Saved      string `json:"saved,omitempty"` // file under the journal's files/ directory

	// rename: From was renamed to To.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`

	// service: an action taken on a managed service.
	Action  string      `json:"action,omitempty"` // started | stopped | disabled | enabled | loaded | unloaded
	Service hostService `json:"service,omitempty"`
	Argv    []string    `json:"argv,omitempty"` // how to start a stopped process-mode server again
	Dir     string      `json:"dir,omitempty"`
	Port    int         `json:"port,omitempty"`
}

// journal is an append-only list of reversible steps for one promotion on one
// machine, plus the saved bytes of every file it overwrote or removed.
type journal struct {
	dir   string
	seen  map[string]bool // file paths already saved: only the first save matters
	count int
}

func journalFile(dir string) string { return filepath.Join(dir, "journal.jsonl") }

func openJournal(dir string) (*journal, error) {
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o700); err != nil {
		return nil, err
	}
	j := &journal{dir: dir, seen: map[string]bool{}}
	entries, err := j.entries()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		j.count++
		if e.Op == "file" {
			j.seen[e.Path] = true
		}
	}
	return j, nil
}

func (j *journal) entries() ([]journalEntry, error) {
	f, err := os.Open(journalFile(j.dir)) //nolint:gosec // our own journal
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []journalEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e journalEntry
		if err := json.Unmarshal(line, &e); err != nil {
			// A torn last line (crash mid-append) is the only damage an
			// append-only file can have; everything before it is intact.
			break
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

func (j *journal) append(e journalEntry) error {
	e.At = nowStamp()
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(journalFile(j.dir), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // our own journal
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	j.count++
	return f.Close()
}

// saveFile records what path looks like now, once. Calling it again for the
// same path (a resumed step) records nothing: the original state is what
// undo must restore, not the state after our own earlier write.
func (j *journal) saveFile(path string) error {
	if j.seen[path] {
		return nil
	}
	e := journalEntry{Op: "file", Path: path}
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Existed stays false: undo removes whatever we create here.
	case err != nil:
		return err
	case info.Mode()&os.ModeSymlink != 0:
		target, rerr := os.Readlink(path)
		if rerr != nil {
			return rerr
		}
		e.Existed, e.IsSymlink, e.LinkTarget = true, true, target
	case info.Mode().IsRegular():
		data, rerr := os.ReadFile(path) //nolint:gosec // file we are about to replace
		if rerr != nil {
			return rerr
		}
		e.Existed = true
		e.Mode = uint32(info.Mode().Perm())
		e.Saved = fmt.Sprintf("%04d", j.count+1)
		if werr := os.WriteFile(filepath.Join(j.dir, "files", e.Saved), data, 0o600); werr != nil {
			return werr
		}
	default:
		return fmt.Errorf("%s is not a regular file or symlink; refusing to replace it", path)
	}
	if err := j.append(e); err != nil {
		return err
	}
	j.seen[path] = true
	return nil
}

// writeFile journals then atomically replaces path with data.
func (j *journal) writeFile(path string, data []byte, mode os.FileMode) error {
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, data) { //nolint:gosec // our own target
		if info, serr := os.Lstat(path); serr == nil && info.Mode().IsRegular() && info.Mode().Perm() == mode {
			return nil
		}
	}
	if err := j.saveFile(path); err != nil {
		return err
	}
	return hostWriteFile(path, data, mode)
}

// writeSymlink journals then replaces path with a symlink to target.
func (j *journal) writeSymlink(path, target string) error {
	if cur, err := os.Readlink(path); err == nil && cur == target {
		return nil
	}
	if err := j.saveFile(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // link directory
		return err
	}
	tmp := path + ".brain-promote.tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// removeFile journals then removes path (if it exists).
func (j *journal) removeFile(path string) error {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := j.saveFile(path); err != nil {
		return err
	}
	return os.Remove(path)
}

// rename journals then renames from → to. A rename whose source is already gone
// and whose destination exists is treated as done (a resumed step).
func (j *journal) rename(from, to string) error {
	if _, err := os.Lstat(from); errors.Is(err, fs.ErrNotExist) {
		if _, terr := os.Lstat(to); terr == nil {
			return nil
		}
		return fmt.Errorf("rename %s → %s: source does not exist", from, to)
	}
	if _, err := os.Lstat(to); err == nil {
		return fmt.Errorf("rename %s → %s: destination already exists", from, to)
	}
	if err := j.append(journalEntry{Op: "rename", From: from, To: to}); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil { //nolint:gosec // data directory parent
		return err
	}
	return os.Rename(from, to)
}

// recordService journals an action taken on a managed service so undo can reverse it.
func (j *journal) recordService(action string, svc hostService) error {
	return j.append(journalEntry{Op: "service", Action: action, Service: svc})
}

// recordServiceStop journals that a process-mode server is about to be stopped,
// with the command that starts it again.
func (j *journal) recordServiceStop(svc hostService, argv []string, dir string, port int) error {
	return j.append(journalEntry{Op: "service", Action: "stopped", Service: svc, Argv: argv, Dir: dir, Port: port})
}

// undo reverses every entry, newest first. Each reversal is idempotent and
// tolerant: a step that never completed, or was already undone, is skipped.
// Returns the steps it took (for the operator) and the first hard error.
func (j *journal) undo(svc serviceController) (steps []string, err error) {
	entries, rerr := j.entries()
	if rerr != nil {
		return nil, rerr
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		switch e.Op {
		case "file":
			step, uerr := j.undoFile(e)
			if step != "" {
				steps = append(steps, step)
			}
			if uerr != nil && err == nil {
				err = uerr
			}
		case "rename":
			if _, ferr := os.Lstat(e.From); ferr == nil {
				continue // already back
			}
			if _, terr := os.Lstat(e.To); terr != nil {
				continue // neither exists: the rename never happened
			}
			if rerr := os.Rename(e.To, e.From); rerr != nil {
				if err == nil {
					err = rerr
				}
				continue
			}
			steps = append(steps, fmt.Sprintf("moved %s back to %s", e.To, e.From))
		case "service":
			if svc == nil {
				continue
			}
			step, uerr := svc.undo(e)
			if step != "" {
				steps = append(steps, step)
			}
			if uerr != nil && err == nil {
				err = uerr
			}
		}
	}
	if err == nil && len(entries) > 0 {
		// Retire the journal so a second undo is a no-op and the record of
		// what was undone is kept.
		done := filepath.Join(j.dir, fmt.Sprintf("journal.undone-%s.jsonl", time.Now().UTC().Format("20060102T150405Z")))
		if rerr := os.Rename(journalFile(j.dir), done); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			err = rerr
		}
		j.seen = map[string]bool{}
		j.count = 0
	}
	return steps, err
}

func (j *journal) undoFile(e journalEntry) (string, error) {
	cur, lerr := os.Lstat(e.Path)
	exists := lerr == nil
	if !e.Existed {
		if !exists {
			return "", nil
		}
		if cur.IsDir() {
			return "", fmt.Errorf("%s is now a directory; not removing it", e.Path)
		}
		if err := os.Remove(e.Path); err != nil {
			return "", err
		}
		return "removed " + e.Path, nil
	}
	if e.IsSymlink {
		if now, err := os.Readlink(e.Path); err == nil && now == e.LinkTarget {
			return "", nil
		}
		_ = os.Remove(e.Path)
		if err := os.Symlink(e.LinkTarget, e.Path); err != nil {
			return "", err
		}
		return "restored link " + e.Path, nil
	}
	saved, err := os.ReadFile(filepath.Join(j.dir, "files", e.Saved)) //nolint:gosec // our journal
	if err != nil {
		return "", fmt.Errorf("journal copy of %s is missing: %w", e.Path, err)
	}
	if now, rerr := os.ReadFile(e.Path); rerr == nil && bytes.Equal(now, saved) { //nolint:gosec // restoring our own target
		return "", nil
	}
	if err := hostWriteFile(e.Path, saved, os.FileMode(e.Mode)); err != nil {
		return "", err
	}
	return "restored " + e.Path, nil
}

// ---------------------------------------------------------------------------
// Promotion state (source side)
// ---------------------------------------------------------------------------

type promoteOutcome string

const (
	outcomeRunning    promoteOutcome = "running"
	outcomeFlipped    promoteOutcome = "flipped" // the role moved; follow-up steps may remain
	outcomeDone       promoteOutcome = "done"
	outcomeRolledBack promoteOutcome = "rolled-back"
	outcomeAborted    promoteOutcome = "aborted"
)

// promotePhases are the steps of a promotion, in order. A resumed promotion
// skips the ones recorded as done and re-runs the first that is not.
var promotePhases = []string{
	"preflight", "seed", "files", "freeze", "delta", "restore", "serve", "parity", "fence", "flip", "clients", "verify",
}

// dbCopy is what the copy phases know about one database.
type dbCopy struct {
	Name       string `json:"name"`
	SourceHash string `json:"source_hash,omitempty"` // hash while frozen
	CopiedHash string `json:"copied_hash,omitempty"` // hash the backup was proven to hold
	BackupDir  string `json:"backup_dir,omitempty"`  // staging on the source
	Bytes      int64  `json:"bytes,omitempty"`
}

// promoteSummary is what the later phases need after the old host has stopped
// being inspectable (its wrappers are client wrappers, its server may be down).
type promoteSummary struct {
	SourceServer hostServer `json:"source_server"`
	TargetServer hostServer `json:"target_server"`
	StagingDir   string     `json:"staging_dir,omitempty"`
	Stores       []string   `json:"stores,omitempty"`
	StoreBeads   []string   `json:"store_beads,omitempty"` // source .beads dirs, parallel to Stores
	StageBeads   []string   `json:"stage_beads,omitempty"` // staged .beads dirs on the target, parallel to Stores
	FinalBeads   []string   `json:"final_beads,omitempty"` // final .beads dirs on the target, parallel to Stores
	Wrappers     []string   `json:"wrappers,omitempty"`    // wrapper names
	BinDir       string     `json:"bin_dir,omitempty"`     // target's wrapper directory
}

// promoteState is the authoritative record of one promotion, kept on the old
// host. Phase completion is written after the work, and every phase is safe to
// repeat, so a crash anywhere resumes by re-running the unfinished phase.
type promoteState struct {
	Version    int               `json:"version"`
	ID         string            `json:"id"`
	Target     string            `json:"target"`
	Options    promoteOptions    `json:"options"`
	Outcome    promoteOutcome    `json:"outcome"`
	Done       map[string]string `json:"done"` // phase → completion time
	Started    string            `json:"started"`
	Updated    string            `json:"updated"`
	SourceHome string            `json:"source_home"`
	TargetHome string            `json:"target_home,omitempty"`
	PeerBin    string            `json:"peer_bin,omitempty"`
	Generation int               `json:"generation"` // the generation the target takes
	Databases  []dbCopy          `json:"databases,omitempty"`
	Manifests  map[string]string `json:"manifests,omitempty"` // db → manifest json at freeze
	FlipHashes map[string]string `json:"flip_hashes,omitempty"`
	FlipHeads  map[string]string `json:"flip_heads,omitempty"` // default-branch commit of each database at the freeze
	Summary    promoteSummary    `json:"summary"`
	// ServerStopped is true when this promotion stopped the old server, so
	// rollback knows to start it again.
	ServerStopped bool             `json:"server_stopped,omitempty"`
	Frozen        bool             `json:"frozen,omitempty"`
	ClientTargets []string         `json:"client_targets,omitempty"`
	Clients       []string         `json:"clients_updated,omitempty"`
	Failed        []string         `json:"clients_failed,omitempty"`
	Baseline      map[string]int64 `json:"baseline,omitempty"` // store → `count` on the old host while frozen
	Log           []string         `json:"log,omitempty"`
}

func promoteDir(home, id string) string { return filepath.Join(home, promoteRootRel, id) }
func promoteStateFile(home, id string) string {
	return filepath.Join(promoteDir(home, id), "state.json")
}
func promoteCurrentFile(home string) string { return filepath.Join(home, promoteRootRel, "current") }

func (s *promoteState) save(home string) error {
	s.Updated = nowStamp()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return hostWriteFile(promoteStateFile(home, s.ID), append(data, '\n'), 0o600)
}

func loadPromoteState(home, id string) (*promoteState, error) {
	if !validPromoteID(id) {
		return nil, fmt.Errorf("invalid promotion id %q", id)
	}
	data, err := os.ReadFile(promoteStateFile(home, id)) //nolint:gosec // our own state file
	if err != nil {
		return nil, err
	}
	var s promoteState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", promoteStateFile(home, id), err)
	}
	if s.Done == nil {
		s.Done = map[string]string{}
	}
	return &s, nil
}

// currentPromoteID returns the id of the promotion this machine has not
// finished, or "".
func currentPromoteID(home string) string {
	data, err := os.ReadFile(promoteCurrentFile(home)) //nolint:gosec // our own pointer file
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func setCurrentPromote(home, id string) error {
	return hostWriteFile(promoteCurrentFile(home), []byte(id+"\n"), 0o600)
}

func clearCurrentPromote(home string) {
	_ = os.Remove(promoteCurrentFile(home))
}

// lastPromoteID is the newest promotion directory (finished or not).
func lastPromoteID(home string) string {
	entries, err := os.ReadDir(filepath.Join(home, promoteRootRel))
	if err != nil {
		return ""
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && validPromoteID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return ""
	}
	return ids[len(ids)-1]
}

func (s *promoteState) phaseDone(name string) bool { _, ok := s.Done[name]; return ok }

func (s *promoteState) markDone(home, name string) error {
	if s.Done == nil {
		s.Done = map[string]string{}
	}
	s.Done[name] = nowStamp()
	return s.save(home)
}

func (s *promoteState) logf(format string, args ...any) {
	s.Log = append(s.Log, nowStamp()+" "+fmt.Sprintf(format, args...))
	if len(s.Log) > 200 {
		s.Log = s.Log[len(s.Log)-200:]
	}
}
