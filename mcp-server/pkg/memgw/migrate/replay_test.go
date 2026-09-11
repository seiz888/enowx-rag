package migrate_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/memgwtest"
	"github.com/enowdev/enowx-rag/pkg/memgw/migrate"
	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
)

type replayEnv struct {
	t         *testing.T
	ctx       context.Context
	pool      *pgstore.Pool
	target    *migrate.PGTarget
	project   uuid.UUID
	principal uuid.UUID
}

func newReplayEnv(t *testing.T) *replayEnv {
	t.Helper()
	pool := memgwtest.Pool(t)
	e := &replayEnv{t: t, ctx: t.Context(), pool: pool, project: uuid.New(), principal: uuid.New()}
	if _, err := pool.Pgx().Exec(e.ctx, `
		INSERT INTO principals (principal_id, principal_type, host_id, agent_id, display_name, status, writer_epoch)
		VALUES ($1, 'service', $2, 'replay-test', 'replay-test', 'active', 1)`,
		e.principal, uuid.New()); err != nil {
		t.Fatalf("seed principal: %v", err)
	}
	target, err := migrate.NewPGTarget(e.ctx, pool)
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	e.target = target
	return e
}

func (e *replayEnv) chunk(id, document string) {
	e.t.Helper()
	if _, err := e.pool.Pgx().Exec(e.ctx, `
		INSERT INTO legacy_chunk_map (chunk_id, project_id, rag_project, document_id, source_digest)
		VALUES ($1,$2,'memory',$3,repeat('a',64))`, id, e.project, document); err != nil {
		e.t.Fatalf("seed chunk %s: %v", id, err)
	}
}

// tombstone writes a journal row directly. The gateway writes it through an
// event; what the replay reads is this row, so this is the state under test.
func (e *replayEnv) tombstone(subject string, chunks []string, at time.Time) {
	e.t.Helper()
	if chunks == nil {
		chunks = []string{}
	}
	if _, err := e.pool.Pgx().Exec(e.ctx, `
		INSERT INTO tombstones (tombstone_id, subject_type, subject_id, project_id,
		                        reason_class, issued_by_principal_id, issued_at, legacy_chunk_ids)
		VALUES ($1,'document',$2,$3,'user_request',$4,$5,$6)`,
		uuid.New(), subject, e.project, e.principal, at, chunks); err != nil {
		e.t.Fatalf("seed tombstone %s: %v", subject, err)
	}
}

func (e *replayEnv) marked(id string) *time.Time {
	e.t.Helper()
	var at *time.Time
	if err := e.pool.Pgx().QueryRow(e.ctx,
		`SELECT tombstoned_at FROM legacy_chunk_map WHERE chunk_id = $1`, id).Scan(&at); err != nil {
		e.t.Fatalf("read %s: %v", id, err)
	}
	return at
}

// TestReplayReachesARowImportedAfterTheDeletion is the case that made this
// function necessary: the deletion was ordered, then the mapping arrived. Until
// the replay runs, the corpus answers with content somebody deleted.
func TestReplayReachesARowImportedAfterTheDeletion(t *testing.T) {
	e := newReplayEnv(t)
	issued := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	e.tombstone("session-2026-08-04", nil, issued)
	e.chunk("lg-0001", "session-2026-08-04")
	e.chunk("lg-0002", "session-2026-08-04")
	e.chunk("lg-0003", "session-2026-08-11")

	r, err := e.target.Replay(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	if r.Tombstones != 1 || r.Marked != 2 {
		t.Fatalf("replay saw %d tombstones and marked %d rows, want 1 and 2", r.Tombstones, r.Marked)
	}
	for _, id := range []string{"lg-0001", "lg-0002"} {
		at := e.marked(id)
		if at == nil {
			t.Fatalf("%s is still readable after a replay", id)
		}
		// The mark is when the deletion was ordered, not when the repair ran.
		if !at.UTC().Equal(issued) {
			t.Fatalf("%s is marked %s, want the tombstone's own %s", id, at.UTC(), issued)
		}
	}
	if at := e.marked("lg-0003"); at != nil {
		t.Fatal("the replay marked a row no tombstone names")
	}
}

// TestReplayReachesANamedChunkID: re-chunking changes ids, so a tombstone that
// names chunks has to be honoured by id as well as by document.
func TestReplayReachesANamedChunkID(t *testing.T) {
	e := newReplayEnv(t)
	e.chunk("lg-0009", "some-other-document")
	e.tombstone("a-document-that-matches-nothing", []string{"lg-0009"}, time.Now().UTC())

	r, err := e.target.Replay(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	if r.Marked != 1 {
		t.Fatalf("marked %d rows, want 1", r.Marked)
	}
	if e.marked("lg-0009") == nil {
		t.Fatal("a chunk named by id was left readable")
	}
}

// TestReplayIsIdempotentAndNeverMovesADeletionLater: running it twice, or
// running it after the gateway already marked a row, must not rewrite the
// existing mark. A deletion's timestamp is evidence.
func TestReplayIsIdempotentAndNeverMovesADeletionLater(t *testing.T) {
	e := newReplayEnv(t)
	first := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Millisecond)
	e.chunk("lg-0100", "doc-a")
	e.tombstone("doc-a", nil, first)
	if _, err := e.target.Replay(e.ctx, e.project); err != nil {
		t.Fatal(err)
	}
	// A second, later deletion naming the same document must not push the mark
	// forward, and the second replay must change nothing at all.
	e.tombstone("doc-a", nil, time.Now().UTC())

	r, err := e.target.Replay(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	if r.Marked != 0 {
		t.Fatalf("a second replay marked %d rows, want 0", r.Marked)
	}
	if at := e.marked("lg-0100"); at == nil || !at.UTC().Equal(first) {
		t.Fatalf("the mark moved to %v, want %s", at, first)
	}
}

// TestReplayScopeIsOneProject: an operator repairing one project must not
// silently delete another project's corpus.
func TestReplayScopeIsOneProject(t *testing.T) {
	e := newReplayEnv(t)
	e.chunk("lg-0200", "doc-b")
	e.tombstone("doc-b", nil, time.Now().UTC())

	r, err := e.target.Replay(e.ctx, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if r.Tombstones != 0 || r.Marked != 0 {
		t.Fatalf("a replay scoped elsewhere saw %d tombstones and marked %d rows", r.Tombstones, r.Marked)
	}
	if e.marked("lg-0200") != nil {
		t.Fatal("a replay scoped to another project marked this one")
	}
}
