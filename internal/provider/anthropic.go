package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Anthropic implements Provider for Anthropic's native Messages API, which
// doesn't share OpenAI's wire shape: the system prompt is a top-level
// field rather than a message with role "system", auth is `x-api-key` +
// `anthropic-version` rather than a bearer token, and there's no
// embeddings endpoint at all. This adapter exists so that difference stops
// at this file — every caller still only sees the Provider interface.
type Anthropic struct {
	name        string
	vendor      string
	model       string
	visionModel string
	baseURL     string
	apiKey      string
	apiVersion  string
	client      *http.Client
}

type AnthropicConfig struct {
	Name       string
	Vendor     string // almost always "anthropic"; still explicit, not hardcoded, matching OpenAICompatConfig's shape
	Model      string // e.g. "claude-sonnet-5"
	BaseURL    string // e.g. https://api.anthropic.com/v1
	APIKey     string
	APIVersion string // e.g. "2023-06-01"; required by the API on every request
	Client     *http.Client
	// VisionModel optionally names a separate upstream model for
	// DescribeImage calls — falls back to Model when empty, see
	// resolveVisionModel. In practice all current Claude models are
	// vision-capable, so this exists mainly for profiles pointed at a
	// future non-vision model.
	VisionModel string
}

func NewAnthropic(cfg AnthropicConfig) *Anthropic {
	client := cfg.Client
	if client == nil {
		client = http.DefaultClient
	}
	return &Anthropic{
		name:        cfg.Name,
		vendor:      cfg.Vendor,
		model:       cfg.Model,
		visionModel: cfg.VisionModel,
		baseURL:     strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:      cfg.APIKey,
		apiVersion:  cfg.APIVersion,
		client:      client,
	}
}

func (p *Anthropic) Name() string   { return p.name }
func (p *Anthropic) Vendor() string { return p.vendor }
func (p *Anthropic) Model() string  { return p.model }

func (p *Anthropic) resolveModel(requested string) string {
	if requested != "" {
		return requested
	}
	return p.model
}

// resolveVisionModel mirrors resolveModel's own fallback shape.
func (p *Anthropic) resolveVisionModel() string {
	if p.visionModel != "" {
		return p.visionModel
	}
	return p.model
}

// splitSystem pulls leading role="system" messages out into Anthropic's
// separate `system` field; anything after the first non-system message
// stays in the message list, matching how the gateway's context injection
// (a prepended system/developer message) is expected to arrive.
func splitSystem(messages []Message) (system string, rest []Message) {
	var sysParts []string
	i := 0
	for ; i < len(messages) && messages[i].Role == RoleSystem; i++ {
		sysParts = append(sysParts, messages[i].Content)
	}
	return strings.Join(sysParts, "\n\n"), messages[i:]
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature *float64           `json:"temperature,omitempty"`
	Stream      bool               `json:"stream"`
}

type anthropicMessage struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Model string `json:"model"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (p *Anthropic) toAnthropicRequest(req ChatRequest, stream bool) anthropicRequest {
	system, rest := splitSystem(req.Messages)
	msgs := make([]anthropicMessage, len(rest))
	for i, m := range rest {
		msgs[i] = anthropicMessage{Role: m.Role, Content: m.Content}
	}
	maxTokens := 4096
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}
	return anthropicRequest{
		Model:       p.resolveModel(req.Model),
		System:      system,
		Messages:    msgs,
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
		Stream:      stream,
	}
}

// buildRequest builds a fresh *http.Request for body — a func, not a
// pre-built *http.Request, since a retry needs a brand new request each
// attempt (an http.Request's body is a single-read io.Reader).
func (p *Anthropic) buildRequest(ctx context.Context, body any, stream bool) func() (*http.Request, error) {
	return func() (*http.Request, error) {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("provider %s: encode request: %w", p.name, err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/messages", bytes.NewReader(buf))
		if err != nil {
			return nil, fmt.Errorf("provider %s: build request: %w", p.name, err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", p.apiKey)
		req.Header.Set("anthropic-version", p.apiVersion)
		if stream {
			req.Header.Set("Accept", "text/event-stream")
		}
		return req, nil
	}
}

// anthropicVisionMessage/anthropicContentPart/anthropicImageSource are
// request-only structs for DescribeImage — the response is decoded with
// the existing anthropicResponse unchanged, since its content-block
// text-filtering already handles exactly the shape a vision reply
// returns. Deliberately separate from anthropicRequest/anthropicMessage
// rather than teaching those types about multimodal content.
type anthropicVisionMessage struct {
	Role    Role                   `json:"role"`
	Content []anthropicContentPart `json:"content"`
}

type anthropicContentPart struct {
	Type   string                `json:"type"` // "text" | "image"
	Text   string                `json:"text,omitempty"`
	Source *anthropicImageSource `json:"source,omitempty"`
}

type anthropicImageSource struct {
	Type string `json:"type"` // "base64"
	// MediaType/Data are always caller-supplied bytes re-encoded here,
	// never a client-supplied URL the server would fetch — avoids SSRF
	// entirely.
	MediaType string `json:"media_type"`
	Data      string `json:"data"` // base64, no "data:" prefix — Anthropic's own convention differs from OpenAI's
}

type anthropicVisionRequest struct {
	Model     string                   `json:"model"`
	Messages  []anthropicVisionMessage `json:"messages"`
	MaxTokens int                      `json:"max_tokens"`
}

// anthropicVisionMaxTokens bounds a caption response — generous for a
// descriptive paragraph, nowhere near a real chat completion's own
// default (anthropicRequest's toAnthropicRequest uses 4096).
const anthropicVisionMaxTokens = 1024

// DescribeImage calls the configured vision model once to produce a
// factual text description of an image — see provider.VisionCapable's
// own doc comment for why this is a narrow, separate interface rather
// than a change to Message/ChatRequest.
func (p *Anthropic) DescribeImage(ctx context.Context, img ImageInput, instruction string) (string, error) {
	body := anthropicVisionRequest{
		Model: p.resolveVisionModel(),
		Messages: []anthropicVisionMessage{{
			Role: RoleUser,
			Content: []anthropicContentPart{
				{Type: "image", Source: &anthropicImageSource{
					Type:      "base64",
					MediaType: img.MIMEType,
					Data:      base64.StdEncoding.EncodeToString(img.Data),
				}},
				{Type: "text", Text: instruction},
			},
		}},
		MaxTokens: anthropicVisionMaxTokens,
	}
	status, respBody, err := sendWithRetry(ctx, p.client, p.name, false, p.buildRequest(ctx, body, false))
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", fmt.Errorf("provider %s: /messages (vision) returned %d: %s", p.name, status, string(respBody))
	}
	var raw anthropicResponse
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return "", fmt.Errorf("provider %s: decode vision response: %w", p.name, err)
	}
	var text strings.Builder
	for _, block := range raw.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	return text.String(), nil
}

func (p *Anthropic) ChatCompletion(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	status, respBody, err := sendWithRetry(ctx, p.client, p.name, false, p.buildRequest(ctx, p.toAnthropicRequest(req, false), false))
	if err != nil {
		return ChatResponse{}, err
	}
	if status >= 400 {
		return ChatResponse{}, fmt.Errorf("provider %s: /messages returned %d: %s", p.name, status, string(respBody))
	}
	var raw anthropicResponse
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return ChatResponse{}, fmt.Errorf("provider %s: decode response: %w", p.name, err)
	}
	var text strings.Builder
	for _, block := range raw.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	return ChatResponse{
		Model:   raw.Model,
		Message: Message{Role: RoleAssistant, Content: text.String()},
		Usage: Usage{
			PromptTokens:     raw.Usage.InputTokens,
			CompletionTokens: raw.Usage.OutputTokens,
			TotalTokens:      raw.Usage.InputTokens + raw.Usage.OutputTokens,
		},
	}, nil
}

// StreamChatCompletion parses Anthropic's SSE event stream
// (content_block_delta carries text, message_stop ends the stream). Unlike
// the OpenAI-shaped adapter there's no single "usage" field per chunk;
// final usage arrives on message_delta and is attached to the closing
// chunk.
func (p *Anthropic) StreamChatCompletion(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error) {
	resp, err := connectWithRetry(ctx, p.client, p.name, false, p.buildRequest(ctx, p.toAnthropicRequest(req, true), true))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("provider %s: /messages returned %d: %s", p.name, resp.StatusCode, string(b))
	}

	out := make(chan StreamChunk)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		// send is a select on ctx.Done() alongside the channel send itself —
		// see openai_compat.go's identical helper for the real leak this
		// guards against: an unconditional `out <- chunk` blocks forever
		// once a caller stops draining (client disconnect, cancelled
		// context), leaking this goroutine and the response body its
		// deferred Close() never reaches.
		send := func(c StreamChunk) bool {
			select {
			case out <- c:
				return true
			case <-ctx.Done():
				return false
			}
		}
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var event string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				switch event {
				case "content_block_delta":
					var delta struct {
						Delta struct {
							Text string `json:"text"`
						} `json:"delta"`
					}
					if err := json.Unmarshal([]byte(payload), &delta); err != nil {
						send(StreamChunk{Err: fmt.Errorf("provider %s: decode delta: %w", p.name, err)})
						return
					}
					if !send(StreamChunk{Delta: delta.Delta.Text}) {
						return
					}
				case "message_delta":
					var md struct {
						Usage struct {
							OutputTokens int `json:"output_tokens"`
						} `json:"usage"`
					}
					_ = json.Unmarshal([]byte(payload), &md)
				case "message_stop":
					send(StreamChunk{Done: true})
					return
				}
			}
		}
		if err := scanner.Err(); err != nil {
			send(StreamChunk{Err: fmt.Errorf("provider %s: reading stream: %w", p.name, err)})
		}
	}()
	return out, nil
}

// Embed: Anthropic has no embeddings endpoint. The registry (registry.go)
// never routes the configured embedding role to an Anthropic profile, but
// this satisfies the interface for any direct/misconfigured caller.
func (p *Anthropic) Embed(ctx context.Context, req EmbedRequest) (EmbedResponse, error) {
	return EmbedResponse{}, ErrEmbedNotSupported
}
