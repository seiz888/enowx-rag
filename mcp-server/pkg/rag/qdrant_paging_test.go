package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// scrollStore is a fake Qdrant scroll endpoint holding a fixed set of points.
// It serves /points/scroll pages of scrollLimit points and records the limit
// of every request so tests can assert the provider never over-fetches.
type scrollStore struct {
	total       int   // points in the store
	scrollLimit int   // page size the fake server enforces
	reqLimits   []int // limits requested by the provider, in order
	reqCount    int   // number of scroll requests made
	payload     func(i int) map[string]any
}

func newScrollServer(t *testing.T, st *scrollStore) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/collections/project_p/points/scroll" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Limit  int `json:"limit"`
			Offset any `json:"offset"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		st.reqCount++
		st.reqLimits = append(st.reqLimits, body.Limit)

		// Determine the logical start position from the cursor. The fake
		// cursor is the string "<index of last served point>".
		start := 0
		if body.Offset != nil {
			s, _ := body.Offset.(string)
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 || n >= st.total {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			start = n + 1
		}
		// Serve at most the requested limit and the fake page size.
		page := body.Limit
		if page <= 0 || page > st.scrollLimit {
			page = st.scrollLimit
		}
		end := start + page
		if end > st.total {
			end = st.total
		}
		points := []map[string]any{}
		for i := start; i < end; i++ {
			payload := map[string]any{
				"content": fmt.Sprintf("content-%d", i),
				"doc_id":  fmt.Sprintf("doc-%d", i),
				"bucket":  "log",
			}
			if st.payload != nil {
				for k, v := range st.payload(i) {
					payload[k] = v
				}
			}
			points = append(points, map[string]any{
				"id":      fmt.Sprintf("uuid-%04d", i),
				"payload": payload,
			})
		}
		resp := map[string]any{"result": map[string]any{"points": points}}
		if end < st.total {
			resp["result"].(map[string]any)["next_page_offset"] = fmt.Sprintf("%d", end-1)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newScrollProvider(t *testing.T, srv *httptest.Server) *QdrantProvider {
	t.Helper()
	p, err := NewQdrantProvider(context.Background(), srv.URL, "", &mockQueryEmbedder{})
	if err != nil {
		t.Fatalf("NewQdrantProvider: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func idsOf(pts []PointInfo) []string {
	out := make([]string, len(pts))
	for i, pt := range pts {
		out[i] = pt.ID
	}
	return out
}

// TestQdrantListPointsPageSlicesWithoutGaps: paging across the whole store by
// offset/limit must return every point exactly once — no missing, no
// duplicates — and stop issuing scroll requests once the page is complete.
func TestQdrantListPointsPageSlicesWithoutGaps(t *testing.T) {
	st := &scrollStore{total: 25, scrollLimit: 4}
	srv := newScrollServer(t, st)
	p := newScrollProvider(t, srv)

	var got []string
	for off := 0; ; off += 10 {
		pts, err := p.ListPointsPage(context.Background(), "p", nil, off, 10)
		if err != nil {
			t.Fatalf("ListPointsPage(offset=%d): %v", off, err)
		}
		for _, pt := range pts {
			got = append(got, pt.ID)
		}
		if len(pts) < 10 {
			break
		}
	}
	if len(got) != st.total {
		t.Fatalf("page walk: got %d points, want %d (missing/duplicated chunks)", len(got), st.total)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Errorf("duplicate point %s in page walk", id)
		}
		seen[id] = true
	}
	if len(seen) != st.total {
		t.Errorf("page walk covered %d unique points, want %d", len(seen), st.total)
	}
}

// TestQdrantListPointsPageBoundaries: offset at/after the end returns an empty
// page (not an error, not a wrap-around); offset+limit crossing the end returns
// only the remaining points.
func TestQdrantListPointsPageBoundaries(t *testing.T) {
	st := &scrollStore{total: 10, scrollLimit: 4}
	srv := newScrollServer(t, st)
	p := newScrollProvider(t, srv)

	pts, err := p.ListPointsPage(context.Background(), "p", nil, 10, 10)
	if err != nil {
		t.Fatalf("offset == total: %v", err)
	}
	if len(pts) != 0 {
		t.Errorf("offset == total: got %d points, want 0", len(pts))
	}

	pts, err = p.ListPointsPage(context.Background(), "p", nil, 99, 10)
	if err != nil {
		t.Fatalf("offset > total: %v", err)
	}
	if len(pts) != 0 {
		t.Errorf("offset > total: got %d points, want 0", len(pts))
	}

	// offset 6 of 10, limit 4 asks for positions 6..9 — all remaining.
	pts, err = p.ListPointsPage(context.Background(), "p", nil, 6, 4)
	if err != nil {
		t.Fatalf("offset near end: %v", err)
	}
	got := idsOf(pts)
	want := []string{"uuid-0006", "uuid-0007", "uuid-0008", "uuid-0009"}
	if len(got) != 4 || got[0] != want[0] || got[3] != want[3] {
		t.Errorf("offset near end: got %v, want %v..%v", got, want[0], want[3])
	}

	// offset 6 of 10, limit 10 crosses the end: only 4 remain.
	pts, err = p.ListPointsPage(context.Background(), "p", nil, 6, 10)
	if err != nil {
		t.Fatalf("offset+limit over end: %v", err)
	}
	if len(pts) != 4 {
		t.Errorf("offset+limit over end: got %d points, want 4", len(pts))
	}
}

// TestQdrantListPointsPageBoundedRequests: the provider must never ask Qdrant
// for more points than the page needs (offset + limit, clamped to a sane
// scroll batch), and must stop scrolling as soon as the page is complete.
func TestQdrantListPointsPageBoundedRequests(t *testing.T) {
	st := &scrollStore{total: 40, scrollLimit: 256}
	srv := newScrollServer(t, st)
	p := newScrollProvider(t, srv)

	if _, err := p.ListPointsPage(context.Background(), "p", nil, 5, 10); err != nil {
		t.Fatalf("ListPointsPage: %v", err)
	}
	// offset 5 + limit 10 = 15 needed; one bounded scroll request must cover it.
	if st.reqCount != 1 {
		t.Errorf("expected 1 scroll request for a 15-point page, got %d", st.reqCount)
	}
	if len(st.reqLimits) != 1 || st.reqLimits[0] > 15 {
		t.Errorf("scroll request asked for %v points; must not exceed the 15 the page needs", st.reqLimits)
	}
}

// TestQdrantListPointsPageStopsAtPage: once the requested number of retained
// points is reached, no further scroll requests are issued (continuation is
// the caller's job via offset).
func TestQdrantListPointsPageStopsAtPage(t *testing.T) {
	st := &scrollStore{total: 30, scrollLimit: 2}
	srv := newScrollServer(t, st)
	p := newScrollProvider(t, srv)

	pts, err := p.ListPointsPage(context.Background(), "p", nil, 0, 5)
	if err != nil {
		t.Fatalf("ListPointsPage: %v", err)
	}
	if len(pts) != 5 {
		t.Fatalf("got %d points, want 5", len(pts))
	}
	// 5 points at fake page size 2 = 3 scroll requests (2+2+1), not 15.
	if st.reqCount > 3 {
		t.Errorf("issued %d scroll requests for a 5-point page; scrolling did not stop at the page boundary", st.reqCount)
	}
}

// TestQdrantListPointsPagePreservesFilter: the filter must be forwarded on
// every scroll request, including continuation requests past the first batch.
func TestQdrantListPointsPagePreservesFilter(t *testing.T) {
	st := &scrollStore{total: 20, scrollLimit: 4}
	var filters []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/collections/project_p/points/scroll" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		filters = append(filters, body)
		start := 0
		if off, ok := body["offset"].(string); ok {
			n, _ := strconv.Atoi(off)
			start = n + 1
		}
		points := []map[string]any{}
		for i := start; i < start+4 && i < st.total; i++ {
			points = append(points, map[string]any{
				"id":      fmt.Sprintf("uuid-%04d", i),
				"payload": map[string]any{"content": fmt.Sprintf("content-%d", i)},
			})
		}
		resp := map[string]any{"result": map[string]any{"points": points}}
		if start+4 < st.total {
			resp["result"].(map[string]any)["next_page_offset"] = fmt.Sprintf("%d", start+3)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	p := newScrollProvider(t, srv)

	if _, err := p.ListPointsPage(context.Background(), "p", map[string]string{"bucket": "travelya"}, 0, 0); err != nil {
		t.Fatalf("ListPointsPage full scan with filter: %v", err)
	}
	if len(filters) == 0 {
		t.Fatal("no scroll requests captured")
	}
	for i, f := range filters {
		if _, present := f["filter"]; !present {
			t.Errorf("scroll request %d missing filter", i)
		}
	}
}

// TestQdrantListPointsPageFullScanUnbounded: a zero limit (internal
// bookkeeping path) must still return every point exactly once.
func TestQdrantListPointsPageFullScanUnbounded(t *testing.T) {
	st := &scrollStore{total: 12, scrollLimit: 5}
	srv := newScrollServer(t, st)
	p := newScrollProvider(t, srv)

	pts, err := p.ListPointsPage(context.Background(), "p", nil, 0, 0)
	if err != nil {
		t.Fatalf("ListPointsPage full scan: %v", err)
	}
	if len(pts) != st.total {
		t.Fatalf("full scan got %d points, want %d", len(pts), st.total)
	}
	seen := map[string]bool{}
	for _, pt := range pts {
		if seen[pt.ID] {
			t.Errorf("duplicate point %s in full scan", pt.ID)
		}
		seen[pt.ID] = true
	}
}
