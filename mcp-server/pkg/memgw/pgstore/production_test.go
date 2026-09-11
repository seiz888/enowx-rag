package pgstore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Every refusal exercised here happens before the runner touches the network,
// which is the point: a target that was never asserted is refused without a
// connection being made to it at all. The checks that do need a server -- role,
// primary, ownership, foreign objects, deployment identity -- are proved against
// the disposable target instead.

func prodPool(mutate func(*Config)) *Pool {
	cfg := Config{
		DSN:    "postgres://user:secret@db.example:5432/ledger_alpha?sslmode=verify-full",
		Schema: "memgw",
		Env:    EnvProduction,
		Production: &ProductionTarget{
			Database:        "ledger_alpha",
			Schema:          "memgw",
			Role:            "memgw_migrator",
			Deployment:      "alpha-1",
			Allow:           []Purpose{PurposeSchemaMigration},
			ExpectedObjects: []string{"events"},
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return &Pool{cfg: cfg}
}

func TestProductionIsNeverTheDefault(t *testing.T) {
	cases := map[string]func(*Config){
		"the environment is not production": func(c *Config) { c.Env = EnvDevelopment },
		"no assertion at all":               func(c *Config) { c.Production = nil },
		"no database":                       func(c *Config) { c.Production.Database = "" },
		"no schema":                         func(c *Config) { c.Production.Schema = "" },
		"no role":                           func(c *Config) { c.Production.Role = "" },
		"no deployment":                     func(c *Config) { c.Production.Deployment = "" },
		"a blank deployment":                func(c *Config) { c.Production.Deployment = "   " },
		// Declaring nothing is not the same as declaring an empty set: a caller
		// that cannot say what it owns cannot judge what is foreign.
		"no declared objects": func(c *Config) { c.Production.ExpectedObjects = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := VerifyProduction(context.Background(), prodPool(mutate), PurposeSchemaMigration)
			if !errors.Is(err, ErrProductionNotSelected) {
				t.Fatalf("got %v, want ErrProductionNotSelected", err)
			}
		})
	}
}

func TestApprovalDoesNotGeneraliseAcrossPurposes(t *testing.T) {
	// Approving a schema migration must not approve rewriting history.
	p := prodPool(nil)
	for _, purpose := range []Purpose{PurposeHistoryImport, PurposeServe} {
		if _, err := VerifyProduction(context.Background(), p, purpose); !errors.Is(err, ErrPurposeNotAllowed) {
			t.Fatalf("%s: got %v, want ErrPurposeNotAllowed", purpose, err)
		}
	}
	if !p.cfg.Production.Allows(PurposeSchemaMigration) {
		t.Fatal("the purpose that was allowed is not allowed")
	}
	var nilTarget *ProductionTarget
	if nilTarget.Allows(PurposeSchemaMigration) {
		t.Fatal("a nil assertion authorised something")
	}
}

func TestAssertedSchemaMustBeTheSchemaBeingWritten(t *testing.T) {
	// Otherwise the schema that is checked and the schema that is changed are
	// two different schemas.
	_, err := VerifyProduction(context.Background(),
		prodPool(func(c *Config) { c.Production.Schema = "other" }), PurposeSchemaMigration)
	if !errors.Is(err, ErrTargetMismatch) {
		t.Fatalf("got %v, want ErrTargetMismatch", err)
	}
}

func TestTransportRefusesAnythingUnverified(t *testing.T) {
	for _, dsn := range []string{
		"postgres://user:secret@db.example:5432/ledger_alpha",
		"postgres://user:secret@db.example:5432/ledger_alpha?sslmode=disable",
		"postgres://user:secret@db.example:5432/ledger_alpha?sslmode=allow",
		"postgres://user:secret@db.example:5432/ledger_alpha?sslmode=prefer",
		// require encrypts and verifies nothing, so it proves the peer is
		// speaking TLS and not that it is the peer we meant.
		"postgres://user:secret@db.example:5432/ledger_alpha?sslmode=require",
		"host=db.example user=u password=secret sslmode=require",
	} {
		_, err := verifyTransport(context.Background(), prodPool(func(c *Config) { c.DSN = dsn }))
		if !errors.Is(err, ErrInsecureTransport) {
			t.Fatalf("%s: got %v, want ErrInsecureTransport", dsn, err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("the refusal printed the password: %v", err)
		}
	}
}

func TestTransportAcceptsALocalSocketWithoutAskingTheServer(t *testing.T) {
	// A unix socket has no network to intercept, and this path must not need a
	// live connection to say so.
	for _, dsn := range []string{
		"postgres:///ledger_alpha?host=/var/run/postgresql",
		"host=/var/run/postgresql dbname=ledger_alpha",
		"host=@memgw dbname=ledger_alpha",
		"dbname=ledger_alpha",
	} {
		got, err := verifyTransport(context.Background(), prodPool(func(c *Config) { c.DSN = dsn }))
		if err != nil {
			t.Fatalf("%s: %v", dsn, err)
		}
		if got != "unix-socket" {
			t.Fatalf("%s: transport %q", dsn, got)
		}
	}
}

func TestDsnParamNeverReturnsTheCredential(t *testing.T) {
	for _, dsn := range []string{
		"postgres://user:secret@db.example:5432/ledger_alpha?sslmode=verify-full",
		"host=db.example user=user password=secret sslmode=verify-full",
	} {
		if got := dsnParam(dsn, "sslmode"); got != "verify-full" {
			t.Fatalf("%s: sslmode=%q", dsn, got)
		}
		for _, key := range []string{"password", "user"} {
			if v := dsnParam(dsn, key); strings.Contains(v, "secret") {
				t.Fatalf("%s: %s leaked %q", dsn, key, v)
			}
		}
	}
}

func TestDeclareManagedObjectsRefusesToInventAnEmptySet(t *testing.T) {
	p := prodPool(func(c *Config) { c.Production.ExpectedObjects = nil })
	p.DeclareManagedObjects(nil)
	if p.cfg.Production.ExpectedObjects != nil {
		t.Fatal("declaring nothing was recorded as declaring an empty set")
	}
	p.DeclareManagedObjects([]string{"events", "receipts"})
	if len(p.cfg.Production.ExpectedObjects) != 2 {
		t.Fatalf("declared objects not recorded: %v", p.cfg.Production.ExpectedObjects)
	}
	// A pool with no production assertion must not panic when a caller declares
	// what it manages; every write path does this unconditionally.
	(&Pool{cfg: Config{Env: EnvTest}}).DeclareManagedObjects([]string{"events"})
}

func TestVerifyTargetRoutesNonProductionToTheUnchangedCheck(t *testing.T) {
	// Development and test must reach VerifyNonProduction, not the assertion
	// path. With a nil underlying pool that shows up as a panic-free error from
	// the old code rather than an ErrProductionNotSelected from the new one.
	for _, env := range []Environment{EnvDevelopment, EnvTest} {
		p := &Pool{cfg: Config{Env: env, Schema: "memgw", DSN: "postgres://u@127.0.0.1:5432/memgw_test"}}
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = errors.New("reached the server")
				}
			}()
			return VerifyTarget(context.Background(), p, PurposeSchemaMigration)
		}()
		if errors.Is(err, ErrProductionNotSelected) || errors.Is(err, ErrPurposeNotAllowed) {
			t.Fatalf("%s was routed through the production path: %v", env, err)
		}
	}
}

func TestIsProductionReadsTheEnvironmentOnly(t *testing.T) {
	// It must not be confused with "was verified": a pool declared production
	// and never checked still reports true, so callers gate on it and then
	// verify rather than treating it as a result.
	if !prodPool(nil).IsProduction() {
		t.Fatal("a production pool did not report itself as one")
	}
	if prodPool(nil).ProductionFacts() != nil {
		t.Fatal("facts existed before any verification ran")
	}
	if (&Pool{cfg: Config{Env: EnvTest}}).IsProduction() {
		t.Fatal("a test pool reported itself as production")
	}
}

func TestRedactedTargetCarriesNoPassword(t *testing.T) {
	got := prodPool(nil).RedactedTarget()
	if strings.Contains(got, "secret") {
		t.Fatalf("redaction left the password in: %s", got)
	}
	if !strings.Contains(got, "ledger_alpha") {
		t.Fatalf("redaction removed the part an operator needs: %s", got)
	}
}
