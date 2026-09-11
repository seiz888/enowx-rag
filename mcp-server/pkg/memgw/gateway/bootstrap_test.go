// The bootstrap, judged by what it refuses to say.
//
// A context pack is read by an agent that will act on it, so the failure that
// matters is not "it returned too little" but "it returned something that is no
// longer true and did not say so". These tests build a real history through the
// real write path -- candidate, promotion, supersession, tombstone -- and then
// ask what the pack claims.
package gateway_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/gateway"
	"github.com/enowdev/enowx-rag/pkg/memgw/principal"
)

type pack struct {
	Notice      string `json:"notice"`
	GeneratedAt string `json:"generated_at"`
	Work        *struct {
		WorkID     uuid.UUID `json:"work_id"`
		State      string    `json:"state"`
		Revision   int64     `json:"revision"`
		Tombstoned bool      `json:"tombstoned"`
	} `json:"work"`
	Checkpoint *struct {
		Revision       int64  `json:"revision"`
		Objective      string `json:"objective"`
		NextSafeAction string `json:"next_safe_action"`
		AgeSeconds     int64  `json:"age_seconds"`
	} `json:"checkpoint"`
	Evidence []struct {
		Type    string `json:"evidence_type"`
		Locator string `json:"locator"`
	} `json:"evidence"`
	Facts []struct {
		Subject         string `json:"subject"`
		Predicate       string `json:"predicate"`
		Cardinality     string `json:"cardinality"`
		Revision        int64  `json:"revision"`
		Status          string `json:"status"`
		Conflicted      bool   `json:"conflicted"`
		SupersededCount int    `json:"superseded_count"`
		Values          []struct {
			FactID     uuid.UUID       `json:"fact_id"`
			Object     json.RawMessage `json:"object"`
			Status     string          `json:"status"`
			Revision   int64           `json:"revision"`
			Freshness  string          `json:"freshness"`
			Provenance []struct {
				SourceType string `json:"source_type"`
				SourceRef  string `json:"source_ref"`
			} `json:"provenance"`
		} `json:"values"`
	} `json:"facts"`
	Partial   bool     `json:"partial"`
	Omissions []string `json:"omissions"`
}

func (e *env) bootstrap(a actor, body map[string]any) pack {
	e.t.Helper()
	body["project_id"] = e.projectID.String()
	body["workspace_id"] = e.workspaceID.String()
	status, raw := e.do(http.MethodPost, "/v1/bootstrap", a.token, body)
	if status != http.StatusOK {
		e.t.Fatalf("bootstrap: %d %s", status, raw)
	}
	var p pack
	decodeInto(e.t, raw, &p)
	return p
}

// submit posts an event and fails the test if it did not commit. It returns the
// event id, which is what an evidence reference is.
func (e *env) submit(a actor, eventType, key string, extra map[string]any) uuid.UUID {
	e.t.Helper()
	body := e.submission(a, eventType, key, extra)
	status, raw := e.do(http.MethodPost, "/v1/events", a.token, body)
	if status != http.StatusCreated {
		e.t.Fatalf("%s: %d %s", eventType, status, raw)
	}
	var r struct {
		EventID uuid.UUID `json:"event_id"`
	}
	decodeInto(e.t, raw, &r)
	return r.EventID
}

// promote runs the whole candidate-then-promotion path, which is the only way a
// fact comes to exist: a proposer that cannot promote, and a reviewer that did
// not propose.
func (e *env) promote(proposer, reviewer actor, evidenceRef uuid.UUID,
	subject, predicate, object, cardinality string, factID uuid.UUID,
	expectedRevision *int64, supersedes []string, keySuffix string) {
	e.t.Helper()
	candidateID := uuid.New()
	e.submit(proposer, "fact.candidate_proposed", "propose:"+keySuffix, map[string]any{
		"payload": map[string]any{
			"candidate_id": candidateID.String(), "subject": subject, "predicate": predicate,
			"object": json.RawMessage(object), "cardinality": cardinality,
			"source": "session transcript", "extractor": "test-extractor",
			"evidence_class": "derived",
		},
	})
	payload := map[string]any{
		"candidate_id": candidateID.String(), "fact_id": factID.String(),
		"subject": subject, "predicate": predicate,
		"object": json.RawMessage(object), "cardinality": cardinality,
		"evidence_class": "observed",
	}
	if len(supersedes) > 0 {
		payload["supersedes_fact_ids"] = supersedes
	}
	extra := map[string]any{
		"payload":       payload,
		"evidence_refs": []string{evidenceRef.String()},
	}
	if expectedRevision != nil {
		extra["expected_revision"] = *expectedRevision
	}
	e.submit(reviewer, "fact.promoted", "promote:"+keySuffix, extra)
}

// ---------------------------------------------------------------------------

// TestBootstrapNeverPresentsASupersededFactAsCurrent is the whole point of the
// read path. A value that has been replaced is not returned -- not last, not
// flagged, not at all -- and what is returned instead is a count saying that
// something was replaced, so the reader can tell "nothing else is known" from
// "there is history here".
func TestBootstrapNeverPresentsASupersededFactAsCurrent(t *testing.T) {
	e := newEnv(t)
	proposer := e.actor("proposer", principal.RoleCandidateWrite, principal.RoleWorkRead)
	reviewer := e.actor("reviewer", principal.RoleFactPromote, principal.RoleHistoryRead, principal.RoleWorkRead)

	evidenceRef := e.submit(proposer, "evidence.recorded", "proposer:ev:1", map[string]any{
		"payload": map[string]any{
			"evidence_type": "tool_output", "evidence_class": "observed", "locator": "run-1",
		},
	})

	first := uuid.New()
	e.promote(proposer, reviewer, evidenceRef, "deployment", "target", `"vps-1"`, "single", first, nil, nil, "1")

	got := e.bootstrap(reviewer, map[string]any{"subject": "deployment"})
	if len(got.Facts) != 1 || len(got.Facts[0].Values) != 1 {
		t.Fatalf("after one promotion: %+v", got.Facts)
	}
	if string(got.Facts[0].Values[0].Object) != `"vps-1"` {
		t.Fatalf("value is %s", got.Facts[0].Values[0].Object)
	}
	if got.Facts[0].SupersededCount != 0 {
		t.Fatalf("nothing has been superseded yet, count is %d", got.Facts[0].SupersededCount)
	}
	if len(got.Facts[0].Values[0].Provenance) == 0 {
		t.Fatal("a promoted fact carries no provenance")
	}
	slotRevision := got.Facts[0].Revision

	second := uuid.New()
	e.promote(proposer, reviewer, evidenceRef, "deployment", "target", `"vps-2"`, "single", second,
		&slotRevision, []string{first.String()}, "2")

	got = e.bootstrap(reviewer, map[string]any{"subject": "deployment"})
	if len(got.Facts) != 1 {
		t.Fatalf("%d slots after supersession", len(got.Facts))
	}
	slot := got.Facts[0]
	if len(slot.Values) != 1 {
		t.Fatalf("%d current values; a superseded value is still being returned: %+v", len(slot.Values), slot.Values)
	}
	if string(slot.Values[0].Object) != `"vps-2"` || slot.Values[0].FactID != second {
		t.Fatalf("the current value is %s (%s)", slot.Values[0].Object, slot.Values[0].FactID)
	}
	if slot.SupersededCount != 1 {
		t.Fatalf("superseded_count is %d, want 1", slot.SupersededCount)
	}
	if slot.Revision <= slotRevision {
		t.Fatalf("the slot revision did not advance: %d", slot.Revision)
	}
	// The revision the pack reports is the one a writer must present next. If it
	// were stale, every host reading the pack would collide on its first write.
	third := uuid.New()
	e.promote(proposer, reviewer, evidenceRef, "deployment", "target", `"vps-3"`, "single", third,
		&slot.Revision, []string{second.String()}, "3")
}

// TestBootstrapDoesNotHandBackAResumePointForADeletedWork. The work is still
// reported -- a host resuming into it must learn it was deleted rather than see
// an empty pack and assume it is starting fresh -- but its checkpoint is
// withheld, because a resume point for a tombstoned work is the resurrection
// path with extra steps.
func TestBootstrapDoesNotHandBackAResumePointForADeletedWork(t *testing.T) {
	e := newEnv(t)
	writer := e.actor("writer", principal.RoleCheckpointWrite, principal.RoleWorkRead, principal.RoleHistoryRead)
	admin := e.actor("admin", principal.RoleAdmin)

	workID := uuid.New()
	e.submit(writer, "work.planned", "writer:plan:1", map[string]any{
		"work_id": workID.String(), "payload": workPayload("wire the gateway"),
	})
	rev := int64(1)
	e.submit(writer, "work.activated", "writer:activate:1", map[string]any{
		"work_id": workID.String(), "payload": workPayload("wire the gateway"),
		"expected_revision": rev,
	})
	rev = 2
	e.submit(writer, "checkpoint.recorded", "writer:ckpt:1", map[string]any{
		"work_id":           workID.String(),
		"payload":           checkpointPayload("wire the gateway", "write the bootstrap test"),
		"expected_revision": rev,
	})

	got := e.bootstrap(writer, map[string]any{"work_id": workID.String()})
	if got.Work == nil || got.Work.State != "active" {
		t.Fatalf("work: %+v", got.Work)
	}
	if got.Checkpoint == nil || got.Checkpoint.NextSafeAction != "write the bootstrap test" {
		t.Fatalf("checkpoint: %+v", got.Checkpoint)
	}
	if got.Checkpoint.Revision != got.Work.Revision {
		t.Fatalf("the checkpoint is at revision %d and the work at %d; a writer told the wrong one collides",
			got.Checkpoint.Revision, got.Work.Revision)
	}
	if got.Notice != gateway.RecalledContentNotice {
		t.Fatalf("the pack does not carry the recalled-content notice: %q", got.Notice)
	}

	e.submit(admin, "tombstone.issued", "admin:tombstone:1", map[string]any{
		"payload": map[string]any{
			"subject_type": "work", "subject_id": workID.String(),
			"reason_class": "user_request", "erasure_required": false,
		},
	})

	got = e.bootstrap(writer, map[string]any{"work_id": workID.String()})
	if got.Work == nil || !got.Work.Tombstoned {
		t.Fatalf("a tombstoned work is not reported as such: %+v", got.Work)
	}
	if got.Checkpoint != nil {
		t.Fatalf("a resume point was handed back for a deleted work: %+v", got.Checkpoint)
	}
	if !got.Partial || len(got.Omissions) == 0 {
		t.Fatalf("the pack withheld something and did not say so: partial=%v omissions=%v",
			got.Partial, got.Omissions)
	}
}

// TestBootstrapSaysWhatItCouldNotAnswer: an absence must be distinguishable
// from a truncation, and a work id from another project must read as absent
// rather than as somebody else's work.
func TestBootstrapSaysWhatItCouldNotAnswer(t *testing.T) {
	e := newEnv(t)
	reader := e.actor("reader", principal.RoleHistoryRead, principal.RoleWorkRead)

	// A project with nothing in it: not partial, and nothing invented.
	got := e.bootstrap(reader, map[string]any{})
	if got.Partial || len(got.Omissions) != 0 {
		t.Fatalf("an empty project reported omissions: %v", got.Omissions)
	}
	if got.Work != nil || got.Checkpoint != nil || len(got.Facts) != 0 || len(got.Evidence) != 0 {
		t.Fatalf("an empty project returned content: %+v", got)
	}

	// A work that does not exist here is named as missing.
	got = e.bootstrap(reader, map[string]any{"work_id": uuid.New().String()})
	if got.Work != nil {
		t.Fatalf("an unknown work was returned: %+v", got.Work)
	}
	if !got.Partial || len(got.Omissions) == 0 {
		t.Fatal("an unknown work was silently ignored")
	}
}

// TestBootstrapIsScoped: reading is a granted act. A principal with a valid
// credential and no grant in this project gets nothing, and the refusal is
// scope_denied rather than an empty pack -- an empty pack would read as "this
// project knows nothing".
func TestBootstrapIsScoped(t *testing.T) {
	e := newEnv(t)
	stranger := e.actor("stranger")
	status, raw := e.do(http.MethodPost, "/v1/bootstrap", stranger.token, map[string]any{
		"project_id": e.projectID.String(), "workspace_id": e.workspaceID.String(),
	})
	if status != http.StatusForbidden || class(t, raw) != "scope_denied" {
		t.Fatalf("want 403 scope_denied, got %d %s", status, raw)
	}

	// A write role is not a read role in the other direction either: a
	// candidate_write principal holds no work_read, so it cannot read the pack.
	proposer := e.actor("proposer", principal.RoleCandidateWrite)
	status, raw = e.do(http.MethodPost, "/v1/bootstrap", proposer.token, map[string]any{
		"project_id": e.projectID.String(), "workspace_id": e.workspaceID.String(),
	})
	if status != http.StatusForbidden {
		t.Fatalf("candidate_write read the pack: %d %s", status, raw)
	}
}

// TestBootstrapReturnsEvidenceAsEvidence: a subagent's finding appears in the
// pack, and it appears as evidence -- never as a fact and never as a
// checkpoint. INV-16 is a read-path property as much as a write-path one.
func TestBootstrapReturnsEvidenceAsEvidence(t *testing.T) {
	e := newEnv(t)
	writer := e.actor("writer", principal.RoleCheckpointWrite, principal.RoleWorkRead, principal.RoleHistoryRead)

	workID := uuid.New()
	e.submit(writer, "work.planned", "writer:plan:1", map[string]any{
		"work_id": workID.String(), "payload": workPayload("investigate"),
	})
	e.submit(writer, "evidence.recorded", "writer:ev:1", map[string]any{
		"work_id": workID.String(),
		"payload": map[string]any{
			"evidence_type": "tool_output", "evidence_class": "observed", "locator": "go test ./...",
		},
	})

	got := e.bootstrap(writer, map[string]any{"work_id": workID.String()})
	if len(got.Evidence) != 1 || got.Evidence[0].Locator != "go test ./..." {
		t.Fatalf("evidence: %+v", got.Evidence)
	}
	if len(got.Facts) != 0 {
		t.Fatalf("evidence was promoted into the fact list: %+v", got.Facts)
	}
	if got.Checkpoint != nil {
		t.Fatalf("evidence became a checkpoint: %+v", got.Checkpoint)
	}
}
