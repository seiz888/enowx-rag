// Shutdown, with a write actually in flight and a real projection worker
// actually running.
//
// This is the scenario the whole receipt mechanism exists for and the one it is
// easiest to fake: a test that starts a server, cancels it and observes a clean
// exit has proved that an idle server can stop. What has to be true is that a
// caller whose write was mid-transaction when the signal arrived gets an answer
// -- a committed receipt or a refusal -- rather than a severed connection it
// cannot interpret. A client cut at that moment does not know whether its write
// landed, and the only thing worse than an error is an unanswerable question.
//
// So the request here is genuinely blocked inside PostgreSQL, on a real table
// lock held by a second connection, and the worker is a real outbox worker
// draining real rows.
package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/httpapi"
	"github.com/enowdev/enowx-rag/pkg/memgw/gateway"
	"github.com/enowdev/enowx-rag/pkg/memgw/memgwtest"
	"github.com/enowdev/enowx-rag/pkg/memgw/outbox"
	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
	"github.com/enowdev/enowx-rag/pkg/memgw/principal"
)

// waitUntilBlocked waits until a backend is actually waiting on a lock, rather
// than sleeping a hopeful interval. The difference matters: a sleep that was
// too short would start the shutdown before the request had reached the
// database, and the test would pass without ever having had a write in flight.
func waitUntilBlocked(t *testing.T, pool *pgstore.Pool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int64
		if err := pool.Pgx().QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no backend ever blocked on the table lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// countingApplier is a real Applier that records what it was handed. It writes
// to nothing, which is honest: there is no projection target in this package,
// and a fake that pretended to write to Qdrant would prove less than one that
// admits it counts.
type countingApplier struct {
	applied atomic.Int64
	seen    sync.Map
}

func (a *countingApplier) Apply(_ context.Context, it outbox.Item) error {
	// Idempotent, because the queue is at-least-once and a lease that expires
	// mid-apply hands the same row to somebody else.
	if _, loaded := a.seen.LoadOrStore(it.EventID, true); !loaded {
		a.applied.Add(1)
	}
	return nil
}

func TestShutdownAnswersAnInFlightWriteAndStopsTheWorker(t *testing.T) {
	pool := memgwtest.Pool(t)
	projectID, workspaceID, branchID := uuid.New(), uuid.New(), uuid.New()
	memgwtest.SeedWorkspace(t, pool, projectID, workspaceID)

	store := principal.NewStore(pool)
	p, err := store.CreatePrincipal(t.Context(), principal.Principal{
		Type: principal.TypeAgent, HostID: uuid.New(), AgentID: "writer", DisplayName: "writer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GrantScope(t.Context(), principal.Grant{
		PrincipalID: p.ID, Role: principal.RoleCheckpointWrite, ProjectID: projectID, Provenance: "test",
	}); err != nil {
		t.Fatal(err)
	}
	_, token, err := store.Issue(t.Context(), p.ID, "writer", nil)
	if err != nil {
		t.Fatal(err)
	}

	// The worker: real store, real claiming, real leases.
	applier := &countingApplier{}
	worker := outbox.NewWorker(outbox.NewStore(pool), applier,
		outbox.WorkerConfig{Projection: "qdrant", Owner: "test-worker", IdleWait: 20 * time.Millisecond})
	workerCtx, stopWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()

	r := chi.NewRouter()
	r.Mount(gateway.MountPath, gateway.New(pool).Routes())

	var workerStopped atomic.Bool
	srv := httpapi.NewServer("127.0.0.1:0", r, httpapi.DefaultServerOptions(), httpapi.Hooks{
		StopWorkers: func(ctx context.Context) error {
			stopWorker()
			select {
			case err := <-workerDone:
				if err != nil && !errors.Is(err, context.Canceled) {
					return err
				}
			case <-ctx.Done():
				return errors.New("the worker did not stop inside the shutdown budget")
			}
			workerStopped.Store(true)
			return nil
		},
	})
	ln, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()

	serveCtx, shutdown := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(serveCtx) }()

	// One ordinary write first, drained by the worker. This is what makes the
	// worker in this test a worker rather than a goroutine that was started and
	// never asked to do anything.
	post := func(key string, workID uuid.UUID) (int, []byte) {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"event_id": uuid.New().String(), "idempotency_key": key,
			"project_id": projectID.String(), "workspace_id": workspaceID.String(),
			"work_id": workID.String(), "session_id": "writer:host:sess-1",
			"branch_id": branchID.String(), "type": "work.planned",
			"payload":           map[string]any{"title": key, "reason_class": "planned"},
			"sensitivity_class": "internal", "policy_version": "1.0.0", "schema_version": "1.0.0",
			"occurred_at": time.Now().UTC().Format(time.RFC3339Nano), "writer_epoch": 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, base+gateway.MountPath+"/v1/events", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}
	if status, out := post("writer:plan:first", uuid.New()); status != http.StatusCreated {
		t.Fatalf("first write: %d %s", status, out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for applier.applied.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the worker drained nothing; it is not really running")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Block the event log from a second connection. The submit below will get
	// as far as its INSERT and wait there, which is a request in flight in the
	// only sense that matters: a transaction is open and its outcome is not yet
	// decided.
	blocker, err := pool.Pgx().Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(t.Context(), `LOCK TABLE events IN EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	type result struct {
		status int
		body   []byte
	}
	inFlight := make(chan result, 1)
	go func() {
		status, out := post("writer:plan:blocked", uuid.New())
		inFlight <- result{status: status, body: out}
	}()

	// Give the request time to reach the lock, then start the shutdown while it
	// is still waiting there.
	waitUntilBlocked(t, pool)
	shutdown()

	// Release the lock so the in-flight transaction can finish inside the
	// drain budget. What is being tested is that the server waited for it.
	time.Sleep(50 * time.Millisecond)
	if err := blocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	got := <-inFlight
	if got.status != http.StatusCreated {
		t.Fatalf("the in-flight write was answered %d: %s", got.status, got.body)
	}
	var receipt struct {
		State string `json:"state"`
		Seq   int64  `json:"seq"`
	}
	decodeInto(t, got.body, &receipt)
	if receipt.State != "committed" || receipt.Seq == 0 {
		t.Fatalf("the drained request got an unusable receipt: %+v", receipt)
	}

	if err := <-serveErr; err != nil {
		t.Fatalf("shutdown reported %v", err)
	}
	if !workerStopped.Load() {
		t.Fatal("the worker did not stop during shutdown")
	}

	// The write is durable and its outbox row exists whether or not the worker
	// got to it: the commit and the enqueue were one transaction, so a
	// shutdown between them is not a state the database can be in.
	var events, queued int64
	if err := pool.Pgx().QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM events), (SELECT count(*) FROM projection_outbox)`).Scan(&events, &queued); err != nil {
		t.Fatal(err)
	}
	if events != 2 || queued != 2 {
		t.Fatalf("after shutdown: %d events, %d outbox rows", events, queued)
	}
}
