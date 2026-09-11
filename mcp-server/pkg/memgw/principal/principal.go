// Package principal is the identity and authorization layer of the memory
// gateway: who is writing, and what they are allowed to touch.
//
// The service it sits beside today has neither. pkg/httpapi's
// AdminTokenMiddleware compares one shared token and authorises everything that
// passes, so there is no identity to attribute a write to and nothing to
// revoke. That token is also treated as compromised. Nothing here reads it,
// issues it, or accepts it as authority: see LegacyBearerNotice.
//
// Authentication -- proving a caller is a principal -- is deliberately left as
// an interface. Phase 3 implements authorization: given a principal and a
// requested scope, decide. Issuing credentials is a production operation and is
// out of scope for this phase.
package principal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
)

// Role is a capability grant. Roles compose and none implies another: in
// particular checkpoint_write does not imply fact_promote, and admin is not a
// convenience superset.
type Role string

const (
	RoleHistoryRead      Role = "history_read"
	RoleWorkRead         Role = "work_read"
	RoleCheckpointWrite  Role = "checkpoint_write"
	RoleCandidateWrite   Role = "candidate_write"
	RoleFactPromote      Role = "fact_promote"
	RoleProjectionWorker Role = "projection_worker"
	RoleAdmin            Role = "admin"
)

// Type classifies a principal.
type Type string

const (
	TypeAgent            Type = "agent"
	TypeSubagent         Type = "subagent"
	TypeService          Type = "service"
	TypeHuman            Type = "human"
	TypeProjectionWorker Type = "projection_worker"
)

// Status is the lifecycle of a principal.
type Status string

const (
	StatusActive    Status = "active"
	StatusSuspended Status = "suspended"
	StatusRevoked   Status = "revoked"
)

// LegacyBearerNotice is what a compatibility adapter must log if the old shared
// bearer is ever used to reach a ledger route in a non-production environment.
// It is a string constant rather than a code path because Phase 3 has no such
// adapter: a bootstrap that quietly worked would become the thing everyone
// relies on.
const LegacyBearerNotice = "memgw: the shared bearer token is a legacy transport credential; " +
	"it is not a principal and proves no scope. It must never be presented as evidence of isolation."

// Errors returned by this package. They map to the frozen error classes:
// ErrScopeDenied is scope_denied, and nothing here ever returns an error that
// contains a payload or a credential.
var (
	ErrScopeDenied = errors.New("memgw: scope_denied")
	ErrNotFound    = errors.New("memgw: principal not found")
)

// Principal is an authenticated identity.
type Principal struct {
	ID          uuid.UUID
	Type        Type
	HostID      uuid.UUID
	AgentID     string
	ParentID    *uuid.UUID
	DisplayName string
	Status      Status
	WriterEpoch int64
	CreatedAt   time.Time
	RevokedAt   *time.Time
}

// Grant is one scope grant. A nil narrowing field means "any within the
// enclosing scope"; it never means "any project".
type Grant struct {
	ID          uuid.UUID
	PrincipalID uuid.UUID
	Role        Role
	ProjectID   uuid.UUID
	WorkspaceID *uuid.UUID
	WorkID      *uuid.UUID
	SessionID   *string
	BranchID    *uuid.UUID
	IssuedAt    time.Time
	IssuedBy    *uuid.UUID
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
	Provenance  string
}

// Active reports whether the grant is usable at the given instant.
func (g Grant) Active(at time.Time) bool {
	if g.RevokedAt != nil && !g.RevokedAt.After(at) {
		return false
	}
	if g.ExpiresAt != nil && !g.ExpiresAt.After(at) {
		return false
	}
	return true
}

// Scope is a request's declared target. Unset narrowing fields mean the request
// did not narrow; they are never treated as a wildcard that widens.
type Scope struct {
	ProjectID   uuid.UUID
	WorkspaceID *uuid.UUID
	WorkID      *uuid.UUID
	SessionID   *string
	BranchID    *uuid.UUID
}

// Covers reports whether the grant admits the requested scope. This is the
// intersection rule from the frozen contract, and it only ever narrows:
//
//   - a grant pinned to a workspace does not admit a request for another
//     workspace, nor a request that names no workspace at all;
//   - a grant that names no workspace admits any workspace within its project.
func (g Grant) Covers(s Scope) bool {
	if g.ProjectID != s.ProjectID {
		return false
	}
	if !coversUUID(g.WorkspaceID, s.WorkspaceID) {
		return false
	}
	if !coversUUID(g.WorkID, s.WorkID) {
		return false
	}
	if !coversString(g.SessionID, s.SessionID) {
		return false
	}
	if !coversUUID(g.BranchID, s.BranchID) {
		return false
	}
	return true
}

// coversUUID implements the narrowing rule for one dimension. A grant that
// pins a value requires the request to name that same value: a request that
// leaves the dimension unset is asking for all of them, which is wider than
// the grant.
func coversUUID(grant, requested *uuid.UUID) bool {
	if grant == nil {
		return true
	}
	if requested == nil {
		return false
	}
	return *grant == *requested
}

func coversString(grant, requested *string) bool {
	if grant == nil {
		return true
	}
	if requested == nil {
		return false
	}
	return *grant == *requested
}

// Store reads principals and grants.
type Store struct {
	pool *pgstore.Pool
	now  func() time.Time
}

// NewStore returns a store over the given pool.
func NewStore(pool *pgstore.Pool) *Store {
	return &Store{pool: pool, now: time.Now}
}

// WithClock replaces the clock. Tests use it for expiry; nothing in the ledger
// derives ordering from it.
func (s *Store) WithClock(now func() time.Time) *Store {
	return &Store{pool: s.pool, now: now}
}

// CreatePrincipal inserts a principal. It creates no credential: a principal is
// an identity row, and how a caller proves it is that identity is a separate,
// production concern this phase does not touch.
func (s *Store) CreatePrincipal(ctx context.Context, p Principal) (Principal, error) {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	if p.Status == "" {
		p.Status = StatusActive
	}
	if p.WriterEpoch == 0 {
		p.WriterEpoch = 1
	}
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()
	err := s.pool.Pgx().QueryRow(ctx, `
		INSERT INTO principals (principal_id, principal_type, host_id, agent_id, parent_principal_id,
		                        display_name, status, writer_epoch)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING created_at`,
		p.ID, p.Type, p.HostID, p.AgentID, p.ParentID, p.DisplayName, p.Status, p.WriterEpoch,
	).Scan(&p.CreatedAt)
	if err != nil {
		return Principal{}, fmt.Errorf("memgw: create principal: %w", err)
	}
	return p, nil
}

// Get loads a principal by id.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (Principal, error) {
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()
	var p Principal
	err := s.pool.Pgx().QueryRow(ctx, `
		SELECT principal_id, principal_type, host_id, agent_id, parent_principal_id,
		       display_name, status, writer_epoch, created_at, revoked_at
		FROM principals WHERE principal_id = $1`, id,
	).Scan(&p.ID, &p.Type, &p.HostID, &p.AgentID, &p.ParentID, &p.DisplayName,
		&p.Status, &p.WriterEpoch, &p.CreatedAt, &p.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrNotFound
	}
	if err != nil {
		return Principal{}, fmt.Errorf("memgw: get principal: %w", err)
	}
	return p, nil
}

// Revoke marks a principal revoked. Revocation is immediate for authorization:
// Authorize refuses a revoked principal regardless of its grants.
func (s *Store) Revoke(ctx context.Context, id uuid.UUID) error {
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()
	tag, err := s.pool.Pgx().Exec(ctx,
		`UPDATE principals SET status = 'revoked', revoked_at = now() WHERE principal_id = $1`, id)
	if err != nil {
		return fmt.Errorf("memgw: revoke principal: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GrantScope issues a grant.
func (s *Store) GrantScope(ctx context.Context, g Grant) (Grant, error) {
	if g.ID == uuid.Nil {
		g.ID = uuid.New()
	}
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()
	err := s.pool.Pgx().QueryRow(ctx, `
		INSERT INTO grants (grant_id, principal_id, role, project_id, workspace_id, work_id,
		                    session_id, branch_id, issued_by_principal_id, expires_at, provenance_note)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING issued_at`,
		g.ID, g.PrincipalID, g.Role, g.ProjectID, g.WorkspaceID, g.WorkID,
		g.SessionID, g.BranchID, g.IssuedBy, g.ExpiresAt, g.Provenance,
	).Scan(&g.IssuedAt)
	if err != nil {
		return Grant{}, fmt.Errorf("memgw: grant scope: %w", err)
	}
	return g, nil
}

// RevokeGrant revokes one grant by id.
func (s *Store) RevokeGrant(ctx context.Context, id uuid.UUID) error {
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()
	tag, err := s.pool.Pgx().Exec(ctx,
		`UPDATE grants SET revoked_at = now() WHERE grant_id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("memgw: revoke grant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Grants returns every grant held by a principal, revoked ones included, so the
// caller can reason about why something was refused.
func (s *Store) Grants(ctx context.Context, principalID uuid.UUID) ([]Grant, error) {
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()
	rows, err := s.pool.Pgx().Query(ctx, `
		SELECT grant_id, principal_id, role, project_id, workspace_id, work_id, session_id,
		       branch_id, issued_at, issued_by_principal_id, expires_at, revoked_at, provenance_note
		FROM grants WHERE principal_id = $1 ORDER BY issued_at`, principalID)
	if err != nil {
		return nil, fmt.Errorf("memgw: read grants: %w", err)
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.ID, &g.PrincipalID, &g.Role, &g.ProjectID, &g.WorkspaceID, &g.WorkID,
			&g.SessionID, &g.BranchID, &g.IssuedAt, &g.IssuedBy, &g.ExpiresAt, &g.RevokedAt, &g.Provenance); err != nil {
			return nil, fmt.Errorf("memgw: read grants: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Authorize decides whether a principal may act in a scope with a role.
//
// The refusal is deliberately uninformative about what else exists: it names
// the role and says denied, never "that project belongs to someone else". A
// principal must not be able to map the system by probing it.
func (s *Store) Authorize(ctx context.Context, principalID uuid.UUID, scope Scope, role Role) error {
	p, err := s.Get(ctx, principalID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: unknown principal", ErrScopeDenied)
		}
		return err
	}
	if p.Status != StatusActive {
		return fmt.Errorf("%w: principal is %s", ErrScopeDenied, p.Status)
	}
	grants, err := s.Grants(ctx, principalID)
	if err != nil {
		return err
	}
	now := s.now()
	for _, g := range grants {
		if g.Role != role || !g.Active(now) {
			continue
		}
		if g.Covers(scope) {
			return nil
		}
	}
	return fmt.Errorf("%w: no active grant for role %s in the requested scope", ErrScopeDenied, role)
}

// AuthorizeAny decides whether a principal may act in a scope holding any one
// of the given roles, and returns the principal it authorised.
//
// The principal comes back from here rather than being read again by the
// caller, because the caller must not be able to act on an identity it composed
// itself: what a submission claims about its own type, parent or host is not
// evidence, and only the row that authorised the write is.
func (s *Store) AuthorizeAny(ctx context.Context, principalID uuid.UUID, scope Scope, roles []Role) (Principal, error) {
	p, err := s.Get(ctx, principalID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Principal{}, fmt.Errorf("%w: unknown principal", ErrScopeDenied)
		}
		return Principal{}, err
	}
	if p.Status != StatusActive {
		return Principal{}, fmt.Errorf("%w: principal is %s", ErrScopeDenied, p.Status)
	}
	grants, err := s.Grants(ctx, principalID)
	if err != nil {
		return Principal{}, err
	}
	now := s.now()
	for _, g := range grants {
		if !g.Active(now) || !g.Covers(scope) {
			continue
		}
		for _, want := range roles {
			if g.Role == want {
				return p, nil
			}
		}
	}
	return Principal{}, fmt.Errorf("%w: no active grant in the requested scope", ErrScopeDenied)
}
