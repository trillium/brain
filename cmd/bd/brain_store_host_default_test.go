package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/doltserver"
)

// storeHostTestLayers builds the planner's input from plain maps: env is the
// process environment, file the shared store-host config (nil when absent).
func storeHostTestLayers(env map[string]string, file *configfile.StoreHostDefaults, serverStore bool) storeHostLayers {
	return storeHostLayers{
		Getenv: func(k string) string { return env[k] },
		ReadFile: func() (configfile.StoreHostDefaults, bool, error) {
			if file == nil {
				return configfile.StoreHostDefaults{}, false, nil
			}
			return *file, true, nil
		},
		ServerStore: func() bool { return serverStore },
	}
}

func planAsMap(t *testing.T, in storeHostLayers) map[string]string {
	t.Helper()
	plan, err := planStoreHost(in)
	if err != nil {
		t.Fatalf("planStoreHost: %v", err)
	}
	got := map[string]string{}
	for _, kv := range plan.Set {
		got[kv.Key] = kv.Value
	}
	return got
}

func TestPlanStoreHost_Precedence(t *testing.T) {
	const (
		hostKey = configfile.StoreHostKeyHost
		portKey = configfile.StoreHostKeyPort
		userKey = configfile.StoreHostKeyUser
	)
	file := &configfile.StoreHostDefaults{Host: "file.example", Port: "3307", User: "brain_app"}

	t.Run("the file fills what the environment leaves unset", func(t *testing.T) {
		got := planAsMap(t, storeHostTestLayers(map[string]string{"BD_NAME": "task"}, file, true))
		want := map[string]string{hostKey: "file.example", portKey: "3307", userKey: "brain_app"}
		if len(got) != len(want) || got[hostKey] != want[hostKey] || got[portKey] != want[portKey] || got[userKey] != want[userKey] {
			t.Errorf("plan = %v, want %v", got, want)
		}
	})

	t.Run("the environment wins over the file, key by key", func(t *testing.T) {
		env := map[string]string{"BD_NAME": "task", hostKey: "env.example"}
		got := planAsMap(t, storeHostTestLayers(env, file, true))
		if _, set := got[hostKey]; set {
			t.Errorf("the file overrode an env host: %v", got)
		}
		if got[portKey] != "3307" || got[userKey] != "brain_app" {
			t.Errorf("plan = %v, want the file's port and user under the env host", got)
		}

		env = map[string]string{"BD_NAME": "task", hostKey: "env.example", portKey: "3399", userKey: "root"}
		if got := planAsMap(t, storeHostTestLayers(env, file, true)); len(got) != 0 {
			t.Errorf("a fully set environment must leave nothing to the file, got %v", got)
		}
	})

	t.Run("the flag wins over the environment and the file", func(t *testing.T) {
		env := map[string]string{"BD_NAME": "task", hostKey: "env.example", portKey: "3399"}
		in := storeHostTestLayers(env, file, true)
		in.FlagSet, in.Flag = true, "flag.example:3311"
		got := planAsMap(t, in)
		if got[hostKey] != "flag.example" || got[portKey] != "3311" {
			t.Errorf("plan = %v, want the flag's flag.example:3311", got)
		}
	})

	t.Run("a flag without a port leaves the port to the next layer", func(t *testing.T) {
		in := storeHostTestLayers(map[string]string{"BD_NAME": "task", portKey: "3399"}, file, true)
		in.FlagSet, in.Flag = true, "flag.example"
		got := planAsMap(t, in)
		if got[hostKey] != "flag.example" {
			t.Errorf("host = %q, want the flag's", got[hostKey])
		}
		if _, set := got[portKey]; set {
			t.Errorf("an env port must survive a host-only flag, got %v", got)
		}

		in = storeHostTestLayers(map[string]string{"BD_NAME": "task"}, file, true)
		in.FlagSet, in.Flag = true, "flag.example"
		if got := planAsMap(t, in); got[hostKey] != "flag.example" || got[portKey] != "3307" {
			t.Errorf("plan = %v, want the flag's host over the file's port", got)
		}
	})

	t.Run("the flag applies to any invocation, the file only to store commands", func(t *testing.T) {
		in := storeHostTestLayers(map[string]string{}, file, true) // bare bd: no BD_NAME
		in.FlagSet, in.Flag = true, "flag.example:3311"
		got := planAsMap(t, in)
		if got[hostKey] != "flag.example" || got[portKey] != "3311" || got[userKey] != "" {
			t.Errorf("plan = %v, want only the flag's host and port", got)
		}
	})
}

// TestPlanStoreHost_AbsentFileIsNoOp is the "nothing changes" guarantee: with
// no shared file the planner exports nothing, whatever the store looks like.
func TestPlanStoreHost_AbsentFileIsNoOp(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"store command":           {"BD_NAME": "task"},
		"store command, env host": {"BD_NAME": "task", configfile.StoreHostKeyHost: "env.example"},
		"wrapper in server mode":  {"BD_NAME": "task", "BEADS_DOLT_SERVER_MODE": "1"},
		"bare bd":                 {},
	} {
		t.Run(name, func(t *testing.T) {
			plan, err := planStoreHost(storeHostTestLayers(env, nil, true))
			if err != nil || len(plan.Set) != 0 || plan.Warning != "" {
				t.Errorf("plan = %+v err = %v, want nothing exported and no warning", plan, err)
			}
		})
	}
}

// TestPlanStoreHost_FileScope pins who reads the file. A bare bd and an
// embedded store must never be repointed at the federation's server.
func TestPlanStoreHost_FileScope(t *testing.T) {
	file := &configfile.StoreHostDefaults{Host: "file.example", Port: "3307"}
	cases := []struct {
		name        string
		env         map[string]string
		serverStore bool
		want        bool
	}{
		{"store command on a server store", map[string]string{"BD_NAME": "task"}, true, true},
		{"store command, wrapper exports server mode", map[string]string{"BD_NAME": "task", "BEADS_DOLT_SERVER_MODE": "1"}, false, true},
		{"store command, wrapper exports shared server", map[string]string{"BD_NAME": "task", "BEADS_DOLT_SHARED_SERVER": "true"}, false, true},
		{"store command on an embedded store", map[string]string{"BD_NAME": "notes"}, false, false},
		{"bare bd on a server store", map[string]string{}, true, false},
		{"bare bd in server mode", map[string]string{"BEADS_DOLT_SERVER_MODE": "1"}, true, false},
		{"blank BD_NAME is a bare bd", map[string]string{"BD_NAME": "  "}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planAsMap(t, storeHostTestLayers(tc.env, file, tc.serverStore))
			if applied := got[configfile.StoreHostKeyHost] == "file.example"; applied != tc.want {
				t.Errorf("file applied = %v, want %v (plan %v)", applied, tc.want, got)
			}
			if !tc.want && len(got) != 0 {
				t.Errorf("out of scope must export nothing, got %v", got)
			}
		})
	}
}

func TestPlanStoreHost_UnusableFileWarnsAndKeepsGoodKeys(t *testing.T) {
	env := map[string]string{"BD_NAME": "task"}

	in := storeHostTestLayers(env, nil, true)
	in.ReadFile = func() (configfile.StoreHostDefaults, bool, error) {
		return configfile.StoreHostDefaults{Host: "file.example"}, true, errors.New(`/h/store-host.env:2: BEADS_DOLT_SERVER_PORT="x" is not a valid port`)
	}
	plan, err := planStoreHost(in)
	if err != nil {
		t.Fatalf("a bad port line must not fail the command: %v", err)
	}
	if len(plan.Set) != 1 || plan.Set[0].Key != configfile.StoreHostKeyHost || plan.Set[0].Value != "file.example" {
		t.Errorf("plan.Set = %v, want only the usable host", plan.Set)
	}
	if !strings.Contains(plan.Warning, "store-host.env:2") {
		t.Errorf("warning %q must name the file and line", plan.Warning)
	}

	in.ReadFile = func() (configfile.StoreHostDefaults, bool, error) {
		return configfile.StoreHostDefaults{}, false, errors.New("reading /h/store-host.env: permission denied")
	}
	plan, err = planStoreHost(in)
	if err != nil || len(plan.Set) != 0 {
		t.Fatalf("an unreadable file exports nothing: plan=%v err=%v", plan.Set, err)
	}
	if !strings.Contains(plan.Warning, "permission denied") || !strings.Contains(plan.Warning, "not applied") {
		t.Errorf("warning %q must say the default was not applied and why", plan.Warning)
	}
}

func TestParseStoreHostFlag(t *testing.T) {
	good := []struct{ in, host, port string }{
		{"mini0", "mini0", ""},
		{"100.84.99.99:3307", "100.84.99.99", "3307"},
		{" brain.example.ts.net:3307 ", "brain.example.ts.net", "3307"},
		{"::1", "::1", ""},
		{"[::1]", "::1", ""},
		{"[::1]:3307", "::1", "3307"},
		{"fd7a:115c:a1e0::1", "fd7a:115c:a1e0::1", ""},
	}
	for _, tc := range good {
		host, port, err := parseStoreHostFlag(tc.in)
		if err != nil || host != tc.host || port != tc.port {
			t.Errorf("parseStoreHostFlag(%q) = %q, %q, %v; want %q, %q", tc.in, host, port, err, tc.host, tc.port)
		}
	}
	for _, bad := range []string{"", "   ", ":3307", "host:", "host:port", "host:0", "host:70000", "[]", "[::1]:x"} {
		if host, port, err := parseStoreHostFlag(bad); err == nil {
			t.Errorf("parseStoreHostFlag(%q) = %q, %q; want an error", bad, host, port)
		}
	}
}

func TestBeadsDirIsServerStore(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		if body != "" {
			if err := os.WriteFile(filepath.Join(dir, configfile.ConfigFileName), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	cases := []struct {
		name, body string
		want       bool
	}{
		{"server store", `{"backend":"dolt","dolt_mode":"server","dolt_database":"brain_unified"}`, true},
		{"server store, legacy empty backend", `{"dolt_mode":"Server"}`, true},
		{"embedded store", `{"backend":"dolt","dolt_mode":"embedded"}`, false},
		{"no mode", `{"backend":"dolt"}`, false},
		{"proxied server", `{"backend":"dolt","dolt_mode":"proxied-server"}`, false},
		{"another backend", `{"backend":"sqlite","dolt_mode":"server"}`, false},
		{"unparseable", `{not json`, false},
		{"no metadata.json", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := beadsDirIsServerStore(write(t, tc.body)); got != tc.want {
				t.Errorf("beadsDirIsServerStore = %v, want %v", got, tc.want)
			}
		})
	}
	if beadsDirIsServerStore("") {
		t.Error("an empty BEADS_DIR is not a server store")
	}
}

// storeHostFixture is a throwaway machine: a home with (optionally) the shared
// store-host file, and a server store whose own metadata.json pins the
// "existing default" address 127.0.0.1:3399. It clears the three variables for
// the test and restores them afterwards, so the real machine's are untouched.
func storeHostFixture(t *testing.T, fileBody string) (beadsDir string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, k := range []string{
		configfile.StoreHostKeyHost, configfile.StoreHostKeyPort, configfile.StoreHostKeyUser,
		"BEADS_DOLT_PORT", "BEADS_DOLT_SERVER_MODE", "BEADS_DOLT_SHARED_SERVER", "BD_NAME", "BEADS_DIR",
	} {
		t.Setenv(k, "")
	}
	if fileBody != "" {
		path := configfile.DefaultStoreHostPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(fileBody), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	beadsDir = filepath.Join(home, "data", "tasks", ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"backend":"dolt","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":3399,"dolt_database":"brain_unified"}`
	if err := os.WriteFile(filepath.Join(beadsDir, configfile.ConfigFileName), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_DIR", beadsDir)
	t.Setenv("BD_NAME", "task")
	return beadsDir
}

// runStoreHostDefault parses args on a root that carries the real flag and
// runs the layer, as the root pre-run does.
func runStoreHostDefault(t *testing.T, args ...string) error {
	t.Helper()
	saved := storeHostFlag
	t.Cleanup(func() { storeHostFlag = saved })
	storeHostFlag = ""
	root := &cobra.Command{Use: "bd", Run: func(*cobra.Command, []string) {}}
	root.PersistentFlags().StringVar(&storeHostFlag, storeHostFlagName, "", "")
	if err := root.ParseFlags(args); err != nil {
		t.Fatalf("parsing %v: %v", args, err)
	}
	return applyStoreHostDefault(root)
}

// resolvedServer is the address bd itself resolves for the store, through the
// same functions every command uses.
func resolvedServer(t *testing.T, beadsDir string) (host string, port int, user string) {
	t.Helper()
	cfg, err := configfile.Load(beadsDir)
	if err != nil || cfg == nil {
		t.Fatalf("loading the store's metadata.json: %v", err)
	}
	return cfg.GetDoltServerHost(), doltserver.DefaultConfig(beadsDir).Port, cfg.GetDoltServerUser()
}

// TestStoreHostDefault_ResolvedAddress drives the whole order end to end:
// flag > environment > shared file > what bd resolved before (the store's own
// metadata.json), and proves an absent file leaves that last answer alone.
func TestStoreHostDefault_ResolvedAddress(t *testing.T) {
	const fileBody = "export BEADS_DOLT_SERVER_HOST=file.example\nexport BEADS_DOLT_SERVER_PORT=3307\nexport BEADS_DOLT_SERVER_USER=brain_app\n"

	t.Run("no file: the existing default stands", func(t *testing.T) {
		beadsDir := storeHostFixture(t, "")
		if err := runStoreHostDefault(t); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{configfile.StoreHostKeyHost, configfile.StoreHostKeyPort, configfile.StoreHostKeyUser} {
			if v := os.Getenv(k); v != "" {
				t.Errorf("%s = %q with no file, want it left unset", k, v)
			}
		}
		if host, port, user := resolvedServer(t, beadsDir); host != "127.0.0.1" || port != 3399 || user != "root" {
			t.Errorf("resolved %s@%s:%d, want the store's own root@127.0.0.1:3399", user, host, port)
		}
	})

	t.Run("file: it replaces the existing default", func(t *testing.T) {
		beadsDir := storeHostFixture(t, fileBody)
		if err := runStoreHostDefault(t); err != nil {
			t.Fatal(err)
		}
		if host, port, user := resolvedServer(t, beadsDir); host != "file.example" || port != 3307 || user != "brain_app" {
			t.Errorf("resolved %s@%s:%d, want the file's brain_app@file.example:3307", user, host, port)
		}
	})

	t.Run("environment: it wins over the file", func(t *testing.T) {
		beadsDir := storeHostFixture(t, fileBody)
		t.Setenv(configfile.StoreHostKeyHost, "env.example")
		t.Setenv(configfile.StoreHostKeyPort, "3355")
		if err := runStoreHostDefault(t); err != nil {
			t.Fatal(err)
		}
		if host, port, _ := resolvedServer(t, beadsDir); host != "env.example" || port != 3355 {
			t.Errorf("resolved %s:%d, want the environment's env.example:3355", host, port)
		}
	})

	t.Run("flag: it wins over the environment and the file", func(t *testing.T) {
		beadsDir := storeHostFixture(t, fileBody)
		t.Setenv(configfile.StoreHostKeyHost, "env.example")
		t.Setenv(configfile.StoreHostKeyPort, "3355")
		if err := runStoreHostDefault(t, "--store-host", "flag.example:3311"); err != nil {
			t.Fatal(err)
		}
		if host, port, _ := resolvedServer(t, beadsDir); host != "flag.example" || port != 3311 {
			t.Errorf("resolved %s:%d, want the flag's flag.example:3311", host, port)
		}
	})

	t.Run("bare bd: the file is not read", func(t *testing.T) {
		beadsDir := storeHostFixture(t, fileBody)
		t.Setenv("BD_NAME", "")
		if err := runStoreHostDefault(t); err != nil {
			t.Fatal(err)
		}
		if host, port, _ := resolvedServer(t, beadsDir); host != "127.0.0.1" || port != 3399 {
			t.Errorf("resolved %s:%d, want the store's own 127.0.0.1:3399", host, port)
		}
	})

	t.Run("embedded store: the file is not read", func(t *testing.T) {
		beadsDir := storeHostFixture(t, fileBody)
		meta := `{"backend":"dolt","dolt_mode":"embedded"}`
		if err := os.WriteFile(filepath.Join(beadsDir, configfile.ConfigFileName), []byte(meta), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := runStoreHostDefault(t); err != nil {
			t.Fatal(err)
		}
		if v := os.Getenv(configfile.StoreHostKeyHost); v != "" {
			t.Errorf("%s = %q for an embedded store, want it left unset", configfile.StoreHostKeyHost, v)
		}
		cfg, err := configfile.Load(beadsDir)
		if err != nil || cfg == nil {
			t.Fatalf("loading metadata.json: %v", err)
		}
		if cfg.IsDoltServerMode() {
			t.Error("the federation's file must not turn an embedded store into a server store")
		}
	})

	t.Run("a bad flag is an error, nothing exported", func(t *testing.T) {
		storeHostFixture(t, fileBody)
		err := runStoreHostDefault(t, "--store-host", "host:notaport")
		if err == nil || !strings.Contains(err.Error(), "--store-host") {
			t.Fatalf("err = %v, want it to name --store-host", err)
		}
		if v := os.Getenv(configfile.StoreHostKeyHost); v != "" {
			t.Errorf("%s = %q after a refused flag, want it left unset", configfile.StoreHostKeyHost, v)
		}
	})
}

// TestStoreHostFlagIsRegistered keeps the real root command and the layer
// pointed at the same flag.
func TestStoreHostFlagIsRegistered(t *testing.T) {
	f := rootCmd.PersistentFlags().Lookup(storeHostFlagName)
	if f == nil {
		t.Fatalf("rootCmd has no --%s flag", storeHostFlagName)
	}
	if !strings.Contains(f.Usage, configfile.StoreHostFileName) {
		t.Errorf("--%s help must name %s, got %q", storeHostFlagName, configfile.StoreHostFileName, f.Usage)
	}
}
