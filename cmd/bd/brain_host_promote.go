// Package main — brain_host_promote.go
//
// 'brain host promote <ssh-target>': move the brain host role from this machine
// to another, end to end, in phases that can each be repeated safely.
//
//	preflight  seed  files  freeze  delta  restore  serve  parity  fence  flip  clients  verify
//
// State is written after every phase and every step that changes a machine is
// journaled first, so a promotion killed at any instant resumes by running the
// same command again, and 'brain host rollback' puts both machines back.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type promoter struct {
	home string
	opts promoteOptions
	st   *promoteState
	peer *peerConn
	out  io.Writer
	in   io.Reader
	svc  *serviceManager
	j    *journal
	plan *promotePlan
	prep *peerPrepareResp
}

func newPromoter(home string, opts promoteOptions, st *promoteState, out io.Writer, in io.Reader) (*promoter, error) {
	peer, err := newPeerConn(opts.Target, opts.SSHOptions)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(promoteDir(home, st.ID), 0o700); err != nil {
		return nil, err
	}
	j, err := openJournal(promoteDir(home, st.ID))
	if err != nil {
		return nil, err
	}
	return &promoter{home: home, opts: opts, st: st, peer: peer, out: out, in: in, svc: newServiceManager(home), j: j}, nil
}

func (p *promoter) say(format string, a ...any) { fmt.Fprintf(p.out, format+"\n", a...) }

func (p *promoter) save() error { return p.st.save(p.home) }

func (p *promoter) mark(name string) error { return p.st.markDone(p.home, name) }

// run executes every phase that is not yet done.
func (p *promoter) run(ctx context.Context) error {
	if err := p.preflight(ctx); err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	if p.opts.StopAfter == "preflight" {
		p.say("\nStopped after preflight, as asked. Run the same command again to continue.")
		return nil
	}
	phases := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"seed", p.phaseSeed}, {"files", p.phaseFiles}, {"freeze", p.phaseFreeze}, {"delta", p.phaseDelta},
		{"restore", p.phaseRestore}, {"serve", p.phaseServe}, {"parity", p.phaseParity}, {"fence", p.phaseFence},
		{"flip", p.phaseFlip}, {"clients", p.phaseClients}, {"verify", p.phaseVerify},
	}
	for i, ph := range phases {
		if p.st.phaseDone(ph.name) {
			p.say("[%2d/12] %-8s already done (%s)", i+2, ph.name, p.st.Done[ph.name])
		} else {
			start := time.Now()
			p.say("\n[%2d/12] %s", i+2, ph.name)
			if err := ph.fn(ctx); err != nil {
				p.st.logf("phase %s failed: %v", ph.name, err)
				_ = p.save()
				return fmt.Errorf("phase %s: %w", ph.name, err)
			}
			if err := p.mark(ph.name); err != nil {
				return err
			}
			p.say("        done in %s", time.Since(start).Round(time.Millisecond))
		}
		if ph.name == p.opts.StopAfter {
			p.say("\nStopped after %s, as asked. Run the same command again to continue.", ph.name)
			return nil
		}
	}
	p.st.Outcome = outcomeDone
	if err := p.save(); err != nil {
		return err
	}
	clearCurrentPromote(p.home)
	return nil
}

// ---------------------------------------------------------------------------
// preflight
// ---------------------------------------------------------------------------

func (p *promoter) preflight(ctx context.Context) error {
	if p.st.phaseDone("flip.demote") {
		// The old host is already a client; only the target's state is needed.
		_, err := p.peer.hello(ctx, p.opts.PeerBin, nil)
		return err
	}
	pl, err := p.buildPlan(ctx)
	if err != nil {
		return err
	}
	p.plan = pl
	if p.opts.DryRun {
		return nil
	}
	if len(pl.Blockers) > 0 {
		pl.print(p.out, p.opts)
		return errors.New("refusing to start: see the reasons above")
	}
	first := !p.st.phaseDone("preflight")
	if first {
		pl.print(p.out, p.opts)
		if !p.confirm() {
			return errors.New("not confirmed; nothing was changed (--yes to proceed without asking)")
		}
	} else {
		p.say("Resuming promotion %s to %s (%d phases done).", p.st.ID, p.opts.Target, len(p.st.Done))
		for _, w := range pl.Warnings {
			p.say("  ! %s", w)
		}
	}
	p.st.TargetHome, p.st.PeerBin, p.st.Generation = pl.TargetHome, pl.Hello.PeerBin, pl.Generation
	p.peer.bin = pl.Hello.PeerBin
	if len(p.st.Databases) == 0 {
		for _, db := range pl.Layout.Databases {
			p.st.Databases = append(p.st.Databases, dbCopy{Name: db, Bytes: pl.DBBytes[db]})
		}
	}
	s := &p.st.Summary
	s.SourceServer = pl.Layout.Server
	s.TargetServer = hostServer{DataDir: pl.DataDir, Bind: pl.Bind, Port: pl.Port, RemotesAPIPort: p.opts.RemotesAPIPort, Databases: pl.Layout.Databases}
	s.StagingDir = pl.StagingDir
	s.BinDir = filepath.Join(pl.TargetHome, ".local", "bin")
	s.Stores, s.StoreBeads, s.StageBeads, s.FinalBeads = nil, nil, nil, nil
	for _, sm := range pl.Stores {
		s.Stores = append(s.Stores, sm.Name)
		s.StoreBeads = append(s.StoreBeads, sm.SrcBeads)
		s.StageBeads = append(s.StageBeads, filepath.Join(pl.StagingDir, "stores", sm.Name, ".beads"))
		s.FinalBeads = append(s.FinalBeads, filepath.Join(sm.DstRoot, ".beads"))
	}
	s.Wrappers = nil
	for _, w := range pl.Wrappers {
		s.Wrappers = append(s.Wrappers, filepath.Base(w.Path))
	}
	p.st.ClientTargets = pl.Clients
	if err := p.save(); err != nil {
		return err
	}
	if first {
		if err := p.mark("preflight"); err != nil {
			return err
		}
	}
	return nil
}

func (p *promoter) confirm() bool {
	if p.opts.Yes || p.opts.StopAfter == "preflight" {
		return true
	}
	f, ok := p.in.(*os.File)
	if !ok || !fdIsTerminal(f) {
		return false
	}
	fmt.Fprintf(p.out, "\nProceed with the promotion to %s? [y/N] ", p.opts.Target)
	line, _ := bufio.NewReader(p.in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// copying
// ---------------------------------------------------------------------------

var storeExcludes = []string{
	"/.beads/backup/", "/.beads/dolt/", "/.beads/embeddeddolt/",
	"/.beads/*.log", "/.beads/dolt-server.pid", "/.beads/dolt-server.port", "/.beads/dolt-server.lock",
}

func (p *promoter) ensurePrep(ctx context.Context) error {
	if p.prep != nil {
		return nil
	}
	var resp peerPrepareResp
	if err := p.peer.call(ctx, "prepare", peerPrepareReq{ID: p.st.ID, Source: machineID(p.home), DataDir: p.st.Summary.TargetServer.DataDir}, &resp); err != nil {
		return fmt.Errorf("preparing the target's staging area: %w", err)
	}
	p.prep = &resp
	return nil
}

func (p *promoter) localBackupDir(db string) string {
	return filepath.Join(promoteDir(p.home, p.st.ID), "backup", db)
}

// copyDB backs the database up and ships the backup to the target. The hash is
// read before and after the backup: equal means the backup holds exactly that
// state (always so while writes are frozen).
func (p *promoter) copyDB(ctx context.Context, db string, frozen bool) (hash string, exact bool, err error) {
	port := p.st.Summary.SourceServer.Port
	before, err := databaseHash(ctx, port, db)
	if err != nil {
		return "", false, err
	}
	dir := p.localBackupDir(db)
	if err := backupSync(ctx, port, db, dir); err != nil {
		return "", false, err
	}
	after, err := databaseHash(ctx, port, db)
	if err != nil {
		return "", false, err
	}
	exact = before == after
	if frozen {
		want := p.dbCopy(db).SourceHash
		if !exact || before != want {
			return "", false, fmt.Errorf("database %s changed while writes were frozen (%s at the freeze, %s now): something wrote to it without going through a brain command", db, want, after)
		}
	}
	if err := p.peer.rsyncTo(ctx, dir, filepath.Join(p.prep.BackupRoot, db), nil, nil); err != nil {
		return "", false, err
	}
	return before, exact, nil
}

func (p *promoter) dbCopy(db string) *dbCopy {
	for i := range p.st.Databases {
		if p.st.Databases[i].Name == db {
			return &p.st.Databases[i]
		}
	}
	return nil
}

// syncFiles copies store directories (and the Dolt user files) to the target's
// staging area. Safe to repeat: rsync sends only what differs.
func (p *promoter) syncFiles(ctx context.Context) error {
	if err := p.ensurePrep(ctx); err != nil {
		return err
	}
	type job struct{ src, dst string }
	var jobs []job
	for _, sm := range p.plan.Stores {
		if hostPathExists(sm.SrcRoot) {
			jobs = append(jobs, job{sm.SrcRoot, filepath.Join(p.prep.StoresRoot, sm.Name)})
		}
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	var mu sync.Mutex
	var firstErr error
	for _, jb := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(jb job) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := p.peer.rsyncTo(ctx, jb.src, jb.dst, storeExcludes, nil); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(jb)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if p.plan.Layout.DoltCfg && !p.opts.NoDoltcfg {
		if err := p.peer.rsyncTo(ctx, filepath.Join(p.plan.Layout.Server.DataDir, ".doltcfg"), filepath.Join(p.prep.FilesRoot, "doltcfg"), nil, nil); err != nil {
			return err
		}
	}
	p.say("        %d store directories in step", len(jobs))
	return nil
}

func (p *promoter) restoreAll(ctx context.Context, want map[string]string) (*peerRestoreResp, error) {
	var resp peerRestoreResp
	if err := p.peer.call(ctx, "restore", peerRestoreReq{ID: p.st.ID, Databases: want}, &resp); err != nil {
		return nil, fmt.Errorf("restoring on the target: %w", err)
	}
	return &resp, nil
}

func (p *promoter) phaseSeed(ctx context.Context) error {
	if p.opts.NoSeed {
		p.say("        skipped (--no-seed): the whole copy will happen while writes are paused")
		return nil
	}
	if err := p.ensurePrep(ctx); err != nil {
		return err
	}
	want := map[string]string{}
	for _, d := range p.st.Databases {
		start := time.Now()
		hash, exact, err := p.copyDB(ctx, d.Name, false)
		if err != nil {
			return err
		}
		if exact {
			want[d.Name] = hash
		} else {
			want[d.Name] = "" // writes continued during the copy; the restore is checked for validity, not for a hash
		}
		p.say("        %s copied to the target in %s (%s on disk)", d.Name, time.Since(start).Round(time.Millisecond), humanBytes(d.Bytes))
	}
	start := time.Now()
	resp, err := p.restoreAll(ctx, want)
	if err != nil {
		return err
	}
	p.say("        restored on the target in %s: dolt opens the copy (%d database(s))", time.Since(start).Round(time.Millisecond), len(resp.Hashes))
	return nil
}

func (p *promoter) phaseFiles(ctx context.Context) error {
	return p.syncFiles(ctx)
}

// ---------------------------------------------------------------------------
// freeze / delta / restore
// ---------------------------------------------------------------------------

func (p *promoter) hostBase() *hostConfig {
	cfg, _ := loadHostConfig(p.home)
	if cfg != nil {
		return cfg
	}
	srv := p.st.Summary.SourceServer
	return &hostConfig{Role: roleHost, Generation: p.st.Generation - 1, Since: nowStamp(), Server: &srv}
}

// lineage is this host's chain of promotions plus the one in progress.
func (p *promoter) lineage() []lineageEntry {
	var chain []lineageEntry
	if cfg, _ := loadHostConfig(p.home); cfg != nil {
		for _, e := range cfg.Lineage {
			if e.PromoteID != p.st.ID {
				chain = append(chain, e)
			}
		}
	}
	to := ""
	if p.plan != nil && p.plan.Hello != nil {
		to = p.plan.Hello.MachineID
	}
	return append(chain, lineageEntry{PromoteID: p.st.ID, Generation: p.st.Generation, From: machineID(p.home), To: to, At: p.st.Started})
}

func (p *promoter) writeHostConfig(cfg *hostConfig) error {
	data, err := marshalHostConfig(cfg)
	if err != nil {
		return err
	}
	return p.j.writeFile(hostConfigFile(p.home), data, 0o644) //nolint:gosec // no secrets here
}

func (p *promoter) phaseFreeze(ctx context.Context) error {
	cfg := p.hostBase()
	if cfg.Role != roleFrozen {
		cfg.Role, cfg.FrozenSince, cfg.PromoteID = roleFrozen, nowStamp(), p.st.ID
		if cfg.Server == nil {
			srv := p.st.Summary.SourceServer
			cfg.Server = &srv
		}
		if err := p.writeHostConfig(cfg); err != nil {
			return err
		}
	}
	p.st.Frozen = true
	if err := p.save(); err != nil {
		return err
	}
	p.say("        writes are paused on this machine (reads still work); waiting for in-flight commands")
	port := p.st.Summary.SourceServer.Port
	if p.st.FlipHeads == nil {
		p.st.FlipHeads = map[string]string{}
	}
	for i := range p.st.Databases {
		d := &p.st.Databases[i]
		h, err := quiesce(ctx, port, d.Name, 500*time.Millisecond)
		if err != nil {
			return err
		}
		if d.SourceHash != "" && d.SourceHash != h {
			d.CopiedHash = "" // it moved since an earlier freeze: copy again
		}
		d.SourceHash = h
		p.st.FlipHeads[d.Name] = headCommit(ctx, port, d.Name)
		p.say("        %s frozen at hash %s", d.Name, shortHash(h))
	}
	return p.save()
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func (p *promoter) phaseDelta(ctx context.Context) error {
	if err := p.ensurePrep(ctx); err != nil {
		return err
	}
	for i := range p.st.Databases {
		d := &p.st.Databases[i]
		start := time.Now()
		hash, _, err := p.copyDB(ctx, d.Name, true)
		if err != nil {
			return err
		}
		d.CopiedHash = hash
		if err := p.save(); err != nil {
			return err
		}
		p.say("        %s: the frozen state copied in %s", d.Name, time.Since(start).Round(time.Millisecond))
	}
	return p.syncFiles(ctx)
}

func (p *promoter) wantHashes() map[string]string {
	want := map[string]string{}
	for _, d := range p.st.Databases {
		want[d.Name] = d.CopiedHash
	}
	return want
}

func (p *promoter) phaseRestore(ctx context.Context) error {
	resp, err := p.restoreAll(ctx, p.wantHashes())
	if err != nil {
		return err
	}
	for _, d := range p.st.Databases {
		how := "rebuilt"
		if containsStr(resp.Reused, d.Name) {
			how = "kept from the seed (nothing had changed)"
		}
		p.say("        %s %s on the target; its hash equals the frozen %s", d.Name, how, shortHash(resp.Hashes[d.Name]))
	}
	return nil
}

func (p *promoter) phaseServe(ctx context.Context) error {
	req := peerServeReq{
		ID: p.st.ID, DataDir: p.st.Summary.TargetServer.DataDir, Bind: p.st.Summary.TargetServer.Bind, Port: p.st.Summary.TargetServer.Port,
		RemotesAPI: p.opts.RemotesAPIPort, Service: p.plan.Service, ServiceLabel: p.opts.ServiceLabel,
		Databases: p.wantHashes(), ReplaceStale: p.opts.ReplaceStale,
		CopyDoltCfg: p.plan.Layout.DoltCfg && !p.opts.NoDoltcfg,
	}
	var resp peerServeResp
	if err := p.peer.call(ctx, "serve", req, &resp); err != nil {
		return fmt.Errorf("starting the server on the target: %w", err)
	}
	p.st.Summary.TargetServer = resp.Server
	if err := p.save(); err != nil {
		return err
	}
	verb := "was already running"
	if resp.Started {
		verb = "started"
	}
	p.say("        the Dolt server %s on the target (%s, port %d, %s)", verb, resp.Server.Service.Kind, resp.Server.Port, resp.Server.Bind)
	for db, h := range resp.Hashes {
		p.say("        %s served with hash %s — equal to the frozen database", db, shortHash(h))
	}
	return nil
}

// ---------------------------------------------------------------------------
// parity
// ---------------------------------------------------------------------------

func (p *promoter) phaseParity(ctx context.Context) error {
	s := p.st.Summary
	dbs := make([]string, 0, len(p.st.Databases))
	for _, d := range p.st.Databases {
		dbs = append(dbs, d.Name)
	}
	// Both manifests are read at the same time: the old host is frozen, so its
	// manifest is as it was at the freeze, and reading it here keeps it out of
	// the pause's critical path.
	var resp peerManifestResp
	var perr error
	srcManifests := map[string]*dbManifest{}
	var serr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		perr = p.peer.call(ctx, "manifest", peerManifestReq{Port: s.TargetServer.Port, Databases: dbs}, &resp)
	}()
	go func() {
		defer wg.Done()
		for _, d := range p.st.Databases {
			m, err := computeManifest(ctx, s.SourceServer.Port, d.Name)
			if err != nil {
				serr = err
				return
			}
			if m.Hash != d.SourceHash {
				serr = fmt.Errorf("database %s on this machine changed after the freeze (%s → %s)", d.Name, shortHash(d.SourceHash), shortHash(m.Hash))
				return
			}
			srcManifests[d.Name] = m
		}
	}()
	wg.Wait()
	if perr != nil {
		return perr
	}
	if serr != nil {
		return serr
	}
	var problems []string
	for _, db := range dbs {
		src := *srcManifests[db]
		dst := resp.Manifests[db]
		if dst == nil {
			problems = append(problems, db+": the target returned no manifest")
			continue
		}
		diffs := compareManifests(&src, dst)
		for _, d := range diffs {
			problems = append(problems, db+": "+d)
		}
		p.say("        %s: %d tables and %d stores compared, %d difference(s)", db, len(src.Tables), len(src.Stores), len(diffs))
		if len(diffs) == 0 {
			var issues int64
			for _, st := range src.Stores {
				issues += st.Tables["issues"].Rows
			}
			if issues > 0 {
				p.say("        %s: %d issues across %d stores — every store's rows and checksums equal on both sides", db, issues, len(src.Stores))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("the copy is not identical to the source:\n  - %s", strings.Join(problems, "\n  - "))
	}
	if p.opts.NoSmoke {
		return nil
	}
	return p.smokeParity(ctx)
}

// smokeParity runs `count` through each store's real config on both copies.
func (p *promoter) smokeParity(ctx context.Context) error {
	s := p.st.Summary
	srcReq := peerSmokeReq{Port: s.SourceServer.Port}
	dstReq := peerSmokeReq{Port: s.TargetServer.Port}
	for i, name := range s.Stores {
		srcReq.Stores = append(srcReq.Stores, peerSmokeStore{Name: name, BeadsDir: s.StoreBeads[i]})
		meta := ""
		for _, sm := range p.plan.Stores {
			if sm.Name == name {
				meta = sm.Metadata
			}
		}
		dstReq.Stores = append(dstReq.Stores, peerSmokeStore{Name: name, BeadsDir: s.StageBeads[i], Metadata: meta})
	}
	env := &peerEnv{home: p.home, svc: p.svc}
	var src, dst *peerSmokeResp
	var serr, derr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); src, serr = env.smoke(ctx, srcReq) }()
	go func() {
		defer wg.Done()
		var r peerSmokeResp
		if derr = p.peer.call(ctx, "smoke", dstReq, &r); derr == nil {
			dst = &r
		}
	}()
	wg.Wait()
	if serr != nil {
		return serr
	}
	if derr != nil {
		return derr
	}
	if p.st.Baseline == nil {
		p.st.Baseline = map[string]int64{}
	}
	var bad []string
	byMessage := map[string][]string{}
	var order []string
	note := func(msg, store string) {
		if _, ok := byMessage[msg]; !ok {
			order = append(order, msg)
		}
		byMessage[msg] = append(byMessage[msg], store)
	}
	for i, r := range src.Results {
		d := dst.Results[i]
		switch {
		case r.Error != "":
			note("`count` fails on the old host: "+r.Error, r.Name)
		case d.Error != "":
			note("`count` fails on the new host: "+d.Error, r.Name)
		case r.Count != d.Count:
			bad = append(bad, fmt.Sprintf("%s: %d issues here, %d there", r.Name, r.Count, d.Count))
		default:
			p.st.Baseline[r.Name] = r.Count
		}
	}
	for _, msg := range order {
		bad = append(bad, fmt.Sprintf("%s  (%d store(s): %s)", msg, len(byMessage[msg]), joinMax(byMessage[msg], 5)))
	}
	if len(bad) > 0 {
		return fmt.Errorf("stores that do not answer alike on both copies:\n  - %s", strings.Join(bad, "\n  - "))
	}
	var total int64
	for _, n := range p.st.Baseline {
		total += n
	}
	p.say("        `count` through each store's own config agrees on all %d stores (%d issues)", len(src.Results), total)
	return p.save()
}

// ---------------------------------------------------------------------------
// fence / flip
// ---------------------------------------------------------------------------

func (p *promoter) phaseFence(ctx context.Context) error {
	port := p.st.Summary.SourceServer.Port
	for _, d := range p.st.Databases {
		h, err := databaseHash(ctx, port, d.Name)
		if err != nil {
			return err
		}
		if h != d.SourceHash {
			return fmt.Errorf("database %s on this machine changed after the freeze (%s → %s): something wrote to it behind the freeze. Nothing has been switched; re-run to copy again, or 'brain host rollback'", d.Name, shortHash(d.SourceHash), shortHash(h))
		}
	}
	dbs := make([]string, 0, len(p.st.Databases))
	for _, d := range p.st.Databases {
		dbs = append(dbs, d.Name)
	}
	var resp peerHashesResp
	if err := p.peer.call(ctx, "hashes", peerHashesReq{Port: p.st.Summary.TargetServer.Port, Databases: dbs}, &resp); err != nil {
		return err
	}
	p.st.FlipHashes = map[string]string{}
	for _, d := range p.st.Databases {
		if resp.Hashes[d.Name] != d.SourceHash {
			return fmt.Errorf("database %s on the new host no longer matches the frozen copy (%s vs %s)", d.Name, shortHash(resp.Hashes[d.Name]), shortHash(d.SourceHash))
		}
		p.st.FlipHashes[d.Name] = d.SourceHash
	}
	p.say("        both copies still hash to the frozen state; nothing wrote to either since the freeze")
	return p.save()
}

func (p *promoter) phaseFlip(ctx context.Context) error {
	if !p.st.phaseDone("flip.activate") {
		if err := p.activateTarget(ctx); err != nil {
			return err
		}
		if err := p.mark("flip.activate"); err != nil {
			return err
		}
	}
	if !p.st.phaseDone("flip.demote") {
		if err := p.demoteSource(ctx); err != nil {
			return err
		}
		if err := p.mark("flip.demote"); err != nil {
			return err
		}
	}
	p.st.Outcome = outcomeFlipped
	if err := p.save(); err != nil {
		return err
	}
	if p.opts.StopSourceServer && !p.st.phaseDone("flip.stopsrc") {
		if err := p.stopSourceServer(ctx); err != nil {
			return err
		}
		if err := p.mark("flip.stopsrc"); err != nil {
			return err
		}
	}
	return nil
}

func (p *promoter) activateTarget(ctx context.Context) error {
	if err := p.ensurePrep(ctx); err != nil {
		return err
	}
	pl := p.plan
	req := peerActivateReq{
		ID: p.st.ID, Generation: p.st.Generation, Registry: pl.Registry, Wrappers: pl.Wrappers, Helpers: pl.Helpers,
		Links: pl.Links, Services: pl.Services, Server: p.st.Summary.TargetServer, FlipHashes: p.st.FlipHashes,
		MoveAside: p.opts.MoveAside || p.opts.ReplaceStale, ForceWrappers: p.opts.ForceWrappers, ReplaceStale: p.opts.ReplaceStale,
	}
	for _, s := range pl.Services {
		req.DeclaredServices = append(req.DeclaredServices, s.Label)
	}
	for _, c := range p.st.ClientTargets {
		req.Clients = append(req.Clients, hostClient{Target: c, Added: nowStamp()})
	}
	req.Lineage = p.lineage()
	for _, sm := range pl.Stores {
		req.Stores = append(req.Stores, peerStoreMove{Name: sm.Name, Stage: filepath.Join(p.prep.StoresRoot, sm.Name), Final: sm.DstRoot, Metadata: sm.Metadata})
	}
	var resp peerActivateResp
	if err := p.peer.call(ctx, "activate", req, &resp); err != nil {
		return fmt.Errorf("activating the target: %w", err)
	}
	p.say("        %s is now the brain host: %d stores, %d wrappers, %d agent links, %d jobs", p.opts.Target, resp.Stores, resp.Wrappers, resp.Links, resp.Services)
	for _, n := range resp.Notes {
		p.say("        note: %s", n)
	}
	return nil
}

// demoteSource turns this machine into a client of the new host. Every file it
// replaces is journaled first; host.yaml, the marker of the role change, is
// written last.
func (p *promoter) demoteSource(ctx context.Context) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if r, rerr := filepath.EvalSymlinks(self); rerr == nil {
		self = r
	}
	n := 0
	for _, w := range p.plan.Layout.Wrappers {
		if err := p.j.writeFile(w.Path, []byte(remoteWrapperBody(w.Name, self)), 0o755); err != nil { //nolint:gosec // executable wrapper
			return err
		}
		n++
	}
	rc := &remoteConfig{Host: p.opts.Target, SSHOptions: p.opts.SSHOptions}
	data, err := yaml.Marshal(rc)
	if err != nil {
		return err
	}
	header := "# brain client mode — managed by 'brain remote'. Commands run on this host over ssh.\n"
	if err := p.j.writeFile(remoteConfigFile(), append([]byte(header), data...), 0o644); err != nil { //nolint:gosec // no secrets here
		return err
	}
	for _, svc := range p.plan.Layout.Services {
		if !svc.Declared {
			continue
		}
		hs := hostService{Kind: "launchd", Label: svc.Label, Plist: svc.Plist}
		if err := p.svc.stop(ctx, hs, 0, true, nil, p.j); err != nil {
			p.say("        note: could not stop job %s here: %v", svc.Label, err)
		}
	}
	// The durable forwarding record: clients that still point here (offline
	// during the move, or never registered) are answered from it.
	rec := &forwardRecord{To: p.opts.Target, PromoteID: p.st.ID, Generation: p.st.Generation, FromMachine: machineID(p.home), MovedAt: nowStamp()}
	recData, err := marshalForwardRecord(rec)
	if err != nil {
		return err
	}
	if err := p.j.writeFile(forwardRecordFile(p.home), recData, 0o644); err != nil { //nolint:gosec // no secrets here
		return err
	}
	cfg := p.hostBase()
	cfg.Role, cfg.MovedTo, cfg.MovedAt, cfg.PromoteID, cfg.FrozenSince = roleMoved, p.opts.Target, rec.MovedAt, p.st.ID, ""
	cfg.Lineage = p.lineage()
	if err := p.writeHostConfig(cfg); err != nil {
		return err
	}
	p.say("        this machine is now a client of %s (%d wrappers forward there; its own data is untouched)", p.opts.Target, n)
	return nil
}

func (p *promoter) stopSourceServer(ctx context.Context) error {
	srv := p.st.Summary.SourceServer
	doltBin, _, _ := doltBinary()
	spec := &serverSpec{DoltBin: doltBin, DataDir: srv.DataDir, Bind: srv.Bind, Port: srv.Port, RemotesAPIPort: srv.RemotesAPIPort}
	if err := p.svc.stop(ctx, srv.Service, srv.Port, true, spec, p.j); err != nil {
		return fmt.Errorf("stopping the old Dolt server: %w", err)
	}
	p.st.ServerStopped = true
	p.say("        the old Dolt server is stopped (%s); its data is untouched", srv.Service.Kind)
	return p.save()
}

// ---------------------------------------------------------------------------
// clients / verify
// ---------------------------------------------------------------------------

const clientFollowScript = `d="$HOME/.local/bin"; PATH="$d:/opt/homebrew/bin:/usr/local/bin:$PATH"; BRAIN_REMOTE=off; export PATH BRAIN_REMOTE; new="$1"; shift; ` +
	`for c in "$@"; do if "$c" remote --help >/dev/null 2>&1 </dev/null; then "$c" remote set "$new" && "$c" remote install; exit $?; fi; done; ` +
	`echo "no brain binary with client mode found" >&2; exit 127`

func (p *promoter) phaseClients(ctx context.Context) error {
	if len(p.st.ClientTargets) == 0 {
		p.say("        no known clients (add some with 'brain host clients add'); a client that still points at the old host is redirected the first time it runs a command")
		return nil
	}
	p.st.Clients, p.st.Failed = nil, nil
	for _, c := range p.st.ClientTargets {
		if c == p.opts.Target {
			continue
		}
		conn, err := newPeerConn(c, p.opts.SSHOptions)
		if err != nil {
			p.st.Failed = append(p.st.Failed, c+": "+err.Error())
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		stdout, stderr, rerr := conn.runScript(cctx, clientFollowScript, nil, p.opts.Target, "brain", "bd")
		cancel()
		if rerr != nil {
			msg := strings.TrimSpace(string(stderr))
			if msg == "" {
				msg = rerr.Error()
			}
			p.st.Failed = append(p.st.Failed, c+": "+hostFirstLine(msg))
			p.say("        ✗ %s: %s", c, hostFirstLine(msg))
			continue
		}
		_ = stdout
		p.st.Clients = append(p.st.Clients, c)
		p.say("        ✓ %s now follows %s", c, p.opts.Target)
	}
	if len(p.st.Failed) > 0 {
		p.say("        %d client(s) could not be updated now; each is redirected the first time it runs a command, or run on it: brain remote set %s", len(p.st.Failed), p.opts.Target)
	}
	return p.save()
}

func (p *promoter) phaseVerify(ctx context.Context) error {
	s := p.st.Summary
	var st peerStatusResp
	if err := p.peer.call(ctx, "status", peerStatusReq{ID: p.st.ID, Check: true}, &st); err != nil {
		return err
	}
	var problems []string
	switch {
	case st.Host == nil || st.Host.Role != roleHost:
		problems = append(problems, "the new host's host.yaml does not say it is the host")
	case st.Host.Generation != p.st.Generation:
		problems = append(problems, fmt.Sprintf("the new host is generation %d, expected %d", st.Host.Generation, p.st.Generation))
	}
	local, _ := loadHostConfig(p.home)
	if local == nil || local.Role != roleMoved || local.MovedTo != p.opts.Target {
		problems = append(problems, "this machine's host.yaml does not say it moved to the target")
	}
	for _, d := range p.st.Databases {
		if st.Hashes[d.Name] != d.SourceHash {
			problems = append(problems, fmt.Sprintf("database %s on the new host differs from the frozen copy (it has been written to since — that is expected once you use it)", d.Name))
		}
	}
	if len(problems) == 0 {
		p.say("        the new host is generation %d, serves the frozen databases, and this machine records the move", p.st.Generation)
	}

	// Through the old host's own client config, the way a user would.
	rc, err := loadRemoteConfig()
	if err == nil && rc.Host == p.opts.Target {
		if len(s.Wrappers) == 0 {
			p.say("        (no store wrappers were moved, so the client path was not exercised)")
		} else if cerr := checkClientPath(ctx, rc, s.Wrappers[0]); cerr != nil {
			problems = append(problems, "reaching the new host from here as a client failed: "+cerr.Error())
		} else {
			p.say("        this machine, now a client, runs '%s' on the new host over ssh", s.Wrappers[0])
		}
	}

	if !p.opts.NoSmoke {
		req := peerSmokeReq{Port: s.TargetServer.Port}
		for i, name := range s.Stores {
			wrapper := filepath.Join(s.BinDir, name)
			if !containsStr(s.Wrappers, name) {
				req.Stores = append(req.Stores, peerSmokeStore{Name: name, BeadsDir: s.FinalBeads[i]})
				continue
			}
			req.Stores = append(req.Stores, peerSmokeStore{Name: name, Wrapper: wrapper, BeadsDir: s.FinalBeads[i]})
		}
		var sm peerSmokeResp
		if err := p.peer.call(ctx, "smoke", req, &sm); err != nil {
			return err
		}
		drift := 0
		for _, r := range sm.Results {
			switch {
			case r.Error != "":
				problems = append(problems, fmt.Sprintf("%s: the new host's wrapper fails: %s", r.Name, r.Error))
			case p.st.Baseline != nil && p.st.Baseline[r.Name] != r.Count:
				drift++
			}
		}
		p.say("        each store answers through its installed wrapper on the new host (%d stores; %d changed since the freeze)", len(sm.Results), drift)
	}
	if len(problems) > 0 {
		return fmt.Errorf("the promotion completed but verification found:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// misc
// ---------------------------------------------------------------------------

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

func writeJSON(w io.Writer, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	_, _ = w.Write(buf.Bytes())
}

// checkClientPath runs a store command on the host the way client mode does and
// reports whether it answered.
func checkClientPath(ctx context.Context, rc *remoteConfig, name string) error {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := remoteCapture(cctx, rc, name, "--version")
	if err != nil {
		return err
	}
	if !strings.Contains(string(out), "version") {
		return fmt.Errorf("%s --version answered %q", name, hostFirstLine(string(out)))
	}
	return nil
}
