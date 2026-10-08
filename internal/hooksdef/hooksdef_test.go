package hooksdef

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeHook(t *testing.T, dir, file, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, file)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAllMissingDirIsEmptyNotError(t *testing.T) {
	defs, err := LoadAll(filepath.Join(t.TempDir(), "hooks.d"))
	if err != nil || len(defs) != 0 {
		t.Fatalf("defs=%v err=%v; an absent hooks.d is no hooks, not a failure", defs, err)
	}
}

func TestLoadFileDefaultsAndDeclared(t *testing.T) {
	dir := t.TempDir()
	p := writeHook(t, dir, "g.toml", "run = \"true\"\npolicy = \"guard\"\nwhen = \"close\"\ntimeout = \"3s\"\nrefusal_exit = 9\n")
	d, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "g" || d.Policy != PolicyGuard || d.When != EventClose || d.Timeout != 3*time.Second || d.RefusalExit != 9 {
		t.Fatalf("unexpected definition %+v", d)
	}

	p = writeHook(t, dir, "u.toml", "run = \"true\"\n")
	d, err = LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if d.Declared() || d.Event() != EventAll || d.Timeout != DefaultTimeout || d.RefusalExit != DefaultRefusalExit {
		t.Fatalf("unset hook should default to undeclared/*/10s/2, got %+v", d)
	}
	if !strings.Contains(d.Describe(), "warns") {
		t.Fatalf("Describe must say an unset hook warns: %q", d.Describe())
	}
}

func TestLoadRefusesRatherThanGuesses(t *testing.T) {
	cases := map[string]struct{ file, body, want string }{
		"bad toml":          {"a.toml", "run = = =", "parsing TOML"},
		"missing run":       {"a.toml", "policy = \"guard\"\n", "no run command"},
		"unknown policy":    {"a.toml", "run = \"true\"\npolicy = \"gaurd\"\n", "unknown policy"},
		"unknown when":      {"a.toml", "run = \"true\"\nwhen = \"creat\"\n", "unknown when"},
		"misspelled key":    {"a.toml", "run = \"true\"\npolcy = \"guard\"\n", "unknown key"},
		"name mismatch":     {"a.toml", "name = \"b\"\nrun = \"true\"\n", "does not match the file name"},
		"bad timeout":       {"a.toml", "run = \"true\"\ntimeout = \"soon\"\n", "bad timeout"},
		"negative timeout":  {"a.toml", "run = \"true\"\ntimeout = \"-1s\"\n", "must be positive"},
		"refusal_exit 300":  {"a.toml", "run = \"true\"\nrefusal_exit = 300\n", "out of range"},
		"wrong type policy": {"a.toml", "run = \"true\"\npolicy = 3\n", "parsing TOML"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeHook(t, dir, c.file, c.body)
			_, err := LoadAll(dir)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
			if !strings.Contains(err.Error(), filepath.Join(dir, c.file)) {
				t.Fatalf("error must name the file: %v", err)
			}
		})
	}
}

func TestLoadAllRefusesStrayFilesAndDuplicatesAndKeepsGoodHooks(t *testing.T) {
	dir := t.TempDir()
	writeHook(t, dir, "good.toml", "run = \"true\"\npolicy = \"observer\"\n")
	writeHook(t, dir, "notes.txt", "hello")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	defs, err := LoadAll(dir)
	if err == nil || !strings.Contains(err.Error(), "notes.txt") || !strings.Contains(err.Error(), "sub") {
		t.Fatalf("stray file and directory must both be named, got %v", err)
	}
	if len(defs) != 1 || defs[0].Name != "good" {
		t.Fatalf("good hook should still be returned alongside the error, got %v", defs)
	}
}

func TestDecideMatrix(t *testing.T) {
	guard := Definition{Name: "g", Path: "/h/g.toml", Policy: PolicyGuard, RefusalExit: 2, Timeout: time.Second}
	observer := Definition{Name: "o", Path: "/h/o.toml", Policy: PolicyObserver, RefusalExit: 2, Timeout: time.Second}
	unset := Definition{Name: "u", Path: "/h/u.toml", RefusalExit: 2, Timeout: time.Second}

	t.Run("guard pass", func(t *testing.T) {
		if d := Decide(guard, EventCreate, RunResult{}); d.Refusal != nil || d.Warning != nil {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("guard refusal exit refuses and carries stderr", func(t *testing.T) {
		d := Decide(guard, EventCreate, RunResult{ExitCode: 2, Stderr: "no secrets\nmore"})
		if d.Refusal == nil || d.Refusal.Cause != CauseRefused || d.Warning != nil {
			t.Fatalf("%+v", d)
		}
		msg := d.Refusal.Error()
		if !strings.Contains(msg, "bead not created") || !strings.Contains(msg, `"g"`) || !strings.Contains(msg, "no secrets") || !strings.Contains(msg, "no secrets / more") {
			t.Fatalf("refusal must name the hook, say the bead was not created, quote the whole output on one line: %q", msg)
		}
	})
	t.Run("guard other exit fails closed", func(t *testing.T) {
		d := Decide(guard, EventClose, RunResult{ExitCode: 7, Stderr: "boom"})
		if d.Refusal == nil || d.Refusal.Cause != CauseFailed || !strings.Contains(d.Refusal.Error(), "failed closed") || !strings.Contains(d.Refusal.Error(), "exit") {
			t.Fatalf("%+v", d)
		}
		if strings.Contains(d.Refusal.Error(), "bead not created") {
			t.Fatalf("a close refusal must not claim a create: %q", d.Refusal.Error())
		}
	})
	t.Run("guard timeout and start failure fail closed", func(t *testing.T) {
		if d := Decide(guard, EventCreate, RunResult{TimedOut: true}); d.Refusal == nil || d.Refusal.Cause != CauseTimeout {
			t.Fatalf("%+v", d)
		}
		if d := Decide(guard, EventCreate, RunResult{StartErr: os.ErrNotExist}); d.Refusal == nil || d.Refusal.Cause != CauseLoad {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("observer pass is silent", func(t *testing.T) {
		if d := Decide(observer, EventCreate, RunResult{}); d.Refusal != nil || d.Warning != nil {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("observer failure fails open with a warning, even on the refusal exit", func(t *testing.T) {
		for _, res := range []RunResult{{ExitCode: 3, Stderr: "down"}, {ExitCode: 2}, {TimedOut: true}, {StartErr: os.ErrPermission}} {
			d := Decide(observer, EventCreate, res)
			if d.Refusal != nil || d.Warning == nil || d.Warning.Reason != ReasonFailure || d.Warning.Hook != "o" || d.Warning.Status != StatusOpen {
				t.Fatalf("res=%+v -> %+v", res, d)
			}
		}
	})
	t.Run("unset warns on success and on failure, never blocks", func(t *testing.T) {
		d := Decide(unset, EventCreate, RunResult{})
		if d.Refusal != nil || d.Warning == nil || d.Warning.Reason != ReasonUnsetPolicy || !strings.Contains(d.Warning.Detail, "declares no failure policy") {
			t.Fatalf("%+v", d)
		}
		d = Decide(unset, EventCreate, RunResult{ExitCode: 2})
		if d.Refusal != nil || d.Warning == nil || d.Warning.Reason != ReasonFailure || !strings.Contains(d.Warning.Detail, "declares no failure policy") {
			t.Fatalf("an unset hook exiting the refusal code still must not block: %+v", d)
		}
	})
}

func TestRunRealShell(t *testing.T) {
	def := Definition{Name: "r", Run: `cat >/dev/null; echo out; echo err >&2; exit 4`, Timeout: 5 * time.Second}
	res := Run(context.Background(), def, Payload{Event: EventCreate})
	if res.ExitCode != 4 || strings.TrimSpace(res.Stdout) != "out" || strings.TrimSpace(res.Stderr) != "err" {
		t.Fatalf("%+v", res)
	}

	def = Definition{Name: "slow", Run: "sleep 5", Timeout: 200 * time.Millisecond}
	start := time.Now()
	res = Run(context.Background(), def, Payload{Event: EventCreate})
	if !res.TimedOut || time.Since(start) > 4*time.Second {
		t.Fatalf("timeout not enforced: %+v after %v", res, time.Since(start))
	}
}

func TestRunPassesPayloadAndStoreBlindEnvironment(t *testing.T) {
	t.Setenv("BEADS_DIR", "/secret/store")
	t.Setenv("BD_NAME", "x")
	t.Setenv("DOLT_ROOT_PATH", "/dolt")
	t.Setenv("KEEP_ME", "yes")
	def := Definition{Name: "env", Timeout: 5 * time.Second,
		Run: `pwd; env | grep -E '^(BEADS_|BD_NAME|DOLT_|KEEP_ME|BD_INSIDE_HOOK)' | sort; cat`}
	res := Run(context.Background(), def, Payload{Event: EventCreate, Issue: &PayloadIssue{Title: "hello"}})
	if res.ExitCode != 0 {
		t.Fatalf("%+v", res)
	}
	for _, leaked := range []string{"BEADS_DIR", "BD_NAME", "DOLT_ROOT_PATH", "/secret/store"} {
		if strings.Contains(res.Stdout, leaked) {
			t.Fatalf("hook environment leaked %s:\n%s", leaked, res.Stdout)
		}
	}
	for _, want := range []string{"KEEP_ME=yes", EnvInsideHook + "=1", `"title":"hello"`} {
		if !strings.Contains(res.Stdout, want) {
			t.Fatalf("hook should see %s:\n%s", want, res.Stdout)
		}
	}
}

func TestWarningRecordRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	w := Warning{ID: NewWarningID(now), CreatedAt: now, Hook: "h", Reason: ReasonFailure, Detail: "d", Status: StatusOpen}
	if !strings.HasPrefix(w.ID, "hw-20261008T120000Z-") {
		t.Fatalf("id %q", w.ID)
	}
	enc, err := w.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, errs := WarningsFromConfig(map[string]string{WarningKey(w.ID): enc, "other.key": "x"})
	if len(errs) != 0 || len(got) != 1 || got[0].Hook != "h" {
		t.Fatalf("%v %v", got, errs)
	}
	_, errs = WarningsFromConfig(map[string]string{WarningKey("hw-1"): "{not json", WarningKey("hw-2"): enc})
	if len(errs) != 2 {
		t.Fatalf("an unreadable or key-mismatched record must be reported, got %v", errs)
	}
}

func TestRunMarksStoreWhileHookRunsAndAncestorCheckSeesIt(t *testing.T) {
	beadsDir := t.TempDir()
	def := Definition{Name: "m", Path: filepath.Join(beadsDir, DefaultHooksDirName, "m.toml"),
		Timeout: 5 * time.Second, Run: `ls ` + beadsDir}

	// While a hook runs, a marker naming this process exists in the store dir.
	res := Run(context.Background(), def, Payload{Event: EventCreate})
	if !strings.Contains(res.Stdout, RunningPrefix) {
		t.Fatalf("store dir should hold a %s marker while the hook runs, saw %q", RunningPrefix, res.Stdout)
	}
	// ...and it is gone afterwards.
	if m, _ := filepath.Glob(filepath.Join(beadsDir, RunningPrefix+"*")); len(m) != 0 {
		t.Fatalf("marker not released: %v", m)
	}
	if AncestorRunningHook(beadsDir) {
		t.Fatal("no hook is running, so no ancestor can be")
	}

	// A marker naming one of this process's ancestors means "a hook is running above me".
	marker := filepath.Join(beadsDir, RunningPrefix+strconv.Itoa(os.Getppid()))
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !AncestorRunningHook(beadsDir) {
		t.Fatal("a live ancestor with a hook running must be detected")
	}
	// A marker naming a live non-ancestor (pid 1 is never our parent chain's
	// hook-runner) does not count.
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(beadsDir, RunningPrefix+"1")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if AncestorRunningHook(beadsDir) {
		t.Fatal("an unrelated process's marker must not block this one")
	}
	// A stale marker (dead pid) is swept.
	stale := filepath.Join(beadsDir, RunningPrefix+"999999")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = AncestorRunningHook(beadsDir)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale marker should be removed, stat err=%v", err)
	}
}
