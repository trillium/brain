// Package transfer implements the `brain transfer <id> <dest>` verb.
//
// `brain transfer` is the brain v0.3 verb for atomic cross-store moves on
// the shared Dolt SQL server. All 10 named PAI stores live as separate
// databases on the same Dolt instance (default 127.0.0.1:3307), so a
// transfer between any two stores is a single MySQL transaction that
// touches multiple databases via `<db>.<table>` SQL refs — no
// multi-binary handoff, no eventually-consistent sync, no separate
// commit per side.
//
// # Spec mapping (TARGET_REPO: /Users/trilliumsmith/code/brain)
//
//   - ISC-46  verb implemented                                       — Verb + Run
//   - ISC-47  inbox→brain works                                      — Run
//   - ISC-48  inbox→task works                                       — Run
//   - ISC-49  any source store works                                 — Registry resolution
//   - ISC-50  dest title defaults to source title                    — readSource → insertDest
//   - ISC-51  dest description defaults to source body               — readSource → insertDest
//   - ISC-52  source closed with dest id in reason                   — closeSource
//   - ISC-53  supersede link recorded                                — recordSupersede
//   - ISC-54  bad source prefix exits non-zero                       — Registry.ResolveSource
//   - ISC-55  bad dest name exits non-zero                           — Registry.ResolveDest
//   - ISC-56  resolves by DB name on 3307, not BEADS_DIR             — all SQL uses `<db>`.table refs
//   - ISC-56.1 atomic — single MySQL transaction                     — Run takes one *sql.Conn, one BEGIN/COMMIT
//
// # Why a *sql.Conn rather than the *sql.DB pool
//
// The transaction has to walk multiple databases (`USE source_db`,
// `USE dest_db`, …). On Go's *sql.DB the next call after BEGIN can pick
// up a different pooled connection, dropping any session-level state.
// Pinning one connection via DB.Conn(ctx) keeps every statement inside
// the same MySQL session for the lifetime of the TX. Combined with
// fully-qualified `<db>.<table>` SQL refs (which work without a USE),
// the verb's writes are guaranteed to land on one server-side session,
// one transaction, one commit.
//
// # Why supersede edges land in the SOURCE store, not the hub
//
// The task spec sketch suggests writing the supersede edge into the
// brain hub database's `dependencies` table. The actual schema has a
// foreign key fk_dep_issue from `dependencies.issue_id` → `issues.id`
// scoped to the same database (migrations 0042 / 0043). A
// cross-database edge in the hub would require the source ID to exist
// in `dolt.issues` — it does not — and the INSERT would fail FK
// validation, aborting the whole transfer.
//
// The verb instead writes the supersede row to the *source* store's
// `dependencies` table, where `issue_id` = source-id satisfies the FK
// (the source row still exists, only now closed) and
// `depends_on_external` carries the cross-store destination id (no FK
// on that column, exactly because it is designed for cross-bucket /
// cross-store targets). The edge is still discoverable from either
// side: `bd dep list <source-id>` in the source store finds it, and a
// reverse search on `depends_on_external = <dest-id>` across stores
// finds it from the destination side.
//
// # Shape
//
// The package exports the standard four pieces:
//
//   - Args     — `{Source, Dest}`; positional inputs the wrapper parses.
//   - Result   — `{Source, Dest, Store, Supersede}`; the new id + flag
//     for the wrapper's "✓ transferred: A → B (supersede link written)"
//     line and the structured --json output.
//   - Verb     — implements verb.BrainVerb[Args, Result].
//   - New(db, registry, actor) — constructor that returns Verb.
//
// The narrow TransferDB seam is a *sql.DB-shaped interface
// (TransferDB), satisfied by storage.RawDBAccessor.DB() in production
// and by an in-memory fake in transfer_test.go.
package transfer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/steveyegge/beads/internal/brain/verb"
	"github.com/steveyegge/beads/internal/idgen"
	"github.com/steveyegge/beads/internal/storage/depid"
)

// supersedeType is the dependency `type` value written to the source
// store when a transfer closes a source row. Matches the well-known
// vocabulary at types.WellKnownDependencyTypes — we use the string
// directly (rather than importing the typed constant) to keep this
// package free of upstream-bd vocabulary churn.
const supersedeType = "supersedes"

// destDescriptionTag is appended to the destination row's description
// to record provenance. Plain-text, single line, recognisable by a
// human reader and by grep. Not load-bearing; the supersede edge is
// the authoritative back-pointer.
const destDescriptionPrefix = "(transferred from "

// closeReasonFmt is the close_reason written on the source row. The
// string is the spec-required wording for ISC-52: it must contain the
// dest store name and the new dest id so that `bd show <source-id>`
// makes the destination discoverable from the close audit alone.
const closeReasonFmt = "transferred to %s: %s"

// TransferDB is the narrow seam Run needs from storage. Satisfied by
// the production *sql.DB returned by storage.RawDBAccessor.DB() and by
// the in-memory fake in transfer_test.go. Keeping the interface to a
// single method (Conn) avoids overconstraining the seam: the verb's
// per-call needs are entirely expressible through a *sql.Conn it
// acquires once and releases when Run returns.
type TransferDB interface {
	// Conn returns a single connection pinned to one underlying
	// database/sql connection. The verb uses it to hold a multi-
	// statement transaction across multiple Dolt databases on the
	// same SQL server. Callers must Close() the returned connection
	// when done.
	Conn(ctx context.Context) (*sql.Conn, error)
}

// Args carries the positional inputs the Cobra wrapper parses.
//
// Source is the issue id of the row to move (e.g. "inbox-abc"). Dest
// is the user-typed destination store name (e.g. "brain", "task"); it
// is resolved against the Registry inside Run, not by the wrapper, so
// the wrapper does not need to know about store-name aliases.
type Args struct {
	Source string
	Dest   string
}

// Result is what the verb returns on success.
type Result struct {
	// Source is the source id the wrapper echoes back. Unchanged from
	// Args.Source; kept on Result so the wrapper does not have to
	// re-read Args when formatting the success line.
	Source string `json:"source"`

	// Dest is the newly-allocated destination id, shaped
	// "<dest-prefix>-<hash>". This is the id `brain show` can look up
	// in the destination store after the transfer commits.
	Dest string `json:"dest"`

	// Store is the canonical destination store name the verb routed
	// to. Echoed back so the wrapper can render "transferred to
	// <store>" without knowing about aliases. Always the canonical
	// name in the registry, never an alias.
	Store string `json:"store"`

	// Supersede is true iff Run successfully wrote the supersede edge
	// row to the source store's dependencies table. False would
	// indicate the edge insert silently no-opped (ON DUPLICATE KEY)
	// or was skipped; the verb today always writes the edge, so a
	// false value in production is a strong signal of an unexpected
	// path.
	Supersede bool `json:"supersede"`
}

// Verb implements verb.BrainVerb[Args, Result].
type Verb struct {
	db       TransferDB
	registry *Registry
	actor    string
	// now is the clock the verb uses for all timestamps written in a
	// single transfer. Default time.Now.UTC; overridden by tests so
	// timestamps are deterministic and id generation is reproducible.
	now func() time.Time
}

// Compile-time proof that Verb satisfies BrainVerb with the concrete
// types declared in this package.
var _ verb.BrainVerb[Args, Result] = Verb{}

// New constructs a Verb. db is the seam Run pulls a connection from
// (in cmd/bd/, the wrapper adapts storage.RawDBAccessor into a
// TransferDB). registry is the resolved store mapping. actor is the
// audit-trail actor string — the same one the new/link verbs use,
// fetched via PersistentPreRun.
//
// New does NOT load the registry itself: the Cobra wrapper calls
// Load() so the wrapper can surface a clear error on a malformed
// stores.yaml before the verb ever runs.
func New(db TransferDB, registry *Registry, actor string) Verb {
	return Verb{db: db, registry: registry, actor: actor, now: defaultNow}
}

// defaultNow is the production clock — UTC, monotonic-free for the
// stored value (UTC strips the monotonic reading at TZ conversion).
// Pulled out as a package var so transfer_test.go can construct a
// Verb with a deterministic clock without exporting an Option type.
func defaultNow() time.Time { return time.Now().UTC() }

// Name returns the verb word as it appears on the CLI ("transfer").
func (Verb) Name() string { return "transfer" }

// transferSource holds the subset of issue columns we read from the
// source row before the insert. Keeping it local (rather than reaching
// for types.Issue) keeps the verb decoupled from any upstream-bd
// schema additions to the Issue struct that we don't need.
type transferSource struct {
	id          string
	title       string
	description string
	issueType   string
	status      string
}

// Run is the entire behaviour of `brain transfer <source> <dest>`.
//
// Validation order:
//
//  1. Empty source                → "brain transfer: source id is required"
//  2. Empty dest                  → "brain transfer: destination store name is required"
//  3. Source prefix unresolvable  → "brain transfer: unknown source store for prefix %q-"  (ISC-54)
//  4. Dest name unresolvable      → "brain transfer: unknown destination store %q"          (ISC-55)
//  5. Source row missing          → "brain transfer: %s not found in %s"
//  6. Source row already closed   → "brain transfer: %s is already closed"
//
// On success a single transaction:
//
//   - selects the source row (read-locked via SELECT … FOR UPDATE so a
//     concurrent transfer attempt on the same id fails or waits)
//   - reads the destination store's config issue_prefix (matches what
//     `brain new` would have used had the row been created there
//     natively; preserves the per-store prefix override if any)
//   - generates a new destination id via idgen.GenerateHashID with the
//     dest prefix and a collision check against `<dest>`.issues
//   - inserts the new row into `<dest>`.issues with status=open,
//     issue_type carried from the source (or "knowledge" if the source
//     was a kind the dest store cannot accept — but in practice every
//     store accepts the same kind vocabulary so we forward unchanged)
//   - updates the source row: status=closed, close_reason set, closed_at
//     and updated_at stamped, all in `<source>`.issues
//   - inserts the supersede edge in `<source>`.dependencies with
//     depends_on_external = <new-dest-id>
//
// Then commits. Any error rolls everything back; partial state can
// never be observed.
//
// Run never writes to stdout/stderr.
func (v Verb) Run(ctx context.Context, args Args) (Result, error) {
	if v.registry == nil {
		return Result{}, errors.New("brain transfer: registry is not configured")
	}
	if v.db == nil {
		return Result{}, errors.New("brain transfer: storage is not configured")
	}

	srcID := strings.TrimSpace(args.Source)
	destName := strings.ToLower(strings.TrimSpace(args.Dest))
	if srcID == "" {
		return Result{}, errors.New("brain transfer: source id is required")
	}
	if destName == "" {
		return Result{}, errors.New("brain transfer: destination store name is required")
	}

	srcStore, srcDB, err := v.registry.ResolveSource(srcID)
	if err != nil {
		return Result{}, err
	}
	destDB, destPrefix, err := v.registry.ResolveDest(destName)
	if err != nil {
		return Result{}, err
	}
	// Canonicalise dest name for the Result. If the user typed an
	// alias (e.g. "task"), the Result reports the registered canonical
	// name (e.g. "tasks") so downstream consumers don't have to know
	// about the alias table. Unknown names — which should be
	// impossible at this point because ResolveDest already succeeded —
	// fall through unchanged.
	canonicalDest := canonicalStoreName(destName)

	// One pinned connection for the entire TX. Releasing it at the end
	// returns the conn to the pool; the caller never sees raw conn lifetime.
	conn, err := v.db.Conn(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("brain transfer: acquire conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("brain transfer: begin: %w", err)
	}
	// rollbackOnly is set false after Commit; on any error path we
	// roll back so partial writes never escape.
	var committed bool
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Step 1 — read the source row. SELECT … FOR UPDATE serializes a
	// concurrent transfer attempt on the same id: the second TX waits
	// until we commit / abort, then sees status='closed' and bails
	// out via the "already closed" check below.
	src, err := readSource(ctx, tx, srcDB, srcID)
	if err != nil {
		return Result{}, err
	}
	if src == nil {
		return Result{}, fmt.Errorf("brain transfer: %s not found in %s", srcID, srcStore)
	}
	if strings.EqualFold(src.status, "closed") {
		return Result{}, fmt.Errorf("brain transfer: %s is already closed", srcID)
	}

	// Step 2 — generate the destination id. We do not call into
	// issueops.GenerateIssueIDInTable because that path is wired to a
	// single-database transaction context; here the TX spans two
	// databases. The hash-id loop is the same algorithm — try widths
	// 6..8 with 10 nonces each — and matches what `brain new` would
	// have produced if the row had been created in the dest store
	// natively.
	now := v.now()
	newID, err := generateDestID(ctx, tx, destDB, destPrefix, src.title, src.description, v.actor, now)
	if err != nil {
		return Result{}, err
	}

	// Step 3 — insert into the destination store. We deliberately
	// preserve the source's issue_type so a knowledge doc transferred
	// from inbox arrives in brain as a knowledge doc, not a task.
	if err := insertDest(ctx, tx, destDB, newID, src, srcID, canonicalDest, v.actor, now); err != nil {
		return Result{}, fmt.Errorf("brain transfer: insert dest %s: %w", newID, err)
	}

	// Step 4 — close the source. close_reason carries the dest id so
	// the audit trail makes the supersession self-describing.
	if err := closeSource(ctx, tx, srcDB, srcID, canonicalDest, newID, now); err != nil {
		return Result{}, fmt.Errorf("brain transfer: close source %s: %w", srcID, err)
	}

	// Step 5 — record the supersede edge in the source store's
	// dependencies table. depends_on_external carries the cross-store
	// dest id; depid.New makes the row's primary key deterministic so
	// the edge is merge-safe across Dolt clones (gastownhall/beads#4259).
	if err := recordSupersede(ctx, tx, srcDB, srcID, newID, v.actor, now); err != nil {
		return Result{}, fmt.Errorf("brain transfer: record supersede %s -> %s: %w", srcID, newID, err)
	}

	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("brain transfer: commit: %w", err)
	}
	committed = true

	return Result{
		Source:    srcID,
		Dest:      newID,
		Store:     canonicalDest,
		Supersede: true,
	}, nil
}

// readSource locks and returns the source row's id, title, description,
// issue_type, and status. Uses SELECT … FOR UPDATE so the row is
// row-level locked for the rest of the TX. nil return means "not
// found"; an error return means "transport failed".
//
// The query is built with the source DB name backtick-quoted directly;
// callers must validate the db name before calling. Registry.ResolveSource
// only returns names that have passed the validDBName regex.
func readSource(ctx context.Context, tx *sql.Tx, db, id string) (*transferSource, error) {
	query := fmt.Sprintf(
		"SELECT id, title, description, issue_type, status FROM `%s`.issues WHERE id = ? FOR UPDATE",
		db,
	)
	row := tx.QueryRowContext(ctx, query, id)
	var s transferSource
	if err := row.Scan(&s.id, &s.title, &s.description, &s.issueType, &s.status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("brain transfer: read source %s.%s: %w", db, id, err)
	}
	return &s, nil
}

// generateDestID mints a unique issue id in the destination store
// using the same hash-id algorithm `brain new` uses. The loop tries
// lengths 6..8 with 10 nonces each — matches issueops.GenerateIssueIDInTable
// without taking that helper's single-database assumption.
//
// The collision-check query reads `<dest>`.issues, never the source
// store, so we cannot accidentally collide with a source-store id of
// the same shape.
func generateDestID(ctx context.Context, tx *sql.Tx, destDB, prefix, title, description, actor string, now time.Time) (string, error) {
	const minLen, maxLen, nonces = 6, 8, 10
	for length := minLen; length <= maxLen; length++ {
		for nonce := 0; nonce < nonces; nonce++ {
			candidate := idgen.GenerateHashID(prefix, title, description, actor, now, length, nonce)
			var count int
			query := fmt.Sprintf("SELECT COUNT(*) FROM `%s`.issues WHERE id = ?", destDB)
			if err := tx.QueryRowContext(ctx, query, candidate).Scan(&count); err != nil {
				return "", fmt.Errorf("brain transfer: dest id collision check: %w", err)
			}
			if count == 0 {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("brain transfer: could not allocate dest id for prefix %q after %d lengths × %d nonces", prefix, maxLen-minLen+1, nonces)
}

// insertDest writes the new row into <destDB>.issues. The column list
// is the minimal viable subset to satisfy the table's NOT NULL +
// DEFAULT constraints (see migration 0001_create_issues.up.sql).
// Columns not named in the INSERT take their defaults — '' for the
// many TEXT-with-default-empty-string columns, 0 for the TINYINT flags,
// JSON_OBJECT() for metadata, etc.
//
// We carry source title and description verbatim and append a
// provenance tag to the description so the move is visible even if
// the supersede edge is lost (defence-in-depth — the edge is the
// authoritative back-pointer, but tag in the body costs nothing).
func insertDest(ctx context.Context, tx *sql.Tx, destDB, newID string, src *transferSource, srcID, destStore, actor string, now time.Time) error {
	description := src.description
	tag := destDescriptionPrefix + srcID + ")"
	switch {
	case description == "":
		description = tag
	case strings.Contains(description, tag):
		// Avoid double-tagging if a row is somehow transferred twice
		// (it should not be — the source is closed after the move —
		// but the defence is cheap).
	default:
		description = description + "\n\n" + tag
	}

	// Carry issue_type. If the source had no type (legacy data), fall
	// back to "knowledge" because that is the only kind every store
	// accepts as a destination shape for a moved doc.
	issueType := strings.TrimSpace(src.issueType)
	if issueType == "" {
		issueType = "knowledge"
	}

	// content_hash is a stable hash for change detection elsewhere in
	// bd; we mint a fresh UUID rather than recompute the SHA so this
	// path has no dependency on types.Issue's hash machinery. The DB
	// only requires the column to be non-NULL (CHAR(64)); UUIDs in
	// canonical form are 36 chars and fit.
	contentHash := strings.ReplaceAll(uuid.New().String(), "-", "")

	query := fmt.Sprintf(`INSERT INTO `+"`%s`"+`.issues
		(id, content_hash, title, description, design, acceptance_criteria, notes,
		 status, priority, issue_type, created_at, created_by, updated_at, metadata)
		VALUES (?, ?, ?, ?, '', '', '',
		        'open', 2, ?, ?, ?, ?, JSON_OBJECT())`, destDB)
	_, err := tx.ExecContext(ctx, query,
		newID, contentHash, src.title, description,
		issueType, now, actor, now,
	)
	if err != nil {
		return err
	}
	_ = destStore // retained in signature for future audit-event hooks; intentionally unused today
	return nil
}

// closeSource flips the source row to status=closed and stamps
// close_reason / closed_at / updated_at. Uses the same wording the
// spec requires for ISC-52.
//
// The UPDATE is row-level: it touches one row matched by id and
// participates in the same TX as the destination INSERT, so either
// the source closes AND the dest exists, or neither happens.
func closeSource(ctx context.Context, tx *sql.Tx, srcDB, srcID, destStore, newID string, now time.Time) error {
	reason := fmt.Sprintf(closeReasonFmt, destStore, newID)
	query := fmt.Sprintf(
		"UPDATE `%s`.issues SET status = 'closed', close_reason = ?, closed_at = ?, updated_at = ? WHERE id = ?",
		srcDB,
	)
	res, err := tx.ExecContext(ctx, query, reason, now, now, srcID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("brain transfer: rows affected on close: %w", err)
	}
	if n == 0 {
		// Should not happen — we just SELECT … FOR UPDATEd the row —
		// but if it does, fail loudly so the TX rolls back rather
		// than silently committing a half-move.
		return fmt.Errorf("brain transfer: source %s vanished mid-transfer", srcID)
	}
	return nil
}

// recordSupersede inserts the cross-store supersede edge into the
// SOURCE store's `dependencies` table.
//
//   - issue_id = srcID            — FK fk_dep_issue requires this to
//     exist in `<srcDB>`.issues. The source row is closed but still
//     present, so the FK is satisfied.
//   - depends_on_external = newID — no FK, holds the cross-store id.
//   - depends_on_issue_id, depends_on_wisp_id = NULL — the row is
//     external-type, only one of the three target columns is set.
//   - type = 'supersedes'         — well-known dependency type.
//   - id   = depid.New(...)       — deterministic primary key, see
//     internal/storage/depid for the rationale (#4259).
//
// thread_id is set to '' to satisfy the column's NOT NULL + default ''
// shape on older schemas; metadata defaults to JSON_OBJECT().
func recordSupersede(ctx context.Context, tx *sql.Tx, srcDB, srcID, destID, actor string, now time.Time) error {
	rowID := depid.New(srcID, destID)
	query := fmt.Sprintf(`INSERT INTO `+"`%s`"+`.dependencies
		(id, issue_id, depends_on_issue_id, depends_on_wisp_id, depends_on_external,
		 type, created_at, created_by, metadata, thread_id)
		VALUES (?, ?, NULL, NULL, ?, ?, ?, ?, JSON_OBJECT(), '')
		ON DUPLICATE KEY UPDATE type = VALUES(type)`, srcDB)
	_, err := tx.ExecContext(ctx, query, rowID, srcID, destID, supersedeType, now, actor)
	return err
}

// canonicalStoreName returns the canonical store name for a possibly-
// alias user-typed name. Aliases in the package-private builtinAliases
// table map to their canonical name; everything else is returned
// unchanged. Lowercased input — callers must pre-normalise to lower
// case (Run does).
func canonicalStoreName(name string) string {
	if canonical, ok := builtinAliases[name]; ok {
		return canonical
	}
	return name
}
