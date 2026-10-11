package provider

import "encoding/json"

// Tool calling ("function calling") in the OpenAI chat-completions shape,
// which is also HUPI's own wire shape. The OpenAI-compatible adapter passes
// these through as-is; the Anthropic adapter translates them to and from
// tool_use/tool_result content blocks. HUPI never runs a tool itself — the
// client does (the gateway only relays calls and results, see
// internal/gateway).

// ToolDef is one tool offered to the model in ChatRequest.Tools.
type ToolDef struct {
	Type     string       `json:"type"` // always "function"
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Parameters is the tool's JSON Schema, passed through unparsed.
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

func (t ToolDef) MarshalJSON() ([]byte, error) {
	type wire ToolDef
	if t.Type == "" {
		t.Type = "function"
	}
	return json.Marshal(wire(t))
}

// ToolCall is one call the model asked for, on an assistant Message.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // always "function"
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name string `json:"name"`
	// Arguments is the model's JSON-encoded argument object, as a string
	// (OpenAI's convention).
	Arguments string `json:"arguments"`
}

func (t ToolCall) MarshalJSON() ([]byte, error) {
	type wire ToolCall
	if t.Type == "" {
		t.Type = "function"
	}
	return json.Marshal(wire(t))
}

// ToolCallDelta is one streamed fragment of a tool call. Fragments with the
// same Index belong to the same call: ID and Name arrive once (usually
// first), Arguments arrives in pieces to be concatenated in order.
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// Finish reasons, in OpenAI's vocabulary.
const (
	FinishStop      = "stop"
	FinishToolCalls = "tool_calls"
	FinishLength    = "length"
)

// ToolCapable is implemented by adapters that can do tool calling. Like
// VisionCapable it's a separate interface rather than a Provider method, so
// test fakes don't need a stub; use SupportsTools to check.
type ToolCapable interface {
	SupportsTools() bool
}

// SupportsTools reports whether p can take ChatRequest.Tools. A profile
// with `supports_tools: false` in providers.yaml (a local model without
// tool support, say) reports false, so callers can refuse a tool request
// with a clear error instead of the tools being silently ignored.
func SupportsTools(p Provider) bool {
	tc, ok := p.(ToolCapable)
	return ok && tc.SupportsTools()
}
