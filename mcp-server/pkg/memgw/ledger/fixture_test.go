// The Phase 2 fixtures are the oracle here. Rather than restating what the
// ledger should do in Go -- which would only prove the implementation agrees
// with itself -- these tests read the frozen fixture files and replay their
// steps against a real PostgreSQL, asserting the outcome the fixture declares.
package ledger_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/ledger"
	"github.com/enowdev/enowx-rag/pkg/memgw/memgwtest"
	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
	"github.com/enowdev/enowx-rag/pkg/memgw/principal"
)

const fixtureDir = "../contract/testdata/fixtures"

type fixture struct {
	Name       string         `json:"fixture"`
	Title      string         `json:"title"`
	Invariants []string       `json:"invariants"`
	Principals []fixturePrin  `json:"principals"`
	Given      map[string]any `json:"given"`
	Steps      []fixtureStep  `json:"steps"`
}

type fixturePrin struct {
	ID     string   `json:"principal_id"`
	Type   string   `json:"principal_type"`
	Agent  string   `json:"agent_id"`
	Parent string   `json:"parent_principal_id"`
	Roles  []string `json:"roles"`
	Grants []struct {
		ProjectID   string `json:"project_id"`
		WorkspaceID string `json:"workspace_id"`
	} `json:"grants"`
}

type fixtureStep struct {
	Name      string         `json:"name"`
	Submit    *fixtureSubmit `json:"submit"`
	Operation string         `json:"operation"`
	Expect    fixtureExpect  `json:"expect"`
}

type fixtureSubmit struct {
	EventID          string   `json:"event_id"`
	IdempotencyKey   string   `json:"idempotency_key"`
	PrincipalID      string   `json:"principal_id"`
	ProjectID        string   `json:"project_id"`
	WorkspaceID      string   `json:"workspace_id"`
	WorkID           string   `json:"work_id"`
	SessionID        string   `json:"session_id"`
	BranchID         string   `json:"branch_id"`
	EventType        string   `json:"event_type"`
	Payload          any      `json:"payload"`
	EvidenceRefs     []string `json:"evidence_refs"`
	ExpectedRevision *int64   `json:"expected_revision"`
	SensitivityClass string   `json:"sensitivity_class"`
	PolicyVersion    string   `json:"policy_version"`
	SchemaVersion    string   `json:"schema_version"`
	OccurredAt       string   `json:"occurred_at"`
}

type fixtureExpect struct {
	Outcome           string `json:"outcome"`
	ReceiptState      string `json:"receipt_state"`
	ErrorClass        string `json:"error_class"`
	Effect            string `json:"effect"`
	ResultingRevision *int64 `json:"resulting_revision"`
}

func loadFixture(t *testing.T, name string) fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, name+".json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	return f
}

// harness is one migrated schema with the fixture's principals and grants
// installed. Grants are derived from the fixture's declared roles, so a fixture
// that says a principal holds checkpoint_write gets exactly that and no more.
type harness struct {
	t    *testing.T
	pool *pgstore.Pool
	sub  *ledger.Submitter
	auth *principal.Store
	ctx  context.Context
}

func newHarness(t *testing.T, f fixture) *harness {
	t.Helper()
	pool := memgwtest.Pool(t)
	h := &harness{
		t:    t,
		pool: pool,
		sub:  ledger.NewSubmitter(pool),
		auth: principal.NewStore(pool),
		ctx:  t.Context(),
	}
	// The registry comes first: grants carry a foreign key to a project, and a
	// fixture that could grant a role over a project nobody registered would be
	// testing an authorization system with no subject.
	h.seedRegistry(f)
	for _, fp := range f.Principals {
		h.installPrincipal(fp, f)
	}
	return h
}

// seedRegistry creates the projects and workspaces the fixture's ids name.
//
// The frozen fixtures predate the registry and carry bare uuids, because what
// they are an oracle for is the write path, not how a host comes to know its
// project id. Registering them here is the step a real host performs once, at
// install time, and doing it in the harness keeps the fixtures unmodified.
func (h *harness) seedRegistry(f fixture) {
	h.t.Helper()
	for _, s := range f.Steps {
		if s.Submit == nil {
			continue
		}
		h.ensureProject(mustUUID(h.t, s.Submit.ProjectID))
		h.ensureWorkspace(mustUUID(h.t, s.Submit.ProjectID), mustUUID(h.t, s.Submit.WorkspaceID))
	}
	for _, fp := range f.Principals {
		for _, g := range fp.Grants {
			h.ensureProject(mustUUID(h.t, g.ProjectID))
			h.ensureWorkspace(mustUUID(h.t, g.ProjectID), mustUUID(h.t, g.WorkspaceID))
		}
	}
}

func (h *harness) ensureProject(id uuid.UUID) {
	h.t.Helper()
	memgwtest.SeedProject(h.t, h.pool, id)
}

func (h *harness) ensureWorkspace(projectID, workspaceID uuid.UUID) {
	h.t.Helper()
	memgwtest.SeedWorkspace(h.t, h.pool, projectID, workspaceID)
}

func (h *harness) ensureSession(sessionID string, projectID, workspaceID, principalID uuid.UUID) {
	h.t.Helper()
	memgwtest.SeedSession(h.t, h.pool, sessionID, projectID, workspaceID, principalID)
}

func (h *harness) ensureBranch(branchID, projectID uuid.UUID, sessionID string) {
	h.t.Helper()
	memgwtest.SeedBranch(h.t, h.pool, branchID, projectID, sessionID)
}

// ensureWork materialises the work the fixture's "given" block assumes already
// exists, in the state a checkpoint can still be taken against.
func (h *harness) ensureWork(workID, projectID, workspaceID uuid.UUID, state string, revision int64) {
	h.t.Helper()
	if workID == uuid.Nil {
		return
	}
	if _, err := h.pool.Pgx().Exec(h.ctx, `
		INSERT INTO works (work_id, project_id, workspace_id, title, state, revision, last_event_seq)
		VALUES ($1,$2,$3,'fixture work',$4,$5,0)
		ON CONFLICT (work_id) DO UPDATE SET state = EXCLUDED.state, revision = EXCLUDED.revision`,
		workID, projectID, workspaceID, state, revision); err != nil {
		h.t.Fatalf("seed work: %v", err)
	}
}

func (h *harness) installPrincipal(fp fixturePrin, f fixture) {
	h.t.Helper()
	id := mustUUID(h.t, fp.ID)
	// The parent link is installed from the fixture rather than inferred: the
	// domain reads the principal type from this row, and a subagent that could
	// be installed as an agent would make INV-16 untestable.
	var parent *uuid.UUID
	if fp.Parent != "" {
		pid := mustUUID(h.t, fp.Parent)
		parent = &pid
	}
	if _, err := h.auth.CreatePrincipal(h.ctx, principal.Principal{
		ID:          id,
		Type:        principal.Type(fp.Type),
		HostID:      uuid.New(),
		AgentID:     fp.Agent,
		ParentID:    parent,
		DisplayName: fp.Agent,
	}); err != nil {
		h.t.Fatalf("install principal %s: %v", fp.Agent, err)
	}

	// Where the fixture states grants explicitly, use them. Where it only lists
	// roles, grant those roles over the project and workspace its steps target.
	scopes := fp.Grants
	if len(scopes) == 0 {
		for _, s := range f.Steps {
			if s.Submit == nil || s.Submit.PrincipalID != fp.ID {
				continue
			}
			scopes = append(scopes, struct {
				ProjectID   string `json:"project_id"`
				WorkspaceID string `json:"workspace_id"`
			}{ProjectID: s.Submit.ProjectID})
			break
		}
	}
	for _, role := range fp.Roles {
		for _, sc := range scopes {
			g := principal.Grant{
				PrincipalID: id,
				Role:        principal.Role(role),
				ProjectID:   mustUUID(h.t, sc.ProjectID),
				Provenance:  "fixture " + f.Name,
			}
			if sc.WorkspaceID != "" {
				ws := mustUUID(h.t, sc.WorkspaceID)
				g.WorkspaceID = &ws
			}
			if _, err := h.auth.GrantScope(h.ctx, g); err != nil {
				h.t.Fatalf("grant %s: %v", role, err)
			}
		}
	}
}

// seedRevision fabricates the aggregate history the fixture's "given" block
// assumes. One synthetic event carrying the stated revision is enough: the
// ledger derives the current revision from MAX(revision), not from a count.
func (h *harness) seedRevision(principalID, projectID, workspaceID, workID, branchID uuid.UUID, sessionID string, revision int64) {
	h.t.Helper()
	h.ensureProject(projectID)
	h.ensureWorkspace(projectID, workspaceID)
	h.ensureSession(sessionID, projectID, workspaceID, principalID)
	h.ensureBranch(branchID, projectID, sessionID)
	h.ensureWork(workID, projectID, workspaceID, "active", revision)
	if _, err := h.pool.Pgx().Exec(h.ctx, `
		INSERT INTO events (event_id, idempotency_key, principal_id, project_id, workspace_id, work_id,
		                    session_id, branch_id, event_type, payload, payload_digest, sensitivity_class,
		                    policy_version, schema_version, occurred_at, writer_epoch,
		                    aggregate_type, aggregate_id, revision)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'work.planned','{"seed":true}',
		        repeat('0', 64),'internal','1.0.0','1.0.0', now(), 1, 'work', $6, $9)`,
		uuid.New(), "seed:"+uuid.NewString(), principalID, projectID, workspaceID, workID,
		sessionID, branchID, revision); err != nil {
		h.t.Fatalf("seed revision %d: %v", revision, err)
	}
}

func (s fixtureSubmit) event(t *testing.T) ledger.Event {
	t.Helper()
	occurred, err := time.Parse(time.RFC3339, s.OccurredAt)
	if err != nil {
		t.Fatalf("occurred_at %q: %v", s.OccurredAt, err)
	}
	e := ledger.Event{
		EventID:          mustUUID(t, s.EventID),
		IdempotencyKey:   s.IdempotencyKey,
		PrincipalID:      mustUUID(t, s.PrincipalID),
		ProjectID:        mustUUID(t, s.ProjectID),
		WorkspaceID:      mustUUID(t, s.WorkspaceID),
		SessionID:        s.SessionID,
		BranchID:         mustUUID(t, s.BranchID),
		Type:             s.EventType,
		Payload:          s.Payload,
		ExpectedRevision: s.ExpectedRevision,
		SensitivityClass: s.SensitivityClass,
		PolicyVersion:    s.PolicyVersion,
		SchemaVersion:    s.SchemaVersion,
		OccurredAt:       occurred,
		WriterEpoch:      1,
	}
	for _, ref := range s.EvidenceRefs {
		e.EvidenceRefs = append(e.EvidenceRefs, mustUUID(t, ref))
	}
	if s.WorkID != "" {
		w := mustUUID(t, s.WorkID)
		e.WorkID = &w
	}
	return e
}

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	if s == "" {
		return uuid.Nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("fixture uuid %q: %v", s, err)
	}
	return id
}

// currentRevision reads the aggregate revision the same way the ledger does.
func (h *harness) currentRevision(aggregateID uuid.UUID) int64 {
	h.t.Helper()
	var rev int64
	if err := h.pool.Pgx().QueryRow(h.ctx,
		`SELECT COALESCE(MAX(revision), 0) FROM events WHERE aggregate_type = 'work' AND aggregate_id = $1`,
		aggregateID).Scan(&rev); err != nil {
		h.t.Fatalf("read revision: %v", err)
	}
	return rev
}

func (h *harness) countEvents() int64 {
	h.t.Helper()
	var n int64
	if err := h.pool.Pgx().QueryRow(h.ctx, `SELECT count(*) FROM events`).Scan(&n); err != nil {
		h.t.Fatalf("count events: %v", err)
	}
	return n
}

func (h *harness) countOutbox() int64 {
	h.t.Helper()
	var n int64
	if err := h.pool.Pgx().QueryRow(h.ctx, `SELECT count(*) FROM projection_outbox`).Scan(&n); err != nil {
		h.t.Fatalf("count outbox: %v", err)
	}
	return n
}

func (h *harness) countReceipts() int64 {
	h.t.Helper()
	var n int64
	if err := h.pool.Pgx().QueryRow(h.ctx, `SELECT count(*) FROM write_receipts`).Scan(&n); err != nil {
		h.t.Fatalf("count receipts: %v", err)
	}
	return n
}

// seedRevisionEpoch is seedRevision for a test that has fenced epoch 1: the
// synthetic history has to be stamped with an epoch that is still admitted, or
// the seed itself would violate the foreign key it is standing in for.
func (h *harness) seedRevisionEpoch(e ledger.Event, revision, epoch int64) {
	h.t.Helper()
	h.ensureProject(e.ProjectID)
	h.ensureWorkspace(e.ProjectID, e.WorkspaceID)
	h.ensureSession(e.SessionID, e.ProjectID, e.WorkspaceID, e.PrincipalID)
	h.ensureBranch(e.BranchID, e.ProjectID, e.SessionID)
	if e.WorkID != nil {
		h.ensureWork(*e.WorkID, e.ProjectID, e.WorkspaceID, "active", revision)
	}
	if _, err := h.pool.Pgx().Exec(h.ctx, `
		INSERT INTO events (event_id, idempotency_key, principal_id, project_id, workspace_id, work_id,
		                    session_id, branch_id, event_type, payload, payload_digest, sensitivity_class,
		                    policy_version, schema_version, occurred_at, writer_epoch,
		                    aggregate_type, aggregate_id, revision)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'work.planned','{"seed":true}',
		        repeat('0', 64),'internal','1.0.0','1.0.0', now(), $10, 'work', $6, $9)`,
		uuid.New(), "seed:"+uuid.NewString(), e.PrincipalID, e.ProjectID, e.WorkspaceID, e.WorkID,
		e.SessionID, e.BranchID, revision, epoch); err != nil {
		h.t.Fatalf("seed revision %d at epoch %d: %v", revision, epoch, err)
	}
}

// installOperator creates an admin principal scoped to the given project, for
// tests that need an operation the fixture's own principals may not perform.
func (h *harness) installOperator(projectID, workspaceID uuid.UUID) uuid.UUID {
	h.t.Helper()
	p, err := h.auth.CreatePrincipal(h.ctx, principal.Principal{
		Type: principal.TypeHuman, HostID: uuid.New(), AgentID: "operator", DisplayName: "operator",
	})
	if err != nil {
		h.t.Fatalf("install operator: %v", err)
	}
	if _, err := h.auth.GrantScope(h.ctx, principal.Grant{
		PrincipalID: p.ID, Role: principal.RoleAdmin, ProjectID: projectID,
		Provenance: "test operator",
	}); err != nil {
		h.t.Fatalf("grant admin: %v", err)
	}
	_ = workspaceID
	return p.ID
}
