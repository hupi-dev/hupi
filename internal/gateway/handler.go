// Package gateway implements the OpenAI-compatible HTTP surface described
// in ARCHITECTURE.md § Component overview: the one place a live chat turn
// touches retrieval, the provider registry, and capture. Retrieval and
// storage are deliberately behind narrow interfaces here (Retriever,
// Capturer) — their implementations aren't sketched in this package.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"hupi/internal/identity"
	"hupi/internal/metrics"
	"hupi/internal/provider"
)

// MemoryGate mirrors the outcome defined in ARCHITECTURE.md § Request
// lifecycle: skipped means retrieval never ran; partial/full both mean it
// ran and differ only in whether anything cleared the match threshold.
type MemoryGate string

const (
	GateSkipped MemoryGate = "skipped"
	GatePartial MemoryGate = "partial"
	GateFull    MemoryGate = "full"
)

// RetrievalResult is what the retrieval engine hands back: the gate
// outcome, the context to inject as a system message (empty if skipped),
// and exactly what was injected — recorded onto the episode so retrieval
// quality is auditable later (ARCHITECTURE.md § Retrieval observability).
type RetrievalResult struct {
	Gate           MemoryGate
	ContextMessage string
	Refs           []identity.Ref // scope-qualified — see identity.Ref and docs/TIER3_PLAN.md D2
}

// Retriever is implemented by the retrieval engine — not sketched in this
// package, see ARCHITECTURE.md § Request lifecycle step 2. The handler
// only needs this one seam into it.
//
// actingUser and workspace are deliberately separate (docs/TIER3_PLAN.md
// D3): actingUser is always the caller's own private scope, and anchors
// self_model — communication style stays personal no matter which
// workspace a turn happens in. workspace is what's actually searched:
// the caller's own private scope for a normal request, or a specific
// team's shared scope for a request routed through
// HandleTeamChatCompletions. For Tier 1/2 (no auth) and any private-scope
// request, the two are simply equal.
type Retriever interface {
	Retrieve(ctx context.Context, actingUser, workspace identity.Scope, messages []provider.Message) (RetrievalResult, error)
}

// FeedbackRating is the cheapest possible signal channel for "the memory
// system got this turn wrong" — ARCHITECTURE.md § Retrieval observability.
// Mirrors the check constraint on episodes.rating in schema/0001_init.sql.
type FeedbackRating string

const (
	RatingCorrect FeedbackRating = "memory_correct"
	RatingWrong   FeedbackRating = "memory_wrong"
	RatingMissing FeedbackRating = "memory_missing"
)

func (r FeedbackRating) valid() bool {
	switch r {
	case RatingCorrect, RatingWrong, RatingMissing:
		return true
	default:
		return false
	}
}

// Episode is what gets written to the `episodes` table
// (schema/0001_init.sql) once a turn completes. Type "feedback" rows use
// only ID, TS, Type, RefersTo, Rating, and Note — the rest are zero-valued,
// same table, different shape, exactly as MEMORY_FORMAT.md § Episode
// record specifies.
type Episode struct {
	ID             string
	TS             time.Time
	Type           string
	ProviderVendor string
	ProviderModel  string
	InputText      string
	OutputText     string
	Importance     float64
	Hash           string
	Truncated      bool
	MemoryGate     MemoryGate
	RetrievedRefs  []identity.Ref

	// ActorUserID is the caller's own private-scope owner — always equal
	// to the capture Scope's owner for a private-scope request, but
	// distinct from it (and worth recording separately) for a team
	// request: Scope is the team's shared workspace, ActorUserID is which
	// member actually sent the message. Used only for audit logging
	// (internal/audit) — retrieval/consolidation don't read it.
	ActorUserID string

	// Feedback-only fields (Type == "feedback").
	RefersTo string
	Rating   FeedbackRating
	Note     string
}

// Capturer persists an Episode into the given scope. Per ARCHITECTURE.md §
// Capture (durable and synchronous), this is called and awaited before the
// handler considers the turn complete — never fire-and-forget.
type Capturer interface {
	Capture(ctx context.Context, scope identity.Scope, ep Episode) error
}

// Authenticator resolves a raw API key into an identity — implemented by
// internal/auth.TeamStore. Left nil, the Handler runs in Tier 1/2 mode: no
// Authorization header required, every request resolves to
// identity.DefaultScope, exactly as before Tier 3 existed. Set it to
// require real authentication (docs/TIER3_PLAN.md Phase 3).
type Authenticator interface {
	Resolve(ctx context.Context, apiKey string) (identity.Identity, error)
}

// MountTeamRoutes is nil in the OSS build — set by team.go's init() when
// the Tier-3 extension is present. cmd/hupi calls this unconditionally
// after mounting the private routes; nil means "no team routes to
// mount," the correct Tier 1/2 state, not an error.
var MountTeamRoutes func(mux *http.ServeMux, h *Handler)

// Handler serves /v1/chat/completions. Mount it with, e.g.:
//
//	mux.HandleFunc("/v1/chat/completions", handler.HandleChatCompletions)
type Handler struct {
	Registry  *provider.Registry
	Retriever Retriever
	Capturer  Capturer
	Auth      Authenticator    // nil = Tier 1/2 mode, no auth required
	Now       func() time.Time // seam for tests; nil uses time.Now
	NewID     func() string    // seam for tests; nil uses newEpisodeID
	Logger    *slog.Logger     // nil uses slog.Default()
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Handler) newID() string {
	if h.NewID != nil {
		return h.NewID()
	}
	return newEpisodeID(h.now())
}

func (h *Handler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// HandleChatCompletions serves the private-scope route,
// POST /v1/chat/completions.
func (h *Handler) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, err := h.resolveScope(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	// Private scope: actingUser and workspace are the same value.
	h.handleChatCompletionsScoped(w, r, scope, scope)
}

func (h *Handler) handleChatCompletionsScoped(w http.ResponseWriter, r *http.Request, actingUser, workspace identity.Scope) {
	var req chatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if len(req.Messages) == 0 {
		http.Error(w, "messages must not be empty", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	messages := toProviderMessages(req.Messages)

	// Request lifecycle step 2: retrieval, unless the client opted out
	// (ARCHITECTURE.md § Retrieval Engine, "Per-request opt-out").
	result := RetrievalResult{Gate: GateSkipped}
	if r.Header.Get("X-Hupi-Memory") != "off" {
		var err error
		result, err = h.Retriever.Retrieve(ctx, actingUser, workspace, messages)
		if err != nil {
			// A broken retrieval path degrades to "no memory this turn,"
			// not a failed chat — see ARCHITECTURE.md § Resilience.
			h.log().Error("retrieval failed, continuing without memory", "error", err)
			result = RetrievalResult{Gate: GateSkipped}
		}
	}
	metrics.RetrievalGateTotal.WithLabelValues(string(result.Gate)).Inc()

	// Separate, orthogonal opt-out from retrieval above: skips writing this
	// turn to memory at all. Added for clients whose turns are never worth
	// remembering — e.g. a ghost-text-style inline completion firing on
	// every debounced typing pause (vscode-extension's
	// inlineCompletionProvider.ts) — since capture previously ran
	// unconditionally regardless of how trivial the turn was.
	skipCapture := r.Header.Get("X-Hupi-Capture") == "off"

	// Step 3: context injection.
	augmented := messages
	if result.ContextMessage != "" {
		augmented = append(
			[]provider.Message{{Role: provider.RoleSystem, Content: result.ContextMessage}},
			messages...,
		)
	}

	// Step 1 (model name -> provider profile) + step 4 (forward upstream).
	target := h.resolveProvider(req.Model)
	inputText := lastUserMessage(messages)

	if req.Stream {
		h.handleStream(w, ctx, workspace, req, augmented, target, result, inputText, actingUser.Owner, skipCapture)
		return
	}
	h.handleNonStream(w, ctx, workspace, req, augmented, target, result, inputText, actingUser.Owner, skipCapture)
}

// resolveIdentity authenticates a request. With h.Auth nil (Tier 1/2
// mode), every request resolves to identity.DefaultUserID with no team
// memberships and no Authorization header required — unchanged from
// before Tier 3 existed. With h.Auth set, a valid
// `Authorization: Bearer <key>` header is required.
func (h *Handler) resolveIdentity(r *http.Request) (identity.Identity, error) {
	if h.Auth == nil {
		return identity.Identity{UserID: identity.DefaultUserID}, nil
	}
	token := bearerToken(r)
	if token == "" {
		metrics.AuthResolveTotal.WithLabelValues("missing").Inc()
		return identity.Identity{}, errors.New("missing Authorization: Bearer <api key> header")
	}
	id, err := h.Auth.Resolve(r.Context(), token)
	if err != nil {
		// The real reason (wrong audience, expired, JWKS fetch failure,
		// unknown key hash, ...) is deliberately not returned to the
		// caller — that would let an attacker probe why a token was
		// rejected. It's still worth an operator being able to see it.
		h.log().Warn("auth: token rejected", "err", err)
		metrics.AuthResolveTotal.WithLabelValues("invalid").Inc()
		return identity.Identity{}, errors.New("invalid API key")
	}
	metrics.AuthResolveTotal.WithLabelValues("ok").Inc()
	return id, nil
}

// resolveScope determines the scope for a private-route request: always
// the caller's own private scope.
func (h *Handler) resolveScope(r *http.Request) (identity.Scope, error) {
	id, err := h.resolveIdentity(r)
	if err != nil {
		return identity.Scope{}, err
	}
	return id.PrivateScope(), nil
}

// bearerToken extracts the token from an `Authorization: Bearer <token>`
// header, or "" if the header is missing or malformed.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimPrefix(h, prefix)
}

// resolveProvider maps the client's `model` field to a configured profile
// by name (e.g. a client asking for "personal-claude" reaches that exact
// profile); anything that isn't a known profile name falls back to
// active_chat_provider, so clients that just send a bare model id like
// "gpt-4.1" still work.
func (h *Handler) resolveProvider(model string) provider.Provider {
	if p, ok := h.Registry.Named(model); ok {
		return p
	}
	return h.Registry.Chat()
}

// HandleFeedback serves POST /v1/feedback — the cheapest possible signal
// channel for "the memory system got this turn wrong," per
// ARCHITECTURE.md § Retrieval observability. Mount it alongside
// HandleChatCompletions:
//
//	mux.HandleFunc("/v1/feedback", handler.HandleFeedback)
func (h *Handler) HandleFeedback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, err := h.resolveScope(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	h.handleFeedbackScoped(w, r, scope, scope.Owner)
}

func (h *Handler) handleFeedbackScoped(w http.ResponseWriter, r *http.Request, scope identity.Scope, actor string) {
	var req feedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if req.EpisodeID == "" {
		http.Error(w, "episode_id is required", http.StatusBadRequest)
		return
	}
	rating := FeedbackRating(req.Rating)
	if !rating.valid() {
		http.Error(w, fmt.Sprintf("rating must be one of %s, %s, %s", RatingCorrect, RatingWrong, RatingMissing), http.StatusBadRequest)
		return
	}

	ep := Episode{
		ID:          h.newID(),
		TS:          h.now(),
		Type:        "feedback",
		RefersTo:    req.EpisodeID,
		Rating:      rating,
		Note:        req.Note,
		ActorUserID: actor,
	}

	// Unlike a chat turn's capture (best-effort, logged on failure —
	// there's an answer to protect the user from losing), a feedback
	// submission *is* the entire point of this request: if it doesn't
	// persist, the caller needs to know, not get a silent 200.
	if err := h.Capturer.Capture(r.Context(), scope, ep); err != nil {
		http.Error(w, fmt.Sprintf("failed to record feedback: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(feedbackResponse{ID: ep.ID})
}

func (h *Handler) handleNonStream(
	w http.ResponseWriter,
	ctx context.Context,
	scope identity.Scope,
	req chatCompletionRequest,
	augmented []provider.Message,
	target provider.Provider,
	result RetrievalResult,
	inputText string,
	actor string,
	skipCapture bool,
) {
	// Model is left blank here on purpose: req.Model was used above only
	// to pick a provider profile (resolveProvider) and may well be a
	// profile name like "personal-claude" rather than a real vendor model
	// id — the adapter falls back to its own configured Model in that
	// case (see Provider.Model's doc comment).
	callStart := time.Now()
	resp, err := target.ChatCompletion(ctx, provider.ChatRequest{
		Messages:    augmented,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	})
	if err != nil {
		metrics.ProviderCallErrorsTotal.WithLabelValues(target.Name(), target.Vendor()).Inc()
		http.Error(w, fmt.Sprintf("upstream provider error: %v", err), http.StatusBadGateway)
		return
	}
	metrics.ProviderCallDuration.WithLabelValues(target.Name(), target.Vendor(), "false").Observe(time.Since(callStart).Seconds())

	id := h.newID()
	ep := h.buildEpisode(id, target, inputText, resp.Message.Content, result, false, actor)

	// Capture is awaited, not fired-and-forgotten (ARCHITECTURE.md §
	// Capture), but a capture failure logs rather than fails the user's
	// request — losing this one turn's memory is preferable to losing the
	// answer the user is waiting on. That's a deliberate availability vs.
	// durability call, not an oversight.
	if skipCapture {
		metrics.CaptureTotal.WithLabelValues("skipped").Inc()
	} else {
		captureStart := time.Now()
		captureErr := h.Capturer.Capture(ctx, scope, ep)
		metrics.CaptureDuration.Observe(time.Since(captureStart).Seconds())
		if captureErr != nil {
			metrics.CaptureTotal.WithLabelValues("error").Inc()
			h.log().Error("capture failed", "episode_id", ep.ID, "error", captureErr)
		} else {
			metrics.CaptureTotal.WithLabelValues("ok").Inc()
		}
	}

	out := chatCompletionResponse{
		ID:      ep.ID,
		Object:  "chat.completion",
		Created: h.now().Unix(),
		Model:   resp.Model,
		Choices: []chatCompletionChoice{{
			Index:        0,
			Message:      chatMessage{Role: string(resp.Message.Role), Content: resp.Message.Content},
			FinishReason: "stop",
		}},
		Usage: chatCompletionUsage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handler) handleStream(
	w http.ResponseWriter,
	ctx context.Context,
	scope identity.Scope,
	req chatCompletionRequest,
	augmented []provider.Message,
	target provider.Provider,
	result RetrievalResult,
	inputText string,
	actor string,
	skipCapture bool,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// See the comment in handleNonStream: Model is left blank so the
	// adapter uses its own configured model rather than whatever profile
	// name the client sent as req.Model.
	streamStart := time.Now()
	chunks, err := target.StreamChatCompletion(ctx, provider.ChatRequest{
		Messages:    augmented,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	})
	if err != nil {
		metrics.ProviderCallErrorsTotal.WithLabelValues(target.Name(), target.Vendor()).Inc()
		http.Error(w, fmt.Sprintf("upstream provider error: %v", err), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	id := h.newID()
	created := h.now().Unix()

	var buf strings.Builder
	truncated := false

drain:
	for {
		select {
		case <-ctx.Done():
			// Client disconnected or the request was cancelled: whatever
			// arrived so far is still captured, flagged truncated — a
			// partial memory beats none (ARCHITECTURE.md § Capture).
			truncated = true
			break drain
		case chunk, more := <-chunks:
			if !more {
				break drain
			}
			if chunk.Err != nil {
				h.log().Error("stream error from provider", "error", chunk.Err)
				truncated = true
				break drain
			}
			if chunk.Delta != "" {
				buf.WriteString(chunk.Delta)
				writeSSEChunk(w, chatCompletionChunk{
					ID: id, Object: "chat.completion.chunk", Created: created, Model: target.Model(),
					Choices: []chatCompletionChunkChoice{{Index: 0, Delta: chatCompletionChunkDelta{Content: chunk.Delta}}},
				})
				flusher.Flush()
			}
			if chunk.Done {
				break drain
			}
		}
	}

	metrics.ProviderCallDuration.WithLabelValues(target.Name(), target.Vendor(), "true").Observe(time.Since(streamStart).Seconds())

	ep := h.buildEpisode(id, target, inputText, buf.String(), result, truncated, actor)
	if skipCapture {
		metrics.CaptureTotal.WithLabelValues("skipped").Inc()
	} else {
		captureStart := time.Now()
		captureErr := h.Capturer.Capture(ctx, scope, ep)
		metrics.CaptureDuration.Observe(time.Since(captureStart).Seconds())
		if captureErr != nil {
			metrics.CaptureTotal.WithLabelValues("error").Inc()
			h.log().Error("capture failed", "episode_id", ep.ID, "error", captureErr)
		} else {
			metrics.CaptureTotal.WithLabelValues("ok").Inc()
		}
	}

	// The terminal [DONE] is held back until capture has been attempted:
	// ARCHITECTURE.md's "before the turn is considered done" means before
	// the client is *told* the turn is done, not merely before the content
	// finishes arriving — the content itself was already streamed above.
	finish := "stop"
	writeSSEChunk(w, chatCompletionChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: target.Model(),
		Choices: []chatCompletionChunkChoice{{Index: 0, Delta: chatCompletionChunkDelta{}, FinishReason: &finish}},
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (h *Handler) buildEpisode(
	id string,
	p provider.Provider,
	inputText, outputText string,
	result RetrievalResult,
	truncated bool,
	actor string,
) Episode {
	return Episode{
		ID:             id,
		TS:             h.now(),
		Type:           "interaction",
		ProviderVendor: p.Vendor(),
		ProviderModel:  p.Model(),
		InputText:      inputText,
		OutputText:     outputText,
		Importance:     estimateImportance(inputText, outputText),
		Hash:           hashText(inputText + "\x00" + outputText),
		Truncated:      truncated,
		MemoryGate:     result.Gate,
		RetrievedRefs:  result.Refs,
		ActorUserID:    actor,
	}
}

func writeSSEChunk(w http.ResponseWriter, chunk chatCompletionChunk) {
	b, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func toProviderMessages(msgs []chatMessage) []provider.Message {
	out := make([]provider.Message, len(msgs))
	for i, m := range msgs {
		out[i] = provider.Message{Role: provider.Role(m.Role), Content: m.Content}
	}
	return out
}

func lastUserMessage(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == provider.RoleUser {
			return msgs[i].Content
		}
	}
	return ""
}
