package main

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	braindocs "github.com/steveyegge/beads/docs/brain"
)

// runbookTopic is one embedded runbook: the topic name is the file name
// without RUNBOOK_ and .md, lowercased, underscores as hyphens
// (RUNBOOK_STORES_AND_DEPLOY.md -> stores-and-deploy).
type runbookTopic struct {
	Name  string
	Title string
	File  string
}

func runbookTopics(fsys fs.FS) ([]runbookTopic, error) {
	files, err := fs.Glob(fsys, "RUNBOOK_*.md")
	if err != nil {
		return nil, err
	}
	topics := make([]runbookTopic, 0, len(files))
	for _, f := range files {
		body, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, err
		}
		name := strings.ToLower(strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(f, "RUNBOOK_"), ".md"), "_", "-"))
		title := name
		for _, line := range strings.Split(string(body), "\n") {
			if t, ok := strings.CutPrefix(line, "# "); ok {
				title = strings.TrimSpace(t)
				break
			}
		}
		topics = append(topics, runbookTopic{Name: name, Title: title, File: f})
	}
	sort.Slice(topics, func(i, j int) bool { return topics[i].Name < topics[j].Name })
	return topics, nil
}

var brainRunbookCmd = &cobra.Command{
	Use:   "runbook [topic]",
	Short: "Print an operator runbook embedded in this binary",
	Long: `Operator runbooks are compiled into the binary, so they are available on a
machine that has no checkout of the repository.

  brain runbook            list the topics
  brain runbook <topic>    print one runbook (markdown)

Source files: docs/brain/RUNBOOK_*.md.`,
	Args:         cobra.MaximumNArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		topics, err := runbookTopics(braindocs.Runbooks)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if len(args) == 0 {
			for _, t := range topics {
				fmt.Fprintf(out, "%-20s %s\n", t.Name, t.Title)
			}
			return nil
		}
		want := strings.ToLower(args[0])
		for _, t := range topics {
			if t.Name == want {
				body, err := fs.ReadFile(braindocs.Runbooks, t.File)
				if err != nil {
					return err
				}
				_, err = out.Write(body)
				return err
			}
		}
		return fmt.Errorf("unknown runbook topic %q; run 'brain runbook' to list them", args[0])
	},
}

func init() {
	brainCmd.AddCommand(brainRunbookCmd)
	rootCmd.AddCommand(brainRunbookCmd)
}
