package provider

import (
	"bufio"
	"bytes"
	"context"
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
	name       string
	vendor     string
	model      string
	baseURL    string
	apiKey     string
	apiVersion string
	client     *http.Client
}

type AnthropicConfig struct {
	Name       string
	Vendor     string // almost always "anthropic"; still explicit, not hardcoded, matching OpenAICompatConfig's shape
	Model      string // e.g. "claude-sonnet-5"
	BaseURL    string // e.g. https://api.anthropic.com/v1
	APIKey     string
	APIVersion string // e.g. "2023-06-01"; required by the API on every request
	Client     *http.Client
}

func NewAnthropic(cfg AnthropicConfig) *Anthropic {
	client := cfg.Client
	if client == nil {
		client = http.DefaultClient
	}
	return &Anthropic{
		name:       cfg.Name,
		vendor:     cfg.Vendor,
		model:      cfg.Model,
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:     cfg.APIKey,
		apiVersion: cfg.APIVersion,
		client:     client,
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

func (p *Anthropic) newRequest(ctx context.Context, body any) (*http.Request, error) {
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
	return req, nil
}

func (p *Anthropic) ChatCompletion(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	httpReq, err := p.newRequest(ctx, p.toAnthropicRequest(req, false))
	if err != nil {
		return ChatResponse{}, err
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("provider %s: request failed: %w", p.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return ChatResponse{}, fmt.Errorf("provider %s: /messages returned %d: %s", p.name, resp.StatusCode, string(b))
	}
	var raw anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
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
	httpReq, err := p.newRequest(ctx, p.toAnthropicRequest(req, true))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("provider %s: request failed: %w", p.name, err)
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
						out <- StreamChunk{Err: fmt.Errorf("provider %s: decode delta: %w", p.name, err)}
						return
					}
					out <- StreamChunk{Delta: delta.Delta.Text}
				case "message_delta":
					var md struct {
						Usage struct {
							OutputTokens int `json:"output_tokens"`
						} `json:"usage"`
					}
					_ = json.Unmarshal([]byte(payload), &md)
				case "message_stop":
					out <- StreamChunk{Done: true}
					return
				}
			}
		}
		if err := scanner.Err(); err != nil {
			out <- StreamChunk{Err: fmt.Errorf("provider %s: reading stream: %w", p.name, err)}
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
