//go:build cgo

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hookBD runs bd in dir and returns combined output and whether it exited 0.
func hookBD(t *testing.T, bd, dir string, args ...string) (string, bool) {
	t.Helper()
	cmd := exec.Command(bd, args...)
	cmd.Dir = dir
	cmd.Env = append(bdEnv(dir), "BD_DISABLE_METRICS=1")
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

func hookTitles(t *testing.T, bd, dir string) string {
	t.Helper()
	out, ok := hookBD(t, bd, dir, "list", "--all", "--json")
	if !ok {
		t.Fatalf("bd list failed: %s", out)
	}
	return out
}

func writeHookDecl(t *testing.T, beadsDir, name, body string) {
	t.Helper()
	d := filepath.Join(beadsDir, "hooks.d")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, name+".toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hookWarnings(t *testing.T, bd, dir string) []map[string]interface{} {
	t.Helper()
	out, ok := hookBD(t, bd, dir, "hook", "warnings", "--status", "all", "--json")
	if !ok {
		t.Fatalf("bd hook warnings failed: %s", out)
	}
	var parsed struct {
		Warnings []map[string]interface{} `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &parsed); err != nil {
		t.Fatalf("unparseable warnings output: %v\n%s", err, out)
	}
	return parsed.Warnings
}

func TestEmbeddedDeclaredHooks(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "hk")

	t.Run("guard_refuses_and_bead_does_not_exist", func(t *testing.T) {
		writeHookDecl(t, beadsDir, "no-forbidden", "run = 'cat | grep -q FORBIDDEN && { echo \"no forbidden titles\" >&2; exit 2; }; exit 0'\npolicy = \"guard\"\nwhen = \"create\"\n")
		out, ok := hookBD(t, bd, dir, "create", "FORBIDDEN thing", "-t", "task")
		if ok || !strings.Contains(out, `refused by guard hook "no-forbidden"`) || !strings.Contains(out, "bead not created") {
			t.Fatalf("want a refusal naming the hook, got ok=%v: %s", ok, out)
		}
		if strings.Contains(hookTitles(t, bd, dir), "FORBIDDEN") {
			t.Fatal("refused bead exists")
		}
		if out, ok := hookBD(t, bd, dir, "create", "fine thing", "-t", "task"); !ok {
			t.Fatalf("a passing guard must allow the write: %s", out)
		}
		_ = os.Remove(filepath.Join(beadsDir, "hooks.d", "no-forbidden.toml"))
	})

	t.Run("observer_failure_fails_open_with_routable_warning", func(t *testing.T) {
		writeHookDecl(t, beadsDir, "mirror", "run = \"echo mirror down >&2; exit 3\"\npolicy = \"observer\"\nwhen = \"create\"\n")
		out, ok := hookBD(t, bd, dir, "create", "observer bead", "-t", "task")
		if !ok || !strings.Contains(out, "hook warning hw-") {
			t.Fatalf("observer failure must not block and must announce the record: ok=%v %s", ok, out)
		}
		if !strings.Contains(hookTitles(t, bd, dir), "observer bead") {
			t.Fatal("write did not happen")
		}
		var found map[string]interface{}
		for _, w := range hookWarnings(t, bd, dir) {
			if w["hook"] == "mirror" {
				found = w
			}
		}
		if found == nil || found["status"] != "open" || found["reason"] != "hook-failed" || found["subject"] == "" {
			t.Fatalf("durable warning missing or wrong: %v", found)
		}
		if out, ok := hookBD(t, bd, dir, "hook", "warnings", "--ack", found["id"].(string)); !ok {
			t.Fatalf("ack failed: %s", out)
		}
		out, _ = hookBD(t, bd, dir, "hook", "warnings")
		if strings.Contains(out, found["id"].(string)) {
			t.Fatalf("acknowledged warning still listed as open: %s", out)
		}
		_ = os.Remove(filepath.Join(beadsDir, "hooks.d", "mirror.toml"))
	})

	t.Run("unset_policy_warns", func(t *testing.T) {
		writeHookDecl(t, beadsDir, "undeclared", "run = \"exit 0\"\nwhen = \"create\"\n")
		if out, ok := hookBD(t, bd, dir, "create", "unset bead", "-t", "task"); !ok {
			t.Fatalf("unset hook must not block: %s", out)
		}
		var reasons []interface{}
		for _, w := range hookWarnings(t, bd, dir) {
			if w["hook"] == "undeclared" {
				reasons = append(reasons, w["reason"])
			}
		}
		if len(reasons) != 1 || reasons[0] != "policy-unset" {
			t.Fatalf("want one policy-unset warning, got %v", reasons)
		}
		_ = os.Remove(filepath.Join(beadsDir, "hooks.d", "undeclared.toml"))
	})

	t.Run("failing_fail_closed_guard_blocks_naming_hook", func(t *testing.T) {
		writeHookDecl(t, beadsDir, "crasher", "run = \"echo license down >&2; exit 7\"\npolicy = \"guard\"\n")
		out, ok := hookBD(t, bd, dir, "create", "never exists", "-t", "task")
		if ok || !strings.Contains(out, `"crasher"`) || !strings.Contains(out, "failed closed") {
			t.Fatalf("want fail-closed refusal naming the hook, got ok=%v: %s", ok, out)
		}
		if strings.Contains(hookTitles(t, bd, dir), "never exists") {
			t.Fatal("bead exists despite fail-closed guard")
		}
		_ = os.Remove(filepath.Join(beadsDir, "hooks.d", "crasher.toml"))
	})

	t.Run("hook_that_writes_is_refused_even_with_marker_scrubbed", func(t *testing.T) {
		attempts := map[string]string{
			"plain":    "BEADS_DIR='" + beadsDir + "' '" + bd + "' create 'hook wrote this' -t task",
			"scrubbed": "env -u BD_INSIDE_HOOK BEADS_DIR='" + beadsDir + "' '" + bd + "' create 'hook wrote this' -t task",
		}
		for name, run := range attempts {
			writeHookDecl(t, beadsDir, "sneaky", "run = \""+strings.ReplaceAll(run, `"`, `\"`)+"\"\npolicy = \"observer\"\nwhen = \"create\"\n")
			if out, ok := hookBD(t, bd, dir, "create", "trigger "+name, "-t", "task"); !ok {
				t.Fatalf("%s: %s", name, out)
			}
			if strings.Contains(hookTitles(t, bd, dir), "hook wrote this") {
				t.Fatalf("%s: a hook managed to write", name)
			}
			var detail string
			for _, w := range hookWarnings(t, bd, dir) {
				if w["hook"] == "sneaky" {
					detail, _ = w["detail"].(string)
				}
			}
			if !strings.Contains(detail, "started by a hook") {
				t.Fatalf("%s: the write attempt should be recorded as refused, detail=%q", name, detail)
			}
		}
		_ = os.Remove(filepath.Join(beadsDir, "hooks.d", "sneaky.toml"))
	})

	t.Run("unreadable_definition_refuses_writes_but_not_reads", func(t *testing.T) {
		writeHookDecl(t, beadsDir, "typo", "run = \"true\"\npolcy = \"guard\"\n")
		out, ok := hookBD(t, bd, dir, "create", "blocked", "-t", "task")
		if ok || !strings.Contains(out, "typo.toml") || !strings.Contains(out, "unknown key") {
			t.Fatalf("want a loud named refusal, got ok=%v: %s", ok, out)
		}
		if _, ok := hookBD(t, bd, dir, "list", "--all"); !ok {
			t.Fatal("reads must keep working while hooks.d is broken")
		}
		if out, ok := hookBD(t, bd, dir, "hook", "list"); ok || !strings.Contains(out, "typo.toml") {
			t.Fatalf("bd hook list must report the broken file: ok=%v %s", ok, out)
		}
		_ = os.Remove(filepath.Join(beadsDir, "hooks.d", "typo.toml"))
	})
}
