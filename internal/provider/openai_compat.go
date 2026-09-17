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

// OpenAICompat implements Provider for any vendor speaking the OpenAI
// chat-completions/embeddings wire format: OpenAI itself, Azure OpenAI,
// Groq, OpenRouter, local Ollama/vLLM, etc. Vendor differences live
// entirely in config (base URL, key, model name), never in code — this is
// the one adapter that covers most of "plug in any endpoint."
type OpenAICompat struct {
	name    string
	vendor  string
	model   string
	baseURL string
	apiKey  string
	client  *http.Client
}

type OpenAICompatConfig struct {
	Name    string
	Vendor  string       // e.g. "openai", "azure-openai", "groq", "ollama" — recorded on every episode
	Model   string       // upstream model id, e.g. "gpt-4.1" — what's actually sent, see Provider.Model doc
	BaseURL string       // e.g. https://api.openai.com/v1 (no trailing slash)
	APIKey  string       // empty for unauthenticated local endpoints (Ollama)
	Client  *http.Client // optional, defaults to http.DefaultClient
}

func NewOpenAICompat(cfg OpenAICompatConfig) *OpenAICompat {
	client := cfg.Client
	if client == nil {
		client = http.DefaultClient
	}
	return &OpenAICompat{
		name:    cfg.Name,
		vendor:  cfg.Vendor,
		model:   cfg.Model,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		client:  client,
	}
}

func (p *OpenAICompat) Vendor() string { return p.vendor }
func (p *OpenAICompat) Model() string  { return p.model }

// resolveModel lets a caller override the profile's configured model via
// ChatRequest/EmbedRequest.Model when it genuinely knows a real upstream
// model id — but the common case, and what the gateway handler does, is
// leave it blank and take the profile's own Model, since a client-supplied
// model string is often actually a profile *name*, not a vendor model id.
func (p *OpenAICompat) resolveModel(requested string) string {
	if requested != "" {
		return requested
	}
	return p.model
}

func (p *OpenAICompat) Name() string { return p.name }

type openAIChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
}

type openAIChatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
}

func (p *OpenAICompat) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("provider %s: encode request: %w", p.name, err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("provider %s: build request: %w", p.name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("provider %s: request failed: %w", p.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("provider %s: %s returned %d: %s", p.name, path, resp.StatusCode, string(b))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("provider %s: decode response: %w", p.name, err)
		}
	}
	return nil
}

func (p *OpenAICompat) ChatCompletion(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	var raw openAIChatResponse
	body := openAIChatRequest{
		Model:       p.resolveModel(req.Model),
		Messages:    req.Messages,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	}
	if err := p.do(ctx, http.MethodPost, "/chat/completions", body, &raw); err != nil {
		return ChatResponse{}, err
	}
	if len(raw.Choices) == 0 {
		return ChatResponse{}, fmt.Errorf("provider %s: empty choices in response", p.name)
	}
	return ChatResponse{Model: raw.Model, Message: raw.Choices[0].Message, Usage: raw.Usage}, nil
}

// StreamChatCompletion issues a streamed request and translates the
// vendor's SSE stream into StreamChunks. Callers must drain the returned
// channel — it's closed once a Done/Err chunk has been sent — or the
// response body leaks.
func (p *OpenAICompat) StreamChatCompletion(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error) {
	body := openAIChatRequest{
		Model:       p.resolveModel(req.Model),
		Messages:    req.Messages,
		Stream:      true,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("provider %s: encode request: %w", p.name, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("provider %s: build request: %w", p.name, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("provider %s: request failed: %w", p.name, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("provider %s: /chat/completions returned %d: %s", p.name, resp.StatusCode, string(b))
	}

	out := make(chan StreamChunk)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				out <- StreamChunk{Done: true}
				return
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
				Usage *Usage `json:"usage"`
			}
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				out <- StreamChunk{Err: fmt.Errorf("provider %s: decode stream chunk: %w", p.name, err)}
				return
			}
			if len(chunk.Choices) > 0 {
				out <- StreamChunk{Delta: chunk.Choices[0].Delta.Content, Usage: chunk.Usage}
			}
		}
		if err := scanner.Err(); err != nil {
			out <- StreamChunk{Err: fmt.Errorf("provider %s: reading stream: %w", p.name, err)}
		}
	}()
	return out, nil
}

type openAIEmbedRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

type openAIEmbedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage Usage `json:"usage"`
}

func (p *OpenAICompat) Embed(ctx context.Context, req EmbedRequest) (EmbedResponse, error) {
	var raw openAIEmbedResponse
	body := openAIEmbedRequest{Model: p.resolveModel(req.Model), Input: req.Input, Dimensions: req.Dimensions}
	if err := p.do(ctx, http.MethodPost, "/embeddings", body, &raw); err != nil {
		return EmbedResponse{}, err
	}
	vectors := make([][]float32, len(raw.Data))
	for i, d := range raw.Data {
		vectors[i] = d.Embedding
	}
	return EmbedResponse{Vectors: vectors, Usage: raw.Usage}, nil
}
