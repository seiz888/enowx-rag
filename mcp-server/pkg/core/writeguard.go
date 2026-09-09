package core

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/enowdev/enowx-rag/pkg/rag"
)

// WriteGuard rejects documents that violate a project's write contract.
//
// Why reject rather than repair: this server is the only chokepoint every writer
// shares. Client-side conventions do not bind agents that never load the client,
// and a convention nothing enforces decays silently -- on the `memory` project it
// did, twice over. An audit on 2026-09-08 found 294 of 1667 chunks carrying meta
// no reader filters on (invented `kind` values, absent `agent`, alias fields), and
// 48 of 67 single-vector documents over 1500 characters, the largest 13707. Both
// were invisible until someone went looking, and repairing them meant recovering
// full chunk text one search at a time.
//
// Why it does not chunk oversized documents itself: it cannot do it well. The
// indexer's chunkText splits on line boundaries and hard-splits mid-line past the
// limit; the client-side chunker splits on heading boundaries, packs adjacent
// sections, and prepends a `title > heading path` context line to every chunk.
// Silently substituting the worse split -- and inventing `#n` ids the caller does
// not know about -- trades one invisible problem for another. Refusing with the
// offending id and reason costs the caller one retry through a chunker that
// already exists, and keeps id ownership where it belongs.
//
// Disabled unless RAG_GUARD_PROJECTS names at least one project, so every other
// deployment and every other project on this one is unaffected.
type WriteGuard struct {
	projects map[string]bool
	maxChars int
	required []string
	enums    map[string][]string
}

// WriteGuardFromEnv builds a guard from the environment, or returns nil when
// RAG_GUARD_PROJECTS is unset or empty.
//
//	RAG_GUARD_PROJECTS      memory              projects the guard applies to
//	RAG_GUARD_MAX_CHARS     3000                reject a document longer than this (0 = no limit)
//	RAG_GUARD_REQUIRE_META  chunk,bucket,kind   meta keys that must be present and non-empty
//	RAG_GUARD_ENUM_KIND     log,ref             allowed values for meta.kind
//	RAG_GUARD_ENUM_SOURCE   session,multibrain  allowed values for meta.source
func WriteGuardFromEnv() *WriteGuard {
	projects := splitList(os.Getenv("RAG_GUARD_PROJECTS"))
	if len(projects) == 0 {
		return nil
	}
	g := &WriteGuard{
		projects: make(map[string]bool, len(projects)),
		required: splitList(os.Getenv("RAG_GUARD_REQUIRE_META")),
		enums:    map[string][]string{},
	}
	for _, p := range projects {
		g.projects[p] = true
	}
	if v := os.Getenv("RAG_GUARD_MAX_CHARS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			g.maxChars = n
		}
	}
	for _, f := range []string{"kind", "source"} {
		if vs := splitList(os.Getenv("RAG_GUARD_ENUM_" + strings.ToUpper(f))); len(vs) > 0 {
			g.enums[f] = vs
		}
	}
	return g
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Describe reports the active contract, for the startup log.
// CheckProjectScan refuses a directory-scan index into a guarded project.
//
// Why refuse rather than validate per document: a scan cannot satisfy the
// contract by construction. It produces chunks whose meta describes a file on
// disk (`source_file`, offsets) and it has nowhere to get `bucket`, `kind`,
// `ts`, `agent` or `source` from -- those describe an authored record, not a
// file. So every document would fail the per-document check anyway; failing
// once, up front, says why instead of emitting a wall of violations.
//
// This closes a real hole rather than a theoretical one: IndexProject writes
// through indexer.IndexProject, which calls provider.Index directly and never
// passes through IndexDocuments, so until now the guard on `memory` did not
// see this path at all.
func (g *WriteGuard) CheckProjectScan(projectID string) error {
	if g == nil || !g.projects[projectID] {
		return nil
	}
	return fmt.Errorf("write guard: project %q hanya menerima dokumen ber-meta "+
		"lengkap (%s); scan direktori tidak bisa menyediakannya -- tulis lewat "+
		"rag_index setelah dipotong dengan rag_chunk.py",
		projectID, strings.Join(g.required, ","))
}

func (g *WriteGuard) Describe() string {
	if g == nil {
		return "off"
	}
	names := make([]string, 0, len(g.projects))
	for p := range g.projects {
		names = append(names, p)
	}
	sort.Strings(names)
	return fmt.Sprintf("projects=%s max_chars=%d require=%s",
		strings.Join(names, "+"), g.maxChars, strings.Join(g.required, ","))
}

// Check returns an error naming every violation, or nil. A nil guard, or a
// project the guard does not cover, always passes.
//
// All documents are inspected before returning, so one call surfaces every
// problem in a batch instead of making the caller fix them one round-trip at a
// time. Length is counted in runes: the corpus is prose with em-dashes and
// Indonesian text, and counting bytes would reject a legitimate chunk for being
// non-ASCII.
func (g *WriteGuard) Check(projectID string, docs []rag.Document) error {
	if g == nil || !g.projects[projectID] {
		return nil
	}
	var bad []string
	for _, d := range docs {
		id := d.ID
		if id == "" {
			id = "(tanpa id)"
		}
		if g.maxChars > 0 {
			if n := utf8.RuneCountInString(d.Content); n > g.maxChars {
				bad = append(bad, fmt.Sprintf(
					"%s: %d karakter, batas %d -- potong dulu (rag_chunk.py) lalu tulis sebagai <id>#1..#n",
					id, n, g.maxChars))
			}
		}
		for _, k := range g.required {
			if strings.TrimSpace(d.Meta[k]) == "" {
				bad = append(bad, fmt.Sprintf("%s: meta.%s kosong atau tidak ada", id, k))
			}
		}
		for f, allowed := range g.enums {
			v := strings.TrimSpace(d.Meta[f])
			if v == "" {
				continue // absence is the required-field check's business, not this one
			}
			if !contains(allowed, v) {
				bad = append(bad, fmt.Sprintf("%s: meta.%s=%q, hanya boleh %s",
					id, f, v, strings.Join(allowed, "/")))
			}
		}
	}
	if len(bad) == 0 {
		return nil
	}
	const show = 10
	msg := bad
	if len(bad) > show {
		msg = append(append([]string{}, bad[:show]...),
			fmt.Sprintf("... dan %d pelanggaran lain", len(bad)-show))
	}
	return fmt.Errorf("write guard menolak %d dari %d dokumen:\n  %s",
		countBadDocs(bad), len(docs), strings.Join(msg, "\n  "))
}

func countBadDocs(violations []string) int {
	seen := map[string]bool{}
	for _, v := range violations {
		seen[strings.SplitN(v, ":", 2)[0]] = true
	}
	return len(seen)
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
