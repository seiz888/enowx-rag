package core

import (
	"context"
	"os"
	"strconv"
	"time"
)

// QueryLogEntry is one query as it was actually asked, with just enough of the
// outcome to tell a good answer from a bad one.
//
// Why the query text and not only aggregates: /api/metrics already reports
// counts, latency percentiles and token spend, and none of it can answer the
// only question that matters when tuning retrieval -- which real questions came
// back with nothing useful. Without this, every tuning decision rests on a
// hand-written eval set, and a hand-written set only contains failures its
// author already thought of. On 2026-09-09 that set was 32 questions and two of
// its three "failures" turned out to be mislabelled by the author, not missed by
// the retriever.
//
// What is deliberately NOT stored: result text. The top result's id, score and
// doc_id are enough to judge whether a query landed, and storing the chunks
// themselves would duplicate the corpus into a second file with different
// handling. TopScore is the reranked score of rank 1, or 0 when nothing matched.
type QueryLogEntry struct {
	Ts        time.Time `json:"ts"`
	ProjectID string    `json:"project_id"`
	Query     string    `json:"query"`
	Results   int       `json:"results"`
	TopScore  float64   `json:"top_score"`
	TopDocID  string    `json:"top_doc_id"`
	Reranked  bool      `json:"reranked"`
	LatencyMs float64   `json:"latency_ms"`
}

// QueryLogStore durably records queries and reads them back.
type QueryLogStore interface {
	LogQuery(ctx context.Context, e QueryLogEntry) error
	RecentQueries(ctx context.Context, limit int) ([]QueryLogEntry, error)
}

// DefaultQueryLogKeep is how many entries are retained when no cap is set.
// Bounded on purpose: an unbounded log on a small VPS is a disk-full incident
// waiting for a busy week, and old queries answer nothing that recent ones
// don't.
const DefaultQueryLogKeep = 5000

// QueryLogEnabled reports whether query logging was switched on, and how many
// entries to keep.
//
// Off by default, and it must stay that way: this is the one thing in the server
// that writes user-authored text to a second place on disk. Turning it on is a
// decision about what may be stored, so it belongs to whoever runs the server,
// not to a default.
func QueryLogEnabled() (bool, int) {
	v := os.Getenv("RAG_QUERY_LOG")
	if v != "1" && v != "true" && v != "yes" {
		return false, 0
	}
	keep := DefaultQueryLogKeep
	if n, err := strconv.Atoi(os.Getenv("RAG_QUERY_LOG_KEEP")); err == nil && n > 0 {
		keep = n
	}
	return true, keep
}

// SetQueryLog injects a durable query log. nil disables logging.
func (s *Service) SetQueryLog(store QueryLogStore) {
	s.queryLog = store
}

// RecentQueries reads back the query log. It returns nil when logging is off,
// which the handler reports as "disabled" rather than as an empty log -- those
// are different facts and confusing them would make a switched-off log look
// like a corpus nobody queries.
func (s *Service) RecentQueries(ctx context.Context, limit int) ([]QueryLogEntry, bool, error) {
	if s.queryLog == nil {
		return nil, false, nil
	}
	e, err := s.queryLog.RecentQueries(ctx, limit)
	return e, true, err
}
