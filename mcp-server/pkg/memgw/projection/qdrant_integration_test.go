// Integration tests against a real, disposable Qdrant.
//
// They skip when MEMGW_TEST_QDRANT_URL is unset, so `go test ./...` on a
// machine with no container stays green and needs no network. When it is set,
// the target must be a loopback address: these tests create and delete
// collections, and a URL that had drifted to a shared instance would delete
// somebody's collection rather than fail.
//
// What they prove is that the wiring is real -- points are written, filtered,
// found and deleted by the same code the worker runs. What they emphatically do
// NOT prove is retrieval quality: the vectors come from FixtureEmbedder, which
// has no semantic content. Semantic quality is untested.
package projection

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/outbox"
	"github.com/enowdev/enowx-rag/pkg/rag"
)

// QdrantEnv names the environment variable holding the disposable Qdrant.
const QdrantEnv = "MEMGW_TEST_QDRANT_URL"

func qdrantIndex(t *testing.T) (*QdrantIndex, *rag.QdrantProvider, string) {
	t.Helper()
	url := strings.TrimSpace(os.Getenv(QdrantEnv))
	if url == "" {
		t.Skipf("%s is not set; skipping the Qdrant integration test", QdrantEnv)
	}
	// The same refusal the PostgreSQL side makes, for the same reason: a test
	// that creates and deletes collections must not be able to reach a shared
	// instance because a variable was edited.
	if !strings.Contains(url, "127.0.0.1") && !strings.Contains(url, "localhost") && !strings.Contains(url, "[::1]") {
		t.Fatalf("%s must point at a loopback address; refusing to run against %q", QdrantEnv, url)
	}
	p, err := rag.NewQdrantProvider(t.Context(), url, "", NewFixtureEmbedder(384))
	if err != nil {
		t.Fatalf("connect to the disposable Qdrant: %v", err)
	}
	// A fresh project id per test, so two runs never share a collection and a
	// leftover collection is traceable to the run that made it.
	project := uuid.NewString()
	t.Cleanup(func() {
		if err := p.DeleteCollection(context.Background(), project); err != nil {
			t.Logf("cleanup: delete collection for %s: %v", project, err)
		}
		p.Close()
	})
	return NewQdrantIndex(p), p, project
}

// realHarness is the applier over a real index, with the ledger and the
// deletion journal still stood in for -- those have their own integration
// tests, and mixing all three would make a failure ambiguous.
func realHarness(t *testing.T, idx Index) *harness {
	t.Helper()
	h := &harness{
		events:    fakeEvents{},
		deletions: &fakeDeletions{tombstoned: map[string]bool{}},
	}
	h.applier = New(h.events, h.deletions, idx, Config{})
	return h
}

func TestARealCollectionHoldsTheProjectedCheckpoint(t *testing.T) {
	idx, p, projectStr := qdrantIndex(t)
	project := uuid.MustParse(projectStr)
	h := realHarness(t, idx)

	r := record(t, "checkpoint.recorded", 100, checkpointPayload("verify the projection against a real store"))
	r.ProjectID = project
	if err := h.apply(t, r); err != nil {
		t.Fatal(err)
	}

	pts, err := p.ListPoints(t.Context(), projectStr, map[string]string{"doc_id": r.EventID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 {
		t.Fatalf("the checkpoint is in the collection %d times", len(pts))
	}
	if pts[0].Meta["kind"] != "checkpoint" || pts[0].Meta["event_seq"] != "100" {
		t.Fatalf("the stored point lost its metadata: %v", pts[0].Meta)
	}
	// The point says what embedded it, so a collection built from fixtures can
	// never be mistaken for one built by a real model.
	if pts[0].Meta["embed_model"] != FixtureModelName {
		t.Fatalf("the point does not record the fixture embedder: %q", pts[0].Meta["embed_model"])
	}
	if !strings.Contains(pts[0].Content, "verify the projection") {
		t.Fatalf("the stored content is not the checkpoint: %q", pts[0].Content)
	}
}

func TestApplyingTheSameEventTwiceLeavesOnePointInQdrant(t *testing.T) {
	// The at-least-once queue meeting a real store. The derived id is what
	// makes the second write an overwrite instead of a second point.
	idx, p, projectStr := qdrantIndex(t)
	project := uuid.MustParse(projectStr)
	h := realHarness(t, idx)

	r := record(t, "checkpoint.recorded", 101, checkpointPayload("delivered at least once"))
	r.ProjectID = project
	for i := 0; i < 3; i++ {
		if err := h.apply(t, r); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	n, err := p.CountPoints(t.Context(), projectStr)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("three applications of one event left %d points", n)
	}
}

func TestATombstoneRemovesThePointFromARealCollection(t *testing.T) {
	idx, p, projectStr := qdrantIndex(t)
	project := uuid.MustParse(projectStr)
	h := realHarness(t, idx)

	cp := record(t, "checkpoint.recorded", 102, checkpointPayload("about to be erased for real"))
	cp.ProjectID = project
	if err := h.apply(t, cp); err != nil {
		t.Fatal(err)
	}
	if n, _ := p.CountPoints(t.Context(), projectStr); n != 1 {
		t.Fatalf("setup: the collection holds %d points", n)
	}

	tomb := record(t, "tombstone.issued", 103, map[string]any{
		"subject_type": "checkpoint", "subject_id": cp.EventID.String(),
		"reason_class": "user_request", "erasure_required": true,
	})
	tomb.ProjectID = project
	if err := h.apply(t, tomb); err != nil {
		t.Fatal(err)
	}
	n, err := p.CountPoints(t.Context(), projectStr)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the erasure left %d points in the collection", n)
	}
}

func TestTheProjectedCheckpointIsRetrievable(t *testing.T) {
	// This asserts the retrieval path is connected end to end -- embed, store,
	// query, score, return -- by searching for text the document actually
	// contains. It is NOT a measurement of retrieval quality: with fixture
	// vectors, similarity is token overlap, and a paraphrase would not be
	// found. Nothing here should be quoted as an accuracy number.
	idx, p, projectStr := qdrantIndex(t)
	project := uuid.MustParse(projectStr)
	h := realHarness(t, idx)

	target := record(t, "checkpoint.recorded", 104, checkpointPayload("restore the writer epoch fencing rehearsal"))
	target.ProjectID = project
	other := record(t, "checkpoint.recorded", 105, checkpointPayload("index the graphify coordinator manifest"))
	other.ProjectID = project
	if err := h.apply(t, target); err != nil {
		t.Fatal(err)
	}
	if err := h.apply(t, other); err != nil {
		t.Fatal(err)
	}

	res, err := p.SemanticSearch(t.Context(), projectStr, "restore the writer epoch fencing rehearsal", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 {
		t.Fatal("the projected checkpoint could not be retrieved at all")
	}
	if res[0].ID != target.EventID.String() && res[0].Meta["doc_id"] != target.EventID.String() {
		t.Fatalf("the closest result is not the checkpoint that contains the query text: %+v", res[0])
	}
}

func TestWithheldContentNeverReachesTheRealStore(t *testing.T) {
	// The end of the sensitivity argument: not "the applier declines", but "the
	// bytes are not in the store". A collection that was never created is the
	// strongest form of that.
	idx, p, projectStr := qdrantIndex(t)
	project := uuid.MustParse(projectStr)
	h := realHarness(t, idx)

	r := record(t, "checkpoint.recorded", 106, checkpointPayload("this must not be embedded"))
	r.ProjectID = project
	r.Sensitivity = "restricted"
	if err := h.apply(t, r); err != nil {
		t.Fatal(err)
	}
	// The collection is never created, so counting it is expected to fail. If
	// it does exist, it must be empty.
	if n, err := p.CountPoints(t.Context(), projectStr); err == nil && n != 0 {
		t.Fatalf("restricted content reached the store: %d points", n)
	}
}

func TestTheQdrantIndexRefusesAnEmptyDeletionFilter(t *testing.T) {
	// A filter that matches everything would empty a project's collection, and
	// the caller that produced it meant something else.
	idx, _, projectStr := qdrantIndex(t)
	if _, err := idx.DeleteMatching(t.Context(), projectStr, nil); err == nil {
		t.Fatal("an empty deletion filter was accepted")
	}
}

var _ outbox.Applier = (*Applier)(nil)
