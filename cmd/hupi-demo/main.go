// Command hupi-demo serves the public hosted demo (docs/TODO.md #2):
// anonymous, short-lived guest sessions against the real
// gateway.Handler — retrieval, injection, and capture run completely
// unmodified, since internal/demo.Store implements gateway.Authenticator
// directly. Deliberately its own binary and its own listen address, not
// a mode flag on cmd/hupi: an anonymous public chat endpoint is a
// fundamentally different trust boundary than the self-hosted gateway
// every other deployment runs, and keeping it a separate process means a
// bug here can never affect a real self-hosted instance. Point
// HUPI_PROVIDERS_CONFIG at a dedicated providers.demo.yaml containing
// only cheap/fast model profiles — see providers.demo.yaml.example —
// so a public, unauthenticated endpoint can never reach an expensive
// model regardless of what a client requests
// (gateway.Handler.resolveProvider falls back to the registry's one chat
// profile for any model name it doesn't recognize).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"hupi/internal/auth"
	"hupi/internal/bootstrap"
	"hupi/internal/consolidation"
	"hupi/internal/demo"
	"hupi/internal/gateway"
	"hupi/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("hupi-demo exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	deps, err := bootstrap.Load(ctx)
	if err != nil {
		return err
	}
	defer deps.DB.Close()
	if err := bootstrap.VerifyEmbedding(ctx, deps); err != nil {
		return err
	}

	st := store.New(deps.DB, deps.Keys, deps.Registry.Embedding())
	authStore := auth.New(deps.DB, deps.Keys)
	runner := consolidation.New(
		deps.DB, deps.Keys,
		deps.Registry.Consolidation(), deps.Registry.Grounding(), deps.Registry.Embedding(),
	)
	demoStore := demo.New(deps.DB, authStore, runner, loadLimits())
	ipLimiter := demo.NewIPRateLimiter(envInt("HUPI_DEMO_MAX_SESSIONS_PER_IP_PER_HOUR", 3), time.Hour)

	handler := &gateway.Handler{
		Registry:  deps.Registry,
		Retriever: st,
		Capturer:  st,
		Auth:      demoStore,
	}

	allowedOrigin := envOr("HUPI_DEMO_ALLOWED_ORIGIN", "https://hupi.dev")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /demo/session", handleCreateSession(demoStore, ipLimiter))
	mux.HandleFunc("POST /v1/chat/completions", handler.HandleChatCompletions)
	mux.HandleFunc("POST /demo/consolidate-now", handleConsolidateNow(demoStore))
	mux.HandleFunc("GET /healthz", handleLiveness)
	mux.HandleFunc("GET /readyz", handleReadiness(deps.DB))

	addr := envOr("HUPI_DEMO_LISTEN_ADDR", "127.0.0.1:8789")
	srv := &http.Server{Addr: addr, Handler: withCORS(allowedOrigin, mux)}

	slog.Info("hupi-demo listening", "addr", addr, "allowed_origin", allowedOrigin)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func loadLimits() demo.Limits {
	l := demo.DefaultLimits
	l.SessionTTL = envDuration("HUPI_DEMO_SESSION_TTL", l.SessionTTL)
	l.MaxMessagesPerSession = envInt("HUPI_DEMO_MAX_MESSAGES_PER_SESSION", l.MaxMessagesPerSession)
	l.MaxConsolidatesPerSession = envInt("HUPI_DEMO_MAX_CONSOLIDATE_PER_SESSION", l.MaxConsolidatesPerSession)
	l.MaxSessionsPerDay = envInt("HUPI_DEMO_MAX_SESSIONS_PER_DAY", l.MaxSessionsPerDay)
	return l
}

func handleCreateSession(store *demo.Store, limiter *demo.IPRateLimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !limiter.Allow(clientIP(r)) {
			http.Error(w, "too many demo sessions from this address, try again later", http.StatusTooManyRequests)
			return
		}
		sess, err := store.CreateSession(r.Context())
		if err != nil {
			if errors.Is(err, demo.ErrDailySessionCapReached) {
				http.Error(w, "the demo has hit its daily visitor cap — please try again tomorrow, or install HUPI yourself (see docs/INSTALL.md)", http.StatusTooManyRequests)
				return
			}
			slog.Error("create demo session failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"token":              sess.Token,
			"expires_at":         sess.ExpiresAt,
			"messages_remaining": sess.MessagesRemaining,
		})
	}
}

func handleConsolidateNow(store *demo.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			http.Error(w, "missing Authorization: Bearer <demo token> header", http.StatusUnauthorized)
			return
		}
		err := store.ConsolidateNow(r.Context(), token)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, demo.ErrConsolidateLimitReached):
			http.Error(w, "consolidate-now limit reached for this session", http.StatusTooManyRequests)
		case errors.Is(err, demo.ErrSessionInvalid):
			http.Error(w, "demo session expired — start a new one", http.StatusUnauthorized)
		default:
			slog.Error("consolidate-now failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	}
}

func handleLiveness(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func handleReadiness(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "database unreachable: %v\n", err)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// bearerToken mirrors internal/gateway's own unexported helper of the
// same name — small enough that duplicating it here beats exporting it
// from gateway just for this one call site.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimPrefix(h, prefix)
}

// clientIP trusts X-Forwarded-For — safe only because this binary is
// meant to sit behind our own reverse proxy (see docs/INSTALL.md's demo
// deployment note), never directly internet-facing. Falls back to the
// raw connection's address when the header is absent, e.g. for local
// testing without a proxy in front.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// withCORS allows only HUPI_DEMO_ALLOWED_ORIGIN — this stops casual
// cross-site embedding from a browser, not a substitute for the cost
// caps in internal/demo, since a direct curl/script bypasses CORS
// entirely (it's a browser-enforced restriction, not a server-side one).
func withCORS(allowedOrigin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin == allowedOrigin {
			w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("invalid integer env var, using default", "var", name, "value", v, "default", def)
		return def
	}
	return n
}

func envDuration(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("invalid duration env var, using default", "var", name, "value", v, "default", def)
		return def
	}
	return d
}
