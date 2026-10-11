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
	noTools     bool
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
	// NoTools marks a profile that shouldn't be offered tools
	// (providers.yaml `supports_tools: false`), see SupportsTools.
	NoTools bool
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
		noTools:     cfg.NoTools,
	}
}

// SupportsTools implements ToolCapable.
func (p *Anthropic) SupportsTools() bool { return !p.noTools }

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
	Tools       []anthropicTool    `json:"tools,omitempty"`
	ToolChoice  *anthropicToolPick `json:"tool_choice,omitempty"`
}

// anthropicMessage's Content is a plain string for ordinary text turns,
// or a []anthropicBlock once tool calls or tool results are involved.
type anthropicMessage struct {
	Role    Role `json:"role"`
	Content any  `json:"content"`
}

// anthropicBlock is one request content block: text, tool_use (the
// assistant's call) or tool_result (the tool's answer, in a user turn).
type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicToolPick struct {
	Type                   string `json:"type"` // auto | any | tool | none
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

type anthropicResponse struct {
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (p *Anthropic) toAnthropicRequest(req ChatRequest, stream bool) anthropicRequest {
	system, rest := splitSystem(req.Messages)
	maxTokens := 4096
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}
	out := anthropicRequest{
		Model:       p.resolveModel(req.Model),
		System:      system,
		Messages:    toAnthropicMessages(rest),
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
		Stream:      stream,
	}
	for _, t := range req.Tools {
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out.Tools = append(out.Tools, anthropicTool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema})
	}
	if len(out.Tools) > 0 {
		out.ToolChoice = anthropicToolChoice(req.ToolChoice, req.ParallelToolCalls)
	}
	return out
}

// toAnthropicMessages maps OpenAI-shaped messages to Anthropic's: an
// assistant message with tool calls becomes text + tool_use blocks, and
// each RoleTool message becomes a tool_result block in a user turn, with
// consecutive results merged into one turn (Anthropic requires roles to
// alternate).
func toAnthropicMessages(msgs []Message) []anthropicMessage {
	var out []anthropicMessage
	for _, m := range msgs {
		switch {
		case m.Role == RoleTool:
			block := anthropicBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content}
			if n := len(out); n > 0 && out[n-1].Role == RoleUser {
				if blocks, ok := out[n-1].Content.([]anthropicBlock); ok && len(blocks) > 0 && blocks[0].Type == "tool_result" {
					out[n-1].Content = append(blocks, block)
					continue
				}
			}
			out = append(out, anthropicMessage{Role: RoleUser, Content: []anthropicBlock{block}})
		case m.Role == RoleAssistant && len(m.ToolCalls) > 0:
			var blocks []anthropicBlock
			if m.Content != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthropicBlock{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: toolInput(tc.Function.Arguments)})
			}
			out = append(out, anthropicMessage{Role: RoleAssistant, Content: blocks})
		default:
			out = append(out, anthropicMessage{Role: m.Role, Content: m.Content})
		}
	}
	return out
}

// toolInput turns OpenAI's JSON-string arguments into Anthropic's input
// object. Arguments that aren't a JSON object (empty, or malformed by the
// model) are passed as {} or wrapped, so one bad call can't make the
// whole request invalid.
func toolInput(args string) json.RawMessage {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return json.RawMessage(`{}`)
	}
	var obj map[string]any
	if json.Unmarshal([]byte(trimmed), &obj) == nil {
		return json.RawMessage(trimmed)
	}
	wrapped, _ := json.Marshal(map[string]string{"raw_arguments": args})
	return wrapped
}

// anthropicToolChoice maps OpenAI's tool_choice ("auto" | "none" |
// "required" | {"type":"function","function":{"name":...}}) and
// parallel_tool_calls to Anthropic's tool_choice.
func anthropicToolChoice(choice json.RawMessage, parallel *bool) *anthropicToolPick {
	pick := &anthropicToolPick{Type: "auto"}
	var s string
	var named struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	switch {
	case len(choice) == 0:
	case json.Unmarshal(choice, &s) == nil:
		switch s {
		case "none":
			pick.Type = "none"
		case "required":
			pick.Type = "any"
		}
	case json.Unmarshal(choice, &named) == nil && named.Function.Name != "":
		pick.Type, pick.Name = "tool", named.Function.Name
	}
	if parallel != nil && !*parallel && pick.Type != "none" {
		pick.DisableParallelToolUse = true
	}
	return pick
}

// openAIFinishReason maps Anthropic's stop_reason to OpenAI's vocabulary.
func openAIFinishReason(stop string) string {
	switch stop {
	case "tool_use":
		return FinishToolCalls
	case "max_tokens":
		return FinishLength
	case "end_turn", "stop_sequence":
		return FinishStop
	default:
		return stop
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
	var calls []ToolCall
	for _, block := range raw.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			args := string(block.Input)
			if args == "" {
				args = "{}"
			}
			calls = append(calls, ToolCall{ID: block.ID, Type: "function", Function: ToolCallFunction{Name: block.Name, Arguments: args}})
		}
	}
	return ChatResponse{
		Model:   raw.Model,
		Message: Message{Role: RoleAssistant, Content: text.String(), ToolCalls: calls},
		Usage: Usage{
			PromptTokens:     raw.Usage.InputTokens,
			CompletionTokens: raw.Usage.OutputTokens,
			TotalTokens:      raw.Usage.InputTokens + raw.Usage.OutputTokens,
		},
		FinishReason: openAIFinishReason(raw.StopReason),
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
		// Anthropic numbers content blocks (text and tool_use mixed);
		// OpenAI numbers tool calls only. toolIndex maps one to the other.
		toolIndex := map[int]int{}
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				switch event {
				case "content_block_start":
					var start struct {
						Index        int `json:"index"`
						ContentBlock struct {
							Type string `json:"type"`
							ID   string `json:"id"`
							Name string `json:"name"`
						} `json:"content_block"`
					}
					if err := json.Unmarshal([]byte(payload), &start); err != nil {
						send(StreamChunk{Err: fmt.Errorf("provider %s: decode block start: %w", p.name, err)})
						return
					}
					if start.ContentBlock.Type == "tool_use" {
						i := len(toolIndex)
						toolIndex[start.Index] = i
						if !send(StreamChunk{ToolCalls: []ToolCallDelta{{Index: i, ID: start.ContentBlock.ID, Name: start.ContentBlock.Name}}}) {
							return
						}
					}
				case "content_block_delta":
					var delta struct {
						Index int `json:"index"`
						Delta struct {
							Type        string `json:"type"`
							Text        string `json:"text"`
							PartialJSON string `json:"partial_json"`
						} `json:"delta"`
					}
					if err := json.Unmarshal([]byte(payload), &delta); err != nil {
						send(StreamChunk{Err: fmt.Errorf("provider %s: decode delta: %w", p.name, err)})
						return
					}
					var sc StreamChunk
					switch delta.Delta.Type {
					case "input_json_delta":
						i, ok := toolIndex[delta.Index]
						if !ok || delta.Delta.PartialJSON == "" {
							continue
						}
						sc.ToolCalls = []ToolCallDelta{{Index: i, Arguments: delta.Delta.PartialJSON}}
					case "text_delta", "":
						if delta.Delta.Text == "" {
							continue
						}
						sc.Delta = delta.Delta.Text
					default:
						continue // thinking/signature deltas etc. carry no answer text
					}
					if !send(sc) {
						return
					}
				case "message_delta":
					var md struct {
						Delta struct {
							StopReason string `json:"stop_reason"`
						} `json:"delta"`
						Usage struct {
							OutputTokens int `json:"output_tokens"`
						} `json:"usage"`
					}
					_ = json.Unmarshal([]byte(payload), &md)
					if md.Delta.StopReason != "" {
						if !send(StreamChunk{FinishReason: openAIFinishReason(md.Delta.StopReason)}) {
							return
						}
					}
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
