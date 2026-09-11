package migrate

import (
	"context"

	"github.com/google/uuid"
)

// Replayed is what a tombstone replay reached.
type Replayed struct {
	// Tombstones is how many journal rows were in scope.
	Tombstones int64
	// Marked is how many mapping rows the replay newly marked. Zero is the
	// expected result on a healthy ledger: it means every deletion already
	// reached everything it names.
	Marked int64
}

// Replay re-applies the deletion journal to the chunk mapping.
//
// The gateway marks mapping rows at the moment a tombstone commits, so on a
// ledger that has only ever been written through the gateway this finds
// nothing. It exists for the two cases where that is not true: a mapping row
// imported after the deletion was ordered, and a mapping row restored from a
// backup taken before it. In both cases the row is readable again while a
// tombstone that names it is sitting in the journal, and anything that queries
// the corpus before the replay runs will hand back content somebody asked to
// have deleted. That is why the order is replay, then query -- never the
// reverse.
//
// The predicate is the same one applyTombstone uses (subject_id against
// document_id, or a named chunk id), deliberately including the absence of a
// subject_type filter: a replay that reached more or less than the live writer
// would make the two disagree, and then neither could be trusted.
//
// The mark is the tombstone's own issued_at, not now(): the deletion happened
// when it was ordered. Where several tombstones reach one row the earliest
// wins. Rows already marked are left exactly as they are -- a replay never
// moves a deletion later.
//
// uuid.Nil means every project.
func (t *PGTarget) Replay(ctx context.Context, project uuid.UUID) (Replayed, error) {
	var r Replayed
	tx, err := t.pool.Pgx().Begin(ctx)
	if err != nil {
		return r, err
	}
	defer tx.Rollback(ctx)

	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM tombstones
		WHERE $1 = '00000000-0000-0000-0000-000000000000'::uuid OR project_id = $1`,
		project).Scan(&r.Tombstones); err != nil {
		return Replayed{}, err
	}

	tag, err := tx.Exec(ctx, `
		WITH reach AS (
			SELECT m.chunk_id, min(t.issued_at) AS issued_at
			FROM legacy_chunk_map m
			JOIN tombstones t
			  ON t.project_id = m.project_id
			 AND (t.subject_id = m.document_id OR m.chunk_id = ANY(t.legacy_chunk_ids))
			WHERE m.tombstoned_at IS NULL
			  AND ($1 = '00000000-0000-0000-0000-000000000000'::uuid OR m.project_id = $1)
			GROUP BY m.chunk_id
		)
		UPDATE legacy_chunk_map m
		SET tombstoned_at = reach.issued_at
		FROM reach
		WHERE m.chunk_id = reach.chunk_id AND m.tombstoned_at IS NULL`, project)
	if err != nil {
		return Replayed{}, err
	}
	r.Marked = tag.RowsAffected()

	if err := tx.Commit(ctx); err != nil {
		return Replayed{}, err
	}
	return r, nil
}
