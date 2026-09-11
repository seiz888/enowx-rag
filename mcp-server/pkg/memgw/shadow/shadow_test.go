package shadow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func f64(v float64) *float64 { return &v }

func dataset() Dataset {
	d := Dataset{
		Name: "test",
		Thresholds: Thresholds{
			MinCorrectness: f64(1.0),
			K:              3,
		},
		Cases: []Case{
			{
				ID: "c-plain", Lane: Correctness,
				Record: &CaseRecord{ChunkID: "a", RAGProject: "memory", DocumentID: "d", Text: "an ordinary note"},
				Expect: &CaseExpect{Decision: "imported"},
			},
			{
				ID: "c-secret", Lane: Correctness,
				Record: &CaseRecord{ChunkID: "b", RAGProject: "memory", DocumentID: "d", Text: "api_key: placeholder-for-a-fixture"},
				Expect: &CaseExpect{Decision: "quarantined"},
			},
			{
				ID: "c-fact", Lane: Correctness,
				Record: &CaseRecord{ChunkID: "c", RAGProject: "memory", DocumentID: "d", Text: "decision: we always write through the gateway"},
				Expect: &CaseExpect{Decision: "imported", Candidate: true},
			},
		},
	}
	d.Freeze()
	return d
}

// TestCorrectnessLaneIsExact: three cases, three matches, and a threshold of 1.
func TestCorrectnessLaneIsExact(t *testing.T) {
	rep, err := Runner{Dataset: dataset()}.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Correctness.Status != StatusMeasured || rep.Correctness.Matched != 3 {
		t.Fatalf("correctness: %+v", rep.Correctness)
	}
	if len(rep.Correctness.Failures) != 0 {
		t.Fatalf("failures: %+v", rep.Correctness.Failures)
	}
	if rep.RetrievalLane.Status != StatusEmpty {
		t.Fatalf("retrieval lane with no cases was %q", rep.RetrievalLane.Status)
	}
	if rep.Verdict != Pass {
		t.Fatalf("verdict %q: %s", rep.Verdict, rep.Note)
	}
}

// TestOneMismatchFailsTheLane: an exact lane with a tolerance is not exact.
func TestOneMismatchFailsTheLane(t *testing.T) {
	d := dataset()
	d.Cases[0].Expect.Decision = "rejected"
	d.Freeze()
	rep, err := Runner{Dataset: d}.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != Fail {
		t.Fatalf("verdict %q", rep.Verdict)
	}
	if len(rep.Correctness.Failures) != 1 || rep.Correctness.Failures[0].CaseID != "c-plain" {
		t.Fatalf("failures: %+v", rep.Correctness.Failures)
	}
	body, _ := json.Marshal(rep)
	if strings.Contains(string(body), "an ordinary note") {
		t.Fatal("the report quotes the case text")
	}
}

type fakeRetriever struct {
	answers map[string][]string
	err     error
}

func (f fakeRetriever) Name() string { return "fake" }
func (f fakeRetriever) Search(_ context.Context, q string, k int) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	got := f.answers[q]
	if len(got) > k {
		got = got[:k]
	}
	return got, nil
}

func withRetrievalCases(t *testing.T, minRecall *float64) Dataset {
	t.Helper()
	d := dataset()
	d.Thresholds.MinRecallAtK = minRecall
	d.Cases = append(d.Cases,
		Case{ID: "r-1", Lane: Retrieval, Query: "where is the runbook", Relevant: []string{"doc-a", "doc-b"}},
		Case{ID: "r-2", Lane: Retrieval, Query: "who fenced the writer", Relevant: []string{"doc-c"}},
	)
	d.Freeze()
	return d
}

// TestRetrievalWithoutARetrieverIsBlockedNotZero. This is the whole point of
// the package: a lane that did not run must not produce a number.
func TestRetrievalWithoutARetrieverIsBlockedNotZero(t *testing.T) {
	rep, err := Runner{
		Dataset:          withRetrievalCases(t, f64(0.8)),
		RetrievalBlocked: "no embedding provider is approved for this run",
	}.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.RetrievalLane.Status != StatusBlocked {
		t.Fatalf("status %q", rep.RetrievalLane.Status)
	}
	if rep.RetrievalLane.RecallAtK != nil || rep.RetrievalLane.LatencyMillis != nil || rep.RetrievalLane.Met != nil {
		t.Fatalf("a blocked lane produced numbers: %+v", rep.RetrievalLane)
	}
	if rep.RetrievalLane.Blocked != "no embedding provider is approved for this run" {
		t.Fatalf("reason %q", rep.RetrievalLane.Blocked)
	}
	if rep.Verdict != Incomplete {
		t.Fatalf("a blocked lane produced verdict %q", rep.Verdict)
	}
	body, _ := json.Marshal(rep)
	if strings.Contains(string(body), `"recall_at_k"`) {
		t.Fatal("the report carries a recall field for a lane that did not run")
	}
}

// TestRetrievalIsMeasuredWhenARetrieverAnswers.
func TestRetrievalIsMeasuredWhenARetrieverAnswers(t *testing.T) {
	r := fakeRetriever{answers: map[string][]string{
		"where is the runbook":  {"doc-a", "doc-z", "doc-b"},
		"who fenced the writer": {"doc-c"},
	}}
	rep, err := Runner{Dataset: withRetrievalCases(t, f64(0.9)), Retriever: r}.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.RetrievalLane.Status != StatusMeasured || rep.RetrievalLane.RecallAtK == nil {
		t.Fatalf("retrieval: %+v", rep.RetrievalLane)
	}
	if got := *rep.RetrievalLane.RecallAtK; got != 1.0 {
		t.Fatalf("recall %v, want 1", got)
	}
	if rep.Verdict != Pass {
		t.Fatalf("verdict %q: %s", rep.Verdict, rep.Note)
	}

	// One document short of the truth: recall must fall and the verdict with it.
	r.answers["where is the runbook"] = []string{"doc-a", "doc-z", "doc-y"}
	rep, err = Runner{Dataset: withRetrievalCases(t, f64(0.9)), Retriever: r}.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := *rep.RetrievalLane.RecallAtK; got != 0.75 {
		t.Fatalf("recall %v, want 0.75", got)
	}
	if rep.Verdict != Fail {
		t.Fatalf("verdict %q", rep.Verdict)
	}
}

// TestMeasuredRetrievalWithNoThresholdIsNotAPass: a number nobody agreed a bar
// for is reported, not judged.
func TestMeasuredRetrievalWithNoThresholdIsNotAPass(t *testing.T) {
	r := fakeRetriever{answers: map[string][]string{
		"where is the runbook":  {"doc-a", "doc-b"},
		"who fenced the writer": {"doc-c"},
	}}
	rep, err := Runner{Dataset: withRetrievalCases(t, nil), Retriever: r}.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.RetrievalLane.Met != nil {
		t.Fatal("a lane with no frozen threshold was judged")
	}
	if rep.Verdict != Incomplete {
		t.Fatalf("verdict %q", rep.Verdict)
	}
}

// TestAFailingRetrieverBlocksRatherThanReportsAPartialNumber.
func TestAFailingRetrieverBlocksRatherThanReportsAPartialNumber(t *testing.T) {
	rep, err := Runner{
		Dataset:   withRetrievalCases(t, f64(0.5)),
		Retriever: fakeRetriever{err: errors.New("connection refused")},
	}.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.RetrievalLane.Status != StatusBlocked || rep.RetrievalLane.RecallAtK != nil {
		t.Fatalf("retrieval: %+v", rep.RetrievalLane)
	}
	if !strings.Contains(rep.RetrievalLane.Blocked, "connection refused") {
		t.Fatalf("reason %q", rep.RetrievalLane.Blocked)
	}
	if rep.Verdict != Incomplete {
		t.Fatalf("verdict %q", rep.Verdict)
	}
}

// TestAnEditedDatasetIsRefused: a threshold chosen after seeing the result is
// not a threshold.
func TestAnEditedDatasetIsRefused(t *testing.T) {
	d := withRetrievalCases(t, f64(0.9))
	path := filepath.Join(t.TempDir(), "dataset.json")
	if err := d.Write(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDataset(path); err != nil {
		t.Fatalf("round trip: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(body), "0.9", "0.1", 1)
	if edited == string(body) {
		t.Fatal("the test did not manage to lower the threshold")
	}
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDataset(path); err == nil {
		t.Fatal("a dataset whose threshold was lowered after freezing was accepted")
	}
}

// TestAnUnfrozenDatasetIsRefused.
func TestAnUnfrozenDatasetIsRefused(t *testing.T) {
	d := dataset()
	d.Digest = ""
	if _, err := (Runner{Dataset: d}).Run(context.Background()); err == nil {
		t.Fatal("an unfrozen dataset ran")
	}
	if err := d.Write(filepath.Join(t.TempDir(), "d.json")); err == nil {
		t.Fatal("an unfrozen dataset was written")
	}
}

// TestMalformedCasesAreRefusedAtLoad.
func TestMalformedCasesAreRefusedAtLoad(t *testing.T) {
	dir := t.TempDir()
	for name, mutate := range map[string]func(*Dataset){
		"no-id":        func(d *Dataset) { d.Cases[0].ID = "" },
		"duplicate-id": func(d *Dataset) { d.Cases[1].ID = d.Cases[0].ID },
		"unknown-lane": func(d *Dataset) { d.Cases[0].Lane = "vibes" },
		"no-expect":    func(d *Dataset) { d.Cases[0].Expect = nil },
	} {
		d := dataset()
		mutate(&d)
		d.Freeze()
		path := filepath.Join(dir, name+".json")
		if err := d.Write(path); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadDataset(path); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}
