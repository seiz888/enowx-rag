// What a credential has to be worth: the secret is not recoverable from the
// database, a wrong one is refused, and every way of ending a credential --
// revoking it, expiring it, revoking the principal behind it -- actually ends
// it. A store that authenticated the happy path and nothing else would pass a
// suite that only issued and logged in.
package principal_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/principal"
)

func issue(t *testing.T, e *env, p principal.Principal) (principal.Credential, string) {
	t.Helper()
	c, token, err := e.store.Issue(e.ctx, p.ID, "test", nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return c, token
}

// The round trip, and the identity that comes back: authentication returns the
// principal row, which is what the ledger builds its domain event from. A
// caller that could describe its own type or parent would be describing its own
// permissions.
func TestAuthenticateReturnsTheStoredPrincipal(t *testing.T) {
	e := newEnv(t)
	p := e.principal(t, "claude-code")

	c, token := issue(t, e, p)
	got, err := e.store.Authenticate(e.ctx, token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.ID != p.ID || got.Type != p.Type || got.HostID != p.HostID {
		t.Fatalf("authenticated as %+v, issued to %+v", got, p)
	}
	if !strings.HasPrefix(token, "memgw_"+c.KeyID+".") {
		t.Fatal("the token does not carry the key id it was issued under")
	}
}

// The secret is not in the database. This is the whole reason the table stores
// a verifier: reading every column must not be enough to authenticate.
func TestTheSecretIsNotStored(t *testing.T) {
	e := newEnv(t)
	p := e.principal(t, "claude-code")
	c, token := issue(t, e, p)

	secret := token[strings.Index(token, ".")+1:]
	if len(secret) < 40 {
		t.Fatalf("the secret is only %d characters", len(secret))
	}

	rows, err := e.pool.Pgx().Query(e.ctx, `
		SELECT credential_id::text, principal_id::text, key_id, algorithm,
		       iterations::text, encode(salt, 'hex'), encode(verifier, 'hex'),
		       label, coalesce(expires_at::text, ''), coalesce(revoked_at::text, '')
		FROM principal_credentials WHERE credential_id = $1`, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no credential row")
	}
	values, err := rows.Values()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range values {
		s, _ := v.(string)
		if s != "" && strings.Contains(s, secret) {
			t.Fatalf("a stored column carries the secret")
		}
	}
}

// Everything that is not the right secret is one indistinguishable refusal. A
// caller that could tell "no such key" from "wrong secret" could enumerate key
// ids, and an error naming the principal would leak who holds what.
func TestWrongCredentialsAreOneRefusal(t *testing.T) {
	e := newEnv(t)
	p := e.principal(t, "claude-code")
	c, token := issue(t, e, p)
	secret := token[strings.Index(token, ".")+1:]

	cases := map[string]string{
		"empty":            "",
		"no prefix":        c.KeyID + "." + secret,
		"no secret":        "memgw_" + c.KeyID,
		"unknown key":      "memgw_" + strings.Repeat("A", 24) + "." + secret,
		"wrong secret":     "memgw_" + c.KeyID + "." + strings.Repeat("B", 43),
		"truncated secret": token[:len(token)-4],
		"key id only":      "memgw_" + c.KeyID + ".",
		"not base64":       "memgw_" + c.KeyID + ".@@@@@@@@@@@@@@@@@@@@@@",
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := e.store.Authenticate(e.ctx, bad)
			if !errors.Is(err, principal.ErrBadCredential) {
				t.Fatalf("want ErrBadCredential, got %v (principal %v)", err, got.ID)
			}
			if strings.Contains(err.Error(), c.KeyID) || strings.Contains(err.Error(), p.ID.String()) {
				t.Fatalf("the refusal names the credential: %v", err)
			}
		})
	}
}

// Two credentials issued in a row must not share a salt or a key id: a shared
// salt would make one derivation answer for both.
func TestIssuedCredentialsAreDistinct(t *testing.T) {
	e := newEnv(t)
	p := e.principal(t, "claude-code")
	c1, t1 := issue(t, e, p)
	c2, t2 := issue(t, e, p)
	if c1.KeyID == c2.KeyID || t1 == t2 {
		t.Fatal("two issues produced the same credential")
	}
	var same int
	if err := e.pool.Pgx().QueryRow(e.ctx, `
		SELECT count(*) FROM principal_credentials a JOIN principal_credentials b
		  ON a.credential_id < b.credential_id AND a.salt = b.salt`).Scan(&same); err != nil {
		t.Fatal(err)
	}
	if same != 0 {
		t.Fatal("two credentials share a salt")
	}
	// Rotation: the second works while the first is being retired.
	if _, err := e.store.Authenticate(e.ctx, t2); err != nil {
		t.Fatalf("the new credential does not work: %v", err)
	}
	if err := e.store.RevokeCredential(e.ctx, c1.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := e.store.Authenticate(e.ctx, t1); !errors.Is(err, principal.ErrBadCredential) {
		t.Fatalf("a revoked credential still authenticates: %v", err)
	}
	if _, err := e.store.Authenticate(e.ctx, t2); err != nil {
		t.Fatalf("revoking one credential disabled another: %v", err)
	}
}

// Expiry is read from the row at authentication time, not assumed at issue
// time: a process that has been running since before the expiry must not keep
// working because it authenticated once.
func TestExpiredCredentialIsRefused(t *testing.T) {
	e := newEnv(t)
	p := e.principal(t, "claude-code")
	past := time.Now().Add(-time.Minute)
	_, token, err := e.store.Issue(e.ctx, p.ID, "short-lived", &past)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := e.store.Authenticate(e.ctx, token); !errors.Is(err, principal.ErrBadCredential) {
		t.Fatalf("an expired credential authenticated: %v", err)
	}
}

// Revoking the principal revokes every way of being it. Otherwise revocation
// would be a list nobody finishes: each credential would have to be found and
// revoked one by one, and the one that was missed is the one that matters.
func TestRevokingThePrincipalEndsItsCredentials(t *testing.T) {
	e := newEnv(t)
	p := e.principal(t, "claude-code")
	_, tokenA := issue(t, e, p)
	_, tokenB := issue(t, e, p)

	if err := e.store.Revoke(e.ctx, p.ID); err != nil {
		t.Fatalf("revoke principal: %v", err)
	}
	for _, token := range []string{tokenA, tokenB} {
		if _, err := e.store.Authenticate(e.ctx, token); !errors.Is(err, principal.ErrBadCredential) {
			t.Fatalf("a revoked principal still authenticates: %v", err)
		}
	}
	// And no new credential can be issued for it either.
	if _, _, err := e.store.Issue(e.ctx, p.ID, "after", nil); !errors.Is(err, principal.ErrScopeDenied) {
		t.Fatalf("issuing to a revoked principal: want ErrScopeDenied, got %v", err)
	}
}

// The credential row is evidence. Revocation is an update; deletion would erase
// the fact that the credential ever existed, which is exactly what an incident
// review needs to see.
func TestCredentialRowsCannotBeDeleted(t *testing.T) {
	e := newEnv(t)
	p := e.principal(t, "claude-code")
	c, _ := issue(t, e, p)
	if _, err := e.pool.Pgx().Exec(e.ctx, `DELETE FROM principal_credentials`); err == nil {
		t.Fatal("a credential row was deleted")
	}
	if err := e.store.RevokeCredential(e.ctx, c.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	list, err := e.store.Credentials(e.ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].RevokedAt == nil {
		t.Fatalf("the audit list does not show the revocation: %+v", list)
	}
	// Revoking twice must not silently move the revocation time forward.
	if err := e.store.RevokeCredential(e.ctx, c.ID); !errors.Is(err, principal.ErrNotFound) {
		t.Fatalf("re-revoking: got %v", err)
	}
	if err := e.store.RevokeCredential(e.ctx, uuid.New()); !errors.Is(err, principal.ErrNotFound) {
		t.Fatalf("revoking nothing: got %v", err)
	}
}

// Authentication is not authorization. It answers "who", and the grant check
// still answers "may they" -- so a principal with a perfectly valid credential
// and no grant reaches nothing.
func TestAValidCredentialGrantsNoScope(t *testing.T) {
	e := newEnv(t)
	p := e.principal(t, "claude-code")
	_, token := issue(t, e, p)

	who, err := e.store.Authenticate(e.ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	err = e.store.Authorize(e.ctx, who.ID, principal.Scope{ProjectID: e.projectA}, principal.RoleCheckpointWrite)
	if !errors.Is(err, principal.ErrScopeDenied) {
		t.Fatalf("a credential without a grant reached a project: %v", err)
	}
}
