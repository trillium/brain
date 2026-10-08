// Package exfiltrator — manifest.go
//
// The render manifest: the durable record of every markdown file the
// exfiltrator has written, keyed by bead id.
//
// Why it exists: a deletion in the rendered directory is invisible to the
// substrate — the file is simply gone. Nothing can tell "this bead's render
// was deleted" apart from "this bead was never rendered". The manifest is
// that memory: the render path records every file it writes and the removal
// path removes the entry it deletes. The edit-back run (internal/brain/editback)
// reads the manifest to catch deletions and mark the affected bead instead of
// forgetting it — the captain's verdict that deleting a rendered file never
// deletes a bead, it marks the bead for deletion.
//
// The manifest is advisory: losing it never loses data. A missing manifest
// only means deletion detection is unavailable until the next render
// recreates it (the edit-back run reports that as a loud, named refusal).

package exfiltrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// DeletionMarkLabel is the label a bead carries while its rendered file is
// missing and the deletion has not been reviewed. The captain's suggestion
// for the mark was a label, and a label it is: visible to every read that
// hydrates labels (bd show, bd list, bd ready), removable with bd label
// remove, and — critically — never a status transition, so the bead's own
// lifecycle is untouched by an accidental file deletion.
//
// While a bead carries this label the exfiltrator refuses to render it: a
// render that the mark has not been explicitly cleared must not resurrect
// the file the operator (or a sync accident in the synced folder) deleted.
// The only way to unmark and re-render is the explicit
// 'bd render-marks clear <id>' verb.
const DeletionMarkLabel = "marked-for-deletion"

// ManifestFilename is the relative filename, under the configured root's
// entries/ directory, where the render manifest lives.
const ManifestFilename = ".render-manifest.json"

// ManifestVersion is the current manifest format version. Bump on a
// shape change; readers refuse manifests from later versions rather than
// guessing at an unknown shape.
const ManifestVersion = 1

// ErrNoManifest is the sentinel the manifest read path returns when no
// manifest file exists yet (a store that has never rendered, or a render
// directory whose dot-files were pruned). Callers errors.Is it to report
// "deletion detection unavailable" as a named refusal rather than guessing.
var ErrNoManifest = errors.New("no render manifest")

// ManifestEntry records one rendered file.
type ManifestEntry struct {
	// Kind is the issue's kind at render time. The kind names which
	// entries/ subdirectory the file lives under (or is empty-irrelevant
	// in flat mode, but is still recorded so the removal path can
	// reproduce the path).
	Kind string `json:"kind"`
	// Slug is the bead's render-key slug.
	Slug string `json:"slug"`
	// Rendered is when the render last succeeded.
	Rendered time.Time `json:"rendered"`
}

// ManifestFile is the on-disk manifest shape.
type ManifestFile struct {
	Version int `json:"version"`
	// Updated is the RFC3339 timestamp of the last manifest write.
	Updated string                       `json:"updated"`
	Files   map[string]ManifestEntryView `json:"files"`
}

// ManifestEntryView is the stored per-bead shape; Path is relative to the
// root so the file survives the markdown directory being moved or synced
// between machines.
type ManifestEntryView struct {
	Path string    `json:"path"`
	Kind string    `json:"kind"`
	Slug string    `json:"slug"`
	Time time.Time `json:"rendered"`
}

// manifestPath returns the absolute path of the manifest file.
func (m *MarkdownExfiltrator) manifestPath() string {
	return filepath.Join(m.root, "entries", ManifestFilename)
}

// ManifestPath exposes the manifest's absolute path for the edit-back run
// and tests.
func (m *MarkdownExfiltrator) ManifestPath() string { return m.manifestPath() }

// RecordManifest adds (or refreshes) the manifest entry for a bead after a
// successful render. It reads, mutates, writes atomically — serialized under
// the exfiltrator's own mutex.
func (m *MarkdownExfiltrator) recordManifest(issueID, kind, slug string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	mf, readErr := m.readManifestLocked()
	if readErr != nil && !errors.Is(readErr, ErrNoManifest) {
		return fmt.Errorf("reading render manifest for %s: %w", issueID, readErr)
	}
	if mf == nil {
		mf = &ManifestFile{Version: ManifestVersion, Files: map[string]ManifestEntryView{}}
	}
	if mf.Files == nil {
		mf.Files = map[string]ManifestEntryView{}
	}
	rel, err := filepath.Rel(m.root, m.pathFor(types.IssueType(kind), slug))
	if err != nil {
		// Absurd roots ("", ".") cannot only degrade to the absolute path;
		// the entry stays legible either way.
		rel = m.pathFor(types.IssueType(kind), slug)
	}
	mf.Files[issueID] = ManifestEntryView{
		Path: rel,
		Kind: kind,
		Slug: slug,
		Time: time.Now().UTC(),
	}
	return m.writeManifestLocked(mf)
}

// DropManifest removes the manifest entry for a bead after its rendered file
// was removed by the store itself (kind transitions), so the entry cannot be
// mistaken for a later deletion by the edit-back run.
func (m *MarkdownExfiltrator) dropManifest(issueID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	mf, readErr := m.readManifestLocked()
	if readErr != nil {
		if errors.Is(readErr, ErrNoManifest) {
			return nil
		}
		return fmt.Errorf("reading render manifest for %s: %w", issueID, readErr)
	}
	if _, ok := mf.Files[issueID]; !ok {
		return nil
	}
	delete(mf.Files, issueID)
	return m.writeManifestLocked(mf)
}

// readManifestLocked reads the manifest file. A missing file is ErrNoManifest
// with a nil manifest.
//
// Caller must hold m.mu.
func (m *MarkdownExfiltrator) readManifestLocked() (*ManifestFile, error) {
	data, err := os.ReadFile(m.manifestPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoManifest
		}
		return nil, err
	}
	var mf ManifestFile
	if err := json.Unmarshal(data, &mf); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", m.manifestPath(), err)
	}
	if mf.Version > ManifestVersion {
		return nil, fmt.Errorf("%s: manifest version %d is newer than this bd understands (known: %d)",
			m.manifestPath(), mf.Version, ManifestVersion)
	}
	return &mf, nil
}

// writeManifestLocked atomically writes the manifest.
//
// Caller must hold m.mu.
func (m *MarkdownExfiltrator) writeManifestLocked(mf *ManifestFile) error {
	mf.Version = ManifestVersion
	mf.Updated = time.Now().UTC().Format(time.RFC3339)
	if err := ensureDir(filepath.Dir(m.manifestPath())); err != nil {
		return err
	}
	data, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWrite(m.manifestPath(), data)
}

// ReadManifest reads the manifest for an exfil root without an exfiltrator.
// A missing manifest is ErrNoManifest. Exported for the edit-back run, which
// needs the same read the render path uses so the two can never disagree
// about the manifest's location or shape.
func ReadManifest(root string) (*ManifestFile, string, error) {
	path := filepath.Join(expandHome(root), "entries", ManifestFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, path, ErrNoManifest
		}
		return nil, path, err
	}
	var mf ManifestFile
	if err := json.Unmarshal(data, &mf); err != nil {
		return nil, path, fmt.Errorf("parsing %s: %w", path, err)
	}
	if mf.Version > ManifestVersion {
		return nil, path, fmt.Errorf("%s: manifest version %d is newer than this bd understands (known: %d)",
			path, mf.Version, ManifestVersion)
	}
	return &mf, path, nil
}

// SortedManifestIDs answers the manifest's bead ids in a stable order, for
// reporting.
func SortedManifestIDs(mf *ManifestFile) []string {
	ids := make([]string, 0, len(mf.Files))
	for id := range mf.Files {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
