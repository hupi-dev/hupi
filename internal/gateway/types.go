package gateway

// Wire-format types for the client-facing /v1/chat/completions endpoint —
// deliberately separate from provider.ChatRequest/ChatResponse
// (internal/provider). This is the shape any OpenAI SDK sends and expects
// on the inbound side; what gets forwarded upstream, and in what shape, is
// the Provider adapter's concern, not the handler's.

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatCompletionRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
}

type chatCompletionChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatCompletionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []chatCompletionChoice `json:"choices"`
	Usage   chatCompletionUsage    `json:"usage"`
	// Citations is only populated when the request set X-Hupi-Explain: on
	// (docs/ANSWER_CITATIONS_PLAN.md) — an additive field a strict OpenAI
	// client simply never looks for, so its absence changes nothing for
	// existing callers.
	Citations []Citation `json:"hupi_citations,omitempty"`
}

type chatCompletionChunkDelta struct {
	Content string `json:"content,omitempty"`
}

type chatCompletionChunkChoice struct {
	Index        int                      `json:"index"`
	Delta        chatCompletionChunkDelta `json:"delta"`
	FinishReason *string                  `json:"finish_reason,omitempty"`
}

// feedbackRequest/feedbackResponse are the wire format for POST
// /v1/feedback — not an OpenAI-shaped endpoint at all, this one's HUPI's
// own, see gateway.HandleFeedback.
type feedbackRequest struct {
	EpisodeID string `json:"episode_id"`
	Rating    string `json:"rating"`
	Note      string `json:"note,omitempty"`
}

type feedbackResponse struct {
	ID string `json:"id"`
}

type chatCompletionChunk struct {
	ID      string                      `json:"id"`
	Object  string                      `json:"object"`
	Created int64                       `json:"created"`
	Model   string                      `json:"model"`
	Choices []chatCompletionChunkChoice `json:"choices"`
	// Citations is only set on the terminal chunk (the one carrying
	// FinishReason), and only when X-Hupi-Explain was set — same
	// additive, omitempty field as chatCompletionResponse.Citations, for
	// clients that stream instead of using the non-streamed response
	// shape (docs/ANSWER_CITATIONS_PLAN.md).
	Citations []Citation `json:"hupi_citations,omitempty"`
}
