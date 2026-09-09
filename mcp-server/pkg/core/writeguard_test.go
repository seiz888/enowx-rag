package core

import (
	"strings"
	"testing"

	"github.com/enowdev/enowx-rag/pkg/rag"
)

func guard(t *testing.T, env map[string]string) *WriteGuard {
	t.Helper()
	for _, k := range []string{"RAG_GUARD_PROJECTS", "RAG_GUARD_MAX_CHARS",
		"RAG_GUARD_REQUIRE_META", "RAG_GUARD_ENUM_KIND", "RAG_GUARD_ENUM_SOURCE"} {
		t.Setenv(k, env[k])
	}
	return WriteGuardFromEnv()
}

func okDoc(id string) rag.Document {
	return rag.Document{ID: id, Content: "isi pendek", Meta: map[string]string{
		"chunk": "1/1", "bucket": "rag-memory", "kind": "log",
		"title": id, "ts": "2026-09-09T11:00:00+07:00",
		"agent": "claude-code", "source": "session",
	}}
}

func full() map[string]string {
	return map[string]string{
		"RAG_GUARD_PROJECTS":     "memory",
		"RAG_GUARD_MAX_CHARS":    "3000",
		"RAG_GUARD_REQUIRE_META": "chunk,bucket,kind,title,ts,agent,source",
		"RAG_GUARD_ENUM_KIND":    "log,ref",
		"RAG_GUARD_ENUM_SOURCE":  "session,multibrain",
	}
}

// A guard nobody configured must not exist: this ships to deployments that never
// asked for a write contract, and an accidentally-on guard rejects their writes.
func TestDisabledUnlessConfigured(t *testing.T) {
	if g := guard(t, map[string]string{}); g != nil {
		t.Fatalf("guard aktif tanpa RAG_GUARD_PROJECTS: %s", g.Describe())
	}
	if err := (*WriteGuard)(nil).Check("memory", []rag.Document{{ID: "x"}}); err != nil {
		t.Fatalf("guard nil menolak: %v", err)
	}
}

// The guard is per-project on purpose; `memory` has a schema, other projects
// (code indexes, TravelYA) do not and must keep writing freely.
func TestOnlyNamedProjects(t *testing.T) {
	g := guard(t, full())
	junk := []rag.Document{{ID: "bebas", Content: strings.Repeat("x", 9000)}}
	if err := g.Check("travelya", junk); err != nil {
		t.Fatalf("proyek di luar daftar ditolak: %v", err)
	}
	if err := g.Check("memory", junk); err == nil {
		t.Fatal("proyek dalam daftar tidak diperiksa")
	}
}

func TestAcceptsConformingDoc(t *testing.T) {
	g := guard(t, full())
	if err := g.Check("memory", []rag.Document{okDoc("a"), okDoc("b")}); err != nil {
		t.Fatalf("dokumen sah ditolak: %v", err)
	}
}

func TestRejectsOversized(t *testing.T) {
	g := guard(t, full())
	d := okDoc("gemuk")
	d.Content = strings.Repeat("a", 3001)
	err := g.Check("memory", []rag.Document{d})
	if err == nil {
		t.Fatal("dokumen 3001 karakter lolos")
	}
	// The message has to say what to do next; "invalid document" would send the
	// caller back to read the source.
	for _, want := range []string{"gemuk", "3001", "3000", "rag_chunk.py"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("pesan tidak memuat %q: %v", want, err)
		}
	}
}

// Counted in runes, not bytes: a chunk of Indonesian prose with em-dashes is not
// oversized just because it is not ASCII.
func TestLengthCountedInRunes(t *testing.T) {
	g := guard(t, map[string]string{"RAG_GUARD_PROJECTS": "memory", "RAG_GUARD_MAX_CHARS": "100"})
	d := okDoc("unicode")
	d.Content = strings.Repeat("—", 100) // 100 runes, 300 bytes
	if err := g.Check("memory", []rag.Document{d}); err != nil {
		t.Fatalf("100 rune ditolak karena dihitung sebagai byte: %v", err)
	}
	d.Content = strings.Repeat("—", 101)
	if err := g.Check("memory", []rag.Document{d}); err == nil {
		t.Fatal("101 rune lolos batas 100")
	}
}

func TestRejectsMissingAndBlankMeta(t *testing.T) {
	g := guard(t, full())
	d := okDoc("kurang")
	delete(d.Meta, "agent")
	d.Meta["bucket"] = "   " // whitespace is absence, not a value
	err := g.Check("memory", []rag.Document{d})
	if err == nil {
		t.Fatal("meta kurang lolos")
	}
	for _, want := range []string{"meta.agent", "meta.bucket"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("pesan tidak memuat %q: %v", want, err)
		}
	}
}

func TestRejectsInventedEnumValues(t *testing.T) {
	g := guard(t, full())
	d := okDoc("enum")
	d.Meta["kind"] = "session-log" // one of the 16 real cases found in the audit
	d.Meta["source"] = "vault-migrate"
	err := g.Check("memory", []rag.Document{d})
	if err == nil {
		t.Fatal("kind/source palsu lolos")
	}
	for _, want := range []string{"session-log", "log/ref", "vault-migrate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("pesan tidak memuat %q: %v", want, err)
		}
	}
}

// An enum field left empty must be reported once, by the required-field check,
// not twice with a confusing "hanya boleh log/ref" for a value nobody wrote.
func TestEmptyEnumReportedOnceAsMissing(t *testing.T) {
	g := guard(t, full())
	d := okDoc("kosong")
	d.Meta["kind"] = ""
	err := g.Check("memory", []rag.Document{d})
	if err == nil {
		t.Fatal("kind kosong lolos")
	}
	if n := strings.Count(err.Error(), "kind"); n != 1 {
		t.Errorf("kind disebut %d kali, mau 1: %v", n, err)
	}
}

// One call must surface every problem in the batch; fixing a 16-document write
// one rejection at a time is 16 round-trips and 16 chances to give up.
func TestReportsWholeBatch(t *testing.T) {
	g := guard(t, full())
	docs := []rag.Document{okDoc("baik")}
	for _, id := range []string{"buruk1", "buruk2", "buruk3"} {
		d := okDoc(id)
		delete(d.Meta, "ts")
		docs = append(docs, d)
	}
	err := g.Check("memory", docs)
	if err == nil {
		t.Fatal("batch bermasalah lolos")
	}
	if !strings.Contains(err.Error(), "3 dari 4 dokumen") {
		t.Errorf("hitungan dokumen salah: %v", err)
	}
	for _, id := range []string{"buruk1", "buruk2", "buruk3"} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("%s tidak dilaporkan: %v", id, err)
		}
	}
	if strings.Contains(err.Error(), "baik:") {
		t.Errorf("dokumen sah dilaporkan sebagai pelanggaran: %v", err)
	}
}

// Each knob is independent: a deployment may want the size limit without the
// schema, or the reverse.
func TestKnobsAreIndependent(t *testing.T) {
	sizeOnly := guard(t, map[string]string{"RAG_GUARD_PROJECTS": "memory", "RAG_GUARD_MAX_CHARS": "50"})
	if err := sizeOnly.Check("memory", []rag.Document{{ID: "polos", Content: "pendek"}}); err != nil {
		t.Fatalf("tanpa REQUIRE_META tetap menuntut meta: %v", err)
	}
	metaOnly := guard(t, map[string]string{"RAG_GUARD_PROJECTS": "memory", "RAG_GUARD_REQUIRE_META": "kind"})
	big := rag.Document{ID: "besar", Content: strings.Repeat("x", 50000),
		Meta: map[string]string{"kind": "log"}}
	if err := metaOnly.Check("memory", []rag.Document{big}); err != nil {
		t.Fatalf("tanpa MAX_CHARS tetap membatasi ukuran: %v", err)
	}
	if guard(t, map[string]string{"RAG_GUARD_PROJECTS": "memory", "RAG_GUARD_MAX_CHARS": "nol"}).maxChars != 0 {
		t.Error("MAX_CHARS tidak valid tidak diabaikan")
	}
}

// The real corpus contains chunks up to 2558 characters -- a single unsplittable
// prose line -- so the deployed limit must not reject its own corpus.
func TestDeployedLimitAcceptsRealCorpusMax(t *testing.T) {
	g := guard(t, full())
	d := okDoc("baris-panjang")
	d.Content = strings.Repeat("k", 2558)
	if err := g.Check("memory", []rag.Document{d}); err != nil {
		t.Fatalf("chunk 2558 karakter yang sah ditolak: %v", err)
	}
}
