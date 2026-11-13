package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/brain/exfiltrator"
	"github.com/steveyegge/beads/internal/memoryapi"
	"github.com/steveyegge/beads/internal/metrics"
	"github.com/steveyegge/beads/internal/storage/kvkeys"
	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/memoryops"
)

// openMemories hands back the persistent-memory role for whichever route this
// invocation is on, each through its OWN capability accessor — the store's for
// the direct route and the provider's for the proxied one.
//
// directRequirement is the message `ensureDirectMode` reports when a workspace
// is reachable by neither route. It is per-verb because the shipped text names
// the verb.
func openMemories(directRequirement string) (memoryops.Memories, error) {
	if usesProxiedServer() {
		return proxiedMemories()
	}
	if err := ensureDirectMode(directRequirement); err != nil {
		return nil, err
	}
	return store.Memories()
}

// proxiedMemories hands back the guarded persistent-memory surface for this
// invocation's proxied-server provider, through the provider's OWN capability
// accessor — the same two-step proxiedWorkspaceConfig performs.
func proxiedMemories() (memoryops.Memories, error) {
	if uowProvider == nil {
		return nil, errors.New("proxied-server UOW provider not initialized")
	}
	return memoriesFromProvider(uowProvider)
}

// memoriesFromProvider is that accessor step for a provider the caller names.
//
// It takes the provider rather than reading the global one because `bd prime`
// opens a provider SCOPED to its read — prime is in noDbCommands, so the root
// pre-run opens nothing — and one spelling of "ask this provider for the memory
// surface" is the whole point of having an accessor at all.
func memoriesFromProvider(provider uow.UnitOfWorkProvider) (memoryops.Memories, error) {
	src, ok := provider.(uow.MemoriesSource)
	if !ok {
		return nil, fmt.Errorf("proxied-server provider %T does not offer the persistent-memory surface", provider)
	}
	return src.Memories()
}

// noteDirectMemoryWrite marks the invocation as having written, which is what
// the auto-commit epilogue in main.go keys on.
//
// It is DIRECT-ROUTE ONLY, and both halves of that matter. A direct memory
// write lands in the Dolt working set and nothing else commits it, so a verb
// that forgets to call this stores a memory that exists until the process exits
// and then sits uncommitted — visible to the session that wrote it and to
// nothing after. A proxied write already committed inside the role's unit of
// work, so flagging it there would ask the epilogue to commit a second time on
// a route with nothing outstanding.
//
// The RunEs cannot make that distinction themselves: openMemories hides which
// route they are on, which is the point. So the guard lives here, once, the way
// noteDirectConfigWrite does for the settings plane.
func noteDirectMemoryWrite() {
	if !usesProxiedServer() {
		commandDidWrite.Store(true)
	}
}

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

// memoryBeadsAvailable reports whether this invocation can mint and index
// companion beads: the direct route, where the open store holds both the
// issue and the kv.membead.* index row. The proxied route stores the memory
// alone.
func memoryBeadsAvailable() bool {
	return store != nil && !usesProxiedServer()
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

	linkedID, err := store.GetConfig(ctx, idxKey)
	if err != nil {
		return "", fmt.Errorf("reading memory link state: %w", err)
	}
	if linkedID != "" {
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
			commandDidWrite.Store(true)
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
		CreatedBy:   getFilingActor(),
		Owner:       getOwner(),
		Metadata:    withAuthorKind(metadata),
	})
	if err := store.CreateIssue(ctx, issue, actor); err != nil {
		return "", fmt.Errorf("creating memory bead: %w", err)
	}
	commandDidWrite.Store(true)

	if err := store.SetConfig(ctx, idxKey, issue.ID); err != nil {
		// The bead exists; only the link back from the memory key failed. Hand
		// the ID to the caller anyway -- it is the useful half -- along with
		// the error, so the user is told the next remember will mint a second
		// bead rather than update this one.
		return issue.ID, fmt.Errorf("linking memory %q to bead %s: %w", key, issue.ID, err)
	}
	return issue.ID, nil
}

// memoryBeadLinks reads the memory key -> issue ID index (kv.membead.* rows)
// on the direct route. The proxied route has no index, so it answers empty.
func memoryBeadLinks(ctx context.Context) map[string]string {
	beads := map[string]string{}
	if !memoryBeadsAvailable() {
		return beads
	}
	allConfig, err := store.GetAllConfig(ctx)
	if err != nil {
		return beads
	}
	for k, v := range allConfig {
		if userKey, ok := strings.CutPrefix(k, kvkeys.MemoryBeadConfigKeyPrefix); ok && v != "" {
			beads[userKey] = v
		}
	}
	return beads
}

// matchesKnownCommand reports whether insight is a single bare word that
// matches the name or an alias of a top-level bd command. It is used to catch
// `bd remember <subcommand>` mistakes before they become accidental memories.
// Multi-word insights (the normal case) always pass, since they contain
// whitespace and so cannot be a single command token.
func matchesKnownCommand(cmd *cobra.Command, insight string) (string, bool) {
	word := strings.TrimSpace(insight)
	if word == "" || strings.ContainsAny(word, " \t\r\n") {
		return "", false
	}
	for _, c := range cmd.Root().Commands() {
		if strings.EqualFold(c.Name(), word) {
			return c.Name(), true
		}
		for _, alias := range c.Aliases {
			if strings.EqualFold(alias, word) {
				return c.Name(), true
			}
		}
	}
	return "", false
}

// rememberBareKeyPath implements the desire-path / footgun guard for
// `bd remember <bare-slug>` (no --key): a bare slug naming an EXISTING memory
// is recalled instead of stored; a bare slug naming nothing is refused. The
// caller only invokes it when memoryKeyFlag == "" and the insight round-trips
// through memoryapi.DeriveKey unchanged, having already read the key.
func rememberBareKeyPath(key, insight, existing string) error {
	if existing != "" {
		if jsonOutput {
			return outputJSON(map[string]interface{}{
				"key":    key,
				"value":  existing,
				"found":  true,
				"action": "recalled",
			})
		}
		fmt.Fprintf(os.Stderr,
			"(recalled %q -- a bare existing key READS. To overwrite: `bd remember \"<new content>\" --key %s`)\n",
			key, key)
		fmt.Printf("%s\n", existing)
		return nil
	}
	return HandleErrorRespectJSON(
		"no memory named %q to recall -- and refusing to store a bare key-like token as its own content. "+
			"`bd remember` WRITES (its positional arg is CONTENT, not a key). "+
			"To store it anyway: `bd remember %q --key %s`. To browse keys: `bd memories`",
		key, insight, key)
}

// printRememberResult renders the `bd remember` success output. beadID names
// the companion issue (empty when --no-bead was passed or minting failed).
func printRememberResult(verb, key, insight, beadID string, beadErr error) error {
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
}

// printMemoriesResult renders the `bd memories` output. beads names the
// companion issue of each memory that has one.
func printMemoriesResult(memories map[string]string, search string, beads map[string]string) error {
	if jsonOutput {
		memArr := make([]map[string]interface{}, 0, len(memories))
		for k, v := range memories {
			entry := map[string]interface{}{
				"key":   k,
				"value": v,
			}
			if beadID := beads[k]; beadID != "" {
				entry["bead"] = beadID
			}
			memArr = append(memArr, entry)
		}
		sort.Slice(memArr, func(i, j int) bool {
			return memArr[i]["key"].(string) < memArr[j]["key"].(string)
		})
		return outputJSON(memArr)
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
}

// printForgetNotFound renders the `bd forget` missing-key output (including
// the SilentExit contract).
func printForgetNotFound(key string) error {
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

// printForgetResult renders the `bd forget` success output. beadID names the
// issue the memory was linked to; it still exists -- forget removes the
// memory, not the issue.
func printForgetResult(key, existing, beadID string) error {
	if jsonOutput {
		return outputJSON(map[string]string{
			"key":     key,
			"deleted": "true",
			"bead":    beadID,
		})
	}
	fmt.Printf("Forgot [%s]: %s\n", key, truncateMemory(existing, 80))
	if beadID != "" {
		fmt.Printf("  Issue %s still exists -- close it with '%s close %s' if it should go too.\n",
			beadID, memoryToolName(), beadID)
	}
	return nil
}

// printRecallResult renders the `bd recall` output (including the not-found
// SilentExit contract).
func printRecallResult(key, value string) error {
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
}

// rememberCmd stores a memory.
var rememberCmd = &cobra.Command{
	Use:   `remember "<insight>"`,
	Short: "Store a persistent memory",
	Long: `Store a memory that persists across sessions and account rotations.

Memories are injected at prime time (bd prime) so you have them
in every session without manual loading.

The positional arg is the memory CONTENT (the key is auto-generated from it
unless --key is given). As a convenience, if the arg is a bare key naming an
existing memory, it is RECALLED instead of stored (same as 'bd recall');
a bare key naming nothing is refused. Use --key to store slug-like content.

Each memory also gets a companion knowledge issue holding the same text, so
the insight can be commented on, tagged, shown, and searched like any other
bead. The memory key and the issue ID both name it: the key is what 'bd
memories' and 'bd prime' read, the issue ID is what comment/tag/show take
(and the key resolves to it). Pass --no-bead to store the memory alone.

Examples:
  bd remember "always run tests with -race flag"
  bd remember "Dolt phantom DBs hide in three places" --key dolt-phantoms
  bd remember "auth module uses JWT not sessions" --key auth-jwt
  bd remember "prod deploy needs a second approver" --no-bead
  bd remember dolt-phantoms        # bare existing key: reads it (= bd recall)`,
	GroupID:       "setup",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		CheckReadonly("remember") // also covers the migration freeze check (dc-6jaq)

		evt := metrics.NewCommandEvent("remember")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		insight := args[0]

		// Guard against a subcommand-like first argument being silently stored
		// as memory content. `bd remember` is a leaf command, so a mistaken
		// `bd remember recall` (or any bare bd command name) would otherwise
		// store the word "recall" as a memory instead of doing what the user
		// intended (GH#4401). A genuine insight is a phrase, so only a single
		// bare word that matches a known command is treated as suspect, and an
		// explicit --key signals deliberate intent and bypasses the guard.
		//
		// It stays at the FRONT DOOR and stays FIRST: it reads the cobra command
		// tree, which no role can see, and it must answer before any storage is
		// opened so that `bd remember list` in a directory with no workspace
		// still says "looks like a command".
		if memoryKeyFlag == "" {
			if name, ok := matchesKnownCommand(cmd, insight); ok {
				return HandleErrorWithHintRespectJSON(
					fmt.Sprintf("%q looks like a command, not something to remember", insight),
					fmt.Sprintf("Did you mean 'bd %s'? To store %q as a memory anyway, give it an explicit key: bd remember %q --key <key>", name, insight, insight),
				)
			}
		}

		memories, err := openMemories("remember requires direct database access")
		if err != nil {
			return HandleError("%v", err)
		}

		// Desire path + footgun guard: `bd remember <x>` is a WRITE whose positional arg is
		// the CONTENT, not a key -- but "remember X" reads as a getter in English, so agents
		// routinely type `bd remember some-key` meaning "do you remember X?". The tell-tale of
		// a mistyped read is content that round-trips through the key derivation unchanged (a
		// bare slug); real prose insights never do. When that happens and no explicit --key was
		// given:
		//   - the key EXISTS  -> pave the desire path: recall it instead of writing
		//   - no such key     -> refuse; storing a key-like token as its own content would
		//                        create a junk memory that hides the mistake
		// Passing --key states write intent and bypasses both branches.
		//
		// It stays ABOVE the role because it decides WHETHER TO WRITE AT ALL, and
		// because it exists to disambiguate English: an HTTP POST is not ambiguous
		// and must not inherit it. The read below is a plain Recall, so this whole
		// branch touches nothing.
		//
		// `derived != ""` is load-bearing and is not decoration: DeriveKey("")
		// is "", so without it every empty or unslugifiable insight would satisfy
		// derived == insight and be routed into a "recall" of the empty key. The
		// shipped code was saved from that by an empty-content check that ran
		// first; that check is the role's now, so the condition has to say it.
		derived := memoryapi.DeriveKey(insight)
		if memoryKeyFlag == "" && derived != "" && derived == insight {
			recalled, err := memories.Recall(rootCtx, memoryops.RecallRequest{Key: derived})
			if err != nil {
				return HandleErrorRespectJSON("recalling memory: %v", err)
			}
			return rememberBareKeyPath(derived, insight, recalled.Value)
		}

		result, err := memories.Remember(rootCtx, memoryops.RememberRequest{Key: memoryKeyFlag, Content: insight})
		if err != nil {
			// The role's two refusals ARE this command's shipped sentences —
			// "memory content cannot be empty" and "could not generate key from
			// content; use --key to specify one" — so they print as themselves.
			// Wrapping would reword output an agent may be matching on into
			// "storing memory: validation failed: ..." to say the same thing.
			if errors.Is(err, memoryops.ErrValidation) {
				return HandleErrorRespectJSON("%s", strings.TrimPrefix(err.Error(), memoryops.ErrValidation.Error()+": "))
			}
			return HandleErrorRespectJSON("storing memory: %v", err)
		}
		noteDirectMemoryWrite()

		// Read the memory back before reporting success. A write returning nil
		// is not by itself proof the row landed, and an unbacked success line
		// plus a returned key is exactly what makes an agent move on and lose
		// the insight. So an unverifiable write is reported as a failure too: a
		// readback we could not run is not evidence of anything. The two cases
		// get different wording because they mean different things -- a failed
		// read leaves the row's fate unknown, a clean read that disagrees means
		// the write is genuinely gone.
		readback, rbErr := memories.Recall(rootCtx, memoryops.RecallRequest{Key: result.Key})
		if rbErr != nil {
			return HandleErrorRespectJSON(
				"memory %q could not be verified: reading it back failed: %v. The write may or may "+
					"not have landed -- check with '%s memories %s' before assuming it is stored",
				result.Key, rbErr, memoryToolName(), result.Key)
		}
		if readback.Value != result.Value {
			return HandleErrorRespectJSON(
				"memory %q did not persist: wrote %d bytes, read back %d. Nothing was stored -- "+
					"capture this with '%s create' instead", result.Key, len(result.Value), len(readback.Value), memoryToolName())
		}

		// Mint (or refresh) the companion knowledge bead. A failure here does
		// not fail the command -- the memory itself is stored and verified --
		// but it is reported loudly, because the bead is what makes the
		// insight taggable and an agent that assumes one exists would flag
		// nothing for review.
		var beadID string
		var beadErr error
		switch {
		case !memoryBeadsAvailable():
			// Proxied route: the memory is stored alone.
		case !memoryNoBeadFlag:
			beadID, beadErr = syncMemoryBead(rootCtx, result.Key, result.Value)
		case result.Replaced:
			// --no-bead skips syncMemoryBead, but if a link already exists,
			// warn that the stale issue now holds old text.
			if linkedID, gerr := store.GetConfig(rootCtx, memoryBeadConfigKey(result.Key)); gerr == nil && linkedID != "" {
				fmt.Fprintf(os.Stderr,
					"  Warning: memory %q now holds new text, but issue %s still holds the old version.\n"+
						"  Refresh the link with: %s remember %q --key %s\n",
					result.Key, linkedID, memoryToolName(), result.Value, result.Key)
			}
		}

		// Remembered versus Updated is Replaced, observed in the SAME
		// transaction as the write. The shipped code read the row first and
		// described a moment that had already passed.
		verb := "Remembered"
		if result.Replaced {
			verb = "Updated"
		}
		return printRememberResult(verb, result.Key, result.Value, beadID, beadErr)
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

		var search string
		if len(args) > 0 {
			search = args[0]
		}

		memories, err := openMemories("memories requires direct database access")
		if err != nil {
			return HandleError("%v", err)
		}
		// The term goes to the role RAW. Case folding is List's, so the two
		// routes cannot come to disagree about what matches — which is the
		// whole reason the filter moved down.
		result, err := memories.List(rootCtx, memoryops.ListRequest{Search: search})
		if err != nil {
			return HandleErrorRespectJSON("listing memories: %v", err)
		}

		// The ECHO, on the other hand, has always been lowercased: `bd memories
		// FOO` prints `No memories matching "foo"`. It is a wart — a front door
		// should say back what the user typed — but it is shipped output, and
		// this commit is a convergence, not a change. Fixing it is a one-liner
		// with its own test, like truncateMemory's rune splitting.
		return printMemoriesResult(result.Memories, strings.ToLower(search), memoryBeadLinks(rootCtx))
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

		memories, err := openMemories("forget requires direct database access")
		if err != nil {
			return HandleError("%v", err)
		}
		// No pre-read here, deliberately: the value printed below is the one
		// the role's transaction actually deleted, not the one an earlier read
		// happened to see.
		result, err := memories.Forget(rootCtx, memoryops.ForgetRequest{Key: args[0]})
		if err != nil {
			return HandleErrorRespectJSON("forgetting memory: %v", err)
		}
		if !result.Found {
			return printForgetNotFound(result.Key)
		}
		noteDirectMemoryWrite()

		// Drop the memory -> issue link, but leave the issue itself alone. It
		// may carry comments, labels (including 'human'), and dependencies
		// that forgetting a config row has no business destroying; naming it
		// lets the caller close it deliberately.
		var beadID string
		if memoryBeadsAvailable() {
			beadID, _ = store.GetConfig(rootCtx, memoryBeadConfigKey(result.Key))
			if beadID != "" {
				if err := store.DeleteConfig(rootCtx, memoryBeadConfigKey(result.Key)); err != nil {
					WarnError("failed to unlink memory %q from issue %s: %v", result.Key, beadID, err)
				} else {
					commandDidWrite.Store(true)
				}
			}
		}

		return printForgetResult(result.Key, result.Value, beadID)
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

		memories, err := openMemories("recall requires direct database access")
		if err != nil {
			return HandleError("%v", err)
		}
		result, err := memories.Recall(rootCtx, memoryops.RecallRequest{Key: args[0]})
		if err != nil {
			return HandleErrorRespectJSON("recalling memory: %v", err)
		}

		return printRecallResult(result.Key, result.Value)
	},
}

// truncateMemory shortens a string to maxLen for display.
func truncateMemory(s string, maxLen int) string {
	// Replace newlines with spaces for single-line display
	s = strings.ReplaceAll(s, "\n", " ")
	return truncate(s, maxLen)
}

func init() {
	rememberCmd.Flags().StringVar(&memoryKeyFlag, "key", "", "Explicit key for the memory (auto-generated from content if not set). If a memory with this key already exists, it will be updated in place")
	rememberCmd.Flags().BoolVar(&memoryNoBeadFlag, "no-bead", false, "Store the memory only; do not mint the companion knowledge issue that comment/tag/show act on")

	rootCmd.AddCommand(rememberCmd)
	rootCmd.AddCommand(memoriesCmd)
	rootCmd.AddCommand(forgetCmd)
	rootCmd.AddCommand(recallCmd)
}
