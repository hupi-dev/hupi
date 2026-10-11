package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// captureServer records the last request body and answers with reply
// (JSON, or an SSE stream when reply starts with "event:" or "data:").
func captureServer(t *testing.T, reply string) (*httptest.Server, *map[string]any) {
	t.Helper()
	got := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		if strings.HasPrefix(reply, "event:") || strings.HasPrefix(reply, "data:") {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func toolLoopRequest() ChatRequest {
	no := false
	return ChatRequest{
		Messages: []Message{
			{Role: RoleSystem, Content: "memory context"},
			{Role: RoleUser, Content: "What's the weather in Paris and Rome?"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: "call_1", Function: ToolCallFunction{Name: "get_weather", Arguments: `{"city":"Paris"}`}},
				{ID: "call_2", Function: ToolCallFunction{Name: "get_weather", Arguments: `not json`}},
			}},
			{Role: RoleTool, ToolCallID: "call_1", Content: "18C, sunny"},
			{Role: RoleTool, ToolCallID: "call_2", Content: "22C, cloudy"},
		},
		Tools: []ToolDef{{Function: ToolFunction{
			Name: "get_weather", Description: "Current weather",
			Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		}}},
		ToolChoice:        json.RawMessage(`"required"`),
		ParallelToolCalls: &no,
	}
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMessageJSON(t *testing.T) {
	cases := []struct {
		msg  Message
		want string
	}{
		{Message{Role: RoleUser, Content: ""}, `{"role":"user","content":""}`},
		{Message{Role: RoleUser, Content: "hi"}, `{"role":"user","content":"hi"}`},
		{Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Function: ToolCallFunction{Name: "f", Arguments: "{}"}}}},
			`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}`},
		{Message{Role: RoleAssistant, Content: "checking", ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: ToolCallFunction{Name: "f", Arguments: "{}"}}}},
			`{"role":"assistant","content":"checking","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}`},
		{Message{Role: RoleTool, ToolCallID: "c1", Content: "result"}, `{"role":"tool","content":"result","tool_call_id":"c1"}`},
	}
	for _, tc := range cases {
		if got := asJSON(t, tc.msg); got != tc.want {
			t.Errorf("marshal %+v\n got  %s\n want %s", tc.msg, got, tc.want)
		}
	}
	var m Message
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":null,"tool_calls":[{"id":"x","index":0,"type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Content != "" || len(m.ToolCalls) != 1 || m.ToolCalls[0].Function.Arguments != `{"a":1}` {
		t.Errorf("unmarshal = %+v", m)
	}
}

func TestOpenAICompat_ToolCalls_NonStream(t *testing.T) {
	// Ollama-style reply: content "" (not null) and an index on each call.
	srv, got := captureServer(t, `{"model":"m","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"call_9","index":0,"type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}}]}}],"usage":{"total_tokens":5}}`)
	p := NewOpenAICompat(OpenAICompatConfig{Name: "t", Model: "m", BaseURL: srv.URL})

	resp, err := p.ChatCompletion(context.Background(), toolLoopRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != FinishToolCalls || len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	if c := resp.Message.ToolCalls[0]; c.ID != "call_9" || c.Function.Name != "get_weather" || c.Function.Arguments != `{"city":"Oslo"}` {
		t.Errorf("tool call = %+v", c)
	}

	body := *got
	if tools := body["tools"].([]any); len(tools) != 1 || tools[0].(map[string]any)["type"] != "function" {
		t.Errorf("tools sent = %v", body["tools"])
	}
	if body["tool_choice"] != "required" || body["parallel_tool_calls"] != false {
		t.Errorf("tool_choice/parallel = %v / %v", body["tool_choice"], body["parallel_tool_calls"])
	}
	msgs := body["messages"].([]any)
	asst := msgs[2].(map[string]any)
	if asst["content"] != nil || len(asst["tool_calls"].([]any)) != 2 {
		t.Errorf("assistant tool-call message sent as %v", asst)
	}
	if tool := msgs[3].(map[string]any); tool["role"] != "tool" || tool["tool_call_id"] != "call_1" {
		t.Errorf("tool message sent as %v", tool)
	}
}

func TestOpenAICompat_PlainRequestUnchanged(t *testing.T) {
	srv, got := captureServer(t, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`)
	p := NewOpenAICompat(OpenAICompatConfig{Name: "t", Model: "m", BaseURL: srv.URL})
	resp, err := p.ChatCompletion(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "hi" || resp.FinishReason != FinishStop {
		t.Errorf("resp = %+v", resp)
	}
	for _, k := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
		if _, ok := (*got)[k]; ok {
			t.Errorf("plain request sent %q", k)
		}
	}
}

func TestOpenAICompat_ToolCalls_Stream(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"ci"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"Paris\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`, ``,
	}, "\n\n")
	srv, _ := captureServer(t, sse)
	p := NewOpenAICompat(OpenAICompatConfig{Name: "t", Model: "m", BaseURL: srv.URL})
	ch, err := p.StreamChatCompletion(context.Background(), toolLoopRequest())
	if err != nil {
		t.Fatal(err)
	}
	var deltas []ToolCallDelta
	var finish string
	for c := range ch {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
		deltas = append(deltas, c.ToolCalls...)
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
	}
	calls := assemble(deltas)
	if finish != FinishToolCalls || len(calls) != 1 || calls[0].ID != "call_1" || calls[0].Function.Name != "get_weather" || calls[0].Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("finish %q, calls %+v", finish, calls)
	}
}

// assemble joins streamed fragments by index, the way a client does.
func assemble(deltas []ToolCallDelta) []ToolCall {
	var calls []ToolCall
	for _, d := range deltas {
		for len(calls) <= d.Index {
			calls = append(calls, ToolCall{Type: "function"})
		}
		c := &calls[d.Index]
		if d.ID != "" {
			c.ID = d.ID
		}
		if d.Name != "" {
			c.Function.Name = d.Name
		}
		c.Function.Arguments += d.Arguments
	}
	return calls
}

func TestAnthropic_ToolRequestTranslation(t *testing.T) {
	srv, got := captureServer(t, `{"model":"claude","stop_reason":"end_turn","content":[{"type":"text","text":"Paris 18C, Rome 22C."}],"usage":{"input_tokens":3,"output_tokens":4}}`)
	p := NewAnthropic(AnthropicConfig{Name: "a", Model: "claude", BaseURL: srv.URL, APIKey: "k", APIVersion: "2023-06-01"})
	resp, err := p.ChatCompletion(context.Background(), toolLoopRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "Paris 18C, Rome 22C." || resp.FinishReason != FinishStop {
		t.Errorf("resp = %+v", resp)
	}

	body := *got
	if body["system"] != "memory context" {
		t.Errorf("system = %v", body["system"])
	}
	tools := body["tools"].([]any)
	if tool := tools[0].(map[string]any); tool["name"] != "get_weather" || tool["input_schema"] == nil {
		t.Errorf("tools = %v", tools)
	}
	if pick := body["tool_choice"].(map[string]any); pick["type"] != "any" || pick["disable_parallel_tool_use"] != true {
		t.Errorf("tool_choice = %v", pick)
	}

	msgs := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want user, assistant(tool_use), user(tool_results merged): %v", len(msgs), msgs)
	}
	if first := msgs[0].(map[string]any); first["content"] != "What's the weather in Paris and Rome?" {
		t.Errorf("plain user turn should stay a string: %v", first)
	}
	asst := msgs[1].(map[string]any)
	blocks := asst["content"].([]any)
	use0, use1 := blocks[0].(map[string]any), blocks[1].(map[string]any)
	if asst["role"] != "assistant" || use0["type"] != "tool_use" || use0["id"] != "call_1" || !reflect.DeepEqual(use0["input"], map[string]any{"city": "Paris"}) {
		t.Errorf("assistant blocks = %v", blocks)
	}
	if !reflect.DeepEqual(use1["input"], map[string]any{"raw_arguments": "not json"}) {
		t.Errorf("malformed arguments should be wrapped, got %v", use1["input"])
	}
	results := msgs[2].(map[string]any)
	rb := results["content"].([]any)
	if results["role"] != "user" || len(rb) != 2 || rb[0].(map[string]any)["tool_use_id"] != "call_1" || rb[1].(map[string]any)["content"] != "22C, cloudy" {
		t.Errorf("tool results = %v", results)
	}
}

func TestAnthropic_ToolChoiceMapping(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		choice   string
		parallel *bool
		want     anthropicToolPick
	}{
		{"", nil, anthropicToolPick{Type: "auto"}},
		{`"auto"`, &yes, anthropicToolPick{Type: "auto"}},
		{`"none"`, &no, anthropicToolPick{Type: "none"}},
		{`"required"`, nil, anthropicToolPick{Type: "any"}},
		{`{"type":"function","function":{"name":"get_weather"}}`, &no, anthropicToolPick{Type: "tool", Name: "get_weather", DisableParallelToolUse: true}},
	}
	for _, tc := range cases {
		if got := anthropicToolChoice(json.RawMessage(tc.choice), tc.parallel); *got != tc.want {
			t.Errorf("tool_choice %s: got %+v, want %+v", tc.choice, *got, tc.want)
		}
	}
}

func TestAnthropic_ToolUseResponse(t *testing.T) {
	srv, _ := captureServer(t, `{"model":"claude","stop_reason":"tool_use","content":[{"type":"text","text":"Let me check."},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}]}`)
	p := NewAnthropic(AnthropicConfig{Name: "a", Model: "claude", BaseURL: srv.URL, APIVersion: "v"})
	resp, err := p.ChatCompletion(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "weather?"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != FinishToolCalls || resp.Message.Content != "Let me check." || len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	if c := resp.Message.ToolCalls[0]; c.ID != "toolu_1" || c.Type != "function" || c.Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("call = %+v", c)
	}
}

func TestAnthropic_ToolUseStream(t *testing.T) {
	sse := strings.Join([]string{
		"event: message_start\ndata: {\"type\":\"message_start\"}",
		"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}",
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Checking.\"}}",
		"event: content_block_start\ndata: {\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"get_weather\",\"input\":{}}}",
		"event: content_block_delta\ndata: {\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\"}}",
		"event: content_block_delta\ndata: {\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"Paris\\\"}\"}}",
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":9}}",
		"event: message_stop\ndata: {}", "",
	}, "\n\n")
	srv, _ := captureServer(t, sse)
	p := NewAnthropic(AnthropicConfig{Name: "a", Model: "claude", BaseURL: srv.URL, APIVersion: "v"})
	ch, err := p.StreamChatCompletion(context.Background(), toolLoopRequest())
	if err != nil {
		t.Fatal(err)
	}
	var text, finish string
	var deltas []ToolCallDelta
	var empty int
	for c := range ch {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
		text += c.Delta
		deltas = append(deltas, c.ToolCalls...)
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
		if !c.Done && c.Delta == "" && len(c.ToolCalls) == 0 && c.FinishReason == "" {
			empty++
		}
	}
	calls := assemble(deltas)
	if text != "Checking." || finish != FinishToolCalls || empty != 0 {
		t.Errorf("text %q, finish %q, empty chunks %d", text, finish, empty)
	}
	if len(calls) != 1 || calls[0].ID != "toolu_1" || calls[0].Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("calls = %+v (tool block index 1 must map to tool call index 0)", calls)
	}
}

func TestSupportsTools(t *testing.T) {
	reg, err := NewRegistry(Config{
		ActiveChatProvider: "a", ActiveConsolidationProvider: "a", ActiveEmbeddingProvider: "a",
		Providers: map[string]ProfileConfig{
			"a":       {Kind: KindOpenAICompat, Model: "m"},
			"local":   {Kind: KindOpenAICompat, Model: "m", NoTools: true},
			"claude":  {Kind: KindAnthropic, Model: "c"},
			"claude2": {Kind: KindAnthropic, Model: "c", NoTools: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"a": true, "local": false, "claude": true, "claude2": false} {
		p, _ := reg.Named(name)
		if got := SupportsTools(p); got != want {
			t.Errorf("SupportsTools(%s) = %v, want %v", name, got, want)
		}
	}
	if SupportsTools(nonToolProvider{}) {
		t.Error("a provider without ToolCapable must report false")
	}
}

type nonToolProvider struct{ Provider }
