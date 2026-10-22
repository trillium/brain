// Package braindocs embeds the operator runbooks in this directory so the
// binary can print them (`brain runbook`) without the repository checked out.
package braindocs

import "embed"

// Runbooks holds every RUNBOOK_*.md file in docs/brain. A new runbook is
// picked up by `brain runbook` by adding such a file; no registration needed.
//
//go:embed RUNBOOK_*.md
var Runbooks embed.FS
