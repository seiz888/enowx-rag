package core

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/enowdev/enowx-rag/pkg/rag"
)

// These tests pin the honest end-to-end metrics contract:
//
//   - recorded latency includes the reranker (and every other post-retrieval
//     step), so a slow reranker must visibly raise the recorded number;
//   - the durable store stamps every new row with the current measurement
//     version and aggregates only that version, so legacy retrieval-only rows
//     are retained but never averaged into the end-to-end distribution;
//   - Search caps k and recall at 100, bounding reranking cost no matter what
//     the caller asks for.

// slowReranker sleeps before scoring, simulating a remote rerank call.
type slowReranker struct {
	delay time.Duration
}

func (s *slowReranker) Rerank(ctx context.Context, query string, docs []string, topK int) ([]rag.RerankHit, error) {
	time.Sleep(s.delay)
	hits := make([]rag.RerankHit, 0, topK)
	for i := 0; i < topK && i < len(docs); i++ {
		hits = append(hits, rag.RerankHit{Index: i, Score: 1.0 - float64(i)*0.01})
	}
	return hits, nil
}

var _ rag.Reranker = (*slowReranker)(nil)

// TestSearchLatencyIncludesSlowReranker pins that the recorded latency covers
// the whole search, reranker included. A reranker that sleeps delay must move
// the recorded latency above delay; under the old retrieval-only measurement
// this test fails, which is exactly what makes it a regression test for the
// semantics change.
func TestSearchLatencyIncludesSlowReranker(t *testing.T) {
	const delay = 120 * time.Millisecond
	store, err := NewSQLiteMetricsStore(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatalf("NewSQLiteMetricsStore: %v", err)
	}
	defer store.Close()
	svc := NewService(&mockProvider{}, &slowReranker{delay: delay}, nil)
	svc.SetMetricsStore(store)

	if _, err := svc.Search(context.Background(), "proj", "q", SearchOpts{K: 3, Recall: 10, Rerank: true}); err != nil {
		t.Fatalf("Search: %v", err)
	}

	// The durable store is written asynchronously (by design, so query latency
	// is unaffected); wait for the row to land BEFORE reading the snapshot, which
	// prefers the durable aggregates over the in-memory ones.
	sum := pollSummary(t, store)

	snap := svc.MetricsSnapshot(context.Background())
	if snap.QueryCount != 1 {
		t.Fatalf("QueryCount = %d, want 1", snap.QueryCount)
	}
	// Rerank sleep alone is delay; the recorded end-to-end number must exceed it
	// (retrieval time is on top). Guard against slow CI machines by comparing
	// against delay, not delay+epsilon.
	if snap.AvgLatencyMs < float64(delay.Milliseconds()) {
		t.Errorf("AvgLatencyMs = %.1f, want >= %d ms: latency must include the slow reranker",
			snap.AvgLatencyMs, delay.Milliseconds())
	}
	if snap.P95LatencyMs < snap.AvgLatencyMs {
		t.Errorf("P95LatencyMs = %.1f, want >= avg %.1f for a single sample", snap.P95LatencyMs, snap.AvgLatencyMs)
	}
	// The persisted row must carry the same end-to-end number.
	if sum.QueryCount != 1 {
		t.Fatalf("persisted QueryCount = %d, want 1", sum.QueryCount)
	}
	if sum.AvgLatencyMs < float64(delay.Milliseconds()) {
		t.Errorf("persisted AvgLatencyMs = %.1f, want >= %d ms (slow reranker must be included)",
			sum.AvgLatencyMs, delay.Milliseconds())
	}
}

// TestSearchLatencyUnreranked_ExcludesNothing pins the complementary case:
// without a reranker the same end-to-end measurement applies, and a fast
// search stays well below the slow-reranker threshold.
func TestSearchLatencyUnreranked_ExcludesNothing(t *testing.T) {
	svc := NewService(&mockProvider{}, nil, nil)
	if _, err := svc.Search(context.Background(), "proj", "q", SearchOpts{K: 3, Recall: 10}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	snap := svc.MetricsSnapshot(context.Background())
	if snap.QueryCount != 1 {
		t.Fatalf("QueryCount = %d, want 1", snap.QueryCount)
	}
	// A mock retrieval is effectively instant; 100ms is two orders of magnitude
	// above it and still far below the 120ms slow-reranker test threshold.
	if snap.AvgLatencyMs > 100 {
		t.Errorf("AvgLatencyMs = %.1f, want < 100 ms for instant mock retrieval", snap.AvgLatencyMs)
	}
}

// TestSearchCaps_KAndRecallAt100 pins the cost bound: no matter how large k
// and recall are, the provider is asked for at most 100 candidates and the
// reranker scores at most 100 docs.
func TestSearchCaps_KAndRecallAt100(t *testing.T) {
	p := &mockProvider{}
	rr := &capRecordingReranker{}
	svc := NewService(p, rr, nil)

	if _, err := svc.Search(context.Background(), "proj", "q",
		SearchOpts{K: 5000, Recall: 9000, Rerank: true}); err != nil {
		t.Fatalf("Search: %v", err)
	}

	p.mu.Lock()
	gotRecall := p.searchLimit
	p.mu.Unlock()
	if gotRecall != 100 {
		t.Errorf("provider recall limit = %d, want capped at 100", gotRecall)
	}
	rr.mu.Lock()
	gotTopK := rr.lastTopK
	rr.mu.Unlock()
	// k=5000 clamps to 100, so the reranker is asked for exactly 100, never the
	// caller's 5000 — that is the cost bound this test pins. (It scores at most
	// the 100 capped candidates in practice; MaxPerDoc is what asks for all.)
	if gotTopK != 100 {
		t.Errorf("reranker topK = %d, want 100 (k clamped from 5000)", gotTopK)
	}
}

// capRecordingReranker records the topK it was called with.
type capRecordingReranker struct {
	mu       sync.Mutex
	lastTopK int
}

func (c *capRecordingReranker) Rerank(ctx context.Context, query string, docs []string, topK int) ([]rag.RerankHit, error) {
	c.mu.Lock()
	c.lastTopK = topK
	c.mu.Unlock()
	hits := make([]rag.RerankHit, 0, topK)
	for i := 0; i < topK && i < len(docs); i++ {
		hits = append(hits, rag.RerankHit{Index: i, Score: 1.0})
	}
	return hits, nil
}

var _ rag.Reranker = (*capRecordingReranker)(nil)

// TestSearchCaps_RecallOverCapStillReturnsResults pins that a recall over the
// cap still returns results (clamped, not rejected).
func TestSearchCaps_RecallOverCapStillReturnsResults(t *testing.T) {
	svc := NewService(&mockProvider{}, nil, nil)
	res, err := svc.Search(context.Background(), "proj", "q", SearchOpts{K: 200, Recall: 200})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) == 0 {
		t.Error("oversized k/recall must clamp, not return zero results")
	}
}

// --- Legacy schema migration regression ---

// TestSQLiteMetrics_LegacyRowsExcludedFromSummary pins the versioned cutover:
// a database written by an older build (retrieval-only latency, no
// measurement_version column) is migrated in place; its rows survive verbatim
// but are excluded from Summary aggregates, while new rows are included.
func TestSQLiteMetrics_LegacyRowsExcludedFromSummary(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "metrics.db")

	// 1. Create a legacy database exactly as an older build left it: the
	// original schema, no measurement_version column, one retrieval-only row.
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE query_metrics (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	ts            INTEGER NOT NULL DEFAULT (unixepoch()),
	latency_ms    REAL    NOT NULL,
	hybrid        INTEGER NOT NULL DEFAULT 0,
	reranked      INTEGER NOT NULL DEFAULT 0,
	candidates    INTEGER NOT NULL DEFAULT 0,
	results       INTEGER NOT NULL DEFAULT 0,
	dense_count   INTEGER NOT NULL DEFAULT 0,
	lexical_count INTEGER NOT NULL DEFAULT 0,
	rerank_moved  INTEGER NOT NULL DEFAULT 0
);`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := legacy.Exec(
		`INSERT INTO query_metrics (latency_ms, hybrid, reranked, candidates, results) VALUES (1.5, 0, 0, 40, 5)`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	// 2. Open with the current build: the ALTER must run and backfill the
	// legacy row as version 1.
	store, err := NewSQLiteMetricsStore(path)
	if err != nil {
		t.Fatalf("open store over legacy db: %v", err)
	}
	defer store.Close()

	// 3. Persist one end-to-end row.
	if err := store.PersistQueryMetric(ctx, 30.0, QueryComposition{Candidates: 25, Results: 5}); err != nil {
		t.Fatalf("PersistQueryMetric: %v", err)
	}

	// 4. Summary aggregates ONLY the new row.
	sum, err := store.Summary(ctx)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.QueryCount != 1 {
		t.Errorf("QueryCount = %d, want 1 (legacy row must be excluded)", sum.QueryCount)
	}
	if sum.AvgLatencyMs != 30.0 {
		t.Errorf("AvgLatencyMs = %v, want 30 (only the version-2 row)", sum.AvgLatencyMs)
	}
	if sum.P50LatencyMs != 30.0 || sum.P95LatencyMs != 30.0 {
		t.Errorf("p50/p95 = %v/%v, want 30/30 (only the version-2 row)", sum.P50LatencyMs, sum.P95LatencyMs)
	}

	// 5. The legacy row is still there, untouched — retained for history.
	var total int
	var legacyLatency float64
	var legacyVersion int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MAX(CASE WHEN measurement_version = 1 THEN latency_ms END), MIN(measurement_version) FROM query_metrics`).
		Scan(&total, &legacyLatency, &legacyVersion); err != nil {
		t.Fatalf("count all rows: %v", err)
	}
	if total != 2 {
		t.Errorf("total rows = %d, want 2 (legacy row must be retained, not deleted)", total)
	}
	if legacyLatency != 1.5 {
		t.Errorf("legacy row latency = %v, want 1.5 (must be unchanged)", legacyLatency)
	}
	if legacyVersion != 1 {
		t.Errorf("MIN(measurement_version) = %d, want 1 (legacy row backfilled as version 1)", legacyVersion)
	}
}

// TestSQLiteMetrics_ReopenIsIdempotent pins that opening a store that already
// carries the measurement_version column (second boot after migration) does
// not fail on the duplicate-column ALTER and keeps aggregating correctly.
func TestSQLiteMetrics_ReopenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")

	store, err := NewSQLiteMetricsStore(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := store.PersistQueryMetric(context.Background(), 10.0, QueryComposition{}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	store2, err := NewSQLiteMetricsStore(path)
	if err != nil {
		t.Fatalf("second open (duplicate column must be tolerated): %v", err)
	}
	defer store2.Close()

	sum, err := store2.Summary(context.Background())
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.QueryCount != 1 || sum.AvgLatencyMs != 10.0 {
		t.Errorf("after reopen: count=%d avg=%v, want 1 / 10", sum.QueryCount, sum.AvgLatencyMs)
	}
}

// pollSummary waits for the async persist goroutine to land in the store and
// returns its Summary.
func pollSummary(t *testing.T, store *SQLiteMetricsStore) MetricsSummary {
	t.Helper()
	for i := 0; i < 100; i++ {
		if sum, err := store.Summary(context.Background()); err == nil && sum.QueryCount >= 1 {
			return sum
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("metric was not persisted within timeout")
	return MetricsSummary{}
}
