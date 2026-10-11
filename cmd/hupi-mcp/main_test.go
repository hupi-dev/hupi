package main

import (
	"bytes"
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hupi/internal/gateway"
	"hupi/internal/identity"
	"hupi/internal/mcpserver"
	"hupi/internal/store"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// runRelay feeds stdin lines through a relay pointed at endpoint and
// returns the stdout lines.
func runRelay(t *testing.T, cfg config, stdin string) []string {
	t.Helper()
	var out bytes.Buffer
	r := newRelay(cfg, &http.Client{Timeout: 5 * time.Second}, &out, quietLog())
	if err := r.run(context.Background(), strings.NewReader(stdin)); err != nil {
		t.Fatalf("run: %v", err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	c, err := configFromEnv(env(nil))
	if err != nil || c.endpoint != "http://127.0.0.1:8787/mcp" || c.timeout != defaultTimeout || c.apiKey != "" {
		t.Errorf("defaults = %+v, %v", c, err)
	}
	c, err = configFromEnv(env(map[string]string{"HUPI_BASE_URL": "https://hupi.example.com/", "HUPI_TEAM_ID": "core team", "HUPI_API_KEY": " k ", "HUPI_MCP_TIMEOUT": "30s"}))
	if err != nil || c.endpoint != "https://hupi.example.com/v1/team/core%20team/mcp" || c.apiKey != "k" || c.timeout != 30*time.Second {
		t.Errorf("team config = %+v, %v", c, err)
	}
	for _, bad := range []map[string]string{
		{"HUPI_BASE_URL": "ftp://x"},
		{"HUPI_BASE_URL": "127.0.0.1:8787"},
		{"HUPI_MCP_TIMEOUT": "soon"},
		{"HUPI_MCP_TIMEOUT": "-1s"},
	} {
		if _, err := configFromEnv(env(bad)); err == nil {
			t.Errorf("configFromEnv(%v) accepted", bad)
		}
	}
}

func TestInsecureKeyWarning(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1:8787/mcp":    false,
		"http://localhost:8787/mcp":    false,
		"http://[::1]:8787/mcp":        false,
		"https://hupi.example.com/mcp": false,
		"http://hupi.example.com/mcp":  true,
		"http://10.0.0.5:8787/mcp":     true,
	}
	for ep, want := range cases {
		if got := (config{endpoint: ep, apiKey: "k"}).insecureKey(); got != want {
			t.Errorf("%s: insecureKey = %v, want %v", ep, got, want)
		}
	}
	if (config{endpoint: "http://hupi.example.com/mcp"}).insecureKey() {
		t.Errorf("warned with no API key")
	}
}

func TestRelay_HeadersAndProtocolVersion(t *testing.T) {
	var mu sync.Mutex
	var seen []http.Header
	var paths []string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		var env envelope
		_ = json.Unmarshal(body, &env)
		switch env.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			// Pretty-printed on purpose: the relay must compact it to one line.
			io.WriteString(w, "{\n  \"jsonrpc\": \"2.0\",\n  \"id\": 1,\n  \"result\": {\"protocolVersion\": \"2025-06-18\"}\n}")
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\ndata: \"result\":{\"tools\":[]}}\n\n")
		}
	}))
	defer gw.Close()

	stdin := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
	cfg := config{endpoint: gw.URL + "/v1/team/core/mcp", apiKey: "hupi_sk_test"}
	r := newRelay(cfg, gw.Client(), &bytes.Buffer{}, quietLog())
	var out bytes.Buffer
	r.out = &out
	if err := r.run(context.Background(), strings.NewReader(stdin)); err != nil {
		t.Fatal(err)
	}
	// A second batch on the same relay, after initialize negotiated a version.
	if err := r.run(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`+"\n")); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout = %q, want 2 lines (initialize result, tools/list result; nothing for the notification)", out.String())
	}
	if lines[0] != `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}` {
		t.Errorf("initialize line = %s", lines[0])
	}
	if lines[1] != `{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}` {
		t.Errorf("SSE line = %s", lines[1])
	}
	for i, h := range seen {
		if h.Get("Authorization") != "Bearer hupi_sk_test" || !strings.Contains(h.Get("Accept"), "text/event-stream") || paths[i] != "/v1/team/core/mcp" {
			t.Errorf("request %d: path %s headers %v", i, paths[i], h)
		}
	}
	if v := seen[len(seen)-1].Get("MCP-Protocol-Version"); v != "2025-06-18" {
		t.Errorf("tools/list MCP-Protocol-Version = %q, want the negotiated 2025-06-18", v)
	}
}

func TestRelay_GatewayErrorsBecomeJSONRPCErrors(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="hupi"`)
		http.Error(w, "invalid API key", http.StatusUnauthorized)
	}))
	defer gw.Close()

	lines := runRelay(t, config{endpoint: gw.URL + "/mcp", apiKey: "wrong"},
		`{"jsonrpc":"2.0","id":"a-1","method":"tools/list"}`+"\n"+
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"+
			"this is not json\n")
	if len(lines) != 2 {
		t.Fatalf("stdout = %q, want an error for the request and the parse error only", lines)
	}
	byID := map[string]map[string]any{}
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("stdout line isn't JSON: %q", l)
		}
		id, _ := json.Marshal(m["id"])
		byID[string(id)] = m
	}
	authErr := byID[`"a-1"`]["error"].(map[string]any)
	if msg := authErr["message"].(string); !strings.Contains(msg, "401") || !strings.Contains(msg, "HUPI_API_KEY") || strings.Contains(msg, "wrong") {
		t.Errorf("401 error message = %q", msg)
	}
	if code := byID["null"]["error"].(map[string]any)["code"].(float64); code != -32700 {
		t.Errorf("parse error code = %v", code)
	}
}

func TestRelay_UnreachableGateway(t *testing.T) {
	gw := httptest.NewServer(http.NotFoundHandler())
	url := gw.URL
	gw.Close()
	lines := runRelay(t, config{endpoint: url + "/mcp"}, `{"jsonrpc":"2.0","id":7,"method":"ping"}`+"\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"id":7`) || !strings.Contains(lines[0], "unreachable") {
		t.Errorf("stdout = %q", lines)
	}
}

func TestRelay_StopsOnContextCancelWhileStdinOpen(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- newRelay(config{endpoint: "http://127.0.0.1:1/mcp"}, http.DefaultClient, io.Discard, quietLog()).run(ctx, pr)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay didn't stop after cancel while blocked on stdin")
	}
}

// memBackend is just enough of mcpserver.Backend for an end-to-end run.
type memBackend struct {
	mu     sync.Mutex
	scopes []identity.Scope
	eps    []gateway.Episode
}

func (b *memBackend) Capture(_ context.Context, sc identity.Scope, ep gateway.Episode) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.scopes = append(b.scopes, sc)
	b.eps = append(b.eps, ep)
	return nil
}

func (b *memBackend) Search(_ context.Context, _, ws identity.Scope, q string, _ time.Time, _ store.SearchOptions) (store.SearchResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var res store.SearchResult
	for i, ep := range b.eps {
		if b.scopes[i] == ws && strings.Contains(strings.ToLower(ep.InputText), strings.ToLower(q)) {
			res.Citations = append(res.Citations, gateway.Citation{Ref: identity.Ref{Kind: identity.RefKindEpisode, Scope: ws, ID: ep.ID}, Snippet: ep.InputText})
		}
	}
	return res, nil
}

func (b *memBackend) LookupEntity(context.Context, identity.Scope, identity.Scope, string, store.ReadAudit) (store.EntityLookup, error) {
	return store.EntityLookup{}, nil
}

func (b *memBackend) ListEntities(context.Context, identity.Scope, identity.Scope, store.EntityFilter, store.ReadAudit) ([]store.EntityRow, error) {
	return nil, nil
}

func (b *memBackend) RecentSummaries(context.Context, identity.Scope, identity.Scope, store.RecentQuery, time.Time, store.ReadAudit) (store.RecentResult, error) {
	return store.RecentResult{}, nil
}

type keyAuth map[string]string

func (a keyAuth) Resolve(_ context.Context, key string) (identity.Identity, error) {
	if uid, ok := a[key]; ok {
		return identity.Identity{UserID: uid}, nil
	}
	return identity.Identity{}, io.ErrUnexpectedEOF
}

// TestEndToEnd_SDKClientThroughBridge runs the real MCP SDK client over
// stdio pipes, through the relay, into the real /mcp server.
func TestEndToEnd_SDKClientThroughBridge(t *testing.T) {
	backend := &memBackend{}
	ms := mcpserver.New(backend, keyAuth{"hupi_sk_alice": "user:alice"}, mcpserver.Config{})
	gw := httptest.NewServer(ms.Handler())
	defer gw.Close()

	clientToRelay, relayIn := io.Pipe()
	relayOut, relayToClient := io.Pipe()
	r := newRelay(config{endpoint: gw.URL, apiKey: "hupi_sk_alice", timeout: 5 * time.Second}, gw.Client(), relayToClient, quietLog())
	relayDone := make(chan error, 1)
	go func() {
		err := r.run(context.Background(), clientToRelay)
		relayToClient.Close()
		relayDone <- err
	}()

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "claude-desktop-standin", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.IOTransport{Reader: relayOut, Writer: relayIn}, nil)
	if err != nil {
		t.Fatalf("connect through bridge: %v", err)
	}

	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 6 {
		t.Fatalf("ListTools through bridge: %d tools, %v", len(tools.Tools), err)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "remember", Arguments: map[string]any{"fact": "The quoll service pages the infra rota."}})
	if err != nil || res.IsError {
		t.Fatalf("remember through bridge: %v %+v", err, res)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "search_memory", Arguments: map[string]any{"query": "quoll service"}})
	if err != nil || res.IsError {
		t.Fatalf("search through bridge: %v %+v", err, res)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(raw), "quoll service pages the infra rota") {
		t.Errorf("search result = %s", raw)
	}
	if len(backend.scopes) != 1 || backend.scopes[0].Owner != "user:alice" {
		t.Errorf("captured under %+v, want user:alice (the bridge's API key)", backend.scopes)
	}

	cs.Close()
	select {
	case err := <-relayDone:
		if err != nil {
			t.Errorf("relay exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("relay didn't exit after the client closed stdin")
	}
}

// TestDependencyGuard keeps the bridge stdlib-only: it must never link the
// database, encryption, provider or bootstrap packages, so it can be
// handed to an end user's machine holding nothing but their own API key.
func TestDependencyGuard(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if first := strings.SplitN(path, "/", 2)[0]; strings.Contains(first, ".") || first == "hupi" {
			t.Errorf("main.go imports %q; hupi-mcp must stay standard-library only", path)
		}
	}
}
