package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// treePaths returns every visible command path in the live tree, using the same
// visibility rule the report uses, so a mismatch is a real coverage bug.
func treePaths(root *cobra.Command) map[string]bool {
	out := map[string]bool{}
	var walk func(cmd *cobra.Command, prefix string)
	walk = func(cmd *cobra.Command, prefix string) {
		path := cmd.Name()
		if prefix != "" {
			path = prefix + " " + cmd.Name()
		}
		out[path] = true
		for _, sub := range visibleChildren(cmd) {
			walk(sub, path)
		}
	}
	for _, top := range root.Commands() {
		if top.IsAvailableCommand() {
			walk(top, "")
		}
	}
	return out
}

func TestRemoteCommandsCoversWholeRegistry(t *testing.T) {
	remoteEnv(t)
	rows := collectRemoteCommandRows(true)

	got := map[string]int{}
	for _, r := range rows {
		got[r.Path]++
		switch r.Category {
		case remoteAppliesClient, remoteAppliesLocal, remoteAppliesHost:
		default:
			t.Errorf("%s: unknown category %q", r.Path, r.Category)
		}
	}
	want := treePaths(rootCmd)
	for path := range want {
		if got[path] != 1 {
			t.Errorf("command %q appears %d time(s) in the report, want exactly 1", path, got[path])
		}
	}
	for path := range got {
		if !want[path] {
			t.Errorf("report row %q is not a visible command in the tree", path)
		}
	}

	// Spot-check the three classes against the namespaces they come from.
	byPath := map[string]remoteCommandRow{}
	for _, r := range rows {
		byPath[r.Path] = r
	}
	for path, wantCat := range map[string]string{
		"create":          remoteAppliesClient,
		"brain":           remoteAppliesClient,
		"brain new":       remoteAppliesClient,
		"brain stores":    remoteAppliesClient,
		"remote":          remoteAppliesLocal,
		"remote set":      remoteAppliesLocal,
		"remote commands": remoteAppliesLocal,
		"host":            remoteAppliesHost,
		"host promote":    remoteAppliesHost,
		"host clients":    remoteAppliesHost,
	} {
		if r, ok := byPath[path]; !ok || r.Category != wantCat {
			t.Errorf("%q: category = %q (found=%v), want %q", path, r.Category, ok, wantCat)
		}
	}

	// The default (top-level) view must be a prefix of the full tree.
	for _, r := range collectRemoteCommandRows(false) {
		if strings.Contains(r.Path, " ") {
			t.Errorf("top-level report contains nested command %q", r.Path)
		}
		if !want[r.Path] {
			t.Errorf("top-level row %q missing from the full tree", r.Path)
		}
	}
}

// TestRemoteCommandsDriftAgainstForwarder is the drift guard: the report is
// derived from annotations, and the forwarder decides by name. If a new
// namespace is added to either side without the other, this fails.
func TestRemoteCommandsDriftAgainstForwarder(t *testing.T) {
	remoteEnv(t)
	if err := saveRemoteConfig(&remoteConfig{Host: "me@host"}); err != nil {
		t.Fatal(err)
	}
	for _, r := range collectRemoteCommandRows(true) {
		args := strings.Fields(r.Path)
		_, _, active, err := remoteForwardingActive(args)
		if err != nil {
			t.Fatalf("remoteForwardingActive(%v): %v", args, err)
		}
		if active != r.Forwarded {
			t.Errorf("%s: report says forwarded=%v but the forwarder says active=%v; annotate the command or teach the report",
				r.Path, r.Forwarded, active)
		}
	}
}

func TestRemoteCommandsAvailabilityFromClient(t *testing.T) {
	remoteEnv(t)
	if err := saveRemoteConfig(&remoteConfig{Host: "me@host"}); err != nil {
		t.Fatal(err)
	}
	rep, err := buildRemoteCommandsReport("", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Mode != "client" || rep.Host != "me@host" {
		t.Fatalf("mode/host = %q/%q, want client/me@host", rep.Mode, rep.Host)
	}
	sawHost, sawLocal, sawClient := false, false, false
	for _, r := range rep.Commands {
		switch r.Category {
		case remoteAppliesHost:
			sawHost = true
			if r.Available {
				t.Errorf("%s: host-only command reported available from a client", r.Path)
			}
			if r.Why == "" || r.Fix == "" {
				t.Errorf("%s: unavailable command has no why/fix", r.Path)
			}
			if !strings.Contains(r.Fix, "ssh me@host") {
				t.Errorf("%s: fix should name the host: %q", r.Path, r.Fix)
			}
		case remoteAppliesLocal:
			sawLocal = true
			if !r.Available || r.Forwarded {
				t.Errorf("%s: local command available=%v forwarded=%v, want true/false", r.Path, r.Available, r.Forwarded)
			}
		case remoteAppliesClient:
			sawClient = true
			if !r.Available || !r.Forwarded {
				t.Errorf("%s: client command available=%v forwarded=%v, want true/true", r.Path, r.Available, r.Forwarded)
			}
			if !strings.Contains(r.RunsOn, "me@host") {
				t.Errorf("%s: runs_on should name the host: %q", r.Path, r.RunsOn)
			}
		}
	}
	if !sawHost || !sawLocal || !sawClient {
		t.Fatalf("missing a category: host=%v local=%v client=%v", sawHost, sawLocal, sawClient)
	}
	if rep.Totals["unavailable"] != rep.Totals[remoteAppliesHost] {
		t.Errorf("unavailable = %d, host-only = %d; every host-only command should be unavailable here",
			rep.Totals["unavailable"], rep.Totals[remoteAppliesHost])
	}
}

func TestRemoteCommandsOnTheHostItself(t *testing.T) {
	home, _ := remoteEnv(t)
	if err := saveHostConfig(home, &hostConfig{Role: roleHost, Generation: 1, Since: nowStamp()}); err != nil {
		t.Fatal(err)
	}
	rep, err := buildRemoteCommandsReport("", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Mode != "host" {
		t.Fatalf("mode = %q, want host", rep.Mode)
	}
	for _, r := range rep.Commands {
		if r.Category == remoteAppliesHost && !r.Available {
			t.Errorf("%s: host-only command unavailable on the host itself", r.Path)
		}
		if r.Category == remoteAppliesClient && !strings.Contains(r.RunsOn, "no brain host configured") {
			t.Errorf("%s: local run should say so: %q", r.Path, r.RunsOn)
		}
	}
}

func TestRemoteCommandsClientRegistration(t *testing.T) {
	home, _ := remoteEnv(t)
	if err := saveHostConfig(home, &hostConfig{
		Role: roleHost, Generation: 1, Since: nowStamp(),
		Clients: []hostClient{{Target: "me@laptop", Added: nowStamp()}},
	}); err != nil {
		t.Fatal(err)
	}
	known, err := buildRemoteCommandsReport("me@laptop", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if known.ClientRegistered == nil || !*known.ClientRegistered {
		t.Fatalf("me@laptop should be reported registered: %+v", known.ClientRegistered)
	}
	if known.Mode != "client" {
		t.Errorf("mode = %q, want client when a client is named", known.Mode)
	}

	unknown, err := buildRemoteCommandsReport("me@other", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if unknown.ClientRegistered == nil || *unknown.ClientRegistered {
		t.Fatalf("me@other should be reported unregistered: %+v", unknown.ClientRegistered)
	}
	joined := strings.Join(unknown.Notes, "\n")
	if !strings.Contains(joined, "brain host clients add me@other") {
		t.Errorf("unregistered client should carry the fix hint, notes: %v", unknown.Notes)
	}
	for _, r := range unknown.Commands {
		if r.Category == remoteAppliesHost {
			if r.Available {
				t.Errorf("%s: host-only command must be unavailable to a named client", r.Path)
			}
			if !strings.Contains(r.Fix, "this host (the brain host)") {
				t.Errorf("%s: describing a client from the host should point at this host: %q", r.Path, r.Fix)
			}
		}
	}

	if _, err := buildRemoteCommandsReport("bad host", false, "", ""); err == nil {
		t.Error("an invalid client target must be rejected")
	}
}

func TestRemoteCommandsJSONShape(t *testing.T) {
	remoteEnv(t)
	if err := saveRemoteConfig(&remoteConfig{Host: "me@host"}); err != nil {
		t.Fatal(err)
	}
	rep, err := buildRemoteCommandsReport("", true, "promote", "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"mode", "host", "find", "commands", "totals"} {
		if _, ok := m[key]; !ok {
			t.Errorf("JSON missing %q: %s", key, raw)
		}
	}
	if m["find"] != "promote" {
		t.Errorf("find = %v, want \"promote\"", m["find"])
	}
	cmds, ok := m["commands"].([]any)
	if !ok || len(cmds) == 0 {
		t.Fatalf("commands is not a non-empty array: %s", raw)
	}
	first, _ := cmds[0].(map[string]any)
	for _, key := range []string{"path", "name", "category", "runs_on", "forwarded", "available", "summary"} {
		if _, ok := first[key]; !ok {
			t.Errorf("command row missing %q: %v", key, first)
		}
	}
}

func TestRemoteCommandsHumanOutput(t *testing.T) {
	remoteEnv(t)
	if err := saveRemoteConfig(&remoteConfig{Host: "me@host"}); err != nil {
		t.Fatal(err)
	}
	rep, err := buildRemoteCommandsReport("", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	printRemoteCommandsReport(&buf, rep)
	out := buf.String()
	for _, want := range []string{
		"brain remote commands",
		"mode:    client of me@host",
		"Runs on the brain host",
		"Runs on this machine (client-mode configuration)",
		"Host-only",
		"remote set",
		"host promote",
		"ssh me@host brain host",
		"Totals:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("human output missing %q:\n%s", want, out)
		}
	}
}

func TestRemoteCommandsIsRegisteredAsAClientVerb(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"remote", "commands"})
	if err != nil || cmd.Name() != "commands" {
		t.Fatalf("brain remote commands not found: %v", err)
	}
	if !isBrainRemoteVerb(cmd) {
		t.Error("'brain remote commands' must be a brain remote verb (it must never be forwarded)")
	}
	if remoteCommandCategory(cmd) != remoteAppliesLocal {
		t.Errorf("category = %q, want local", remoteCommandCategory(cmd))
	}
}

// TestRemoteCommandsFindFilters is the --find contract: it narrows the report to
// matching commands, keeps their why/fix, and reports a clear note when nothing
// matches. It also checks the query is echoed for scripts.
func TestRemoteCommandsFindFilters(t *testing.T) {
	remoteEnv(t)
	if err := saveRemoteConfig(&remoteConfig{Host: "me@host"}); err != nil {
		t.Fatal(err)
	}
	rep, err := buildRemoteCommandsReport("", true, "promote", "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Find != "promote" {
		t.Errorf("find = %q, want promote", rep.Find)
	}
	if len(rep.Commands) == 0 {
		t.Fatal("find should match at least 'host promote'")
	}
	sawHostOnly := false
	for _, r := range rep.Commands {
		if !remoteCommandRowMatches(r, "promote") {
			t.Errorf("row %q does not match 'promote'", r.Path)
		}
		if r.Category == remoteAppliesHost {
			sawHostOnly = true
			if r.Why == "" || r.Fix == "" {
				t.Errorf("%s: filtered host-only row lost why/fix", r.Path)
			}
		}
	}
	if !sawHostOnly {
		t.Error("expected at least one host-only command matching 'promote'")
	}

	none, err := buildRemoteCommandsReport("", true, "zzzz-no-such-command", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(none.Commands) != 0 {
		t.Errorf("no-match should yield no commands, got %d", len(none.Commands))
	}
	for _, cat := range []string{remoteAppliesClient, remoteAppliesLocal, remoteAppliesHost} {
		if none.Totals[cat] != 0 {
			t.Errorf("no-match %s total = %d, want 0", cat, none.Totals[cat])
		}
	}
	if !strings.Contains(strings.Join(none.Notes, "\n"), "no commands match") {
		t.Errorf("no-match report should carry a note: %v", none.Notes)
	}
}

// TestRemoteCommandsFindMatchesAliasesCaseInsensitive checks that --find sees
// the alias and the summary, not just the canonical path.
func TestRemoteCommandsFindMatchesAliasesCaseInsensitive(t *testing.T) {
	remoteEnv(t)
	rows := filterRemoteCommandRows(collectRemoteCommandRows(true), "NEW")
	var paths []string
	for _, r := range rows {
		paths = append(paths, r.Path)
	}
	found := false
	for _, r := range rows {
		if r.Path == "create" {
			found = true
		}
	}
	if !found {
		t.Errorf("find 'NEW' should match 'create' via its alias; got %v", paths)
	}
}

func TestRemoteCommandsHumanOutputShowsFind(t *testing.T) {
	remoteEnv(t)
	rep, err := buildRemoteCommandsReport("", true, "promote", "")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	printRemoteCommandsReport(&buf, rep)
	if !strings.Contains(buf.String(), "find:    promote") {
		t.Errorf("human output should echo the find term:\n%s", buf.String())
	}
}

// TestRemoteCommandsCategoryFilter is the --category contract: it narrows the
// report to one group (client | local | host), keeps each row's why/fix, keeps
// the totals consistent with the listed rows, echoes the choice for scripts,
// and rejects unknown values.
func TestRemoteCommandsCategoryFilter(t *testing.T) {
	remoteEnv(t)
	if err := saveRemoteConfig(&remoteConfig{Host: "me@host"}); err != nil {
		t.Fatal(err)
	}
	full, err := buildRemoteCommandsReport("", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, cat := range []string{remoteAppliesClient, remoteAppliesLocal, remoteAppliesHost} {
		rep, err := buildRemoteCommandsReport("", true, "", cat)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Category != cat {
			t.Errorf("category = %q, want %q", rep.Category, cat)
		}
		var want int
		for _, r := range full.Commands {
			if r.Category == cat {
				want++
			}
		}
		if len(rep.Commands) != want {
			t.Errorf("--category %s: %d rows, want %d", cat, len(rep.Commands), want)
		}
		for _, r := range rep.Commands {
			if r.Category != cat {
				t.Errorf("--category %s: row %q has category %q", cat, r.Path, r.Category)
			}
		}
		if rep.Totals[cat] != want {
			t.Errorf("--category %s: totals[%s] = %d, want %d", cat, cat, rep.Totals[cat], want)
		}
		for _, other := range []string{remoteAppliesClient, remoteAppliesLocal, remoteAppliesHost} {
			if other != cat && rep.Totals[other] != 0 {
				t.Errorf("--category %s: totals[%s] = %d, want 0", cat, other, rep.Totals[other])
			}
		}
	}

	// Host-only rows keep their why/fix under the filter.
	host, err := buildRemoteCommandsReport("", true, "", remoteAppliesHost)
	if err != nil {
		t.Fatal(err)
	}
	if len(host.Commands) == 0 {
		t.Fatal("expected host-only commands")
	}
	for _, r := range host.Commands {
		if r.Why == "" || r.Fix == "" {
			t.Errorf("%s: filtered host-only row lost why/fix", r.Path)
		}
	}

	// Combining --category with --find narrows further; no match notes it.
	none, err := buildRemoteCommandsReport("", true, "rollback", remoteAppliesLocal)
	if err != nil {
		t.Fatal(err)
	}
	if len(none.Commands) != 0 {
		t.Errorf("--category local --find rollback: %d rows, want 0", len(none.Commands))
	}
	if !strings.Contains(strings.Join(none.Notes, "\n"), "no commands match") {
		t.Errorf("empty category+find report should carry a note: %v", none.Notes)
	}

	if _, err := buildRemoteCommandsReport("", true, "", "bogus"); err == nil {
		t.Error("an unknown --category must be rejected")
	}
}

// TestRemoteCommandsJSONEchoesCategory checks the --category choice is part of
// the machine-readable report, like --find.
func TestRemoteCommandsJSONEchoesCategory(t *testing.T) {
	remoteEnv(t)
	rep, err := buildRemoteCommandsReport("", true, "", remoteAppliesHost)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["category"] != remoteAppliesHost {
		t.Errorf("category = %v, want %q", m["category"], remoteAppliesHost)
	}
}

func TestRemoteCommandsHumanOutputShowsCategory(t *testing.T) {
	remoteEnv(t)
	rep, err := buildRemoteCommandsReport("", true, "", remoteAppliesHost)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	printRemoteCommandsReport(&buf, rep)
	if !strings.Contains(buf.String(), "category: host") {
		t.Errorf("human output should echo the category:\n%s", buf.String())
	}
}
