package main

// The launchd poller: the backstop that keeps an idle machine converged. It
// runs 'brain stores sync' at load and every five minutes. Installing it is a
// deliberate, per-machine act (see docs/brain/STORE_REGISTRY.md); nothing in
// the build or the tests installs it.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/spf13/cobra"
)

const (
	pollerLabel        = "com.brain.stores-sync"
	pollerDefaultSecs  = 300
	pollerLogDirPath   = "Library/Logs/brain"
	pollerPlistDirPath = "Library/LaunchAgents"
)

var pollerPlistTmpl = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>{{.Label}}</string>
  <key>ProgramArguments</key>
  <array>
    <string>{{.Wrapper}}</string>
{{- range .Args}}
    <string>{{.}}</string>
{{- end}}
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>StartInterval</key>
  <integer>{{.Interval}}</integer>
  <key>ThrottleInterval</key>
  <integer>30</integer>
  <key>ProcessType</key>
  <string>Background</string>
  <key>LowPriorityIO</key>
  <true/>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>{{.Path}}</string>
  </dict>
  <key>StandardOutPath</key>
  <string>{{.Log}}</string>
  <key>StandardErrorPath</key>
  <string>{{.Log}}</string>
</dict>
</plist>
`))

type pollerSpec struct {
	Label    string
	Wrapper  string
	Args     []string
	Interval int
	Path     string
	Log      string
}

// renderPollerPlist renders the launch agent. Logs go under ~/Library/Logs/brain/.
func renderPollerPlist(home, wrapper string, interval int) (string, error) {
	if wrapper == "" {
		wrapper = filepath.Join(home, ".local", "bin", "brain")
	}
	return renderLaunchdJobPlist(pollerLabel, home, wrapper,
		[]string{"stores", "sync", "--quiet", "--trigger", "timer"}, interval, "stores-sync.log")
}

// renderLaunchdJobPlist renders a launch agent that runs wrapper with args at
// load and every interval seconds, logging to ~/Library/Logs/brain/<logName>.
func renderLaunchdJobPlist(label, home, wrapper string, args []string, interval int, logName string) (string, error) {
	if interval < 60 {
		return "", fmt.Errorf("interval %ds is too short (minimum 60)", interval)
	}
	if !filepath.IsAbs(wrapper) {
		return "", fmt.Errorf("--wrapper must be an absolute path, got %q", wrapper)
	}
	escaped := make([]string, len(args))
	for i, a := range args {
		escaped[i] = xmlEscape(a)
	}
	spec := pollerSpec{
		Label:    label,
		Wrapper:  xmlEscape(wrapper),
		Args:     escaped,
		Interval: interval,
		Path:     xmlEscape(strings.Join([]string{filepath.Join(home, ".local", "bin"), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin"}, ":")),
		Log:      xmlEscape(filepath.Join(home, pollerLogDirPath, logName)),
	}
	var b bytes.Buffer
	if err := pollerPlistTmpl.Execute(&b, spec); err != nil {
		return "", err
	}
	return b.String(), nil
}

func pollerPlistFile() string {
	return homeFile(filepath.Join(pollerPlistDirPath, pollerLabel+".plist"))
}

// pollerInstalled reports whether the launch agent file is in place.
func pollerInstalled() bool {
	p := pollerPlistFile()
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

var (
	pollerWrapper  string
	pollerInterval int
	pollerNoLoad   bool
	pollerDryRun   bool
)

var brainStoresPollerCmd = &cobra.Command{
	Use:   "poller",
	Short: "The per-machine launchd job that runs 'brain stores sync' (print, install, uninstall, status)",
	Long: `The poller is the backstop that keeps an idle machine converged: launchd runs
'brain stores sync --quiet' at load and every 5 minutes, logging under
~/Library/Logs/brain/stores-sync.log. A laptop that slept for a week converges
within one tick of waking, in one step.

  brain stores poller print        the launch agent plist, nothing written
  brain stores poller install      write ~/Library/LaunchAgents/com.brain.stores-sync.plist and load it
  brain stores poller uninstall    unload and remove it (sync is idempotent; stopping only stops catch-up)
  brain stores poller status       is it installed and loaded`,
	Run: func(cmd *cobra.Command, _ []string) { _ = cmd.Help() },
}

var brainStoresPollerPrintCmd = &cobra.Command{
	Use:   "print",
	Short: "Print the launch agent plist (writes nothing)",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		home, _ := os.UserHomeDir()
		body, err := renderPollerPlist(home, pollerWrapper, pollerInterval)
		if err != nil {
			FatalError("%v", err)
		}
		fmt.Print(body)
	},
}

var brainStoresPollerInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install and load the launch agent on this machine",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		home, _ := os.UserHomeDir()
		body, err := renderPollerPlist(home, pollerWrapper, pollerInterval)
		if err != nil {
			FatalError("%v", err)
		}
		wrapper := pollerWrapper
		if wrapper == "" {
			wrapper = filepath.Join(home, ".local", "bin", "brain")
		}
		plist := pollerPlistFile()
		logDir := filepath.Join(home, pollerLogDirPath)
		if pollerDryRun {
			fmt.Printf("would write %s\nwould create %s\nwould run: launchctl bootstrap gui/%d %s\n", plist, logDir, os.Getuid(), plist)
			return
		}
		if runtime.GOOS != "darwin" && !pollerNoLoad {
			FatalError("launchd is macOS only; use --no-load to just write the plist")
		}
		if st, err := os.Stat(wrapper); err != nil || st.Mode()&0o111 == 0 {
			FatalError("%s is not an executable wrapper; pass --wrapper <path to the brain wrapper>", wrapper)
		}
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			FatalError("creating %s: %v", logDir, err)
		}
		if err := writeFileAtomic(plist, []byte(body), 0o644); err != nil {
			FatalError("writing %s: %v", plist, err)
		}
		if pollerNoLoad {
			fmt.Printf("wrote %s (not loaded)\n", plist)
			return
		}
		domain := "gui/" + strconv.Itoa(os.Getuid())
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "launchctl", "bootout", domain+"/"+pollerLabel).Run() // reload cleanly
		if out, err := exec.CommandContext(ctx, "launchctl", "bootstrap", domain, plist).CombinedOutput(); err != nil {
			FatalError("launchctl bootstrap failed: %v: %s", err, strings.TrimSpace(string(out)))
		}
		fmt.Printf("installed %s (every %ds and at load); log: %s/stores-sync.log\n", pollerLabel, pollerInterval, logDir)
	},
}

var brainStoresPollerUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Unload and remove the launch agent",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		plist := pollerPlistFile()
		if pollerDryRun {
			fmt.Printf("would run: launchctl bootout gui/%d/%s and remove %s\n", os.Getuid(), pollerLabel, plist)
			return
		}
		if !pollerNoLoad && runtime.GOOS == "darwin" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = exec.CommandContext(ctx, "launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+pollerLabel).Run()
		}
		if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
			FatalError("removing %s: %v", plist, err)
		}
		fmt.Println("poller removed; 'brain stores sync' still works by hand and on every command")
	},
}

var brainStoresPollerStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Say whether the launch agent is installed and loaded",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		installed := pollerInstalled()
		loaded := false
		if runtime.GOOS == "darwin" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			loaded = exec.CommandContext(ctx, "launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+pollerLabel).Run() == nil
		}
		applied, _ := readAppliedState()
		if jsonOutput {
			_ = outputJSON(map[string]any{"installed": installed, "loaded": loaded, "label": pollerLabel, "applied_epoch": applied.Epoch, "applied_at": applied.AppliedAt})
			return
		}
		fmt.Printf("%s: installed=%v loaded=%v; last applied epoch %d at %s\n", pollerLabel, installed, loaded, applied.Epoch, applied.AppliedAt)
		if !installed || !loaded {
			os.Exit(1)
		}
	},
}

func init() {
	for _, c := range []*cobra.Command{brainStoresPollerPrintCmd, brainStoresPollerInstallCmd} {
		c.Flags().StringVar(&pollerWrapper, "wrapper", "", "Absolute path of the brain wrapper the job runs (default ~/.local/bin/brain)")
		c.Flags().IntVar(&pollerInterval, "interval", pollerDefaultSecs, "Seconds between runs")
	}
	brainStoresPollerInstallCmd.Flags().BoolVar(&pollerNoLoad, "no-load", false, "Write the plist but do not run launchctl")
	brainStoresPollerInstallCmd.Flags().BoolVar(&pollerDryRun, "dry-run", false, "Say what would be done and change nothing")
	brainStoresPollerUninstallCmd.Flags().BoolVar(&pollerNoLoad, "no-load", false, "Remove the plist without running launchctl")
	brainStoresPollerUninstallCmd.Flags().BoolVar(&pollerDryRun, "dry-run", false, "Say what would be done and change nothing")
	brainStoresPollerCmd.AddCommand(brainStoresPollerPrintCmd, brainStoresPollerInstallCmd, brainStoresPollerUninstallCmd, brainStoresPollerStatusCmd)
	brainStoresCmd.AddCommand(brainStoresPollerCmd)
}
