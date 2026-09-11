package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/errclass"
)

// Materialise writes the readable form of an event: the work's new state, the
// checkpoint row, the fact, the tombstone.
//
// It runs after the event row is inserted and inside the same transaction, so
// the materialised state and the event that explains it commit together or not
// at all. If this function fails, the event does not exist either -- which is
// the only arrangement in which "the work is in a state no event explains" is
// impossible rather than merely unlikely.
func Materialise(ctx context.Context, tx DB, ev Event, seq int64, revision *int64) error {
	if _, err := tx.Exec(ctx,
		`UPDATE sessions SET last_event_seq = GREATEST(last_event_seq, $2) WHERE session_id = $1`,
		ev.SessionID, seq); err != nil {
		return err
	}

	switch {
	case ev.Type == "checkpoint.recorded":
		if err := advanceWork(ctx, tx, ev, seq, revision, ""); err != nil {
			return err
		}
		return insertCheckpoint(ctx, tx, ev, seq, revision)
	case workMutating[ev.Type]:
		return advanceWork(ctx, tx, ev, seq, revision, targetState[ev.Type])
	case ev.Type == "evidence.recorded":
		return insertEvidence(ctx, tx, ev, seq)
	case ev.Type == "session.ended":
		_, err := tx.Exec(ctx,
			`UPDATE sessions SET ended_at = $2 WHERE session_id = $1 AND ended_at IS NULL`,
			ev.SessionID, ev.OccurredAt)
		return err
	case ev.Type == "fact.candidate_proposed":
		return applyCandidate(ctx, tx, ev, seq)
	case ev.Type == "fact.promoted":
		return applyPromotion(ctx, tx, ev, seq, revision)
	case ev.Type == "fact.superseded", ev.Type == "fact.retracted", ev.Type == "fact.conflict_flagged":
		return applyFactChange(ctx, tx, ev, revision)
	case ev.Type == "tombstone.issued":
		return applyTombstone(ctx, tx, ev, seq)
	case ev.Type == "writer_epoch.opened", ev.Type == "writer_epoch.fenced":
		return applyWriterEpoch(ctx, tx, ev, seq)
	default:
		return nil
	}
}

func advanceWork(ctx context.Context, tx DB, ev Event, seq int64, revision *int64, state string) error {
	if revision == nil {
		return errclass.New(errclass.PolicyRejected, "%s advances a work and must carry a revision", ev.Type)
	}
	var err error
	if state == "" {
		// A checkpoint advances the revision without changing the state, which
		// is what makes it safe to take one at any moment.
		_, err = tx.Exec(ctx, `
			UPDATE works SET revision = $2, last_event_seq = $3, updated_at = $4 WHERE work_id = $1`,
			*ev.WorkID, *revision, seq, ev.OccurredAt)
	} else {
		_, err = tx.Exec(ctx, `
			UPDATE works SET state = $2, revision = $3, last_event_seq = $4, updated_at = $5 WHERE work_id = $1`,
			*ev.WorkID, state, *revision, seq, ev.OccurredAt)
	}
	return err
}

func insertCheckpoint(ctx context.Context, tx DB, ev Event, seq int64, revision *int64) error {
	var p CheckpointPayload
	if err := decode(ev.Payload, &p); err != nil {
		return err
	}
	files, err := json.Marshal(nonNilStrings(p.ModifiedFiles))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(ev.Payload)
	_, err = tx.Exec(ctx, `
		INSERT INTO checkpoints (checkpoint_id, event_id, project_id, workspace_id, work_id, session_id,
		                         branch_id, principal_id, revision, seq, objective, completed_work,
		                         pending_actions, blockers, next_safe_action, modified_files, evidence_refs,
		                         content_digest, recorded_at)
		VALUES ($1,$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		ev.EventID, ev.ProjectID, ev.WorkspaceID, *ev.WorkID, ev.SessionID, ev.BranchID, ev.PrincipalID,
		*revision, seq, p.Objective, p.CompletedWork, p.PendingActions, blockersText(p.Blockers),
		p.NextSafeAction, files, nonNilUUIDs(ev.EvidenceRefs), hex.EncodeToString(sum[:]), ev.OccurredAt)
	return err
}

func insertEvidence(ctx context.Context, tx DB, ev Event, seq int64) error {
	var p EvidencePayload
	if err := decode(ev.Payload, &p); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO evidence (evidence_id, event_id, project_id, workspace_id, work_id, session_id,
		                      principal_id, evidence_type, evidence_class, locator, seq, recorded_at)
		VALUES ($1,$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		ev.EventID, ev.ProjectID, ev.WorkspaceID, ev.WorkID, ev.SessionID, ev.PrincipalID,
		truncate(p.EvidenceType, 60), p.EvidenceClass, truncate(p.Locator, 400), seq, ev.OccurredAt)
	return err
}

// applyTombstone appends to the deletion journal and marks what the deletion
// reaches. The journal row records which event ordered the deletion and where
// that event sits in the log: a resurrection race is decided by sequence, and a
// wall clock cannot decide it at all.
func applyTombstone(ctx context.Context, tx DB, ev Event, seq int64) error {
	var p TombstonePayload
	if err := decode(ev.Payload, &p); err != nil {
		return err
	}
	chunks := p.LegacyChunkIDs
	if chunks == nil {
		chunks = []string{}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO tombstones (tombstone_id, subject_type, subject_id, project_id, workspace_id,
		                        reason_class, issued_by_principal_id, issued_at, erasure_required,
		                        legacy_chunk_ids, issued_event_id, issued_event_seq)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$1,$11)`,
		ev.EventID, p.SubjectType, p.SubjectID, ev.ProjectID, ev.WorkspaceID, p.ReasonClass,
		ev.PrincipalID, ev.OccurredAt, p.ErasureRequired, chunks, seq); err != nil {
		return err
	}
	// Chunks written before the gateway existed are marked in the same breath,
	// so a deletion ordered today reaches points whose ids re-chunking changed.
	if _, err := tx.Exec(ctx, `
		UPDATE legacy_chunk_map SET tombstoned_at = $4
		WHERE project_id = $1 AND (document_id = $2 OR chunk_id = ANY($3))`,
		ev.ProjectID, p.SubjectID, chunks, ev.OccurredAt); err != nil {
		return err
	}

	switch p.SubjectType {
	case "fact":
		id, err := uuid.Parse(p.SubjectID)
		if err != nil {
			return errclass.New(errclass.PolicyRejected, "a fact tombstone must name a fact id")
		}
		_, err = tx.Exec(ctx,
			`UPDATE facts SET tombstoned_at = $2 WHERE fact_id = $1 AND project_id = $3`,
			id, ev.OccurredAt, ev.ProjectID)
		return err
	case "work":
		id, err := uuid.Parse(p.SubjectID)
		if err != nil {
			return errclass.New(errclass.PolicyRejected, "a work tombstone must name a work id")
		}
		_, err = tx.Exec(ctx,
			`UPDATE works SET tombstoned_at = $2 WHERE work_id = $1 AND project_id = $3`,
			id, ev.OccurredAt, ev.ProjectID)
		return err
	}
	return nil
}

// applyWriterEpoch opens or fences a writer epoch.
//
// Fencing is what makes a cutover safe: the check that refuses a write runs
// server-side against this table, so a host that went offline before it was
// told anything is still refused when it comes back. That only works if the
// event actually writes the row, which is why this exists -- the event type was
// accepted and admin-gated long before it did anything, and an admin fencing an
// epoch would have got a committed receipt for a fence that never happened.
func applyWriterEpoch(ctx context.Context, tx DB, ev Event, seq int64) error {
	var p WriterEpochPayload
	if err := decode(ev.Payload, &p); err != nil {
		return err
	}
	if ev.Type == "writer_epoch.opened" {
		tag, err := tx.Exec(ctx, `
			INSERT INTO writer_epochs (epoch, opened_at, opened_by_principal_id, reason_class)
			VALUES ($1,$2,$3,$4) ON CONFLICT (epoch) DO NOTHING`,
			p.Epoch, ev.OccurredAt, ev.PrincipalID, p.ReasonClass)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// Re-opening an epoch that exists would move opened_at, and every
			// event already stamped with it points at that row. The duplicate
			// is refused rather than absorbed: two opens of one epoch mean two
			// hosts believe they were admitted separately.
			return errclass.New(errclass.PolicyRejected, "that writer epoch is already open")
		}
		return nil
	}

	drain := seq
	if p.DrainWatermarkSeq != nil {
		drain = *p.DrainWatermarkSeq
	}
	// Only an unfenced epoch is fenced. A second fence leaves the first
	// timestamp and the first watermark alone -- when the epoch stopped
	// accepting writes is evidence, and a retry during an incident must not
	// rewrite it. The epoch has to exist: fencing one that was never opened
	// would create a row that no event was ever stamped with.
	tag, err := tx.Exec(ctx, `
		UPDATE writer_epochs
		SET fenced_at = $2, reason_class = $3, drain_watermark_seq = $4
		WHERE epoch = $1 AND fenced_at IS NULL`,
		p.Epoch, ev.OccurredAt, p.ReasonClass, drain)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM writer_epochs WHERE epoch = $1)`, p.Epoch).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errclass.New(errclass.PolicyRejected, "that writer epoch was never opened")
	}
	return nil
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilUUIDs(u []uuid.UUID) []uuid.UUID {
	if u == nil {
		return []uuid.UUID{}
	}
	return u
}
