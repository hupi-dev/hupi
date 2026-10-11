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
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"hupi/internal/identity"
	"hupi/internal/metrics"
	"hupi/internal/provider"
)

// defaultMaxRequestBodyBytes covers a handful of attachments at
// maxAttachmentBytes (8 MiB each) plus base64's ~33% size inflation and
// JSON overhead — same "informed minority override" pattern as
// internal/store/retrieve.go's contextCharBudget.
const defaultMaxRequestBodyBytes = 20 << 20 // 20 MiB

func maxRequestBodyBytes() int64 {
	if v := os.Getenv("HUPI_MAX_REQUEST_BODY_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxRequestBodyBytes
}

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
	// Citations mirrors Refs one-for-one with a human-readable snippet
	// per reference — see docs/ANSWER_CITATIONS_PLAN.md. This is
	// retrieval-level ("what was available"), not generation-level
	// ("what the answer actually relied on") — that distinction is the
	// plan doc's own Phase 1 vs Phase 2 split.
	Citations []Citation
	// NeedsAggregationPass mirrors internal/store's own cheap
	// looksLikeOrderingRequest query-shape check (already computed
	// during retrieval to widen search, docs/CONSOLIDATION_COMPLETENESS_PLAN.md
	// Phase D item 2) — surfaced here so Handler can gate the answer-time
	// aggregation reasoning pass (Gap 4 mechanism 2) without re-detecting
	// query shape itself.
	NeedsAggregationPass bool
}

// Citation pairs a Ref with the exact text that was injected into
// context for it, so a caller can see not just *that* a summary/entity/
// episode fed an answer but *what it said*, without re-fetching and
// re-decrypting the underlying record.
type Citation struct {
	Ref     identity.Ref `json:"ref"`
	Snippet string       `json:"snippet"`
	// Used is nil unless X-Hupi-Explain: deep ran attributionCheck
	// (attribution.go, Phase 2 of docs/ANSWER_CITATIONS_PLAN.md) — the
	// retrieval-level Citation above only shows what was *available*;
	// this is the generation-level verdict on what the answer actually
	// *relied on*. Left nil (omitted) rather than false when not
	// checked, so a caller can't mistake "not checked" for "checked and
	// found unused."
	Used *bool `json:"used,omitempty"`
	// ParentSummaryID is memories.summary_id, carried onto a
	// RefKindMemory citation so a caller/UI can group fact-level
	// citations under their parent summary without a second lookup —
	// docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md's citation model. Empty
	// for a citation with no owning summary (an attribute, or a
	// RefKindEpisode/RefKindSummary citation).
	ParentSummaryID string `json:"parent_summary_id,omitempty"`
	// IsInference is set for a RefKindMemory key-fact citation (a
	// pointer to the real, known memories.is_inference value, never left
	// nil — that column is a definitive true/false for every fact, not
	// "not yet evaluated") — Phase 4 of
	// docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md. Left nil for an entity-
	// attribute citation: nothing sets is_inference=true for an
	// attribute today (Phase 4's extraction only ever produces inferred
	// key facts, not attributes), so there's no real answer to report
	// there yet.
	IsInference *bool `json:"is_inference,omitempty"`
	// Relations is populated only when the cited memory has an
	// updates/extends/derives link worth surfacing (Phase 3,
	// memory_relations) — always empty until that phase ships, same
	// forward-reservation reasoning as IsInference above.
	Relations []RelationRef `json:"relations,omitempty"`
}

// RelationRef names one memory_relations edge from the cited memory to
// another — Phase 3 of docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md. Defined
// now, alongside the rest of the Phase 0 citation model, so Citation's
// JSON shape is already stable by the time Phase 3 starts populating it.
type RelationRef struct {
	Type string       `json:"type"` // "updates" | "extends" | "derives"
	Ref  identity.Ref `json:"ref"`
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
	// now is the request's own current-time reference — h.now() for a
	// real request, so a test seam that fabricates historical timestamps
	// (benchmarks, hupi-demo) also gets a coherent "now" for retrieval to
	// resolve a query's own relative time reference against ("last
	// month"), not the real wall clock —
	// docs/CONSOLIDATION_COMPLETENESS_PLAN.md Phase E.
	Retrieve(ctx context.Context, actingUser, workspace identity.Scope, messages []provider.Message, now time.Time) (RetrievalResult, error)
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

// ErrFeedbackEpisodeNotFound is returned by Capturer.Capture for a
// feedback episode (review finding B17) whose RefersTo doesn't name a
// real episode in the caller's own scope — the feedback endpoint used
// to build and persist a feedback row pointing at any client-supplied
// episode id with no verification it belonged to the caller at all,
// a real (if low-exploitability — ids are high-entropy and unguessable,
// and nothing reads content back through this path) unvalidated
// cross-scope reference. handleFeedbackScoped checks for this specific
// sentinel to answer with 400, not the generic 500 every other Capture
// failure gets — a bad episode_id is the client's mistake, not an
// infrastructure failure.
var ErrFeedbackEpisodeNotFound = errors.New("gateway: episode_id does not refer to an episode in this scope")

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
	// ContextTags labels where an episode came from (e.g. "mcp", "remember"); stored in episodes.context_tags.
	ContextTags []string

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

// ResolveTeamScope is nil in the OSS build — set by team.go's init() so routes outside this package (cmd/hupi's /mcp) can reuse the Tier-3 membership check.
var ResolveTeamScope func(h *Handler, r *http.Request) (actingUser, workspace identity.Scope, status int, err error)

// Handler serves /v1/chat/completions. Mount it with, e.g.:
//
//	mux.HandleFunc("/v1/chat/completions", handler.HandleChatCompletions)
type Handler struct {
	Registry  *provider.Registry
	Retriever Retriever
	Capturer  Capturer
	Auth      Authenticator    // nil = Tier 1/2 mode, no auth required
	Now       func() time.Time // seam for tests; nil uses time.Now
	NewID     func() string    // seam for tests; nil uses NewEpisodeID
	Logger    *slog.Logger     // nil uses slog.Default()
	// DisallowTools refuses tool calling with a 400 — set by the public
	// demo (cmd/hupi-demo), which shouldn't relay arbitrary tool traffic
	// and counts every request against a guest's message cap.
	DisallowTools bool
	// OnRetrieve, if set, is called with the exact RetrievalResult this
	// request's own retrieval step computed (including ContextMessage —
	// what step 3 below actually injects), right before that context gets
	// used. A seam for external diagnostic tooling (see
	// docs/EVALMEM_INTEGRATION_PLAN.md's C_original/retrieve_original
	// requirement) that needs the real, exact context a request used,
	// not a second, separately-computed Retrieve() call that risks
	// silently drifting from what the request actually saw. nil by
	// default: zero behavior change for every real deployment.
	OnRetrieve func(RetrievalResult)

	agentTurnsOnce sync.Once
	agentTurns     *agentTurnCache
}

func (h *Handler) turnCache() *agentTurnCache {
	h.agentTurnsOnce.Do(func() { h.agentTurns = newAgentTurnCache() })
	return h.agentTurns
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
	return NewEpisodeID(h.now())
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
	// http.MaxBytesReader, not an unbounded decode: a latent gap for
	// plain text before attachments existed, and attachments (base64
	// file/image data) make it both more valuable to exploit and more
	// likely to be hit by accident. A decode failure specifically from
	// exceeding this maps to 413, not the generic 400 below.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes())
	var req chatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, fmt.Sprintf("request body exceeds %d byte limit", maxBytesErr.Limit), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if len(req.Messages) == 0 {
		http.Error(w, "messages must not be empty", http.StatusBadRequest)
		return
	}
	for _, m := range req.Messages {
		if !provider.Role(m.Role).Valid() {
			http.Error(w, fmt.Sprintf("invalid message role %q: must be one of %s, %s, %s, %s", m.Role, provider.RoleSystem, provider.RoleUser, provider.RoleAssistant, provider.RoleTool), http.StatusBadRequest)
			return
		}
	}

	ctx := r.Context()
	messages := toProviderMessages(req.Messages)

	// Tool calling (agentturn.go): relayed, never run here. agent marks a
	// request that's part of an agent turn; plain chat skips all of this.
	agent := usesTools(req, messages)
	if agent && h.DisallowTools {
		http.Error(w, "tool calling is not available on this endpoint", http.StatusBadRequest)
		return
	}
	if err := validateToolRequest(req); err != nil {
		http.Error(w, fmt.Sprintf("invalid tool request: %v", err), http.StatusBadRequest)
		return
	}
	// Step 1 (model name -> provider profile), resolved early so a tool
	// request to a profile without tool calling fails before any work.
	target := h.resolveProvider(req.Model)
	if agent && !provider.SupportsTools(target) {
		http.Error(w, fmt.Sprintf("provider profile %q does not support tool calling (supports_tools: false)", target.Name()), http.StatusBadRequest)
		return
	}
	turn := agentTurnInfo{agent: agent, continuation: isContinuation(messages)}
	if agent {
		turn.tags = agentTags(messages)
	}

	// An agent turn's later requests reuse what its first request
	// computed: the same retrieval, aggregation hint and merged
	// attachments, so memory is searched once per turn and every step
	// sees the same context.
	var (
		turnKey            string
		cached             agentTurnState
		haveCached         bool
		result             = RetrievalResult{Gate: GateSkipped}
		aggregationHint    string
		attachmentWarnings []string
	)
	if agent {
		turnKey = agentTurnKey(actingUser, workspace, target.Name(), req.Messages, req.Attachments)
		cached, haveCached = h.turnCache().get(turnKey, h.now())
		if haveCached {
			metrics.AgentTurnCacheTotal.WithLabelValues("hit").Inc()
		} else {
			metrics.AgentTurnCacheTotal.WithLabelValues("miss").Inc()
		}
	}
	if haveCached {
		result, aggregationHint, attachmentWarnings = cached.result, cached.aggregationHint, cached.attachmentWarnings
		if i := lastUserIndex(messages); i >= 0 {
			messages[i].Content = cached.mergedUserContent
		}
	}

	// Request lifecycle step 2: retrieval, unless the client opted out
	// (ARCHITECTURE.md § Retrieval Engine, "Per-request opt-out").
	if !haveCached && !strings.EqualFold(r.Header.Get("X-Hupi-Memory"), "off") {
		var err error
		result, err = h.Retriever.Retrieve(ctx, actingUser, workspace, messages, h.now())
		if err != nil {
			// A broken retrieval path degrades to "no memory this turn,"
			// not a failed chat — see ARCHITECTURE.md § Resilience.
			h.log().Error("retrieval failed, continuing without memory", "error", err)
			result = RetrievalResult{Gate: GateSkipped}
		}
	}
	if !haveCached {
		metrics.RetrievalGateTotal.WithLabelValues(string(result.Gate)).Inc()
		if h.OnRetrieve != nil {
			h.OnRetrieve(result)
		}
	}

	// Separate, orthogonal opt-out from retrieval above: skips writing this
	// turn to memory at all. Added for clients whose turns are never worth
	// remembering — e.g. a ghost-text-style inline completion firing on
	// every debounced typing pause (vscode-extension's
	// inlineCompletionProvider.ts) — since capture previously ran
	// unconditionally regardless of how trivial the turn was.
	skipCapture := strings.EqualFold(r.Header.Get("X-Hupi-Capture"), "off")

	// Opt-in citations (docs/ANSWER_CITATIONS_PLAN.md): retrieval already
	// computes Citations unconditionally (cheap — no extra LLM call, see
	// RetrievalResult's own doc comment), so this header only controls
	// whether the response includes them, not whether they're computed.
	// Two levels, deliberately not one bool: "on" is retrieval-level only
	// (Phase 1, free); "deep" additionally runs attributionCheck (Phase
	// 2, one extra real LLM call) — a caller who only wants "what was
	// available" shouldn't pay for "what was actually used" by default.
	explainMode := r.Header.Get("X-Hupi-Explain")

	// Step 3: context injection.
	var systemMessages []provider.Message
	if result.ContextMessage != "" {
		systemMessages = append(systemMessages, provider.Message{Role: provider.RoleSystem, Content: result.ContextMessage})
	}

	// Attachments (file-ingestion design): merged after retrieval (so a
	// turn's retrieval query doesn't balloon with a document's full
	// text — only capture does) and before inputText is computed below,
	// so the merged content flows through buildEpisode/Capture/
	// EpisodeEmbedText/chunking with no further code changes anywhere
	// else. A validation failure here (too many attachments, oversized,
	// bad MIME, no user message present) is a 400; a per-attachment
	// extraction/captioning failure degrades to a placeholder marker
	// instead (see mergeAttachments' own doc comment).
	if len(req.Attachments) > 0 && !haveCached {
		var err error
		messages, attachmentWarnings, err = h.mergeAttachments(ctx, messages, req.Attachments)
		if err != nil {
			http.Error(w, fmt.Sprintf("invalid attachments: %v", err), http.StatusBadRequest)
			return
		}
	}

	inputText := lastUserMessage(messages)

	// Answer-time aggregation reasoning pass (Gap 4 mechanism 2,
	// docs/CONSOLIDATION_COMPLETENESS_PLAN.md): gated on retrieval's own
	// cheap query-shape check, so this costs nothing for the common
	// case. One extra real LLM call only for ordering/counting-shaped
	// questions; best-effort — a failure or an empty result here just
	// means no hint gets added, never a blocked turn. Reuses target, the
	// same provider already answering this turn — no separate "judge"
	// provider role, same reasoning as attributionCheck's own choice.
	if !haveCached && result.NeedsAggregationPass && result.ContextMessage != "" {
		aggregationHint = AggregationHint(ctx, target, inputText, result.ContextMessage, h.now())
	}
	if aggregationHint != "" {
		systemMessages = append(systemMessages, provider.Message{Role: provider.RoleSystem, Content: aggregationHint})
	}
	if agent && !haveCached {
		st := agentTurnState{result: result, aggregationHint: aggregationHint, attachmentWarnings: attachmentWarnings}
		if i := lastUserIndex(messages); i >= 0 {
			st.mergedUserContent = messages[i].Content
		}
		h.turnCache().put(turnKey, st, h.now())
	}

	augmented := append(systemMessages, messages...)

	if req.Stream {
		h.handleStream(w, ctx, workspace, req, augmented, target, result, inputText, actingUser.Owner, skipCapture, explainMode, attachmentWarnings, turn)
		return
	}
	h.handleNonStream(w, ctx, workspace, req, augmented, target, result, inputText, actingUser.Owner, skipCapture, explainMode, attachmentWarnings, turn)
}

// agentTurnInfo is what handleStream/handleNonStream need to know about
// an agent turn (agentturn.go).
type agentTurnInfo struct {
	agent        bool     // tools offered, or this continues an agent turn
	continuation bool     // tool calls/results already follow the user's question
	tags         []string // context_tags for the captured turn
}

// chatRequestFor builds the upstream request. Model is left blank on
// purpose: req.Model was used only to pick a provider profile
// (resolveProvider) and may be a profile name like "personal-claude"
// rather than a real vendor model id — the adapter falls back to its own
// configured Model (see Provider.Model's doc comment).
func chatRequestFor(req chatCompletionRequest, augmented []provider.Message) provider.ChatRequest {
	return provider.ChatRequest{
		Messages:          augmented,
		Temperature:       req.Temperature,
		MaxTokens:         req.MaxTokens,
		Tools:             req.Tools,
		ToolChoice:        req.ToolChoice,
		ParallelToolCalls: req.ParallelToolCalls,
	}
}

// capture writes ep unless the client opted out, recording metrics. A
// capture failure is logged, not returned: losing this one turn's memory
// is preferable to losing the answer the user is waiting on
// (ARCHITECTURE.md § Capture).
func (h *Handler) capture(ctx context.Context, scope identity.Scope, ep Episode, skip bool) {
	if skip {
		metrics.CaptureTotal.WithLabelValues("skipped").Inc()
		return
	}
	captureStart := time.Now()
	err := h.Capturer.Capture(ctx, scope, ep)
	metrics.CaptureDuration.Observe(time.Since(captureStart).Seconds())
	if err != nil {
		metrics.CaptureTotal.WithLabelValues("error").Inc()
		h.log().Error("capture failed", "episode_id", ep.ID, "error", err)
		return
	}
	metrics.CaptureTotal.WithLabelValues("ok").Inc()
}

// resolveIdentity authenticates a request. With h.Auth nil (Tier 1/2
// mode), every request resolves to identity.DefaultUserID with no team
// memberships and no Authorization header required — unchanged from
// before Tier 3 existed. With h.Auth set, a valid
// `Authorization: Bearer <key>` header is required.
func (h *Handler) resolveIdentity(r *http.Request) (identity.Identity, error) {
	return ResolveIdentity(r, h.Auth, h.log())
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
		if errors.Is(err, ErrFeedbackEpisodeNotFound) {
			http.Error(w, "episode_id does not refer to an episode in this scope", http.StatusBadRequest)
			return
		}
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
	explainMode string,
	attachmentWarnings []string,
	turn agentTurnInfo,
) {
	callStart := time.Now()
	resp, err := target.ChatCompletion(ctx, chatRequestFor(req, augmented))
	if err != nil {
		metrics.ProviderCallErrorsTotal.WithLabelValues(target.Name(), target.Vendor()).Inc()
		http.Error(w, fmt.Sprintf("upstream provider error: %v", err), http.StatusBadGateway)
		return
	}
	metrics.ProviderCallDuration.WithLabelValues(target.Name(), target.Vendor(), "false").Observe(time.Since(callStart).Seconds())

	// A tool-call step isn't an answer yet: nothing is captured until the
	// agent turn's final text answer (agentturn.go).
	toolStep := len(resp.Message.ToolCalls) > 0 || resp.FinishReason == provider.FinishToolCalls
	recordAgentStep(turn, toolStep)

	id := h.newID()
	if !toolStep {
		ep := h.buildEpisode(id, target, inputText, resp.Message.Content, result, false, actor)
		ep.ContextTags = turn.tags
		h.capture(ctx, scope, ep, skipCapture)
	}

	finish := resp.FinishReason
	if finish == "" {
		finish = provider.FinishStop
	}
	out := chatCompletionResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: h.now().Unix(),
		Model:   resp.Model,
		Choices: []chatCompletionChoice{{
			Index:        0,
			Message:      chatMessage{Role: string(resp.Message.Role), Content: resp.Message.Content, ToolCalls: resp.Message.ToolCalls},
			FinishReason: finish,
		}},
		Usage: chatCompletionUsage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
		AttachmentWarnings: attachmentWarnings,
	}
	if explainMode != "" && !toolStep {
		out.Citations = h.explainCitations(ctx, target, explainMode, resp.Message.Content, result)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// explainCitations returns the turn's citations for X-Hupi-Explain, with
// "deep" also running attributionCheck. A failed attribution check
// degrades to the plain citation list (Used left nil, i.e. "not
// checked"), never a failed response — the answer is already in hand.
func (h *Handler) explainCitations(ctx context.Context, target provider.Provider, explainMode, answer string, result RetrievalResult) []Citation {
	citations := append([]Citation{}, result.Citations...)
	if explainMode == "deep" && len(citations) > 0 {
		used, attrErr := attributionCheck(ctx, target, answer, citations)
		if attrErr != nil {
			h.log().Error("attribution check failed, returning citations without Used", "error", attrErr)
		} else {
			for i := range citations {
				u := used[i]
				citations[i].Used = &u
			}
		}
	}
	return citations
}

func recordAgentStep(turn agentTurnInfo, toolStep bool) {
	if !turn.agent {
		return
	}
	outcome := "final"
	if toolStep {
		outcome = "tool_calls"
	}
	metrics.AgentStepsTotal.WithLabelValues(outcome).Inc()
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
	explainMode string,
	attachmentWarnings []string,
	turn agentTurnInfo,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	streamStart := time.Now()
	chunks, err := target.StreamChatCompletion(ctx, chatRequestFor(req, augmented))
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
	sawToolCalls := false
	finish := ""

drain:
	for {
		select {
		case <-ctx.Done():
			// Client disconnected or the request was cancelled: whatever
			// arrived so far is still captured, flagged truncated — a
			// partial memory beats none (ARCHITECTURE.md § Capture). Not
			// for an agent turn, though: see the capture rule below.
			truncated = true
			break drain
		case chunk, more := <-chunks:
			if !more {
				break drain
			}
			if chunk.Err != nil {
				metrics.ProviderCallErrorsTotal.WithLabelValues(target.Name(), target.Vendor()).Inc()
				h.log().Error("stream error from provider", "error", chunk.Err)
				truncated = true
				break drain
			}
			if chunk.FinishReason != "" {
				finish = chunk.FinishReason
			}
			if chunk.Delta != "" || len(chunk.ToolCalls) > 0 {
				buf.WriteString(chunk.Delta)
				delta := chatCompletionChunkDelta{Content: chunk.Delta}
				for _, tc := range chunk.ToolCalls {
					sawToolCalls = true
					part := chatToolCallDeltaWire{Index: tc.Index, ID: tc.ID, Function: chatToolCallFunctionPart{Name: tc.Name, Arguments: tc.Arguments}}
					if tc.ID != "" {
						part.Type = "function"
					}
					delta.ToolCalls = append(delta.ToolCalls, part)
				}
				writeSSEChunk(w, chatCompletionChunk{
					ID: id, Object: "chat.completion.chunk", Created: created, Model: target.Model(),
					Choices: []chatCompletionChunkChoice{{Index: 0, Delta: delta}},
				})
				flusher.Flush()
			}
			if chunk.Done {
				break drain
			}
		}
	}

	metrics.ProviderCallDuration.WithLabelValues(target.Name(), target.Vendor(), "true").Observe(time.Since(streamStart).Seconds())

	// Capture rules (agentturn.go): a tool-call step isn't an answer yet,
	// and an agent turn cut off mid-way stores nothing. A plain reply cut
	// off mid-stream is still captured, flagged truncated, as before.
	toolStep := sawToolCalls || finish == provider.FinishToolCalls
	if !truncated {
		recordAgentStep(turn, toolStep)
	}
	if !toolStep && !(truncated && turn.continuation) {
		ep := h.buildEpisode(id, target, inputText, buf.String(), result, truncated, actor)
		ep.ContextTags = turn.tags
		h.capture(ctx, scope, ep, skipCapture)
	}

	// The terminal [DONE] is held back until capture has been attempted:
	// ARCHITECTURE.md's "before the turn is considered done" means before
	// the client is *told* the turn is done, not merely before the content
	// finishes arriving — the content itself was already streamed above.
	if finish == "" {
		finish = provider.FinishStop
	}
	terminal := chatCompletionChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: target.Model(),
		Choices:            []chatCompletionChunkChoice{{Index: 0, Delta: chatCompletionChunkDelta{}, FinishReason: &finish}},
		AttachmentWarnings: attachmentWarnings,
	}
	if explainMode != "" && !toolStep {
		terminal.Citations = h.explainCitations(ctx, target, explainMode, buf.String(), result)
	}
	writeSSEChunk(w, terminal)
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
	ep := NewInteractionEpisode(id, h.now(), p.Vendor(), p.Model(), inputText, outputText)
	ep.Truncated = truncated
	ep.MemoryGate = result.Gate
	ep.RetrievedRefs = result.Refs
	ep.ActorUserID = actor
	return ep
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
		out[i] = provider.Message{Role: provider.Role(m.Role), Content: m.Content, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
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
