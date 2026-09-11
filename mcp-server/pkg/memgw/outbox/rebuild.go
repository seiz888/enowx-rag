package outbox

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Rebuilt is what a rebuild queued.
type Rebuilt struct {
	// Restored are events that had no outbox row at all. In a healthy database
	// this is zero: the row is written in the same transaction as the event. It
	// is not zero after a restore that lost the queue, which is exactly the
	// case a rebuild has to be able to repair.
	Restored int64
	// Requeued are rows that were reset to pending.
	Requeued int64
	// Watermark is where the projection had got to. A rebuild does not move it
	// backwards -- it records how far the projection has been applied at least
	// once, and a replay of older events does not make that less true.
	Watermark int64
}

// Rebuild re-queues a projection from the events that are still in the ledger.
//
// This is the recovery path the whole design rests on: the index is a cache and
// the ledger is the truth, so an index that is lost, corrupted or restored from
// an old backup is repaired by replaying events rather than by restoring the
// index. Nothing here writes to the vector store; it only marks work as
// outstanding, and the worker does the rest.
//
// A replay is safe against deletion because the applier consults the tombstone
// journal before it renders anything: an event older than a tombstone is
// skipped rather than resurrected. That ordering is what makes "replay
// tombstones before you serve a query" a property of the code and not of the
// runbook.
//
// In-flight rows are left alone. Resetting a row another worker holds a lease
// on would produce two workers applying the same event, and the fix for a
// genuinely stuck lease is the lease expiring, not a second writer.
func (s *Store) Rebuild(ctx context.Context, p Projection, project uuid.UUID) (Rebuilt, error) {
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()

	var out Rebuilt
	tx, err := s.pool.Pgx().Begin(ctx)
	if err != nil {
		return out, fmt.Errorf("memgw: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// A NULL project means every project. It is spelled as a parameter rather
	// than as two statements so the scope of a rebuild is one value an operator
	// passed, visible in one place.
	var scope any
	if project != uuid.Nil {
		scope = project
	}

	restored, err := tx.Exec(ctx, `
		INSERT INTO projection_outbox (event_id, event_seq, projection)
		SELECT e.event_id, e.seq, $1
		FROM events e
		WHERE ($2::uuid IS NULL OR e.project_id = $2::uuid)
		ON CONFLICT (event_id, projection) DO NOTHING`, p, scope)
	if err != nil {
		return out, fmt.Errorf("memgw: restore missing outbox rows: %w", err)
	}
	out.Restored = restored.RowsAffected()

	requeued, err := tx.Exec(ctx, `
		UPDATE projection_outbox o
		SET state = 'pending',
		    attempts = 0,
		    not_before = now(),
		    lease_owner = NULL,
		    lease_expires_at = NULL,
		    last_error_class = NULL,
		    applied_at = NULL
		FROM events e
		WHERE o.event_id = e.event_id
		  AND o.projection = $1
		  AND o.state <> 'in_flight'
		  AND ($2::uuid IS NULL OR e.project_id = $2::uuid)`, p, scope)
	if err != nil {
		return out, fmt.Errorf("memgw: requeue outbox rows: %w", err)
	}
	out.Requeued = requeued.RowsAffected()

	if err := tx.QueryRow(ctx,
		`SELECT applied_seq FROM projection_watermark WHERE projection = $1`, p).Scan(&out.Watermark); err != nil {
		return out, fmt.Errorf("memgw: read watermark: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Rebuilt{}, fmt.Errorf("memgw: commit rebuild: %w", err)
	}
	return out, nil
}
