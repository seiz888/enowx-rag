package main

import (
	"strings"
	"testing"

	"github.com/enowdev/enowx-rag/pkg/rag"
)

// TestChooseEmbedderVoyageNeedsAKey pins the refusal that keeps a misconfigured
// worker from silently building a collection with no semantic content: with no
// voyage key in the environment, selecting voyage is an error, not a fallback.
func TestChooseEmbedderVoyageNeedsAKey(t *testing.T) {
	t.Setenv("RAG_VOYAGE_API_KEY", "")
	if _, err := chooseEmbedder("voyage", "", "", 0); err == nil {
		t.Fatal("voyage without RAG_VOYAGE_API_KEY was accepted")
	}
}

// TestChooseEmbedderVoyageUsesTheApprovedClient checks the positive path: with
// a key present, voyage returns a real VoyageEmbeddingClient (the same client
// the RAG service uses), named by its model so the point payload records it.
func TestChooseEmbedderVoyageUsesTheApprovedClient(t *testing.T) {
	t.Setenv("RAG_VOYAGE_API_KEY", "pa-test-key-not-used")
	e, err := chooseEmbedder("voyage", "", "", 0)
	if err != nil {
		t.Fatalf("voyage with a key failed: %v", err)
	}
	v, ok := e.(*rag.VoyageEmbeddingClient)
	if !ok {
		t.Fatalf("voyage returned %T, want *rag.VoyageEmbeddingClient", e)
	}
	if got := v.ModelName(); got != "voyage-4" {
		t.Fatalf("default model is %q, want voyage-4", got)
	}
}

// TestChooseEmbedderFixtureIsExplicitAndTeiNeedsAURL guards the two other
// paths: fixture is still reachable by name (tests use it), and tei without a
// URL is refused rather than pointed somewhere default.
func TestChooseEmbedderFixtureIsExplicitAndTeiNeedsAURL(t *testing.T) {
	if _, err := chooseEmbedder("fixture", "", "", 0); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if _, err := chooseEmbedder("tei", "", "", 0); err == nil || !strings.Contains(err.Error(), "tei-url") {
		t.Fatalf("tei without a url: got %v, want an error naming --tei-url", err)
	}
	// No embedder at all must refuse, never default.
	if _, err := chooseEmbedder("", "", "", 0); err == nil {
		t.Fatal("an empty embedder was accepted; it must refuse and name the choices")
	}
}
