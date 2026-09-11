package migrations

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
)

// --- database-free tests -----------------------------------------------------
//
// validatePlan holds the runner's entire safety argument, so it is tested
// directly. These run everywhere, with or without a disposable database.

func mig(v int, name, body string) Migration {
	set, err := NewWithSources(nil, fstest.MapFS{
		fmt.Sprintf("sql/%04d_%s.sql", v, name): {Data: []byte(body)},
	}).Load()
	if err != nil {
		panic(err)
	}
	return set[0]
}

func TestLoadRejectsMalformedNames(t *testing.T) {
	_, err := NewWithSources(nil, fstest.MapFS{"sql/init.sql": {Data: []byte("SELECT 1")}}).Load()
	if err == nil || !strings.Contains(err.Error(), "NNNN_name.sql") {
		t.Fatalf("want a naming error, got %v", err)
	}
}

func TestLoadOrdersByVersion(t *testing.T) {
	set, err := NewWithSources(nil, fstest.MapFS{
		"sql/0002_second.sql": {Data: []byte("SELECT 2")},
		"sql/0001_first.sql":  {Data: []byte("SELECT 1")},
	}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 2 || set[0].Version != 1 || set[1].Version != 2 {
		t.Fatalf("migrations are not ordered: %+v", set)
	}
	if set[0].Checksum == set[1].Checksum {
		t.Fatal("different files must have different checksums")
	}
	if !set[0].InTx {
		t.Fatal("a migration without the marker must run in a transaction")
	}
}

func TestLoadHonoursNoTransactionMarker(t *testing.T) {
	set, err := NewWithSources(nil, fstest.MapFS{
		"sql/0001_concurrent.sql": {Data: []byte(noTransactionMarker + "\nCREATE INDEX CONCURRENTLY x ON y (z);")},
	}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if set[0].InTx {
		t.Fatal("the marker must take the migration out of the transaction")
	}
}

func TestValidatePlanRefusesDirtySchema(t *testing.T) {
	m := mig(1, "init", "SELECT 1")
	_, err := validatePlan([]Migration{m}, map[int]AppliedMigration{
		1: {Version: 1, Name: "init", Checksum: m.Checksum, Dirty: true},
	})
	if !errors.Is(err, ErrDirty) {
		t.Fatalf("want ErrDirty, got %v", err)
	}
}

func TestValidatePlanRefusesChecksumDrift(t *testing.T) {
	m := mig(1, "init", "SELECT 1")
	_, err := validatePlan([]Migration{m}, map[int]AppliedMigration{
		1: {Version: 1, Name: "init", Checksum: "deadbeef"},
	})
	if !errors.Is(err, ErrChecksumDrift) {
		t.Fatalf("want ErrChecksumDrift, got %v", err)
	}
}

func TestValidatePlanRefusesDeletedMigration(t *testing.T) {
	_, err := validatePlan(nil, map[int]AppliedMigration{
		1: {Version: 1, Name: "init", Checksum: "abc"},
	})
	if !errors.Is(err, ErrChecksumDrift) {
		t.Fatalf("want ErrChecksumDrift for a missing file, got %v", err)
	}
}

func TestValidatePlanRefusesOutOfOrderInsertion(t *testing.T) {
	first := mig(1, "init", "SELECT 1")
	inserted := mig(2, "sneaked_in", "SELECT 2")
	third := mig(3, "later", "SELECT 3")
	_, err := validatePlan([]Migration{first, inserted, third}, map[int]AppliedMigration{
		1: {Version: 1, Name: "init", Checksum: first.Checksum},
		3: {Version: 3, Name: "later", Checksum: third.Checksum},
	})
	if !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("want ErrOutOfOrder, got %v", err)
	}
}

func TestValidatePlanReturnsPendingInOrder(t *testing.T) {
	first := mig(1, "init", "SELECT 1")
	second := mig(2, "more", "SELECT 2")
	pending, err := validatePlan([]Migration{first, second}, map[int]AppliedMigration{
		1: {Version: 1, Name: "init", Checksum: first.Checksum},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Version != 2 {
		t.Fatalf("unexpected plan: %+v", pending)
	}
}

// TestBuiltinSetLoads keeps the shipped migrations honest even with no
// database: a malformed file name or a duplicate version fails here.
func TestBuiltinSetLoads(t *testing.T) {
	set, err := New(nil).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(set) == 0 {
		t.Fatal("no migrations are embedded")
	}
	if set[0].Version != 1 || set[0].Name != "memgw_init" {
		t.Fatalf("first migration is %d_%s", set[0].Version, set[0].Name)
	}
	// The first migration must not create the deferred entities. Naming them
	// here is what makes "deferred" a checked claim rather than a comment.
	for _, deferred := range []string{
		"CREATE TABLE projects", "CREATE TABLE workspaces", "CREATE TABLE works",
		"CREATE TABLE sessions", "CREATE TABLE checkpoints", "CREATE TABLE facts",
	} {
		if strings.Contains(set[0].SQL, deferred) {
			t.Errorf("migration 0001 contains %q, which Phase 3 defers", deferred)
		}
	}
}
