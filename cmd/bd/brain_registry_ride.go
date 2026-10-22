package main

// The ride-along: the registry epoch travels with every command. Each bd/brain
// command already connects to the shared database, so reading the epoch is one
// primary-key row. If this machine's last-applied epoch is behind, the
// idempotent sync runs inline and the command continues. Offline or failing,
// it never blocks the command: it warns at most once an hour. The 5-minute
// launchd poller stays as the backstop for machines nobody is typing on.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/storeregistry"
)

const (
	rideAlongReadTimeout = 2 * time.Second
	rideAlongSyncTimeout = 60 * time.Second
	rideAlongWarnEvery   = time.Hour
	rideAlongRetryAfter  = 5 * time.Minute
)

// registryRideAlongSkipped reports whether the ride-along must not run for cmd:
// the registry commands themselves (a sync must never trigger a sync, and a
// doctor must observe, not repair), help and completion.
func registryRideAlongSkipped(cmd *cobra.Command) bool {
	if cmd == nil {
		return true
	}
	if strings.EqualFold(os.Getenv(envRegistryOff), "off") || os.Getenv(envRegistrySyncing) != "" {
		return true
	}
	if registryPolicy() == "off" {
		return true
	}
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case "sync", "verify", "doctor", "registry", "reconcile", "poller", "shell-hook", "epoch":
			if c.Parent() != nil && c.Parent().Name() == "stores" {
				return true // these manage or observe the registry themselves
			}
			if c.Name() == "registry" || c.Name() == "poller" {
				return true
			}
		case "__complete", "__completeNoDesc", "completion", "help", "version", "prime", "runbook":
			return true
		}
	}
	return false
}

const registryPolicyPath = ".config/brain/registry.policy"

// registryPolicy is the ride-along policy: auto (default), notify or off. The
// environment wins over the per-machine file ~/.config/brain/registry.policy.
func registryPolicy() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(envRegistryPolicy)))
	if v == "" {
		if data, err := os.ReadFile(homeFile(registryPolicyPath)); err == nil {
			v = strings.ToLower(strings.TrimSpace(string(data)))
		}
	}
	switch v {
	case "notify", "off":
		return v
	}
	return "auto"
}

// registryRideAlong is called once per command, after the store is open.
func registryRideAlong(ctx context.Context, cmd *cobra.Command) {
	if registryRideAlongSkipped(cmd) || store == nil {
		return
	}
	db, err := registryDBFunc()
	if err != nil || db == nil {
		return // not a shared-server store: nothing to compare against
	}
	rideAlongWith(ctx, dbSource{db: db}, db, os.Stderr)
}

// rideAlongWith is the testable core: compare, and sync when behind.
func rideAlongWith(ctx context.Context, src registrySource, q storeregistry.DBTX, errOut interface{ Write([]byte) (int, error) }) {
	readCtx, cancel := context.WithTimeout(ctx, rideAlongReadTimeout)
	epoch, digest, found, err := storeregistry.ReadEpoch(readCtx, q)
	cancel()
	if err != nil {
		warnRegistryOnce(errOut, "store registry unreachable ("+firstErrLine(err)+"); continuing with the local store list")
		return
	}
	if !found {
		return
	}
	applied, _ := readAppliedState()
	if applied.Epoch == epoch && applied.Digest == digest {
		return
	}
	if recentRegistryFailure() {
		return // a sync just failed here; the poller retries, commands must not keep paying for it
	}
	if registryPolicy() == "notify" {
		warnRegistryOnce(errOut, fmt.Sprintf("store list changed (epoch %d -> %d): run 'brain stores sync'", applied.Epoch, epoch))
		return
	}
	fmt.Fprintf(errOut, "store list changed (epoch %d -> %d): running brain stores sync\n", applied.Epoch, epoch)
	syncCtx, cancelSync := context.WithTimeout(ctx, rideAlongSyncTimeout)
	defer cancelSync()
	rep, err := runRegistrySync(syncCtx, syncOptions{Source: src, Trigger: "ride-along"})
	switch {
	case err != nil:
		markRegistryFailure()
		warnRegistryOnce(errOut, "store registry sync failed ("+firstErrLine(err)+"); continuing")
	case rep.Status == "partial":
		warnRegistryOnce(errOut, fmt.Sprintf("store registry sync incomplete (%d item(s) still missing); run 'brain stores sync' to see why", len(rep.Missing)+len(rep.Failed)))
	}
}

func firstErrLine(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// warnRegistryOnce prints msg unless a warning went out within the last hour.
func warnRegistryOnce(w interface{ Write([]byte) (int, error) }, msg string) {
	path := homeFile(registryWarnedPath)
	if path != "" {
		if st, err := os.Stat(path); err == nil && time.Since(st.ModTime()) < rideAlongWarnEvery {
			if data, _ := os.ReadFile(path); strings.TrimSpace(string(data)) == msg {
				return
			}
		}
		_ = os.MkdirAll(dirOf(path), 0o755)
		_ = os.WriteFile(path, []byte(msg+"\n"), 0o644) //nolint:gosec
	}
	fmt.Fprintf(w, "brain: %s\n", msg)
}

func dirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "."
}

const registryFailedPath = ".config/brain/registry.failed"

func markRegistryFailure() {
	if p := homeFile(registryFailedPath); p != "" {
		_ = os.MkdirAll(dirOf(p), 0o755)
		_ = os.WriteFile(p, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644) //nolint:gosec
	}
}

func recentRegistryFailure() bool {
	p := homeFile(registryFailedPath)
	st, err := os.Stat(p)
	return err == nil && time.Since(st.ModTime()) < rideAlongRetryAfter
}

func clearRegistryFailure() {
	if p := homeFile(registryFailedPath); p != "" {
		_ = os.Remove(p)
	}
}
