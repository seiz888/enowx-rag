package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/enowdev/enowx-rag/pkg/rag"
)

// mockExporter arms mockProvider with a full-content ExportPoints.
type mockExporter struct {
	mockProvider
	exportDocs  []rag.Document
	exportCalls int
	exportErr   error
	mu          sync.Mutex
}

func (m *mockExporter) ExportPoints(ctx context.Context, projectID string) ([]rag.Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.exportCalls++
	if m.exportErr != nil {
		return nil, m.exportErr
	}
	return m.exportDocs, nil
}

// TestExportProject_FullContentNotTruncated: the export endpoint must return
// the provider's documents with FULL content — not the 200-byte preview the
// points listing hands out. Backups and the secrets audit read every byte;
// a truncation here turns "clean" into a verdict about the first 200 bytes.
func TestExportProject_FullContentNotTruncated(t *testing.T) {
	long := strings.Repeat("x", 5000)
	p := &mockExporter{exportDocs: []rag.Document{
		{ID: "memory/log/doc#1", Content: long, Meta: map[string]string{
			"title": "doc", "bucket": "misc", "kind": "log", "chunk": "1/2", "content_hash": "h1",
		}},
		{ID: "memory/log/doc#2", Content: "second chunk", Meta: map[string]string{"chunk": "2/2"}},
	}}
	_, router := newTestServer(t, p, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/projects/memory/export", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var docs []rag.Document
	if err := json.Unmarshal(w.Body.Bytes(), &docs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 documents, got %d", len(docs))
	}
	if len(docs[0].Content) != 5000 {
		t.Errorf("content truncated: got %d chars, want 5000 (export must never truncate)", len(docs[0].Content))
	}
	if docs[0].Meta["title"] != "doc" || docs[0].ID != "memory/log/doc#1" {
		t.Errorf("identity/metadata not preserved: %+v", docs[0])
	}
	if p.exportCalls != 1 {
		t.Errorf("expected exactly 1 ExportPoints call, got %d", p.exportCalls)
	}
}

// TestExportProject_EmptyIsArrayNotNull: an empty project must serialize as
// [] so JSON consumers (python scripts) iterate it without a None check.
func TestExportProject_EmptyIsArrayNotNull(t *testing.T) {
	p := &mockExporter{}
	_, router := newTestServer(t, p, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/projects/empty/export", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if body := strings.TrimSpace(w.Body.String()); body != "[]" {
		t.Errorf("empty export must be [], got: %s", body)
	}
}

// TestExportProject_RequiresAdminToken: the export returns the whole corpus
// untruncated, so it must sit behind the same admin-token gate as /points —
// with a token configured, a bare request is 401 and a valid bearer passes.
func TestExportProject_RequiresAdminToken(t *testing.T) {
	p := &mockExporter{exportDocs: []rag.Document{{ID: "d1", Content: "c"}}}
	_, router := newTestServer(t, p, nil)
	t.Setenv("RAG_ADMIN_TOKEN", "secret-token")

	req := httptest.NewRequest(http.MethodGet, "/api/projects/memory/export", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("without bearer: expected 401, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/projects/memory/export", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("with valid bearer: expected 200, got %d", w.Code)
	}
	var docs []rag.Document
	if err := json.Unmarshal(w.Body.Bytes(), &docs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(docs) != 1 || docs[0].ID != "d1" {
		t.Errorf("unexpected export body: %+v", docs)
	}
}
