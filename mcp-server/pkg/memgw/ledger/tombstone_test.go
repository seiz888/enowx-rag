// Tombstones as the deletion journal, and what "ordered before" has to mean
// when a rebuild, a retry and a live writer are all in flight at once.
//
// Not doing a DELETE is not a proof of anything. What is tested here is the
// ordering: the journal row records the sequence at which the deletion was
// ordered, so a replay that arrives later can decide whether what it is about
// to write was already deleted -- without consulting a clock that two hosts
// would disagree about.
package ledger_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/ledger"
)

func tombstoneEvent(t *testing.T, f fixture) ledger.Event {
	t.Helper()
	return f.Steps[0].Submit.event(t)
}

// TestFixtureTombstoneIssued is INV-12's write half: the tombstone is a row in
// a journal that is never deleted, it names the event and the sequence that
// ordered it, and it reaches the legacy chunks whose ids re-chunking changed.
func TestFixtureTombstoneIssued(t *testing.T) {
	f := loadFixture(t, "tombstone-rebuild")
	h := newHarness(t, f)
	e := tombstoneEvent(t, f)

	// The legacy chunks exist because they were written before the gateway did.
	for _, chunk := range []string{"chunk-0417", "chunk-0418"} {
		if _, err := h.pool.Pgx().Exec(h.ctx, `
			INSERT INTO legacy_chunk_map (chunk_id, project_id, rag_project, document_id, source_digest)
			VALUES ($1,$2,'memory','doc-0417',repeat('0',64))`, chunk, e.ProjectID); err != nil {
			t.Fatalf("seed legacy chunk: %v", err)
		}
	}

	r, err := h.sub.Submit(h.ctx, e)
	if err != nil {
		t.Fatalf("issue tombstone: %v", err)
	}

	var issuedEvent uuid.UUID
	var issuedSeq int64
	var erasureRequired bool
	var reason string
	var completed *string
	if err := h.pool.Pgx().QueryRow(h.ctx, `
		SELECT issued_event_id, issued_event_seq, erasure_required, reason_class, erasure_completed_at::text
		FROM tombstones WHERE subject_type = 'document' AND subject_id = 'doc-0417'`).
		Scan(&issuedEvent, &issuedSeq, &erasureRequired, &reason, &completed); err != nil {
		t.Fatalf("no journal row: %v", err)
	}
	if issuedEvent != r.EventID || issuedSeq != r.Seq {
		t.Fatalf("the journal row points at %s/%d, the event is %s/%d", issuedEvent, issuedSeq, r.EventID, r.Seq)
	}
	if !erasureRequired || reason != "sensitive_content" {
		t.Fatalf("journal row reads erasure=%v reason=%s", erasureRequired, reason)
	}
	// Erasure is a separate, explicitly scheduled step. Issuing a tombstone
	// does not overwrite bytes, and claiming it did would be the lie.
	if completed != nil {
		t.Fatal("issuing a tombstone must not mark erasure complete")
	}

	if n := h.scalar(
		`SELECT count(*) FROM legacy_chunk_map WHERE project_id = $1 AND tombstoned_at IS NOT NULL`,
		e.ProjectID); n != 2 {
		t.Fatalf("%d legacy chunks marked, want 2", n)
	}
}

// TestTombstoneJournalIsNotDeletable: the journal has to outlive the thing it
// deletes, or a rebuild would have nothing to consult. UPDATE stays allowed on
// purpose -- that is how a later erasure step stamps erasure_completed_at.
func TestTombstoneJournalIsNotDeletable(t *testing.T) {
	f := loadFixture(t, "tombstone-rebuild")
	h := newHarness(t, f)
	if _, err := h.sub.Submit(h.ctx, tombstoneEvent(t, f)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Pgx().Exec(h.ctx, `DELETE FROM tombstones`); err == nil {
		t.Fatal("the deletion journal must not be deletable")
	}
	if _, err := h.pool.Pgx().Exec(h.ctx, `UPDATE tombstones SET erasure_completed_at = now()`); err != nil {
		t.Fatalf("the erasure step must be able to stamp completion: %v", err)
	}
}

// TestTombstonedWorkTakesNoFurtherEvents is the resurrection race in its
// simplest form: a host that was mid-flight when the deletion was ordered
// flushes afterwards. The write is refused rather than quietly re-creating the
// subject, and the refusal is a policy class, not a foreign-key error.
func TestTombstonedWorkTakesNoFurtherEvents(t *testing.T) {
	f := loadFixture(t, "duplicate-retry")
	h := newHarness(t, f)
	e := f.Steps[0].Submit.event(t)
	h.seedRevision(e.PrincipalID, e.ProjectID, e.WorkspaceID, *e.WorkID, e.BranchID, e.SessionID, 7)

	// An admin tombstones the work. The fixture's checkpoint principal does not
	// hold admin, so the operator from the tombstone fixture is installed here.
	operator := h.installOperator(e.ProjectID, e.WorkspaceID)
	kill := e
	kill.EventID = uuid.New()
	kill.IdempotencyKey = "operator:tombstone:work"
	kill.PrincipalID = operator
	kill.SessionID = "operator:host-a:manual"
	kill.Type = "tombstone.issued"
	kill.ExpectedRevision = nil
	kill.Payload = map[string]any{
		"subject_type":     "work",
		"subject_id":       e.WorkID.String(),
		"reason_class":     "user_request",
		"erasure_required": false,
	}
	if _, err := h.sub.Submit(h.ctx, kill); err != nil {
		t.Fatalf("tombstone the work: %v", err)
	}

	late := e
	late.EventID = uuid.New()
	late.IdempotencyKey = "claude:sess-1:ckpt:0008-late"
	rev := int64(7)
	late.ExpectedRevision = &rev
	if _, err := h.sub.Submit(h.ctx, late); !ledger.IsClass(err, ledger.ClassPolicyRejected) {
		t.Fatalf("a late write to a tombstoned work: want policy_rejected, got %v", err)
	}
	if n := h.scalar(`SELECT count(*) FROM checkpoints WHERE work_id = $1`, *e.WorkID); n != 0 {
		t.Fatalf("a late write resurrected the work with %d checkpoints", n)
	}
	if n := h.scalar(`SELECT count(*) FROM works WHERE work_id = $1 AND tombstoned_at IS NOT NULL`, *e.WorkID); n != 1 {
		t.Fatal("the work is not marked tombstoned")
	}
}

// TestTombstoneRefusesUnknownClasses: reason_class is a class, never free text.
// A reason that quoted the offending content would re-introduce exactly what
// the tombstone removes.
func TestTombstoneRefusesUnknownClasses(t *testing.T) {
	f := loadFixture(t, "tombstone-rebuild")
	h := newHarness(t, f)
	base := tombstoneEvent(t, f)

	cases := map[string]map[string]any{
		"free-text reason": {
			"subject_type": "document", "subject_id": "doc-0417",
			"reason_class": "contained the password for the admin account",
		},
		"unknown subject": {
			"subject_type": "everything", "subject_id": "doc-0417", "reason_class": "user_request",
		},
		"no subject": {
			"subject_type": "document", "subject_id": "", "reason_class": "user_request",
		},
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			e := base
			e.EventID = uuid.New()
			e.IdempotencyKey = "tombstone:" + name
			e.Payload = payload
			if _, err := h.sub.Submit(h.ctx, e); !ledger.IsClass(err, ledger.ClassPolicyRejected) {
				t.Fatalf("want policy_rejected, got %v", err)
			}
		})
	}
	if n := h.scalar(`SELECT count(*) FROM tombstones`); n != 0 {
		t.Fatalf("a refused tombstone wrote %d journal rows", n)
	}
}
