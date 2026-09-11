package projection

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/rag"
)

// Record is a canonical event as the projection reads it.
//
// It is a read model with no methods that write anything, which is how the
// "projections never mutate canonical state" rule is expressed in types rather
// than in a comment somebody has to remember.
type Record struct {
	EventID     uuid.UUID
	Seq         int64
	Type        string
	ProjectID   uuid.UUID
	WorkspaceID uuid.UUID
	WorkID      *uuid.UUID
	SessionID   string
	BranchID    uuid.UUID
	PrincipalID uuid.UUID
	Sensitivity string
	Revision    *int64
	Payload     []byte
	OccurredAt  time.Time
}

// EventSource reads canonical events by id.
type EventSource interface {
	Event(ctx context.Context, id uuid.UUID) (Record, error)
}

// Deletions is the part of the deletion journal a projection needs: whether a
// subject has been ordered deleted, and which pre-gateway chunk ids a deletion
// in this project still has to reach.
type Deletions interface {
	IsTombstoned(ctx context.Context, subjectType, subjectID string) (bool, error)
	TombstonedChunkIDs(ctx context.Context, projectID uuid.UUID) ([]string, error)
}

// Index is the vector store as this package needs it. It is an interface for
// one reason only: the applier's decisions -- what to project, what to withhold,
// what a tombstone reaches -- are testable without a running Qdrant, and the
// Qdrant-specific part is small enough to see in one screen (qdrant.go).
type Index interface {
	// EnsureCollection creates the project's collection if it is absent.
	EnsureCollection(ctx context.Context, projectID string) error
	// Upsert writes documents by id. Writing the same id twice replaces.
	Upsert(ctx context.Context, projectID string, docs []rag.Document) error
	// Delete removes documents by id. Deleting an absent id is not an error.
	Delete(ctx context.Context, projectID string, docIDs []string) error
	// Lookup returns the document currently held under an id, if any.
	Lookup(ctx context.Context, projectID, docID string) (rag.PointInfo, bool, error)
	// DeleteMatching removes every document whose payload matches meta. It is
	// how a tombstone reaches a subject whose documents the caller cannot name.
	DeleteMatching(ctx context.Context, projectID string, meta map[string]string) (int, error)
}

// ErrEventGone is what an EventSource returns when the event is not there.
//
// It is separated from a transport failure because the two need opposite
// treatment: a missing event will still be missing on the next attempt, and
// retrying it five times before dead-lettering only delays the moment somebody
// notices the outbox and the log disagree.
var ErrEventGone = &Error{class: "event_missing", reason: "the event named by this outbox row is not in the ledger"}

// Error carries a short class and a reason that is safe to log.
//
// The class is what the outbox stores; the reason is never derived from the
// event payload, because a projection failure is exactly the moment a naive
// message copies the content that failed to project into the queue.
type Error struct {
	class  string
	reason string
}

func (e *Error) Error() string { return "memgw projection: " + e.class + ": " + e.reason }

// Class is read by the outbox worker, which records the class and never the text.
func (e *Error) Class() string { return e.class }

func failure(class, format string, args ...any) *Error {
	return &Error{class: class, reason: fmt.Sprintf(format, args...)}
}
