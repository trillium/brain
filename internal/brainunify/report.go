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
	if _, err := fmt.Fprintf(w, "participating sources: %d\nsource rows read:  %d\ndistinct beads:     %d\nduplicated ids:     %d (losing copies: %d)\n\n",
		len(plan.Sources), plan.TotalBeads(), plan.DistinctBeads(), len(plan.Collisions), plan.DuplicateCopies()); err != nil {
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
	if _, err := fmt.Fprintln(w); err != nil {
		return err
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
		divergent := plan.DivergentCollisions()
		if _, err := fmt.Fprintf(w, "\nCOLLISIONS (%d ids exist in more than one database; %d differ in any column, %d differ in content)\n",
			len(plan.Collisions), len(divergent), len(plan.DataLossCollisions())); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%-20s %-14s %-20s %-20s %-26s %-18s %s\n", "ID", "PREFIX", "WINNER", "LOSERS", "REASON", "DIFFERS", "DIFFERING COLUMNS"); err != nil {
			return err
		}
		for _, c := range plan.Collisions {
			mark := "no"
			if c.Divergent {
				mark = "yes"
			}
			if _, err := fmt.Fprintf(w, "%-20s %-14s %-20s %-20s %-26s %-18s %s\n",
				c.ID, c.Prefix, c.Winner, strings.Join(c.Losers, ","), c.Reason, mark,
				strings.Join(c.DifferingColumns, ",")); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w, "\nEvery losing copy is preserved in full in brain_unify_collisions.losing_row."); err != nil {
			return err
		}
	}

	if reason, blocked := plan.Blocks(); blocked {
		if _, err := fmt.Fprintf(w, "\nBUILD BLOCKED: %s\n", reason); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, "A build stops here unless --allow-collisions is passed. Review the differing columns above first: keeping the winner's copy discards the other copy's values, which are preserved in full in brain_unify_collisions.losing_row."); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintln(w, "\nNo blocking condition: no duplicated id has copies that disagree on content."); err != nil {
			return err
		}
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
	_, err := fmt.Fprintf(w, "\ntotal rows skipped: %d (all recorded in brain_unify_collisions)\n", skipped)
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
	if _, err := fmt.Fprintf(w, "brain unify verification\n=======================\n\nchecks run:  %d\npassed:      %d\nfailed:      %d\nnamespaces:  %d\ncollisions confirmed: %d\n\nreference: %s\n\n",
		len(res.Checks), passed, failed, res.NamespacesChecked, res.CollisionsConfirmed, referenceDescription(res.Reference)); err != nil {
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

// WriteReplay renders what a replay did.
func WriteReplay(w io.Writer, res ReplayResult) error {
	if _, err := fmt.Fprintf(w, "brain unify replay\n==================\n\ndatabase: %s\ndata dir: %s\nelapsed: %s\nstores with changes in their history: %s\nbeads reconciled: %d\n\n",
		res.Database, res.DataDir, res.Elapsed.Round(1e6), joinOrNone(res.ChangedStores), len(res.Beads)); err != nil {
		return err
	}
	if len(res.Tables) > 0 {
		if _, err := fmt.Fprintf(w, "%-34s %9s %9s %9s %s\n", "TABLE", "DELETED", "INSERTED", "SKIPPED", ""); err != nil {
			return err
		}
		for _, t := range res.Tables {
			note := ""
			if t.Full {
				note = "(reloaded whole: the sources keep no history for this table)"
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
	_, err := fmt.Fprintf(w, "\nrun 'unify verify --reference live' to prove the merged database now equals the sources\n")
	return err
}

func joinOrNone(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}
