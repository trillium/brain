package main

// Markdown ownership. The store markdown under ~/data/<store>/ is a derived
// view that exactly one machine writes: the brain host. A machine whose
// wrappers reach a Dolt server on another machine exports BRAIN_NO_EXFIL=1
// (see syncWrapperBody), and the host re-renders every store on a timer:
//
//	brain host render                    render every store's markdown now
//	brain host render-job print|install|uninstall   the launchd job that does it

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	renderJobLabel       = "com.brain.host-render"
	renderJobDefaultSecs = 300
)

// effectiveStoreHost is the Dolt host a store wrapper on this machine ends up
// using: store-host.env wins over the baked default, as it does in the wrapper;
// fallback is the process environment, then "" (the loopback default).
func effectiveStoreHost(home, envHost string) string {
	f, err := os.Open(filepath.Join(home, ".config", "brain", "store-host.env")) //nolint:gosec // our own config file
	if err == nil {
		defer f.Close()
		host := ""
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimPrefix(strings.TrimSpace(sc.Text()), "export ")
			if v, ok := strings.CutPrefix(line, "BEADS_DOLT_SERVER_HOST="); ok {
				host = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
		if host != "" {
			return host
		}
	}
	return envHost
}

// hostIsLocal reports whether host names this machine: empty, loopback, this
// machine's hostname or one of its own interface addresses.
func hostIsLocal(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	switch host {
	case "", "localhost", "::1":
		return true
	}
	if name, err := os.Hostname(); err == nil {
		short, _, _ := strings.Cut(strings.ToLower(name), ".")
		if host == short || host == strings.ToLower(name) {
			return true
		}
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

type hostRenderResult struct {
	Store string `json:"store"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// renderStoresViaWrappers runs '<store> render-all' through each store's own
// wrapper (so pins, host and credentials are the ones every other command
// uses). One failing store never stops the rest.
func renderStoresViaWrappers(ctx context.Context, home string, stores []string, perStore time.Duration) []hostRenderResult {
	bin := filepath.Join(home, ".local", "bin")
	results := make([]hostRenderResult, 0, len(stores))
	for _, name := range stores {
		res := hostRenderResult{Store: name}
		wrapper := filepath.Join(bin, name)
		if st, err := os.Stat(wrapper); err != nil || st.Mode()&0o111 == 0 {
			res.Error = "no executable wrapper at " + wrapper
			results = append(results, res)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, perStore)
		cmd := exec.CommandContext(cctx, wrapper, "render-all") //nolint:gosec // wrapper path is built from our own stores.yaml names
		cmd.Env = append(os.Environ(), "BRAIN_REGISTRY=off")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			res.Error = err.Error()
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				res.Error += ": " + lastOutputLine(msg)
			}
		} else {
			res.OK = true
		}
		cancel()
		results = append(results, res)
	}
	return results
}

func lastOutputLine(s string) string {
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// hostRenderStoreNames lists the stores this machine renders: the server-kind
// stores in stores.yaml (aliases are the same data under a second name).
func hostRenderStoreNames() ([]string, error) {
	reg, err := loadStoresRegistry()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(reg))
	for name := range reg {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

var (
	hostRenderQuiet   bool
	hostRenderTimeout time.Duration
)

var brainHostRenderCmd = &cobra.Command{
	Use:   "render",
	Short: "Render every store's markdown on the host (clients never write it)",
	Long: `Markdown under ~/data/<store>/ is a derived view of the database that only the
brain host writes. A machine whose wrappers reach a Dolt server on another
machine runs with BRAIN_NO_EXFIL=1, so this command refuses to run there.

It runs '<store> render-all' through each store's wrapper, one store at a time;
a store that fails is reported and the rest still render. The host role carries
a launchd job that runs it every 5 minutes (see 'brain host render-job').`,
	Args:        cobra.NoArgs,
	Annotations: hostAnno(),
	RunE: func(cmd *cobra.Command, _ []string) error {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		if h := effectiveStoreHost(home, os.Getenv("BEADS_DOLT_SERVER_HOST")); !hostIsLocal(h) {
			return fmt.Errorf("this machine reaches the Dolt server on %s: the host renders the markdown, run this there", h)
		}
		names, err := hostRenderStoreNames()
		if err != nil {
			return err
		}
		failed := 0
		for _, r := range renderStoresViaWrappers(cmd.Context(), home, names, hostRenderTimeout) {
			switch {
			case !r.OK:
				failed++
				fmt.Fprintf(os.Stderr, "%s: render failed: %s\n", r.Store, r.Error)
			case !hostRenderQuiet:
				fmt.Printf("%s: rendered\n", r.Store)
			}
		}
		if failed > 0 {
			return fmt.Errorf("%d of %d stores failed to render", failed, len(names))
		}
		return nil
	},
}

// renderJobPlist renders the launch agent that runs 'brain host render'.
func renderJobPlist(home, wrapper string, interval int) (string, error) {
	if wrapper == "" {
		wrapper = filepath.Join(home, ".local", "bin", "brain")
	}
	return renderLaunchdJobPlist(renderJobLabel, home, wrapper,
		[]string{"host", "render", "--quiet"}, interval, "host-render.log")
}

func renderJobPlistFile() string {
	return homeFile(filepath.Join(pollerPlistDirPath, renderJobLabel+".plist"))
}

var (
	renderJobWrapper  string
	renderJobInterval int
	renderJobNoLoad   bool
)

var brainHostRenderJobCmd = &cobra.Command{
	Use:         "render-job",
	Short:       "The launchd job that renders store markdown on the host (print, install, uninstall)",
	Annotations: hostAnno(),
	Run:         func(cmd *cobra.Command, _ []string) { _ = cmd.Help() },
}

var brainHostRenderJobPrintCmd = &cobra.Command{
	Use: "print", Short: "Print the launch agent plist (writes nothing)", Args: cobra.NoArgs, Annotations: hostAnno(),
	RunE: func(cmd *cobra.Command, _ []string) error {
		home, _ := os.UserHomeDir()
		body, err := renderJobPlist(home, renderJobWrapper, renderJobInterval)
		if err != nil {
			return err
		}
		fmt.Print(body)
		return nil
	},
}

var brainHostRenderJobInstallCmd = &cobra.Command{
	Use: "install", Short: "Install the job, load it and declare it part of the host role", Args: cobra.NoArgs, Annotations: hostAnno(),
	RunE: func(cmd *cobra.Command, _ []string) error {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		body, err := renderJobPlist(home, renderJobWrapper, renderJobInterval)
		if err != nil {
			return err
		}
		if runtime.GOOS != "darwin" && !renderJobNoLoad {
			return fmt.Errorf("launchd is macOS only; use --no-load to just write the plist")
		}
		logDir := filepath.Join(home, pollerLogDirPath)
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", logDir, err)
		}
		plist := renderJobPlistFile()
		if err := writeFileAtomic(plist, []byte(body), 0o644); err != nil { //nolint:gosec // LaunchAgent plists are world-readable by convention
			return fmt.Errorf("writing %s: %w", plist, err)
		}
		if err := editHostConfig(func(cfg *hostConfig) error {
			if !containsStr(cfg.Services, renderJobLabel) {
				cfg.Services = append(cfg.Services, renderJobLabel)
				sort.Strings(cfg.Services)
			}
			return nil
		}); err != nil {
			return err
		}
		if renderJobNoLoad {
			fmt.Printf("wrote %s (not loaded)\n", plist)
			return nil
		}
		domain := "gui/" + strconv.Itoa(os.Getuid())
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "launchctl", "bootout", domain+"/"+renderJobLabel).Run() // reload cleanly
		if out, err := exec.CommandContext(ctx, "launchctl", "bootstrap", domain, plist).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl bootstrap failed: %w: %s", err, strings.TrimSpace(string(out)))
		}
		fmt.Printf("installed %s (every %ds and at load); it moves with the host role\n", renderJobLabel, renderJobInterval)
		return nil
	},
}

var brainHostRenderJobUninstallCmd = &cobra.Command{
	Use: "uninstall", Short: "Unload and remove the job and stop carrying it with the host role", Args: cobra.NoArgs, Annotations: hostAnno(),
	RunE: func(cmd *cobra.Command, _ []string) error {
		if !renderJobNoLoad && runtime.GOOS == "darwin" {
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			_ = exec.CommandContext(ctx, "launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+renderJobLabel).Run()
		}
		if err := os.Remove(renderJobPlistFile()); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := editHostConfig(func(cfg *hostConfig) error {
			cfg.Services = removeString(cfg.Services, renderJobLabel)
			return nil
		}); err != nil {
			return err
		}
		fmt.Println("render job removed")
		return nil
	},
}

func init() {
	brainHostRenderCmd.Flags().BoolVar(&hostRenderQuiet, "quiet", false, "Print only failures")
	brainHostRenderCmd.Flags().DurationVar(&hostRenderTimeout, "timeout", 5*time.Minute, "Longest one store may take")
	for _, c := range []*cobra.Command{brainHostRenderJobPrintCmd, brainHostRenderJobInstallCmd} {
		c.Flags().StringVar(&renderJobWrapper, "wrapper", "", "Absolute path of the brain wrapper the job runs (default ~/.local/bin/brain)")
		c.Flags().IntVar(&renderJobInterval, "interval", renderJobDefaultSecs, "Seconds between runs")
	}
	brainHostRenderJobInstallCmd.Flags().BoolVar(&renderJobNoLoad, "no-load", false, "Write the plist but do not run launchctl")
	brainHostRenderJobUninstallCmd.Flags().BoolVar(&renderJobNoLoad, "no-load", false, "Remove the plist without running launchctl")
	brainHostRenderJobCmd.AddCommand(brainHostRenderJobPrintCmd, brainHostRenderJobInstallCmd, brainHostRenderJobUninstallCmd)
	brainHostCmd.AddCommand(brainHostRenderCmd, brainHostRenderJobCmd)
}
