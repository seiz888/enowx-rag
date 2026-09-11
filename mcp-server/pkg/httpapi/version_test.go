package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestVersionEndpointReportsBuildIdentity guards the property Phase 0.5 exists
// for: the server must be able to say which build it is. A test binary is built
// from the same tree as the server, so the fields it reports here are the same
// ones production reports -- what matters is that none of them is the old
// placeholder and that a version is always present.
func TestVersionEndpointReportsBuildIdentity(t *testing.T) {
	h := &Handlers{}
	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	rec := httptest.NewRecorder()

	h.Version(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got struct {
		Version   string `json:"version"`
		CommitSHA string `json:"commit_sha"`
		BuildTime string `json:"build_time"`
		DirtyTree bool   `json:"dirty_tree"`
		GoVersion string `json:"go_version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.Version == "" {
		t.Error("version is empty; the endpoint must always name a build")
	}
	if got.GoVersion == "" {
		t.Error("go_version is empty; build info was not readable")
	}
	// "dev" was the old placeholder for every build regardless of source. It is
	// still the honest answer when there is no VCS stamp at all (a tarball
	// build), so this only fails when a commit IS known and the version still
	// says "dev" -- that would mean the stamp was read and then thrown away.
	if got.Version == "dev" && got.CommitSHA != "" {
		t.Errorf("version = %q while commit_sha = %q; the commit must be surfaced", got.Version, got.CommitSHA)
	}
}
