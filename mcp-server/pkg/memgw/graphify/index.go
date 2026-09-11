package graphify

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Entry is one file in the index.
//
// Path, size and a content digest, plus the language guessed from the
// extension. Deliberately no symbols and no call graph: extracting those
// correctly needs a parser per language, and a wrong one produces an index that
// is confidently incorrect about where a function is defined. Topology is the
// part that is cheap to get right, and it is the part an agent actually asks
// for -- which files exist, which changed, where a name appears.
type Entry struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Digest   string `json:"digest"`
	Language string `json:"language,omitempty"`
	Lines    int64  `json:"lines,omitempty"`
}

// maxIndexedBytes bounds what is hashed and counted per file. A generated
// 200 MB fixture contributes its size and its first chunk; reading all of it
// would make a rebuild's cost depend on the largest file in the tree.
const maxIndexedBytes = 4 << 20

// buildIndex walks the tree and writes the entries as newline-delimited JSON.
//
// NDJSON rather than one array: a generation is written once and read by
// something that wants to stream it, and a reader that has to hold the whole
// index in memory to look at one entry is a reader that will not be used on a
// large repository.
func buildIndex(ctx context.Context, root, dest string) (entries int64, bytesSeen int64, excluded int64, digest string, err error) {
	f, err := os.Create(dest)
	if err != nil {
		return 0, 0, 0, "", err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<16)

	// The index digest is computed over the entries in path order, so two
	// builds of the same tree produce the same digest regardless of the order
	// the filesystem handed them over. That is what makes "did anything
	// change" answerable without a diff.
	var paths []string
	byPath := map[string]Entry{}

	walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			// An unreadable directory is not a reason to abandon the rebuild.
			// It is a reason for the index to be missing part of the tree, and
			// the file count in the manifest is what makes that visible.
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if ExcludedDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			// Symlinks are not followed. A link out of the repository would let
			// a walk that is supposed to be bounded by one directory read
			// anything the user can read.
			return nil
		}
		if ExcludedFile(rel) {
			excluded++
			return nil
		}
		e, eerr := indexFile(p, rel)
		if eerr != nil {
			return nil
		}
		paths = append(paths, rel)
		byPath[rel] = e
		bytesSeen += e.Size
		return nil
	})
	if walkErr != nil {
		return 0, 0, 0, "", walkErr
	}

	sort.Strings(paths)
	h := sha256.New()
	enc := json.NewEncoder(w)
	for _, rel := range paths {
		e := byPath[rel]
		if err := enc.Encode(e); err != nil {
			return 0, 0, 0, "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00%s\x00", e.Path, e.Size, e.Digest)
		entries++
	}
	if err := w.Flush(); err != nil {
		return 0, 0, 0, "", err
	}
	if err := f.Sync(); err != nil {
		return 0, 0, 0, "", err
	}
	return entries, bytesSeen, excluded, hex.EncodeToString(h.Sum(nil)), nil
}

func indexFile(full, rel string) (Entry, error) {
	fi, err := os.Stat(full)
	if err != nil {
		return Entry{}, err
	}
	f, err := os.Open(full)
	if err != nil {
		return Entry{}, err
	}
	defer f.Close()

	h := sha256.New()
	var lines int64
	buf := make([]byte, 32<<10)
	var read int64
	for read < maxIndexedBytes {
		n, rerr := f.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			h.Write(chunk)
			for _, b := range chunk {
				if b == '\n' {
					lines++
				}
			}
			read += int64(n)
		}
		if rerr != nil {
			break
		}
	}
	return Entry{
		Path:     rel,
		Size:     fi.Size(),
		Digest:   hex.EncodeToString(h.Sum(nil)),
		Language: languageOf(rel),
		Lines:    lines,
	}, nil
}

// ReadIndex streams a generation's entries. It exists so a caller does not have
// to know the file is NDJSON.
func ReadIndex(r io.Reader, fn func(Entry) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return err
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return sc.Err()
}

var languages = map[string]string{
	".go": "go", ".ts": "typescript", ".tsx": "typescript", ".js": "javascript",
	".jsx": "javascript", ".py": "python", ".rs": "rust", ".java": "java",
	".rb": "ruby", ".php": "php", ".cs": "csharp", ".c": "c", ".h": "c",
	".cc": "cpp", ".cpp": "cpp", ".hpp": "cpp", ".sh": "shell", ".ps1": "powershell",
	".sql": "sql", ".md": "markdown", ".json": "json", ".yaml": "yaml", ".yml": "yaml",
	".toml": "toml", ".html": "html", ".css": "css", ".scss": "css",
}

func languageOf(rel string) string {
	return languages[strings.ToLower(filepath.Ext(rel))]
}
