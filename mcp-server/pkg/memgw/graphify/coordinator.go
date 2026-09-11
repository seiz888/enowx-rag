package graphify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Config names one repository and where its index lives.
//
// IndexDir is separate from the repository on purpose. An index written inside
// the tree it describes changes the tree it describes, which makes the dirty
// digest move every time a rebuild runs and every rebuild look stale to the
// next one.
type Config struct {
	ProjectID   uuid.UUID
	WorkspaceID uuid.UUID
	RepoPath    string
	IndexDir    string

	// Keep is how many published generations survive a rebuild. Two is the
	// useful minimum: a reader that opened the previous generation a moment
	// before the swap must still be able to finish reading it.
	Keep int

	// Now is injectable for tests. Nothing else in this package reads a clock.
	Now func() time.Time
}

const (
	currentFileName  = "current.json"
	lockFileName     = "rebuild.lock"
	generationsD     = "generations"
	indexFileName    = "index.ndjson"
	manifestFileName = "manifest.json"
)

// Coordinator owns one repository's index.
type Coordinator struct {
	cfg Config
}

// New checks the configuration and prepares the index directory.
func New(cfg Config) (*Coordinator, error) {
	if strings.TrimSpace(cfg.RepoPath) == "" {
		return nil, errors.New("graphify: a repository path is required")
	}
	if strings.TrimSpace(cfg.IndexDir) == "" {
		return nil, errors.New("graphify: an index directory is required")
	}
	repo, err := filepath.Abs(cfg.RepoPath)
	if err != nil {
		return nil, err
	}
	idx, err := filepath.Abs(cfg.IndexDir)
	if err != nil {
		return nil, err
	}
	if within(idx, repo) {
		// See Config.IndexDir.
		return nil, fmt.Errorf("graphify: the index directory %s is inside the repository it indexes", idx)
	}
	cfg.RepoPath, cfg.IndexDir = repo, idx
	if cfg.Keep <= 0 {
		cfg.Keep = 2
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if err := os.MkdirAll(filepath.Join(idx, generationsD), 0o755); err != nil {
		return nil, err
	}
	return &Coordinator{cfg: cfg}, nil
}

// Rebuild builds one generation and publishes it.
//
// The order is the contract:
//
//  1. take the lock, or return ErrLocked without doing any work;
//  2. read the repository state;
//  3. walk into a directory nobody is reading;
//  4. read the repository state again;
//  5. write the manifest, marking the generation stale if the state moved;
//  6. swap the pointer;
//  7. prune.
//
// Step 4 is the one that is easy to leave out. A rebuild of a large tree takes
// long enough for an editor to save twice, and an index published without the
// recheck claims to describe a tree that no longer exists.
func (c *Coordinator) Rebuild(ctx context.Context) (Manifest, error) {
	lock, err := AcquireLock(filepath.Join(c.cfg.IndexDir, lockFileName))
	if err != nil {
		return Manifest{}, err
	}
	defer lock.Release()

	started := c.cfg.Now()
	before, err := InspectRepo(ctx, c.cfg.RepoPath)
	if err != nil {
		return Manifest{}, err
	}

	gen := generationName(started)
	genDir := filepath.Join(c.cfg.IndexDir, generationsD, gen)
	// A leftover directory under this name is a build that died between
	// creating it and publishing it. It was never pointed at, so nothing can be
	// reading it.
	if err := os.RemoveAll(genDir); err != nil {
		return Manifest{}, err
	}
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		return Manifest{}, err
	}

	entries, bytesSeen, excluded, digest, err := buildIndex(ctx, c.cfg.RepoPath, filepath.Join(genDir, indexFileName))
	if err != nil {
		os.RemoveAll(genDir)
		return Manifest{}, err
	}

	after, err := InspectRepo(ctx, c.cfg.RepoPath)
	if err != nil {
		os.RemoveAll(genDir)
		return Manifest{}, err
	}

	m := Manifest{
		Generation:    gen,
		ProjectID:     c.cfg.ProjectID,
		WorkspaceID:   c.cfg.WorkspaceID,
		RepoPath:      c.cfg.RepoPath,
		Commit:        before.Commit,
		DirtyDigest:   before.DirtyDigest,
		ToolVersion:   ToolVersion,
		SchemaVersion: IndexSchemaVersion,
		GeneratedAt:   started.UTC(),
		BuildMillis:   c.cfg.Now().Sub(started).Milliseconds(),
		FileCount:     entries,
		ByteCount:     bytesSeen,
		IndexDigest:   digest,
		ExcludedCount: excluded,
	}
	if after != before {
		// Published anyway, and published saying so. Discarding the work would
		// mean a repository somebody is actively editing never gets an index at
		// all, which is the case an index is most useful in.
		m.Stale = true
		m.StaleReason = "the working tree changed while the index was being built"
	}

	if err := writeManifest(filepath.Join(genDir, manifestFileName), m); err != nil {
		os.RemoveAll(genDir)
		return Manifest{}, err
	}
	// The pointer swap. One small file replaced by rename: a reader either sees
	// the generation it named before or the one it names after.
	if err := writeManifest(filepath.Join(c.cfg.IndexDir, currentFileName), m); err != nil {
		os.RemoveAll(genDir)
		return Manifest{}, err
	}
	c.prune(gen)
	return m, nil
}

// Status describes what a reader would get right now.
type Status struct {
	Published bool      `json:"published"`
	Manifest  *Manifest `json:"manifest,omitempty"`
	Fresh     bool      `json:"fresh"`
	// Reasons is empty when the index is fresh. It is a list because an index
	// can be behind for more than one reason at once, and telling an operator
	// only the first one they must fix is how a rebuild gets run three times.
	Reasons []string `json:"reasons,omitempty"`
}

// Validate reports whether the published generation still describes the
// repository, and whether it is intact.
//
// "Intact" is checked as well as "current" because the two failures need
// different responses: a generation whose files are missing must be rebuilt
// before it can be read at all, while a generation that is merely behind can
// still answer questions as long as the caller knows to confirm them.
func (c *Coordinator) Validate(ctx context.Context) (Status, error) {
	m, err := readManifest(filepath.Join(c.cfg.IndexDir, currentFileName))
	if errors.Is(err, os.ErrNotExist) {
		return Status{Published: false, Fresh: false, Reasons: []string{"no generation has been published"}}, nil
	}
	if err != nil {
		return Status{}, err
	}
	st := Status{Published: true, Manifest: &m, Fresh: true}
	add := func(r string) {
		st.Fresh = false
		st.Reasons = append(st.Reasons, r)
	}

	if m.Stale {
		add(m.StaleReason)
	}
	if m.SchemaVersion != IndexSchemaVersion {
		add(fmt.Sprintf("the generation was built to schema %s and this build reads %s", m.SchemaVersion, IndexSchemaVersion))
	}
	genDir := filepath.Join(c.cfg.IndexDir, generationsD, m.Generation)
	if _, err := os.Stat(filepath.Join(genDir, indexFileName)); err != nil {
		add("the generation the pointer names is not on disk")
		return st, nil
	}
	gm, err := readManifest(filepath.Join(genDir, manifestFileName))
	if err != nil {
		add("the generation has no readable manifest of its own")
		return st, nil
	}
	if gm.IndexDigest != m.IndexDigest || gm.Generation != m.Generation {
		// The pointer and the generation disagree, which means something wrote
		// one of them by hand. Neither can be trusted.
		add("the published pointer does not match the generation it names")
		return st, nil
	}

	now, err := InspectRepo(ctx, c.cfg.RepoPath)
	if err != nil {
		return st, err
	}
	if now.Commit != m.Commit {
		add(fmt.Sprintf("the repository is at a different commit than the index (%s)", short(m.Commit)))
	}
	if now.DirtyDigest != m.DirtyDigest {
		add("the working tree has changed since the index was built")
	}
	return st, nil
}

// Current returns the published manifest, if there is one.
func (c *Coordinator) Current() (Manifest, bool, error) {
	m, err := readManifest(filepath.Join(c.cfg.IndexDir, currentFileName))
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, false, nil
	}
	if err != nil {
		return Manifest{}, false, err
	}
	return m, true, nil
}

// IndexPath is where the published generation's entries are.
func (c *Coordinator) IndexPath(m Manifest) string {
	return filepath.Join(c.cfg.IndexDir, generationsD, m.Generation, indexFileName)
}

// prune removes old generations, keeping the newest Keep of them and always
// keeping the one that is published.
func (c *Coordinator) prune(keepGen string) {
	dir := filepath.Join(c.cfg.IndexDir, generationsD)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	// Generation names sort in time order by construction, so this is oldest
	// first without stat-ing anything.
	sort.Strings(names)
	for len(names) > c.cfg.Keep {
		victim := names[0]
		names = names[1:]
		if victim == keepGen {
			continue
		}
		os.RemoveAll(filepath.Join(dir, victim))
	}
}

// generationName is sortable, unique per rebuild and readable in a directory
// listing. The nanoseconds are what make two rebuilds in the same second two
// generations.
func generationName(t time.Time) string {
	return t.UTC().Format("20060102T150405.000000000Z")
}

func within(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "no commit"
	}
	return s
}
