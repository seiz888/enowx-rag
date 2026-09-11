package principal

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Authentication: proving a caller is the principal it claims to be.
//
// Phase 3 left this as an interface on purpose and said so. What filled the gap
// in the meantime was the shared RAG_ADMIN_TOKEN, which is not a principal: it
// proves no scope, every tool on the machine holds a copy, and it is treated as
// compromised. A gateway authenticated by it would have per-principal grants on
// paper and one shared key in practice.
//
// A credential here is two halves. The key id is public, unguessable and safe
// to log; the secret is shown once at issue time and never stored -- what is
// stored is a PBKDF2-HMAC-SHA256 verifier over a per-credential salt. Stealing
// this table does not let anybody write.

const (
	// credentialPrefix makes a leaked secret recognisable to a scanner. A
	// string that looks like nothing in particular is a string nobody notices
	// in a log.
	credentialPrefix = "memgw_"

	// pbkdf2Iterations is the work factor. It is stored per row so it can be
	// raised later without invalidating credentials already issued.
	pbkdf2Iterations = 210000

	saltBytes     = 16
	secretBytes   = 32
	verifierBytes = 32
	keyIDBytes    = 18
)

// ErrBadCredential is returned for every authentication failure: unknown key,
// wrong secret, revoked, expired, or a principal that is no longer active.
//
// One error for all of them is deliberate. A caller learning *which* of those
// it was would learn whether a key id exists, and a key id that can be probed
// for existence is a key id that can be enumerated.
var ErrBadCredential = errors.New("memgw: credential is not valid")

// Credential is the stored half. The secret is not a field here because it is
// not stored anywhere.
type Credential struct {
	ID          uuid.UUID
	PrincipalID uuid.UUID
	KeyID       string
	Label       string
	IssuedAt    time.Time
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
}

// Issue creates a credential and returns it together with the secret token,
// which is the only time the token exists outside the caller's hands.
//
// The token is returned rather than written anywhere: no log line, no file, no
// event payload. A function that persisted it "for convenience" would undo the
// reason the verifier is hashed.
func (s *Store) Issue(ctx context.Context, principalID uuid.UUID, label string, expiresAt *time.Time) (Credential, string, error) {
	p, err := s.Get(ctx, principalID)
	if err != nil {
		return Credential{}, "", err
	}
	if p.Status != StatusActive {
		return Credential{}, "", fmt.Errorf("%w: principal is %s", ErrScopeDenied, p.Status)
	}

	keyID, err := randomToken(keyIDBytes)
	if err != nil {
		return Credential{}, "", err
	}
	secret, err := randomToken(secretBytes)
	if err != nil {
		return Credential{}, "", err
	}
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return Credential{}, "", fmt.Errorf("memgw: read entropy: %w", err)
	}
	verifier, err := pbkdf2.Key(sha256.New, secret, salt, pbkdf2Iterations, verifierBytes)
	if err != nil {
		return Credential{}, "", fmt.Errorf("memgw: derive verifier: %w", err)
	}

	c := Credential{
		ID:          uuid.New(),
		PrincipalID: principalID,
		KeyID:       keyID,
		Label:       label,
		ExpiresAt:   expiresAt,
	}
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()
	if err := s.pool.Pgx().QueryRow(ctx, `
		INSERT INTO principal_credentials (credential_id, principal_id, key_id, iterations, salt, verifier, label, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING issued_at`,
		c.ID, c.PrincipalID, c.KeyID, pbkdf2Iterations, salt, verifier, c.Label, c.ExpiresAt,
	).Scan(&c.IssuedAt); err != nil {
		return Credential{}, "", fmt.Errorf("memgw: issue credential: %w", err)
	}
	return c, credentialPrefix + keyID + "." + secret, nil
}

// Authenticate resolves a presented token to a principal.
//
// It derives the verifier every time rather than caching: a cache keyed by the
// token would be a place the secret lives, and the whole arrangement exists so
// that no such place is created.
func (s *Store) Authenticate(ctx context.Context, token string) (Principal, error) {
	keyID, secret, ok := splitToken(token)
	if !ok {
		return Principal{}, ErrBadCredential
	}

	queryCtx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()
	var (
		credentialID uuid.UUID
		principalID  uuid.UUID
		iterations   int
		salt         []byte
		verifier     []byte
		expiresAt    *time.Time
		revokedAt    *time.Time
	)
	err := s.pool.Pgx().QueryRow(queryCtx, `
		SELECT credential_id, principal_id, iterations, salt, verifier, expires_at, revoked_at
		FROM principal_credentials WHERE key_id = $1`, keyID,
	).Scan(&credentialID, &principalID, &iterations, &salt, &verifier, &expiresAt, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrBadCredential
	}
	if err != nil {
		return Principal{}, fmt.Errorf("memgw: read credential: %w", err)
	}

	// The derivation runs before the liveness checks so that a revoked or
	// expired credential costs the same as a live one to reject. Answering
	// "revoked" instantly would tell a holder of an old secret that the key id
	// is real.
	got, err := pbkdf2.Key(sha256.New, secret, salt, iterations, len(verifier))
	if err != nil {
		return Principal{}, fmt.Errorf("memgw: derive verifier: %w", err)
	}
	matches := hmac.Equal(got, verifier)

	now := s.now()
	switch {
	case !matches:
		return Principal{}, ErrBadCredential
	case revokedAt != nil && !revokedAt.After(now):
		return Principal{}, ErrBadCredential
	case expiresAt != nil && !expiresAt.After(now):
		return Principal{}, ErrBadCredential
	}

	p, err := s.Get(ctx, principalID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Principal{}, ErrBadCredential
		}
		return Principal{}, err
	}
	// Revoking a principal revokes every way of being it, immediately, without
	// anyone having to remember to revoke its credentials one by one.
	if p.Status != StatusActive {
		return Principal{}, ErrBadCredential
	}

	// Best effort, and to day granularity: an exact last-use timestamp on every
	// request would turn this table into a write-hot log of who was working
	// when, which is a surveillance record nobody asked for.
	touchCtx, touchCancel := s.pool.WithQueryTimeout(context.WithoutCancel(ctx))
	defer touchCancel()
	_, _ = s.pool.Pgx().Exec(touchCtx,
		`UPDATE principal_credentials SET last_used_day = CURRENT_DATE
		 WHERE credential_id = $1 AND (last_used_day IS DISTINCT FROM CURRENT_DATE)`, credentialID)

	return p, nil
}

// RevokeCredential revokes one credential without touching the principal, which
// is what a rotation is: a new secret is issued, then the old one is revoked.
func (s *Store) RevokeCredential(ctx context.Context, id uuid.UUID) error {
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()
	tag, err := s.pool.Pgx().Exec(ctx,
		`UPDATE principal_credentials SET revoked_at = now() WHERE credential_id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("memgw: revoke credential: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Credentials lists a principal's credentials, revoked ones included, so an
// operator auditing a rotation can see what was replaced rather than only what
// is live.
func (s *Store) Credentials(ctx context.Context, principalID uuid.UUID) ([]Credential, error) {
	ctx, cancel := s.pool.WithQueryTimeout(ctx)
	defer cancel()
	rows, err := s.pool.Pgx().Query(ctx, `
		SELECT credential_id, principal_id, key_id, label, issued_at, expires_at, revoked_at
		FROM principal_credentials WHERE principal_id = $1 ORDER BY issued_at`, principalID)
	if err != nil {
		return nil, fmt.Errorf("memgw: read credentials: %w", err)
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		var c Credential
		if err := rows.Scan(&c.ID, &c.PrincipalID, &c.KeyID, &c.Label, &c.IssuedAt, &c.ExpiresAt, &c.RevokedAt); err != nil {
			return nil, fmt.Errorf("memgw: read credentials: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// splitToken parses "memgw_<key id>.<secret>". It validates shape only; a
// well-formed token that belongs to nobody is still ErrBadCredential.
//
// The secret is returned in its presented encoding, which is what the verifier
// was derived over. Decoding it first and hashing the bytes would work equally
// well, but only if both halves agreed forever; hashing exactly what was typed
// leaves nothing to disagree about.
func splitToken(token string) (keyID, secret string, ok bool) {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, credentialPrefix) {
		return "", "", false
	}
	rest := token[len(credentialPrefix):]
	id, encoded, found := strings.Cut(rest, ".")
	if !found || len(id) < 16 || len(id) > 64 {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) < 16 {
		return "", "", false
	}
	return id, encoded, true
}

func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("memgw: read entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
