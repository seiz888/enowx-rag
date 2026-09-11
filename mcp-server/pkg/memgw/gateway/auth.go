package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/enowdev/enowx-rag/pkg/memgw/principal"
)

// Why this is not pkg/httpapi.AdminTokenMiddleware.
//
// That middleware compares one shared token, RAG_ADMIN_TOKEN, and authorises
// everything that passes. It is not a principal: it names nobody, proves no
// scope, cannot be narrowed to a project, is held by every tool on this
// machine, and is currently treated as compromised. When it is unset it
// authorises everything unauthenticated.
//
// The gateway therefore mounts outside /api with its own authentication. A
// caller presents a credential issued to a principal; what comes back is the
// principals row, and every later decision -- scope, roles, the principal_id
// written into the event -- is made from that row. Mounting under /api instead
// would have made every one of the six hosts hold the shared bearer, which is
// the arrangement per-principal isolation exists to replace.

type ctxKey int

const principalKey ctxKey = iota

// authenticate resolves the bearer credential to a principal and puts it in the
// request context. A request without one never reaches a handler.
func (g *Gateway) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r)
		if !ok {
			// The realm names the scheme so a client knows what to present.
			// It names nothing about why the request failed.
			w.Header().Set("WWW-Authenticate", `Bearer realm="memgw"`)
			writeProblem(w, http.StatusUnauthorized, "unauthenticated", "a memgw credential is required")
			return
		}

		// Authentication is a bounded operation -- one indexed read and one
		// key derivation -- so it gets its own deadline rather than inheriting
		// however long the client is willing to wait.
		ctx, cancel := context.WithTimeout(r.Context(), authTimeout)
		defer cancel()

		who, err := g.auth.Authenticate(ctx, token)
		if err != nil {
			if errors.Is(err, principal.ErrBadCredential) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="memgw"`)
				writeProblem(w, http.StatusUnauthorized, "unauthenticated", "the credential is not valid")
				return
			}
			writeError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, who)))
	})
}

// caller returns the authenticated principal. It is only ever called from a
// handler behind authenticate, so a missing principal is a routing bug rather
// than an anonymous request, and it fails closed.
func caller(ctx context.Context) (principal.Principal, bool) {
	p, ok := ctx.Value(principalKey).(principal.Principal)
	return p, ok
}

// bearer extracts the credential from the Authorization header.
//
// Only the header is read. A token accepted from a query parameter would end
// up in access logs, browser history and referrers -- the ways secrets usually
// escape are not cryptographic.
func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	scheme, value, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	return value, true
}
