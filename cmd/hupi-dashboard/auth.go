package main

import (
	"context"
	"net/http"

	"hupi/internal/identity"
)

// mountDashboardAuthRoutes and resolveDashboardSession are nil in this
// OSS build — set via init() by hupi-t3's cmd/hupi-dashboard/team.go
// overlay file, the same hook-var pattern cmd/hupi-admin-ui/handlers.go
// already uses for mountTeamRoutes. mountDashboardAuthRoutes mounts the
// password/OIDC login routes (outside requireDashboardSession, same
// reasoning as assets.go's serveSPA); resolveDashboardSession turns an
// Authorization: Bearer <dashboard session token> header into the real
// per-user identity.Scope that token was minted for.
var (
	mountDashboardAuthRoutes func(mux *http.ServeMux, s *server)
	resolveDashboardSession  func(r *http.Request) (identity.Scope, bool)
)

// scopeContextKey is unexported by design — scopeFromContext is the only
// way to read the resolved scope back out, so every handler goes through
// the same accessor regardless of which tier set it.
type scopeContextKey struct{}

func scopeFromContext(ctx context.Context) identity.Scope {
	if scope, ok := ctx.Value(scopeContextKey{}).(identity.Scope); ok {
		return scope
	}
	return identity.DefaultScope
}

// requireDashboardSession resolves every /api/ request to a scope before
// it reaches routes(). When resolveDashboardSession is nil (Tier 1/2,
// this OSS build), every request resolves straight to
// identity.DefaultScope with no header required at all — the same
// zero-auth posture cmd/hupi's own gateway already has for Tier 1/2 (see
// internal/gateway/handler.go's resolveIdentity). When hupi-t3's overlay
// sets resolveDashboardSession, a missing or invalid session token is
// rejected outright.
//
// Deliberately no CSRF mitigation here, unlike cmd/hupi-admin-ui's Basic
// Auth: a bearer token attached by this SPA's own JavaScript is never
// resent automatically by the browser the way Basic Auth credentials or
// a cookie would be, so the classic Basic-auth/cookie CSRF gap
// hupi-admin-ui's requireCSRFSafe defends against doesn't apply to a
// bearer-token session at all.
func requireDashboardSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if resolveDashboardSession == nil {
			ctx := context.WithValue(r.Context(), scopeContextKey{}, identity.DefaultScope)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		scope, ok := resolveDashboardSession(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="hupi-dashboard"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), scopeContextKey{}, scope)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
