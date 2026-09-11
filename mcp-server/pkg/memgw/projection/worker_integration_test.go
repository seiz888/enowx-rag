// The whole path, with nothing stood in for: real events in PostgreSQL, the
// real outbox queue, the real worker, and a real Qdrant collection.
//
// It needs both MEMGW_TEST_DSN and MEMGW_TEST_QDRANT_URL and skips otherwise.
// The unit tests prove what the applier decides; this proves the decisions
// survive being carried by the queue that actually carries them.
package projection_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/domain"
	"github.com/enowdev/enowx-rag/pkg/memgw/memgwtest"
	"github.com/enowdev/enowx-rag/pkg/memgw/outbox"
	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
	"github.com/enowdev/enowx-rag/pkg/memgw/projection"
	"github.com/enowdev/enowx-rag/pkg/rag"
)

type endToEnd struct {
	t         *testing.T
	ctx       context.Context
	pool      *pgstore.Pool
	store     *outbox.Store
	provider  *rag.QdrantProvider
	applier   *projection.Applier
	project   uuid.UUID
	workspace uuid.UUID
	principal uuid.UUID
	branch    uuid.UUID
	work      uuid.UUID
}

func newEndToEnd(t *testing.T) *endToEnd {
	t.Helper()
	url := strings.TrimSpace(os.Getenv(projection.QdrantEnv))
	if url == "" {
		t.Skipf("%s is not set; skipping the end-to-end projection test", projection.QdrantEnv)
	}
	if !strings.Contains(url, "127.0.0.1") && !strings.Contains(url, "localhost") && !strings.Contains(url, "[::1]") {
		t.Fatalf("%s must point at a loopback address; refusing to run against %q", projection.QdrantEnv, url)
	}
	pool := memgwtest.Pool(t) // skips when MEMGW_TEST_DSN is unset

	e := &endToEnd{
		t: t, ctx: t.Context(), pool: pool, store: outbox.NewStore(pool),
		project: uuid.New(), workspace: uuid.New(), principal: uuid.New(),
		branch: uuid.New(), work: uuid.New(),
	}
	if _, err := pool.Pgx().Exec(e.ctx, `
		INSERT INTO principals (principal_id, principal_type, host_id, agent_id, display_name, status, writer_epoch)
		VALUES ($1, 'service', $2, 'projection-e2e', 'projection-e2e', 'active', 1)`,
		e.principal, uuid.New()); err != nil {
		t.Fatalf("seed principal: %v", err)
	}
	memgwtest.SeedWorkspace(t, pool, e.project, e.workspace)
	memgwtest.SeedSession(t, pool, "e2e:sess", e.project, e.workspace, e.principal)
	memgwtest.SeedBranch(t, pool, e.branch, e.project, "e2e:sess")
	// The event log points at a work, so the checkpoints in this test need one
	// to belong to. It is created directly rather than through the ledger for
	// the same reason the events are: a refusal from the write path would make
	// a projection failure ambiguous.
	if _, err := pool.Pgx().Exec(e.ctx, `
		INSERT INTO works (work_id, project_id, workspace_id, title, state, revision, last_event_seq)
		VALUES ($1, $2, $3, 'projection end-to-end', 'active', 1, 0)`,
		e.work, e.project, e.workspace); err != nil {
		t.Fatalf("seed work: %v", err)
	}

	provider, err := rag.NewQdrantProvider(e.ctx, url, "", projection.NewFixtureEmbedder(384))
	if err != nil {
		t.Fatalf("connect to the disposable Qdrant: %v", err)
	}
	e.provider = provider
	t.Cleanup(func() {
		if err := provider.DeleteCollection(context.Background(), e.project.String()); err != nil {
			t.Logf("cleanup: delete collection %s: %v", e.project, err)
		}
		provider.Close()
	})

	e.applier = projection.New(
		projection.NewPgEvents(pool),
		e.store,
		projection.NewQdrantIndex(provider),
		projection.Config{},
	)
	return e
}

// submit writes one event and its outbox row the way the ledger's commit
// transaction does. It is deliberately raw SQL rather than a call into the
// ledger: what is under test is the projection, and a ledger refusal in the
// middle of it would make a failure ambiguous.
func (e *endToEnd) submit(typ string, payload any, sensitivity string) uuid.UUID {
	e.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		e.t.Fatal(err)
	}
	id := uuid.New()
	var seq int64
	if err := e.pool.Pgx().QueryRow(e.ctx, `
		INSERT INTO events (event_id, idempotency_key, principal_id, project_id, workspace_id, work_id,
		                    session_id, branch_id, event_type, payload, payload_digest,
		                    sensitivity_class, policy_version, schema_version, occurred_at, writer_epoch)
		VALUES ($1, $2, $3, $4, $5, $6, 'e2e:sess', $7, $8, $9, repeat('0', 64), $10, '1.0.0', '1.0.0', now(), 1)
		RETURNING seq`,
		id, "e2e:"+id.String(), e.principal, e.project, e.workspace, e.work, e.branch,
		typ, raw, sensitivity).Scan(&seq); err != nil {
		e.t.Fatalf("seed %s: %v", typ, err)
	}
	e.enqueue(id, seq)
	return id
}

func (e *endToEnd) enqueue(eventID uuid.UUID, seq int64) {
	e.t.Helper()
	if _, err := e.pool.Pgx().Exec(e.ctx, `
		INSERT INTO projection_outbox (event_id, event_seq, projection) VALUES ($1, $2, 'qdrant')
		ON CONFLICT (event_id, projection) DO UPDATE
		SET state = 'pending', attempts = 0, not_before = now(), applied_at = NULL`,
		eventID, seq); err != nil {
		e.t.Fatalf("seed outbox row: %v", err)
	}
}

// drain runs the real worker until the queue is empty.
func (e *endToEnd) drain() {
	e.t.Helper()
	cfg := outbox.DefaultWorkerConfig(outbox.ProjectionQdrant, "e2e-worker")
	cfg.MaxBatches = 12
	cfg.IdleWait = 10 * time.Millisecond
	// No backoff: the test wants a retry to be observable inside one drain.
	// Recovering from a transient failure is part of what is under test, not
	// something to be waited out.
	cfg.Backoff = 0
	w := outbox.NewWorker(e.store, e.applier, cfg)
	ctx, cancel := context.WithTimeout(e.ctx, 60*time.Second)
	defer cancel()
	if err := w.Run(ctx); err != nil && !strings.Contains(err.Error(), "context") {
		e.t.Fatalf("worker: %v", err)
	}
}

func (e *endToEnd) count() int {
	e.t.Helper()
	n, err := e.provider.CountPoints(e.ctx, e.project.String())
	if err != nil {
		return 0 // the collection does not exist yet, which counts as empty
	}
	return n
}

func (e *endToEnd) states() map[string]int {
	e.t.Helper()
	rows, err := e.pool.Pgx().Query(e.ctx,
		`SELECT state, count(*) FROM projection_outbox WHERE projection = 'qdrant' GROUP BY state`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			e.t.Fatal(err)
		}
		out[s] = n
	}
	return out
}

func checkpoint(objective string) domain.CheckpointPayload {
	return domain.CheckpointPayload{
		Objective:      objective,
		CompletedWork:  "synthetic sanitised fixture; nothing here came from a real session",
		PendingActions: "none",
		Blockers:       "none",
		NextSafeAction: "continue with the next section of the mandate",
		ModifiedFiles:  []string{"pkg/memgw/projection/applier.go"},
	}
}

func TestTheWorkerDrainsRealEventsIntoARealCollection(t *testing.T) {
	e := newEndToEnd(t)
	cpA := e.submit("checkpoint.recorded", checkpoint("first synthetic checkpoint"), "internal")
	e.submit("checkpoint.recorded", checkpoint("second synthetic checkpoint"), "public")
	// An event with nothing to project still gets an outbox row, and the queue
	// must finish it rather than leave it pending forever.
	e.submit("evidence.recorded", map[string]any{
		"evidence_type": "test_run", "evidence_class": "observed", "locator": "go test ./...",
	}, "internal")
	// And one the sensitivity gate must withhold.
	e.submit("checkpoint.recorded", checkpoint("a confidential checkpoint"), "confidential")

	e.drain()

	if got := e.count(); got != 2 {
		t.Fatalf("the collection holds %d points; two checkpoints were projectable", got)
	}
	states := e.states()
	if states["applied"] != 4 {
		t.Fatalf("the queue did not finish every row: %v", states)
	}
	if states["dead_letter"] != 0 || states["failed"] != 0 {
		t.Fatalf("rows failed: %v", states)
	}
	// The watermark reports progress and must have moved past every row.
	wm, err := e.store.Watermark(e.ctx, outbox.ProjectionQdrant)
	if err != nil {
		t.Fatal(err)
	}
	if wm == 0 {
		t.Fatal("the watermark did not advance")
	}
	// The projected checkpoint is findable by its own identifiers, which is
	// what a later session needs in order to resume from it.
	pts, err := e.provider.ListPoints(e.ctx, e.project.String(), map[string]string{"doc_id": cpA.String()})
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 {
		t.Fatalf("the first checkpoint is in the collection %d times", len(pts))
	}
	if pts[0].Meta["work_id"] != e.work.String() {
		t.Fatalf("the point lost the work it belongs to: %v", pts[0].Meta)
	}
}

func TestATombstoneOrderedLaterErasesWhatWasAlreadyProjected(t *testing.T) {
	e := newEndToEnd(t)
	cp := e.submit("checkpoint.recorded", checkpoint("this one gets deleted"), "internal")
	e.submit("checkpoint.recorded", checkpoint("this one stays"), "internal")
	e.drain()
	if got := e.count(); got != 2 {
		t.Fatalf("setup: the collection holds %d points", got)
	}

	// The journal entry the gateway writes, and the event that orders it.
	if _, err := e.store.IssueTombstone(e.ctx, outbox.Tombstone{
		SubjectType: "checkpoint", SubjectID: cp.String(), ProjectID: e.project,
		ReasonClass: "user_request", IssuedBy: e.principal, ErasureRequired: true,
	}); err != nil {
		t.Fatal(err)
	}
	e.submit("tombstone.issued", domain.TombstonePayload{
		SubjectType: "checkpoint", SubjectID: cp.String(),
		ReasonClass: "user_request", ErasureRequired: true,
	}, "internal")
	e.drain()

	if got := e.count(); got != 1 {
		t.Fatalf("after the erasure the collection holds %d points, want 1", got)
	}
	pts, err := e.provider.ListPoints(e.ctx, e.project.String(), map[string]string{"doc_id": cp.String()})
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 0 {
		t.Fatal("the tombstoned checkpoint is still in the collection")
	}
}

func TestARebuildDoesNotRestoreATombstonedCheckpoint(t *testing.T) {
	// The scenario the deletion journal exists for, run for real: the index is
	// discarded and rebuilt from a ledger whose events are all older than the
	// deletion order. A rebuild that replayed them faithfully would restore
	// exactly what somebody asked to have removed.
	e := newEndToEnd(t)
	deleted := e.submit("checkpoint.recorded", checkpoint("deleted before the rebuild"), "internal")
	kept := e.submit("checkpoint.recorded", checkpoint("survives the rebuild"), "internal")
	e.drain()

	if _, err := e.store.IssueTombstone(e.ctx, outbox.Tombstone{
		SubjectType: "checkpoint", SubjectID: deleted.String(), ProjectID: e.project,
		ReasonClass: "user_request", IssuedBy: e.principal, ErasureRequired: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Throw the collection away, then replay every event from the ledger.
	if err := e.provider.DeleteCollection(e.ctx, e.project.String()); err != nil {
		t.Fatalf("discard the collection: %v", err)
	}
	rows, err := e.pool.Pgx().Query(e.ctx,
		`SELECT event_id, seq FROM events WHERE project_id = $1 ORDER BY seq`, e.project)
	if err != nil {
		t.Fatal(err)
	}
	type replay struct {
		id  uuid.UUID
		seq int64
	}
	var all []replay
	for rows.Next() {
		var r replay
		if err := rows.Scan(&r.id, &r.seq); err != nil {
			t.Fatal(err)
		}
		all = append(all, r)
	}
	rows.Close()
	for _, r := range all {
		e.enqueue(r.id, r.seq)
	}
	e.drain()

	// The applier is the one that was running before the collection was
	// dropped, deliberately: a worker that had cached the collection's
	// existence must recover rather than fail identically forever. The first
	// attempt fails, the row is retried, and the collection is recreated.
	if got := e.count(); got != 1 {
		t.Fatalf("the rebuild produced %d points, want only the checkpoint that was not deleted", got)
	}
	back, err := e.provider.ListPoints(e.ctx, e.project.String(), map[string]string{"doc_id": deleted.String()})
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 0 {
		t.Fatal("the rebuild resurrected a tombstoned checkpoint")
	}
	survivor, err := e.provider.ListPoints(e.ctx, e.project.String(), map[string]string{"doc_id": kept.String()})
	if err != nil {
		t.Fatal(err)
	}
	if len(survivor) != 1 {
		t.Fatal("the rebuild lost a checkpoint that was never deleted")
	}
	// And the queue records the skip as done, not as a failure: a tombstoned
	// subject that stayed in the dead-letter list would hide real problems.
	if states := e.states(); states["dead_letter"] != 0 || states["failed"] != 0 {
		t.Fatalf("the rebuild left rows unfinished: %v", states)
	}
}

func TestAnUnreachableIndexIsRetriedAndThenDeadLettered(t *testing.T) {
	// The queue's contract at the projection boundary: the canonical event is
	// untouched, the row is visible, and nothing was silently dropped.
	e := newEndToEnd(t)
	applier := projection.New(
		projection.NewPgEvents(e.pool), e.store,
		projection.NewQdrantIndex(deadStore{}), projection.Config{})

	e.submit("checkpoint.recorded", checkpoint("nobody can index this"), "internal")
	cfg := outbox.DefaultWorkerConfig(outbox.ProjectionQdrant, "e2e-worker-dead")
	cfg.MaxBatches = 1
	cfg.Backoff = 0
	cfg.IdleWait = time.Millisecond
	for i := 0; i < outbox.DefaultMaxAttempts; i++ {
		w := outbox.NewWorker(e.store, applier, cfg)
		ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
		if err := w.Run(ctx); err != nil && !strings.Contains(err.Error(), "context") {
			cancel()
			t.Fatalf("worker: %v", err)
		}
		cancel()
	}
	states := e.states()
	if states["dead_letter"] != 1 {
		t.Fatalf("a permanently failing projection did not become visible: %v", states)
	}
	letters, err := e.store.DeadLetters(e.ctx, outbox.ProjectionQdrant)
	if err != nil {
		t.Fatal(err)
	}
	if len(letters) != 1 {
		t.Fatalf("the dead-letter list holds %d rows", len(letters))
	}
	// The canonical event is untouched: a projection that cannot keep up must
	// never change the ledger.
	var events int
	if err := e.pool.Pgx().QueryRow(e.ctx,
		`SELECT count(*) FROM events WHERE project_id = $1`, e.project).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("the ledger holds %d events after a failed projection", events)
	}
	var class string
	if err := e.pool.Pgx().QueryRow(e.ctx,
		`SELECT last_error_class FROM projection_outbox WHERE projection = 'qdrant'`).Scan(&class); err != nil {
		t.Fatal(err)
	}
	if class != "collection_failed" {
		t.Fatalf("the recorded failure class is %q", class)
	}
}

// deadStore is a vector store that refuses everything, standing in for one that
// is down. It is not a mock of Qdrant: it exists to make the queue's failure
// path run against a real worker and a real database.
type deadStore struct{}

func (deadStore) CreateCollection(context.Context, string) error {
	return errUnreachable
}
func (deadStore) Index(context.Context, string, []rag.Document) error { return errUnreachable }
func (deadStore) DeletePoints(context.Context, string, []string) error {
	return errUnreachable
}
func (deadStore) ListPoints(context.Context, string, map[string]string) ([]rag.PointInfo, error) {
	return nil, errUnreachable
}

var errUnreachable = errUnreachableType{}

type errUnreachableType struct{}

func (errUnreachableType) Error() string { return "the vector store is unreachable" }
