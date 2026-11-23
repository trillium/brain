package main

import (
	"fmt"

	"github.com/steveyegge/beads/internal/hooksdef"
)

// brainServeGate decides whether bd serve may serve this brain store.
//
// On a Dolt SQL server bd serve answers from a unit-of-work provider that sits
// beneath the store. Namespace scoping now lives in that provider's issue
// repository (issueops.Scope*Filter), so a served unified database answers for
// the wrapper's own namespace by default and for every store under --wide, the
// same as the CLI. What the provider still does not run is the hooks.d guards
// and observers, so declared hooks refuse the server outright, as they refuse
// writes in proxied-server mode: a guard that silently stops guarding is worse
// than no server. (Writes also render no markdown; render-all and the host
// render job catch up.)
func brainServeGate(beadsDir string) error {
	dir := hooksdef.HooksDir(beadsDir)
	defs, err := hooksdef.LoadAll(dir)
	if err != nil {
		return fmt.Errorf("hook definitions in %s are unusable, so writes cannot be guarded: %v", dir, err)
	}
	if len(defs) > 0 {
		return fmt.Errorf("%d hook(s) are declared in %s, but bd serve does not run them, so its writes could not be guarded; serve a store without hooks.d definitions", len(defs), dir)
	}
	return nil
}
