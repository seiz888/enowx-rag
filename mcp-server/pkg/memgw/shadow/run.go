package shadow

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/migrate"
)

// Verdict is what a whole report amounts to.
type Verdict string

const (
	// Pass: every lane ran and every judged lane met its threshold.
	Pass Verdict = "pass"
	// Fail: a lane ran and did not meet its threshold.
	Fail Verdict = "fail"
	// Incomplete: a lane could not run. This is not a pass. A report that ends
	// here is evidence about the lane that ran and about nothing else.
	Incomplete Verdict = "incomplete"
)

// Status is how one lane ended.
type Status string

const (
	StatusMeasured Status = "measured"
	StatusBlocked  Status = "blocked"
	StatusEmpty    Status = "empty"
)

// Failure names one correctness case that did not match. It carries decisions
// and ids, never the case text: a report is a document people circulate.
type Failure struct {
	CaseID string `json:"case_id"`
	Want   string `json:"want"`
	Got    string `json:"got"`
}

// CorrectnessResult is the exact lane.
type CorrectnessResult struct {
	Status   Status    `json:"status"`
	Blocked  string    `json:"blocked_because,omitempty"`
	Total    int       `json:"total"`
	Matched  int       `json:"matched"`
	Failures []Failure `json:"failures,omitempty"`
	// Fraction is reported only when the lane ran. A lane that did not run has
	// no fraction, not a zero.
	Fraction *float64 `json:"fraction,omitempty"`
	Met      *bool    `json:"threshold_met,omitempty"`
}

// RetrievalResult is the measured lane.
type RetrievalResult struct {
	Status  Status `json:"status"`
	Blocked string `json:"blocked_because,omitempty"`
	Total   int    `json:"total"`
	K       int    `json:"k,omitempty"`
	// Retriever names what answered. A recall figure is a fact about one
	// retriever over one corpus, so a report that carried the number without
	// the name would invite the reading that retrieval, in general, scores
	// this well.
	Retriever string `json:"retriever,omitempty"`
	// RecallAtK is nil unless it was actually measured. There is no default and
	// no fallback: a number here means a retriever answered.
	RecallAtK *float64 `json:"recall_at_k,omitempty"`
	Met       *bool    `json:"threshold_met,omitempty"`
	// LatencyMillis is the wall time of the retrieval calls, present only when
	// they happened. It is a run's own timing on one machine, not a benchmark.
	LatencyMillis *int64 `json:"latency_millis,omitempty"`
}

// Report is what a run produces.
type Report struct {
	ReportVersion string            `json:"report_version"`
	Dataset       string            `json:"dataset"`
	DatasetDigest string            `json:"dataset_digest"`
	Thresholds    Thresholds        `json:"thresholds"`
	RanAt         time.Time         `json:"ran_at"`
	Correctness   CorrectnessResult `json:"correctness"`
	RetrievalLane RetrievalResult   `json:"retrieval"`
	Verdict       Verdict           `json:"verdict"`
	// Note says in words what the verdict means, so a reader who sees only this
	// file cannot mistake an incomplete run for a clean one.
	Note string `json:"note"`
}

// ReportVersion is the shape of a report.
const ReportVersion = "memgw-shadow-report/1"

// Retriever is the retrieval lane's dependency. It is an interface with one
// method, and the runner is given one or not given one; there is no code path
// that fabricates results when it is absent.
type Retriever interface {
	// Search returns document ids, best first.
	Search(ctx context.Context, query string, k int) ([]string, error)
	// Name identifies what answered, because a recall number without the
	// provider that produced it is not comparable to anything.
	Name() string
}

// Runner executes a frozen dataset.
type Runner struct {
	Dataset   Dataset
	Retriever Retriever
	// RetrievalBlocked explains why the retrieval lane cannot run, when it
	// cannot. The caller sets it because the reason is an operational fact --
	// no provider credential, no corpus, a paid API nobody approved -- and the
	// report has to name it rather than say "unavailable".
	RetrievalBlocked string
	Now              func() time.Time
}

// Run evaluates every case and judges only the lanes that ran.
func (r Runner) Run(ctx context.Context) (Report, error) {
	now := r.Now
	if now == nil {
		now = time.Now
	}
	rep := Report{
		ReportVersion: ReportVersion,
		Dataset:       r.Dataset.Name,
		DatasetDigest: r.Dataset.Digest,
		Thresholds:    r.Dataset.Thresholds,
		RanAt:         now().UTC(),
	}
	if rep.DatasetDigest == "" {
		return Report{}, fmt.Errorf("shadow: refusing to run an unfrozen dataset")
	}

	var correctness, retrieval []Case
	for _, c := range r.Dataset.Cases {
		switch c.Lane {
		case Correctness:
			correctness = append(correctness, c)
		case Retrieval:
			retrieval = append(retrieval, c)
		}
	}

	rep.Correctness = runCorrectness(correctness, r.Dataset.Thresholds)
	var err error
	rep.RetrievalLane, err = r.runRetrieval(ctx, retrieval)
	if err != nil {
		return Report{}, err
	}
	rep.Verdict, rep.Note = verdict(rep)
	return rep, nil
}

func runCorrectness(cases []Case, th Thresholds) CorrectnessResult {
	res := CorrectnessResult{Total: len(cases)}
	if len(cases) == 0 {
		res.Status = StatusEmpty
		return res
	}
	res.Status = StatusMeasured
	for _, c := range cases {
		got := migrate.Classify(migrate.Record{
			ChunkID:      c.Record.ChunkID,
			RAGProject:   c.Record.RAGProject,
			DocumentID:   c.Record.DocumentID,
			SourceDigest: c.Record.SourceDigest,
			Sensitivity:  c.Record.Sensitivity,
			Text:         c.Record.Text,
		}, nil, uuid.Nil)
		want := c.Expect
		ok := string(got.Decision) == want.Decision && got.Candidate == want.Candidate
		if ok && want.Reason != "" {
			ok = got.Reason == want.Reason
		}
		if ok {
			res.Matched++
			continue
		}
		res.Failures = append(res.Failures, Failure{
			CaseID: c.ID,
			Want:   describe(want.Decision, want.Reason, want.Candidate),
			Got:    describe(string(got.Decision), got.Reason, got.Candidate),
		})
	}
	sort.Slice(res.Failures, func(i, j int) bool { return res.Failures[i].CaseID < res.Failures[j].CaseID })

	f := float64(res.Matched) / float64(res.Total)
	res.Fraction = &f
	bar := 1.0
	if th.MinCorrectness != nil {
		bar = *th.MinCorrectness
	}
	met := f >= bar
	res.Met = &met
	return res
}

func describe(decision, reason string, candidate bool) string {
	s := decision
	if reason != "" {
		s += " (" + reason + ")"
	}
	if candidate {
		s += " +candidate"
	}
	return s
}

func (r Runner) runRetrieval(ctx context.Context, cases []Case) (RetrievalResult, error) {
	res := RetrievalResult{Total: len(cases), K: r.Dataset.Thresholds.K}
	if res.K <= 0 {
		res.K = 5
	}
	switch {
	case len(cases) == 0:
		res.Status = StatusEmpty
		return res, nil
	case r.Retriever == nil:
		res.Status = StatusBlocked
		res.Blocked = r.RetrievalBlocked
		if res.Blocked == "" {
			res.Blocked = "no retriever was supplied to the run"
		}
		return res, nil
	}

	res.Status = StatusMeasured
	res.Retriever = r.Retriever.Name()
	started := time.Now()
	var sum float64
	for _, c := range cases {
		got, err := r.Retriever.Search(ctx, c.Query, res.K)
		if err != nil {
			// A retriever that failed part way through has produced a partial
			// measurement, and a partial measurement reported as a number is
			// exactly the kind of invented figure this package must not make.
			return RetrievalResult{
				Total: len(cases), K: res.K, Status: StatusBlocked, Retriever: r.Retriever.Name(),
				Blocked: fmt.Sprintf("the retriever (%s) failed on case %s: %v", r.Retriever.Name(), c.ID, err),
			}, nil
		}
		if len(got) > res.K {
			got = got[:res.K]
		}
		found := map[string]bool{}
		for _, id := range got {
			found[id] = true
		}
		var hit int
		for _, want := range c.Relevant {
			if found[want] {
				hit++
			}
		}
		sum += float64(hit) / float64(len(c.Relevant))
	}
	ms := time.Since(started).Milliseconds()
	res.LatencyMillis = &ms
	recall := sum / float64(len(cases))
	res.RecallAtK = &recall
	if r.Dataset.Thresholds.MinRecallAtK != nil {
		met := recall >= *r.Dataset.Thresholds.MinRecallAtK
		res.Met = &met
	}
	return res, nil
}

// verdict refuses to call an unfinished run a pass.
func verdict(rep Report) (Verdict, string) {
	if rep.Correctness.Status == StatusMeasured && rep.Correctness.Met != nil && !*rep.Correctness.Met {
		return Fail, "the correctness lane did not meet its threshold; the retrieval lane is not what is wrong here"
	}
	if rep.RetrievalLane.Status == StatusMeasured && rep.RetrievalLane.Met != nil && !*rep.RetrievalLane.Met {
		return Fail, "retrieval recall is below the frozen threshold"
	}
	if rep.Correctness.Status == StatusBlocked || rep.RetrievalLane.Status == StatusBlocked {
		return Incomplete, "a lane could not run, so this report is evidence about the lane that did and about nothing else"
	}
	if rep.RetrievalLane.Status == StatusMeasured && rep.RetrievalLane.Met == nil {
		return Incomplete, "retrieval was measured but no threshold was frozen for it, so the number is reported and not judged"
	}
	return Pass, "every lane ran and met the threshold frozen with the dataset"
}
