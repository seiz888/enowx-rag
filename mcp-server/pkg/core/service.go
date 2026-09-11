// Package core provides the service layer shared by MCP stdio and HTTP API.
// It wraps a rag.Provider, an optional rag.Reranker, and an *indexer.Indexer
// behind a single Service struct with methods that both transport layers call.
package core

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/enowdev/enowx-rag/pkg/indexer"
	"github.com/enowdev/enowx-rag/pkg/rag"
)

// DefaultK is the default number of final results returned by Search.
const DefaultK = 5

// DefaultRecall is the default number of candidates retrieved before rerank.
//
// 25, not 40: rerank is ~99.7% of this instance's token spend and its bill scales
// with this number alone (k only trims what was already scored). Measured on the
// `memory` corpus, 51 paraphrased questions with every acceptable answer labelled
// (~/.claude/scripts/rag_eval.py --set hard --sweep 12,25,40,60): hit@1 is 36/51 at
// 25, 40 and 60 alike, hit@3 peaks at 25 (47/51) and DROPS as recall grows -- extra
// candidates give the reranker more chances to promote a near-miss above the right
// document. So 40 was paying 60% more per query to answer no additional question.
//
// 12 is genuinely worse (43/51 hit@3), so this is a knee and not a "smaller is
// better" curve. Re-run that harness before moving this number again; it moved once
// already because the corpus was re-chunked underneath the old measurement, and the
// value it moved away from had been fitted to a 12-question set whose labels were
// too narrow to see this.
const DefaultRecall = 25

// SearchOpts controls the behaviour of Service.Search.
type SearchOpts struct {
	K        int  // final top-K results (default 5)
	Recall   int  // retrieval recall before rerank (default 40)
	Hybrid   bool // use dense+lexical RRF (provider must support it)
	Rerank   bool // use reranker if configured
	Compress bool // drop near-duplicate results (same content_hash / identical content)

	// NoLog keeps this query out of the query log. Metrics still count it.
	//
	// For synthetic traffic -- the eval harness in ~/.claude/scripts/rag_eval.py
	// fires 71 questions per run and a sweep multiplies that by four. Those
	// questions are already written down; logging them buries the handful of real
	// questions the log exists to surface. Within hours of switching the log on,
	// 280 of 280 entries were the harness's own, which makes the log useless for
	// exactly the purpose it was built for.
	//
	// Deliberately not the inverse (an opt-in "log this"): a caller that forgets
	// the flag should end up in the log, because a missing real query is the
	// failure that costs something and a missing synthetic one costs nothing.
	NoLog bool

	// MaxPerDoc caps how many chunks of the same document may appear in the
	// results. 0 (the default) means no cap, which is what the server has always
	// done and what a caller reading a long document still wants.
	//
	// Why it is worth having: `k` counts chunks, not documents, so sibling chunks
	// of one document can take most of the answer. Measured on the `memory`
	// corpus over the 71 eval questions at k=10 -- 710 slots returned covered 414
	// distinct documents, so 42% of the slots (and 40% of the returned text) were
	// repeat documents. A caller asking for 10 got fewer than 6 documents.
	//
	// Why it is NOT the default: those sibling chunks are often the rest of the
	// answer. Capping helps breadth per token, not ranking quality -- the
	// document-level hit@3 gain it shows in the harness is partly an artefact of
	// measuring by document. So the caller who wants breadth asks for it.
	//
	// When set, the reranker is asked to score every candidate rather than only
	// the top k, so a capped-out slot can be refilled from further down. That
	// costs nothing extra: rerank is billed per candidate, and k only trims what
	// was already scored.
	MaxPerDoc int
}

// ProjectStat holds per-project statistics returned by ListProjects.
type ProjectStat struct {
	ProjectID  string `json:"project_id"`
	ChunkCount int    `json:"chunk_count"`
}

// Event is a single SSE event published by the EventBus.
type Event struct {
	Type      string    `json:"type"` // e.g. "index_started", "query_executed"
	Timestamp time.Time `json:"timestamp"`
	Data      any       `json:"data,omitempty"`
}

// EventBus is a simple pub/sub mechanism for SSE events. Subscribers receive
// events on a buffered channel; the bus is safe for concurrent use.
type EventBus struct {
	mu          sync.Mutex
	subscribers map[chan Event]struct{}
}

// NewEventBus creates a new EventBus.
func NewEventBus() *EventBus {
	return &EventBus{
		subscribers: make(map[chan Event]struct{}),
	}
}

// Subscribe returns a buffered channel on which events are delivered.
// The caller must call Unsubscribe with the returned channel to stop
// receiving and allow the bus to clean up.
func (b *EventBus) Subscribe() chan Event {
	ch := make(chan Event, 64)
	b.mu.Lock()
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel and closes it.
func (b *EventBus) Unsubscribe(ch chan Event) {
	b.mu.Lock()
	delete(b.subscribers, ch)
	b.mu.Unlock()
	// Close the channel so any range-loop consumer exits.
	close(ch)
}

// Publish sends an event to all current subscribers. If a subscriber's
// channel buffer is full the event is dropped for that subscriber (non-blocking).
func (b *EventBus) Publish(ev Event) {
	b.mu.Lock()
	subs := make([]chan Event, 0, len(b.subscribers))
	for ch := range b.subscribers {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}
	for _, ch := range subs {
		select {
		case ch <- ev:
		default: // drop if buffer full
		}
	}
}

// Service wraps a provider, an optional reranker, and an indexer behind a
// single API used by both the MCP stdio handlers and the HTTP API layer.
type Service struct {
	provider     rag.Provider
	reranker     rag.Reranker // may be nil
	indexer      *indexer.Indexer
	events       *EventBus
	embedModel   string // optional: set by main.go for stats endpoint
	metrics      *Metrics
	metricsStore MetricsStore  // optional durable metrics; nil = in-memory only
	queryLog     QueryLogStore // optional durable query log; nil = not logging
	backend      string        // vector store name (e.g. "qdrant"), set by main.go
	writeGuard   *WriteGuard   // optional per-project write contract; nil = no checks
}

// MetricsStore is an optional durable sink for query metrics, injected into
// Service (not a provider method — that would create a rag→core import cycle).
// When set, Service persists each query and reads durable aggregates so the
// dashboard survives restarts; when nil, metrics are in-memory only and the
// snapshot reports Persistent=false. SQLiteMetricsStore is the default
// implementation and works with any vector-store backend.
type MetricsStore interface {
	// PersistQueryMetric durably records one query's latency and composition.
	PersistQueryMetric(ctx context.Context, latencyMs float64, comp QueryComposition) error
	// Summary returns durable aggregates across all persisted queries.
	Summary(ctx context.Context) (MetricsSummary, error)
}

// MetricsSummary holds durable metric aggregates read back from a MetricsStore.
type MetricsSummary struct {
	QueryCount   int64
	AvgLatencyMs float64
	P50LatencyMs float64
	P95LatencyMs float64
}

// MetricsSnapshot is the JSON-serializable metrics response returned by
// Service.MetricsSnapshot and the GET /api/metrics endpoint.
type MetricsSnapshot struct {
	QueryCount   int64             `json:"query_count"`
	AvgLatencyMs float64           `json:"avg_latency_ms"`
	P50LatencyMs float64           `json:"p50_latency_ms"`
	P95LatencyMs float64           `json:"p95_latency_ms"`
	TokensTotal  int64             `json:"tokens_total"`
	TokensEmbed  int64             `json:"tokens_embed"`
	TokensRerank int64             `json:"tokens_rerank"`
	Persistent   bool              `json:"persistent"` // true iff backend implements MetricsStore
	Backend      string            `json:"backend"`
	LastQuery    *QueryComposition `json:"last_query,omitempty"` // nil until first query
}

// NewService creates a Service from the given components. reranker may be nil.
// indexer may be nil; if so, IndexProject will create one with default chunk size.
func NewService(provider rag.Provider, reranker rag.Reranker, idx *indexer.Indexer) *Service {
	svc := &Service{
		provider: provider,
		reranker: reranker,
		indexer:  idx,
		events:   NewEventBus(),
		metrics:  NewMetrics(),
	}
	return svc
}

// Events returns the EventBus for SSE publishing/subscribing.
func (s *Service) Events() *EventBus {
	return s.events
}

// Provider returns the underlying rag.Provider. This is intended for
// advanced use cases where the caller needs direct provider access.
func (s *Service) Provider() rag.Provider {
	return s.provider
}

// SetEmbedModel sets the embedding model name used by the service. This is
// used by the HTTP API stats endpoint to report the active embedding model.
func (s *Service) SetEmbedModel(model string) {
	s.embedModel = model
}

// EmbedModel returns the embedding model name, or "unknown" if not set.
func (s *Service) EmbedModel() string {
	if s.embedModel != "" {
		return s.embedModel
	}
	return "unknown"
}

// SetWriteGuard installs the per-project write contract. A nil guard disables
// every check, which is the default.
func (s *Service) SetWriteGuard(g *WriteGuard) {
	s.writeGuard = g
}

// SetBackend records the active vector store name (e.g. "qdrant", "pgvector")
// so the metrics snapshot can report it honestly.
func (s *Service) SetBackend(name string) {
	s.backend = name
}

// SetMetricsStore injects a durable metrics store. When set, query metrics are
// persisted and the snapshot reports Persistent=true with durable aggregates.
func (s *Service) SetMetricsStore(store MetricsStore) {
	s.metricsStore = store
}

// MetricsSnapshot returns a point-in-time view of query metrics: in-memory
// latency aggregates and query count, plus token usage read from the provider
// and reranker when they implement rag.TokenCounter. Persistent reflects
// whether the backend can store metrics durably (implements MetricsStore).
func (s *Service) MetricsSnapshot(ctx context.Context) MetricsSnapshot {
	avg, p50, p95, count, comp, haveComp := s.metrics.snapshot()

	var embedTok, rerankTok int64
	if tc, ok := s.provider.(rag.TokenCounter); ok {
		embedTok = tc.TokensUsed()
	}
	if tc, ok := s.reranker.(rag.TokenCounter); ok {
		rerankTok = tc.TokensUsed()
	}
	// When a durable store is configured, prefer its aggregates so counts and
	// latencies survive restarts. Fall back to in-memory on read error.
	persistent := s.metricsStore != nil
	if persistent {
		if sum, err := s.metricsStore.Summary(ctx); err == nil {
			count = sum.QueryCount
			avg = sum.AvgLatencyMs
			p50 = sum.P50LatencyMs
			p95 = sum.P95LatencyMs
		}
	}

	snap := MetricsSnapshot{
		QueryCount:   count,
		AvgLatencyMs: avg,
		P50LatencyMs: p50,
		P95LatencyMs: p95,
		TokensEmbed:  embedTok,
		TokensRerank: rerankTok,
		TokensTotal:  embedTok + rerankTok,
		Persistent:   persistent,
		Backend:      s.backend,
	}
	if haveComp {
		c := comp
		snap.LastQuery = &c
	}
	return snap
}

// retrieveCandidates fetches recall candidates from the provider. When hybrid
// is true and the provider implements HybridSearcher, it uses the hybrid
// search path (dense + lexical RRF). Otherwise it falls back to dense-only
// SemanticSearch.
func (s *Service) retrieveCandidates(ctx context.Context, projectID, query string, recall int, hybrid bool) ([]rag.Result, error) {
	if hybrid {
		if hs, ok := s.provider.(rag.HybridSearcher); ok {
			return hs.SemanticSearchHybrid(ctx, projectID, query, recall)
		}
	}
	return s.provider.SemanticSearch(ctx, projectID, query, recall)
}

// Search performs a semantic search with optional reranking.
//
// Flow:
//  1. Retrieve `recall` candidates via provider.SemanticSearch.
//  2. If rerank is requested and a reranker is configured, rerank candidates
//     and return the top-K results with updated scores.
//  3. If the reranker fails, fall back to semantic order truncated to K.
//  4. If no rerank, truncate to K.
//
// Defaults: K=5, Recall=25. Both are capped at 100 to bound reranking cost.
func (s *Service) Search(ctx context.Context, projectID, query string, opts SearchOpts) ([]rag.Result, error) {
	k := opts.K
	if k <= 0 {
		k = DefaultK
	}
	recall := opts.Recall
	if recall <= 0 {
		recall = DefaultRecall
	}
	if recall > 100 {
		recall = 100
	}
	if k > 100 {
		k = 100
	}

	// Record the complete search, including reranking and fallback processing.
	t0 := time.Now()
	cands, err := s.retrieveCandidates(ctx, projectID, query, recall, opts.Hybrid)
	if err != nil {
		return nil, err
	}

	hybridUsed := opts.Hybrid && s.providerSupportsHybrid()
	comp := QueryComposition{
		Hybrid:     hybridUsed,
		Candidates: len(cands),
	}
	// Compute dense/lexical composition from per-result origin when the hybrid
	// path populated it (pgvector). Non-hybrid or backends that don't project
	// origin leave these at zero — an honest fallback.
	for _, c := range cands {
		if c.InDense {
			comp.DenseCount++
		}
		if c.InLexical {
			comp.LexicalCount++
		}
	}

	// Record metrics once, at return, regardless of which branch produced out.
	out := cands
	defer func() {
		latencyMs := float64(time.Since(t0).Microseconds()) / 1000.0
		comp.Results = len(out)
		s.metrics.RecordQuery(latencyMs, comp)
		if s.metricsStore != nil {
			// Persist asynchronously so query latency is unaffected. A copy of
			// comp is captured; failures are non-fatal.
			c := comp
			lat := latencyMs
			go func() { _ = s.metricsStore.PersistQueryMetric(context.WithoutCancel(context.Background()), lat, c) }()
		}
		if s.queryLog != nil && !opts.NoLog {
			// Same treatment as the metrics persist: asynchronous, and a failure
			// here must never turn a successful search into an error. A query
			// the caller already got results for is not going to be retracted
			// because a log line could not be written.
			e := QueryLogEntry{
				Ts:        time.Now(),
				ProjectID: projectID,
				Query:     query,
				Results:   len(out),
				Reranked:  comp.Reranked,
				LatencyMs: latencyMs,
			}
			if len(out) > 0 {
				e.TopScore = out[0].Score
				e.TopDocID = out[0].Meta["doc_id"]
			}
			go func() { _ = s.queryLog.LogQuery(context.WithoutCancel(context.Background()), e) }()
		}
	}()

	// Publish a query event for SSE listeners.
	s.events.Publish(Event{
		Type:      "query_executed",
		Timestamp: time.Now(),
		Data: map[string]any{
			"project_id": projectID,
			"query":      query,
			"candidates": len(cands),
		},
	})

	if opts.Rerank && s.reranker != nil && len(cands) > 0 {
		docs := make([]string, len(cands))
		for i, c := range cands {
			docs[i] = c.Content
		}
		// Score everything when a per-document cap is in play; see MaxPerDoc.
		topK := k
		if opts.MaxPerDoc > 0 {
			topK = len(cands)
		}
		hits, rerr := s.reranker.Rerank(ctx, query, docs, topK)
		// A silent fallback makes a transient reranker failure indistinguishable
		// from "no reranker configured". Log to stderr — stdout carries the MCP
		// protocol in stdio mode, so only stderr is safe here.
		switch {
		case rerr != nil:
			log.Printf("rerank: %d candidates failed, falling back to semantic order: %v", len(docs), rerr)
		case len(hits) == 0:
			log.Printf("rerank: %d candidates returned no hits, falling back to semantic order", len(docs))
		}
		if rerr == nil && len(hits) > 0 {
			reranked := make([]rag.Result, 0, len(hits))
			for _, h := range hits {
				if h.Index < 0 || h.Index >= len(cands) {
					continue
				}
				r := cands[h.Index]
				r.Score = h.Score
				reranked = append(reranked, r)
			}
			reranked = capPerDoc(reranked, opts.MaxPerDoc)
			// Defensive clamp: don't rely on the reranker API honoring top_k.
			if len(reranked) > k {
				reranked = reranked[:k]
			}
			comp.Reranked = true
			comp.RerankMoved = rerankMoved(cands, reranked)
			out = maybeCompress(reranked, opts.Compress)
			return out, nil
		}
		// Reranker failed or returned empty: fall back to semantic order.
	}

	cands = capPerDoc(cands, opts.MaxPerDoc)
	if len(cands) > k {
		cands = cands[:k]
	}
	out = maybeCompress(cands, opts.Compress)
	return out, nil
}

// providerSupportsHybrid reports whether the provider implements the hybrid
// search path, mirroring the type assertion in retrieveCandidates.
func (s *Service) providerSupportsHybrid() bool {
	_, ok := s.provider.(rag.HybridSearcher)
	return ok
}

// rerankMoved counts how many of the reranked results changed position relative
// to their original order in cands (by result ID).
func rerankMoved(cands, reranked []rag.Result) int {
	origPos := make(map[string]int, len(cands))
	for i, c := range cands {
		origPos[c.ID] = i
	}
	moved := 0
	for newPos, r := range reranked {
		if old, ok := origPos[r.ID]; ok && old != newPos {
			moved++
		}
	}
	return moved
}

// capPerDoc keeps at most max chunks per document, preserving score order.
//
// Grouping is by the `doc_id` meta with any `#N` chunk suffix removed, because
// that suffix is how this corpus names chunks of one document. A result with no
// doc_id is never grouped with another -- it is its own document as far as this
// function can tell, and silently collapsing unlabelled results would drop
// content for a reason the caller cannot see.
func capPerDoc(results []rag.Result, max int) []rag.Result {
	if max <= 0 || len(results) < 2 {
		return results
	}
	seen := make(map[string]int, len(results))
	out := results[:0:0] // new backing array; don't mutate the caller's slice
	for _, r := range results {
		doc := r.Meta["doc_id"]
		if i := strings.LastIndex(doc, "#"); i > 0 {
			doc = doc[:i]
		}
		if doc == "" {
			out = append(out, r)
			continue
		}
		if seen[doc] >= max {
			continue
		}
		seen[doc]++
		out = append(out, r)
	}
	return out
}

// maybeCompress removes near-duplicate results when compress is true, preserving
// input order (which is already score-sorted). Two results are considered
// duplicates when they share a non-empty content_hash or have identical content.
func maybeCompress(results []rag.Result, compress bool) []rag.Result {
	if !compress || len(results) < 2 {
		return results
	}
	seenHash := make(map[string]struct{}, len(results))
	seenContent := make(map[string]struct{}, len(results))
	out := results[:0:0] // new backing array; don't mutate caller's slice
	for _, r := range results {
		hash := r.Meta["content_hash"]
		if hash != "" {
			if _, dup := seenHash[hash]; dup {
				continue
			}
			seenHash[hash] = struct{}{}
		}
		if _, dup := seenContent[r.Content]; dup {
			continue
		}
		seenContent[r.Content] = struct{}{}
		out = append(out, r)
	}
	return out
}

// IndexProject scans the given directory and indexes all code/text files
// into the project collection. Delegates to the indexer.
func (s *Service) IndexProject(ctx context.Context, projectID, dir string) (*indexer.SyncResult, error) {
	// Checked before the collection is even touched: a guarded project must not
	// be reachable from the directory-scan path at all.
	if err := s.writeGuard.CheckProjectScan(projectID); err != nil {
		return nil, err
	}

	s.events.Publish(Event{
		Type:      "index_started",
		Timestamp: time.Now(),
		Data:      map[string]any{"project_id": projectID, "directory": dir},
	})

	// Ensure the collection exists before indexing. The MCP flow calls
	// CreateProject first, but the HTTP reindex endpoint indexes a project
	// directly, so a first-time index would otherwise fail with a missing
	// collection. CreateCollection is idempotent across providers.
	if err := s.provider.CreateCollection(ctx, projectID); err != nil {
		s.events.Publish(Event{
			Type:      "index_failed",
			Timestamp: time.Now(),
			Data:      map[string]any{"project_id": projectID, "error": err.Error()},
		})
		return nil, err
	}

	idx := s.indexer
	if idx == nil {
		idx = indexer.NewIndexer(s.provider, 1500)
	}

	result, err := idx.IndexProject(ctx, projectID, dir)
	if err != nil {
		s.events.Publish(Event{
			Type:      "index_failed",
			Timestamp: time.Now(),
			Data:      map[string]any{"project_id": projectID, "error": err.Error()},
		})
		return nil, err
	}

	s.events.Publish(Event{
		Type:      "index_completed",
		Timestamp: time.Now(),
		Data: map[string]any{
			"project_id":    projectID,
			"indexed":       result.Indexed,
			"deleted":       result.Deleted,
			"files_scanned": result.FilesScanned,
			"skipped":       result.Skipped,
		},
	})

	return result, nil
}

// ListProjects returns statistics for all known projects. It queries the
// provider's ListPoints for each project ID it can discover.
// Since the Provider interface doesn't have a "list collections" method,
// this method uses a best-effort approach by checking known project IDs
// via the provider. The caller may pass project IDs to check.
func (s *Service) ListProjects(ctx context.Context) ([]ProjectStat, error) {
	// The Provider interface does not expose a "list collections" method.
	// We rely on a helper that may be implemented by specific providers.
	// For now, we use a type assertion to check if the provider supports
	// listing project IDs.
	lister, ok := s.provider.(ProjectLister)
	if ok {
		ids, err := lister.ListProjectIDs(ctx)
		if err != nil {
			return nil, err
		}
		// Prefer an efficient count when the provider supports it. Otherwise
		// fall back to counting via ListPoints (which scrolls every point with
		// its payload — correct but slow for large projects).
		counter, hasCounter := s.provider.(ProjectCounter)
		stats := make([]ProjectStat, 0, len(ids))
		for _, id := range ids {
			var count int
			if hasCounter {
				c, err := counter.CountPoints(ctx, id)
				if err != nil {
					continue
				}
				count = c
			} else {
				points, err := s.provider.ListPoints(ctx, id, nil)
				if err != nil {
					continue
				}
				count = len(points)
			}
			stats = append(stats, ProjectStat{
				ProjectID:  id,
				ChunkCount: count,
			})
		}
		return stats, nil
	}
	return []ProjectStat{}, nil
}

// ProjectCounter is an optional interface for providers that can count points
// in a project cheaply (e.g. Qdrant points/count, pgvector COUNT(*)), avoiding
// a full ListPoints scroll just to size a project.
type ProjectCounter interface {
	CountPoints(ctx context.Context, projectID string) (int, error)
}

// ProjectLister is an optional interface that providers may implement to
// support listing all project IDs. This allows ListProjects to enumerate
// collections without prior knowledge.
type ProjectLister interface {
	ListProjectIDs(ctx context.Context) ([]string, error)
}

// CreateProject creates a new RAG collection for the given project.
func (s *Service) CreateProject(ctx context.Context, projectID string) error {
	if err := s.provider.CreateCollection(ctx, projectID); err != nil {
		return err
	}
	s.events.Publish(Event{
		Type:      "project_created",
		Timestamp: time.Now(),
		Data:      map[string]any{"project_id": projectID},
	})
	return nil
}

// DeleteProject deletes the project collection and all its indexed memory.
func (s *Service) DeleteProject(ctx context.Context, projectID string) error {
	if err := s.writeGuard.CheckDeleteProject(projectID); err != nil {
		return err
	}
	if err := s.provider.DeleteCollection(ctx, projectID); err != nil {
		return err
	}
	s.events.Publish(Event{
		Type:      "project_deleted",
		Timestamp: time.Now(),
		Data:      map[string]any{"project_id": projectID},
	})
	return nil
}

// ListPoints returns all points (ID + metadata) in the project collection,
// optionally filtered by metadata.
func (s *Service) ListPoints(ctx context.Context, projectID string, metaFilter map[string]string) ([]rag.PointInfo, error) {
	return s.provider.ListPoints(ctx, projectID, metaFilter)
}

// ListPointsPage bounds external pages; full scans remain available to indexing.
func (s *Service) ListPointsPage(ctx context.Context, projectID string, metaFilter map[string]string, offset, limit int) ([]rag.PointInfo, error) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	if p, ok := s.provider.(interface {
		ListPointsPage(context.Context, string, map[string]string, int, int) ([]rag.PointInfo, error)
	}); ok {
		return p.ListPointsPage(ctx, projectID, metaFilter, offset, limit)
	}
	points, err := s.provider.ListPoints(ctx, projectID, metaFilter)
	if err != nil {
		return nil, err
	}
	if offset >= len(points) {
		return []rag.PointInfo{}, nil
	}
	points = points[offset:]
	if len(points) > limit {
		points = points[:limit]
	}
	return points, nil
}

// ExportProject returns every point of a project with full content and metadata,
// for migration / re-embedding. Requires the provider to implement rag.Exporter
// (all built-in providers do); returns an error otherwise.
func (s *Service) ExportProject(ctx context.Context, projectID string) ([]rag.Document, error) {
	exporter, ok := s.provider.(rag.Exporter)
	if !ok {
		return nil, fmt.Errorf("this vector store does not support export")
	}
	return exporter.ExportPoints(ctx, projectID)
}

// ProjectExists checks whether a project with the given ID has any indexed
// data. It first tries the ProjectLister interface (if the provider supports
// it) for an O(1) set lookup; otherwise it falls back to ListPoints which
// works for all providers. Returns false if the project does not exist or
// an error occurs during the check.
func (s *Service) ProjectExists(ctx context.Context, projectID string) bool {
	// Fast path: if the provider can list project IDs, check membership.
	if lister, ok := s.provider.(ProjectLister); ok {
		ids, err := lister.ListProjectIDs(ctx)
		if err != nil {
			return false
		}
		for _, id := range ids {
			if id == projectID {
				return true
			}
		}
		return false
	}
	// Fallback: query ListPoints for the project. If it returns nil or an
	// error, the project doesn't exist.
	points, err := s.provider.ListPoints(ctx, projectID, nil)
	if err != nil {
		return false
	}
	return points != nil
}

// DeletePoints removes specific points by ID from the project collection.
func (s *Service) DeletePoints(ctx context.Context, projectID string, pointIDs []string) error {
	if err := s.writeGuard.CheckDeletePoints(projectID, len(pointIDs)); err != nil {
		return err
	}
	if err := s.provider.DeletePoints(ctx, projectID, pointIDs); err != nil {
		return err
	}
	s.events.Publish(Event{
		Type:      "points_deleted",
		Timestamp: time.Now(),
		Data:      map[string]any{"project_id": projectID, "count": len(pointIDs)},
	})
	return nil
}

// IndexDocuments indexes a batch of documents into the project collection
// directly (without scanning a directory). This is used by the rag_index MCP tool.
func (s *Service) IndexDocuments(ctx context.Context, projectID string, docs []rag.Document) error {
	// Checked before the provider call, so a rejected batch is never partially
	// embedded: Voyage bills per embed and a half-written batch leaves the caller
	// guessing which documents landed.
	if err := s.writeGuard.Check(projectID, docs); err != nil {
		return err
	}
	if err := s.provider.Index(ctx, projectID, docs); err != nil {
		return fmt.Errorf("index documents: %w", err)
	}
	s.events.Publish(Event{
		Type:      "documents_indexed",
		Timestamp: time.Now(),
		Data:      map[string]any{"project_id": projectID, "count": len(docs)},
	})
	return nil
}

// RetrieveContext performs a semantic search and returns a concatenated
// context string suitable for feeding into an LLM, along with the raw results.
func (s *Service) RetrieveContext(ctx context.Context, projectID, query string, opts SearchOpts) (string, []rag.Result, error) {
	if opts.K <= 0 {
		opts.K = DefaultK
	}
	results, err := s.Search(ctx, projectID, query, opts)
	if err != nil {
		return "", nil, err
	}
	parts := make([]string, 0, len(results))
	for _, r := range results {
		parts = append(parts, fmt.Sprintf("[score %.3f] %s", r.Score, r.Content))
	}
	return strings.Join(parts, "\n\n"), results, nil
}
