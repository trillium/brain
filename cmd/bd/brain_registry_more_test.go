package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/steveyegge/beads/internal/storeregistry"
	"github.com/steveyegge/beads/internal/storeregistry/regtest"
)

// ---------------------------------------------------------------------------
// reconcile: adopt what is certain, ask about the rest, never guess
// ---------------------------------------------------------------------------

func TestReconcileAsksAboutDatabaseOnlyNamesAndNeverGuesses(t *testing.T) {
	f := newFleet(t)
	a := f.addMachine("mini0")
	a.use()
	ctx := context.Background()

	// The eight names the database knows that no registry lists.
	dbOnly := []string{"db:beads_global", "db:commitments", "db:feedtack", "db:gcnotes", "db:guard", "db:retro", "db:rt", "db:TinyKeyboard"}
	for i, k := range dbOnly {
		f.mustExec("REPLACE INTO brain_stores (`store`,`registered`,`source_database`,`beads_dir`,`project_id`,`schema_version`,`template_source`,`imported_at`) VALUES (?, 0, ?, '', '', '', 0, '2026-10-21 00:00:00')", k, strings.TrimPrefix(k, "db:"))
		f.mustExec("REPLACE INTO brain_store_prefixes (`prefix`,`store`,`owner_reason`,`declared_by`,`observed_by`,`bead_count`,`ambiguous`) VALUES (?, ?, 'observed', '', '', ?, 0)", strings.TrimPrefix(k, "db:"), k, i+1)
	}
	f.mustExec("REPLACE INTO brain_stores (`store`,`registered`,`source_database`,`beads_dir`,`project_id`,`schema_version`,`template_source`,`imported_at`) VALUES ('task', 1, 'brain_unified', '', 'p', '', 0, '2026-10-21 00:00:00')")

	report := filepath.Join(a.home, ".config/brain/reconcile-report.md")
	decisions := filepath.Join(a.home, ".config/brain/reconcile-decisions.yaml")
	snap, adopted, err := storeregistry.ReadSnapshot(ctx, a.db)
	if err != nil || adopted {
		t.Fatalf("fresh database must not look adopted: %v %v", adopted, err)
	}
	plan, err := planReconcile(ctx, a.db, snap, adopted, loadReconcileDecisions(decisions))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Adopt) != 1 || plan.Adopt[0].Name != "task" {
		t.Fatalf("the one store this machine registers is certain: %+v", plan.Adopt)
	}
	if len(plan.DBOnly) != 8 || len(plan.Undecided) != 8 || len(plan.Decided) != 0 {
		t.Fatalf("all 8 database-only names must be asked about, none guessed: dbonly=%d undecided=%d decided=%d",
			len(plan.DBOnly), len(plan.Undecided), len(plan.Decided))
	}
	if err := writeReconcileFiles(report, decisions, plan, 0, false); err != nil {
		t.Fatal(err)
	}
	rep, _ := os.ReadFile(report)
	for _, k := range dbOnly {
		if !strings.Contains(string(rep), "`"+k+"`") {
			t.Errorf("the report must list %s", k)
		}
	}
	// The decisions template starts undecided and survives a second write untouched.
	var d reconcileDecisionFile
	data, _ := os.ReadFile(decisions)
	if err := yaml.Unmarshal(data, &d); err != nil || len(d.Decisions) != 8 {
		t.Fatalf("decisions template: %v %d", err, len(d.Decisions))
	}
	for k, v := range d.Decisions {
		if v.Action != "" {
			t.Errorf("%s: the template must not pre-decide (got %q)", k, v.Action)
		}
	}

	// Apply with nothing decided: only the certain store is adopted.
	res, err := applyReconcile(ctx, a.db, plan)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := storeregistry.ReadEntries(ctx, a.db)
	if len(entries) != 1 || entries[0].Name != "task" || res.Epoch != 1 {
		t.Fatalf("only task should be registered, got %+v (epoch %d)", entries, res.Epoch)
	}

	// The human decides: retro and TinyKeyboard are stores, rt is not.
	d.Decisions["db:retro"] = reconcileDecision{Action: "store"}
	d.Decisions["db:TinyKeyboard"] = reconcileDecision{Action: "store", Name: "tinykeyboard"}
	d.Decisions["db:rt"] = reconcileDecision{Action: "not-a-store"}
	out, _ := yaml.Marshal(d)
	mustWrite(t, decisions, string(out), 0o644)
	snap, adopted, _ = storeregistry.ReadSnapshot(ctx, a.db)
	plan, err = planReconcile(ctx, a.db, snap, adopted, loadReconcileDecisions(decisions))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Decided) != 2 || len(plan.NotStores) != 1 || len(plan.Undecided) != 5 {
		t.Fatalf("decided=%d notstores=%d undecided=%d", len(plan.Decided), len(plan.NotStores), len(plan.Undecided))
	}
	if _, err := applyReconcile(ctx, a.db, plan); err != nil {
		t.Fatal(err)
	}
	entries, _ = storeregistry.ReadEntries(ctx, a.db)
	names := map[string]storeregistry.Entry{}
	for _, e := range entries {
		names[e.Name] = e
	}
	if len(names) != 3 || names["retro"].Store != "db:retro" || names["tinykeyboard"].Store != "db:TinyKeyboard" {
		t.Fatalf("registry after decisions: %+v", names)
	}
	if _, ok := names["rt"]; ok {
		t.Error("a not-a-store name must never be registered")
	}
	var canonical string
	if err := f.srv.DB.QueryRow("SELECT `store` FROM brain_store_aliases WHERE alias = 'tinykeyboard'").Scan(&canonical); err != nil || canonical != "db:TinyKeyboard" {
		t.Errorf("a decided name needs its database alias so BD_NAME resolves: %q %v", canonical, err)
	}
	// Idempotent: applying again changes nothing.
	snap, adopted, _ = storeregistry.ReadSnapshot(ctx, a.db)
	plan, _ = planReconcile(ctx, a.db, snap, adopted, loadReconcileDecisions(decisions))
	again, err := applyReconcile(ctx, a.db, plan)
	if err != nil || again.Changed {
		t.Errorf("second apply must be a no-op: %+v %v", again, err)
	}
}

// ---------------------------------------------------------------------------
// the database supplies data, never code
// ---------------------------------------------------------------------------

func TestPoisonedRegistryRowsNeverReachTheDisk(t *testing.T) {
	f := newFleet(t)
	a, b := f.addMachine("mini0"), f.addMachine("mini1")
	f.adopt(a)
	ctx := context.Background()

	// A hand edit that keeps the digest valid (the worst case: someone with
	// database write access recomputes it). The entries are hostile.
	for _, row := range [][]any{
		{"evil;touch pwned", "store", "evil", "active", "data/x/.beads"},
		{"ok-name", "store", "ok-name", "active", "data/../../../etc/x"},
		{"quote", "store", "quote", "active", "data/a\"b/.beads"},
		{"dollar", "store", "dollar", "active", "data/$(id)/.beads"},
	} {
		f.mustExec("REPLACE INTO brain_registry_entries (name, kind, store, state, rel_path, wrapper_kind, about, edit_back, created_epoch, changed_epoch) VALUES (?, ?, ?, ?, ?, 'server', '', 0, 2, 2)", row...)
	}
	entries, _ := storeregistry.ReadEntries(ctx, a.db)
	f.mustExec("UPDATE brain_registry_state SET epoch = 2, digest = ? WHERE id = 1", storeregistry.Digest(entries))

	b.use()
	shims := func() string { // everything a hostile row could add: commands, links, data directories
		var sb strings.Builder
		for _, d := range []string{".local", ".pi", "data"} {
			sb.WriteString(snapshotTree(t, filepath.Join(b.home, d)))
		}
		return sb.String()
	}
	before := shims()
	rep, err := runRegistrySync(ctx, syncOptions{Source: dbSource{db: b.db}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != "partial" {
		t.Errorf("hostile rows must leave the machine not-converged, got %s", rep.Status)
	}
	if after := shims(); after != before {
		t.Errorf("hostile rows changed the shims:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	for _, p := range []string{filepath.Join(b.home, "pwned"), filepath.Join(filepath.Dir(b.home), "etc")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s exists", p)
		}
	}
}

func TestHandEditedRowsWithoutDigestAreRefused(t *testing.T) {
	f := newFleet(t)
	a, b := f.addMachine("mini0"), f.addMachine("mini1")
	f.adopt(a)
	a.createStore("recipes")
	f.mustExec("UPDATE brain_registry_entries SET about = 'sneaky' WHERE name = 'recipes'")
	b.use()
	if _, err := runRegistrySync(context.Background(), syncOptions{Source: dbSource{db: b.db}}); err == nil {
		t.Fatal("a torn read / hand edit must be refused, not applied")
	}
	if b.has(".local/bin/recipes") {
		t.Error("nothing may be applied from a torn read")
	}
}

// ---------------------------------------------------------------------------
// generated wrappers
// ---------------------------------------------------------------------------

func TestGeneratedWrappersAreValidShellAndHomeRelative(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	env := shimEnv{Home: "/home/u", Host: "100.84.99.99", Port: 3307}
	cases := []storeregistry.Entry{
		storeregistry.New("recipes"),
		{Name: "meals", Kind: storeregistry.KindStore, Store: "recipes", State: storeregistry.StateActive, RelPath: "data/meals/.beads", WrapperKind: storeregistry.WrapperServer},
		{Name: "food", Kind: storeregistry.KindAlias, Store: "meals", State: storeregistry.StateActive, RelPath: "data/meals/.beads", WrapperKind: storeregistry.WrapperServer},
		{Name: "emb", Kind: storeregistry.KindStore, Store: "emb", State: storeregistry.StateActive, RelPath: "data/emb/.beads", WrapperKind: storeregistry.WrapperEmbedded},
	}
	for _, e := range cases {
		body := syncWrapperBody(e, env)
		if strings.Contains(body, "/home/u") {
			t.Errorf("%s: wrapper carries the machine's absolute home:\n%s", e.Name, body)
		}
		if !strings.HasPrefix(body, "#!/bin/sh\n# Auto-generated by 'brain stores sync'") {
			t.Errorf("%s: missing the generated marker:\n%s", e.Name, body)
		}
		if !isGeneratedWrapperBody([]byte(body)) {
			t.Errorf("%s: not recognised as generated", e.Name)
		}
		p := filepath.Join(t.TempDir(), e.Name)
		mustWrite(t, p, body, 0o755)
		if out, err := exec.Command("sh", "-n", p).CombinedOutput(); err != nil {
			t.Errorf("%s: sh -n: %v\n%s", e.Name, err, out)
		}
	}
	meals := syncWrapperBody(cases[1], env)
	if !strings.Contains(meals, `BD_NAME="meals"`) {
		t.Errorf("a renamed store's BD_NAME is its new name (the database alias resolves it):\n%s", meals)
	}
	if food := syncWrapperBody(cases[2], env); !strings.Contains(food, `BD_NAME="meals"`) {
		t.Errorf("an alias pins the canonical store:\n%s", food)
	}
	if emb := syncWrapperBody(cases[3], env); strings.Contains(emb, "BEADS_DOLT_SERVER") {
		t.Errorf("an embedded store gets no server pins:\n%s", emb)
	}
	// The invoking environment's host must not make two runs differ.
	other := syncWrapperBody(cases[0], shimEnv{Home: "/home/u", Host: "127.0.0.1", Port: 3310})
	if other != syncWrapperBody(cases[0], env) {
		t.Error("the sync wrapper must not depend on the invoking environment's host/port")
	}
}

// A store wrapper written by 'brain stores sync' is already what
// 'brain stores rewrite-wrappers' installs: sync, rewrite, sync changes
// nothing and leaves no repoint backup, and a wrapper an older sync baked
// host defaults into converges after one pass of each.
func TestSyncAndRewriteWrappersAgree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	home := t.TempDir()
	e := storeregistry.New("recipes")
	body := syncWrapperBody(e, shimEnv{Home: home, Host: "100.84.99.99", Port: 3307})
	bin := filepath.Join(home, ".local", "bin")
	wrapper := filepath.Join(bin, "recipes")

	assertNoop := func(stage string) {
		t.Helper()
		plan := &syncPlan{Missing: map[string][]string{"wrappers": nil}}
		plan.addWrapper("recipes", wrapper, []byte(body), "generated wrapper")
		if len(plan.Actions) != 0 {
			t.Errorf("%s: sync wants to change the wrapper: %+v", stage, plan.Actions)
		}
	}
	assertNoBackup := func(stage string) {
		t.Helper()
		ents, err := os.ReadDir(bin)
		if err != nil {
			t.Fatal(err)
		}
		for _, ent := range ents {
			if strings.Contains(ent.Name(), wrapperBackupMarker) {
				t.Errorf("%s: rewrite left a backup %s", stage, ent.Name())
			}
		}
	}

	mustWrite(t, wrapper, body, 0o755)
	assertNoop("after sync")
	repointed, err := rewriteWrapperInPlace(wrapper, true)
	if err != nil || repointed {
		t.Fatalf("rewrite of a sync wrapper: repointed=%v err=%v", repointed, err)
	}
	assertNoBackup("after rewrite")
	assertNoop("after rewrite")

	legacy := strings.Replace(body, sharedServerHostBlock, "", 1)
	legacy = strings.Replace(legacy, "exec ", "export BEADS_DOLT_SERVER_HOST=\"${BEADS_DOLT_SERVER_HOST:-127.0.0.1}\"\n"+
		"export BEADS_DOLT_SERVER_PORT=\"${BEADS_DOLT_SERVER_PORT:-3307}\"\nexec ", 1)
	mustWrite(t, wrapper, legacy, 0o755)
	plan := &syncPlan{Missing: map[string][]string{"wrappers": nil}}
	plan.addWrapper("recipes", wrapper, []byte(body), "generated wrapper")
	if len(plan.Actions) != 1 || plan.Actions[0].Op != "update" {
		t.Fatalf("a wrapper with baked host defaults must be updated: %+v", plan.Actions)
	}
	mustWrite(t, wrapper, body, 0o755)
	if repointed, err := rewriteWrapperInPlace(wrapper, true); err != nil || repointed {
		t.Fatalf("rewrite after the update: repointed=%v err=%v", repointed, err)
	}
	assertNoBackup("after the update")

	// No shared store host config and no env value: a loud refusal naming the
	// file, never a silent 127.0.0.1.
	mustWrite(t, filepath.Join(bin, "bd"), "#!/bin/sh\necho \"host=$BEADS_DOLT_SERVER_HOST\"\n", 0o755)
	cmd := exec.Command(wrapper)
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "BD_BIN=" + filepath.Join(bin, "bd")}
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 78 || !strings.Contains(string(out), filepath.Join(home, ".config", "brain", "store-host.env")) {
		t.Errorf("missing store-host.env: exit=%v out=%q", err, out)
	}
	mustWrite(t, filepath.Join(home, ".config", "brain", "store-host.env"),
		"BEADS_DOLT_SERVER_HOST=100.84.99.99\nBEADS_DOLT_SERVER_PORT=3307\n", 0o644)
	cmd = exec.Command(wrapper)
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "BD_BIN=" + filepath.Join(bin, "bd")}
	if out, err := cmd.Output(); err != nil || strings.TrimSpace(string(out)) != "host=100.84.99.99" {
		t.Errorf("with store-host.env: %q %v", out, err)
	}
}

func TestEmbeddedStoreWithoutItsDirectoryIsSkippedNotBroken(t *testing.T) {
	f := newFleet(t)
	a, b := f.addMachine("mini0"), f.addMachine("mini1")
	f.adopt(a)
	a.use()
	emb := storeregistry.Entry{Name: "emb", Kind: storeregistry.KindStore, Store: "emb", State: storeregistry.StateActive, RelPath: "data/emb/.beads", WrapperKind: storeregistry.WrapperEmbedded}
	if _, err := storeregistry.Mutate(context.Background(), a.db, registryMeta(), storeregistry.PlanCreate(emb)); err != nil {
		t.Fatal(err)
	}
	b.use()
	rep, err := runRegistrySync(context.Background(), syncOptions{Source: dbSource{db: b.db}})
	if err != nil || rep.Status != "converged" {
		t.Fatalf("an embedded store that lives on another machine is not drift: %+v %v", rep, err)
	}
	if b.has(".local/bin/emb") {
		t.Error("no wrapper for an embedded store whose directory is not here")
	}
}

// ---------------------------------------------------------------------------
// client machine (the macbook): reads through its relay host
// ---------------------------------------------------------------------------

func TestClientMachineSyncsThroughItsRelayHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	f := newFleet(t)
	a := f.addMachine("mini0")
	f.adopt(a)
	a.createStore("recipes")

	// What the relay host would answer to 'brain stores registry export --json'.
	ctx := context.Background()
	snap, _, err := storeregistry.ReadSnapshot(ctx, a.db)
	if err != nil {
		t.Fatal(err)
	}
	machines, _ := storeregistry.ReadMachines(ctx, a.db)
	wire := registrySnapshotJSON{State: snap.State, Entries: snap.Entries, Found: true, UnifiedDB: regtest.Database, Machines: machines}
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "export.json"), string(jsonBytes(wire)), 0o644)
	ssh := filepath.Join(dir, "ssh")
	mustWrite(t, ssh, `#!/bin/sh
for last; do :; done
case "$last" in
  *"'registry' 'export'"*) cat "$FAKE_EXPORT" ;;
  *"'registry' 'report'"*) echo "$last" >> "$FAKE_REPORTS" ;;
  *) echo "unexpected: $last" >&2; exit 9 ;;
esac
`, 0o755)

	client := t.TempDir()
	for _, d := range []string{".local/bin", ".config/brain"} {
		if err := os.MkdirAll(filepath.Join(client, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(client, ".config/brain/remote.yaml"), "host: me@relay\n", 0o644)
	t.Setenv("HOME", client)
	t.Setenv(envMachine, "macbook")
	t.Setenv(envRemoteSSH, ssh)
	t.Setenv("FAKE_EXPORT", filepath.Join(dir, "export.json"))
	t.Setenv("FAKE_REPORTS", filepath.Join(dir, "reports.log"))
	_ = os.Unsetenv(envRemoteOff)

	src, ok := clientSourceIfConfigured()
	if !ok {
		t.Fatal("remote.yaml should make this machine a client")
	}
	rep, err := runRegistrySync(ctx, syncOptions{Source: src})
	if err != nil || rep.Status != "converged" {
		t.Fatalf("client sync: %+v %v", rep, err)
	}
	body, err := os.ReadFile(filepath.Join(client, ".local/bin/recipes"))
	if err != nil || !strings.Contains(string(body), remoteWrapperMarker) || !strings.Contains(string(body), "BRAIN_REMOTE_STORE='recipes'") {
		t.Fatalf("a client gets the forwarding wrapper, not a server wrapper: %v\n%s", err, body)
	}
	if _, err := os.Stat(filepath.Join(client, "data")); err == nil {
		t.Error("a client machine has no store directories")
	}
	if !isRemoteWrapper(filepath.Join(client, ".local/bin/recipes")) {
		t.Error("the generated client wrapper must carry the remote marker so install/prune recognise it")
	}
	reports, _ := os.ReadFile(filepath.Join(dir, "reports.log"))
	if !strings.Contains(string(reports), "macbook") || !strings.Contains(string(reports), `"role":"client"`) && !strings.Contains(string(reports), "client") {
		t.Errorf("the client must report back through the relay: %q", reports)
	}

	// Removal reaches a client too.
	a.removeStore("recipes")
	snap, _, _ = storeregistry.ReadSnapshot(ctx, a.db)
	wire.State, wire.Entries = snap.State, snap.Entries
	mustWrite(t, filepath.Join(dir, "export.json"), string(jsonBytes(wire)), 0o644)
	t.Setenv("HOME", client)
	if rep, err = runRegistrySync(ctx, syncOptions{Source: src}); err != nil || rep.Status != "converged" {
		t.Fatalf("%+v %v", rep, err)
	}
	if _, err := os.Stat(filepath.Join(client, ".local/bin/recipes")); err == nil {
		t.Error("the removed store's client wrapper should be gone")
	}
}

// ---------------------------------------------------------------------------
// runtime resolution and the command-not-found hook
// ---------------------------------------------------------------------------

func TestRuntimeResolutionFindsAStoreBeforeAnyWrapperExists(t *testing.T) {
	f := newFleet(t)
	a, b := f.addMachine("mini0"), f.addMachine("mini1")
	f.adopt(a)
	b.anyCommand()
	a.createStore("recipes")

	b.use()
	// The cache has never heard of it: the unknown name triggers one sync and a retry.
	if ok, code := registryCheckEarly([]string{"store", "--check", "recipes"}); !ok || code != 1 {
		t.Fatalf("--check on an unknown name: handled=%v code=%d", ok, code)
	}
	inv, err := resolveStore(context.Background(), "recipes", dbSource{db: b.db})
	if err != nil {
		t.Fatal(err)
	}
	if inv.BDName != "recipes" || inv.BeadsDir != filepath.Join(b.home, "data/recipes/.beads") {
		t.Errorf("invocation: %+v", inv)
	}
	if !strings.Contains(strings.Join(inv.Env, " "), "BD_NAME=recipes") {
		t.Errorf("env: %v", inv.Env)
	}
	if ok, code := registryCheckEarly([]string{"store", "--check", "recipes"}); !ok || code != 0 {
		t.Errorf("--check after resolution: handled=%v code=%d", ok, code)
	}
	if _, err := resolveStore(context.Background(), "no-such-store", dbSource{db: b.db}); err == nil || !strings.Contains(err.Error(), "unknown store") ||
		!strings.Contains(err.Error(), "recipes") {
		t.Errorf("an unknown store should list the known ones: %v", err)
	}
	if _, err := resolveStore(context.Background(), "x;y", nil); err == nil {
		t.Error("an invalid name must be refused")
	}
	if ok, _ := registryCheckEarly([]string{"list"}); ok {
		t.Error("only 'store --check' is answered early")
	}
}

func TestResolveStoreWorksOnlyOfflineFromTheCache(t *testing.T) {
	f := newFleet(t)
	a, b := f.addMachine("mini0"), f.addMachine("mini1")
	f.adopt(a)
	a.createStore("recipes")
	b.anyCommand()
	b.use()
	// Offline: no registry source at all. The cache still resolves.
	if _, err := resolveStore(context.Background(), "recipes", nil); err != nil {
		t.Errorf("a cached store must resolve with the database unreachable: %v", err)
	}
}

func TestShellHooksCallBrainStoreAndFallThrough(t *testing.T) {
	for shell, fn := range map[string]string{"zsh": "command_not_found_handler", "bash": "command_not_found_handle"} {
		var out bytes.Buffer
		brainStoresShellHookCmd.SetOut(&out)
		// The command prints to stdout directly; capture by re-running its body.
		old := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w
		brainStoresShellHookCmd.Run(brainStoresShellHookCmd, []string{shell})
		_ = w.Close()
		os.Stdout = old
		_, _ = out.ReadFrom(r)
		for _, want := range []string{fn + "()", `brain store --check "$1"`, `brain store "$@"`, "return 127"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%s hook missing %q:\n%s", shell, want, out.String())
			}
		}
		p := filepath.Join(t.TempDir(), "hook.sh")
		mustWrite(t, p, out.String(), 0o644)
		if shell == "bash" {
			if b, err := exec.Command("bash", "-n", p).CombinedOutput(); err != nil {
				t.Errorf("bash -n: %v %s", err, b)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// routing: which registry verbs act on this machine even on a client
// ---------------------------------------------------------------------------

func TestRegistryLocalVerbRouting(t *testing.T) {
	for args, want := range map[string]bool{
		"stores sync":            true,
		"stores sync --dry-run":  true,
		"brain stores sync":      true,
		"stores verify --json":   true,
		"stores poller install":  true,
		"stores shell-hook zsh":  true,
		"stores doctor --fleet":  true,
		"stores doctor":          false,
		"stores list":            false,
		"stores create recipes":  false,
		"stores registry export": false,
		"stores reconcile":       false,
		"list --limit 1":         false,
		"stores epoch":           false,
	} {
		if got := registryLocalVerb(strings.Fields(args)); got != want {
			t.Errorf("registryLocalVerb(%q) = %v, want %v", args, got, want)
		}
	}
}

func TestStoreFreeClassification(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	find := func(path ...string) *cobra.Command {
		c, _, err := rootCmd.Find(path)
		if err != nil || c == nil {
			t.Fatalf("no command %v", path)
		}
		return c
	}
	sync := find("stores", "sync")
	if storesCommandCanRunWithoutStore(sync) {
		t.Error("on a direct machine 'stores sync' needs the connected store to reach the database")
	}
	mustWrite(t, filepath.Join(os.Getenv("HOME"), ".config/brain/remote.yaml"), "host: me@relay\n", 0o644)
	if !storesCommandCanRunWithoutStore(sync) {
		t.Error("on a client machine 'stores sync' reads through the relay and needs no local store")
	}
	if !storesCommandCanRunWithoutStore(find("stores", "poller", "print")) || !storesCommandCanRunWithoutStore(find("stores", "shell-hook")) {
		t.Error("poller and shell-hook never touch a store")
	}
	if storesCommandCanRunWithoutStore(find("stores", "list")) {
		t.Error("list is not exempt")
	}
}

func TestRideAlongIsWiredIntoEveryCommand(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	pre := string(src)
	i := strings.Index(pre, "PersistentPreRunE: func(cmd *cobra.Command, args []string) error {")
	j := strings.Index(pre, "PersistentPostRunE:")
	if i < 0 || j < i || !strings.Contains(pre[i:j], "registryRideAlong(rootCtx, cmd)") {
		t.Fatal("PersistentPreRunE must call registryRideAlong: every command carries the epoch")
	}
	if !strings.Contains(string(src), "registryCheckEarly(os.Args[1:])") {
		t.Error("main must answer 'brain store --check' before any command machinery")
	}
}

// ---------------------------------------------------------------------------
// the poller (rendered only; never installed by a test)
// ---------------------------------------------------------------------------

func TestPollerPlist(t *testing.T) {
	home := "/Users/someone"
	body, err := renderPollerPlist(home, "", 300)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<string>com.brain.stores-sync</string>",
		"<string>/Users/someone/.local/bin/brain</string>",
		"<string>stores</string>", "<string>sync</string>", "<string>--quiet</string>",
		"<key>RunAtLoad</key>\n  <true/>",
		"<key>StartInterval</key>\n  <integer>300</integer>",
		"/Users/someone/Library/Logs/brain/stores-sync.log",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plist missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(strings.ToLower(body), "pai") {
		t.Errorf("the poller must not mention pai:\n%s", body)
	}
	if _, err := renderPollerPlist(home, "", 30); err == nil {
		t.Error("an interval under a minute must be refused")
	}
	if _, err := renderPollerPlist(home, "brain", 300); err == nil {
		t.Error("a relative wrapper path must be refused")
	}
	if esc, err := renderPollerPlist(home, "/Users/some&one/bin/brain", 300); err != nil || !strings.Contains(esc, "some&amp;one") {
		t.Errorf("wrapper path must be XML-escaped: %v", err)
	}
	if p, err := exec.LookPath("plutil"); err == nil {
		f := filepath.Join(t.TempDir(), "x.plist")
		mustWrite(t, f, body, 0o644)
		if out, err := exec.Command(p, "-lint", f).CombinedOutput(); err != nil {
			t.Errorf("plutil -lint: %v\n%s", err, out)
		}
	}
	t.Setenv("HOME", t.TempDir())
	if pollerInstalled() {
		t.Error("nothing in the build or the tests installs the poller")
	}
}

func TestPollerInstallDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	pollerDryRun, pollerNoLoad, pollerWrapper, pollerInterval = true, false, "", 300
	defer func() { pollerDryRun = false }()
	brainStoresPollerInstallCmd.Run(brainStoresPollerInstallCmd, nil)
	if _, err := os.Stat(filepath.Join(home, "Library")); err == nil {
		t.Error("--dry-run created files")
	}
}

// ---------------------------------------------------------------------------
// the old local-only behaviour, fixed
// ---------------------------------------------------------------------------

func TestLegacyRemoveRegeneratesEnvAndSaysWhatItLeft(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	registryDBFunc = func() (*sql.DB, error) { return nil, os.ErrNotExist }
	defer func() { registryDBFunc = defaultRegistryDB }()
	if err := saveStoresRegistry(map[string]storeEntry{"keep": {Path: home + "/data/keep/.beads"}, "gone": {Path: home + "/data/gone/.beads"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := writeStoreWrapper("gone", home+"/data/gone/.beads", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := regenerateStoresEnv(map[string]storeEntry{"keep": {Path: home + "/data/keep/.beads"}, "gone": {Path: home + "/data/gone/.beads"}}); err != nil {
		t.Fatal(err)
	}
	brainStoresRemoveCmd.Run(brainStoresRemoveCmd, []string{"gone"})
	envBody, _ := os.ReadFile(filepath.Join(home, ".config/brain/stores.env"))
	if strings.Contains(string(envBody), "GONE") || !strings.Contains(string(envBody), "BRAIN_STORE_KEEP") {
		t.Errorf("remove must regenerate stores.env:\n%s", envBody)
	}
	if _, err := os.Stat(filepath.Join(home, ".local/bin/gone")); err != nil {
		t.Error("local-only remove leaves the wrapper (and says so); it must not delete it silently")
	}
}

func TestJSONHelpers(t *testing.T) {
	var v verifyReport
	if err := json.Unmarshal(jsonBytes(verifyReport{Machine: "m", Missing: map[string][]string{"wrappers": {"a"}}}), &v); err != nil || v.Machine != "m" {
		t.Fatal(err)
	}
	if v.Converged() {
		t.Error("a report with missing wrappers is not converged")
	}
}

func TestRideAlongPolicyFileHoldsAMachineOnNotify(t *testing.T) {
	f := newFleet(t)
	a, b := f.addMachine("mini0"), f.addMachine("mini1")
	f.adopt(a)
	a.createStore("recipes")
	b.use()
	mustWrite(t, filepath.Join(b.home, ".config/brain/registry.policy"), "notify\n", 0o644)
	if registryPolicy() != "notify" {
		t.Fatal("the per-machine file should set the policy")
	}
	said := b.anyCommand()
	if !strings.Contains(said, "run 'brain stores sync'") || b.has(".local/bin/recipes") {
		t.Errorf("a machine on notify tells and waits: said=%q wrapper=%v", said, b.has(".local/bin/recipes"))
	}
	// The environment wins over the file; deleting the file returns to auto.
	t.Setenv(envRegistryPolicy, "auto")
	if registryPolicy() != "auto" {
		t.Error("BRAIN_REGISTRY_POLICY overrides the file")
	}
	t.Setenv(envRegistryPolicy, "")
	if err := os.Remove(filepath.Join(b.home, ".config/brain/registry.policy")); err != nil {
		t.Fatal(err)
	}
	b.anyCommand()
	if !b.has(".local/bin/recipes") {
		t.Error("back on auto, the next command syncs")
	}
}
