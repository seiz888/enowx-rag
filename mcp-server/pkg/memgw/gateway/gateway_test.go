// The gateway over real HTTP against a real ledger.
//
// Everything here goes through an httptest server and a live PostgreSQL,
// because the claims being made are about a request: that an unauthenticated
// one reaches nothing, that a body cannot name its own principal, that a replay
// is answered from the receipt rather than written twice. None of those can be
// demonstrated by calling a Go function.
package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/gateway"
	"github.com/enowdev/enowx-rag/pkg/memgw/memgwtest"
	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
	"github.com/enowdev/enowx-rag/pkg/memgw/principal"
)

type env struct {
	t           *testing.T
	ctx         context.Context
	pool        *pgstore.Pool
	store       *principal.Store
	srv         *httptest.Server
	projectID   uuid.UUID
	workspaceID uuid.UUID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := memgwtest.Pool(t)
	e := &env{
		t:           t,
		ctx:         t.Context(),
		pool:        pool,
		store:       principal.NewStore(pool),
		projectID:   uuid.New(),
		workspaceID: uuid.New(),
	}
	memgwtest.SeedWorkspace(t, pool, e.projectID, e.workspaceID)

	// Mounted the way the service mounts it, so the test exercises the real
	// path prefix rather than a bare subrouter that would hide a routing bug.
	r := chi.NewRouter()
	r.Mount(gateway.MountPath, gateway.New(pool).Routes())
	e.srv = httptest.NewServer(r)
	t.Cleanup(e.srv.Close)
	return e
}

// actor is a principal plus the credential it presents.
type actor struct {
	principal.Principal
	token string
}

func (e *env) actor(name string, roles ...principal.Role) actor {
	e.t.Helper()
	p, err := e.store.CreatePrincipal(e.ctx, principal.Principal{
		Type: principal.TypeAgent, HostID: uuid.New(), AgentID: name, DisplayName: name,
	})
	if err != nil {
		e.t.Fatalf("create principal: %v", err)
	}
	for _, role := range roles {
		if _, err := e.store.GrantScope(e.ctx, principal.Grant{
			PrincipalID: p.ID, Role: role, ProjectID: e.projectID, Provenance: "test",
		}); err != nil {
			e.t.Fatalf("grant %s: %v", role, err)
		}
	}
	_, token, err := e.store.Issue(e.ctx, p.ID, name, nil)
	if err != nil {
		e.t.Fatalf("issue credential: %v", err)
	}
	return actor{Principal: p, token: token}
}

func (e *env) do(method, path, token string, body any) (int, []byte) {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(e.ctx, method, e.srv.URL+gateway.MountPath+path, r)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp.StatusCode, out
}

func (e *env) scalar(query string, args ...any) int64 {
	e.t.Helper()
	var n int64
	if err := e.pool.Pgx().QueryRow(e.ctx, query, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// submission is written as a map rather than a struct so a test can put a field
// on the wire that the server's type does not have -- which is precisely what
// the principal_id test needs to do.
func (e *env) submission(a actor, eventType, key string, extra map[string]any) map[string]any {
	body := map[string]any{
		"event_id":          uuid.New().String(),
		"idempotency_key":   key,
		"project_id":        e.projectID.String(),
		"workspace_id":      e.workspaceID.String(),
		"session_id":        a.AgentID + ":host:sess-1",
		"branch_id":         branchFor(a).String(),
		"type":              eventType,
		"sensitivity_class": "internal",
		"policy_version":    "1.0.0",
		"schema_version":    "1.0.0",
		"occurred_at":       time.Now().UTC().Format(time.RFC3339Nano),
		"writer_epoch":      1,
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// branchFor gives each actor one stable branch, derived from its id so a test
// does not have to thread one through every call.
func branchFor(a actor) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("branch:"+a.ID.String()))
}

func decodeInto(t *testing.T, raw []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, raw)
	}
}

func class(t *testing.T, raw []byte) string {
	t.Helper()
	var p struct {
		Class   string `json:"class"`
		Message string `json:"message"`
	}
	decodeInto(t, raw, &p)
	return p.Class
}

func workPayload(title string) map[string]any {
	return map[string]any{"title": title, "reason_class": "planned"}
}

func checkpointPayload(objective, next string) map[string]any {
	return map[string]any{
		"objective": objective, "completed_work": "", "pending_actions": "",
		"blockers": nil, "next_safe_action": next, "modified_files": []string{},
	}
}

// ---------------------------------------------------------------------------

// TestUnauthenticatedRequestsReachNothing: there is no route that answers
// without a credential, including the ones that only read. A gateway with one
// open route is a gateway with one route worth attacking.
func TestUnauthenticatedRequestsReachNothing(t *testing.T) {
	e := newEnv(t)
	routes := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/v1/events", map[string]any{}},
		{http.MethodGet, "/v1/receipts/anything", nil},
		{http.MethodPost, "/v1/bootstrap", map[string]any{}},
		{http.MethodPost, "/v1/projects", map[string]any{"slug": "x"}},
		{http.MethodPost, "/v1/workspaces", map[string]any{}},
		{http.MethodGet, "/v1/projects/by-repo?remote=https://github.com/a/b", nil},
	}
	for _, r := range routes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			for _, token := range []string{"", "not-a-memgw-token", "memgw_" + uuid.NewString() + ".AAAA"} {
				status, body := e.do(r.method, r.path, token, r.body)
				if status != http.StatusUnauthorized {
					t.Fatalf("token %q got %d: %s", token, status, body)
				}
				if got := class(t, body); got != "unauthenticated" {
					t.Fatalf("class %q", got)
				}
			}
		})
	}
	if n := e.scalar(`SELECT count(*) FROM events`); n != 0 {
		t.Fatalf("%d events were written by unauthenticated requests", n)
	}
}

// TestTheSharedAdminTokenIsNotACredential. The service's other routes accept
// RAG_ADMIN_TOKEN; this one must not, whatever it happens to be set to. It is
// not a principal, and a gateway that accepted it would have per-principal
// grants on paper and one shared key in practice.
func TestTheSharedAdminTokenIsNotACredential(t *testing.T) {
	e := newEnv(t)
	t.Setenv("RAG_ADMIN_TOKEN", "a-shared-bearer-that-names-nobody")
	status, body := e.do(http.MethodPost, "/v1/bootstrap", "a-shared-bearer-that-names-nobody",
		map[string]any{"project_id": e.projectID.String(), "workspace_id": e.workspaceID.String()})
	if status != http.StatusUnauthorized {
		t.Fatalf("the shared bearer was accepted: %d %s", status, body)
	}
}

// TestTheBodyCannotNameItsOwnPrincipal: the write is attributed to the
// credential, and a body that tries to say otherwise is refused outright rather
// than having its claim quietly ignored. A silently ignored field is a field a
// client author believes in.
func TestTheBodyCannotNameItsOwnPrincipal(t *testing.T) {
	e := newEnv(t)
	writer := e.actor("writer", principal.RoleCheckpointWrite, principal.RoleWorkRead)
	victim := e.actor("victim", principal.RoleCheckpointWrite)

	workID := uuid.New()
	body := e.submission(writer, "work.planned", "writer:plan:1", map[string]any{
		"work_id": workID.String(), "payload": workPayload("impersonation attempt"),
	})
	body["principal_id"] = victim.ID.String()

	status, raw := e.do(http.MethodPost, "/v1/events", writer.token, body)
	if status != http.StatusBadRequest {
		t.Fatalf("a body naming a principal got %d: %s", status, raw)
	}

	// Without the field it commits, and the event carries the authenticated
	// principal.
	delete(body, "principal_id")
	status, raw = e.do(http.MethodPost, "/v1/events", writer.token, body)
	if status != http.StatusCreated {
		t.Fatalf("submit: %d %s", status, raw)
	}
	if n := e.scalar(`SELECT count(*) FROM events WHERE principal_id = $1`, writer.ID); n != 1 {
		t.Fatalf("%d events attributed to the writer", n)
	}
	if n := e.scalar(`SELECT count(*) FROM events WHERE principal_id = $1`, victim.ID); n != 0 {
		t.Fatalf("%d events attributed to the victim", n)
	}
}

// TestReplayIsAnsweredFromTheReceipt: the second identical request writes
// nothing and returns the first receipt. This is what makes a retry after a
// timeout safe to send blindly, which is the only retry an offline host can
// send.
func TestReplayIsAnsweredFromTheReceipt(t *testing.T) {
	e := newEnv(t)
	writer := e.actor("writer", principal.RoleCheckpointWrite)
	workID := uuid.New()
	body := e.submission(writer, "work.planned", "writer:plan:1", map[string]any{
		"work_id": workID.String(), "payload": workPayload("the work"),
	})

	status, raw := e.do(http.MethodPost, "/v1/events", writer.token, body)
	if status != http.StatusCreated {
		t.Fatalf("first submit: %d %s", status, raw)
	}
	var first struct {
		EventID uuid.UUID `json:"event_id"`
		State   string    `json:"state"`
		Seq     int64     `json:"seq"`
	}
	decodeInto(t, raw, &first)

	status, raw = e.do(http.MethodPost, "/v1/events", writer.token, body)
	if status != http.StatusOK {
		t.Fatalf("replay: %d %s", status, raw)
	}
	var second struct {
		EventID uuid.UUID `json:"event_id"`
		State   string    `json:"state"`
		Seq     int64     `json:"seq"`
	}
	decodeInto(t, raw, &second)
	if second.State != "duplicate" {
		t.Fatalf("replay state is %q", second.State)
	}
	if second.EventID != first.EventID || second.Seq != first.Seq {
		t.Fatalf("the replay got a different receipt: %+v vs %+v", second, first)
	}
	if n := e.scalar(`SELECT count(*) FROM events`); n != 1 {
		t.Fatalf("a replay wrote %d events", n)
	}

	// A different payload under the same key is a mismatch, not a duplicate.
	body["payload"] = workPayload("a different work entirely")
	body["event_id"] = uuid.New().String()
	status, raw = e.do(http.MethodPost, "/v1/events", writer.token, body)
	if status != http.StatusConflict || class(t, raw) != "idempotency_payload_mismatch" {
		t.Fatalf("payload mismatch got %d %s", status, raw)
	}
}

// TestReceiptsArePerPrincipal: a receipt lookup is how a client resolves
// unknown_commit_status, and it must not become a way to read what another
// principal wrote. The answer for someone else's key is "no such receipt", not
// "not yours" -- which would confirm the key exists.
func TestReceiptsArePerPrincipal(t *testing.T) {
	e := newEnv(t)
	writer := e.actor("writer", principal.RoleCheckpointWrite)
	other := e.actor("other", principal.RoleCheckpointWrite)

	key := "writer:plan:1"
	status, raw := e.do(http.MethodPost, "/v1/events", writer.token,
		e.submission(writer, "work.planned", key, map[string]any{
			"work_id": uuid.New().String(), "payload": workPayload("a work"),
		}))
	if status != http.StatusCreated {
		t.Fatalf("submit: %d %s", status, raw)
	}

	if status, raw := e.do(http.MethodGet, "/v1/receipts/"+key, writer.token, nil); status != http.StatusOK {
		t.Fatalf("the writer cannot read its own receipt: %d %s", status, raw)
	}
	if status, raw := e.do(http.MethodGet, "/v1/receipts/"+key, other.token, nil); status != http.StatusNotFound {
		t.Fatalf("another principal read the receipt: %d %s", status, raw)
	}
}

// TestAuthenticatedIsNotAuthorized: a valid credential with no grant in the
// scope is 403, and nothing is written. Authentication answers who; the grant
// still answers whether.
func TestAuthenticatedIsNotAuthorized(t *testing.T) {
	e := newEnv(t)
	nobody := e.actor("nobody")
	status, raw := e.do(http.MethodPost, "/v1/events", nobody.token,
		e.submission(nobody, "work.planned", "nobody:plan:1", map[string]any{
			"work_id": uuid.New().String(), "payload": workPayload("a work"),
		}))
	if status != http.StatusForbidden || class(t, raw) != "scope_denied" {
		t.Fatalf("want 403 scope_denied, got %d %s", status, raw)
	}
	if n := e.scalar(`SELECT count(*) FROM events`); n != 0 {
		t.Fatalf("%d events written without a grant", n)
	}
}

// TestARefusedTransitionIsUnprocessableNotServerError: the domain's refusals
// reach the client as the frozen class, which is what an adapter branches on.
// A 500 would tell every adapter to retry forever.
func TestARefusedTransitionIsUnprocessableNotServerError(t *testing.T) {
	e := newEnv(t)
	writer := e.actor("writer", principal.RoleCheckpointWrite)
	workID := uuid.New()

	status, raw := e.do(http.MethodPost, "/v1/events", writer.token,
		e.submission(writer, "work.planned", "writer:plan:1", map[string]any{
			"work_id": workID.String(), "payload": workPayload("a work"),
		}))
	if status != http.StatusCreated {
		t.Fatalf("plan: %d %s", status, raw)
	}
	// planned -> completed is not in the transition table.
	rev := int64(1)
	status, raw = e.do(http.MethodPost, "/v1/events", writer.token,
		e.submission(writer, "work.completed", "writer:complete:1", map[string]any{
			"work_id": workID.String(), "payload": workPayload("a work"),
			"expected_revision": rev,
		}))
	if status != http.StatusUnprocessableEntity || class(t, raw) != "policy_rejected" {
		t.Fatalf("want 422 policy_rejected, got %d %s", status, raw)
	}
}

// TestRegistryStoresNoPathAndNoRemoteCredential: the workspace root and the git
// remote both arrive in the clear and neither is stored that way. A path names
// a user; a remote can carry a token.
func TestRegistryStoresNoPathAndNoRemoteCredential(t *testing.T) {
	e := newEnv(t)
	admin := e.actor("admin", principal.RoleAdmin)

	planted := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	status, raw := e.do(http.MethodPost, "/v1/projects", admin.token, map[string]any{
		"slug": "enowx-rag", "display_name": "enowx-rag",
		"repo_remote": "https://x-access-token:" + planted + "@github.com/enowdev/enowx-rag.git",
	})
	if status != http.StatusOK {
		t.Fatalf("ensure project: %d %s", status, raw)
	}
	var project struct {
		ProjectID uuid.UUID `json:"project_id"`
		RepoKey   string    `json:"repo_key"`
	}
	decodeInto(t, raw, &project)
	if project.RepoKey != "github.com/enowdev/enowx-rag" {
		t.Fatalf("repo key is %q", project.RepoKey)
	}
	if n := e.scalar(`SELECT count(*) FROM project_repos WHERE repo_key LIKE '%' || $1 || '%'`, planted); n != 0 {
		t.Fatal("the stored repo key carries the token")
	}

	root := `D:\PROJECTS\enowx-rag`
	status, raw = e.do(http.MethodPost, "/v1/workspaces", admin.token, map[string]any{
		"project_id": project.ProjectID.String(), "host_id": uuid.New().String(),
		"root_path": root, "label": "windows",
	})
	if status != http.StatusOK {
		t.Fatalf("ensure workspace: %d %s", status, raw)
	}
	var ws struct {
		RootPathDigest string `json:"root_path_digest"`
	}
	decodeInto(t, raw, &ws)
	if len(ws.RootPathDigest) != 64 {
		t.Fatalf("digest is %q", ws.RootPathDigest)
	}
	if n := e.scalar(`SELECT count(*) FROM workspaces WHERE root_path_digest = $1`, root); n != 0 {
		t.Fatal("the raw path was stored")
	}

	// And the mapping is resolvable by a host that only knows its remote.
	status, raw = e.do(http.MethodGet,
		"/v1/projects/by-repo?remote=git@github.com:enowdev/enowx-rag.git", admin.token, nil)
	if status != http.StatusOK {
		t.Fatalf("by-repo: %d %s", status, raw)
	}
	var found struct {
		ProjectID uuid.UUID `json:"project_id"`
	}
	decodeInto(t, raw, &found)
	if found.ProjectID != project.ProjectID {
		t.Fatalf("a different spelling of the remote resolved to %s", found.ProjectID)
	}
}

// TestOversizedAndMalformedBodies: the gateway reads a bounded body and answers
// a bad one with a class, not a stack trace.
func TestOversizedAndMalformedBodies(t *testing.T) {
	e := newEnv(t)
	writer := e.actor("writer", principal.RoleCheckpointWrite)

	huge := e.submission(writer, "checkpoint.recorded", "writer:big:1", map[string]any{
		"work_id": uuid.New().String(),
		"payload": checkpointPayload(string(bytes.Repeat([]byte("a"), 2<<20)), "stop"),
	})
	status, raw := e.do(http.MethodPost, "/v1/events", writer.token, huge)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized body got %d: %s", status, raw)
	}

	req, err := http.NewRequestWithContext(e.ctx, http.MethodPost,
		e.srv.URL+gateway.MountPath+"/v1/events", bytes.NewBufferString(`{"type":"work.planned"}{"type":"work.planned"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+writer.token)
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("two JSON documents in one body got %d", resp.StatusCode)
	}
	if n := e.scalar(`SELECT count(*) FROM events`); n != 0 {
		t.Fatalf("%d events written by malformed requests", n)
	}
}

// TestEnsureWorkSelectsThenCreates: the work-ensure route names the work a
// session belongs to without the caller ever supplying a work id. The first
// call creates a planned work; a second call returns the same work rather than
// a second; and once that work is completed the next call creates a fresh one.
func TestEnsureWorkSelectsThenCreates(t *testing.T) {
	e := newEnv(t)
	writer := e.actor("writer", principal.RoleCheckpointWrite)

	var first struct {
		WorkID    uuid.UUID `json:"work_id"`
		Title     string    `json:"title"`
		State     string    `json:"state"`
		Created   bool      `json:"created"`
		ProjectID uuid.UUID `json:"project_id"`
	}
	status, raw := e.do(http.MethodPost, "/v1/works/ensure", writer.token, map[string]any{
		"project_id": e.projectID.String(), "workspace_id": e.workspaceID.String(),
	})
	if status != http.StatusCreated {
		t.Fatalf("first ensure got %d: %s", status, raw)
	}
	decodeInto(t, raw, &first)
	if !first.Created {
		t.Fatal("the first ensure should create a work")
	}
	if first.WorkID == uuid.Nil || first.ProjectID != e.projectID {
		t.Fatalf("ensure returned an unusable work: %+v", first)
	}
	if first.State != "planned" {
		t.Fatalf("a freshly ensured work should be planned, got %q", first.State)
	}

	// The second call names the same work; nothing new is created.
	var second struct {
		WorkID  uuid.UUID `json:"work_id"`
		Created bool      `json:"created"`
	}
	status, raw = e.do(http.MethodPost, "/v1/works/ensure", writer.token, map[string]any{
		"project_id": e.projectID.String(), "workspace_id": e.workspaceID.String(),
	})
	if status != http.StatusOK {
		t.Fatalf("second ensure got %d: %s", status, raw)
	}
	decodeInto(t, raw, &second)
	if second.Created {
		t.Fatal("the second ensure should select, not create")
	}
	if second.WorkID != first.WorkID {
		t.Fatalf("the second ensure named %s, not %s", second.WorkID, first.WorkID)
	}

	// Activate then complete the work through the event path, then ensure again:
	// a new work. (planned -> active -> completed; planned -> completed is not a
	// legal transition, which is the point of the state machine.)
	body := e.submission(writer, "work.activated", "writer:activate:1", map[string]any{
		"work_id": first.WorkID.String(), "payload": workPayload("done"), "expected_revision": 1,
	})
	if status, raw = e.do(http.MethodPost, "/v1/events", writer.token, body); status != http.StatusCreated {
		t.Fatalf("activate: %d %s", status, raw)
	}
	body = e.submission(writer, "work.completed", "writer:complete:1", map[string]any{
		"work_id": first.WorkID.String(), "payload": workPayload("done"), "expected_revision": 2,
	})
	if status, raw = e.do(http.MethodPost, "/v1/events", writer.token, body); status != http.StatusCreated {
		t.Fatalf("complete: %d %s", status, raw)
	}
	var third struct {
		WorkID  uuid.UUID `json:"work_id"`
		Created bool      `json:"created"`
	}
	status, raw = e.do(http.MethodPost, "/v1/works/ensure", writer.token, map[string]any{
		"project_id": e.projectID.String(), "workspace_id": e.workspaceID.String(),
	})
	if status != http.StatusCreated {
		t.Fatalf("ensure after complete got %d: %s", status, raw)
	}
	decodeInto(t, raw, &third)
	if !third.Created || third.WorkID == first.WorkID {
		t.Fatalf("a completed work must not be the current work: created=%v id=%s", third.Created, third.WorkID)
	}
}

// TestEnsureWorkRefusesWithoutTheRole: creating work is a checkpoint_write
// act; a principal that can only read must not be able to conjure work.
func TestEnsureWorkRefusesWithoutTheRole(t *testing.T) {
	e := newEnv(t)
	reader := e.actor("reader", principal.RoleWorkRead)
	status, raw := e.do(http.MethodPost, "/v1/works/ensure", reader.token, map[string]any{
		"project_id": e.projectID.String(), "workspace_id": e.workspaceID.String(),
	})
	if status != http.StatusForbidden && status != http.StatusUnauthorized {
		t.Fatalf("a read-only principal got %d: %s", status, raw)
	}
	if n := e.scalar(`SELECT count(*) FROM works`); n != 0 {
		t.Fatalf("%d works created by a read-only principal", n)
	}
}
