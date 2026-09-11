package migrations

import (
	"strings"
	"testing"
	"time"

	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
)

// The database-backed proofs for this path live in the disposable-target
// exercise; what is checked here is the part that decides *whether* an apply may
// proceed, which is pure and therefore testable without a server.

func plan(t *testing.T, mutate func(*MigrationPlan)) *MigrationPlan {
	t.Helper()
	p := &MigrationPlan{
		Purpose: string(pgstore.PurposeSchemaMigration),
		Target: &pgstore.ProductionFacts{
			Database: "ledger_alpha", Schema: "memgw",
			Role: "memgw_migrator", Deployment: "alpha-1",
		},
		Applied:        []PlanMigration{{Version: 1, Name: "init", Checksum: "aaaa", InTx: true}},
		Pending:        []PlanMigration{{Version: 2, Name: "next", Checksum: "bbbb", InTx: true}},
		ManagedObjects: []string{"events", "schema_migrations"},
		GeneratedAt:    time.Now(),
	}
	if mutate != nil {
		mutate(p)
	}
	d, err := p.computeDigest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	p.Digest = d
	return p
}

func TestDigestIgnoresWhenItWasBuilt(t *testing.T) {
	// Re-planning an unchanged target must produce the same digest, or the
	// digest would only ever prove that time had passed.
	a := plan(t, nil)
	b := plan(t, func(p *MigrationPlan) { p.GeneratedAt = a.GeneratedAt.Add(9 * time.Hour) })
	if a.Digest != b.Digest {
		t.Fatalf("digest moved with the clock alone: %s vs %s", a.Digest, b.Digest)
	}
}

func TestDigestCoversEveryPartOfTheDecision(t *testing.T) {
	base := plan(t, nil)
	cases := map[string]func(*MigrationPlan){
		"a different database": func(p *MigrationPlan) { p.Target.Database = "ledger_beta" },
		"a different schema":   func(p *MigrationPlan) { p.Target.Schema = "other" },
		"a different role":     func(p *MigrationPlan) { p.Target.Role = "postgres" },
		// The same names on a restored copy is exactly what deployment identity
		// exists to separate.
		"a different deployment": func(p *MigrationPlan) { p.Target.Deployment = "alpha-2" },
		"a different purpose":    func(p *MigrationPlan) { p.Purpose = string(pgstore.PurposeHistoryImport) },
		// History drift: a plan reviewed at version 1 must not survive a fourth
		// migration landing, even when the pending list still looks the same.
		"another migration landed": func(p *MigrationPlan) {
			p.Applied = append(p.Applied, PlanMigration{Version: 3, Name: "other", Checksum: "cccc", InTx: true})
		},
		"an edited migration file": func(p *MigrationPlan) { p.Pending[0].Checksum = "dddd" },
		"an extra pending migration": func(p *MigrationPlan) {
			p.Pending = append(p.Pending, PlanMigration{Version: 3, Name: "more", Checksum: "eeee", InTx: true})
		},
		"a changed managed set": func(p *MigrationPlan) { p.ManagedObjects = append(p.ManagedObjects, "extra") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if got := plan(t, mutate); got.Digest == base.Digest {
				t.Fatalf("%s did not change the digest", name)
			}
		})
	}
}

func TestManagedObjectsCoversTheRunnersOwnTables(t *testing.T) {
	// The runner creates these outside any migration file, so a set derived only
	// from the SQL would report them as foreign objects in its own schema.
	got, err := New(nil).ManagedObjects()
	if err != nil {
		t.Fatalf("managed objects: %v", err)
	}
	in := map[string]bool{}
	for _, o := range got {
		in[o] = true
	}
	for _, want := range []string{"schema_migrations", pgstore.DeploymentTable, "migration_audit"} {
		if !in[want] {
			t.Fatalf("%q is missing from the managed set %v", want, got)
		}
	}
	// And the set must actually be read from the embedded SQL, not just be the
	// three tables above.
	if len(got) <= len(runnerOwnedTables) {
		t.Fatalf("the managed set holds nothing from the migration files: %v", got)
	}
	for _, o := range got {
		if o != strings.ToLower(o) || strings.Contains(o, ".") || strings.Contains(o, `"`) {
			t.Fatalf("managed object %q is not a bare lower-case relation name", o)
		}
	}
}

func TestManagedObjectsReadsCreateStatements(t *testing.T) {
	got, err := New(nil).ManagedObjects()
	if err != nil {
		t.Fatalf("managed objects: %v", err)
	}
	in := map[string]bool{}
	for _, o := range got {
		in[o] = true
	}
	// legacy_chunk_map is created by the migration set and written by the
	// history importer; if the extractor stopped seeing it, an import would
	// refuse against a schema it legitimately owns.
	if !in["legacy_chunk_map"] {
		t.Fatalf("legacy_chunk_map is missing from %v", got)
	}
}

func TestStripSQLCommentsCannotHideAStatement(t *testing.T) {
	sql := "-- DROP TABLE events;\nALTER TABLE t /* DROP COLUMN x */ ADD COLUMN y int;\n"
	stripped := stripSQLComments(sql)
	if strings.Contains(strings.ToUpper(stripped), "DROP") {
		t.Fatalf("a commented DROP survived stripping: %q", stripped)
	}
	if !strings.Contains(stripped, "ADD COLUMN y") {
		t.Fatalf("stripping ate real SQL: %q", stripped)
	}
}

func TestRiskyPatternsSeeWhatTheyClaimTo(t *testing.T) {
	cases := map[string]string{
		"DROP TABLE events;":                                   "drops a table",
		"ALTER TABLE events DROP COLUMN body;":                 "drops a column",
		"TRUNCATE events;":                                     "truncates a table",
		"DELETE FROM events WHERE seq < 10;":                   "deletes rows",
		"UPDATE events SET body = '{}';":                       "updates rows",
		"ALTER TABLE events RENAME TO old_events;":             "renames an object",
		"ALTER TABLE events ALTER COLUMN seq TYPE bigint;":     "changes a column type",
		"ALTER TABLE events ALTER COLUMN seq SET NOT NULL;":    "adds a NOT NULL constraint, which scans the table",
		"ALTER TABLE events ADD CONSTRAINT c CHECK (seq > 0);": "adds a validated constraint, which scans the table",
		"CREATE INDEX events_seq_idx ON events (seq);":         "builds an index without CONCURRENTLY, which locks writes",
	}
	for sql, want := range cases {
		t.Run(want, func(t *testing.T) {
			found := false
			for _, rp := range riskyPatterns {
				if rp.what == want && rp.matches(sql) {
					found = true
				}
			}
			if !found {
				t.Fatalf("%q was not reported as %q", sql, want)
			}
		})
	}
}

func TestRiskyPatternsLeaveSafeStatementsAlone(t *testing.T) {
	safe := []string{
		"CREATE TABLE events (seq bigint);",
		"CREATE INDEX CONCURRENTLY events_seq_idx ON events (seq);",
		"ALTER TABLE events ADD CONSTRAINT c CHECK (seq > 0) NOT VALID;",
		"ALTER TABLE events ADD COLUMN body jsonb;",
	}
	for _, sql := range safe {
		for _, rp := range riskyPatterns {
			if rp.matches(sql) {
				t.Fatalf("%q was wrongly reported: %s", sql, rp.what)
			}
		}
	}
}

func TestAdvisoryKeyIsPerSchema(t *testing.T) {
	if advisoryKey("memgw") == advisoryKey("memgw_other") {
		t.Fatal("two schemas share a migration lock")
	}
	if advisoryKey("memgw") != advisoryKey("memgw") {
		t.Fatal("the lock key is not stable")
	}
}

func TestClassifyMigrationErrorNeverLeaksTheMessage(t *testing.T) {
	// A driver error can carry the DSN, and the audit table is the last place a
	// credential should come to rest.
	err := &dsnCarryingError{}
	if got := classifyMigrationError(err); got != "migration_failed" {
		t.Fatalf("unclassified error became %q", got)
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{ErrPlanStale, "plan_stale"},
		{ErrLockHeld, "lock_held"},
		{pgstore.ErrTargetMismatch, "target_mismatch"},
		{pgstore.ErrForeignObject, "foreign_object"},
		{pgstore.ErrInsecureTransport, "insecure_transport"},
		{pgstore.ErrProductionNotSelected, "not_selected"},
		{pgstore.ErrPurposeNotAllowed, "purpose_not_allowed"},
	} {
		if got := classifyMigrationError(c.err); got != c.want {
			t.Fatalf("%v classified as %q, want %q", c.err, got, c.want)
		}
	}
}

type dsnCarryingError struct{}

func (e *dsnCarryingError) Error() string {
	return "dial postgres://memgw:hunter2@10.0.0.5:5432/ledger: refused"
}
