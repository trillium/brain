package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	recallverb "github.com/steveyegge/beads/internal/brain/verb/recall"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/dolt"
	"github.com/steveyegge/beads/internal/ui"
	"golang.org/x/sync/errgroup"
)

// brainRecallCmd is the Cobra wrapper for `brain recall <query>`.
//
// Ranking lives in internal/brain/verb/recall (the BrainVerb seam). This
// file reads the corpus from Dolt, applies the wrapper's namespace scope,
// and renders the result. It is read-only: four SELECTs, no transaction,
// no writes, so CheckReadonly is not called.
//
// Why Dolt and not the exfil markdown (idea-y0d proposed the markdown): the
// brain wrapper runs with BRAIN_NO_EXFIL=1, so ~/data/brain/entries lags
// the database (2083 files against 3606 beads on 2026-11-16, and only
// three of those files carried a comment). Comments are the point of
// recall, and the database is the only place every one of them lives.
//
// Why the reads are shaped the way they are: the shared server sits on the
// tailnet, and the brain store's comments alone are ~24MB, so pulling every
// body for every query costs seconds of transfer (and a common word like
// "phone" still matches 14MB of comments). Instead the server sends metadata
// and character lengths for every bead, then sends body and comment text only
// for rows matching the ranker's broad query prefilter. The pure ranker
// tokenizes that candidate text itself, so its exact, stem, and prefix rules
// remain the single definition of what a match is. The five reads run
// concurrently.
var brainRecallCmd = &cobra.Command{
	Use:   "recall <query>",
	Short: "BM25-ranked search over full bead bodies and comments",
	Long: `brain recall ranks every bead in this wrapper's namespace against a
free-text query and prints the best matches with their scores.

brain search is a substring filter: every word of the query must appear
somewhere in the bead, and the survivors are ordered by where the words sit.
recall is a ranking instead. It weighs the whole bead — title, description,
design, notes, acceptance criteria, close reason, labels and the full text
of every comment — with BM25 (term frequency against document length, rare
words counting more), so a bead that mentions the topic only in a comment,
or many times in a long note, surfaces with a score that says how well it
fits. Words need not all match; the ones that do decide the rank. All
statuses are searched, closed beads included.

Each hit prints its score, status icon, id, kind, title, and which fields
matched (id, title, body, comment, label). --json emits the same as an object
with query, tokens, corpus size and hits. No hit at all exits non-zero.

--kind filters on the bead's issue_type exactly (knowledge, task, isa,
decision, …). --wide ranks every store's beads instead of this wrapper's.

The memory plane's read-by-key verb moved to 'bd memories recall <key>'
('bd remember <key>' reads it too).

Examples:
  brain recall "kokoro speak daemon"
  brain recall --top 10 "tailnet phone"
  brain recall --kind knowledge --json "talon vocabulary"`,
	Args:          cobra.MinimumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		top, _ := cmd.Flags().GetInt("top")
		kind, _ := cmd.Flags().GetString("kind")
		query := strings.Join(args, " ")

		ctx := rootCtx
		if ctx == nil {
			ctx = context.Background()
		}
		if store == nil {
			return HandleErrorRespectJSON("no database connection available (%s)", diagHint())
		}
		accessor, ok := storage.UnwrapStore(store).(storage.RawDBAccessor)
		if !ok {
			return HandleErrorRespectJSON("brain recall needs a Dolt-backed store with raw database access")
		}
		db := accessor.UnderlyingDB()
		if db == nil {
			return HandleErrorRespectJSON("brain recall: raw database handle is unavailable")
		}

		src := recallverb.SourceFunc(func(ctx context.Context) ([]recallverb.Doc, error) {
			return loadRecallCorpus(ctx, db, namespaceWide, recallverb.LikeTerms(query))
		})
		result, err := recallverb.New(src).Run(ctx, recallverb.Args{
			Query: query,
			Top:   top,
			Kind:  kind,
		})
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}

		if jsonOutput {
			if err := outputJSON(result); err != nil {
				return err
			}
		} else {
			renderRecall(result)
		}
		if len(result.Hits) == 0 {
			return SilentExit()
		}
		return nil
	},
}

// recallPrefixes answers the `id LIKE ?` patterns a read should carry. Empty
// means unscoped: wide mode, or a database that is not unified.
func recallPrefixes(ctx context.Context, db *sql.DB, wide bool) []string {
	if wide {
		return nil
	}
	prefixes, unified := dolt.NarrowNamespacePrefixes(ctx, db)
	if !unified {
		return nil
	}
	out := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		out = append(out, strings.TrimSuffix(p, "-")+"-%")
	}
	return out
}

// recallScope builds the namespace clause on idCol: "" when unscoped.
func recallScope(idCol string, patterns []string) (string, []any) {
	if len(patterns) == 0 {
		return "", nil
	}
	ors := make([]string, len(patterns))
	args := make([]any, len(patterns))
	for i, p := range patterns {
		ors[i] = idCol + " LIKE ?"
		args[i] = p
	}
	return "(" + strings.Join(ors, " OR ") + ")", args
}

const recallBodyExpr = "CONCAT_WS(' ', COALESCE(description,''), COALESCE(design,''), " +
	"COALESCE(notes,''), COALESCE(acceptance_criteria,''), COALESCE(close_reason,''))"

// loadRecallCorpus reads every bead in scope (id, title, kind, status,
// priority, text length), every label, every bead's comment count and
// length, and — for the beads whose body or comments pass the broad query
// prefilter — the text that the pure ranker must tokenize. The five reads run
// concurrently on the pool's connections.
func loadRecallCorpus(ctx context.Context, db *sql.DB, wide bool, likes []string) ([]recallverb.Doc, error) {
	patterns := recallPrefixes(ctx, db, wide)
	regex := strings.Join(likes, "|")

	type labelRow struct {
		id, label string
		chars     int
	}
	type textRow struct{ id, text string }
	type lenRow struct {
		id       string
		comments int
		chars    int
	}
	var (
		docs     []recallverb.Doc
		bodies   []textRow
		comments []textRow
		labels   []labelRow
		clens    []lenRow
	)

	scanText := func(rows *sql.Rows, what string) ([]textRow, error) {
		defer rows.Close()
		var out []textRow
		for rows.Next() {
			var r textRow
			if err := rows.Scan(&r.id, &r.text); err != nil {
				return nil, fmt.Errorf("recall: scan %s: %w", what, err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("recall: read %s: %w", what, err)
		}
		return out, nil
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		where, args := recallScope("id", patterns)
		if where != "" {
			where = " WHERE " + where
		}
		rows, err := db.QueryContext(gctx,
			"SELECT id, COALESCE(title,''), COALESCE(issue_type,''), COALESCE(status,''), COALESCE(priority,0), "+
				"CHAR_LENGTH(COALESCE(title,'')) + CHAR_LENGTH("+recallBodyExpr+") FROM issues"+where, args...)
		if err != nil {
			return fmt.Errorf("recall: read issues: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var d recallverb.Doc
			if err := rows.Scan(&d.ID, &d.Title, &d.Kind, &d.Status, &d.Priority, &d.Length); err != nil {
				return fmt.Errorf("recall: scan issue: %w", err)
			}
			docs = append(docs, d)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("recall: read issues: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		scope, args := recallScope("id", patterns)
		if scope != "" {
			scope += " AND "
		}
		q := "SELECT id, " + recallBodyExpr + " FROM issues WHERE " +
			scope + "REGEXP_LIKE(" + recallBodyExpr + ", ?, 'i')"
		rows, err := db.QueryContext(gctx, q, append(args, regex)...)
		if err != nil {
			return fmt.Errorf("recall: read bodies: %w", err)
		}
		out, err := scanText(rows, "body")
		if err != nil {
			return err
		}
		bodies = out
		return nil
	})
	g.Go(func() error {
		where, args := recallScope("issue_id", patterns)
		if where != "" {
			where = " WHERE " + where
		}
		rows, err := db.QueryContext(gctx, "SELECT issue_id, label, CHAR_LENGTH(label) FROM labels"+where, args...)
		if err != nil {
			return fmt.Errorf("recall: read labels: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r labelRow
			if err := rows.Scan(&r.id, &r.label, &r.chars); err != nil {
				return fmt.Errorf("recall: scan label: %w", err)
			}
			labels = append(labels, r)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("recall: read labels: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		where, args := recallScope("issue_id", patterns)
		if where != "" {
			where = " WHERE " + where
		}
		rows, err := db.QueryContext(gctx,
			"SELECT issue_id, COUNT(*), SUM(CHAR_LENGTH(text)) FROM comments"+where+" GROUP BY issue_id", args...)
		if err != nil {
			return fmt.Errorf("recall: read comment lengths: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r lenRow
			if err := rows.Scan(&r.id, &r.comments, &r.chars); err != nil {
				return fmt.Errorf("recall: scan comment length: %w", err)
			}
			clens = append(clens, r)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("recall: read comment lengths: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		scope, args := recallScope("issue_id", patterns)
		if scope != "" {
			scope += " AND "
		}
		q := "SELECT issue_id, text FROM comments WHERE " +
			scope + "REGEXP_LIKE(text, ?, 'i')"
		rows, err := db.QueryContext(gctx, q, append(args, regex)...)
		if err != nil {
			return fmt.Errorf("recall: read comments: %w", err)
		}
		out, err := scanText(rows, "comment")
		if err != nil {
			return err
		}
		comments = out
		return nil
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}

	index := make(map[string]int, len(docs))
	for i := range docs {
		index[docs[i].ID] = i
	}
	for _, r := range bodies {
		if i, ok := index[r.id]; ok {
			docs[i].Body = append(docs[i].Body, r.text)
		}
	}
	for _, r := range labels {
		if i, ok := index[r.id]; ok {
			docs[i].Labels = append(docs[i].Labels, r.label)
			docs[i].Length += r.chars
		}
	}
	for _, r := range clens {
		if i, ok := index[r.id]; ok {
			docs[i].Length += r.chars
			docs[i].CommentCount = r.comments
		}
	}
	for _, r := range comments {
		if i, ok := index[r.id]; ok {
			docs[i].Comments = append(docs[i].Comments, r.text)
		}
	}
	return docs, nil
}

// renderRecall prints one line per hit:
//
//	12.34  ○ brain-abc1  [knowledge]  Title of the bead   (title, comment)
func renderRecall(result recallverb.Result) {
	if len(result.Hits) == 0 {
		fmt.Fprintf(os.Stderr, "No matches for %q across %d beads\n", result.Query, result.Corpus)
		return
	}
	for _, h := range result.Hits {
		kind := h.Kind
		if kind == "" {
			kind = "-"
		}
		fmt.Printf("%7.2f  %s %s  [%s]  %s   (%s)\n",
			h.Score,
			ui.RenderStatusIcon(h.Status),
			styleID(h.ID, h.Status == "closed"),
			kind,
			h.Title,
			strings.Join(h.Matched, ", "))
	}
	fmt.Printf("\ntop %d of %d matched (%d beads)\n", len(result.Hits), result.Matched, result.Corpus)
}

func init() {
	brainRecallCmd.Flags().Int("top", recallverb.DefaultTop, "Number of hits to print (negative for all)")
	brainRecallCmd.Flags().String("kind", "", "Only beads of this kind (issue_type): knowledge, task, isa, decision, …")
	brainCmd.AddCommand(brainRecallCmd)
	rootCmd.AddCommand(brainRecallCmd)
}
