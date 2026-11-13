package main

import (
	"context"
	"fmt"

	"github.com/steveyegge/beads/internal/hooksdef"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/issueops"
)

// brainServeGate decides whether bd serve may serve this brain store.
//
// On a Dolt SQL server bd serve answers from a unit-of-work provider that sits
// beneath the store, so none of the store's brain layers reach HTTP: reads are
// not narrowed to the wrapper's namespace (on the unified database they answer
// for every store), the hooks.d guards and observers do not run, and writes
// render no markdown (render-all and the host render job catch up). Creates do
// mint under the wrapper's own prefix: that check lives below the provider.
// Until the rest is routed through the provider:
//
//   - declared hooks refuse the server outright, as they refuse writes in
//     proxied-server mode: a guard that silently stops guarding is worse than
//     no server;
//   - on the unified database the operator asks for the every-store view by
//     name, with --wide, the flag every other brain read uses for it.
func brainServeGate(ctx context.Context, beadsDir string, st storage.DoltStorage, wide bool) error {
	dir := hooksdef.HooksDir(beadsDir)
	defs, err := hooksdef.LoadAll(dir)
	if err != nil {
		return fmt.Errorf("hook definitions in %s are unusable, so writes cannot be guarded: %v", dir, err)
	}
	if len(defs) > 0 {
		return fmt.Errorf("%d hook(s) are declared in %s, but bd serve does not run them, so its writes could not be guarded; serve a store without hooks.d definitions", len(defs), dir)
	}
	if wide {
		return nil
	}
	accessor, ok := storage.UnwrapStore(st).(storage.RawDBAccessor)
	if !ok || accessor.DB() == nil {
		return nil
	}
	if sc := issueops.UnifiedScopeForTx(ctx, accessor.DB()); sc.Unified {
		return fmt.Errorf("this store (%s) is a namespace of the unified database, and bd serve reads every store's beads there, not only %s's; pass --wide to serve that every-store view", sc.Store, sc.Store)
	}
	return nil
}
