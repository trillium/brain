//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSubscriberScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sub.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A hanging command with a background child must time out near the timeout,
// not when the child finally exits.
func TestDeliverCommandTimeoutKillsProcessGroup(t *testing.T) {
	sub := &outboxSubscriber{Name: "slow", Command: writeSubscriberScript(t, "sleep 20 & sleep 20")}
	start := time.Now()
	err := deliverToSubscriber(context.Background(), sub, []byte(`{}`), 1*time.Second)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want a timeout error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took %s; the process group was not killed", elapsed)
	}
}

// A clean exit that leaves a background child is bounded by WaitDelay and acknowledged.
func TestDeliverCommandCleanExitWithBackgroundChildIsBounded(t *testing.T) {
	sub := &outboxSubscriber{Name: "bg", Command: writeSubscriberScript(t, "sleep 20 & exit 0")}
	start := time.Now()
	if err := deliverToSubscriber(context.Background(), sub, []byte(`{}`), 10*time.Second); err != nil {
		t.Fatalf("exit 0 must acknowledge, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Fatalf("delivery waited %s for a background child", elapsed)
	}
}

// Subscriber commands must not see store credentials.
func TestDeliverCommandEnvironmentIsScrubbed(t *testing.T) {
	t.Setenv("BEADS_DOLT_PASSWORD", "placeholder-one")
	t.Setenv("DOLT_CLI_PASSWORD", "placeholder-two")
	t.Setenv("OUTBOX_KEEP", "yes")
	out := filepath.Join(t.TempDir(), "env.txt")
	sub := &outboxSubscriber{Name: "env", Command: writeSubscriberScript(t, "env > "+out)}
	if err := deliverToSubscriber(context.Background(), sub, []byte(`{}`), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	env := string(raw)
	if strings.Contains(env, "placeholder-") {
		t.Fatalf("store credentials leaked to subscriber:\n%s", env)
	}
	if !strings.Contains(env, "OUTBOX_KEEP=yes") {
		t.Fatal("unrelated environment should be preserved")
	}
	if strings.Contains(env, "BD_INSIDE_HOOK") {
		t.Fatal("subscriber is not a hook; it must not get the hook marker")
	}
}
