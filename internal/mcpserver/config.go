package mcpserver

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// Config holds the /mcp endpoint's environment-driven settings.
type Config struct {
	// AllowedOrigins lists the browser origins allowed to call /mcp. MCP
	// clients that aren't browsers (Claude Code, Cursor, hupi-mcp) send no
	// Origin header at all and are unaffected; a request that does carry
	// one is rejected unless it's listed here.
	AllowedOrigins []string
	// AllowedHosts, if non-empty, restricts the Host header — for a
	// deployment listening on a non-loopback address. Loopback deployments
	// are already covered by the SDK's built-in DNS-rebinding protection.
	AllowedHosts []string
	// ContextCharBudget caps search_memory's compact context text.
	ContextCharBudget int
	// MaxBodyBytes caps a single JSON-RPC request body.
	MaxBodyBytes int64
}

const (
	defaultContextCharBudget = 6000
	defaultMaxBodyBytes      = 1 << 20
)

// Enabled reports whether cmd/hupi should mount /mcp (on unless HUPI_ENABLE_MCP=false).
func Enabled() bool {
	return os.Getenv("HUPI_ENABLE_MCP") != "false"
}

// ConfigFromEnv reads HUPI_MCP_* settings.
func ConfigFromEnv() Config {
	return Config{
		AllowedOrigins:    splitList(os.Getenv("HUPI_MCP_ALLOWED_ORIGINS")),
		AllowedHosts:      splitList(os.Getenv("HUPI_MCP_ALLOWED_HOSTS")),
		ContextCharBudget: envInt("HUPI_MCP_CONTEXT_CHAR_BUDGET", defaultContextCharBudget),
		MaxBodyBytes:      int64(envInt("HUPI_MCP_MAX_BODY_BYTES", defaultMaxBodyBytes)),
	}
}

func (c Config) withDefaults() Config {
	if c.ContextCharBudget <= 0 {
		c.ContextCharBudget = defaultContextCharBudget
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = defaultMaxBodyBytes
	}
	return c
}

// checkRequest rejects cross-origin browser requests and, when configured,
// unexpected Host headers. Returns 0 if the request may proceed.
func (c Config) checkRequest(r *http.Request) (int, string) {
	if origin := r.Header.Get("Origin"); origin != "" {
		if origin == "null" || !contains(c.AllowedOrigins, origin) {
			return http.StatusForbidden, "origin not allowed"
		}
	}
	if len(c.AllowedHosts) > 0 {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !contains(c.AllowedHosts, strings.ToLower(host)) {
			return http.StatusForbidden, "host not allowed"
		}
	}
	return 0, ""
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.ToLower(strings.TrimSpace(part)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func contains(list []string, v string) bool {
	v = strings.ToLower(v)
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
