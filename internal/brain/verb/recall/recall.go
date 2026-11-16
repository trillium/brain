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
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
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
	Matched  []string `json:"matched"` // which fields matched: title, body, comment, label
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
// one tokenizer for both the corpus and the query, so the two always agree.
func Tokenize(text string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		if tok := stem(cur.String()); runeLen(tok) >= 2 {
			out = append(out, tok)
		}
		cur.Reset()
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(unicode.ToLower(r))
			continue
		}
		flush()
	}
	flush()
	return out
}

// QueryTerm is one distinct query token with the substring a source may
// use to find it on the server.
type QueryTerm struct {
	// Token is the stemmed token the ranker scores.
	Token string
	// Like is the longest common prefix of the raw word and its stem, so a
	// `LOWER(col) LIKE '%like%'` (or a case-insensitive regex) built from it
	// keeps every row Rank could score: both "policies" and "policy"
	// survive a filter built from "polic". Lowercase alphanumeric only.
	Like string
}

// QueryTerms tokenizes the query the way Tokenize does and pairs each
// distinct token with its Like substring (the shortest seen for that token).
func QueryTerms(query string) []QueryTerm {
	var out []QueryTerm
	at := map[string]int{}
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		raw := cur.String()
		cur.Reset()
		if runeLen(raw) < 2 {
			return
		}
		tok := stem(raw)
		if runeLen(tok) < 2 {
			return
		}
		n := 0
		for n < len(raw) && n < len(tok) && raw[n] == tok[n] {
			n++
		}
		like := raw[:n]
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
	}
	for _, r := range query {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(unicode.ToLower(r))
			continue
		}
		flush()
	}
	flush()
	return out
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
	switch {
	case strings.HasSuffix(w, "ies") && runeLen(w) > 4:
		return w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "sses") && runeLen(w) > 5:
		return w[:len(w)-2]
	case strings.HasSuffix(w, "ing") && runeLen(w) > 6:
		return w[:len(w)-3]
	case strings.HasSuffix(w, "ed") && runeLen(w) > 5:
		return w[:len(w)-2]
	case strings.HasSuffix(w, "es") && runeLen(w) > 4 && !strings.HasSuffix(w, "ses"):
		return w[:len(w)-2]
	case strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && runeLen(w) > 3:
		return w[:len(w)-1]
	}
	return w
}

// fieldMask records which fields of a doc held a match.
type fieldMask uint8

const (
	maskTitle fieldMask = 1 << iota
	maskBody
	maskComment
	maskLabel
)

func (m fieldMask) names() []string {
	out := make([]string, 0, 4)
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
// are counted, so the index is as small as the query, not the corpus.
type indexed struct {
	doc    *Doc
	length int                // tokens across all fields
	tf     map[string]float64 // weighted term frequency per query term
	mask   fieldMask
	title  []string // title tokens, for the phrase bonus
	tTerms map[string]bool
	lTerms map[string]bool
}

// Rank scores every doc against the query and returns all docs with a
// positive score, best first, ties broken by id so output is reproducible.
func Rank(docs []Doc, query string) []Hit {
	terms := QueryTerms(query)
	if len(terms) == 0 || len(docs) == 0 {
		return nil
	}
	qTokens := make([]string, 0, len(terms))
	qSet := make(map[string]bool, len(terms))
	for _, t := range terms {
		qTokens = append(qTokens, t.Token)
		qSet[t.Token] = true
	}
	// Prefix expansion applies to query tokens of at least four characters:
	// "tailnet" should also pick up "tailnets"-like words the stemmer missed
	// and "kokoro" should match "kokoro82m". Shorter tokens stay exact to
	// keep "go" from matching "goal" and "god".
	prefixes := make([]string, 0, len(qTokens))
	for _, t := range qTokens {
		if runeLen(t) >= 4 {
			prefixes = append(prefixes, t)
		}
	}

	idx := make([]indexed, len(docs))
	var wg sync.WaitGroup
	workers := 8
	if len(docs) < workers {
		workers = 1
	}
	chunk := (len(docs) + workers - 1) / workers
	for w := 0; w < workers; w++ {
		lo, hi := w*chunk, (w+1)*chunk
		if hi > len(docs) {
			hi = len(docs)
		}
		if lo >= hi {
			break
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			for i := lo; i < hi; i++ {
				idx[i] = indexDoc(&docs[i], terms, qSet, prefixes)
			}
		}(lo, hi)
	}
	wg.Wait()

	// Document frequency per query term, and the average length, both over
	// the corpus the caller handed in (after the kind filter).
	df := make(map[string]int, len(qTokens))
	totalLen := 0
	for i := range idx {
		totalLen += idx[i].length
		for t := range idx[i].tf {
			df[t]++
		}
	}
	n := float64(len(docs))
	avgLen := float64(totalLen) / float64(len(docs))
	if avgLen == 0 {
		avgLen = 1
	}
	idf := make(map[string]float64, len(qTokens))
	for _, t := range qTokens {
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
		if len(d.tf) == 0 {
			continue
		}
		score := 0.0
		norm := k1 * (1 - b + b*float64(d.length)/avgLen)
		for t, f := range d.tf {
			score += idf[t] * (f * (k1 + 1)) / (f + norm)
		}
		for t := range d.tTerms {
			score += titleTermBonus * idf[t]
		}
		for t := range d.lTerms {
			score += labelTermBonus * idf[t]
		}
		if len(qTokens) > 1 && containsPhrase(d.title, qTokens) {
			for _, t := range qTokens {
				score += titlePhraseBonus * idf[t]
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

// indexDoc tokenizes one doc and counts the query terms it holds. A token
// that exactly equals a query term counts 1; a token that merely starts with
// a query prefix counts prefixWeight toward that term.
func indexDoc(doc *Doc, terms []QueryTerm, qSet map[string]bool, prefixes []string) indexed {
	d := indexed{doc: doc, tf: map[string]float64{}}
	count := func(tokens []string, mask fieldMask, terms map[string]bool) {
		for _, tok := range tokens {
			if qSet[tok] {
				d.tf[tok]++
				d.mask |= mask
				if terms != nil {
					terms[tok] = true
				}
				continue
			}
			for _, p := range prefixes {
				if runeLen(tok) > runeLen(p) && strings.HasPrefix(tok, p) {
					d.tf[p] += prefixWeight
					d.mask |= mask
					if terms != nil {
						terms[p] = true
					}
					break
				}
			}
		}
	}
	d.tTerms = map[string]bool{}
	d.lTerms = map[string]bool{}

	// The id and its tail are indexed as title tokens so `brain recall p20f`
	// finds project-p20f even though the body never repeats the id.
	d.title = Tokenize(doc.Title)
	idTokens := Tokenize(doc.ID)
	d.length = len(d.title)
	count(d.title, maskTitle, d.tTerms)
	count(idTokens, maskTitle, d.tTerms)

	for _, text := range doc.Body {
		toks := Tokenize(text)
		d.length += len(toks)
		count(toks, maskBody, nil)
	}
	for _, text := range doc.Comments {
		toks := Tokenize(text)
		d.length += len(toks)
		count(toks, maskComment, nil)
	}
	for _, label := range doc.Labels {
		toks := Tokenize(label)
		d.length += len(toks)
		count(toks, maskLabel, d.lTerms)
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
