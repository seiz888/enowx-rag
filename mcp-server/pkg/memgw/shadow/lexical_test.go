package shadow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// corpus writes a small document tree and returns its root.
func corpus(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestAnEmptyCorpusIsRefusedRatherThanScoredZero(t *testing.T) {
	// A recall of zero that means "nothing was indexed" is the invented figure
	// this package exists to refuse.
	if _, err := NewLexicalRetriever(t.TempDir()); err == nil {
		t.Fatal("an empty corpus produced a retriever")
	}
}

func TestTheBaselineRanksTheDocumentThatAnswersTheQuestion(t *testing.T) {
	root := corpus(t, map[string]string{
		"docs/fencing.md":   "A writer that reconnects with an old epoch is fenced and its writes are refused.",
		"docs/spool.md":     "Releasing a quarantined row from the collector spool puts it back in the queue.",
		"docs/unrelated.md": "The dashboard is served by nginx behind a password.",
	})
	r, err := NewLexicalRetriever(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Search(t.Context(), "what happens when a writer reconnects with an old epoch", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0] != "docs/fencing.md" {
		t.Fatalf("the baseline ranked %v first", got)
	}
	if !strings.Contains(r.Name(), "3 documents") {
		t.Fatalf("the name does not say how big the corpus was: %s", r.Name())
	}
}

func TestTheRankingIsTheSameOnEveryRun(t *testing.T) {
	// A baseline whose tail reorders between runs measures nothing: two runs of
	// the same corpus would disagree about recall for no reason anyone could
	// investigate.
	root := corpus(t, map[string]string{
		"a.md": "collector spool row",
		"b.md": "collector spool row",
		"c.md": "collector spool row",
	})
	r, err := NewLexicalRetriever(root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.Search(t.Context(), "collector spool", 3)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := r.Search(t.Context(), "collector spool", 3)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(again, ",") != strings.Join(first, ",") {
			t.Fatalf("run %d ranked %v, the first run ranked %v", i, again, first)
		}
	}
}

func TestAQueryWithNothingInCommonReturnsNothing(t *testing.T) {
	// Padding the answer to k with the least-bad document would make recall
	// look like retrieval when it is arithmetic.
	root := corpus(t, map[string]string{"a.md": "collector spool row"})
	r, err := NewLexicalRetriever(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Search(t.Context(), "payroll approval workflow", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a query sharing no term returned %v", got)
	}
}
