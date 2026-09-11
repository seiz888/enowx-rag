package rag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// TestDeletePointsUsesTheSameIDMappingAsUpsert guards the asymmetry that made a
// deletion unable to reach anything: Index stores a document under
// pointID(d.ID), so a delete that sent the raw document id named a point that
// was never written. Qdrant rejects a non-UUID id outright, which at least
// fails loudly; an id that happened to parse as a UUID would have deleted
// nothing and reported success.
func TestDeletePointsUsesTheSameIDMappingAsUpsert(t *testing.T) {
	var got []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if pts, ok := body["points"].([]any); ok {
			got = pts
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{}, "status": "ok"})
	}))
	defer srv.Close()

	p, err := NewQdrantProvider(context.Background(), srv.URL, "", &mockQueryEmbedder{})
	if err != nil {
		t.Fatalf("NewQdrantProvider: %v", err)
	}
	defer p.Close()

	already := uuid.NewString()
	raw := "docs/plans/shared-memory-gateway.md#chunk3"
	if err := p.DeletePoints(context.Background(), "proj", []string{raw, already}); err != nil {
		t.Fatalf("DeletePoints: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("the request carried %d ids, want 2", len(got))
	}
	if want := pointID(raw); got[0] != want {
		t.Fatalf("a human-readable id was sent as %v, want the upsert's %s", got[0], want)
	}
	// A caller that already holds a Qdrant id -- which is what listing points
	// hands back -- must keep working unchanged.
	if got[1] != already {
		t.Fatalf("an existing point id was rewritten to %v", got[1])
	}
}
