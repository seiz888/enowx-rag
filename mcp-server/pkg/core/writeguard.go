package core

import (
	"fmt"
	"os"
	"regexp"
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
	projects  map[string]bool
	maxChars  int
	maxDelete int
	required  []string
	enums     map[string][]string
}

// WriteGuardFromEnv builds a guard from the environment, or returns nil when
// RAG_GUARD_PROJECTS is unset or empty.
//
//	RAG_GUARD_PROJECTS      memory              projects the guard applies to
//	RAG_GUARD_MAX_CHARS     3000                reject a document longer than this (0 = no limit)
//	RAG_GUARD_REQUIRE_META  chunk,bucket,kind   meta keys that must be present and non-empty
//	RAG_GUARD_ENUM_KIND     log,ref             allowed values for meta.kind
//	RAG_GUARD_ENUM_SOURCE   session,multibrain  allowed values for meta.source
//	RAG_GUARD_MAX_DELETE    25                  reject a delete of more points than this
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
	// Note the asymmetry with maxChars: an unset value here means the DEFAULT
	// cap, not "no cap". A guard that silently permits unlimited deletion when
	// an operator forgets one variable is the failure this exists to prevent.
	// 0 is accepted and means "no deletes at all".
	g.maxDelete = DefaultMaxDelete
	if v := os.Getenv("RAG_GUARD_MAX_DELETE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			g.maxDelete = n
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

// DefaultMaxDelete caps how many points one call may remove from a guarded
// project. Chosen to fit repair work and nothing else: fixing a bad write means
// deleting the handful of chunks it produced, while any legitimate reshaping of
// the corpus is a re-chunk that upserts over the same doc_ids rather than
// deleting them.
const DefaultMaxDelete = 25

// CheckDeleteProject refuses to drop a guarded project's entire collection.
//
// This closes an asymmetry, not a hypothetical: since 2026-09-09 a malformed
// document is rejected on write, yet DeleteProject and DeletePoints called
// straight through to the provider with no check at all. So the corpus could not
// be written badly but could be removed completely -- by one tool call, from any
// agent holding the bearer. The snapshot chain is proven to restore, but "we can
// get yesterday back" is a recovery story, not a control.
//
// There is deliberately no override flag. Deleting this project is a thing a
// person should do on the host, with the systemd unit in view, not something an
// agent can talk its way into mid-session.
func (g *WriteGuard) CheckDeleteProject(projectID string) error {
	if g == nil || !g.projects[projectID] {
		return nil
	}
	return fmt.Errorf("write guard: project %q dilindungi -- hapus koleksi "+
		"ditolak. Kalau memang mau menghapusnya, lakukan di host (lepas "+
		"%s dari RAG_GUARD_PROJECTS lalu restart), bukan lewat tool call",
		projectID, projectID)
}

// CheckDeletePoints caps a bulk point delete on a guarded project.
//
// A cap rather than a ban: deleting a few points is how a bad write gets
// repaired, and forbidding it would push that work onto a project-wide delete or
// onto nothing at all. What the cap stops is the shape that cannot be repair --
// one call carrying hundreds of ids, which is either a mistake or a wipe.
func (g *WriteGuard) CheckDeletePoints(projectID string, n int) error {
	if g == nil || !g.projects[projectID] {
		return nil
	}
	if n <= g.maxDelete {
		return nil
	}
	return fmt.Errorf("write guard: %d titik dalam satu panggilan hapus di "+
		"project %q, batas %d. Hapus per bagian kalau ini perbaikan; kalau ini "+
		"perapian korpus, tulis ulang lewat rag_index dengan doc_id yang sama "+
		"(upsert) daripada menghapus", n, projectID, g.maxDelete)
}

// Describe reports the active contract, for the startup log.
func (g *WriteGuard) Describe() string {
	if g == nil {
		return "off"
	}
	names := make([]string, 0, len(g.projects))
	for p := range g.projects {
		names = append(names, p)
	}
	sort.Strings(names)
	return fmt.Sprintf("projects=%s max_chars=%d max_delete=%d require=%s",
		strings.Join(names, "+"), g.maxChars, g.maxDelete,
		strings.Join(g.required, ","))
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
		if containsCredential(d.Content) || containsCredential(d.ID) {
			return fmt.Errorf("write guard: credential-like content rejected before embedding; store sensitive material in the private vault")
		}
		for key, value := range d.Meta {
			if containsCredential(value) || containsCredential(key+"="+value) {
				return fmt.Errorf("write guard: credential-like metadata rejected before embedding; store sensitive material in the private vault")
			}
		}
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

// Fixed credential shapes and explicit assignments avoid treating topology,
// checksums, and Git revisions as secrets. Errors never echo matched content.
var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-(?:proj-|ant-)?[A-Za-z0-9_-]{20,})`),
	regexp.MustCompile(`\b(?:cfk_[A-Za-z0-9]{30,}|v1\.0-[A-Za-z0-9_-]{30,}|xox[baprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16})\b`),
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9_./+~-]{16,}`),
	regexp.MustCompile(`(?i)\b(?:[A-Za-z0-9]+_)*(?:password|passwd|api[_-]?key|access[_-]?token|refresh[_-]?token|client[_-]?secret|admin[_-]?token|secret)\b["'\s]*[:=][\s"']*([A-Za-z0-9_./+!@#%^&*~-]{8,})`),
}

// ContainsCredential reports whether text carries something shaped like a
// credential. It is exported so the memory gateway's ledger ingress scans with
// the same patterns this guard uses; a second copy of the pattern list would
// drift, and the copy that drifted would be the one that let something through.
func ContainsCredential(text string) bool { return containsCredential(text) }

func containsCredential(text string) bool {
	for _, pattern := range credentialPatterns {
		if pattern.MatchString(text) {
			return true
		}
	}
	return false
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
