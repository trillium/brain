// Package main — brain_host_peer.go
//
// The conversation between the machine giving up the host role (the source) and
// the one taking it (the target). The source runs the promotion; the target
// does its own local work when asked, by running 'brain host peer <op>' over
// ssh with a JSON request on stdin and a JSON answer on stdout. Doing the
// target's work in the same binary, rather than in shell fragments, keeps one
// implementation of every step and lets it be tested without a second machine.
//
// Every op is idempotent. Every change an op makes on the target is journaled
// first, so 'undo' can reverse a promotion that did not finish.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	peerBegin = "BRAIN-PEER-BEGIN"
	peerEnd   = "BRAIN-PEER-END"

	// peerEnvScript runs its arguments with the PATH a non-interactive ssh
	// session lacks and with forwarding off (the target may itself be a client
	// of the machine asking).
	peerEnvScript = `d="$HOME/.local/bin"; PATH="$d:/opt/homebrew/bin:/usr/local/bin:$PATH"; BRAIN_REMOTE=off; export PATH BRAIN_REMOTE; exec "$@"`

	// peerFindScript tries each candidate command until one answers 'host peer
	// hello', and prints that answer.
	peerFindScript = `d="$HOME/.local/bin"; PATH="$d:/opt/homebrew/bin:/usr/local/bin:$PATH"; BRAIN_REMOTE=off; export PATH BRAIN_REMOTE; ` +
		`for c in "$@"; do if out=$("$c" host peer hello 2>/dev/null </dev/null) && [ -n "$out" ]; then printf '%s\n' "$out"; exit 0; fi; done; ` +
		`echo "brain-peer: no command that supports 'brain host' was found on the target (tried: $*); install this brain build there first" >&2; exit 127`
)

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

type peerHelloResp struct {
	Protocol       int              `json:"protocol"`
	Version        string           `json:"version"`
	Build          string           `json:"build"`
	Home           string           `json:"home"`
	OS             string           `json:"os"`
	Arch           string           `json:"arch"`
	Hostname       string           `json:"hostname"`
	MachineID      string           `json:"machine_id"`
	PeerBin        string           `json:"peer_bin"`       // the brain binary that answered
	BdOnPath       string           `json:"bd_on_path"`     // what wrappers' 'exec bd' will run there
	DoltPath       string           `json:"dolt_path"`      //
	DoltVersion    string           `json:"dolt_version"`   //
	Rsync          bool             `json:"rsync"`          //
	Free           map[string]int64 `json:"free,omitempty"` // bytes available, by path asked about
	Role           hostRole         `json:"role,omitempty"` // host.yaml, when there is one
	Generation     int              `json:"generation,omitempty"`
	ClientOf       string           `json:"client_of,omitempty"` // remote.yaml host, when this machine is a client
	LaunchctlReady bool             `json:"launchctl,omitempty"`
}

type peerHelloReq struct {
	FreePaths []string `json:"free_paths,omitempty"`
}

type peerFileRef struct {
	Path string `json:"path"`
	SHA  string `json:"sha,omitempty"`
}

type peerLinkRef struct {
	Path   string `json:"path"`
	Target string `json:"target"`
}

type peerProbeReq struct {
	ID         string        `json:"id"`
	Source     string        `json:"source"` // source machine id
	DataDir    string        `json:"data_dir"`
	Port       int           `json:"port"`
	Databases  []string      `json:"databases"`
	StoreRoots []string      `json:"store_roots"`
	Wrappers   []peerFileRef `json:"wrappers"`
	Links      []peerLinkRef `json:"links"`
	Plists     []peerFileRef `json:"plists"`
	NeedBytes  int64         `json:"need_bytes"`
}

type peerProbeResp struct {
	Home              string         `json:"home"`
	Role              hostRole       `json:"role,omitempty"`
	Generation        int            `json:"generation,omitempty"`
	PromoteID         string         `json:"promote_id,omitempty"`
	MovedTo           string         `json:"moved_to,omitempty"`
	ClientOf          string         `json:"client_of,omitempty"`
	OldServerPort     int            `json:"old_server_port,omitempty"` // a former host\'s own server, still answering
	ServerListening   bool           `json:"server_listening"`
	ServerOurs        bool           `json:"server_ours"`
	DatabasesPresent  []string       `json:"databases_present,omitempty"`
	DatabasesOurs     []string       `json:"databases_ours,omitempty"`
	StoreRootsPresent []string       `json:"store_roots_present,omitempty"`
	StoreRootsOurs    []string       `json:"store_roots_ours,omitempty"`
	WrapperConflicts  []string       `json:"wrapper_conflicts,omitempty"`
	LinkConflicts     []string       `json:"link_conflicts,omitempty"`
	PlistConflicts    []string       `json:"plist_conflicts,omitempty"`
	RenameBlocked     []string       `json:"rename_blocked,omitempty"`
	FreeBytes         int64          `json:"free_bytes"`
	Incoming          *incomingState `json:"incoming,omitempty"`
}

// incomingState is what the target remembers about one promotion in progress.
type incomingState struct {
	ID            string            `json:"id"`
	Source        string            `json:"source"`
	Created       string            `json:"created"`
	StagingRoot   string            `json:"staging_root"`
	DataDir       string            `json:"data_dir,omitempty"`
	Restored      map[string]string `json:"restored,omitempty"` // db → offline hash of the staged copy
	Placed        []string          `json:"placed,omitempty"`   // final paths this promotion put in place
	ServerStarted bool              `json:"server_started,omitempty"`
	ServerPort    int               `json:"server_port,omitempty"`
	Service       hostService       `json:"service,omitempty"`
	Activated     string            `json:"activated,omitempty"`
}

type peerPrepareReq struct {
	ID      string `json:"id"`
	Source  string `json:"source"`
	DataDir string `json:"data_dir"`
}

type peerPrepareResp struct {
	StagingRoot string `json:"staging_root"`
	BackupRoot  string `json:"backup_root"` // <staging>/backup/<db>
	StoresRoot  string `json:"stores_root"` // <staging>/stores/<store>
	FilesRoot   string `json:"files_root"`  // <staging>/files
}

type peerRestoreReq struct {
	ID        string            `json:"id"`
	Databases map[string]string `json:"databases"` // db → hash the restore must reproduce
}

type peerRestoreResp struct {
	Hashes map[string]string `json:"hashes"`
	Reused []string          `json:"reused,omitempty"`
}

type peerServeReq struct {
	ID           string            `json:"id"`
	DataDir      string            `json:"data_dir"`
	Bind         string            `json:"bind"`
	Port         int               `json:"port"`
	RemotesAPI   int               `json:"remotesapi_port"`
	Service      string            `json:"service"` // auto | launchd | process | external
	ServiceLabel string            `json:"service_label,omitempty"`
	Databases    map[string]string `json:"databases"` // db → expected hash
	ReplaceStale bool              `json:"replace_stale"`
	CopyDoltCfg  bool              `json:"copy_doltcfg"`
}

type peerServeResp struct {
	Hashes  map[string]string `json:"hashes"`
	Server  hostServer        `json:"server"`
	Started bool              `json:"started"`
}

type peerManifestReq struct {
	Port      int      `json:"port"`
	Databases []string `json:"databases"`
}

type peerManifestResp struct {
	Manifests map[string]*dbManifest `json:"manifests"`
}

type peerSmokeStore struct {
	Name     string `json:"name"`
	BeadsDir string `json:"beads_dir"`
	Wrapper  string `json:"wrapper,omitempty"`  // run this wrapper instead of pinning the directory
	Metadata string `json:"metadata,omitempty"` // install this metadata.json in the (staged) directory first
}

type peerSmokeReq struct {
	Port   int              `json:"port"`
	Stores []peerSmokeStore `json:"stores"`
}

type peerSmokeResult struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
	Error string `json:"error,omitempty"`
}

type peerSmokeResp struct {
	Results []peerSmokeResult `json:"results"`
}

type peerFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    uint32 `json:"mode"`
}

type peerStoreMove struct {
	Name     string `json:"name"`
	Stage    string `json:"stage"`
	Final    string `json:"final"`
	Metadata string `json:"metadata"` // rebased metadata.json to install in the staged copy
}

type peerServiceSpec struct {
	Label    string     `json:"label"`
	Plist    peerFile   `json:"plist"`
	Files    []peerFile `json:"files,omitempty"`
	Disabled bool       `json:"disabled,omitempty"`
}

type peerActivateReq struct {
	ID               string                `json:"id"`
	Generation       int                   `json:"generation"`
	Stores           []peerStoreMove       `json:"stores"`
	Registry         map[string]storeEntry `json:"registry"`
	Wrappers         []peerFile            `json:"wrappers"`
	Helpers          []peerFile            `json:"helpers"`
	Links            []peerLinkRef         `json:"links"`
	Services         []peerServiceSpec     `json:"services"`
	DeclaredServices []string              `json:"declared_services,omitempty"`
	Server           hostServer            `json:"server"`
	Clients          []hostClient          `json:"clients,omitempty"`
	Lineage          []lineageEntry        `json:"lineage,omitempty"`
	FlipHashes       map[string]string     `json:"flip_hashes"`
	MoveAside        bool                  `json:"move_aside"`
	ForceWrappers    bool                  `json:"force_wrappers"`
	ReplaceStale     bool                  `json:"replace_stale"`
}

type peerActivateResp struct {
	Wrappers int      `json:"wrappers"`
	Links    int      `json:"links"`
	Stores   int      `json:"stores"`
	Services int      `json:"services"`
	Notes    []string `json:"notes,omitempty"`
}

type peerUndoReq struct {
	ID string `json:"id"`
	// ForwardID and ForwardGeneration, when set, ask the target to leave a
	// forwarding record behind after the undo, naming the host it follows again
	// (its restored remote.yaml). Clients that already followed the move to this
	// machine then follow it back.
	ForwardID         string `json:"forward_id,omitempty"`
	ForwardGeneration int    `json:"forward_generation,omitempty"`
}

type peerUndoResp struct {
	Steps     []string `json:"steps"`
	MachineID string   `json:"machine_id,omitempty"`
	ForwardTo string   `json:"forward_to,omitempty"`
}

type peerStatusReq struct {
	ID    string `json:"id,omitempty"`
	Port  int    `json:"port,omitempty"`
	Check bool   `json:"check,omitempty"` // also hash the databases on the server
}

type peerStatusResp struct {
	Home      string            `json:"home"`
	MachineID string            `json:"machine_id,omitempty"`
	Forward   *forwardRecord    `json:"forward,omitempty"`
	Host      *hostConfig       `json:"host,omitempty"`
	ClientOf  string            `json:"client_of,omitempty"`
	Incoming  *incomingState    `json:"incoming,omitempty"`
	Hashes    map[string]string `json:"hashes,omitempty"`
	Listing   []string          `json:"databases,omitempty"`
	Commits   map[string]bool   `json:"commits,omitempty"` // commit asked about → in history
}

type peerHashesReq struct {
	Port      int      `json:"port"`
	Databases []string `json:"databases"`
}

type peerHashesResp struct {
	Hashes map[string]string `json:"hashes"`
	Heads  map[string]string `json:"heads"`
}

type peerHistoryReq struct {
	Port    int               `json:"port"`
	Commits map[string]string `json:"commits"` // db → commit that must be in its history
}

type peerHistoryResp struct {
	Contains map[string]bool `json:"contains"`
}

// ---------------------------------------------------------------------------
// Target side
// ---------------------------------------------------------------------------

type peerEnv struct {
	home string
	svc  *serviceManager
}

func newPeerEnv() (*peerEnv, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, errors.New("cannot resolve the home directory")
	}
	return &peerEnv{home: home, svc: newServiceManager(home)}, nil
}

func (e *peerEnv) incomingDir(id string) string { return filepath.Join(e.home, incomingRootRel, id) }
func (e *peerEnv) incomingFile(id string) string {
	return filepath.Join(e.incomingDir(id), "incoming.json")
}

func (e *peerEnv) loadIncoming(id string) (*incomingState, error) {
	data, err := os.ReadFile(e.incomingFile(id)) //nolint:gosec // our own state
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st incomingState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", e.incomingFile(id), err)
	}
	if st.Restored == nil {
		st.Restored = map[string]string{}
	}
	return &st, nil
}

func (e *peerEnv) saveIncoming(st *incomingState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return hostWriteFile(e.incomingFile(st.ID), append(data, '\n'), 0o600)
}

func (e *peerEnv) mustIncoming(id string) (*incomingState, error) {
	st, err := e.loadIncoming(id)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, fmt.Errorf("promotion %s is not prepared on this machine (run the earlier phases first)", id)
	}
	return st, nil
}

func (st *incomingState) placed(path string) bool {
	for _, p := range st.Placed {
		if p == path {
			return true
		}
	}
	return false
}

func (st *incomingState) addPlaced(path string) {
	if !st.placed(path) {
		st.Placed = append(st.Placed, path)
	}
}

func (e *peerEnv) hello(req peerHelloReq) (*peerHelloResp, error) {
	exe, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	host, _ := os.Hostname()
	resp := &peerHelloResp{
		Protocol: hostProtocol, Version: Version + "+brain." + BrainVersion, Build: Build,
		Home: e.home, OS: runtime.GOOS, Arch: runtime.GOARCH, Hostname: host, MachineID: machineID(e.home),
		PeerBin: exe,
	}
	if p, err := exec.LookPath("bd"); err == nil {
		if r, rerr := filepath.EvalSymlinks(p); rerr == nil {
			p = r
		}
		resp.BdOnPath = p
	}
	resp.DoltPath, resp.DoltVersion, _ = doltBinary()
	_, rerr := exec.LookPath("rsync")
	resp.Rsync = rerr == nil
	if _, err := exec.LookPath("launchctl"); err == nil {
		resp.LaunchctlReady = true
	}
	if len(req.FreePaths) > 0 {
		resp.Free = map[string]int64{}
		for _, p := range req.FreePaths {
			resp.Free[p] = freeBytes(p)
		}
	}
	if cfg, err := loadHostConfig(e.home); err == nil && cfg != nil {
		resp.Role, resp.Generation = cfg.Role, cfg.Generation
	}
	if rc, err := loadRemoteConfig(); err == nil && rc.Host != "" {
		resp.ClientOf = rc.Host
	}
	return resp, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// probe reports everything on this machine that a promotion would collide with.
func (e *peerEnv) probe(ctx context.Context, req peerProbeReq) (*peerProbeResp, error) {
	resp := &peerProbeResp{Home: e.home, FreeBytes: freeBytes(req.DataDir)}
	if cfg, err := loadHostConfig(e.home); err != nil {
		return nil, err
	} else if cfg != nil {
		resp.Role, resp.Generation, resp.PromoteID, resp.MovedTo = cfg.Role, cfg.Generation, cfg.PromoteID, cfg.MovedTo
		if cfg.Role == roleMoved && cfg.Server != nil && cfg.Server.Port > 0 && portListening(cfg.Server.Port) {
			resp.OldServerPort = cfg.Server.Port
		}
	}
	if rc, err := loadRemoteConfig(); err == nil {
		resp.ClientOf = rc.Host
	}
	st, err := e.loadIncoming(req.ID)
	if err != nil {
		return nil, err
	}
	resp.Incoming = st
	ours := func(p string) bool { return st != nil && st.placed(p) }

	resp.ServerListening = portListening(req.Port)
	if resp.ServerListening && st != nil && st.ServerStarted && st.ServerPort == req.Port {
		resp.ServerOurs = true
	}
	for _, db := range req.Databases {
		p := filepath.Join(req.DataDir, db)
		if hostPathExists(p) {
			resp.DatabasesPresent = append(resp.DatabasesPresent, db)
			if ours(p) {
				resp.DatabasesOurs = append(resp.DatabasesOurs, db)
			}
		}
	}
	for _, r := range req.StoreRoots {
		if hostPathExists(r) {
			resp.StoreRootsPresent = append(resp.StoreRootsPresent, r)
			if ours(r) {
				resp.StoreRootsOurs = append(resp.StoreRootsOurs, r)
			}
		}
	}
	for _, w := range req.Wrappers {
		if conflict := fileConflict(w.Path, w.SHA); conflict {
			resp.WrapperConflicts = append(resp.WrapperConflicts, w.Path)
		}
	}
	for _, l := range req.Links {
		if cur, err := os.Readlink(l.Path); err == nil {
			if cur != l.Target {
				resp.LinkConflicts = append(resp.LinkConflicts, l.Path)
			}
		} else if hostPathExists(l.Path) {
			resp.LinkConflicts = append(resp.LinkConflicts, l.Path)
		}
	}
	for _, p := range req.Plists {
		if _, err := os.Lstat(p.Path); err == nil {
			if data, rerr := os.ReadFile(p.Path); rerr != nil || sha256Hex(data) != p.SHA { //nolint:gosec // inspecting a LaunchAgent we may replace
				resp.PlistConflicts = append(resp.PlistConflicts, p.Path)
			}
		}
	}
	// Renaming the staged copies into place must work: same filesystem.
	staging := filepath.Join(filepath.Dir(req.DataDir), ".brain-promote-"+req.ID)
	if err := os.MkdirAll(staging, 0o700); err == nil {
		finals := append([]string{filepath.Join(req.DataDir, "x")}, req.StoreRoots...)
		for _, f := range finals {
			if !renameFeasible(staging, filepath.Dir(f)) {
				resp.RenameBlocked = append(resp.RenameBlocked, f)
			}
		}
		if st == nil {
			_ = os.Remove(staging) // probing must not leave a directory behind
		}
	}
	_ = ctx
	return resp, nil
}

// fileConflict reports whether replacing path with content of the given hash
// would overwrite something that is not ours to replace: it exists, is not a
// generated client wrapper, and is not already identical.
func fileConflict(path, wantSHA string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if !info.Mode().IsRegular() {
		return true
	}
	data, err := os.ReadFile(path) //nolint:gosec // inspecting a file we may replace
	if err != nil {
		return true
	}
	if wantSHA != "" && sha256Hex(data) == wantSHA {
		return false
	}
	return !isRemoteWrapper(path)
}

// renameFeasible checks that a directory can be renamed from under stagingRoot
// into parent (they must share a filesystem).
func renameFeasible(stagingRoot, parent string) bool {
	if err := os.MkdirAll(parent, 0o755); err != nil { //nolint:gosec // data directory parent
		return false
	}
	src, err := os.MkdirTemp(stagingRoot, "renametest-")
	if err != nil {
		return false
	}
	dst := filepath.Join(parent, ".brain-promote-renametest-"+filepath.Base(src))
	if err := os.Rename(src, dst); err != nil {
		_ = os.Remove(src)
		return false
	}
	_ = os.Remove(dst)
	return true
}

func (e *peerEnv) prepare(req peerPrepareReq) (*peerPrepareResp, error) {
	if !validPromoteID(req.ID) {
		return nil, fmt.Errorf("invalid promotion id %q", req.ID)
	}
	staging := filepath.Join(filepath.Dir(req.DataDir), ".brain-promote-"+req.ID)
	for _, d := range []string{"backup", "restored", "stores", "files"} {
		if err := os.MkdirAll(filepath.Join(staging, d), 0o700); err != nil {
			return nil, err
		}
	}
	st, err := e.loadIncoming(req.ID)
	if err != nil {
		return nil, err
	}
	if st == nil {
		st = &incomingState{ID: req.ID, Source: req.Source, Created: nowStamp(), StagingRoot: staging, DataDir: req.DataDir, Restored: map[string]string{}}
		if err := e.saveIncoming(st); err != nil {
			return nil, err
		}
	} else if st.Source != req.Source {
		return nil, fmt.Errorf("promotion %s already exists here from a different machine", req.ID)
	}
	return &peerPrepareResp{
		StagingRoot: staging,
		BackupRoot:  filepath.Join(staging, "backup"),
		StoresRoot:  filepath.Join(staging, "stores"),
		FilesRoot:   filepath.Join(staging, "files"),
	}, nil
}

// restore turns each staged backup into a staged database and proves it matches
// the hash the source reported. A staged copy that already matches is kept.
func (e *peerEnv) restore(ctx context.Context, req peerRestoreReq) (*peerRestoreResp, error) {
	st, err := e.mustIncoming(req.ID)
	if err != nil {
		return nil, err
	}
	doltBin, _, err := doltBinary()
	if err != nil {
		return nil, err
	}
	ensureDoltIdentityIfMissing(doltBin) // a fresh account has no author identity; restoring commits need one
	resp := &peerRestoreResp{Hashes: map[string]string{}}
	restoredDir := filepath.Join(st.StagingRoot, "restored")
	names := sortedKeysStr(req.Databases)
	for _, db := range names {
		want := req.Databases[db]
		dest := filepath.Join(restoredDir, db)
		// Already placed by an earlier pass and still the right content: keep it.
		if final := filepath.Join(st.DataDir, db); st.placed(final) && hostPathExists(final) {
			var got string
			var herr error
			if portListening(st.ServerPort) && st.ServerStarted {
				got, herr = databaseHash(ctx, st.ServerPort, db)
			} else {
				got, herr = offlineHash(ctx, doltBin, final)
			}
			if herr == nil && got == want {
				st.Restored[db] = got
				resp.Hashes[db], resp.Reused = got, append(resp.Reused, db)
				continue
			}
		}
		if hostPathExists(dest) {
			if got, herr := offlineHash(ctx, doltBin, dest); herr == nil && got == want {
				st.Restored[db] = got
				resp.Hashes[db], resp.Reused = got, append(resp.Reused, db)
				continue
			}
			// A staged copy of an older pass, or an interrupted restore. It is
			// scratch this promotion made itself and the backup can rebuild it.
			if err := os.RemoveAll(dest); err != nil {
				return nil, err
			}
		}
		got, rerr := restoreBackup(ctx, doltBin, filepath.Join(st.StagingRoot, "backup", db), restoredDir, db, want)
		if rerr != nil {
			return nil, rerr
		}
		st.Restored[db] = got
		resp.Hashes[db] = got
	}
	return resp, e.saveIncoming(st)
}

func sortedKeysStr[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// serve places the staged databases in the data directory and brings the
// server up on them, then proves the server is serving exactly what was staged.
func (e *peerEnv) serve(ctx context.Context, req peerServeReq) (*peerServeResp, error) {
	st, err := e.mustIncoming(req.ID)
	if err != nil {
		return nil, err
	}
	doltBin, _, err := doltBinary()
	if err != nil {
		return nil, err
	}
	j, err := openJournal(e.incomingDir(req.ID))
	if err != nil {
		return nil, err
	}
	kind := req.Service
	if kind == "" || kind == "auto" {
		kind = defaultServiceKind()
	}
	svc := e.svc.newServerService(kind, req.ServiceLabel)
	if st.Service.Kind != "" {
		svc = st.Service // resumed: the service this promotion already chose
	}
	spec := serverSpec{DoltBin: doltBin, DataDir: req.DataDir, Bind: req.Bind, Port: req.Port, RemotesAPIPort: req.RemotesAPI}
	if err := os.MkdirAll(req.DataDir, 0o755); err != nil { //nolint:gosec // the Dolt data directory
		return nil, err
	}

	// A running server does not notice a database directory that appears after
	// it started, so if databases still have to be placed, ours must be down.
	needPlace := false
	for db := range req.Databases {
		final := filepath.Join(req.DataDir, db)
		if !st.placed(final) || hostPathExists(filepath.Join(st.StagingRoot, "restored", db)) {
			needPlace = true
		}
	}
	if portListening(req.Port) {
		if !(st.ServerStarted && st.ServerPort == req.Port) {
			return nil, fmt.Errorf("something that this promotion did not start is already listening on port %d", req.Port)
		}
		if needPlace {
			if err := e.svc.stop(ctx, svc, req.Port, false, &spec, j); err != nil {
				return nil, fmt.Errorf("stopping the staging server before placing databases: %w", err)
			}
		}
	}

	for _, db := range sortedKeysStr(req.Databases) {
		final := filepath.Join(req.DataDir, db)
		staged := filepath.Join(st.StagingRoot, "restored", db)
		want := req.Databases[db]
		if st.placed(final) && hostPathExists(final) {
			if !hostPathExists(staged) {
				continue // placed earlier, nothing newer staged
			}
			// A newer staged copy (a later freeze): the placed one is superseded.
			if err := j.rename(final, final+".superseded-"+req.ID+"-"+time.Now().UTC().Format("150405")); err != nil {
				return nil, err
			}
			st.Placed = removeString(st.Placed, final)
		}
		if hostPathExists(final) {
			if !req.ReplaceStale {
				return nil, fmt.Errorf("%s already holds a database named %s that this promotion did not place", req.DataDir, db)
			}
			if err := j.rename(final, final+".pre-promote-"+req.ID); err != nil {
				return nil, err
			}
		}
		if !hostPathExists(staged) {
			return nil, fmt.Errorf("no staged copy of %s to place (restore it first)", db)
		}
		if got := st.Restored[db]; got != want {
			return nil, fmt.Errorf("staged copy of %s has hash %s, expected %s: restore again", db, got, want)
		}
		if err := j.rename(staged, final); err != nil {
			return nil, err
		}
		st.addPlaced(final)
	}
	if req.CopyDoltCfg {
		staged := filepath.Join(st.StagingRoot, "files", "doltcfg")
		final := filepath.Join(req.DataDir, ".doltcfg")
		if hostPathExists(staged) && !hostPathExists(final) {
			if err := j.rename(staged, final); err != nil {
				return nil, err
			}
			st.addPlaced(final)
		}
	}
	if err := e.saveIncoming(st); err != nil {
		return nil, err
	}

	started := false
	if !portListening(req.Port) {
		if err := e.svc.start(ctx, svc, spec, j); err != nil {
			return nil, err
		}
		started = true
	}
	st.ServerStarted, st.ServerPort, st.Service = true, req.Port, svc
	if err := e.saveIncoming(st); err != nil {
		return nil, err
	}
	if err := waitServerReady(ctx, req.Port, 60*time.Second); err != nil {
		return nil, err
	}
	have, err := listDatabases(ctx, req.Port)
	if err != nil {
		return nil, err
	}
	resp := &peerServeResp{Hashes: map[string]string{}, Started: started, Server: hostServer{
		DataDir: req.DataDir, Bind: req.Bind, Port: req.Port, RemotesAPIPort: req.RemotesAPI,
		Databases: sortedKeysStr(req.Databases), Service: svc,
	}}
	for _, db := range sortedKeysStr(req.Databases) {
		if !containsStr(have, db) {
			return nil, fmt.Errorf("the server on port %d does not list database %s", req.Port, db)
		}
		h, herr := databaseHash(ctx, req.Port, db)
		if herr != nil {
			return nil, herr
		}
		if h != req.Databases[db] {
			return nil, fmt.Errorf("database %s on the new server hashes to %s, not the source's %s", db, h, req.Databases[db])
		}
		resp.Hashes[db] = h
	}
	return resp, nil
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func removeString(list []string, s string) []string {
	out := list[:0]
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

func (e *peerEnv) manifest(ctx context.Context, req peerManifestReq) (*peerManifestResp, error) {
	resp := &peerManifestResp{Manifests: map[string]*dbManifest{}}
	for _, db := range req.Databases {
		m, err := computeManifest(ctx, req.Port, db)
		if err != nil {
			return nil, err
		}
		resp.Manifests[db] = m
	}
	return resp, nil
}

func (e *peerEnv) hashes(ctx context.Context, req peerHashesReq) (*peerHashesResp, error) {
	resp := &peerHashesResp{Hashes: map[string]string{}, Heads: map[string]string{}}
	for _, db := range req.Databases {
		h, err := databaseHash(ctx, req.Port, db)
		if err != nil {
			return nil, err
		}
		resp.Hashes[db] = h
		resp.Heads[db] = headCommit(ctx, req.Port, db)
	}
	return resp, nil
}

func (e *peerEnv) history(ctx context.Context, req peerHistoryReq) (*peerHistoryResp, error) {
	resp := &peerHistoryResp{Contains: map[string]bool{}}
	for db, c := range req.Commits {
		resp.Contains[db] = commitInHistory(ctx, req.Port, db, c)
	}
	return resp, nil
}

// smoke runs a real brain command against each store the way a user would:
// through the installed wrapper after activation, or through the staged config
// directory before it. A store passes when `count` answers with a number.
func (e *peerEnv) smoke(ctx context.Context, req peerSmokeReq) (*peerSmokeResp, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	resp := &peerSmokeResp{Results: make([]peerSmokeResult, len(req.Stores))}
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, s := range req.Stores {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, s peerSmokeStore) {
			defer wg.Done()
			defer func() { <-sem }()
			resp.Results[i] = smokeOne(ctx, exe, req.Port, s)
		}(i, s)
	}
	wg.Wait()
	return resp, nil
}

func smokeOne(ctx context.Context, exe string, port int, s peerSmokeStore) peerSmokeResult {
	r := peerSmokeResult{Name: s.Name}
	if s.Metadata != "" && s.Wrapper == "" {
		if err := hostWriteFile(filepath.Join(s.BeadsDir, "metadata.json"), []byte(s.Metadata), 0o644); err != nil { //nolint:gosec // same mode bd writes
			r.Error = err.Error()
			return r
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	env := append(os.Environ(), "BRAIN_REMOTE=off")
	if s.Wrapper != "" {
		cmd = exec.CommandContext(cctx, s.Wrapper, "count", "--json") //nolint:gosec // wrapper we installed
		env = append(env, "PATH="+filepath.Dir(exe)+string(os.PathListSeparator)+os.Getenv("PATH"))
	} else {
		cmd = exec.CommandContext(cctx, exe, "count", "--json") //nolint:gosec // our own binary
		env = append(env,
			// A staged store is probed before this machine is the host (it may be a
			// former host whose own gate would refuse everything).
			envHostGate+"=off",
			"BEADS_DIR="+s.BeadsDir, "BD_NAME="+s.Name,
			"BEADS_DOLT_SERVER_MODE=1", "BEADS_DOLT_SERVER_HOST=127.0.0.1",
			"BEADS_DOLT_SERVER_PORT="+strconv.Itoa(port), "BEADS_DOLT_SHARED_SERVER=1",
			"BRAIN_EXFIL_FLAT=1")
	}
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if rerr := cmd.Run(); rerr != nil {
		r.Error = strings.TrimSpace(hostFirstLine(errb.String() + " " + rerr.Error()))
		return r
	}
	n, perr := parseCountOutput(out.Bytes())
	if perr != nil {
		r.Error = perr.Error()
		return r
	}
	r.Count = n
	return r
}

func hostFirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// parseCountOutput reads `count --json`: {"count": N}, or a bare number.
func parseCountOutput(out []byte) (int64, error) {
	out = bytes.TrimSpace(out)
	if i := bytes.IndexAny(out, "{0123456789"); i > 0 {
		out = out[i:]
	}
	var obj struct {
		Count *int64 `json:"count"`
		Total *int64 `json:"total"`
	}
	if json.Unmarshal(out, &obj) == nil {
		switch {
		case obj.Count != nil:
			return *obj.Count, nil
		case obj.Total != nil:
			return *obj.Total, nil
		}
	}
	if n, err := strconv.ParseInt(string(out), 10, 64); err == nil {
		return n, nil
	}
	return 0, fmt.Errorf("count did not answer with a number: %q", hostFirstLine(string(out)))
}

// activate makes this machine the host: stores, registry, wrappers, links,
// services and host.yaml, every change journaled. Nothing is changed unless
// every precondition holds.
func (e *peerEnv) activate(ctx context.Context, req peerActivateReq) (*peerActivateResp, error) {
	st, err := e.mustIncoming(req.ID)
	if err != nil {
		return nil, err
	}
	if !st.ServerStarted || !portListening(st.ServerPort) {
		return nil, errors.New("the new server is not running; serve the databases before activating")
	}
	for db, want := range req.FlipHashes {
		got, herr := databaseHash(ctx, st.ServerPort, db)
		if herr != nil {
			return nil, herr
		}
		if got != want {
			return nil, fmt.Errorf("database %s on this server hashes to %s, not the %s the source froze: refusing to activate", db, got, want)
		}
	}
	// Preconditions first: refuse before changing anything.
	for _, sm := range req.Stores {
		if st.placed(sm.Final) {
			continue
		}
		if hostPathExists(sm.Final) && !req.MoveAside {
			return nil, fmt.Errorf("%s already exists and is not part of this promotion (use --move-aside to rename it)", sm.Final)
		}
		if !hostPathExists(sm.Stage) {
			return nil, fmt.Errorf("staged store %s is missing at %s (copy the files first)", sm.Name, sm.Stage)
		}
	}
	for _, f := range append(append([]peerFile{}, req.Wrappers...), req.Helpers...) {
		if !req.ForceWrappers && fileConflict(f.Path, sha256Hex([]byte(f.Content))) {
			return nil, fmt.Errorf("%s exists and was not generated by 'brain remote install' (use --force-wrappers to replace it; the original is journaled)", f.Path)
		}
	}
	for _, l := range req.Links {
		if cur, lerr := os.Readlink(l.Path); lerr == nil {
			if cur != l.Target && !req.ForceWrappers {
				return nil, fmt.Errorf("%s is a link to %s (use --force-wrappers to replace it)", l.Path, cur)
			}
		} else if hostPathExists(l.Path) && !req.ForceWrappers {
			return nil, fmt.Errorf("%s exists and is not a link (use --force-wrappers to replace it)", l.Path)
		}
	}

	j, err := openJournal(e.incomingDir(req.ID))
	if err != nil {
		return nil, err
	}
	resp := &peerActivateResp{}

	// 1. Store directories: install the rebased metadata, then rename into place.
	for _, sm := range req.Stores {
		if !st.placed(sm.Final) {
			if sm.Metadata != "" {
				if err := hostWriteFile(filepath.Join(sm.Stage, ".beads", "metadata.json"), []byte(sm.Metadata), 0o644); err != nil { //nolint:gosec // same mode bd writes
					return nil, err
				}
			}
			if hostPathExists(sm.Final) {
				if err := j.rename(sm.Final, sm.Final+".pre-promote-"+req.ID); err != nil {
					return nil, err
				}
			}
			if err := j.rename(sm.Stage, sm.Final); err != nil {
				return nil, err
			}
			st.addPlaced(sm.Final)
			if err := e.saveIncoming(st); err != nil {
				return nil, err
			}
		}
		resp.Stores++
	}

	// 2. Registry and env, written by the same code 'brain stores' uses.
	for _, p := range []string{storesYamlFile(), storesYamlLegacyFile(), storesEnvFile(), storesEnvLegacyFile()} {
		if err := j.saveFile(p); err != nil {
			return nil, err
		}
	}
	if err := saveStoresRegistry(req.Registry); err != nil {
		return nil, err
	}
	if _, err := regenerateStoresEnv(req.Registry); err != nil {
		return nil, err
	}

	// 3. Wrappers, the scripts they source, and the agent links.
	for _, f := range req.Wrappers {
		if err := j.writeFile(f.Path, []byte(f.Content), os.FileMode(f.Mode)); err != nil {
			return nil, err
		}
		resp.Wrappers++
	}
	for _, f := range req.Helpers {
		if err := j.writeFile(f.Path, []byte(f.Content), os.FileMode(f.Mode)); err != nil {
			return nil, err
		}
	}
	for _, l := range req.Links {
		if err := j.writeSymlink(l.Path, l.Target); err != nil {
			return nil, err
		}
		resp.Links++
	}

	// 4. This machine stops being a client: the client config goes, the host
	// config arrives, in that order, so there is never a moment it is both.
	if err := j.removeFile(remoteConfigFile()); err != nil {
		return nil, err
	}
	// A forwarding record from an earlier life as a former host is stale now.
	if err := j.removeFile(forwardRecordFile(e.home)); err != nil {
		return nil, err
	}

	// 5. Role-bound launchd jobs.
	for _, s := range req.Services {
		for _, f := range s.Files {
			if err := j.writeFile(f.Path, []byte(f.Content), os.FileMode(f.Mode)); err != nil {
				return nil, err
			}
		}
		if err := j.writeFile(s.Plist.Path, []byte(s.Plist.Content), os.FileMode(s.Plist.Mode)); err != nil {
			return nil, err
		}
		if s.Disabled {
			resp.Notes = append(resp.Notes, fmt.Sprintf("%s was disabled on the old host; installed here but not loaded", s.Label))
		} else if _, err := e.svc.loadJob(ctx, s.Label, s.Plist.Path, j); err != nil {
			resp.Notes = append(resp.Notes, fmt.Sprintf("%s: %v (installed, not loaded)", s.Label, err))
		}
		resp.Services++
	}

	// 6. The role itself.
	host := &hostConfig{
		Role: roleHost, Generation: req.Generation, PromoteID: req.ID, Since: nowStamp(),
		Server: &req.Server, Clients: req.Clients, Services: req.DeclaredServices, Lineage: req.Lineage,
	}
	host.Server.Service = st.Service
	data, err := marshalHostConfig(host)
	if err != nil {
		return nil, err
	}
	if err := j.writeFile(hostConfigFile(e.home), data, 0o644); err != nil { //nolint:gosec // no secrets here
		return nil, err
	}
	st.Activated = nowStamp()
	return resp, e.saveIncoming(st)
}

// undo reverses everything this machine did for a promotion.
func (e *peerEnv) undo(req peerUndoReq) (*peerUndoResp, error) {
	if !validPromoteID(req.ID) {
		return nil, fmt.Errorf("invalid promotion id %q", req.ID)
	}
	if _, err := os.Stat(e.incomingDir(req.ID)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &peerUndoResp{}, nil
		}
		return nil, err
	}
	j, err := openJournal(e.incomingDir(req.ID))
	if err != nil {
		return nil, err
	}
	steps, uerr := j.undo(e.svc)
	if uerr != nil {
		return &peerUndoResp{Steps: steps}, uerr
	}
	if st, lerr := e.loadIncoming(req.ID); lerr == nil && st != nil {
		st.Placed, st.ServerStarted, st.Activated, st.Restored = nil, false, "", map[string]string{}
		_ = e.saveIncoming(st)
	}
	resp := &peerUndoResp{Steps: steps, MachineID: machineID(e.home)}
	if req.ForwardID != "" {
		if rc, rerr := loadRemoteConfig(); rerr == nil && rc.Host != "" {
			rec := &forwardRecord{To: rc.Host, PromoteID: req.ForwardID, Generation: req.ForwardGeneration, FromMachine: resp.MachineID, MovedAt: nowStamp()}
			if data, merr := marshalForwardRecord(rec); merr == nil {
				if werr := hostWriteFile(forwardRecordFile(e.home), data, 0o644); werr == nil { //nolint:gosec // no secrets here
					resp.ForwardTo = rc.Host
				}
			}
		}
	}
	return resp, nil
}

func (e *peerEnv) status(ctx context.Context, req peerStatusReq) (*peerStatusResp, error) {
	resp := &peerStatusResp{Home: e.home, MachineID: machineID(e.home)}
	if rec, rerr := loadForwardRecord(e.home); rerr == nil {
		resp.Forward = rec
	}
	cfg, err := loadHostConfig(e.home)
	if err != nil {
		return nil, err
	}
	resp.Host = cfg
	if rc, rerr := loadRemoteConfig(); rerr == nil {
		resp.ClientOf = rc.Host
	}
	if req.ID != "" {
		if st, lerr := e.loadIncoming(req.ID); lerr == nil {
			resp.Incoming = st
		}
	}
	if req.Check && cfg != nil && cfg.Server != nil {
		port := cfg.Server.Port
		if req.Port > 0 {
			port = req.Port
		}
		if portListening(port) {
			resp.Hashes = map[string]string{}
			for _, db := range cfg.Server.Databases {
				if h, herr := databaseHash(ctx, port, db); herr == nil {
					resp.Hashes[db] = h
				}
			}
			resp.Listing, _ = listDatabases(ctx, port)
		}
	}
	return resp, nil
}

// loadJob installs a launchd job on this machine and starts it.
func (m *serviceManager) loadJob(ctx context.Context, label, plist string, j *journal) (bool, error) {
	if !validLaunchdLabel(label) {
		return false, fmt.Errorf("invalid launchd label %q", label)
	}
	svc := hostService{Kind: "launchd", Label: label, Plist: plist}
	if j != nil {
		if err := j.recordService("loaded", svc); err != nil {
			return false, err
		}
	}
	if out, err := m.run(ctx, "launchctl", "bootstrap", m.domain(), plist); err != nil {
		return false, fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// Dispatch (what 'brain host peer <op>' runs)
// ---------------------------------------------------------------------------

func peerErrorJSON(err error) []byte {
	b, _ := json.Marshal(map[string]string{"error": err.Error()})
	return b
}

// runPeerOp reads a JSON request from in, runs op, and writes the framed JSON
// answer to out. It returns the process exit status.
func runPeerOp(ctx context.Context, op string, in io.Reader, out io.Writer) int {
	emit := func(payload []byte, code int) int {
		fmt.Fprintf(out, "%s\n%s\n%s\n", peerBegin, payload, peerEnd)
		return code
	}
	env, err := newPeerEnv()
	if err != nil {
		return emit(peerErrorJSON(err), 1)
	}
	raw, err := io.ReadAll(io.LimitReader(in, 256<<20))
	if err != nil {
		return emit(peerErrorJSON(err), 1)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	var resp any
	switch op {
	case "hello":
		resp, err = decodeAnd(raw, env.hello)
	case "probe":
		resp, err = decodeAnd(raw, func(r peerProbeReq) (*peerProbeResp, error) { return env.probe(ctx, r) })
	case "prepare":
		resp, err = decodeAnd(raw, env.prepare)
	case "restore":
		resp, err = decodeAnd(raw, func(r peerRestoreReq) (*peerRestoreResp, error) { return env.restore(ctx, r) })
	case "serve":
		resp, err = decodeAnd(raw, func(r peerServeReq) (*peerServeResp, error) { return env.serve(ctx, r) })
	case "manifest":
		resp, err = decodeAnd(raw, func(r peerManifestReq) (*peerManifestResp, error) { return env.manifest(ctx, r) })
	case "hashes":
		resp, err = decodeAnd(raw, func(r peerHashesReq) (*peerHashesResp, error) { return env.hashes(ctx, r) })
	case "history":
		resp, err = decodeAnd(raw, func(r peerHistoryReq) (*peerHistoryResp, error) { return env.history(ctx, r) })
	case "smoke":
		resp, err = decodeAnd(raw, func(r peerSmokeReq) (*peerSmokeResp, error) { return env.smoke(ctx, r) })
	case "activate":
		resp, err = decodeAnd(raw, func(r peerActivateReq) (*peerActivateResp, error) { return env.activate(ctx, r) })
	case "undo":
		resp, err = decodeAnd(raw, env.undo)
	case "status":
		resp, err = decodeAnd(raw, func(r peerStatusReq) (*peerStatusResp, error) { return env.status(ctx, r) })
	default:
		err = fmt.Errorf("unknown peer op %q", op)
	}
	if err != nil {
		return emit(peerErrorJSON(err), 1)
	}
	payload, err := json.Marshal(resp)
	if err != nil {
		return emit(peerErrorJSON(err), 1)
	}
	return emit(payload, 0)
}

func decodeAnd[Req, Resp any](raw []byte, fn func(Req) (Resp, error)) (any, error) {
	var req Req
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("bad request: %w", err)
	}
	return fn(req)
}

// ---------------------------------------------------------------------------
// Source side: calling the target
// ---------------------------------------------------------------------------

// peerConn talks to the target machine.
type peerConn struct {
	cfg *remoteConfig
	bin string // resolved by hello
}

func newPeerConn(target string, sshOptions []string) (*peerConn, error) {
	cfg := &remoteConfig{Host: target, SSHOptions: sshOptions}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &peerConn{cfg: cfg}, nil
}

// runScript runs an sh script on the target with args as positional parameters
// and stdin as given.
func (p *peerConn) runScript(ctx context.Context, script string, stdin []byte, args ...string) (stdout, stderr []byte, err error) {
	parts := []string{"exec", "/bin/sh", "-c", remoteQuote(script), "brain-peer"}
	for _, a := range args {
		parts = append(parts, remoteQuote(a))
	}
	a := append(p.cfg.sshBaseArgs(), "-T", p.cfg.Host, "--", strings.Join(parts, " "))
	cmd := exec.CommandContext(ctx, sshBinary(), a...) //nolint:gosec // validated target, quoted args
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	err = cmd.Run()
	return so.Bytes(), se.Bytes(), err
}

func extractPeerPayload(stdout []byte) ([]byte, bool) {
	s := string(stdout)
	i := strings.Index(s, peerBegin+"\n")
	if i < 0 {
		return nil, false
	}
	rest := s[i+len(peerBegin)+1:]
	j := strings.Index(rest, "\n"+peerEnd)
	if j < 0 {
		return nil, false
	}
	return []byte(rest[:j]), true
}

func (p *peerConn) decode(stdout, stderr []byte, runErr error, resp any) error {
	payload, ok := extractPeerPayload(stdout)
	if ok {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(payload, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		if runErr == nil {
			if resp == nil {
				return nil
			}
			return json.Unmarshal(payload, resp)
		}
	}
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) && ee.ExitCode() == sshConnectFailed {
			return errors.New(strings.TrimSpace(unreachableMessage(p.cfg)) + "\n" + strings.TrimSpace(string(stderr)))
		}
		msg := strings.TrimSpace(string(stderr))
		if msg == "" {
			msg = runErr.Error()
		}
		return fmt.Errorf("target %s: %s", p.cfg.Host, msg)
	}
	return fmt.Errorf("target %s answered without a result: %q", p.cfg.Host, hostFirstLine(string(stdout)))
}

// hello finds the brain binary on the target and asks it about itself.
func (p *peerConn) hello(ctx context.Context, explicitBin string, freePaths []string) (*peerHelloResp, error) {
	// The script puts ~/.local/bin first on PATH, so the bare names find the
	// user's own install before a system one.
	cands := []string{"brain", "bd"}
	if explicitBin != "" {
		cands = []string{explicitBin}
	}
	stdout, stderr, err := p.runScript(ctx, peerFindScript, nil, cands...)
	var resp peerHelloResp
	if derr := p.decode(stdout, stderr, err, &resp); derr != nil {
		return nil, derr
	}
	if resp.Protocol != hostProtocol {
		return nil, fmt.Errorf("target %s speaks promotion protocol %d, this machine speaks %d: install matching brain builds on both", p.cfg.Host, resp.Protocol, hostProtocol)
	}
	p.bin = resp.PeerBin
	if len(freePaths) > 0 { // second call: free space needs the paths
		var again peerHelloResp
		if err := p.call(ctx, "hello", peerHelloReq{FreePaths: freePaths}, &again); err != nil {
			return nil, err
		}
		return &again, nil
	}
	return &resp, nil
}

// call runs one peer op on the target.
func (p *peerConn) call(ctx context.Context, op string, req, resp any) error {
	if p.bin == "" {
		return errors.New("internal error: peer binary not resolved (hello first)")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	stdout, stderr, runErr := p.runScript(ctx, peerEnvScript, body, p.bin, "host", "peer", op)
	return p.decode(stdout, stderr, runErr, resp)
}

// sshCommandForRsync is the "-e" value that makes rsync use the same ssh options
// and binary as every other call.
func (p *peerConn) sshCommandForRsync() string {
	args := append([]string{sshBinary()}, p.cfg.sshBaseArgs()...)
	args = append(args, "-T")
	quoted := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t'\"") {
			quoted[i] = `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
		} else {
			quoted[i] = a
		}
	}
	return strings.Join(quoted, " ")
}

// rsyncTo mirrors srcDir/ into the target's dstDir/ (created if needed),
// deleting what the source no longer has, resumable by nature.
func (p *peerConn) rsyncTo(ctx context.Context, srcDir, dstDir string, excludes []string, stdout io.Writer) error {
	if !strings.HasSuffix(srcDir, "/") {
		srcDir += "/"
	}
	args := []string{"-a", "--delete", "--partial", "-e", p.sshCommandForRsync()}
	for _, x := range excludes {
		args = append(args, "--exclude="+x)
	}
	args = append(args, srcDir, p.cfg.Host+":"+rsyncQuotePath(dstDir)+"/")
	cmd := exec.CommandContext(ctx, "rsync", args...) //nolint:gosec // validated target and paths
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rsync %s → %s:%s: %w: %s", srcDir, p.cfg.Host, dstDir, err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// rsyncQuotePath protects spaces in a remote path (the remote shell splits it).
func rsyncQuotePath(p string) string {
	return strings.ReplaceAll(p, " ", `\ `)
}
