package projection

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
)

// PgEvents reads canonical events from PostgreSQL.
//
// It is a separate type from the outbox store and holds only a pool, so the
// projection path has no route to a write: there is no Exec here and no
// transaction, and adding one would be visible in a diff of eleven lines rather
// than buried in a package that also writes.
type PgEvents struct {
	pool *pgstore.Pool
}

// NewPgEvents returns a read-only event source over the pool.
func NewPgEvents(pool *pgstore.Pool) *PgEvents { return &PgEvents{pool: pool} }

// Event implements EventSource.
func (p *PgEvents) Event(ctx context.Context, id uuid.UUID) (Record, error) {
	ctx, cancel := p.pool.WithQueryTimeout(ctx)
	defer cancel()
	var r Record
	err := p.pool.Pgx().QueryRow(ctx, `
		SELECT event_id, seq, event_type, project_id, workspace_id, work_id, session_id,
		       branch_id, principal_id, sensitivity_class, revision, payload, occurred_at
		FROM events WHERE event_id = $1`, id).Scan(
		&r.EventID, &r.Seq, &r.Type, &r.ProjectID, &r.WorkspaceID, &r.WorkID, &r.SessionID,
		&r.BranchID, &r.PrincipalID, &r.Sensitivity, &r.Revision, &r.Payload, &r.OccurredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrEventGone
	}
	if err != nil {
		// No detail is carried out: the class is what the outbox stores, and
		// the driver's message can quote the row it failed to read.
		return Record{}, failure("event_read_failed", "the event could not be read from the ledger")
	}
	return r, nil
}
