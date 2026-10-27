//go:build cgo

package main

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
)

func TestHierarchySpecificityCreate(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "hs")

	// Key unset: a child at priority 0 is today's behavior.
	unsetParent := bdCreate(t, bd, dir, "Unset parent", "-p", "0", "--type", "task")
	unsetChild := bdCreate(t, bd, dir, "Unset child", "-p", "0", "--type", "task", "--parent", unsetParent.ID)
	if unsetChild.Priority != 0 {
		t.Fatalf("unset child priority = %d, want 0", unsetChild.Priority)
	}

	bdConfig(t, bd, dir, "set", "hierarchy.priority", "specificity")
	if got := strings.TrimSpace(bdConfig(t, bd, dir, "get", "hierarchy.priority")); got != "specificity" {
		t.Fatalf("config get hierarchy.priority = %q", got)
	}

	parent0 := bdCreate(t, bd, dir, "P0 parent", "-p", "0", "--type", "task")
	sameLevel := bdCreateFail(t, bd, dir, "P0 child", "-p", "0", "--type", "task", "--parent", parent0.ID)
	wantChild := parent0.ID + ".1"
	if !strings.Contains(sameLevel, parent0.ID) || !strings.Contains(sameLevel, wantChild) || strings.Count(sameLevel, "priority 0") < 2 {
		t.Fatalf("0/0 error should name both ids and priority 0 twice:\n%s", sameLevel)
	}
	if hierarchyShowOK(bd, dir, wantChild) {
		t.Fatalf("rejected child %s was created", wantChild)
	}

	child1 := bdCreate(t, bd, dir, "P1 child", "-p", "1", "--type", "task", "--parent", parent0.ID)
	if child1.Priority != 1 {
		t.Fatalf("0/1 child priority = %d", child1.Priority)
	}

	parent0b := bdCreate(t, bd, dir, "P0 for skip", "-p", "0", "--type", "task")
	if child := bdCreate(t, bd, dir, "P4 skip", "-p", "4", "--type", "task", "--parent", parent0b.ID); child.Priority != 4 {
		t.Fatalf("0/4 child priority = %d", child.Priority)
	}

	parent4 := bdCreate(t, bd, dir, "P4 parent", "-p", "4", "--type", "task")
	if child := bdCreate(t, bd, dir, "P4 child", "-p", "4", "--type", "task", "--parent", parent4.ID); child.Priority != 4 {
		t.Fatalf("4/4 child priority = %d", child.Priority)
	}

	parent2 := bdCreate(t, bd, dir, "P2 parent", "-p", "2", "--type", "task")
	inverted := bdCreateFail(t, bd, dir, "P1 under P2", "-p", "1", "--type", "task", "--parent", parent2.ID)
	if !strings.Contains(inverted, parent2.ID) || !strings.Contains(inverted, parent2.ID+".1") ||
		!strings.Contains(inverted, "priority 1") || !strings.Contains(inverted, "priority 2") {
		t.Fatalf("2/1 error should name both ids and both numbers:\n%s", inverted)
	}
}

func TestHierarchySpecificityReport(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "hr")

	sameParent := bdCreate(t, bd, dir, "Same parent", "-p", "2", "--type", "task")
	sameChild := bdCreate(t, bd, dir, "Same child", "-p", "2", "--type", "task", "--parent", sameParent.ID)
	invParent := bdCreate(t, bd, dir, "Inverted parent", "-p", "3", "--type", "task")
	invChild := bdCreate(t, bd, dir, "Inverted child", "-p", "1", "--type", "task", "--parent", invParent.ID)
	legalParent := bdCreate(t, bd, dir, "Legal parent", "-p", "0", "--type", "task")
	legalChild := bdCreate(t, bd, dir, "Legal child", "-p", "4", "--type", "task", "--parent", legalParent.ID)

	off, offCode := bdLint(t, bd, dir, "hierarchy")
	if offCode != 0 {
		t.Fatalf("gate off exit = %d, want 0\n%s", offCode, off)
	}
	if strings.Contains(off, sameChild.ID) || strings.Contains(off, invChild.ID) || strings.Contains(off, "Issue not found: hierarchy") {
		t.Fatalf("gate off should be quiet:\n%s", off)
	}

	bdConfig(t, bd, dir, "set", "hierarchy.priority", "specificity")

	before := hierarchyIssueCount(t, beadsDir)
	out, code := bdLint(t, bd, dir, "hierarchy")
	after := hierarchyIssueCount(t, beadsDir)
	if after != before {
		t.Fatalf("issue count changed: before %d after %d", before, after)
	}
	if code == 0 {
		t.Fatalf("report should exit non-zero:\n%s", out)
	}
	if !strings.Contains(out, sameChild.ID) || !strings.Contains(out, sameParent.ID) || !strings.Contains(out, "priority 2") {
		t.Fatalf("report missing same-level edge:\n%s", out)
	}
	if !strings.Contains(out, invChild.ID) || !strings.Contains(out, invParent.ID) ||
		!strings.Contains(out, "priority 1") || !strings.Contains(out, "priority 3") {
		t.Fatalf("report missing inverted edge:\n%s", out)
	}
	if strings.Contains(out, legalChild.ID) {
		t.Fatalf("report included legal child %s:\n%s", legalChild.ID, out)
	}

	lines := hierarchyReportLines(out)
	if len(lines) != 2 {
		t.Fatalf("report lines = %d, want 2:\n%s", len(lines), out)
	}
	if hierarchyLineParent(lines[0]) > hierarchyLineParent(lines[1]) {
		t.Fatalf("report not sorted by parent id:\n%s", out)
	}
}

func TestHierarchySpecificityParentChange(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "hp")

	unsetPlan := writeHierarchyPlan(t, dir, "unset-graph.json", `{
		"nodes": [
			{"key": "parent", "title": "Unset graph parent", "priority": 2},
			{"key": "child", "title": "Unset graph child", "priority": 1, "parent_key": "parent"}
		]
	}`)
	bdCreateGraph(t, bd, dir, unsetPlan)

	bdConfig(t, bd, dir, "set", "hierarchy.priority", "specificity")

	parentKeyPlan := writeHierarchyPlan(t, dir, "parent-key-graph.json", `{
		"nodes": [
			{"key": "parent", "title": "Graph parent", "priority": 2},
			{"key": "child", "title": "Graph child", "priority": 1, "parent_key": "parent"}
		]
	}`)
	before := hierarchyIssueCount(t, beadsDir)
	parentKeyOut := bdCreateGraphFail(t, bd, dir, parentKeyPlan)
	after := hierarchyIssueCount(t, beadsDir)
	if after != before {
		t.Fatalf("failed graph changed issue count: before %d after %d", before, after)
	}
	if !strings.Contains(parentKeyOut, "priority 1") || !strings.Contains(parentKeyOut, "priority 2") {
		t.Fatalf("graph parent-key error should name both numbers:\n%s", parentKeyOut)
	}

	edgePlan := writeHierarchyPlan(t, dir, "edge-graph.json", `{
		"nodes": [
			{"key": "parent", "title": "Edge parent", "priority": 2},
			{"key": "child", "title": "Edge child", "priority": 1}
		],
		"edges": [{"from_key": "child", "to_key": "parent", "type": "parent-child"}]
	}`)
	before = hierarchyIssueCount(t, beadsDir)
	edgeOut := bdCreateGraphFail(t, bd, dir, edgePlan)
	after = hierarchyIssueCount(t, beadsDir)
	if after != before {
		t.Fatalf("failed edge graph changed issue count: before %d after %d", before, after)
	}
	if !strings.Contains(edgeOut, "priority 1") || !strings.Contains(edgeOut, "priority 2") {
		t.Fatalf("graph edge error should name both numbers:\n%s", edgeOut)
	}

	broad := bdCreate(t, bd, dir, "Broad", "-p", "0", "--type", "task")
	mid := bdCreate(t, bd, dir, "Mid", "-p", "2", "--type", "task")
	leaf := bdCreate(t, bd, dir, "Leaf", "-p", "3", "--type", "task")
	bdUpdate(t, bd, dir, leaf.ID, "--parent", broad.ID)
	if got := hierarchyPriorityOf(t, beadsDir, leaf.ID); got != 3 {
		t.Fatalf("legal reparent changed priority to %d", got)
	}
	if got := hierarchyParentOf(t, beadsDir, leaf.ID); got != broad.ID {
		t.Fatalf("legal reparent parent = %q, want %s", got, broad.ID)
	}

	rejected := bdUpdateFail(t, bd, dir, leaf.ID, "--parent", mid.ID, "--priority", "1")
	if !strings.Contains(rejected, leaf.ID) || !strings.Contains(rejected, mid.ID) ||
		!strings.Contains(rejected, "priority 1") || !strings.Contains(rejected, "priority 2") {
		t.Fatalf("reparent error should name both ids and both numbers:\n%s", rejected)
	}
	if got := hierarchyPriorityOf(t, beadsDir, leaf.ID); got != 3 {
		t.Fatalf("rejected reparent wrote priority %d", got)
	}
	if got := hierarchyParentOf(t, beadsDir, leaf.ID); got != broad.ID {
		t.Fatalf("rejected reparent parent = %q, want %s", got, broad.ID)
	}

	specific := bdCreate(t, bd, dir, "Specific enough", "-p", "1", "--type", "task")
	depFail := bdDepFail(t, bd, dir, "add", specific.ID, mid.ID, "--type", "parent-child")
	if !strings.Contains(depFail, specific.ID) || !strings.Contains(depFail, mid.ID) ||
		!strings.Contains(depFail, "priority 1") || !strings.Contains(depFail, "priority 2") {
		t.Fatalf("dep add error should name both ids and both numbers:\n%s", depFail)
	}
	bdDep(t, bd, dir, "add", specific.ID, broad.ID, "--type", "parent-child")
}

func hierarchyShowOK(bd, dir, id string) bool {
	cmd := exec.Command(bd, "show", id)
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	return cmd.Run() == nil
}

func hierarchyOpen(t *testing.T, beadsDir string) (*sql.DB, func() error) {
	t.Helper()
	cfg, err := configfile.Load(beadsDir)
	if err != nil || cfg == nil || cfg.GetDoltDatabase() == "" {
		t.Fatalf("load database name: %v", err)
	}
	dataDir := filepath.Join(beadsDir, "embeddeddolt")
	db, cleanup, err := embeddeddolt.OpenSQL(t.Context(), dataDir, cfg.GetDoltDatabase(), "main")
	if err != nil {
		t.Fatalf("OpenSQL: %v", err)
	}
	return db, cleanup
}

func hierarchyIssueCount(t *testing.T, beadsDir string) int {
	t.Helper()
	db, cleanup := hierarchyOpen(t, beadsDir)
	defer func() { _ = cleanup() }()
	var n int
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM issues").Scan(&n); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	return n
}

func hierarchyPriorityOf(t *testing.T, beadsDir, id string) int {
	t.Helper()
	db, cleanup := hierarchyOpen(t, beadsDir)
	defer func() { _ = cleanup() }()
	var n int
	if err := db.QueryRowContext(t.Context(), "SELECT priority FROM issues WHERE id = ?", id).Scan(&n); err != nil {
		t.Fatalf("priority of %s: %v", id, err)
	}
	return n
}

func hierarchyParentOf(t *testing.T, beadsDir, id string) string {
	t.Helper()
	db, cleanup := hierarchyOpen(t, beadsDir)
	defer func() { _ = cleanup() }()
	var parent string
	err := db.QueryRowContext(t.Context(), `
		SELECT COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external, '')
		FROM dependencies
		WHERE issue_id = ? AND type = 'parent-child'
	`, id).Scan(&parent)
	if err != nil {
		t.Fatalf("parent of %s: %v", id, err)
	}
	return parent
}

func hierarchyReportLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "child ") {
			lines = append(lines, line)
		}
	}
	return lines
}

func hierarchyLineParent(line string) string {
	const marker = " parent "
	i := strings.Index(line, marker)
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(line[i+len(marker):])
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func writeHierarchyPlan(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	return path
}

func bdCreateGraphFail(t *testing.T, bd, dir, planFile string) string {
	t.Helper()
	cmd := exec.Command(bd, "create", "--graph", planFile)
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("graph create should have failed:\n%s", out)
	}
	return string(out)
}
