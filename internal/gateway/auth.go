package gateway

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"hupi/internal/identity"
	"hupi/internal/metrics"
)

// ResolveIdentity authenticates a request the same way for every route
// that serves user memory. With auth nil (Tier 1/2 mode), every request
// resolves to identity.DefaultUserID with no team memberships and no
// Authorization header required. With auth set, a valid
// `Authorization: Bearer <key>` header is required.
func ResolveIdentity(r *http.Request, auth Authenticator, log *slog.Logger) (identity.Identity, error) {
	if auth == nil {
		return identity.Identity{UserID: identity.DefaultUserID}, nil
	}
	token := BearerToken(r)
	if token == "" {
		metrics.AuthResolveTotal.WithLabelValues("missing").Inc()
		return identity.Identity{}, errors.New("missing Authorization: Bearer <api key> header")
	}
	id, err := auth.Resolve(r.Context(), token)
	if err != nil {
		// The real reason (wrong audience, expired, JWKS fetch failure,
		// unknown key hash, ...) is deliberately not returned to the
		// caller — that would let an attacker probe why a token was
		// rejected. It's still worth an operator being able to see it.
		log.Warn("auth: token rejected", "err", err)
		metrics.AuthResolveTotal.WithLabelValues("invalid").Inc()
		return identity.Identity{}, errors.New("invalid API key")
	}
	metrics.AuthResolveTotal.WithLabelValues("ok").Inc()
	return id, nil
}

// BearerToken extracts the token from an `Authorization: Bearer <token>`
// header, or "" if the header is missing or malformed.
func BearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimPrefix(h, prefix)
}
