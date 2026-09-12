-- memgw ledger digest -- a content-level fingerprint of the whole ledger.
--
-- WHY THIS EXISTS
-- ---------------
-- Row counts do not identify a ledger. During this migration a staging ledger
-- reported 135 events / 135 receipts and `max(seq) = 135`, identical to the
-- authoritative source, and was nevertheless a DIFFERENT ledger: a probe event
-- occupied seq 135 there while the source had a real `omp.compact` committed at
-- that position. A reconciliation built on counts, or even on `max(seq)`, would
-- have declared that a success and then overwritten acknowledged writes.
--
-- So the comparison is made over content, using an algorithm stated here so it
-- can be re-derived by anyone rather than trusted:
--
--   algorithm   memgw-ledger-digest/v1
--   hash        SHA-256 (PostgreSQL built-in `sha256()`, pgcrypto not required)
--   encoding    UTF-8, lowercase hex output
--   timezone    forced to UTC before hashing, so `timestamptz` text is
--               independent of the server's local setting
--
-- Over, per table `T` in schema `memgw`, ordered by table name:
--
--   table <T> rows=<n> sha256=<h>
--
-- where `<h>` = SHA-256 over the UTF-8 bytes of the newline-joined JSONB text
-- of every row, **sorted by that text** (`ORDER BY to_jsonb(t)::text`). Sorting
-- by the row's own serialization makes the digest a canonical set-hash: it is
-- independent of physical row order and of the table's primary key, so it needs
-- no per-table knowledge and automatically covers every table, including any
-- added later. JSONB sorts object keys, so the serialization is canonical.
--
-- Two additional lines preserve ORDER where order is meaningful, because a
-- set-hash deliberately discards it and the chain is not a set:
--
--   chain events         rows=<n> sha256=<h>   (rows in `seq` order)
--   chain write_receipts rows=<n> sha256=<h>   (rows in `seq` order)
--
-- A rollup ties the whole report together so one value can be compared:
--
--   ledger sha256=<h>   = SHA-256 over every emitted line, sorted, newline-joined
--
-- WHAT IT COVERS
-- --------------
-- Both append-only history (events, write_receipts, evidence, checkpoints,
-- tombstones, principal_credentials) and materialized/aggregate state (works,
-- sessions, branches, facts, fact_slots, fact_candidates, legacy_chunk_map,
-- provenance_refs, project_repos, principals, grants, projects, workspaces,
-- projection_outbox, projection_watermark, deployment_identity, writer_epochs).
-- Historical records and the state derived from them are hashed together: a
-- restore that reproduced the log but not the aggregates would pass a log-only
-- check and still be unusable.
--
-- EXPECTED DELTAS
-- ---------------
-- Two deliberate differences between the source ledger and the restored server
-- ledger are not corruption, and each is isolated to named lines:
--
--   1. `deployment_identity` is changed from the workstation name to the
--      server's name after the restore. The gateway refuses to serve when
--      `MEMGW_PRODUCTION_DEPLOYMENT` does not match this row, so the change is
--      functional. The delta appears on the `table deployment_identity` line.
--   2. `writer_epochs` gains the epoch opened on the server at cutover. That is
--      an append to the ledger's own log, and appears on the
--      `table writer_epochs` line.
--
-- Before either step, a correct restore must produce a digest IDENTICAL to the
-- source. If it does not, the restore is wrong and nothing downstream may run.
--
-- USAGE
--   psql -p 5433 -d memgw -f ledger-digest.sql
-- Run with `-t -A` for a compact machine-comparable report.

\set ON_ERROR_STOP on
SET TIME ZONE 'UTC';
SET extra_float_digits = 3;

-- No `ON COMMIT DROP`: without an explicit transaction each statement commits
-- on its own, so the table would be dropped at the end of this very CREATE and
-- every later INSERT would fail. It is a TEMP table, so it disappears with the
-- session anyway.
DROP TABLE IF EXISTS memgw_digest_lines;
CREATE TEMP TABLE memgw_digest_lines (line text);

DO $$
DECLARE
    r    record;
    n    bigint;
    h    text;
BEGIN
    FOR r IN
        SELECT tablename FROM pg_tables WHERE schemaname = 'memgw' ORDER BY tablename
    LOOP
        -- `%I` quotes the identifier, so a table name can never break out of the
        -- format string. The aggregate is over the row's own canonical JSONB
        -- text, ordered by itself: a deterministic set-hash.
        EXECUTE format(
            $f$SELECT count(*),
                      encode(sha256(convert_to(
                        COALESCE(string_agg(to_jsonb(t)::text, E'\n' ORDER BY to_jsonb(t)::text), ''),
                        'UTF8')), 'hex')
                 FROM memgw.%I t$f$, r.tablename)
        INTO n, h;

        INSERT INTO memgw_digest_lines
        VALUES (format('table %s rows=%s sha256=%s', r.tablename, n, COALESCE(h, '-empty-')));
    END LOOP;
END $$;

-- Order-preserving digests for the two append-only logs that form the chain.
-- `seq` is meaningful here in a way it is not for a set of rows.
INSERT INTO memgw_digest_lines
SELECT format('chain events rows=%s sha256=%s', count(*),
              COALESCE(encode(sha256(convert_to(
                  string_agg(to_jsonb(e)::text, E'\n' ORDER BY e.seq), 'UTF8')), 'hex'), '-empty-'))
FROM memgw.events e;

INSERT INTO memgw_digest_lines
SELECT format('chain write_receipts rows=%s sha256=%s', count(*),
              COALESCE(encode(sha256(convert_to(
                  string_agg(to_jsonb(w)::text, E'\n' ORDER BY w.seq), 'UTF8')), 'hex'), '-empty-'))
FROM memgw.write_receipts w;

-- The per-row digest of the chain, retaining `seq` explicitly. This is what
-- localises a divergence to a position instead of merely reporting inequality.
INSERT INTO memgw_digest_lines
SELECT format('chain-position events seq=%s event_id=%s epoch=%s sha256=%s',
              e.seq, e.event_id, e.writer_epoch,
              encode(sha256(convert_to(to_jsonb(e)::text, 'UTF8')), 'hex'))
FROM memgw.events e
WHERE e.seq > (SELECT COALESCE(max(seq), 0) - 3 FROM memgw.events);

SELECT line FROM memgw_digest_lines ORDER BY line;

SELECT format('ledger sha256=%s',
              encode(sha256(convert_to(
                  string_agg(line, E'\n' ORDER BY line), 'UTF8')), 'hex'))
FROM memgw_digest_lines;
