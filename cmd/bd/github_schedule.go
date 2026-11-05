package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The schedule is a macOS launchd job that runs 'github sync' for one store on
// an interval. It is off unless someone runs 'github schedule install', it is
// per store (the label names the store), and removing it leaves pairings and
// synced items alone. Where launchd is not available, 'github sync --watch'
// does the same job in the foreground, and the same command line fits a cron
// entry.

const githubScheduleLabelPrefix = "dev.brain.github-sync."

var (
	githubScheduleInterval time.Duration
)

var githubScheduleCmd = &cobra.Command{
	Use:   "schedule",
	Short: "Run 'github sync' for this store on a timer (off by default)",
	Long: `Manage a background job that runs 'github sync' for this store.

  brain github schedule print      show the launchd job without installing it
  brain github schedule install    write the job and load it (macOS launchd)
  brain github schedule status     say whether the job is installed and loaded
  brain github schedule remove     unload and delete the job

The job runs the store's own command, so it uses the store's pairing and
direction. It needs a recorded pairing ('github pair'), and the GitHub login
the sync uses (GITHUB_TOKEN or the gh CLI's login) must be available to a
background process. Nothing is scheduled until 'install' is run.

Without launchd, 'brain github sync --watch --interval 10m' keeps syncing in a
terminal, and the same command line works in a cron entry.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
}

var githubSchedulePrintCmd = &cobra.Command{
	Use:           "print",
	Short:         "Show the launchd job without installing it",
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		job, err := newGitHubScheduleJob()
		if err != nil {
			return HandleError("%v", err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "# %s\n%s", job.PlistPath, job.Plist())
		return nil
	},
}

var githubScheduleInstallCmd = &cobra.Command{
	Use:           "install",
	Short:         "Write the launchd job and load it",
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runGitHubScheduleInstall,
}

var githubScheduleRemoveCmd = &cobra.Command{
	Use:           "remove",
	Short:         "Unload and delete the launchd job",
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runGitHubScheduleRemove,
}

var githubScheduleStatusCmd = &cobra.Command{
	Use:           "status",
	Short:         "Say whether the launchd job is installed and loaded",
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runGitHubScheduleStatus,
}

func init() {
	githubCmd.AddCommand(githubScheduleCmd)
	githubScheduleCmd.AddCommand(githubSchedulePrintCmd, githubScheduleInstallCmd, githubScheduleRemoveCmd, githubScheduleStatusCmd)
	for _, c := range []*cobra.Command{githubSchedulePrintCmd, githubScheduleInstallCmd} {
		c.Flags().DurationVar(&githubScheduleInterval, "interval", 15*time.Minute, "Time between syncs (minimum 1m)")
	}
}

// githubScheduleJob is one store's launchd job.
type githubScheduleJob struct {
	Store     string
	Label     string
	PlistPath string
	LogPath   string
	Command   []string
	Interval  time.Duration
	Path      string
}

// newGitHubScheduleJob describes the job for the store this run addresses.
func newGitHubScheduleJob() (*githubScheduleJob, error) {
	name := currentStoreName()
	if name == "" {
		return nil, fmt.Errorf("cannot tell which store this is: run through the store's command (for example 'brain github schedule ...') so BD_NAME is set")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	if githubScheduleInterval < time.Minute {
		return nil, fmt.Errorf("--interval must be at least 1m")
	}
	return buildGitHubScheduleJob(name, home, githubScheduleInterval, os.Getenv("PATH"), os.Executable)
}

// buildGitHubScheduleJob assembles the job. The command is the store's own
// wrapper in ~/.local/bin when there is one, because the wrapper carries the
// store's environment (which database, which namespace); otherwise the running
// executable.
func buildGitHubScheduleJob(store, home string, interval time.Duration, path string, executable func() (string, error)) (*githubScheduleJob, error) {
	if !validScheduleStoreName(store) {
		return nil, fmt.Errorf("store name %q cannot be used in a job label", store)
	}
	command := filepath.Join(home, ".local", "bin", store)
	if _, err := os.Stat(command); err != nil {
		exe, exeErr := executable()
		if exeErr != nil {
			return nil, exeErr
		}
		command = exe
	}
	label := githubScheduleLabelPrefix + store
	return &githubScheduleJob{
		Store:     store,
		Label:     label,
		PlistPath: filepath.Join(home, "Library", "LaunchAgents", label+".plist"),
		LogPath:   filepath.Join(home, "Library", "Logs", "brain-github-sync-"+store+".log"),
		Command:   []string{command, "github", "sync"},
		Interval:  interval,
		Path:      path,
	}, nil
}

func validScheduleStoreName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// Plist renders the launchd property list.
func (j *githubScheduleJob) Plist() string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + xmlEscape(j.Label) + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range j.Command {
		b.WriteString("\t\t<string>" + xmlEscape(a) + "</string>\n")
	}
	fmt.Fprintf(&b, `	</array>
	<key>StartInterval</key>
	<integer>%d</integer>
	<key>RunAtLoad</key>
	<false/>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>%s</string>
	</dict>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, int(j.Interval.Seconds()), xmlEscape(j.Path), xmlEscape(j.LogPath), xmlEscape(j.LogPath))
	return b.String()
}

func requireLaunchd() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("launchd is macOS-only; use 'github sync --watch' or a cron entry running 'github sync' instead")
	}
	return nil
}

func launchdDomain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func runGitHubScheduleInstall(cmd *cobra.Command, args []string) error {
	if err := requireLaunchd(); err != nil {
		return HandleError("%v", err)
	}
	if err := ensureStoreActive(); err != nil {
		return HandleError("database not available: %v", err)
	}
	ctx := context.Background()
	pairing, err := loadGitHubPairing(func(key string) string { return getGitHubConfigValue(ctx, key) }, currentStoreName())
	if err != nil {
		return HandleError("%v", err)
	}
	if !pairing.Recorded {
		return HandleError("this store has no recorded pairing; run 'github pair <owner/repo>' first so the job knows what to sync")
	}
	job, err := newGitHubScheduleJob()
	if err != nil {
		return HandleError("%v", err)
	}
	if err := os.MkdirAll(filepath.Dir(job.PlistPath), 0o755); err != nil {
		return HandleError("%v", err)
	}
	if err := os.MkdirAll(filepath.Dir(job.LogPath), 0o755); err != nil {
		return HandleError("%v", err)
	}
	if err := os.WriteFile(job.PlistPath, []byte(job.Plist()), 0o644); err != nil { //nolint:gosec // launchd reads agent plists as the user; same mode launchctl expects
		return HandleError("%v", err)
	}
	// Reload cleanly if an older copy is loaded.
	_ = exec.Command("launchctl", "bootout", launchdDomain()+"/"+job.Label).Run()
	if out, err := exec.Command("launchctl", "bootstrap", launchdDomain(), job.PlistPath).CombinedOutput(); err != nil {
		return HandleError("launchctl bootstrap failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Scheduled %s <-> %s (%s) every %s\nJob: %s\nLog: %s\nRemove with: brain github schedule remove\n",
		pairing.Store, pairing.Repository(), pairing.Direction, job.Interval, job.PlistPath, job.LogPath)
	return nil
}

func runGitHubScheduleRemove(cmd *cobra.Command, args []string) error {
	githubScheduleInterval = time.Minute // not used; keeps newGitHubScheduleJob's check satisfied
	job, err := newGitHubScheduleJob()
	if err != nil {
		return HandleError("%v", err)
	}
	if runtime.GOOS == "darwin" {
		_ = exec.Command("launchctl", "bootout", launchdDomain()+"/"+job.Label).Run()
	}
	if err := os.Remove(job.PlistPath); err != nil && !os.IsNotExist(err) {
		return HandleError("%v", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Schedule removed for %s. Pairing and synced items are unchanged.\n", job.Store)
	return nil
}

func runGitHubScheduleStatus(cmd *cobra.Command, args []string) error {
	githubScheduleInterval = time.Minute
	job, err := newGitHubScheduleJob()
	if err != nil {
		return HandleError("%v", err)
	}
	out := cmd.OutOrStdout()
	if _, err := os.Stat(job.PlistPath); err != nil {
		_, _ = fmt.Fprintf(out, "Not scheduled (no %s)\n", job.PlistPath)
		return nil
	}
	loaded := false
	if runtime.GOOS == "darwin" {
		loaded = exec.Command("launchctl", "print", launchdDomain()+"/"+job.Label).Run() == nil
	}
	_, _ = fmt.Fprintf(out, "Installed: %s\nLoaded:    %t\nLog:       %s\n", job.PlistPath, loaded, job.LogPath)
	return nil
}
