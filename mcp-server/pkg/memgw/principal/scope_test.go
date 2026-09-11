// The six negative tests the phase mandates live here. They are written as
// refusals on purpose: a permission model is only worth what its denials are
// worth, and a suite that only proves the allowed path works would pass just as
// happily against a store that allowed everything.
package principal_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/memgwtest"
	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
	"github.com/enowdev/enowx-rag/pkg/memgw/principal"
)

type env struct {
	store    *principal.Store
	pool     *pgstore.Pool
	ctx      context.Context
	projectA uuid.UUID
	projectB uuid.UUID
	wsA      uuid.UUID
	wsB      uuid.UUID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := memgwtest.Pool(t)
	e := &env{
		store:    principal.NewStore(pool),
		pool:     pool,
		ctx:      t.Context(),
		projectA: uuid.New(),
		projectB: uuid.New(),
		wsA:      uuid.New(),
		wsB:      uuid.New(),
	}
	// A grant names a project by foreign key: a role over a project nobody
	// registered would be a permission over nothing.
	memgwtest.SeedWorkspace(t, pool, e.projectA, e.wsA)
	memgwtest.SeedWorkspace(t, pool, e.projectB, e.wsB)
	return e
}

func (e *env) principal(t *testing.T, agentID string) principal.Principal {
	t.Helper()
	p, err := e.store.CreatePrincipal(e.ctx, principal.Principal{
		Type:        principal.TypeAgent,
		HostID:      uuid.New(),
		AgentID:     agentID,
		DisplayName: agentID,
	})
	if err != nil {
		t.Fatalf("create principal: %v", err)
	}
	return p
}

func (e *env) grant(t *testing.T, g principal.Grant) principal.Grant {
	t.Helper()
	out, err := e.store.GrantScope(e.ctx, g)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	return out
}

func denied(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, principal.ErrScopeDenied) {
		t.Fatalf("want ErrScopeDenied, got %v", err)
	}
}

// 1. A principal scoped to project A cannot read project B.
func TestPrincipalCannotReachAnotherProject(t *testing.T) {
	e := newEnv(t)
	p := e.principal(t, "droid")
	e.grant(t, principal.Grant{PrincipalID: p.ID, Role: principal.RoleHistoryRead, ProjectID: e.projectA})

	if err := e.store.Authorize(e.ctx, p.ID, principal.Scope{ProjectID: e.projectA}, principal.RoleHistoryRead); err != nil {
		t.Fatalf("its own project should be readable: %v", err)
	}
	denied(t, e.store.Authorize(e.ctx, p.ID, principal.Scope{ProjectID: e.projectB}, principal.RoleHistoryRead))
}

// 2. A revoked grant is refused.
func TestRevokedGrantIsRefused(t *testing.T) {
	e := newEnv(t)
	p := e.principal(t, "codex")
	g := e.grant(t, principal.Grant{PrincipalID: p.ID, Role: principal.RoleCheckpointWrite, ProjectID: e.projectA})

	if err := e.store.Authorize(e.ctx, p.ID, principal.Scope{ProjectID: e.projectA}, principal.RoleCheckpointWrite); err != nil {
		t.Fatalf("before revocation: %v", err)
	}
	if err := e.store.RevokeGrant(e.ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	denied(t, e.store.Authorize(e.ctx, p.ID, principal.Scope{ProjectID: e.projectA}, principal.RoleCheckpointWrite))
}

// 3. A client declaring a scope wider than it holds is refused. The grant pins
// a workspace; the request names the project alone, which is every workspace.
func TestWiderDeclaredScopeIsRefused(t *testing.T) {
	e := newEnv(t)
	p := e.principal(t, "opencode")
	e.grant(t, principal.Grant{
		PrincipalID: p.ID, Role: principal.RoleHistoryRead,
		ProjectID: e.projectA, WorkspaceID: &e.wsA,
	})

	if err := e.store.Authorize(e.ctx, p.ID,
		principal.Scope{ProjectID: e.projectA, WorkspaceID: &e.wsA}, principal.RoleHistoryRead); err != nil {
		t.Fatalf("its own workspace should be readable: %v", err)
	}
	denied(t, e.store.Authorize(e.ctx, p.ID, principal.Scope{ProjectID: e.projectA}, principal.RoleHistoryRead))
	denied(t, e.store.Authorize(e.ctx, p.ID,
		principal.Scope{ProjectID: e.projectA, WorkspaceID: &e.wsB}, principal.RoleHistoryRead))
}

// 4. An expired grant is refused. The clock is injected rather than the test
// sleeping, but the comparison under test is the real one.
func TestExpiredGrantIsRefused(t *testing.T) {
	e := newEnv(t)
	p := e.principal(t, "hermes")
	expiry := time.Now().Add(time.Hour)
	e.grant(t, principal.Grant{
		PrincipalID: p.ID, Role: principal.RoleCandidateWrite,
		ProjectID: e.projectA, ExpiresAt: &expiry,
	})

	if err := e.store.Authorize(e.ctx, p.ID, principal.Scope{ProjectID: e.projectA}, principal.RoleCandidateWrite); err != nil {
		t.Fatalf("an unexpired grant should work: %v", err)
	}
	later := e.store.WithClock(func() time.Time { return expiry.Add(time.Minute) })
	denied(t, later.Authorize(e.ctx, p.ID, principal.Scope{ProjectID: e.projectA}, principal.RoleCandidateWrite))
}

// 5. An admin-only operation is refused to a reader. The role is part of the
// grant, so holding the right project is not enough.
func TestAdminOperationRefusedToReader(t *testing.T) {
	e := newEnv(t)
	p := e.principal(t, "reader")
	e.grant(t, principal.Grant{PrincipalID: p.ID, Role: principal.RoleHistoryRead, ProjectID: e.projectA})

	denied(t, e.store.Authorize(e.ctx, p.ID, principal.Scope{ProjectID: e.projectA}, principal.RoleAdmin))
	denied(t, e.store.Authorize(e.ctx, p.ID, principal.Scope{ProjectID: e.projectA}, principal.RoleFactPromote))
}

// 6. No credential and no payload appears in a refusal. The store never sees a
// credential -- it does not store one -- so what this checks is that the
// refusal also does not leak the shape of the system it is protecting.
func TestRefusalCarriesNoSecretOrTopology(t *testing.T) {
	e := newEnv(t)
	p := e.principal(t, "droid")
	e.grant(t, principal.Grant{PrincipalID: p.ID, Role: principal.RoleHistoryRead, ProjectID: e.projectA})

	err := e.store.Authorize(e.ctx, p.ID, principal.Scope{ProjectID: e.projectB}, principal.RoleHistoryRead)
	denied(t, err)
	msg := err.Error()
	for _, forbidden := range []string{
		e.projectB.String(), // do not confirm the id exists
		e.projectA.String(), // do not disclose what the principal does hold
		"Bearer", "token", "password", "postgres://",
	} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("refusal leaked %q: %s", forbidden, msg)
		}
	}
}

// A revoked principal loses everything at once. This is separate from a revoked
// grant: revoking a host that has gone rogue must not require walking its
// grants one at a time.
func TestRevokedPrincipalLosesEveryGrant(t *testing.T) {
	e := newEnv(t)
	p := e.principal(t, "compromised")
	e.grant(t, principal.Grant{PrincipalID: p.ID, Role: principal.RoleHistoryRead, ProjectID: e.projectA})
	e.grant(t, principal.Grant{PrincipalID: p.ID, Role: principal.RoleCheckpointWrite, ProjectID: e.projectA})

	if err := e.store.Revoke(e.ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	denied(t, e.store.Authorize(e.ctx, p.ID, principal.Scope{ProjectID: e.projectA}, principal.RoleHistoryRead))
	denied(t, e.store.Authorize(e.ctx, p.ID, principal.Scope{ProjectID: e.projectA}, principal.RoleCheckpointWrite))
}

// An unknown principal is denied, not reported as missing: the difference is
// how an attacker enumerates valid ids.
func TestUnknownPrincipalIsDenied(t *testing.T) {
	e := newEnv(t)
	denied(t, e.store.Authorize(e.ctx, uuid.New(), principal.Scope{ProjectID: e.projectA}, principal.RoleHistoryRead))
}

// The grant table's uniqueness rule is part of the model: issuing the same
// tuple twice must not silently create two rows that revoke independently.
func TestDuplicateGrantTupleIsRefused(t *testing.T) {
	e := newEnv(t)
	p := e.principal(t, "twice")
	g := principal.Grant{PrincipalID: p.ID, Role: principal.RoleHistoryRead, ProjectID: e.projectA}
	e.grant(t, g)
	if _, err := e.store.GrantScope(e.ctx, principal.Grant{
		PrincipalID: p.ID, Role: principal.RoleHistoryRead, ProjectID: e.projectA,
	}); err == nil {
		t.Fatal("the same grant tuple should not be issuable twice")
	}
}
