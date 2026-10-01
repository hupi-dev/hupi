// Package provider defines the one seam every LLM vendor integration goes
// through. The gateway, and the consolidation engine, both talk to
// Providers only — never to a specific vendor's SDK directly — so "plug in
// any OpenAI-compatible endpoint" is a config change (see registry.go),
// and a genuinely different wire format (Anthropic's native API) is one
// bounded adapter, not a change to callers.
package provider

import (
	"context"
	"log/slog"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Valid reports whether r is one of the roles above — used by
// internal/gateway to reject a message with an unrecognized role at
// request-parse time (docs/CODEBASE_SURVEY_AND_REVIEW.md's C-section:
// previously an invalid role just got forwarded to the vendor API
// unchanged, surfacing as an opaque upstream error later instead of a
// clear 400 here).
func (r Role) Valid() bool {
	switch r {
	case RoleSystem, RoleUser, RoleAssistant:
		return true
	default:
		return false
	}
}

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

	// Dimensions, if non-zero, asks the provider to truncate its output
	// to this many dimensions (OpenAI's text-embedding-3-* family
	// supports this; not every model does — see EmbeddingDimensions and
	// VerifyEmbeddingDimensions). Callers normally leave this at 0 and
	// let VerifyEmbeddingDimensions's wrapper set it, rather than setting
	// it themselves per call.
	Dimensions int `json:"dimensions,omitempty"`
}

// EmbeddingDimensions is the fixed vector length every embedding column
// in the schema expects (summaries/episodes/entities.embedding, all
// `vector(1536)` — schema/0001_init.sql, schema/0012_entity_embeddings.sql).
// pgvector enforces this exactly: inserting a vector of any other length
// is a hard error, not a soft mismatch, so every Provider actually used
// for embedding must produce vectors of exactly this length — see
// VerifyEmbeddingDimensions.
const EmbeddingDimensions = 1536

type EmbedResponse struct {
	Vectors [][]float32
	Usage   Usage
}

// embedTruncateRuneBudget is a conservative hard cap well under real
// embedding models' own input-token limits (OpenAI's text-embedding-3-*
// family caps around 8191 tokens; ~4 characters/token is a reasonable
// English-text estimate, but non-English/punctuation-heavy text can run
// fewer characters per token, so this stays well under the naive
// 8191*4 estimate rather than cutting it close).
const embedTruncateRuneBudget = 20000

// TruncateForEmbedding defensively hard-truncates text before it's sent
// to an Embed call. No caller of Embed anywhere in this codebase chunks
// or caps its input today (consolidation/store.go, cluster.go,
// runner.go, reembed/reembed.go, store/retrieve.go) — fine for a chat
// turn's worth of text, but file/image attachment ingestion makes it
// realistic for a single episode's text to approach or exceed a real
// embedding model's own input-token limit, which fails the whole
// request rather than degrading gracefully. This is deliberately a flat
// safety truncation, not multi-chunk-averaging — what "the embedding of
// a whole long document" should even mean for similarity search is a
// real retrieval-quality design question of its own, out of scope here.
func TruncateForEmbedding(text string) string {
	runes := []rune(text)
	if len(runes) <= embedTruncateRuneBudget {
		return text
	}
	slog.Warn("provider: truncating text before embedding, exceeds safety budget",
		"original_runes", len(runes), "budget", embedTruncateRuneBudget)
	return string(runes[:embedTruncateRuneBudget])
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

// ImageInput is the ingest-time-only multimodal payload DescribeImage
// accepts. It deliberately never touches Message/ChatRequest — this is a
// one-shot captioning call, not a general chat capability.
type ImageInput struct {
	Data     []byte
	MIMEType string // e.g. "image/png", "image/jpeg"
}

// VisionCapable is implemented by adapters that can call a vision-capable
// model to describe an image — a separate interface from Provider, not a
// new Provider method, so adding it doesn't force every existing
// fake/mock Provider in tests to grow a stub method just to keep
// compiling. Callers type-assert: vp, ok := p.(VisionCapable).
type VisionCapable interface {
	DescribeImage(ctx context.Context, img ImageInput, instruction string) (string, error)
}
