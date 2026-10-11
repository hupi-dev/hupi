package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hupi/internal/gateway"
	"hupi/internal/identity"
)

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`

func postMCP(mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(initializeBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestMountMCP(t *testing.T) {
	t.Run("mounted by default", func(t *testing.T) {
		// An Enterprise build's init() sets the hook; test the OSS state.
		prev := gateway.ResolveTeamScope
		gateway.ResolveTeamScope = nil
		t.Cleanup(func() { gateway.ResolveTeamScope = prev })
		mux := http.NewServeMux()
		mountMCP(mux, nil, nil, &gateway.Handler{})
		rec := postMCP(mux, "/mcp")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"hupi"`) {
			t.Errorf("POST /mcp initialize: %d %s", rec.Code, rec.Body.String())
		}
		if rec := postMCP(mux, "/v1/team/core/mcp"); rec.Code != http.StatusNotFound {
			t.Errorf("team route without ResolveTeamScope: %d, want 404", rec.Code)
		}
	})

	t.Run("HUPI_ENABLE_MCP=false unmounts it", func(t *testing.T) {
		t.Setenv("HUPI_ENABLE_MCP", "false")
		mux := http.NewServeMux()
		mountMCP(mux, nil, nil, &gateway.Handler{})
		if rec := postMCP(mux, "/mcp"); rec.Code != http.StatusNotFound {
			t.Errorf("POST /mcp with MCP disabled: %d, want 404", rec.Code)
		}
	})

	t.Run("team route uses ResolveTeamScope", func(t *testing.T) {
		prev := gateway.ResolveTeamScope
		t.Cleanup(func() { gateway.ResolveTeamScope = prev })
		h := &gateway.Handler{}
		var gotTeam string
		var gotHandler *gateway.Handler
		gateway.ResolveTeamScope = func(hh *gateway.Handler, r *http.Request) (identity.Scope, identity.Scope, int, error) {
			gotTeam, gotHandler = r.PathValue("team_id"), hh
			return identity.DefaultScope, identity.Scope{Kind: identity.ScopeKindShared, Owner: "team:" + gotTeam}, 0, nil
		}
		mux := http.NewServeMux()
		mountMCP(mux, nil, nil, h)
		if rec := postMCP(mux, "/v1/team/core/mcp"); rec.Code != http.StatusOK {
			t.Errorf("POST team mcp: %d %s", rec.Code, rec.Body.String())
		}
		if gotTeam != "core" || gotHandler != h {
			t.Errorf("hook got team %q, handler %p (want %p)", gotTeam, gotHandler, h)
		}
	})
}
