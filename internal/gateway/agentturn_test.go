package gateway

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"hupi/internal/metrics"
	"hupi/internal/provider"
)

// scriptedUpstream is a fake OpenAI-compatible upstream that records each
// request body and answers with reply(body) — a JSON response, or an SSE
// stream when the request asked for one.
type scriptedUpstream struct {
	mu     sync.Mutex
	bodies []map[string]any
	reply  func(body map[string]any) (message map[string]any, finish string)
	// streamReply, if set, writes the SSE stream itself.
	streamReply func(w http.ResponseWriter, r *http.Request, body map[string]any)
}

func (u *scriptedUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	u.mu.Lock()
	u.bodies = append(u.bodies, body)
	u.mu.Unlock()
	if body["stream"] == true && u.streamReply != nil {
		w.Header().Set("Content-Type", "text/event-stream")
		u.streamReply(w, r, body)
		return
	}
	msg, finish := u.reply(body)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model":   "fake-model",
		"choices": []map[string]any{{"message": msg, "finish_reason": finish}},
		"usage":   map[string]int{"total_tokens": 2},
	})
}

func newAgentHandler(t *testing.T, up *scriptedUpstream, profile provider.ProfileConfig) (*Handler, *fakeRetriever, *fakeCapturer) {
	t.Helper()
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	profile.Kind, profile.BaseURL, profile.Model = provider.KindOpenAICompat, srv.URL, "fake-model"
	reg, err := provider.NewRegistry(provider.Config{
		ActiveChatProvider: "test", ActiveConsolidationProvider: "test", ActiveEmbeddingProvider: "test",
		Providers: map[string]provider.ProfileConfig{"test": profile},
	})
	if err != nil {
		t.Fatal(err)
	}
	retriever := &fakeRetriever{result: RetrievalResult{Gate: GateFull, ContextMessage: "MEMORY-CONTEXT"}}
	capturer := &fakeCapturer{}
	return &Handler{Registry: reg, Retriever: retriever, Capturer: capturer}, retriever, capturer
}

const weatherTool = `{"type":"function","function":{"name":"get_weather","description":"Current weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}`
const orderTool = `{"type":"function","function":{"name":"lookup_order","parameters":{"type":"object"}}}`

func post(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleChatCompletions(w, req)
	return w
}

func toolCallMsg(id, name, args string) map[string]any {
	return map[string]any{"role": "assistant", "content": nil, "tool_calls": []map[string]any{
		{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}},
	}}
}

// A three-request agent turn: two tool-call steps, then the answer.
func TestAgentTurn_NonStream_RetrievesOnceCapturesOnce(t *testing.T) {
	up := &scriptedUpstream{reply: func(body map[string]any) (map[string]any, string) {
		msgs := body["messages"].([]any)
		switch len(msgs) {
		case 2: // system memory + user question
			return toolCallMsg("call_1", "lookup_order", `{"order_id":"A-1"}`), "tool_calls"
		case 4: // + assistant call + tool result
			return toolCallMsg("call_2", "get_weather", `{"city":"Oslo"}`), "tool_calls"
		default:
			return map[string]any{"role": "assistant", "content": "Your order shipped; Oslo is 5C."}, "stop"
		}
	}}
	h, retriever, capturer := newAgentHandler(t, up, provider.ProfileConfig{})
	hitsBefore := testutil.ToFloat64(metrics.AgentTurnCacheTotal.WithLabelValues("hit"))

	question := `{"role":"user","content":"Where is order A-1, and what's the weather in Oslo?"}`
	tools := `"tools":[` + orderTool + `,` + weatherTool + `]`
	step1 := `{"model":"test",` + tools + `,"messages":[` + question + `]}`
	w := post(t, h, step1)
	if w.Code != http.StatusOK {
		t.Fatalf("step 1: %d %s", w.Code, w.Body.String())
	}
	var resp1 struct {
		Choices []struct {
			Message      map[string]any `json:"message"`
			FinishReason string         `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp1); err != nil {
		t.Fatal(err)
	}
	c := resp1.Choices[0]
	if c.FinishReason != "tool_calls" || c.Message["content"] != nil || len(c.Message["tool_calls"].([]any)) != 1 {
		t.Errorf("step 1 response = %s", w.Body.String())
	}

	call1 := `{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup_order","arguments":"{\"order_id\":\"A-1\"}"}}]}`
	result1 := `{"role":"tool","tool_call_id":"call_1","content":"SECRET-ORDER-RESULT shipped"}`
	call2 := `{"role":"assistant","content":null,"tool_calls":[{"id":"call_2","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}}]}`
	result2 := `{"role":"tool","tool_call_id":"call_2","content":"SECRET-WEATHER-RESULT 5C"}`
	for i, body := range []string{
		`{"model":"test",` + tools + `,"messages":[` + question + `,` + call1 + `,` + result1 + `]}`,
		`{"model":"test",` + tools + `,"messages":[` + question + `,` + call1 + `,` + result1 + `,` + call2 + `,` + result2 + `]}`,
	} {
		if w := post(t, h, body); w.Code != http.StatusOK {
			t.Fatalf("step %d: %d %s", i+2, w.Code, w.Body.String())
		}
	}

	if retriever.calls != 1 {
		t.Errorf("retrieval ran %d times, want once per turn", retriever.calls)
	}
	if hits := testutil.ToFloat64(metrics.AgentTurnCacheTotal.WithLabelValues("hit")) - hitsBefore; hits != 2 {
		t.Errorf("cache hits = %v, want 2", hits)
	}
	for i, b := range up.bodies {
		first := b["messages"].([]any)[0].(map[string]any)
		if first["role"] != "system" || first["content"] != "MEMORY-CONTEXT" {
			t.Errorf("upstream request %d doesn't start with the same injected memory: %v", i+1, first)
		}
		if len(b["tools"].([]any)) != 2 {
			t.Errorf("upstream request %d tools = %v", i+1, b["tools"])
		}
	}
	if len(capturer.episodes) != 1 {
		t.Fatalf("captured %d episodes, want exactly 1 for the whole turn", len(capturer.episodes))
	}
	ep := capturer.episodes[0]
	if ep.InputText != "Where is order A-1, and what's the weather in Oslo?" || ep.OutputText != "Your order shipped; Oslo is 5C." {
		t.Errorf("episode texts = %q / %q", ep.InputText, ep.OutputText)
	}
	if got := strings.Join(ep.ContextTags, ","); got != "agent,tool:lookup_order,tool:get_weather" {
		t.Errorf("tags = %s", got)
	}
	for _, s := range []string{ep.InputText, ep.OutputText, strings.Join(ep.ContextTags, ",")} {
		if strings.Contains(s, "SECRET") || strings.Contains(s, "order_id") || strings.Contains(s, "Oslo\\\"") {
			t.Errorf("tool output or arguments leaked into the episode: %q", s)
		}
	}
}

func TestAgentTurn_Stream_RelaysToolCallDeltasAndCapturesFinal(t *testing.T) {
	up := &scriptedUpstream{streamReply: func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		msgs := body["messages"].([]any)
		var lines []string
		if len(msgs) == 2 {
			lines = []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Oslo\"}"}}]}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			}
		} else {
			lines = []string{
				`{"choices":[{"delta":{"content":"It is "}}]}`,
				`{"choices":[{"delta":{"content":"5C."},"finish_reason":"stop"}]}`,
			}
		}
		for _, l := range lines {
			fmt.Fprintf(w, "data: %s\n\n", l)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}}
	h, retriever, capturer := newAgentHandler(t, up, provider.ProfileConfig{})

	question := `{"role":"user","content":"Weather in Oslo?"}`
	step1 := `{"model":"test","stream":true,"tools":[` + weatherTool + `],"messages":[` + question + `]}`
	w := post(t, h, step1)
	chunks := sseChunks(t, w.Body.String())
	var args, name, id, finish string
	for _, c := range chunks {
		ch := c.Choices[0]
		for _, tc := range ch.Delta.ToolCalls {
			args += tc.Function.Arguments
			if tc.Function.Name != "" {
				name = tc.Function.Name
			}
			if tc.ID != "" {
				id = tc.ID
			}
		}
		if ch.FinishReason != nil {
			finish = *ch.FinishReason
		}
	}
	if id != "call_1" || name != "get_weather" || args != `{"city":"Oslo"}` || finish != "tool_calls" {
		t.Errorf("relayed tool call: id %q name %q args %q finish %q", id, name, args, finish)
	}
	if len(capturer.episodes) != 0 {
		t.Fatalf("a tool-call step was captured: %+v", capturer.episodes)
	}

	call := `{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}}]}`
	step2 := `{"model":"test","stream":true,"tools":[` + weatherTool + `],"messages":[` + question + `,` + call + `,{"role":"tool","tool_call_id":"call_1","content":"5C"}]}`
	w = post(t, h, step2)
	if !strings.Contains(w.Body.String(), `"finish_reason":"stop"`) {
		t.Errorf("final stream = %s", w.Body.String())
	}
	if retriever.calls != 1 || len(capturer.episodes) != 1 {
		t.Fatalf("retrieval calls %d, episodes %d", retriever.calls, len(capturer.episodes))
	}
	if ep := capturer.episodes[0]; ep.OutputText != "It is 5C." || ep.Truncated || strings.Join(ep.ContextTags, ",") != "agent,tool:get_weather" {
		t.Errorf("episode = %+v", ep)
	}
}

type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

func sseChunks(t *testing.T, body string) []sseChunk {
	t.Helper()
	var out []sseChunk
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), "data: ")
		if line == "" || line == "[DONE]" || line == sc.Text() {
			continue
		}
		var c sseChunk
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatalf("bad chunk %q: %v", line, err)
		}
		out = append(out, c)
	}
	return out
}

// cancelMidStream posts body, cancels the request once the upstream has
// sent its first chunk, and returns what was captured.
func cancelMidStream(t *testing.T, body string) []Episode {
	t.Helper()
	sent := make(chan struct{})
	up := &scriptedUpstream{streamReply: func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial \"}}]}\n\n")
		w.(http.Flusher).Flush()
		close(sent)
		<-r.Context().Done()
	}}
	h, _, capturer := newAgentHandler(t, up, provider.ProfileConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.HandleChatCompletions(httptest.NewRecorder(), req)
	}()
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never sent")
	}
	time.Sleep(50 * time.Millisecond) // let the handler forward the chunk
	cancel()
	<-done
	return capturer.episodes
}

func TestAgentTurn_CancelledContinuationStoresNothing(t *testing.T) {
	question := `{"role":"user","content":"Weather?"}`
	call := `{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]}`
	body := `{"model":"test","stream":true,"tools":[` + weatherTool + `],"messages":[` + question + `,` + call + `,{"role":"tool","tool_call_id":"c1","content":"5C"}]}`
	if eps := cancelMidStream(t, body); len(eps) != 0 {
		t.Errorf("a cancelled agent turn stored %+v", eps)
	}
}

func TestAgentTurn_TruncatedPlainReplyStillStored(t *testing.T) {
	eps := cancelMidStream(t, `{"model":"test","stream":true,"messages":[{"role":"user","content":"Tell me a story"}]}`)
	if len(eps) != 1 || !eps[0].Truncated || eps[0].OutputText != "partial " || eps[0].ContextTags != nil {
		t.Errorf("plain truncated reply should be stored as before, got %+v", eps)
	}
}

func TestAgentTurn_PlainChatUnchanged(t *testing.T) {
	up := &scriptedUpstream{reply: func(map[string]any) (map[string]any, string) {
		return map[string]any{"role": "assistant", "content": "hi"}, "stop"
	}}
	h, retriever, capturer := newAgentHandler(t, up, provider.ProfileConfig{})
	missesBefore := testutil.ToFloat64(metrics.AgentTurnCacheTotal.WithLabelValues("miss"))
	for i := 0; i < 2; i++ {
		post(t, h, `{"model":"test","messages":[{"role":"user","content":"hello"}]}`)
	}
	if retriever.calls != 2 || len(capturer.episodes) != 2 || capturer.episodes[0].ContextTags != nil {
		t.Errorf("plain chat: retrieval %d, episodes %+v", retriever.calls, capturer.episodes)
	}
	if testutil.ToFloat64(metrics.AgentTurnCacheTotal.WithLabelValues("miss")) != missesBefore {
		t.Error("plain chat touched the agent-turn cache")
	}
	if _, ok := up.bodies[0]["tools"]; ok {
		t.Error("plain chat sent tools upstream")
	}
}

func TestAgentTurn_NewQuestionOrExpiryRetrievesAgain(t *testing.T) {
	up := &scriptedUpstream{reply: func(map[string]any) (map[string]any, string) {
		return toolCallMsg("c1", "get_weather", "{}"), "tool_calls"
	}}
	h, retriever, _ := newAgentHandler(t, up, provider.ProfileConfig{})
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	h.Now = func() time.Time { return now }
	ask := func(q string) {
		post(t, h, `{"model":"test","tools":[`+weatherTool+`],"messages":[{"role":"user","content":"`+q+`"}]}`)
	}
	ask("Weather in Oslo?")
	ask("Weather in Oslo?") // same turn key: reused
	ask("Weather in Rome?") // different question: new turn
	now = now.Add(agentTurnCacheTTL + time.Minute)
	ask("Weather in Oslo?") // expired
	if retriever.calls != 3 {
		t.Errorf("retrieval calls = %d, want 3", retriever.calls)
	}
}

func TestAgentTurn_Validation(t *testing.T) {
	up := &scriptedUpstream{reply: func(map[string]any) (map[string]any, string) {
		return map[string]any{"role": "assistant", "content": "x"}, "stop"
	}}
	h, _, _ := newAgentHandler(t, up, provider.ProfileConfig{})
	cases := map[string]string{
		"tool message without id": `{"model":"test","messages":[{"role":"user","content":"q"},{"role":"tool","content":"r"}]}`,
		"bad tool name":           `{"model":"test","tools":[{"type":"function","function":{"name":"has space"}}],"messages":[{"role":"user","content":"q"}]}`,
		"tool_calls on user":      `{"model":"test","messages":[{"role":"user","content":"q","tool_calls":[{"id":"x","function":{"name":"f","arguments":"{}"}}]}]}`,
		"call without id":         `{"model":"test","messages":[{"role":"user","content":"q"},{"role":"assistant","tool_calls":[{"function":{"name":"f"}}]}]}`,
		"non-function tool":       `{"model":"test","tools":[{"type":"retrieval","function":{"name":"f"}}],"messages":[{"role":"user","content":"q"}]}`,
	}
	for name, body := range cases {
		if w := post(t, h, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%s)", name, w.Code, w.Body.String())
		}
	}
	ok := `{"model":"test","messages":[{"role":"user","content":"q"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"r"}]}`
	if w := post(t, h, ok); w.Code != http.StatusOK {
		t.Errorf("valid tool conversation: %d %s", w.Code, w.Body.String())
	}

	h.DisallowTools = true
	if w := post(t, h, `{"model":"test","tools":[`+weatherTool+`],"messages":[{"role":"user","content":"q"}]}`); w.Code != http.StatusBadRequest {
		t.Errorf("DisallowTools: %d", w.Code)
	}
	if w := post(t, h, `{"model":"test","messages":[{"role":"user","content":"q"}]}`); w.Code != http.StatusOK {
		t.Errorf("DisallowTools must not affect plain chat: %d", w.Code)
	}

	noTools, _, _ := newAgentHandler(t, up, provider.ProfileConfig{NoTools: true})
	w := post(t, noTools, `{"model":"test","tools":[`+weatherTool+`],"messages":[{"role":"user","content":"q"}]}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "supports_tools") {
		t.Errorf("profile without tools: %d %s", w.Code, w.Body.String())
	}
}

func TestAgentTags(t *testing.T) {
	call := func(names ...string) provider.Message {
		m := provider.Message{Role: provider.RoleAssistant}
		for _, n := range names {
			m.ToolCalls = append(m.ToolCalls, provider.ToolCall{ID: n, Function: provider.ToolCallFunction{Name: n}})
		}
		return m
	}
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "old question"},
		call("old_tool"),
		{Role: provider.RoleUser, Content: "new question"},
		call("a", "b"),
		{Role: provider.RoleTool, ToolCallID: "a"},
		call("a", "c"),
	}
	if got := strings.Join(agentTags(msgs), ","); got != "agent,tool:a,tool:b,tool:c" {
		t.Errorf("tags = %s (only tools since the last user message, deduped)", got)
	}
	if agentTags(msgs[:3]) != nil {
		t.Error("no tool calls since the question: want nil tags")
	}
	var many []string
	for i := 0; i < 30; i++ {
		many = append(many, fmt.Sprintf("t%d", i))
	}
	if n := len(agentTags([]provider.Message{{Role: provider.RoleUser}, call(many...)})); n != 1+maxToolTags {
		t.Errorf("tag count = %d, want capped at %d tools", n, maxToolTags)
	}
	if !isContinuation(msgs) || isContinuation(msgs[:3]) {
		t.Error("isContinuation wrong")
	}
}

func TestAgentTurn_AttachmentMergedOnceAndReused(t *testing.T) {
	up := &scriptedUpstream{reply: func(body map[string]any) (map[string]any, string) {
		if len(body["messages"].([]any)) == 2 {
			return toolCallMsg("c1", "lookup_order", "{}"), "tool_calls"
		}
		return map[string]any{"role": "assistant", "content": "Done."}, "stop"
	}}
	h, _, capturer := newAgentHandler(t, up, provider.ProfileConfig{})
	att := `"attachments":[{"type":"document","filename":"notes.txt","data":"` + base64Text("ORDER-NOTES-TEXT") + `"}]`
	question := `{"role":"user","content":"Check the order in my notes"}`
	tools := `"tools":[` + orderTool + `]`
	post(t, h, `{"model":"test",`+tools+`,`+att+`,"messages":[`+question+`]}`)
	call := `{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup_order","arguments":"{}"}}]}`
	post(t, h, `{"model":"test",`+tools+`,`+att+`,"messages":[`+question+`,`+call+`,{"role":"tool","tool_call_id":"c1","content":"ok"}]}`)

	for i, b := range up.bodies {
		user := b["messages"].([]any)[1].(map[string]any)
		if !strings.Contains(user["content"].(string), "ORDER-NOTES-TEXT") {
			t.Errorf("upstream request %d lost the attachment text: %v", i+1, user["content"])
		}
	}
	if len(capturer.episodes) != 1 || !strings.Contains(capturer.episodes[0].InputText, "ORDER-NOTES-TEXT") {
		t.Errorf("episodes = %+v", capturer.episodes)
	}
}

func base64Text(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}
