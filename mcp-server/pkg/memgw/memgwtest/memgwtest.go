// Package memgwtest wires integration tests to a disposable PostgreSQL.
//
// Tests skip when MEMGW_TEST_DSN is unset, so `go test ./...` stays green on a
// machine with no database and needs no credential, no network, no Qdrant and
// no embedding provider. When a DSN is present, every test gets its own schema
// inside that database, migrated from empty, and dropped afterwards -- which is
// also how the "migrate from an empty database" acceptance criterion is
// exercised on every single run rather than once by hand.
package memgwtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/migrations"
	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
)

// The event log carries real foreign keys to the registry, so a test whose
// subject is not the registry still has to have one. These helpers create the
// minimum rows an event can point at. They are deliberately not a fixture
// factory: each one does exactly what a host does once at install time, so a
// test never gets state it did not ask for.

// SeedProject registers a project. The slug is the id, which is unique by
// construction and satisfies the column's shape.
func SeedProject(t *testing.T, p *pgstore.Pool, projectID uuid.UUID) {
	t.Helper()
	if projectID == uuid.Nil {
		return
	}
	if _, err := p.Pgx().Exec(context.Background(), `
		INSERT INTO projects (project_id, slug, display_name)
		VALUES ($1, $2, 'test project') ON CONFLICT (project_id) DO NOTHING`,
		projectID, projectID.String()); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

// SeedWorkspace registers a workspace in a project. The root is a digest of the
// workspace id: the test has no checkout, and the column takes no path anyway.
func SeedWorkspace(t *testing.T, p *pgstore.Pool, projectID, workspaceID uuid.UUID) {
	t.Helper()
	if projectID == uuid.Nil || workspaceID == uuid.Nil {
		return
	}
	SeedProject(t, p, projectID)
	sum := sha256.Sum256([]byte(workspaceID.String()))
	if _, err := p.Pgx().Exec(context.Background(), `
		INSERT INTO workspaces (workspace_id, project_id, host_id, root_path_digest, label)
		VALUES ($1,$2,$3,$4,'test') ON CONFLICT (workspace_id) DO NOTHING`,
		workspaceID, projectID, uuid.New(), hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
}

// SeedSession registers a session. The principal must already exist.
func SeedSession(t *testing.T, p *pgstore.Pool, sessionID string, projectID, workspaceID, principalID uuid.UUID) {
	t.Helper()
	if sessionID == "" {
		return
	}
	SeedWorkspace(t, p, projectID, workspaceID)
	if _, err := p.Pgx().Exec(context.Background(), `
		INSERT INTO sessions (session_id, project_id, workspace_id, principal_id, host_id)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT (session_id) DO NOTHING`,
		sessionID, projectID, workspaceID, principalID, uuid.New()); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

// SeedBranch registers the initial branch of a session.
func SeedBranch(t *testing.T, p *pgstore.Pool, branchID, projectID uuid.UUID, sessionID string) {
	t.Helper()
	if branchID == uuid.Nil {
		return
	}
	if _, err := p.Pgx().Exec(context.Background(), `
		INSERT INTO branches (branch_id, project_id, session_id, reason_class)
		VALUES ($1,$2,$3,'initial') ON CONFLICT (branch_id) DO NOTHING`,
		branchID, projectID, sessionID); err != nil {
		t.Fatalf("seed branch: %v", err)
	}
}

// DSNEnv names the environment variable holding the disposable test database.
const DSNEnv = "MEMGW_TEST_DSN"

// schemaPrefix marks schemas this package created. Cleanup refuses to drop
// anything without it, so a misconfigured DSN cannot turn a test run into a
// deletion of somebody's schema.
const schemaPrefix = "memgw_t_"

// Pool returns a migrated, isolated schema for one test, or skips the test when
// no disposable database is configured.
func Pool(t *testing.T) *pgstore.Pool {
	t.Helper()
	return pool(t, true)
}

// UnmigratedPool returns an isolated empty schema with no migrations applied.
// Migration tests use it; everything else wants Pool.
func UnmigratedPool(t *testing.T) *pgstore.Pool {
	t.Helper()
	return pool(t, false)
}

func pool(t *testing.T, migrate bool) *pgstore.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(DSNEnv))
	if dsn == "" {
		t.Skipf("%s is not set; skipping the integration test (see docs/architecture/shared-memory-gateway.md)", DSNEnv)
	}

	schema := schemaPrefix + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	cfg := pgstore.DefaultConfig()
	cfg.DSN = dsn
	cfg.Schema = schema
	cfg.Env = pgstore.EnvTest
	// Other tests running in parallel own the sibling schemas; they are not
	// evidence that this database belongs to somebody else.
	cfg.SiblingSchemaPrefix = schemaPrefix

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p, err := pgstore.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	// Every test re-proves the target is disposable. The check is cheap and the
	// alternative is a suite that would happily run against production if the
	// DSN were ever edited.
	if err := pgstore.VerifyNonProduction(ctx, p); err != nil {
		p.Close()
		t.Fatalf("test target refused: %v", err)
	}

	t.Cleanup(func() {
		dropSchema(t, p, schema)
		p.Close()
	})

	if migrate {
		if _, err := migrations.New(p).Up(ctx); err != nil {
			t.Fatalf("migrate test schema: %v", err)
		}
	}
	return p
}

func dropSchema(t *testing.T, p *pgstore.Pool, schema string) {
	t.Helper()
	if !strings.HasPrefix(schema, schemaPrefix) {
		t.Fatalf("refusing to drop schema %q: not a test schema", schema)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := p.Pgx().Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, schema)); err != nil {
		t.Logf("cleanup: drop schema %s: %v", schema, err)
	}
}
