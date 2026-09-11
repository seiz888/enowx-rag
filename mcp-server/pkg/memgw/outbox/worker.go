package outbox

import (
	"context"
	"errors"
	"time"
)

// Applier writes one event into one projection. It is an interface because
// Phase 3 has no projection to write to: the crash and retry behaviour has to be
// testable before Qdrant or Graphify are wired in, and wiring them in must not
// change the queue semantics.
//
// An Applier must be idempotent. The queue is at-least-once, so applying the
// same event twice will happen and must be a no-op the second time.
type Applier interface {
	Apply(ctx context.Context, it Item) error
}

// WorkerConfig bounds a worker. Every field has a limit on purpose: an
// unbounded projection worker against a shared cluster is a way to starve the
// neighbour that shares it.
type WorkerConfig struct {
	Projection Projection
	Owner      string
	Lease      time.Duration
	BatchSize  int
	Backoff    time.Duration
	IdleWait   time.Duration
	MaxBatches int // 0 means "until the context ends"
}

// DefaultWorkerConfig returns conservative settings for one projection.
func DefaultWorkerConfig(p Projection, owner string) WorkerConfig {
	return WorkerConfig{
		Projection: p,
		Owner:      owner,
		Lease:      30 * time.Second,
		BatchSize:  16,
		Backoff:    5 * time.Second,
		IdleWait:   time.Second,
	}
}

// Worker drains the outbox into an Applier.
type Worker struct {
	store *Store
	apply Applier
	cfg   WorkerConfig
}

// NewWorker returns a worker. It does not start anything; Run does.
func NewWorker(store *Store, apply Applier, cfg WorkerConfig) *Worker {
	if cfg.Lease <= 0 {
		cfg.Lease = 30 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 16
	}
	if cfg.IdleWait <= 0 {
		cfg.IdleWait = time.Second
	}
	return &Worker{store: store, apply: apply, cfg: cfg}
}

// Run drains until the context ends or MaxBatches batches have been processed.
//
// Failure handling is deliberately dull: a failed apply is retried after a
// backoff and eventually dead-lettered. Nothing is dropped, and nothing about
// the canonical event changes because a projection could not keep up.
func (w *Worker) Run(ctx context.Context) error {
	batches := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if w.cfg.MaxBatches > 0 && batches >= w.cfg.MaxBatches {
			return nil
		}
		batches++

		if _, err := w.store.ReclaimExpiredLeases(ctx); err != nil {
			return err
		}
		items, err := w.store.Claim(ctx, w.cfg.Projection, w.cfg.Owner, w.cfg.Lease, w.cfg.BatchSize)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(w.cfg.IdleWait):
			}
			continue
		}
		for _, it := range items {
			if err := w.applyOne(ctx, it); err != nil {
				return err
			}
		}
	}
}

func (w *Worker) applyOne(ctx context.Context, it Item) error {
	err := w.apply.Apply(ctx, it)
	switch {
	case err == nil:
		return w.store.MarkApplied(ctx, it)
	case errors.Is(err, ErrSkippedTombstoned):
		// Declining to project a tombstoned subject is success: the row has
		// been dealt with, and retrying it would only decline again. This is
		// how a tombstone beats a rebuild that is replaying older events.
		return w.store.MarkApplied(ctx, it)
	default:
		// Only a class is recorded. The error may quote the content that failed
		// to project, and the queue is not a place for content.
		if markErr := w.store.MarkFailed(ctx, it, classOf(err), w.cfg.Backoff); markErr != nil {
			return markErr
		}
		return nil
	}
}

// classOf reduces an apply error to a short class, never its text.
func classOf(err error) string {
	if err == nil {
		return ""
	}
	if c, ok := err.(interface{ Class() string }); ok {
		return c.Class()
	}
	return "apply_failed"
}

// ErrSkippedTombstoned is what an Applier returns when it declines to project a
// subject that has been ordered deleted. It is not a failure: the row is done.
var ErrSkippedTombstoned = errors.New("memgw: subject is tombstoned; nothing was projected")
