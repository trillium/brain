package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/configfile"
)

// The store-host record is ONE file per machine — ~/.config/brain/store-host.env
// — that says which Dolt sql-server the federation lives on and where its
// credentials are resolved from. Every generated store wrapper reads it at run
// time and bd itself reads it during host resolution, so a machine, a wrapper,
// and a store's own metadata can never disagree about the server; repointing a
// machine is one file edit and no wrapper rewrite.
//
// The record NEVER holds a secret. It carries the server (host, port) and a
// credential REFERENCE (the MySQL user, the path of the password file); the
// password stays in the file that reference names, or in the environment.
// 'brain stores host' prints the record and never prints a password, and
// 'brain stores host set' refuses to write a secret into it.

var (
	storeHostJSON     bool
	storeHostProbe    time.Duration
	storeHostSetHost  string
	storeHostSetPort  int
	storeHostSetUser  string
	storeHostSetCreds string
)

// recordSecretKeys are keys that would turn the record into a secret store.
// They are refused on write and reported (by name only) on read.
var recordSecretKeys = []string{"BEADS_DOLT_PASSWORD", "DOLT_PASSWORD"}

var brainStoresHostCmd = &cobra.Command{
	Use:   "host",
	Short: "Show the one store-host record every machine reads (host, port, credential reference)",
	Long: `Show the federation's store-host record: which Dolt sql-server the stores live
on, where that value came from, and the credential REFERENCE (MySQL user and
the path of the password file) — never a password.

The record lives at ~/.config/brain/store-host.env (override the path with
BRAIN_STORE_HOST_FILE), as KEY=VALUE lines so the generated store wrappers can
source it at run time:

  BEADS_DOLT_SERVER_HOST=<host>
  BEADS_DOLT_SERVER_PORT=<port>
  BEADS_DOLT_SERVER_USER=<mysql user>          # optional
  BEADS_CREDENTIALS_FILE=<password file path>  # optional

A process environment value of the same name overrides the record. The record
is one file per machine because where the federation lives is one fact per
machine: baking it into wrappers, metadata and shell rc files is how a host's
stores drift apart and read as 'registered but does not answer'.

The server is also probed, so 'no record', 'record says somewhere dead' and
'wrong credentials' are distinguishable without opening a store.`,
	Args: cobra.NoArgs,
	Run:  runBrainStoresHost,
}

// storeHostReport is the JSON/text shape of 'brain stores host'.
type storeHostReport struct {
	File        string `json:"file"`
	Exists      bool   `json:"exists"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Source      string `json:"source,omitempty"` // "env" | "file" | "mixed"
	EnvOverride bool   `json:"env_override,omitempty"`
	// Live is the probe result for the resolved server: "answers", "refused",
	// "timeout", "unresolvable" or "unprobed".
	Live     string `json:"live"`
	ProbeMS  int64  `json:"probe_ms,omitempty"`
	User     string `json:"user"`
	UserFrom string `json:"user_from,omitempty"` // "env" | "file" | "default"
	Creds    string `json:"credentials_file,omitempty"`
	CredsOK  bool   `json:"credentials_file_exists,omitempty"`
	// Secrets names any key in the record that holds a secret and must be
	// removed. The value is never read out, printed, or returned.
	Secrets []string `json:"secrets_in_record,omitempty"`
	Fix     string   `json:"fix,omitempty"`
}

func runBrainStoresHost(_ *cobra.Command, _ []string) {
	report := readStoreHostReport()

	if storeHostJSON {
		if err := outputJSON(report); err != nil {
			FatalError("encoding report: %v", err)
		}
	} else {
		printStoreHostReport(report)
	}
	if len(report.Secrets) > 0 {
		os.Exit(1)
	}
}

// readStoreHostReport gathers the record's state: the resolved host/port and
// where each part came from, the credential reference, and whether the server
// actually answers. It reads no password and never returns one.
func readStoreHostReport() storeHostReport {
	file := configfile.DefaultStoreHostPath()
	report := storeHostReport{File: file}
	if file != "" {
		if _, err := os.Stat(file); err == nil {
			report.Exists = true
		}
	}

	// Secrets are detected by key name only: the record must not carry them.
	if values, err := configfile.ReadStoreHostValues(file); err == nil {
		for _, k := range recordSecretKeys {
			if _, ok := values[k]; ok {
				report.Secrets = append(report.Secrets, k)
			}
		}
	}
	sort.Strings(report.Secrets)

	resolved, err := configfile.Resolve()
	if err == nil {
		report.Host, report.Port, report.Source = resolved.Host, resolved.Port, resolved.Source
	} else if fileHost, filePort, ok := configfile.StoreHostFromFile(); ok {
		report.Host, report.Port, report.Source = fileHost, filePort, "file"
	}
	// An env value that disagrees with the record is worth saying out loud:
	// this shell reaches a different server than every wrapper does.
	if report.Host != "" {
		if envHost := strings.TrimSpace(os.Getenv(configfile.StoreHostKeyHost)); envHost != "" && envHost != report.Host {
			report.EnvOverride = true
		}
		if envPort := strings.TrimSpace(os.Getenv(configfile.StoreHostKeyPort)); envPort != "" && envPort != strconv.Itoa(report.Port) {
			report.EnvOverride = true
		}
	}

	ref := configfile.ResolveCredentialsRef()
	if ref.User != "" {
		report.User, report.UserFrom = ref.User, "record"
		if strings.TrimSpace(os.Getenv(configfile.StoreHostKeyUser)) != "" {
			report.UserFrom = "env"
		}
	} else {
		report.User, report.UserFrom = configfile.DefaultDoltServerUser, "default"
	}
	report.Creds, report.CredsOK = ref.CredentialsFile, ref.FileExists

	switch {
	case report.Host == "" || report.Port <= 0:
		report.Live = "unresolvable"
		report.Fix = "brain stores host set --host <host> --port <port>"
	case storeHostProbe <= 0:
		report.Live = "unprobed"
	default:
		started := time.Now()
		conn, derr := net.DialTimeout("tcp", net.JoinHostPort(report.Host, strconv.Itoa(report.Port)), storeHostProbe)
		report.ProbeMS = time.Since(started).Milliseconds()
		switch {
		case derr == nil:
			_ = conn.Close()
			report.Live = "answers"
		case isTimeout(derr):
			report.Live = "timeout"
		default:
			report.Live = "refused"
		}
		if report.Live != "answers" {
			report.Fix = fmt.Sprintf("brain stores host set --host <live host> --port <live port>  # record says %s:%d and it %s",
				report.Host, report.Port, report.Live)
		}
	}
	if !report.Exists {
		report.Fix = "brain stores host set --host <host> --port <port>"
	}
	return report
}

func isTimeout(err error) bool {
	if ne, ok := err.(net.Error); ok {
		return ne.Timeout()
	}
	return false
}

func printStoreHostReport(r storeHostReport) {
	fmt.Printf("store host record: %s\n", r.File)
	if !r.Exists {
		fmt.Printf("  missing: no record on this machine\n")
	} else {
		fmt.Printf("  host: %s\n", r.Host)
		fmt.Printf("  port: %d\n", r.Port)
		if r.Source != "" {
			fmt.Printf("  source: %s\n", r.Source)
		}
	}
	if r.Live == "answers" {
		fmt.Printf("  server: answers (%dms)\n", r.ProbeMS)
	} else {
		fmt.Printf("  server: %s\n", r.Live)
	}
	fmt.Printf("  user: %s (%s)\n", r.User, r.UserFrom)
	if r.Creds == "" {
		fmt.Printf("  credentials: not configured (password comes from the environment, or the %s file)\n",
			configfile.DefaultCredentialsPath())
	} else if r.CredsOK {
		fmt.Printf("  credentials: %s (present)\n", r.Creds)
	} else {
		fmt.Printf("  credentials: %s (MISSING)\n", r.Creds)
	}
	if r.EnvOverride {
		fmt.Printf("  WARNING: this shell's BEADS_DOLT_SERVER_HOST/PORT disagree with the record\n")
	}
	for _, s := range r.Secrets {
		fmt.Printf("  ERROR: the record defines %s — a secret does not belong in it; keep the password in the file %s names\n",
			s, configfile.StoreHostKeyCredentialsFile)
	}
	if r.Fix != "" {
		fmt.Printf("  fix: %s\n", r.Fix)
	}
}

// ensureStoreHostRecordFromEnv writes the store-host record from the process
// environment when the record is missing (or unusable) and
// BEADS_DOLT_SERVER_HOST/PORT are both set. It is how a machine being
// provisioned gets its one record: the environment already says where the
// federation lives, and this persists that as the file every wrapper reads. It
// returns the path it wrote, or "" when it had nothing to do; it never invents
// a server and never writes a secret.
func ensureStoreHostRecordFromEnv() (string, error) {
	// The decision is about the FILE, not about resolution: the environment can
	// satisfy Resolve() while the record every wrapper reads is still missing,
	// which is exactly the machine this repairs.
	if _, _, ok := configfile.StoreHostFromFile(); ok {
		return "", nil
	}
	host := strings.TrimSpace(os.Getenv(configfile.StoreHostKeyHost))
	portRaw := strings.TrimSpace(os.Getenv(configfile.StoreHostKeyPort))
	if host == "" || portRaw == "" {
		return "", nil
	}
	port, err := strconv.Atoi(portRaw)
	if err != nil || port <= 0 {
		return "", fmt.Errorf("%s=%q is not a valid port", configfile.StoreHostKeyPort, portRaw)
	}
	return writeStoreHostRecord(host, port,
		os.Getenv(configfile.StoreHostKeyUser),
		os.Getenv(configfile.StoreHostKeyCredentialsFile))
}

// storeHostRecordSummary is the one line the doctor prints about the connection
// itself: which record this machine reads, which server it names, whether that
// server answers, and (when it does not) the exact command that fixes it. It is
// how a stale local daemon looks different from a missing record.
func storeHostRecordSummary() string {
	report := readStoreHostReport()
	switch {
	case !report.Exists:
		return fmt.Sprintf("Host record: %s is MISSING — stores cannot resolve the shared server. Fix: %s",
			report.File, report.Fix)
	case report.Live == "answers":
		return fmt.Sprintf("Host record: %s → %s:%d (%s, %dms)",
			report.File, report.Host, report.Port, report.Live, report.ProbeMS)
	case report.Live == "unresolvable":
		return fmt.Sprintf("Host record: %s names no usable server. Fix: %s", report.File, report.Fix)
	default:
		return fmt.Sprintf("Host record: %s → %s:%d (%s). Fix: %s",
			report.File, report.Host, report.Port, report.Live, report.Fix)
	}
}

var brainStoresHostSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Write the store-host record (host, port, credential reference) — never a secret",
	Long: `Write the federation's one store-host record: the Dolt host and port every
store on this machine should use, plus an optional credential REFERENCE (the
MySQL user and the path of the password file).

Unrelated lines in the record are preserved, so a hand-written comment or an
extra setting survives. The file is written atomically with 0600 permissions.
Secrets are refused: pass the password file's PATH with --credentials, never a
password.`,
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		if strings.TrimSpace(storeHostSetHost) == "" {
			FatalError("--host is required")
		}
		if storeHostSetPort <= 0 {
			FatalError("--port must be a positive port number")
		}
		path, err := writeStoreHostRecord(storeHostSetHost, storeHostSetPort, storeHostSetUser, storeHostSetCreds)
		if err != nil {
			FatalError("%v", err)
		}
		if jsonOutput {
			if err := outputJSON(map[string]any{
				"file":        path,
				"host":        storeHostSetHost,
				"port":        storeHostSetPort,
				"user":        storeHostSetUser,
				"credentials": storeHostSetCreds,
			}); err != nil {
				FatalError("encoding result: %v", err)
			}
			return
		}
		fmt.Printf("wrote %s\n", path)
		fmt.Printf("  %s=%s\n", configfile.StoreHostKeyHost, storeHostSetHost)
		fmt.Printf("  %s=%d\n", configfile.StoreHostKeyPort, storeHostSetPort)
		if storeHostSetUser != "" {
			fmt.Printf("  %s=%s\n", configfile.StoreHostKeyUser, storeHostSetUser)
		}
		if storeHostSetCreds != "" {
			fmt.Printf("  %s=%s\n", configfile.StoreHostKeyCredentialsFile, storeHostSetCreds)
		}
		fmt.Printf("Converge this machine's wrappers: brain stores rewrite-wrappers\n")
	},
}

// writeStoreHostRecord writes the store-host record, preserving every line it
// was not asked to change. It refuses a secret-shaped value so a password can
// never be persisted here, and writes through a temp file + rename so a crash
// leaves either the old record or the new one, never half of one.
func writeStoreHostRecord(host string, port int, user, credsFile string) (string, error) {
	path := configfile.DefaultStoreHostPath()
	if path == "" {
		return "", fmt.Errorf("cannot resolve the store-host record path (no home directory)")
	}
	host = strings.TrimSpace(host)
	user = strings.TrimSpace(user)
	credsFile = strings.TrimSpace(credsFile)
	if host == "" || port <= 0 {
		return "", fmt.Errorf("host and port are required")
	}
	for _, v := range []struct{ name, value string }{
		{"host", host}, {"user", user}, {"credentials", credsFile},
	} {
		if looksLikeSecret(v.value) {
			return "", fmt.Errorf("refusing to write a secret into the store-host record: %s looks like a credential value; pass the password file's path with --credentials instead", v.name)
		}
	}

	values := map[string]string{
		configfile.StoreHostKeyHost: host,
		configfile.StoreHostKeyPort: strconv.Itoa(port),
	}
	if user != "" {
		values[configfile.StoreHostKeyUser] = user
	}
	if credsFile != "" {
		values[configfile.StoreHostKeyCredentialsFile] = credsFile
	}

	// Keep any line we were not asked to change: preserve order, comments and
	// settings that are not ours to drop.
	var kept []string
	written := map[string]bool{}
	if data, rerr := os.ReadFile(path); rerr == nil { //nolint:gosec // operator-owned config path
		for _, line := range strings.Split(string(data), "\n") {
			key, _, ok := configfile.ParseStoreHostLine(line)
			if ok {
				if _, mine := values[key]; mine {
					continue // replaced below
				}
				kept = append(kept, line)
				written[key] = true
				continue
			}
			if strings.TrimSpace(line) != "" {
				kept = append(kept, line)
			}
		}
	}

	var b strings.Builder
	b.WriteString("# brain federation store host — one record per machine, read at run time.\n")
	b.WriteString("# Managed by 'brain stores host set'. No secrets: the password lives in the\n")
	b.WriteString("# file BEADS_CREDENTIALS_FILE names, or in the environment.\n")
	for _, line := range kept {
		key, _, ok := configfile.ParseStoreHostLine(line)
		if ok && written[key] {
			b.WriteString(line)
			b.WriteString("\n")
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	// Deterministic order so the file reads the same on every machine.
	for _, key := range []string{
		configfile.StoreHostKeyHost,
		configfile.StoreHostKeyPort,
		configfile.StoreHostKeyUser,
		configfile.StoreHostKeyCredentialsFile,
	} {
		if v, ok := values[key]; ok {
			fmt.Fprintf(&b, "%s=%s\n", key, v)
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".store-host-*.tmp")
	if err != nil {
		return "", fmt.Errorf("creating a temp file next to %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.WriteString(b.String()); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("setting permissions on %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("closing %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("installing %s: %w", path, err)
	}
	return path, nil
}

// looksLikeSecret reports values that are obviously credentials rather than a
// host, a user name, or a path. It errs toward refusing the write: a wrong
// refusal costs an operator one flag, a leaked password costs a rotation.
func looksLikeSecret(v string) bool {
	if v == "" {
		return false
	}
	// A path, a host or a plain user name never looks like this.
	if strings.ContainsAny(v, " \t") {
		return true
	}
	lower := strings.ToLower(v)
	if strings.HasPrefix(lower, "mysql://") || strings.HasPrefix(lower, "postgres://") {
		return true
	}
	return false
}

func init() {
	brainStoresHostCmd.Flags().BoolVar(&storeHostJSON, "json", false, "Emit a structured JSON object")
	brainStoresHostCmd.Flags().DurationVar(&storeHostProbe, "probe", 2*time.Second,
		"Dial the resolved server this long to report whether it answers (0 to skip)")

	brainStoresHostSetCmd.Flags().StringVar(&storeHostSetHost, "host", "", "Dolt sql-server host (required)")
	brainStoresHostSetCmd.Flags().IntVar(&storeHostSetPort, "port", 0, "Dolt sql-server port (required)")
	brainStoresHostSetCmd.Flags().StringVar(&storeHostSetUser, "user", "",
		"MySQL user for this server (a credential reference, not a secret)")
	brainStoresHostSetCmd.Flags().StringVar(&storeHostSetCreds, "credentials", "",
		"Path of the password file the record should reference (never a password)")

	brainStoresHostCmd.AddCommand(brainStoresHostSetCmd)
	brainStoresCmd.AddCommand(brainStoresHostCmd)
}
