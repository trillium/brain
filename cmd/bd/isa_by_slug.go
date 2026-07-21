package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var isaBySlugCmd = &cobra.Command{
	Use:     "isa-by-slug <slug>",
	GroupID: "issues",
	Short:   "Resolve a slug to a brain id (O(1) lookup against the issues.slug column)",
	Long: `Resolve an ISA slug to its brain id. Lookup is O(1) against the
issues.slug column (added in F1a; auto-populated in F1d).

  bd isa-by-slug ship-the-thing   # prints the id on success, exit 0
                                  # missing slug exits 1 with stderr message

isa-by-slug is restricted to kind=isa rows. A slug present on a non-isa row
is treated as not-found and exits 1.`,
	Args: cobra.ExactArgs(1),
	Run:  runISABySlug,
}

func init() {
	rootCmd.AddCommand(isaBySlugCmd)
}

// runISABySlug looks up issues.id WHERE slug=? AND issue_type='isa'. The
// "kind filter applies" semantic is intentional: a knowledge or task row
// sharing a slug with no ISA is not addressable through this verb. It
// guarantees the returned id is always an ISA id, no further check needed.
func runISABySlug(cmd *cobra.Command, args []string) {
	slug := args[0]
	ctx := rootCtx

	if slug == "" {
		fmt.Fprintln(os.Stderr, "slug is required")
		os.Exit(1)
	}

	db, err := openReadDB()
	if err != nil {
		FatalErrorRespectJSON("opening database: %v", err)
	}

	var id string
	err = db.QueryRowContext(ctx,
		"SELECT id FROM issues WHERE slug = ? AND issue_type = 'isa' LIMIT 1",
		slug,
	).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			fmt.Fprintf(os.Stderr, "slug not found: %s\n", slug)
			os.Exit(1)
		}
		FatalErrorRespectJSON("looking up slug: %v", err)
	}

	fmt.Println(id)
}
