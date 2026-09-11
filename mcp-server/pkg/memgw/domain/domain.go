// Package domain is the canonical meaning of an event: which aggregate it
// advances, whether the transition it asks for is legal, and what materialised
// state it leaves behind.
//
// It runs inside the ledger's commit transaction, never beside it. That is the
// whole point: the event, the aggregate update, the receipt and the outbox row
// are one atomic act, so a work can never be in a state no event explains, and
// an event can never exist that the work never reflected.
//
// The layering rule: ledger imports domain, domain never imports ledger.
// Classified refusals therefore come from pkg/memgw/errclass, which both use.
package domain

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/errclass"
)

// Event is the subset of a submission the domain needs. The ledger builds it
// after validating the envelope and after loading the principal, so everything
// here is already known to be well-formed and authenticated.
type Event struct {
	EventID     uuid.UUID
	PrincipalID uuid.UUID
	// PrincipalType and ParentPrincipalID come from the principals table, not
	// from the payload. A submission that could declare its own type could
	// declare itself not to be a subagent.
	PrincipalType     string
	ParentPrincipalID *uuid.UUID
	HostID            uuid.UUID

	ProjectID   uuid.UUID
	WorkspaceID uuid.UUID
	WorkID      *uuid.UUID
	SessionID   string
	BranchID    uuid.UUID

	Type             string
	Payload          []byte // canonical JSON, as digested
	ExpectedRevision *int64
	EvidenceRefs     []uuid.UUID
	SensitivityClass string
	OccurredAt       time.Time
}

// Aggregate names what an event advances.
//
// A mutating event is one that changes a thing other writers may also be
// changing, and it is the only kind that needs compare-and-set. Recording
// evidence or the start of a session is additive: it says something happened,
// not that the work moved.
type Aggregate struct {
	Type     string
	ID       uuid.UUID
	Mutating bool
}

const (
	// AggregateWork is a unit of work: its state and its checkpoints.
	AggregateWork = "work"
	// AggregateFactSlot is one (project, subject, predicate). The contested
	// thing about a single-valued fact is the predicate, not any one value of
	// it, so that is what compare-and-set operates on.
	AggregateFactSlot = "fact_slot"
)

// slotNamespace makes slot ids deterministic, so two hosts that promote into
// the same predicate contend on the same aggregate without having to agree on
// an id first.
var slotNamespace = uuid.MustParse("6d656d67-772d-4f61-b374-736c6f740001")

// SlotID derives the fact-slot id for a predicate.
func SlotID(projectID uuid.UUID, subject, predicate string) uuid.UUID {
	return uuid.NewSHA1(slotNamespace, []byte(projectID.String()+"\x00"+subject+"\x00"+predicate))
}

// CAS is the ledger's compare-and-set, handed to the domain so it can be
// applied the moment the aggregate is known and before the event's meaning is
// judged.
//
// The ordering is deliberate. expected_revision is the writer's statement of
// what it saw; if that is already wrong, every later judgement -- "this
// transition is illegal", "that value already stands" -- would be answering a
// question about a state the writer never read. A host flushing a buffer after
// hours offline must hear stale_revision, not a verdict on a transition it
// never proposed against this state.
//
// It returns nil for a non-mutating aggregate, which needs no revision.
type CAS func(ctx context.Context, agg Aggregate) (*int64, error)

// workMutating lists the event types that advance a work aggregate.
var workMutating = map[string]bool{
	"work.planned": true, "work.activated": true, "work.blocked": true,
	"work.unblocked": true, "work.review_requested": true, "work.completed": true,
	"work.abandoned": true, "checkpoint.recorded": true,
}

// Transitions is the work state machine. A transition absent from this table
// does not happen: there is no default, so adding a state without deciding what
// may reach it fails closed.
//
// completed and abandoned are terminal. Reopening finished work is a new work
// that cites the old one, not a state change -- otherwise "completed" would
// mean "completed for now", and a checkpoint referring to it would be
// ambiguous forever.
var Transitions = map[string][]string{
	"planned":   {"active", "abandoned"},
	"active":    {"blocked", "review", "completed", "abandoned"},
	"blocked":   {"active", "abandoned"},
	"review":    {"active", "completed", "abandoned"},
	"completed": {},
	"abandoned": {},
}

// targetState maps a work event to the state it asks for. checkpoint.recorded
// is absent on purpose: a checkpoint advances the revision without changing
// the state, which is what makes it safe to take one at any moment.
var targetState = map[string]string{
	"work.planned":          "planned",
	"work.activated":        "active",
	"work.blocked":          "blocked",
	"work.unblocked":        "active",
	"work.review_requested": "review",
	"work.completed":        "completed",
	"work.abandoned":        "abandoned",
}

// canTransition reports whether from -> to is allowed.
func canTransition(from, to string) bool {
	for _, s := range Transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// subagentForbidden lists what a subagent may never submit. A subagent's
// conclusion is input to the parent, never a state transition -- INV-16. The
// check is on the principal type as recorded in the principals table, so a
// subagent cannot escape it by omitting a field.
var subagentForbidden = map[string]bool{
	"work.planned": true, "work.activated": true, "work.blocked": true,
	"work.unblocked": true, "work.review_requested": true, "work.completed": true,
	"work.abandoned": true, "checkpoint.recorded": true,
	"fact.promoted": true, "fact.superseded": true, "fact.retracted": true,
	"tombstone.issued": true, "projection.rebuild_requested": true,
	"writer_epoch.opened": true, "writer_epoch.fenced": true,
}

// ---------------------------------------------------------------------------
// Payloads
// ---------------------------------------------------------------------------

// WorkPayload is the payload of a work.* event.
type WorkPayload struct {
	Title       string `json:"title"`
	ReasonClass string `json:"reason_class"`
}

// CheckpointPayload is the payload of checkpoint.recorded. Every text field is
// bounded by the frozen contract; a checkpoint is a handover note, not a
// transcript.
type CheckpointPayload struct {
	Objective      string   `json:"objective"`
	CompletedWork  string   `json:"completed_work"`
	PendingActions string   `json:"pending_actions"`
	Blockers       any      `json:"blockers"`
	NextSafeAction string   `json:"next_safe_action"`
	ModifiedFiles  []string `json:"modified_files"`
}

// EvidencePayload is the payload of evidence.recorded.
type EvidencePayload struct {
	EvidenceType  string `json:"evidence_type"`
	EvidenceClass string `json:"evidence_class"`
	Locator       string `json:"locator"`
}

// SessionPayload is the payload of session.*.
type SessionPayload struct {
	ParentSessionID string     `json:"parent_session_id"`
	ParentBranchID  *uuid.UUID `json:"parent_branch_id"`
	BranchPointSeq  *int64     `json:"branch_point_seq"`
	ReasonClass     string     `json:"reason_class"`
}

// CandidatePayload is the payload of fact.candidate_proposed.
type CandidatePayload struct {
	CandidateID   uuid.UUID       `json:"candidate_id"`
	Subject       string          `json:"subject"`
	Predicate     string          `json:"predicate"`
	Object        json.RawMessage `json:"object"`
	Cardinality   string          `json:"cardinality"`
	Source        string          `json:"source"`
	Extractor     string          `json:"extractor"`
	Model         *string         `json:"model"`
	ModelVersion  *string         `json:"model_version"`
	Confidence    *float64        `json:"confidence"`
	EvidenceClass string          `json:"evidence_class"`
	ExpiresAt     *time.Time      `json:"expires_at"`
}

// PromotionPayload is the payload of fact.promoted. subject/predicate/object
// may be restated for readability but must agree with the candidate: a
// promotion that could redefine what it promotes would let extracted text
// authorise a different claim than the one that was reviewed.
type PromotionPayload struct {
	CandidateID     uuid.UUID       `json:"candidate_id"`
	FactID          *uuid.UUID      `json:"fact_id"`
	Subject         string          `json:"subject"`
	Predicate       string          `json:"predicate"`
	Object          json.RawMessage `json:"object"`
	Cardinality     string          `json:"cardinality"`
	ValidFrom       *time.Time      `json:"valid_from"`
	SupersedesFacts []uuid.UUID     `json:"supersedes_fact_ids"`
	EvidenceClass   string          `json:"evidence_class"`
}

// FactChangePayload is the payload of fact.superseded, fact.retracted and
// fact.conflict_flagged.
type FactChangePayload struct {
	FactID       *uuid.UUID `json:"fact_id"`
	SupersededBy *uuid.UUID `json:"superseded_by"`
	Subject      string     `json:"subject"`
	Predicate    string     `json:"predicate"`
	ReasonClass  string     `json:"reason_class"`
}

// TombstonePayload is the payload of tombstone.issued.
type TombstonePayload struct {
	SubjectType     string   `json:"subject_type"`
	SubjectID       string   `json:"subject_id"`
	ReasonClass     string   `json:"reason_class"`
	ErasureRequired bool     `json:"erasure_required"`
	LegacyChunkIDs  []string `json:"legacy_chunk_ids"`
}

// WriterEpochPayload is the payload of writer_epoch.opened and
// writer_epoch.fenced.
//
// Epoch is the epoch the event is about, which is not the same as the epoch the
// event is stamped with: an admin fences the epoch a departing host wrote
// under while writing under its own.
type WriterEpochPayload struct {
	Epoch       int64  `json:"epoch"`
	ReasonClass string `json:"reason_class"`
	// DrainWatermarkSeq records how far the fenced epoch's writes reach. Left
	// unset, the fencing event's own sequence is used, which is the correct
	// answer by construction: nothing stamped with the old epoch can commit
	// after the fence, so everything it ever wrote is at or below that point.
	DrainWatermarkSeq *int64 `json:"drain_watermark_seq,omitempty"`
}

// writerEpochReasons is the vocabulary a writer_epoch.* event may use. It is
// closed for the same reason every other reason_class is: an open string turns
// into free text, and then nothing can be counted or alerted on.
var writerEpochReasons = map[string]bool{
	"cutover":             true,
	"host_replaced":       true,
	"credential_rotation": true,
	"incident":            true,
	"maintenance":         true,
}

func decode(payload []byte, into any) error {
	if err := json.Unmarshal(payload, into); err != nil {
		// The payload is never echoed: a decode failure is exactly when a
		// naive message would quote the thing that failed to decode.
		return errclass.New(errclass.PolicyRejected, "payload does not match the shape this event type requires")
	}
	return nil
}

// blockersText renders the blockers field, which the fixtures write both as a
// list and as a string. Accepting both is not laxity: the frozen fixture set
// contains both shapes, and rejecting one of them would fail a contract the
// adapters were written against.
func blockersText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		out := ""
		for i, item := range t {
			if i > 0 {
				out += "; "
			}
			if s, ok := item.(string); ok {
				out += s
			}
		}
		return out
	default:
		return ""
	}
}
