package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

// EnsureWork talks to the gateway's work-ensure route and returns the work id
// without ever inventing one locally. This test pins the wire contract: the
// request names project and workspace and nothing else, and the response's
// work_id is what comes back, not something the adapter guessed.
func TestEnsureWorkPostsAndReadsBack(t *testing.T) {
	want := uuid.New()
	project := uuid.New()
	workspace := uuid.New()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/memgw/v1/works/ensure" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("authorization header was %q", got)
		}
		var in map[string]any
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Fatal(err)
		}
		if in["project_id"] != project.String() || in["workspace_id"] != workspace.String() {
			t.Fatalf("request carried %v", in)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"work_id": want.String(), "project_id": project.String(),
			"workspace_id": workspace.String(), "title": "t", "state": "planned",
			"revision": 0, "created": true,
		})
	}))
	defer srv.Close()

	cfg := Config{
		ProjectID:   project,
		WorkspaceID: workspace,
		Gateway:     GatewayConfig{BaseURL: srv.URL},
	}
	got, err := EnsureWork(context.Background(), cfg, "test-token", 5*time.Second)
	if err != nil {
		t.Fatalf("EnsureWork: %v", err)
	}
	if got.WorkID != want {
		t.Fatalf("EnsureWork returned %s, want %s", got.WorkID, want)
	}
	if !got.Created {
		t.Fatal("the response said created but the client dropped it")
	}
}

// EnsureWork refuses to guess when the config names no project or workspace: a
// work id invented for a target the operator never mapped is the exact "filed
// under the wrong unit" failure the whole path exists to prevent.
func TestEnsureWorkRefusesUnresolvedTarget(t *testing.T) {
	cfg := Config{Gateway: GatewayConfig{BaseURL: "http://127.0.0.1:1"}}
	if _, err := EnsureWork(context.Background(), cfg, "tok", time.Second); err == nil {
		t.Fatal("EnsureWork accepted a config with no project or workspace")
	}
}
