package migrate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// sliceSource hands over records the test wrote, in the order it wrote them.
type sliceSource struct {
	name string
	recs []Record
}

func (s *sliceSource) Name() string { return s.name }
func (s *sliceSource) Each(ctx context.Context, fn func(Record) error) error {
	for _, r := range s.recs {
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

func digestOf(t *testing.T, text string) string {
	t.Helper()
	return classify(Record{ChunkID: "x", RAGProject: "p", DocumentID: "d", Text: text}, nil, uuid.Nil).SourceDigest
}

func corpus(t *testing.T) []Record {
	t.Helper()
	return []Record{
		{ChunkID: "c-2", RAGProject: "memory", DocumentID: "doc-a", Text: "a note about the deploy"},
		{ChunkID: "c-1", RAGProject: "memory", DocumentID: "doc-a", Text: "another note"},
		{ChunkID: "c-3", RAGProject: "memory", Text: "no document id, derived from the chunk"},
		{ChunkID: "", RAGProject: "memory", DocumentID: "doc-b", Text: "no chunk id"},
		{ChunkID: "c-4", RAGProject: "", DocumentID: "doc-b", Text: "no project"},
		{ChunkID: "c-5", RAGProject: "memory", DocumentID: "doc-c", Sensitivity: "top-secret", Text: "unknown class"},
		{ChunkID: "c-6", RAGProject: "memory", DocumentID: "doc-c", Text: "decision: we always use the ledger as the source of truth"},
		{ChunkID: "c-7", RAGProject: "memory", DocumentID: "doc-d", Text: "the config has password: hunter2 in it"},
		{ChunkID: "c-8", RAGProject: "memory", DocumentID: "doc-e", SourceDigest: "not-a-digest", Text: "bad digest"},
		{ChunkID: "c-9", RAGProject: "memory", DocumentID: "doc-f", Text: strings.Repeat("x", payloadBytesMax+1)},
	}
}

func planOf(t *testing.T, recs []Record, known map[string]Known) Plan {
	t.Helper()
	p, err := Planner{Source: &sliceSource{"test", recs}, Known: known}.Plan(context.Background())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return p
}

// TestTwoRunsProduceTheSamePlan is the property the whole dry run rests on: a
// rehearsal that does not predict the run is not a rehearsal.
func TestTwoRunsProduceTheSamePlan(t *testing.T) {
	recs := corpus(t)
	a := planOf(t, recs, nil)

	// Reversed, because a Qdrant scroll and a file export do not agree on order
	// and must still agree on the plan.
	rev := make([]Record, len(recs))
	for i, r := range recs {
		rev[len(recs)-1-i] = r
	}
	b := planOf(t, rev, nil)

	if a.Digest != b.Digest {
		t.Fatalf("digests differ: %s vs %s", a.Digest, b.Digest)
	}
	an, _ := json.Marshal(a.Entries)
	bn, _ := json.Marshal(b.Entries)
	if string(an) != string(bn) {
		t.Fatal("entries differ between runs")
	}
	if time.Since(a.GeneratedAt) > time.Hour {
		t.Fatal("generated_at is not a real timestamp")
	}
}

// TestEveryRecordGetsExactlyOneDecision: no silent drops, no double counting.
func TestEveryRecordGetsExactlyOneDecision(t *testing.T) {
	recs := corpus(t)
	p := planOf(t, recs, nil)
	if p.Total != int64(len(recs)) {
		t.Fatalf("planned %d of %d records", p.Total, len(recs))
	}
	if !p.Balanced() {
		t.Fatalf("counts do not add up: %v against %d", p.Counts, p.Total)
	}
	for _, e := range p.Entries {
		switch e.Decision {
		case Imported, Quarantined, Rejected, Skipped:
		default:
			t.Fatalf("chunk %q has decision %q", e.ChunkID, e.Decision)
		}
		if e.Reason == "" {
			t.Fatalf("chunk %q has no reason", e.ChunkID)
		}
	}
}

func find(t *testing.T, p Plan, chunk string) Entry {
	t.Helper()
	for _, e := range p.Entries {
		if e.ChunkID == chunk {
			return e
		}
	}
	t.Fatalf("chunk %q is not in the plan", chunk)
	return Entry{}
}

// TestCredentialsAreQuarantinedAndNeverQuoted: the point of the quarantine is
// that a human reads a list of ids, not a list of secrets.
func TestCredentialsAreQuarantinedAndNeverQuoted(t *testing.T) {
	p := planOf(t, corpus(t), nil)
	e := find(t, p, "c-7")
	if e.Decision != Quarantined || e.Reason != ReasonSecretSuspected {
		t.Fatalf("credential record was %s (%s)", e.Decision, e.Reason)
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"hunter2", "another note", "the deploy"} {
		if strings.Contains(string(body), leak) {
			t.Fatalf("the plan quotes the corpus: %q", leak)
		}
	}
}

// TestNothingIsPromoted: a fact-shaped record is a line in a review list. If it
// ever becomes anything else, this test is where that shows up.
func TestNothingIsPromoted(t *testing.T) {
	p := planOf(t, corpus(t), nil)
	e := find(t, p, "c-6")
	if !e.Candidate {
		t.Fatal("the fact-shaped record was not marked as a candidate")
	}
	if e.Decision != Imported {
		t.Fatalf("a candidate must still be an ordinary import, got %s", e.Decision)
	}
	body, _ := json.Marshal(p)
	if strings.Contains(strings.ToLower(string(body)), "promoted") {
		t.Fatal("the plan claims something was promoted")
	}
}

// TestRejectionsAndQuarantinesAreClassified pins the reason for each shape.
func TestRejectionsAndQuarantinesAreClassified(t *testing.T) {
	p := planOf(t, corpus(t), nil)
	want := map[string][2]string{
		"c-4": {string(Rejected), ReasonNoProject},
		"c-5": {string(Quarantined), ReasonUnknownSensitive},
		"c-8": {string(Rejected), ReasonBadDigest},
		"c-9": {string(Quarantined), ReasonTooLarge},
	}
	for chunk, w := range want {
		e := find(t, p, chunk)
		if string(e.Decision) != w[0] || e.Reason != w[1] {
			t.Fatalf("chunk %s: got %s/%s want %s/%s", chunk, e.Decision, e.Reason, w[0], w[1])
		}
	}
	// The record with no chunk id keeps an empty id and is still counted.
	var empty int
	for _, e := range p.Entries {
		if e.ChunkID == "" {
			empty++
			if e.Decision != Rejected || e.Reason != ReasonNoChunkID {
				t.Fatalf("the id-less record was %s/%s", e.Decision, e.Reason)
			}
		}
	}
	if empty != 1 {
		t.Fatalf("%d id-less entries, want 1", empty)
	}
}

// TestDerivedIdsAreStable: the ids must come from content, not from a clock.
func TestDerivedIdsAreStable(t *testing.T) {
	a := find(t, planOf(t, corpus(t), nil), "c-3")
	b := find(t, planOf(t, corpus(t), nil), "c-3")
	if a.DocumentID != b.DocumentID || a.ProjectID != b.ProjectID {
		t.Fatal("derived ids are not stable across runs")
	}
	if a.DocumentID == "" {
		t.Fatal("no document id was derived")
	}
	if ProjectID("MEMORY").String() != ProjectID("memory").String() {
		t.Fatal("the project id depends on the case of the project name")
	}
}

// TestKnownRowsBecomeSkipsAndRemapsBecomeQuarantines: this is what makes a
// second apply a no-op instead of a rewrite.
func TestKnownRowsBecomeSkipsAndRemapsBecomeQuarantines(t *testing.T) {
	recs := corpus(t)
	known := map[string]Known{
		"c-1": {DocumentID: "doc-a", SourceDigest: digestOf(t, "another note"), ProjectID: ProjectID("memory")},
		"c-2": {DocumentID: "doc-somewhere-else", SourceDigest: digestOf(t, "a note about the deploy"), ProjectID: ProjectID("memory")},
	}
	p := planOf(t, recs, known)
	if e := find(t, p, "c-1"); e.Decision != Skipped || e.Reason != ReasonAlreadyMapped {
		t.Fatalf("known row was %s/%s", e.Decision, e.Reason)
	}
	if e := find(t, p, "c-2"); e.Decision != Quarantined || e.Reason != ReasonRemappedDigest {
		t.Fatalf("remapped row was %s/%s", e.Decision, e.Reason)
	}
}

// TestDuplicateChunkIdsAreRejected: one record, one line, one count.
func TestDuplicateChunkIdsAreRejected(t *testing.T) {
	recs := []Record{
		{ChunkID: "dup", RAGProject: "memory", DocumentID: "doc-a", Text: "first"},
		{ChunkID: "dup", RAGProject: "memory", DocumentID: "doc-b", Text: "second"},
	}
	p := planOf(t, recs, nil)
	if p.Total != 2 || !p.Balanced() {
		t.Fatalf("total %d counts %v", p.Total, p.Counts)
	}
	if p.Counts[Rejected] != 1 {
		t.Fatalf("%d rejections, want 1", p.Counts[Rejected])
	}
}

// TestPlanRoundTripRefusesAnEditedPlan.
func TestPlanRoundTripRefusesAnEditedPlan(t *testing.T) {
	p := planOf(t, corpus(t), nil)
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := p.Write(path); err != nil {
		t.Fatal(err)
	}
	back, err := ReadPlan(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if back.Digest != p.Digest || back.Total != p.Total {
		t.Fatal("the plan did not survive a round trip")
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The decision on one line, not the counts: an edit that flips a record
	// from held-back to imported is exactly the tampering this guards against.
	edited := strings.Replace(string(body),
		"\"decision\": \""+string(Quarantined)+"\"",
		"\"decision\": \""+string(Imported)+"\"", 1)
	if edited == string(body) {
		t.Fatal("the test did not manage to edit the plan")
	}
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPlan(path); err == nil {
		t.Fatal("an edited plan was accepted")
	}
}

// TestFileSourceRefusesAnUnknownShape.
func TestFileSourceRefusesAnUnknownShape(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.ndjson")
	lines := "{\"chunk_id\":\"c-1\",\"rag_project\":\"memory\",\"document_id\":\"doc-a\",\"text\":\"hello\"}\n\n" +
		"{\"chunk_id\":\"c-2\",\"rag_project\":\"memory\",\"document_id\":\"doc-a\",\"text\":\"world\"}\n"
	if err := os.WriteFile(good, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Planner{Source: NewFileSource(good)}.Plan(context.Background())
	if err != nil {
		t.Fatalf("good file: %v", err)
	}
	if p.Total != 2 {
		t.Fatalf("%d entries from two lines and a blank", p.Total)
	}

	bad := filepath.Join(dir, "bad.ndjson")
	if err := os.WriteFile(bad, []byte("{\"chunk_id\":\"c-1\",\"body\":\"wrong field\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Planner{Source: NewFileSource(bad)}).Plan(context.Background()); err == nil {
		t.Fatal("an export with an unknown field was accepted")
	}
}

// TestADeclaredProjectIdIsUsedVerbatim.
//
// This is the defect the Section H rehearsal found: a mapping filed under an id
// derived from the rag project name belongs to no project in the registry, so a
// tombstone issued against the real project updates nothing and still reports
// success. The derived id is a fallback for a corpus that has not been placed;
// a declared one must reach the row unchanged.
func TestADeclaredProjectIdIsUsedVerbatim(t *testing.T) {
	ledgerProject := uuid.MustParse("7627423a-fd6e-459d-87c0-be32cd47c8cb")
	p, err := Planner{
		Source:    &sliceSource{"test", corpus(t)},
		ProjectID: ledgerProject,
	}.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.ProjectID != ledgerProject || p.ProjectIDSource != "declared by the operator" {
		t.Fatalf("plan records %s (%s)", p.ProjectID, p.ProjectIDSource)
	}
	for _, e := range p.Entries {
		if e.Decision != Imported {
			continue
		}
		if e.ProjectID != ledgerProject {
			t.Fatalf("chunk %s was filed under %s", e.ChunkID, e.ProjectID)
		}
	}

	// The same corpus with no declared project must not silently produce the
	// same plan: the two are different writes and must have different digests.
	derived := planOf(t, corpus(t), nil)
	if derived.Digest == p.Digest {
		t.Fatal("a declared project and a derived one produced the same plan")
	}
	if derived.ProjectIDSource != "derived from the rag project name" {
		t.Fatalf("source %q", derived.ProjectIDSource)
	}
}

// TestARowFiledUnderAnotherProjectIsQuarantined: re-planning against a mapping
// table whose rows sit under a different project must hold them for a human
// rather than treat them as already done.
func TestARowFiledUnderAnotherProjectIsQuarantined(t *testing.T) {
	known := map[string]Known{
		"c-1": {DocumentID: "doc-a", SourceDigest: digestOf(t, "another note"), ProjectID: ProjectID("memory")},
	}
	p, err := Planner{
		Source:    &sliceSource{"test", corpus(t)},
		Known:     known,
		ProjectID: uuid.MustParse("7627423a-fd6e-459d-87c0-be32cd47c8cb"),
	}.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if e := find(t, p, "c-1"); e.Decision != Quarantined || e.Reason != ReasonRemappedDigest {
		t.Fatalf("a row under another project was %s/%s", e.Decision, e.Reason)
	}
}
