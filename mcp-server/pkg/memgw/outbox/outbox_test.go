// These tests are about what happens when things go wrong, because a queue
// that only works when nothing fails is not a queue. Each one names the failure
// it simulates and what the queue must still guarantee afterwards.
package outbox_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/memgwtest"
	"github.com/enowdev/enowx-rag/pkg/memgw/outbox"
	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
)

type env struct {
	t         *testing.T
	ctx       context.Context
	pool      *pgstore.Pool
	store     *outbox.Store
	principal uuid.UUID
	project   uuid.UUID
	workspace uuid.UUID
	branch    uuid.UUID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := memgwtest.Pool(t)
	e := &env{
		t:         t,
		ctx:       t.Context(),
		pool:      pool,
		store:     outbox.NewStore(pool),
		project:   uuid.New(),
		workspace: uuid.New(),
		branch:    uuid.New(),
	}
	e.principal = uuid.New()
	if _, err := pool.Pgx().Exec(e.ctx, `
		INSERT INTO principals (principal_id, principal_type, host_id, agent_id, display_name, status, writer_epoch)
		VALUES ($1, 'service', $2, 'projection-test', 'projection-test', 'active', 1)`,
		e.principal, uuid.New()); err != nil {
		t.Fatalf("seed principal: %v", err)
	}
	// The event log points at the registry, so a queue test still needs a
	// project, a workspace, a session and a branch to hang its events on.
	memgwtest.SeedWorkspace(t, pool, e.project, e.workspace)
	memgwtest.SeedSession(t, pool, "test:sess", e.project, e.workspace, e.principal)
	memgwtest.SeedBranch(t, pool, e.branch, e.project, "test:sess")
	return e
}

// enqueue writes one event and its outbox row the way Submit does, so the tests
// exercise real rows rather than a mock queue.
func (e *env) enqueue(n int) []uuid.UUID {
	e.t.Helper()
	var ids []uuid.UUID
	for i := 0; i < n; i++ {
		id := uuid.New()
		var seq int64
		if err := e.pool.Pgx().QueryRow(e.ctx, `
			INSERT INTO events (event_id, idempotency_key, principal_id, project_id, workspace_id,
			                    session_id, branch_id, event_type, payload, payload_digest,
			                    sensitivity_class, policy_version, schema_version, occurred_at, writer_epoch)
			VALUES ($1, $2, $3, $4, $5, 'test:sess', $6, 'evidence.recorded', '{"n":1}',
			        repeat('0', 64), 'internal', '1.0.0', '1.0.0', now(), 1)
			RETURNING seq`,
			id, "queue:"+id.String(), e.principal, e.project, e.workspace, e.branch).Scan(&seq); err != nil {
			e.t.Fatalf("seed event: %v", err)
		}
		if _, err := e.pool.Pgx().Exec(e.ctx, `
			INSERT INTO projection_outbox (event_id, event_seq, projection) VALUES ($1, $2, 'qdrant')`,
			id, seq); err != nil {
			e.t.Fatalf("seed outbox row: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

func (e *env) state(outboxID int64) (string, int) {
	e.t.Helper()
	var state string
	var attempts int
	if err := e.pool.Pgx().QueryRow(e.ctx,
		`SELECT state, attempts FROM projection_outbox WHERE outbox_id = $1`, outboxID).Scan(&state, &attempts); err != nil {
		e.t.Fatalf("read state: %v", err)
	}
	return state, attempts
}

// applier records what it was asked to project and can be told to fail.
type applier struct {
	seen []uuid.UUID
	fail error
}

func (a *applier) Apply(_ context.Context, it outbox.Item) error {
	a.seen = append(a.seen, it.EventID)
	return a.fail
}

func TestClaimLeasesAndAdvancesWatermark(t *testing.T) {
	e := newEnv(t)
	e.enqueue(2)

	items, err := e.store.Claim(e.ctx, outbox.ProjectionQdrant, "worker-1", time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("claimed %d rows, want 2", len(items))
	}
	for _, it := range items {
		if st, attempts := e.state(it.OutboxID); st != "in_flight" || attempts != 1 {
			t.Fatalf("row %d is %s with %d attempts", it.OutboxID, st, attempts)
		}
		if err := e.store.MarkApplied(e.ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	wm, err := e.store.Watermark(e.ctx, outbox.ProjectionQdrant)
	if err != nil {
		t.Fatal(err)
	}
	if wm != items[len(items)-1].EventSeq {
		t.Fatalf("watermark = %d, want %d", wm, items[len(items)-1].EventSeq)
	}
}

// A second worker must not see rows the first is holding. This is the property
// that lets two hosts run a projection worker without coordinating.
func TestClaimDoesNotHandOutTheSameRowTwice(t *testing.T) {
	e := newEnv(t)
	e.enqueue(3)

	first, err := e.store.Claim(e.ctx, outbox.ProjectionQdrant, "worker-1", time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.store.Claim(e.ctx, outbox.ProjectionQdrant, "worker-2", time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 || len(second) != 0 {
		t.Fatalf("worker-1 got %d rows, worker-2 got %d; want 3 and 0", len(first), len(second))
	}
}

// Crash before apply: the worker took the lease and died. The row must come
// back after the lease expires, and nothing was projected.
func TestCrashBeforeApplyReturnsTheRow(t *testing.T) {
	e := newEnv(t)
	e.enqueue(1)

	claimed, err := e.store.Claim(e.ctx, outbox.ProjectionQdrant, "doomed", time.Millisecond, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v (%d rows)", err, len(claimed))
	}
	// The worker dies here. Nothing acknowledges the row.
	time.Sleep(20 * time.Millisecond)

	n, err := e.store.ReclaimExpiredLeases(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d rows, want 1", n)
	}
	again, err := e.store.Claim(e.ctx, outbox.ProjectionQdrant, "successor", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].EventID != claimed[0].EventID {
		t.Fatal("the abandoned row was not handed to the successor")
	}
	if again[0].Attempts != 2 {
		t.Fatalf("attempts = %d, want 2: the crashed attempt still counts", again[0].Attempts)
	}
}

// Crash after apply, before ack: the projection was written but MarkApplied
// never ran. The row is redelivered, which is why an Applier must be idempotent.
// What must not happen is the row being lost, or the watermark claiming
// progress that was never acknowledged.
func TestCrashAfterApplyBeforeAckRedelivers(t *testing.T) {
	e := newEnv(t)
	e.enqueue(1)

	claimed, err := e.store.Claim(e.ctx, outbox.ProjectionQdrant, "doomed", time.Millisecond, 1)
	if err != nil {
		t.Fatal(err)
	}
	a := &applier{}
	if err := a.Apply(e.ctx, claimed[0]); err != nil {
		t.Fatal(err)
	}
	// The process dies between the projection write and the acknowledgement.
	if wm, err := e.store.Watermark(e.ctx, outbox.ProjectionQdrant); err != nil || wm != 0 {
		t.Fatalf("watermark moved without an ack: %d (%v)", wm, err)
	}

	time.Sleep(20 * time.Millisecond)
	if _, err := e.store.ReclaimExpiredLeases(e.ctx); err != nil {
		t.Fatal(err)
	}
	redelivered, err := e.store.Claim(e.ctx, outbox.ProjectionQdrant, "successor", time.Minute, 1)
	if err != nil || len(redelivered) != 1 {
		t.Fatalf("the row was not redelivered: %v (%d)", err, len(redelivered))
	}
	if err := a.Apply(e.ctx, redelivered[0]); err != nil {
		t.Fatal(err)
	}
	if err := e.store.MarkApplied(e.ctx, redelivered[0]); err != nil {
		t.Fatal(err)
	}
	if len(a.seen) != 2 || a.seen[0] != a.seen[1] {
		t.Fatalf("expected the same event applied twice, got %v", a.seen)
	}
	if wm, _ := e.store.Watermark(e.ctx, outbox.ProjectionQdrant); wm != redelivered[0].EventSeq {
		t.Fatalf("watermark = %d after the ack", wm)
	}
}

// A row that keeps failing is dead-lettered, not dropped. The failure class is
// stored; the payload that failed is not.
func TestRepeatedFailureDeadLettersVisibly(t *testing.T) {
	e := newEnv(t)
	e.enqueue(1)
	store := e.store.WithMaxAttempts(3)

	var last outbox.Item
	for attempt := 1; attempt <= 3; attempt++ {
		items, err := store.Claim(e.ctx, outbox.ProjectionQdrant, "worker", time.Minute, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 {
			t.Fatalf("attempt %d claimed %d rows", attempt, len(items))
		}
		last = items[0]
		if err := store.MarkFailed(e.ctx, last, "apply_failed", 0); err != nil {
			t.Fatal(err)
		}
	}
	if st, attempts := e.state(last.OutboxID); st != "dead_letter" || attempts != 3 {
		t.Fatalf("row is %s with %d attempts, want dead_letter/3", st, attempts)
	}
	dead, err := store.DeadLetters(e.ctx, outbox.ProjectionQdrant)
	if err != nil {
		t.Fatal(err)
	}
	if len(dead) != 1 {
		t.Fatalf("dead letters = %d, want 1: a queue must not drop what it cannot deliver", len(dead))
	}

	// A dead-lettered row is out of the rotation but still on the books.
	more, err := store.Claim(e.ctx, outbox.ProjectionQdrant, "worker", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(more) != 0 {
		t.Fatal("a dead-lettered row must not be claimed again")
	}
	if wm, _ := store.Watermark(e.ctx, outbox.ProjectionQdrant); wm != 0 {
		t.Fatalf("watermark = %d; a failed row must not count as progress", wm)
	}
}

// The failure class is the only thing recorded about a failure.
func TestFailureRecordsClassNotContent(t *testing.T) {
	e := newEnv(t)
	e.enqueue(1)
	items, _ := e.store.Claim(e.ctx, outbox.ProjectionQdrant, "worker", time.Minute, 1)
	if err := e.store.MarkFailed(e.ctx, items[0], "apply_failed", time.Minute); err != nil {
		t.Fatal(err)
	}
	var class string
	if err := e.pool.Pgx().QueryRow(e.ctx,
		`SELECT last_error_class FROM projection_outbox WHERE outbox_id = $1`, items[0].OutboxID).Scan(&class); err != nil {
		t.Fatal(err)
	}
	if class != "apply_failed" {
		t.Fatalf("last_error_class = %q", class)
	}
	// The backoff must actually hold the row back.
	again, _ := e.store.Claim(e.ctx, outbox.ProjectionQdrant, "worker", time.Minute, 1)
	if len(again) != 0 {
		t.Fatal("a backed-off row was claimed before its not_before")
	}
}

// --- worker ------------------------------------------------------------------

func TestWorkerDrainsAndStops(t *testing.T) {
	e := newEnv(t)
	ids := e.enqueue(3)
	a := &applier{}

	cfg := outbox.DefaultWorkerConfig(outbox.ProjectionQdrant, "worker-1")
	cfg.MaxBatches = 1
	if err := outbox.NewWorker(e.store, a, cfg).Run(e.ctx); err != nil {
		t.Fatalf("worker: %v", err)
	}
	if len(a.seen) != len(ids) {
		t.Fatalf("applied %d of %d events", len(a.seen), len(ids))
	}
	if wm, _ := e.store.Watermark(e.ctx, outbox.ProjectionQdrant); wm == 0 {
		t.Fatal("the watermark did not move after a clean drain")
	}
}

// A worker that cannot apply must not lose the row, and must not change
// anything about the canonical event.
func TestWorkerFailureLeavesTheEventUntouched(t *testing.T) {
	e := newEnv(t)
	ids := e.enqueue(1)
	a := &applier{fail: errors.New("projection unavailable")}

	cfg := outbox.DefaultWorkerConfig(outbox.ProjectionQdrant, "worker-1")
	cfg.MaxBatches = 1
	cfg.Backoff = time.Hour
	if err := outbox.NewWorker(e.store, a, cfg).Run(e.ctx); err != nil {
		t.Fatalf("worker: %v", err)
	}

	var state string
	if err := e.pool.Pgx().QueryRow(e.ctx,
		`SELECT state FROM projection_outbox WHERE event_id = $1`, ids[0]).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Fatalf("row state = %s, want failed", state)
	}
	var count int
	if err := e.pool.Pgx().QueryRow(e.ctx, `SELECT count(*) FROM events WHERE event_id = $1`, ids[0]).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("the event changed because a projection failed")
	}
}

// --- tombstones --------------------------------------------------------------

// A tombstone must beat a rebuild that is replaying older events: the rebuild
// arrives after the deletion was ordered and must not put the subject back.
func TestTombstoneWinsOverALaterRebuild(t *testing.T) {
	e := newEnv(t)
	ids := e.enqueue(1)

	subject := ids[0].String()
	if _, err := e.store.IssueTombstone(e.ctx, outbox.Tombstone{
		SubjectType: "event",
		SubjectID:   subject,
		ProjectID:   e.project,
		ReasonClass: "sensitive_content",
		IssuedBy:    e.principal,
	}); err != nil {
		t.Fatal(err)
	}

	tombstoneAware := &tombstoneApplier{store: e.store}
	cfg := outbox.DefaultWorkerConfig(outbox.ProjectionQdrant, "rebuild")
	cfg.MaxBatches = 1
	if err := outbox.NewWorker(e.store, tombstoneAware, cfg).Run(e.ctx); err != nil {
		t.Fatal(err)
	}

	if len(tombstoneAware.projected) != 0 {
		t.Fatalf("the rebuild projected a tombstoned subject: %v", tombstoneAware.projected)
	}
	// Declining is success: the row is done, not retried forever.
	var state string
	if err := e.pool.Pgx().QueryRow(e.ctx,
		`SELECT state FROM projection_outbox WHERE event_id = $1`, ids[0]).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "applied" {
		t.Fatalf("a skipped tombstoned row is %s; it must be applied so it stops being retried", state)
	}
}

type tombstoneApplier struct {
	store     *outbox.Store
	projected []uuid.UUID
}

func (a *tombstoneApplier) Apply(ctx context.Context, it outbox.Item) error {
	tombstoned, err := a.store.IsTombstoned(ctx, "event", it.EventID.String())
	if err != nil {
		return err
	}
	if tombstoned {
		return outbox.ErrSkippedTombstoned
	}
	a.projected = append(a.projected, it.EventID)
	return nil
}

// The legacy chunk map is what lets a deletion reach points written before the
// gateway existed, when the chunk ids no longer match anything derivable.
func TestTombstoneMarksLegacyChunks(t *testing.T) {
	e := newEnv(t)
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	if err := e.store.MapLegacyChunks(e.ctx, []outbox.LegacyChunk{
		{ChunkID: "chunk-1", ProjectID: e.project, RAGProject: "memory", DocumentID: "doc-a", SourceDigest: digest},
		{ChunkID: "chunk-2", ProjectID: e.project, RAGProject: "memory", DocumentID: "doc-a", SourceDigest: digest},
		{ChunkID: "chunk-3", ProjectID: e.project, RAGProject: "memory", DocumentID: "doc-b", SourceDigest: digest},
	}); err != nil {
		t.Fatal(err)
	}
	// Recording the same mapping twice is the same mapping.
	if err := e.store.MapLegacyChunks(e.ctx, []outbox.LegacyChunk{
		{ChunkID: "chunk-1", ProjectID: e.project, RAGProject: "memory", DocumentID: "doc-a", SourceDigest: digest},
	}); err != nil {
		t.Fatalf("remapping must be idempotent: %v", err)
	}

	if _, err := e.store.IssueTombstone(e.ctx, outbox.Tombstone{
		SubjectType:     "document",
		SubjectID:       "doc-a",
		ProjectID:       e.project,
		ReasonClass:     "user_request",
		IssuedBy:        e.principal,
		ErasureRequired: true,
	}); err != nil {
		t.Fatal(err)
	}

	pending, err := e.store.TombstonedChunkIDs(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0] != "chunk-1" || pending[1] != "chunk-2" {
		t.Fatalf("erasure worklist = %v, want chunk-1 and chunk-2 only", pending)
	}
}

// The journal is monotonic: a tombstone cannot be withdrawn. Deletion is
// refused by a trigger; UPDATE is deliberately left open, because an erasure
// step has to stamp erasure_completed_at on a row that already exists.
func TestTombstoneJournalCannotBeErased(t *testing.T) {
	e := newEnv(t)
	tomb, err := e.store.IssueTombstone(e.ctx, outbox.Tombstone{
		SubjectType: "document", SubjectID: "doc-a", ProjectID: e.project,
		ReasonClass: "user_request", IssuedBy: e.principal, ErasureRequired: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Pgx().Exec(e.ctx, `DELETE FROM tombstones`); err == nil {
		t.Fatal("tombstones must not be deletable")
	}
	if _, err := e.pool.Pgx().Exec(e.ctx,
		`UPDATE tombstones SET erasure_completed_at = now() WHERE tombstone_id = $1`, tomb.ID); err != nil {
		t.Fatalf("an erasure step must be able to record completion: %v", err)
	}
	ok, err := e.store.IsTombstoned(e.ctx, "document", "doc-a")
	if err != nil || !ok {
		t.Fatalf("the tombstone stopped being visible after erasure: ok=%v err=%v", ok, err)
	}
}
