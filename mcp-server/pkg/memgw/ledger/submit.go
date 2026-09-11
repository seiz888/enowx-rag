// Package ledger implements the durable write path: validate, authenticate,
// authorize, deduplicate, compare-and-set, append, receipt, enqueue, commit.
//
// The ordering matters and is the contract's, not an implementation detail. In
// particular the receipt is written inside the same transaction as the event,
// so a receipt existing is proof the event committed -- and a transaction that
// fails leaves neither. Nothing here talks to Qdrant, Voyage or enowx-rag: a
// commit only enqueues an outbox row, and projecting it is somebody else's job.
package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/enowdev/enowx-rag/pkg/core"
	"github.com/enowdev/enowx-rag/pkg/memgw/domain"
	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
	"github.com/enowdev/enowx-rag/pkg/memgw/principal"
)

// ContractSchemaVersion and ContractPolicyVersion are the frozen versions this
// implementation speaks. A client announcing anything else is refused rather
// than served by a best guess.
const (
	ContractSchemaVersion = "1.0.0"
	ContractPolicyVersion = "1.0.0"
)

// ReceiptState is the durable state of a write attempt.
type ReceiptState string

const (
	StateCommitted      ReceiptState = "committed"
	StateDuplicate      ReceiptState = "duplicate"
	StateConflict       ReceiptState = "conflict"
	StateRejectedPolicy ReceiptState = "rejected_policy"
	StateQuarantined    ReceiptState = "quarantined"
)

// Event is one submission. Server-assigned fields (seq, recorded_at, revision)
// are not settable: they come back on the receipt.
type Event struct {
	EventID          uuid.UUID
	IdempotencyKey   string
	PrincipalID      uuid.UUID
	ProjectID        uuid.UUID
	WorkspaceID      uuid.UUID
	WorkID           *uuid.UUID
	SessionID        string
	BranchID         uuid.UUID
	Type             string
	Payload          any
	ExpectedRevision *int64
	EvidenceRefs     []uuid.UUID
	SensitivityClass string
	PolicyVersion    string
	SchemaVersion    string
	OccurredAt       time.Time
	WriterEpoch      int64
}

// The aggregate an event advances is derived by the domain, not accepted from
// the submission. A writer that could name its own aggregate could
// compare-and-set against something no other writer is contending for, which
// would make CAS a formality.

// Receipt is the durable record of a write attempt. A receipt is only ever
// returned after the transaction that produced it committed.
type Receipt struct {
	PrincipalID    uuid.UUID    `json:"principal_id"`
	IdempotencyKey string       `json:"idempotency_key"`
	EventID        uuid.UUID    `json:"event_id"`
	PayloadDigest  string       `json:"payload_digest"`
	State          ReceiptState `json:"state"`
	Seq            int64        `json:"seq"`
	Revision       *int64       `json:"revision,omitempty"`
	RecordedAt     time.Time    `json:"recorded_at"`
}

// requiredRoles maps an event type to the roles that may submit it. A
// principal needs any one of them. There is no default: an event type absent
// from this table cannot be written at all, so adding a type to the contract
// without deciding who may write it fails closed.
//
// evidence.recorded takes work_read as well as checkpoint_write because a
// subagent that may read a work may say what it found about it. Evidence is
// non-authoritative by construction -- it moves nothing -- so the boundary that
// matters is the one on state transitions, which subagents are refused outright.
var requiredRoles = map[string][]principal.Role{
	"work.planned":                 {principal.RoleCheckpointWrite},
	"work.activated":               {principal.RoleCheckpointWrite},
	"work.blocked":                 {principal.RoleCheckpointWrite},
	"work.unblocked":               {principal.RoleCheckpointWrite},
	"work.review_requested":        {principal.RoleCheckpointWrite},
	"work.completed":               {principal.RoleCheckpointWrite},
	"work.abandoned":               {principal.RoleCheckpointWrite},
	"checkpoint.recorded":          {principal.RoleCheckpointWrite},
	"evidence.recorded":            {principal.RoleCheckpointWrite, principal.RoleWorkRead},
	"session.started":              {principal.RoleCheckpointWrite, principal.RoleWorkRead},
	"session.resumed":              {principal.RoleCheckpointWrite, principal.RoleWorkRead},
	"session.branched":             {principal.RoleCheckpointWrite},
	"session.compacted":            {principal.RoleCheckpointWrite},
	"session.ended":                {principal.RoleCheckpointWrite, principal.RoleWorkRead},
	"fact.candidate_proposed":      {principal.RoleCandidateWrite},
	"fact.promoted":                {principal.RoleFactPromote},
	"fact.superseded":              {principal.RoleFactPromote},
	"fact.retracted":               {principal.RoleFactPromote},
	"fact.conflict_flagged":        {principal.RoleFactPromote},
	"tombstone.issued":             {principal.RoleAdmin},
	"projection.rebuild_requested": {principal.RoleAdmin},
	"writer_epoch.opened":          {principal.RoleAdmin},
	"writer_epoch.fenced":          {principal.RoleAdmin},
}

var validSensitivity = map[string]bool{
	"public": true, "internal": true, "confidential": true, "restricted": true,
}

// Submitter is the durable write path.
type Submitter struct {
	pool  *pgstore.Pool
	auth  *principal.Store
	scan  func(string) bool
	clock func() time.Time
}

// NewSubmitter returns a submitter over the given pool. The credential scanner
// is pkg/core's, the same one the live write guard uses, so the ledger and the
// RAG ingress cannot disagree about what a secret looks like.
func NewSubmitter(pool *pgstore.Pool) *Submitter {
	return &Submitter{
		pool:  pool,
		auth:  principal.NewStore(pool),
		scan:  core.ContainsCredential,
		clock: time.Now,
	}
}

// Submit runs the durable write path and returns the receipt.
//
// A duplicate is not an error: it returns the original receipt with state
// duplicate, because the caller's write did happen -- once. Everything else
// that is refused returns an *Error carrying a frozen class.
func (s *Submitter) Submit(ctx context.Context, e Event) (Receipt, error) {
	digest, canonical, err := s.validate(e)
	if err != nil {
		return Receipt{}, err
	}

	scope := principal.Scope{
		ProjectID:   e.ProjectID,
		WorkspaceID: &e.WorkspaceID,
		WorkID:      e.WorkID,
		SessionID:   &e.SessionID,
		BranchID:    &e.BranchID,
	}
	roles, ok := requiredRoles[e.Type]
	if !ok {
		return Receipt{}, classErr(ClassPolicyRejected, "event type %q has no writer role and cannot be submitted", e.Type)
	}
	who, err := s.auth.AuthorizeAny(ctx, e.PrincipalID, scope, roles)
	if err != nil {
		if errors.Is(err, principal.ErrScopeDenied) {
			return Receipt{}, classErr(ClassScopeDenied, "principal may not submit %s in the requested scope", e.Type)
		}
		return Receipt{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, s.pool.QueryTimeout())
	defer cancel()

	tx, err := s.pool.Pgx().Begin(ctx)
	if err != nil {
		return Receipt{}, fmt.Errorf("memgw: begin: %w", err)
	}
	// Rollback on every path that is not an explicit commit. After a successful
	// commit this is a no-op, so a single deferred call covers every early
	// return without the caller having to be careful.
	defer func() { _ = tx.Rollback(ctx) }()

	// Idempotency first: a duplicate must not pay for authorization work twice,
	// and more importantly must not be able to fail for a different reason the
	// second time.
	if r, found, err := lookupReceipt(ctx, tx, e.PrincipalID, e.IdempotencyKey); err != nil {
		return Receipt{}, err
	} else if found {
		if r.PayloadDigest != digest {
			return Receipt{}, classErr(ClassPayloadMismatch,
				"idempotency key was already used for a different payload")
		}
		r.State = StateDuplicate
		return r, nil
	}

	if err := checkWriterEpoch(ctx, tx, e.WriterEpoch); err != nil {
		return Receipt{}, err
	}

	// The domain decides what this event advances and whether the transition
	// it asks for is legal, and creates the rows the event must be able to
	// point at. It runs inside this transaction, so everything it does is
	// undone with the rest if anything below fails.
	dev := domainEvent(e, who, canonical)
	agg, revision, err := domain.Prepare(ctx, tx, dev, func(ctx context.Context, agg domain.Aggregate) (*int64, error) {
		return resolveRevision(ctx, tx, agg, e.ExpectedRevision)
	})
	if err != nil {
		return Receipt{}, err
	}

	var seq int64
	var recordedAt time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO events (event_id, idempotency_key, principal_id, project_id, workspace_id, work_id,
		                    session_id, branch_id, event_type, payload, payload_digest, expected_revision,
		                    evidence_refs, sensitivity_class, policy_version, schema_version, occurred_at,
		                    writer_epoch, aggregate_type, aggregate_id, revision)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
		RETURNING seq, recorded_at`,
		e.EventID, e.IdempotencyKey, e.PrincipalID, e.ProjectID, e.WorkspaceID, e.WorkID,
		e.SessionID, e.BranchID, e.Type, canonical, digest, e.ExpectedRevision,
		evidenceRefs(e.EvidenceRefs), e.SensitivityClass, e.PolicyVersion, e.SchemaVersion,
		e.OccurredAt, e.WriterEpoch, nullString(agg.Type), aggregateID(agg), revision,
	).Scan(&seq, &recordedAt)
	if err != nil {
		return Receipt{}, translateInsertError(err)
	}

	// Materialise after the event exists, so the readable state and the event
	// that explains it are the same commit.
	if err := domain.Materialise(ctx, tx, dev, seq, revision); err != nil {
		return Receipt{}, translateInsertError(err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO write_receipts (principal_id, idempotency_key, event_id, payload_digest, state, seq)
		VALUES ($1,$2,$3,$4,'committed',$5)`,
		e.PrincipalID, e.IdempotencyKey, e.EventID, digest, seq); err != nil {
		return Receipt{}, fmt.Errorf("memgw: write receipt: %w", err)
	}

	// The outbox row is part of the same transaction. An event that committed
	// without its outbox row would be invisible to every projection forever,
	// which is the failure mode an outbox exists to prevent.
	if _, err := tx.Exec(ctx, `
		INSERT INTO projection_outbox (event_id, event_seq, projection) VALUES ($1,$2,'qdrant')`,
		e.EventID, seq); err != nil {
		return Receipt{}, fmt.Errorf("memgw: enqueue projection: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		// The caller is now in unknown_commit_status: the commit may or may not
		// have landed. Resolution is a receipt lookup, never a new key.
		return Receipt{}, classErr(ClassUnknownCommitStatus,
			"commit outcome is unknown; look up the receipt for this idempotency key before retrying")
	}

	return Receipt{
		PrincipalID:    e.PrincipalID,
		IdempotencyKey: e.IdempotencyKey,
		EventID:        e.EventID,
		PayloadDigest:  digest,
		State:          StateCommitted,
		Seq:            seq,
		Revision:       revision,
		RecordedAt:     recordedAt,
	}, nil
}

// LookupReceipt is how a client in unknown_commit_status finds out what
// happened. A principal can only see its own receipts.
func (s *Submitter) LookupReceipt(ctx context.Context, principalID uuid.UUID, key string) (Receipt, bool, error) {
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()
	return lookupReceipt(ctx, s.pool.Pgx(), principalID, key)
}

// querier is the subset of pgx both a pool and a transaction satisfy, so the
// same lookup serves the in-transaction dedupe check and the public API.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func lookupReceipt(ctx context.Context, q querier, principalID uuid.UUID, key string) (Receipt, bool, error) {
	var r Receipt
	var eventID *uuid.UUID
	var seq *int64
	err := q.QueryRow(ctx, `
		SELECT event_id, payload_digest, state, seq, recorded_at
		FROM write_receipts WHERE principal_id = $1 AND idempotency_key = $2`,
		principalID, key,
	).Scan(&eventID, &r.PayloadDigest, &r.State, &seq, &r.RecordedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, fmt.Errorf("memgw: receipt lookup: %w", err)
	}
	r.PrincipalID = principalID
	r.IdempotencyKey = key
	if eventID != nil {
		r.EventID = *eventID
	}
	if seq != nil {
		r.Seq = *seq
	}
	return r, true, nil
}

// checkWriterEpoch refuses a write stamped with an epoch that has been fenced.
// The check is server-side and does not depend on the writer noticing anything.
func checkWriterEpoch(ctx context.Context, tx pgx.Tx, epoch int64) error {
	var fencedAt *time.Time
	err := tx.QueryRow(ctx, `SELECT fenced_at FROM writer_epochs WHERE epoch = $1`, epoch).Scan(&fencedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return classErr(ClassWriterEpochFenced, "writer epoch %d is not admitted", epoch)
	}
	if err != nil {
		return fmt.Errorf("memgw: read writer epoch: %w", err)
	}
	if fencedAt != nil {
		return classErr(ClassWriterEpochFenced, "writer epoch %d was fenced", epoch)
	}
	return nil
}

// domainEvent projects a submission onto what the domain needs, adding the
// facts about the principal that the submission is not allowed to assert about
// itself: its type, its parent and its host.
func domainEvent(e Event, who principal.Principal, canonical []byte) domain.Event {
	return domain.Event{
		EventID:           e.EventID,
		PrincipalID:       e.PrincipalID,
		PrincipalType:     string(who.Type),
		ParentPrincipalID: who.ParentID,
		HostID:            who.HostID,
		ProjectID:         e.ProjectID,
		WorkspaceID:       e.WorkspaceID,
		WorkID:            e.WorkID,
		SessionID:         e.SessionID,
		BranchID:          e.BranchID,
		Type:              e.Type,
		Payload:           canonical,
		ExpectedRevision:  e.ExpectedRevision,
		EvidenceRefs:      e.EvidenceRefs,
		SensitivityClass:  e.SensitivityClass,
		OccurredAt:        e.OccurredAt,
	}
}

func aggregateID(agg domain.Aggregate) *uuid.UUID {
	if agg.Type == "" {
		return nil
	}
	id := agg.ID
	return &id
}

// resolveRevision applies compare-and-set to the aggregate the domain derived.
//
// The current revision is read from the event log itself, so no separate state
// table has to exist or be kept in step with it, and the unique partial index
// on (aggregate_type, aggregate_id, revision) is what decides a race between
// two writers that both computed the same next revision.
func resolveRevision(ctx context.Context, tx pgx.Tx, agg domain.Aggregate, expected *int64) (*int64, error) {
	if !agg.Mutating {
		return nil, nil
	}
	var current int64
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(revision), 0) FROM events
		WHERE aggregate_type = $1 AND aggregate_id = $2`, agg.Type, agg.ID).Scan(&current)
	if err != nil {
		return nil, fmt.Errorf("memgw: read aggregate revision: %w", err)
	}
	if expected == nil {
		// Creating something nobody else has touched needs no expectation to
		// state. Advancing something that exists does: a writer that omits it
		// is a writer that did not look.
		if current != 0 {
			return nil, classErr(ClassPolicyRejected,
				"%s advances an aggregate that is already at revision %d and must carry expected_revision",
				agg.Type, current)
		}
	} else {
		switch {
		case *expected < current:
			return nil, classErr(ClassStaleRevision,
				"expected revision %d is behind the current revision %d", *expected, current)
		case *expected > current:
			return nil, classErr(ClassCASConflict,
				"expected revision %d is ahead of the current revision %d", *expected, current)
		}
	}
	next := current + 1
	return &next, nil
}

// translateInsertError maps PostgreSQL constraint violations onto frozen error
// classes. Two writers that both computed the same next revision race on the
// unique aggregate index, and the loser is a CAS conflict -- which is why the
// index is unique rather than a lock being taken.
func translateInsertError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return fmt.Errorf("memgw: append event: %w", err)
	}
	switch {
	case strings.Contains(pgErr.ConstraintName, "aggregate_revision"):
		return classErr(ClassCASConflict, "another writer committed this revision first")
	case strings.Contains(pgErr.ConstraintName, "idempotency"):
		return classErr(ClassDuplicate, "this idempotency key is already committed")
	case strings.Contains(pgErr.ConstraintName, "pkey"):
		return classErr(ClassPolicyRejected,
			"this event_id is already used by another write; a retry must reuse both the event_id and the idempotency key")
	default:
		return fmt.Errorf("memgw: append event: %w", err)
	}
}

// validate enforces the frozen envelope. It runs before anything touches the
// database, and its errors never quote the payload.
func (s *Submitter) validate(e Event) (string, []byte, error) {
	switch {
	case e.EventID == uuid.Nil:
		return "", nil, classErr(ClassPolicyRejected, "event_id is required and must be stable across retries")
	case e.IdempotencyKey == "":
		return "", nil, classErr(ClassPolicyRejected, "idempotency_key is required")
	case len(e.IdempotencyKey) > 128:
		return "", nil, classErr(ClassPolicyRejected, "idempotency_key exceeds 128 bytes")
	case e.PrincipalID == uuid.Nil:
		return "", nil, classErr(ClassScopeDenied, "no principal")
	case e.ProjectID == uuid.Nil || e.WorkspaceID == uuid.Nil || e.BranchID == uuid.Nil:
		return "", nil, classErr(ClassPolicyRejected, "project_id, workspace_id and branch_id are required")
	case e.SessionID == "" || len(e.SessionID) > 128:
		return "", nil, classErr(ClassPolicyRejected, "session_id is required and bounded to 128 bytes")
	case !validSensitivity[e.SensitivityClass]:
		return "", nil, classErr(ClassPolicyRejected, "sensitivity_class is missing or unknown")
	case e.SchemaVersion != ContractSchemaVersion:
		return "", nil, classErr(ClassPolicyRejected, "schema_version %q is not this contract version", e.SchemaVersion)
	case e.PolicyVersion != ContractPolicyVersion:
		return "", nil, classErr(ClassPolicyRejected, "policy_version %q is not this contract version", e.PolicyVersion)
	case e.OccurredAt.IsZero():
		return "", nil, classErr(ClassPolicyRejected, "occurred_at is required")
	case len(e.EvidenceRefs) > 64:
		return "", nil, classErr(ClassPolicyRejected, "evidence_refs exceeds 64 entries")
	case e.WriterEpoch <= 0:
		return "", nil, classErr(ClassWriterEpochFenced, "no writer epoch was stamped")
	case e.Payload == nil:
		return "", nil, classErr(ClassPolicyRejected, "payload is required")
	}

	digest, canonical, err := PayloadDigest(e.Payload)
	if err != nil {
		return "", nil, classErr(ClassPolicyRejected, "payload could not be canonicalised")
	}
	if len(canonical) > MaxPayloadBytes {
		return "", nil, classErr(ClassPayloadTooLarge,
			"payload is %d bytes, over the %d byte bound", len(canonical), MaxPayloadBytes)
	}
	// The credential scan runs on the canonical bytes, so a secret cannot hide
	// in a field name, a nested object or an array the caller hoped nobody
	// would look at. The rejection names no content.
	if s.scan(string(canonical)) {
		return "", nil, classErr(ClassPolicyRejected,
			"payload matches a credential shape and was refused before it was written or sent anywhere")
	}
	return digest, canonical, nil
}

func evidenceRefs(refs []uuid.UUID) []uuid.UUID {
	if refs == nil {
		return []uuid.UUID{}
	}
	return refs
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// MarshalPayload is a convenience for callers holding raw JSON: it decodes into
// the generic form Submit canonicalises, so the digest rule stays in one place.
func MarshalPayload(raw []byte) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("memgw: payload is not valid JSON: %w", err)
	}
	return v, nil
}
