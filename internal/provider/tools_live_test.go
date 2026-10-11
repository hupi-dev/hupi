package provider

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// Live checks that tool calling works end to end against real models, not
// just the canned shapes in tools_test.go: the model must ask for the
// tool (streamed, so the fragment handling is exercised for real), accept
// the tool result back in that vendor's own format, and answer from it.
// Skipped unless the vendor's key (or, for Ollama, a model name) is set:
//
//	OPENAI_API_KEY=...                go test -run TestLiveToolLoop ./internal/provider/
//	ANTHROPIC_API_KEY=...             go test -run TestLiveToolLoop ./internal/provider/
//	HUPI_LIVE_OLLAMA_MODEL=qwen2.5:7b go test -run TestLiveToolLoop ./internal/provider/
//
// HUPI_LIVE_TIMEOUT (a Go duration, default 10m) bounds the whole loop.
func TestLiveToolLoopOpenAI(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	runLiveToolLoop(t, NewOpenAICompat(OpenAICompatConfig{Name: "live", Vendor: "openai", Model: "gpt-4.1", BaseURL: "https://api.openai.com/v1", APIKey: key}))
}

func TestLiveToolLoopAnthropic(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	runLiveToolLoop(t, NewAnthropic(AnthropicConfig{Name: "live", Vendor: "anthropic", Model: "claude-sonnet-5", BaseURL: "https://api.anthropic.com/v1", APIKey: key, APIVersion: "2023-06-01"}))
}

func TestLiveToolLoopOllama(t *testing.T) {
	model := os.Getenv("HUPI_LIVE_OLLAMA_MODEL")
	if model == "" {
		t.Skip("HUPI_LIVE_OLLAMA_MODEL not set")
	}
	base := os.Getenv("HUPI_LIVE_OLLAMA_BASE_URL")
	if base == "" {
		base = "http://localhost:11434/v1"
	}
	runLiveToolLoop(t, NewOpenAICompat(OpenAICompatConfig{Name: "live", Vendor: "ollama", Model: model, BaseURL: base}))
}

func runLiveToolLoop(t *testing.T, p Provider) {
	t.Helper()
	timeout := 10 * time.Minute // HUPI_LIVE_TIMEOUT overrides, e.g. for a CPU-only Ollama
	if d, err := time.ParseDuration(os.Getenv("HUPI_LIVE_TIMEOUT")); err == nil && d > 0 {
		timeout = d
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req := ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "Use the lookup_order tool to find the status of order A-1042, then tell me the status."}},
		Tools: []ToolDef{{Function: ToolFunction{
			Name:        "lookup_order",
			Description: "Look up an order's status by its id.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"order_id":{"type":"string"}},"required":["order_id"]}`),
		}}},
	}

	// Step 1, streamed: the model should ask for the tool.
	ch, err := p.StreamChatCompletion(ctx, req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var deltas []ToolCallDelta
	var text, finish string
	for c := range ch {
		if c.Err != nil {
			t.Fatalf("stream chunk: %v", c.Err)
		}
		text += c.Delta
		deltas = append(deltas, c.ToolCalls...)
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
	}
	calls := assemble(deltas)
	t.Logf("step 1: finish=%q text=%q calls=%+v", finish, text, calls)
	if len(calls) != 1 || calls[0].Function.Name != "lookup_order" || !strings.Contains(calls[0].Function.Arguments, "A-1042") {
		t.Fatalf("want one lookup_order call for A-1042, got %+v", calls)
	}
	if finish != FinishToolCalls {
		t.Errorf("finish_reason = %q, want %q", finish, FinishToolCalls)
	}

	// Step 2, non-streamed: send the result back; the model should use it.
	req.Messages = append(req.Messages,
		Message{Role: RoleAssistant, Content: text, ToolCalls: calls},
		Message{Role: RoleTool, ToolCallID: calls[0].ID, Content: `{"order_id":"A-1042","status":"shipped","carrier":"DHL"}`},
	)
	resp, err := p.ChatCompletion(ctx, req)
	if err != nil {
		t.Fatalf("step 2: %v", err)
	}
	t.Logf("step 2: finish=%q answer=%q", resp.FinishReason, resp.Message.Content)
	if !strings.Contains(strings.ToLower(resp.Message.Content), "shipped") {
		t.Errorf("final answer %q doesn't use the tool result", resp.Message.Content)
	}
}
