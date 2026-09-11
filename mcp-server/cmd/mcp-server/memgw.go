package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/enowdev/enowx-rag/pkg/memgw/gateway"
	"github.com/enowdev/enowx-rag/pkg/memgw/migrations"
	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
)

// runMemgw is the migration command line for the shared memory gateway.
//
// It is a separate subcommand rather than something the server does at startup.
// A server that migrates on boot decides to change a schema at the moment
// nobody is watching, and the whole point of this runner is that a schema
// change is a deliberate act with a plan you can read first.
//
// Configuration comes from the environment because a DSN on the command line
// ends up in shell history:
//
//	MEMGW_DSN     connection string for the target database
//	MEMGW_SCHEMA  schema to own (default "memgw")
//	MEMGW_ENV     "development", "test" or "production"
//
// Production is never selected by MEMGW_ENV alone: it additionally requires an
// explicit assertion of the target that the live connection must match, and a
// purpose named in MEMGW_PRODUCTION_ALLOW. See the usage text below.
func runMemgw(args []string) {
	fs := flag.NewFlagSet("memgw", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: enowx-rag memgw <command>

  status   show each migration and what the database says about it
  plan     dry run: list the migrations "up" would apply, and refuse for the same
           reasons; --out FILE writes the reviewable plan, digest included
  up       apply pending migrations; --plan FILE applies exactly the plan in
           that file and refuses if the target or the history has moved since
  principal  create principals, grant scopes, issue and revoke credentials
  collector  run the local durable collector (Windows, Linux) or print its stats
  projection drain the projection outbox into the vector store, or report its state
  serve      serve the gateway's HTTP routes alone, without the rest of the service
  adapter    translate one agent host's lifecycle hook into a canonical event
  checkpoint submit a handover a capture turn authored, or decide whether one
             is warranted (never fabricates content)
  work       complete or abandon the current unit of work, or report it
  graphify   rebuild the repository code index, or report whether it is still current
  history    plan, apply or replay over the pre-gateway chunk map (a dry run
             until "apply")
  shadow     run a frozen evaluation dataset; it observes and gates nothing

Environment: MEMGW_DSN (required), MEMGW_SCHEMA (default "memgw"),
             MEMGW_ENV (development|test|production).

Production is not selected by MEMGW_ENV alone. It additionally requires an
explicit assertion of the target, and refuses if the live connection disagrees
with any part of it:

  MEMGW_PRODUCTION_DATABASE    must equal current_database()
  MEMGW_PRODUCTION_SCHEMA      must equal MEMGW_SCHEMA
  MEMGW_PRODUCTION_ROLE        must equal current_user
  MEMGW_PRODUCTION_DEPLOYMENT  identity recorded in the schema on first apply
  MEMGW_PRODUCTION_ALLOW       comma-separated: schema_migration,history_import,serve

Approving a schema migration does not approve a history import; each is named
separately in MEMGW_PRODUCTION_ALLOW. The connection must be a local unix
socket or TLS with sslmode=verify-ca or verify-full.
`)
	}
	_ = fs.Parse(args)

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(2)
	}

	// principal has its own flag set and its own lifetime; it is dispatched
	// before the migration pool is opened so the two do not share a timeout
	// sized for a schema change.
	if fs.Arg(0) == "principal" {
		runMemgwPrincipal(fs.Args()[1:])
		return
	}
	// The collector never touches PostgreSQL: it speaks to the gateway over
	// HTTP with its own credential, so it must not open the migration pool.
	if fs.Arg(0) == "collector" {
		runMemgwCollector(fs.Args()[1:])
		return
	}
	// The projection worker opens its own pool and runs until interrupted, so
	// it must not inherit the five-minute deadline a migration run wants.
	if fs.Arg(0) == "projection" {
		runMemgwProjection(fs.Args()[1:])
		return
	}
	// The adapter is invoked by a host's hook, once per lifecycle moment. It
	// never opens a pool at all: it reaches the ledger through the collector or
	// the gateway, exactly as any other client would.
	if fs.Arg(0) == "adapter" {
		// A capture-turn child inherits MEMGW_CAPTURE_TURN_CHILD and its own
		// hooks must not run: an adapter hook firing inside the child would
		// write a spurious session event, and a checkpoint hook would recurse.
		// This is the recursion guard, placed before any work is done.
		if os.Getenv(captureTurnRecursionGuardEnv) != "" {
			return
		}
		runMemgwAdapter(fs.Args()[1:])
		return
	}
	// The checkpoint writer submits a handover a capture turn authored. It
	// reaches the ledger through the same collector/gateway path as the
	// adapter and never opens a pool.
	if fs.Arg(0) == "checkpoint" {
		if os.Getenv(captureTurnRecursionGuardEnv) != "" {
			return
		}
		runMemgwCheckpoint(fs.Args()[1:])
		return
	}
	// The work command completes or abandons the current unit of work, or
	// reports which one the cwd resolves to. It reaches the gateway through the
	// same adapter path and never opens a pool.
	if fs.Arg(0) == "work" {
		if os.Getenv(captureTurnRecursionGuardEnv) != "" {
			return
		}
		runMemgwWork(fs.Args()[1:])
		return
	}
	// The code index is derived from the working tree, not from the ledger, so
	// it needs no database connection either.
	if fs.Arg(0) == "graphify" {
		runMemgwGraphify(fs.Args()[1:])
		return
	}
	// serve runs the gateway's HTTP routes and nothing else. It opens its own
	// pool through the same path the full service uses, so it must not inherit
	// the migration deadline either.
	if fs.Arg(0) == "serve" {
		runMemgwServe(fs.Args()[1:])
		return
	}
	// history plans against a file and only opens a pool when it is asked to
	// read the existing mapping or to apply one, so it manages its own.
	if fs.Arg(0) == "history" {
		runMemgwHistory(fs.Args()[1:])
		return
	}
	// Shadow evaluation reads a frozen file and writes a report. It touches
	// neither the ledger nor the vector store.
	if fs.Arg(0) == "shadow" {
		runMemgwShadow(fs.Args()[1:])
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := openMemgwPool(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memgw: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	runner := migrations.New(pool)
	switch cmd := fs.Arg(0); cmd {
	case "status":
		err = memgwStatus(ctx, runner, pool)
	case "plan":
		err = memgwPlan(ctx, runner, pool, fs.Args()[1:])
	case "up":
		err = memgwUp(ctx, runner, pool, fs.Args()[1:])
	default:
		fmt.Fprintf(os.Stderr, "memgw: unknown command %q\n", cmd)
		fs.Usage()
		os.Exit(2)
	}
	if err != nil {
		// The target check is separated out because it is the failure an
		// operator is most likely to hit and most needs to understand.
		// Every refusal that means "this is not the target you said it was"
		// shares an exit code, so a wrapper can tell a refusal from a crash
		// without parsing English.
		switch {
		case errors.Is(err, pgstore.ErrUnsafeTarget),
			errors.Is(err, pgstore.ErrTargetMismatch),
			errors.Is(err, pgstore.ErrForeignObject),
			errors.Is(err, pgstore.ErrInsecureTransport),
			errors.Is(err, pgstore.ErrProductionNotSelected),
			errors.Is(err, pgstore.ErrPurposeNotAllowed),
			errors.Is(err, migrations.ErrPlanRequired),
			errors.Is(err, migrations.ErrPlanStale),
			errors.Is(err, migrations.ErrLockHeld),
			errors.Is(err, migrations.ErrChecksumDrift),
			errors.Is(err, migrations.ErrDirty),
			errors.Is(err, migrations.ErrOutOfOrder):
			fmt.Fprintf(os.Stderr, "memgw: refusing: %s\n", withoutOwnPrefix(err))
			os.Exit(3)
		}
		fmt.Fprintf(os.Stderr, "memgw: %s\n", withoutOwnPrefix(err))
		os.Exit(1)
	}
}

// withoutOwnPrefix drops the package's own "memgw: " from an error that already
// carries it, so a refusal does not read "memgw: memgw: ...".
func withoutOwnPrefix(err error) string {
	return strings.TrimPrefix(err.Error(), "memgw: ")
}

func openMemgwPool(ctx context.Context) (*pgstore.Pool, error) {
	dsn := strings.TrimSpace(os.Getenv("MEMGW_DSN"))
	if dsn == "" {
		return nil, errors.New("MEMGW_DSN is not set; the runner never guesses a target")
	}
	cfg := pgstore.DefaultConfig()
	cfg.DSN = dsn
	if schema := strings.TrimSpace(os.Getenv("MEMGW_SCHEMA")); schema != "" {
		cfg.Schema = schema
	}
	cfg.Env = pgstore.Environment(strings.TrimSpace(os.Getenv("MEMGW_ENV")))
	if cfg.Env == "" {
		return nil, errors.New("MEMGW_ENV is not set; declare \"development\", \"test\" or \"production\"")
	}
	if cfg.Env == pgstore.EnvProduction {
		target, err := productionTargetFromEnv(cfg.Schema)
		if err != nil {
			return nil, err
		}
		cfg.Production = target
	}
	return pgstore.Open(ctx, cfg)
}

// productionTargetFromEnv builds the operator's assertion about a production
// target. Every field is required and none is defaulted: the whole value of the
// assertion is that somebody typed it deliberately, and a default would be the
// runner guessing on their behalf.
//
// Nothing here is a secret. Database, schema, role and deployment identity are
// names; the credential stays in the DSN, which is never printed.
func productionTargetFromEnv(schema string) (*pgstore.ProductionTarget, error) {
	get := func(k string) string { return strings.TrimSpace(os.Getenv(k)) }
	t := &pgstore.ProductionTarget{
		Database:   get("MEMGW_PRODUCTION_DATABASE"),
		Schema:     get("MEMGW_PRODUCTION_SCHEMA"),
		Role:       get("MEMGW_PRODUCTION_ROLE"),
		Deployment: get("MEMGW_PRODUCTION_DEPLOYMENT"),
	}
	missing := []string{}
	for _, f := range []struct{ env, val string }{
		{"MEMGW_PRODUCTION_DATABASE", t.Database},
		{"MEMGW_PRODUCTION_SCHEMA", t.Schema},
		{"MEMGW_PRODUCTION_ROLE", t.Role},
		{"MEMGW_PRODUCTION_DEPLOYMENT", t.Deployment},
	} {
		if f.val == "" {
			missing = append(missing, f.env)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("MEMGW_ENV=production selects nothing on its own; %s must also be set",
			strings.Join(missing, ", "))
	}
	if t.Schema != schema {
		return nil, fmt.Errorf("MEMGW_PRODUCTION_SCHEMA=%q disagrees with MEMGW_SCHEMA=%q", t.Schema, schema)
	}
	for _, raw := range strings.Split(get("MEMGW_PRODUCTION_ALLOW"), ",") {
		switch p := pgstore.Purpose(strings.TrimSpace(raw)); p {
		case "":
		case pgstore.PurposeSchemaMigration, pgstore.PurposeHistoryImport, pgstore.PurposeServe:
			t.Allow = append(t.Allow, p)
		default:
			return nil, fmt.Errorf("MEMGW_PRODUCTION_ALLOW lists unknown purpose %q", raw)
		}
	}
	if len(t.Allow) == 0 {
		return nil, errors.New("MEMGW_PRODUCTION_ALLOW is empty; name schema_migration, history_import and/or serve")
	}
	return t, nil
}

func memgwStatus(ctx context.Context, r *migrations.Runner, pool *pgstore.Pool) error {
	rows, err := r.Status(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("target: %s schema=%s\n\n", pgstore.RedactDSN(os.Getenv("MEMGW_DSN")), pool.Schema())
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VERSION\tNAME\tSTATE\tAPPLIED AT")
	for _, s := range rows {
		state := "pending"
		switch {
		case s.Dirty:
			state = "DIRTY"
		case s.Drifted:
			state = "DRIFTED"
		case s.Applied:
			state = "applied"
		}
		at := "-"
		if s.Applied {
			at = s.AppliedAt.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%04d\t%s\t%s\t%s\n", s.Version, s.Name, state, at)
	}
	return w.Flush()
}

func memgwPlan(ctx context.Context, r *migrations.Runner, pool *pgstore.Pool, args []string) error {
	fs := flag.NewFlagSet("memgw plan", flag.ExitOnError)
	out := fs.String("out", "", "write the reviewable plan, with its digest, to this file")
	_ = fs.Parse(args)

	// BuildPlan verifies the target the same way an apply would, so a plan that
	// would be refused at "up" time is refused here, which costs nothing.
	plan, err := r.BuildPlan(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("target: %s schema=%s\n", plan.Redacted, pool.Schema())
	if t := plan.Target; t != nil {
		fmt.Printf("  database=%s role=%s deployment=%s\n", t.Database, t.Role, t.Deployment)
		fmt.Printf("  server=%s transport=%s synchronous_commit=%s schema_owner=%s\n",
			t.ServerVersion, t.Transport, t.SyncCommit, t.SchemaOwner)
	}
	fmt.Printf("current version: %04d   digest: %s\n", plan.CurrentVersion, plan.Digest)

	if len(plan.Pending) == 0 {
		fmt.Println("nothing to apply")
	} else {
		fmt.Printf("%d migration(s) would be applied:\n", len(plan.Pending))
		for _, m := range plan.Pending {
			mode := "in a transaction"
			if !m.InTx {
				mode = "OUTSIDE a transaction"
			}
			fmt.Printf("  %04d_%s  %s  checksum=%s\n", m.Version, m.Name, mode, m.Checksum[:12])
		}
	}
	if len(plan.Risky) > 0 {
		fmt.Printf("\n%d risky operation(s) -- read these before approving:\n", len(plan.Risky))
		for _, o := range plan.Risky {
			fmt.Printf("  %04d_%s  %s\n", o.Version, o.Name, o.Concern)
		}
	}
	if *out != "" {
		body, err := json.MarshalIndent(plan, "", "  ")
		if err != nil {
			return err
		}
		// 0600: the plan carries no credential, but it is the artefact an
		// approval hangs on and it should not be world-writable.
		if err := os.WriteFile(*out, append(body, '\n'), 0o600); err != nil {
			return err
		}
		fmt.Printf("\nplan written to %s\n", *out)
	}
	return nil
}

func memgwUp(ctx context.Context, r *migrations.Runner, pool *pgstore.Pool, args []string) error {
	fs := flag.NewFlagSet("memgw up", flag.ExitOnError)
	planFile := fs.String("plan", "", "apply exactly this reviewed plan; required in production")
	_ = fs.Parse(args)

	var approved *migrations.MigrationPlan
	if *planFile != "" {
		body, err := os.ReadFile(*planFile)
		if err != nil {
			return fmt.Errorf("memgw: could not read the plan: %w", err)
		}
		approved = &migrations.MigrationPlan{}
		if err := json.Unmarshal(body, approved); err != nil {
			return fmt.Errorf("memgw: the plan file is not a plan: %w", err)
		}
	}

	applied, err := r.ApplyPlan(ctx, approved)
	// Whatever happened, report what did land: a partial run is exactly when an
	// operator needs to know where it stopped.
	for _, m := range applied {
		fmt.Printf("applied %04d_%s\n", m.Version, m.Name)
	}
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		fmt.Println("nothing to apply")
	}
	return nil
}

// openMemgwGateway builds the memory gateway's HTTP handler, or returns nil if
// the service is not configured for it.
//
// MEMGW_DSN being unset means "this deployment does not serve the gateway", not
// "guess a database". The gateway stays off rather than half-on: a route that
// existed but could not reach a ledger would answer every write with a 500 that
// looks like a transient fault, and adapters would retry it forever.
//
// Serving is not migrating, and the two are gated differently. A migration
// changes the shape of the ledger and goes through the plan-and-digest path; a
// server appends events to a shape that already exists. What serving does need
// is that the target is the one the operator meant, so in production the same
// assertion is verified here -- database, schema, role, deployment, transport,
// writable primary, and no foreign object in the managed schema -- before a
// single route is mounted.
//
// Development and test are deliberately untouched: they open exactly as they
// always did. An earlier version of this comment claimed the non-production
// check ran here; it did not, and saying so was worse than the gap.
func openMemgwGateway(ctx context.Context) (http.Handler, func(), error) {
	if strings.TrimSpace(os.Getenv("MEMGW_DSN")) == "" {
		return nil, func() {}, nil
	}
	pool, err := openMemgwPool(ctx)
	if err != nil {
		return nil, func() {}, err
	}
	if pool.IsProduction() {
		pool.DeclareManagedObjects(managedObjectsOrNil(pool))
		if _, err := pgstore.VerifyProduction(ctx, pool, pgstore.PurposeServe); err != nil {
			pool.Close()
			return nil, func() {}, err
		}
	}
	return gateway.New(pool).Routes(), pool.Close, nil
}

// managedObjectsOrNil returns what the migration set says this schema should
// contain. On failure it returns nil, which the production check treats as "the
// caller cannot describe what it owns" and refuses -- the safe direction.
func managedObjectsOrNil(pool *pgstore.Pool) []string {
	objects, err := migrations.New(pool).ManagedObjects()
	if err != nil {
		return nil
	}
	return objects
}
