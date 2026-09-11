-- 0002_memgw_domain: the canonical entities 0001 deliberately deferred.
--
-- 0001 built the durable primitives -- identity, fencing, the append-only log,
-- receipts, the outbox and the deletion journal -- and named the entities it
-- was leaving out so the omission could not be mistaken for a decision that
-- they were unnecessary. This migration creates them: the project and
-- workspace registry, work state, session and branch lineage, immutable
-- checkpoints, the fact/candidate separation with its provenance, and the
-- foreign keys that make the scope columns on events mean something.
--
-- The ordering rule this migration follows: every relationship that can be
-- expressed as a constraint is expressed as one. Phase 3 enforced scope in Go
-- because the tables to point at did not exist. Now they do, and a check that
-- only lives in the caller is a check that the next caller forgets.

-- ---------------------------------------------------------------------------
-- Registry
-- ---------------------------------------------------------------------------

-- A project is the unit of authority: grants name one, and nothing crosses
-- between them. The id is stable across folder renames, which is why the
-- registry exists at all -- a basename is not an identity.
CREATE TABLE projects (
	project_id   UUID        PRIMARY KEY,
	slug         TEXT        NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
	display_name TEXT        NOT NULL DEFAULT '' CHECK (length(display_name) <= 200),
	status       TEXT        NOT NULL DEFAULT 'active' CHECK (status IN ('active','archived')),
	created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Repository identities are mapped explicitly. The key is a normalised
-- host/path with any userinfo stripped by the caller: a git remote URL can
-- carry a token, and a registry that stored the URL verbatim would be a
-- credential store nobody meant to build.
CREATE TABLE project_repos (
	-- host/path only: no scheme, no userinfo, no '@'. A key that cannot hold a
	-- token is a key that cannot leak one.
	repo_key    TEXT        PRIMARY KEY
		CHECK (repo_key ~ '^[a-z0-9][a-z0-9.-]*(:[0-9]+)?(/[A-Za-z0-9._-]+)+$'),
	project_id  UUID        NOT NULL REFERENCES projects (project_id),
	vcs         TEXT        NOT NULL DEFAULT 'git' CHECK (vcs IN ('git','none')),
	recorded_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX project_repos_project_idx ON project_repos (project_id);

-- A workspace is one checkout on one host. The root path is stored as a
-- digest, never as text: a path contains a user name, and the registry is read
-- by every host that shares the project.
CREATE TABLE workspaces (
	workspace_id     UUID        PRIMARY KEY,
	project_id       UUID        NOT NULL REFERENCES projects (project_id),
	host_id          UUID        NOT NULL,
	root_path_digest TEXT        NOT NULL CHECK (root_path_digest ~ '^[0-9a-f]{64}$'),
	label            TEXT        NOT NULL DEFAULT '' CHECK (length(label) <= 120),
	created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
	-- Redundant on its own; it is the target a composite foreign key needs, so
	-- that a row naming a workspace also proves the workspace is in that
	-- project rather than merely existing somewhere.
	CONSTRAINT workspaces_project_unique UNIQUE (project_id, workspace_id)
);

CREATE UNIQUE INDEX workspaces_host_root_idx ON workspaces (host_id, root_path_digest);

-- ---------------------------------------------------------------------------
-- Sessions and branches
-- ---------------------------------------------------------------------------

-- A session is one conversation on one host. parent_session_id is what a
-- resume records: the new session says which one it continues, and neither is
-- rewritten.
CREATE TABLE sessions (
	session_id        TEXT        PRIMARY KEY CHECK (length(session_id) BETWEEN 1 AND 128),
	project_id        UUID        NOT NULL,
	workspace_id      UUID        NOT NULL,
	principal_id      UUID        NOT NULL REFERENCES principals (principal_id),
	parent_session_id TEXT        REFERENCES sessions (session_id),
	host_id           UUID        NOT NULL,
	started_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
	ended_at          TIMESTAMPTZ,
	last_event_seq    BIGINT      NOT NULL DEFAULT 0,
	CONSTRAINT sessions_workspace_fk FOREIGN KEY (project_id, workspace_id)
		REFERENCES workspaces (project_id, workspace_id),
	CONSTRAINT sessions_end_after_start CHECK (ended_at IS NULL OR ended_at >= started_at)
);

CREATE INDEX sessions_project_idx ON sessions (project_id, started_at DESC);
CREATE INDEX sessions_parent_idx ON sessions (parent_session_id) WHERE parent_session_id IS NOT NULL;

-- A branch is a line of conversation. A rewind opens a new branch that names
-- where it diverged; the parent branch keeps every event it had and becomes
-- superseded. Nothing is renumbered and nothing is deleted -- INV-19.
CREATE TABLE branches (
	branch_id        UUID        PRIMARY KEY,
	project_id       UUID        NOT NULL REFERENCES projects (project_id),
	session_id       TEXT        NOT NULL REFERENCES sessions (session_id),
	parent_branch_id UUID        REFERENCES branches (branch_id),
	branch_point_seq BIGINT,
	reason_class     TEXT        CHECK (reason_class IS NULL OR reason_class IN ('rewind','fork','resume','compaction','initial')),
	status           TEXT        NOT NULL DEFAULT 'active' CHECK (status IN ('active','superseded','abandoned')),
	created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
	-- A branch with a parent must say where it left it. A branch point is the
	-- only thing that makes a rewind auditable rather than a second history.
	CONSTRAINT branches_child_has_point CHECK (parent_branch_id IS NULL OR branch_point_seq IS NOT NULL),
	CONSTRAINT branches_not_own_parent CHECK (parent_branch_id IS NULL OR parent_branch_id <> branch_id)
);

CREATE INDEX branches_session_idx ON branches (session_id);
CREATE INDEX branches_parent_idx ON branches (parent_branch_id) WHERE parent_branch_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Work
-- ---------------------------------------------------------------------------

-- The materialised work aggregate. revision is advanced by the same
-- transaction that appends the event, and the event log's unique
-- (aggregate_type, aggregate_id, revision) index is what decides the race --
-- this row is the readable form of that decision, never a second authority.
CREATE TABLE works (
	work_id        UUID        PRIMARY KEY,
	project_id     UUID        NOT NULL,
	workspace_id   UUID        NOT NULL,
	title          TEXT        NOT NULL DEFAULT '' CHECK (length(title) <= 200),
	state          TEXT        NOT NULL CHECK (state IN ('planned','active','blocked','review','completed','abandoned')),
	revision       BIGINT      NOT NULL CHECK (revision >= 0),
	last_event_seq BIGINT      NOT NULL,
	created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
	tombstoned_at  TIMESTAMPTZ,
	CONSTRAINT works_workspace_fk FOREIGN KEY (project_id, workspace_id)
		REFERENCES workspaces (project_id, workspace_id),
	CONSTRAINT works_project_unique UNIQUE (project_id, work_id)
);

CREATE INDEX works_project_state_idx ON works (project_id, state, updated_at DESC);

-- Checkpoints are events made queryable, not editable. A new checkpoint is a
-- new row at a new revision; no prior checkpoint is ever updated or deleted --
-- INV-09, enforced by trigger below rather than by convention.
CREATE TABLE checkpoints (
	checkpoint_id    UUID        PRIMARY KEY,
	event_id         UUID        NOT NULL UNIQUE REFERENCES events (event_id),
	project_id       UUID        NOT NULL,
	workspace_id     UUID        NOT NULL,
	work_id          UUID        NOT NULL REFERENCES works (work_id),
	session_id       TEXT        NOT NULL REFERENCES sessions (session_id),
	branch_id        UUID        NOT NULL REFERENCES branches (branch_id),
	principal_id     UUID        NOT NULL REFERENCES principals (principal_id),
	revision         BIGINT      NOT NULL,
	seq              BIGINT      NOT NULL,
	objective        TEXT        NOT NULL CHECK (length(objective) BETWEEN 1 AND 4096),
	completed_work   TEXT        NOT NULL DEFAULT '' CHECK (length(completed_work) <= 4096),
	pending_actions  TEXT        NOT NULL DEFAULT '' CHECK (length(pending_actions) <= 4096),
	blockers         TEXT        NOT NULL DEFAULT '' CHECK (length(blockers) <= 4096),
	next_safe_action TEXT        NOT NULL CHECK (length(next_safe_action) BETWEEN 1 AND 4096),
	modified_files   JSONB       NOT NULL DEFAULT '[]'
		CHECK (jsonb_typeof(modified_files) = 'array' AND jsonb_array_length(modified_files) <= 200),
	evidence_refs    UUID[]      NOT NULL DEFAULT '{}' CHECK (cardinality(evidence_refs) <= 64),
	content_digest   TEXT        NOT NULL CHECK (content_digest ~ '^[0-9a-f]{64}$'),
	recorded_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
	CONSTRAINT checkpoints_work_revision_unique UNIQUE (work_id, revision)
);

CREATE INDEX checkpoints_work_seq_idx ON checkpoints (work_id, seq DESC);
CREATE INDEX checkpoints_branch_idx ON checkpoints (branch_id, seq DESC);

CREATE TRIGGER checkpoints_append_only
	BEFORE UPDATE OR DELETE ON checkpoints
	FOR EACH ROW EXECUTE FUNCTION memgw_refuse_mutation();

-- ---------------------------------------------------------------------------
-- Facts, candidates, provenance
-- ---------------------------------------------------------------------------

-- A slot is one (project, subject, predicate). It is the aggregate a
-- single-valued promotion compare-and-sets against: the contested thing is the
-- predicate, not any individual value of it. A multi-valued slot exists too,
-- but promotions into it are additive and never contend.
CREATE TABLE fact_slots (
	slot_id     UUID        PRIMARY KEY,
	project_id  UUID        NOT NULL REFERENCES projects (project_id),
	subject     TEXT        NOT NULL CHECK (length(subject) BETWEEN 1 AND 200),
	predicate   TEXT        NOT NULL CHECK (length(predicate) BETWEEN 1 AND 120),
	cardinality TEXT        NOT NULL CHECK (cardinality IN ('single','multi')),
	revision    BIGINT      NOT NULL DEFAULT 0 CHECK (revision >= 0),
	status      TEXT        NOT NULL DEFAULT 'active' CHECK (status IN ('active','conflicted')),
	updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
	CONSTRAINT fact_slots_unique UNIQUE (project_id, subject, predicate)
);

CREATE TABLE facts (
	fact_id                  UUID        PRIMARY KEY,
	slot_id                  UUID        NOT NULL REFERENCES fact_slots (slot_id),
	project_id               UUID        NOT NULL REFERENCES projects (project_id),
	workspace_id             UUID,
	subject                  TEXT        NOT NULL,
	predicate                TEXT        NOT NULL,
	object                   JSONB       NOT NULL,
	object_digest            TEXT        NOT NULL CHECK (object_digest ~ '^[0-9a-f]{64}$'),
	cardinality              TEXT        NOT NULL CHECK (cardinality IN ('single','multi')),
	status                   TEXT        NOT NULL CHECK (status IN ('active','superseded','retracted','conflicted')),
	evidence_class           TEXT        NOT NULL CHECK (evidence_class IN ('observed','derived','asserted','unverified')),
	sensitivity_class        TEXT        NOT NULL DEFAULT 'internal'
		CHECK (sensitivity_class IN ('public','internal','confidential','restricted')),
	valid_from               TIMESTAMPTZ NOT NULL,
	valid_to                 TIMESTAMPTZ,
	recorded_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
	superseded_by            UUID        REFERENCES facts (fact_id),
	promoted_by_principal_id UUID        NOT NULL REFERENCES principals (principal_id),
	promoted_event_id        UUID        NOT NULL REFERENCES events (event_id),
	candidate_id             UUID,
	revision                 BIGINT      NOT NULL CHECK (revision >= 1),
	tombstoned_at            TIMESTAMPTZ,
	CONSTRAINT facts_valid_range CHECK (valid_to IS NULL OR valid_to > valid_from),
	-- A superseded fact must say what replaced it, and an active one must not.
	CONSTRAINT facts_superseded_has_successor CHECK (
		(status = 'superseded' AND superseded_by IS NOT NULL) OR
		(status <> 'superseded' AND superseded_by IS NULL))
);

-- The same value may not be promoted twice into the same slot while it still
-- stands. Retracted values are excluded so a retraction can be reversed by a
-- fresh, explicit promotion.
CREATE UNIQUE INDEX facts_slot_object_live_idx ON facts (slot_id, object_digest)
	WHERE status <> 'retracted';
CREATE INDEX facts_slot_status_idx ON facts (slot_id, status);
CREATE INDEX facts_project_subject_idx ON facts (project_id, subject, predicate);
CREATE INDEX facts_live_idx ON facts (project_id, status) WHERE tombstoned_at IS NULL;

-- A candidate is a proposal. It is not readable as current state and it cannot
-- authorise itself: promotion is a separate event by a principal holding
-- fact_promote -- INV-10.
CREATE TABLE fact_candidates (
	candidate_id             UUID        PRIMARY KEY,
	project_id               UUID        NOT NULL REFERENCES projects (project_id),
	workspace_id             UUID,
	subject                  TEXT        NOT NULL CHECK (length(subject) BETWEEN 1 AND 200),
	predicate                TEXT        NOT NULL CHECK (length(predicate) BETWEEN 1 AND 120),
	object                   JSONB       NOT NULL,
	object_digest            TEXT        NOT NULL CHECK (object_digest ~ '^[0-9a-f]{64}$'),
	cardinality              TEXT        NOT NULL CHECK (cardinality IN ('single','multi')),
	-- source and extractor are NOT NULL because a claim with no traceable
	-- origin is not reviewable. model and model_version may be null: a
	-- deterministic extractor has neither.
	source                   TEXT        NOT NULL CHECK (length(source) BETWEEN 1 AND 200),
	extractor                TEXT        NOT NULL CHECK (length(extractor) BETWEEN 1 AND 120),
	model                    TEXT,
	model_version            TEXT,
	confidence               DOUBLE PRECISION CHECK (confidence IS NULL OR (confidence >= 0 AND confidence <= 1)),
	evidence_class           TEXT        NOT NULL DEFAULT 'derived'
		CHECK (evidence_class IN ('observed','derived','asserted','unverified')),
	review_status            TEXT        NOT NULL DEFAULT 'pending'
		CHECK (review_status IN ('pending','promoted','rejected','expired')),
	proposed_by_principal_id UUID        NOT NULL REFERENCES principals (principal_id),
	proposed_event_id        UUID        NOT NULL REFERENCES events (event_id),
	promoted_fact_id         UUID        REFERENCES facts (fact_id),
	promoted_event_id        UUID        REFERENCES events (event_id),
	proposed_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
	reviewed_at              TIMESTAMPTZ,
	expires_at               TIMESTAMPTZ,
	CONSTRAINT candidates_promoted_has_fact CHECK (
		review_status <> 'promoted' OR (promoted_fact_id IS NOT NULL AND promoted_event_id IS NOT NULL))
);

CREATE INDEX candidates_pending_idx ON fact_candidates (project_id, review_status, proposed_at);

-- Provenance is kept separately from the fact so a value can carry several
-- independent sources, and so a source can be recorded for a candidate that
-- never becomes a fact.
CREATE TABLE provenance_refs (
	provenance_id  UUID        PRIMARY KEY,
	project_id     UUID        NOT NULL REFERENCES projects (project_id),
	fact_id        UUID        REFERENCES facts (fact_id),
	candidate_id   UUID        REFERENCES fact_candidates (candidate_id),
	source_type    TEXT        NOT NULL CHECK (source_type IN ('event','document','file','tool_output','human')),
	source_ref     TEXT        NOT NULL CHECK (length(source_ref) BETWEEN 1 AND 400),
	source_digest  TEXT        CHECK (source_digest IS NULL OR source_digest ~ '^[0-9a-f]{64}$'),
	evidence_class TEXT        NOT NULL CHECK (evidence_class IN ('observed','derived','asserted','unverified')),
	recorded_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
	CONSTRAINT provenance_has_target CHECK (fact_id IS NOT NULL OR candidate_id IS NOT NULL)
);

CREATE INDEX provenance_fact_idx ON provenance_refs (fact_id) WHERE fact_id IS NOT NULL;
CREATE INDEX provenance_candidate_idx ON provenance_refs (candidate_id) WHERE candidate_id IS NOT NULL;

-- Evidence recorded by an agent or a subagent. It is addressable so an event
-- can cite it, and it is deliberately not a fact: a subagent's finding is
-- input to the parent's decision, never the decision -- INV-16.
CREATE TABLE evidence (
	evidence_id    UUID        PRIMARY KEY,
	event_id       UUID        NOT NULL UNIQUE REFERENCES events (event_id),
	project_id     UUID        NOT NULL REFERENCES projects (project_id),
	workspace_id   UUID        NOT NULL,
	work_id        UUID        REFERENCES works (work_id),
	session_id     TEXT        NOT NULL REFERENCES sessions (session_id),
	principal_id   UUID        NOT NULL REFERENCES principals (principal_id),
	evidence_type  TEXT        NOT NULL CHECK (length(evidence_type) BETWEEN 1 AND 60),
	evidence_class TEXT        NOT NULL CHECK (evidence_class IN ('observed','derived','asserted','unverified')),
	locator        TEXT        NOT NULL DEFAULT '' CHECK (length(locator) <= 400),
	seq            BIGINT      NOT NULL,
	recorded_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX evidence_work_idx ON evidence (work_id, seq DESC) WHERE work_id IS NOT NULL;

CREATE TRIGGER evidence_append_only
	BEFORE UPDATE OR DELETE ON evidence
	FOR EACH ROW EXECUTE FUNCTION memgw_refuse_mutation();

-- ---------------------------------------------------------------------------
-- Tombstone ordering
-- ---------------------------------------------------------------------------

-- A tombstone now records which event ordered it and where that event sits in
-- the log. Ordering by wall clock would make a resurrection race unresolvable:
-- the projection needs to know that the deletion is at seq N, not that it
-- happened "recently".
ALTER TABLE tombstones ADD COLUMN issued_event_id UUID REFERENCES events (event_id);
ALTER TABLE tombstones ADD COLUMN issued_event_seq BIGINT;

CREATE INDEX tombstones_seq_idx ON tombstones (issued_event_seq) WHERE issued_event_seq IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Foreign keys on the event log
-- ---------------------------------------------------------------------------

-- 0001 left these columns as bare UUIDs because the tables to point at did not
-- exist. They exist now. An event whose project, workspace, session, branch or
-- work is unknown is a bug in the writer, and the database is the last place
-- that can still say so.
ALTER TABLE events ADD CONSTRAINT events_project_fk
	FOREIGN KEY (project_id) REFERENCES projects (project_id);
ALTER TABLE events ADD CONSTRAINT events_workspace_fk
	FOREIGN KEY (project_id, workspace_id) REFERENCES workspaces (project_id, workspace_id);
ALTER TABLE events ADD CONSTRAINT events_session_fk
	FOREIGN KEY (session_id) REFERENCES sessions (session_id);
ALTER TABLE events ADD CONSTRAINT events_branch_fk
	FOREIGN KEY (branch_id) REFERENCES branches (branch_id);
ALTER TABLE events ADD CONSTRAINT events_work_fk
	FOREIGN KEY (project_id, work_id) REFERENCES works (project_id, work_id);

-- Grants may only name a project that exists. A grant on a typo'd project id
-- is a grant that silently covers nothing, which is the kind of failure that
-- is discovered during an incident.
ALTER TABLE grants ADD CONSTRAINT grants_project_fk
	FOREIGN KEY (project_id) REFERENCES projects (project_id);
