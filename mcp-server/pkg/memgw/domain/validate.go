package domain

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/enowdev/enowx-rag/pkg/memgw/errclass"
)

// Prepare validates an event against canonical state and creates the rows the
// event has to be able to point at: the session, the branch, and -- for
// work.planned -- the work itself.
//
// It runs inside the commit transaction, before the event row is inserted, so
// everything it does is undone if anything later in the transaction fails. It
// returns the aggregate the event advances, which is what compare-and-set is
// then applied to. The aggregate is derived here rather than accepted from the
// submission: a writer that could name its own aggregate could compare-and-set
// against something nobody else is contending for.
func Prepare(ctx context.Context, tx DB, ev Event, cas CAS) (Aggregate, *int64, error) {
	if subagentForbidden[ev.Type] && ev.PrincipalType == "subagent" {
		// INV-16. A subagent's finding is input to the parent's decision. The
		// refusal is scope_denied rather than policy_rejected because it is
		// about who is asking, not about what was asked.
		return Aggregate{}, nil, errclass.New(errclass.ScopeDenied,
			"a subagent may contribute evidence and candidates but may not submit %s", ev.Type)
	}

	if err := checkProject(ctx, tx, ev); err != nil {
		return Aggregate{}, nil, err
	}
	if err := checkWorkspace(ctx, tx, ev); err != nil {
		return Aggregate{}, nil, err
	}
	if err := ensureSession(ctx, tx, ev); err != nil {
		return Aggregate{}, nil, err
	}
	if err := ensureBranch(ctx, tx, ev); err != nil {
		return Aggregate{}, nil, err
	}
	if err := checkEvidenceRefs(ctx, tx, ev); err != nil {
		return Aggregate{}, nil, err
	}

	switch {
	case workMutating[ev.Type]:
		return prepareWork(ctx, tx, ev, cas)
	case ev.Type == "evidence.recorded":
		return Aggregate{}, nil, prepareEvidence(ctx, tx, ev)
	case ev.Type == "fact.candidate_proposed":
		return Aggregate{}, nil, prepareCandidate(ctx, tx, ev)
	case ev.Type == "fact.promoted":
		return preparePromotion(ctx, tx, ev, cas)
	case ev.Type == "fact.superseded", ev.Type == "fact.retracted", ev.Type == "fact.conflict_flagged":
		return prepareFactChange(ctx, tx, ev, cas)
	case ev.Type == "tombstone.issued":
		return Aggregate{}, nil, prepareTombstone(ev)
	case ev.Type == "writer_epoch.opened", ev.Type == "writer_epoch.fenced":
		return Aggregate{}, nil, prepareWriterEpoch(ev)
	default:
		// session.* and projection.rebuild_requested are additive: they record
		// that something happened without claiming an aggregate moved.
		return Aggregate{}, nil, nil
	}
}

func checkProject(ctx context.Context, tx DB, ev Event) error {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM projects WHERE project_id = $1`, ev.ProjectID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		// Not "no such project": a principal must not be able to map the
		// registry by submitting to guessed ids.
		return errclass.New(errclass.ScopeDenied, "the requested project is not available to this principal")
	}
	if err != nil {
		return err
	}
	if status != "active" {
		return errclass.New(errclass.PolicyRejected, "the project is archived and accepts no new events")
	}
	return nil
}

func checkWorkspace(ctx context.Context, tx DB, ev Event) error {
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM workspaces WHERE workspace_id = $1 AND project_id = $2)`,
		ev.WorkspaceID, ev.ProjectID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errclass.New(errclass.ScopeDenied, "the requested workspace is not part of the requested project")
	}
	return nil
}

// ensureSession creates the session on first use and refuses a session id that
// already belongs to a different project, workspace or principal.
//
// Auto-creation is deliberate: requiring a session.started before anything else
// would mean an adapter that missed the first hook could never write at all,
// and the recovery path for a missed lifecycle event is exactly what these
// hosts are bad at. Refusing a reused id is equally deliberate: a session id
// that moved between projects would carry its history with it.
func ensureSession(ctx context.Context, tx DB, ev Event) error {
	var projectID, workspaceID, principalID uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT project_id, workspace_id, principal_id FROM sessions WHERE session_id = $1`, ev.SessionID,
	).Scan(&projectID, &workspaceID, &principalID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		var parent *string
		if ev.Type == "session.started" || ev.Type == "session.resumed" {
			var p SessionPayload
			if err := decode(ev.Payload, &p); err != nil {
				return err
			}
			if p.ParentSessionID != "" {
				parent = &p.ParentSessionID
			}
		}
		if parent != nil {
			var parentExists bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM sessions WHERE session_id = $1 AND project_id = $2)`,
				*parent, ev.ProjectID).Scan(&parentExists); err != nil {
				return err
			}
			if !parentExists {
				return errclass.New(errclass.PolicyRejected,
					"the session this one resumes is unknown in this project")
			}
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO sessions (session_id, project_id, workspace_id, principal_id, parent_session_id, host_id, started_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			ev.SessionID, ev.ProjectID, ev.WorkspaceID, ev.PrincipalID, parent, ev.HostID, ev.OccurredAt)
		return err
	case err != nil:
		return err
	}
	if projectID != ev.ProjectID || workspaceID != ev.WorkspaceID {
		return errclass.New(errclass.ScopeDenied,
			"this session id already belongs to a different project or workspace")
	}
	// A subagent writes under its own session id, so the owning principal check
	// is exact rather than "same principal or its parent".
	if principalID != ev.PrincipalID {
		return errclass.New(errclass.ScopeDenied, "this session id belongs to a different principal")
	}
	return nil
}

// ensureBranch creates the branch on first use, and handles a rewind.
//
// A rewind opens a new branch that names where it diverged and marks the parent
// superseded. The parent keeps every event it had: nothing is renumbered and
// nothing is deleted -- INV-19.
func ensureBranch(ctx context.Context, tx DB, ev Event) error {
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM branches WHERE branch_id = $1)`, ev.BranchID).Scan(&exists); err != nil {
		return err
	}

	if ev.Type != "session.branched" {
		if exists {
			return checkBranchBelongs(ctx, tx, ev)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO branches (branch_id, project_id, session_id, reason_class, status, created_at)
			VALUES ($1,$2,$3,'initial','active',$4)`,
			ev.BranchID, ev.ProjectID, ev.SessionID, ev.OccurredAt)
		return err
	}

	var p SessionPayload
	if err := decode(ev.Payload, &p); err != nil {
		return err
	}
	if p.ParentBranchID == nil || p.BranchPointSeq == nil {
		return errclass.New(errclass.PolicyRejected,
			"session.branched must name parent_branch_id and branch_point_seq; a rewind with no divergence point is not auditable")
	}
	if *p.ParentBranchID == ev.BranchID {
		return errclass.New(errclass.PolicyRejected, "a branch cannot be its own parent")
	}
	var parentProject uuid.UUID
	err := tx.QueryRow(ctx, `SELECT project_id FROM branches WHERE branch_id = $1`, *p.ParentBranchID).Scan(&parentProject)
	if errors.Is(err, pgx.ErrNoRows) {
		return errclass.New(errclass.PolicyRejected, "the parent branch is unknown")
	}
	if err != nil {
		return err
	}
	if parentProject != ev.ProjectID {
		return errclass.New(errclass.ScopeDenied, "the parent branch belongs to a different project")
	}
	if exists {
		// A rewind that reuses an existing branch id would merge two histories
		// into one line and make the branch point a lie.
		return errclass.New(errclass.PolicyRejected, "this branch id already exists; a rewind opens a new one")
	}
	reason := p.ReasonClass
	if reason == "" {
		reason = "rewind"
	}
	if !branchReasonClasses[reason] {
		// The column is bounded by a CHECK constraint. Letting an unknown class
		// reach it turns a policy decision into a driver error, which the
		// gateway reports as a 500 and a durable client then retries forever:
		// one mis-classified event becomes a queue that never drains.
		return errclass.New(errclass.PolicyRejected,
			"session.branched reason_class is not a known branch class")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO branches (branch_id, project_id, session_id, parent_branch_id, branch_point_seq, reason_class, status, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,'active',$7)`,
		ev.BranchID, ev.ProjectID, ev.SessionID, *p.ParentBranchID, *p.BranchPointSeq, reason, ev.OccurredAt); err != nil {
		return err
	}
	// Superseded, not deleted. The parent branch's events stay queryable.
	_, err = tx.Exec(ctx,
		`UPDATE branches SET status = 'superseded' WHERE branch_id = $1 AND status = 'active'`, *p.ParentBranchID)
	return err
}

// branchReasonClasses is the closed set the branches table accepts. It is
// duplicated here from the migration on purpose: the constraint is the last
// line of defence, and a refusal that arrives as a policy class is one a client
// can act on, while the same refusal arriving as SQLSTATE 23514 is not.
var branchReasonClasses = map[string]bool{
	"rewind": true, "fork": true, "resume": true, "compaction": true, "initial": true,
}

func checkBranchBelongs(ctx context.Context, tx DB, ev Event) error {
	var projectID uuid.UUID
	var sessionID string
	if err := tx.QueryRow(ctx,
		`SELECT project_id, session_id FROM branches WHERE branch_id = $1`, ev.BranchID,
	).Scan(&projectID, &sessionID); err != nil {
		return err
	}
	if projectID != ev.ProjectID {
		return errclass.New(errclass.ScopeDenied, "this branch belongs to a different project")
	}
	return nil
}

// checkEvidenceRefs requires every cited evidence reference to be an event that
// exists in this project. A reference to nothing is worse than no reference: it
// reads like corroboration.
func checkEvidenceRefs(ctx context.Context, tx DB, ev Event) error {
	if len(ev.EvidenceRefs) == 0 {
		return nil
	}
	var found int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE event_id = ANY($1) AND project_id = $2`,
		ev.EvidenceRefs, ev.ProjectID).Scan(&found); err != nil {
		return err
	}
	if found != len(ev.EvidenceRefs) {
		return errclass.New(errclass.PolicyRejected,
			"an evidence reference names an event that does not exist in this project")
	}
	return nil
}

// prepareWork applies compare-and-set to the work, then validates the
// transition. In that order: see CAS.
func prepareWork(ctx context.Context, tx DB, ev Event, cas CAS) (Aggregate, *int64, error) {
	if ev.WorkID == nil {
		return Aggregate{}, nil, errclass.New(errclass.PolicyRejected, "%s must name a work_id", ev.Type)
	}
	agg := Aggregate{Type: AggregateWork, ID: *ev.WorkID, Mutating: true}
	revision, err := cas(ctx, agg)
	if err != nil {
		return Aggregate{}, nil, err
	}

	current, err := readWork(ctx, tx, *ev.WorkID)
	switch {
	case errors.Is(err, ErrNotFound):
		if ev.Type != "work.planned" {
			return Aggregate{}, nil, errclass.New(errclass.PolicyRejected,
				"%s refers to a work that has not been planned", ev.Type)
		}
		var p WorkPayload
		if err := decode(ev.Payload, &p); err != nil {
			return Aggregate{}, nil, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO works (work_id, project_id, workspace_id, title, state, revision, last_event_seq, created_at, updated_at)
			VALUES ($1,$2,$3,$4,'planned',0,0,$5,$5)`,
			*ev.WorkID, ev.ProjectID, ev.WorkspaceID, truncate(p.Title, 200), ev.OccurredAt); err != nil {
			return Aggregate{}, nil, err
		}
		return agg, revision, nil
	case err != nil:
		return Aggregate{}, nil, err
	}

	if current.ProjectID != ev.ProjectID || current.WorkspaceID != ev.WorkspaceID {
		return Aggregate{}, nil, errclass.New(errclass.ScopeDenied,
			"this work belongs to a different project or workspace")
	}
	if current.Tombstoned {
		return Aggregate{}, nil, errclass.New(errclass.PolicyRejected, "this work has been tombstoned")
	}
	if ev.Type == "work.planned" {
		return Aggregate{}, nil, errclass.New(errclass.PolicyRejected, "this work has already been planned")
	}
	if want, ok := targetState[ev.Type]; ok && !canTransition(current.State, want) {
		return Aggregate{}, nil, errclass.New(errclass.PolicyRejected,
			"a work in state %s cannot move to %s", current.State, want)
	}
	if ev.Type == "checkpoint.recorded" {
		if current.State == "completed" || current.State == "abandoned" {
			return Aggregate{}, nil, errclass.New(errclass.PolicyRejected,
				"a work in state %s takes no further checkpoints", current.State)
		}
		if err := validateCheckpoint(ev); err != nil {
			return Aggregate{}, nil, err
		}
	}
	return agg, revision, nil
}

func validateCheckpoint(ev Event) error {
	var p CheckpointPayload
	if err := decode(ev.Payload, &p); err != nil {
		return err
	}
	switch {
	case p.Objective == "":
		return errclass.New(errclass.PolicyRejected, "a checkpoint must state an objective")
	case p.NextSafeAction == "":
		return errclass.New(errclass.PolicyRejected, "a checkpoint must state the next safe action")
	case len(p.Objective) > 4096 || len(p.NextSafeAction) > 4096 ||
		len(p.CompletedWork) > 4096 || len(p.PendingActions) > 4096:
		return errclass.New(errclass.PolicyRejected, "a checkpoint text field exceeds the 4096 byte bound")
	case len(p.ModifiedFiles) > 200:
		return errclass.New(errclass.PolicyRejected, "a checkpoint lists more than 200 modified files")
	}
	return nil
}

func prepareEvidence(ctx context.Context, tx DB, ev Event) error {
	var p EvidencePayload
	if err := decode(ev.Payload, &p); err != nil {
		return err
	}
	if p.EvidenceType == "" {
		return errclass.New(errclass.PolicyRejected, "evidence must state its evidence_type")
	}
	if !validEvidenceClass[p.EvidenceClass] {
		return errclass.New(errclass.PolicyRejected, "evidence must state a known evidence_class")
	}
	if ev.WorkID != nil {
		if _, err := readWork(ctx, tx, *ev.WorkID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return errclass.New(errclass.PolicyRejected, "evidence refers to a work that does not exist")
			}
			return err
		}
	}
	return nil
}

var validEvidenceClass = map[string]bool{
	"observed": true, "derived": true, "asserted": true, "unverified": true,
}

func prepareTombstone(ev Event) error {
	var p TombstonePayload
	if err := decode(ev.Payload, &p); err != nil {
		return err
	}
	if !validTombstoneSubject[p.SubjectType] {
		return errclass.New(errclass.PolicyRejected, "tombstone subject_type is not a known subject class")
	}
	if p.SubjectID == "" {
		return errclass.New(errclass.PolicyRejected, "tombstone must name its subject")
	}
	if !validTombstoneReason[p.ReasonClass] {
		// A class, never free text: a reason that quoted the offending content
		// would re-introduce exactly what the tombstone removes.
		return errclass.New(errclass.PolicyRejected, "tombstone reason_class is not a known class")
	}
	return nil
}

// prepareWriterEpoch checks the shape of an epoch event before anything is
// written. The epoch itself is a positive number and never zero: zero is what
// an omitted field decodes to, and admitting it would let a malformed payload
// fence whatever epoch happened to be numbered zero.
func prepareWriterEpoch(ev Event) error {
	var p WriterEpochPayload
	if err := decode(ev.Payload, &p); err != nil {
		return err
	}
	if p.Epoch <= 0 {
		return errclass.New(errclass.PolicyRejected, "a writer_epoch event must name the epoch it is about")
	}
	if !writerEpochReasons[p.ReasonClass] {
		return errclass.New(errclass.PolicyRejected, "writer_epoch reason_class is not a known class")
	}
	if p.DrainWatermarkSeq != nil && *p.DrainWatermarkSeq < 0 {
		return errclass.New(errclass.PolicyRejected, "drain_watermark_seq cannot be negative")
	}
	return nil
}

var validTombstoneSubject = map[string]bool{
	"document": true, "event": true, "checkpoint": true, "fact": true,
	"candidate": true, "work": true, "session": true,
}

var validTombstoneReason = map[string]bool{
	"sensitive_content": true, "user_request": true, "policy_violation": true,
	"duplicate": true, "superseded": true, "legal_hold_release": true,
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// nowOr returns the event's own time when a payload leaves a timestamp unset.
// Validity intervals come from the writer, never from the server clock, so a
// replay produces the same interval it did the first time.
func nowOr(t *time.Time, fallback time.Time) time.Time {
	if t == nil {
		return fallback
	}
	return *t
}
