package shadow

import (
	"context"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// A lexical retriever, so that the retrieval lane has a floor it can measure
// without an embedding provider.
//
// What this is: BM25 over the repository's own documents, with the document id
// being the path the dataset names. Deterministic, offline, no model, no
// credential, no corpus export. Two runs of the same corpus give the same
// number, which is what makes it usable as a baseline at all.
//
// What this is not: the retriever the gateway will actually use. That one is
// dense, needs an embedding provider, and will answer questions this cannot --
// "what happens when a writer reconnects with an old epoch" shares almost no
// vocabulary with the paragraph that answers it. So a lexical number is a
// lower bound on retrieval quality and is reported under the retriever's own
// name, never as "retrieval works". A dense lane that scores below this floor
// is broken; one that scores above it has only beaten a keyword search.
type LexicalRetriever struct {
	docs   []lexicalDoc
	df     map[string]int
	avgLen float64
}

type lexicalDoc struct {
	id     string
	tf     map[string]int
	length int
}

// BM25 with the usual constants. They are named rather than inlined because a
// baseline whose parameters are invisible is a baseline nobody can reproduce.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// NewLexicalRetriever indexes every file under root whose extension is in
// exts (".md" for the documentation corpus). Document ids are slash-separated
// paths relative to root, which is the shape the frozen datasets use.
func NewLexicalRetriever(root string, exts ...string) (*LexicalRetriever, error) {
	if len(exts) == 0 {
		exts = []string{".md"}
	}
	want := map[string]bool{}
	for _, e := range exts {
		want[strings.ToLower(e)] = true
	}
	r := &LexicalRetriever{df: map[string]int{}}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// A vendored tree or a build directory would swamp the corpus with
			// documents no question is about.
			switch d.Name() {
			case ".git", "node_modules", "vendor", "dist", "build":
				return fs.SkipDir
			}
			return nil
		}
		if !want[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		doc := lexicalDoc{id: filepath.ToSlash(rel), tf: map[string]int{}}
		for _, tok := range lexTokens(string(body)) {
			doc.tf[tok]++
			doc.length++
		}
		for tok := range doc.tf {
			r.df[tok]++
		}
		r.docs = append(r.docs, doc)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("shadow: index the corpus at %s: %w", root, err)
	}
	if len(r.docs) == 0 {
		// An empty corpus would score zero on every case, and a zero that means
		// "nothing was indexed" reported as a recall number is exactly the
		// invented figure this package refuses to produce.
		return nil, fmt.Errorf("shadow: no documents were found under %s", root)
	}
	var total int
	for _, d := range r.docs {
		total += d.length
	}
	r.avgLen = float64(total) / float64(len(r.docs))
	return r, nil
}

// Name identifies what produced a number, including the corpus size, because a
// recall figure over 9 documents and one over 900 are not comparable.
func (r *LexicalRetriever) Name() string {
	return fmt.Sprintf("lexical-bm25 (offline baseline, %d documents)", len(r.docs))
}

// Search returns the k best document ids for a query.
func (r *LexicalRetriever) Search(_ context.Context, query string, k int) ([]string, error) {
	if k <= 0 {
		k = 5
	}
	n := float64(len(r.docs))
	type scored struct {
		id    string
		score float64
	}
	out := make([]scored, 0, len(r.docs))
	terms := lexTokens(query)
	for _, d := range r.docs {
		var score float64
		for _, t := range terms {
			tf := float64(d.tf[t])
			if tf == 0 {
				continue
			}
			df := float64(r.df[t])
			idf := math.Log(1 + (n-df+0.5)/(df+0.5))
			norm := tf * (bm25K1 + 1) /
				(tf + bm25K1*(1-bm25B+bm25B*float64(d.length)/r.avgLen))
			score += idf * norm
		}
		if score > 0 {
			out = append(out, scored{id: d.id, score: score})
		}
	}
	// Ties break on the id so the ranking is stable across runs and across
	// machines; an unstable tail would make the same corpus score differently
	// on two runs and the baseline would measure nothing.
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].id < out[j].id
	})
	if len(out) > k {
		out = out[:k]
	}
	ids := make([]string, 0, len(out))
	for _, s := range out {
		ids = append(ids, s.id)
	}
	return ids, nil
}

// lexTokens lowercases and splits on anything that is not a letter or a digit.
// No stemming and no stop-word list: both are choices that would need their own
// justification, and the point of this retriever is that every part of it can
// be read in one sitting.
func lexTokens(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := fields[:0]
	for _, f := range fields {
		if len(f) > 1 {
			out = append(out, f)
		}
	}
	return out
}
