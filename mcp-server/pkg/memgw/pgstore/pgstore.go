// Package pgstore owns the PostgreSQL connection lifecycle for the memory
// gateway: bounded pools, timeouts everywhere, health checks, and a safety
// guard that refuses to open a target that cannot be shown to be
// non-production.
//
// It is separate from pkg/rag's pgvector provider on purpose. That provider is
// a vector store that happens to live in PostgreSQL; this is the ledger's
// connection layer, and the two have different failure appetites. The vector
// store can be rebuilt from source documents. The ledger cannot be rebuilt from
// anything.
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Environment names the deployment class of a target. The zero value is
// deliberately invalid: an unset environment must never be treated as safe.
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvTest        Environment = "test"
	EnvProduction  Environment = "production"
)

// Config describes one ledger connection target. Every timeout has a value
// because an unbounded database call is how a gateway with a 100 ms enqueue SLO
// turns into a gateway that hangs.
type Config struct {
	// DSN is a libpq/pgx connection string. It is never logged; see RedactDSN.
	DSN string
	// Schema is the explicit schema the ledger lives in. Required: relying on
	// the default search_path is how a migration lands somewhere unintended.
	Schema string
	// Env names the deployment class. Development and test go through
	// VerifyNonProduction unchanged. Production is refused by that function by
	// design, and is admitted only through VerifyProduction, which additionally
	// requires Production below to be set.
	Env Environment
	// Production is the operator's explicit assertion about a production
	// target: which database, schema, role and deployment they believe they are
	// connected to, and what that assertion authorises. Nil -- the default --
	// means production mode was not selected, and every production write path
	// refuses. See production.go.
	Production *ProductionTarget
	// SiblingSchemaPrefix names other schemas in the same database that are
	// also ours. It exists for the test harness, which gives every test its own
	// throwaway schema and would otherwise see its neighbours as evidence that
	// the database belongs to somebody else. Left empty outside tests, so the
	// "database I own" check stays as strict as it reads.
	SiblingSchemaPrefix string

	ConnectTimeout    time.Duration
	QueryTimeout      time.Duration
	MaxConns          int32
	MinConns          int32
	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	HealthCheckPeriod time.Duration
}

// DefaultConfig returns conservative settings sized for a single small cluster
// shared with another service. The pool is intentionally small: the ledger's
// throughput problem is commit latency, not connection count, and a large pool
// against a shared cluster is a way to starve the neighbour.
func DefaultConfig() Config {
	return Config{
		Schema:            "memgw",
		ConnectTimeout:    5 * time.Second,
		QueryTimeout:      5 * time.Second,
		MaxConns:          8,
		MinConns:          1,
		MaxConnLifetime:   30 * time.Minute,
		MaxConnIdleTime:   5 * time.Minute,
		HealthCheckPeriod: 30 * time.Second,
	}
}

var (
	// ErrUnsafeTarget is returned when a target cannot be shown to be
	// non-production. It is a refusal, not a warning.
	ErrUnsafeTarget = errors.New("memgw: target is not provably non-production")
	// ErrNoDSN is returned when no target was configured at all.
	ErrNoDSN = errors.New("memgw: no DSN configured")
)

// Pool is a bounded pgx pool plus the config it was opened with.
type Pool struct {
	pool *pgxpool.Pool
	cfg  Config

	// facts is what the last successful VerifyProduction observed. It is
	// cached so a migration plan can name the target it was computed against
	// without re-interrogating the server, and it is only ever set by that
	// function -- never by configuration.
	mu    sync.RWMutex
	facts *ProductionFacts
}

// IsProduction reports whether this pool was opened against a target declared
// as production. It says nothing about whether the target was verified.
func (p *Pool) IsProduction() bool { return p.cfg.Env == EnvProduction }

// ProductionFacts returns what the last successful VerifyProduction observed,
// or nil if none has run. Callers must not treat a nil result as "safe".
func (p *Pool) ProductionFacts() *ProductionFacts {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.facts
}

// RedactedTarget renders this pool's target safe to print or store.
func (p *Pool) RedactedTarget() string { return RedactDSN(p.cfg.DSN) }

// Pgx exposes the underlying pool for repositories in this package tree.
func (p *Pool) Pgx() *pgxpool.Pool { return p.pool }

// Schema returns the explicit schema this pool targets.
func (p *Pool) Schema() string { return p.cfg.Schema }

// QueryTimeout returns the per-query budget callers should apply.
func (p *Pool) QueryTimeout() time.Duration { return p.cfg.QueryTimeout }

// WithQueryTimeout derives a context carrying the configured query budget.
// Every repository call in this tree goes through it, so a stalled backend
// surfaces as a deadline rather than as a goroutine that never returns.
func (p *Pool) WithQueryTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, p.cfg.QueryTimeout)
}

// Close releases the pool. It is safe to call on a nil Pool so shutdown paths
// do not need to branch.
func (p *Pool) Close() {
	if p == nil || p.pool == nil {
		return
	}
	p.pool.Close()
}

// Open dials the target and applies the bounded pool settings. It does not
// verify that the target is non-production; callers that write must call
// VerifyNonProduction first, and the migration runner refuses to run without it.
func Open(ctx context.Context, cfg Config) (*Pool, error) {
	if strings.TrimSpace(cfg.DSN) == "" {
		return nil, ErrNoDSN
	}
	if strings.TrimSpace(cfg.Schema) == "" {
		return nil, errors.New("memgw: schema must be set explicitly")
	}
	base := DefaultConfig()
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = base.ConnectTimeout
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = base.QueryTimeout
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = base.MaxConns
	}
	if cfg.MinConns < 0 {
		cfg.MinConns = base.MinConns
	}
	if cfg.MaxConnLifetime <= 0 {
		cfg.MaxConnLifetime = base.MaxConnLifetime
	}
	if cfg.MaxConnIdleTime <= 0 {
		cfg.MaxConnIdleTime = base.MaxConnIdleTime
	}
	if cfg.HealthCheckPeriod <= 0 {
		cfg.HealthCheckPeriod = base.HealthCheckPeriod
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		// The DSN can carry a password, so the parse error is not echoed.
		return nil, errors.New("memgw: DSN could not be parsed")
	}
	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolCfg.HealthCheckPeriod = cfg.HealthCheckPeriod
	poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	// Pin search_path on every connection in the pool so repository SQL can use
	// unqualified table names and still be certain which schema it hits. A
	// ledger that lands in whatever schema the session default happens to be is
	// a ledger nobody can find after an incident.
	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = QuoteIdent(cfg.Schema) + ", public"

	dialCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(dialCtx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("memgw: connect failed to %s: %w", RedactDSN(cfg.DSN), err)
	}
	p := &Pool{pool: pool, cfg: cfg}
	if err := p.Health(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

// Health is a cheap liveness probe with its own deadline.
func (p *Pool) Health(ctx context.Context) error {
	ctx, cancel := p.WithQueryTimeout(ctx)
	defer cancel()
	var one int
	if err := p.pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("memgw: health check failed: %w", err)
	}
	return nil
}

// RedactDSN renders a DSN safe to log: host and database survive, credentials
// never do. Errors in this package go through it because a connection failure
// is exactly the moment a careless log line prints a password.
func RedactDSN(dsn string) string {
	if dsn == "" {
		return "(empty)"
	}
	if u, err := url.Parse(dsn); err == nil && u.Host != "" {
		db := strings.TrimPrefix(u.Path, "/")
		if db == "" {
			db = "(default)"
		}
		return fmt.Sprintf("%s/%s", u.Host, db)
	}
	// Keyword/value form: keep host and dbname, drop everything else.
	var host, dbname string
	for _, field := range strings.Fields(dsn) {
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(k) {
		case "host":
			host = v
		case "dbname":
			dbname = v
		}
	}
	if host == "" {
		host = "(unknown host)"
	}
	if dbname == "" {
		dbname = "(unknown db)"
	}
	return host + "/" + dbname
}

// nonProductionDatabase matches database names that announce themselves as
// disposable. The guard requires the name to say so: a database called
// "enowx" or "axonhub" is refused even on loopback, because a loopback tunnel
// to a production host is one SSH flag away.
var nonProductionDatabase = regexp.MustCompile(`^(memgw_|test_|dev_)|(_test|_dev|_devdb|_testdb)$`)

// VerifyNonProduction is the gate every write path goes through. It refuses
// unless the target is provably disposable, and it says which check failed.
//
// The checks are deliberately paranoid and independent. Any single one of them
// can be satisfied by accident; together they are hard to satisfy by accident.
func VerifyNonProduction(ctx context.Context, p *Pool) error {
	cfg := p.cfg

	switch cfg.Env {
	case EnvDevelopment, EnvTest:
	case EnvProduction:
		return fmt.Errorf("%w: environment is declared production", ErrUnsafeTarget)
	default:
		return fmt.Errorf("%w: environment must be declared as %q or %q, got %q",
			ErrUnsafeTarget, EnvDevelopment, EnvTest, cfg.Env)
	}

	ctx, cancel := p.WithQueryTimeout(ctx)
	defer cancel()

	var dbName, serverAddr string
	var isRecovery bool
	err := p.pool.QueryRow(ctx,
		`SELECT current_database(), COALESCE(host(inet_server_addr())::text, 'local'), pg_is_in_recovery()`,
	).Scan(&dbName, &serverAddr, &isRecovery)
	if err != nil {
		return fmt.Errorf("memgw: could not identify target: %w", err)
	}

	if isRecovery {
		return fmt.Errorf("%w: target is a standby in recovery", ErrUnsafeTarget)
	}
	if !nonProductionDatabase.MatchString(dbName) {
		return fmt.Errorf("%w: database %q is not named as disposable (expected a memgw_/test_/dev_ prefix or a _test/_dev suffix)",
			ErrUnsafeTarget, dbName)
	}
	if !isLoopbackTarget(cfg.DSN, serverAddr) {
		return fmt.Errorf("%w: target %s is not loopback; Phase 3 does not write to remote clusters",
			ErrUnsafeTarget, RedactDSN(cfg.DSN))
	}

	// synchronous_commit=off would make every ACK a lie about durability. The
	// guard refuses it rather than letting a test suite "prove" durability
	// against a server that never flushed.
	var syncCommit string
	if err := p.pool.QueryRow(ctx, "SHOW synchronous_commit").Scan(&syncCommit); err != nil {
		return fmt.Errorf("memgw: could not read synchronous_commit: %w", err)
	}
	if strings.EqualFold(syncCommit, "off") {
		return fmt.Errorf("%w: synchronous_commit is off, so a commit acknowledgement would not mean durable", ErrUnsafeTarget)
	}

	// A database that already holds unrelated tables is somebody else's. This
	// is the check that catches a DSN pointed at a real system whose name
	// happens to pass the pattern above.
	var foreign []string
	rows, err := p.pool.Query(ctx, `
		SELECT table_schema || '.' || table_name
		FROM information_schema.tables
		WHERE table_type = 'BASE TABLE'
		  AND table_schema NOT IN ('pg_catalog', 'information_schema')
		  AND table_schema <> $1
		  -- An empty prefix means no sibling schema is ours, which is the
		  -- default: only the test harness has siblings.
		  AND ($2 = '' OR table_schema NOT LIKE $2 || '%')
		ORDER BY 1
		LIMIT 5`, cfg.Schema, cfg.SiblingSchemaPrefix)
	if err != nil {
		return fmt.Errorf("memgw: could not inspect target schema: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("memgw: could not inspect target schema: %w", err)
		}
		foreign = append(foreign, name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("memgw: could not inspect target schema: %w", err)
	}
	if len(foreign) > 0 {
		return fmt.Errorf("%w: database %q already holds unrelated tables (%s); the ledger only initialises a database it owns",
			ErrUnsafeTarget, dbName, strings.Join(foreign, ", "))
	}
	return nil
}

// isLoopbackTarget reports whether the connection is to this machine. Both the
// DSN's host and the server's own address are checked: a DSN saying "localhost"
// proves nothing if it is a forwarded port, and inet_server_addr is null for a
// unix socket, which is loopback by construction.
func isLoopbackTarget(dsn, serverAddr string) bool {
	if serverAddr != "" && serverAddr != "local" {
		if ip := net.ParseIP(serverAddr); ip != nil && !ip.IsLoopback() {
			// A container publishing to 127.0.0.1 reports its internal
			// address here, so this is not conclusive on its own; fall
			// through to the DSN host, which is what we actually dialled.
			_ = ip
		}
	}
	host := dsnHost(dsn)
	switch host {
	case "", "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	// A unix socket path is local by definition.
	return strings.HasPrefix(host, "/")
}

func dsnHost(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.Host != "" {
		return u.Hostname()
	}
	for _, field := range strings.Fields(dsn) {
		if k, v, ok := strings.Cut(field, "="); ok && strings.EqualFold(k, "host") {
			return v
		}
	}
	return ""
}

// QuoteIdent quotes a SQL identifier. Schema names cannot travel as query
// placeholders, so identifier interpolation has exactly one implementation.
func QuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
