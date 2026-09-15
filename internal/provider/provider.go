// Package provider defines the one seam every LLM vendor integration goes
// through. The gateway, and the consolidation engine, both talk to
// Providers only — never to a specific vendor's SDK directly — so "plug in
// any OpenAI-compatible endpoint" is a config change (see registry.go),
// and a genuinely different wire format (Anthropic's native API) is one
// bounded adapter, not a change to callers.
package provider

import "context"

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ChatResponse struct {
	Model   string  `json:"model"`
	Message Message `json:"message"`
	Usage   Usage   `json:"usage"`
}

// StreamChunk is one delta of a streamed chat completion. The gateway both
// forwards Delta to the client as it arrives and appends it to a buffer for
// capture (see ARCHITECTURE.md § Capture) — a stream that ends with Err set
// or without a final chunk carrying Done=true should still be captured,
// with the episode's `truncated` field set, not discarded.
type StreamChunk struct {
	Delta string
	Done  bool
	Usage *Usage // populated on the final chunk, if the provider reports it
	Err   error
}

type EmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type EmbedResponse struct {
	Vectors [][]float32
	Usage   Usage
}

// Provider is implemented once per vendor wire format, not once per vendor:
// OpenAI, Azure OpenAI, Groq, OpenRouter, and local Ollama/vLLM all share
// the same OpenAI-shaped schema and are served by a single implementation
// (see openai_compat.go) configured with different base URLs. Anthropic's
// native shape gets its own implementation (see anthropic.go). Embedding
// support is optional — a chat-only provider returns ErrEmbedNotSupported.
type Provider interface {
	// Name identifies this configured provider instance for logging and
	// for the manifest's consolidation_provider_history entries — e.g.
	// "work-openai", not the vendor name, since one vendor can be
	// configured multiple times under different profiles.
	Name() string

	// Vendor and Model are the values captured onto every episode's
	// provider_vendor/provider_model columns (schema/0001_init.sql) — kept
	// distinct from Name because "work-openai" (a profile you might
	// rename) isn't the same fact as "openai" + "gpt-4.1" (what actually
	// produced the text). Model is also what's sent upstream: callers pass
	// ChatRequest.Model as a hint for logging only, adapters use the
	// profile's own configured Model for the actual request, since the
	// caller's request may have used a profile name, not a real vendor
	// model id, to select this Provider in the first place.
	Vendor() string
	Model() string

	ChatCompletion(ctx context.Context, req ChatRequest) (ChatResponse, error)

	// StreamChatCompletion returns a channel of deltas. The channel is
	// closed after a chunk with Done=true or Err set; callers must drain
	// it to avoid leaking the underlying HTTP response body.
	StreamChatCompletion(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error)

	Embed(ctx context.Context, req EmbedRequest) (EmbedResponse, error)
}
