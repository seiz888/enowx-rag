package adapter

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Lifecycle is what a hook means, once the host's vocabulary is removed.
//
// The set is closed and has no default. A host event this build does not
// recognise becomes Ignored and is reported as such, never guessed into the
// nearest-looking member: an unrecognised compaction silently filed as a
// session end would corrupt the history of every session on that host.
type Lifecycle string

const (
	// SessionStart is a session beginning with no prior state.
	SessionStart Lifecycle = "session_start"
	// SessionResume is a session continuing state it already had.
	SessionResume Lifecycle = "session_resume"
	// SessionBranch is a session diverging from another at a known point.
	SessionBranch Lifecycle = "session_branch"
	// SessionCompact is context being discarded or summarised.
	SessionCompact Lifecycle = "session_compact"
	// SessionEnd is a session stopping, cleanly or otherwise.
	SessionEnd Lifecycle = "session_end"
	// Checkpoint is a handover note the host supplied in full. It is never
	// derived from a lifecycle moment; see the package comment.
	Checkpoint Lifecycle = "checkpoint"
	// Ignored is a host event that is real but carries no canonical meaning --
	// a tool call, a prompt submission. It is a successful outcome.
	Ignored Lifecycle = "ignored"
)

// eventType maps a lifecycle to the frozen contract's event type.
var eventType = map[Lifecycle]string{
	SessionStart:   "session.started",
	SessionResume:  "session.resumed",
	SessionBranch:  "session.branched",
	SessionCompact: "session.compacted",
	SessionEnd:     "session.ended",
	Checkpoint:     "checkpoint.recorded",
}

// Hook is one lifecycle moment, normalised.
//
// Every field is either something the host stated or something derived from
// what the host stated. Nothing here is read from the clock or from the
// environment, because everything here feeds the idempotency key.
type Hook struct {
	// Host is which agent host produced this, as named in Hosts.
	Host string `json:"host"`
	// Native is the host's own name for the event, kept verbatim so a
	// quarantined submission can be traced back to the hook that made it.
	Native string `json:"native_event,omitempty"`
	// Lifecycle is the canonical meaning.
	Lifecycle Lifecycle `json:"event"`
	// SessionID is the host's session identifier. It is used as-is: a
	// rewritten session id would break the join between the ledger and the
	// host's own transcript.
	SessionID string `json:"session_id"`
	// ParentSessionID is set for a resume or a branch.
	ParentSessionID string `json:"parent_session_id,omitempty"`
	// BranchPointSeq is where a branch diverged. A branch without one is
	// refused rather than recorded as a fresh session.
	BranchPointSeq *int64 `json:"branch_point_seq,omitempty"`
	// ParentBranchID is the branch a branch came from.
	ParentBranchID *uuid.UUID `json:"parent_branch_id,omitempty"`
	// ReasonClass is a bounded classification, never free text from a user.
	ReasonClass string `json:"reason_class,omitempty"`
	// Cwd is the working directory the host reported. It is used to choose a
	// workspace and is never sent as payload content.
	Cwd string `json:"cwd,omitempty"`

	// Discriminator holds host-supplied values that distinguish one occurrence
	// of this event from the next. It is part of the idempotency key, so a
	// value that changes between two attempts at the same moment -- a
	// timestamp, a pid -- must never be put here. Decoders are responsible for
	// that, and validateDiscriminator enforces the shape.
	Discriminator map[string]string `json:"discriminator,omitempty"`

	// Checkpoint is the handover note, present only for the Checkpoint
	// lifecycle.
	Checkpoint *CheckpointInput `json:"checkpoint,omitempty"`
}

// CheckpointInput is a checkpoint the host handed over in full.
//
// WorkID and ExpectedRevision are required and are not defaulted. A checkpoint
// with no work is a note about nothing; a checkpoint with no expected revision
// is a write that cannot lose a race it should lose.
type CheckpointInput struct {
	WorkID           uuid.UUID `json:"work_id"`
	ExpectedRevision int64     `json:"expected_revision"`
	Objective        string    `json:"objective"`
	CompletedWork    string    `json:"completed_work"`
	PendingActions   string    `json:"pending_actions"`
	Blockers         string    `json:"blockers"`
	NextSafeAction   string    `json:"next_safe_action"`
	ModifiedFiles    []string  `json:"modified_files,omitempty"`
	// Provenance marks how each field was arrived at: "capture" (a model turn
	// authored it) or "observed" (read from evidence). Optional for backward
	// compatibility with checkpoints submitted by hosts that predate the mark,
	// but the producer sets it always.
	Provenance string `json:"provenance,omitempty"`
}

// Hosts is every host this build can decode. The list is closed: --host with
// anything else is refused, so a typo in a settings file fails at the first
// hook instead of quietly writing events attributed to a host that does not
// exist.
var Hosts = []string{"omp", "opencode", "claude", "codex", "droid", "hermes"}

// KnownHost reports whether name is a host this build decodes.
func KnownHost(name string) bool {
	for _, h := range Hosts {
		if h == name {
			return true
		}
	}
	return false
}

// Bounds mirrored from the frozen contract. They are checked here so an
// oversized session id is refused before it reaches a queue, not after.
const (
	maxSessionIDBytes  = 128
	maxTextFieldBytes  = 4096
	maxModifiedFiles   = 200
	maxDiscriminators  = 8
	maxDiscriminatorSz = 256
)

// Validate checks a hook is self-consistent before anything is derived from it.
func (h *Hook) Validate() error {
	if !KnownHost(h.Host) {
		return fmt.Errorf("adapter: unknown host %q", h.Host)
	}
	if h.Lifecycle == Ignored {
		return nil
	}
	if _, ok := eventType[h.Lifecycle]; !ok {
		return fmt.Errorf("adapter: unknown lifecycle %q", h.Lifecycle)
	}
	if strings.TrimSpace(h.SessionID) == "" {
		return fmt.Errorf("adapter: %s carries no session id", h.Lifecycle)
	}
	if len(h.SessionID) > maxSessionIDBytes {
		return fmt.Errorf("adapter: the session id is %d bytes, over the contract's %d",
			len(h.SessionID), maxSessionIDBytes)
	}
	if h.Lifecycle == SessionBranch {
		// The domain refuses a branch with no divergence point, and refusing it
		// here as well means the adapter fails at the hook rather than filling
		// a spool with events that will be rejected one at a time.
		if h.ParentSessionID == "" || h.BranchPointSeq == nil {
			return fmt.Errorf("adapter: a branch must name the session it left and the point it left at")
		}
	}
	if h.Lifecycle == Checkpoint {
		if h.Checkpoint == nil {
			return fmt.Errorf("adapter: a checkpoint event carries no checkpoint")
		}
		if err := h.Checkpoint.validate(); err != nil {
			return err
		}
	} else if h.Checkpoint != nil {
		return fmt.Errorf("adapter: %s must not carry a checkpoint", h.Lifecycle)
	}
	return validateDiscriminator(h.Discriminator)
}

func (c *CheckpointInput) validate() error {
	if c.WorkID == uuid.Nil {
		return fmt.Errorf("adapter: a checkpoint must name the work it belongs to")
	}
	if c.ExpectedRevision < 0 {
		return fmt.Errorf("adapter: expected_revision may not be negative")
	}
	if strings.TrimSpace(c.Objective) == "" || strings.TrimSpace(c.NextSafeAction) == "" {
		// These two are what a later session reads first. A checkpoint missing
		// either is a note nobody can resume from, and accepting it would make
		// the store look fuller than it is.
		return fmt.Errorf("adapter: a checkpoint needs an objective and a next safe action")
	}
	for _, f := range []struct {
		name  string
		value string
	}{
		{"objective", c.Objective},
		{"completed_work", c.CompletedWork},
		{"pending_actions", c.PendingActions},
		{"blockers", c.Blockers},
		{"next_safe_action", c.NextSafeAction},
	} {
		if len(f.value) > maxTextFieldBytes {
			return fmt.Errorf("adapter: checkpoint %s is %d bytes, over the contract's %d",
				f.name, len(f.value), maxTextFieldBytes)
		}
	}
	if len(c.ModifiedFiles) > maxModifiedFiles {
		return fmt.Errorf("adapter: the checkpoint lists %d modified files, over the contract's %d",
			len(c.ModifiedFiles), maxModifiedFiles)
	}
	return nil
}

// validateDiscriminator bounds what a host may push into the idempotency key.
//
// The key is a hash, so a large discriminator costs nothing to hash -- but it
// is also the only host-controlled input to an identifier the ledger trusts,
// and an unbounded map is an unbounded surface. Small and few is enough for
// every real discriminator: an occurrence number, a trigger, a digest.
func validateDiscriminator(d map[string]string) error {
	if len(d) > maxDiscriminators {
		return fmt.Errorf("adapter: %d discriminator fields, at most %d are accepted", len(d), maxDiscriminators)
	}
	for k, v := range d {
		if k == "" {
			return fmt.Errorf("adapter: a discriminator field has no name")
		}
		if len(k)+len(v) > maxDiscriminatorSz {
			return fmt.Errorf("adapter: discriminator %q is longer than %d bytes", k, maxDiscriminatorSz)
		}
	}
	return nil
}

// sortedDiscriminator renders the map in a fixed order. Go map iteration is
// randomised per process, so hashing the map directly would produce a different
// key in every process -- exactly the failure this package exists to prevent.
func sortedDiscriminator(d map[string]string) string {
	if len(d) == 0 {
		return ""
	}
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\x1f')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(d[k])
	}
	return b.String()
}

// DecodeCanonical reads the shape the fragments we author emit.
//
// Unknown fields are refused. A plugin that renames a field in a later version
// then fails loudly on the first hook, instead of silently sending events with
// no session id.
func DecodeCanonical(raw []byte) (Hook, error) {
	var h Hook
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil {
		return Hook{}, fmt.Errorf("adapter: the hook payload is not a canonical hook: %w", err)
	}
	return h, nil
}

// occurredAt is the envelope timestamp. It is deliberately not part of any
// derived identifier, which is why it may be read from the clock at all.
func occurredAt() time.Time { return time.Now().UTC() }
