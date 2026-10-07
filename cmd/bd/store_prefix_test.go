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
