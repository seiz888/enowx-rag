-- 0003_memgw_credentials: how a caller proves it is a principal.
--
-- 0001 created principals and grants but issued no credential, and said so:
-- authorization was implemented, authentication was left as an interface.
-- Without this table the only thing standing between a caller and the ledger is
-- the shared RAG_ADMIN_TOKEN, which is not a principal, proves no scope, is
-- held by every tool on the machine, and is treated as compromised.
--
-- What is stored is a PBKDF2-HMAC-SHA256 verifier, never the secret. A stolen
-- copy of this table does not let anyone write: the salt and the iteration
-- count are per credential and the digest is not reversible. The key_id is the
-- public half and is what a caller presents alongside the secret, so a lookup
-- costs one indexed read rather than a scan that would have to derive a hash
-- per row.

CREATE TABLE principal_credentials (
	credential_id UUID        PRIMARY KEY,
	principal_id  UUID        NOT NULL REFERENCES principals (principal_id),
	-- The public half. Opaque, unguessable, and safe to log: it names the
	-- credential without being able to authenticate on its own.
	key_id        TEXT        NOT NULL UNIQUE CHECK (key_id ~ '^[A-Za-z0-9_-]{16,64}$'),
	algorithm     TEXT        NOT NULL DEFAULT 'pbkdf2-hmac-sha256'
		CHECK (algorithm IN ('pbkdf2-hmac-sha256')),
	-- Stored per credential rather than as a constant, so the cost can be
	-- raised later without invalidating what is already issued.
	iterations    INTEGER     NOT NULL CHECK (iterations >= 100000),
	salt          BYTEA       NOT NULL CHECK (length(salt) >= 16),
	verifier      BYTEA       NOT NULL CHECK (length(verifier) >= 32),
	label         TEXT        NOT NULL DEFAULT '' CHECK (length(label) <= 120),
	issued_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
	issued_by     UUID        REFERENCES principals (principal_id),
	expires_at    TIMESTAMPTZ,
	revoked_at    TIMESTAMPTZ,
	-- Coarse on purpose: an exact last-use timestamp on every request would
	-- make the credential table a write-hot audit log of who was working when.
	last_used_day DATE
);

CREATE INDEX principal_credentials_principal_idx ON principal_credentials (principal_id);
CREATE INDEX principal_credentials_live_idx ON principal_credentials (principal_id)
	WHERE revoked_at IS NULL;

-- A credential row is not append-only: revocation and the last-used day are
-- both updates. Deletion is refused, because a revoked credential that could be
-- deleted would erase the evidence that it ever existed.
CREATE TRIGGER principal_credentials_no_delete
	BEFORE DELETE ON principal_credentials
	FOR EACH ROW EXECUTE FUNCTION memgw_refuse_mutation();

-- ---------------------------------------------------------------------------
-- Read path the bootstrap depends on
-- ---------------------------------------------------------------------------

-- The bootstrap reads a project's live facts newest-first. 0002 indexes
-- (slot_id, status), which finds them but leaves the sort to be done every
-- time; this covers the order as well.
--
-- Only this one is added. The other two reads the bootstrap makes are already
-- covered: checkpoints by checkpoints_work_revision_unique, evidence by
-- evidence_work_idx. An index that duplicates an existing one costs every
-- write and buys no read.
CREATE INDEX facts_slot_live_recorded_idx ON facts (slot_id, recorded_at DESC)
	WHERE status IN ('active', 'conflicted') AND tombstoned_at IS NULL;
