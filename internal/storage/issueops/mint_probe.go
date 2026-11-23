package issueops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// MintProbe is the answer to "what would a create through this connection mint
// under?", obtained without minting anything. A host verification reads it
// through each store's wrapper to prove the wrapper's store owns the prefix its
// beads carry.
type MintProbe struct {
	// Store is the wrapper-pinned namespace (BD_NAME); empty when none is set.
	Store string `json:"store"`
	// Unified is true when the connected database is the unified database.
	Unified bool `json:"unified"`
	// Routed is true when the config read is routed to the store's own row
	// (unified database AND a pinned store).
	Routed bool `json:"routed"`
	// MintPrefix is the prefix a create would mint with: exactly what the create
	// path reads (ReadConfigPrefix), so the probe cannot drift from it.
	MintPrefix string `json:"mint_prefix"`
	// DBIssuePrefix is the database-wide config.issue_prefix, the value stock bd
	// mints from. It is reported so a disagreement shows its cause.
	DBIssuePrefix string `json:"db_issue_prefix"`
	// SampleID is the id shape the create would produce for the prefix; it is
	// built, never inserted.
	SampleID string `json:"sample_id"`
	// Owned reports whether the pinned namespace owns MintPrefix on the unified
	// database (the same check every create runs). Absent off the unified database.
	Owned *bool `json:"owned,omitempty"`
	// Error is the refusal the create path would raise, when it would refuse.
	Error string `json:"error,omitempty"`
}

// ProbeMintPrefix resolves the prefix a create would mint under, without
// inserting. It only reads, so it leaves no bead (and no write) behind whatever
// handle it is given; a caller may still pass a transaction and roll it back.
func ProbeMintPrefix(ctx context.Context, q DBTX) (MintProbe, error) {
	sc := UnifiedScopeForTx(ctx, q)
	p := MintProbe{Store: sc.Store, Unified: sc.Unified, Routed: sc.Enabled()}

	var dbWide sql.NullString
	if err := q.QueryRowContext(ctx, "SELECT value FROM config WHERE `key` = ?", "issue_prefix").Scan(&dbWide); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return p, fmt.Errorf("reading the database-wide issue_prefix: %w", err)
	}
	p.DBIssuePrefix = strings.TrimSuffix(strings.TrimSpace(dbWide.String), "-")

	if err := RefuseStorelessMint(ctx, q); err != nil {
		p.Error = err.Error()
		return p, nil
	}
	prefix, err := ReadConfigPrefix(ctx, q)
	if err != nil {
		p.Error = err.Error()
		return p, nil
	}
	p.MintPrefix = prefix
	p.SampleID = prefix + "-probe"
	if sc.Unified && sc.Store != "" {
		owned, err := namespaceOwnsPrefix(ctx, q, sc.Store, prefix)
		if err != nil {
			return p, fmt.Errorf("checking namespace ownership of %q: %w", prefix, err)
		}
		p.Owned = &owned
	}
	return p, nil
}
