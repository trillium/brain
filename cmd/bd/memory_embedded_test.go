//go:build cgo

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// bdRemember runs "bd remember" with the given args and returns stdout.
func bdRemember(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"remember"}, args...)
	cmd := exec.Command(bd, fullArgs...)
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	stdout, stderr, err := runCommandBuffers(t, cmd)
	if err != nil {
		t.Fatalf("bd remember %s failed: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// bdRememberFail runs "bd remember" expecting failure.
func bdRememberFail(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"remember"}, args...)
	cmd := exec.Command(bd, fullArgs...)
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected bd remember %s to fail, but succeeded:\n%s", strings.Join(args, " "), out)
	}
	return string(out)
}

// bdRecall runs "bd recall" with the given args and returns stdout.
func bdRecall(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"recall"}, args...)
	cmd := exec.Command(bd, fullArgs...)
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	stdout, stderr, err := runCommandBuffers(t, cmd)
	if err != nil {
		t.Fatalf("bd recall %s failed: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// bdRecallFail runs "bd recall" expecting failure.
func bdRecallFail(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"recall"}, args...)
	cmd := exec.Command(bd, fullArgs...)
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected bd recall %s to fail, but succeeded:\n%s", strings.Join(args, " "), out)
	}
	return string(out)
}

// bdMemories runs "bd memories" with the given args and returns stdout.
func bdMemories(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"memories"}, args...)
	cmd := exec.Command(bd, fullArgs...)
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	stdout, stderr, err := runCommandBuffers(t, cmd)
	if err != nil {
		t.Fatalf("bd memories %s failed: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// bdForget runs "bd forget" with the given args and returns stdout.
func bdForget(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"forget"}, args...)
	cmd := exec.Command(bd, fullArgs...)
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	stdout, stderr, err := runCommandBuffers(t, cmd)
	if err != nil {
		t.Fatalf("bd forget %s failed: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// bdForgetFail runs "bd forget" expecting failure.
func bdForgetFail(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"forget"}, args...)
	cmd := exec.Command(bd, fullArgs...)
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected bd forget %s to fail, but succeeded:\n%s", strings.Join(args, " "), out)
	}
	return string(out)
}

func TestEmbeddedMemory(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "tm")

	// ===== Remember and Recall =====

	t.Run("remember_and_recall", func(t *testing.T) {
		bdRemember(t, bd, dir, "always run tests with -race flag")
		// Recall using auto-generated key — list memories to find it
		out := bdMemories(t, bd, dir)
		if !strings.Contains(out, "always run tests with -race flag") {
			t.Errorf("expected memory in list: %s", out)
		}
	})

	t.Run("remember_with_key", func(t *testing.T) {
		bdRemember(t, bd, dir, "auth module uses JWT not sessions", "--key", "auth-jwt")
		out := bdRecall(t, bd, dir, "auth-jwt")
		if !strings.Contains(out, "JWT") {
			t.Errorf("expected 'JWT' in recall output: %s", out)
		}
	})

	t.Run("remember_overwrite", func(t *testing.T) {
		bdRemember(t, bd, dir, "first version", "--key", "overwrite-test")
		bdRemember(t, bd, dir, "second version", "--key", "overwrite-test")
		out := bdRecall(t, bd, dir, "overwrite-test")
		if !strings.Contains(out, "second version") {
			t.Errorf("expected 'second version' after overwrite: %s", out)
		}
	})

	// ===== Memories List =====

	t.Run("memories_list", func(t *testing.T) {
		out := bdMemories(t, bd, dir)
		// Should show at least the memories we stored above
		if !strings.Contains(out, "auth-jwt") {
			t.Errorf("expected 'auth-jwt' in memories list: %s", out)
		}
	})

	t.Run("memories_search", func(t *testing.T) {
		bdRemember(t, bd, dir, "dolt phantom DBs hide in three places", "--key", "dolt-phantoms")
		out := bdMemories(t, bd, dir, "dolt")
		if !strings.Contains(out, "dolt") {
			t.Errorf("expected 'dolt' related memory in search: %s", out)
		}
	})

	t.Run("memories_search_no_match", func(t *testing.T) {
		out := bdMemories(t, bd, dir, "zzz_nonexistent_term_xyz")
		// Should succeed but show no results or empty
		_ = out
	})

	// ===== Forget =====

	t.Run("forget", func(t *testing.T) {
		bdRemember(t, bd, dir, "temporary memory to forget", "--key", "forget-me")
		out := bdRecall(t, bd, dir, "forget-me")
		if !strings.Contains(out, "temporary memory") {
			t.Fatalf("expected memory before forget: %s", out)
		}
		bdForget(t, bd, dir, "forget-me")
		// After forget, recall should fail
		bdRecallFail(t, bd, dir, "forget-me")
	})

	// ===== Error Cases =====

	t.Run("recall_missing_key", func(t *testing.T) {
		bdRecallFail(t, bd, dir, "nonexistent-key-xyz")
	})

	t.Run("forget_missing_key", func(t *testing.T) {
		bdForgetFail(t, bd, dir, "nonexistent-key-xyz")
	})

	t.Run("remember_no_args", func(t *testing.T) {
		bdRememberFail(t, bd, dir)
	})

	t.Run("recall_no_args", func(t *testing.T) {
		bdRecallFail(t, bd, dir)
	})

	t.Run("forget_no_args", func(t *testing.T) {
		bdForgetFail(t, bd, dir)
	})

	// ===== robots-eq8z: remember must not read like a created issue =====

	// The old success line was "Remembered [some-slug]: ...", which agents read
	// as "issue created, ID = some-slug". They then fed the slug to show/tag,
	// got "no issue found", and reported silent data loss on a memory that had
	// in fact persisted. The output has to say memory and name the read command.
	t.Run("remember_output_disambiguates_from_issue_id", func(t *testing.T) {
		out := bdRemember(t, bd, dir, "slug shape must not look like an issue id", "--key", "eq8z-shape")
		// The key is labelled a memory and paired with the read command;
		// the issue ID is named separately as the one comment/tag/show take.
		// Both identifiers appear so neither can be mistaken for the other.
		for _, want := range []string{"memory [eq8z-shape]", "memories eq8z-shape", "use this ID for"} {
			if !strings.Contains(out, want) {
				t.Errorf("remember output missing %q:\n%s", want, out)
			}
		}
	})

	// A memory key pasted into an issue verb must point back at the memory
	// rather than dead-ending on "no issue found".
	// --no-bead is the case that still has nothing in the issue table, and the
	// case every memory written before minting existed is in.
	t.Run("issue_verb_on_memory_key_points_at_memory", func(t *testing.T) {
		bdRemember(t, bd, dir, "keys resolve nowhere in the issue table", "--key", "eq8z-resolve", "--no-bead")
		cmd := exec.Command(bd, "show", "eq8z-resolve")
		cmd.Dir = dir
		cmd.Env = bdEnv(dir)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected 'bd show <memory-key>' to fail:\n%s", out)
		}
		for _, want := range []string{"stored memory key", "memories eq8z-resolve"} {
			if !strings.Contains(string(out), want) {
				t.Errorf("show output missing %q:\n%s", want, out)
			}
		}
	})

	// `search` is where the reporter looked for the lost insight; a bare
	// "No issues found" there is what made the memory look gone. A memory with
	// a companion issue is now found by the ordinary search path, so the hint
	// is exercised where it is still the only thing standing between the
	// reader and a false "No issues found": a --no-bead memory.
	t.Run("search_miss_surfaces_matching_memory", func(t *testing.T) {
		bdRemember(t, bd, dir, "zqxjv distinctive body text", "--key", "eq8z-search", "--no-bead")
		out := bdSearch(t, bd, dir, "zqxjv")
		for _, want := range []string{"stored memory", "eq8z-search"} {
			if !strings.Contains(out, want) {
				t.Errorf("search output missing %q:\n%s", want, out)
			}
		}
	})

	// With a companion issue, the insight is a first-class search hit -- no
	// hint needed, because the text is in the issues table.
	t.Run("search_finds_remembered_insight_directly", func(t *testing.T) {
		bdRemember(t, bd, dir, "wqbtz distinctive body text", "--key", "b1ic-search")
		out := bdSearch(t, bd, dir, "wqbtz")
		if !strings.Contains(out, "wqbtz distinctive body text") {
			t.Errorf("search did not find the remembered insight:\n%s", out)
		}
	})

	// The hint is scoped to real matches: an unrelated miss stays quiet.
	t.Run("search_miss_without_memory_match_stays_quiet", func(t *testing.T) {
		out := bdSearch(t, bd, dir, "qqqnosuchtermanywhere")
		if strings.Contains(out, "stored memory") {
			t.Errorf("unmatched search should not mention memories:\n%s", out)
		}
	})

	// ===== robots-b1ic: a remembered insight must be taggable =====

	// The whole point of the fix: the documented flag-for-review recipe --
	// remember the finding, then tag it 'human' -- has to run end to end using
	// only what remember printed. Before this, step two dead-ended and the
	// finding never reached 'human list'.
	t.Run("remembered_insight_reaches_human_queue", func(t *testing.T) {
		bdRemember(t, bd, dir, "b1ic finding worth a human's eyes", "--key", "b1ic-flag")
		// Tag by the memory key -- the identifier remember hands back.
		bdLabel(t, bd, dir, "add", "b1ic-flag", "human")
		out := bdCommand(t, bd, dir, "human", "list")
		if !strings.Contains(out, "b1ic finding worth a human") {
			t.Errorf("tagged memory did not reach the human queue:\n%s", out)
		}
	})

	// comment and show take the memory key too, via the same alias.
	t.Run("comment_and_show_accept_memory_key", func(t *testing.T) {
		bdRemember(t, bd, dir, "b1ic evidence needs a comment", "--key", "b1ic-comment")
		bdCommand(t, bd, dir, "comment", "b1ic-comment", "the supporting evidence")
		out := bdShowRaw(t, bd, dir, "b1ic-comment")
		for _, want := range []string{"b1ic evidence needs a comment", "the supporting evidence"} {
			if !strings.Contains(out, want) {
				t.Errorf("show on memory key missing %q:\n%s", want, out)
			}
		}
	})

	// Re-remembering a key must update the one issue, not accumulate a new one
	// per write -- otherwise comments and labels scatter across duplicates.
	t.Run("remember_update_reuses_the_same_issue", func(t *testing.T) {
		first := rememberBeadID(t, bd, dir, "b1ic first text", "--key", "b1ic-update")
		second := rememberBeadID(t, bd, dir, "b1ic second text", "--key", "b1ic-update")
		if first == "" {
			t.Fatalf("remember reported no issue ID")
		}
		if first != second {
			t.Fatalf("remember minted a second issue on update: %s then %s", first, second)
		}
		out := bdShowRaw(t, bd, dir, first)
		if !strings.Contains(out, "b1ic second text") {
			t.Errorf("issue was not updated to the new text:\n%s", out)
		}
	})

	t.Run("no_bead_skips_the_issue", func(t *testing.T) {
		id := rememberBeadID(t, bd, dir, "b1ic memory without an issue", "--key", "b1ic-nobead", "--no-bead")
		if id != "" {
			t.Fatalf("--no-bead still minted issue %s", id)
		}
		out := bdRecall(t, bd, dir, "b1ic-nobead")
		if !strings.Contains(out, "without an issue") {
			t.Errorf("--no-bead dropped the memory itself:\n%s", out)
		}
	})

	// forget drops the memory and the link; the issue survives, because it may
	// carry comments and labels that removing a config row must not destroy.
	t.Run("forget_keeps_the_issue", func(t *testing.T) {
		id := rememberBeadID(t, bd, dir, "b1ic memory to forget", "--key", "b1ic-forget")
		if id == "" {
			t.Fatalf("remember reported no issue ID")
		}
		out := bdForget(t, bd, dir, "b1ic-forget")
		if !strings.Contains(out, id) {
			t.Errorf("forget output did not name the surviving issue %s:\n%s", id, out)
		}
		shown := bdShowRaw(t, bd, dir, id)
		if !strings.Contains(shown, "b1ic memory to forget") {
			t.Errorf("forget destroyed issue %s:\n%s", id, shown)
		}
		// The key no longer resolves: its memory and its link are both gone.
		bdShowFail(t, bd, dir, "b1ic-forget")
	})
}

// rememberBeadID runs "bd remember --json" and returns the ID of the issue it
// minted ("" when --no-bead was passed).
func rememberBeadID(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	out := bdRemember(t, bd, dir, append(args, "--json")...)
	// map[string]any, not map[string]string: the envelope carries a numeric
	// schema_version alongside the string fields.
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("remember --json returned unparseable output: %v\n%s", err, out)
	}
	if msg, _ := payload["bead_error"].(string); msg != "" {
		t.Fatalf("remember could not mint an issue: %s", msg)
	}
	id, _ := payload["bead"].(string)
	return id
}

// TestEmbeddedMemoryConcurrent exercises memory operations concurrently.
func TestEmbeddedMemoryConcurrent(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "mx")

	// Disable auto-export: this test exercises concurrent memory
	// flock contention, not export behavior. With export.auto=true
	// (the default since GH#2973), 8 concurrent writers also trigger
	// post-write read paths that race with in-flight commits.
	//
	// The underlying race is not flock-level (flock already serializes
	// bd subprocesses) but engine-shutdown-level: Dolt's working-set
	// persistence can lag behind flock release, so the next subprocess
	// sometimes commits with a stale view and overwrites a prior forget.
	// See GH#3260 for the investigation (PR #3269 was the CI probe).
	// Fix would require synchronous flush on engine close or a long-lived
	// engine per store; both are substantial work. Until then, this
	// workaround keeps the test deterministic.
	disableAutoExport := exec.Command(bd, "config", "set", "export.auto", "false")
	disableAutoExport.Dir = dir
	disableAutoExport.Env = bdEnv(dir)
	if out, err := disableAutoExport.CombinedOutput(); err != nil {
		t.Fatalf("disable export.auto: %v\n%s", err, out)
	}

	const numWorkers = 8

	type workerResult struct {
		worker int
		err    error
	}

	results := make([]workerResult, numWorkers)
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	for w := 0; w < numWorkers; w++ {
		go func(worker int) {
			defer wg.Done()
			r := workerResult{worker: worker}

			// Each worker stores memories with unique keys
			for i := 0; i < 3; i++ {
				key := fmt.Sprintf("w%d-mem%d", worker, i)
				content := fmt.Sprintf("worker %d memory %d content", worker, i)

				cmd := exec.Command(bd, "remember", content, "--key", key)
				cmd.Dir = dir
				cmd.Env = bdEnv(dir)
				out, err := cmd.CombinedOutput()
				if err != nil {
					r.err = fmt.Errorf("remember %s: %v\n%s", key, err, out)
					results[worker] = r
					return
				}
			}

			// Recall each memory
			for i := 0; i < 3; i++ {
				key := fmt.Sprintf("w%d-mem%d", worker, i)
				expected := fmt.Sprintf("worker %d memory %d content", worker, i)

				cmd := exec.Command(bd, "recall", key)
				cmd.Dir = dir
				cmd.Env = bdEnv(dir)
				out, err := cmd.CombinedOutput()
				if err != nil {
					r.err = fmt.Errorf("recall %s: %v\n%s", key, err, out)
					results[worker] = r
					return
				}
				if !strings.Contains(string(out), expected) {
					r.err = fmt.Errorf("recall %s: expected %q, got %q", key, expected, string(out))
					results[worker] = r
					return
				}
			}

			// Forget one memory
			forgetKey := fmt.Sprintf("w%d-mem0", worker)
			cmd := exec.Command(bd, "forget", forgetKey)
			cmd.Dir = dir
			cmd.Env = bdEnv(dir)
			out, err := cmd.CombinedOutput()
			if err != nil {
				r.err = fmt.Errorf("forget %s: %v\n%s", forgetKey, err, out)
				results[worker] = r
				return
			}

			// List memories
			cmd = exec.Command(bd, "memories")
			cmd.Dir = dir
			cmd.Env = bdEnv(dir)
			out, err = cmd.CombinedOutput()
			if err != nil {
				r.err = fmt.Errorf("memories: %v\n%s", err, out)
				results[worker] = r
				return
			}

			results[worker] = r
		}(w)
	}
	wg.Wait()

	for _, r := range results {
		if r.err != nil && !strings.Contains(r.err.Error(), "one writer at a time") {
			t.Errorf("worker %d failed: %v", r.worker, r.err)
		}
	}

	// Verify memories only for workers that succeeded (err==nil).
	// With exclusive flock, some workers may fail with "one writer at a time".
	out := bdMemories(t, bd, dir)
	var successCount int
	for _, r := range results {
		if r.err != nil {
			continue
		}
		successCount++
		w := r.worker
		forgottenKey := fmt.Sprintf("w%d-mem0", w)
		if strings.Contains(out, forgottenKey) {
			t.Errorf("expected %s to be forgotten", forgottenKey)
		}
		for i := 1; i < 3; i++ {
			key := fmt.Sprintf("w%d-mem%d", w, i)
			if !strings.Contains(out, key) {
				t.Errorf("expected %s to still exist in memories", key)
			}
		}
	}
	if successCount == 0 {
		t.Fatal("expected at least 1 worker to succeed")
	}
}
