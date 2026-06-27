// Package exfiltrator renders every bd issue, regardless of kind, to a
// markdown file under a configurable knowledge root.
//
// The exfiltrator is the *render half* of brain v0.3's Dolt → markdown
// exfiltration loop. Dolt remains the source of truth; the markdown files
// are derived render artifacts that Pulse (and other markdown consumers)
// can read without knowing anything about Dolt.
//
// This package owns three load-bearing concerns:
//
//  1. **Slug derivation.** Issue → on-disk filename. Stable across title
//     edits via a `brain_slug` field in `issues.metadata` JSON. See
//     deriveSlug / persistSlug.
//  2. **Markdown render.** Issue → frontmatter + body, written atomically
//     via tmp+rename. See Render.
//  3. **Checkpoint file (ISC-121).** Pending writes are recorded at
//     `entries/.checkpoint.json` BEFORE the on-disk render starts and
//     cleared AFTER the render succeeds. A crash mid-write leaves the
//     checkpoint for `brain reconcile` to finish. See checkpoint helpers.
//
// The package is pure Go — no Dolt, no sql, no cgo. That keeps the test
// surface fast and lets it run on machines whose libicu/icu4c is stuck
// (see USER/FRICTION.md, 2026-05-31).
//
// See divergence/0012-exfiltration-decorator.md for the landing notes
// and the documented "sensible default" choices this package commits to.
package exfiltrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// BrainSlugMetadataKey is the JSON field name in issues.metadata where
// the stable slug is stored. Kept stable here (not derived from the
// title) so renaming an issue does NOT relocate its markdown file.
const BrainSlugMetadataKey = "brain_slug"

// CheckpointFilename is the relative filename, under the configured
// root, where pending render operations are recorded for crash
// recovery (ISC-121).
const CheckpointFilename = ".checkpoint.json"

// ── Exfiltrator interface ──────────────────────────────────────────

// Exfiltrator is the rendering surface the storage decorator depends on.
//
// Kept narrow on purpose (two methods) so the storage decorator can be
// unit-tested with a fake Exfiltrator without bringing up the filesystem.
type Exfiltrator interface {
	// Render writes the markdown for issue to disk. If issue.IssueType
	// is not a brain kind, Render returns nil without touching disk.
	//
	// The slug used for the on-disk filename is the one persisted in
	// issue.Metadata under BrainSlugMetadataKey when present; otherwise
	// derived from issue.Title and written back to the storage layer
	// by SlugPersister (when provided to the markdown exfiltrator).
	Render(ctx context.Context, issue *types.Issue) error

	// Remove deletes the markdown for an issue at the given (kind, slug)
	// pair. Used when an issue's kind transitions out of the brain set
	// (e.g., knowledge → bug) so the stale file does not orphan.
	//
	// If the file is missing, Remove returns nil — it is idempotent.
	Remove(ctx context.Context, issueID string, kind types.IssueType, slug string) error
}

// SlugPersister is the optional seam the markdown exfiltrator uses to
// persist a freshly-derived slug back into the storage layer's
// issues.metadata column. Production wires this to the storage's
// SlotSet method (or equivalent) so the slug survives across reads.
//
// When nil, the slug is derived on every render — correct, but a
// title edit may relocate the file. Production code must always
// provide a SlugPersister.
type SlugPersister interface {
	// SetSlug records slug as the canonical filename slug for issueID.
	// Implementations should write to issues.metadata.brain_slug.
	SetSlug(ctx context.Context, issueID, slug string) error
}

// ── MarkdownExfiltrator ────────────────────────────────────────────

// MarkdownExfiltrator implements Exfiltrator by writing
// `<root>/entries/{kind}/{slug}.md` files with YAML frontmatter +
// markdown body. Atomic via tmp+rename. Checkpointed for crash
// recovery (ISC-121).
//
// When BRAIN_EXFIL_FLAT=1, the kind subdirectory is omitted and files
// land at `<root>/entries/{slug}.md` instead. Use this for dedicated
// stores where the store name already implies the kind.
type MarkdownExfiltrator struct {
	// root is the absolute filesystem path under which `entries/`
	// lives. Computed at construction; never reread.
	root string

	// flat skips the {kind}/ subdirectory when true. Set via
	// BRAIN_EXFIL_FLAT=1 at construction time.
	flat bool

	// persister, when non-nil, records freshly-derived slugs back
	// into the storage layer. May be nil in tests.
	persister SlugPersister

	// mu serializes checkpoint and slug-allocation writes. The
	// render itself is concurrent-safe by virtue of atomic rename,
	// but the checkpoint file and the slug-uniqueness allocation
	// path require serialization.
	mu sync.Mutex

	// allocatedSlugs tracks slugs we have already handed out during
	// this process's lifetime, keyed by `{kind}/{slug}`. Used to
	// disambiguate brand-new issues that collide before either has
	// been re-fetched. This is a best-effort in-memory dedup; the
	// reconciler is the source of truth for cross-process collisions.
	allocatedSlugs map[string]string // key: "{kind}/{slug}" → issueID
}

// NewMarkdownExfiltrator constructs a MarkdownExfiltrator rooted at
// root. `~` in root is expanded against the current process's HOME.
//
// persister may be nil — tests typically pass nil; production wires
// it to a storage-backed implementation so the slug survives across
// reads.
func NewMarkdownExfiltrator(root string, persister SlugPersister) *MarkdownExfiltrator {
	return &MarkdownExfiltrator{
		root:           expandHome(root),
		flat:           os.Getenv("BRAIN_EXFIL_FLAT") == "1",
		persister:      persister,
		allocatedSlugs: make(map[string]string),
	}
}

// Root returns the resolved on-disk root the exfiltrator writes under.
// Useful in tests and in `brain reconcile` for path computation.
func (m *MarkdownExfiltrator) Root() string { return m.root }

// ── Public API ─────────────────────────────────────────────────────

// Render is the load-bearing entry point. Implements ISC-117, ISC-118,
// ISC-119, ISC-120 (perf), ISC-121 (checkpoint).
func (m *MarkdownExfiltrator) Render(ctx context.Context, issue *types.Issue) error {
	if issue == nil {
		return errors.New("exfiltrator: nil issue")
	}
	if issue.ID == "" {
		return errors.New("exfiltrator: issue has no ID")
	}
	if issue.IssueType == "" {
		return errors.New("exfiltrator: issue has no kind")
	}

	slug, persistedNow, err := m.slugFor(issue)
	if err != nil {
		return fmt.Errorf("exfiltrator: derive slug for %s: %w", issue.ID, err)
	}

	path := m.pathFor(issue.IssueType, slug)
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("exfiltrator: mkdir for %s: %w", issue.ID, err)
	}

	if err := m.writeCheckpoint(checkpointEntry{
		ID:   issue.ID,
		Kind: string(issue.IssueType),
		Op:   "write",
		Slug: slug,
	}); err != nil {
		return fmt.Errorf("exfiltrator: write checkpoint for %s: %w", issue.ID, err)
	}

	body := renderMarkdown(issue)
	if err := atomicWrite(path, []byte(body)); err != nil {
		return fmt.Errorf("exfiltrator: write %s: %w", path, err)
	}

	if persistedNow && m.persister != nil {
		if err := m.persister.SetSlug(ctx, issue.ID, slug); err != nil {
			// Best-effort: the file is on disk, just the back-reference
			// failed. Log via error return; the decorator can choose to
			// surface or swallow.
			_ = m.clearCheckpoint()
			return fmt.Errorf("exfiltrator: persist slug for %s: %w", issue.ID, err)
		}
	}

	if err := m.clearCheckpoint(); err != nil {
		return fmt.Errorf("exfiltrator: clear checkpoint for %s: %w", issue.ID, err)
	}
	return nil
}

// Remove deletes the markdown for the given (kind, slug) pair if it
// exists. ENOENT is not an error.
//
// Used by the decorator when an issue's kind transitions out of the
// brain set so the stale file does not orphan.
func (m *MarkdownExfiltrator) Remove(_ context.Context, issueID string, kind types.IssueType, slug string) error {
	if slug == "" {
		return fmt.Errorf("exfiltrator: remove %s: empty slug", issueID)
	}
	if kind == "" {
		return fmt.Errorf("exfiltrator: remove %s: empty kind", issueID)
	}

	path := m.pathFor(kind, slug)
	if err := m.writeCheckpoint(checkpointEntry{
		ID:   issueID,
		Kind: string(kind),
		Op:   "delete",
		Slug: slug,
	}); err != nil {
		return fmt.Errorf("exfiltrator: write checkpoint for %s: %w", issueID, err)
	}

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("exfiltrator: remove %s: %w", path, err)
	}

	if err := m.clearCheckpoint(); err != nil {
		return fmt.Errorf("exfiltrator: clear checkpoint for %s: %w", issueID, err)
	}
	return nil
}

// PathFor exposes the on-disk path the exfiltrator would write for a
// given (kind, slug) pair. Exported so the storage decorator and tests
// can introspect the layout without duplicating it.
func (m *MarkdownExfiltrator) PathFor(kind types.IssueType, slug string) string {
	return m.pathFor(kind, slug)
}

// SlugFor exposes the slug the exfiltrator would write for issue,
// preferring an already-persisted slug in issue.Metadata over a
// freshly-derived one. The boolean indicates whether the returned
// slug was derived on this call (i.e., not present in metadata).
//
// Exported so the storage decorator can resolve a slug before the
// kind transition path overwrites it.
func (m *MarkdownExfiltrator) SlugFor(issue *types.Issue) (string, bool, error) {
	return m.slugFor(issue)
}

// ── Slug derivation ────────────────────────────────────────────────

// slugFor returns the canonical filename slug for issue.
//
// Lookup order (sensible default, documented in divergence/0012):
//  1. issue.Metadata.brain_slug — stable across title edits.
//  2. kebab(issue.Title) — derived; persisted via SlugPersister.
//  3. kebab(issue.ID) — fallback when title is empty.
//
// Collision handling: if the derived slug is already allocated in
// this process to a different ID at the same kind, append the last
// 6 characters of the issue ID (stripped of any "B-" prefix) to
// disambiguate.
func (m *MarkdownExfiltrator) slugFor(issue *types.Issue) (slug string, derivedNow bool, err error) {
	if persisted := metadataSlug(issue.Metadata); persisted != "" {
		return persisted, false, nil
	}

	base := kebab(issue.Title)
	if base == "" {
		base = kebab(issue.ID)
	}
	if base == "" {
		return "", false, errors.New("issue has no title and no id")
	}

	slug = base
	var key string
	if m.flat {
		key = slug
	} else {
		key = fmt.Sprintf("%s/%s", issue.IssueType, slug)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if owner, ok := m.allocatedSlugs[key]; ok && owner != issue.ID {
		slug = base + "-" + shortID(issue.ID)
		if m.flat {
			key = slug
		} else {
			key = fmt.Sprintf("%s/%s", issue.IssueType, slug)
		}
	}
	m.allocatedSlugs[key] = issue.ID
	return slug, true, nil
}

// metadataSlug returns the brain_slug field stored in raw, or "" when
// raw is nil, empty, malformed, or missing the field.
func metadataSlug(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	if v, ok := m[BrainSlugMetadataKey].(string); ok {
		return v
	}
	return ""
}

// kebab converts s to a lowercase, hyphen-separated slug.
//
// Rules:
//   - lowercase ASCII letters and digits survive
//   - runs of non-[a-z0-9] characters collapse to a single "-"
//   - leading and trailing "-" are stripped
//   - unicode letters are passed through tolower then filtered (the
//     ASCII filter strips anything non-[a-z0-9]); non-ASCII collapses
//     to "-" — sensible default; we do not ship a transliterator
var nonSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

func kebab(s string) string {
	if s == "" {
		return ""
	}
	lower := strings.ToLower(s)
	hyphenated := nonSlugRe.ReplaceAllString(lower, "-")
	return strings.Trim(hyphenated, "-")
}

// shortID returns the last 6 characters of id (sans any "B-" prefix),
// suitable for slug disambiguation. Empty id → empty string.
func shortID(id string) string {
	trimmed := strings.TrimPrefix(id, "B-")
	if len(trimmed) <= 6 {
		return trimmed
	}
	return trimmed[len(trimmed)-6:]
}

// ── Path layout ────────────────────────────────────────────────────

func (m *MarkdownExfiltrator) pathFor(kind types.IssueType, slug string) string {
	if m.flat {
		return filepath.Join(m.root, "entries", slug+".md")
	}
	return filepath.Join(m.root, "entries", string(kind), slug+".md")
}

// ── Markdown render ────────────────────────────────────────────────

// renderMarkdown returns the full file body: YAML frontmatter +
// "# {title}\n\n{body}".
//
// Shape documented in divergence/0012 § Decisions. Any change to this
// shape ripples into the reconciler's idempotence guarantee (ISC-123),
// so future edits must keep the byte-for-byte rendering stable for a
// given (issue snapshot) input.
func renderMarkdown(issue *types.Issue) string {
	var b strings.Builder
	b.Grow(len(issue.Description) + 256)

	b.WriteString("---\n")
	b.WriteString("id: ")
	b.WriteString(issue.ID)
	b.WriteByte('\n')
	b.WriteString("title: ")
	b.WriteString(yamlString(issue.Title))
	b.WriteByte('\n')
	b.WriteString("kind: ")
	b.WriteString(string(issue.IssueType))
	b.WriteByte('\n')
	if issue.Status != "" {
		b.WriteString("status: ")
		b.WriteString(string(issue.Status))
		b.WriteByte('\n')
	}
	b.WriteString("priority: ")
	b.WriteString(fmt.Sprintf("%d", issue.Priority))
	b.WriteByte('\n')
	if !issue.CreatedAt.IsZero() {
		b.WriteString("created: ")
		b.WriteString(issue.CreatedAt.UTC().Format(time.RFC3339))
		b.WriteByte('\n')
	}
	if !issue.UpdatedAt.IsZero() {
		b.WriteString("updated: ")
		b.WriteString(issue.UpdatedAt.UTC().Format(time.RFC3339))
		b.WriteByte('\n')
	}
	if len(issue.Labels) > 0 {
		b.WriteString("labels: [")
		for i, l := range issue.Labels {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(yamlString(l))
		}
		b.WriteString("]\n")
	}
	b.WriteString("---\n\n")

	if issue.Title != "" {
		b.WriteString("# ")
		b.WriteString(issue.Title)
		b.WriteString("\n\n")
	}
	if issue.Description != "" {
		b.WriteString(issue.Description)
		if !strings.HasSuffix(issue.Description, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// yamlString quotes s for safe inclusion as a YAML scalar.
//
// Always-quote policy: simpler, no special-character analysis. JSON
// encoding is a valid YAML scalar so we reuse encoding/json to handle
// escaping deterministically.
func yamlString(s string) string {
	enc, _ := json.Marshal(s)
	return string(enc)
}

// ── Atomic write ───────────────────────────────────────────────────

// atomicWrite writes data to path via a tmp sibling + rename.
// Guarantees no half-written file is ever observed.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".exf-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = os.Remove(tmpName)
	}
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
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

func ensureDir(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

// ── Checkpoint (ISC-121) ───────────────────────────────────────────

type checkpointEntry struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Op   string `json:"op"` // "write" | "delete"
	Slug string `json:"slug"`
}

type checkpointFile struct {
	Pending []checkpointEntry `json:"pending"`
	Ts      string            `json:"ts"`
}

// writeCheckpoint records the in-flight operation before the render
// begins. The file is removed by clearCheckpoint after the render
// succeeds. A crash between writeCheckpoint and clearCheckpoint leaves
// the file behind for `brain reconcile` to finish (future tranche).
func (m *MarkdownExfiltrator) writeCheckpoint(entry checkpointEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	path := m.checkpointPath()
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	cf := checkpointFile{
		Pending: []checkpointEntry{entry},
		Ts:      time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWrite(path, data)
}

// clearCheckpoint removes the checkpoint file. ENOENT is not an error
// — a passthrough mutation (non-brain kind) never wrote the file.
func (m *MarkdownExfiltrator) clearCheckpoint() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	path := m.checkpointPath()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// CheckpointPath exposes the on-disk path of the checkpoint file
// (under `<root>/entries/`). Exported so the reconciler can locate
// the file without duplicating the layout decision.
func (m *MarkdownExfiltrator) CheckpointPath() string { return m.checkpointPath() }

func (m *MarkdownExfiltrator) checkpointPath() string {
	return filepath.Join(m.root, "entries", CheckpointFilename)
}

// ── Helpers ────────────────────────────────────────────────────────

// expandHome replaces a leading "~" or "~/" in p with the value of
// $HOME (or, when $HOME is unset, leaves p unchanged). The exfiltrator
// intentionally does not consult os/user — $HOME is the dependable
// reference on every supported platform.
func expandHome(p string) string {
	if p == "" {
		return p
	}
	if p == "~" {
		if home := os.Getenv("HOME"); home != "" {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home := os.Getenv("HOME"); home != "" {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// MetadataWithSlug returns a JSON metadata blob with brain_slug set to
// slug, merged with any prior fields in existing. Exported so the
// storage layer's SetSlug implementation can construct the correct
// JSON payload without duplicating the field name.
func MetadataWithSlug(existing json.RawMessage, slug string) (json.RawMessage, error) {
	m := map[string]any{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &m); err != nil {
			return nil, err
		}
	}
	m[BrainSlugMetadataKey] = slug
	out, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return out, nil
}
