package graphify

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The repositories these tests build are made here, file by file. Nothing is
// copied from a real checkout: an index is a list of every path in a tree, and
// a fixture taken from a working repository would put this machine's directory
// layout into a test file and, on a bad day, into a failure message. The files
// that stand in for secret material below contain no secret material -- what is
// being tested is that a path with that shape is never opened.

func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "main.go", "package main\n\nfunc main() {}\n")
	write(t, dir, "internal/store/store.go", "package store\n\ntype Store struct{}\n")
	write(t, dir, "README.md", "# fixture\n")
	return dir
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newCoordinator(t *testing.T, repo string) *Coordinator {
	t.Helper()
	c, err := New(Config{
		ProjectID:   uuid.MustParse("7627423a-fd6e-459d-87c0-be32cd47c8cb"),
		WorkspaceID: uuid.MustParse("20bc010b-9676-4d03-a6de-9812bf195f2f"),
		RepoPath:    repo,
		IndexDir:    t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func paths(t *testing.T, c *Coordinator, m Manifest) map[string]Entry {
	t.Helper()
	f, err := os.Open(c.IndexPath(m))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]Entry{}
	if err := ReadIndex(f, func(e Entry) error {
		out[e.Path] = e
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRebuildPublishesAndValidates is the base case: a tree that is not moving
// produces an index that says it is fresh, and says it again after a second
// look.
func TestRebuildPublishesAndValidates(t *testing.T) {
	repo := newRepo(t)
	c := newCoordinator(t, repo)

	m, err := c.Rebuild(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if m.FileCount != 3 {
		t.Fatalf("indexed %d files, want 3", m.FileCount)
	}
	if m.Stale {
		t.Fatalf("a still tree produced a stale generation: %s", m.StaleReason)
	}
	if m.IndexDigest == "" || m.ToolVersion != ToolVersion || m.SchemaVersion != IndexSchemaVersion {
		t.Fatalf("the manifest does not identify what built it: %+v", m)
	}

	got := paths(t, c, m)
	for _, want := range []string{"main.go", "internal/store/store.go", "README.md"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("%s is missing from the index", want)
		}
	}
	if got["main.go"].Language != "go" {
		t.Fatalf("main.go was not recognised as go")
	}

	st, err := c.Validate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Published || !st.Fresh {
		t.Fatalf("a fresh index did not validate: %+v", st.Reasons)
	}

	// The same tree twice is the same digest. Without that, "has anything
	// changed" cannot be answered without a diff.
	m2, err := c.Rebuild(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if m2.IndexDigest != m.IndexDigest {
		t.Fatalf("two builds of one tree produced different digests")
	}
	if m2.Generation == m.Generation {
		t.Fatalf("two rebuilds reused one generation name")
	}
}

// TestTwoConcurrentRebuildsDoNotBothRun: one coordinator per repository is the
// requirement, and it has to hold when the second rebuild is a second process
// that knows nothing about the first.
func TestTwoConcurrentRebuildsDoNotBothRun(t *testing.T) {
	repo := newRepo(t)
	c := newCoordinator(t, repo)

	held, err := AcquireLock(filepath.Join(c.cfg.IndexDir, lockFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Rebuild(t.Context()); !errors.Is(err, ErrLocked) {
		held.Release()
		t.Fatalf("a rebuild ran while the lock was held: %v", err)
	}
	// Nothing was published, and nothing half-built was left behind.
	if _, ok, _ := c.Current(); ok {
		held.Release()
		t.Fatal("a refused rebuild published a generation")
	}
	ents, _ := os.ReadDir(filepath.Join(c.cfg.IndexDir, generationsD))
	if len(ents) != 0 {
		held.Release()
		t.Fatalf("a refused rebuild left %d generation directories", len(ents))
	}

	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Rebuild(t.Context()); err != nil {
		t.Fatalf("the rebuild did not proceed after the lock was released: %v", err)
	}
}

// TestConcurrentRebuildsLeaveOneCoherentIndex: many goroutines, one repository.
// Exactly one may win at a time, and whatever is published at the end must be a
// complete generation -- never a pointer at a directory that is still being
// written.
func TestConcurrentRebuildsLeaveOneCoherentIndex(t *testing.T) {
	repo := newRepo(t)
	c := newCoordinator(t, repo)

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ran, locked int
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Rebuild(context.Background())
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ran++
			case errors.Is(err, ErrLocked):
				locked++
			default:
				t.Errorf("unexpected rebuild error: %v", err)
			}
		}()
	}
	wg.Wait()
	if ran == 0 {
		t.Fatal("no rebuild succeeded")
	}
	if ran+locked != n {
		t.Fatalf("%d ran, %d were locked out, want %d in total", ran, locked, n)
	}

	st, err := c.Validate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Published || !st.Fresh {
		t.Fatalf("after concurrent rebuilds the index is not usable: %+v", st.Reasons)
	}
	if len(paths(t, c, *st.Manifest)) != 3 {
		t.Fatal("the published generation is not a complete index")
	}
}

// TestALockDiesWithItsProcess is the crash case. A lock held by a process that
// is killed must be gone, with no file to clean up and no procedure to follow.
// The child is this test binary re-invoked, so what is being tested is a real
// process holding a real OS lock and being killed.
func TestALockDiesWithItsProcess(t *testing.T) {
	if os.Getenv("MEMGW_GRAPHIFY_LOCK_CHILD") != "" {
		// Child: take the lock, say so, and wait to be killed.
		l, err := AcquireLock(os.Getenv("MEMGW_GRAPHIFY_LOCK_PATH"))
		if err != nil {
			os.Stdout.WriteString("failed\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("held\n")
		_ = l
		select {}
	}

	repo := newRepo(t)
	c := newCoordinator(t, repo)
	lockPath := filepath.Join(c.cfg.IndexDir, lockFileName)

	cmd := exec.Command(os.Args[0], "-test.run", "TestALockDiesWithItsProcess", "-test.timeout", "60s")
	cmd.Env = append(os.Environ(),
		"MEMGW_GRAPHIFY_LOCK_CHILD=1",
		"MEMGW_GRAPHIFY_LOCK_PATH="+lockPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	buf := make([]byte, 16)
	deadline := time.Now().Add(30 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		n, rerr := stdout.Read(buf)
		if n > 0 {
			got += string(buf[:n])
		}
		if strings.Contains(got, "held") || rerr != nil {
			break
		}
	}
	if !strings.Contains(got, "held") {
		t.Fatalf("the child never reported holding the lock, it said %q", got)
	}

	// While the child holds it, this process cannot rebuild.
	if _, err := c.Rebuild(t.Context()); !errors.Is(err, ErrLocked) {
		t.Fatalf("the lock did not exclude another process: %v", err)
	}

	// Kill it the way a crash would, with no chance to release anything.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()

	// The lock file is still there; that is the point. It must not matter.
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("the lock file vanished, so this is not testing what it claims: %v", err)
	}
	var lastErr error
	for i := 0; i < 50; i++ {
		if _, lastErr = c.Rebuild(t.Context()); lastErr == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the lock survived the process that held it: %v", lastErr)
}

// TestASourceChangeDuringABuildPublishesStale: the recheck. An index built
// while the tree moves is still published -- a repository being edited is
// exactly when an index is wanted -- but it says it is behind, and Validate
// repeats that rather than deciding it looks fine.
func TestASourceChangeDuringABuildPublishesStale(t *testing.T) {
	repo := newRepo(t)
	c := newCoordinator(t, repo)

	// A build long enough to change something underneath: the walk reads every
	// file, so a large tree is the honest way to buy the time.
	for i := 0; i < 400; i++ {
		write(t, repo, "pkg/gen/f"+itoa(i)+".go", strings.Repeat("// filler\n", 200))
	}

	done := make(chan Manifest, 1)
	errc := make(chan error, 1)
	go func() {
		m, err := c.Rebuild(context.Background())
		if err != nil {
			errc <- err
			return
		}
		done <- m
	}()
	// Keep the tree moving for as long as the build runs.
	stop := make(chan struct{})
	go func() {
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				p := filepath.Join(repo, "moving.go")
				_ = os.WriteFile(p, []byte("package moving\n\n// "+itoa(i)+"\n"), 0o644)
				i++
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	var m Manifest
	select {
	case m = <-done:
	case err := <-errc:
		close(stop)
		t.Fatal(err)
	case <-time.After(90 * time.Second):
		close(stop)
		t.Fatal("the rebuild did not finish")
	}
	close(stop)

	if !m.Stale {
		t.Skip("the build finished before the tree moved; nothing to assert about the recheck")
	}
	if m.StaleReason == "" {
		t.Fatal("a stale generation with no reason is a stale generation nobody can act on")
	}
	st, err := c.Validate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Fresh {
		t.Fatal("Validate called a stale generation fresh")
	}
	// Stale is not broken: the entries are still readable.
	if len(paths(t, c, *st.Manifest)) == 0 {
		t.Fatal("a stale generation published no entries")
	}
}

// TestDeletedAndRenamedFilesLeaveTheIndex: an index that keeps a deleted path
// is worse than no index, because the answer it gives is a file that is not
// there. A rename must move, not duplicate.
func TestDeletedAndRenamedFilesLeaveTheIndex(t *testing.T) {
	repo := newRepo(t)
	c := newCoordinator(t, repo)

	m, err := c.Rebuild(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := paths(t, c, m)["internal/store/store.go"]; !ok {
		t.Fatal("the fixture was not indexed")
	}

	if err := os.Remove(filepath.Join(repo, "README.md")); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(repo, "internal", "store", "store.go")
	newPath := filepath.Join(repo, "internal", "store", "repository.go")
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}

	m2, err := c.Rebuild(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := paths(t, c, m2)
	if _, ok := got["README.md"]; ok {
		t.Fatal("a deleted file is still in the index")
	}
	if _, ok := got["internal/store/store.go"]; ok {
		t.Fatal("the old name of a renamed file is still in the index")
	}
	if _, ok := got["internal/store/repository.go"]; !ok {
		t.Fatal("the new name of a renamed file is not in the index")
	}
	if m2.IndexDigest == m.IndexDigest {
		t.Fatal("a delete and a rename did not change the index digest")
	}
}

// TestSecretPathsAreNeverIndexed: the exclusion is on the walk. A path that is
// counted but not named is still a path that was opened and read.
func TestSecretPathsAreNeverIndexed(t *testing.T) {
	repo := newRepo(t)
	write(t, repo, ".env", "PLACEHOLDER=this file is a fixture and holds nothing\n")
	write(t, repo, ".env.production", "PLACEHOLDER=fixture\n")
	write(t, repo, "deploy/server.key", "fixture, not key material\n")
	write(t, repo, "config/secrets.yaml", "placeholder: fixture\n")
	write(t, repo, "secrets/db.txt", "fixture\n")
	write(t, repo, "node_modules/left-pad/index.js", "module.exports = 1\n")
	write(t, repo, "app/service_token.go", "package app\n")

	c := newCoordinator(t, repo)
	m, err := c.Rebuild(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := paths(t, c, m)
	for _, must := range []string{
		".env", ".env.production", "deploy/server.key", "config/secrets.yaml",
		"secrets/db.txt", "node_modules/left-pad/index.js", "app/service_token.go",
	} {
		if _, ok := got[must]; ok {
			t.Fatalf("%s was indexed", must)
		}
	}
	if _, ok := got["main.go"]; !ok {
		t.Fatal("the exclusion rules removed ordinary source")
	}
	if m.ExcludedCount == 0 {
		t.Fatal("the manifest does not report that anything was excluded")
	}
}

// TestTheIndexMayNotLiveInsideTheRepository: an index written into the tree it
// describes changes that tree, so every rebuild would see the previous one as a
// modification and no generation could ever be fresh.
func TestTheIndexMayNotLiveInsideTheRepository(t *testing.T) {
	repo := newRepo(t)
	if _, err := New(Config{RepoPath: repo, IndexDir: filepath.Join(repo, ".graphify")}); err == nil {
		t.Fatal("an index inside the repository was accepted")
	}
	if _, err := New(Config{RepoPath: repo, IndexDir: repo}); err == nil {
		t.Fatal("an index directory equal to the repository was accepted")
	}
}

// TestValidateCatchesAGenerationThatIsGone: the pointer and the generation are
// two files, and something can delete one of them. A reader must be told the
// index is unusable rather than handed a missing-file error at query time.
func TestValidateCatchesAGenerationThatIsGone(t *testing.T) {
	repo := newRepo(t)
	c := newCoordinator(t, repo)
	m, err := c.Rebuild(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(c.cfg.IndexDir, generationsD, m.Generation)); err != nil {
		t.Fatal(err)
	}
	st, err := c.Validate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Fresh {
		t.Fatal("Validate called a missing generation fresh")
	}
	if len(st.Reasons) == 0 || !strings.Contains(strings.Join(st.Reasons, " "), "not on disk") {
		t.Fatalf("the reason does not say what is wrong: %+v", st.Reasons)
	}
}

// TestNoGenerationYetIsNotAnError: a repository nobody has indexed is a normal
// state, and reporting it as a failure would make every first run look broken.
func TestNoGenerationYetIsNotAnError(t *testing.T) {
	c := newCoordinator(t, newRepo(t))
	st, err := c.Validate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Published || st.Fresh || len(st.Reasons) != 1 {
		t.Fatalf("an unindexed repository reported %+v", st)
	}
}

// TestPruneKeepsThePublishedGeneration: old generations are removed, but never
// the one the pointer names.
func TestPruneKeepsThePublishedGeneration(t *testing.T) {
	repo := newRepo(t)
	c := newCoordinator(t, repo)
	var last Manifest
	for i := 0; i < 5; i++ {
		write(t, repo, "main.go", "package main\n\nfunc main() { _ = "+itoa(i)+" }\n")
		m, err := c.Rebuild(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		last = m
	}
	ents, err := os.ReadDir(filepath.Join(c.cfg.IndexDir, generationsD))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) > 2 {
		t.Fatalf("%d generations kept, want at most 2", len(ents))
	}
	found := false
	for _, e := range ents {
		if e.Name() == last.Generation {
			found = true
		}
	}
	if !found {
		t.Fatal("the published generation was pruned")
	}
	st, err := c.Validate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Fresh {
		t.Fatalf("after pruning the index is not usable: %+v", st.Reasons)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// --- git-backed freshness ----------------------------------------------------

// gitRepo makes a real checkout in a temporary directory. It is a real one
// because the thing being tested is what git reports, and a fake would be a
// test of the fake.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	dir := newRepo(t)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid",
			"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid",
			"GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "--quiet")
	run("add", "-A")
	run("commit", "--quiet", "-m", "fixture")
	return dir
}

// TestADirtyWorktreeIsPartOfTheIdentity: the commit alone would say the index
// is fresh for a tree it has never seen, because an agent is nearly always
// looking at uncommitted work.
func TestADirtyWorktreeIsPartOfTheIdentity(t *testing.T) {
	repo := gitRepo(t)
	c := newCoordinator(t, repo)

	clean, err := c.Rebuild(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if clean.Commit == "" {
		t.Fatal("the manifest does not name the commit it was built from")
	}
	if clean.DirtyDigest != "clean" {
		t.Fatalf("a committed tree reported dirty: %s", clean.DirtyDigest)
	}
	if st, err := c.Validate(t.Context()); err != nil || !st.Fresh {
		t.Fatalf("a clean checkout did not validate: %v %+v", err, st.Reasons)
	}

	// One uncommitted edit, same commit. The index must stop claiming freshness.
	write(t, repo, "main.go", "package main\n\nfunc main() { println(1) }\n")
	st, err := c.Validate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Fresh {
		t.Fatal("an edited working tree still validated as fresh")
	}
	if !strings.Contains(strings.Join(st.Reasons, " "), "working tree has changed") {
		t.Fatalf("the reason does not name the working tree: %+v", st.Reasons)
	}
	if st.Manifest.Commit != clean.Commit {
		t.Fatal("the commit changed, so this is not testing a dirty tree")
	}

	// Rebuilt against the dirty tree: fresh again, and now carrying a digest.
	dirty, err := c.Rebuild(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if dirty.Commit != clean.Commit {
		t.Fatal("the commit moved")
	}
	if dirty.DirtyDigest == "clean" || dirty.DirtyDigest == clean.DirtyDigest {
		t.Fatalf("the dirty tree produced digest %q", dirty.DirtyDigest)
	}
	if st, err := c.Validate(t.Context()); err != nil || !st.Fresh {
		t.Fatalf("the rebuilt index did not validate: %v %+v", err, st.Reasons)
	}

	// An untracked file counts too: it is source the agent can read.
	write(t, repo, "scratch.go", "package main\n")
	st2, err := c.Validate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st2.Fresh {
		t.Fatal("an untracked file did not make the index stale")
	}
}

// TestAnIndexIsStaleAtADifferentCommit: the other half of the identity.
func TestAnIndexIsStaleAtADifferentCommit(t *testing.T) {
	repo := gitRepo(t)
	c := newCoordinator(t, repo)
	if _, err := c.Rebuild(t.Context()); err != nil {
		t.Fatal(err)
	}

	write(t, repo, "second.go", "package main\n")
	for _, args := range [][]string{{"add", "-A"}, {"commit", "--quiet", "-m", "second"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid",
			"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}

	st, err := c.Validate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Fresh {
		t.Fatal("the index validated against a commit it was not built from")
	}
	if !strings.Contains(strings.Join(st.Reasons, " "), "different commit") {
		t.Fatalf("the reason does not name the commit: %+v", st.Reasons)
	}
	if _, err := c.Rebuild(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st, err := c.Validate(t.Context()); err != nil || !st.Fresh {
		t.Fatalf("a rebuild at the new commit did not validate: %v %+v", err, st.Reasons)
	}
}

// TestTheGitDirectoryIsNeverIndexed: .git holds packed objects, config with
// remote URLs, and a credential helper's cache. None of it is source.
func TestTheGitDirectoryIsNeverIndexed(t *testing.T) {
	repo := gitRepo(t)
	c := newCoordinator(t, repo)
	m, err := c.Rebuild(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for p := range paths(t, c, m) {
		if strings.HasPrefix(p, ".git/") || p == ".git" {
			t.Fatalf("%s was indexed", p)
		}
	}
}
