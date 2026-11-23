package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// verifyHome builds a provisioned fake home: both registry files, and for each
// store a wrapper plus a beads dir whose config.yaml names its prefix.
func verifyHome(t *testing.T, stores map[string]string) string {
	t.Helper()
	home := t.TempDir()
	var reg strings.Builder
	reg.WriteString("stores:\n")
	for name, prefix := range stores {
		beads := filepath.Join(home, "data", name, ".beads")
		if err := os.MkdirAll(beads, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := "BD_NAME: " + name + "\n"
		if prefix != "" {
			cfg = "issue-prefix: " + prefix + "\n" + cfg
		}
		if err := os.WriteFile(filepath.Join(beads, "config.yaml"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		reg.WriteString("  " + name + ":\n    path: " + beads + "\n")
		writeVerifyWrapper(t, home, name)
	}
	for _, rel := range verifyHostRegistryPaths {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(reg.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func writeVerifyWrapper(t *testing.T, home, name string) {
	t.Helper()
	dir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeRun answers bd's version and each wrapper's probe. mint maps a wrapper's
// base name to the probe JSON it prints; record collects every call.
func fakeRun(version string, mint map[string]string, calls *[]string) verifyRunner {
	return func(ctx context.Context, name string, args ...string) (string, string, error) {
		*calls = append(*calls, name+" "+strings.Join(args, " "))
		if len(args) > 0 && args[0] == "version" {
			return version, "", nil
		}
		out, ok := mint[filepath.Base(name)]
		if !ok {
			return "", "no probe for " + name, errors.New("exit status 1")
		}
		return out, "", nil
	}
}

const brainVersion = `{"version":"1.3.4","brain":"0.6.0"}`
const stockVersion = `{"version":"1.3.4"}`

func probeJSON(mint, db string) string {
	b, _ := json.Marshal(map[string]any{"store": "x", "mint_prefix": mint, "db_issue_prefix": db})
	return string(b)
}

func probeJSONOwned(mint, db string, owned bool) string {
	b, _ := json.Marshal(map[string]any{"store": "x", "mint_prefix": mint, "db_issue_prefix": db, "owned": owned})
	return string(b)
}

func findCheck(rep verifyHostReport, check, store string) *verifyCheck {
	for i := range rep.Checks {
		if rep.Checks[i].Check == check && rep.Checks[i].Store == store && rep.Checks[i].Status == "fail" {
			return &rep.Checks[i]
		}
	}
	return nil
}

func TestVerifyHostHealthy(t *testing.T) {
	home := verifyHome(t, map[string]string{"task": "task", "ideas": "idea"})
	var calls []string
	rep := verifyHost(context.Background(), home, "/bin/bd", time.Second, fakeRun(brainVersion, map[string]string{
		"task": probeJSON("task", "lifespan"), "ideas": probeJSON("idea", "lifespan"),
	}, &calls))
	if !rep.OK || len(rep.Failing) != 0 {
		t.Fatalf("healthy host reported failures: %+v", rep.Checks)
	}
	// ideas mints idea-: the expected prefix is config.yaml's, not the name.
	for _, s := range rep.Stores {
		if s.Name == "ideas" && s.ExpectedPrefix != "idea" {
			t.Fatalf("ideas expected prefix = %q, want idea", s.ExpectedPrefix)
		}
		if !s.Probed || s.DBIssuePrefix != "lifespan" {
			t.Fatalf("store %s not probed or db prefix lost: %+v", s.Name, s)
		}
	}
}

func TestVerifyHostStockBd(t *testing.T) {
	home := verifyHome(t, map[string]string{"task": "task"})
	var calls []string
	rep := verifyHost(context.Background(), home, "/opt/homebrew/bin/bd", time.Second, fakeRun(stockVersion, nil, &calls))
	if rep.OK {
		t.Fatal("a stock bd host verified")
	}
	c := findCheck(rep, verifyCheckBdBuild, "")
	if c == nil || !strings.Contains(c.Message, "not the brain build") || !strings.Contains(c.Message, "/opt/homebrew/bin/bd") {
		t.Fatalf("bd-build finding = %+v", c)
	}
	// Stock bd has no probe verb, and a real mint would leave a bead: no wrapper is run.
	for _, call := range calls {
		if strings.Contains(call, "store-prefix") {
			t.Fatalf("probed a wrapper under stock bd: %s", call)
		}
	}
}

func TestVerifyHostMissingRegistry(t *testing.T) {
	home := verifyHome(t, map[string]string{"task": "task"})
	if err := os.Remove(filepath.Join(home, ".config/trillium-stores/stores.yaml")); err != nil {
		t.Fatal(err)
	}
	var calls []string
	rep := verifyHost(context.Background(), home, "bd", time.Second, fakeRun(brainVersion, map[string]string{"task": probeJSON("task", "lifespan")}, &calls))
	c := findCheck(rep, verifyCheckRegistry, "")
	if rep.OK || c == nil || !strings.Contains(c.Message, "trillium-stores/stores.yaml is missing") {
		t.Fatalf("registry finding = %+v ok=%v", c, rep.OK)
	}
}

func TestVerifyHostMissingWrapper(t *testing.T) {
	home := verifyHome(t, map[string]string{"task": "task", "robots": "agent"})
	if err := os.Remove(filepath.Join(home, ".local/bin/robots")); err != nil {
		t.Fatal(err)
	}
	// A backup copy of a wrapper is not the wrapper.
	writeVerifyWrapper(t, home, "robots.bak")
	var calls []string
	rep := verifyHost(context.Background(), home, "bd", time.Second, fakeRun(brainVersion, map[string]string{"task": probeJSON("task", "lifespan")}, &calls))
	c := findCheck(rep, verifyCheckWrapper, "robots")
	if rep.OK || c == nil || !strings.Contains(c.Message, `store "robots" has no executable wrapper`) {
		t.Fatalf("wrapper finding = %+v ok=%v", c, rep.OK)
	}
	if findCheck(rep, verifyCheckWrapper, "task") != nil {
		t.Fatal("a healthy store was flagged")
	}
}

func TestVerifyHostWrongMintPrefix(t *testing.T) {
	home := verifyHome(t, map[string]string{"task": "task", "robots": "agent"})
	var calls []string
	rep := verifyHost(context.Background(), home, "bd", time.Second, fakeRun(brainVersion, map[string]string{
		"task": probeJSON("lifespan", "lifespan"), "robots": probeJSON("agent", "lifespan"),
	}, &calls))
	c := findCheck(rep, verifyCheckMint, "task")
	if rep.OK || c == nil {
		t.Fatalf("a wrong mint prefix verified: %+v", rep.Checks)
	}
	for _, want := range []string{`prefix "lifespan"`, `want "task"`, `database issue_prefix "lifespan"`} {
		if !strings.Contains(c.Message, want) {
			t.Fatalf("mint finding %q lacks %q", c.Message, want)
		}
	}
	if findCheck(rep, verifyCheckMint, "robots") != nil {
		t.Fatal("robots mints agent- and must pass")
	}
	if len(rep.Failing) != 1 || rep.Failing[0] != "mint-prefix:task" {
		t.Fatalf("failing = %v", rep.Failing)
	}
}

func TestVerifyHostRejectsUnownedMintPrefix(t *testing.T) {
	home := verifyHome(t, map[string]string{"robots": "agent"})
	var calls []string
	rep := verifyHost(context.Background(), home, "bd", time.Second, fakeRun(brainVersion, map[string]string{
		"robots": probeJSONOwned("agent", "lifespan", false),
	}, &calls))
	c := findCheck(rep, verifyCheckMint, "robots")
	if rep.OK || c == nil || !strings.Contains(c.Message, "unowned prefix") {
		t.Fatalf("an unowned mint prefix verified: %+v", rep.Checks)
	}
}

func TestVerifyHostProbesRegistryUnion(t *testing.T) {
	home := verifyHome(t, map[string]string{"task": "task"})
	robots := filepath.Join(home, "data", "robots", ".beads")
	if err := os.MkdirAll(robots, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(robots, "config.yaml"), []byte("issue-prefix: agent\nBD_NAME: robots\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeVerifyWrapper(t, home, "robots")
	registry := "stores:\n  task:\n    path: " + filepath.Join(home, "data", "task", ".beads") + "\n  robots:\n    path: " + robots + "\n"
	if err := os.WriteFile(filepath.Join(home, ".config", "trillium-stores", "stores.yaml"), []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}

	var calls []string
	rep := verifyHost(context.Background(), home, "bd", time.Second, fakeRun(brainVersion, map[string]string{
		"task": probeJSON("task", "lifespan"), "robots": probeJSON("lifespan", "lifespan"),
	}, &calls))
	if rep.OK || findCheck(rep, verifyCheckMint, "robots") == nil {
		t.Fatalf("store found only in the second registry was not verified: %+v", rep.Checks)
	}
}

func TestVerifyHostProbeFailureAndRefusal(t *testing.T) {
	home := verifyHome(t, map[string]string{"task": "task", "robots": "agent"})
	var calls []string
	rep := verifyHost(context.Background(), home, "bd", time.Second, fakeRun(brainVersion, map[string]string{
		"robots": `{"error":"unified database refused a storeless mint","db_issue_prefix":"lifespan"}`,
	}, &calls))
	if c := findCheck(rep, verifyCheckMint, "task"); c == nil || !strings.Contains(c.Message, "failed") {
		t.Fatalf("a probe that printed nothing must fail: %+v", c)
	}
	if c := findCheck(rep, verifyCheckMint, "robots"); c == nil || !strings.Contains(c.Message, "refused") {
		t.Fatalf("a refusal must fail: %+v", c)
	}
}

func TestVerifyHostProbesOnlyWithReadOnlyVerb(t *testing.T) {
	home := verifyHome(t, map[string]string{"task": "task"})
	var calls []string
	verifyHost(context.Background(), home, "bd", time.Second, fakeRun(brainVersion, map[string]string{"task": probeJSON("task", "lifespan")}, &calls))
	for _, c := range calls {
		if strings.Contains(c, "create") || strings.Contains(c, "delete") || strings.Contains(c, "close") {
			t.Fatalf("the verification ran a mutating command: %s", c)
		}
	}
}

func TestVerifyHostReportRendering(t *testing.T) {
	home := verifyHome(t, map[string]string{"task": "task"})
	var calls []string
	rep := verifyHost(context.Background(), home, "bd", time.Second, fakeRun(brainVersion, map[string]string{"task": probeJSON("lifespan", "lifespan")}, &calls))

	var out, errb bytes.Buffer
	writeVerifyHostReport(&out, &errb, rep, true)
	var got verifyHostReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, out.String())
	}
	if got.OK || len(got.Failing) != 1 || errb.Len() != 0 {
		t.Fatalf("json report = %+v stderr=%q", got, errb.String())
	}

	out.Reset()
	errb.Reset()
	writeVerifyHostReport(&out, &errb, rep, false)
	text := errb.String()
	if !strings.Contains(text, "FAIL  mint-prefix") || !strings.Contains(text, "FAILING CHECKS: mint-prefix:task") || !strings.Contains(text, "fix:") {
		t.Fatalf("text report lacks the finding, its fix or the failing line:\n%s", text)
	}
}

func TestVerifyExpectedPrefixFallbacks(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) storeEntry {
		d := t.TempDir()
		if body != "" {
			if err := os.WriteFile(filepath.Join(d, "config.yaml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return storeEntry{Path: d}
	}
	_ = dir
	if got := verifyExpectedPrefix("ideas", write("issue-prefix: idea\nBD_NAME: ideas\n")); got != "idea" {
		t.Fatalf("issue-prefix must win, got %q", got)
	}
	if got := verifyExpectedPrefix("person", write("BD_NAME: persons\n")); got != "persons" {
		t.Fatalf("BD_NAME is the fallback, got %q", got)
	}
	if got := verifyExpectedPrefix("ops", write("")); got != "ops" {
		t.Fatalf("the registry name is the last resort, got %q", got)
	}
}

func TestVerifyHostIsStoreFree(t *testing.T) {
	if !storesCommandCanRunWithoutStore(brainStoresVerifyHostCmd) {
		t.Fatal("verify-host must run with no usable caller store")
	}
	if !registryRideAlongSkipped(brainStoresVerifyHostCmd) {
		t.Fatal("verify-host observes the registry itself and must not ride along a sync")
	}
}
