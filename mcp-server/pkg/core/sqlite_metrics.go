package core

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)
)

// SQLiteMetricsStore is a durable MetricsStore backed by a local SQLite file.
// It works with any vector-store backend (the metrics live independently of the
// vector store), keeping the "local-first, single static binary" goal intact —
// modernc.org/sqlite is pure Go, so no cgo is required.
type SQLiteMetricsStore struct {
	db           *sql.DB
	queryLogKeep int
}

// measurementVersion is the current latency measurement semantics, stamped on
// every row this build writes. Version 1 rows (and any row written before the
// column existed, which the ALTER backfills as 1) measured retrieval only —
// rerank, per-doc capping and compression were excluded. Version 2 measures
// the complete search end-to-end. Summary aggregates version 2 only so the two
// distributions are never averaged together; version 1 rows are retained for
// history but excluded from every aggregate the API reports.
const measurementVersion = 2

// NewSQLiteMetricsStore opens (creating if needed) a SQLite metrics database at
// path and ensures the schema exists. Callers should Close it on shutdown.
func NewSQLiteMetricsStore(path string) (*SQLiteMetricsStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open metrics db: %w", err)
	}
	// A single connection avoids "database is locked" under concurrent writes
	// from the async persist goroutines; SQLite serializes writers anyway.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS query_metrics (
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
		db.Close()
		return nil, fmt.Errorf("create metrics schema: %w", err)
	}

	// Existing databases (measurement version 1: retrieval-only latency) are
	// migrated in place: the column is added with DEFAULT 1 so pre-existing
	// rows keep their historical meaning, and new inserts stamp 2 explicitly.
	if _, err := db.Exec(`
ALTER TABLE query_metrics ADD COLUMN measurement_version INTEGER NOT NULL DEFAULT 1`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("migrate metrics schema: %w", err)
		}
	}

	// Separate table, not extra columns on query_metrics: the aggregate table is
	// read by every /api/metrics call and must stay narrow, and the query log has
	// to be droppable on its own without touching the metric history.
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS query_log (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	ts         INTEGER NOT NULL DEFAULT (unixepoch()),
	project_id TEXT    NOT NULL,
	query      TEXT    NOT NULL,
	results    INTEGER NOT NULL DEFAULT 0,
	top_score  REAL    NOT NULL DEFAULT 0,
	top_doc_id TEXT    NOT NULL DEFAULT '',
	reranked   INTEGER NOT NULL DEFAULT 0,
	latency_ms REAL    NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS query_log_ts ON query_log(ts DESC);`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create query log schema: %w", err)
	}

	return &SQLiteMetricsStore{db: db}, nil
}

// SetQueryLogKeep bounds the query log. Zero or negative means keep everything,
// which is not a mode this server should be run in for long.
func (s *SQLiteMetricsStore) SetQueryLogKeep(n int) { s.queryLogKeep = n }

// LogQuery appends one query and prunes the log back to the retention bound.
//
// Pruning happens on write rather than on a timer: a timer is one more thing
// that can quietly stop, and this file is on a VPS whose root filesystem was
// 72% full when the log was added.
func (s *SQLiteMetricsStore) LogQuery(ctx context.Context, e QueryLogEntry) error {
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO query_log (ts, project_id, query, results, top_score, top_doc_id, reranked, latency_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Ts.Unix(), e.ProjectID, e.Query, e.Results, e.TopScore, e.TopDocID,
		b2i(e.Reranked), e.LatencyMs); err != nil {
		return err
	}
	if s.queryLogKeep <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
DELETE FROM query_log WHERE id <= (
	SELECT MAX(id) FROM query_log
) - ?`, s.queryLogKeep)
	return err
}

// RecentQueries returns the newest entries first.
func (s *SQLiteMetricsStore) RecentQueries(ctx context.Context, limit int) ([]QueryLogEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT ts, project_id, query, results, top_score, top_doc_id, reranked, latency_ms
FROM query_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]QueryLogEntry, 0, limit)
	for rows.Next() {
		var (
			e   QueryLogEntry
			ts  int64
			rrk int
		)
		if err := rows.Scan(&ts, &e.ProjectID, &e.Query, &e.Results, &e.TopScore,
			&e.TopDocID, &rrk, &e.LatencyMs); err != nil {
			return nil, err
		}
		e.Ts = time.Unix(ts, 0).UTC()
		e.Reranked = rrk != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// Close releases the database handle.
func (s *SQLiteMetricsStore) Close() error { return s.db.Close() }

// PersistQueryMetric inserts one query's latency and composition.
func (s *SQLiteMetricsStore) PersistQueryMetric(ctx context.Context, latencyMs float64, comp QueryComposition) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO query_metrics
	(latency_ms, hybrid, reranked, candidates, results, dense_count, lexical_count, rerank_moved, measurement_version)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		latencyMs, b2i(comp.Hybrid), b2i(comp.Reranked), comp.Candidates,
		comp.Results, comp.DenseCount, comp.LexicalCount, comp.RerankMoved, measurementVersion)
	return err
}

// Summary computes durable aggregates over persisted queries measured with the
// current semantics (end-to-end latency). Version 1 rows measured retrieval
// only and are excluded: mixing the two distributions would report a p50/p95
// that describes neither. Percentiles use nearest-rank over the latency column.
func (s *SQLiteMetricsStore) Summary(ctx context.Context) (MetricsSummary, error) {
	var out MetricsSummary
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(AVG(latency_ms), 0) FROM query_metrics WHERE measurement_version = ?`,
		measurementVersion).
		Scan(&out.QueryCount, &out.AvgLatencyMs)
	if err != nil {
		return out, err
	}
	if out.QueryCount == 0 {
		return out, nil
	}
	out.P50LatencyMs = s.percentile(ctx, 0.50)
	out.P95LatencyMs = s.percentile(ctx, 0.95)
	return out, nil
}

// percentile returns the nearest-rank p-quantile (0..1) of latency_ms, or 0.
func (s *SQLiteMetricsStore) percentile(ctx context.Context, p float64) float64 {
	// nearest-rank: order ascending, pick row at ceil(p*N). OFFSET is 0-based.
	var v float64
	row := s.db.QueryRowContext(ctx, `
SELECT latency_ms FROM query_metrics
WHERE measurement_version = ?
ORDER BY latency_ms
LIMIT 1 OFFSET CAST(ROUND(? * (SELECT COUNT(*) - 1 FROM query_metrics WHERE measurement_version = ?)) AS INTEGER)`,
		measurementVersion, p, measurementVersion)
	if err := row.Scan(&v); err != nil {
		return 0
	}
	return v
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
