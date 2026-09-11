package httpapi

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// These tests pin the fail-closed contract of EffectiveAdminToken():
//
//   - a MISSING config file means "intentionally no auth" (local-first default)
//     and requests pass through — this is a choice, not a failure;
//   - a MALFORMED (unparseable) or unreadable config file is an error, and both
//     middlewares must fail closed with 503 instead of silently treating it as
//     "no auth" — a broken config must not open the door it was supposed to guard.
//
// RAG_ADMIN_TOKEN set at all bypasses the file entirely (env precedence).

// writeBadConfig writes an unparseable YAML config at ~/.enowx-rag/config.yaml
// under the isolated home, returning nothing (t.Fatal on failure).
func writeBadConfig(t *testing.T, home, content string) string {
	t.Helper()
	dir := filepath.Join(home, ".enowx-rag")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	return path
}

// TestAdminToken_MalformedConfig_FailsClosed503 pins the main fail-closed
// behavior: unparseable YAML under a SET RAG_ADMIN_TOKEN-less home denies
// every /api request with 503 rather than passing through.
func TestAdminToken_MalformedConfig_FailsClosed503(t *testing.T) {
	home := isolateHome(t, "")
	t.Setenv("RAG_ADMIN_TOKEN", "")
	writeBadConfig(t, home, "embedder: [unclosed\n  bad: : : yaml\n")

	called := false
	h := AdminTokenMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if called {
		t.Fatal("handler must not be called when auth config is malformed")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 for malformed auth config", w.Code)
	}
}

// TestAdminToken_MalformedConfig_EnvStillAuthenticates pins that RAG_ADMIN_TOKEN
// takes precedence over a broken file: when the env var is set, auth works
// normally even though the config file is garbage.
func TestAdminToken_MalformedConfig_EnvStillAuthenticates(t *testing.T) {
	home := isolateHome(t, "")
	t.Setenv("RAG_ADMIN_TOKEN", "envtoken")
	writeBadConfig(t, home, "embedder: [unclosed\n")

	h := AdminTokenMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Correct env token passes.
	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.Header.Set("Authorization", "Bearer envtoken")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("with env token set: status = %d, want 200 (env overrides broken file)", w.Code)
	}

	// Wrong token still 401, not 503: the env var supplies a valid config.
	req = httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong env token: status = %d, want 401", w.Code)
	}
}

// TestAdminToken_MissingConfig_NoAuthIsIntentional pins that an absent config
// file (fresh install, local-first default) means "no auth" and requests pass
// through — the documented intentional behavior.
func TestAdminToken_MissingConfig_NoAuthIsIntentional(t *testing.T) {
	isolateHome(t, "") // empty temp home: config file does not exist
	t.Setenv("RAG_ADMIN_TOKEN", "")

	called := false
	h := AdminTokenMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !called {
		t.Error("handler should be called: missing config file means intentional no-auth")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for missing config (no-auth local default)", w.Code)
	}
}

// TestLocalOrAdmin_MalformedConfig_RemoteFailsClosed503 pins the same
// fail-closed contract on the setup-wizard middleware for REMOTE callers:
// loopback is allowed (setup must remain usable), but anything else gets 503
// when the auth config is broken.
func TestLocalOrAdmin_MalformedConfig_RemoteFailsClosed503(t *testing.T) {
	home := isolateHome(t, "")
	t.Setenv("RAG_ADMIN_TOKEN", "")
	writeBadConfig(t, home, "admin_token: \"unbalanced\n  - : :\n")

	h := LocalOrAdminMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Remote caller (non-loopback RemoteAddr) must be denied with 503.
	req := httptest.NewRequest(http.MethodGet, "/api/setup/config", nil)
	req.RemoteAddr = net.JoinHostPort("203.0.113.7", "40000")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("remote: status = %d, want 503 for malformed auth config", w.Code)
	}

	// Loopback still gets through: local setup wizard stays usable even when
	// the config file is corrupt (it is the tool that fixes the file).
	req = httptest.NewRequest(http.MethodGet, "/api/setup/config", nil)
	req.RemoteAddr = net.JoinHostPort("127.0.0.1", "40000")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("loopback: status = %d, want 200 (local-first, setup must stay usable)", w.Code)
	}
}

// TestAdminToken_UnreadableConfig_FailsClosed503 pins fail-closed for a config
// that exists but cannot be READ. A directory at the config path makes
// os.ReadFile fail deterministically on every platform (chmod 000 does not:
// Windows maps it to a read-only bit that still allows reads, and POSIX roots
// bypass it entirely) — which is exactly why the read path, not the permission
// mechanism, is what gets pinned here.
func TestAdminToken_UnreadableConfig_FailsClosed503(t *testing.T) {
	home := isolateHome(t, "")
	t.Setenv("RAG_ADMIN_TOKEN", "")
	path := filepath.Join(home, ".enowx-rag", "config.yaml")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("mkdir at config path: %v", err)
	}

	h := AdminTokenMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 for unreadable auth config", w.Code)
	}
}
