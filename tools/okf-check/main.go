// Command okf-check is a headless, file-based Open Knowledge Format (OKF)
// v0.1 conformance checker for brain's markdown exfil surface.
//
// It exists so a brain store's `entries/` directory can be verified in CI
// WITHOUT the Obsidian desktop app or the app-dependent official `obsidian`
// CLI (which is a client to a running desktop instance). See the runbook at
// docs/brain/OKF_OBSIDIAN_VERIFICATION.md for the interactive secondary
// check.
//
// Usage:
//
//	okf-check [--json] [path ...]
//
// Each path may be a store root, an `entries/` directory, or a single `.md`
// file. With no paths it checks the current directory. The tool walks every
// path recursively for `.md` files and validates the OKF v0.1 conformance
// requirements this checker covers:
//
//	#1 Parseable frontmatter — every non-reserved `.md` begins with a
//	   `---` … `---` YAML block that parses as YAML.
//	#2 Non-empty `type`     — that frontmatter carries a non-empty `type`.
//	#3 Reserved index.md    — if an `index.md` is present it must NOT carry
//	   a frontmatter block (OKF reserves index.md as frontmatter-free).
//
// Reserved OKF files (`index.md`, `log.md`) are exempt from the #1/#2
// `type` checks — they legitimately carry no frontmatter — but `index.md`
// is still subject to the #3 must-not-have-frontmatter check.
//
// The tool collects ALL violations (it does not stop at the first) and
// exits non-zero if any file fails; exit 0 when clean.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	jsonOut := flag.Bool("json", false, "emit a machine-readable JSON report")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: okf-check [--json] [path ...]\n\n")
		fmt.Fprintf(os.Stderr, "OKF v0.1 conformance checker (headless, file-based).\n")
		fmt.Fprintf(os.Stderr, "Each path may be a store root, an entries/ dir, or a single .md file.\n")
		fmt.Fprintf(os.Stderr, "With no paths, the current directory is checked.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	paths := flag.Args()
	if len(paths) == 0 {
		paths = []string{"."}
	}

	report, err := Check(paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "okf-check: %v\n", err)
		os.Exit(2)
	}

	if *jsonOut {
		if err := report.WriteJSON(os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "okf-check: %v\n", err)
			os.Exit(2)
		}
	} else {
		report.WriteText(os.Stdout)
	}

	if report.Failed > 0 {
		os.Exit(1)
	}
}
