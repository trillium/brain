package main

// The default server address of a store command.
//
// Every machine keeps the Dolt sql-server its stores live on in one file,
// ~/.config/brain/store-host.env (see internal/configfile/storehost.go). Until
// now only a shell read it: each store wrapper sourced the file before exec'ing
// bd, so a wrapper that did not carry that line fell back to whatever host its
// store's metadata.json pinned. This file makes bd read it itself, so the same
// binary and the same three-line wrapper work on every machine and the address
// is one file a machine's configuration can own.
//
// The order, most specific first:
//
//	--store-host host[:port]            this invocation
//	BEADS_DOLT_SERVER_HOST / _PORT      the process environment
//	~/.config/brain/store-host.env      this machine's default
//	metadata.json, config.yaml, 127.0.0.1   what bd resolved before
//
// It is applied by exporting the winning value into the process environment
// before anything resolves a server. Every reader of the address (the storage
// mode, the dial host and port, the credentials lookup, a child bd) already
// honors those variables, so they cannot disagree about which layer won, and
// the file behaves exactly as it does when a wrapper sources it.
//
// The file is a default for store commands only. A bare bd in a project
// directory never reads it, and neither does a store that runs its own embedded
// Dolt, so the federation's address cannot repoint an unrelated database.

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/configfile"
)

const storeHostFlagName = "store-host"

// storeHostFlag is --store-host: the server for this one invocation.
var storeHostFlag string

// storeHostLayers is everything the choice depends on, so tests can supply it
// without touching the real home directory or process environment.
type storeHostLayers struct {
	FlagSet  bool
	Flag     string
	Getenv   func(string) string
	ReadFile func() (configfile.StoreHostDefaults, bool, error)
	// ServerStore reports whether the store being opened is server-backed.
	ServerStore func() bool
}

// envAssignment is one variable the chosen layer exports.
type envAssignment struct{ Key, Value string }

// storeHostPlan is what planStoreHost decided: the variables to export, and a
// warning when the shared file is there but partly unusable.
type storeHostPlan struct {
	Set     []envAssignment
	Warning string
}

// parseStoreHostFlag splits --store-host into a host and an optional port.
// A bare IPv6 address is a host; "[::1]:3307" carries a port.
func parseStoreHostFlag(v string) (host, port string, err error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", "", fmt.Errorf("--%s cannot be empty; omit the flag to use %s or the machine's default", storeHostFlagName, configfile.StoreHostKeyHost)
	}
	if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
		v = v[1 : len(v)-1]
		if v == "" {
			return "", "", fmt.Errorf("--%s \"[]\" names no host", storeHostFlagName)
		}
		return v, "", nil
	}
	if strings.Count(v, ":") != 1 && !strings.HasPrefix(v, "[") {
		return v, "", nil
	}
	host, port, err = net.SplitHostPort(v)
	if err != nil || host == "" {
		return "", "", fmt.Errorf("--%s %q is not host or host:port", storeHostFlagName, v)
	}
	if p, perr := strconv.Atoi(port); perr != nil || p <= 0 || p > 65535 {
		return "", "", fmt.Errorf("--%s %q: %q is not a valid port", storeHostFlagName, v, port)
	}
	return host, port, nil
}

// storeCommandReadsHostFile reports whether this invocation takes its default
// server from the shared file: a store command (the wrapper exported BD_NAME)
// on a server-backed store.
func storeCommandReadsHostFile(in storeHostLayers) bool {
	if strings.TrimSpace(in.Getenv("BD_NAME")) == "" {
		return false
	}
	if in.Getenv("BEADS_DOLT_SERVER_MODE") == "1" {
		return true
	}
	if v := in.Getenv("BEADS_DOLT_SHARED_SERVER"); v == "1" || strings.EqualFold(v, "true") {
		return true
	}
	return in.ServerStore != nil && in.ServerStore()
}

// beadsDirIsServerStore reports whether dir/metadata.json says dolt_mode=server,
// the shape every store on the shared server has. It reads the file as it is:
// configfile.Load would migrate a legacy config, which is not this check's to do.
func beadsDirIsServerStore(beadsDir string) bool {
	if beadsDir == "" {
		return false
	}
	data, err := os.ReadFile(configfile.ConfigPath(beadsDir)) // #nosec G304 -- the store's own metadata.json
	if err != nil {
		return false
	}
	var meta struct {
		Backend  string `json:"backend"`
		DoltMode string `json:"dolt_mode"`
	}
	if json.Unmarshal(data, &meta) != nil {
		return false
	}
	if meta.Backend != "" && meta.Backend != configfile.BackendDolt {
		return false
	}
	return strings.EqualFold(meta.DoltMode, configfile.DoltModeServer)
}

// planStoreHost applies the layers in order. The flag overrides the
// environment; the file then fills only what neither set, key by key, the way
// a generated wrapper's run-time block lets a process env host or port win
// over the file. An absent file changes nothing.
func planStoreHost(in storeHostLayers) (storeHostPlan, error) {
	var plan storeHostPlan
	set := map[string]string{}
	assign := func(key, value string) {
		set[key] = value
		plan.Set = append(plan.Set, envAssignment{key, value})
	}
	current := func(key string) string {
		if v, ok := set[key]; ok {
			return v
		}
		return in.Getenv(key)
	}

	if in.FlagSet {
		host, port, err := parseStoreHostFlag(in.Flag)
		if err != nil {
			return storeHostPlan{}, err
		}
		assign(configfile.StoreHostKeyHost, host)
		if port != "" {
			assign(configfile.StoreHostKeyPort, port)
		}
	}

	if in.ReadFile == nil || !storeCommandReadsHostFile(in) {
		return plan, nil
	}
	file, found, err := in.ReadFile()
	if !found {
		if err != nil {
			plan.Warning = fmt.Sprintf("%v; this machine's default store host was not applied", err)
		}
		return plan, nil
	}
	if err != nil {
		plan.Warning = fmt.Sprintf("%s; ignored", strings.ReplaceAll(err.Error(), "\n", "; "))
	}
	for _, kv := range []envAssignment{
		{configfile.StoreHostKeyHost, file.Host},
		{configfile.StoreHostKeyPort, file.Port},
		{configfile.StoreHostKeyUser, file.User},
	} {
		if kv.Value != "" && current(kv.Key) == "" {
			assign(kv.Key, kv.Value)
		}
	}
	return plan, nil
}

// applyStoreHostDefault runs the plan against the real process. The root
// pre-run calls it before any command resolves a server.
func applyStoreHostDefault(cmd *cobra.Command) error {
	in := storeHostLayers{
		Getenv:      os.Getenv,
		ReadFile:    configfile.ReadStoreHostDefaults,
		ServerStore: func() bool { return beadsDirIsServerStore(os.Getenv("BEADS_DIR")) },
	}
	if cmd != nil && cmd.Root() != nil {
		if f := cmd.Root().PersistentFlags().Lookup(storeHostFlagName); f != nil && f.Changed {
			in.FlagSet, in.Flag = true, storeHostFlag
		}
	}
	plan, err := planStoreHost(in)
	if err != nil {
		return err
	}
	if plan.Warning != "" {
		fmt.Fprintf(os.Stderr, "beads: %s\n", plan.Warning)
	}
	for _, kv := range plan.Set {
		if err := os.Setenv(kv.Key, kv.Value); err != nil {
			return fmt.Errorf("exporting %s: %w", kv.Key, err)
		}
	}
	return nil
}
