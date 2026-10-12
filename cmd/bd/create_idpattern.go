package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/idpattern"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// storeIDPattern returns the store's date-id pattern (create.id-pattern in
// config.yaml), or ok=false when the store mints ordinary ids.
func storeIDPattern() (idpattern.Kind, bool, error) {
	return idpattern.Parse(config.GetString(idpattern.ConfigKey))
}

// resolveCreateID applies the store's date-id pattern to a single bead
// create. It returns the id the bead must carry, or explicitID unchanged when
// the store has no pattern or the bead is exempt (a wisp, or a child of
// another bead). Without a pattern --date is an error rather than ignored.
func resolveCreateID(prefix, explicitID, date, parentID, mintPrefix string, ephemeral bool, now time.Time) (string, error) {
	kind, ok, err := storeIDPattern()
	if err != nil {
		return "", err
	}
	if !ok {
		if date != "" {
			return "", fmt.Errorf("--date needs a store with %s set (day, week, month, quarter or year)", idpattern.ConfigKey)
		}
		return explicitID, nil
	}
	if ephemeral || parentID != "" {
		if date != "" {
			return "", fmt.Errorf("--date does not apply to wisps or --parent children")
		}
		return explicitID, nil
	}
	if mintPrefix != "" {
		return "", fmt.Errorf("--prefix would mint an id outside this store's %s pattern", kind)
	}
	r, err := idpattern.New(kind, prefix, now)
	if err != nil {
		return "", err
	}
	return r.Resolve(explicitID, date)
}

// rejectIDPatternRoute refuses a bulk create route (--file, --graph) in a
// store with a date-id pattern: those routes mint their own ids, so they
// would silently break the one-bead-per-period rule.
func rejectIDPatternRoute(route string) error {
	kind, ok, err := storeIDPattern()
	if err != nil {
		return err
	}
	if ok {
		return fmt.Errorf("%s cannot be used in a store with %s=%s; create each bead singly so it gets its %s id", route, idpattern.ConfigKey, kind, kind)
	}
	return nil
}

// ensureIDPatternFree refuses to create id when a bead already holds it,
// naming that bead. get returns storage.ErrNotFound for an unused id.
func ensureIDPatternFree(ctx context.Context, id string, get func(context.Context, string) (*types.Issue, error)) error {
	existing, err := get(ctx, id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("check for existing %s: %w", id, err)
	}
	if existing == nil {
		return nil
	}
	return fmt.Errorf("%s already exists (%q, %s); this store keeps one bead per period, update that one instead", existing.ID, existing.Title, existing.Status)
}

// createIDPrefix is the store's issue prefix as create validates it: the
// config.yaml issue-prefix when set, else the database's issue_prefix.
func createIDPrefix() string {
	if p := config.GetString("issue-prefix"); p != "" {
		return p
	}
	if store == nil {
		return ""
	}
	p, _ := store.GetConfig(rootCtx, "issue_prefix")
	return p
}
