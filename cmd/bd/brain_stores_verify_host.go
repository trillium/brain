package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// 'brain stores verify-host' answers one question for the machine it runs on:
// will a store command here mint beads under that store's own prefix?
//
// A store and its id prefix are one and the same (the prefix is what makes a
// bead part of a store), so a host that mints task beads as lifespan-… is
// misconfigured, not merely untidy. The causes are all host-side and all
// silent until a bead lands in the wrong store: bd is stock rather than the
// brain build (stock bd mints from the database-wide issue_prefix, which on the
// shared database is the template store's seed), the registry is missing, a
// registered store has no wrapper, or a wrapper's store mints under another
// prefix. This command reads each of those and exits non-zero naming exactly
// which, so a drift check (dotfiles, a scheduler) can run it with --json.

// Verification check names, stable for JSON consumers.
const (
	verifyCheckBdBuild  = "bd-build"
	verifyCheckRegistry = "registry"
	verifyCheckWrapper  = "wrapper"
	verifyCheckMint     = "mint-prefix"
)

var (
	verifyHostJSON    bool
	verifyHostTimeout time.Duration
)

// verifyHostRegistryPaths are the registry locations a healthy host carries,
// relative to the home directory: the one this build reads and the shared
// name the rest of the fleet's tooling resolves.
var verifyHostRegistryPaths = []string{
	storesYamlPath,
	".config/trillium-stores/stores.yaml",
}

// verifyCheck is one finding. Status is "ok" or "fail".
type verifyCheck struct {
	Check   string `json:"check"`
	Store   string `json:"store,omitempty"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

// verifyStore is the per-store detail the mint probe produced.
type verifyStore struct {
	Name           string `json:"name"`
	Wrapper        string `json:"wrapper,omitempty"`
	WrapperFound   bool   `json:"wrapper_found"`
	ExpectedPrefix string `json:"expected_prefix"`
	MintPrefix     string `json:"mint_prefix,omitempty"`
	DBIssuePrefix  string `json:"db_issue_prefix,omitempty"`
	Probed         bool   `json:"probed"`
}

type verifyBdBuild struct {
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
	Brain   string `json:"brain,omitempty"`
	IsBrain bool   `json:"is_brain"`
}

// verifyHostReport is the --json shape.
type verifyHostReport struct {
	OK      bool          `json:"ok"`
	Home    string        `json:"home"`
	Checks  []verifyCheck `json:"checks"`
	Stores  []verifyStore `json:"stores"`
	Failing []string      `json:"failing"`
}

// verifyRunner runs a command and returns its stdout, stderr and error. It is
// the seam tests replace so no real bd or wrapper is needed.
type verifyRunner func(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)

func execVerifyRunner(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // a wrapper or bd path this command resolved itself
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = probeWaitDelay
	err := cmd.Run()
	return out.String(), errb.String(), err
}

var brainStoresVerifyHostCmd = &cobra.Command{
	Use:   "verify-host",
	Short: "Prove this host mints each store's beads under that store's own prefix",
	Long: `Verify this machine is set up so every store command mints beads under its
own store's id prefix. A store and its prefix are one and the same: a task bead
minted as lifespan-… is in the wrong store.

It fails (exit 1, one FAIL line per cause, each with its fix) when:

  - the bd that wrappers run is not the brain build (stock bd mints from the
    database-wide issue_prefix, which on the shared database is the template
    store's seed, so every store mints lifespan-…);
  - ~/.config/brain/stores.yaml or ~/.config/trillium-stores/stores.yaml is
    missing;
  - a store in the registry has no executable wrapper in ~/.local/bin;
  - a dry-run mint through a store's wrapper ('bd store-prefix probe', which
    resolves the prefix a create would use and inserts nothing) does not carry
    that store's prefix. The finding also reports the database's issue_prefix.

A store's expected prefix is the issue-prefix in its config.yaml, else its
BD_NAME, else its registry name. Only stores in the registry are checked;
backup copies of wrappers are ignored.

The probe writes nothing, so no bead is left behind. With --json it emits one
object for drift checks; the exit code is the same. Exit codes: 0 verified,
1 at least one check failed.`,
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			fmt.Fprintln(os.Stderr, "Error: cannot resolve the home directory")
			os.Exit(1)
		}
		timeout := verifyHostTimeout
		if timeout <= 0 {
			timeout = defaultStoreProbeTimeout
		}
		rep := verifyHost(context.Background(), home, timeout, execVerifyRunner)
		writeVerifyHostReport(os.Stdout, os.Stderr, rep, verifyHostJSON)
		if !rep.OK {
			os.Exit(1)
		}
	},
}

// verifyHost runs every check against home. It never returns an error: each
// failure is a finding.
func verifyHost(ctx context.Context, home string, timeout time.Duration, run verifyRunner) verifyHostReport {
	rep := verifyHostReport{Home: home, Checks: []verifyCheck{}, Stores: []verifyStore{}, Failing: []string{}}
	fail := func(check, store, msg, fix string) {
		rep.Checks = append(rep.Checks, verifyCheck{Check: check, Store: store, Status: "fail", Message: msg, Fix: fix})
	}
	pass := func(check, store, msg string) {
		rep.Checks = append(rep.Checks, verifyCheck{Check: check, Store: store, Status: "ok", Message: msg})
	}

	// 1. The registry files exist.
	registryOK := true
	for _, rel := range verifyHostRegistryPaths {
		path := filepath.Join(home, rel)
		if _, err := os.Stat(path); err != nil {
			registryOK = false
			fail(verifyCheckRegistry, "", fmt.Sprintf("%s is missing (%v)", path, simplifyPathError(err)),
				"provision this host's store registry: 'brain stores sync', or restore the file from the fleet's registry")
			continue
		}
		pass(verifyCheckRegistry, "", path+" present")
	}

	// 2 and 3. Each registered store: wrapper present, is the brain build, and mints its own prefix.
	stores, registryErr := verifyHostRegistry(home)
	if registryErr != nil {
		registryOK = false
		fail(verifyCheckRegistry, "", registryErr.Error(), "reconcile the registry files: 'brain stores sync'")
	}
	if len(stores) == 0 && registryOK {
		fail(verifyCheckRegistry, "", "the registry lists no stores", "register this host's stores: 'brain stores sync'")
	}
	for _, name := range sortedKeys(stores) {
		entry := stores[name]
		vs := verifyStore{Name: name, ExpectedPrefix: verifyExpectedPrefix(name, entry)}
		wrapper := filepath.Join(home, ".local", "bin", name)
		vs.Wrapper = wrapper
		if info, err := os.Stat(wrapper); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			fail(verifyCheckWrapper, name, fmt.Sprintf("store %q has no executable wrapper at %s", name, wrapper),
				fmt.Sprintf("generate the wrapper: 'brain stores rewrite-wrappers' (or 'brain stores sync')"))
			rep.Stores = append(rep.Stores, vs)
			continue
		}
		vs.WrapperFound = true
		pass(verifyCheckWrapper, name, wrapper+" present")

		wrapperBd := verifyBdIdentity(ctx, wrapper, timeout, run)
		if !wrapperBd.IsBrain {
			what := "reported no brain version"
			if wrapperBd.Version == "" {
				what = "did not answer 'version --json'"
			}
			fail(verifyCheckBdBuild, name,
				fmt.Sprintf("wrapper %s does not run the brain build (%s): stock bd mints from the database-wide issue_prefix, so this store mints under that prefix instead of its own", wrapper, what),
				"install the brain build and regenerate this wrapper: 'brain stores rewrite-wrappers'")
			// Stock bd has no probe verb; the build check already failed, and
			// a mint here would leave a bead, so the probe is not attempted.
			rep.Stores = append(rep.Stores, vs)
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, timeout)
		stdout, stderr, err := run(pctx, wrapper, "store-prefix", "probe", "--json")
		cancel()
		probe, perr := parseVerifyProbe(stdout)
		if perr != nil {
			detail := strings.TrimSpace(firstLines(stderr+"\n"+stdout, 3))
			if err != nil {
				detail = fmt.Sprintf("%v: %s", err, detail)
			}
			fail(verifyCheckMint, name, fmt.Sprintf("the dry-run mint through %s failed: %s", wrapper, strings.TrimSpace(detail)),
				fmt.Sprintf("run '%s store-prefix probe' to see the error", name))
			rep.Stores = append(rep.Stores, vs)
			continue
		}
		vs.Probed = true
		vs.MintPrefix, vs.DBIssuePrefix = probe.MintPrefix, probe.DBIssuePrefix
		switch {
		case probe.Error != "":
			fail(verifyCheckMint, name, fmt.Sprintf("a dry-run mint through %s was refused: %s (database issue_prefix %q)", wrapper, probe.Error, probe.DBIssuePrefix),
				fmt.Sprintf("run '%s store-prefix probe' to see the refusal", name))
		case probe.Owned != nil && !*probe.Owned:
			fail(verifyCheckMint, name, fmt.Sprintf("a dry-run mint through %s carries unowned prefix %q (database issue_prefix %q)", wrapper, probe.MintPrefix, probe.DBIssuePrefix),
				fmt.Sprintf("claim prefix %q for %s with 'bd store-prefix add %s'", probe.MintPrefix, name, probe.MintPrefix))
		case probe.MintPrefix != vs.ExpectedPrefix:
			fail(verifyCheckMint, name, fmt.Sprintf("a dry-run mint through %s carries prefix %q, want %q (database issue_prefix %q)", wrapper, probe.MintPrefix, vs.ExpectedPrefix, probe.DBIssuePrefix),
				fmt.Sprintf("install the brain build and reprovision this wrapper (BD_NAME=%s); the database issue_prefix is shared state and is not changed here", probe.Store))
		default:
			pass(verifyCheckMint, name, fmt.Sprintf("a dry-run mint through %s carries prefix %q (database issue_prefix %q)", wrapper, probe.MintPrefix, probe.DBIssuePrefix))
		}
		rep.Stores = append(rep.Stores, vs)
	}

	seen := map[string]bool{}
	rep.OK = true
	for _, c := range rep.Checks {
		if c.Status != "fail" {
			continue
		}
		rep.OK = false
		key := c.Check
		if c.Store != "" {
			key += ":" + c.Store
		}
		if !seen[key] {
			seen[key] = true
			rep.Failing = append(rep.Failing, key)
		}
	}
	return rep
}

// verifyBdIdentity asks bd for its identity. Only the brain build carries the
// "brain" key in 'version --json'.
func verifyBdIdentity(ctx context.Context, bdPath string, timeout time.Duration, run verifyRunner) verifyBdBuild {
	b := verifyBdBuild{Path: bdPath}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout, _, err := run(cctx, bdPath, "version", "--json")
	if err != nil && strings.TrimSpace(stdout) == "" {
		return b
	}
	payload, err := unwrapVerifyJSON(stdout)
	if err != nil {
		return b
	}
	var v struct {
		Version string `json:"version"`
		Brain   string `json:"brain"`
	}
	if json.Unmarshal(payload, &v) != nil {
		return b
	}
	b.Version, b.Brain = v.Version, v.Brain
	b.IsBrain = v.Brain != ""
	return b
}

func verifyHostRegistry(home string) (map[string]storeEntry, error) {
	all := make(map[string]storeEntry)
	var errs []error
	for _, rel := range verifyHostRegistryPaths {
		stores, found, err := readStoresFile(filepath.Join(home, rel))
		if !found || err != nil {
			if err != nil {
				errs = append(errs, err)
			}
			continue
		}
		for name, entry := range stores {
			if prior, exists := all[name]; exists && prior != entry {
				errs = append(errs, fmt.Errorf("registry entries for store %q disagree", name))
				continue
			}
			all[name] = entry
		}
	}
	return all, errors.Join(errs...)
}

// verifyExpectedPrefix is the prefix a store's beads must carry: the
// issue-prefix of its config.yaml, else its BD_NAME, else the registry name.
// The prefix does not always equal the name (ideas mints idea-…), so the
// name is only the last resort.
func verifyExpectedPrefix(name string, entry storeEntry) string {
	data, err := os.ReadFile(filepath.Join(expandPath(entry.Path), "config.yaml")) //nolint:gosec
	if err == nil {
		var cfg map[string]any
		if yaml.Unmarshal(data, &cfg) == nil {
			for _, key := range []string{"issue-prefix", "BD_NAME"} {
				if v, ok := cfg[key].(string); ok && strings.TrimSpace(v) != "" {
					return strings.TrimSuffix(strings.TrimSpace(v), "-")
				}
			}
		}
	}
	return name
}

type verifyProbeResult struct {
	Store         string `json:"store"`
	MintPrefix    string `json:"mint_prefix"`
	DBIssuePrefix string `json:"db_issue_prefix"`
	Owned         *bool  `json:"owned"`
	Error         string `json:"error"`
}

// parseVerifyProbe decodes 'store-prefix probe --json'. A refusal exits
// non-zero but still prints the JSON, so the body, not the exit code, decides.
func parseVerifyProbe(stdout string) (verifyProbeResult, error) {
	var p verifyProbeResult
	payload, err := unwrapVerifyJSON(stdout)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return p, fmt.Errorf("unreadable output: %w", err)
	}
	return p, nil
}

func unwrapVerifyJSON(stdout string) (json.RawMessage, error) {
	if strings.TrimSpace(stdout) == "" {
		return nil, errors.New("no output")
	}
	payload := json.RawMessage(stdout)
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("unreadable output: %w", err)
	}
	if len(envelope.Data) != 0 {
		payload = envelope.Data
	}
	return payload, nil
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " | ")
}

func simplifyPathError(err error) string {
	if errors.Is(err, os.ErrNotExist) {
		return "no such file"
	}
	return err.Error()
}

// writeVerifyHostReport renders the report: JSON to stdout, or per-check text
// to stderr (matching 'stores doctor') plus a FAILING line a scheduler can split.
func writeVerifyHostReport(stdout, stderr io.Writer, rep verifyHostReport, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
		return
	}
	for _, c := range rep.Checks {
		label := "ok  "
		if c.Status == "fail" {
			label = "FAIL"
		}
		_, _ = fmt.Fprintf(stderr, "%s  %-11s %s\n", label, c.Check, c.Message)
		if c.Status == "fail" && c.Fix != "" {
			_, _ = fmt.Fprintf(stderr, "      fix: %s\n", c.Fix)
		}
	}
	if rep.OK {
		_, _ = fmt.Fprintf(stderr, "\nHost verified: %d stores mint under their own prefix.\n", len(rep.Stores))
		return
	}
	_, _ = fmt.Fprintf(stderr, "\nHost NOT verified. FAILING CHECKS: %s\n", strings.Join(rep.Failing, " "))
}

func init() {
	brainStoresVerifyHostCmd.Flags().BoolVar(&verifyHostJSON, "json", false, "Emit one structured JSON object (for drift checks); the exit code is unchanged")
	brainStoresVerifyHostCmd.Flags().DurationVar(&verifyHostTimeout, "timeout", defaultStoreProbeTimeout, "Per-command time limit")
	brainStoresCmd.AddCommand(brainStoresVerifyHostCmd)
}
