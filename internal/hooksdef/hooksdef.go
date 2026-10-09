// Package hooksdef declares and loads brain's refusal-capable hooks.
//
// Vision (VISION.md, §"Hooks observe, and may refuse"): a hook may refuse a
// write before it happens, and the refusal is surfaced as the reason the bead
// was never created. Whether a hook's own failure blocks the write is declared
// by that hook: a guard that must refuse fails closed, an observer fails open.
// A hook nobody has configured warns, so the gap is visible rather than
// silently resolved in either direction. The store remains the only writer, so
// a write is never the work of a hook.
//
// # Where hooks are declared
//
// A hook is one TOML file in the store's <beadsDir>/hooks.d/ directory — the
// same `.beads` directory every other per-store artifact lives in, next to the
// legacy `.beads/hooks/` exec-hook channel, which this package does not touch.
// Discovery is a directory listing at command start: no daemon, no hidden
// directory, no registry to keep in sync. One file is one hook; the file's
// basename (minus .toml) is the hook's name, and a declared name that disagrees
// with its filename is a loud refusal, not a silent resolution.
//
// # The three failure policies
//
//	policy = "guard"     — the hook is allowed to refuse. Exit 0 passes the
//	                       write; exit refusal_exit (default 2) refuses it and
//	                       names the hook; ANY other exit, a load failure, or a
//	                       timeout fails CLOSED: the write is refused because
//	                       the guard could not finish deciding.
//
//	policy = "observer"  — the hook observes after the fact. Exit 0 is a pass;
//	                       any non-zero exit, a load failure, or a timeout
//	                       fails OPEN: the write proceeds and the failure is
//	                       recorded as a durable, addressable warning.
//
//	policy unset         — declares nothing. behaves like an observer for
//	                       pass/fail, but additionally emits the unset-policy
//	                       warning every time it runs, so a hook whose author
//	                       never decided is visible rather than silently one
//	                       thing or the other.
//
// # Why TOML
//
// BurntSushi/toml is already a direct dependency (internal/formula). TOML gives
// unambiguous typed parsing where a value of the wrong type is a parse error —
// exactly the "refuse rather than guess" surface the vision requires — without
// adding a dependency or inventing a format.
package hooksdef

import (
	"fmt"
	"time"
)

// Event values select which mutations a hook observes or guards.
const (
	EventCreate = "create" // a bead is being created
	EventUpdate = "update" // a bead is being changed in place
	EventClose  = "close"  // a bead is being closed
	EventDelete = "delete" // a bead is being deleted
	EventAll    = "*"      // every mutation (the default when `when` is unset)
)

// Policy values are the declared failure policies.
const (
	// PolicyGuard declares fail-closed: the hook may refuse, and any other
	// failure refused the write too.
	PolicyGuard = "guard"
	// PolicyObserver declares fail-open: the hook observes, and its own
	// failure degrades the hook, not the write.
	PolicyObserver = "observer"
	// PolicyUnset is the empty declaration. It warns.
	PolicyUnset = ""
)

// Default values applied when a declaration leaves a field unset.
const (
	DefaultPolicy       = PolicyUnset
	DefaultTimeout      = 10 * time.Second
	DefaultRefusalExit  = 2
	defaultWhen         = EventAll
	DefaultHooksDirName = "hooks.d"
)

// Definition is one declared hook, fully resolved: every field it declares
// plus every field defaulted at load time. A Definition is immutable once
// returned by LoadAll; the decision machinery reads it, never rewrites it.
type Definition struct {
	// Name is the hook's identity: the file's basename without .toml.
	// A [name] key inside the file must match it exactly, so a renamed file
	// is a refusal rather than a silent re-identification.
	Name string
	// Path is the absolute path the definition was loaded from. It appears
	// in every refusal and warning that names this hook.
	Path string
	// Run is the shell command executed via sh -c. Required.
	Run string
	// When is one of the Event values. It selects which mutation events
	// trigger this hook.
	When string
	// Policy is one of the Policy values. Unset ("") means warn.
	Policy string
	// Timeout bounds one execution of Run. Default 10s.
	Timeout time.Duration
	// RefusalExit is the exit code Run may exit with to refuse a write
	// deliberately. Only meaningful for PolicyGuard. Default 2.
	RefusalExit int
}

// Event returns the normalized event selector.
func (d Definition) Event() string {
	if d.When == "" {
		return defaultWhen
	}
	return d.When
}

// MatchesEvent reports whether this hook fires for the named event.
func (d Definition) MatchesEvent(event string) bool {
	if event == "" {
		return false
	}
	e := d.Event()
	return e == EventAll || e == event
}

// Declared reports whether the hook declared a failure policy.
func (d Definition) Declared() bool { return d.Policy != PolicyUnset }

// IsGuard reports whether the hook declared the guard policy.
func (d Definition) IsGuard() bool { return d.Policy == PolicyGuard }

// Describe renders the definition's one-line identity, used in listings and
// in refusal/warning messages.
func (d Definition) Describe() string {
	policy := d.Policy
	if !d.Declared() {
		policy = "<unset:warns>"
	}
	return fmt.Sprintf("%s (policy=%s when=%s)", d.Name, policy, d.Event())
}

// LoadError is a loud, named failure to load one hook definition. LoadAll
// refuses rather than guessing on any of these; the returned error carries
// the file path so the operator can fix the right file.
type LoadError struct {
	Path    string
	Hook    string
	Problem string
}

func (e *LoadError) Error() string {
	if e.Hook != "" {
		return fmt.Sprintf("hook %q (%s): %s", e.Hook, e.Path, e.Problem)
	}
	return fmt.Sprintf("hook definition %s: %s", e.Path, e.Problem)
}

// Refusal is the reason a hook refused a write. It names the hook, the event,
// and whatever the hook said about the refusal. The command that receives it
// surfaces it as the reason the bead was never created.
type Refusal struct {
	// Hook is the refusing hook's name.
	Hook string
	// Path is the refusing hook's definition file.
	Path string
	// Event is the mutation the hook was run against.
	Event string
	// Detail is the hook's own explanation, taken from its stderr, when it
	// said one. May be empty.
	Detail string
	// Cause distinguishes a deliberate refusal from a failed guard:
	// CauseRefused for the declared refusal exit, CauseFailed for any other
	// failure of a fail-closed guard, CauseLoad when the definition could
	// not even be run.
	Cause string
}

// Causes a Refusal can carry.
const (
	CauseRefused = "refused" // hook exited with the declared refusal exit
	CauseFailed  = "hookfailed"
	CauseLoad    = "load"
	CauseTimeout = "timeout"
)

func (r *Refusal) Error() string {
	consequence := "write refused"
	if r.Event == EventCreate {
		consequence = "bead not created"
	}
	detail := ""
	if r.Detail != "" {
		detail = ": " + r.Detail
	}
	switch r.Cause {
	case CauseRefused:
		return fmt.Sprintf("%s: refused by guard hook %q (%s)%s", consequence, r.Hook, r.Path, detail)
	default:
		return fmt.Sprintf("%s: guard hook %q (%s) failed closed [%s]%s", consequence, r.Hook, r.Path, r.Cause, detail)
	}
}

// Warning is one durable, addressable hook_failure or unset-policy record.
// It is what makes a warning "not a quieter failure": it says what degraded,
// names the hook, and carries a status a person or a repair agent can act on
// and later acknowledge.
type Warning struct {
	// ID is a unique, sortable identifier for the record.
	ID string `json:"id"`
	// CreatedAt is when the record was written.
	CreatedAt time.Time `json:"created_at"`
	// Hook is the hook whose failure or unset policy this records.
	Hook string `json:"hook"`
	// Path is the hook's definition file.
	Path string `json:"path"`
	// Event is the mutation the hook was run against.
	Event string `json:"event"`
	// Subject is the bead the mutation concerned. Observers run after the
	// write, so a create has its id by then; it is empty only when the
	// mutation named no single bead.
	Subject string `json:"subject,omitempty"`
	// Command is the bd command that performed the mutation, when known.
	Command string `json:"command,omitempty"`
	// Reason is one of WarningReason values.
	Reason string `json:"reason"`
	// Detail is human-readable text: hook stderr for a failure, a fixed
	// sentence for an unset policy.
	Detail string `json:"detail"`
	// Status is StatusOpen until someone acknowledges it.
	Status string `json:"status"`
	// AckBy and AckAt record the acknowledgement.
	AckBy string     `json:"ack_by,omitempty"`
	AckAt *time.Time `json:"ack_at,omitempty"`
}

// Warning reasons.
const (
	// ReasonFailure: a declared-observer hook failed (fail-open).
	ReasonFailure = "hook-failed"
	// ReasonUnsetPolicy: a hook declared no failure policy.
	ReasonUnsetPolicy = "policy-unset"
)

// Warning statuses.
const (
	StatusOpen         = "open"
	StatusAcknowledged = "acknowledged"
)

// Payload is the JSON document piped to a hook's stdin. It describes the
// mutation that is about to happen (guards) or just happened (observers),
// and nothing about where the store lives — carrying database coordinates
// into a hook process is exactly how a hook could become a writer, so the
// payload carries names, not paths.
type Payload struct {
	// Event is the mutation event: create | update | close | delete.
	Event string `json:"event"`
	// Command is the bd command performing the mutation ("create", "close", ...).
	Command string `json:"command,omitempty"`
	// Store is the store's logical name (BD_NAME), never a path.
	Store string `json:"store,omitempty"`
	// Issue is the proposed row for a create, or the current row for an
	// update/close/delete when it could be read. Field names follow the
	// issue's JSON shape.
	Issue *PayloadIssue `json:"issue,omitempty"`
	// Updates is the field map for an update event.
	Updates map[string]interface{} `json:"updates,omitempty"`
	// Reason is the close/delete reason when the event carries one.
	Reason string `json:"reason,omitempty"`
	// Actor is who performed the mutation.
	Actor string `json:"actor,omitempty"`
}

// PayloadIssue is the slimmed issue shape handed to hooks. It is a subset of
// types.Issue chosen at this boundary rather than the full row, so a hook
// payload's schema is explicit and additive rather than whatever the issue
// type happens to hold.
type PayloadIssue struct {
	ID          string   `json:"id,omitempty"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Status      string   `json:"status,omitempty"`
	Priority    int      `json:"priority"` // 0 is valid (P0)
	IssueType   string   `json:"issue_type,omitempty"`
	Assignee    string   `json:"assignee,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	CreatedBy   string   `json:"created_by,omitempty"`
}
