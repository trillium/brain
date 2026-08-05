package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/brain/exfiltrator"
	"github.com/steveyegge/beads/internal/metrics"
	"github.com/steveyegge/beads/internal/storage/kvkeys"
	"github.com/steveyegge/beads/internal/types"
)

// memoryPrefix is prepended (after kvPrefix) to all memory keys.
const memoryPrefix = kvkeys.MemoryPrefix

// memoryBeadPrefix is prepended (after kvPrefix) to the memory-key -> bead-ID
// index rows that back the companion knowledge bead.
const memoryBeadPrefix = kvkeys.MemoryBeadPrefix

// memoryBeadPriority is the priority of a minted memory bead. A remembered
// insight is a record, not queued work: P3 keeps it out of the top of `bd
// ready` while leaving it listable, searchable, and taggable.
const memoryBeadPriority = 3

// memoryKeyFlag allows explicit key override for bd remember.
var memoryKeyFlag string

// memoryNoBeadFlag opts out of minting the companion knowledge bead.
var memoryNoBeadFlag bool

// slugify converts a string to a URL-friendly slug for use as a memory key.
// Takes the first ~8 words, lowercases, replaces non-alphanumeric with hyphens.
func slugify(s string) string {
	s = strings.ToLower(s)
	// Replace non-alphanumeric chars with hyphens
	re := regexp.MustCompile(`[^a-z0-9]+`)
	s = re.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")

	// Limit to first ~8 "words" (hyphen-separated segments)
	parts := strings.SplitN(s, "-", 10)
	if len(parts) > 8 {
		parts = parts[:8]
	}
	slug := strings.Join(parts, "-")

	// Cap total length
	if len(slug) > 60 {
		slug = slug[:60]
		// Don't end on a hyphen
		slug = strings.TrimRight(slug, "-")
	}
	return slug
}

// memoryToolName returns the command name the user actually typed, so hint
// text reads "brain memories ..." under the brain wrapper rather than always
// claiming "bd". main() points rootCmd.Use at $BD_NAME for exactly this.
func memoryToolName() string {
	if n := rootCmd.Name(); n != "" {
		return n
	}
	return "bd"
}

// memoryBeadConfigKey returns the config-table key that indexes a memory key
// to the ID of the knowledge bead minted for it.
func memoryBeadConfigKey(key string) string {
	return kvPrefix + memoryBeadPrefix + key
}

// memoryBeadTitle renders a one-line title for a memory's companion bead: the
// first line of the insight, capped so a paragraph-length memory does not
// become a paragraph-length title. The full text lives in the description.
func memoryBeadTitle(insight string) string {
	line := strings.TrimSpace(insight)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	return truncateMemory(line, 120)
}

// syncMemoryBead creates (or updates) the knowledge bead that mirrors a
// memory, and returns its issue ID.
//
// A memory alone is write-only from an agent's side: it lives in the config
// table, so comment / tag / show / search / list all miss it, and the
// documented flag-for-review recipe ("remember the finding, then tag it
// human") dead-ends at step two. Minting a companion bead gives the memory a
// real issue ID to hang those verbs off, while the config row keeps doing what
// only it can do -- get injected at prime time.
//
// The bead is typed knowledge and priority P3: it is a record, not queued
// work. Re-remembering the same key updates the bead in place rather than
// minting a second one, so `remember` stays idempotent per key.
func syncMemoryBead(ctx context.Context, key, insight string) (string, error) {
	idxKey := memoryBeadConfigKey(key)

	if linkedID, err := store.GetConfig(ctx, idxKey); err == nil && linkedID != "" {
		// Only update through an index entry that still points at a live
		// issue. A stale pointer (bead deleted, or the index row synced into a
		// store that never had the issue) falls through to a fresh mint --
		// better than failing the remember over a dangling reference.
		if issue, gerr := store.GetIssue(ctx, linkedID); gerr == nil && issue != nil {
			updates := map[string]interface{}{
				"title":       memoryBeadTitle(insight),
				"description": insight,
			}
			if uerr := store.UpdateIssue(ctx, linkedID, updates, actor); uerr != nil {
				return "", fmt.Errorf("updating memory bead %s: %w", linkedID, uerr)
			}
			return linkedID, nil
		}
	}

	// brain_slug carries the memory key onto the issue so the two identifiers
	// stay tied together in exported markdown, where the config table is not
	// present to consult.
	metadata, err := exfiltrator.MetadataWithSlug(nil, key)
	if err != nil {
		return "", fmt.Errorf("building memory bead metadata: %w", err)
	}

	issue := buildCreateIssue(createIssueParams{
		Title:       memoryBeadTitle(insight),
		Description: insight,
		Priority:    memoryBeadPriority,
		IssueType:   types.TypeKnowledge,
		CreatedBy:   getActorWithGit(),
		Owner:       getOwner(),
		Metadata:    metadata,
	})
	if err := store.CreateIssue(ctx, issue, actor); err != nil {
		return "", fmt.Errorf("creating memory bead: %w", err)
	}

	if err := store.SetConfig(ctx, idxKey, issue.ID); err != nil {
		// The bead exists; only the link back from the memory key failed. Hand
		// the ID to the caller anyway -- it is the useful half -- along with
		// the error, so the user is told the next remember will mint a second
		// bead rather than update this one.
		return issue.ID, fmt.Errorf("linking memory %q to bead %s: %w", key, issue.ID, err)
	}
	return issue.ID, nil
}

// rememberCmd stores a memory.
var rememberCmd = &cobra.Command{
	Use:   `remember "<insight>"`,
	Short: "Store a persistent memory",
	Long: `Store a memory that persists across sessions and account rotations.

Memories are injected at prime time (bd prime) so you have them
in every session without manual loading.

Each memory also gets a companion knowledge issue holding the same text, so
the insight can be commented on, tagged, shown, and searched like any other
bead. The memory key and the issue ID both name it: the key is what 'bd
memories' and 'bd prime' read, the issue ID is what comment/tag/show take
(and the key resolves to it). Pass --no-bead to store the memory alone.

Examples:
  bd remember "always run tests with -race flag"
  bd remember "Dolt phantom DBs hide in three places" --key dolt-phantoms
  bd remember "auth module uses JWT not sessions" --key auth-jwt
  bd remember "prod deploy needs a second approver" --no-bead`,
	GroupID:       "setup",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		CheckReadonly("remember")

		evt := metrics.NewCommandEvent("remember")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if err := ensureDirectMode("remember requires direct database access"); err != nil {
			return HandleError("%v", err)
		}

		insight := args[0]
		if strings.TrimSpace(insight) == "" {
			return HandleErrorRespectJSON("memory content cannot be empty")
		}

		key := memoryKeyFlag
		if key == "" {
			key = slugify(insight)
		}
		if key == "" {
			return HandleErrorRespectJSON("could not generate key from content; use --key to specify one")
		}

		storageKey := kvPrefix + memoryPrefix + key

		ctx := rootCtx

		existing, _ := store.GetConfig(ctx, storageKey)
		verb := "Remembered"
		if existing != "" {
			verb = "Updated"
		}

		if err := store.SetConfig(ctx, storageKey, insight); err != nil {
			return HandleErrorRespectJSON("storing memory: %v", err)
		}
		commandDidWrite.Store(true)

		// Read the memory back before reporting success. SetConfig returning nil
		// is not by itself proof the row landed, and an unbacked success line
		// plus a returned key is exactly what makes an agent move on and lose
		// the insight -- the failure this whole command is being hardened
		// against. So an unverifiable write is reported as a failure too: a
		// readback we could not run is not evidence of anything, and claiming
		// success on it would reintroduce the bug in a quieter form. The two
		// cases get different wording because they mean different things -- a
		// failed read leaves the row's fate unknown, a clean read that
		// disagrees means the write is genuinely gone.
		readback, rbErr := store.GetConfig(ctx, storageKey)
		if rbErr != nil {
			return HandleErrorRespectJSON(
				"memory %q could not be verified: reading it back failed: %v. The write may or may "+
					"not have landed -- check with '%s memories %s' before assuming it is stored",
				key, rbErr, memoryToolName(), key)
		}
		if readback != insight {
			return HandleErrorRespectJSON(
				"memory %q did not persist: wrote %d bytes, read back %d. Nothing was stored -- "+
					"capture this with '%s create' instead", key, len(insight), len(readback), memoryToolName())
		}

		// Mint (or refresh) the companion knowledge bead. A failure here does
		// not fail the command -- the memory itself is stored and verified --
		// but it is reported loudly, because the bead is what makes the
		// insight taggable and an agent that assumes one exists would flag
		// nothing for review.
		var beadID string
		var beadErr error
		if !memoryNoBeadFlag {
			beadID, beadErr = syncMemoryBead(ctx, key, insight)
		}

		if jsonOutput {
			out := map[string]string{
				"key":    key,
				"value":  insight,
				"action": strings.ToLower(verb),
				// kind/read disambiguate the slug-shaped key from an issue ID.
				"kind": "memory",
				"read": fmt.Sprintf("%s memories %s", memoryToolName(), key),
				// bead is the issue ID that comment/tag/show accept. Empty
				// when --no-bead was passed or minting failed.
				"bead": beadID,
			}
			if beadErr != nil {
				out["bead_error"] = beadErr.Error()
			}
			return outputJSON(out)
		}
		// Say "memory" explicitly and name both identifiers. The original line
		// was "Remembered [some-slug]: ...", which reads like a created issue
		// whose ID is the slug -- so agents fed the slug to show/tag/comment,
		// got "no issue found", and concluded the write was silently dropped.
		tool := memoryToolName()
		fmt.Printf("%s memory [%s]: %s\n", verb, key, truncateMemory(insight, 80))
		fmt.Printf("  Read it with '%s memories %s'; it is injected by '%s prime'.\n", tool, key, tool)
		switch {
		case beadErr != nil && beadID == "":
			fmt.Fprintf(os.Stderr,
				"  Warning: no issue was minted for this memory: %v\n"+
					"  The memory is stored, but comment/tag/show have nothing to act on. "+
					"Use '%s create' if you need a taggable record.\n", beadErr, tool)
		case beadErr != nil:
			fmt.Fprintf(os.Stderr,
				"  Warning: issue %s was created but not linked to memory %q: %v\n"+
					"  Tag and comment work on it now; a later '%s remember' with this key will "+
					"mint a second issue instead of updating it.\n", beadID, key, beadErr, tool)
		case beadID != "":
			fmt.Printf("  Issue %s -- use this ID for '%s comment', '%s tag', '%s show'.\n", beadID, tool, tool, tool)
		}
		return nil
	},
}

// memoriesCmd lists and searches memories.
var memoriesCmd = &cobra.Command{
	Use:   "memories [search]",
	Short: "List or search persistent memories",
	Long: `List all memories, or search by keyword.

Examples:
  bd memories              # list all memories
  bd memories dolt         # search for memories about dolt
  bd memories "race flag"  # search for a phrase`,
	GroupID:       "setup",
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("memories")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if err := ensureDirectMode("memories requires direct database access"); err != nil {
			return HandleError("%v", err)
		}

		ctx := rootCtx
		allConfig, err := store.GetAllConfig(ctx)
		if err != nil {
			return HandleErrorRespectJSON("listing memories: %v", err)
		}

		// Filter for kv.memory.* keys. kv.membead.* rows in the same table are
		// the memory -> issue index, not memories; they are collected
		// separately so each memory can name the issue that carries it.
		fullPrefix := kvkeys.MemoryConfigKeyPrefix
		beadPrefix := kvkeys.MemoryBeadConfigKeyPrefix
		memories := make(map[string]string)
		beads := make(map[string]string)
		for k, v := range allConfig {
			switch {
			case strings.HasPrefix(k, fullPrefix):
				memories[strings.TrimPrefix(k, fullPrefix)] = v
			case strings.HasPrefix(k, beadPrefix):
				beads[strings.TrimPrefix(k, beadPrefix)] = v
			}
		}

		var search string
		if len(args) > 0 {
			search = strings.ToLower(args[0])
		}
		if search != "" {
			filtered := make(map[string]string)
			for k, v := range memories {
				if strings.Contains(strings.ToLower(k), search) ||
					strings.Contains(strings.ToLower(v), search) {
					filtered[k] = v
				}
			}
			memories = filtered
		}

		if jsonOutput {
			return outputJSON(memories)
		}

		if len(memories) == 0 {
			if search != "" {
				fmt.Printf("No memories matching %q\n", search)
			} else {
				fmt.Println("No memories stored. Use 'bd remember \"insight\"' to add one.")
			}
			return nil
		}

		keys := make([]string, 0, len(memories))
		for k := range memories {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		if search != "" {
			fmt.Printf("Memories matching %q:\n\n", search)
		} else {
			fmt.Printf("Memories (%d):\n\n", len(memories))
		}
		for _, k := range keys {
			v := memories[k]
			// Name the issue when there is one: the key alone is not something
			// comment/tag/show accept, and that gap is what sent agents
			// looking for a bug in remember.
			if beadID := beads[k]; beadID != "" {
				fmt.Printf("  %s  (issue %s)\n", k, beadID)
			} else {
				fmt.Printf("  %s\n", k)
			}
			fmt.Printf("    %s\n\n", truncateMemory(v, 120))
		}
		return nil
	},
}

// forgetCmd removes a memory.
var forgetCmd = &cobra.Command{
	Use:   "forget <key>",
	Short: "Remove a persistent memory",
	Long: `Remove a memory by its key.

Use 'bd memories' to see available keys.

Examples:
  bd forget dolt-phantoms
  bd forget auth-jwt`,
	GroupID:       "setup",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		CheckReadonly("forget")

		evt := metrics.NewCommandEvent("forget")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if err := ensureDirectMode("forget requires direct database access"); err != nil {
			return HandleError("%v", err)
		}

		key := args[0]
		storageKey := kvPrefix + memoryPrefix + key

		ctx := rootCtx

		existing, _ := store.GetConfig(ctx, storageKey)
		if existing == "" {
			if jsonOutput {
				if jerr := outputJSON(map[string]string{
					"key":   key,
					"found": "false",
				}); jerr != nil {
					return jerr
				}
				return SilentExit()
			}
			fmt.Fprintf(os.Stderr, "No memory with key %q\n", key)
			return SilentExit()
		}

		if err := store.DeleteConfig(ctx, storageKey); err != nil {
			return HandleErrorRespectJSON("forgetting memory: %v", err)
		}
		commandDidWrite.Store(true)

		// Drop the memory -> issue link, but leave the issue itself alone. It
		// may carry comments, labels (including 'human'), and dependencies
		// that forgetting a config row has no business destroying; naming it
		// lets the caller close it deliberately.
		beadID, _ := store.GetConfig(ctx, memoryBeadConfigKey(key))
		if beadID != "" {
			if err := store.DeleteConfig(ctx, memoryBeadConfigKey(key)); err != nil {
				WarnError("failed to unlink memory %q from issue %s: %v", key, beadID, err)
			}
		}

		if jsonOutput {
			return outputJSON(map[string]string{
				"key":     key,
				"deleted": "true",
				// bead names the issue the memory was linked to. It still
				// exists -- forget removes the memory, not the issue.
				"bead": beadID,
			})
		}
		fmt.Printf("Forgot [%s]: %s\n", key, truncateMemory(existing, 80))
		if beadID != "" {
			fmt.Printf("  Issue %s still exists -- close it with '%s close %s' if it should go too.\n",
				beadID, memoryToolName(), beadID)
		}
		return nil
	},
}

// recallCmd retrieves a specific memory by key.
var recallCmd = &cobra.Command{
	Use:   "recall <key>",
	Short: "Retrieve a specific memory",
	Long: `Retrieve the full content of a memory by its key.

Examples:
  bd recall dolt-phantoms
  bd recall auth-jwt`,
	GroupID:       "setup",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("recall")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if err := ensureDirectMode("recall requires direct database access"); err != nil {
			return HandleError("%v", err)
		}

		key := args[0]
		storageKey := kvPrefix + memoryPrefix + key

		ctx := rootCtx
		value, err := store.GetConfig(ctx, storageKey)
		if err != nil {
			return HandleErrorRespectJSON("recalling memory: %v", err)
		}

		if jsonOutput {
			if jerr := outputJSON(map[string]interface{}{
				"key":   key,
				"value": value,
				"found": value != "",
			}); jerr != nil {
				return jerr
			}
			if value == "" {
				return SilentExit()
			}
			return nil
		}
		if value == "" {
			fmt.Fprintf(os.Stderr, "No memory with key %q\n", key)
			return SilentExit()
		}
		fmt.Printf("%s\n", value)
		return nil
	},
}

// truncateMemory shortens a string to maxLen for display.
func truncateMemory(s string, maxLen int) string {
	// Replace newlines with spaces for single-line display
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func init() {
	rememberCmd.Flags().StringVar(&memoryKeyFlag, "key", "", "Explicit key for the memory (auto-generated from content if not set). If a memory with this key already exists, it will be updated in place")
	rememberCmd.Flags().BoolVar(&memoryNoBeadFlag, "no-bead", false, "Store the memory only; do not mint the companion knowledge issue that comment/tag/show act on")

	rootCmd.AddCommand(rememberCmd)
	rootCmd.AddCommand(memoriesCmd)
	rootCmd.AddCommand(forgetCmd)
	rootCmd.AddCommand(recallCmd)
}
