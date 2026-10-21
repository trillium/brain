package storeregistry

import "fmt"

// New builds an active store entry on the default data/<name>/.beads path.
func New(name string) Entry {
	return Entry{Name: name, Kind: KindStore, Store: name, State: StateActive,
		RelPath: "data/" + name + "/.beads", WrapperKind: WrapperServer}
}

// PlanCreate adds a store. Re-creating an active identical store is a no-op
// (the verb is idempotent); a name held by another active entry is refused.
func PlanCreate(e Entry) PlanFunc {
	return func(cur map[string]Entry) ([]Change, error) {
		if old, ok := cur[e.Name]; ok {
			if old.State == StateActive {
				if old.Kind == e.Kind && old.Store == e.Store && old.RelPath == e.RelPath && old.WrapperKind == e.WrapperKind {
					return nil, nil
				}
				return nil, fmt.Errorf("%q is already registered as an active %s of %q", e.Name, old.Kind, old.Store)
			}
			// A tombstone: creating the name again revives it.
			e.About, e.EditBack = old.About, old.EditBack
		}
		return []Change{{Op: OpCreate, Entry: e}}, nil
	}
}

// PlanRemove tombstones a store and every alias that points at it. Data and
// database rows are never touched.
func PlanRemove(name string) PlanFunc {
	return func(cur map[string]Entry) ([]Change, error) {
		e, ok := cur[name]
		if !ok || e.State != StateActive {
			return nil, fmt.Errorf("%q is not an active registry entry", name)
		}
		var out []Change
		e.State = StateRemoved
		out = append(out, Change{Op: OpRemove, Entry: e})
		if e.Kind == KindStore {
			for _, a := range cur {
				if a.Kind == KindAlias && a.State == StateActive && a.Store == e.Store {
					a.State = StateRemoved
					out = append(out, Change{Op: OpRemove, Entry: a, Detail: `{"cascade":"alias of ` + name + `"}`})
				}
			}
		}
		sortChanges(out)
		return out, nil
	}
}

// PlanRename tombstones old and activates new. The canonical store (its
// namespace and prefix) stays the old name; new is a store entry that resolves
// to it through brain_store_aliases. keepOld leaves the old name working as an
// alias entry instead of a tombstone. relPath is the new HOME-relative dir.
func PlanRename(oldName, newName, relPath string, keepOld bool) PlanFunc {
	return func(cur map[string]Entry) ([]Change, error) {
		old, ok := cur[oldName]
		if !ok || old.State != StateActive || old.Kind != KindStore {
			return nil, fmt.Errorf("%q is not an active store", oldName)
		}
		if ex, ok := cur[newName]; ok && ex.State == StateActive {
			return nil, fmt.Errorf("%q already exists in the registry", newName)
		}
		canonical := old.Store
		n := old
		n.Name, n.Store, n.RelPath, n.WrapperKind, n.State = newName, canonical, relPath, WrapperServer, StateActive
		n.CreatedEpoch, n.ChangedEpoch = 0, 0
		if ex, ok := cur[newName]; ok {
			n.CreatedEpoch = ex.CreatedEpoch
		}
		o := old
		if keepOld {
			o.Kind, o.Store, o.RelPath = KindAlias, canonical, relPath
		} else {
			o.State = StateRemoved
		}
		// Aliases of the old name follow the store to its new path.
		out := []Change{{Op: OpRename, Entry: o, OldName: oldName}, {Op: OpRename, Entry: n, OldName: oldName}}
		for _, a := range cur {
			if a.Kind == KindAlias && a.State == StateActive && a.Store == canonical && a.Name != newName && a.Name != oldName {
				a.RelPath = relPath
				out = append(out, Change{Op: OpAlias, Entry: a})
			}
		}
		sortChanges(out[2:])
		return out, nil
	}
}

// PlanAlias adds a second command name for an existing active store.
func PlanAlias(target, alias string) PlanFunc {
	return func(cur map[string]Entry) ([]Change, error) {
		t, ok := cur[target]
		if !ok || t.State != StateActive {
			return nil, fmt.Errorf("%q is not an active registry entry", target)
		}
		if ex, ok := cur[alias]; ok && ex.State == StateActive {
			if ex.Kind == KindAlias && ex.Store == t.Store && ex.RelPath == t.RelPath {
				return nil, nil
			}
			return nil, fmt.Errorf("%q already exists in the registry", alias)
		}
		a := Entry{Name: alias, Kind: KindAlias, Store: t.Store, State: StateActive,
			RelPath: t.RelPath, WrapperKind: t.WrapperKind}
		return []Change{{Op: OpAlias, Entry: a}}, nil
	}
}

// PlanAbout sets the about blurb of an active store.
func PlanAbout(name, about string) PlanFunc {
	return func(cur map[string]Entry) ([]Change, error) {
		e, ok := cur[name]
		if !ok || e.State != StateActive {
			return nil, fmt.Errorf("%q is not an active registry entry", name)
		}
		if e.About == about {
			return nil, nil
		}
		e.About = about
		return []Change{{Op: OpAbout, Entry: e}}, nil
	}
}

// PlanEditBack sets the edit-back declaration of an active store.
func PlanEditBack(name string, on bool) PlanFunc {
	return func(cur map[string]Entry) ([]Change, error) {
		e, ok := cur[name]
		if !ok || e.State != StateActive {
			return nil, fmt.Errorf("%q is not an active registry entry", name)
		}
		if e.EditBack == on {
			return nil, nil
		}
		e.EditBack = on
		return []Change{{Op: OpEditBack, Entry: e}}, nil
	}
}

// PlanBackfill adds entries that are not yet in the registry and leaves every
// existing row alone, so it is safe to run twice and from several machines.
func PlanBackfill(entries []Entry) PlanFunc {
	return func(cur map[string]Entry) ([]Change, error) {
		var out []Change
		for _, e := range entries {
			if _, ok := cur[e.Name]; ok {
				continue
			}
			out = append(out, Change{Op: OpBackfill, Entry: e})
		}
		sortChanges(out)
		return out, nil
	}
}

func sortChanges(c []Change) {
	for i := 1; i < len(c); i++ {
		for j := i; j > 0 && c[j].Entry.Name < c[j-1].Entry.Name; j-- {
			c[j], c[j-1] = c[j-1], c[j]
		}
	}
}
