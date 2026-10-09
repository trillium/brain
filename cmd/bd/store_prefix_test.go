package main

import "testing"

// ── tests ─────────────────────────────────────────────────────────────────────

// The store-prefix surface is the runtime edge of brain_store_prefixes; the
// owned/conflict logic itself is tested at the storage layer
// (internal/storage/issueops/unified_namespaces_test.go).
func TestStorePrefixCommandRegistered(t *testing.T) {
	if _, _, err := storePrefixAddCmd.Find([]string{"add", "proto"}); err != nil {
		t.Fatalf("store-prefix add not reachable: %v", err)
	}
	if storePrefixAddCmd.Flags().Lookup("store") == nil {
		t.Fatal("--store flag missing on store-prefix add")
	}
}

// Release is the owner's own act: it proves authority through the wrapper's
// BD_NAME, and adding the claim flag's --store escape (or the overruled
// --with-beads override) back onto it would be exactly the silent path the
// design refused — asserted here so the surface cannot drift back.
func TestStorePrefixReleaseCommandRegistered(t *testing.T) {
	if _, _, err := storePrefixReleaseCmd.Find([]string{"release", "proto"}); err != nil {
		t.Fatalf("store-prefix release not reachable: %v", err)
	}
	if storePrefixReleaseCmd.Flags().Lookup("confirm") == nil {
		t.Fatal("--confirm flag missing on store-prefix release")
	}
	if storePrefixReleaseCmd.Flags().Lookup("store") != nil {
		t.Fatal("--store must not exist on store-prefix release: only the owner's namespace releases")
	}
	if storePrefixReleaseCmd.Flags().Lookup("with-beads") != nil {
		t.Fatal("--with-beads must not exist: the beads-carrying refusal is absolute (decided 2026-10-07)")
	}
	if storePrefixReleaseCmd.Flags().Lookup("reason") == nil {
		t.Fatal("--reason flag missing on store-prefix release (the event records the why)")
	}
}

func TestStorePrefixHistoryCommandRegistered(t *testing.T) {
	if _, _, err := storePrefixHistoryCmd.Find([]string{"history", "proto"}); err != nil {
		t.Fatalf("store-prefix history not reachable: %v", err)
	}
}

func TestResolveStorePrefixTargetPrefersFlag(t *testing.T) {
	t.Setenv("BD_NAME", "task")
	storePrefixTargetStore = "stories"
	defer func() { storePrefixTargetStore = "" }()
	got, err := resolveStorePrefixTarget()
	if err != nil {
		t.Fatalf("--store must win over BD_NAME: %v", err)
	}
	if got != "stories" {
		t.Fatalf("target = %q, want stories", got)
	}
}

func TestResolveStorePrefixTargetDefaultsToBDName(t *testing.T) {
	t.Setenv("BD_NAME", "task")
	defer func() { storePrefixTargetStore = "" }()
	got, err := resolveStorePrefixTarget()
	if err != nil {
		t.Fatalf("wrapper-pinned default: %v", err)
	}
	if got != "task" {
		t.Fatalf("target = %q, want task", got)
	}
}

func TestResolveStorePrefixTargetRefusesStoreless(t *testing.T) {
	t.Setenv("BD_NAME", "")
	defer func() { storePrefixTargetStore = "" }()
	if _, err := resolveStorePrefixTarget(); err == nil {
		t.Fatal("storeless store-prefix add must refuse")
	}
}
