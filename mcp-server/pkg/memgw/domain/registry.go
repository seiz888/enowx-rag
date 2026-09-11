package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/enowdev/enowx-rag/pkg/memgw/errclass"
	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
)

// DB is the subset of pgx that a pool and a transaction both satisfy, so the
// same statement serves the registry API and the in-transaction domain apply.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Project is a unit of authority. Ids are stable across folder renames, which
// is the reason a registry exists rather than a convention over basenames.
type Project struct {
	ID          uuid.UUID
	Slug        string
	DisplayName string
	Status      string
	CreatedAt   time.Time
}

// Workspace is one checkout on one host.
type Workspace struct {
	ID             uuid.UUID
	ProjectID      uuid.UUID
	HostID         uuid.UUID
	RootPathDigest string
	Label          string
	CreatedAt      time.Time
}

// Work is the materialised work aggregate.
type Work struct {
	ID           uuid.UUID
	ProjectID    uuid.UUID
	WorkspaceID  uuid.UUID
	Title        string
	State        string
	Revision     int64
	LastEventSeq int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Tombstoned   bool
}

// ErrNotFound is returned by the registry lookups. It is deliberately not
// classified: whether a missing project is a scope denial or a bad request is
// the caller's decision, and answering it here would leak existence.
var ErrNotFound = errors.New("memgw: not found")

// Registry reads and writes the project/workspace registry.
type Registry struct {
	pool *pgstore.Pool
}

// NewRegistry returns a registry over the given pool.
func NewRegistry(pool *pgstore.Pool) *Registry { return &Registry{pool: pool} }

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// EnsureProject creates a project or returns the existing one with that slug.
//
// It is idempotent by slug rather than by id, because the caller that knows the
// slug is a host that just opened a folder and does not know what id the
// project was given the first time.
func (r *Registry) EnsureProject(ctx context.Context, p Project) (Project, error) {
	if !slugPattern.MatchString(p.Slug) {
		return Project{}, errclass.New(errclass.PolicyRejected,
			"project slug must be lowercase alphanumeric with . _ - and at most 64 bytes")
	}
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	if p.Status == "" {
		p.Status = "active"
	}
	ctx, cancel := r.pool.WithQueryTimeout(ctx)
	defer cancel()
	err := r.pool.Pgx().QueryRow(ctx, `
		INSERT INTO projects (project_id, slug, display_name, status)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (slug) DO UPDATE SET slug = EXCLUDED.slug
		RETURNING project_id, slug, display_name, status, created_at`,
		p.ID, p.Slug, p.DisplayName, p.Status,
	).Scan(&p.ID, &p.Slug, &p.DisplayName, &p.Status, &p.CreatedAt)
	if err != nil {
		return Project{}, fmt.Errorf("memgw: ensure project: %w", err)
	}
	return p, nil
}

// Project loads one project by id.
func (r *Registry) Project(ctx context.Context, id uuid.UUID) (Project, error) {
	ctx, cancel := r.pool.WithQueryTimeout(ctx)
	defer cancel()
	var p Project
	err := r.pool.Pgx().QueryRow(ctx,
		`SELECT project_id, slug, display_name, status, created_at FROM projects WHERE project_id = $1`, id,
	).Scan(&p.ID, &p.Slug, &p.DisplayName, &p.Status, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("memgw: read project: %w", err)
	}
	return p, nil
}

// EnsureWorkspace creates a workspace or returns the one already registered for
// that host and root digest.
func (r *Registry) EnsureWorkspace(ctx context.Context, w Workspace) (Workspace, error) {
	if len(w.RootPathDigest) != 64 {
		return Workspace{}, errclass.New(errclass.PolicyRejected,
			"workspace root must be given as a sha256 digest, not as a path")
	}
	if w.ID == uuid.Nil {
		w.ID = uuid.New()
	}
	ctx, cancel := r.pool.WithQueryTimeout(ctx)
	defer cancel()
	err := r.pool.Pgx().QueryRow(ctx, `
		INSERT INTO workspaces (workspace_id, project_id, host_id, root_path_digest, label)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (host_id, root_path_digest) DO UPDATE SET label = workspaces.label
		RETURNING workspace_id, project_id, host_id, root_path_digest, label, created_at`,
		w.ID, w.ProjectID, w.HostID, w.RootPathDigest, w.Label,
	).Scan(&w.ID, &w.ProjectID, &w.HostID, &w.RootPathDigest, &w.Label, &w.CreatedAt)
	if err != nil {
		return Workspace{}, fmt.Errorf("memgw: ensure workspace: %w", err)
	}
	return w, nil
}

// WorkspaceByRoot finds a workspace by host and root digest.
func (r *Registry) WorkspaceByRoot(ctx context.Context, hostID uuid.UUID, rootDigest string) (Workspace, error) {
	ctx, cancel := r.pool.WithQueryTimeout(ctx)
	defer cancel()
	var w Workspace
	err := r.pool.Pgx().QueryRow(ctx, `
		SELECT workspace_id, project_id, host_id, root_path_digest, label, created_at
		FROM workspaces WHERE host_id = $1 AND root_path_digest = $2`, hostID, rootDigest,
	).Scan(&w.ID, &w.ProjectID, &w.HostID, &w.RootPathDigest, &w.Label, &w.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Workspace{}, ErrNotFound
	}
	if err != nil {
		return Workspace{}, fmt.Errorf("memgw: read workspace: %w", err)
	}
	return w, nil
}

// MapRepo records that a repository identity belongs to a project.
func (r *Registry) MapRepo(ctx context.Context, repoKey string, projectID uuid.UUID) error {
	key, err := NormaliseRepoKey(repoKey)
	if err != nil {
		return err
	}
	ctx, cancel := r.pool.WithQueryTimeout(ctx)
	defer cancel()
	if _, err := r.pool.Pgx().Exec(ctx, `
		INSERT INTO project_repos (repo_key, project_id) VALUES ($1,$2)
		ON CONFLICT (repo_key) DO UPDATE SET project_id = EXCLUDED.project_id`,
		key, projectID); err != nil {
		return fmt.Errorf("memgw: map repo: %w", err)
	}
	return nil
}

// ProjectByRepo resolves a repository identity to its project.
func (r *Registry) ProjectByRepo(ctx context.Context, repoKey string) (Project, error) {
	key, err := NormaliseRepoKey(repoKey)
	if err != nil {
		return Project{}, err
	}
	ctx, cancel := r.pool.WithQueryTimeout(ctx)
	defer cancel()
	var p Project
	err = r.pool.Pgx().QueryRow(ctx, `
		SELECT p.project_id, p.slug, p.display_name, p.status, p.created_at
		FROM project_repos r JOIN projects p ON p.project_id = r.project_id
		WHERE r.repo_key = $1`, key,
	).Scan(&p.ID, &p.Slug, &p.DisplayName, &p.Status, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("memgw: read project by repo: %w", err)
	}
	return p, nil
}

// Work loads one materialised work.
func (r *Registry) Work(ctx context.Context, id uuid.UUID) (Work, error) {
	ctx, cancel := r.pool.WithQueryTimeout(ctx)
	defer cancel()
	return readWork(ctx, r.pool.Pgx(), id)
}

// CurrentWork returns the most recently updated work for a project and
// workspace that is still in a non-terminal state. Completed and abandoned work
// is deliberately not returned: a handover against finished work is refused by
// the ledger, so "the current work" is by definition one that is still open.
//
// When more than one work is open the most recently updated one wins, so two
// hosts that both opened sessions in the same project and workspace converge on
// the same work rather than each silently creating a second. That convergence
// is the whole point: work is shared, and a second open work is a second
// opinion about what is "current", not a second place to file.
func (r *Registry) CurrentWork(ctx context.Context, projectID, workspaceID uuid.UUID) (Work, error) {
	ctx, cancel := r.pool.WithQueryTimeout(ctx)
	defer cancel()
	var w Work
	var tombstoned *time.Time
	err := r.pool.Pgx().QueryRow(ctx, `
		SELECT work_id, project_id, workspace_id, title, state, revision, last_event_seq,
		       created_at, updated_at, tombstoned_at
		FROM works
		WHERE project_id = $1 AND workspace_id = $2
		  AND state IN ('planned','active','blocked','review')
		ORDER BY updated_at DESC
		LIMIT 1`,
		projectID, workspaceID,
	).Scan(&w.ID, &w.ProjectID, &w.WorkspaceID, &w.Title, &w.State, &w.Revision,
		&w.LastEventSeq, &w.CreatedAt, &w.UpdatedAt, &tombstoned)
	if errors.Is(err, pgx.ErrNoRows) {
		return Work{}, ErrNotFound
	}
	if err != nil {
		return Work{}, fmt.Errorf("memgw: read current work: %w", err)
	}
	w.Tombstoned = tombstoned != nil
	return w, nil
}

func readWork(ctx context.Context, db DB, id uuid.UUID) (Work, error) {
	var w Work
	var tombstoned *time.Time
	err := db.QueryRow(ctx, `
		SELECT work_id, project_id, workspace_id, title, state, revision, last_event_seq,
		       created_at, updated_at, tombstoned_at
		FROM works WHERE work_id = $1`, id,
	).Scan(&w.ID, &w.ProjectID, &w.WorkspaceID, &w.Title, &w.State, &w.Revision,
		&w.LastEventSeq, &w.CreatedAt, &w.UpdatedAt, &tombstoned)
	if errors.Is(err, pgx.ErrNoRows) {
		return Work{}, ErrNotFound
	}
	if err != nil {
		return Work{}, fmt.Errorf("memgw: read work: %w", err)
	}
	w.Tombstoned = tombstoned != nil
	return w, nil
}

// NormaliseRepoKey reduces a git remote to a stable host/path identity.
//
// Credentials are stripped rather than rejected: a remote URL with a token in
// it is a normal thing to find in a checkout, and the registry's job is to
// record which project it is, not to keep the token. The result is checked
// against the same shape the column constrains, so a URL that cannot be
// reduced to host/path is refused instead of stored half-parsed.
func NormaliseRepoKey(remote string) (string, error) {
	s := strings.TrimSpace(remote)
	if s == "" {
		return "", errclass.New(errclass.PolicyRejected, "repository identity is empty")
	}
	// scp-style: git@host:owner/repo.git
	if !strings.Contains(s, "://") {
		if at := strings.LastIndex(s, "@"); at >= 0 {
			s = s[at+1:]
		}
		s = strings.Replace(s, ":", "/", 1)
	} else {
		u, err := url.Parse(s)
		if err != nil {
			return "", errclass.New(errclass.PolicyRejected, "repository identity is not a parseable remote")
		}
		// u.User is dropped here and never stored anywhere.
		s = u.Host + u.Path
	}
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimSuffix(s, "/")
	s = strings.ToLower(path.Clean(s))
	if !repoKeyPattern.MatchString(s) {
		return "", errclass.New(errclass.PolicyRejected, "repository identity does not reduce to host/path")
	}
	return s, nil
}

var repoKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*(:[0-9]+)?(/[A-Za-z0-9._-]+)+$`)

// RootPathDigest is how a workspace root is identified without storing it. A
// path contains a user name, and the registry is read by every host that shares
// the project.
func RootPathDigest(root string) string {
	cleaned := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(root), `\`, `/`))
	cleaned = strings.TrimSuffix(cleaned, "/")
	sum := sha256.Sum256([]byte(cleaned))
	return hex.EncodeToString(sum[:])
}
