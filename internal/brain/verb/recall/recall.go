// Package recall implements `brain recall`: a BM25-ranked search over every
// text field a bead carries — title, description, design, notes, acceptance
// criteria, close reason, labels, and the full text of its comments.
//
// It exists next to `brain search` (a LIKE match on title/description with a
// structural tiebreak) because the two answer different questions. `search`
// finds beads whose title or description contain every query token; `recall`
// ranks every bead by how relevant its whole body is to the query, so a bead
// that only mentions the thing in a comment, or mentions it many times in a
// long design note, still surfaces. See divergence/0040-brain-recall.md.
//
// The package is pure: it ranks a corpus handed to it by a Source. The cmd/bd
// wrapper is the only place that knows how to read the corpus from Dolt.
package recall

import (
	"bytes"
	"context"
	"errors"
	"math"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"
)

// Name is the verb's name on the brain command tree.
const Name = "recall"

// BM25 parameters. k1 controls term-frequency saturation, b how strongly a
// document's length normalises its score. These are the textbook defaults
// (Robertson et al.); idea-y0d asked for exactly this pair.
const (
	k1 = 1.5
	b  = 0.75
)

// Structural bonuses, each scaled by the matched term's idf so a bonus for a
// rare word is worth more than one for a common word (same shape as the
// idea-y0d proposal of "title +10, label +5" but relative, not absolute).
const (
	titleTermBonus   = 1.0 // each distinct query term found in the title
	titlePhraseBonus = 1.0 // the whole query, in order, inside the title
	labelTermBonus   = 0.5 // each distinct query term found among the labels
	prefixWeight     = 0.5 // a corpus term that merely starts with a query term
)

// Field names reported in Hit.Matched.
const (
	FieldID      = "id"
	FieldTitle   = "title"
	FieldBody    = "body"
	FieldComment = "comment"
	FieldLabel   = "label"
)

// DefaultTop is the number of hits returned when Args.Top is zero.
const DefaultTop = 5

// Doc is one bead as the ranker sees it. Body holds every long-form field in
// one slice (description, design, notes, acceptance criteria, close reason);
// the ranker does not distinguish them, only title/body/comment/label.
type Doc struct {
	ID       string
	Title    string
	Kind     string // issues.issue_type: task, knowledge, isa, decision, …
	Status   string
	Priority int
	Body     []string
	Comments []string
	Labels   []string
	// CommentCount is reported on the hit when Comments contains only the
	// matching comments rather than every comment in the document.
	CommentCount int
	// Length is the doc's full-text length in the source's own unit, when
	// the source did not ship the whole text. Zero means "count my tokens".
	// Every doc of one corpus must use the same unit, because BM25 only
	// ever compares lengths to their average.
	Length int
}

// Source loads the corpus the verb ranks. Load must be read-only.
type Source interface {
	Load(ctx context.Context) ([]Doc, error)
}

// SourceFunc adapts a function to Source.
type SourceFunc func(ctx context.Context) ([]Doc, error)

// Load implements Source.
func (f SourceFunc) Load(ctx context.Context) ([]Doc, error) { return f(ctx) }

// Args is the verb's input.
type Args struct {
	// Query is the free-text query. Required.
	Query string
	// Top caps the number of hits; 0 means DefaultTop, negative means all.
	Top int
	// Kind, when set, keeps only docs whose Kind equals it exactly.
	Kind string
}

// Hit is one ranked bead.
type Hit struct {
	ID       string   `json:"id"`
	Score    float64  `json:"score"`
	Title    string   `json:"title"`
	Kind     string   `json:"kind,omitempty"`
	Status   string   `json:"status,omitempty"`
	Priority int      `json:"priority"`
	Matched  []string `json:"matched"` // which fields matched: id, title, body, comment, label
	Comments int      `json:"comments"`
}

// Result is the verb's output.
type Result struct {
	Query  string   `json:"query"`
	Kind   string   `json:"kind,omitempty"`
	Tokens []string `json:"tokens"`
	// Corpus is the number of docs considered after the kind filter.
	Corpus int `json:"corpus"`
	// Matched is the number of docs with a non-zero score, before Top cuts
	// the list; len(Hits) is what was kept.
	Matched int   `json:"matched"`
	Hits    []Hit `json:"hits"`
}

// Verb ranks a Source's corpus for a query.
type Verb struct {
	src Source
}

// New builds the verb over a Source.
func New(src Source) *Verb { return &Verb{src: src} }

// Name implements verb.BrainVerb.
func (v *Verb) Name() string { return Name }

// ErrEmptyQuery is returned when the query has no indexable token.
var ErrEmptyQuery = errors.New("recall: query has no searchable term")

// Run implements verb.BrainVerb. It never writes to stdout.
func (v *Verb) Run(ctx context.Context, args Args) (Result, error) {
	tokens := Tokenize(args.Query)
	if len(tokens) == 0 {
		return Result{}, ErrEmptyQuery
	}
	docs, err := v.src.Load(ctx)
	if err != nil {
		return Result{}, err
	}
	if args.Kind != "" {
		kept := docs[:0:0]
		for _, d := range docs {
			if d.Kind == args.Kind {
				kept = append(kept, d)
			}
		}
		docs = kept
	}
	hits := Rank(docs, args.Query)
	matched := len(hits)
	top := args.Top
	if top == 0 {
		top = DefaultTop
	}
	if top > 0 && len(hits) > top {
		hits = hits[:top]
	}
	return Result{
		Query:   args.Query,
		Kind:    args.Kind,
		Tokens:  tokens,
		Corpus:  len(docs),
		Matched: matched,
		Hits:    hits,
	}, nil
}

// Tokenize lowercases text, splits on anything that is not a letter or digit,
// drops one-character pieces, and applies a light English suffix stem so that
// "comments", "commented" and "commenting" all land on "comment". It is the
// one tokenizer for both the corpus and the query, so the two always agree:
// the ranker streams the same scanner over the corpus without materialising
// the slice.
func Tokenize(text string) []string {
	var out []string
	scanTokens(text, nil, func(tok []byte) { out = append(out, string(tok)) })
	return out
}

// scanWords calls fn once per maximal run of letters and digits in text,
// lowercased. The slice handed to fn is scratch, valid only during the call;
// buf is the scratch to reuse and the grown buffer is returned so a caller
// scanning many strings allocates once.
func scanWords(text string, buf []byte, fn func(word []byte)) []byte {
	buf = buf[:0]
	for i := 0; i < len(text); {
		c := text[i]
		if c < utf8.RuneSelf {
			i++
			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
				buf = append(buf, c)
				continue
			case c >= 'A' && c <= 'Z':
				buf = append(buf, c+('a'-'A'))
				continue
			}
		} else {
			r, size := utf8.DecodeRuneInString(text[i:])
			i += size
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				buf = utf8.AppendRune(buf, unicode.ToLower(r))
				continue
			}
		}
		if len(buf) > 0 {
			fn(buf)
			buf = buf[:0]
		}
	}
	if len(buf) > 0 {
		fn(buf)
	}
	return buf
}

// scanTokens is scanWords followed by the stem and the two-character floor:
// exactly the tokens Tokenize would return, streamed.
func scanTokens(text string, buf []byte, fn func(tok []byte)) []byte {
	return scanWords(text, buf, func(word []byte) {
		if tok := stemBytes(word); utf8.RuneCount(tok) >= 2 {
			fn(tok)
		}
	})
}

// QueryTerm is one distinct query token with the substring a source may
// use to find it on the server.
type QueryTerm struct {
	// Token is the stemmed token the ranker scores.
	Token string
	// Like is the longest prefix shared by the raw word, its stem, and
	// every corpus word that stems to the same token, so a filter built from
	// it (a `LOWER(col) LIKE '%like%'`, a case-insensitive regex, or a
	// word-start test) keeps every word Rank could score: "policies",
	// "policy" and "vocabularies" all survive filters built from "polic"
	// and "vocabular". Lowercase alphanumeric only.
	Like string
}

// QueryTerms tokenizes the query the way Tokenize does and pairs each
// distinct token with its Like substring (the shortest seen for that token).
func QueryTerms(query string) []QueryTerm {
	var out []QueryTerm
	at := map[string]int{}
	scanWords(query, nil, func(word []byte) {
		raw := string(word)
		if runeLen(raw) < 2 {
			return
		}
		tok := stem(raw)
		if runeLen(tok) < 2 {
			return
		}
		like := commonPrefix(raw, tok)
		// A corpus word ending in "ies" stems to this token's "y" form
		// ("vocabularies" to "vocabulary"), and shares only the stem minus
		// that "y" with it, so the filter must stop before the "y" too.
		if strings.HasSuffix(tok, "y") && runeLen(tok) >= 3 {
			like = commonPrefix(like, tok[:len(tok)-1])
		}
		if runeLen(like) < 2 {
			like = raw
		}
		if i, ok := at[tok]; ok {
			if runeLen(like) < runeLen(out[i].Like) {
				out[i].Like = like
			}
			return
		}
		at[tok] = len(out)
		out = append(out, QueryTerm{Token: tok, Like: like})
	})
	return out
}

// commonPrefix answers the longest common byte prefix of a and b.
func commonPrefix(a, b string) string {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return a[:n]
}

// LikeTerms answers the distinct Like substrings of QueryTerms, minus any
// that starts with another (whatever it matches, the shorter one matches
// too). This is the set a server-side prefilter needs.
func LikeTerms(query string) []string {
	terms := QueryTerms(query)
	var out []string
	seen := map[string]bool{}
	for _, t := range terms {
		if seen[t.Like] {
			continue
		}
		seen[t.Like] = true
		out = append(out, t.Like)
	}
	pruned := out[:0:0]
	for _, t := range out {
		redundant := false
		for _, u := range out {
			if u != t && strings.HasPrefix(t, u) {
				redundant = true
				break
			}
		}
		if !redundant {
			pruned = append(pruned, t)
		}
	}
	return pruned
}

// stem strips a handful of English suffixes when a reasonably long stem
// remains. It is deliberately crude: recall matches, it does not parse.
func stem(w string) string {
	return string(stemBytes([]byte(w)))
}

// stemBytes is stem over a scratch slice. It may write into w's backing
// array (the "ies" case overwrites in place) and returns the stemmed slice.
// Lengths are character counts, not byte counts, so a non-ASCII word is held
// to the same floor as an ASCII one.
func stemBytes(w []byte) []byte {
	switch {
	case bytes.HasSuffix(w, sufIES) && utf8.RuneCount(w) > 4:
		return append(w[:len(w)-3], 'y')
	case bytes.HasSuffix(w, sufSSES) && utf8.RuneCount(w) > 5:
		return w[:len(w)-2]
	case bytes.HasSuffix(w, sufING) && utf8.RuneCount(w) > 6:
		return w[:len(w)-3]
	case bytes.HasSuffix(w, sufED) && utf8.RuneCount(w) > 5:
		return w[:len(w)-2]
	case bytes.HasSuffix(w, sufES) && utf8.RuneCount(w) > 4 && !bytes.HasSuffix(w, sufSES):
		return w[:len(w)-2]
	case bytes.HasSuffix(w, sufS) && !bytes.HasSuffix(w, sufSS) && utf8.RuneCount(w) > 3:
		return w[:len(w)-1]
	}
	return w
}

var (
	sufIES  = []byte("ies")
	sufSSES = []byte("sses")
	sufING  = []byte("ing")
	sufED   = []byte("ed")
	sufES   = []byte("es")
	sufSES  = []byte("ses")
	sufS    = []byte("s")
	sufSS   = []byte("ss")
)

// fieldMask records which fields of a doc held a match.
type fieldMask uint8

const (
	maskID fieldMask = 1 << iota
	maskTitle
	maskBody
	maskComment
	maskLabel
)

func (m fieldMask) names() []string {
	out := make([]string, 0, 5)
	if m&maskID != 0 {
		out = append(out, FieldID)
	}
	if m&maskTitle != 0 {
		out = append(out, FieldTitle)
	}
	if m&maskBody != 0 {
		out = append(out, FieldBody)
	}
	if m&maskComment != 0 {
		out = append(out, FieldComment)
	}
	if m&maskLabel != 0 {
		out = append(out, FieldLabel)
	}
	return out
}

// indexed is the per-doc term view the ranker scores. Only the query's terms
// are counted, so the index is as small as the query, not the corpus. Term
// slots are indexed by the query term's position in Rank's term list.
type indexed struct {
	doc     *Doc
	length  int       // tokens across all fields
	tf      []float64 // weighted term frequency per query term
	matched bool      // any tf slot is non-zero
	mask    fieldMask
	title   []string // title tokens, for the phrase bonus
	tTerms  []bool   // query terms found in the title
	lTerms  []bool   // query terms found among the labels
}

// Rank scores every doc against the query and returns all docs with a
// positive score, best first, ties broken by id so output is reproducible.
func Rank(docs []Doc, query string) []Hit {
	terms := QueryTerms(query)
	if len(terms) == 0 || len(docs) == 0 {
		return nil
	}
	qTokens := make([]string, 0, len(terms))
	slot := make(map[string]int, len(terms))
	for i, t := range terms {
		qTokens = append(qTokens, t.Token)
		slot[t.Token] = i
	}
	// Prefix expansion applies to query tokens of at least four characters:
	// "tailnet" should also pick up "tailnets"-like words the stemmer missed
	// and "kokoro" should match "kokoro82m". Shorter tokens stay exact to
	// keep "go" from matching "goal" and "god".
	var prefixes []prefixTerm
	for i, t := range qTokens {
		if runeLen(t) >= 4 {
			prefixes = append(prefixes, prefixTerm{text: []byte(t), slot: i})
		}
	}

	// Index in parallel. Docs vary from a one-line task to a design note with
	// a hundred comments, so workers pull the next doc from a shared counter
	// rather than splitting the slice into equal ranges: no worker idles
	// while another finishes a run of long documents.
	idx := make([]indexed, len(docs))
	workers := runtime.NumCPU()
	if workers > len(docs) {
		workers = len(docs)
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ix := indexer{slot: slot, prefixes: prefixes, nTerms: len(terms), phrase: len(qTokens) > 1}
			for {
				i := int(next.Add(1) - 1)
				if i >= len(docs) {
					return
				}
				idx[i] = ix.index(&docs[i])
			}
		}()
	}
	wg.Wait()

	// Document frequency per query term, and the average length, both over
	// the corpus the caller handed in (after the kind filter).
	df := make([]int, len(terms))
	totalLen := 0
	for i := range idx {
		totalLen += idx[i].length
		if !idx[i].matched {
			continue
		}
		for t, f := range idx[i].tf {
			if f > 0 {
				df[t]++
			}
		}
	}
	n := float64(len(docs))
	avgLen := float64(totalLen) / float64(len(docs))
	if avgLen == 0 {
		avgLen = 1
	}
	idf := make([]float64, len(terms))
	for t := range qTokens {
		d := float64(df[t])
		idf[t] = math.Log(1 + (n-d+0.5)/(d+0.5))
	}

	type scoredHit struct {
		hit   Hit
		score float64
	}
	hits := make([]scoredHit, 0, len(idx))
	for i := range idx {
		d := &idx[i]
		if !d.matched {
			continue
		}
		score := 0.0
		norm := k1 * (1 - b + b*float64(d.length)/avgLen)
		for t, f := range d.tf {
			if f > 0 {
				score += idf[t] * (f * (k1 + 1)) / (f + norm)
			}
		}
		for t, ok := range d.tTerms {
			if ok {
				score += titleTermBonus * idf[t]
			}
		}
		for t, ok := range d.lTerms {
			if ok {
				score += labelTermBonus * idf[t]
			}
		}
		if len(qTokens) > 1 && containsPhrase(d.title, qTokens) {
			for _, t := range qTokens {
				score += titlePhraseBonus * idf[slot[t]]
			}
		}
		if score <= 0 {
			continue
		}
		comments := d.doc.CommentCount
		if comments == 0 {
			comments = len(d.doc.Comments)
		}
		hits = append(hits, scoredHit{score: score, hit: Hit{
			ID:       d.doc.ID,
			Title:    d.doc.Title,
			Kind:     d.doc.Kind,
			Status:   d.doc.Status,
			Priority: d.doc.Priority,
			Matched:  d.mask.names(),
			Comments: comments,
		}})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].hit.ID < hits[j].hit.ID
	})
	result := make([]Hit, len(hits))
	for i := range hits {
		hits[i].hit.Score = round2(hits[i].score)
		result[i] = hits[i].hit
	}
	return result
}

// prefixTerm is a query token long enough for prefix expansion, with the
// term slot its partial matches count toward.
type prefixTerm struct {
	text []byte
	slot int
}

// indexer counts query terms in one doc at a time. One lives per worker: the
// scratch buffer is reused across every string the worker tokenizes, so
// indexing a 34 MB corpus allocates per doc, not per token.
type indexer struct {
	slot     map[string]int
	prefixes []prefixTerm
	nTerms   int
	phrase   bool
	buf      []byte
}

// index tokenizes one doc and counts the query terms it holds. A token that
// exactly equals a query term counts 1; a token that merely starts with a
// query prefix counts prefixWeight toward that term.
func (ix *indexer) index(doc *Doc) indexed {
	d := indexed{
		doc:    doc,
		tf:     make([]float64, ix.nTerms),
		tTerms: make([]bool, ix.nTerms),
		lTerms: make([]bool, ix.nTerms),
	}
	count := func(text string, mask fieldMask, terms []bool) {
		ix.buf = scanTokens(text, ix.buf, func(tok []byte) {
			d.length++
			// string(tok) in a map index does not allocate.
			if s, ok := ix.slot[string(tok)]; ok {
				d.tf[s]++
				d.matched = true
				d.mask |= mask
				if terms != nil {
					terms[s] = true
				}
				return
			}
			for _, p := range ix.prefixes {
				if len(tok) > len(p.text) && bytes.HasPrefix(tok, p.text) {
					d.tf[p.slot] += prefixWeight
					d.matched = true
					d.mask |= mask
					if terms != nil {
						terms[p.slot] = true
					}
					break
				}
			}
		})
	}

	// The id and its tail are indexed so `brain recall p20f`
	// finds project-p20f even though the body never repeats the id.
	count(doc.Title, maskTitle, d.tTerms)
	if ix.phrase {
		d.title = Tokenize(doc.Title)
	}
	idLen := d.length
	count(doc.ID, maskID, nil)
	d.length = idLen // the id is matchable but not part of the doc's length

	for _, text := range doc.Body {
		count(text, maskBody, nil)
	}
	for _, text := range doc.Comments {
		count(text, maskComment, nil)
	}
	for _, label := range doc.Labels {
		count(label, maskLabel, d.lTerms)
	}
	if doc.Length > 0 {
		d.length = doc.Length
	}
	return d
}

// containsPhrase reports whether tokens holds phrase as a contiguous run.
func containsPhrase(tokens, phrase []string) bool {
	if len(phrase) == 0 || len(tokens) < len(phrase) {
		return false
	}
outer:
	for i := 0; i+len(phrase) <= len(tokens); i++ {
		for j := range phrase {
			if tokens[i+j] != phrase[j] {
				continue outer
			}
		}
		return true
	}
	return false
}

func round2(f float64) float64 {
	return math.Round(f*100) / 100
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }
