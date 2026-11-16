package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
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
// and renders the result. It is read-only: SELECTs only, no transaction,
// no writes, so CheckReadonly is not called.
//
// Why Dolt and not the exfil markdown (idea-y0d proposed the markdown): the
// brain wrapper runs with BRAIN_NO_EXFIL=1, so ~/data/brain/entries lags
// the database (2083 files against 3606 beads on 2026-11-16, and only
// three of those files carried a comment). Comments are the point of
// recall, and the database is the only place every one of them lives.
//
// Why the reads are shaped the way they are: the shared server sits on the
// tailnet and the brain store's comments alone are 24 MB, so the cost of a
// query is the text it moves, not the regex it runs. The pure ranker
// tokenizes text itself, so its exact, stem and prefix rules stay the single
// definition of a match; the loader's job is to hand it every word that
// could match, and as little else as possible. Measured 2026-11-16 on the
// live brain store (3,608 beads, 1,539 comments), a cold run for a query of
// common words that touches three comments in four:
//
//   - Only rows passing a broad REGEXP_LIKE prefilter on the query terms
//     ship anything; metadata and CHAR_LENGTH document lengths ship for
//     every bead. The prefilter is decisive for rare words (load 0.4 to
//     0.6 s against 1.2 to 1.6 s without it).
//   - Of a passing row, only the words that begin with a query term cross
//     the wire: the server runs one word-boundary REGEXP_REPLACE
//     (recallKeepPattern) that blanks every other word. Shipping the full
//     text of the same rows, compressed and split eight ways, took 1.9 to
//     2.5 s; shipping the candidate words takes 0.7 to 0.8 s. The ranker
//     then applies its exact/stem/prefix rules to those words, so "go"
//     still does not match "goal": the server trims, it never decides.
//   - The text reads go over a separate pool that asks for MySQL protocol
//     compression (dolt.DoltStore.BulkReadDB) and are split into
//     recallCommentParts and recallBodyParts streams by CRC32(id), all
//     concurrent, so the server's regex work spreads over its cores.
//
// Counting matches on the server was measured and rejected: one
// word-boundary REGEXP_REPLACE per term costs about 1.2 s per term per
// table (5 s for a five-word query), and the plain REPLACE counting the
// first version used matched substrings (go in goal), which the review
// rounds ruled out.
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
		if opener, ok := storage.UnwrapStore(store).(recallBulkReader); ok {
			bulk, err := opener.BulkReadDB(recallReadStreams)
			if err != nil {
				return HandleErrorRespectJSON("brain recall: %v", err)
			}
			defer bulk.Close()
			db = bulk
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

// recallBulkReader is the store seam for the compressed, wider read pool
// (dolt.DoltStore.BulkReadDB). A store without it is read on its own pool.
type recallBulkReader interface {
	BulkReadDB(maxConns int) (*sql.DB, error)
}

// The text reads are partitioned by CRC32(id) into this many concurrent
// streams each; the metadata, label and comment-length reads add three more.
const (
	recallCommentParts = 8
	recallBodyParts    = 4
	recallReadStreams  = recallCommentParts + recallBodyParts + 3
)

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

// recallKeepPattern builds the regex the text streams hand to REGEXP_REPLACE
// so that only words which could match the query reach the client. A word
// is a maximal run of letters and digits, the same unit the ranker's
// tokenizer uses; it survives when it begins with one of the query's Like
// substrings (case-insensitive), and every other word, and every run of
// non-word characters, is replaced by a single space. The ranker still
// applies its own exact, stem and prefix rules to what arrives, so this
// trims the transfer without changing which words count.
func recallKeepPattern(likes []string) string {
	quoted := make([]string, 0, len(likes))
	for _, l := range likes {
		quoted = append(quoted, regexp.QuoteMeta(l))
	}
	return `(?i)(?<![\p{L}\p{N}])(?!(?:` + strings.Join(quoted, "|") + `))[\p{L}\p{N}]+|[^\p{L}\p{N}]+`
}

// loadRecallCorpus reads every bead in scope (id, title, kind, status,
// priority, text length), every label, every bead's comment count and
// length, and — for the beads whose body or comments pass the broad query
// prefilter — the text that the pure ranker must tokenize. The text reads
// are partitioned by CRC32(id); every read runs concurrently on db.
func loadRecallCorpus(ctx context.Context, db *sql.DB, wide bool, likes []string) ([]recallverb.Doc, error) {
	patterns := recallPrefixes(ctx, db, wide)
	regex := strings.Join(likes, "|")
	keep := recallKeepPattern(likes)

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
		bodies   = make([][]textRow, recallBodyParts)
		comments = make([][]textRow, recallCommentParts)
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
	// textWhere builds the WHERE clause of one text stream: namespace scope,
	// the stream's CRC32 slice, then the query prefilter.
	textWhere := func(idCol, textExpr string, parts, part int) (string, []any) {
		where, args := recallScope(idCol, patterns)
		if where != "" {
			where += " AND "
		}
		where += "MOD(CRC32(id), ?) = ? AND REGEXP_LIKE(" + textExpr + ", ?, 'i')"
		return " WHERE " + where, append(args, parts, part, regex)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		where, args := recallScope("id", patterns)
		if where != "" {
			where = " WHERE " + where
		}
		// #nosec G202 -- query structure comes only from fixed expressions and scoped placeholders.
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
	for part := range bodies {
		g.Go(func() error {
			where, args := textWhere("id", recallBodyExpr, recallBodyParts, part)
			// #nosec G202 -- query structure comes only from fixed expressions and scoped placeholders.
			rows, err := db.QueryContext(gctx,
				"SELECT id, REGEXP_REPLACE("+recallBodyExpr+", ?, ' ') FROM issues"+where,
				append([]any{keep}, args...)...)
			if err != nil {
				return fmt.Errorf("recall: read bodies: %w", err)
			}
			out, err := scanText(rows, "body")
			if err != nil {
				return err
			}
			bodies[part] = out
			return nil
		})
	}
	g.Go(func() error {
		where, args := recallScope("issue_id", patterns)
		if where != "" {
			where = " WHERE " + where
		}
		// #nosec G202 -- query structure comes only from fixed expressions and scoped placeholders.
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
		// #nosec G202 -- query structure comes only from fixed expressions and scoped placeholders.
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
	for part := range comments {
		g.Go(func() error {
			where, args := textWhere("issue_id", "text", recallCommentParts, part)
			// #nosec G202 -- query structure comes only from fixed expressions and scoped placeholders.
			rows, err := db.QueryContext(gctx,
				"SELECT issue_id, REGEXP_REPLACE(text, ?, ' ') FROM comments"+where,
				append([]any{keep}, args...)...)
			if err != nil {
				return fmt.Errorf("recall: read comments: %w", err)
			}
			out, err := scanText(rows, "comment")
			if err != nil {
				return err
			}
			comments[part] = out
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	index := make(map[string]int, len(docs))
	for i := range docs {
		index[docs[i].ID] = i
	}
	for _, part := range bodies {
		for _, r := range part {
			if i, ok := index[r.id]; ok {
				docs[i].Body = append(docs[i].Body, r.text)
			}
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
	for _, part := range comments {
		for _, r := range part {
			if i, ok := index[r.id]; ok {
				docs[i].Comments = append(docs[i].Comments, r.text)
			}
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
