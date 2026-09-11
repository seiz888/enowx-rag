package adapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// Tests for cwd -> project/workspace/work resolution.
//
// Nothing here touches a network: the git-remote fallback is deliberately
// unwired in this build (it refuses), so the only exercised path is the work
// map, which is exactly the deterministic, testable part.

func writeMap(t *testing.T, entries []WorkMapEntry) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "work-map.json")
	data := mustJSON(t, WorkMap{Entries: entries})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write map: %v", err)
	}
	return path
}

func TestResolveMatchesLongestPrefix(t *testing.T) {
	proj1 := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	proj2 := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	ws1 := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	ws2 := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	work1 := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")

	root := filepath.Clean(t.TempDir())
	sub := filepath.Join(root, "services", "api")
	mapPath := writeMap(t, []WorkMapEntry{
		{Prefix: root, ProjectID: proj1, WorkspaceID: ws1},
		{Prefix: sub, ProjectID: proj2, WorkspaceID: ws2, WorkID: work1},
	})

	r, err := ResolveTarget(filepath.Join(sub, "src"), Resolution{MapPath: mapPath})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.ProjectID != proj2 || r.WorkspaceID != ws2 || r.WorkID != work1 {
		t.Fatalf("longest prefix did not win: %+v", r)
	}
}

func TestResolveFallsBackToBroaderPrefix(t *testing.T) {
	proj := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	ws := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	root := filepath.Clean(t.TempDir())
	mapPath := writeMap(t, []WorkMapEntry{{Prefix: root, ProjectID: proj, WorkspaceID: ws}})

	r, err := ResolveTarget(filepath.Join(root, "elsewhere"), Resolution{MapPath: mapPath})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.ProjectID != proj || r.WorkspaceID != ws || r.WorkID != uuid.Nil {
		t.Fatalf("unexpected resolution: %+v", r)
	}
}

func TestResolveNoMatchIsAnErrorNotAGuess(t *testing.T) {
	proj := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	ws := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	root := filepath.Clean(t.TempDir())
	mapPath := writeMap(t, []WorkMapEntry{{Prefix: root, ProjectID: proj, WorkspaceID: ws}})

	// A cwd outside the mapped tree, with no git-remote fallback, must refuse
	// rather than silently file under the one entry that exists.
	_, err := ResolveTarget(filepath.Clean(t.TempDir()), Resolution{MapPath: mapPath})
	if err == nil {
		t.Fatal("expected an error for an unmatched cwd, got success")
	}
}

func TestResolveSiblingPrefixDoesNotMatch(t *testing.T) {
	proj := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	ws := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	base := filepath.Clean(t.TempDir())
	mapPath := writeMap(t, []WorkMapEntry{{Prefix: filepath.Join(base, "repo-a"), ProjectID: proj, WorkspaceID: ws}})

	// repo-b is a sibling of repo-a: a naive strings.HasPrefix would match
	// "repo-a" as a prefix of "repo-ab", but "repo-b" must not match.
	_, err := ResolveTarget(filepath.Join(base, "repo-b"), Resolution{MapPath: mapPath})
	if err == nil {
		t.Fatal("sibling prefix matched; the prefix must be a path segment boundary")
	}
}

func TestConfigCheckRejectsBothFixedAndResolve(t *testing.T) {
	cfg := testConfig()
	cfg.Resolve = Resolution{MapPath: "x.json"}
	if err := cfg.check(); err == nil {
		t.Fatal("a config with both fixed ids and a resolve block must be refused")
	}
}

func TestConfigCheckRequiresOneOfFixedOrResolve(t *testing.T) {
	cfg := testConfig()
	cfg.ProjectID = uuid.Nil
	cfg.WorkspaceID = uuid.Nil
	if err := cfg.check(); err == nil {
		t.Fatal("a config with neither fixed ids nor a resolve block must be refused")
	}
}

func TestResolveForCwdOverridesFixedIds(t *testing.T) {
	proj := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	ws := uuid.MustParse("dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	work := uuid.MustParse("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee")
	root := filepath.Clean(t.TempDir())
	mapPath := writeMap(t, []WorkMapEntry{{Prefix: root, ProjectID: proj, WorkspaceID: ws, WorkID: work}})

	cfg := Config{Resolve: Resolution{MapPath: mapPath}, WriterEpoch: 1}
	out, err := cfg.ResolveForCwd(filepath.Join(root, "x"))
	if err != nil {
		t.Fatalf("ResolveForCwd: %v", err)
	}
	if out.ProjectID != proj || out.WorkspaceID != ws || out.Work.WorkID != work {
		t.Fatalf("resolved ids not applied: %+v", out)
	}
}
