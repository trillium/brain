package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storeregistry"
)

func TestOffHostWrapperExportsNoExfil(t *testing.T) {
	e := storeregistry.New("task")
	off := syncWrapperBody(e, shimEnv{Home: "/home/u", OffHost: true})
	if !strings.Contains(off, "export BRAIN_NO_EXFIL=1\n") {
		t.Errorf("an off-host wrapper must not render markdown:\n%s", off)
	}
	on := syncWrapperBody(e, shimEnv{Home: "/home/u"})
	if strings.Contains(on, "BRAIN_NO_EXFIL") {
		t.Errorf("the host's own wrapper keeps rendering markdown:\n%s", on)
	}
	emb := syncWrapperBody(storeregistry.Entry{Name: "emb", Kind: storeregistry.KindStore, Store: "emb",
		State: storeregistry.StateActive, RelPath: "data/emb/.beads", WrapperKind: storeregistry.WrapperEmbedded},
		shimEnv{Home: "/home/u", OffHost: true})
	if strings.Contains(emb, "BRAIN_NO_EXFIL") {
		t.Errorf("an embedded store is local data, not a client of the host:\n%s", emb)
	}
	if sameWrapper([]byte(off), []byte(on)) {
		t.Error("moving between host and client must rewrite the wrapper")
	}
	// Behaviour, not just text: the exported variable reaches the command.
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "bd"), "#!/bin/sh\necho \"exfil-off=${BRAIN_NO_EXFIL:-unset}\"\n", 0o755)
	wrapper := filepath.Join(dir, "task")
	mustWrite(t, wrapper, off, 0o755)
	cmd := exec.Command(wrapper)
	cmd.Env = []string{"HOME=" + dir, "PATH=/usr/bin:/bin", "BD_BIN=" + filepath.Join(dir, "bd")}
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "exfil-off=1" {
		t.Errorf("wrapper run: %q %v", out, err)
	}
}

func TestEffectiveStoreHostAndLocality(t *testing.T) {
	home := t.TempDir()
	if got := effectiveStoreHost(home, "10.0.0.5"); got != "10.0.0.5" {
		t.Errorf("without store-host.env the environment decides, got %q", got)
	}
	mustWrite(t, filepath.Join(home, ".config", "brain", "store-host.env"),
		"# comment\nexport BEADS_DOLT_SERVER_HOST=100.84.99.99\nexport BEADS_DOLT_SERVER_PORT=3307\nexport BEADS_DOLT_SERVER_USER=brain_app\n", 0o644)
	if got := effectiveStoreHost(home, "127.0.0.1"); got != "100.84.99.99" {
		t.Errorf("store-host.env wins over the baked default, got %q", got)
	}
	for _, h := range []string{"", "127.0.0.1", "localhost", "::1", "127.0.0.2"} {
		if !hostIsLocal(h) {
			t.Errorf("hostIsLocal(%q) = false", h)
		}
	}
	if hostIsLocal("192.0.2.1") {
		t.Error("a TEST-NET address is not this machine")
	}
}

func TestPollerPlistStillHasItsArguments(t *testing.T) {
	body, err := renderPollerPlist("/Users/someone", "", 300)
	if err != nil {
		t.Fatal(err)
	}
	want := "<string>/Users/someone/.local/bin/brain</string>\n    <string>stores</string>\n    <string>sync</string>\n    <string>--quiet</string>\n    <string>--trigger</string>\n    <string>timer</string>\n  </array>"
	if !strings.Contains(body, want) {
		t.Errorf("poller arguments changed:\n%s", body)
	}
}

func TestRenderJobPlist(t *testing.T) {
	body, err := renderJobPlist("/Users/someone", "", 300)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<string>com.brain.host-render</string>",
		"<string>/Users/someone/.local/bin/brain</string>\n    <string>host</string>\n    <string>render</string>\n    <string>--quiet</string>\n  </array>",
		"<key>StartInterval</key>\n  <integer>300</integer>",
		"/Users/someone/Library/Logs/brain/host-render.log",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plist missing %q:\n%s", want, body)
		}
	}
	if p, err := exec.LookPath("plutil"); err == nil {
		f := filepath.Join(t.TempDir(), "x.plist")
		mustWrite(t, f, body, 0o644)
		if out, err := exec.Command(p, "-lint", f).CombinedOutput(); err != nil {
			t.Errorf("plutil -lint: %v\n%s", err, out)
		}
	}
	if _, err := renderJobPlist("/Users/someone", "", 30); err == nil {
		t.Error("an interval under a minute must be refused")
	}
	t.Setenv("HOME", t.TempDir())
	if _, err := os.Stat(renderJobPlistFile()); err == nil {
		t.Error("nothing in the build or the tests installs the render job")
	}
}

func TestRenderStoresViaWrappersContinuesPastFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, ".local", "bin")
	mustWrite(t, filepath.Join(bin, "good"), "#!/bin/sh\n[ \"$1\" = render-all ] || exit 9\necho rendered >> \"$HOME/good.log\"\n", 0o755)
	mustWrite(t, filepath.Join(bin, "bad"), "#!/bin/sh\necho boom >&2\nexit 3\n", 0o755)
	mustWrite(t, filepath.Join(bin, "after"), "#!/bin/sh\necho rendered >> \"$HOME/after.log\"\n", 0o755)

	res := renderStoresViaWrappers(context.Background(), home, []string{"good", "bad", "missing", "after"}, 10*time.Second)
	ok := map[string]bool{}
	for _, r := range res {
		ok[r.Store] = r.OK
		if !r.OK && r.Error == "" {
			t.Errorf("%s failed without saying why", r.Store)
		}
	}
	if !ok["good"] || ok["bad"] || ok["missing"] || !ok["after"] {
		t.Errorf("results: %+v", res)
	}
	if !strings.Contains(res[1].Error, "boom") {
		t.Errorf("the wrapper's own message should surface: %q", res[1].Error)
	}
	if _, err := os.Stat(filepath.Join(home, "after.log")); err != nil {
		t.Error("a failing store must not stop the stores after it")
	}
}

func TestHostRenderRefusesOnAClient(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mustWrite(t, filepath.Join(home, ".config", "brain", "store-host.env"),
		"export BEADS_DOLT_SERVER_HOST=192.0.2.1\n", 0o644)
	err := brainHostRenderCmd.RunE(brainHostRenderCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "the host renders the markdown") {
		t.Errorf("a client must not render: %v", err)
	}
}

// directStubSource is a direct-role registry source that needs no database.
type directStubSource struct{ registrySource }

func (directStubSource) Role() (string, string)           { return "direct", "" }
func (directStubSource) UnifiedDB(context.Context) string { return "" }

func TestNewShimEnvMarksAMachineOffHostByItsStoreHost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BEADS_DOLT_SERVER_HOST", "")
	if env := newShimEnv(context.Background(), directStubSource{}, "m"); env.OffHost {
		t.Error("no store-host.env: the Dolt server is local")
	}
	mustWrite(t, filepath.Join(home, ".config", "brain", "store-host.env"),
		"export BEADS_DOLT_SERVER_HOST=192.0.2.1\n", 0o644)
	if env := newShimEnv(context.Background(), directStubSource{}, "m"); !env.OffHost {
		t.Error("store-host.env names another machine: this machine is off-host")
	}
}
