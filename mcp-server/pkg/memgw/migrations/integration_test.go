// Integration tests live in an external test package: memgwtest migrates a
// throwaway schema and therefore imports this package, which an in-package test
// file cannot do without an import cycle.
package migrations_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/enowdev/enowx-rag/pkg/memgw/memgwtest"
	. "github.com/enowdev/enowx-rag/pkg/memgw/migrations"
	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
)

// --- integration tests -------------------------------------------------------

func TestUpFromEmptyDatabase(t *testing.T) {
	pool := memgwtest.UnmigratedPool(t)
	r := New(pool)
	ctx := t.Context()

	plan, err := r.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) == 0 {
		t.Fatal("an empty schema should have a non-empty plan")
	}

	applied, err := r.Up(ctx)
	if err != nil {
		t.Fatalf("Up from empty: %v", err)
	}
	if len(applied) != len(plan) {
		t.Fatalf("applied %d migrations, planned %d", len(applied), len(plan))
	}

	for _, table := range []string{
		"schema_migrations", "writer_epochs", "principals", "grants", "events",
		"write_receipts", "projection_outbox", "projection_watermark", "tombstones", "legacy_chunk_map",
	} {
		var exists bool
		if err := pool.Pgx().QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM information_schema.tables
			               WHERE table_schema = $1 AND table_name = $2)`,
			pool.Schema(), table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("table %q was not created", table)
		}
	}
}

func TestUpIsIdempotent(t *testing.T) {
	pool := memgwtest.Pool(t) // already migrated once
	ctx := t.Context()
	r := New(pool)

	applied, err := r.Up(ctx)
	if err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("a rerun applied %d migrations; it must apply none", len(applied))
	}
	status, err := r.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range status {
		if !s.Applied || s.Dirty || s.Drifted {
			t.Errorf("version %d: applied=%v dirty=%v drifted=%v", s.Version, s.Applied, s.Dirty, s.Drifted)
		}
	}
}

// TestUpRefusesChecksumDrift edits the file, not the database: this is the
// real-world shape of the accident, where someone "just fixes a typo" in a
// migration that already ran somewhere.
func TestUpRefusesChecksumDrift(t *testing.T) {
	pool := memgwtest.UnmigratedPool(t)
	ctx := t.Context()

	original := fstest.MapFS{"sql/0001_memgw_init.sql": {Data: []byte(`CREATE TABLE drift_probe (id int primary key);`)}}
	if _, err := NewWithSources(pool, original).Up(ctx); err != nil {
		t.Fatal(err)
	}

	edited := fstest.MapFS{"sql/0001_memgw_init.sql": {Data: []byte(`CREATE TABLE drift_probe (id int primary key, extra text);`)}}
	if _, err := NewWithSources(pool, edited).Up(ctx); !errors.Is(err, ErrChecksumDrift) {
		t.Fatalf("want ErrChecksumDrift, got %v", err)
	}

	status, err := NewWithSources(pool, edited).Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status[0].Drifted {
		t.Fatal("Status should report the drift without changing anything")
	}
}

// TestUpRefusesDirtyHistory simulates the crash case by marking the row dirty,
// which is exactly the state applyOutsideTx leaves behind when it dies.
func TestUpRefusesDirtyHistory(t *testing.T) {
	pool := memgwtest.Pool(t)
	ctx := t.Context()
	if _, err := pool.Pgx().Exec(ctx, `UPDATE schema_migrations SET dirty = true WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := New(pool).Up(ctx); !errors.Is(err, ErrDirty) {
		t.Fatalf("want ErrDirty, got %v", err)
	}
}

// TestUpRefusesFailingMigrationAtomically proves the migration and its history
// row share a fate: a failing migration leaves neither its table nor its row.
func TestUpRefusesFailingMigrationAtomically(t *testing.T) {
	pool := memgwtest.UnmigratedPool(t)
	ctx := t.Context()

	broken := fstest.MapFS{
		"sql/0001_ok.sql":     {Data: []byte(`CREATE TABLE half_a (id int primary key);`)},
		"sql/0002_broken.sql": {Data: []byte(`CREATE TABLE half_b (id int primary key); SELECT this_function_does_not_exist();`)},
	}
	applied, err := NewWithSources(pool, broken).Up(ctx)
	if err == nil {
		t.Fatal("a broken migration should fail")
	}
	if len(applied) != 1 {
		t.Fatalf("expected only 0001 to be applied, got %d", len(applied))
	}
	var count int
	if err := pool.Pgx().QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = $1 AND table_name = 'half_b'`, pool.Schema()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("the failing migration left its table behind")
	}
	if err := pool.Pgx().QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version = 2`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("the failing migration left a history row behind")
	}
}

// TestUpRefusesARelabelledTarget is the acceptance gate's "no production
// database" criterion expressed as code: the same pool, relabelled production
// and nothing more, is refused before any DDL is attempted.
//
// Two refusals are checked because they guard different mistakes. Up refuses
// because production never applies a plan nobody reviewed; BuildPlan refuses
// because MEMGW_ENV=production on its own asserts nothing about the target. The
// second is the one that matters: it is what stops a relabel from becoming an
// authorisation.
func TestUpRefusesARelabelledTarget(t *testing.T) {
	dsn := testDSN(t)
	cfg := pgstore.DefaultConfig()
	cfg.DSN = dsn
	cfg.Env = pgstore.EnvProduction

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgstore.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	if _, err := New(pool).Up(ctx); !errors.Is(err, ErrPlanRequired) {
		t.Fatalf("want ErrPlanRequired, got %v", err)
	}
	if _, err := New(pool).BuildPlan(ctx); !errors.Is(err, pgstore.ErrProductionNotSelected) {
		t.Fatalf("want ErrProductionNotSelected, got %v", err)
	}

	// And nothing was created on the way to either refusal.
	var n int
	if err := pool.Pgx().QueryRow(ctx,
		`SELECT count(*) FROM pg_class c JOIN pg_namespace ns ON ns.oid = c.relnamespace
		 WHERE ns.nspname = $1 AND c.relname = 'schema_migrations'`, pool.Schema()).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatal("a refused production run still created the history table")
	}
}

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(memgwtest.DSNEnv))
	if dsn == "" {
		t.Skipf("%s is not set", memgwtest.DSNEnv)
	}
	return dsn
}
