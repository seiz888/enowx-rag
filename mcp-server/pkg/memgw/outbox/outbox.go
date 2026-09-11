// Package outbox is the projection queue and the deletion journal.
//
// Nothing here projects anything. Phase 3 builds the primitives -- claiming with
// a lease, retrying with backoff, dead-lettering, watermarks, tombstones and the
// legacy chunk mapping -- and deliberately does not connect them to Qdrant or
// Graphify. An Applier is an interface so the crash scenarios can be tested
// without a projection existing.
//
// The rule a projection worker may never break: it reads canonical state and
// writes projections. It never writes an event, a receipt or a fact.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
)

// Projection names a derived view.
type Projection string

const (
	ProjectionQdrant   Projection = "qdrant"
	ProjectionGraphify Projection = "graphify"
)

// State is the lifecycle of one outbox row.
type State string

const (
	StatePending    State = "pending"
	StateInFlight   State = "in_flight"
	StateApplied    State = "applied"
	StateFailed     State = "failed"
	StateDeadLetter State = "dead_letter"
)

// DefaultMaxAttempts is when a row stops being retried and becomes visible as a
// dead letter. It is small on purpose: a row that failed this many times needs
// a human, and retrying it forever hides that.
const DefaultMaxAttempts = 5

// Item is one claimed unit of projection work.
type Item struct {
	OutboxID   int64
	EventID    uuid.UUID
	EventSeq   int64
	Projection Projection
	Attempts   int
}

// Store is the repository over the outbox, the watermark, the tombstone journal
// and the legacy chunk map.
type Store struct {
	pool        *pgstore.Pool
	maxAttempts int
}

// NewStore returns a store over the given pool.
func NewStore(pool *pgstore.Pool) *Store {
	return &Store{pool: pool, maxAttempts: DefaultMaxAttempts}
}

// WithMaxAttempts returns a copy with a different dead-letter threshold.
func (s *Store) WithMaxAttempts(n int) *Store {
	return &Store{pool: s.pool, maxAttempts: n}
}

// Claim leases up to limit rows for one worker.
//
// SKIP LOCKED is what makes two workers safe against each other: the second one
// walks past rows the first is already holding instead of blocking on them. The
// lease is what makes a worker crash safe -- an in-flight row whose lease
// expires becomes claimable again, so a crash costs a retry, never a lost row.
func (s *Store) Claim(ctx context.Context, p Projection, owner string, lease time.Duration, limit int) ([]Item, error) {
	if owner == "" {
		return nil, errors.New("memgw: a claim must name its owner")
	}
	if limit <= 0 {
		limit = 1
	}
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()

	rows, err := s.pool.Pgx().Query(ctx, `
		WITH claimable AS (
			SELECT outbox_id FROM projection_outbox
			WHERE projection = $1
			  AND state IN ('pending','failed')
			  AND not_before <= now()
			ORDER BY outbox_id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE projection_outbox o
		SET state = 'in_flight',
		    lease_owner = $3,
		    lease_expires_at = now() + $4::interval,
		    attempts = o.attempts + 1
		FROM claimable c
		WHERE o.outbox_id = c.outbox_id
		RETURNING o.outbox_id, o.event_id, o.event_seq, o.projection, o.attempts`,
		p, limit, owner, lease.String())
	if err != nil {
		return nil, fmt.Errorf("memgw: claim outbox rows: %w", err)
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.OutboxID, &it.EventID, &it.EventSeq, &it.Projection, &it.Attempts); err != nil {
			return nil, fmt.Errorf("memgw: claim outbox rows: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// MarkApplied records a successful projection and advances the watermark.
//
// Both happen in one transaction, and the watermark only ever moves forward:
// rows are claimed in order but can finish out of order, and a watermark that
// went backwards would tell a reader the projection is less complete than it is.
func (s *Store) MarkApplied(ctx context.Context, it Item) error {
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()
	tx, err := s.pool.Pgx().Begin(ctx)
	if err != nil {
		return fmt.Errorf("memgw: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		UPDATE projection_outbox
		SET state = 'applied', applied_at = now(), lease_owner = NULL, lease_expires_at = NULL
		WHERE outbox_id = $1`, it.OutboxID); err != nil {
		return fmt.Errorf("memgw: mark applied: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE projection_watermark
		SET applied_seq = GREATEST(applied_seq, $2), updated_at = now()
		WHERE projection = $1`, it.Projection, it.EventSeq); err != nil {
		return fmt.Errorf("memgw: advance watermark: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("memgw: commit projection progress: %w", err)
	}
	return nil
}

// MarkFailed schedules a retry, or dead-letters the row once it has failed
// enough times. Only an error class is stored: the failing payload is never
// copied into the queue.
func (s *Store) MarkFailed(ctx context.Context, it Item, errorClass string, backoff time.Duration) error {
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()
	state := StateFailed
	if it.Attempts >= s.maxAttempts {
		state = StateDeadLetter
	}
	if _, err := s.pool.Pgx().Exec(ctx, `
		UPDATE projection_outbox
		SET state = $2, last_error_class = $3, not_before = now() + $4::interval,
		    lease_owner = NULL, lease_expires_at = NULL
		WHERE outbox_id = $1`, it.OutboxID, state, errorClass, backoff.String()); err != nil {
		return fmt.Errorf("memgw: mark failed: %w", err)
	}
	return nil
}

// ReclaimExpiredLeases returns rows whose worker died mid-flight to the queue.
// This is the other half of crash safety: MarkApplied never ran, so the row is
// still owed, and reclaiming it is what makes delivery at-least-once.
func (s *Store) ReclaimExpiredLeases(ctx context.Context) (int64, error) {
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()
	tag, err := s.pool.Pgx().Exec(ctx, `
		UPDATE projection_outbox
		SET state = 'pending', lease_owner = NULL, lease_expires_at = NULL
		WHERE state = 'in_flight' AND lease_expires_at < now()`)
	if err != nil {
		return 0, fmt.Errorf("memgw: reclaim leases: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Watermark reports how far a projection has been applied.
func (s *Store) Watermark(ctx context.Context, p Projection) (int64, error) {
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()
	var seq int64
	if err := s.pool.Pgx().QueryRow(ctx,
		`SELECT applied_seq FROM projection_watermark WHERE projection = $1`, p).Scan(&seq); err != nil {
		return 0, fmt.Errorf("memgw: read watermark: %w", err)
	}
	return seq, nil
}

// DeadLetters lists rows that stopped being retried. They are visible on
// purpose: a queue that silently drops what it cannot deliver is worse than one
// that stops.
func (s *Store) DeadLetters(ctx context.Context, p Projection) ([]Item, error) {
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()
	rows, err := s.pool.Pgx().Query(ctx, `
		SELECT outbox_id, event_id, event_seq, projection, attempts
		FROM projection_outbox WHERE projection = $1 AND state = 'dead_letter' ORDER BY outbox_id`, p)
	if err != nil {
		return nil, fmt.Errorf("memgw: read dead letters: %w", err)
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.OutboxID, &it.EventID, &it.EventSeq, &it.Projection, &it.Attempts); err != nil {
			return nil, fmt.Errorf("memgw: read dead letters: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Tombstone is one entry in the deletion journal.
type Tombstone struct {
	ID              uuid.UUID
	SubjectType     string
	SubjectID       string
	ProjectID       uuid.UUID
	WorkspaceID     *uuid.UUID
	ReasonClass     string
	IssuedBy        uuid.UUID
	IssuedAt        time.Time
	ErasureRequired bool
	LegacyChunkIDs  []string
}

// IssueTombstone appends to the deletion journal. The journal is monotonic:
// there is no delete, and a rebuild replays it.
func (s *Store) IssueTombstone(ctx context.Context, t Tombstone) (Tombstone, error) {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	if t.LegacyChunkIDs == nil {
		t.LegacyChunkIDs = []string{}
	}
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()
	err := s.pool.Pgx().QueryRow(ctx, `
		INSERT INTO tombstones (tombstone_id, subject_type, subject_id, project_id, workspace_id,
		                        reason_class, issued_by_principal_id, erasure_required, legacy_chunk_ids)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING issued_at`,
		t.ID, t.SubjectType, t.SubjectID, t.ProjectID, t.WorkspaceID, t.ReasonClass,
		t.IssuedBy, t.ErasureRequired, t.LegacyChunkIDs,
	).Scan(&t.IssuedAt)
	if err != nil {
		return Tombstone{}, fmt.Errorf("memgw: issue tombstone: %w", err)
	}
	// Chunks known for this subject are marked in the same breath, so a
	// deletion ordered today reaches points written before the gateway existed.
	if _, err := s.pool.Pgx().Exec(ctx, `
		UPDATE legacy_chunk_map SET tombstoned_at = now()
		WHERE project_id = $1 AND (document_id = $2 OR chunk_id = ANY($3))`,
		t.ProjectID, t.SubjectID, t.LegacyChunkIDs); err != nil {
		return Tombstone{}, fmt.Errorf("memgw: mark legacy chunks: %w", err)
	}
	return t, nil
}

// IsTombstoned reports whether a subject has been ordered deleted. A projection
// applier calls it before writing anything, which is what makes a tombstone win
// over a rebuild that is replaying older events.
func (s *Store) IsTombstoned(ctx context.Context, subjectType, subjectID string) (bool, error) {
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()
	var exists bool
	if err := s.pool.Pgx().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM tombstones WHERE subject_type = $1 AND subject_id = $2)`,
		subjectType, subjectID).Scan(&exists); err != nil {
		return false, fmt.Errorf("memgw: tombstone lookup: %w", err)
	}
	return exists, nil
}

// LegacyChunk maps one pre-gateway vector point back to the document it came
// from, so a deletion can find it after re-chunking changed the ids.
type LegacyChunk struct {
	ChunkID      string
	ProjectID    uuid.UUID
	RAGProject   string
	DocumentID   string
	SourceDigest string
}

// MapLegacyChunks records chunk-to-document mappings. It is idempotent: the
// same mapping recorded twice is the same mapping.
func (s *Store) MapLegacyChunks(ctx context.Context, chunks []LegacyChunk) error {
	if len(chunks) == 0 {
		return nil
	}
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()
	batch := &pgx.Batch{}
	for _, c := range chunks {
		batch.Queue(`
			INSERT INTO legacy_chunk_map (chunk_id, project_id, rag_project, document_id, source_digest)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (chunk_id) DO UPDATE
			SET project_id = EXCLUDED.project_id,
			    rag_project = EXCLUDED.rag_project,
			    document_id = EXCLUDED.document_id,
			    source_digest = EXCLUDED.source_digest`,
			c.ChunkID, c.ProjectID, c.RAGProject, c.DocumentID, c.SourceDigest)
	}
	results := s.pool.Pgx().SendBatch(ctx, batch)
	defer results.Close()
	for range chunks {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("memgw: map legacy chunks: %w", err)
		}
	}
	return nil
}

// TombstonedChunkIDs lists the chunk ids a deletion still has to remove from a
// vector store. It is the worklist an erasure step consumes; this phase does not
// run one.
func (s *Store) TombstonedChunkIDs(ctx context.Context, projectID uuid.UUID) ([]string, error) {
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()
	rows, err := s.pool.Pgx().Query(ctx,
		`SELECT chunk_id FROM legacy_chunk_map WHERE project_id = $1 AND tombstoned_at IS NOT NULL ORDER BY chunk_id`,
		projectID)
	if err != nil {
		return nil, fmt.Errorf("memgw: read tombstoned chunks: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("memgw: read tombstoned chunks: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
