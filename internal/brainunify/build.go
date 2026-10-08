package brainunify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BuildOptions configures a unification build.
type BuildOptions struct {
	// DataDir is the scratch directory the isolated Dolt server runs in. It
	// becomes the home of the unified database, and it must not be a
	// production data directory.
	DataDir string
	// Database is the unified database's name inside the isolated server.
	Database string
	// DoltBin is the dolt binary used to start the isolated server.
	DoltBin string
	// Aliases maps a wrapper name (BD_NAME) to the store it addresses, for the
	// wrappers whose name is not the store's. It is written to
	// brain_store_aliases.
	Aliases map[string]string
	// Host and Port locate the Dolt server holding the sources. These are
	// needed for the per-database history read that records every source's
	// commit at the moment the build sees it, which is what a later replay
	// takes as its starting point.
	Host string
	Port int
	// Logf receives progress lines.
	Logf func(format string, args ...any)
}

// storeColumn names the column that carries the source store's identity in
// every re-keyed database-state table.
const storeColumn = "store"

// brainunifyCopy is what one source contributed to one table: a digest taken
// server-side with the same expression the verifier will run on the unified
// table, and the namespace or store it belongs to.
type brainunifyCopy struct {
	Fingerprint
	Source string
	Group  string
}

// ImportStats records what happened to one source table.
type ImportStats struct {
	Store string
	Table string
	// SourceRows is what the source table held.
	SourceRows int64
	// ImportedRows is what became rows in the unified database.
	ImportedRows int64
	// SkippedRows is what did not, because a colliding id already existed.
	SkippedRows int64
	// Note explains a non-zero SkippedRows.
	Note string
}

// BuildResult is the outcome of a build.
type BuildResult struct {
	// Database is the unified database's name.
	Database string
	// DataDir is where the unified database now lives.
	DataDir string
	// Stats is one entry per (store, table).
	Stats []ImportStats
	// Collisions is the collision set that was applied.
	Collisions []Collision
	// Namespaces is the prefix ownership map that was written.
	Namespaces []Namespace
	// Template is the source whose schema the unified database inherited.
	Template string
	// Elapsed is the wall-clock duration of the build.
	Elapsed time.Duration
	// ServerLogPath is where the isolated server's log was written.
	ServerLogPath string
}

// GroupFingerprint is what the build read from one source, for one table, in
// one namespace.
type GroupFingerprint struct {
	Store string
	Table string
	Group string
	Fingerprint
}

// SkippedFor returns the total number of rows skipped for a store.
func (r BuildResult) SkippedFor(store string) int64 {
	var n int64
	for _, s := range r.Stats {
		if s.Store == store {
			n += s.SkippedRows
		}
	}
	return n
}

// Builder constructs the unified database from read-only sources.
type Builder struct {
	source           *readOnlySource
	plan             Plan
	opts             BuildOptions
	log              func(format string, args ...any)
	template         SourceFacts
	plans            []TablePlan
	templateDatabase string
	// jsonCols maps a source table to its json-typed columns, resolved once
	// from the template. Resolving it per source instead would cost two
	// information_schema queries per (store, table) — thousands of them
	// against a live server — and the unified schema is the template's, so
	// the template's answer is the correct one. A source that genuinely
	// differs still fails loudly on insert rather than silently.
	jsonCols map[string][]string
	// recorded accumulates the copy-time digests, keyed by table and group.
	recorded map[string]GroupFingerprint
	// mapper decides what each source row becomes: skipped, moved to a
	// conflict's copy, or copied as it is.
	mapper *rowMapper
}

// NewBuilder returns a builder reading from source and migrating according to
// plan.
func NewBuilder(source *readOnlySource, plan Plan, opts BuildOptions) *Builder {
	log := opts.Logf
	if log == nil {
		log = func(string, ...any) {}
	}
	return &Builder{source: source, plan: plan, opts: opts, log: log}
}

// Discovery is the full analysis of a federation: which databases
// participate, which namespaces they own, and which ids disagree.
type Discovery struct {
	// Facts is the per-source analysis.
	Facts []SourceFacts
	// Copies holds every id found in more than one source.
	Copies map[string][]IDCopy
	// Replicas lists databases excluded as cross-store indexes.
	Replicas []string
	// Unregistered lists participating databases claimed by no store.
	Unregistered []string
	// Rescued lists replica databases from which the beads no store holds were
	// brought in (see Registry.RescueOrphansFrom).
	Rescued []string
	// IDs holds every bead id of every participating source, so an id minted
	// for a conflict's copy can be checked against the ids that exist.
	IDs map[string]bool
}

// Discover inspects the server and the registry.
//
// The inclusion rule is mechanical rather than curated:
//
//   - a database claimed by a registered store always participates;
//   - a database claimed by no store participates when it contains a single
//     namespace, because that is a store the registry has not caught up with;
//   - a database claimed by no store holding several namespaces is excluded as
//     a cross-store replica. beads_global is exactly this: it holds 1206
//     brain- ids and 164 agent- ids, all copies of rows that live in their
//     own stores, so importing it would duplicate the whole federation.
//
// Every exclusion is recorded with its bead count, so nothing disappears
// silently.
func Discover(ctx context.Context, source *readOnlySource, reg Registry) (Discovery, error) {
	dbs, err := source.Databases(ctx)
	if err != nil {
		return Discovery{}, fmt.Errorf("listing databases on the source server: %w", err)
	}

	claimedBy := map[string][]string{}
	for _, s := range reg.Stores {
		claimedBy[s.Database] = append(claimedBy[s.Database], s.Namespace)
	}

	d := Discovery{Copies: map[string][]IDCopy{}, IDs: map[string]bool{}}
	idOwners := map[string][]IDCopy{}
	var rescue []string

	for _, dbName := range dbs {
		facts := SourceFacts{Database: dbName, Fingerprints: map[string]Fingerprint{}}
		owners := append([]string(nil), claimedBy[dbName]...)
		sort.Strings(owners)
		for range owners {
			facts.Registered = true
		}
		if len(owners) > 0 {
			facts.Namespace = owners[0]
			if st, ok := storeByNamespace(reg, owners[0]); ok {
				facts.ProjectID = st.ProjectID
				facts.DeclaredPrefixes = st.DeclaredPrefixes
			}
		} else {
			facts.Namespace = "db:" + dbName
		}

		has, err := source.HasTable(ctx, dbName, "issues")
		if err != nil {
			d.Facts = append(d.Facts, unreadable(facts, fmt.Sprintf("cannot inspect: %v", err)))
			continue
		}
		if !has {
			// Not a beads database at all (TinyKeyboard, pins, backup).
			d.Facts = append(d.Facts, facts)
			continue
		}

		prefixes, err := source.IssuePrefixes(ctx, dbName)
		if err != nil {
			d.Facts = append(d.Facts, unreadable(facts, fmt.Sprintf("cannot read ids: %v", err)))
			continue
		}
		var beads int64
		for _, n := range prefixes {
			beads += n
		}

		if !facts.Registered && len(prefixes) > 1 && !contains(reg.IncludeDatabases, dbName) {
			if contains(reg.RescueOrphansFrom, dbName) {
				rescue = append(rescue, dbName)
			}
			d.Replicas = append(d.Replicas, dbName)
			facts.Reachable = false
			facts.SkipReason = ExcludeReplica
			facts.BeadCount = beads
			facts.Prefixes = prefixes
			d.Facts = append(d.Facts, facts)
			continue
		}

		tables, err := source.BaseTables(ctx, dbName)
		if err != nil {
			d.Facts = append(d.Facts, unreadable(facts, fmt.Sprintf("cannot list tables: %v", err)))
			continue
		}
		ids, err := source.IssueIdentities(ctx, dbName)
		if err != nil {
			d.Facts = append(d.Facts, unreadable(facts, fmt.Sprintf("cannot read issue identities: %v", err)))
			continue
		}

		facts.Reachable = true
		facts.BeadCount = beads
		facts.Prefixes = prefixes
		facts.Tables = tables
		// The database's own identity wins when it has one. A database that does
		// not (job, lifespan and others carry no _project_id row) keeps the
		// identity its store's metadata.json gave it; overwriting that with the
		// empty answer is what left nine stores unidentified in the first build.
		if v, err := source.MetadataValue(ctx, dbName, "_project_id"); err == nil && v != "" {
			facts.ProjectID = v
		}
		if !facts.Registered {
			d.Unregistered = append(d.Unregistered, dbName)
		}
		for _, r := range ids {
			d.IDs[r.ID] = true
			hash := r.ContentHash
			if hash == "" {
				hash = shortDigest(r.ID, r.CreatedAt, r.UpdatedAt)
			}
			idOwners[r.ID] = append(idOwners[r.ID], IDCopy{
				Source:      facts.Namespace,
				UpdatedAt:   r.UpdatedAt,
				CreatedAt:   r.CreatedAt,
				ContentHash: hash,
			})
		}
		d.Facts = append(d.Facts, facts)
	}

	for _, dbName := range rescue {
		f, err := rescueOrphans(ctx, source, dbName, d.IDs)
		if err != nil {
			return Discovery{}, err
		}
		for i := range d.Facts {
			if d.Facts[i].Database == dbName {
				d.Facts[i] = f
			}
		}
		d.Unregistered = append(d.Unregistered, dbName)
		d.Rescued = append(d.Rescued, dbName)
	}

	sort.Strings(d.Replicas)
	sort.Strings(d.Unregistered)
	sort.Strings(d.Rescued)
	for id, group := range idOwners {
		if len(group) < 2 {
			continue
		}
		// Duplicated ids are rare, so every copy is read in full. Comparing
		// the rows column by column is what distinguishes "these two copies
		// were touched at different times" from "these two copies disagree",
		// and only the second kind should stop a migration.
		databaseOf := map[string]string{}
		for _, f := range d.Facts {
			if f.Reachable {
				databaseOf[f.Namespace] = f.Database
			}
		}
		for i := range group {
			db, ok := databaseOf[group[i].Source]
			if !ok {
				continue
			}
			row, err := source.IssueRow(ctx, db, id)
			if err != nil {
				continue
			}
			group[i].Row = row
		}
		d.Copies[id] = group
	}
	sort.Slice(d.Facts, func(i, j int) bool { return d.Facts[i].Namespace < d.Facts[j].Namespace })
	return d, nil
}

// rescueOrphans makes a replica database a participating source that carries
// only the beads no other participating source holds. Every other bead of the
// replica is a copy of a bead that lives in its own store; those are listed in
// SkipIDs so the build, the fingerprints, the replay and the verifier all leave
// them alone, through the same exclusion that skips an identical duplicate's
// second copy.
func rescueOrphans(ctx context.Context, source *readOnlySource, dbName string, held map[string]bool) (SourceFacts, error) {
	ids, err := source.IssueIdentities(ctx, dbName)
	if err != nil {
		return SourceFacts{}, fmt.Errorf("reading the beads of replica %s: %w", dbName, err)
	}
	tables, err := source.BaseTables(ctx, dbName)
	if err != nil {
		return SourceFacts{}, fmt.Errorf("listing the tables of replica %s: %w", dbName, err)
	}
	f := SourceFacts{
		Namespace: "db:" + dbName, Database: dbName, Reachable: true, Tables: tables,
		Prefixes: map[string]int64{}, Fingerprints: map[string]Fingerprint{},
	}
	orphan := map[string]bool{}
	for _, r := range ids {
		if held[r.ID] {
			f.SkipIDs = append(f.SkipIDs, r.ID)
			continue
		}
		orphan[r.ID] = true
		f.BeadCount++
		f.Prefixes[PrefixOf(r.ID)]++
		held[r.ID] = true
	}
	// A replica also carries child rows (isa_sections, labels, ...) of beads
	// whose own row is not in it; those belong to the stores that hold the bead,
	// so every id a bead-scoped table of the replica names, other than the
	// orphans', is skipped.
	skip := map[string]bool{}
	for _, id := range f.SkipIDs {
		skip[id] = true
	}
	for _, table := range tables {
		cols, err := source.Columns(ctx, dbName, table)
		if err != nil {
			return SourceFacts{}, fmt.Errorf("listing the columns of replica %s.%s: %w", dbName, table, err)
		}
		for _, col := range []string{"issue_id", "parent_id"} {
			if !contains(cols, col) || strings.HasPrefix(table, NamespacedPrefix) || contains(brainTables, table) {
				continue
			}
			rows, err := source.query(ctx, fmt.Sprintf("select distinct `%s` from `%s`.`%s`", col, dbName, table))
			if err != nil {
				return SourceFacts{}, fmt.Errorf("reading the beads replica %s.%s names: %w", dbName, table, err)
			}
			for rows.Next() {
				var raw any
				if err := rows.Scan(&raw); err != nil {
					_ = rows.Close()
					return SourceFacts{}, err
				}
				if id := cellText(raw); id != "" && !orphan[id] && !skip[id] {
					skip[id] = true
					f.SkipIDs = append(f.SkipIDs, id)
				}
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return SourceFacts{}, err
			}
		}
	}
	sort.Strings(f.SkipIDs)
	if v, err := source.MetadataValue(ctx, dbName, "_project_id"); err == nil {
		f.ProjectID = v
	}
	return f, nil
}

func unreadable(f SourceFacts, reason string) SourceFacts {
	f.Reachable = false
	f.SkipReason = reason
	return f
}

// Plan builds the deterministic mapping from the discovery.
// ApplyProjectIDs supplies the project identity of stores that neither their
// database nor the registry can: ids maps a store's namespace (or its source
// database name) to its project id. An id already known is not replaced.
func (d *Discovery) ApplyProjectIDs(ids map[string]string) {
	for i := range d.Facts {
		f := &d.Facts[i]
		if !f.Reachable || f.ProjectID != "" {
			continue
		}
		if id := ids[f.Namespace]; id != "" {
			f.ProjectID = id
		} else if id := ids[f.Database]; id != "" {
			f.ProjectID = id
		}
	}
}

// Unidentified lists the participating stores that have no project id: the
// cutover repoints each store's wrapper by it, so a store without one would be
// skipped silently.
func (d Discovery) Unidentified() []string {
	var out []string
	for _, f := range d.Facts {
		if f.Reachable && f.ProjectID == "" {
			out = append(out, f.Namespace+" (database "+f.Database+")")
		}
	}
	sort.Strings(out)
	return out
}

// Plan builds the deterministic mapping from the discovery.
func (d Discovery) Plan() Plan {
	p := BuildPlan(d.Facts, d.Copies)
	sort.Slice(p.Excluded, func(i, j int) bool { return p.Excluded[i].Database < p.Excluded[j].Database })
	p.Refusals = append(p.Refusals, mintedClashes(p.Conflicts(), d.IDs)...)
	return p
}

func storeByNamespace(reg Registry, ns string) (Store, bool) {
	for _, s := range reg.Stores {
		if s.Namespace == ns {
			return s, true
		}
	}
	return Store{}, false
}

// shortDigest is a stable, non-cryptographic digest used only to detect that
// two copies of an id differ. FNV-1a over the joined fields.
func shortDigest(parts ...string) string {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i, p := range parts {
		if i > 0 {
			h ^= 0x1f
			h *= prime64
		}
		for j := 0; j < len(p); j++ {
			h ^= uint64(p[j])
			h *= prime64
		}
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 16)
	for i := 15; i >= 0; i-- {
		out[i] = hexdigits[h&0xf]
		h >>= 4
	}
	return string(out)
}

// Build constructs the unified database and returns what it did.
func (b *Builder) Build(ctx context.Context) (BuildResult, error) {
	start := time.Now()
	if err := b.plan.Refusal(); err != nil {
		return BuildResult{}, fmt.Errorf("refusing to build: %w", err)
	}
	for _, alias := range sortedKeys(b.opts.Aliases) {
		if _, ok := b.plan.SourceByNamespace(b.opts.Aliases[alias]); !ok {
			return BuildResult{}, fmt.Errorf("refusing to build: alias %q names store %q, which is not a participating source", alias, b.opts.Aliases[alias])
		}
	}
	if len(b.plan.Sources) == 0 {
		return BuildResult{}, fmt.Errorf("nothing to migrate: no source database was reachable")
	}
	if b.opts.DataDir == "" {
		return BuildResult{}, fmt.Errorf("build requires a data directory: the unified database must never be written into a production dolt data directory")
	}

	template, err := b.templateSource()
	if err != nil {
		return BuildResult{}, err
	}
	b.template = template
	b.templateDatabase = template.Database

	database := b.opts.Database
	if database == "" {
		database = "brain_unified"
	}
	b.plans, err = b.tablePlans(ctx)
	if err != nil {
		return BuildResult{}, err
	}
	b.mapper = newRowMapper(b.plan)
	if len(b.plan.Conflicts()) > 0 {
		if err := validateRekeying(b.plans); err != nil {
			return BuildResult{}, fmt.Errorf("refusing to build: %w", err)
		}
		if err := validateConflictTables(b.plans); err != nil {
			return BuildResult{}, fmt.Errorf("refusing to build: %w", err)
		}
	}
	b.jsonCols = map[string][]string{}
	for _, tp := range b.plans {
		cols, err := b.source.JSONColumns(ctx, b.templateDatabase, tp.Table)
		if err != nil {
			return BuildResult{}, fmt.Errorf("reading json columns of %s: %w", tp.Table, err)
		}
		b.jsonCols[tp.Table] = cols
	}

	// Record each source's committing point before anything is copied, so
	// the window a later replay reads is a super-set of "everything the
	// build did not measure". See readSourceHeads.
	heads, err := b.readSourceHeads(ctx)
	if err != nil {
		return BuildResult{}, err
	}

	if un := b.plan.Unidentified(); len(un) > 0 {
		b.log("warning: store(s) %s have no project id from their database, the registry or --project-id; they are recorded with an empty identity and a cutover cannot repoint a wrapper to them by it", strings.Join(un, ", "))
	}
	srv, err := StartIsolatedServer(ctx, b.opts.DoltBin, b.opts.DataDir)
	if err != nil {
		return BuildResult{}, err
	}
	defer func() { _ = srv.Stop() }()
	b.log("isolated dolt server on 127.0.0.1:%d, data dir %s", srv.Port, b.opts.DataDir)

	admin, err := srv.OpenTarget(ctx, "")
	if err != nil {
		return BuildResult{}, err
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.ExecContext(ctx, "create database `"+database+"`"); err != nil {
		return BuildResult{}, fmt.Errorf("creating unified database %s: %w", database, err)
	}

	target, err := srv.OpenTarget(ctx, database)
	if err != nil {
		return BuildResult{}, err
	}
	defer func() { _ = target.Close() }()

	res := BuildResult{
		Database:      database,
		DataDir:       b.opts.DataDir,
		Collisions:    b.plan.Collisions,
		Namespaces:    SortedNamespaces(b.plan.Namespaces),
		Template:      template.Namespace,
		ServerLogPath: filepath.Join(b.opts.DataDir, "unified-server.log"),
	}

	if err := b.createSchema(ctx, target); err != nil {
		return res, err
	}
	if err := b.copySources(ctx, target, &res); err != nil {
		return res, err
	}
	if err := b.writeConflictBeads(ctx, target, &res); err != nil {
		return res, err
	}
	if err := b.writeProvenance(ctx, target); err != nil {
		return res, err
	}
	if err := b.writeRecordedFingerprints(ctx, target); err != nil {
		return res, err
	}
	if err := WriteImportLog(ctx, target, res.Stats); err != nil {
		return res, err
	}
	if err := b.writeSourceCommits(ctx, target, heads); err != nil {
		return res, err
	}
	b.log("recorded %d source commit(s): every later change a source makes is inside a replay's window", len(heads))
	// dolt 2.x exposes dolt_commit as a stored procedure ("call"), not a
	// scalar function: the build's previous "select dolt_commit(...)" failed
	// with "function: 'dolt_commit' not found" on every run, leaving the
	// unified database with zero dolt history — a crash between build and
	// first write would lose the whole working set (cutover probe, rough
	// edge 4). The call returns a result set; drain it, keep the error.
	{
		rows, err := target.QueryContext(ctx, "call dolt_commit('-Am', 'brain unify: import federation into one database')")
		if err != nil {
			// A commit failure is reported, not swallowed: the rows are
			// already in the database, but an uncommitted build is not a
			// durable result.
			b.log("warning: could not create a dolt commit: %v", err)
		} else {
			for rows.Next() {
			}
			_ = rows.Err()
			_ = rows.Close()
			b.log("created initial dolt commit of the unified database")
		}
	}
	res.Elapsed = time.Since(start)
	return res, nil
}

// templateSource picks the database whose schema the unified database
// inherits. Explicit choice wins; otherwise the source holding the most beads
// wins, with the namespace name as a deterministic tiebreak.
func (b *Builder) templateSource() (SourceFacts, error) {
	for _, s := range b.plan.Sources {
		if s.Namespace == b.plan.TemplateNamespace {
			return s, nil
		}
	}
	var best SourceFacts
	for _, s := range b.plan.Sources {
		if s.BeadCount > best.BeadCount || (s.BeadCount == best.BeadCount && s.Namespace < best.Namespace) {
			best = s
		}
	}
	if best.Namespace == "" {
		return SourceFacts{}, fmt.Errorf("no source available to supply the unified schema")
	}
	return best, nil
}

// tablePlans classifies the template's tables once. Every source is loaded
// through the same plan, so a source with an unusual table set cannot silently
// change the unified schema.
func (b *Builder) tablePlans(ctx context.Context) ([]TablePlan, error) {
	if b.plans != nil {
		return b.plans, nil
	}
	plans, err := buildTablePlans(ctx, b.source, b.template)
	if err != nil {
		return nil, err
	}
	b.plans = plans
	return plans, nil
}

// TablePlansFor recomputes the table plans a build would use, so a verifier
// reads the unified database through the same classification the builder wrote
// it with. An empty template uses the same default the builder uses: the
// source holding the most beads.
func TablePlansFor(ctx context.Context, source *ReadOnlySource, plan Plan, template string) ([]TablePlan, error) {
	var chosen SourceFacts
	if template != "" {
		s, ok := plan.SourceByNamespace(template)
		if !ok {
			return nil, fmt.Errorf("template %q is not a participating source", template)
		}
		chosen = s
	} else {
		for _, s := range plan.Sources {
			if s.BeadCount > chosen.BeadCount || (s.BeadCount == chosen.BeadCount && s.Namespace < chosen.Namespace) {
				chosen = s
			}
		}
	}
	if chosen.Namespace == "" {
		return nil, fmt.Errorf("no source available to supply the unified schema")
	}
	return buildTablePlans(ctx, source, chosen)
}

// buildTablePlans classifies a template source's tables.
func buildTablePlans(ctx context.Context, source *ReadOnlySource, template SourceFacts) ([]TablePlan, error) {
	db := template.Database
	metaOf := func(t string) ([]string, []string, error) {
		cols, err := source.Columns(ctx, db, t)
		if err != nil {
			return nil, nil, err
		}
		pk, err := source.PrimaryKeys(ctx, db, t)
		if err != nil {
			return nil, nil, err
		}
		return cols, pk, nil
	}
	return PlanTables(template.Tables, metaOf)
}

// createSchema gives the unified database the template source's own schema
// rather than a hand-written copy, then adds the re-keyed database-state
// tables and the provenance tables.
func (b *Builder) createSchema(ctx context.Context, target *sql.DB) error {
	if _, err := target.ExecContext(ctx, "set foreign_key_checks = 0"); err != nil {
		return fmt.Errorf("disabling foreign key checks on the unified database: %w", err)
	}
	for _, tp := range b.plans {
		src, err := b.source.CreateTableDDL(ctx, b.templateDatabase, tp.Table)
		if err != nil {
			return err
		}
		if tp.Scope != ScopeDatabaseState {
			if _, err := target.ExecContext(ctx, src); err != nil {
				return fmt.Errorf("creating unified table %s from %s: %w", tp.Target, b.templateDatabase, err)
			}
			b.log("created %-34s scope=%s", tp.Target, tp.Scope)
			continue
		}
		// A database-state table lands in two places. The re-keyed copy
		// holds every store's rows under its own key space, which is where
		// the migration's record lives. The original, single-valued table
		// exists so bd can open the unified database at all: it is seeded
		// from the template store, because a key like issue_prefix can only
		// have one value in a database-wide key space.
		if _, err := target.ExecContext(ctx, src); err != nil {
			return fmt.Errorf("creating unified table %s from %s: %w", tp.Table, b.templateDatabase, err)
		}
		ddl, err := NamespacedDDL(tp.Target, src, storeColumn, tp.PrimaryKey)
		if err != nil {
			return err
		}
		if _, err := target.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("creating unified table %s from %s: %w", tp.Target, b.templateDatabase, err)
		}
		b.log("created %-34s scope=%s (+ %s seeded from the template store)", tp.Target, tp.Scope, tp.Table)
	}
	for _, name := range brainTables {
		if _, err := target.ExecContext(ctx, BrainTableDDL[name]); err != nil {
			return fmt.Errorf("creating provenance table %s: %w", name, err)
		}
	}
	return nil
}

// copySources loads every source into the unified database.
func (b *Builder) copySources(ctx context.Context, target *sql.DB, res *BuildResult) error {
	order := LoadOrder(tableNames(b.plans), func(t string) []string {
		refs, err := b.source.ForeignKeys(ctx, b.templateDatabase, t)
		if err != nil {
			return nil
		}
		return refs
	})

	for _, src := range b.plan.Sources {
		b.log("importing %-24s from database %-18s beads=%d", src.Namespace, src.Database, src.BeadCount)
		// The template store goes first so the unified database's own
		// single-valued config/metadata tables exist and bd can open the
		// unified database before anything else lands.
		if src.Namespace == b.template.Namespace {
			if err := b.copyStoreState(ctx, target, src); err != nil {
				return err
			}
		}
		for _, table := range order {
			stats, err := b.copyTable(ctx, target, src, table)
			if err != nil {
				return err
			}
			if stats.SourceRows > 0 || stats.ImportedRows > 0 || stats.SkippedRows > 0 {
				res.Stats = append(res.Stats, stats)
			}
		}
	}
	return nil
}

// copyStoreState gives the template store's config/metadata rows a home in the
// unified database's own single-valued tables, so bd can open the unified
// database. Every store's equivalent rows also live in the re-keyed
// brain_unified_* tables, which is where the full per-store record is.
func (b *Builder) copyStoreState(ctx context.Context, target *sql.DB, src SourceFacts) error {
	for _, tp := range b.plans {
		if tp.Scope != ScopeDatabaseState {
			continue
		}
		has, err := b.source.HasTable(ctx, src.Database, tp.Table)
		if err != nil || !has {
			continue
		}
		count, err := b.copyRaw(ctx, target, src.Database, tp.Table, tp.Columns, tp.Table, nil)
		if err != nil {
			return fmt.Errorf("seeding unified %s from %s: %w", tp.Table, src.Database, err)
		}
		b.log("  seeded unified %-26s with %d row(s) from %s", tp.Table, count, src.Namespace)
	}
	return nil
}

// copyTable imports one table of one source.
func (b *Builder) copyTable(ctx context.Context, target *sql.DB, src SourceFacts, table string) (ImportStats, error) { //nolint:gocyclo // one linear pipeline: digest, then copy
	stats := ImportStats{Store: src.Namespace, Table: table}
	tp := b.planFor(table)
	if tp == nil {
		return stats, nil
	}
	has, err := b.source.HasTable(ctx, src.Database, table)
	if err != nil {
		return stats, err
	}
	if !has {
		return stats, nil
	}
	total, err := b.source.CountRows(ctx, src.Database, table)
	if err != nil {
		return stats, err
	}
	stats.SourceRows = total

	// Digest the source with the SAME server-side expression the verifier
	// will use on the unified table, immediately before copying.
	//
	// This replaced a client-side digest of the copied rows. The client-side
	// version was exact but not comparable: reproducing the server's rendering
	// of every column type (notably datetimes) is a second implementation of
	// the same rules, and it drifted from the server by a few bytes per row,
	// which is indistinguishable from real corruption. One implementation, used
	// on both sides, is worth more than exactness against a second one.
	//
	// The cost is that the digest and the copy are two reads rather than
	// one, so a row written in the gap between them would show up as a
	// mismatch. That window is milliseconds per table rather than the minutes
	// a post-copy re-read allowed.
	if tp.Scope == ScopeDatabaseState {
		// Digested over the unified table's column set, store column included,
		// because that is the row the verifier will compare against. See
		// ReferenceDigest.
		fp, err := b.source.ReferenceDigest(ctx, src.Database, tp, src.Namespace)
		if err != nil {
			return stats, err
		}
		if fp.Rows > 0 {
			copied := brainunifyCopy{Fingerprint: fp, Source: src.Namespace, Group: src.Namespace}
			b.record(copied, tp.Target, src.Namespace)
		}
	} else {
		byGroup, err := b.source.FingerprintByGroup(ctx, src.Database, tp.Table,
			tp.ScopeColumnName(), "", b.plan.sourceExclusions(src.Namespace))
		if err != nil {
			return stats, err
		}
		for group, fp := range byGroup {
			if fp.Rows == 0 {
				continue
			}
			b.record(brainunifyCopy{Fingerprint: fp, Source: src.Namespace, Group: group}, tp.Target, src.Namespace)
		}
	}

	switch tp.Scope {
	case ScopeIssues:
		cols, err := b.sharedColumns(ctx, src, tp)
		if err != nil {
			return stats, err
		}
		imported, skipped, err := b.copyMapped(ctx, target, src, tp, cols)
		if err != nil {
			return stats, err
		}
		stats.ImportedRows, stats.SkippedRows = imported, skipped
		if skipped > 0 {
			stats.Note = fmt.Sprintf("%d row(s) were the skipped copy of an identical duplicate; every one is recorded in brain_unify_collisions", skipped)
		}
	case ScopeIssueChild:
		imported, skipped, err := b.copyMapped(ctx, target, src, tp, tp.Columns)
		if err != nil {
			return stats, err
		}
		stats.ImportedRows, stats.SkippedRows = imported, skipped
		if skipped > 0 {
			stats.Note = fmt.Sprintf("%d row(s) belonged to the skipped copy of an identical duplicate", skipped)
		}
	case ScopeDatabaseState:
		imported, err := b.copyRaw(ctx, target, src.Database, table, tp.Columns, tp.Target, []any{src.Namespace})
		if err != nil {
			return stats, fmt.Errorf("importing %s.%s: %w", src.Database, table, err)
		}
		stats.ImportedRows = imported
	}
	return stats, nil
}

func (b *Builder) planFor(table string) *TablePlan {
	for i := range b.plans {
		if b.plans[i].Table == table {
			return &b.plans[i]
		}
	}
	return nil
}

// jsonColsFor returns the json-typed columns resolved from the template.
func (b *Builder) jsonColsFor(table string) []string { return b.jsonCols[table] }

// sharedColumns returns the columns both the template schema and this source
// have, so a source that lags the template is reported by column count rather
// than failing mid-insert with an opaque SQL error.
func (b *Builder) sharedColumns(ctx context.Context, src SourceFacts, tp *TablePlan) ([]string, error) {
	have, err := b.source.Columns(ctx, src.Database, tp.Table)
	if err != nil {
		return nil, err
	}
	var common []string
	for _, c := range tp.Columns {
		if contains(have, c) {
			common = append(common, c)
		}
	}
	if len(common) != len(tp.Columns) {
		b.log("  note: %s.%s has %d of the template's %d columns; the remainder take their defaults",
			src.Database, tp.Table, len(common), len(tp.Columns))
	}
	if len(common) == 0 {
		return nil, fmt.Errorf("no columns in common between unified %s and %s.%s", tp.Table, src.Database, tp.Table)
	}
	return common, nil
}

// copyMapped imports a bead-scoped table of one source through the row mapper:
// the rows of an identical duplicate's skipped copy are dropped, the rows of a
// conflicted id this source holds a copy of land under that copy's minted id
// (with a new key where the table needs one), and every other row is copied as
// it is. The replay maps through the same function.
func (b *Builder) copyMapped(ctx context.Context, target *sql.DB, src SourceFacts, tp *TablePlan, cols []string) (imported, skipped int64, err error) {
	if !contains(cols, tp.ScopeColumn) {
		return 0, 0, fmt.Errorf("table %s has no %s column to scope by", tp.Table, tp.ScopeColumn)
	}
	stmt := insertPrefix(tp.Target, cols)
	err = b.source.CopyRows(ctx, src.Database, tp.Table, cols, b.jsonColsFor(tp.Table), func(rows [][]any) error {
		kept, sk, err := b.mapper.mapRows(*tp, src.Namespace, cols, rows)
		if err != nil {
			return fmt.Errorf("mapping %s.%s of store %s: %w", src.Database, tp.Table, src.Namespace, err)
		}
		skipped += sk
		if len(kept) == 0 {
			return nil
		}
		batch := make([]string, 0, len(kept))
		for _, r := range kept {
			batch = append(batch, valueTuple(r))
		}
		if _, err := target.ExecContext(ctx, stmt+strings.Join(batch, ",")); err != nil {
			return fmt.Errorf("inserting into %s from %s: %w\nstatement: %s", tp.Target, src.Database, err, truncate(stmt+batch[0], 1200))
		}
		imported += int64(len(batch))
		return nil
	})
	if err == nil && skipped > 0 && tp.Scope == ScopeIssues {
		b.log("  %-24s %d bead(s) skipped as identical duplicates", src.Namespace, skipped)
	}
	return imported, skipped, err
}

// copyRaw copies a table verbatim, optionally stamping each row with the
// store it came from.
func (b *Builder) copyRaw(ctx context.Context, target *sql.DB, database, table string, cols []string, targetTable string, prefixValues []any) (int64, error) {
	full := make([]string, 0, len(cols)+len(prefixValues))
	for range prefixValues {
		// insertPrefix adds the backticks; passing an already-quoted name
		// here would produce ``store`` and fail to parse.
		full = append(full, storeColumn)
	}
	for _, c := range cols {
		full = append(full, c)
	}
	stmt := insertPrefix(targetTable, full)

	var imported int64
	err := b.source.CopyRows(ctx, database, table, cols, b.jsonColsFor(table), func(rows [][]any) error {
		batch := make([]string, 0, len(rows))
		for _, r := range rows {
			vals := make([]any, 0, len(r)+len(prefixValues))
			vals = append(vals, prefixValues...)
			vals = append(vals, r...)
			batch = append(batch, valueTuple(vals))
		}
		if _, err := target.ExecContext(ctx, stmt+strings.Join(batch, ",")); err != nil {
			return fmt.Errorf("inserting into %s from %s.%s: %w\nstatement: %s",
				targetTable, database, table, err, truncate(stmt+batch[0], 1200))
		}
		imported += int64(len(rows))
		return nil
	})
	return imported, err
}

// namespaceFor maps a Dolt database to the unified namespace it migrates as.
func (b *Builder) namespaceFor(database string) string {
	for _, s := range b.plan.Sources {
		if s.Database == database {
			return s.Namespace
		}
	}
	return "db:" + database
}

// insertPrefix builds "insert into `table` (`c1`,`c2`) values". The row tuples
// are appended by the caller as rendered literals, so the prefix deliberately
// carries no placeholders: a stray '?' here reads as an unbound parameter and
// the statement fails to parse.
func insertPrefix(table string, columns []string) string {
	parts := make([]string, 0, len(columns))
	for _, c := range columns {
		parts = append(parts, "`"+c+"`")
	}
	return fmt.Sprintf("insert into `%s` (%s) values ", table, strings.Join(parts, ","))
}

// valueTuple renders one VALUES tuple, keeping NULL as NULL and quoting
// everything else. Rows are batched into one statement, so values are escaped
// here rather than bound one parameter at a time.
func valueTuple(values []any) string {
	var b strings.Builder
	b.WriteByte('(')
	for i, v := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		if v == nil {
			b.WriteString("NULL")
			continue
		}
		switch t := v.(type) {
		case string:
			b.WriteString(quoteLiteral(t))
		case []byte:
			b.WriteString(quoteLiteral(string(t)))
		case bool:
			if t {
				b.WriteString("1")
			} else {
				b.WriteString("0")
			}
		case int, int8, int16, int32, int64,
			uint, uint8, uint16, uint32, uint64, float32, float64:
			b.WriteString(fmt.Sprint(t))
		default:
			b.WriteString(quoteLiteral(fmt.Sprint(t)))
		}
	}
	b.WriteByte(')')
	return b.String()
}

// quoteLiteral escapes a string for a single-quoted SQL literal. Backslash
// escapes are included because the connection may be established with
// NO_BACKSLASH_ESCAPES disabled, which is Dolt's default.
func quoteLiteral(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\'':
			b.WriteString("''")
		case '\\':
			b.WriteString("\\\\")
		case 0:
			b.WriteString("\\0")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case 0x1a:
			b.WriteString("\\Z")
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// tableNames extracts the source table names from a plan set.
func tableNames(plans []TablePlan) []string {
	out := make([]string, 0, len(plans))
	for _, p := range plans {
		out = append(out, p.Table)
	}
	return out
}

func indexOf(cols []string, name string) int {
	for i, c := range cols {
		if c == name {
			return i
		}
	}
	return -1
}

// writeProvenance records the namespace mapping and the collision decisions in
// the unified database, so the mapping stays queryable after the source
// databases are gone.
func (b *Builder) writeProvenance(ctx context.Context, target *sql.DB) error {
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	for _, s := range b.plan.Sources {
		if _, err := target.ExecContext(ctx,
			"replace into `brain_stores` (`store`,`registered`,`source_database`,`beads_dir`,`project_id`,`schema_version`,`template_source`,`imported_at`) values "+
				valueTuple([]any{s.Namespace, boolToInt(s.Registered), s.Database, "", s.ProjectID, s.SchemaVersion, boolToInt(s.Namespace == b.template.Namespace), now})); err != nil {
			return fmt.Errorf("recording store %s: %w", s.Namespace, err)
		}
	}
	for _, ns := range SortedNamespaces(b.plan.Namespaces) {
		if err := writePrefixRow(ctx, target, ns); err != nil {
			return err
		}
	}
	for _, alias := range sortedKeys(b.opts.Aliases) {
		if _, err := target.ExecContext(ctx,
			"replace into `brain_store_aliases` (`alias`,`store`) values "+valueTuple([]any{alias, b.opts.Aliases[alias]})); err != nil {
			return fmt.Errorf("recording alias %s: %w", alias, err)
		}
	}
	for _, c := range b.plan.Collisions {
		rec, err := buildCollisionRecord(ctx, b.source, b.plan, b.plans, b.mapper, c)
		if err != nil {
			return err
		}
		if err := writeCollisionRow(ctx, target, rec); err != nil {
			return err
		}
	}
	return nil
}

// writeConflictBeads inserts the rows the tool itself authors for every
// conflict: the conflict bead, its label, and one dependency from it to each
// copy. The copies' own rows were already written by the copy, under their
// minted ids.
func (b *Builder) writeConflictBeads(ctx context.Context, target *sql.DB, res *BuildResult) error {
	conflicts := b.plan.Conflicts()
	if len(conflicts) == 0 {
		return nil
	}
	for _, tp := range b.plans {
		cols, rows := tableConflictRows(tp, conflicts)
		if len(rows) == 0 {
			continue
		}
		if err := insertRows(ctx, target, tp.Target, cols, rows); err != nil {
			return fmt.Errorf("writing the conflict beads' rows into %s: %w", tp.Target, err)
		}
		res.Stats = append(res.Stats, ImportStats{
			Store: conflictStore, Table: tp.Target, ImportedRows: int64(len(rows)),
			Note: "rows the tool authored for conflict beads; they come from no source",
		})
	}
	b.log("wrote %d conflict bead(s) linking to %d minted cop(ies)", len(conflicts), b.plan.MintedCopies())
	return nil
}

// writePrefixRow records one prefix's ownership.
func writePrefixRow(ctx context.Context, target sqlExecer, ns Namespace) error {
	if _, err := target.ExecContext(ctx,
		"replace into `brain_store_prefixes` (`prefix`,`store`,`owner_reason`,`declared_by`,`observed_by`,`bead_count`,`ambiguous`) values "+
			valueTuple([]any{ns.Prefix, ns.Owner, ns.OwnerReason, strings.Join(ns.DeclaredBy, ","), strings.Join(ns.ObservedBy, ","), ns.BeadCount, boolToInt(ns.Ambiguous)})); err != nil {
		return fmt.Errorf("recording prefix %s: %w", ns.Prefix, err)
	}
	return nil
}

// collisionRecord is one brain_unify_collisions row.
type collisionRecord struct {
	Collision Collision
	// LosingRow is the full row of an identical duplicate's skipped copy. A
	// conflict skips nothing, so its LosingRow is empty.
	LosingRow map[string]any
	// CopyHashes maps each copy of a conflict to a digest of the copy as it
	// should be in the merged database: its row and all its child rows,
	// under the minted id. Empty for identical copies.
	CopyHashes map[string]string
}

// buildCollisionRecord assembles the record of one collision from the sources.
// The build and a replay both write through here, so a record means the same
// thing whichever of them wrote it.
func buildCollisionRecord(ctx context.Context, source *readOnlySource, plan Plan, plans []TablePlan, mapper *rowMapper, c Collision) (collisionRecord, error) {
	rec := collisionRecord{Collision: c, LosingRow: map[string]any{}, CopyHashes: map[string]string{}}
	if !c.Divergent {
		rec.LosingRow = readLosingRow(source, plan, c)
		return rec, nil
	}
	for _, cp := range c.Copies {
		src, ok := plan.SourceByNamespace(cp.Store)
		if !ok {
			return rec, fmt.Errorf("the copy of %s held by store %s has no participating source", c.ID, cp.Store)
		}
		read, err := copyRowsFromSource(ctx, source, plans, mapper, src, c.ID)
		if err != nil {
			return rec, err
		}
		rec.CopyHashes[cp.Store] = digestCopy(read.Rows)
	}
	return rec, nil
}

// writeCollisionRow records one collision decision. A conflict records the
// minted id and the digest of each copy; an identical duplicate records the full
// row of the copy that was skipped.
func writeCollisionRow(ctx context.Context, target sqlExecer, rec collisionRecord) error {
	c := rec.Collision
	losers, _ := json.Marshal(nonNilStrings(c.Losers))
	loserHashes, _ := json.Marshal(nonNilMap(c.LoserHashes))
	row, _ := json.Marshal(rec.LosingRow)
	copyIDs, _ := json.Marshal(nonNilMap(c.copyIDs()))
	copyHashes, _ := json.Marshal(nonNilMap(rec.CopyHashes))
	if _, err := target.ExecContext(ctx,
		"replace into `brain_unify_collisions` (`id`,`prefix`,`owner`,`winner`,`losers`,`reason`,`divergent`,`winner_hash`,`loser_hashes`,`losing_row`,`resolution`,`copy_ids`,`copy_hashes`) values "+
			valueTuple([]any{c.ID, c.Prefix, c.Owner, c.Winner, string(losers), c.Reason, boolToInt(c.Divergent), c.WinnerHash, string(loserHashes), string(row), c.Resolution, string(copyIDs), string(copyHashes)})); err != nil {
		return fmt.Errorf("recording collision %s: %w", c.ID, err)
	}
	return nil
}

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// readLosingRow reads the full row of a collision's first losing copy. The
// build records it at copy time and a replay records it again when a
// duplicated id appears or changes, so both write the same shape.
func readLosingRow(source *readOnlySource, plan Plan, c Collision) map[string]any {
	if len(c.Losers) == 0 {
		return map[string]any{}
	}
	out := map[string]any{"note": "losing copy row was not captured"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	src, ok := plan.SourceByNamespace(c.Losers[0])
	if !ok {
		return out
	}
	cols, err := source.Columns(ctx, src.Database, "issues")
	if err != nil {
		out["note"] = err.Error()
		return out
	}
	quoted := make([]string, 0, len(cols))
	for _, col := range cols {
		quoted = append(quoted, "`"+col+"`")
	}
	stmt := fmt.Sprintf("select %s from `%s`.`issues` where `id` = ?", strings.Join(quoted, ", "), src.Database)
	rows, err := source.query(ctx, stmt, c.ID)
	if err != nil {
		out["note"] = err.Error()
		return out
	}
	defer rows.Close()
	if !rows.Next() {
		return out
	}
	values := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		out["note"] = err.Error()
		return out
	}
	row := make(map[string]any, len(cols))
	for i, col := range cols {
		switch v := values[i].(type) {
		case []byte:
			row[col] = string(v)
		default:
			row[col] = v
		}
	}
	row["_captured_from_store"] = src.Namespace
	row["_captured_from_database"] = src.Database
	return row
}

// recordedSourceCommit is one source's commit as the build read it. A
// replay diffs each source from this commit to the working set to find the
// rows the build never saw.
type recordedSourceCommit struct {
	Store    string
	Database string
	Hash     string
}

// readSourceHeads records, per source, the newest Dolt commit at the moment
// the build is about to read it. Reading happens BEFORE the copy: any commit
// a source makes after this read is later than every row the build measured,
// so a replay's window (recorded commit -> working set) covers it with room
// to spare. Fragment reads that happen after the head read can only overlap
// the replay window, never escape it — a replay applies an overlapping row
// a second time (an idempotent replace), so overlap is safe and underlap is
// what would strand live changes.
//
// A source whose history head cannot be READ is a loud refusal, not a
// build without a starting point: a merged database that looks replayable
// and later refuses to replay is worse than one that refuses to build.
func (b *Builder) readSourceHeads(ctx context.Context) ([]recordedSourceCommit, error) {
	host := b.opts.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := b.opts.Port
	if port == 0 {
		port = 3307
	}
	// A source database backing TWO stores makes a replay's holder
	// resolution ambiguous (two stores answer for one row set, and one
	// store's delete would reach another store's copy). Refuse rather than
	// build a merged database a replay could not attribute.
	byDatabase := map[string][]string{}
	for _, s := range b.plan.Sources {
		byDatabase[s.Database] = append(byDatabase[s.Database], s.Namespace)
	}
	for db, stores := range byDatabase {
		if len(stores) > 1 {
			return nil, fmt.Errorf("refusing to build: stores %s share one source database %s; a replay could not attribute its rows", strings.Join(stores, ", "), db)
		}
	}
	out := make([]recordedSourceCommit, 0, len(b.plan.Sources))
	for _, s := range b.plan.Sources {
		conn, err := OpenReadOnlyDatabase(ctx, host, port, s.Database)
		if err != nil {
			return nil, fmt.Errorf("refusing to build: opening the history of store %s (database %s) to record the replay starting point: %w", s.Namespace, s.Database, err)
		}
		hash, err := conn.HeadCommit(ctx)
		_ = conn.Close()
		if err != nil {
			return nil, fmt.Errorf("refusing to build: reading the dolt_log head of store %s (database %s): %w", s.Namespace, s.Database, err)
		}
		out = append(out, recordedSourceCommit{Store: s.Namespace, Database: s.Database, Hash: hash})
	}
	return out, nil
}

// writeSourceCommits stores the recorded heads in the unified database, so
// a later replay knows where each source's diff starts.
func (b *Builder) writeSourceCommits(ctx context.Context, target *sql.DB, heads []recordedSourceCommit) error {
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	for _, h := range heads {
		if _, err := target.ExecContext(ctx,
			"replace into `brain_unify_source_commits` (`store`,`source_database`,`commit_hash`,`recorded_at`) values "+
				valueTuple([]any{h.Store, h.Database, h.Hash, now})); err != nil {
			return fmt.Errorf("recording the source commit for %s: %w", h.Store, err)
		}
	}
	return nil
}

// record accumulates what one copy pass wrote, keyed by the unified table and
// the namespace (or store) the rows belong to.
//
// The digest is taken from the rows that were actually written rather than from
// a second read of the source. Brain's stores are written continuously, so a
// fingerprint taken after the copy measures a later snapshot: in one run the
// task store alone gained rows between the copy and the measurement, and every
// one of them surfaced as a migration defect that never happened.
func (b *Builder) record(copied brainunifyCopy, target, source string) {
	if b.recorded == nil {
		b.recorded = map[string]GroupFingerprint{}
	}
	if copied.Group == "" {
		return
	}
	// One row per (store, table, group), as a replay re-records them. The build
	// used to fold every store's share of a group into one row labelled with the
	// last store; a replay then deleted that row for the last store alone and
	// re-recorded only its own share, and the verifier's recorded reference no
	// longer matched after any replay that touched a group two stores share.
	key := target + "\x00" + source + "\x00" + copied.Group
	gf := b.recorded[key]
	gf.Store = source
	gf.Table = target
	gf.Group = copied.Group
	gf.Fingerprint = Combine(gf.Fingerprint, copied.Fingerprint)
	b.recorded[key] = gf
}

// writeRecordedFingerprints stores the accumulated copy-time digests so the
// verification compares the unified database against exactly what the build
// read and wrote.
func (b *Builder) writeRecordedFingerprints(ctx context.Context, target *sql.DB) error {
	for _, gf := range b.recorded {
		if err := writeSourceFingerprint(ctx, target, gf); err != nil {
			return err
		}
	}
	b.log("recorded %d source fingerprint(s) from the copy itself", len(b.recorded))
	return nil
}

// sqlExecer is the write surface shared by a connection and a transaction, so
// the build and a replay record fingerprints through one function.
type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// writeSourceFingerprint records one measured source group.
func writeSourceFingerprint(ctx context.Context, target sqlExecer, gf GroupFingerprint) error {
	_, err := target.ExecContext(ctx,
		"replace into `brain_unify_source_fingerprints` (`store`,`table_name`,`group_name`,`row_count`,`byte_count`,`hash_value`) values "+
			valueTuple([]any{gf.Store, gf.Table, gf.Group, gf.Rows, gf.Bytes, gf.Hash}))
	if err != nil {
		return fmt.Errorf("recording source fingerprint for %s.%s/%s: %w", gf.Store, gf.Table, gf.Group, err)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// WriteImportLog records the per-store, per-table accounting so the numbers a
// verifier re-checks are stored next to the data rather than only in a report
// file.
func WriteImportLog(ctx context.Context, target *sql.DB, stats []ImportStats) error {
	for _, s := range stats {
		if _, err := target.ExecContext(ctx,
			"replace into `brain_unify_import_log` (`store`,`table_name`,`source_rows`,`imported_rows`,`skipped_rows`,`verified`,`note`) values "+
				valueTuple([]any{s.Store, s.Table, s.SourceRows, s.ImportedRows, s.SkippedRows, 0, s.Note})); err != nil {
			return fmt.Errorf("recording import log for %s.%s: %w", s.Store, s.Table, err)
		}
	}
	return nil
}

// truncate shortens a statement for an error message so a failure reports the
// offending text rather than a megabyte of INSERT.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + " ...[truncated]"
}
