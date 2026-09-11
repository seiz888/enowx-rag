package migrations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
)

// Production migration: a reviewed plan, a digest, a lock, and an audit row.
//
// The non-production path is unchanged. `memgw up` against a test database
// still verifies the target the way it always did and still applies whatever is
// pending. What is added here is the machinery a production change needs and a
// disposable one does not:
//
//   - a plan an operator can read, that names the target it was computed
//     against and every risky statement it contains;
//   - a digest over that plan *and over the history it was computed from*, so
//     that a target, a migration file or an already-applied row changing
//     between review and apply refuses instead of proceeding;
//   - an advisory lock, so two runners cannot interleave;
//   - an audit row, written whether the run succeeded or failed.
//
// There is still no down migration and no automatic restore. A mistake is
// corrected by a new forward migration -- the same rule the event ledger
// follows -- and recovering from a bad apply is an operator procedure with
// fencing and reconciliation in it, not something a runner should attempt while
// the writers are still connected.

var (
	// ErrPlanStale means the plan being applied no longer describes reality:
	// the target, the migration files or the applied history changed after the
	// plan was reviewed.
	ErrPlanStale = errors.New("memgw: the approved plan no longer matches this target")
	// ErrLockHeld means another runner is already migrating this schema.
	ErrLockHeld = errors.New("memgw: another migration runner holds the lock for this schema")
	// ErrPlanRequired means a production apply was attempted with no reviewed
	// plan. Production never applies what nobody read.
	ErrPlanRequired = errors.New("memgw: a production apply requires an approved plan")
)

// runnerOwnedTables are the tables the runner itself creates, outside any
// migration file. They are part of every expected object set.
var runnerOwnedTables = []string{
	"schema_migrations",
	pgstore.DeploymentTable,
	"migration_audit",
}

// createdObject matches the relations our own migration SQL creates. It only
// has to understand SQL this repository writes, which is why it is a regexp and
// not a parser -- and why Load() rejects any file that does not follow the
// house style in the first place.
var createdObject = regexp.MustCompile(
	`(?im)^\s*CREATE\s+(?:OR\s+REPLACE\s+)?(?:UNIQUE\s+)?(?:MATERIALIZED\s+)?(?:TABLE|INDEX|VIEW|SEQUENCE)\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?("[^"]+"|[a-zA-Z_][a-zA-Z0-9_$]*)(?:\s*\.\s*("[^"]+"|[a-zA-Z_][a-zA-Z0-9_$]*))?`)

// ManagedObjects returns every relation this migration set creates, plus the
// runner's own tables. It is what the production target check compares the live
// schema against: anything in the managed schema that is not in this list was
// put there by somebody else, and the runner refuses rather than adopting it.
func (r *Runner) ManagedObjects() ([]string, error) {
	set, err := r.Load()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, t := range runnerOwnedTables {
		seen[t] = true
	}
	for _, m := range set {
		for _, g := range createdObject.FindAllStringSubmatch(m.SQL, -1) {
			name := g[1]
			if g[2] != "" {
				// schema-qualified: the second group is the object.
				name = g[2]
			}
			seen[strings.ToLower(strings.Trim(name, `"`))] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// riskyPattern names a statement shape that deserves a second read before it
// runs against data somebody depends on. None of them is refused: the runner's
// job is to make them visible in the plan, not to decide they are wrong.
type riskyPattern struct {
	what string
	re   *regexp.Regexp
	// exempt, when set, is applied to each individual match of re: a match that
	// also satisfies exempt is not a concern. Go's regexp engine has no negative
	// lookahead, so "ADD CONSTRAINT but not NOT VALID" is expressed as two
	// passes rather than as one pattern that does not compile.
	exempt *regexp.Regexp
}

// matches reports whether any occurrence of the pattern in this SQL is a real
// concern. It looks at occurrences rather than at the file as a whole, so one
// CREATE INDEX CONCURRENTLY does not excuse a plain CREATE INDEX beside it.
func (p riskyPattern) matches(sql string) bool {
	for _, m := range p.re.FindAllString(sql, -1) {
		if p.exempt == nil || !p.exempt.MatchString(m) {
			return true
		}
	}
	return false
}

var riskyPatterns = []riskyPattern{
	{what: "drops a table", re: regexp.MustCompile(`(?i)\bDROP\s+TABLE\b`)},
	{what: "drops a column", re: regexp.MustCompile(`(?i)\bDROP\s+COLUMN\b`)},
	{what: "drops an index", re: regexp.MustCompile(`(?i)\bDROP\s+INDEX\b`)},
	{what: "drops a schema", re: regexp.MustCompile(`(?i)\bDROP\s+SCHEMA\b`)},
	{what: "truncates a table", re: regexp.MustCompile(`(?i)\bTRUNCATE\b`)},
	{what: "deletes rows", re: regexp.MustCompile(`(?i)\bDELETE\s+FROM\b`)},
	{what: "updates rows", re: regexp.MustCompile(`(?i)\bUPDATE\s+[a-zA-Z_"]`)},
	{what: "renames an object", re: regexp.MustCompile(`(?i)\bRENAME\s+TO\b`)},
	{what: "changes a column type", re: regexp.MustCompile(`(?i)\bALTER\s+COLUMN\b[^;]*\bTYPE\b`)},
	{what: "adds a NOT NULL constraint, which scans the table", re: regexp.MustCompile(`(?i)\bSET\s+NOT\s+NULL\b`)},
	{
		what:   "adds a validated constraint, which scans the table",
		re:     regexp.MustCompile(`(?i)\bADD\s+CONSTRAINT\b[^;]*`),
		exempt: regexp.MustCompile(`(?i)\bNOT\s+VALID\b`),
	},
	{
		what:   "builds an index without CONCURRENTLY, which locks writes",
		re:     regexp.MustCompile(`(?i)\bCREATE\s+(?:UNIQUE\s+)?INDEX\s+\w+`),
		exempt: regexp.MustCompile(`(?i)\bCONCURRENTLY\b`),
	},
}

// PlanMigration is one migration as it appears in a plan.
type PlanMigration struct {
	Version  int    `json:"version"`
	Name     string `json:"name"`
	Checksum string `json:"checksum"`
	InTx     bool   `json:"in_transaction"`
}

// RiskyOperation is one statement shape found in a pending migration.
type RiskyOperation struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
	Concern string `json:"concern"`
}

// MigrationPlan is the reviewable artefact. It carries no credential: the
// target is described by name and by what the server said about itself, and the
// DSN appears only in redacted form.
type MigrationPlan struct {
	Purpose        string                   `json:"purpose"`
	Target         *pgstore.ProductionFacts `json:"target"`
	Redacted       string                   `json:"target_dsn_redacted"`
	CurrentVersion int                      `json:"current_version"`
	Applied        []PlanMigration          `json:"applied"`
	Pending        []PlanMigration          `json:"pending"`
	Risky          []RiskyOperation         `json:"risky_operations"`
	ManagedObjects []string                 `json:"managed_objects"`
	GeneratedAt    time.Time                `json:"generated_at"`
	Digest         string                   `json:"digest"`
}

// digestInput is the canonical form the digest is taken over. GeneratedAt and
// the digest itself are excluded: re-planning the same unchanged target twice
// must produce the same digest, or the digest would only prove that time
// passed.
//
// The applied history is included deliberately. A plan reviewed when three
// migrations were applied must not be usable after a fourth landed, even if the
// pending list happens to look the same.
type digestInput struct {
	Purpose        string          `json:"purpose"`
	Database       string          `json:"database"`
	Schema         string          `json:"schema"`
	Role           string          `json:"role"`
	Deployment     string          `json:"deployment"`
	Applied        []PlanMigration `json:"applied"`
	Pending        []PlanMigration `json:"pending"`
	ManagedObjects []string        `json:"managed_objects"`
}

func (p *MigrationPlan) computeDigest() (string, error) {
	in := digestInput{
		Purpose:        p.Purpose,
		Applied:        p.Applied,
		Pending:        p.Pending,
		ManagedObjects: p.ManagedObjects,
	}
	if p.Target != nil {
		in.Database = p.Target.Database
		in.Schema = p.Target.Schema
		in.Role = p.Target.Role
		in.Deployment = p.Target.Deployment
	}
	b, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// BuildPlan verifies the target for schema migration and returns a reviewable
// plan. It writes nothing at all: not the deployment identity, and not the
// bookkeeping tables either. Both belong to an apply.
//
// It works in every environment. In development and test the target check is
// the unchanged VerifyNonProduction and Target is left nil, because there is no
// assertion to report; the digest still covers the history and the pending set,
// so a stale plan is caught there too.
func (r *Runner) BuildPlan(ctx context.Context) (*MigrationPlan, error) {
	// What this runner manages is derived from the SQL it carries, so it is
	// computed before the target is verified and handed to the check rather
	// than being configured by hand somewhere it could drift.
	managed, err := r.ManagedObjects()
	if err != nil {
		return nil, err
	}
	r.pool.DeclareManagedObjects(managed)

	if err := pgstore.VerifyTarget(ctx, r.pool, pgstore.PurposeSchemaMigration); err != nil {
		return nil, err
	}
	set, err := r.Load()
	if err != nil {
		return nil, err
	}
	// Read the history without creating it. Planning against a schema this
	// runner has never touched must leave that schema exactly as it found it:
	// creating the bookkeeping tables here would end the schema's virginity
	// without recording a deployment identity, and the next run would refuse
	// its own leftovers. A plan is a read.
	done, err := r.appliedIfPresent(ctx)
	if err != nil {
		return nil, err
	}
	pending, err := validatePlan(set, done)
	if err != nil {
		return nil, err
	}

	plan := &MigrationPlan{
		Purpose:        string(pgstore.PurposeSchemaMigration),
		Redacted:       r.pool.RedactedTarget(),
		ManagedObjects: managed,
		GeneratedAt:    time.Now().UTC(),
	}
	if facts := r.pool.ProductionFacts(); facts != nil {
		plan.Target = facts
	}
	for _, v := range sortedVersions(done) {
		a := done[v]
		plan.Applied = append(plan.Applied, PlanMigration{
			Version: a.Version, Name: a.Name, Checksum: a.Checksum, InTx: true,
		})
		if a.Version > plan.CurrentVersion {
			plan.CurrentVersion = a.Version
		}
	}
	for _, m := range pending {
		plan.Pending = append(plan.Pending, PlanMigration{
			Version: m.Version, Name: m.Name, Checksum: m.Checksum, InTx: m.InTx,
		})
		for _, rp := range riskyPatterns {
			if rp.matches(stripSQLComments(m.SQL)) {
				plan.Risky = append(plan.Risky, RiskyOperation{
					Version: m.Version, Name: m.Name, Concern: rp.what,
				})
			}
		}
	}
	digest, err := plan.computeDigest()
	if err != nil {
		return nil, err
	}
	plan.Digest = digest
	return plan, nil
}

// stripSQLComments removes line and block comments so a risky word inside a
// comment does not raise a false concern, and -- more importantly -- so a real
// one cannot be hidden behind one.
func stripSQLComments(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "--"):
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				return b.String()
			}
			i += j
		case strings.HasPrefix(s[i:], "/*"):
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				return b.String()
			}
			i += j + 4
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

// appliedIfPresent reads the migration history, treating a schema that has no
// history table as a schema with no history. That is not the same as swallowing
// an error: a missing table is the honest state of a target nothing has been
// applied to, and it is the only case handled here -- every other failure is
// returned.
func (r *Runner) appliedIfPresent(ctx context.Context) (map[int]AppliedMigration, error) {
	qctx, cancel := r.pool.WithQueryTimeout(ctx)
	defer cancel()
	var present *string
	if err := r.pool.Pgx().QueryRow(qctx, `SELECT to_regclass($1)::text`,
		pgstore.QuoteIdent(r.pool.Schema())+".schema_migrations").Scan(&present); err != nil {
		return nil, fmt.Errorf("memgw: look for the migration history: %w", err)
	}
	if present == nil {
		return map[int]AppliedMigration{}, nil
	}
	return r.applied(ctx)
}

func sortedVersions(m map[int]AppliedMigration) []int {
	out := make([]int, 0, len(m))
	for v := range m {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

// ApplyPlan applies exactly the plan that was approved.
//
// approved is the plan as reviewed. The runner recomputes the plan against the
// live target and refuses unless the digest still matches -- which catches a
// changed target, an edited migration file, a migration applied by someone else
// in the meantime, and an edited plan file, all with the same check.
//
// The whole apply is serialised by an advisory lock held for its duration, and
// an audit row is written whether it succeeds or fails.
func (r *Runner) ApplyPlan(ctx context.Context, approved *MigrationPlan) ([]Migration, error) {
	if r.pool.IsProduction() && approved == nil {
		return nil, ErrPlanRequired
	}

	unlock, err := r.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Recomputed *inside* the lock. A plan built before the lock was taken
	// could describe a history another runner has since moved.
	current, err := r.BuildPlan(ctx)
	if err != nil {
		return nil, err
	}
	if approved != nil {
		if approved.Digest == "" {
			return nil, fmt.Errorf("%w: the approved plan carries no digest", ErrPlanStale)
		}
		if approved.Digest != current.Digest {
			return nil, fmt.Errorf("%w: approved %s, now %s -- re-plan and review it again",
				ErrPlanStale, short(approved.Digest), short(current.Digest))
		}
	}

	// Only now, past every check and past the digest comparison, does anything
	// get written. The bookkeeping tables and the deployment identity are
	// created together: the identity is what says "this runner made this
	// schema", so it is stamped at the moment the schema is made rather than
	// when the last migration happens to succeed. A run that dies halfway then
	// leaves a schema that still verifies, which is what lets an operator try
	// again instead of having to repair it by hand.
	if err := r.ensureHistory(ctx); err != nil {
		return nil, err
	}
	if err := r.recordDeployment(ctx, current); err != nil {
		return nil, err
	}

	set, err := r.Load()
	if err != nil {
		return nil, err
	}
	byVersion := map[int]Migration{}
	for _, m := range set {
		byVersion[m.Version] = m
	}

	schema := pgstore.QuoteIdent(r.pool.Schema())
	var appliedNow []Migration
	started := time.Now()
	applyErr := func() error {
		for _, pm := range current.Pending {
			m, ok := byVersion[pm.Version]
			if !ok || m.Checksum != pm.Checksum {
				return fmt.Errorf("%w: migration %d changed between plan and apply", ErrPlanStale, pm.Version)
			}
			start := time.Now()
			if m.InTx {
				if err := r.applyInTx(ctx, schema, m, start); err != nil {
					return err
				}
			} else {
				if err := r.applyOutsideTx(ctx, schema, m, start); err != nil {
					return err
				}
			}
			appliedNow = append(appliedNow, m)
		}
		return nil
	}()

	if err := r.writeAudit(ctx, current, appliedNow, time.Since(started), applyErr); err != nil {
		// An audit failure must not hide the outcome it was trying to record.
		if applyErr == nil {
			return appliedNow, err
		}
	}
	return appliedNow, applyErr
}

func short(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// lock takes a session-level advisory lock keyed on the database and schema, so
// two runners against the same schema cannot interleave and two runners against
// different schemas do not block each other.
//
// try, not wait: a runner that blocks silently behind another one is
// indistinguishable from a hung migration, and the operator needs to be told
// which it is.
func (r *Runner) lock(ctx context.Context) (func(), error) {
	// The lock is held on a connection of its own for the whole apply, so a pool
	// that can only hand out one connection would deadlock against itself: the
	// lock holder would never release, and every statement of the migration
	// would wait for it. Refusing here turns a hang into a sentence.
	if r.pool.Pgx().Config().MaxConns < 2 {
		return nil, errors.New("memgw: the migration lock needs a connection of its own; the pool must allow at least 2")
	}
	key := advisoryKey(r.pool.Schema())
	conn, err := r.pool.Pgx().Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("memgw: acquire connection for the migration lock: %w", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&got); err != nil {
		conn.Release()
		return nil, fmt.Errorf("memgw: could not take the migration lock: %w", err)
	}
	if !got {
		conn.Release()
		return nil, fmt.Errorf("%w: schema %q", ErrLockHeld, r.pool.Schema())
	}
	return func() {
		// Best effort: releasing the session lock is also implied by returning
		// the connection, but an explicit unlock keeps a pooled connection from
		// carrying the lock to its next borrower.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, key)
		conn.Release()
	}, nil
}

func advisoryKey(schema string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("memgw:migrations:"))
	_, _ = h.Write([]byte(schema))
	return int64(h.Sum64())
}

func (r *Runner) recordDeployment(ctx context.Context, plan *MigrationPlan) error {
	if plan.Target == nil || plan.Target.Deployment == "" {
		return nil
	}
	ctx, cancel := r.pool.WithQueryTimeout(ctx)
	defer cancel()
	schema := pgstore.QuoteIdent(r.pool.Schema())
	_, err := r.pool.Pgx().Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s.%s (only_row, deployment) VALUES (true, $1) ON CONFLICT (only_row) DO NOTHING`,
		schema, pgstore.QuoteIdent(pgstore.DeploymentTable)), plan.Target.Deployment)
	if err != nil {
		return fmt.Errorf("memgw: could not record the deployment identity: %w", err)
	}
	return nil
}

// writeAudit records the attempt. It stores the plan digest, the versions
// applied and an outcome; it stores no DSN, no credential and no error text
// from the driver, which is where a connection string would surface.
func (r *Runner) writeAudit(ctx context.Context, plan *MigrationPlan, applied []Migration, took time.Duration, applyErr error) error {
	ctx, cancel := r.pool.WithQueryTimeout(context.WithoutCancel(ctx))
	defer cancel()

	versions := make([]int32, 0, len(applied))
	for _, m := range applied {
		versions = append(versions, int32(m.Version))
	}
	outcome, failure := "applied", ""
	if applyErr != nil {
		outcome, failure = "failed", classifyMigrationError(applyErr)
	} else if len(applied) == 0 {
		outcome = "no_op"
	}
	deployment := ""
	if plan.Target != nil {
		deployment = plan.Target.Deployment
	}
	_, err := r.pool.Pgx().Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.migration_audit
		  (actor, deployment, plan_digest, versions, outcome, failure_class, duration_ms)
		VALUES (current_user, $1, $2, $3, $4, $5, $6)`, pgstore.QuoteIdent(r.pool.Schema())),
		deployment, plan.Digest, versions, outcome, failure, took.Milliseconds())
	if err != nil {
		return fmt.Errorf("memgw: could not write the migration audit row: %w", err)
	}
	return nil
}

// classifyMigrationError reduces a failure to a stable class. The driver's
// message is deliberately not stored: a connection error can carry the DSN, and
// an audit table is the last place a credential should come to rest.
func classifyMigrationError(err error) string {
	switch {
	case errors.Is(err, ErrPlanStale):
		return "plan_stale"
	case errors.Is(err, ErrLockHeld):
		return "lock_held"
	case errors.Is(err, ErrChecksumDrift):
		return "checksum_drift"
	case errors.Is(err, ErrDirty):
		return "dirty_schema"
	case errors.Is(err, ErrOutOfOrder):
		return "out_of_order"
	case errors.Is(err, pgstore.ErrTargetMismatch):
		return "target_mismatch"
	case errors.Is(err, pgstore.ErrForeignObject):
		return "foreign_object"
	case errors.Is(err, pgstore.ErrInsecureTransport):
		return "insecure_transport"
	case errors.Is(err, pgstore.ErrProductionNotSelected):
		return "not_selected"
	case errors.Is(err, pgstore.ErrPurposeNotAllowed):
		return "purpose_not_allowed"
	case errors.Is(err, pgstore.ErrUnsafeTarget):
		return "unsafe_target"
	}
	return "migration_failed"
}
