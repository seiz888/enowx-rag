package adapter

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// PolicyVersion and SchemaVersion are the frozen contract's versions. They are
// constants rather than configuration: an adapter that could claim a schema
// version it was not built against would let a stale host submit events the
// gateway would then validate against the wrong rules.
const (
	PolicyVersion = "1.0.0"
	SchemaVersion = "1.0.0"
)

// branchNamespace derives a branch id from a session id.
//
// Deriving rather than generating is what lets a hook be stateless. Every event
// in one session lands on one branch without the adapter having to remember
// anything between two invocations that may be minutes and one reboot apart,
// and a re-fired hook derives the same branch instead of opening a second one.
var branchNamespace = uuid.MustParse("a1b6cb26-6c48-4a15-9c1a-1a2f0b8f0f03")

// BranchID is the branch a host's session writes on.
func BranchID(host, sessionID string) uuid.UUID {
	return uuid.NewSHA1(branchNamespace, []byte("memgw-branch\x00"+host+"\x00"+sessionID))
}

// Submission is the body an adapter sends.
//
// It deliberately has no event_id and no principal_id.
//
// The event id is derived from the idempotency key by whatever accepts the
// submission, so a client that restarts cannot invent a new id for an event it
// already sent. The principal is whatever the credential proved; a body that
// could name its own principal would let any host with a token write as any
// other host.
type Submission struct {
	IdempotencyKey   string          `json:"idempotency_key"`
	ProjectID        uuid.UUID       `json:"project_id"`
	WorkspaceID      uuid.UUID       `json:"workspace_id"`
	WorkID           *uuid.UUID      `json:"work_id,omitempty"`
	SessionID        string          `json:"session_id"`
	BranchID         uuid.UUID       `json:"branch_id"`
	Type             string          `json:"type"`
	Payload          json.RawMessage `json:"payload"`
	ExpectedRevision *int64          `json:"expected_revision,omitempty"`
	SensitivityClass string          `json:"sensitivity_class"`
	PolicyVersion    string          `json:"policy_version"`
	SchemaVersion    string          `json:"schema_version"`
	OccurredAt       string          `json:"occurred_at"`
	WriterEpoch      int64           `json:"writer_epoch"`
}

// Translate builds the submission for a hook.
//
// It returns ok=false for a hook with no canonical meaning. That is a success:
// a tool call is a real thing that happened and is deliberately not in the
// ledger, and reporting it as an error would make every hook on a busy host
// look like a failure.
func Translate(h Hook, cfg Config) (Submission, bool, error) {
	if h.Lifecycle == Ignored {
		return Submission{}, false, nil
	}
	typ, ok := eventType[h.Lifecycle]
	if !ok {
		return Submission{}, false, fmt.Errorf("adapter: %q has no event type", h.Lifecycle)
	}

	var payload any
	var workID *uuid.UUID
	var expected *int64

	if h.Lifecycle == Checkpoint {
		c := h.Checkpoint
		workID = &c.WorkID
		rev := c.ExpectedRevision
		expected = &rev
		files := c.ModifiedFiles
		if files == nil {
			// An explicit empty list, not null: the payload digest is computed
			// over the encoded payload, and null and [] encode differently, so
			// a checkpoint re-sent by a client that normalised one into the
			// other would be refused as a payload mismatch.
			files = []string{}
		}
		cpPayload := map[string]any{
			"objective":        c.Objective,
			"completed_work":   c.CompletedWork,
			"pending_actions":  c.PendingActions,
			"blockers":         c.Blockers,
			"next_safe_action": c.NextSafeAction,
			"modified_files":   files,
		}
		if c.Provenance != "" {
			// The provenance is part of the payload, so a capture-authored
			// checkpoint and an observed one never collapse to the same digest.
			cpPayload["provenance"] = c.Provenance
		}
		payload = cpPayload
	} else {
		p := map[string]any{}
		if h.ParentSessionID != "" {
			p["parent_session_id"] = h.ParentSessionID
		}
		if h.Lifecycle == SessionBranch {
			parent := h.ParentBranchID
			if parent == nil {
				// The parent branch is derived from the parent session for the
				// same reason the branch itself is: the host knows which
				// session it left, not which uuid we gave that session's
				// branch.
				derived := BranchID(h.Host, h.ParentSessionID)
				parent = &derived
			}
			p["parent_branch_id"] = parent.String()
			p["branch_point_seq"] = *h.BranchPointSeq
		}
		if h.ReasonClass != "" {
			p["reason_class"] = h.ReasonClass
		}
		payload = p
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return Submission{}, false, fmt.Errorf("adapter: the payload could not be encoded: %w", err)
	}

	return Submission{
		IdempotencyKey:   Key(h),
		ProjectID:        cfg.ProjectID,
		WorkspaceID:      cfg.WorkspaceID,
		WorkID:           workID,
		SessionID:        h.SessionID,
		BranchID:         BranchID(h.Host, h.SessionID),
		Type:             typ,
		Payload:          raw,
		ExpectedRevision: expected,
		SensitivityClass: cfg.Sensitivity,
		PolicyVersion:    PolicyVersion,
		SchemaVersion:    SchemaVersion,
		OccurredAt:       occurredAt().Format("2006-01-02T15:04:05.000000000Z07:00"),
		WriterEpoch:      cfg.WriterEpoch,
	}, true, nil
}
