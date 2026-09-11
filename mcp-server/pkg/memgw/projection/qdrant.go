package projection

import (
	"context"
	"fmt"
	"sync"

	"github.com/enowdev/enowx-rag/pkg/rag"
)

// VectorStore is the part of rag.Provider this package uses. Naming it here
// keeps the applier honest about how much of the RAG surface a projection is
// allowed to touch: it creates collections, writes points, reads points back
// and deletes them. It never searches, and it never deletes a collection.
type VectorStore interface {
	CreateCollection(ctx context.Context, projectID string) error
	Index(ctx context.Context, projectID string, docs []rag.Document) error
	DeletePoints(ctx context.Context, projectID string, pointIDs []string) error
	ListPoints(ctx context.Context, projectID string, metaFilter map[string]string) ([]rag.PointInfo, error)
}

// QdrantIndex adapts a rag vector store to the Index this package needs.
//
// The only state it holds is which collections it is satisfied exist, so a
// batch of a hundred checkpoints in one project does not make a hundred
// round-trips. The cache is an optimisation and never an authority: a failed
// write clears it, so a collection that was dropped underneath a running worker
// is recreated on the retry rather than making every subsequent write fail.
type QdrantIndex struct {
	store VectorStore

	mu      sync.Mutex
	created map[string]bool
}

// NewQdrantIndex wraps a vector store.
func NewQdrantIndex(store VectorStore) *QdrantIndex {
	return &QdrantIndex{store: store, created: map[string]bool{}}
}

// EnsureCollection makes sure the project's collection exists.
//
// Qdrant refuses to create a collection that is already there -- it answers
// "Collection ... already exists!", not a no-op -- so a second worker, or a
// restart, cannot simply call create and ignore the result. Existence is
// probed instead, and the create is only attempted when the probe says the
// collection is absent. A create that loses a race with another worker is
// re-probed rather than reported: two workers agreeing that the collection
// should exist is not a failure.
//
// The probe is a filtered scroll that matches nothing. It is the cheapest
// call the provider exposes that distinguishes "empty" from "absent" --
// CountPoints deliberately reports a missing collection as zero points, which
// is right for a caller asking how much is stored and useless for a caller
// asking whether the collection is there.
func (q *QdrantIndex) EnsureCollection(ctx context.Context, projectID string) error {
	q.mu.Lock()
	done := q.created[projectID]
	q.mu.Unlock()
	if done {
		return nil
	}
	if q.exists(ctx, projectID) {
		q.remember(projectID)
		return nil
	}
	if err := q.store.CreateCollection(ctx, projectID); err != nil {
		if q.exists(ctx, projectID) {
			q.remember(projectID)
			return nil
		}
		return err
	}
	q.remember(projectID)
	return nil
}

// probeFilter matches no document this package ever writes, so the scroll
// returns an empty page from an existing collection and an error from an
// absent one.
var probeFilter = map[string]string{"memgw": "collection-existence-probe"}

func (q *QdrantIndex) exists(ctx context.Context, projectID string) bool {
	_, err := q.store.ListPoints(ctx, projectID, probeFilter)
	return err == nil
}

func (q *QdrantIndex) remember(projectID string) {
	q.mu.Lock()
	q.created[projectID] = true
	q.mu.Unlock()
}

func (q *QdrantIndex) forget(projectID string) {
	q.mu.Lock()
	delete(q.created, projectID)
	q.mu.Unlock()
}

// Upsert writes documents. rag.Index embeds and upserts by point id, and the
// ids this package generates are UUIDs, so the same document written twice
// replaces itself instead of appearing twice.
func (q *QdrantIndex) Upsert(ctx context.Context, projectID string, docs []rag.Document) error {
	err := q.store.Index(ctx, projectID, docs)
	if err != nil {
		// The most likely reason a write fails against a collection this
		// process believed in is that the collection is no longer there -- a
		// rebuild dropped it, or somebody cleaned up. Forgetting it makes the
		// retry recreate it instead of failing identically forever.
		q.forget(projectID)
	}
	return err
}

// Delete removes documents by id.
func (q *QdrantIndex) Delete(ctx context.Context, projectID string, docIDs []string) error {
	if len(docIDs) == 0 {
		return nil
	}
	return q.store.DeletePoints(ctx, projectID, docIDs)
}

// Lookup returns the document held under an id.
//
// It filters on the doc_id payload field rather than fetching the point by its
// id, because the provider owns the mapping from a document id to a point id
// and reproducing that mapping here would be a second copy of a rule that has
// to stay identical.
func (q *QdrantIndex) Lookup(ctx context.Context, projectID, docID string) (rag.PointInfo, bool, error) {
	pts, err := q.store.ListPoints(ctx, projectID, map[string]string{"doc_id": docID})
	if err != nil {
		// Same reasoning as Upsert: the collection this process believed in may
		// be gone. Forgetting it turns a permanent failure into a retry that
		// recreates it.
		q.forget(projectID)
		return rag.PointInfo{}, false, err
	}
	if len(pts) == 0 {
		return rag.PointInfo{}, false, nil
	}
	if len(pts) > 1 {
		// Two points claiming the same document id means the id derivation is
		// no longer deterministic, and every idempotence claim in this package
		// rests on it being deterministic. Failing is the only honest answer.
		return rag.PointInfo{}, false, fmt.Errorf("memgw projection: %d points share one document id", len(pts))
	}
	return pts[0], true, nil
}

// DeleteMatching removes every document whose payload matches meta.
//
// Qdrant can delete by filter in one call. This goes through a scroll and then
// a delete by id on purpose: the count of what was removed is what a deletion
// runbook needs to reconcile against, and a filter delete reports a status
// rather than a list.
func (q *QdrantIndex) DeleteMatching(ctx context.Context, projectID string, meta map[string]string) (int, error) {
	if len(meta) == 0 {
		// A filter that matches everything would empty the collection. It is
		// never what a caller means, so it is refused rather than obeyed.
		return 0, fmt.Errorf("memgw projection: a deletion filter must name at least one field")
	}
	pts, err := q.store.ListPoints(ctx, projectID, meta)
	if err != nil {
		return 0, err
	}
	if len(pts) == 0 {
		return 0, nil
	}
	ids := make([]string, len(pts))
	for i, pt := range pts {
		ids[i] = pt.ID
	}
	if err := q.store.DeletePoints(ctx, projectID, ids); err != nil {
		return 0, err
	}
	return len(ids), nil
}
