package ledger_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/ledger"
	"github.com/enowdev/enowx-rag/pkg/memgw/principal"
)

// --- fixture replays ---------------------------------------------------------

// TestFixtureDuplicateRetry is INV-01 and INV-02: the lost-response retry the
// outbox will perform on every host must commit once.
func TestFixtureDuplicateRetry(t *testing.T) {
	f := loadFixture(t, "duplicate-retry")
	h := newHarness(t, f)

	first := f.Steps[0].Submit.event(t)
	h.seedRevision(first.PrincipalID, first.ProjectID, first.WorkspaceID, *first.WorkID, first.BranchID, first.SessionID, 7)
	before := h.countEvents()

	r1, err := h.sub.Submit(h.ctx, first)
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if r1.State != ledger.StateCommitted {
		t.Fatalf("first submit state = %s", r1.State)
	}
	if r1.Revision == nil || *r1.Revision != 8 {
		t.Fatalf("resulting revision = %v, fixture says 8", r1.Revision)
	}

	retry := f.Steps[1].Submit.event(t)
	r2, err := h.sub.Submit(h.ctx, retry)
	if err != nil {
		t.Fatalf("retry should not error: %v", err)
	}
	if r2.State != ledger.StateDuplicate {
		t.Fatalf("retry state = %s, want duplicate", r2.State)
	}
	if r2.EventID != r1.EventID || r2.Seq != r1.Seq {
		t.Fatal("the retry must return the original receipt, not a new one")
	}
	if got := h.countEvents(); got != before+1 {
		t.Fatalf("the retry wrote a second event: %d events, expected %d", got, before+1)
	}
	if got := h.currentRevision(*first.WorkID); got != 8 {
		t.Fatalf("revision advanced twice: %d", got)
	}
	if got := h.countOutbox(); got != 1 {
		t.Fatalf("the retry enqueued a second projection row: %d", got)
	}
}

// TestFixturePayloadMismatch reuses an idempotency key for different content.
// The fixture's "given" is the committed state duplicate-retry leaves behind, so
// the setup replays that first submit rather than inventing an equivalent.
func TestFixturePayloadMismatch(t *testing.T) {
	setup := loadFixture(t, "duplicate-retry")
	f := loadFixture(t, "payload-mismatch")
	h := newHarness(t, f)

	first := setup.Steps[0].Submit.event(t)
	h.seedRevision(first.PrincipalID, first.ProjectID, first.WorkspaceID, *first.WorkID, first.BranchID, first.SessionID, 7)
	if _, err := h.sub.Submit(h.ctx, first); err != nil {
		t.Fatalf("setup submit: %v", err)
	}
	before := h.countEvents()

	_, err := h.sub.Submit(h.ctx, f.Steps[0].Submit.event(t))
	if !ledger.IsClass(err, ledger.ClassPayloadMismatch) {
		t.Fatalf("want %s, got %v", ledger.ClassPayloadMismatch, err)
	}
	if got := h.countEvents(); got != before {
		t.Fatalf("a rejected mismatch wrote an event: %d, expected %d", got, before)
	}
	if got := h.currentRevision(*first.WorkID); got != 8 {
		t.Fatalf("revision moved on a rejection: %d", got)
	}
}

// TestFixtureOutOfOrderEvent is INV-07 and INV-20: a host that was offline
// flushes stale work, and wall-clock order must not decide anything.
func TestFixtureOutOfOrderEvent(t *testing.T) {
	f := loadFixture(t, "out-of-order-event")
	h := newHarness(t, f)

	stale := f.Steps[0].Submit.event(t)
	h.seedRevision(stale.PrincipalID, stale.ProjectID, stale.WorkspaceID, *stale.WorkID, stale.BranchID, stale.SessionID, 20)

	if _, err := h.sub.Submit(h.ctx, stale); !ledger.IsClass(err, ledger.ClassStaleRevision) {
		t.Fatalf("want %s, got %v", ledger.ClassStaleRevision, err)
	}
	if got := h.currentRevision(*stale.WorkID); got != 20 {
		t.Fatalf("a stale event moved the revision to %d", got)
	}

	// The same late flush also carries evidence, which is additive and must
	// still be accepted: being behind is not a reason to lose the record.
	evidence := f.Steps[1].Submit.event(t)
	r, err := h.sub.Submit(h.ctx, evidence)
	if err != nil {
		t.Fatalf("late evidence should be accepted: %v", err)
	}
	if r.State != ledger.StateCommitted {
		t.Fatalf("evidence state = %s", r.State)
	}
	if r.Revision != nil {
		t.Fatalf("an additive event must not carry a revision, got %d", *r.Revision)
	}
	if got := h.currentRevision(*stale.WorkID); got != 20 {
		t.Fatalf("evidence advanced work state to %d", got)
	}
}

// TestFixtureConcurrentCAS is INV-06. It is run concurrently on purpose: the
// fixture describes two writers that both read revision 12, which is a
// different situation from one writer arriving late. Serialised, the second
// writer would be stale_revision; racing, the loser is a cas_conflict decided
// by the unique index rather than by a lock.
func TestFixtureConcurrentCAS(t *testing.T) {
	f := loadFixture(t, "concurrent-cas")
	h := newHarness(t, f)

	a := f.Steps[0].Submit.event(t)
	b := f.Steps[1].Submit.event(t)
	h.seedRevision(a.PrincipalID, a.ProjectID, a.WorkspaceID, *a.WorkID, a.BranchID, a.SessionID, 12)
	before := h.countEvents()

	type result struct {
		receipt ledger.Receipt
		err     error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, e := range []ledger.Event{a, b} {
		wg.Add(1)
		go func(i int, e ledger.Event) {
			defer wg.Done()
			<-start
			r, err := h.sub.Submit(h.ctx, e)
			results[i] = result{r, err}
		}(i, e)
	}
	close(start)
	wg.Wait()

	var committed, conflicted int
	for _, r := range results {
		switch {
		case r.err == nil && r.receipt.State == ledger.StateCommitted:
			committed++
			if r.receipt.Revision == nil || *r.receipt.Revision != 13 {
				t.Errorf("winner landed at revision %v, fixture says 13", r.receipt.Revision)
			}
		case ledger.IsClass(r.err, ledger.ClassCASConflict), ledger.IsClass(r.err, ledger.ClassStaleRevision):
			// Either class means the loser did not win. The fixture names
			// cas_conflict, which is what a genuinely concurrent loser gets;
			// stale_revision appears only if the runtime serialised them,
			// and is still a refusal, never a lost update.
			conflicted++
		default:
			t.Errorf("unexpected outcome: receipt=%+v err=%v", r.receipt, r.err)
		}
	}
	if committed != 1 || conflicted != 1 {
		t.Fatalf("want exactly one winner and one refusal, got %d/%d", committed, conflicted)
	}
	if got := h.currentRevision(*a.WorkID); got != 13 {
		t.Fatalf("revision = %d, want 13", got)
	}
	if got := h.countEvents(); got != before+1 {
		t.Fatalf("a race wrote %d events, expected 1", got-before)
	}
}

// TestFixtureUnknownAckReconciliation is the lost-response case the whole
// receipt table exists for: the client never learned the outcome, and both
// recovery routes -- lookup, and an identical retry -- must agree.
func TestFixtureUnknownAckReconciliation(t *testing.T) {
	f := loadFixture(t, "unknown-ack-reconciliation")
	h := newHarness(t, f)

	e := f.Steps[0].Submit.event(t)
	h.seedRevision(e.PrincipalID, e.ProjectID, e.WorkspaceID, *e.WorkID, e.BranchID, e.SessionID, 50)

	committed, err := h.sub.Submit(h.ctx, e)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	// The response is "lost" here: nothing below uses `committed` except to
	// check the recovered answer matches what really happened.

	found, ok, err := h.sub.LookupReceipt(h.ctx, e.PrincipalID, e.IdempotencyKey)
	if err != nil || !ok {
		t.Fatalf("receipt lookup: ok=%v err=%v", ok, err)
	}
	if found.EventID != committed.EventID || found.State != ledger.StateCommitted {
		t.Fatalf("lookup returned %+v, want the committed receipt", found)
	}
	if found.Seq != committed.Seq {
		t.Fatal("the recovered receipt must name the same seq")
	}

	retry, err := h.sub.Submit(h.ctx, f.Steps[2].Submit.event(t))
	if err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	if retry.State != ledger.StateDuplicate {
		t.Fatalf("retry state = %s", retry.State)
	}
	if got := h.currentRevision(*e.WorkID); got != 51 {
		t.Fatalf("revision = %d, want 51", got)
	}
}

// TestReceiptLookupIsScopedToItsPrincipal: a receipt is a private answer about
// one principal's write, not a directory of what other hosts have written.
func TestReceiptLookupIsScopedToItsPrincipal(t *testing.T) {
	f := loadFixture(t, "duplicate-retry")
	h := newHarness(t, f)
	e := f.Steps[0].Submit.event(t)
	h.seedRevision(e.PrincipalID, e.ProjectID, e.WorkspaceID, *e.WorkID, e.BranchID, e.SessionID, 7)
	if _, err := h.sub.Submit(h.ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := h.sub.LookupReceipt(h.ctx, uuid.New(), e.IdempotencyKey); err != nil || ok {
		t.Fatalf("another principal saw the receipt: ok=%v err=%v", ok, err)
	}
}

// --- writer epochs -----------------------------------------------------------

// TestWriterEpochFencing replays the fixture's shape: a host whose lease lapsed
// flushes buffered work under the old epoch and is refused, then is readmitted
// under the current one. The refusal is server-side, so it does not depend on
// the writer having noticed it was fenced.
func TestWriterEpochFencing(t *testing.T) {
	f := loadFixture(t, "writer-epoch-fencing")
	h := newHarness(t, f)

	// Epoch 1 is seeded by the migration; fence it and open epoch 2.
	if _, err := h.pool.Pgx().Exec(h.ctx, `UPDATE writer_epochs SET fenced_at = now(), drain_watermark_seq = 0 WHERE epoch = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Pgx().Exec(h.ctx, `INSERT INTO writer_epochs (epoch, opened_at) VALUES (2, now())`); err != nil {
		t.Fatal(err)
	}

	stale := f.Steps[0].Submit.event(t)
	stale.WriterEpoch = 1
	h.seedRevisionEpoch(stale, 40, 2)

	if _, err := h.sub.Submit(h.ctx, stale); !ledger.IsClass(err, ledger.ClassWriterEpochFenced) {
		t.Fatalf("want %s, got %v", ledger.ClassWriterEpochFenced, err)
	}

	readmitted := f.Steps[1].Submit.event(t)
	readmitted.WriterEpoch = 2
	r, err := h.sub.Submit(h.ctx, readmitted)
	if err != nil {
		t.Fatalf("readmitted submit: %v", err)
	}
	if r.Revision == nil || *r.Revision != 41 {
		t.Fatalf("revision = %v, fixture says 41", r.Revision)
	}
}

// TestUnknownWriterEpochIsRefused: an epoch nobody opened is not a lesser
// problem than a fenced one. Both mean the write cannot be ordered.
func TestUnknownWriterEpochIsRefused(t *testing.T) {
	f := loadFixture(t, "duplicate-retry")
	h := newHarness(t, f)
	e := f.Steps[0].Submit.event(t)
	e.WriterEpoch = 999
	h.seedRevision(e.PrincipalID, e.ProjectID, e.WorkspaceID, *e.WorkID, e.BranchID, e.SessionID, 7)
	if _, err := h.sub.Submit(h.ctx, e); !ledger.IsClass(err, ledger.ClassWriterEpochFenced) {
		t.Fatalf("want %s, got %v", ledger.ClassWriterEpochFenced, err)
	}
}

// --- transactional integrity -------------------------------------------------

// TestFailedInsertLeavesNoReceipt is the "transaction failure leaves no fake
// receipt" criterion. Reusing an event_id under a new idempotency key fails on
// the primary key after the receipt and outbox inserts would have been reached,
// so it exercises the rollback rather than an early validation return.
func TestFailedInsertLeavesNoReceipt(t *testing.T) {
	f := loadFixture(t, "duplicate-retry")
	h := newHarness(t, f)
	e := f.Steps[0].Submit.event(t)
	h.seedRevision(e.PrincipalID, e.ProjectID, e.WorkspaceID, *e.WorkID, e.BranchID, e.SessionID, 7)
	if _, err := h.sub.Submit(h.ctx, e); err != nil {
		t.Fatal(err)
	}
	events, receipts, outbox := h.countEvents(), h.countReceipts(), h.countOutbox()

	clash := f.Steps[0].Submit.event(t)
	clash.IdempotencyKey = "omp:sess-1:ckpt:0007-different"
	clash.Payload = map[string]any{"objective": "something else"}
	current := int64(8)
	clash.ExpectedRevision = &current // pass CAS, so the failure lands on the insert
	_, err := h.sub.Submit(h.ctx, clash)
	if !ledger.IsClass(err, ledger.ClassPolicyRejected) {
		t.Fatalf("want a refusal on the duplicate event_id, got %v", err)
	}
	if h.countEvents() != events || h.countReceipts() != receipts || h.countOutbox() != outbox {
		t.Fatalf("a failed submit left rows behind: events %d->%d receipts %d->%d outbox %d->%d",
			events, h.countEvents(), receipts, h.countReceipts(), outbox, h.countOutbox())
	}
	if _, ok, err := h.sub.LookupReceipt(h.ctx, clash.PrincipalID, clash.IdempotencyKey); err != nil || ok {
		t.Fatalf("a failed submit produced a receipt: ok=%v err=%v", ok, err)
	}
}

// TestNoReceiptBeforeCommit: every refused submit must be invisible to a later
// lookup, whatever stage refused it.
func TestNoReceiptBeforeCommit(t *testing.T) {
	f := loadFixture(t, "duplicate-retry")
	h := newHarness(t, f)
	base := f.Steps[0].Submit.event(t)
	h.seedRevision(base.PrincipalID, base.ProjectID, base.WorkspaceID, *base.WorkID, base.BranchID, base.SessionID, 7)

	cases := map[string]func(ledger.Event) ledger.Event{
		"stale revision":     func(e ledger.Event) ledger.Event { r := int64(3); e.ExpectedRevision = &r; return e },
		"unknown epoch":      func(e ledger.Event) ledger.Event { e.WriterEpoch = 44; return e },
		"bad schema version": func(e ledger.Event) ledger.Event { e.SchemaVersion = "0.9.0"; return e },
		"missing payload":    func(e ledger.Event) ledger.Event { e.Payload = nil; return e },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := mutate(f.Steps[0].Submit.event(t))
			e.EventID = uuid.New()
			e.IdempotencyKey = "probe:" + name
			if _, err := h.sub.Submit(h.ctx, e); err == nil {
				t.Fatal("expected a refusal")
			}
			if _, ok, err := h.sub.LookupReceipt(h.ctx, e.PrincipalID, e.IdempotencyKey); err != nil || ok {
				t.Fatalf("a refused submit left a receipt: ok=%v err=%v", ok, err)
			}
		})
	}
}

// TestOutboxRowJoinsTheCommit: the projection row is written in the same
// transaction as the event, so a committed event is never invisible to
// projections.
func TestOutboxRowJoinsTheCommit(t *testing.T) {
	f := loadFixture(t, "duplicate-retry")
	h := newHarness(t, f)
	e := f.Steps[0].Submit.event(t)
	h.seedRevision(e.PrincipalID, e.ProjectID, e.WorkspaceID, *e.WorkID, e.BranchID, e.SessionID, 7)
	r, err := h.sub.Submit(h.ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	var eventID uuid.UUID
	var seq int64
	if err := h.pool.Pgx().QueryRow(h.ctx,
		`SELECT event_id, event_seq FROM projection_outbox WHERE projection = 'qdrant'`).Scan(&eventID, &seq); err != nil {
		t.Fatalf("no outbox row for a committed event: %v", err)
	}
	if eventID != r.EventID || seq != r.Seq {
		t.Fatalf("outbox row points at %s/%d, event is %s/%d", eventID, seq, r.EventID, r.Seq)
	}
}

// TestEventsAreAppendOnly: the trigger, not just convention, is what makes the
// log a ledger.
func TestEventsAreAppendOnly(t *testing.T) {
	f := loadFixture(t, "duplicate-retry")
	h := newHarness(t, f)
	e := f.Steps[0].Submit.event(t)
	h.seedRevision(e.PrincipalID, e.ProjectID, e.WorkspaceID, *e.WorkID, e.BranchID, e.SessionID, 7)
	if _, err := h.sub.Submit(h.ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Pgx().Exec(h.ctx, `UPDATE events SET sensitivity_class = 'public'`); err == nil {
		t.Fatal("events must not be updatable")
	}
	if _, err := h.pool.Pgx().Exec(h.ctx, `DELETE FROM events`); err == nil {
		t.Fatal("events must not be deletable")
	}
}

// --- policy refusals ---------------------------------------------------------

// TestCredentialShapedPayloadIsRefused is INV-14. The value below is not a real
// credential; what is real is the shape the guard matches, and the point of the
// test is that the refusal carries neither the value nor the field.
func TestCredentialShapedPayloadIsRefused(t *testing.T) {
	f := loadFixture(t, "duplicate-retry")
	h := newHarness(t, f)
	e := f.Steps[0].Submit.event(t)
	h.seedRevision(e.PrincipalID, e.ProjectID, e.WorkspaceID, *e.WorkID, e.BranchID, e.SessionID, 7)

	planted := strings.Repeat("A", 24)
	e.Payload = map[string]any{"objective": "wire the gateway", "api_key": planted}
	e.EventID = uuid.New()
	e.IdempotencyKey = "claude:sess-13:ckpt:0001"

	_, err := h.sub.Submit(h.ctx, e)
	if !ledger.IsClass(err, ledger.ClassPolicyRejected) {
		t.Fatalf("want %s, got %v", ledger.ClassPolicyRejected, err)
	}
	if strings.Contains(err.Error(), planted) || strings.Contains(err.Error(), "api_key") {
		t.Fatalf("the refusal echoed the payload: %s", err)
	}
	if h.countEvents() != 1 { // the seed row only
		t.Fatal("a credential-shaped payload was persisted")
	}
}

// TestSubmitWithoutGrantIsScopeDenied: authorization is part of the write path,
// not a layer above it that could be bypassed by calling the ledger directly.
func TestSubmitWithoutGrantIsScopeDenied(t *testing.T) {
	f := loadFixture(t, "duplicate-retry")
	h := newHarness(t, f)
	stranger, err := h.auth.CreatePrincipal(h.ctx, principal.Principal{
		Type: principal.TypeAgent, HostID: uuid.New(), AgentID: "stranger", DisplayName: "stranger",
	})
	if err != nil {
		t.Fatal(err)
	}
	e := f.Steps[0].Submit.event(t)
	e.PrincipalID = stranger.ID
	e.EventID = uuid.New()
	e.IdempotencyKey = "stranger:1"
	if _, err := h.sub.Submit(h.ctx, e); !ledger.IsClass(err, ledger.ClassScopeDenied) {
		t.Fatalf("want %s, got %v", ledger.ClassScopeDenied, err)
	}
}

// TestPayloadOverBoundIsRefused checks the 256 KiB bound is enforced before the
// database sees it, so the same limit holds whatever the column would accept.
func TestPayloadOverBoundIsRefused(t *testing.T) {
	f := loadFixture(t, "duplicate-retry")
	h := newHarness(t, f)
	e := f.Steps[0].Submit.event(t)
	h.seedRevision(e.PrincipalID, e.ProjectID, e.WorkspaceID, *e.WorkID, e.BranchID, e.SessionID, 7)
	e.Payload = map[string]any{"objective": strings.Repeat("x", ledger.MaxPayloadBytes+1)}
	e.EventID = uuid.New()
	e.IdempotencyKey = "too-large"
	if _, err := h.sub.Submit(h.ctx, e); !ledger.IsClass(err, ledger.ClassPayloadTooLarge) {
		t.Fatalf("want %s, got %v", ledger.ClassPayloadTooLarge, err)
	}
}

// TestMutatingEventNeedsCAS: a work-advancing event with no expected_revision
// is refused rather than quietly appended, because a caller that did not read
// the current state cannot claim to have decided anything about it.
func TestMutatingEventNeedsCAS(t *testing.T) {
	f := loadFixture(t, "duplicate-retry")
	h := newHarness(t, f)
	e := f.Steps[0].Submit.event(t)
	h.seedRevision(e.PrincipalID, e.ProjectID, e.WorkspaceID, *e.WorkID, e.BranchID, e.SessionID, 7)
	e.ExpectedRevision = nil
	e.EventID = uuid.New()
	e.IdempotencyKey = "no-cas"
	if _, err := h.sub.Submit(h.ctx, e); !ledger.IsClass(err, ledger.ClassPolicyRejected) {
		t.Fatalf("want %s, got %v", ledger.ClassPolicyRejected, err)
	}
}

func TestQueryTimeoutIsBounded(t *testing.T) {
	f := loadFixture(t, "duplicate-retry")
	h := newHarness(t, f)
	if h.pool.QueryTimeout() <= 0 || h.pool.QueryTimeout() > 30*time.Second {
		t.Fatalf("query timeout %v is not a sane bound", h.pool.QueryTimeout())
	}
}
