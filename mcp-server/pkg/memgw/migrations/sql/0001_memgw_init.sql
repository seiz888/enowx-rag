-- 0001_memgw_init: the Phase 3 durable primitives.
--
-- This migration creates only what the Phase 3 primitives need: identity
-- (principals, grants), fencing (writer_epochs), the append-only event log,
-- receipts, the projection outbox with its watermark and dead letters, and the
-- deletion journal with its legacy chunk mapping.
--
-- Deliberately DEFERRED to a later migration, and named here so the omission is
-- explicit rather than forgotten: projects, workspaces, works, sessions,
-- branches, checkpoints, facts, fact_candidates and provenance_refs. Phase 3
-- does not implement work state, checkpoints or fact promotion, so creating
-- their tables now would be scaffolding that later gets mistaken for a feature.
--
-- Because projects/workspaces/works do not exist yet, the scope columns on
-- events are plain UUIDs with no foreign key. Scope is enforced against grants
-- in code (pkg/memgw/principal) rather than by referential integrity, and the
-- foreign keys arrive with the tables they point at.
--
-- Everything is created in the schema named by the runner's search_path.

-- Writer epochs. Fencing token: a write stamped with an epoch older than the
-- newest fenced epoch is refused, whatever it contains.
CREATE TABLE writer_epochs (
	epoch                 BIGINT      PRIMARY KEY,
	opened_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
	-- No FK: principals references writer_epochs, so a FK here would make the
	-- two tables mutually dependent and unloadable in either order.
	opened_by_principal_id UUID,
	fenced_at             TIMESTAMPTZ,
	reason_class          TEXT,
	drain_watermark_seq   BIGINT,
	CONSTRAINT writer_epochs_fenced_after_open CHECK (fenced_at IS NULL OR fenced_at >= opened_at)
);

-- Epoch 1 exists so the first principal has something to be admitted under.
INSERT INTO writer_epochs (epoch, reason_class) VALUES (1, 'initial_epoch');

CREATE TABLE principals (
	principal_id         UUID        PRIMARY KEY,
	principal_type       TEXT        NOT NULL CHECK (principal_type IN ('agent','subagent','service','human','projection_worker')),
	host_id              UUID        NOT NULL,
	agent_id             TEXT        NOT NULL CHECK (length(agent_id) BETWEEN 1 AND 64),
	parent_principal_id  UUID        REFERENCES principals (principal_id),
	display_name         TEXT        NOT NULL DEFAULT '',
	status               TEXT        NOT NULL DEFAULT 'active' CHECK (status IN ('active','suspended','revoked')),
	writer_epoch         BIGINT      NOT NULL DEFAULT 1 REFERENCES writer_epochs (epoch),
	created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
	revoked_at           TIMESTAMPTZ,
	CONSTRAINT principals_revoked_has_status CHECK (revoked_at IS NULL OR status = 'revoked'),
	-- A subagent must name its parent; that is what makes it a subagent rather
	-- than an unattributed writer.
	CONSTRAINT principals_subagent_has_parent CHECK (principal_type <> 'subagent' OR parent_principal_id IS NOT NULL)
);

CREATE INDEX principals_host_agent_idx ON principals (host_id, agent_id);

-- Grants. NULL in a narrowing column means "any within the enclosing scope".
-- Effective scope is the union of a principal's grants; a request narrows it and
-- can never widen it.
CREATE TABLE grants (
	grant_id               UUID        PRIMARY KEY,
	principal_id           UUID        NOT NULL REFERENCES principals (principal_id),
	role                   TEXT        NOT NULL CHECK (role IN (
		'history_read','work_read','checkpoint_write','candidate_write',
		'fact_promote','projection_worker','admin')),
	project_id             UUID        NOT NULL,
	workspace_id           UUID,
	work_id                UUID,
	session_id             TEXT,
	branch_id              UUID,
	issued_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
	issued_by_principal_id UUID        REFERENCES principals (principal_id),
	expires_at             TIMESTAMPTZ,
	revoked_at             TIMESTAMPTZ,
	provenance_note        TEXT        NOT NULL DEFAULT '',
	CONSTRAINT grants_expiry_after_issue CHECK (expires_at IS NULL OR expires_at > issued_at)
);

-- One row per distinct grant tuple. NULLS NOT DISTINCT so that two grants that
-- differ only by an unset narrowing column collide instead of quietly stacking.
CREATE UNIQUE INDEX grants_unique_tuple_idx
	ON grants (principal_id, role, project_id, workspace_id, work_id, session_id, branch_id)
	NULLS NOT DISTINCT;
CREATE INDEX grants_principal_idx ON grants (principal_id) WHERE revoked_at IS NULL;

-- The event log. Append-only: no UPDATE, no DELETE, enforced by trigger below
-- rather than by convention.
CREATE TABLE events (
	event_id          UUID        PRIMARY KEY,
	seq               BIGINT      GENERATED ALWAYS AS IDENTITY,
	idempotency_key   TEXT        NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 128),
	principal_id      UUID        NOT NULL REFERENCES principals (principal_id),
	project_id        UUID        NOT NULL,
	workspace_id      UUID        NOT NULL,
	work_id           UUID,
	session_id        TEXT        NOT NULL CHECK (length(session_id) BETWEEN 1 AND 128),
	branch_id         UUID        NOT NULL,
	event_type        TEXT        NOT NULL CHECK (event_type IN (
		'work.planned','work.activated','work.blocked','work.unblocked',
		'work.review_requested','work.completed','work.abandoned',
		'checkpoint.recorded','evidence.recorded',
		'fact.candidate_proposed','fact.promoted','fact.superseded',
		'fact.retracted','fact.conflict_flagged',
		'session.started','session.resumed','session.branched',
		'session.compacted','session.ended',
		'tombstone.issued','projection.rebuild_requested',
		'writer_epoch.opened','writer_epoch.fenced')),
	payload           JSONB       NOT NULL,
	payload_digest    TEXT        NOT NULL CHECK (payload_digest ~ '^[0-9a-f]{64}$'),
	expected_revision BIGINT,
	evidence_refs     UUID[]      NOT NULL DEFAULT '{}' CHECK (cardinality(evidence_refs) <= 64),
	sensitivity_class TEXT        NOT NULL CHECK (sensitivity_class IN ('public','internal','confidential','restricted')),
	policy_version    TEXT        NOT NULL,
	schema_version    TEXT        NOT NULL,
	occurred_at       TIMESTAMPTZ NOT NULL,
	recorded_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
	writer_epoch      BIGINT      NOT NULL REFERENCES writer_epochs (epoch),
	aggregate_type    TEXT,
	aggregate_id      UUID,
	revision          BIGINT,
	-- The payload bound from the frozen contract, enforced here as well as in
	-- the ingress path: 256 KiB of canonical JSON.
	CONSTRAINT events_payload_bounded CHECK (pg_column_size(payload) <= 262144),
	CONSTRAINT events_idempotency_unique UNIQUE (principal_id, idempotency_key)
);

CREATE UNIQUE INDEX events_seq_idx ON events (seq);
CREATE INDEX events_project_seq_idx ON events (project_id, seq);
CREATE INDEX events_work_seq_idx ON events (project_id, work_id, seq) WHERE work_id IS NOT NULL;
CREATE INDEX events_branch_seq_idx ON events (branch_id, seq);
-- UNIQUE, not merely indexed: this is what makes compare-and-set work without a
-- lock. Two writers that both computed revision N+1 race to insert it, and the
-- loser gets a uniqueness violation the ledger reports as cas_conflict.
CREATE UNIQUE INDEX events_aggregate_revision_idx ON events (aggregate_type, aggregate_id, revision)
	WHERE aggregate_id IS NOT NULL;

-- Append-only enforcement. A ledger whose immutability is a code convention is
-- immutable until the first hotfix; this makes the database refuse.
CREATE FUNCTION memgw_refuse_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	RAISE EXCEPTION 'memgw: % on % is refused; this table is append-only', TG_OP, TG_TABLE_NAME
		USING ERRCODE = 'raise_exception';
END;
$$;

CREATE TRIGGER events_append_only
	BEFORE UPDATE OR DELETE ON events
	FOR EACH ROW EXECUTE FUNCTION memgw_refuse_mutation();

-- Receipts. Written in the same transaction as the event, so a receipt existing
-- is proof the event committed. Never written before commit.
CREATE TABLE write_receipts (
	principal_id    UUID        NOT NULL REFERENCES principals (principal_id),
	idempotency_key TEXT        NOT NULL,
	event_id        UUID        REFERENCES events (event_id),
	payload_digest  TEXT        NOT NULL CHECK (payload_digest ~ '^[0-9a-f]{64}$'),
	state           TEXT        NOT NULL CHECK (state IN ('committed','duplicate','conflict','rejected_policy','quarantined')),
	seq             BIGINT,
	error_class     TEXT,
	recorded_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (principal_id, idempotency_key),
	CONSTRAINT receipts_committed_has_event CHECK (state <> 'committed' OR (event_id IS NOT NULL AND seq IS NOT NULL))
);

CREATE TRIGGER write_receipts_append_only
	BEFORE DELETE ON write_receipts
	FOR EACH ROW EXECUTE FUNCTION memgw_refuse_mutation();

-- Projection outbox. Filled in the commit transaction; drained by a worker that
-- may only project, never mutate canonical state.
CREATE TABLE projection_outbox (
	outbox_id        BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	event_id         UUID        NOT NULL REFERENCES events (event_id),
	event_seq        BIGINT      NOT NULL,
	projection       TEXT        NOT NULL CHECK (projection IN ('qdrant','graphify')),
	state            TEXT        NOT NULL DEFAULT 'pending'
		CHECK (state IN ('pending','in_flight','applied','failed','dead_letter')),
	attempts         INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
	not_before       TIMESTAMPTZ NOT NULL DEFAULT now(),
	lease_owner      TEXT,
	lease_expires_at TIMESTAMPTZ,
	last_error_class TEXT,
	applied_at       TIMESTAMPTZ,
	created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
	CONSTRAINT outbox_event_projection_unique UNIQUE (event_id, projection),
	CONSTRAINT outbox_in_flight_has_lease CHECK (state <> 'in_flight' OR lease_owner IS NOT NULL)
);

CREATE INDEX outbox_claimable_idx ON projection_outbox (projection, not_before, outbox_id)
	WHERE state IN ('pending', 'failed');

-- How far each projection has been applied. A watermark is a report, never an
-- authority: the ledger is still the only source of truth.
CREATE TABLE projection_watermark (
	projection  TEXT        PRIMARY KEY CHECK (projection IN ('qdrant','graphify')),
	applied_seq BIGINT      NOT NULL DEFAULT 0,
	updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO projection_watermark (projection) VALUES ('qdrant'), ('graphify');

-- The deletion journal. Monotonic and never deleted: it is the proof that a
-- deletion was ordered, and every rebuild and every restore replays it.
CREATE TABLE tombstones (
	tombstone_id           UUID        PRIMARY KEY,
	subject_type           TEXT        NOT NULL CHECK (subject_type IN ('document','event','checkpoint','fact','candidate','work','session')),
	subject_id             TEXT        NOT NULL,
	project_id             UUID        NOT NULL,
	workspace_id           UUID,
	-- A class, never free text: a reason field that quoted the offending
	-- content would re-introduce exactly what the tombstone removes.
	reason_class           TEXT        NOT NULL CHECK (reason_class IN (
		'sensitive_content','user_request','policy_violation','duplicate','superseded','legal_hold_release')),
	issued_by_principal_id UUID        NOT NULL REFERENCES principals (principal_id),
	issued_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
	erasure_required       BOOLEAN     NOT NULL DEFAULT false,
	erasure_completed_at   TIMESTAMPTZ,
	legacy_chunk_ids       TEXT[]      NOT NULL DEFAULT '{}',
	CONSTRAINT tombstones_erasure_requires_flag CHECK (erasure_completed_at IS NULL OR erasure_required)
);

CREATE INDEX tombstones_subject_idx ON tombstones (subject_type, subject_id);
CREATE INDEX tombstones_project_idx ON tombstones (project_id, issued_at);

CREATE TRIGGER tombstones_no_delete
	BEFORE DELETE ON tombstones
	FOR EACH ROW EXECUTE FUNCTION memgw_refuse_mutation();

-- Legacy chunk mapping. Pre-gateway Qdrant points were chunked from documents
-- with no stable identity of their own. Without this table a deletion ordered
-- today cannot find the chunks written last year, and re-chunking resurrects
-- them.
CREATE TABLE legacy_chunk_map (
	chunk_id      TEXT        PRIMARY KEY,
	project_id    UUID        NOT NULL,
	rag_project   TEXT        NOT NULL,
	document_id   TEXT        NOT NULL,
	source_digest TEXT        NOT NULL CHECK (source_digest ~ '^[0-9a-f]{64}$'),
	recorded_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
	tombstoned_at TIMESTAMPTZ
);

CREATE INDEX legacy_chunk_document_idx ON legacy_chunk_map (project_id, document_id);
CREATE INDEX legacy_chunk_untombstoned_idx ON legacy_chunk_map (project_id) WHERE tombstoned_at IS NULL;
