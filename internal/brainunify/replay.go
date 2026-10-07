package brainunify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// A merged database is built from a moment in the past, and every change a
// source makes while (or after) the build runs exists only in the source.
// `replay` carries those changes into the merged database: it reads each
// source's history from the commit the build recorded
// (brain_unify_source_commits), finds the beads and the database-state tables
// that changed, and re-reads exactly those from the sources as they stand now.
//
// What the replay promises, and how:
//
//   - The merged database ends equal to what the sources hold NOW, under the
//     same winner rule the build used for duplicated ids. The rule is not
//     reimplemented: the plan handed in was computed by Discover, the same
//     function the build uses, and the replay applies its collisions.
//   - It never applies a diff row's values. A diff only names WHICH beads
//     changed; the rows written are re-read from the sources through the
//     build's own column encoding, so inserts, updates and deletes are all one
//     operation — "make this bead's rows in the merged database equal what the
//     sources hold for it" — and a winner that moved between stores is handled
//     by the same operation as a plain edit.
//   - Every condition it cannot resolve confidently is a loud refusal naming
//     the store and the table. Every decision is made, and every source is
//     read, before the first write, and the writes run in one transaction, so a
//     refused replay leaves the merged database as it found it.
//   - Sources are only ever read. Every source statement passes
//     readOnlySource's SELECT guard.
//
// The acceptance test is the verifier: `verify --reference live` recomputes
// the expected side from the sources as they stand and compares it with the
// merged database, so a replay that missed a change fails it.

// ReplayOptions configures a replay.
type ReplayOptions struct {
	// DataDir is the directory holding the merged database. The replay starts
	// its own Dolt server over it, as verify does.
	DataDir string
	// Database is the merged database's name inside that server.
	Database string
	// DoltBin is the dolt binary used to start the server.
	DoltBin string
	// Host and Port locate the Dolt server holding the sources.
	Host string
	Port int
	// AllowCollisions mirrors the build's flag: a replay that would resolve a
	// duplicated id by discarding content the copies disagree on is refused
	// without it.
	AllowCollisions bool
	// Logf receives progress lines.
	Logf func(format string, args ...any)
}

// ReplayTableStats is what a replay did to one table of the merged database.
type ReplayTableStats struct {
	Table string
	// Deleted counts rows removed from the merged table before the reload.
	Deleted int64
	// Inserted counts rows written from the sources.
	Inserted int64
	// Skipped counts source rows not written because their bead's copy in that
	// store lost an id collision.
	Skipped int64
	// Full reports that the table was reloaded whole because the sources keep
	// no history for it (a table Dolt does not version, such as wisps).
	Full bool
}

// ReplayResult is the outcome of a replay.
type ReplayResult struct {
	Database string
	DataDir  string
	// Beads are the bead ids whose rows were reconciled, sorted.
	Beads []string
	// Tables is one entry per merged table the replay wrote to.
	Tables []ReplayTableStats
	// ChangedStores are the stores whose history showed changes.
	ChangedStores []string
	// CollisionsWritten and CollisionsRemoved are the ids whose
	// brain_unify_collisions record was written or deleted.
	CollisionsWritten []string
	CollisionsRemoved []string
	// Commits are the new starting points recorded for the next replay.
	Commits []recordedSourceCommit
	// Committed reports whether the result was committed to the merged
	// database's own Dolt history.
	Committed     bool
	Elapsed       time.Duration
	ServerLogPath string
}

// Replayer carries a replay run.
type Replayer struct {
	source *readOnlySource
	plan   Plan
	opts   ReplayOptions
	log    func(format string, args ...any)

	cols map[string][]string
}

// NewReplayer returns a replayer reading the sources through source and
// mapping them by plan. The plan must have been computed from the sources now,
// by Discover, exactly as for a build.
func NewReplayer(source *readOnlySource, plan Plan, opts ReplayOptions) *Replayer {
	log := opts.Logf
	if log == nil {
		log = func(string, ...any) {}
	}
	return &Replayer{source: source, plan: plan, opts: opts, log: log, cols: map[string][]string{}}
}

// replayWork is everything the decision phase settled, before any write.
type replayWork struct {
	bases map[string]recordedSourceCommit
	heads map[string]string
	conns map[string]*readOnlySource
	plans []TablePlan
	// template is the store whose state also seeds the merged database's own
	// single-valued state tables.
	template string

	// touched maps a bead-scoped table to the ids to reconcile in it.
	touched map[string]map[string]bool
	// full marks bead-scoped tables the sources keep no history for.
	full map[string]bool
	// changedTables maps a store to the bead-scoped tables its history showed
	// changes in.
	changedTables map[string]map[string]bool
	// stateChanged maps a store to the state tables to replace for it.
	stateChanged map[string]map[string]bool
	// changedStores are the stores whose history showed changes.
	changedStores map[string]bool

	live map[string]Collision
	// losers maps a duplicated id to the stores whose copy of it lost.
	losers map[string]map[string]bool
	// collisionsToWrite are ids whose record is written; collisionsToDelete
	// are recorded ids that are no longer duplicated.
	collisionsToWrite  []string
	collisionsToDelete []string
	// wide are the stores whose fingerprints are re-recorded for every table
	// because a collision they take part in changed.
	wide map[string]bool
}

// Replay runs the replay.
//
//nolint:gocyclo // one linear pipeline: validate, decide, apply, re-record
func (r *Replayer) Replay(ctx context.Context) (ReplayResult, error) {
	start := time.Now()
	if r.opts.DataDir == "" {
		return ReplayResult{}, fmt.Errorf("--data-dir is required: name the directory that holds the merged database")
	}
	database := r.opts.Database
	if database == "" {
		database = "brain_unified"
	}
	host := r.opts.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := r.opts.Port
	if port == 0 {
		port = 3307
	}
	res := ReplayResult{Database: database, DataDir: r.opts.DataDir}

	// One read-only connection per source database: dolt_log and dolt_diff
	// resolve against the connection's default database.
	conns, err := r.openSourceConns(ctx, host, port)
	if err != nil {
		return res, err
	}
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()

	srv, err := StartIsolatedServer(ctx, r.opts.DoltBin, r.opts.DataDir)
	if err != nil {
		return res, err
	}
	defer func() { _ = srv.Stop() }()
	res.ServerLogPath = filepath.Join(r.opts.DataDir, "unified-server.log")
	r.log("isolated dolt server over %s on 127.0.0.1:%d", r.opts.DataDir, srv.Port)

	merged, err := srv.OpenTarget(ctx, database)
	if err != nil {
		return res, err
	}
	defer func() { _ = merged.Close() }()
	mergedRO, err := OpenSource("127.0.0.1", srv.Port)
	if err != nil {
		return res, err
	}
	defer func() { _ = mergedRO.Close() }()

	w := &replayWork{conns: conns}

	// ---- validate: every premise is checked loudly before anything is read --
	if err := r.validateMerged(ctx, mergedRO, database); err != nil {
		return res, err
	}
	if w.bases, err = r.readRecordedCommits(ctx, mergedRO, database); err != nil {
		return res, err
	}
	if err := r.validateCommits(ctx, w); err != nil {
		return res, err
	}
	if w.template, err = r.readTemplateStore(ctx, mergedRO, database); err != nil {
		return res, err
	}
	if w.plans, err = TablePlansFor(ctx, r.source, r.plan, w.template); err != nil {
		return res, err
	}
	imported, err := r.readImportLog(ctx, mergedRO, database)
	if err != nil {
		return res, err
	}
	if err := r.validateSchema(ctx, mergedRO, database, w.plans); err != nil {
		return res, err
	}
	recorded, err := r.readCollisionRecords(ctx, mergedRO, database)
	if err != nil {
		return res, err
	}

	// ---- decide ------------------------------------------------------------
	if err := r.collectChanges(ctx, mergedRO, database, w, imported); err != nil {
		return res, err
	}
	if err := r.reconcileCollisions(w, recorded); err != nil {
		return res, err
	}

	// ---- apply: one transaction, so a failure leaves the database as found --
	conn, err := merged.Conn(ctx)
	if err != nil {
		return res, fmt.Errorf("opening a connection to the merged database: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "set foreign_key_checks = 0"); err != nil {
		return res, fmt.Errorf("disabling foreign key checks on the merged database: %w", err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("beginning the replay transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	stats := map[string]*ReplayTableStats{}
	if err := r.applyChanges(ctx, tx, mergedRO, database, w, stats); err != nil {
		return res, err
	}
	if err := r.writeCollisionRecords(ctx, tx, w); err != nil {
		return res, err
	}
	if err := r.refreshPrefixes(ctx, tx); err != nil {
		return res, err
	}
	if err := r.rerecordFingerprints(ctx, tx, w); err != nil {
		return res, err
	}
	if res.Commits, err = r.advanceCommits(ctx, tx, w); err != nil {
		return res, err
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("committing the replay transaction: %w", err)
	}
	committed = true

	// The rows are durable once the transaction commits; the Dolt commit
	// makes the replay a point in the merged database's own history.
	if rows, err := merged.QueryContext(ctx, "call dolt_commit('-Am', 'brain unify replay: apply source changes since the recorded commits')"); err != nil {
		r.log("warning: could not create a dolt commit: %v", err)
	} else {
		for rows.Next() {
		}
		_ = rows.Err()
		_ = rows.Close()
		res.Committed = true
		r.log("committed the replay into the merged database's history")
	}

	res.Beads = unionIDs(w.touched)
	res.CollisionsWritten = append(res.CollisionsWritten, w.collisionsToWrite...)
	res.CollisionsRemoved = append(res.CollisionsRemoved, w.collisionsToDelete...)
	res.ChangedStores = sortedKeys(w.changedStores)
	for _, name := range sortedKeys(stats) {
		res.Tables = append(res.Tables, *stats[name])
	}
	res.Elapsed = time.Since(start)
	return res, nil
}

// ---------------------------------------------------------------------------
// validation
// ---------------------------------------------------------------------------

// validateMerged refuses a merged database the replay cannot use: one without
// the recorded starting points, which is the documented rebuild case.
func (r *Replayer) validateMerged(ctx context.Context, mergedRO *readOnlySource, database string) error {
	for _, table := range []string{"brain_unify_source_commits", "brain_unify_collisions", "brain_unify_import_log", "brain_unify_source_fingerprints", "brain_stores"} {
		has, err := mergedRO.HasTable(ctx, database, table)
		if err != nil {
			return fmt.Errorf("refusing to replay: checking %s for table %s: %w", database, table, err)
		}
		if has {
			continue
		}
		if table == "brain_unify_source_commits" {
			return fmt.Errorf("refusing to replay: %s has no brain_unify_source_commits table, so it was built before builds recorded a replay starting point and a replay cannot know what it missed; rebuild it", database)
		}
		return fmt.Errorf("refusing to replay: %s has no %s table, so it is not a database 'unify build' made; rebuild it", database, table)
	}
	return nil
}

// readRecordedCommits reads each source's build-time commit.
func (r *Replayer) readRecordedCommits(ctx context.Context, mergedRO *readOnlySource, database string) (map[string]recordedSourceCommit, error) {
	rows, err := mergedRO.query(ctx, fmt.Sprintf(
		"select `store`, `source_database`, `commit_hash` from `%s`.`brain_unify_source_commits`", database))
	if err != nil {
		return nil, fmt.Errorf("reading brain_unify_source_commits: %w", err)
	}
	defer rows.Close()
	out := map[string]recordedSourceCommit{}
	for rows.Next() {
		var c recordedSourceCommit
		if err := rows.Scan(&c.Store, &c.Database, &c.Hash); err != nil {
			return nil, err
		}
		out[c.Store] = c
	}
	return out, rows.Err()
}

// openSourceConns opens one read-only connection per source database. Two
// stores sharing a database is refused: a replay could not attribute rows.
func (r *Replayer) openSourceConns(ctx context.Context, host string, port int) (map[string]*readOnlySource, error) {
	byDatabase := map[string][]string{}
	for _, s := range r.plan.Sources {
		byDatabase[s.Database] = append(byDatabase[s.Database], s.Namespace)
	}
	conns := map[string]*readOnlySource{}
	for db, stores := range byDatabase {
		if len(stores) > 1 {
			closeAll(conns)
			return nil, fmt.Errorf("refusing to replay: stores %s share one source database %s, so a replay could not attribute its rows; rebuild", strings.Join(stores, ", "), db)
		}
		conn, err := OpenReadOnlyDatabase(ctx, host, port, db)
		if err != nil {
			closeAll(conns)
			return nil, fmt.Errorf("refusing to replay: opening the history of database %s (store %s): %w", db, stores[0], err)
		}
		conns[db] = conn
	}
	return conns, nil
}

func closeAll(conns map[string]*readOnlySource) {
	for _, c := range conns {
		_ = c.Close()
	}
}

// validateCommits refuses every starting point that cannot be trusted:
//
//   - a store with no recorded commit, or a recorded store that no longer
//     participates: the set of sources cannot be cut short or grown quietly;
//   - a store now read from a different database than the one recorded;
//   - a source whose history no longer contains its recorded commit: rewritten
//     or collected history makes "what changed since" unknowable.
func (r *Replayer) validateCommits(ctx context.Context, w *replayWork) error {
	participating := map[string]bool{}
	for _, src := range r.plan.Sources {
		participating[src.Namespace] = true
		base, ok := w.bases[src.Namespace]
		if !ok {
			return fmt.Errorf("refusing to replay: store %s (database %s) has no recorded commit; it joined after the merged database was built, so a replay cannot know what it holds; rebuild", src.Namespace, src.Database)
		}
		if base.Database != src.Database {
			return fmt.Errorf("refusing to replay: store %s was built from database %s but now reads from %s; rebuild", src.Namespace, base.Database, src.Database)
		}
		exists, err := w.conns[src.Database].HasCommit(ctx, base.Hash)
		if err != nil {
			return fmt.Errorf("refusing to replay: reading the history of store %s (database %s): %w", src.Namespace, src.Database, err)
		}
		if !exists {
			return fmt.Errorf("refusing to replay: store %s (database %s) no longer contains its recorded commit %s; its history was rewritten or collected, so what changed since is unknowable; rebuild", src.Namespace, src.Database, base.Hash)
		}
	}
	for _, store := range sortedKeys(w.bases) {
		if !participating[store] {
			return fmt.Errorf("refusing to replay: store %s was built into the merged database but no longer participates (unreachable or unregistered); a replay will not drop a store silently; rebuild or restore it", store)
		}
	}
	return nil
}

// readTemplateStore reads the schema template from the build's own record, so
// a replay classifies tables exactly as the existing schema was classified.
func (r *Replayer) readTemplateStore(ctx context.Context, mergedRO *readOnlySource, database string) (string, error) {
	rows, err := mergedRO.query(ctx, fmt.Sprintf(
		"select `store` from `%s`.`brain_stores` where `template_source` = 1", database))
	if err != nil {
		return "", fmt.Errorf("reading the template store of %s: %w", database, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return "", err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(out) != 1 {
		return "", fmt.Errorf("refusing to replay: %s records %d template stores (%s), so the table classification its schema was built with cannot be re-derived; rebuild", database, len(out), strings.Join(out, ", "))
	}
	return out[0], nil
}

// readImportLog reads which (store, table) pairs the build imported rows from.
func (r *Replayer) readImportLog(ctx context.Context, mergedRO *readOnlySource, database string) (map[string]int64, error) {
	rows, err := mergedRO.query(ctx, fmt.Sprintf(
		"select `store`, `table_name`, `imported_rows` from `%s`.`brain_unify_import_log`", database))
	if err != nil {
		return nil, fmt.Errorf("reading brain_unify_import_log: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var store, table string
		var n int64
		if err := rows.Scan(&store, &table, &n); err != nil {
			return nil, err
		}
		out[store+"\x00"+table] = n
	}
	return out, rows.Err()
}

// validateSchema refuses a merged database whose tables cannot take the rows
// the sources would write: the merged schema is fixed at build time and a
// replay never alters it.
func (r *Replayer) validateSchema(ctx context.Context, mergedRO *readOnlySource, database string, plans []TablePlan) error {
	for _, tp := range plans {
		has, err := mergedRO.HasTable(ctx, database, tp.Target)
		if err != nil {
			return fmt.Errorf("refusing to replay: checking the merged table %s: %w", tp.Target, err)
		}
		if !has {
			return fmt.Errorf("refusing to replay: the sources have table %s but %s has no table %s; the merged schema was fixed at build time; rebuild", tp.Table, database, tp.Target)
		}
		have, err := mergedRO.Columns(ctx, database, tp.Target)
		if err != nil {
			return fmt.Errorf("refusing to replay: listing the columns of merged table %s: %w", tp.Target, err)
		}
		for _, c := range tp.Columns {
			if !contains(have, c) {
				return fmt.Errorf("refusing to replay: the sources' table %s has column %s but merged table %s does not; the merged schema was fixed at build time; rebuild", tp.Table, c, tp.Target)
			}
		}
	}
	return nil
}

// sourceColumns returns a source table's columns, cached for the run.
func (r *Replayer) sourceColumns(ctx context.Context, src SourceFacts, table string) ([]string, error) {
	key := src.Database + "\x00" + table
	if cols, ok := r.cols[key]; ok {
		return cols, nil
	}
	cols, err := r.source.Columns(ctx, src.Database, table)
	if err != nil {
		return nil, err
	}
	r.cols[key] = cols
	return cols, nil
}

// sharedColumns is the column set a source can supply to a merged table: the
// merged table's columns that the source's table also has.
func (r *Replayer) sharedColumns(ctx context.Context, src SourceFacts, tp TablePlan) ([]string, error) {
	have, err := r.sourceColumns(ctx, src, tp.Table)
	if err != nil {
		return nil, fmt.Errorf("refusing to replay: listing the columns of %s.%s (store %s): %w", src.Database, tp.Table, src.Namespace, err)
	}
	var common []string
	for _, c := range tp.Columns {
		if contains(have, c) {
			common = append(common, c)
		}
	}
	if len(common) == 0 {
		return nil, fmt.Errorf("refusing to replay: store %s table %s shares no column with the merged table %s", src.Namespace, tp.Table, tp.Target)
	}
	return common, nil
}

// ---------------------------------------------------------------------------
// decide: what changed
// ---------------------------------------------------------------------------

// collectChanges reads every source's history since its recorded commit and
// settles which beads and which state tables the replay must reconcile.
func (r *Replayer) collectChanges(ctx context.Context, mergedRO *readOnlySource, database string, w *replayWork, imported map[string]int64) error {
	w.heads = map[string]string{}
	w.touched = map[string]map[string]bool{}
	w.full = map[string]bool{}
	w.stateChanged = map[string]map[string]bool{}
	w.changedTables = map[string]map[string]bool{}
	w.changedStores = map[string]bool{}

	for _, src := range r.plan.Sources {
		conn := w.conns[src.Database]
		base := w.bases[src.Namespace]

		// The next window's starting point is read BEFORE any diff: a commit a
		// source makes after this read is later than everything this replay
		// reads, so the next window overlaps this one and cannot miss it. An
		// overlap only re-applies a row, which is idempotent.
		head, err := conn.HeadCommit(ctx)
		if err != nil {
			return fmt.Errorf("refusing to replay: reading the dolt_log head of store %s (database %s): %w", src.Namespace, src.Database, err)
		}
		w.heads[src.Namespace] = head

		for _, tp := range w.plans {
			has, err := r.source.HasTable(ctx, src.Database, tp.Table)
			if err != nil {
				return fmt.Errorf("refusing to replay: checking store %s (database %s) for table %s: %w", src.Namespace, src.Database, tp.Table, err)
			}
			if !has {
				if n := imported[src.Namespace+"\x00"+tp.Table]; n > 0 {
					return fmt.Errorf("refusing to replay: the build imported %d row(s) of table %s from store %s, but that table no longer exists in database %s; what became of those rows is unknowable; rebuild", n, tp.Table, src.Namespace, src.Database)
				}
				continue
			}
			if tp.Scope != ScopeDatabaseState {
				cols, err := r.sourceColumns(ctx, src, tp.Table)
				if err != nil {
					return fmt.Errorf("refusing to replay: listing the columns of %s.%s (store %s): %w", src.Database, tp.Table, src.Namespace, err)
				}
				if !contains(cols, tp.ScopeColumn) {
					return fmt.Errorf("refusing to replay: store %s table %s has no %s column, so its rows cannot be attributed to a bead", src.Namespace, tp.Table, tp.ScopeColumn)
				}
			}

			// A table the source did not hold at the recorded commit has no
			// history to diff: either Dolt does not version it (dolt_ignore,
			// as for wisps) or it was created since. Both are reloaded whole,
			// which is correct for either and needs no history at all.
			versioned, err := conn.TableExistsAt(ctx, base.Hash, tp.Table)
			if err != nil {
				return fmt.Errorf("refusing to replay: checking whether store %s (database %s) held table %s at commit %s: %w", src.Namespace, src.Database, tp.Table, base.Hash, err)
			}
			switch tp.Scope {
			case ScopeDatabaseState:
				changed := !versioned
				if versioned {
					if changed, err = conn.DiffChanged(ctx, base.Hash, tp.Table); err != nil {
						return fmt.Errorf("refusing to replay: store %s table %s: %w", src.Namespace, tp.Table, err)
					}
				}
				if changed {
					if w.stateChanged[src.Namespace] == nil {
						w.stateChanged[src.Namespace] = map[string]bool{}
					}
					w.stateChanged[src.Namespace][tp.Table] = true
					w.changedStores[src.Namespace] = true
				}
			default:
				if !versioned {
					w.full[tp.Table] = true
					w.changedStores[src.Namespace] = true
					continue
				}
				ids, err := conn.DiffScopeValues(ctx, base.Hash, tp.Table, tp.ScopeColumn)
				if err != nil {
					return fmt.Errorf("refusing to replay: store %s table %s: %w", src.Namespace, tp.Table, err)
				}
				if len(ids) == 0 {
					continue
				}
				w.changedStores[src.Namespace] = true
				if w.changedTables[src.Namespace] == nil {
					w.changedTables[src.Namespace] = map[string]bool{}
				}
				w.changedTables[src.Namespace][tp.Table] = true
				if w.touched[tp.Table] == nil {
					w.touched[tp.Table] = map[string]bool{}
				}
				for _, id := range ids {
					w.touched[tp.Table][id] = true
				}
			}
		}
	}
	return nil
}

// recordedCollision is the part of a brain_unify_collisions row a replay
// compares with the sources.
type recordedCollision struct {
	Winner string
	Losers []string
}

func (r *Replayer) readCollisionRecords(ctx context.Context, mergedRO *readOnlySource, database string) (map[string]recordedCollision, error) {
	rows, err := mergedRO.query(ctx, fmt.Sprintf(
		"select `id`, `winner`, `losers` from `%s`.`brain_unify_collisions`", database))
	if err != nil {
		return nil, fmt.Errorf("reading brain_unify_collisions: %w", err)
	}
	defer rows.Close()
	out := map[string]recordedCollision{}
	for rows.Next() {
		var id, winner, losers string
		if err := rows.Scan(&id, &winner, &losers); err != nil {
			return nil, err
		}
		rec := recordedCollision{Winner: winner}
		if err := json.Unmarshal([]byte(losers), &rec.Losers); err != nil {
			return nil, fmt.Errorf("refusing to replay: the losers of recorded collision %s are unreadable (%q): %w", id, losers, err)
		}
		sort.Strings(rec.Losers)
		out[id] = rec
	}
	return out, rows.Err()
}

// reconcileCollisions compares the duplicated ids the sources hold now (the
// plan's collisions, decided by the build's own winner rule) with the ones the
// merged database recorded, and widens the set of beads to reconcile to every
// id whose decision changed. A winner that moved from one store to another
// needs the new winner's rows loaded and the old one's removed; reconciling
// the id in every bead-scoped table does exactly that.
func (r *Replayer) reconcileCollisions(w *replayWork, recorded map[string]recordedCollision) error {
	w.live = map[string]Collision{}
	w.losers = map[string]map[string]bool{}
	for _, c := range r.plan.Collisions {
		w.live[c.ID] = c
		w.losers[c.ID] = map[string]bool{}
		for _, l := range c.Losers {
			w.losers[c.ID][l] = true
		}
	}

	touchedAnywhere := unionSet(w.touched)
	widen := func(id string) {
		for _, tp := range w.plans {
			if tp.Scope == ScopeDatabaseState {
				continue
			}
			if w.touched[tp.Table] == nil {
				w.touched[tp.Table] = map[string]bool{}
			}
			w.touched[tp.Table][id] = true
		}
	}
	w.wide = map[string]bool{}
	noteStores := func(c Collision, rec *recordedCollision) {
		w.wide[c.Winner] = true
		for _, l := range c.Losers {
			w.wide[l] = true
		}
		if rec != nil {
			w.wide[rec.Winner] = true
			for _, l := range rec.Losers {
				w.wide[l] = true
			}
		}
	}

	var blocking []Collision
	for _, id := range sortedKeys(w.live) {
		c := w.live[id]
		rec, had := recorded[id]
		changed := !had || rec.Winner != c.Winner || !equalStrings(rec.Losers, c.Losers)
		if !changed && !touchedAnywhere[id] {
			continue
		}
		if c.DataColumnsDiffer {
			blocking = append(blocking, c)
		}
		w.collisionsToWrite = append(w.collisionsToWrite, id)
		if changed {
			widen(id)
			if had {
				noteStores(c, &rec)
			} else {
				noteStores(c, nil)
			}
		}
	}
	for _, id := range sortedKeys(recorded) {
		if _, still := w.live[id]; still {
			continue
		}
		rec := recorded[id]
		w.collisionsToDelete = append(w.collisionsToDelete, id)
		widen(id)
		w.wide[rec.Winner] = true
		for _, l := range rec.Losers {
			w.wide[l] = true
		}
	}

	if len(blocking) > 0 && !r.opts.AllowCollisions {
		c := blocking[0]
		return fmt.Errorf(
			"refusing to replay: %d duplicated id(s) in table issues have copies that disagree on content (first: %s in stores %s, winner %s, differing columns %s); keeping the winner's copy discards real state, which is a decision for a human; reconcile the copies, or re-run with --allow-collisions",
			len(blocking), c.ID, strings.Join(append([]string{c.Winner}, c.Losers...), ", "), c.Winner, strings.Join(c.DifferingColumns, ", "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// apply
// ---------------------------------------------------------------------------

// applyChanges makes the merged database's rows for every changed bead and
// every changed state table equal to what the sources hold now.
func (r *Replayer) applyChanges(ctx context.Context, tx *sql.Tx, mergedRO *readOnlySource, database string, w *replayWork, stats map[string]*ReplayTableStats) error {
	statFor := func(table string) *ReplayTableStats {
		s := stats[table]
		if s == nil {
			s = &ReplayTableStats{Table: table}
			stats[table] = s
		}
		return s
	}

	for _, tp := range w.plans {
		if tp.Scope == ScopeDatabaseState {
			continue
		}
		ids := sortedKeys(w.touched[tp.Table])
		full := w.full[tp.Table]
		if len(ids) == 0 && !full {
			continue
		}
		st := statFor(tp.Target)
		st.Full = full
		if err := r.reloadBeadTable(ctx, tx, w, tp, ids, full, st); err != nil {
			return err
		}
	}

	for _, store := range sortedKeys(w.stateChanged) {
		src, _ := r.plan.SourceByNamespace(store)
		for _, tp := range w.plans {
			if !w.stateChanged[store][tp.Table] {
				continue
			}
			if err := r.reloadStateTable(ctx, tx, mergedRO, database, w, src, tp, statFor(tp.Target)); err != nil {
				return err
			}
		}
	}
	return nil
}

// reloadBeadTable replaces the merged rows of the given beads (or of the whole
// table) with the rows the sources hold now, leaving out any copy that lost an
// id collision, exactly as the build's copy does.
func (r *Replayer) reloadBeadTable(ctx context.Context, tx *sql.Tx, w *replayWork, tp TablePlan, ids []string, full bool, st *ReplayTableStats) error {
	if full {
		res, err := tx.ExecContext(ctx, "delete from `"+tp.Target+"`")
		if err != nil {
			return fmt.Errorf("refusing to replay: clearing merged table %s: %w", tp.Target, err)
		}
		n, _ := res.RowsAffected()
		st.Deleted += n
	} else {
		for _, chunk := range chunks(ids, 200) {
			clause, args := inClause(tp.ScopeColumn, chunk)
			res, err := tx.ExecContext(ctx, "delete from `"+tp.Target+"` where "+clause, args...)
			if err != nil {
				return fmt.Errorf("refusing to replay: deleting the beads %s from merged table %s: %w", truncate(strings.Join(chunk, ", "), 200), tp.Target, err)
			}
			n, _ := res.RowsAffected()
			st.Deleted += n
		}
	}

	for _, src := range r.plan.Sources {
		has, err := r.source.HasTable(ctx, src.Database, tp.Table)
		if err != nil {
			return fmt.Errorf("refusing to replay: checking store %s for table %s: %w", src.Namespace, tp.Table, err)
		}
		if !has {
			continue
		}
		cols, err := r.sharedColumns(ctx, src, tp)
		if err != nil {
			return err
		}
		scopeIdx := indexOf(cols, tp.ScopeColumn)
		if scopeIdx < 0 {
			return fmt.Errorf("refusing to replay: store %s table %s has no %s column", src.Namespace, tp.Table, tp.ScopeColumn)
		}
		insert := func(rows [][]any) error {
			kept := rows[:0:0]
			for _, row := range rows {
				if w.losers[fmt.Sprint(row[scopeIdx])][src.Namespace] {
					st.Skipped++
					continue
				}
				kept = append(kept, row)
			}
			if err := insertRows(ctx, tx, tp.Target, cols, kept); err != nil {
				return fmt.Errorf("refusing to replay: store %s table %s into merged table %s: %w", src.Namespace, tp.Table, tp.Target, err)
			}
			st.Inserted += int64(len(kept))
			return nil
		}
		if full {
			jsonCols, err := r.source.JSONColumns(ctx, src.Database, tp.Table)
			if err != nil {
				return fmt.Errorf("refusing to replay: reading json columns of %s.%s: %w", src.Database, tp.Table, err)
			}
			if err := r.source.CopyRows(ctx, src.Database, tp.Table, cols, jsonCols, insert); err != nil {
				return fmt.Errorf("refusing to replay: reading store %s table %s: %w", src.Namespace, tp.Table, err)
			}
			continue
		}
		for _, chunk := range chunks(ids, 200) {
			clause, args := inClause(tp.ScopeColumn, chunk)
			rows, err := r.source.ReadRows(ctx, src.Database, tp.Table, cols, clause, args...)
			if err != nil {
				return fmt.Errorf("refusing to replay: reading store %s table %s: %w", src.Namespace, tp.Table, err)
			}
			if err := insert(rows); err != nil {
				return err
			}
		}
	}
	return nil
}

// reloadStateTable replaces one store's slice of a re-keyed database-state
// table, and the template store's slice of the merged database's own
// single-valued table as well. State tables are small configuration, so the
// whole slice is reloaded rather than diffed row by row.
func (r *Replayer) reloadStateTable(ctx context.Context, tx *sql.Tx, mergedRO *readOnlySource, database string, w *replayWork, src SourceFacts, tp TablePlan, st *ReplayTableStats) error {
	has, err := r.source.HasTable(ctx, src.Database, tp.Table)
	if err != nil {
		return fmt.Errorf("refusing to replay: checking store %s for table %s: %w", src.Namespace, tp.Table, err)
	}
	res, err := tx.ExecContext(ctx, "delete from `"+tp.Target+"` where `"+storeColumn+"` = ?", src.Namespace)
	if err != nil {
		return fmt.Errorf("refusing to replay: clearing store %s from merged table %s: %w", src.Namespace, tp.Target, err)
	}
	n, _ := res.RowsAffected()
	st.Deleted += n

	var rows [][]any
	var cols []string
	if has {
		if cols, err = r.sharedColumns(ctx, src, tp); err != nil {
			return err
		}
		if rows, err = r.source.ReadRows(ctx, src.Database, tp.Table, cols, ""); err != nil {
			return fmt.Errorf("refusing to replay: reading store %s table %s: %w", src.Namespace, tp.Table, err)
		}
	}
	if len(rows) > 0 {
		prefixed := append([]string{storeColumn}, cols...)
		stamped := make([][]any, 0, len(rows))
		for _, row := range rows {
			stamped = append(stamped, append([]any{src.Namespace}, row...))
		}
		if err := insertRows(ctx, tx, tp.Target, prefixed, stamped); err != nil {
			return fmt.Errorf("refusing to replay: store %s table %s into merged table %s: %w", src.Namespace, tp.Table, tp.Target, err)
		}
		st.Inserted += int64(len(rows))
	}

	if src.Namespace != w.template {
		return nil
	}
	plain, err := mergedRO.HasTable(ctx, database, tp.Table)
	if err != nil {
		return fmt.Errorf("refusing to replay: checking the merged database for table %s: %w", tp.Table, err)
	}
	if !plain {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "delete from `"+tp.Table+"`"); err != nil {
		return fmt.Errorf("refusing to replay: clearing merged table %s: %w", tp.Table, err)
	}
	if len(rows) > 0 {
		if err := insertRows(ctx, tx, tp.Table, cols, rows); err != nil {
			return fmt.Errorf("refusing to replay: template store %s table %s into merged table %s: %w", src.Namespace, tp.Table, tp.Table, err)
		}
	}
	return nil
}

// insertRows writes rows in size-bounded batches.
func insertRows(ctx context.Context, ex sqlExecer, table string, cols []string, rows [][]any) error {
	const (
		batchRows  = 100
		batchBytes = 4 << 20
	)
	stmt := insertPrefix(table, cols)
	var batch []string
	size := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		_, err := ex.ExecContext(ctx, stmt+strings.Join(batch, ","))
		if err != nil {
			err = fmt.Errorf("%w\nstatement: %s", err, truncate(stmt+batch[0], 1200))
		}
		batch, size = batch[:0], 0
		return err
	}
	for _, row := range rows {
		tuple := valueTuple(row)
		batch = append(batch, tuple)
		size += len(tuple)
		if len(batch) >= batchRows || size >= batchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// writeCollisionRecords brings brain_unify_collisions in line with the
// collisions the sources hold now.
func (r *Replayer) writeCollisionRecords(ctx context.Context, tx *sql.Tx, w *replayWork) error {
	for _, id := range w.collisionsToWrite {
		c := w.live[id]
		if err := writeCollisionRow(ctx, tx, c, readLosingRow(r.source, r.plan, c)); err != nil {
			return fmt.Errorf("refusing to replay: %w", err)
		}
	}
	for _, id := range w.collisionsToDelete {
		if _, err := tx.ExecContext(ctx, "delete from `brain_unify_collisions` where `id` = ?", id); err != nil {
			return fmt.Errorf("refusing to replay: removing the collision record for %s: %w", id, err)
		}
	}
	return nil
}

// refreshPrefixes rewrites the prefix ownership record from the sources as
// they stand: a bead with a new prefix, or a shifted bead count, changes it.
func (r *Replayer) refreshPrefixes(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, "delete from `brain_store_prefixes`"); err != nil {
		return fmt.Errorf("refusing to replay: clearing brain_store_prefixes: %w", err)
	}
	for _, ns := range SortedNamespaces(r.plan.Namespaces) {
		if err := writePrefixRow(ctx, tx, ns); err != nil {
			return fmt.Errorf("refusing to replay: %w", err)
		}
	}
	return nil
}

// rerecordFingerprints replaces the recorded source fingerprints that the
// replay made stale with readings of the sources as they stand now, taken the
// same way the build took them (same server-side expressions, same exclusion
// of collision losers). A store's tables are re-recorded when its history
// showed a change in them, when the sources keep no history for them, or for
// every table of a store whose collision decisions changed.
func (r *Replayer) rerecordFingerprints(ctx context.Context, tx *sql.Tx, w *replayWork) error {
	losing := losingIssueIDs(r.plan.Collisions)
	for _, src := range r.plan.Sources {
		var done []string
		for i := range w.plans {
			tp := &w.plans[i]
			if !w.fingerprintStale(src.Namespace, *tp) {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				"delete from `brain_unify_source_fingerprints` where `store` = ? and `table_name` = ?", src.Namespace, tp.Target); err != nil {
				return fmt.Errorf("refusing to replay: clearing the recorded fingerprints of %s/%s: %w", src.Namespace, tp.Target, err)
			}
			has, err := r.source.HasTable(ctx, src.Database, tp.Table)
			if err != nil {
				return fmt.Errorf("refusing to replay: checking store %s for table %s: %w", src.Namespace, tp.Table, err)
			}
			if !has {
				continue
			}
			done = append(done, tp.Table)
			if tp.Scope == ScopeDatabaseState {
				fp, err := r.source.ReferenceDigest(ctx, src.Database, tp, src.Namespace)
				if err != nil {
					return fmt.Errorf("refusing to replay: digest of %s.%s (store %s): %w", src.Database, tp.Table, src.Namespace, err)
				}
				if fp.Rows > 0 {
					if err := writeSourceFingerprint(ctx, tx, GroupFingerprint{Store: src.Namespace, Table: tp.Target, Group: src.Namespace, Fingerprint: fp}); err != nil {
						return err
					}
				}
				continue
			}
			byGroup, err := r.source.FingerprintByGroup(ctx, src.Database, tp.Table, tp.ScopeColumnName(), "", losingIDsFor(losing, src.Namespace))
			if err != nil {
				return fmt.Errorf("refusing to replay: fingerprint of %s.%s (store %s): %w", src.Database, tp.Table, src.Namespace, err)
			}
			for _, g := range sortedKeys(byGroup) {
				if byGroup[g].Rows == 0 {
					continue
				}
				if err := writeSourceFingerprint(ctx, tx, GroupFingerprint{Store: src.Namespace, Table: tp.Target, Group: g, Fingerprint: byGroup[g]}); err != nil {
					return err
				}
			}
		}
		if len(done) > 0 {
			r.log("re-recorded the source fingerprints of store %s: %s", src.Namespace, strings.Join(done, ", "))
		}
	}
	return nil
}

// fingerprintStale reports whether a store's recorded fingerprint for a table
// no longer describes the source.
func (w *replayWork) fingerprintStale(store string, tp TablePlan) bool {
	if w.wide[store] {
		return true
	}
	if tp.Scope == ScopeDatabaseState {
		return w.stateChanged[store][tp.Table]
	}
	return w.full[tp.Table] || w.changedTables[store][tp.Table]
}

// advanceCommits records every store's new starting point for the next
// replay. The head was read before any diff, so what a source commits from
// here on is inside the next window.
func (r *Replayer) advanceCommits(ctx context.Context, tx *sql.Tx, w *replayWork) ([]recordedSourceCommit, error) {
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	var out []recordedSourceCommit
	for _, src := range r.plan.Sources {
		head := w.heads[src.Namespace]
		if head == w.bases[src.Namespace].Hash {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			"replace into `brain_unify_source_commits` (`store`,`source_database`,`commit_hash`,`recorded_at`) values "+
				valueTuple([]any{src.Namespace, src.Database, head, now})); err != nil {
			return nil, fmt.Errorf("refusing to replay: recording the new starting commit of store %s: %w", src.Namespace, err)
		}
		out = append(out, recordedSourceCommit{Store: src.Namespace, Database: src.Database, Hash: head})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// TableExistsAt reports whether the database held the table at the commit.
func (s *readOnlySource) TableExistsAt(ctx context.Context, commit, table string) (bool, error) {
	stmt := fmt.Sprintf("select 1 from `%s` as of %s limit 1", table, quoteLiteral(commit))
	rows, err := s.query(ctx, stmt)
	if err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1146 {
			return false, nil
		}
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
	}
	return true, rows.Err()
}

func inClause(col string, values []string) (string, []any) {
	marks := make([]string, len(values))
	args := make([]any, len(values))
	for i, v := range values {
		marks[i] = "?"
		args[i] = v
	}
	return "`" + col + "` in (" + strings.Join(marks, ",") + ")", args
}

func chunks(values []string, n int) [][]string {
	var out [][]string
	for len(values) > n {
		out = append(out, values[:n])
		values = values[n:]
	}
	if len(values) > 0 {
		out = append(out, values)
	}
	return out
}

func unionSet(m map[string]map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, ids := range m {
		for id := range ids {
			out[id] = true
		}
	}
	return out
}

func unionIDs(m map[string]map[string]bool) []string { return sortedKeys(unionSet(m)) }

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
