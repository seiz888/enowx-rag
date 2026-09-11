// Package httpapi provides the HTTP API layer for enowx-rag. It exposes
// REST endpoints and an SSE event stream over a chi router, serving both
// the API and an embedded React SPA from a single binary.
package httpapi

import (
	"io/fs"
	"net/http"

	"github.com/enowdev/enowx-rag/pkg/core"
	memgw "github.com/enowdev/enowx-rag/pkg/memgw/gateway"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter creates a chi router with all API routes and SPA fallback.
// svc is the core service layer shared with MCP stdio mode.
// ui is the embedded filesystem containing the SPA dist (index.html + assets).
// mcpHandler, when non-nil, is mounted at /mcp so agents can use enowx-rag as a
// remote MCP server; it is gated by the same RAG_ADMIN_TOKEN as /api.
// memgwHandler, when non-nil, is the memory gateway. It is mounted at /memgw
// and is deliberately NOT inside /api: the /api subtree is gated by
// AdminTokenMiddleware, which compares one shared token that names no
// principal, proves no scope, is held by every tool on the machine, and
// authorises everything when it is unset. The gateway authenticates each
// caller as a principal instead, and mounting it here would have forced all six
// agent hosts to hold the shared bearer that per-principal isolation exists to
// replace.
func NewRouter(svc *core.Service, ui fs.FS, mcpHandler, memgwHandler http.Handler) http.Handler {
	h := &Handlers{svc: svc}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)
	// NOTE: middleware.RealIP is intentionally NOT used. It rewrites
	// r.RemoteAddr from client-controlled X-Forwarded-For / X-Real-IP headers,
	// which would let a remote caller spoof a loopback address and bypass
	// LocalOrAdminMiddleware's localhost check on the setup endpoints.

	// API routes — protected by optional admin token middleware.
	// When RAG_ADMIN_TOKEN is set, all /api/* endpoints require an
	// Authorization: Bearer <token> header. When unset, no auth is required.
	r.Route("/api", func(r chi.Router) {
		r.Use(AdminTokenMiddleware)

		r.Get("/projects", h.ListProjects)
		r.Get("/projects/{id}", h.GetProject)
		r.Get("/projects/{id}/points", h.ListPoints)
		r.Get("/projects/{id}/export", h.ExportProject)
		r.Delete("/projects/{id}/points/{pointId}", h.DeletePoint)
		r.Post("/projects/{id}/reindex", h.ReindexProject)
		r.Delete("/projects/{id}", h.DeleteProject)
		r.Post("/search", h.Search)
		r.Get("/stats", h.Stats)
		r.Get("/version", h.Version)
		r.Get("/metrics", h.Metrics)
		r.Get("/queries", h.Queries)
		r.Get("/events", h.SSE)

		// Setup wizard endpoints. /setup/test and /setup/apply accept
		// user-supplied backend credentials and write config.yaml (with API
		// keys), so they are restricted to localhost or a valid admin token.
		// /setup/status only reports whether a config exists, so it is open.
		r.Group(func(r chi.Router) {
			r.Use(LocalOrAdminMiddleware)
			r.Post("/setup/test", h.SetupTest)
			r.Post("/setup/apply", h.SetupApply)
			// install-mcp writes to another tool's config file in the user's
			// home dir — same risk class as /setup/apply, so gate it too.
			r.Post("/setup/install-mcp", h.SetupInstallMCP)
			// write-agents-md writes AGENTS.md into a project dir — gate it.
			r.Post("/setup/write-agents-md", h.SetupWriteAgentsMD)
			// config reveal returns FULL secrets; update/gen-token write
			// config.yaml (with secrets) — all gated.
			r.Get("/setup/config/reveal", h.SetupConfigReveal)
			r.Post("/setup/config", h.SetupConfigUpdate)
			r.Post("/setup/gen-token", h.SetupGenToken)
			// migrate writes data into a destination vector store (and may use
			// user-supplied credentials) — gate it.
			r.Post("/migrate", h.Migrate)
		})
		r.Get("/setup/status", h.SetupStatus)
		// Read-only helpers for the install / agent-setup step (no file writes).
		r.Get("/setup/clients", h.SetupClients)
		r.Get("/setup/mcp-snippet", h.SetupMCPSnippet)
		r.Get("/setup/skill-guide", h.SetupSkillGuide)
		r.Get("/setup/probe", h.SetupProbe)
		r.Get("/setup/config", h.SetupConfig) // masked config (no full secrets)
		r.Get("/docs", h.DocsList)
		r.Get("/docs/setup", h.SetupDocs) // alias for the agent-setup section
		r.Get("/docs/{section}", h.DocsSection)

		// Unknown /api/ routes return 404 JSON (not SPA fallback)
		r.NotFound(h.NotFound)
	})

	// MCP over HTTP at /mcp — remote MCP transport. Gated by the same admin
	// token as /api. Must be registered before the SPA catch-all below, or the
	// catch-all would swallow /mcp requests.
	if mcpHandler != nil {
		r.Group(func(r chi.Router) {
			r.Use(AdminTokenMiddleware)
			r.Handle("/mcp", mcpHandler)
			r.Handle("/mcp/*", mcpHandler)
		})
	}

	// The memory gateway, with its own authentication. Registered before the
	// SPA catch-all, which would otherwise swallow /memgw and answer a write
	// with index.html.
	if memgwHandler != nil {
		r.Mount(memgw.MountPath, memgwHandler)
	}

	// SPA fallback: serve embedded dist files, fall back to index.html
	// for client-side routes (e.g., /playground, /chunks, /setup).
	if ui != nil {
		spa := &SPAHandler{root: ui}
		r.Handle("/*", spa)
	}

	return r
}

// SPAHandler serves static files from an embedded filesystem. For paths
// that don't match a real file, it falls back to index.html to enable
// client-side routing (SPA mode).
type SPAHandler struct {
	root fs.FS
}

func (s *SPAHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "" || path == "/" {
		path = "/index.html"
	}

	// Try to serve the file directly from the embedded FS.
	data, err := fs.ReadFile(s.root, path[1:]) // strip leading "/"
	if err == nil {
		w.Header().Set("Content-Type", contentTypeFor(path))
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.WriteHeader(http.StatusOK)
		w.Write(data)
		return
	}

	// Fall back to index.html for client-side routes.
	indexData, err := fs.ReadFile(s.root, "index.html")
	if err != nil {
		http.Error(w, "index.html not found in embedded dist", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	w.Write(indexData)
}

// contentTypeFor returns the Content-Type header value for common asset types.
func contentTypeFor(path string) string {
	switch {
	case endsWith(path, ".html"):
		return "text/html; charset=utf-8"
	case endsWith(path, ".js"):
		return "application/javascript"
	case endsWith(path, ".mjs"):
		return "application/javascript"
	case endsWith(path, ".css"):
		return "text/css"
	case endsWith(path, ".json"):
		return "application/json"
	case endsWith(path, ".svg"):
		return "image/svg+xml"
	case endsWith(path, ".png"):
		return "image/png"
	case endsWith(path, ".jpg"), endsWith(path, ".jpeg"):
		return "image/jpeg"
	case endsWith(path, ".gif"):
		return "image/gif"
	case endsWith(path, ".ico"):
		return "image/x-icon"
	case endsWith(path, ".woff"):
		return "font/woff"
	case endsWith(path, ".woff2"):
		return "font/woff2"
	case endsWith(path, ".ttf"):
		return "font/ttf"
	case endsWith(path, ".map"):
		return "application/json"
	case endsWith(path, ".txt"):
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

func endsWith(s, suffix string) bool {
	if len(suffix) > len(s) {
		return false
	}
	return s[len(s)-len(suffix):] == suffix
}
