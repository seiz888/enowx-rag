// Package gateway is the memory gateway's HTTP surface: authenticate a
// principal, submit an event to the ledger, look up a receipt, and answer the
// bootstrap a host needs when it starts a session.
//
// It is a subrouter inside the existing enowx-rag service, not a second
// service. There is one process, one config, one deployment identity and one
// place a request can be traced through; a separate gateway binary would have
// meant two of each and a running argument about which one is authoritative.
//
// It does not mount under /api. See auth.go for why.
package gateway

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/enowdev/enowx-rag/pkg/memgw/domain"
	"github.com/enowdev/enowx-rag/pkg/memgw/ledger"
	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
	"github.com/enowdev/enowx-rag/pkg/memgw/principal"
)

// MountPath is where the gateway is mounted in the service router. It is a
// constant so the router, the adapters and the documentation cannot drift.
const MountPath = "/memgw"

// Per-operation deadlines. The server's WriteTimeout is 0 because /mcp is a
// streaming transport and a write deadline would cut long-lived responses; that
// makes it this package's job to bound its own work. Every route here answers
// in one response, so each one gets a deadline sized to what it does.
//
// These are ceilings, not budgets: they exist so a stuck database cannot
// accumulate connections held by clients that stopped waiting long ago.
const (
	authTimeout      = 5 * time.Second
	submitTimeout    = 15 * time.Second
	readTimeout      = 10 * time.Second
	bootstrapTimeout = 20 * time.Second
)

// maxBodyBytes caps a request body. The ledger has its own payload limit and
// enforces it on the canonical form; this is the cruder guard that stops the
// service reading a gigabyte before it gets the chance.
const maxBodyBytes = 1 << 20

// Gateway serves the memory gateway routes.
type Gateway struct {
	pool *pgstore.Pool
	auth *principal.Store
	sub  *ledger.Submitter
	reg  *domain.Registry
}

// New returns a gateway over an already-verified pool. The pool is the caller's
// to close; nothing here owns it, because the same pool serves the outbox
// worker and the CLI in the same process.
func New(pool *pgstore.Pool) *Gateway {
	return &Gateway{
		pool: pool,
		auth: principal.NewStore(pool),
		sub:  ledger.NewSubmitter(pool),
		reg:  domain.NewRegistry(pool),
	}
}

// Routes returns the gateway's handler, to be mounted at MountPath.
//
// Every route is behind authenticate. There is no unauthenticated route at all,
// not even a health check: a health endpoint that answered before
// authentication would be the one route an unauthenticated caller could use to
// learn the gateway exists and is reachable, and the service already has a
// health check of its own.
func (g *Gateway) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(g.authenticate)

	r.Route("/v1", func(r chi.Router) {
		r.Post("/events", g.submitEvent)
		r.Get("/receipts/{idempotencyKey}", g.getReceipt)
		r.Post("/bootstrap", g.postBootstrap)

		r.Post("/projects", g.ensureProject)
		r.Post("/workspaces", g.ensureWorkspace)
		r.Post("/works/ensure", g.ensureWork)
		r.Get("/projects/by-repo", g.projectByRepo)
	})

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeProblem(w, http.StatusNotFound, "not_found", "no such gateway route")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "that method is not allowed here")
	})
	return r
}
