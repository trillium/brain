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
	// AllowCollisions permits the build to proceed when the plan found
	// divergent id collisions. Without it a divergent collision stops the
	// build, because the winner rule then discards a real state difference
	// and that is a decision for a human.
	AllowCollisions bool
	// Logf receives progress lines.
	Logf func(format string, args ...any)
}

// storeColumn names the column that carries the source store's identity in
// every re-keyed database-state table.
const storeColumn = "store"

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

	d := Discovery{Copies: map[string][]IDCopy{}}
	idOwners := map[string][]IDCopy{}

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

		if !facts.Registered && len(prefixes) > 1 {
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
		if v, err := source.MetadataValue(ctx, dbName, "_project_id"); err == nil {
			facts.ProjectID = v
		}
		if !facts.Registered {
			d.Unregistered = append(d.Unregistered, dbName)
		}
		for _, r := range ids {
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

	sort.Strings(d.Replicas)
	sort.Strings(d.Unregistered)
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

func unreadable(f SourceFacts, reason string) SourceFacts {
	f.Reachable = false
	f.SkipReason = reason
	return f
}

// Plan builds the deterministic mapping from the discovery.
func (d Discovery) Plan() Plan {
	p := BuildPlan(d.Facts, d.Copies)
	sort.Slice(p.Excluded, func(i, j int) bool { return p.Excluded[i].Database < p.Excluded[j].Database })
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
	if reason, blocked := b.plan.Blocks(); blocked && !b.opts.AllowCollisions {
		return BuildResult{}, fmt.Errorf(
			"refusing to build: %s (re-run with --allow-collisions after reviewing the plan)", reason)
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
	b.jsonCols = map[string][]string{}
	for _, tp := range b.plans {
		cols, err := b.source.JSONColumns(ctx, b.templateDatabase, tp.Table)
		if err != nil {
			return BuildResult{}, fmt.Errorf("reading json columns of %s: %w", tp.Table, err)
		}
		b.jsonCols[tp.Table] = cols
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
	if err := b.writeProvenance(ctx, target); err != nil {
		return res, err
	}
	if err := b.recordSourceFingerprints(ctx, target); err != nil {
		return res, err
	}
	if err := WriteImportLog(ctx, target, res.Stats); err != nil {
		return res, err
	}
	if _, err := target.ExecContext(ctx, "select dolt_commit('-Am', 'brain unify: import federation into one database')"); err != nil {
		// A commit failure is reported, not swallowed: the rows are already
		// in the database, but an uncommitted build is not a durable result.
		b.log("warning: could not create a dolt commit: %v", err)
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
	// Rows belonging to an issue copy that lost an id collision must not be
	// imported either, or the unified database would carry relationships to
	// an id whose winning row came from somewhere else. The excluded set is
	// derived from the collision decision, not guessed.
	losingIDs := losingIssueIDs(b.plan.Collisions)

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
			stats, err := b.copyTable(ctx, target, src, table, losingIDs)
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

// losingIssueIDs maps a duplicated id to the source whose copy did not become
// a row of its own.
func losingIssueIDs(collisions []Collision) map[string]string {
	out := map[string]string{}
	for _, c := range collisions {
		for _, l := range c.Losers {
			out[c.ID] = l
		}
	}
	return out
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
func (b *Builder) copyTable(ctx context.Context, target *sql.DB, src SourceFacts, table string, losingIDs map[string]string) (ImportStats, error) {
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

	switch tp.Scope {
	case ScopeIssues:
		cols, err := b.sharedColumns(ctx, src, tp)
		if err != nil {
			return stats, err
		}
		imported, skipped, err := b.copyIssues(ctx, target, src, tp.Target, cols, losingIDs)
		if err != nil {
			return stats, err
		}
		stats.ImportedRows, stats.SkippedRows = imported, skipped
		if skipped > 0 {
			stats.Note = fmt.Sprintf("%d row(s) lost to an id collision; every one is recorded in brain_unify_collisions", skipped)
		}
	case ScopeIssueChild:
		imported, skipped, err := b.copyChildRows(ctx, target, src, tp, losingIDs)
		if err != nil {
			return stats, err
		}
		stats.ImportedRows, stats.SkippedRows = imported, skipped
		if skipped > 0 {
			stats.Note = fmt.Sprintf("%d row(s) belonged to an issue copy that lost an id collision", skipped)
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

// copyIssues imports a source's beads, skipping copies that lost an id
// collision.
func (b *Builder) copyIssues(ctx context.Context, target *sql.DB, src SourceFacts, table string, cols []string, losingIDs map[string]string) (imported, skipped int64, err error) {
	if !contains(cols, "id") {
		return 0, 0, fmt.Errorf("issues copy lost its id column")
	}
	stmt := insertPrefix(table, cols)
	imported, skipped, err = b.copyFiltered(ctx, target, src.Database, table, cols, stmt,
		indexOf(cols, "id"), losingIDsFor(losingIDs, src.Namespace))
	if err != nil {
		return imported, skipped, err
	}
	if skipped > 0 {
		b.log("  %-24s %d bead(s) skipped as collision losers", src.Namespace, skipped)
	}
	return imported, skipped, nil
}

// copyChildRows imports an issue-scoped table, dropping rows whose issue copy
// lost a collision.
func (b *Builder) copyChildRows(ctx context.Context, target *sql.DB, src SourceFacts, tp *TablePlan, losingIDs map[string]string) (imported, skipped int64, err error) {
	if !contains(tp.Columns, tp.ScopeColumn) {
		return 0, 0, fmt.Errorf("table %s has no %s column to scope by", tp.Table, tp.ScopeColumn)
	}
	stmt := insertPrefix(tp.Target, tp.Columns)
	return b.copyFiltered(ctx, target, src.Database, tp.Table, tp.Columns, stmt,
		indexOf(tp.Columns, tp.ScopeColumn), losingIDsFor(losingIDs, src.Namespace))
}

func losingIDsFor(losingIDs map[string]string, source string) []string {
	var out []string
	for id, loser := range losingIDs {
		if loser == source {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
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

// copyFiltered copies a table, dropping rows whose value in keyIdx is one of
// the excluded keys. Exclusion happens in Go rather than SQL so one code path
// serves every table shape.
func (b *Builder) copyFiltered(ctx context.Context, target *sql.DB, database, table string, cols []string, stmt string, keyIdx int, exclude []string) (imported, skipped int64, err error) {
	if keyIdx < 0 || keyIdx >= len(cols) {
		return 0, 0, fmt.Errorf("key column index %d out of range for %s", keyIdx, table)
	}
	excluded := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excluded[e] = true
	}
	var importedCopy int64
	err = b.source.CopyRows(ctx, database, table, cols, b.jsonColsFor(table), func(rows [][]any) error {
		batch := make([]string, 0, len(rows))
		for _, r := range rows {
			key := fmt.Sprint(r[keyIdx])
			if excluded[key] {
				skipped++
				continue
			}
			batch = append(batch, valueTuple(r))
		}
		if len(batch) == 0 {
			return nil
		}
		if _, err := target.ExecContext(ctx, stmt+strings.Join(batch, ",")); err != nil {
			return fmt.Errorf("inserting into %s from %s: %w\nstatement: %s", table, database, err, truncate(stmt+batch[0], 1200))
		}
		importedCopy += int64(len(batch))
		return nil
	})
	return importedCopy, skipped, err
}

// truncate shortens a statement for an error message so a failure reports the
// offending text rather than a megabyte of INSERT.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + " ...[truncated]"
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
		if _, err := target.ExecContext(ctx,
			"replace into `brain_store_prefixes` (`prefix`,`store`,`owner_reason`,`declared_by`,`observed_by`,`bead_count`,`ambiguous`) values "+
				valueTuple([]any{ns.Prefix, ns.Owner, ns.OwnerReason, strings.Join(ns.DeclaredBy, ","), strings.Join(ns.ObservedBy, ","), ns.BeadCount, boolToInt(ns.Ambiguous)})); err != nil {
			return fmt.Errorf("recording prefix %s: %w", ns.Prefix, err)
		}
	}
	for _, c := range b.plan.Collisions {
		losers, _ := json.Marshal(c.Losers)
		loserHashes, _ := json.Marshal(c.LoserHashes)
		losingRow, _ := json.Marshal(b.losingRow(c))
		if _, err := target.ExecContext(ctx,
			"replace into `brain_unify_collisions` (`id`,`prefix`,`owner`,`winner`,`losers`,`reason`,`divergent`,`winner_hash`,`loser_hashes`,`losing_row`) values "+
				valueTuple([]any{c.ID, c.Prefix, c.Owner, c.Winner, string(losers), c.Reason, boolToInt(c.Divergent), c.WinnerHash, string(loserHashes), string(losingRow)})); err != nil {
			return fmt.Errorf("recording collision %s: %w", c.ID, err)
		}
	}
	return nil
}

// losingRow reads the full row of a collision's losing copy, so nothing is lost
// to the winner rule. A read failure is recorded in the row itself rather than
// hidden: an unreadable losing row is exactly the case an operator needs to
// see.
func (b *Builder) losingRow(c Collision) map[string]any {
	if len(c.Losers) == 0 {
		return map[string]any{}
	}
	out := map[string]any{"note": "losing copy row was not captured"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	src, ok := b.plan.SourceByNamespace(c.Losers[0])
	if !ok {
		return out
	}
	cols, err := b.source.Columns(ctx, src.Database, "issues")
	if err != nil {
		out["note"] = err.Error()
		return out
	}
	quoted := make([]string, 0, len(cols))
	for _, col := range cols {
		quoted = append(quoted, "`"+col+"`")
	}
	stmt := fmt.Sprintf("select %s from `%s`.`issues` where `id` = ?", strings.Join(quoted, ", "), src.Database)
	rows, err := b.source.query(ctx, stmt, c.ID)
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

// recordSourceFingerprints stores what the build read from every source, per
// table, per namespace.
//
// This is the reference the verification compares against. Brain's stores are
// written continuously — the lifespan ledger alone takes hundreds of rows an
// hour — so re-reading production after a build measures how far the
// federation moved, not whether the migration was correct. Recording the read
// makes the comparison a snapshot against a snapshot, and makes it
// reproducible: anyone can re-run verification against a finished build and
// get the same answer.
func (b *Builder) recordSourceFingerprints(ctx context.Context, target *sql.DB) error {
	losingIDs := losingIssueIDs(b.plan.Collisions)
	recorded := 0
	for _, tp := range b.plans {
		for _, src := range b.plan.Sources {
			has, err := b.source.HasTable(ctx, src.Database, tp.Table)
			if err != nil || !has {
				continue
			}
			if tp.Scope == ScopeDatabaseState {
				fp, err := b.source.Fingerprint(ctx, src.Database, tp.Table, "")
				if err != nil {
					return err
				}
				if fp.Rows == 0 {
					continue
				}
				if err := writeSourceFingerprint(ctx, target, GroupFingerprint{
					Store: src.Namespace, Table: tp.Table, Group: src.Namespace, Fingerprint: fp,
				}); err != nil {
					return err
				}
				recorded++
				continue
			}
			byGroup, err := b.source.FingerprintByGroup(ctx, src.Database, tp.Table,
				tp.ScopeColumnName(), "", losingIDsFor(losingIDs, src.Namespace))
			if err != nil {
				return err
			}
			for group, fp := range byGroup {
				if fp.Rows == 0 {
					continue
				}
				if err := writeSourceFingerprint(ctx, target, GroupFingerprint{
					Store: src.Namespace, Table: tp.Table, Group: group, Fingerprint: fp,
				}); err != nil {
					return err
				}
				recorded++
			}
		}
	}
	b.log("recorded %d source fingerprint(s) for verification", recorded)
	return nil
}

// writeSourceFingerprint records one measured source group.
func writeSourceFingerprint(ctx context.Context, target *sql.DB, gf GroupFingerprint) error {
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
