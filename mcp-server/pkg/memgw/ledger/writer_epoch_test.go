// Fencing through the event log rather than through a hand-written UPDATE.
//
// The epoch table is what refuses a stale writer, and until an admin can move
// it by submitting an event, a cutover is a manual SQL statement with no record
// of who ordered it. These tests are about that path: the event has to change
// the table, and the change has to be the one that refuses the host afterwards.
package ledger_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/ledger"
	"github.com/enowdev/enowx-rag/pkg/memgw/principal"
)

// admin is the operator principal the fencing fixture installs.
const fencingAdmin = "88888888-8888-4888-8888-888888888888"

// epochEvent builds an admin-submitted writer_epoch event on the fixture's
// project. It uses its own session so it never borrows the writer's line.
func (h *harness) epochEvent(t *testing.T, f fixture, typ string, payload any, epoch int64) ledger.Event {
	t.Helper()
	tmpl := f.Steps[0].Submit.event(t)
	admin := mustUUID(t, fencingAdmin)
	session := "operator:cutover"
	branch := uuid.New()
	// The fixture declares the operator's admin role but no step of its own, so
	// the harness has no scope to hang the grant on. A cutover is exactly the
	// case where the admin submits against a project it never wrote to.
	if _, err := h.auth.GrantScope(h.ctx, principal.Grant{
		PrincipalID: admin,
		Role:        principal.RoleAdmin,
		ProjectID:   tmpl.ProjectID,
		Provenance:  "writer epoch test",
	}); err != nil && !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("grant admin: %v", err)
	}
	h.ensureSession(session, tmpl.ProjectID, tmpl.WorkspaceID, admin)
	h.ensureBranch(branch, tmpl.ProjectID, session)
	return ledger.Event{
		EventID:          uuid.New(),
		IdempotencyKey:   "operator:cutover:" + uuid.NewString(),
		PrincipalID:      admin,
		ProjectID:        tmpl.ProjectID,
		WorkspaceID:      tmpl.WorkspaceID,
		SessionID:        session,
		BranchID:         branch,
		Type:             typ,
		Payload:          payload,
		SensitivityClass: "internal",
		PolicyVersion:    "1.0.0",
		SchemaVersion:    "1.0.0",
		OccurredAt:       time.Now().UTC(),
		WriterEpoch:      epoch,
	}
}

func (h *harness) epochRow(t *testing.T, epoch int64) (fenced *time.Time, reason *string, drain *int64) {
	t.Helper()
	if err := h.pool.Pgx().QueryRow(h.ctx,
		`SELECT fenced_at, reason_class, drain_watermark_seq FROM writer_epochs WHERE epoch = $1`, epoch).
		Scan(&fenced, &reason, &drain); err != nil {
		t.Fatalf("read epoch %d: %v", epoch, err)
	}
	return fenced, reason, drain
}

// TestFencingEventActuallyFencesTheEpoch is the whole point: a committed
// receipt for writer_epoch.fenced has to mean the next write under that epoch
// is refused. A receipt for a fence that changed nothing would be worse than an
// error, because the operator would believe the cutover happened.
func TestFencingEventActuallyFencesTheEpoch(t *testing.T) {
	f := loadFixture(t, "writer-epoch-fencing")
	h := newHarness(t, f)

	// Epoch 2 is opened through the log as well, so the host being cut over to
	// is admitted by an event and not by a hand-written row.
	open := h.epochEvent(t, f, "writer_epoch.opened",
		map[string]any{"epoch": 2, "reason_class": "cutover"}, 1)
	if _, err := h.sub.Submit(h.ctx, open); err != nil {
		t.Fatalf("open epoch 2: %v", err)
	}

	fence := h.epochEvent(t, f, "writer_epoch.fenced",
		map[string]any{"epoch": 1, "reason_class": "cutover"}, 1)
	r, err := h.sub.Submit(h.ctx, fence)
	if err != nil {
		t.Fatalf("fence epoch 1: %v", err)
	}
	fenced, reason, drain := h.epochRow(t, 1)
	if fenced == nil {
		t.Fatal("a committed fence left the epoch open")
	}
	if reason == nil || *reason != "cutover" {
		t.Fatalf("reason_class = %v", reason)
	}
	// The watermark defaults to the fencing event's own sequence: nothing
	// stamped with the old epoch can commit after it.
	if drain == nil || *drain != r.Seq {
		t.Fatalf("drain watermark = %v, want the fencing event's seq %d", drain, r.Seq)
	}

	// The host that was offline through all of this comes back stamped with the
	// epoch it was admitted under.
	stale := f.Steps[0].Submit.event(t)
	stale.WriterEpoch = 1
	h.seedRevisionEpoch(stale, 40, 2)
	if _, err := h.sub.Submit(h.ctx, stale); !ledger.IsClass(err, ledger.ClassWriterEpochFenced) {
		t.Fatalf("the stale writer got %v, want %s", err, ledger.ClassWriterEpochFenced)
	}

	// Re-admitted under the new epoch, the same work continues. Nothing the old
	// epoch committed was lost: the revision it left behind is what the new
	// write advances from.
	readmitted := f.Steps[1].Submit.event(t)
	readmitted.WriterEpoch = 2
	rr, err := h.sub.Submit(h.ctx, readmitted)
	if err != nil {
		t.Fatalf("readmitted submit: %v", err)
	}
	if rr.Revision == nil || *rr.Revision != 41 {
		t.Fatalf("revision = %v, want 41", rr.Revision)
	}
}

// TestFencingTwiceKeepsTheFirstTimestamp: when the epoch stopped accepting
// writes is evidence, and a retry during an incident must not rewrite it.
func TestFencingTwiceKeepsTheFirstTimestamp(t *testing.T) {
	f := loadFixture(t, "writer-epoch-fencing")
	h := newHarness(t, f)

	first := h.epochEvent(t, f, "writer_epoch.fenced",
		map[string]any{"epoch": 1, "reason_class": "incident"}, 1)
	if _, err := h.sub.Submit(h.ctx, first); err != nil {
		t.Fatalf("first fence: %v", err)
	}
	was, _, drainWas := h.epochRow(t, 1)

	// A second fence is stamped with the now-fenced epoch, which the server
	// refuses before it reaches the domain -- that refusal is itself the
	// property under test, and it is why an admin cuts over to a new epoch
	// before fencing the old one in anger.
	second := h.epochEvent(t, f, "writer_epoch.fenced",
		map[string]any{"epoch": 1, "reason_class": "cutover"}, 1)
	if _, err := h.sub.Submit(h.ctx, second); !ledger.IsClass(err, ledger.ClassWriterEpochFenced) {
		t.Fatalf("second fence got %v, want %s", err, ledger.ClassWriterEpochFenced)
	}
	now, reason, drain := h.epochRow(t, 1)
	if now == nil || !now.Equal(*was) {
		t.Fatalf("the fence timestamp moved from %v to %v", was, now)
	}
	if reason == nil || *reason != "incident" || drain == nil || *drain != *drainWas {
		t.Fatalf("the fence record was rewritten: reason %v drain %v", reason, drain)
	}
}

// TestOpeningAnEpochTwiceIsRefused: two opens of one epoch mean two hosts
// believe they were admitted separately, and every event already stamped with
// it points at the row the second open would move.
func TestOpeningAnEpochTwiceIsRefused(t *testing.T) {
	f := loadFixture(t, "writer-epoch-fencing")
	h := newHarness(t, f)

	open := h.epochEvent(t, f, "writer_epoch.opened",
		map[string]any{"epoch": 3, "reason_class": "host_replaced"}, 1)
	if _, err := h.sub.Submit(h.ctx, open); err != nil {
		t.Fatalf("open epoch 3: %v", err)
	}
	again := h.epochEvent(t, f, "writer_epoch.opened",
		map[string]any{"epoch": 3, "reason_class": "host_replaced"}, 1)
	if _, err := h.sub.Submit(h.ctx, again); !ledger.IsClass(err, ledger.ClassPolicyRejected) {
		t.Fatalf("re-opening got %v, want %s", err, ledger.ClassPolicyRejected)
	}
}

// TestFencingAnEpochNobodyOpenedIsRefused: creating the row here would admit an
// epoch by fencing it, which is the opposite of what the operator asked for.
func TestFencingAnEpochNobodyOpenedIsRefused(t *testing.T) {
	f := loadFixture(t, "writer-epoch-fencing")
	h := newHarness(t, f)
	e := h.epochEvent(t, f, "writer_epoch.fenced",
		map[string]any{"epoch": 404, "reason_class": "incident"}, 1)
	if _, err := h.sub.Submit(h.ctx, e); !ledger.IsClass(err, ledger.ClassPolicyRejected) {
		t.Fatalf("got %v, want %s", err, ledger.ClassPolicyRejected)
	}
	var exists bool
	if err := h.pool.Pgx().QueryRow(h.ctx,
		`SELECT EXISTS (SELECT 1 FROM writer_epochs WHERE epoch = 404)`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("a refused fence created the epoch it refused")
	}
}

// TestAWriterCannotFenceItsOwnEpoch: fencing is an admin action. A host that
// could fence would be able to lock every other writer out of the ledger.
func TestAWriterCannotFenceItsOwnEpoch(t *testing.T) {
	f := loadFixture(t, "writer-epoch-fencing")
	h := newHarness(t, f)
	tmpl := f.Steps[0].Submit.event(t)

	e := h.epochEvent(t, f, "writer_epoch.fenced",
		map[string]any{"epoch": 1, "reason_class": "incident"}, 1)
	e.PrincipalID = tmpl.PrincipalID
	e.SessionID = tmpl.SessionID
	e.BranchID = tmpl.BranchID
	if _, err := h.sub.Submit(h.ctx, e); !ledger.IsClass(err, ledger.ClassScopeDenied) {
		t.Fatalf("a checkpoint writer fencing got %v, want %s", err, ledger.ClassScopeDenied)
	}
	if fenced, _, _ := h.epochRow(t, 1); fenced != nil {
		t.Fatal("a refused fence still fenced the epoch")
	}
}

// TestAMalformedEpochPayloadFencesNothing: zero is what an omitted field
// decodes to, and admitting it would fence whatever epoch is numbered zero.
func TestAMalformedEpochPayloadFencesNothing(t *testing.T) {
	f := loadFixture(t, "writer-epoch-fencing")
	h := newHarness(t, f)
	for _, payload := range []map[string]any{
		{"reason_class": "incident"},
		{"epoch": 1},
		{"epoch": 1, "reason_class": "because I said so"},
	} {
		e := h.epochEvent(t, f, "writer_epoch.fenced", payload, 1)
		if _, err := h.sub.Submit(h.ctx, e); !ledger.IsClass(err, ledger.ClassPolicyRejected) {
			t.Fatalf("payload %v got %v, want %s", payload, err, ledger.ClassPolicyRejected)
		}
	}
	if fenced, _, _ := h.epochRow(t, 1); fenced != nil {
		t.Fatal("a malformed payload fenced epoch 1")
	}
}
