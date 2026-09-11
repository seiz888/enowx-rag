package graphify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RepoState is what the index was built against.
//
// Commit alone is not enough. Most of the time an agent is reading a working
// tree that differs from HEAD -- that is what working on something looks like
// -- so an index that only recorded the commit would claim to be fresh for a
// tree it has never seen. DirtyDigest closes that: it changes whenever the
// uncommitted part of the tree changes.
type RepoState struct {
	Commit      string
	DirtyDigest string
}

// Clean reports whether the working tree matched HEAD.
func (r RepoState) Clean() bool { return r.DirtyDigest == cleanDigest }

const cleanDigest = "clean"

// notGit is the state of a directory that is not a checkout. It is a valid
// thing to index -- an unpacked source tree still has a topology -- but nothing
// can be said about its freshness beyond the walk itself, so the digest is
// derived from the tree rather than from git.
const notGitCommit = ""

// InspectRepo reads the commit and computes the dirty-tree digest.
//
// It shells out to git rather than parsing .git itself. Reading a packed-refs
// file or a worktree's gitdir pointer correctly is more code than this whole
// package, and a wrong answer here is an index that silently claims to be
// current.
func InspectRepo(ctx context.Context, dir string) (RepoState, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return treeState(dir)
	}
	commit, err := git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		// Not a checkout, or a checkout with no commits. Either way there is no
		// commit to name, and the tree is all there is.
		return treeState(dir)
	}

	// --porcelain is stable across git versions by contract; -z avoids quoting
	// rules that differ between them. Untracked files are included because an
	// untracked file is source the agent can read.
	out, err := gitRaw(ctx, dir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return RepoState{}, err
	}
	entries := splitZ(out)
	if len(entries) == 0 {
		return RepoState{Commit: commit, DirtyDigest: cleanDigest}, nil
	}

	// The digest covers the status line and, for anything still on disk, its
	// size and modification time. Hashing the contents would be more precise
	// and would also mean reading every dirty file twice per rebuild; size and
	// mtime move whenever an editor writes, which is the case this is for.
	sort.Strings(entries)
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s\x00", e)
		if len(e) > 3 {
			rel := strings.TrimSpace(e[3:])
			if fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err == nil {
				fmt.Fprintf(h, "%d\x00%d\x00", fi.Size(), fi.ModTime().UTC().UnixNano())
			}
		}
	}
	return RepoState{Commit: commit, DirtyDigest: hex.EncodeToString(h.Sum(nil))}, nil
}

// treeState describes a directory that git cannot speak for.
func treeState(dir string) (RepoState, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if ExcludedDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if ExcludedFile(rel) {
			return nil
		}
		fi, ferr := d.Info()
		if ferr != nil {
			return nil
		}
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00", rel, fi.Size(), fi.ModTime().UTC().UnixNano())
		return nil
	})
	if err != nil {
		return RepoState{}, err
	}
	return RepoState{Commit: notGitCommit, DirtyDigest: hex.EncodeToString(h.Sum(nil))}, nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitRaw(ctx, dir, args...)
	return strings.TrimSpace(string(out)), err
}

func gitRaw(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// A rebuild must not be able to prompt for a credential and hang: a hook or
	// a service has no terminal to answer on.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("graphify: git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func splitZ(b []byte) []string {
	parts := bytes.Split(b, []byte{0})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) > 0 {
			out = append(out, string(p))
		}
	}
	return out
}

func dirOf(p string) string { return filepath.Dir(p) }
