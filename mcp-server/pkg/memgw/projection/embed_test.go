package projection

import (
	"context"
	"math"
	"testing"
)

func TestTheFixtureEmbedderIsDeterministic(t *testing.T) {
	// The one property the projection actually requires of an embedder in
	// non-production verification. Without it, re-applying an event would write
	// a second, slightly different point and every idempotence test in this
	// package would be measuring luck.
	e := NewFixtureEmbedder(384)
	text := "Objective: finish the projection applier"
	a, err := e.Embed(context.Background(), []string{text})
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Embed(context.Background(), []string{text})
	if err != nil {
		t.Fatal(err)
	}
	if len(a[0]) != 384 {
		t.Fatalf("the vector is %d wide, not the requested 384", len(a[0]))
	}
	for i := range a[0] {
		if a[0][i] != b[0][i] {
			t.Fatalf("the same text embedded differently at coordinate %d", i)
		}
	}
}

func TestTheFixtureVectorsAreUnitLength(t *testing.T) {
	// Cosine distance on a non-normalised vector is still defined, but a store
	// configured for cosine and fed vectors of wildly different magnitudes
	// makes score thresholds meaningless, and the migration work later reads
	// scores.
	e := NewFixtureEmbedder(64)
	texts := []string{"a checkpoint about the collector", "", "1", "the gateway listens on 7777"}
	vecs, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range vecs {
		var norm float64
		for _, x := range v {
			norm += float64(x) * float64(x)
		}
		if math.Abs(math.Sqrt(norm)-1) > 1e-5 {
			t.Fatalf("vector %d has length %f", i, math.Sqrt(norm))
		}
	}
}

func TestDifferentTextsGetDifferentVectors(t *testing.T) {
	// A hash that collapsed everything to one direction would make every test
	// that writes two documents pass for the wrong reason.
	e := NewFixtureEmbedder(384)
	vecs, err := e.Embed(context.Background(), []string{
		"the collector spools events to disk",
		"the gateway authenticates a principal",
	})
	if err != nil {
		t.Fatal(err)
	}
	var dot float64
	for i := range vecs[0] {
		dot += float64(vecs[0][i]) * float64(vecs[1][i])
	}
	if dot > 0.9 {
		t.Fatalf("two unrelated texts are nearly identical vectors (cosine %f)", dot)
	}
}

func TestTheFixtureNamesItself(t *testing.T) {
	// So a collection built from fixture vectors can be identified by looking
	// at its points rather than by remembering how it was built.
	if NewFixtureEmbedder(8).ModelName() != FixtureModelName {
		t.Fatal("the fixture embedder does not report its own name")
	}
}
