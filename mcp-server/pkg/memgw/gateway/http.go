package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/domain"
	"github.com/enowdev/enowx-rag/pkg/memgw/errclass"
	"github.com/enowdev/enowx-rag/pkg/memgw/ledger"
	"github.com/enowdev/enowx-rag/pkg/memgw/principal"
)

// workEnsureNamespace derives the event id of a work.planned event the
// ensure-work route creates, from its idempotency key. Deriving rather than
// generating means a retried creation reuses the same event id, which is what
// makes the event idempotent across restarts.
var workEnsureNamespace = uuid.MustParse("6d656d67-772d-776f-726b-656e73757265")

// problem is the error body. It carries the frozen error class verbatim,
// because that class is what a client is written against: "cas_conflict" tells
// an adapter to re-read and retry, "policy_rejected" tells it not to.
type problem struct {
	Class   string `json:"class"`
	Message string `json:"message"`
}

// statusForClass maps the frozen classes onto HTTP.
//
// duplicate is absent on purpose: a duplicate is not an error, it is a receipt
// that says the write already happened, and it is answered with 200.
var statusForClass = map[errclass.Class]int{
	errclass.PayloadMismatch:     http.StatusConflict,
	errclass.CASConflict:         http.StatusConflict,
	errclass.StaleRevision:       http.StatusConflict,
	errclass.WriterEpochFenced:   http.StatusConflict,
	errclass.ScopeDenied:         http.StatusForbidden,
	errclass.PolicyRejected:      http.StatusUnprocessableEntity,
	errclass.Quarantined:         http.StatusUnprocessableEntity,
	errclass.PayloadTooLarge:     http.StatusRequestEntityTooLarge,
	errclass.UnknownCommitStatus: http.StatusServiceUnavailable,
}

func writeProblem(w http.ResponseWriter, status int, class, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem{Class: class, Message: message})
}

// writeError turns a ledger or domain error into a response.
//
// A classified error is a refusal the server authored, so its message is
// returned as written. An unclassified error is an infrastructure failure whose
// text may name a table, a constraint or a connection string, so the client
// gets a generic message and the detail goes to the log.
func writeError(w http.ResponseWriter, err error) {
	var classified *errclass.Error
	if errors.As(err, &classified) {
		if status, ok := statusForClass[classified.Class]; ok {
			// The reason, not err.Error(): the wrapper repeats the class, and a
			// message that says "cas_conflict: cas_conflict: ..." is a message
			// nobody reads twice.
			writeProblem(w, status, string(classified.Class), classified.Reason)
			return
		}
	}
	slog.Error("memgw gateway", "error", err)
	writeProblem(w, http.StatusInternalServerError, "internal", "the gateway could not complete the request")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// decode reads a JSON body under the size cap, refusing unknown fields.
//
// Refusing unknown fields is what makes a typo in a client an error rather than
// a silently ignored setting. A submission that spelled expected_revision
// wrongly would otherwise be accepted as a submission that named no revision at
// all -- which is a different, weaker request than the one the client made.
func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProblem(w, http.StatusRequestEntityTooLarge, string(errclass.PayloadTooLarge),
				"the request body is larger than the gateway accepts")
			return false
		}
		writeProblem(w, http.StatusBadRequest, "malformed", "the request body is not the JSON this route accepts")
		return false
	}
	// One JSON value per request. A body with a second document appended is a
	// smuggling shape, not a request.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		writeProblem(w, http.StatusBadRequest, "malformed", "the request body must be a single JSON object")
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Submitting an event
// ---------------------------------------------------------------------------

// submission is the wire shape of a write.
//
// It has no principal_id field, and that absence is the point. The identity a
// write is attributed to is the one the credential proved, never one the body
// asserted. A field here would be a field somebody eventually trusts.
type submission struct {
	EventID          uuid.UUID   `json:"event_id"`
	IdempotencyKey   string      `json:"idempotency_key"`
	ProjectID        uuid.UUID   `json:"project_id"`
	WorkspaceID      uuid.UUID   `json:"workspace_id"`
	WorkID           *uuid.UUID  `json:"work_id,omitempty"`
	SessionID        string      `json:"session_id"`
	BranchID         uuid.UUID   `json:"branch_id"`
	Type             string      `json:"type"`
	Payload          any         `json:"payload"`
	ExpectedRevision *int64      `json:"expected_revision,omitempty"`
	EvidenceRefs     []uuid.UUID `json:"evidence_refs,omitempty"`
	SensitivityClass string      `json:"sensitivity_class"`
	PolicyVersion    string      `json:"policy_version"`
	SchemaVersion    string      `json:"schema_version"`
	OccurredAt       time.Time   `json:"occurred_at"`
	WriterEpoch      int64       `json:"writer_epoch"`
}

func (g *Gateway) submitEvent(w http.ResponseWriter, r *http.Request) {
	who, ok := caller(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "a memgw credential is required")
		return
	}
	var in submission
	if !decode(w, r, &in) {
		return
	}

	ctx, cancel := contextWithTimeout(r, submitTimeout)
	defer cancel()

	receipt, err := g.sub.Submit(ctx, ledger.Event{
		EventID:          in.EventID,
		IdempotencyKey:   in.IdempotencyKey,
		PrincipalID:      who.ID,
		ProjectID:        in.ProjectID,
		WorkspaceID:      in.WorkspaceID,
		WorkID:           in.WorkID,
		SessionID:        in.SessionID,
		BranchID:         in.BranchID,
		Type:             in.Type,
		Payload:          in.Payload,
		ExpectedRevision: in.ExpectedRevision,
		EvidenceRefs:     in.EvidenceRefs,
		SensitivityClass: in.SensitivityClass,
		PolicyVersion:    in.PolicyVersion,
		SchemaVersion:    in.SchemaVersion,
		OccurredAt:       in.OccurredAt,
		WriterEpoch:      in.WriterEpoch,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	// A duplicate is 200, not 201: nothing was created by this request. The
	// receipt is identical to the one the first attempt got, which is what makes
	// a retry safe to issue blindly.
	status := http.StatusCreated
	if receipt.State == ledger.StateDuplicate {
		status = http.StatusOK
	}
	writeJSON(w, status, receipt)
}

// getReceipt is how a client in unknown_commit_status finds out what happened
// without inventing a second idempotency key. A principal sees only its own
// receipts, and a receipt that does not exist is 404 rather than an empty
// success -- "no receipt" and "a receipt saying nothing" are different answers.
func (g *Gateway) getReceipt(w http.ResponseWriter, r *http.Request) {
	who, ok := caller(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "a memgw credential is required")
		return
	}
	key := chi.URLParam(r, "idempotencyKey")
	if key == "" {
		writeProblem(w, http.StatusBadRequest, "malformed", "an idempotency key is required")
		return
	}

	ctx, cancel := contextWithTimeout(r, readTimeout)
	defer cancel()

	receipt, found, err := g.sub.LookupReceipt(ctx, who.ID, key)
	if err != nil {
		writeError(w, err)
		return
	}
	if !found {
		writeProblem(w, http.StatusNotFound, "not_found", "no receipt for that idempotency key")
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

type projectRequest struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	RepoRemote  string `json:"repo_remote,omitempty"`
}

type projectResponse struct {
	ProjectID   uuid.UUID `json:"project_id"`
	Slug        string    `json:"slug"`
	DisplayName string    `json:"display_name"`
	Status      string    `json:"status"`
	RepoKey     string    `json:"repo_key,omitempty"`
}

// ensureProject registers a project, or returns the one that already has that
// slug. It is idempotent because a host that starts twice must not create a
// second project for the same checkout.
//
// Registering is not the same as being allowed to write to it: the grant is a
// separate, deliberate act. This route hands out no authority.
func (g *Gateway) ensureProject(w http.ResponseWriter, r *http.Request) {
	if _, ok := caller(r.Context()); !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "a memgw credential is required")
		return
	}
	var in projectRequest
	if !decode(w, r, &in) {
		return
	}

	ctx, cancel := contextWithTimeout(r, readTimeout)
	defer cancel()

	p, err := g.reg.EnsureProject(ctx, domain.Project{Slug: in.Slug, DisplayName: in.DisplayName})
	if err != nil {
		writeError(w, err)
		return
	}
	out := projectResponse{ProjectID: p.ID, Slug: p.Slug, DisplayName: p.DisplayName, Status: p.Status}

	if in.RepoRemote != "" {
		// The remote is normalised before it is stored, which is what strips
		// any credential a git remote carries in its userinfo. The raw remote
		// is never written anywhere.
		key, err := domain.NormaliseRepoKey(in.RepoRemote)
		if err != nil {
			writeError(w, err)
			return
		}
		if err := g.reg.MapRepo(ctx, key, p.ID); err != nil {
			writeError(w, err)
			return
		}
		out.RepoKey = key
	}
	writeJSON(w, http.StatusOK, out)
}

type workspaceRequest struct {
	ProjectID uuid.UUID `json:"project_id"`
	HostID    uuid.UUID `json:"host_id"`
	RootPath  string    `json:"root_path"`
	Label     string    `json:"label,omitempty"`
}

type workspaceResponse struct {
	WorkspaceID    uuid.UUID `json:"workspace_id"`
	ProjectID      uuid.UUID `json:"project_id"`
	HostID         uuid.UUID `json:"host_id"`
	RootPathDigest string    `json:"root_path_digest"`
	Label          string    `json:"label"`
}

// ensureWorkspace registers one checkout on one host.
//
// The client sends the root path and the server stores only its digest. A path
// contains a user name, and the registry is read by every host that shares the
// project; the digest identifies the checkout without telling the other five
// hosts who is logged in on this one.
func (g *Gateway) ensureWorkspace(w http.ResponseWriter, r *http.Request) {
	if _, ok := caller(r.Context()); !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "a memgw credential is required")
		return
	}
	var in workspaceRequest
	if !decode(w, r, &in) {
		return
	}
	if in.RootPath == "" {
		writeProblem(w, http.StatusBadRequest, "malformed", "a workspace must name its root path")
		return
	}

	ctx, cancel := contextWithTimeout(r, readTimeout)
	defer cancel()

	ws, err := g.reg.EnsureWorkspace(ctx, domain.Workspace{
		ProjectID:      in.ProjectID,
		HostID:         in.HostID,
		RootPathDigest: domain.RootPathDigest(in.RootPath),
		Label:          in.Label,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workspaceResponse{
		WorkspaceID:    ws.ID,
		ProjectID:      ws.ProjectID,
		HostID:         ws.HostID,
		RootPathDigest: ws.RootPathDigest,
		Label:          ws.Label,
	})
}

type workEnsureRequest struct {
	ProjectID   uuid.UUID `json:"project_id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	Title       string    `json:"title,omitempty"`
}

type workEnsureResponse struct {
	WorkID      uuid.UUID `json:"work_id"`
	ProjectID   uuid.UUID `json:"project_id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	Title       string    `json:"title"`
	State       string    `json:"state"`
	Revision    int64     `json:"revision"`
	Created     bool      `json:"created"`
}

// ensureWork returns the work that is currently open for a project and
// workspace, or creates one when none is open. It is the "current work"
// primitive the adapters use so that an operator never has to edit a work id
// into a mapping by hand: a host opens a repo, the adapter resolves the repo to
// its project and workspace, and this route names the work the session belongs
// to -- the one already under way, or a fresh one when the previous work is
// completed or abandoned.
//
// Creation goes through the ledger's event path, never a direct insert: the
// work.row and its work.planned event are one commit, so the store's invariant
// "a work can never be in a state no event explains" holds here exactly as it
// holds for every other write. A concurrent caller racing to create the same
// work is settled by the idempotency of the work.planned event; the loser reads
// back the work the winner created.
//
// This route hands out no new authority: creating work requires the same
// checkpoint_write role as writing a checkpoint, and the scope the credential
// already holds is the scope this route enforces. A caller that cannot write a
// checkpoint in this project cannot conjure work in it either.
func (g *Gateway) ensureWork(w http.ResponseWriter, r *http.Request) {
	who, ok := caller(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "a memgw credential is required")
		return
	}
	var in workEnsureRequest
	if !decode(w, r, &in) {
		return
	}
	if in.ProjectID == uuid.Nil || in.WorkspaceID == uuid.Nil {
		writeProblem(w, http.StatusBadRequest, "malformed", "a work must name a project and a workspace")
		return
	}

	ctx, cancel := contextWithTimeout(r, submitTimeout)
	defer cancel()

	// Authorise before touching state, so a probe cannot learn what work exists.
	if err := g.auth.Authorize(ctx, who.ID, principal.Scope{
		ProjectID:   in.ProjectID,
		WorkspaceID: &in.WorkspaceID,
	}, principal.RoleCheckpointWrite); err != nil {
		if errors.Is(err, principal.ErrScopeDenied) {
			writeProblem(w, http.StatusForbidden, string(errclass.ScopeDenied),
				"principal may not create or select work in the requested scope")
			return
		}
		writeError(w, err)
		return
	}

	cur, err := g.reg.CurrentWork(ctx, in.ProjectID, in.WorkspaceID)
	if err == nil {
		writeJSON(w, http.StatusOK, workEnsureResponse{
			WorkID: cur.ID, ProjectID: cur.ProjectID, WorkspaceID: cur.WorkspaceID,
			Title: cur.Title, State: cur.State, Revision: cur.Revision, Created: false,
		})
		return
	}
	if !errors.Is(err, domain.ErrNotFound) {
		writeError(w, err)
		return
	}

	// No open work: create one. The work id is random (a new unit of work is a
	// new identity), and the session and branch are synthesised from the work id
	// so the event has somewhere to live without borrowing a host's session.
	workID := uuid.New()
	sessionID := "work.ensure:" + workID.String()
	branchID := uuid.NewSHA1(uuid.Nil, []byte("work.ensure\x00"+workID.String()))
	key := "work.ensure." + workID.String()
	eventID := uuid.NewSHA1(workEnsureNamespace, []byte(key))

	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = "work"
	}

	receipt, err := g.sub.Submit(ctx, ledger.Event{
		EventID:          eventID,
		IdempotencyKey:   key,
		PrincipalID:      who.ID,
		ProjectID:        in.ProjectID,
		WorkspaceID:      in.WorkspaceID,
		WorkID:           &workID,
		SessionID:        sessionID,
		BranchID:         branchID,
		Type:             "work.planned",
		Payload:          domain.WorkPayload{Title: title, ReasonClass: "lifecycle"},
		SensitivityClass: "internal",
		PolicyVersion:    ledger.ContractPolicyVersion,
		SchemaVersion:    ledger.ContractSchemaVersion,
		OccurredAt:       time.Now().UTC(),
		WriterEpoch:      who.WriterEpoch,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	_ = receipt

	created, err := g.reg.Work(ctx, workID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, workEnsureResponse{
		WorkID: created.ID, ProjectID: created.ProjectID, WorkspaceID: created.WorkspaceID,
		Title: created.Title, State: created.State, Revision: created.Revision, Created: true,
	})
}

// projectByRepo resolves a git remote to the project it was mapped to, which is
// how a host that only knows its checkout finds the shared project id.
func (g *Gateway) projectByRepo(w http.ResponseWriter, r *http.Request) {
	if _, ok := caller(r.Context()); !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "a memgw credential is required")
		return
	}
	remote := r.URL.Query().Get("remote")
	if remote == "" {
		writeProblem(w, http.StatusBadRequest, "malformed", "a remote is required")
		return
	}
	key, err := domain.NormaliseRepoKey(remote)
	if err != nil {
		writeError(w, err)
		return
	}

	ctx, cancel := contextWithTimeout(r, readTimeout)
	defer cancel()

	p, err := g.reg.ProjectByRepo(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not_found", "no project is mapped to that repository")
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectResponse{
		ProjectID: p.ID, Slug: p.Slug, DisplayName: p.DisplayName, Status: p.Status, RepoKey: key,
	})
}

// contextWithTimeout bounds one operation.
//
// It derives from the request context, so a client that hangs up still cancels
// the work it started; the timeout only adds an upper bound the client cannot
// extend.
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}
