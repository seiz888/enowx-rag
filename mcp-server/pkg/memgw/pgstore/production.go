package pgstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// The production target path.
//
// VerifyNonProduction (pgstore.go) is unchanged and still governs development
// and test. It refuses production by design, and nothing here weakens it: a
// development or test caller reaches exactly the same code it always did.
//
// What this file adds is a second, separately-selected path for a target the
// operator has explicitly named. The difference between the two is not
// strictness -- production is checked harder -- but *what* is being proved.
// VerifyNonProduction proves "this cannot be anything important". This proves
// "this is precisely the thing you said it was". A guard that only knows how to
// say no is a guard people eventually delete.
//
// Three rules shape everything below.
//
//  1. Naming is not evidence. A database called "memgw_prod" and a database
//     called "ledger" are equally unproven until the connection itself agrees
//     with a written-down assertion of database, schema, role and deployment
//     identity. An environment variable alone selects nothing.
//  2. The managed schema is the blast radius. Production clusters are shared:
//     axonhub lives on the same server. So the ownership and foreign-object
//     checks look *only* inside the schema this runner manages. Another
//     application's tables in the same database are none of our business, and
//     finding them is not a reason to refuse -- let alone to touch them.
//  3. Authority does not generalise. Approving a schema migration approves a
//     schema migration. It does not approve importing, overwriting or deleting
//     historical data, which is a different act with a different failure mode.
//     Purpose is carried through every check.

// Purpose names what a caller intends to do with a verified target. It exists
// so that one approval cannot silently authorise a different act.
type Purpose string

const (
	// PurposeSchemaMigration is DDL applied by the migration runner.
	PurposeSchemaMigration Purpose = "schema_migration"
	// PurposeHistoryImport is writing pre-gateway mapping rows. It is separate
	// from schema migration on purpose: a migration adds structure, an import
	// adds and rewrites data.
	PurposeHistoryImport Purpose = "history_import"
	// PurposeServe is opening the gateway against a target: appending events to
	// a schema that already exists. It changes no structure and rewrites no
	// history, so it is the weakest of the three -- but it is still named, so
	// that a host pointed at the wrong database refuses to serve rather than
	// writing a ledger into it.
	PurposeServe Purpose = "serve"
)

var (
	// ErrProductionNotSelected means production mode was not explicitly chosen.
	// It is the default answer, and it is what an unconfigured caller gets.
	ErrProductionNotSelected = errors.New("memgw: production mode was not explicitly selected")
	// ErrTargetMismatch means the live connection disagrees with the assertion
	// the operator wrote down.
	ErrTargetMismatch = errors.New("memgw: the connection does not match the asserted target")
	// ErrPurposeNotAllowed means this assertion does not authorise this act.
	ErrPurposeNotAllowed = errors.New("memgw: this target assertion does not authorise this purpose")
	// ErrInsecureTransport means the connection is neither a local socket nor a
	// TLS connection whose certificate was verified.
	ErrInsecureTransport = errors.New("memgw: production requires a local socket or verified TLS")
	// ErrForeignObject means the managed schema holds something this runner did
	// not create. Fail-closed: it is never dropped, renamed or adopted.
	ErrForeignObject = errors.New("memgw: the managed schema holds an object this runner did not create")
)

// ProductionTarget is the operator's written assertion about what they are
// connecting to. Every field is required; there are no defaults, because a
// default is a guess and the point of this struct is that nothing is guessed.
type ProductionTarget struct {
	// Database must equal current_database() on the live connection.
	Database string
	// Schema must equal the pool's schema and is the only schema examined for
	// ownership and foreign objects.
	Schema string
	// Role must equal current_user on the live connection.
	Role string
	// Deployment is a free-form identity for this installation, recorded in the
	// managed schema on first use and compared on every use afterwards. It is
	// what distinguishes two databases that are otherwise named identically --
	// a restored copy, a staging clone, a second region.
	Deployment string
	// Allow lists the purposes this assertion authorises. An empty list
	// authorises nothing.
	Allow []Purpose
	// ExpectedObjects is the set of relation names the caller manages in
	// Schema, unqualified and lower-cased. A relation found in the schema that
	// is not in this set is refused. Nil means the caller cannot describe what
	// it owns, which is itself a refusal in production.
	ExpectedObjects []string
}

// Allows reports whether this assertion authorises a purpose.
func (t *ProductionTarget) Allows(p Purpose) bool {
	if t == nil {
		return false
	}
	for _, a := range t.Allow {
		if a == p {
			return true
		}
	}
	return false
}

// DeclareManagedObjects records the relation names the caller manages in the
// target schema, filling in ProductionTarget.ExpectedObjects.
//
// It is a separate call rather than a config field because only the component
// that owns the SQL knows the answer, and it learns it after the pool is open.
// Declaring nothing is not the same as declaring an empty set: a nil
// ExpectedObjects is a refusal in production, and passing nil here leaves it
// nil rather than quietly asserting "I manage no objects".
func (p *Pool) DeclareManagedObjects(names []string) {
	if names == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cfg.Production != nil {
		p.cfg.Production.ExpectedObjects = names
	}
}

// ProductionFacts is what the live connection actually said. It is returned so
// a migration plan can name the target it was computed against, and it carries
// nothing that could be a credential.
type ProductionFacts struct {
	Database      string `json:"database"`
	Schema        string `json:"schema"`
	Role          string `json:"role"`
	Deployment    string `json:"deployment"`
	ServerVersion string `json:"server_version"`
	Transport     string `json:"transport"`
	SyncCommit    string `json:"synchronous_commit"`
	SchemaOwner   string `json:"schema_owner"`
	Server        string `json:"server"`
	// SchemaIsVirgin is true when the managed schema does not exist yet or
	// holds no relations. The first migration of a new deployment runs against
	// a virgin schema, and that is the only time a deployment identity may be
	// written.
	SchemaIsVirgin bool `json:"schema_is_virgin"`
}

// VerifyTarget is the single entry point every write path uses. It preserves
// the existing behaviour exactly for development and test -- same function,
// same refusals, same messages -- and routes production to the assertion path.
//
// Callers that have no production story at all can keep calling
// VerifyNonProduction directly; the test harness does.
func VerifyTarget(ctx context.Context, p *Pool, purpose Purpose) error {
	if p.cfg.Env == EnvProduction {
		_, err := VerifyProduction(ctx, p, purpose)
		return err
	}
	return VerifyNonProduction(ctx, p)
}

// VerifyProduction proves that the live connection is the target the operator
// asserted, that the transport is safe, that the server can durably accept
// writes, that we own the schema we are about to change, and that the schema
// holds nothing we did not put there.
//
// It never writes. The one thing production mode may write -- the deployment
// identity row on a virgin schema -- is done by the caller, after apply, so
// that a verification alone leaves no trace.
func VerifyProduction(ctx context.Context, p *Pool, purpose Purpose) (*ProductionFacts, error) {
	cfg := p.cfg

	// 1. Explicit selection. The default is a refusal: an env var saying
	//    "production" with no assertion behind it selects nothing.
	if cfg.Env != EnvProduction {
		return nil, fmt.Errorf("%w: environment is %q, not %q",
			ErrProductionNotSelected, cfg.Env, EnvProduction)
	}
	t := cfg.Production
	if t == nil {
		return nil, fmt.Errorf("%w: no target assertion was supplied; declare database, schema, role and deployment",
			ErrProductionNotSelected)
	}
	for _, f := range []struct{ name, val string }{
		{"database", t.Database}, {"schema", t.Schema},
		{"role", t.Role}, {"deployment", t.Deployment},
	} {
		if strings.TrimSpace(f.val) == "" {
			return nil, fmt.Errorf("%w: the target assertion has no %s", ErrProductionNotSelected, f.name)
		}
	}
	if t.ExpectedObjects == nil {
		return nil, fmt.Errorf("%w: the caller did not declare which objects it manages", ErrProductionNotSelected)
	}

	// 2. Purpose. Schema migration and history import are separate authorities.
	if !t.Allows(purpose) {
		return nil, fmt.Errorf("%w: %q is not in the allowed set %v", ErrPurposeNotAllowed, purpose, t.Allow)
	}

	// The assertion must agree with how the pool was actually opened, or the
	// schema being checked is not the schema being written.
	if t.Schema != cfg.Schema {
		return nil, fmt.Errorf("%w: assertion names schema %q but the pool is pinned to %q",
			ErrTargetMismatch, t.Schema, cfg.Schema)
	}

	// 3. Transport. Checked before anything else touches the network in
	//    earnest, because "who am I talking to" is meaningless over a
	//    connection that could have been intercepted.
	transport, err := verifyTransport(ctx, p)
	if err != nil {
		return nil, err
	}

	qctx, cancel := p.WithQueryTimeout(ctx)
	defer cancel()

	facts := &ProductionFacts{Schema: cfg.Schema, Transport: transport}
	var isRecovery, txReadOnly bool
	err = p.pool.QueryRow(qctx, `
		SELECT current_database(),
		       current_user::text,
		       current_setting('server_version'),
		       pg_is_in_recovery(),
		       current_setting('transaction_read_only')::bool,
		       current_setting('synchronous_commit'),
		       COALESCE(host(inet_server_addr())::text, 'local')`).
		Scan(&facts.Database, &facts.Role, &facts.ServerVersion,
			&isRecovery, &txReadOnly, &facts.SyncCommit, &facts.Server)
	if err != nil {
		return nil, fmt.Errorf("memgw: could not identify target: %w", err)
	}

	// 4. Identity. Database and role must be what was asserted. This is the
	//    check a mistyped DSN, a forwarded port or a restored clone fails.
	if facts.Database != t.Database {
		return nil, fmt.Errorf("%w: connected to database %q, assertion says %q",
			ErrTargetMismatch, facts.Database, t.Database)
	}
	if facts.Role != t.Role {
		return nil, fmt.Errorf("%w: connected as role %q, assertion says %q",
			ErrTargetMismatch, facts.Role, t.Role)
	}

	// 5. Writable primary. A standby answers reads and silently refuses the
	//    write halfway through.
	if isRecovery {
		return nil, fmt.Errorf("%w: target is a standby in recovery", ErrUnsafeTarget)
	}
	if txReadOnly {
		return nil, fmt.Errorf("%w: the session is read-only (default_transaction_read_only)", ErrUnsafeTarget)
	}
	// synchronous_commit=off means an acknowledgement is not a promise. Every
	// other setting -- local, remote_write, remote_apply, on -- flushes locally
	// before acknowledging, which is the property being relied on.
	if strings.EqualFold(facts.SyncCommit, "off") {
		return nil, fmt.Errorf("%w: synchronous_commit is off, so a commit acknowledgement would not mean durable",
			ErrUnsafeTarget)
	}

	// 6. Ownership and privilege, scoped to the managed schema only.
	if err := p.verifySchemaAuthority(qctx, cfg.Schema, facts); err != nil {
		return nil, err
	}

	// 7. Foreign objects inside the managed schema. Other schemas in this
	//    database belong to other applications and are neither inspected nor
	//    touched; see the note at the top of this file.
	if err := p.verifyNoForeignObjects(qctx, cfg.Schema, t.ExpectedObjects, facts); err != nil {
		return nil, err
	}

	// 8. Deployment identity. Absent is only acceptable on a virgin schema --
	//    that is the first migration of a new deployment. On an established
	//    schema a missing or different identity means this is not the
	//    installation the operator thinks it is.
	deployment, found, err := p.readDeployment(qctx, cfg.Schema)
	if err != nil {
		return nil, err
	}
	switch {
	case found && deployment != t.Deployment:
		return nil, fmt.Errorf("%w: the schema records deployment %q, assertion says %q",
			ErrTargetMismatch, deployment, t.Deployment)
	case found:
		facts.Deployment = deployment
	case facts.SchemaIsVirgin:
		// A schema this runner has never touched. The identity is what the
		// operator asserted, and a successful apply is what records it.
		facts.Deployment = t.Deployment
	default:
		return nil, fmt.Errorf("%w: the schema holds objects but records no deployment identity; it was not created by this runner",
			ErrTargetMismatch)
	}

	p.mu.Lock()
	p.facts = facts
	p.mu.Unlock()
	return facts, nil
}

// verifyTransport refuses anything that is not a local unix socket or a TLS
// connection whose certificate this client actually verified.
//
// Both halves matter and neither is sufficient alone. sslmode=require encrypts
// and verifies nothing, so it is refused; and a DSN claiming verify-full proves
// nothing if the connection came up unencrypted, so the server is asked what it
// actually did.
func verifyTransport(ctx context.Context, p *Pool) (string, error) {
	host := dsnHost(p.cfg.DSN)
	if host == "" || strings.HasPrefix(host, "/") || strings.HasPrefix(host, "@") {
		return "unix-socket", nil
	}

	mode := strings.ToLower(strings.TrimSpace(dsnParam(p.cfg.DSN, "sslmode")))
	switch mode {
	case "verify-full", "verify-ca":
	case "":
		return "", fmt.Errorf("%w: the DSN sets no sslmode; production requires verify-ca or verify-full", ErrInsecureTransport)
	default:
		return "", fmt.Errorf("%w: sslmode=%s does not verify the server certificate; use verify-ca or verify-full", ErrInsecureTransport, mode)
	}

	qctx, cancel := p.WithQueryTimeout(ctx)
	defer cancel()
	var ssl bool
	var version, cipher *string
	err := p.pool.QueryRow(qctx,
		`SELECT ssl, version, cipher FROM pg_stat_ssl WHERE pid = pg_backend_pid()`).
		Scan(&ssl, &version, &cipher)
	if err != nil {
		return "", fmt.Errorf("memgw: could not read the connection's TLS state: %w", err)
	}
	if !ssl {
		return "", fmt.Errorf("%w: the DSN asked for %s but the connection is not encrypted", ErrInsecureTransport, mode)
	}
	v := "tls"
	if version != nil {
		v = strings.ToLower(*version)
	}
	return v + " (" + mode + ")", nil
}

// verifySchemaAuthority checks that the managed schema exists, that we own it
// (directly or through role membership), and that we may create in it.
func (p *Pool) verifySchemaAuthority(ctx context.Context, schema string, facts *ProductionFacts) error {
	var owner *string
	var isMember, canCreate, canUse *bool
	err := p.pool.QueryRow(ctx, `
		SELECT n.nspowner::regrole::text,
		       pg_has_role(current_user, n.nspowner, 'USAGE'),
		       has_schema_privilege(current_user, n.nspname, 'CREATE'),
		       has_schema_privilege(current_user, n.nspname, 'USAGE')
		FROM pg_namespace n WHERE n.nspname = $1`, schema).
		Scan(&owner, &isMember, &canCreate, &canUse)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			// The schema does not exist yet. That is the legitimate first-run
			// case, but only if we may create it.
			var canCreateDB bool
			if err := p.pool.QueryRow(ctx,
				`SELECT has_database_privilege(current_user, current_database(), 'CREATE')`).
				Scan(&canCreateDB); err != nil {
				return fmt.Errorf("memgw: could not check database privileges: %w", err)
			}
			if !canCreateDB {
				return fmt.Errorf("%w: schema %q does not exist and this role may not create it",
					ErrUnsafeTarget, schema)
			}
			facts.SchemaOwner = "(to be created)"
			facts.SchemaIsVirgin = true
			return nil
		}
		return fmt.Errorf("memgw: could not inspect schema %q: %w", schema, err)
	}
	if owner != nil {
		facts.SchemaOwner = *owner
	}
	if isMember == nil || !*isMember {
		return fmt.Errorf("%w: schema %q is owned by %s and this role is not a member of it",
			ErrUnsafeTarget, schema, facts.SchemaOwner)
	}
	if canCreate == nil || !*canCreate || canUse == nil || !*canUse {
		return fmt.Errorf("%w: this role lacks CREATE or USAGE on schema %q", ErrUnsafeTarget, schema)
	}
	return nil
}

// verifyNoForeignObjects refuses if the managed schema holds a relation the
// caller did not declare.
//
// Only explicitly-created relations are counted. Indexes that back a constraint
// and sequences behind a serial column are dependent objects -- PostgreSQL made
// them, not us, and they are filtered by their pg_depend entry rather than by
// guessing at their names.
//
// Nothing found here is dropped, renamed or adopted. The refusal is the whole
// action: an unexpected table in the schema we are about to migrate means
// somebody else is using it, and the correct response is to stop and ask.
func (p *Pool) verifyNoForeignObjects(ctx context.Context, schema string, expected []string, facts *ProductionFacts) error {
	want := make(map[string]bool, len(expected))
	for _, o := range expected {
		want[strings.ToLower(strings.TrimSpace(o))] = true
	}
	rows, err := p.pool.Query(ctx, `
		SELECT c.relname, c.relkind
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND c.relkind IN ('r','p','v','m','S','i','I')
		  AND NOT EXISTS (
		        SELECT 1 FROM pg_depend d
		        WHERE d.objid = c.oid AND d.deptype IN ('i','a','e'))
		ORDER BY c.relname`, schema)
	if err != nil {
		return fmt.Errorf("memgw: could not inspect schema %q: %w", schema, err)
	}
	defer rows.Close()

	var foreign []string
	count := 0
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			return fmt.Errorf("memgw: could not inspect schema %q: %w", schema, err)
		}
		count++
		if !want[strings.ToLower(name)] {
			foreign = append(foreign, name+" ("+relkindName(kind)+")")
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("memgw: could not inspect schema %q: %w", schema, err)
	}
	if count == 0 {
		facts.SchemaIsVirgin = true
	}
	if len(foreign) > 0 {
		sort.Strings(foreign)
		if len(foreign) > 5 {
			foreign = append(foreign[:5], fmt.Sprintf("and %d more", len(foreign)-5))
		}
		return fmt.Errorf("%w: schema %q holds %s; nothing was changed",
			ErrForeignObject, schema, strings.Join(foreign, ", "))
	}
	return nil
}

func relkindName(k string) string {
	switch k {
	case "r":
		return "table"
	case "p":
		return "partitioned table"
	case "v":
		return "view"
	case "m":
		return "materialised view"
	case "S":
		return "sequence"
	case "i", "I":
		return "index"
	}
	return "relation"
}

// DeploymentTable is the unqualified name of the one-row table holding this
// installation's identity. It is created by the migration runner alongside the
// history table and is part of every caller's expected object set.
const DeploymentTable = "deployment_identity"

// readDeployment returns the recorded deployment identity. A missing table is
// not an error: it is the state of a schema this runner has never touched.
func (p *Pool) readDeployment(ctx context.Context, schema string) (string, bool, error) {
	var exists *string
	if err := p.pool.QueryRow(ctx, `SELECT to_regclass($1)::text`,
		QuoteIdent(schema)+"."+QuoteIdent(DeploymentTable)).Scan(&exists); err != nil {
		return "", false, fmt.Errorf("memgw: could not look for the deployment identity: %w", err)
	}
	if exists == nil {
		return "", false, nil
	}
	var id *string
	err := p.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT deployment FROM %s.%s LIMIT 1`, QuoteIdent(schema), QuoteIdent(DeploymentTable))).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return "", false, nil
		}
		return "", false, fmt.Errorf("memgw: could not read the deployment identity: %w", err)
	}
	if id == nil {
		return "", false, nil
	}
	return *id, true, nil
}

// credentialParam names the DSN keys whose value is a secret. dsnParam refuses
// them outright rather than trusting every future caller to ask only for
// harmless keys: the one place this function is used wants sslmode, and a
// generic accessor that will hand over a password on request is a leak waiting
// for a careless line.
var credentialParam = map[string]bool{
	"password":    true,
	"passfile":    true,
	"sslpassword": true,
	"sslkey":      true,
}

// dsnParam extracts one non-credential parameter from either DSN form. Asking
// for a credential returns the empty string, not the credential.
func dsnParam(dsn, key string) string {
	if credentialParam[strings.ToLower(key)] {
		return ""
	}
	if u, err := url.Parse(dsn); err == nil && u.Host != "" {
		if v := u.Query().Get(key); v != "" {
			return v
		}
		return ""
	}
	for _, field := range strings.Fields(dsn) {
		if k, v, ok := strings.Cut(field, "="); ok && strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}
