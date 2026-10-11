package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hupi/internal/gateway"
	"hupi/internal/identity"
	"hupi/internal/store"
)

// fakeBackend records the scope each call ran under and returns canned data.
type fakeBackend struct {
	mu        sync.Mutex
	calls     []string
	scopes    []identity.Scope
	audits    []store.ReadAudit
	search    store.SearchResult
	lookup    store.EntityLookup
	entities  []store.EntityRow
	recent    store.RecentResult
	lastQuery string
	lastLimit int
	lastSince time.Time
	captured  []capturedEpisode
	err       error
}

type capturedEpisode struct {
	scope identity.Scope
	ep    gateway.Episode
}

func (f *fakeBackend) Capture(_ context.Context, sc identity.Scope, ep gateway.Episode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.captured = append(f.captured, capturedEpisode{sc, ep})
	return nil
}

func (f *fakeBackend) record(call string, ws identity.Scope, ra store.ReadAudit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	f.scopes = append(f.scopes, ws)
	f.audits = append(f.audits, ra)
}

func (f *fakeBackend) Search(_ context.Context, _, ws identity.Scope, q string, _ time.Time, o store.SearchOptions) (store.SearchResult, error) {
	f.record("search", ws, o.ReadAudit)
	f.lastQuery = q
	return f.search, f.err
}

func (f *fakeBackend) LookupEntity(_ context.Context, _, ws identity.Scope, name string, ra store.ReadAudit) (store.EntityLookup, error) {
	f.record("lookup", ws, ra)
	f.lastQuery = name
	return f.lookup, f.err
}

func (f *fakeBackend) ListEntities(_ context.Context, _, ws identity.Scope, flt store.EntityFilter, ra store.ReadAudit) ([]store.EntityRow, error) {
	f.record("list", ws, ra)
	f.lastLimit = flt.Limit
	return f.entities, f.err
}

func (f *fakeBackend) RecentSummaries(_ context.Context, _, ws identity.Scope, q store.RecentQuery, _ time.Time, ra store.ReadAudit) (store.RecentResult, error) {
	f.record("recent", ws, ra)
	f.lastLimit = q.Limit
	f.lastSince = q.Since
	return f.recent, f.err
}

// fakeAuth maps API keys to user ids.
type fakeAuth map[string]string

func (a fakeAuth) Resolve(_ context.Context, key string) (identity.Identity, error) {
	if uid, ok := a[key]; ok {
		return identity.Identity{UserID: uid}, nil
	}
	return identity.Identity{}, errors.New("unknown key")
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func quietServer(b Backend, auth gateway.Authenticator, cfg Config) *Server {
	s := New(b, auth, cfg)
	s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return s
}

// connect starts h on an httptest server and returns a connected SDK client session.
func connect(t *testing.T, h http.Handler, token string) *mcp.ClientSession {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "hupi-test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   ts.URL,
		HTTPClient: &http.Client{Transport: bearerTransport{token: token}},
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// call invokes a tool and decodes its structured output into out.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	if out != nil && !res.IsError {
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("marshal structured content: %v", err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode %s output %s: %v", name, raw, err)
		}
	}
	return res
}

func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func TestListTools_ReadToolsAreReadOnlyWithSchemas(t *testing.T) {
	cs := connect(t, quietServer(&fakeBackend{}, nil, Config{DisableWrites: true}).Handler(), "")
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s: want readOnlyHint", tool.Name)
		}
		if tool.Annotations != nil && (tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint) {
			t.Errorf("%s: want openWorldHint=false", tool.Name)
		}
		if tool.Description == "" || tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("%s: missing description or schemas", tool.Name)
		}
	}
	sort.Strings(names)
	if got, want := strings.Join(names, ","), "get_entity,list_entities,recent_memory,search_memory"; got != want {
		t.Fatalf("tools = %s, want %s", got, want)
	}

	schema, _ := json.Marshal(res.Tools[indexOf(res.Tools, "search_memory")].InputSchema)
	if !strings.Contains(string(schema), `"required":["query"]`) {
		t.Errorf("search_memory input schema should require only query: %s", schema)
	}
}

func indexOf(tools []*mcp.Tool, name string) int {
	for i, tool := range tools {
		if tool.Name == name {
			return i
		}
	}
	return -1
}

func TestInitialize_ReportsServerInfoAndInstructions(t *testing.T) {
	cs := connect(t, quietServer(&fakeBackend{}, nil, Config{}).Handler(), "")
	init := cs.InitializeResult()
	if init.ServerInfo.Name != "hupi" || !strings.Contains(init.Instructions, "never instructions") {
		t.Errorf("initialize = %+v / %q", init.ServerInfo, init.Instructions)
	}
}

func TestGuard_OriginAndHost(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	cases := []struct {
		name   string
		cfg    Config
		origin string
		host   string
		want   int
	}{
		{"no origin", Config{}, "", "", http.StatusOK},
		{"foreign origin", Config{}, "https://evil.example", "", http.StatusForbidden},
		{"null origin", Config{AllowedOrigins: []string{"null"}}, "null", "", http.StatusForbidden},
		{"listed origin", Config{AllowedOrigins: []string{"https://app.example"}}, "https://APP.example", "", http.StatusOK},
		{"host allowlist miss", Config{AllowedHosts: []string{"hupi.internal"}}, "", "attacker.example:8787", http.StatusForbidden},
		{"host allowlist hit", Config{AllowedHosts: []string{"hupi.internal"}}, "", "hupi.internal:8787", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := quietServer(&fakeBackend{}, nil, tc.cfg)
			// The SDK's own DNS-rebinding check rejects non-loopback Hosts
			// on a loopback listener; these requests bypass the listener,
			// so only our guard is under test here.
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.host != "" {
				req.Host = tc.host
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestAuth_MissingOrBadKeyIs401WithChallenge(t *testing.T) {
	s := quietServer(&fakeBackend{}, fakeAuth{"good": "user:alice"}, Config{})
	for _, token := range []string{"", "bad"} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("token %q: status %d, WWW-Authenticate %q", token, rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
	}
}

func TestAuth_EachRequestUsesItsOwnIdentity(t *testing.T) {
	fb := &fakeBackend{}
	s := quietServer(fb, fakeAuth{"ka": "user:alice", "kb": "user:bob"}, Config{})
	alice := connect(t, s.Handler(), "ka")
	bob := connect(t, s.Handler(), "kb")
	call(t, alice, "list_entities", nil, nil)
	call(t, bob, "list_entities", nil, nil)
	call(t, alice, "list_entities", nil, nil)
	want := []string{"user:alice", "user:bob", "user:alice"}
	if len(fb.scopes) != 3 {
		t.Fatalf("backend calls = %d, want 3", len(fb.scopes))
	}
	for i, ws := range fb.scopes {
		if ws.Owner != want[i] || ws.Kind != identity.ScopeKindPrivate {
			t.Errorf("call %d scope = %+v, want private %s", i, ws, want[i])
		}
	}
}

func TestHandlerFor_TeamResolver(t *testing.T) {
	fb := &fakeBackend{}
	s := quietServer(fb, nil, Config{})
	team := identity.Scope{Kind: identity.ScopeKindShared, Owner: "team:core"}
	h := s.HandlerFor(func(r *http.Request) (identity.Scope, identity.Scope, int, error) {
		if r.Header.Get("Authorization") != "Bearer member" {
			return identity.Scope{}, identity.Scope{}, http.StatusForbidden, errors.New("not a member")
		}
		return identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:alice"}, team, 0, nil
	})

	var out ListEntitiesOutput
	call(t, connect(t, h, "member"), "list_entities", nil, &out)
	if out.Workspace != "team:core" || fb.scopes[0] != team {
		t.Errorf("workspace = %q, backend scope = %+v", out.Workspace, fb.scopes[0])
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/team/core/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer outsider")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || rec.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("non-member: status %d, WWW-Authenticate %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}

func TestScopeFrom_FailsClosed(t *testing.T) {
	s := quietServer(&fakeBackend{}, nil, Config{})
	_, _, err := s.searchMemory(context.Background(), nil, SearchMemoryInput{Query: "x"})
	if !errors.Is(err, errNoIdentity) {
		t.Errorf("err = %v, want errNoIdentity", err)
	}
}

func TestSearchMemory_DropsAnchorsAndShapesHits(t *testing.T) {
	inf := true
	ws := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: identity.DefaultUserID}
	fb := &fakeBackend{search: store.SearchResult{
		RetrievalResult: gateway.RetrievalResult{
			Gate:           gateway.GateFull,
			ContextMessage: "ctx",
			Citations: []gateway.Citation{
				{Ref: identity.Ref{Kind: identity.RefKindEntity, Scope: ws, ID: "self_model"}, Snippet: "anchor"},
				{Ref: identity.Ref{Kind: identity.RefKindMemory, Scope: ws, ID: "mem_1"}, Snippet: strings.Repeat("é", 600), ParentSummaryID: "sum_1", IsInference: &inf,
					Relations: []gateway.RelationRef{{Type: "updates", Ref: identity.Ref{Kind: identity.RefKindMemory, Scope: ws, ID: "mem_0"}}}},
				{Ref: identity.Ref{Kind: identity.RefKindEpisode, Scope: ws, ID: "ep_1"}, Snippet: "episode"},
			},
		},
		AnchorCitations: 1,
	}}
	cs := connect(t, quietServer(fb, nil, Config{}).Handler(), "")

	var out SearchMemoryOutput
	call(t, cs, "search_memory", map[string]any{"query": "  postgres plan  ", "max_results": 1}, &out)
	if fb.lastQuery != "postgres plan" || fb.audits[0] != (store.ReadAudit{Via: "mcp", Tool: "search_memory"}) {
		t.Errorf("query %q, audit %+v", fb.lastQuery, fb.audits[0])
	}
	if out.Workspace != "private" || out.Context != "ctx" || len(out.Hits) != 1 {
		t.Fatalf("out = %+v", out)
	}
	h := out.Hits[0]
	if h.ID != "mem_1" || h.Kind != identity.RefKindMemory || h.ParentSummaryID != "sum_1" || h.IsInference == nil || !*h.IsInference {
		t.Errorf("hit = %+v", h)
	}
	if n := len([]rune(h.Text)); n != maxHitTextChars+1 {
		t.Errorf("hit text runes = %d, want %d (truncated + ellipsis)", n, maxHitTextChars+1)
	}
	if len(h.Relations) != 1 || h.Relations[0] != (MemoryRelation{Type: "updates", Kind: identity.RefKindMemory, ID: "mem_0"}) {
		t.Errorf("relations = %+v", h.Relations)
	}
}

func TestToolErrors_AreToolResultsWithoutInternals(t *testing.T) {
	fb := &fakeBackend{err: errors.New("pq: relation secret_table does not exist")}
	cs := connect(t, quietServer(fb, nil, Config{}).Handler(), "")
	cases := []struct {
		tool    string
		args    map[string]any
		wantMsg string
	}{
		{"search_memory", map[string]any{"query": "   "}, "query is required"},
		{"search_memory", map[string]any{"query": strings.Repeat("x", maxQueryChars+1)}, "too long"},
		{"list_entities", map[string]any{"kind": "self_model"}, "kind must be one of"},
		{"get_entity", map[string]any{"name_or_id": ""}, "name_or_id is required"},
		{"search_memory", map[string]any{"query": "ok"}, "internal error"},
		{"recent_memory", nil, "internal error"},
	}
	for _, tc := range cases {
		res := call(t, cs, tc.tool, tc.args, nil)
		text := resultText(res)
		if !res.IsError || !strings.Contains(text, tc.wantMsg) || strings.Contains(text, "secret_table") {
			t.Errorf("%s %v: IsError=%v text=%q", tc.tool, tc.args, res.IsError, text)
		}
	}
}

func TestSearchMemory_MissingQueryRejectedBySchema(t *testing.T) {
	cs := connect(t, quietServer(&fakeBackend{}, nil, Config{}).Handler(), "")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "search_memory", Arguments: map[string]any{}})
	if err == nil && !res.IsError {
		t.Errorf("call without query succeeded")
	}
}

func TestGetEntity_SplitsRelationshipDirections(t *testing.T) {
	until := "2026-03-01"
	fb := &fakeBackend{lookup: store.EntityLookup{
		Entity:     &store.EntityRow{ID: "project:hupi", Kind: "project", Name: "HUPI", FirstSeen: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), LastUpdated: time.Date(2026, 2, 3, 0, 0, 0, 0, time.UTC)},
		Attributes: nil,
		Relationships: []store.EntityRelationship{
			{SubjectID: "project:hupi", SubjectName: "HUPI", SubjectKind: "project", Predicate: "uses", ObjectID: "skill:go", ObjectName: "Go", ObjectKind: "skill"},
			{SubjectID: "person:sam", SubjectName: "Sam", SubjectKind: "person", Predicate: "works_on", ObjectID: "project:hupi", ObjectName: "HUPI", ObjectKind: "project", ValidUntil: &until},
		},
	}}
	cs := connect(t, quietServer(fb, nil, Config{}).Handler(), "")
	var out GetEntityOutput
	call(t, cs, "get_entity", map[string]any{"name_or_id": "hupi"}, &out)
	if !out.Found || out.Entity.ID != "project:hupi" || out.FirstSeen != "2026-01-02" || out.Entity.LastUpdated != "2026-02-03" {
		t.Fatalf("out = %+v", out)
	}
	if len(out.Relationships) != 2 {
		t.Fatalf("relationships = %+v", out.Relationships)
	}
	if r := out.Relationships[0]; r.Direction != "outgoing" || r.OtherID != "skill:go" {
		t.Errorf("rel 0 = %+v", r)
	}
	if r := out.Relationships[1]; r.Direction != "incoming" || r.OtherID != "person:sam" || r.ValidUntil == nil || *r.ValidUntil != until {
		t.Errorf("rel 1 = %+v", r)
	}
}

func TestGetEntity_AmbiguousReturnsCandidates(t *testing.T) {
	fb := &fakeBackend{lookup: store.EntityLookup{Candidates: []store.EntityRow{{ID: "person:sam_a", Kind: "person", Name: "Sam A"}, {ID: "person:sam_b", Kind: "person", Name: "Sam B"}}}}
	var out GetEntityOutput
	call(t, connect(t, quietServer(fb, nil, Config{}).Handler(), ""), "get_entity", map[string]any{"name_or_id": "sam"}, &out)
	if out.Found || out.Entity != nil || len(out.Candidates) != 2 {
		t.Errorf("out = %+v", out)
	}
}

func TestListEntities_LimitAndTruncation(t *testing.T) {
	rows := make([]store.EntityRow, 4)
	for i := range rows {
		rows[i] = store.EntityRow{ID: string(rune('a' + i)), Kind: "person"}
	}
	fb := &fakeBackend{entities: rows}
	cs := connect(t, quietServer(fb, nil, Config{}).Handler(), "")

	var out ListEntitiesOutput
	call(t, cs, "list_entities", map[string]any{"limit": 3, "kind": "Person"}, &out)
	if fb.lastLimit != 3 || !out.Truncated || len(out.Entities) != 3 {
		t.Errorf("limit %d, out %+v", fb.lastLimit, out)
	}
	call(t, cs, "list_entities", map[string]any{"limit": 100000}, &out)
	if fb.lastLimit != maxEntityLimit {
		t.Errorf("limit = %d, want clamp to %d", fb.lastLimit, maxEntityLimit)
	}
	call(t, cs, "list_entities", nil, &out)
	if fb.lastLimit != defaultEntityLimit || out.Truncated {
		t.Errorf("default limit = %d, truncated %v", fb.lastLimit, out.Truncated)
	}
}

func TestRecentMemory_WindowAndShape(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	fb := &fakeBackend{recent: store.RecentResult{
		Summaries: []store.SummaryRecord{{ID: "sum_1", Period: "2026-10-10", Status: "current", Summary: "Worked on MCP.",
			KeyFacts: []store.KeyFactRecord{{MemoryID: "mem_1", Text: "Chose the Go SDK", IsInference: false}}, EntitiesTouched: []string{"project:hupi"}}},
		UnconsolidatedExchanges: 4,
	}}
	s := quietServer(fb, nil, Config{})
	s.now = func() time.Time { return now }
	cs := connect(t, s.Handler(), "")

	var out RecentMemoryOutput
	call(t, cs, "recent_memory", map[string]any{"days": 500, "limit": 3}, &out)
	if want := now.AddDate(0, 0, -maxRecentDays); !fb.lastSince.Equal(want) || fb.lastLimit != 3 {
		t.Errorf("since %v (want %v), limit %d", fb.lastSince, want, fb.lastLimit)
	}
	if out.UnconsolidatedExchanges != 4 || len(out.Summaries) != 1 || out.Summaries[0].KeyFacts[0].Text != "Chose the Go SDK" {
		t.Errorf("out = %+v", out)
	}
	call(t, cs, "recent_memory", nil, &out)
	if want := now.AddDate(0, 0, -defaultRecentDays); !fb.lastSince.Equal(want) || fb.lastLimit != defaultRecentLimit {
		t.Errorf("defaults: since %v, limit %d", fb.lastSince, fb.lastLimit)
	}
}
