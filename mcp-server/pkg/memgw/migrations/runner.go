// Package migrations is the schema migration runner for the memory gateway.
//
// It exists because the repository had no SQL migration mechanism: pkg/migrate
// moves documents between vector stores, and pkg/core's SQLite metrics store
// creates its tables inline and detects an already-applied ALTER by matching on
// an error string. Neither is a model for a ledger whose whole value is that it
// can be trusted about what it contains.
//
// The design is deliberately small: numbered SQL files embedded in the binary,
// a history table with checksums, one transaction per migration, and a refusal
// on anything ambiguous. There is no down migration -- a mistake is corrected by
// a new forward migration, the same rule the event ledger itself follows.
package migrations

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
)

//go:embed sql/*.sql
var builtinSQL embed.FS

var (
	// ErrChecksumDrift means an already-applied migration file changed on
	// disk. The runner refuses rather than guessing which version of the file
	// the database actually reflects.
	ErrChecksumDrift = errors.New("memgw: applied migration checksum changed")
	// ErrDirty means a previous run left a migration half-applied.
	ErrDirty = errors.New("memgw: schema is dirty from a partial migration")
	// ErrOutOfOrder means a new migration was inserted below an applied one.
	ErrOutOfOrder = errors.New("memgw: migration inserted below an applied version")
)

var fileNamePattern = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// noTransactionMarker lets a migration opt out of the surrounding transaction
// for statements PostgreSQL refuses to run inside one (CREATE INDEX
// CONCURRENTLY being the usual reason). Such a migration is recorded dirty
// before it runs and cleared after, so a crash mid-way is visible.
const noTransactionMarker = "-- memgw:no-transaction"

// Migration is one versioned SQL file.
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
	InTx     bool
}

// AppliedMigration is one row of the history table.
type AppliedMigration struct {
	Version     int
	Name        string
	Checksum    string
	AppliedAt   time.Time
	ExecutionMS int64
	Dirty       bool
}

// Status pairs what is on disk with what the database says about it.
type Status struct {
	Version   int
	Name      string
	Applied   bool
	Dirty     bool
	Drifted   bool
	AppliedAt time.Time
}

// Runner applies migrations to one explicit schema in one explicit database.
type Runner struct {
	pool    *pgstore.Pool
	sources fs.FS
}

// New returns a runner over the embedded migration set.
func New(pool *pgstore.Pool) *Runner {
	return &Runner{pool: pool, sources: builtinSQL}
}

// NewWithSources returns a runner over a caller-supplied migration set. Tests
// use it to exercise drift and out-of-order detection without shipping broken
// SQL in the binary.
func NewWithSources(pool *pgstore.Pool, sources fs.FS) *Runner {
	return &Runner{pool: pool, sources: sources}
}

// Load reads and validates the migration set. It fails on duplicate versions
// and on names that do not follow NNNN_snake_case.sql, because a migration set
// whose order is ambiguous has no defined meaning.
func (r *Runner) Load() ([]Migration, error) {
	entries, err := fs.ReadDir(r.sources, "sql")
	if err != nil {
		return nil, fmt.Errorf("memgw: read migration dir: %w", err)
	}
	var out []Migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := fileNamePattern.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("memgw: migration file %q does not match NNNN_name.sql", e.Name())
		}
		version, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("memgw: migration file %q has an unreadable version", e.Name())
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("memgw: migrations %s and %s share version %d", prev, e.Name(), version)
		}
		seen[version] = e.Name()

		body, err := fs.ReadFile(r.sources, path.Join("sql", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("memgw: read %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{
			Version:  version,
			Name:     m[2],
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
			InTx:     !strings.Contains(string(body), noTransactionMarker),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// ensureHistory creates the schema and the history table. It is the only DDL
// the runner performs outside a migration file, and it is idempotent.
func (r *Runner) ensureHistory(ctx context.Context) error {
	ctx, cancel := r.pool.WithQueryTimeout(ctx)
	defer cancel()
	schema := pgstore.QuoteIdent(r.pool.Schema())
	// deployment_identity and migration_audit are created here rather than in a
	// migration file because both have to exist before the first migration
	// runs: the identity is what proves this schema is the installation the
	// operator named, and the audit table has to be able to record a failure of
	// migration 0001 itself.
	_, err := r.pool.Pgx().Exec(ctx, fmt.Sprintf(`
CREATE SCHEMA IF NOT EXISTS %[1]s;
CREATE TABLE IF NOT EXISTS %[1]s.schema_migrations (
	version       INTEGER PRIMARY KEY,
	name          TEXT        NOT NULL,
	checksum      TEXT        NOT NULL,
	applied_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
	execution_ms  BIGINT      NOT NULL DEFAULT 0,
	dirty         BOOLEAN     NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS %[1]s.%[2]s (
	-- one row, enforced by the primary key rather than by convention
	only_row      BOOLEAN     PRIMARY KEY DEFAULT true CHECK (only_row),
	deployment    TEXT        NOT NULL,
	recorded_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS %[1]s.migration_audit (
	audit_id      BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	at            TIMESTAMPTZ NOT NULL DEFAULT now(),
	actor         TEXT        NOT NULL,
	deployment    TEXT        NOT NULL DEFAULT '',
	plan_digest   TEXT        NOT NULL,
	versions      INTEGER[]   NOT NULL DEFAULT '{}',
	outcome       TEXT        NOT NULL,
	failure_class TEXT        NOT NULL DEFAULT '',
	duration_ms   BIGINT      NOT NULL DEFAULT 0
);`, schema, pgstore.QuoteIdent(pgstore.DeploymentTable)))
	if err != nil {
		return fmt.Errorf("memgw: could not create migration history: %w", err)
	}
	return nil
}

// applied reads the history table.
func (r *Runner) applied(ctx context.Context) (map[int]AppliedMigration, error) {
	ctx, cancel := r.pool.WithQueryTimeout(ctx)
	defer cancel()
	rows, err := r.pool.Pgx().Query(ctx, fmt.Sprintf(
		`SELECT version, name, checksum, applied_at, execution_ms, dirty FROM %s.schema_migrations ORDER BY version`,
		pgstore.QuoteIdent(r.pool.Schema())))
	if err != nil {
		return nil, fmt.Errorf("memgw: read migration history: %w", err)
	}
	defer rows.Close()
	out := map[int]AppliedMigration{}
	for rows.Next() {
		var a AppliedMigration
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum, &a.AppliedAt, &a.ExecutionMS, &a.Dirty); err != nil {
			return nil, fmt.Errorf("memgw: read migration history: %w", err)
		}
		out[a.Version] = a
	}
	return out, rows.Err()
}

// Status reports each known migration and what the database says about it,
// without changing anything. This is the read-only command, and it is read-only
// in the literal sense: a target that has never been migrated is reported as
// having nothing applied, not quietly given a history table so the query works.
func (r *Runner) Status(ctx context.Context) ([]Status, error) {
	set, err := r.Load()
	if err != nil {
		return nil, err
	}
	done, err := r.appliedIfPresent(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(set))
	for _, m := range set {
		s := Status{Version: m.Version, Name: m.Name}
		if a, ok := done[m.Version]; ok {
			s.Applied = true
			s.Dirty = a.Dirty
			s.AppliedAt = a.AppliedAt
			s.Drifted = a.Checksum != m.Checksum
		}
		out = append(out, s)
	}
	return out, nil
}

// Plan is the dry run: the migrations Up would apply, in order, after the same
// validation Up performs. If Plan returns an error, Up would have refused too.
func (r *Runner) Plan(ctx context.Context) ([]Migration, error) {
	set, err := r.Load()
	if err != nil {
		return nil, err
	}
	done, err := r.appliedIfPresent(ctx)
	if err != nil {
		return nil, err
	}
	return validatePlan(set, done)
}

// validatePlan is the whole safety argument of the runner, in one place and
// testable without a database.
func validatePlan(set []Migration, done map[int]AppliedMigration) ([]Migration, error) {
	maxApplied := 0
	for v, a := range done {
		if a.Dirty {
			return nil, fmt.Errorf("%w: version %d (%s) never completed; resolve it by hand before running again",
				ErrDirty, a.Version, a.Name)
		}
		if v > maxApplied {
			maxApplied = v
		}
	}
	var pending []Migration
	for _, m := range set {
		a, ok := done[m.Version]
		if !ok {
			if m.Version < maxApplied {
				return nil, fmt.Errorf("%w: version %d (%s) is new but version %d is already applied",
					ErrOutOfOrder, m.Version, m.Name, maxApplied)
			}
			pending = append(pending, m)
			continue
		}
		if a.Checksum != m.Checksum {
			return nil, fmt.Errorf("%w: version %d (%s); the database reflects a different file than the one on disk",
				ErrChecksumDrift, m.Version, m.Name)
		}
	}
	// A version recorded in the database but absent from the set means someone
	// deleted a migration file. The database cannot be reconciled with a set
	// that no longer describes it.
	known := map[int]bool{}
	for _, m := range set {
		known[m.Version] = true
	}
	for v, a := range done {
		if !known[v] {
			return nil, fmt.Errorf("%w: version %d (%s) is applied but its file is missing",
				ErrChecksumDrift, v, a.Name)
		}
	}
	return pending, nil
}

// Up applies every pending migration.
//
// It is ApplyPlan with no pre-approved plan, which is what a development or
// test run wants: verify the target, take the migration lock, build the plan
// and apply it. Production refuses this form -- there, an apply must name the
// plan somebody read. See production.go.
//
// Each migration is applied in its own transaction together with its history
// row, so a migration and the record that it ran either both exist or neither
// does.
func (r *Runner) Up(ctx context.Context) ([]Migration, error) {
	return r.ApplyPlan(ctx, nil)
}

func (r *Runner) applyInTx(ctx context.Context, schema string, m Migration, start time.Time) error {
	// Migrations get a longer budget than ordinary queries: creating indexes on
	// an empty table is fast, but the budget should not be the reason a
	// migration fails half way.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	tx, err := r.pool.Pgx().Begin(ctx)
	if err != nil {
		return fmt.Errorf("memgw: begin migration %d: %w", m.Version, err)
	}
	// Rollback is unconditional and idempotent after a commit, so an early
	// return can never leave a transaction open holding locks.
	defer func() { _ = tx.Rollback(ctx) }()

	if err := setSearchPath(ctx, tx, schema); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return fmt.Errorf("memgw: migration %d (%s) failed: %w", m.Version, m.Name, err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s.schema_migrations (version, name, checksum, execution_ms, dirty) VALUES ($1,$2,$3,$4,false)`, schema),
		m.Version, m.Name, m.Checksum, time.Since(start).Milliseconds()); err != nil {
		return fmt.Errorf("memgw: record migration %d: %w", m.Version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("memgw: commit migration %d: %w", m.Version, err)
	}
	return nil
}

// applyOutsideTx runs a migration that cannot live in a transaction. The
// history row is written first with dirty=true so that a crash is visible as a
// dirty schema instead of as a silently missing migration.
func (r *Runner) applyOutsideTx(ctx context.Context, schema string, m Migration, start time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if _, err := r.pool.Pgx().Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s.schema_migrations (version, name, checksum, execution_ms, dirty) VALUES ($1,$2,$3,0,true)`, schema),
		m.Version, m.Name, m.Checksum); err != nil {
		return fmt.Errorf("memgw: record migration %d: %w", m.Version, err)
	}
	conn, err := r.pool.Pgx().Acquire(ctx)
	if err != nil {
		return fmt.Errorf("memgw: acquire connection for migration %d: %w", m.Version, err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, fmt.Sprintf("SET search_path TO %s, public", schema)); err != nil {
		return fmt.Errorf("memgw: set search_path: %w", err)
	}
	if _, err := conn.Exec(ctx, m.SQL); err != nil {
		return fmt.Errorf("memgw: migration %d (%s) failed and left the schema dirty: %w", m.Version, m.Name, err)
	}
	if _, err := r.pool.Pgx().Exec(ctx, fmt.Sprintf(
		`UPDATE %s.schema_migrations SET dirty = false, execution_ms = $2 WHERE version = $1`, schema),
		m.Version, time.Since(start).Milliseconds()); err != nil {
		return fmt.Errorf("memgw: clear dirty flag for migration %d: %w", m.Version, err)
	}
	return nil
}

func setSearchPath(ctx context.Context, tx pgx.Tx, schema string) error {
	// SET LOCAL keeps the search_path change inside this transaction, so a
	// pooled connection never leaks it to the next borrower.
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL search_path TO %s, public", schema)); err != nil {
		return fmt.Errorf("memgw: set search_path: %w", err)
	}
	return nil
}
