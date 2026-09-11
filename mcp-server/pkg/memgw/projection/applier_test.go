// These tests are about what the applier decides, not about Qdrant. Each one
// names the mistake it exists to catch: a candidate reaching the index, a
// confidential checkpoint being embedded, a rebuild resurrecting a deleted
// checkpoint, an older event overwriting a newer projection.
//
// The Qdrant half is exercised separately, against a real disposable container,
// in qdrant_integration_test.go.
package projection

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/domain"
	"github.com/enowdev/enowx-rag/pkg/memgw/outbox"
	"github.com/enowdev/enowx-rag/pkg/rag"
)

// ---------------------------------------------------------------------------
// Doubles
// ---------------------------------------------------------------------------

type fakeEvents map[uuid.UUID]Record

func (f fakeEvents) Event(_ context.Context, id uuid.UUID) (Record, error) {
	r, ok := f[id]
	if !ok {
		return Record{}, ErrEventGone
	}
	return r, nil
}

type fakeDeletions struct {
	tombstoned map[string]bool // "type/id"
	chunks     []string
	err        error
}

func (f *fakeDeletions) IsTombstoned(_ context.Context, subjectType, subjectID string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.tombstoned[subjectType+"/"+subjectID], nil
}

func (f *fakeDeletions) TombstonedChunkIDs(_ context.Context, _ uuid.UUID) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.chunks, nil
}

// fakeIndex is an in-memory stand-in that keeps enough structure for the
// filters to mean something: documents keyed by id, with their metadata.
type fakeIndex struct {
	docs        map[string]rag.Document
	collections []string
	upserts     int
	deletes     int
	failUpsert  error
}

func newFakeIndex() *fakeIndex { return &fakeIndex{docs: map[string]rag.Document{}} }

func (f *fakeIndex) EnsureCollection(_ context.Context, projectID string) error {
	for _, c := range f.collections {
		if c == projectID {
			return nil
		}
	}
	f.collections = append(f.collections, projectID)
	return nil
}

func (f *fakeIndex) Upsert(_ context.Context, _ string, docs []rag.Document) error {
	if f.failUpsert != nil {
		return f.failUpsert
	}
	for _, d := range docs {
		f.upserts++
		f.docs[d.ID] = d
	}
	return nil
}

func (f *fakeIndex) Delete(_ context.Context, _ string, ids []string) error {
	for _, id := range ids {
		if _, ok := f.docs[id]; ok {
			f.deletes++
			delete(f.docs, id)
		}
	}
	return nil
}

func (f *fakeIndex) Lookup(_ context.Context, _, docID string) (rag.PointInfo, bool, error) {
	d, ok := f.docs[docID]
	if !ok {
		return rag.PointInfo{}, false, nil
	}
	return rag.PointInfo{ID: d.ID, DocID: d.ID, Content: d.Content, Meta: d.Meta}, true, nil
}

func (f *fakeIndex) DeleteMatching(_ context.Context, _ string, meta map[string]string) (int, error) {
	n := 0
	for id, d := range f.docs {
		match := true
		for k, v := range meta {
			if d.Meta[k] != v {
				match = false
				break
			}
		}
		if match {
			delete(f.docs, id)
			f.deletes++
			n++
		}
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

var (
	fixProject   = uuid.MustParse("7627423a-fd6e-459d-87c0-be32cd47c8cb")
	fixWorkspace = uuid.MustParse("20bc010b-9676-4d03-a6de-9812bf195f2f")
	fixPrincipal = uuid.MustParse("159fdcc6-8f08-4cb9-b57b-f62d6b17db69")
	fixWork      = uuid.MustParse("2f3f1f9a-4f2e-4a51-9a7e-3b1a1b2c3d4e")
	fixBranch    = uuid.MustParse("5b1d2c3e-6f70-4a81-9b2c-3d4e5f607182")
)

func record(t *testing.T, typ string, seq int64, payload any) Record {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	work := fixWork
	return Record{
		EventID:     uuid.New(),
		Seq:         seq,
		Type:        typ,
		ProjectID:   fixProject,
		WorkspaceID: fixWorkspace,
		WorkID:      &work,
		SessionID:   "test:sess",
		BranchID:    fixBranch,
		PrincipalID: fixPrincipal,
		Sensitivity: "internal",
		Payload:     raw,
		OccurredAt:  time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
	}
}

// checkpointPayload is a sanitised synthetic checkpoint. Nothing in this file
// is taken from a real session: the corpus is written here so a test fixture
// can never become a route by which real content reaches a vector store.
func checkpointPayload(objective string) map[string]any {
	return map[string]any{
		"objective":        objective,
		"completed_work":   "wrote the projection applier and its tests",
		"pending_actions":  "wire the worker to the disposable container",
		"blockers":         []string{"none"},
		"next_safe_action": "run the suite against memgw-qdrant",
		"modified_files":   []string{"pkg/memgw/projection/applier.go"},
	}
}

type harness struct {
	events    fakeEvents
	deletions *fakeDeletions
	index     *fakeIndex
	applier   *Applier
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	h := &harness{
		events:    fakeEvents{},
		deletions: &fakeDeletions{tombstoned: map[string]bool{}},
		index:     newFakeIndex(),
	}
	h.applier = New(h.events, h.deletions, h.index, cfg)
	return h
}

func (h *harness) apply(t *testing.T, r Record) error {
	t.Helper()
	h.events[r.EventID] = r
	return h.applier.Apply(context.Background(), outbox.Item{
		EventID: r.EventID, EventSeq: r.Seq, Projection: outbox.ProjectionQdrant,
	})
}

// ---------------------------------------------------------------------------
// What reaches the index
// ---------------------------------------------------------------------------

func TestACheckpointBecomesOneSearchableDocument(t *testing.T) {
	h := newHarness(t, Config{})
	r := record(t, "checkpoint.recorded", 10, checkpointPayload("finish the projection applier"))
	if err := h.apply(t, r); err != nil {
		t.Fatal(err)
	}
	doc, ok := h.index.docs[r.EventID.String()]
	if !ok {
		t.Fatal("the checkpoint was not indexed under its event id")
	}
	for _, want := range []string{"finish the projection applier", "Next safe action", "wrote the projection applier"} {
		if !strings.Contains(doc.Content, want) {
			t.Fatalf("the projected document does not contain %q:\n%s", want, doc.Content)
		}
	}
	if doc.Meta["kind"] != "checkpoint" || doc.Meta["event_seq"] != "10" {
		t.Fatalf("the document does not carry the metadata a rebuild needs: %v", doc.Meta)
	}
	if doc.Meta["work_id"] != fixWork.String() || doc.Meta["project_id"] != fixProject.String() {
		t.Fatalf("the document does not carry its scope: %v", doc.Meta)
	}
}

func TestAProposedCandidateNeverReachesTheIndex(t *testing.T) {
	// The rule this defends: retrieval may only be built out of what the ledger
	// decided is true. A candidate is a proposal, and a search result cannot
	// carry the distinction.
	h := newHarness(t, Config{})
	err := h.apply(t, record(t, "fact.candidate_proposed", 11, map[string]any{
		"candidate_id": uuid.New(), "subject": "gateway", "predicate": "port",
		"object": 7777, "cardinality": "single", "source": "test", "extractor": "hand",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.index.docs) != 0 {
		t.Fatalf("a candidate was indexed: %v", h.index.docs)
	}
}

func TestEventsWithNoProjectionAreDoneNotFailed(t *testing.T) {
	// Every event gets an outbox row. The ones with nothing to project must be
	// applied, not retried and not dead-lettered, or the dead-letter list stops
	// meaning "something needs a human".
	h := newHarness(t, Config{})
	for _, typ := range []string{
		"work.planned", "work.activated", "work.completed", "evidence.recorded",
		"session.started", "session.ended", "fact.conflict_flagged",
		"writer_epoch.opened", "projection.rebuild_requested",
	} {
		if err := h.apply(t, record(t, typ, 12, map[string]any{"title": "t", "reason_class": "r"})); err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
	}
	if len(h.index.docs) != 0 {
		t.Fatalf("an event with no projection wrote to the index: %v", h.index.docs)
	}
	if len(h.index.collections) != 0 {
		t.Fatal("an event with no projection created a collection")
	}
}

func TestAMissingEventIsReportedAndNotSilentlyApplied(t *testing.T) {
	h := newHarness(t, Config{})
	err := h.applier.Apply(context.Background(), outbox.Item{EventID: uuid.New(), Projection: outbox.ProjectionQdrant})
	if err == nil {
		t.Fatal("an outbox row pointing at no event was treated as applied")
	}
	var classed *Error
	if !errors.As(err, &classed) || classed.Class() != "event_missing" {
		t.Fatalf("the failure is not classified as a missing event: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Sensitivity
// ---------------------------------------------------------------------------

func TestConfidentialContentIsNotEmbedded(t *testing.T) {
	// Indexing means embedding, and embedding means the text leaves the
	// process. The gate is on the class recorded on the event, which the
	// submitter cannot revise after the fact.
	h := newHarness(t, Config{})
	for _, class := range []string{"confidential", "restricted"} {
		r := record(t, "checkpoint.recorded", 20, checkpointPayload("handle the "+class+" case"))
		r.Sensitivity = class
		if err := h.apply(t, r); err != nil {
			t.Fatalf("%s: withholding must be success, not failure: %v", class, err)
		}
	}
	if len(h.index.docs) != 0 {
		t.Fatalf("content above the ceiling was indexed: %v", h.index.docs)
	}
	if h.index.upserts != 0 {
		t.Fatal("an upsert was attempted for withheld content")
	}
}

func TestAnUnknownSensitivityClassIsWithheld(t *testing.T) {
	// Fail closed. A class this build does not recognise is one a newer
	// contract added, and guessing that it is safe to embed is the wrong guess
	// to make by default.
	h := newHarness(t, Config{})
	r := record(t, "checkpoint.recorded", 21, checkpointPayload("unknown class"))
	r.Sensitivity = "top-secret-ish"
	if err := h.apply(t, r); err != nil {
		t.Fatal(err)
	}
	if len(h.index.docs) != 0 {
		t.Fatal("an unrecognised sensitivity class was treated as safe to embed")
	}
}

func TestTheCeilingCanBeRaisedDeliberately(t *testing.T) {
	h := newHarness(t, Config{MaxSensitivity: "confidential"})
	r := record(t, "checkpoint.recorded", 22, checkpointPayload("raised ceiling"))
	r.Sensitivity = "confidential"
	if err := h.apply(t, r); err != nil {
		t.Fatal(err)
	}
	if len(h.index.docs) != 1 {
		t.Fatal("raising the ceiling did not admit confidential content")
	}
	r2 := record(t, "checkpoint.recorded", 23, checkpointPayload("still refused"))
	r2.Sensitivity = "restricted"
	if err := h.apply(t, r2); err != nil {
		t.Fatal(err)
	}
	if len(h.index.docs) != 1 {
		t.Fatal("restricted content was admitted by a confidential ceiling")
	}
}

// ---------------------------------------------------------------------------
// Tombstones
// ---------------------------------------------------------------------------

func TestARebuildDoesNotResurrectADeletedCheckpoint(t *testing.T) {
	// The scenario the deletion journal exists for: somebody ordered a
	// checkpoint deleted, and later the projection is rebuilt from events that
	// are all older than the order.
	h := newHarness(t, Config{})
	r := record(t, "checkpoint.recorded", 30, checkpointPayload("delete me"))
	h.deletions.tombstoned["checkpoint/"+r.EventID.String()] = true
	err := h.apply(t, r)
	if !errors.Is(err, outbox.ErrSkippedTombstoned) {
		t.Fatalf("a tombstoned checkpoint was not skipped: %v", err)
	}
	if len(h.index.docs) != 0 {
		t.Fatal("a tombstoned checkpoint was written back into the index")
	}
}

func TestATombstonedWorkStopsItsCheckpoints(t *testing.T) {
	// A deletion is usually ordered against the work, not against each
	// checkpoint of it, so the check has to climb.
	h := newHarness(t, Config{})
	h.deletions.tombstoned["work/"+fixWork.String()] = true
	err := h.apply(t, record(t, "checkpoint.recorded", 31, checkpointPayload("belongs to a deleted work")))
	if !errors.Is(err, outbox.ErrSkippedTombstoned) {
		t.Fatalf("a checkpoint of a tombstoned work was projected: %v", err)
	}
}

func TestATombstoneRemovesWhatItNames(t *testing.T) {
	h := newHarness(t, Config{})
	cp := record(t, "checkpoint.recorded", 32, checkpointPayload("about to be erased"))
	if err := h.apply(t, cp); err != nil {
		t.Fatal(err)
	}
	if len(h.index.docs) != 1 {
		t.Fatal("setup: the checkpoint was not indexed")
	}
	tomb := record(t, "tombstone.issued", 33, domain.TombstonePayload{
		SubjectType: "checkpoint", SubjectID: cp.EventID.String(),
		ReasonClass: "user_request", ErasureRequired: true,
	})
	if err := h.apply(t, tomb); err != nil {
		t.Fatal(err)
	}
	if len(h.index.docs) != 0 {
		t.Fatalf("the tombstone did not remove the document: %v", h.index.docs)
	}
}

func TestATombstoneReachesChunksWrittenBeforeTheGatewayExisted(t *testing.T) {
	// The migration case. Points indexed by the old pipeline have ids this
	// gateway never derived, so the legacy chunk map is the only thing that
	// connects a deletion order to them.
	h := newHarness(t, Config{})
	h.index.docs["legacy-chunk-a"] = rag.Document{ID: "legacy-chunk-a"}
	h.index.docs["legacy-chunk-b"] = rag.Document{ID: "legacy-chunk-b"}
	h.deletions.chunks = []string{"legacy-chunk-b"}
	tomb := record(t, "tombstone.issued", 34, domain.TombstonePayload{
		SubjectType: "document", SubjectID: "notes/old.md",
		ReasonClass: "user_request", ErasureRequired: true,
		LegacyChunkIDs: []string{"legacy-chunk-a"},
	})
	if err := h.apply(t, tomb); err != nil {
		t.Fatal(err)
	}
	if len(h.index.docs) != 0 {
		t.Fatalf("legacy chunks survived the deletion: %v", h.index.docs)
	}
}

func TestATombstoneForAnUnknownSubjectClassIsVisible(t *testing.T) {
	// Silently succeeding on a deletion nobody knows how to carry out is the
	// failure mode where content stays searchable and the queue says it is done.
	h := newHarness(t, Config{})
	err := h.apply(t, record(t, "tombstone.issued", 35, map[string]any{
		"subject_type": "constellation", "subject_id": "x", "reason_class": "user_request",
	}))
	var classed *Error
	if !errors.As(err, &classed) || classed.Class() != "unknown_subject_type" {
		t.Fatalf("an unhandled tombstone subject class was not reported: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Facts
// ---------------------------------------------------------------------------

func promotion(subject, predicate, object, cardinality string, factID uuid.UUID) domain.PromotionPayload {
	obj, _ := json.Marshal(object)
	return domain.PromotionPayload{
		CandidateID: uuid.New(), FactID: &factID, Subject: subject, Predicate: predicate,
		Object: obj, Cardinality: cardinality, EvidenceClass: "observed",
	}
}

func TestASingleValuedSlotHoldsOnlyTheValueThatStands(t *testing.T) {
	// A document per historical value would let a search return a superseded
	// answer beside the current one with nothing to tell them apart.
	h := newHarness(t, Config{})
	first := uuid.New()
	if err := h.apply(t, record(t, "fact.promoted", 40, promotion("gateway", "listen_port", "7777", "single", first))); err != nil {
		t.Fatal(err)
	}
	if err := h.apply(t, record(t, "fact.promoted", 41, promotion("gateway", "listen_port", "7788", "single", uuid.New()))); err != nil {
		t.Fatal(err)
	}
	if len(h.index.docs) != 1 {
		t.Fatalf("a superseded value stayed searchable: %v", h.index.docs)
	}
	for _, d := range h.index.docs {
		if !strings.Contains(d.Content, "7788") || strings.Contains(d.Content, "7777") {
			t.Fatalf("the slot does not hold the current value: %s", d.Content)
		}
	}
}

func TestAMultiValuedSlotKeepsItsValuesApart(t *testing.T) {
	h := newHarness(t, Config{})
	if err := h.apply(t, record(t, "fact.promoted", 42, promotion("host", "runs_agent", "claude-code", "multi", uuid.New()))); err != nil {
		t.Fatal(err)
	}
	if err := h.apply(t, record(t, "fact.promoted", 43, promotion("host", "runs_agent", "codex", "multi", uuid.New()))); err != nil {
		t.Fatal(err)
	}
	if len(h.index.docs) != 2 {
		t.Fatalf("a multi-valued slot collapsed to one document: %v", h.index.docs)
	}
	// Re-promoting the same value must land on the same document rather than
	// adding a third.
	if err := h.apply(t, record(t, "fact.promoted", 44, promotion("host", "runs_agent", "codex", "multi", uuid.New()))); err != nil {
		t.Fatal(err)
	}
	if len(h.index.docs) != 2 {
		t.Fatalf("re-promoting a value added a copy: %v", h.index.docs)
	}
}

func TestARetractedFactStopsBeingSearchable(t *testing.T) {
	h := newHarness(t, Config{})
	factID := uuid.New()
	if err := h.apply(t, record(t, "fact.promoted", 45, promotion("gateway", "listen_port", "7777", "single", factID))); err != nil {
		t.Fatal(err)
	}
	if err := h.apply(t, record(t, "fact.retracted", 46, domain.FactChangePayload{
		FactID: &factID, Subject: "gateway", Predicate: "listen_port", ReasonClass: "user_request",
	})); err != nil {
		t.Fatal(err)
	}
	if len(h.index.docs) != 0 {
		t.Fatalf("a retracted fact is still searchable: %v", h.index.docs)
	}
}

func TestARetractionThatNamesNothingFailsLoudly(t *testing.T) {
	h := newHarness(t, Config{})
	err := h.apply(t, record(t, "fact.retracted", 47, domain.FactChangePayload{ReasonClass: "user_request"}))
	var classed *Error
	if !errors.As(err, &classed) || classed.Class() != "payload_unreadable" {
		t.Fatalf("a retraction that identifies nothing was accepted: %v", err)
	}
}

func TestAPromotionRemovesTheFactsItSupersedes(t *testing.T) {
	h := newHarness(t, Config{})
	old := uuid.New()
	if err := h.apply(t, record(t, "fact.promoted", 48, promotion("host", "runs_agent", "droid", "multi", old))); err != nil {
		t.Fatal(err)
	}
	p := promotion("host", "runs_agent", "hermes", "multi", uuid.New())
	p.SupersedesFacts = []uuid.UUID{old}
	if err := h.apply(t, record(t, "fact.promoted", 49, p)); err != nil {
		t.Fatal(err)
	}
	if len(h.index.docs) != 1 {
		t.Fatalf("the superseded value was left in the index: %v", h.index.docs)
	}
}

// ---------------------------------------------------------------------------
// Idempotence and ordering
// ---------------------------------------------------------------------------

func TestApplyingTheSameEventTwiceChangesNothing(t *testing.T) {
	// The outbox is at-least-once, so this happens in normal operation, not
	// only after a crash.
	h := newHarness(t, Config{})
	r := record(t, "checkpoint.recorded", 50, checkpointPayload("applied twice"))
	if err := h.apply(t, r); err != nil {
		t.Fatal(err)
	}
	firstDoc := h.index.docs[r.EventID.String()]
	if err := h.apply(t, r); err != nil {
		t.Fatal(err)
	}
	if len(h.index.docs) != 1 {
		t.Fatalf("a re-application added a document: %v", h.index.docs)
	}
	if h.index.docs[r.EventID.String()].Content != firstDoc.Content {
		t.Fatal("a re-application changed the document")
	}
}

func TestAnOlderEventDoesNotOverwriteANewerProjection(t *testing.T) {
	// Two workers, or a rebuild running beside live traffic: the older event
	// must not move the index backwards.
	h := newHarness(t, Config{})
	newer := promotion("gateway", "listen_port", "7788", "single", uuid.New())
	older := promotion("gateway", "listen_port", "7777", "single", uuid.New())
	if err := h.apply(t, record(t, "fact.promoted", 60, newer)); err != nil {
		t.Fatal(err)
	}
	if err := h.apply(t, record(t, "fact.promoted", 59, older)); err != nil {
		t.Fatal(err)
	}
	for _, d := range h.index.docs {
		if strings.Contains(d.Content, "7777") {
			t.Fatalf("an older event overwrote a newer projection: %s", d.Content)
		}
	}
	if h.index.upserts != 1 {
		t.Fatalf("the stale write was not skipped: %d upserts", h.index.upserts)
	}
}

func TestAFailedIndexWriteIsClassifiedAndCarriesNoContent(t *testing.T) {
	// The queue stores the class of a failure. A message built from the failing
	// document would put content into the queue, which is the one place it must
	// never be.
	h := newHarness(t, Config{})
	h.index.failUpsert = errors.New("connection refused while writing: " + strings.Repeat("SECRET", 3))
	err := h.apply(t, record(t, "checkpoint.recorded", 61, checkpointPayload("a failing write")))
	var classed *Error
	if !errors.As(err, &classed) {
		t.Fatalf("an index failure was not classified: %v", err)
	}
	if classed.Class() != "index_write_failed" {
		t.Fatalf("the class is %q", classed.Class())
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "a failing write") {
		t.Fatalf("the failure carries the content that failed: %v", err)
	}
}

func TestTheApplierSatisfiesTheOutboxInterface(t *testing.T) {
	// A compile-time claim, asserted here so it is a test failure rather than a
	// build failure in a package that happens to wire them together.
	var _ outbox.Applier = (*Applier)(nil)
}
