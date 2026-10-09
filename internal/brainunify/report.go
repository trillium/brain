package brainunify

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// WritePlan renders the mapping plan as readable text. The output is stable
// for a given federation so two runs can be diffed, which is how a reviewer
// checks that nothing changed between the plan they approved and the build
// that ran.
func WritePlan(w io.Writer, plan Plan, disc Discovery) error {
	if _, err := fmt.Fprintf(w, "brain unify plan\n=================\n\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "participating sources: %d\nsource rows read:  %d\nbeads after merge:  %d\nduplicated ids:     %d (%d identical, merged into one bead: %d copies skipped; %d differ: conflict beads, %d minted copies)\n\n",
		len(plan.Sources), plan.TotalBeads(), plan.DistinctBeads(), len(plan.Collisions),
		len(plan.Collisions)-len(plan.Conflicts()), plan.DuplicateCopies(), len(plan.Conflicts()), plan.MintedCopies()); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(w, "SOURCES\n-------\n%-24s %-20s %-10s %8s  %s\n", "STORE", "SOURCE DATABASE", "REGISTRY", "BEADS", "DECLARED PREFIXES"); err != nil {
		return err
	}
	for _, s := range plan.Sources {
		reg := "no"
		if s.Registered {
			reg = "yes"
		}
		if _, err := fmt.Fprintf(w, "%-24s %-20s %-10s %8d  %s\n",
			s.Namespace, s.Database, reg, s.BeadCount, strings.Join(s.DeclaredPrefixes, ", ")); err != nil {
			return err
		}
	}
	for _, s := range plan.Sources {
		if len(s.SkipIDs) > 0 {
			if _, err := fmt.Fprintf(w, "%s is a replica: only its %d bead(s) no other store holds are carried; its other %d are copies of beads that live in their own stores and are left alone\n", s.Namespace, s.BeadCount, len(s.SkipIDs)); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

	if un := plan.Unidentified(); len(un) > 0 {
		if _, err := fmt.Fprintf(w, "NO PROJECT ID (a cutover cannot repoint a wrapper to these by identity; give one with --project-id <store>=<uuid>)\n--------------------------------------------------------------------------------------------\n%s\n\n", strings.Join(un, ", ")); err != nil {
			return err
		}
	}

	if len(plan.Excluded) > 0 {
		if _, err := fmt.Fprintf(w, "EXCLUDED (nothing is dropped without a line here)\n-------------------------------------\n"); err != nil {
			return err
		}
		for _, e := range plan.Excluded {
			if _, err := fmt.Fprintf(w, "%-20s %8d beads  %s\n", e.Database, e.Beads, e.Reason); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(w, "NAMESPACES (the store separation that survives unification)\n---------------------------------------------------\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%-20s %-22s %-24s %8s  %s\n", "PREFIX", "OWNER", "OWNER REASON", "BEADS", "OBSERVED IN"); err != nil {
		return err
	}
	for _, ns := range SortedNamespaces(plan.Namespaces) {
		marker := " "
		if ns.Ambiguous {
			marker = "*"
		}
		if _, err := fmt.Fprintf(w, "%s%-19s %-22s %-24s %8d  %s\n",
			marker, ns.Prefix, ns.Owner, ns.OwnerReason, ns.BeadCount, strings.Join(ns.ObservedBy, ", ")); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w, "\n* prefix observed in more than one source database"); err != nil {
		return err
	}

	ambiguous := []Namespace{}
	for _, ns := range SortedNamespaces(plan.Namespaces) {
		if ns.Ambiguous {
			ambiguous = append(ambiguous, ns)
		}
	}
	if len(ambiguous) > 0 {
		if _, err := fmt.Fprintf(w, "\nPREFIX SHARED BY MORE THAN ONE DATABASE (%d)\n", len(ambiguous)); err != nil {
			return err
		}
		for _, ns := range ambiguous {
			if _, err := fmt.Fprintf(w, "  %-18s owner=%-20s observed in %s\n", ns.Prefix, ns.Owner, strings.Join(ns.ObservedBy, ", ")); err != nil {
				return err
			}
		}
	}

	if len(plan.Collisions) > 0 {
		if _, err := fmt.Fprintf(w, "\nCOLLISIONS (%d ids exist in more than one database; %d differ in any column, %d differ in content)\n",
			len(plan.Collisions), len(plan.Conflicts()), len(plan.DataLossCollisions())); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%-20s %-14s %-13s %-44s %-26s %s\n", "ID", "PREFIX", "RESOLUTION", "KEPT AS", "REASON", "DIFFERING COLUMNS"); err != nil {
			return err
		}
		for _, c := range plan.Collisions {
			var kept string
			if c.Divergent {
				parts := make([]string, 0, len(c.Copies))
				for _, cp := range c.Copies {
					parts = append(parts, cp.Store+"="+cp.ID)
				}
				kept = "conflict bead + " + strings.Join(parts, " ")
			} else {
				kept = "winner " + c.Winner + ", skipped " + strings.Join(c.Losers, ",")
			}
			if _, err := fmt.Fprintf(w, "%-20s %-14s %-13s %-44s %-26s %s\n",
				c.ID, c.Prefix, c.Resolution, kept, c.Reason, strings.Join(c.DifferingColumns, ",")); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w, "\nIdentical copies merge into one bead; the skipped copy is preserved in full in brain_unify_collisions.losing_row.\nCopies that differ are all kept: each becomes a bead under a minted id with its own child rows, and the original id becomes an open conflict bead linking to them. Nothing is discarded."); err != nil {
			return err
		}
	}

	if err := plan.Refusal(); err != nil {
		if _, err := fmt.Fprintf(w, "\nBUILD REFUSED: %v\n", err); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintln(w, "\nNo blocking condition."); err != nil {
		return err
	}
	return nil
}

// WriteBuild renders what a build did.
func WriteBuild(w io.Writer, res BuildResult) error {
	if _, err := fmt.Fprintf(w, "brain unify build\n==================\n\ndatabase: %s\ndata dir: %s\nschema template: %s\nelapsed: %s\n\n",
		res.Database, res.DataDir, res.Template, res.Elapsed.Round(1e6)); err != nil {
		return err
	}
	byStore := map[string][]ImportStats{}
	for _, s := range res.Stats {
		byStore[s.Store] = append(byStore[s.Store], s)
	}
	stores := make([]string, 0, len(byStore))
	for s := range byStore {
		stores = append(stores, s)
	}
	sort.Strings(stores)

	if _, err := fmt.Fprintf(w, "%-24s %-30s %10s %10s %9s\n", "STORE", "TABLE", "SOURCE", "IMPORTED", "SKIPPED"); err != nil {
		return err
	}
	for _, store := range stores {
		for _, s := range byStore[store] {
			if _, err := fmt.Fprintf(w, "%-24s %-30s %10d %10d %9d\n", s.Store, s.Table, s.SourceRows, s.ImportedRows, s.SkippedRows); err != nil {
				return err
			}
		}
	}
	var skipped int64
	for _, s := range res.Stats {
		skipped += s.SkippedRows
	}
	if _, err := fmt.Fprintf(w, "\ntotal rows skipped: %d (the skipped copies of identical duplicates, all recorded in brain_unify_collisions)\n", skipped); err != nil {
		return err
	}
	conflicts := 0
	for _, c := range res.Collisions {
		if c.Divergent {
			conflicts++
		}
	}
	_, err := fmt.Fprintf(w, "conflict beads: %d (each links to its minted copies; see brain_unify_collisions.copy_ids)\n", conflicts)
	return err
}

// WriteVerify renders the mechanical comparison. Failures are printed in full
// and the count of passing checks is printed too, so the output says what was
// compared rather than only what passed.
func WriteVerify(w io.Writer, res VerifyResult) error {
	var passed, failed int
	for _, c := range res.Checks {
		if c.OK {
			passed++
			continue
		}
		failed++
	}
	if _, err := fmt.Fprintf(w, "brain unify verification\n=======================\n\nchecks run:  %d\npassed:      %d\nfailed:      %d\nnamespaces:  %d\ncollisions confirmed: %d (conflict beads among them: %d)\n\nreference: %s\n\n",
		len(res.Checks), passed, failed, res.NamespacesChecked, res.CollisionsConfirmed, res.ConflictsConfirmed, referenceDescription(res.Reference)); err != nil {
		return err
	}
	if failed > 0 {
		if _, err := fmt.Fprintf(w, "FAILURES\n--------\n%-22s %-34s %s\n", "SCOPE", "TABLE", "DIFFERENCE"); err != nil {
			return err
		}
		for _, c := range res.Failures() {
			if _, err := fmt.Fprintf(w, "%-22s %-34s %s\n", c.Scope, c.Table, c.Difference()); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintln(w, "\nRESULT: FAIL")
		return err
	}
	if _, err := fmt.Fprintf(w, "every source row, in every table, matched the unified database by count, size and content digest\n"); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "\nRESULT: PASS")
	return err
}

// referenceDescription says, in the report, which side the unified database
// was compared against.
func referenceDescription(ref string) string {
	if ref == ReferenceLive {
		return "the sources as they stand now, read live and compared against the unified database"
	}
	return "what the build recorded it read from each source, compared against the unified database"
}

// WriteReplay renders what a replay did, or, for a dry run, what it would do.
func WriteReplay(w io.Writer, res ReplayResult) error {
	title := "brain unify replay"
	if res.DryRun {
		title = "brain unify replay (dry run: nothing was applied)"
	}
	where := res.DataDir
	if where == "" {
		where = "(hosted by a running server)"
	}
	if _, err := fmt.Fprintf(w, "%s\n%s\n\ndatabase: %s\ndata dir: %s\nelapsed: %s\nstores with changes in their history: %s\nbeads %s: %d\n\n",
		title, strings.Repeat("=", len(title)), res.Database, where, res.Elapsed.Round(1e6), joinOrNone(res.ChangedStores),
		map[bool]string{true: "that would be reconciled", false: "reconciled"}[res.DryRun], len(res.Beads)); err != nil {
		return err
	}
	if res.DryRun {
		if len(res.TouchedByTable) > 0 {
			if _, err := fmt.Fprintf(w, "%-34s %s\n", "TABLE", "BEADS TO RELOAD"); err != nil {
				return err
			}
			for _, t := range sortedKeys(res.TouchedByTable) {
				if _, err := fmt.Fprintf(w, "%-34s %d\n", t, res.TouchedByTable[t]); err != nil {
					return err
				}
			}
		}
		if _, err := fmt.Fprintf(w, "tables to reload whole: %s\nstate tables to replace: %s\ncollision records to write: %s\ncollision records to remove: %s\n",
			joinOrNone(res.FullTables), joinStateChanged(res.StateChanged), joinOrNone(res.CollisionsWritten), joinOrNone(res.CollisionsRemoved)); err != nil {
			return err
		}
	} else {
		if len(res.Tables) > 0 {
			if _, err := fmt.Fprintf(w, "%-34s %9s %9s %9s %s\n", "TABLE", "DELETED", "INSERTED", "SKIPPED", ""); err != nil {
				return err
			}
			for _, t := range res.Tables {
				note := ""
				if t.Full {
					note = "(reloaded whole)"
				}
				if _, err := fmt.Fprintf(w, "%-34s %9d %9d %9d %s\n", t.Table, t.Deleted, t.Inserted, t.Skipped, note); err != nil {
					return err
				}
			}
		}
		if _, err := fmt.Fprintf(w, "\ncollision records written: %s\ncollision records removed: %s\n", joinOrNone(res.CollisionsWritten), joinOrNone(res.CollisionsRemoved)); err != nil {
			return err
		}
		for _, c := range res.Commits {
			if _, err := fmt.Fprintf(w, "next replay starts: %-24s %s\n", c.Store, c.Hash); err != nil {
				return err
			}
		}
	}
	if len(res.KeptMerged) > 0 {
		if _, err := fmt.Fprintf(w, "\nkept the merged version of %d bead(s) changed there after the repoint (the old databases' version is older): %s\n", len(res.KeptMerged), strings.Join(res.KeptMerged, ", ")); err != nil {
			return err
		}
	}
	if res.DryRun {
		_, err := fmt.Fprintf(w, "\n%s\n", readinessVerdict(res))
		return err
	}
	_, err := fmt.Fprintf(w, "\nrun 'unify verify --reference live' to prove the merged database now equals the sources\n")
	return err
}

func joinStateChanged(m map[string][]string) string {
	if len(m) == 0 {
		return "none"
	}
	var parts []string
	for _, st := range sortedKeys(m) {
		parts = append(parts, st+": "+strings.Join(m[st], ","))
	}
	return strings.Join(parts, "; ")
}

// Pending is the amount of work a dry-run result says a replay would do: zero
// means the merged database already holds everything the sources hold.
func (res ReplayResult) Pending() int {
	return len(res.Beads) + len(res.FullTables) + len(res.StateChanged) + len(res.CollisionsWritten) + len(res.CollisionsRemoved)
}

// readinessVerdict is the one-line answer of the cutover readiness check.
func readinessVerdict(res ReplayResult) string {
	if res.Pending() == 0 {
		return "READY: the merged database holds everything the sources hold (nothing to replay)"
	}
	return fmt.Sprintf("NOT YET IDENTICAL: a replay would reload %d bead(s), %d table(s) whole, state tables of %d store(s); run the replay again (sources that are written to are never identical for long; what matters is how small this is when you repoint)",
		len(res.Beads), len(res.FullTables), len(res.StateChanged))
}

func joinOrNone(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}
