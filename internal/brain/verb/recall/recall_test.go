package recall_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/beads/internal/brain/verb/recall"
)

func corpus() []recall.Doc {
	return []recall.Doc{
		{ID: "brain-k1", Title: "Kokoro TTS via speak daemon", Kind: "knowledge", Status: "open",
			Body: []string{"Parlay delivers audio through the speak daemon running Kokoro."}},
		{ID: "brain-m2", Title: "Massage tracker submit pipeline", Kind: "task", Status: "open",
			Body:     []string{"Observability dashboard for the submit path."},
			Comments: []string{"Root cause: the kokoro voice model was never loaded on the mini."}},
		{ID: "brain-g3", Title: "Unrelated grocery list", Kind: "knowledge", Status: "closed",
			Body: []string{"milk eggs bread"}, Labels: []string{"household"}},
		{ID: "brain-t4", Title: "Talon vocabulary rewrite rules", Kind: "knowledge", Status: "open",
			Body:   []string{"user.vocabulary applies after recognition, not before."},
			Labels: []string{"talon", "voice"}},
	}
}

func ids(hits []recall.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ID)
	}
	return out
}

func TestTokenize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want []string
	}{
		{"Kokoro TTS via speak daemon", []string{"kokoro", "tts", "via", "speak", "daemon"}},
		{"comments, commented; commenting", []string{"comment", "comment", "comment"}},
		{"brain-se7t.149", []string{"brain", "se7t", "149"}},
		{"a I x", nil},
		{"é éé", []string{"éé"}},
		{"policies classes", []string{"policy", "class"}},
	}
	for _, tt := range tests {
		if got := recall.Tokenize(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Tokenize(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestRank_CommentOnlyMatchSurfaces(t *testing.T) {
	t.Parallel()
	hits := recall.Rank(corpus(), "kokoro voice model")
	got := ids(hits)
	if len(got) < 2 || got[0] != "brain-m2" {
		t.Fatalf("expected the comment-only bead first, got %v", got)
	}
	var matched []string
	for _, h := range hits {
		if h.ID == "brain-m2" {
			matched = h.Matched
		}
	}
	if !reflect.DeepEqual(matched, []string{"comment"}) {
		t.Errorf("brain-m2 matched fields = %v, want [comment]", matched)
	}
}

func TestRank_TitleMatchOutranksBodyMatch(t *testing.T) {
	t.Parallel()
	got := ids(recall.Rank(corpus(), "kokoro"))
	if !reflect.DeepEqual(got, []string{"brain-k1", "brain-m2"}) {
		t.Errorf("order = %v, want [brain-k1 brain-m2]", got)
	}
}

func TestRank_LabelMatch(t *testing.T) {
	t.Parallel()
	hits := recall.Rank(corpus(), "household")
	if len(hits) != 1 || hits[0].ID != "brain-g3" || !reflect.DeepEqual(hits[0].Matched, []string{"label"}) {
		t.Fatalf("label-only query: %+v", hits)
	}
}

func TestRank_IDTokenMatches(t *testing.T) {
	t.Parallel()
	hits := recall.Rank(corpus(), "brain-t4")
	if len(hits) == 0 || hits[0].ID != "brain-t4" {
		t.Fatalf("querying by id should rank that bead first, got %v", ids(hits))
	}
	if !reflect.DeepEqual(hits[0].Matched, []string{"id"}) {
		t.Errorf("id-only matched fields = %v, want [id]", hits[0].Matched)
	}
}

func TestRank_NoMatchIsEmpty(t *testing.T) {
	t.Parallel()
	if hits := recall.Rank(corpus(), "zebra"); len(hits) != 0 {
		t.Errorf("expected no hits, got %v", ids(hits))
	}
}

func TestRank_DeterministicTieBreak(t *testing.T) {
	t.Parallel()
	docs := []recall.Doc{
		{ID: "b", Title: "same thing"},
		{ID: "a", Title: "same thing"},
	}
	got := ids(recall.Rank(docs, "same"))
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("tie-break order = %v, want [a b]", got)
	}
}

func TestRun_KindFilterAndTop(t *testing.T) {
	t.Parallel()
	src := recall.SourceFunc(func(context.Context) ([]recall.Doc, error) { return corpus(), nil })
	v := recall.New(src)

	res, err := v.Run(context.Background(), recall.Args{Query: "kokoro", Kind: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Corpus != 1 || !reflect.DeepEqual(ids(res.Hits), []string{"brain-m2"}) {
		t.Errorf("kind filter: corpus=%d hits=%v", res.Corpus, ids(res.Hits))
	}

	res, err = v.Run(context.Background(), recall.Args{Query: "kokoro", Top: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].ID != "brain-k1" {
		t.Errorf("top=1: %v", ids(res.Hits))
	}
	if res.Corpus != 4 {
		t.Errorf("corpus = %d, want 4", res.Corpus)
	}
	if res.Matched < 2 {
		t.Errorf("matched = %d, want the pre-cut count (at least 2)", res.Matched)
	}
}

func TestRun_EmptyQuery(t *testing.T) {
	t.Parallel()
	v := recall.New(recall.SourceFunc(func(context.Context) ([]recall.Doc, error) { return nil, nil }))
	if _, err := v.Run(context.Background(), recall.Args{Query: " - "}); !errors.Is(err, recall.ErrEmptyQuery) {
		t.Errorf("err = %v, want ErrEmptyQuery", err)
	}
}

func TestRun_SourceErrorPropagates(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	v := recall.New(recall.SourceFunc(func(context.Context) ([]recall.Doc, error) { return nil, boom }))
	if _, err := v.Run(context.Background(), recall.Args{Query: "boom query"}); !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
}

func TestLikeTerms(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want []string
	}{
		{"Policies and policy", []string{"polic", "and"}},
		{"commented comments", []string{"comment"}},
		{"brain-se7t x", []string{"brain", "se7t"}},
		{"Kokoro TTS", []string{"kokoro", "tts"}},
	}
	for _, tt := range tests {
		if got := recall.LikeTerms(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("LikeTerms(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestRank_SourceLengthOverridesTokenCount(t *testing.T) {
	t.Parallel()
	// Same text; the doc the source marks as much longer scores lower.
	docs := []recall.Doc{
		{ID: "long", Title: "kokoro", Length: 10000},
		{ID: "short", Title: "kokoro", Length: 10},
	}
	got := ids(recall.Rank(docs, "kokoro"))
	if !reflect.DeepEqual(got, []string{"short", "long"}) {
		t.Errorf("order = %v, want [short long]", got)
	}
}

func TestRank_UsesFractionalAverageDocumentLength(t *testing.T) {
	t.Parallel()
	hits := recall.Rank([]recall.Doc{
		{ID: "short", Title: "x", Body: []string{"term"}, Length: 1},
		{ID: "long", Title: "x", Body: []string{"term"}, Length: 2},
	}, "term")
	if got := ids(hits); !reflect.DeepEqual(got, []string{"short", "long"}) {
		t.Fatalf("order = %v, want [short long]", got)
	}
	if hits[0].Score != 0.21 || hits[1].Score != 0.16 {
		t.Errorf("scores = %v, %v; want 0.21, 0.16", hits[0].Score, hits[1].Score)
	}
}

func TestRank_SortsBeforeRounding(t *testing.T) {
	t.Parallel()
	hits := recall.Rank([]recall.Doc{
		{ID: "z-short", Title: "x", Body: []string{"term"}, Length: 100},
		{ID: "a-long", Title: "x", Body: []string{"term"}, Length: 101},
	}, "term")
	if got := ids(hits); !reflect.DeepEqual(got, []string{"z-short", "a-long"}) {
		t.Errorf("true-score order = %v, want [z-short a-long]", got)
	}
	if hits[0].Score != hits[1].Score {
		t.Errorf("display scores = %v, %v; want equal rounded scores", hits[0].Score, hits[1].Score)
	}
}

func TestQueryTerms(t *testing.T) {
	t.Parallel()
	got := recall.QueryTerms("Policies policy commented")
	want := []recall.QueryTerm{{Token: "policy", Like: "polic"}, {Token: "comment", Like: "comment"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("QueryTerms = %v, want %v", got, want)
	}
}

func TestRank_TwoRuneTermsDoNotExpandAsPrefixes(t *testing.T) {
	t.Parallel()
	if terms := recall.QueryTerms("é"); len(terms) != 0 {
		t.Errorf("one-rune query terms = %v, want none", terms)
	}
	hits := recall.Rank([]recall.Doc{{ID: "prefix", Title: "ééx"}}, "éé")
	if len(hits) != 0 {
		t.Errorf("two-rune query matched prefix: %+v", hits)
	}
}

func TestRank_ShortTermsStayExact(t *testing.T) {
	t.Parallel()
	hits := recall.Rank([]recall.Doc{
		{ID: "goal", Title: "x", Body: []string{"goal"}},
		{ID: "go", Title: "x", Body: []string{"go"}},
	}, "go")
	if got := ids(hits); !reflect.DeepEqual(got, []string{"go"}) {
		t.Errorf("short exact query matched %v, want [go]", got)
	}
}

func TestRank_StemsOverlappingPrefixesRemainDistinct(t *testing.T) {
	t.Parallel()
	hits := recall.Rank([]recall.Doc{
		{ID: "both", Title: "x", Body: []string{"code coding"}},
		{ID: "coding", Title: "x", Body: []string{"coding"}},
	}, "code coding")
	if got := ids(hits); !reflect.DeepEqual(got, []string{"both", "coding"}) {
		t.Errorf("overlapping query terms ranked %v, want [both coding]", got)
	}
	if hits[0].Score <= hits[1].Score {
		t.Errorf("exact code occurrence did not improve rank: %+v", hits)
	}
}

func TestRank_NonASCIITokensUseCharacterLengths(t *testing.T) {
	t.Parallel()
	hits := recall.Rank([]recall.Doc{
		{ID: "accented", Title: "café", Length: 4},
		{ID: "plain", Title: "café", Length: 5},
	}, "café")
	if got := ids(hits); !reflect.DeepEqual(got, []string{"accented", "plain"}) {
		t.Errorf("character-length ranking = %v, want [accented plain]", got)
	}
}
