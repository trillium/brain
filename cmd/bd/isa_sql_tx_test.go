package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
)

// rawSQLVerbPaths are the verbs that reach the store through the raw-SQL
// seam (sqlTxRunnerFor) rather than the typed storage surface. task-t57uw
// was every one of them failing on a fresh embedded store.
var rawSQLVerbPaths = []string{
	"brain new",
	"patch",
	"isa-section",
	"isa-show",
	"isa-by-slug",
	"isa-list",
	"isa-render",
	"isa-render-all",
	"isa-render-pending",
}

// TestRawSQLVerbsStayRefusedOnProxiedServer pins the proxied half of the
// seam: the proxied provider exposes neither RawDBAccessor nor SQLTxRunner,
// and these verbs are refused at the front door on every proxied topology,
// before a store is built, so the seam is never reached there. The gate
// reports the refusal (to stderr, or JSON on stdout) and returns the exit
// code, which is why only the row carries the code to check.
func TestRawSQLVerbsStayRefusedOnProxiedServer(t *testing.T) {
	topologies := []ProxyTopology{ProxyTopologyManagedLocal, ProxyTopologyExternalTCP, ProxyTopologyExternalUnix}
	for _, path := range rawSQLVerbPaths {
		t.Run(path, func(t *testing.T) {
			cmd, _, err := rootCmd.Find(strings.Fields(path))
			if err != nil {
				t.Fatalf("rootCmd.Find(%q): %v", path, err)
			}
			if got := commandRegistryPath(cmd); got != path {
				t.Fatalf("rootCmd.Find(%q) resolved to %q", path, got)
			}
			row, ok := LookupCapabilityRow(path, "")
			if !ok {
				t.Fatalf("%q has no proxied capability row", path)
			}
			for _, topology := range topologies {
				rule := row.ruleFor(topology)
				if rule.Outcome != ProxyOutcomeRefused || rule.Code != "proxy.brain.unsupported" {
					t.Fatalf("%q on %s: outcome=%q code=%q, want refused with proxy.brain.unsupported", path, topology, rule.Outcome, rule.Code)
				}
				if err := validateProxyRegistryBeforeProvider(cmd, topology); err == nil {
					t.Fatalf("%q on %s: the front-door gate permitted the command", path, topology)
				}
			}
		})
	}
}

// seamlessStore stands in for a provider with neither raw-SQL seam.
type seamlessStore struct{ storage.DoltStorage }

// TestSQLTxRunnerForRefusesStoresWithoutASeam pins the error the verbs
// report if a provider without the seam ever reaches them: a typed sentinel
// the callers wrap, not a panic, and not the old "raw DB access" wording that
// pointed at the server-only *sql.DB.
func TestSQLTxRunnerForRefusesStoresWithoutASeam(t *testing.T) {
	for name, st := range map[string]storage.DoltStorage{
		"nil store":          nil,
		"store without seam": storage.NewHookFiringStore(&seamlessStore{}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			runner, err := sqlTxRunnerFor(st)
			if !errors.Is(err, errNoRawSQLTx) {
				t.Fatalf("sqlTxRunnerFor = (%v, %v), want errNoRawSQLTx", runner, err)
			}
			if runner != nil {
				t.Fatalf("sqlTxRunnerFor returned a runner alongside the error: %T", runner)
			}
			if strings.Contains(err.Error(), "raw DB access") {
				t.Fatalf("error still names the server-only raw DB seam: %v", err)
			}
		})
	}
}
