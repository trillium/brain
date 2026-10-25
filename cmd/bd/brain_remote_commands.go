// Package main — brain_remote_commands.go
//
// 'brain remote commands': what every command in the tree does from a client.
//
// The list is derived, not hand-maintained. It reads the same two annotations
// the rest of client mode uses ('brain-remote' on the verbs that configure this
// client, 'brain-host' on the verbs that manage the host role) and applies the
// same rule the forwarder applies (brain_remote.go: remoteForwardingActive):
// 'brain remote …' and 'brain host …' run on this machine, everything else is
// forwarded to the brain host over ssh. Adding a command to the tree therefore
// adds it here, in the right group, with no table to update; the drift test in
// brain_remote_commands_test.go fails if that ever stops being true.
package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// Applicability classes, from a client's point of view.
const (
	// remoteAppliesClient: in client mode the forwarder sends it to the brain
	// host, where it runs through the host's own wrapper. This is the default
	// for every command that is not part of 'brain remote' or 'brain host'.
	remoteAppliesClient = "client"
	// remoteAppliesLocal: runs on this machine even in client mode; it is how
	// this machine's client configuration is read and changed.
	remoteAppliesLocal = "local"
	// remoteAppliesHost: host-only. It manages the machine that serves the
	// brain, so it is meaningless (and refused) on a client.
	remoteAppliesHost = "host"
)

// remoteCommandRow is one command in 'brain remote commands' output.
type remoteCommandRow struct {
	// Path is the command path under the binary, e.g. "remote set" or "create".
	Path string `json:"path"`
	// Name is the leaf command name.
	Name string `json:"name"`
	// Category is one of client | local | host.
	Category string `json:"category"`
	// RunsOn says where the command executes from a client.
	RunsOn string `json:"runs_on"`
	// Forwarded is true for the commands client mode forwards over ssh (the
	// whole client category), independent of whether a host is configured now.
	Forwarded bool `json:"forwarded"`
	// Available is whether the described client can run the command at all.
	Available bool `json:"available"`
	// Summary is the command's one-line help.
	Summary string `json:"summary"`
	// Aliases are the command's alternate names (e.g. "new" for "create").
	Aliases []string `json:"aliases,omitempty"`
	// Children is the number of visible subcommands.
	Children int `json:"children"`
	// Why and Fix are set only when Available is false: the reason and the
	// command that makes it work.
	Why string `json:"why,omitempty"`
	Fix string `json:"fix,omitempty"`
}

// remoteCommandsReport is the whole answer, for --json and for the printer.
type remoteCommandsReport struct {
	// Client is the ssh target named with --client, if any.
	Client string `json:"client,omitempty"`
	// ClientRegistered is whether the host knows that client (nil when this
	// machine is not the host, so the question cannot be answered here).
	ClientRegistered *bool `json:"client_registered,omitempty"`
	// Host is the brain host this machine (or the named client) uses.
	Host string `json:"host,omitempty"`
	// Mode is client | host | frozen | moved | local.
	Mode string `json:"mode"`
	// All is whether every subcommand is listed (--all) or only top-level ones.
	All bool `json:"all"`
	// Find is the free-text query from --find, if any. Commands that do not
	// match it are left out of Commands (and the totals).
	Find string `json:"find,omitempty"`
	// Category is the group filter from --category (client | local | host), if
	// any. Commands in other categories are left out of Commands (and the
	// totals).
	Category string `json:"category,omitempty"`
	// Commands is the derived list, sorted by path.
	Commands []remoteCommandRow `json:"commands"`
	// Totals counts the listed commands by category, plus "unavailable".
	Totals map[string]int `json:"totals"`
	// Notes are extra, human-facing remarks (registration, next steps).
	Notes []string `json:"notes,omitempty"`
}

// remoteCommandCategory classifies cmd using the registry annotations and the
// forwarder's own rule. It is the single source of truth for the report, and
// brain_remote_commands_test.go checks it against remoteForwardingActive.
func remoteCommandCategory(cmd *cobra.Command) string {
	switch {
	case isBrainHostVerb(cmd):
		return remoteAppliesHost
	case isBrainRemoteVerb(cmd):
		return remoteAppliesLocal
	default:
		return remoteAppliesClient
	}
}

// visibleChildren returns the subcommands a user can see and run.
func visibleChildren(cmd *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, c := range cmd.Commands() {
		if c.IsAvailableCommand() {
			out = append(out, c)
		}
	}
	return out
}

// collectRemoteCommandRows walks the live command tree. With all, every visible
// command is a row; otherwise only the top-level commands are.
func collectRemoteCommandRows(all bool) []remoteCommandRow {
	var rows []remoteCommandRow
	var add func(cmd *cobra.Command, prefix string)
	add = func(cmd *cobra.Command, prefix string) {
		path := cmd.Name()
		if prefix != "" {
			path = prefix + " " + cmd.Name()
		}
		rows = append(rows, remoteCommandRow{
			Path:      path,
			Name:      cmd.Name(),
			Category:  remoteCommandCategory(cmd),
			Forwarded: remoteCommandCategory(cmd) == remoteAppliesClient,
			Summary:   cmd.Short,
			Aliases:   append([]string(nil), cmd.Aliases...),
			Children:  len(visibleChildren(cmd)),
		})
		if !all {
			return
		}
		for _, sub := range visibleChildren(cmd) {
			add(sub, path)
		}
	}
	for _, top := range rootCmd.Commands() {
		if !top.IsAvailableCommand() {
			continue
		}
		add(top, "")
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	return rows
}

// remoteCommandRowMatches reports whether r matches a --find query: a
// case-insensitive substring of the command path, its name, any of its
// aliases, or its one-line summary. An empty query matches everything.
func remoteCommandRowMatches(r remoteCommandRow, find string) bool {
	if find == "" {
		return true
	}
	needle := strings.ToLower(find)
	hay := []string{r.Path, r.Name, r.Summary}
	hay = append(hay, r.Aliases...)
	for _, h := range hay {
		if strings.Contains(strings.ToLower(h), needle) {
			return true
		}
	}
	return false
}

// filterRemoteCommandRows narrows rows to those matching find, preserving the
// order collectRemoteCommandRows produced (sorted by path).
func filterRemoteCommandRows(rows []remoteCommandRow, find string) []remoteCommandRow {
	if find == "" {
		return rows
	}
	var out []remoteCommandRow
	for _, r := range rows {
		if remoteCommandRowMatches(r, find) {
			out = append(out, r)
		}
	}
	return out
}

// remoteCommandCategories are the accepted --category values.
var remoteCommandCategories = []string{remoteAppliesClient, remoteAppliesLocal, remoteAppliesHost}

// remoteCommandCategoryOK reports whether cat is an accepted --category value.
func remoteCommandCategoryOK(cat string) bool {
	for _, c := range remoteCommandCategories {
		if cat == c {
			return true
		}
	}
	return false
}

// filterRemoteCommandRowsByCategory narrows rows to one group (client, local
// or host), preserving the sorted order. It composes with --find: apply it
// after filterRemoteCommandRows.
func filterRemoteCommandRowsByCategory(rows []remoteCommandRow, cat string) []remoteCommandRow {
	if cat == "" {
		return rows
	}
	var out []remoteCommandRow
	for _, r := range rows {
		if r.Category == cat {
			out = append(out, r)
		}
	}
	return out
}

// remoteHostFix is the hint for a host-only command. selfIsHost is true when
// the machine running this command is itself the brain host.
func remoteHostFix(host string, selfIsHost bool) string {
	switch {
	case host != "":
		return "run it on the brain host:  ssh " + host + " brain host <verb>"
	case selfIsHost:
		return "run it on this host (the brain host):  brain host <verb>"
	default:
		return "run it on the brain host (the machine that serves the brain; 'brain remote status' names it)"
	}
}

// applyRemoteAvailability fills in where each command runs and whether the
// described client can run it. selfIsHost means this machine serves the brain;
// host is the configured brain host (may be empty); subject names what is being
// described ("this machine" or a named client target).
func applyRemoteAvailability(rows []remoteCommandRow, selfIsHost bool, host, subject string, clientNamed bool) {
	isHost := selfIsHost && !clientNamed
	for i := range rows {
		r := &rows[i]
		switch r.Category {
		case remoteAppliesHost:
			r.RunsOn = "the brain host"
			if isHost {
				r.Available = true
				r.RunsOn = "this machine (the brain host)"
				continue
			}
			r.Available = false
			r.Why = "host-only: it manages the machine that serves the brain, and " + subject + " is a client"
			r.Fix = remoteHostFix(host, selfIsHost)
		case remoteAppliesLocal:
			r.Available = true
			r.RunsOn = "this machine"
		default:
			r.Available = true
			switch {
			case host != "":
				r.RunsOn = "the brain host (" + host + ")"
			case clientNamed:
				r.RunsOn = "the brain host"
			default:
				r.RunsOn = "this machine (no brain host configured; it runs locally)"
			}
		}
	}
}

// buildRemoteCommandsReport derives the answer from the live registry and this
// machine's configuration. It never contacts the host. category, if non-empty,
// must be one of client | local | host (see remoteCommandCategories).
func buildRemoteCommandsReport(client string, all bool, find, category string) (*remoteCommandsReport, error) {
	if category != "" && !remoteCommandCategoryOK(category) {
		return nil, fmt.Errorf("invalid --category %q (want %s)", category, strings.Join(remoteCommandCategories, ", "))
	}
	home, _ := os.UserHomeDir()
	cfg, err := loadRemoteConfig()
	if err != nil {
		return nil, err
	}
	host := cfg.Host
	if client != "" {
		rc := &remoteConfig{Host: client}
		if err := rc.validate(); err != nil {
			return nil, err
		}
		// The named client's own host is unknown from here; report the
		// command set, which is the same on every client.
		host = ""
	}

	var hostCfg *hostConfig
	if home != "" {
		if hostCfg, err = loadHostConfig(home); err != nil {
			return nil, err
		}
	}
	role := hostRole("")
	if hostCfg != nil {
		role = hostCfg.Role
	}

	rep := &remoteCommandsReport{Client: client, All: all}
	mode := "local"
	switch {
	case client != "":
		mode = "client"
	case host != "":
		mode = "client"
	case role != "":
		mode = string(role)
	}
	rep.Mode = mode
	rep.Host = host

	isHost := host == "" && (role == "" || role == roleHost)
	subject := "this machine"
	if client != "" {
		subject = client
	}
	rows := collectRemoteCommandRows(all)
	applyRemoteAvailability(rows, isHost, host, subject, client != "")
	rows = filterRemoteCommandRows(rows, find)
	rows = filterRemoteCommandRowsByCategory(rows, category)
	rep.Commands = rows
	rep.Find = find
	rep.Category = category

	rep.Totals = map[string]int{}
	for _, r := range rows {
		rep.Totals[r.Category]++
		if !r.Available {
			rep.Totals["unavailable"]++
		}
	}
	if find != "" && len(rows) == 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("no commands match %q; try a shorter --find term", find))
	}

	if client != "" && hostCfg != nil && (hostCfg.Role == roleHost || hostCfg.Role == "") {
		registered := false
		for _, c := range hostCfg.Clients {
			if c.Target == client {
				registered = true
				break
			}
		}
		rep.ClientRegistered = &registered
		if !registered {
			rep.Notes = append(rep.Notes,
				fmt.Sprintf("client %s is not registered with this host; a promotion will not re-point it. Register it:  brain host clients add %s", client, client))
		}
	} else if client != "" && host == "" {
		rep.Notes = append(rep.Notes,
			fmt.Sprintf("run 'brain remote commands --client %s' on the brain host to see whether it is registered there", client))
	}
	if !all {
		rep.Notes = append(rep.Notes, "top-level commands only; add --all for every subcommand")
	}
	return rep, nil
}

func remoteModeDescription(rep *remoteCommandsReport) string {
	switch rep.Mode {
	case "client":
		if rep.Host != "" {
			return "client of " + rep.Host + " (commands are forwarded over ssh)"
		}
		return "client (commands are forwarded to the brain host over ssh)"
	case "host":
		return "host (this machine serves the brain; clients forward their commands here)"
	case "frozen":
		return "host, frozen for a promotion (reads run, writes are refused)"
	case "moved":
		return "moved (this machine is a client of the new host)"
	default:
		return "local (no brain host configured; commands run against a local database)"
	}
}

// printRemoteCommandsReport writes the human view.
func printRemoteCommandsReport(w io.Writer, rep *remoteCommandsReport) {
	fmt.Fprintf(w, "brain remote commands — what runs where\n\n")
	if rep.Client != "" {
		reg := ""
		if rep.ClientRegistered != nil {
			if *rep.ClientRegistered {
				reg = "  (registered with this host)"
			} else {
				reg = "  (NOT registered with this host)"
			}
		}
		fmt.Fprintf(w, "client:  %s%s\n", rep.Client, reg)
	}
	fmt.Fprintf(w, "mode:    %s\n", remoteModeDescription(rep))
	if rep.Host != "" {
		fmt.Fprintf(w, "host:    %s\n", rep.Host)
	}
	if rep.Find != "" {
		fmt.Fprintf(w, "find:    %s\n", rep.Find)
	}
	if rep.Category != "" {
		fmt.Fprintf(w, "category: %s\n", rep.Category)
	}
	fmt.Fprintln(w)

	groups := []struct {
		category string
		title    string
		detail   string
	}{
		{remoteAppliesClient, "Runs on the brain host (forwarded over ssh in client mode)", ""},
		{remoteAppliesLocal, "Runs on this machine (client-mode configuration)", ""},
		{remoteAppliesHost, "Host-only — run these on the machine that serves the brain", "why: it manages the host role, and a client has no brain database"},
	}
	for _, g := range groups {
		var rows []remoteCommandRow
		for _, r := range rep.Commands {
			if r.Category == g.category {
				rows = append(rows, r)
			}
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(w, "%s — %d:\n", g.title, len(rows))
		for _, r := range rows {
			name := r.Path
			if len(r.Aliases) > 0 {
				name += " (" + strings.Join(r.Aliases, ", ") + ")"
			}
			fmt.Fprintf(w, "  %-28s %s\n", name, r.Summary)
		}
		if g.detail != "" {
			fmt.Fprintf(w, "  %s\n", g.detail)
		}
		if rows[0].Fix != "" {
			fmt.Fprintf(w, "  fix: %s\n", rows[0].Fix)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "Totals: %d client, %d local, %d host-only", rep.Totals[remoteAppliesClient], rep.Totals[remoteAppliesLocal], rep.Totals[remoteAppliesHost])
	if n := rep.Totals["unavailable"]; n > 0 {
		fmt.Fprintf(w, "; %d cannot run from a client", n)
	}
	fmt.Fprintln(w)
	for _, n := range rep.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
}

var (
	remoteCommandsClient   string
	remoteCommandsAll      bool
	remoteCommandsFind     string
	remoteCommandsCategory string
)

var brainRemoteCommandsCmd = &cobra.Command{
	Use:   "commands",
	Short: "List the commands a client can run, which are host-only, and why",
	Long: `Show what every command does from a client, derived from the live command
tree (not a hand-maintained list):

  client  works from a client: in client mode it is forwarded to the brain host
          over ssh and runs there, through the host's own store wrapper.
  local   runs on this machine even in client mode: 'brain remote …' reads and
          changes this client's configuration.
  host    host-only: manages the machine that serves the brain. A client cannot
          run it; the fix says where to run it instead.

By default the top-level commands are listed; --all lists every subcommand.
--client <ssh-target> describes that client instead of this machine (and, on
the host, says whether it is registered there). --find <text> narrows the list
to commands whose path, alias or summary contains that text, so "brain remote
commands --find host" answers "can I run this from here?" without paging the
whole tree. --category client|local|host lists only one group, so "brain
remote commands --category host" shows exactly what a client cannot run,
with the why and the fix for each. Nothing is contacted: the answer is
computed locally from the command tree and this machine's config.`,
	Example: `  brain remote commands                 # this machine
  brain remote commands --all           # every subcommand
  brain remote commands --client me@laptop
  brain remote commands --find host
  brain remote commands --category host
  brain remote commands --json`,
	Args:        cobra.NoArgs,
	Annotations: map[string]string{"brain-remote": "1"},
	RunE: func(cmd *cobra.Command, _ []string) error {
		rep, err := buildRemoteCommandsReport(remoteCommandsClient, remoteCommandsAll, remoteCommandsFind, remoteCommandsCategory)
		if err != nil {
			return err
		}
		if jsonOutput {
			return outputJSON(rep)
		}
		printRemoteCommandsReport(cmd.OutOrStdout(), rep)
		return nil
	},
}

func init() {
	brainRemoteCommandsCmd.Flags().StringVar(&remoteCommandsClient, "client", "", "Describe this client (an ssh target) instead of this machine")
	brainRemoteCommandsCmd.Flags().BoolVar(&remoteCommandsAll, "all", false, "List every subcommand, not just the top-level commands")
	brainRemoteCommandsCmd.Flags().StringVar(&remoteCommandsFind, "find", "", "Only list commands whose path, alias or summary contains this text (case-insensitive)")
	brainRemoteCommandsCmd.Flags().StringVar(&remoteCommandsCategory, "category", "", "Only list commands in this category: client, local, or host")
	brainRemoteCmd.AddCommand(brainRemoteCommandsCmd)
}
