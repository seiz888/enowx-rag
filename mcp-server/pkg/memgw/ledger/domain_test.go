// The canonical-domain fixtures: work lineage, branch rewind, the
// candidate/fact separation, conflict, cardinality and the subagent boundary.
//
// These replay the frozen fixtures against a real PostgreSQL like the rest of
// the suite. What they add is that they assert the *materialised* state as well
// as the receipt: a receipt saying "committed" while the fact table disagrees
// with it is exactly the failure the domain running inside the commit
// transaction is supposed to make impossible.
package ledger_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/domain"
	"github.com/enowdev/enowx-rag/pkg/memgw/ledger"
)

// --- seeding the "given" blocks ---------------------------------------------

// seedLine creates a session and its initial branch for rows a fixture assumes
// already exist. The seed line is deliberately not one of the fixture's own
// session ids: a fixture's session must still be created by the write path.
func (h *harness) seedLine(name string, project, workspace, principalID uuid.UUID) (string, uuid.UUID) {
	h.t.Helper()
	session := "seed:" + name
	branch := uuid.New()
	h.ensureProject(project)
	h.ensureWorkspace(project, workspace)
	h.ensureSession(session, project, workspace, principalID)
	h.ensureBranch(branch, project, session)
	return session, branch
}

// seedEventRow appends a synthetic additive event so that a row carrying a
// foreign key to an event has a real one to point at.
func (h *harness) seedEventRow(id, project, workspace, principalID, branch uuid.UUID, session, eventType string) uuid.UUID {
	h.t.Helper()
	if id == uuid.Nil {
		id = uuid.New()
	}
	if _, err := h.pool.Pgx().Exec(h.ctx, `
		INSERT INTO events (event_id, idempotency_key, principal_id, project_id, workspace_id,
		                    session_id, branch_id, event_type, payload, payload_digest,
		                    sensitivity_class, policy_version, schema_version, occurred_at, writer_epoch)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'{"seed":true}',repeat('0',64),'internal','1.0.0','1.0.0', now(), 1)`,
		id, "seed:"+uuid.NewString(), principalID, project, workspace, session, branch, eventType); err != nil {
		h.t.Fatalf("seed event: %v", err)
	}
	return id
}

// seedSlotRevision puts a fact slot at a revision the way the ledger reads it:
// from the event log, not from the slot row. Seeding only the row would let a
// compare-and-set pass that the real log would have refused.
func (h *harness) seedSlotRevision(slot, project, workspace, principalID, branch uuid.UUID, session string, revision int64) {
	h.t.Helper()
	if _, err := h.pool.Pgx().Exec(h.ctx, `
		INSERT INTO events (event_id, idempotency_key, principal_id, project_id, workspace_id,
		                    session_id, branch_id, event_type, payload, payload_digest,
		                    sensitivity_class, policy_version, schema_version, occurred_at, writer_epoch,
		                    aggregate_type, aggregate_id, revision)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'fact.promoted','{"seed":true}',repeat('0',64),
		        'internal','1.0.0','1.0.0', now(), 1, 'fact_slot', $8, $9)`,
		uuid.New(), "seed:"+uuid.NewString(), principalID, project, workspace, session, branch,
		slot, revision); err != nil {
		h.t.Fatalf("seed slot revision: %v", err)
	}
}

func (h *harness) seedSlot(project uuid.UUID, subject, predicate, cardinality string, revision int64) uuid.UUID {
	h.t.Helper()
	slot := domain.SlotID(project, subject, predicate)
	if _, err := h.pool.Pgx().Exec(h.ctx, `
		INSERT INTO fact_slots (slot_id, project_id, subject, predicate, cardinality, revision)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (slot_id) DO NOTHING`,
		slot, project, subject, predicate, cardinality, revision); err != nil {
		h.t.Fatalf("seed slot: %v", err)
	}
	return slot
}

func (h *harness) seedFact(factID, slot, project, workspace, principalID, eventID uuid.UUID,
	subject, predicate, object, cardinality string, revision int64, validFrom time.Time) {
	h.t.Helper()
	digest, canonical := objectDigest(h.t, object)
	if _, err := h.pool.Pgx().Exec(h.ctx, `
		INSERT INTO facts (fact_id, slot_id, project_id, workspace_id, subject, predicate, object,
		                   object_digest, cardinality, status, evidence_class, valid_from,
		                   promoted_by_principal_id, promoted_event_id, revision)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'active','observed',$10,$11,$12,$13)`,
		factID, slot, project, workspace, subject, predicate, canonical, digest, cardinality,
		validFrom, principalID, eventID, revision); err != nil {
		h.t.Fatalf("seed fact: %v", err)
	}
}

func (h *harness) seedCandidate(candidateID, project, workspace, principalID, eventID uuid.UUID,
	subject, predicate, object, cardinality string) {
	h.t.Helper()
	digest, canonical := objectDigest(h.t, object)
	if _, err := h.pool.Pgx().Exec(h.ctx, `
		INSERT INTO fact_candidates (candidate_id, project_id, workspace_id, subject, predicate, object,
		                             object_digest, cardinality, source, extractor, evidence_class,
		                             review_status, proposed_by_principal_id, proposed_event_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'seed-corpus','seed-extractor','derived','pending',$9,$10)`,
		candidateID, project, workspace, subject, predicate, canonical, digest, cardinality,
		principalID, eventID); err != nil {
		h.t.Fatalf("seed candidate: %v", err)
	}
}

// objectDigest is the ledger's canonical-JSON digest rule, restated here so the
// seeded rows agree with what the write path computes.
func objectDigest(t *testing.T, raw string) (string, string) {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("fixture object %q: %v", raw, err)
	}
	canonical, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("canonicalise %q: %v", raw, err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), string(canonical)
}

// --- readers -----------------------------------------------------------------

type factRow struct {
	status       string
	revision     int64
	supersededBy *uuid.UUID
	candidateID  *uuid.UUID
	promotedBy   uuid.UUID
}

func (h *harness) fact(id uuid.UUID) factRow {
	h.t.Helper()
	var f factRow
	if err := h.pool.Pgx().QueryRow(h.ctx,
		`SELECT status, revision, superseded_by, candidate_id, promoted_by_principal_id FROM facts WHERE fact_id = $1`,
		id).Scan(&f.status, &f.revision, &f.supersededBy, &f.candidateID, &f.promotedBy); err != nil {
		h.t.Fatalf("read fact %s: %v", id, err)
	}
	return f
}

func (h *harness) factByObject(slot uuid.UUID, object string) (uuid.UUID, factRow) {
	h.t.Helper()
	digest, _ := objectDigest(h.t, object)
	var id uuid.UUID
	if err := h.pool.Pgx().QueryRow(h.ctx,
		`SELECT fact_id FROM facts WHERE slot_id = $1 AND object_digest = $2`, slot, digest).Scan(&id); err != nil {
		h.t.Fatalf("no fact for %s: %v", object, err)
	}
	return id, h.fact(id)
}

func (h *harness) slotStatus(slot uuid.UUID) (string, int64) {
	h.t.Helper()
	var status string
	var revision int64
	if err := h.pool.Pgx().QueryRow(h.ctx,
		`SELECT status, revision FROM fact_slots WHERE slot_id = $1`, slot).Scan(&status, &revision); err != nil {
		h.t.Fatalf("read slot: %v", err)
	}
	return status, revision
}

func (h *harness) scalar(query string, args ...any) int64 {
	h.t.Helper()
	var n int64
	if err := h.pool.Pgx().QueryRow(h.ctx, query, args...).Scan(&n); err != nil {
		h.t.Fatalf("query %q: %v", query, err)
	}
	return n
}

// --- work lineage ------------------------------------------------------------

// TestWorkStateMachineRefusesIllegalTransitions: the transition table is the
// authority, and a state absent from it is not reachable. Terminal states are
// terminal: reopening finished work is a new work that cites the old one, not a
// state change, or "completed" would only ever mean "completed for now".
func TestWorkStateMachineRefusesIllegalTransitions(t *testing.T) {
	f := loadFixture(t, "duplicate-retry")
	h := newHarness(t, f)
	base := f.Steps[0].Submit.event(t)
	h.seedRevision(base.PrincipalID, base.ProjectID, base.WorkspaceID, *base.WorkID, base.BranchID, base.SessionID, 7)

	move := func(eventType string, expected int64) error {
		e := base
		e.EventID = uuid.New()
		e.IdempotencyKey = "move:" + eventType + ":" + uuid.NewString()
		e.Type = eventType
		e.Payload = map[string]any{"reason_class": "test"}
		e.ExpectedRevision = &expected
		_, err := h.sub.Submit(h.ctx, e)
		return err
	}

	// active -> active is not in the table, and absence is a refusal.
	if err := move("work.activated", 7); !ledger.IsClass(err, ledger.ClassPolicyRejected) {
		t.Fatalf("active -> active should be refused, got %v", err)
	}
	if err := move("work.completed", 7); err != nil {
		t.Fatalf("active -> completed: %v", err)
	}
	if err := move("work.activated", 8); !ledger.IsClass(err, ledger.ClassPolicyRejected) {
		t.Fatalf("completed is terminal, got %v", err)
	}
	// A checkpoint on finished work is refused too: it would read as progress.
	e := base
	e.EventID = uuid.New()
	e.IdempotencyKey = "ckpt-after-complete"
	rev := int64(8)
	e.ExpectedRevision = &rev
	if _, err := h.sub.Submit(h.ctx, e); !ledger.IsClass(err, ledger.ClassPolicyRejected) {
		t.Fatalf("checkpoint on completed work should be refused, got %v", err)
	}

	var state string
	if err := h.pool.Pgx().QueryRow(h.ctx, `SELECT state FROM works WHERE work_id = $1`, *base.WorkID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "completed" {
		t.Fatalf("work state = %s, want completed", state)
	}
}

// TestCheckpointIsImmutableAndAdvancesOnlyTheRevision is INV-09: a checkpoint
// is a new row at a new revision and moves no state, and no earlier checkpoint
// is ever rewritten -- enforced by trigger, not by convention.
func TestCheckpointIsImmutableAndAdvancesOnlyTheRevision(t *testing.T) {
	f := loadFixture(t, "duplicate-retry")
	h := newHarness(t, f)
	e := f.Steps[0].Submit.event(t)
	h.seedRevision(e.PrincipalID, e.ProjectID, e.WorkspaceID, *e.WorkID, e.BranchID, e.SessionID, 7)
	if _, err := h.sub.Submit(h.ctx, e); err != nil {
		t.Fatal(err)
	}

	second := e
	second.EventID = uuid.New()
	second.IdempotencyKey = "claude:sess-1:ckpt:0009"
	rev := int64(8)
	second.ExpectedRevision = &rev
	second.Payload = map[string]any{"objective": "second", "next_safe_action": "keep going"}
	r, err := h.sub.Submit(h.ctx, second)
	if err != nil {
		t.Fatalf("second checkpoint: %v", err)
	}
	if r.Revision == nil || *r.Revision != 9 {
		t.Fatalf("second checkpoint revision = %v, want 9", r.Revision)
	}
	if n := h.scalar(`SELECT count(*) FROM checkpoints WHERE work_id = $1`, *e.WorkID); n != 2 {
		t.Fatalf("%d checkpoint rows, want 2", n)
	}
	var state string
	if err := h.pool.Pgx().QueryRow(h.ctx, `SELECT state FROM works WHERE work_id = $1`, *e.WorkID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "active" {
		t.Fatalf("a checkpoint changed the work state to %s", state)
	}
	if _, err := h.pool.Pgx().Exec(h.ctx, `UPDATE checkpoints SET objective = 'rewritten'`); err == nil {
		t.Fatal("checkpoints must not be updatable")
	}
	if _, err := h.pool.Pgx().Exec(h.ctx, `DELETE FROM checkpoints`); err == nil {
		t.Fatal("checkpoints must not be deletable")
	}
}

// TestFixtureBranchRewind is INV-19: a rewind opens a new branch that records
// where it diverged, and the parent keeps every event it had.
func TestFixtureBranchRewind(t *testing.T) {
	f := loadFixture(t, "branch-rewind")
	h := newHarness(t, f)

	branch := f.Steps[0].Submit.event(t)
	parentBranch := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	// The "given" block: an active branch with four events, and a work at 30.
	h.seedRevision(branch.PrincipalID, branch.ProjectID, branch.WorkspaceID, *branch.WorkID,
		parentBranch, branch.SessionID, 30)
	for i := 0; i < 3; i++ {
		h.seedEventRow(uuid.Nil, branch.ProjectID, branch.WorkspaceID, branch.PrincipalID,
			parentBranch, branch.SessionID, "evidence.recorded")
	}
	parentEvents := h.scalar(`SELECT count(*) FROM events WHERE branch_id = $1`, parentBranch)

	r, err := h.sub.Submit(h.ctx, branch)
	if err != nil {
		t.Fatalf("branch: %v", err)
	}
	if r.Revision != nil {
		t.Fatalf("a rewind is additive and must carry no revision, got %d", *r.Revision)
	}
	if got := h.currentRevision(*branch.WorkID); got != 30 {
		t.Fatalf("a rewind moved the work to revision %d", got)
	}

	var status string
	if err := h.pool.Pgx().QueryRow(h.ctx, `SELECT status FROM branches WHERE branch_id = $1`, parentBranch).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "superseded" {
		t.Fatalf("parent branch status = %s, want superseded", status)
	}
	if got := h.scalar(`SELECT count(*) FROM events WHERE branch_id = $1`, parentBranch); got != parentEvents {
		t.Fatalf("the parent branch lost events: %d -> %d", parentEvents, got)
	}

	var parent *uuid.UUID
	var point *int64
	var reason string
	if err := h.pool.Pgx().QueryRow(h.ctx,
		`SELECT parent_branch_id, branch_point_seq, reason_class FROM branches WHERE branch_id = $1`,
		branch.BranchID).Scan(&parent, &point, &reason); err != nil {
		t.Fatal(err)
	}
	if parent == nil || *parent != parentBranch || point == nil || *point != 102 || reason != "rewind" {
		t.Fatalf("the new branch does not record its divergence: parent=%v point=%v reason=%s", parent, point, reason)
	}

	// The checkpoint on the new branch advances the same work.
	ckpt := f.Steps[1].Submit.event(t)
	r2, err := h.sub.Submit(h.ctx, ckpt)
	if err != nil {
		t.Fatalf("checkpoint on the new branch: %v", err)
	}
	if r2.Revision == nil || *r2.Revision != 31 {
		t.Fatalf("revision = %v, fixture says 31", r2.Revision)
	}
	if n := h.scalar(`SELECT count(*) FROM checkpoints WHERE branch_id = $1`, branch.BranchID); n != 1 {
		t.Fatalf("%d checkpoints on the new branch, want 1", n)
	}
}

// TestBranchRewindMustNameItsDivergence: a rewind with no branch point is not
// auditable, and reusing an existing branch id would merge two histories.
func TestBranchRewindMustNameItsDivergence(t *testing.T) {
	f := loadFixture(t, "branch-rewind")
	h := newHarness(t, f)
	e := f.Steps[0].Submit.event(t)
	parentBranch := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	h.seedRevision(e.PrincipalID, e.ProjectID, e.WorkspaceID, *e.WorkID, parentBranch, e.SessionID, 30)

	blind := e
	blind.EventID = uuid.New()
	blind.IdempotencyKey = "blind-rewind"
	blind.Payload = map[string]any{"reason_class": "rewind"}
	if _, err := h.sub.Submit(h.ctx, blind); !ledger.IsClass(err, ledger.ClassPolicyRejected) {
		t.Fatalf("a rewind with no divergence point should be refused, got %v", err)
	}

	reuse := e
	reuse.EventID = uuid.New()
	reuse.IdempotencyKey = "reuse-branch"
	reuse.BranchID = parentBranch
	if _, err := h.sub.Submit(h.ctx, reuse); !ledger.IsClass(err, ledger.ClassPolicyRejected) {
		t.Fatalf("a rewind onto an existing branch id should be refused, got %v", err)
	}
}

// TestBranchReasonClassIsBounded: reason_class on a branch is a classification
// with a CHECK constraint behind it. An unknown class must be refused as a
// policy decision -- a constraint violation surfacing as a driver error becomes
// a 500 at the gateway, and a durable client retries a 500 forever.
func TestBranchReasonClassIsBounded(t *testing.T) {
	f := loadFixture(t, "branch-rewind")
	h := newHarness(t, f)
	e := f.Steps[0].Submit.event(t)
	parentBranch := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	h.seedRevision(e.PrincipalID, e.ProjectID, e.WorkspaceID, *e.WorkID, parentBranch, e.SessionID, 30)

	bad := e
	bad.EventID = uuid.New()
	bad.IdempotencyKey = "unknown-branch-reason"
	bad.Payload = map[string]any{
		"parent_branch_id": parentBranch.String(),
		"branch_point_seq": 102,
		"reason_class":     "branch",
	}
	if _, err := h.sub.Submit(h.ctx, bad); !ledger.IsClass(err, ledger.ClassPolicyRejected) {
		t.Fatalf("an unknown branch reason should be refused as policy, got %v", err)
	}
	if n := h.scalar(`SELECT count(*) FROM branches WHERE branch_id = $1`, bad.BranchID); n != 0 {
		t.Fatalf("the refused branch was created anyway")
	}
}

// --- candidates and facts ----------------------------------------------------

// TestFixtureCandidatePromotion is INV-10: a candidate is a proposal, it is not
// readable as a fact, and it cannot authorise its own promotion.
func TestFixtureCandidatePromotion(t *testing.T) {
	f := loadFixture(t, "candidate-promotion")
	h := newHarness(t, f)

	propose := f.Steps[0].Submit.event(t)
	promote := f.Steps[3].Submit.event(t)

	// The promotion cites an evidence event, which must be a real event in the
	// project: a reference to nothing reads like corroboration.
	seedSession, seedBranch := h.seedLine("promotion", propose.ProjectID, propose.WorkspaceID, promote.PrincipalID)
	h.seedEventRow(promote.EvidenceRefs[0], propose.ProjectID, propose.WorkspaceID, promote.PrincipalID,
		seedBranch, seedSession, "evidence.recorded")

	if _, err := h.sub.Submit(h.ctx, propose); err != nil {
		t.Fatalf("propose candidate: %v", err)
	}
	candidateID := uuid.MustParse("c0000003-0000-4000-8000-000000000003")
	if n := h.scalar(`SELECT count(*) FROM facts`); n != 0 {
		t.Fatalf("a pending candidate produced %d facts", n)
	}
	if n := h.scalar(`SELECT count(*) FROM fact_slots`); n != 0 {
		t.Fatalf("a pending candidate created %d slots; a proposal must not reserve a predicate", n)
	}
	if n := h.scalar(`SELECT count(*) FROM fact_candidates WHERE candidate_id = $1 AND review_status = 'pending'`,
		candidateID); n != 1 {
		t.Fatalf("the candidate is not pending")
	}
	if n := h.scalar(`SELECT count(*) FROM provenance_refs WHERE candidate_id = $1`, candidateID); n != 1 {
		t.Fatalf("the candidate has %d provenance rows, want 1", n)
	}

	// The extractor holds candidate_write and nothing else; it cannot promote
	// what it proposed.
	if _, err := h.sub.Submit(h.ctx, f.Steps[1].Submit.event(t)); !ledger.IsClass(err, ledger.ClassScopeDenied) {
		t.Fatalf("self-promotion should be scope_denied, got %v", err)
	}
	// A promotion with no evidence is refused even from a principal that holds
	// fact_promote: the role permits promoting, not asserting.
	if _, err := h.sub.Submit(h.ctx, f.Steps[2].Submit.event(t)); !ledger.IsClass(err, ledger.ClassPolicyRejected) {
		t.Fatalf("promotion without evidence should be policy_rejected, got %v", err)
	}
	if n := h.scalar(`SELECT count(*) FROM facts`); n != 0 {
		t.Fatalf("a refused promotion created %d facts", n)
	}

	r, err := h.sub.Submit(h.ctx, promote)
	if err != nil {
		t.Fatalf("authorised promotion: %v", err)
	}
	if r.State != ledger.StateCommitted {
		t.Fatalf("promotion state = %s", r.State)
	}
	slot := domain.SlotID(propose.ProjectID, "postgresql", "archive_mode")
	factID, fact := h.factByObject(slot, `"off"`)
	if fact.status != "active" {
		t.Fatalf("promoted fact status = %s", fact.status)
	}
	if fact.promotedBy != promote.PrincipalID {
		t.Fatal("the fact does not record who promoted it")
	}
	if fact.candidateID == nil || *fact.candidateID != candidateID {
		t.Fatal("the fact does not name the candidate behind it")
	}
	var reviewStatus string
	var promotedFact *uuid.UUID
	if err := h.pool.Pgx().QueryRow(h.ctx,
		`SELECT review_status, promoted_fact_id FROM fact_candidates WHERE candidate_id = $1`,
		candidateID).Scan(&reviewStatus, &promotedFact); err != nil {
		t.Fatal(err)
	}
	if reviewStatus != "promoted" || promotedFact == nil || *promotedFact != factID {
		t.Fatalf("the candidate was not marked promoted: status=%s fact=%v", reviewStatus, promotedFact)
	}
	if n := h.scalar(`SELECT count(*) FROM provenance_refs WHERE fact_id = $1 AND source_type = 'event'`, factID); n != 1 {
		t.Fatalf("the fact carries %d evidence provenance rows, want 1", n)
	}

	// Promoting the same candidate twice is refused: it is already reviewed.
	again := promote
	again.EventID = uuid.New()
	again.IdempotencyKey = "claude:sess-12:promote:0003"
	if _, err := h.sub.Submit(h.ctx, again); !ledger.IsClass(err, ledger.ClassPolicyRejected) {
		t.Fatalf("re-promoting a promoted candidate should be refused, got %v", err)
	}
}

// TestFixtureConflictingFacts is INV-08: two contradicting single-valued
// promotions are both kept and flagged. Nothing is chosen by recency and no
// model is asked to reconcile them.
func TestFixtureConflictingFacts(t *testing.T) {
	f := loadFixture(t, "conflicting-facts")
	h := newHarness(t, f)

	e := f.Steps[0].Submit.event(t)
	owner := uuid.MustParse("55555555-5555-4555-8555-555555555555")
	seedSession, seedBranch := h.seedLine("conflict", e.ProjectID, e.WorkspaceID, owner)
	seedEvent := h.seedEventRow(uuid.Nil, e.ProjectID, e.WorkspaceID, owner, seedBranch, seedSession, "fact.promoted")
	h.seedEventRow(e.EvidenceRefs[0], e.ProjectID, e.WorkspaceID, owner, seedBranch, seedSession, "evidence.recorded")

	slot := h.seedSlot(e.ProjectID, "enowx-rag", "deployment_target", "single", 3)
	standing := uuid.MustParse("f0000001-0000-4000-8000-000000000001")
	h.seedFact(standing, slot, e.ProjectID, e.WorkspaceID, owner, seedEvent,
		"enowx-rag", "deployment_target", `"seiz-vps-arm64"`, "single", 3,
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	h.seedSlotRevision(slot, e.ProjectID, e.WorkspaceID, owner, seedBranch, seedSession, 3)
	h.seedCandidate(uuid.MustParse("c0000001-0000-4000-8000-000000000001"),
		e.ProjectID, e.WorkspaceID, owner, seedEvent,
		"enowx-rag", "deployment_target", `"oracle-vps-amd64"`, "single")

	r, err := h.sub.Submit(h.ctx, e)
	if err != nil {
		t.Fatalf("contradicting promotion: %v", err)
	}
	if r.Revision == nil || *r.Revision != 4 {
		t.Fatalf("resulting revision = %v, fixture says 4", r.Revision)
	}

	newID, newFact := h.factByObject(slot, `"oracle-vps-amd64"`)
	if newFact.status != "conflicted" {
		t.Fatalf("the new value's status = %s, fixture says conflicted", newFact.status)
	}
	old := h.fact(standing)
	if old.status != "conflicted" {
		t.Fatalf("the standing value's status = %s; it must not be dropped or superseded", old.status)
	}
	if old.supersededBy != nil {
		t.Fatal("nothing was chosen by recency, so superseded_by must stay null")
	}
	if newID == standing {
		t.Fatal("the promotion overwrote the standing fact instead of adding a value")
	}
	status, revision := h.slotStatus(slot)
	if status != "conflicted" || revision != 4 {
		t.Fatalf("slot status=%s revision=%d, want conflicted/4", status, revision)
	}
	if n := h.scalar(`SELECT count(*) FROM facts WHERE slot_id = $1 AND status = 'conflicted'`, slot); n != 2 {
		t.Fatalf("%d conflicted values, want both readable", n)
	}
}

// TestExplicitResolutionEndsAConflict: a conflict ends by an explicit
// fact.superseded from a principal holding fact_promote, never by waiting.
func TestExplicitResolutionEndsAConflict(t *testing.T) {
	f := loadFixture(t, "conflicting-facts")
	h := newHarness(t, f)

	e := f.Steps[0].Submit.event(t)
	owner := uuid.MustParse("55555555-5555-4555-8555-555555555555")
	seedSession, seedBranch := h.seedLine("resolve", e.ProjectID, e.WorkspaceID, owner)
	seedEvent := h.seedEventRow(uuid.Nil, e.ProjectID, e.WorkspaceID, owner, seedBranch, seedSession, "fact.promoted")
	h.seedEventRow(e.EvidenceRefs[0], e.ProjectID, e.WorkspaceID, owner, seedBranch, seedSession, "evidence.recorded")
	slot := h.seedSlot(e.ProjectID, "enowx-rag", "deployment_target", "single", 3)
	standing := uuid.MustParse("f0000001-0000-4000-8000-000000000001")
	h.seedFact(standing, slot, e.ProjectID, e.WorkspaceID, owner, seedEvent,
		"enowx-rag", "deployment_target", `"seiz-vps-arm64"`, "single", 3,
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	h.seedSlotRevision(slot, e.ProjectID, e.WorkspaceID, owner, seedBranch, seedSession, 3)
	h.seedCandidate(uuid.MustParse("c0000001-0000-4000-8000-000000000001"),
		e.ProjectID, e.WorkspaceID, owner, seedEvent,
		"enowx-rag", "deployment_target", `"oracle-vps-amd64"`, "single")
	if _, err := h.sub.Submit(h.ctx, e); err != nil {
		t.Fatal(err)
	}
	winner, _ := h.factByObject(slot, `"oracle-vps-amd64"`)

	resolve := e
	resolve.EventID = uuid.New()
	resolve.IdempotencyKey = "opencode:sess-7:supersede:0001"
	resolve.Type = "fact.superseded"
	resolve.Payload = map[string]any{"fact_id": standing.String(), "superseded_by": winner.String()}
	rev := int64(4)
	resolve.ExpectedRevision = &rev
	if _, err := h.sub.Submit(h.ctx, resolve); err != nil {
		t.Fatalf("resolution: %v", err)
	}

	if got := h.fact(standing); got.status != "superseded" || got.supersededBy == nil || *got.supersededBy != winner {
		t.Fatalf("the retired value reads %+v", got)
	}
	if got := h.fact(winner); got.status != "active" {
		t.Fatalf("the surviving value is still %s; resolution left the slot contested", got.status)
	}
	if status, revision := h.slotStatus(slot); status != "active" || revision != 5 {
		t.Fatalf("slot status=%s revision=%d, want active/5", status, revision)
	}
}

// TestFixtureMultiValuedFacts is INV-11: a second value of a multi-valued
// predicate coexists with the first, and contends with nothing.
func TestFixtureMultiValuedFacts(t *testing.T) {
	f := loadFixture(t, "multi-valued-facts")
	h := newHarness(t, f)

	e := f.Steps[0].Submit.event(t)
	owner := e.PrincipalID
	seedSession, seedBranch := h.seedLine("multi", e.ProjectID, e.WorkspaceID, owner)
	seedEvent := h.seedEventRow(uuid.Nil, e.ProjectID, e.WorkspaceID, owner, seedBranch, seedSession, "fact.promoted")
	h.seedEventRow(e.EvidenceRefs[0], e.ProjectID, e.WorkspaceID, owner, seedBranch, seedSession, "evidence.recorded")

	slot := h.seedSlot(e.ProjectID, "enowx-rag", "mcp_client_host", "multi", 1)
	first := uuid.MustParse("f0000002-0000-4000-8000-000000000002")
	h.seedFact(first, slot, e.ProjectID, e.WorkspaceID, owner, seedEvent,
		"enowx-rag", "mcp_client_host", `"windows-desktop"`, "multi", 1,
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	h.seedCandidate(uuid.MustParse("c0000002-0000-4000-8000-000000000002"),
		e.ProjectID, e.WorkspaceID, owner, seedEvent,
		"enowx-rag", "mcp_client_host", `"oracle-vps"`, "multi")

	r, err := h.sub.Submit(h.ctx, e)
	if err != nil {
		t.Fatalf("second value: %v", err)
	}
	if r.Revision != nil {
		t.Fatalf("a multi-valued promotion contends with nothing and must carry no revision, got %d", *r.Revision)
	}

	_, added := h.factByObject(slot, `"oracle-vps"`)
	if added.status != "active" {
		t.Fatalf("the second value's status = %s, fixture says active", added.status)
	}
	if added.revision != 2 {
		t.Fatalf("the second value's revision = %d, want 2", added.revision)
	}
	original := h.fact(first)
	if original.status != "active" || original.revision != 1 || original.supersededBy != nil {
		t.Fatalf("the first value changed: %+v", original)
	}
	if status, revision := h.slotStatus(slot); status != "active" || revision != 1 {
		t.Fatalf("a multi-valued promotion moved the slot to %s/%d", status, revision)
	}
	if n := h.scalar(`SELECT count(*) FROM facts WHERE slot_id = $1 AND status = 'active'`, slot); n != 2 {
		t.Fatalf("%d live values, want 2", n)
	}
}

// TestCardinalityCannotBeChangedByAPromotion: turning a multi-valued predicate
// single would retroactively make coexisting values a conflict nobody declared.
func TestCardinalityCannotBeChangedByAPromotion(t *testing.T) {
	f := loadFixture(t, "multi-valued-facts")
	h := newHarness(t, f)

	e := f.Steps[0].Submit.event(t)
	owner := e.PrincipalID
	seedSession, seedBranch := h.seedLine("cardinality", e.ProjectID, e.WorkspaceID, owner)
	seedEvent := h.seedEventRow(uuid.Nil, e.ProjectID, e.WorkspaceID, owner, seedBranch, seedSession, "fact.promoted")
	h.seedEventRow(e.EvidenceRefs[0], e.ProjectID, e.WorkspaceID, owner, seedBranch, seedSession, "evidence.recorded")
	h.seedSlot(e.ProjectID, "enowx-rag", "mcp_client_host", "multi", 1)
	h.seedCandidate(uuid.MustParse("c0000002-0000-4000-8000-000000000002"),
		e.ProjectID, e.WorkspaceID, owner, seedEvent,
		"enowx-rag", "mcp_client_host", `"oracle-vps"`, "single")

	if _, err := h.sub.Submit(h.ctx, e); !ledger.IsClass(err, ledger.ClassPolicyRejected) {
		t.Fatalf("a cardinality change should be refused, got %v", err)
	}
}

// TestPromotionCannotRedefineWhatItPromotes: a promotion may restate the claim
// for readability, but a restatement that disagrees would let a reviewer
// approve one claim and a writer commit another.
func TestPromotionCannotRedefineWhatItPromotes(t *testing.T) {
	f := loadFixture(t, "multi-valued-facts")
	h := newHarness(t, f)

	e := f.Steps[0].Submit.event(t)
	owner := e.PrincipalID
	seedSession, seedBranch := h.seedLine("redefine", e.ProjectID, e.WorkspaceID, owner)
	seedEvent := h.seedEventRow(uuid.Nil, e.ProjectID, e.WorkspaceID, owner, seedBranch, seedSession, "fact.promoted")
	h.seedEventRow(e.EvidenceRefs[0], e.ProjectID, e.WorkspaceID, owner, seedBranch, seedSession, "evidence.recorded")
	h.seedCandidate(uuid.MustParse("c0000002-0000-4000-8000-000000000002"),
		e.ProjectID, e.WorkspaceID, owner, seedEvent,
		"enowx-rag", "mcp_client_host", `"windows-desktop"`, "multi")

	// The candidate says windows-desktop; the promotion says oracle-vps.
	if _, err := h.sub.Submit(h.ctx, e); !ledger.IsClass(err, ledger.ClassPolicyRejected) {
		t.Fatalf("a promotion restating a different object should be refused, got %v", err)
	}
	if n := h.scalar(`SELECT count(*) FROM facts`); n != 0 {
		t.Fatalf("a refused promotion created %d facts", n)
	}
}

// --- the subagent boundary ---------------------------------------------------

// TestFixtureSubagentEvidence is INV-16: a subagent's finding is input to the
// parent's decision, never a state transition of its own.
func TestFixtureSubagentEvidence(t *testing.T) {
	f := loadFixture(t, "subagent-evidence")
	h := newHarness(t, f)

	evidence := f.Steps[0].Submit.event(t)
	parent := uuid.MustParse("55555555-5555-4555-8555-555555555555")
	h.seedRevision(parent, evidence.ProjectID, evidence.WorkspaceID, *evidence.WorkID,
		evidence.BranchID, "omp:host-a:sess-1", 40)

	r, err := h.sub.Submit(h.ctx, evidence)
	if err != nil {
		t.Fatalf("subagent evidence should be accepted: %v", err)
	}
	if r.Revision != nil {
		t.Fatalf("evidence is additive and must carry no revision, got %d", *r.Revision)
	}
	if n := h.scalar(`SELECT count(*) FROM evidence WHERE work_id = $1 AND principal_id = $2`,
		*evidence.WorkID, evidence.PrincipalID); n != 1 {
		t.Fatalf("the subagent's evidence was not retained (%d rows)", n)
	}

	complete := f.Steps[1].Submit.event(t)
	if _, err := h.sub.Submit(h.ctx, complete); !ledger.IsClass(err, ledger.ClassScopeDenied) {
		t.Fatalf("a subagent completing the parent work should be scope_denied, got %v", err)
	}
	if got := h.currentRevision(*evidence.WorkID); got != 40 {
		t.Fatalf("the refused transition moved the work to revision %d", got)
	}
	var state string
	if err := h.pool.Pgx().QueryRow(h.ctx, `SELECT state FROM works WHERE work_id = $1`, *evidence.WorkID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "active" {
		t.Fatalf("the work state is %s; a subagent moved it", state)
	}
	// The evidence stays non-authoritative rather than becoming state.
	if n := h.scalar(`SELECT count(*) FROM evidence`); n != 1 {
		t.Fatalf("evidence count = %d", n)
	}
}

// TestSubagentCannotCheckpointOrPromote: the boundary is the principal type in
// the principals table, not a field the submission could omit.
func TestSubagentCannotCheckpointOrPromote(t *testing.T) {
	f := loadFixture(t, "subagent-evidence")
	h := newHarness(t, f)
	evidence := f.Steps[0].Submit.event(t)
	parent := uuid.MustParse("55555555-5555-4555-8555-555555555555")
	h.seedRevision(parent, evidence.ProjectID, evidence.WorkspaceID, *evidence.WorkID,
		evidence.BranchID, "omp:host-a:sess-1", 40)

	for _, eventType := range []string{"checkpoint.recorded", "tombstone.issued", "fact.promoted"} {
		e := evidence
		e.EventID = uuid.New()
		e.IdempotencyKey = "subagent:" + eventType
		e.Type = eventType
		e.Payload = map[string]any{"objective": "x", "next_safe_action": "y"}
		rev := int64(40)
		e.ExpectedRevision = &rev
		if _, err := h.sub.Submit(h.ctx, e); !ledger.IsClass(err, ledger.ClassScopeDenied) {
			t.Errorf("subagent %s: want scope_denied, got %v", eventType, err)
		}
	}
}

// --- scope at the persistence boundary ---------------------------------------

// TestUnregisteredProjectIsRefused: the scope columns on an event are foreign
// keys now, and the refusal names nothing about what does exist -- a principal
// must not be able to map the registry by probing it.
func TestUnregisteredProjectIsRefused(t *testing.T) {
	f := loadFixture(t, "duplicate-retry")
	h := newHarness(t, f)
	e := f.Steps[0].Submit.event(t)
	h.seedRevision(e.PrincipalID, e.ProjectID, e.WorkspaceID, *e.WorkID, e.BranchID, e.SessionID, 7)

	stray := e
	stray.EventID = uuid.New()
	stray.IdempotencyKey = "stray-project"
	stray.ProjectID = uuid.New()
	err1 := mustRefuse(t, h, stray)
	if !ledger.IsClass(err1, ledger.ClassScopeDenied) {
		t.Fatalf("unregistered project: want scope_denied, got %v", err1)
	}

	strayWS := e
	strayWS.EventID = uuid.New()
	strayWS.IdempotencyKey = "stray-workspace"
	strayWS.WorkspaceID = uuid.New()
	if err := mustRefuse(t, h, strayWS); !ledger.IsClass(err, ledger.ClassScopeDenied) {
		t.Fatalf("workspace outside the project: want scope_denied, got %v", err)
	}
}

// TestSessionIdCannotMoveBetweenProjectsOrPrincipals: a session id that moved
// would carry its history with it.
func TestSessionIdCannotMoveBetweenProjectsOrPrincipals(t *testing.T) {
	f := loadFixture(t, "duplicate-retry")
	h := newHarness(t, f)
	e := f.Steps[0].Submit.event(t)
	h.seedRevision(e.PrincipalID, e.ProjectID, e.WorkspaceID, *e.WorkID, e.BranchID, e.SessionID, 7)
	if _, err := h.sub.Submit(h.ctx, e); err != nil {
		t.Fatal(err)
	}

	other := uuid.New()
	h.ensureWorkspace(e.ProjectID, other)
	moved := e
	moved.EventID = uuid.New()
	moved.IdempotencyKey = "moved-session"
	moved.WorkspaceID = other
	if err := mustRefuse(t, h, moved); !ledger.IsClass(err, ledger.ClassScopeDenied) {
		t.Fatalf("a session reused in another workspace: want scope_denied, got %v", err)
	}
}

func mustRefuse(t *testing.T, h *harness, e ledger.Event) error {
	t.Helper()
	r, err := h.sub.Submit(h.ctx, e)
	if err == nil {
		t.Fatalf("expected a refusal, got receipt %+v", r)
	}
	return err
}
