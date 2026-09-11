package migrate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/migrations"
	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
)

// Planner turns a source into a plan. It writes nothing.
//
// Known is what the target already holds. It is passed in rather than read
// here so that a plan can be produced against an exported snapshot of the
// mapping table, on a machine that cannot reach the database at all -- which is
// the situation an operator preparing a change window is usually in.
type Planner struct {
	Source Source
	Known  map[string]Known
	// ProjectID is the ledger project the corpus belongs to. Leaving it unset
	// derives one from the rag project name, which is only right when the
	// corpus belongs to no registered project: a mapping filed under a derived
	// id is invisible to a tombstone issued against the real one.
	ProjectID uuid.UUID
	Now       func() time.Time
}

// Plan classifies every record the source hands over.
func (pl Planner) Plan(ctx context.Context) (Plan, error) {
	if pl.Source == nil {
		return Plan{}, errors.New("migrate: a plan needs a source")
	}
	now := pl.Now
	if now == nil {
		now = time.Now
	}
	p := Plan{Source: pl.Source.Name(), Namespace: ProjectNamespace, ProjectID: pl.ProjectID}
	if pl.ProjectID == uuid.Nil {
		p.ProjectIDSource = "derived from the rag project name"
	} else {
		p.ProjectIDSource = "declared by the operator"
	}
	seen := make(map[string]bool)
	err := pl.Source.Each(ctx, func(r Record) error {
		e := classify(r, pl.Known, pl.ProjectID)
		// A source that hands the same chunk over twice would otherwise put two
		// lines in the plan and two counts against one record. The first wins;
		// the duplicate is reported rather than silently folded away, because a
		// corpus with duplicate ids is a fact about the corpus.
		if e.ChunkID != "" {
			if seen[e.ChunkID] {
				e.Decision, e.Reason = Rejected, ReasonDuplicateInSource
			} else {
				seen[e.ChunkID] = true
			}
		}
		p.Entries = append(p.Entries, e)
		return nil
	})
	if err != nil {
		return Plan{}, err
	}
	p.finish()
	p.GeneratedAt = now().UTC()
	if !p.Balanced() {
		return Plan{}, errors.New("migrate: the plan does not account for every record")
	}
	return p, nil
}

// Target is the mapping table a plan is read from and applied to.
type Target interface {
	Known(ctx context.Context, ragProject string) (map[string]Known, error)
	Apply(ctx context.Context, p Plan) (Applied, error)
}

// Applied is what an apply actually did.
type Applied struct {
	// Inserted are rows that were not there before.
	Inserted int64
	// AlreadyPresent are imported entries whose row already existed with the
	// same mapping. Applying a plan twice must be a no-op, and this is how that
	// is observed rather than assumed.
	AlreadyPresent int64
	// Conflicted are imported entries whose row exists with a different
	// document or digest. Nothing is overwritten; they are counted and named.
	Conflicted []string
	// Skipped are the plan lines an apply never touches: everything that is not
	// an import.
	Skipped int64
}

// PGTarget is the mapping table in a verified PostgreSQL target.
//
// The target check runs at construction, not at write time, and the type cannot
// be built without it passing. There is no flag on this type that relaxes it.
//
// The purpose is history_import, and that is not an accident of naming: a
// production target assertion that authorises schema_migration does **not**
// authorise this. Adding structure and rewriting historical rows are different
// acts with different failure modes, so they carry separate authority. An
// operator who approved a migration has not thereby approved an import.
type PGTarget struct{ pool *pgstore.Pool }

// NewPGTarget refuses a target that has not been verified for history import.
// In development and test that is the unchanged non-production check; in
// production it additionally requires an explicit assertion whose allowed set
// contains history_import.
func NewPGTarget(ctx context.Context, pool *pgstore.Pool) (*PGTarget, error) {
	// The mapping table this writes to is created by the migration set, so the
	// same set defines what may legitimately live in the managed schema.
	if managed, err := migrations.New(pool).ManagedObjects(); err == nil {
		pool.DeclareManagedObjects(managed)
	}
	if err := pgstore.VerifyTarget(ctx, pool, pgstore.PurposeHistoryImport); err != nil {
		return nil, err
	}
	return &PGTarget{pool: pool}, nil
}

// Known reads the mapping rows for one rag project.
func (t *PGTarget) Known(ctx context.Context, ragProject string) (map[string]Known, error) {
	rows, err := t.pool.Pgx().Query(ctx,
		`SELECT chunk_id, document_id, source_digest, project_id FROM legacy_chunk_map WHERE rag_project = $1`,
		ragProject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]Known)
	for rows.Next() {
		var chunk string
		var k Known
		if err := rows.Scan(&chunk, &k.DocumentID, &k.SourceDigest, &k.ProjectID); err != nil {
			return nil, err
		}
		out[chunk] = k
	}
	return out, rows.Err()
}

// Apply writes the imported entries and nothing else.
//
// It inserts mapping rows. It does not write events, it does not create facts
// and it has no path that could: the only statement here is an insert into
// legacy_chunk_map, and a row there is a pointer from an old chunk id to the
// document it came from. Promotion is a decision a principal makes through the
// gateway, and this code cannot reach it.
func (t *PGTarget) Apply(ctx context.Context, p Plan) (Applied, error) {
	var a Applied
	if p.PlanVersion != PlanVersion {
		return a, fmt.Errorf("migrate: plan version %q is not %q", p.PlanVersion, PlanVersion)
	}
	tx, err := t.pool.Pgx().Begin(ctx)
	if err != nil {
		return a, err
	}
	defer tx.Rollback(ctx)

	for _, e := range p.Entries {
		if e.Decision != Imported {
			a.Skipped++
			continue
		}
		var doc, digest string
		var project uuid.UUID
		var inserted bool
		// xmax = 0 on the returned row means this statement inserted it; a
		// non-zero xmax means the row was already there and the DO UPDATE only
		// touched it. That is how an apply can report what it changed instead
		// of reporting what it attempted.
		err := tx.QueryRow(ctx, `
			INSERT INTO legacy_chunk_map (chunk_id, project_id, rag_project, document_id, source_digest)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (chunk_id) DO UPDATE SET chunk_id = legacy_chunk_map.chunk_id
			RETURNING document_id, source_digest, project_id, (xmax = 0)`,
			e.ChunkID, e.ProjectID, e.RAGProject, e.DocumentID, e.SourceDigest).Scan(&doc, &digest, &project, &inserted)
		if err != nil {
			return Applied{}, fmt.Errorf("migrate: chunk %s: %w", e.ChunkID, err)
		}
		switch {
		// The project is compared too. A row left under a project the ledger
		// does not know is invisible to every tombstone issued against the real
		// one, and a deletion that reaches nothing while reporting success is
		// the worst failure this table has.
		case doc != e.DocumentID || digest != e.SourceDigest || project != e.ProjectID:
			a.Conflicted = append(a.Conflicted, e.ChunkID)
		case inserted:
			a.Inserted++
		default:
			a.AlreadyPresent++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Applied{}, err
	}
	return a, nil
}
