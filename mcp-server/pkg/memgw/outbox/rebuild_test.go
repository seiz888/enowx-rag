package outbox_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/outbox"
)

// TestRebuildRequeuesAppliedRows is the recovery path: the index is a cache, so
// losing it must be repairable from the events that are still in the ledger.
func TestRebuildRequeuesAppliedRows(t *testing.T) {
	e := newEnv(t)
	e.enqueue(3)

	items, err := e.store.Claim(e.ctx, outbox.ProjectionQdrant, "worker-1", time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if err := e.store.MarkApplied(e.ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	before, err := e.store.Watermark(e.ctx, outbox.ProjectionQdrant)
	if err != nil {
		t.Fatal(err)
	}

	r, err := e.store.Rebuild(e.ctx, outbox.ProjectionQdrant, e.project)
	if err != nil {
		t.Fatal(err)
	}
	if r.Requeued != 3 || r.Restored != 0 {
		t.Fatalf("rebuild queued %d and restored %d, want 3 and 0", r.Requeued, r.Restored)
	}
	// The watermark records how far the projection has been applied at least
	// once. A replay of older events does not make that less true, and moving
	// it backwards would tell a reader the projection is less complete than it
	// is.
	if r.Watermark != before {
		t.Fatalf("rebuild moved the watermark from %d to %d", before, r.Watermark)
	}
	for _, it := range items {
		if st, attempts := e.state(it.OutboxID); st != "pending" || attempts != 0 {
			t.Fatalf("row %d is %s with %d attempts after a rebuild", it.OutboxID, st, attempts)
		}
	}
	again, err := e.store.Claim(e.ctx, outbox.ProjectionQdrant, "worker-2", time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 3 {
		t.Fatalf("a rebuild left %d rows claimable, want 3", len(again))
	}
}

// TestRebuildRestoresAMissingOutboxRow: a restore that lost the queue but kept
// the events must still be repairable, or the ledger would be the truth in
// theory only.
func TestRebuildRestoresAMissingOutboxRow(t *testing.T) {
	e := newEnv(t)
	ids := e.enqueue(2)
	if _, err := e.pool.Pgx().Exec(e.ctx,
		`DELETE FROM projection_outbox WHERE event_id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}

	r, err := e.store.Rebuild(e.ctx, outbox.ProjectionQdrant, e.project)
	if err != nil {
		t.Fatal(err)
	}
	if r.Restored != 1 {
		t.Fatalf("restored %d rows, want 1", r.Restored)
	}
	var n int
	if err := e.pool.Pgx().QueryRow(e.ctx,
		`SELECT count(*) FROM projection_outbox WHERE event_id = $1`, ids[0]).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d rows for the deleted event, want 1", n)
	}
}

// TestRebuildLeavesInFlightRowsAlone: resetting a row another worker holds a
// lease on would put two workers on the same event.
func TestRebuildLeavesInFlightRowsAlone(t *testing.T) {
	e := newEnv(t)
	e.enqueue(2)
	items, err := e.store.Claim(e.ctx, outbox.ProjectionQdrant, "worker-1", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("claimed %d rows, want 1", len(items))
	}

	if _, err := e.store.Rebuild(e.ctx, outbox.ProjectionQdrant, e.project); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.state(items[0].OutboxID); st != "in_flight" {
		t.Fatalf("the leased row is %s after a rebuild", st)
	}
}

// TestRebuildScopeIsOneProject: an operator repairing one project must not
// re-queue every other project on the same ledger.
func TestRebuildScopeIsOneProject(t *testing.T) {
	e := newEnv(t)
	e.enqueue(2)
	items, err := e.store.Claim(e.ctx, outbox.ProjectionQdrant, "worker-1", time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if err := e.store.MarkApplied(e.ctx, it); err != nil {
			t.Fatal(err)
		}
	}

	r, err := e.store.Rebuild(e.ctx, outbox.ProjectionQdrant, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if r.Requeued != 0 || r.Restored != 0 {
		t.Fatalf("a rebuild scoped to another project touched %d rows", r.Requeued+r.Restored)
	}
	for _, it := range items {
		if st, _ := e.state(it.OutboxID); st != "applied" {
			t.Fatalf("row %d is %s", it.OutboxID, st)
		}
	}
}
