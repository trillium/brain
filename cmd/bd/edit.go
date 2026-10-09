package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/metrics"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/ui"
)

var editCmd = &cobra.Command{
	Use:     "edit [id]",
	GroupID: "issues",
	Short:   "Edit an issue field in $EDITOR",
	Long: `Edit an issue field using your configured $EDITOR.

By default, edits the description. Use flags to edit other fields.

Examples:
  bd edit bd-42                    # Edit description
  bd edit bd-42 --title            # Edit title
  bd edit bd-42 --design           # Edit design notes
  bd edit bd-42 --notes            # Edit notes
  bd edit bd-42 --acceptance       # Edit acceptance criteria
  bd edit bd-42 --append           # Append to the description from an empty buffer`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		CheckReadonly("edit")

		evt := metrics.NewCommandEvent("edit")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		id := args[0]
		ctx := rootCtx

		// Resolve ID with prefix routing (supports cross-rig edits like `bd edit xe-5ls`)
		result, err := resolveAndGetIssueForMutation(ctx, store, id)
		if err != nil {
			return HandleErrorRespectJSON("resolving %s: %v", id, err)
		}
		defer result.Close()
		id = result.ResolvedID
		issueStore := result.Store

		fieldToEdit := "description"
		if cmd.Flags().Changed("title") {
			fieldToEdit = "title"
		} else if cmd.Flags().Changed("design") {
			fieldToEdit = "design"
		} else if cmd.Flags().Changed("notes") {
			fieldToEdit = "notes"
		} else if cmd.Flags().Changed("acceptance") {
			fieldToEdit = "acceptance_criteria"
		}

		appendMode, _ := cmd.Flags().GetBool("append")
		if appendMode && fieldToEdit == "title" {
			return HandleErrorRespectJSON("--append cannot be combined with --title")
		}

		editor, err := resolveEditorCommand()
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}

		issue := result.Issue

		var currentValue string
		switch fieldToEdit {
		case "title":
			currentValue = issue.Title
		case "description":
			currentValue = issue.Description
		case "design":
			currentValue = issue.Design
		case "notes":
			currentValue = issue.Notes
		case "acceptance_criteria":
			currentValue = issue.AcceptanceCriteria
		}

		tmpFile, err := os.CreateTemp("", fmt.Sprintf("bd-edit-%s-*.txt", fieldToEdit))
		if err != nil {
			return HandleErrorRespectJSON("creating temp file: %v", err)
		}
		tmpPath := tmpFile.Name()
		editSaved := false
		defer func() {
			if editSaved {
				_ = os.Remove(tmpPath)
			}
		}()

		// In append mode the buffer starts empty: whatever gets written is added
		// to the end of the field rather than replacing what is already there.
		seedValue := currentValue
		if appendMode {
			seedValue = ""
		}
		if _, err := tmpFile.WriteString(seedValue); err != nil {
			_ = tmpFile.Close()
			return HandleErrorRespectJSON("writing to temp file: %v", err)
		}
		_ = tmpFile.Close()

		if err := runEditorOnFile(editor, tmpPath); err != nil {
			return HandleErrorRespectJSON("running editor: %v", err)
		}

		// #nosec G304 -- tmpPath was created earlier in this function
		editedContent, err := os.ReadFile(tmpPath)
		if err != nil {
			return HandleErrorRespectJSON("reading edited file: %v", err)
		}

		newValue := strings.TrimSpace(string(editedContent))

		if appendMode {
			if newValue == "" {
				editSaved = true
				fmt.Println("No changes made")
				return nil
			}
			newValue = appendToField(currentValue, newValue)
		}

		if newValue == currentValue {
			editSaved = true
			fmt.Println("No changes made")
			return nil
		}

		if fieldToEdit == "title" && newValue == "" {
			return HandleErrorRespectJSON("title cannot be empty")
		}

		updates := map[string]interface{}{
			fieldToEdit: newValue,
		}

		err = issueStore.UpdateIssue(ctx, id, updates, actor)
		if err != nil {
			if accessor, ok := storage.UnwrapStore(issueStore).(storage.RawDBAccessor); ok {
				if pingErr := accessor.DB().PingContext(ctx); pingErr != nil {
					accessor.DB().SetConnMaxIdleTime(0)
					_ = accessor.DB().PingContext(ctx)
				}
			}
			err = issueStore.UpdateIssue(ctx, id, updates, actor)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Your edits are preserved in: %s\n", tmpPath)
			return HandleErrorRespectJSON("updating issue: %v", err)
		}
		editSaved = true
		if err := commitPendingIfEmbedded(ctx, issueStore, actor, doltAutoCommitParams{
			Command:  "edit",
			IssueIDs: []string{id},
		}); err != nil {
			fmt.Fprintf(os.Stderr, "Your edits are preserved in: %s\n", tmpPath)
			return HandleErrorRespectJSON("failed to commit: %v", err)
		}

		displayTitle := issue.Title
		if fieldToEdit == "title" {
			displayTitle = newValue
		}

		fieldName := strings.ReplaceAll(fieldToEdit, "_", " ")
		fmt.Printf("%s Updated %s for issue: %s\n", ui.RenderPass("✓"), fieldName, formatFeedbackID(id, displayTitle))
		return nil
	},
}

// appendToField joins an addition onto the end of a field, separated by a blank
// line so markdown blocks stay distinct.
func appendToField(current, addition string) string {
	if current == "" {
		return addition
	}
	return strings.TrimRight(current, "\n") + "\n\n" + addition
}

func init() {
	editCmd.Flags().Bool("title", false, "Edit the title")
	editCmd.Flags().Bool("description", false, "Edit the description (default)")
	editCmd.Flags().Bool("design", false, "Edit the design notes")
	editCmd.Flags().Bool("notes", false, "Edit the notes")
	editCmd.Flags().Bool("acceptance", false, "Edit the acceptance criteria")
	editCmd.Flags().Bool("append", false, "Start from an empty buffer and append what you write to the field")
	editCmd.ValidArgsFunction = issueIDCompletion
	rootCmd.AddCommand(editCmd)
}
