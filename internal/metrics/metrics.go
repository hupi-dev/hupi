// Package metrics defines every Prometheus metric HUPI exposes at
// GET /metrics (cmd/hupi/main.go) — one place holding every metric
// definition, so internal/gateway, internal/consolidation, and cmd/hupi
// itself all record into the same, consistently-named set instead of each
// defining its own ad hoc counters.
//
// Every label set here is deliberately bounded/enum-like (route, method,
// status, gate, vendor, result, ...) — never a user or team id. That
// would blow up Prometheus's cardinality, and would also be a real
// privacy leak in a product whose whole pitch is per-tenant isolation
// (see docs/TODO.md #8 and the new /privacy page: metrics are the one
// surface that's genuinely process-global, so they get held to the same
// "no customer data" bar as everything else).
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hupi_http_requests_total",
		Help: "Total gateway HTTP requests, by route, method, and status code.",
	}, []string{"route", "method", "status"})

	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "hupi_http_request_duration_seconds",
		Help: "Gateway HTTP request duration in seconds, by route and method.",
	}, []string{"route", "method"})

	// AuthResolveTotal only ever increments when h.Auth is configured
	// (Tier 3) — Tier 1/2 has no bearer-token resolution step to measure.
	AuthResolveTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hupi_auth_resolve_total",
		Help: `Bearer token resolution outcomes: result is "ok", "invalid", or "missing".`,
	}, []string{"result"})

	// RetrievalGateTotal mirrors gateway.MemoryGate's three values exactly
	// (internal/gateway/handler.go) — the single cheapest, most direct
	// live signal of how often memory actually gets used.
	RetrievalGateTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hupi_retrieval_gate_total",
		Help: `Retrieval gate outcomes per chat turn: gate is "skipped", "partial", or "full".`,
	}, []string{"gate"})

	ProviderCallDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "hupi_provider_call_duration_seconds",
		Help: "LLM provider call latency in seconds, by provider profile name, vendor, and whether it was streamed.",
	}, []string{"provider", "vendor", "stream"})

	ProviderCallErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hupi_provider_call_errors_total",
		Help: "LLM provider call failures, by provider profile name and vendor.",
	}, []string{"provider", "vendor"})

	CaptureTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hupi_capture_total",
		Help: `Episode capture outcomes: result is "ok", "error", or "skipped" (X-Hupi-Capture: off).`,
	}, []string{"result"})

	CaptureDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "hupi_capture_duration_seconds",
		Help: "Episode capture (the write of one chat turn to durable storage) duration in seconds.",
	})

	ConsolidationRunsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hupi_consolidation_runs_total",
		Help: `Consolidation runs (one per scope per day), by outcome: "ok", "error", or "skipped_no_episodes".`,
	}, []string{"result"})

	ConsolidationDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "hupi_consolidation_duration_seconds",
		Help: "Consolidation run duration in seconds, per scope.",
	})

	// GroundingFactsTotal directly measures the "integrity"/fact-checking
	// pitch operationally — how often the independent grounding pass
	// actually rejects a claimed fact, not just that the feature exists.
	GroundingFactsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hupi_grounding_facts_total",
		Help: `Facts checked by the independent grounding pass, by outcome: grounded is "true" or "false".`,
	}, []string{"grounded"})

	// GroundingSalvageTotal distinguishes a fact forced to ungrounded
	// because the grounding model's response couldn't be cleanly matched
	// to it ("unindexed" — a real, reproduced failure mode, see
	// grounding_test.go's
	// TestLiveGroundingCheckReproducesBatchMismatchWithRealData) from a
	// fact whose verdict the response did index cleanly ("matched",
	// whether that verdict was itself true or false). Previously the
	// unindexed case was only visible by grepping logs for "mismatched
	// result count", and cost every fact in the whole batch rather than
	// just the unmatched one.
	GroundingSalvageTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hupi_grounding_salvage_total",
		Help: `Grounding verdicts by how they were resolved against the model's response: "matched" or "unindexed" (forced ungrounded).`,
	}, []string{"resolution"})

	RollupRunsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hupi_rollup_runs_total",
		Help: `Weekly/monthly/yearly rollup runs, by outcome: "ok" or "error".`,
	}, []string{"result"})

	// KeywordSearchSkippedNoTermsTotal closes a real observability gap
	// (docs/CODEBASE_SURVEY_AND_REVIEW.md finding B13): a near-empty or
	// punctuation-only query (e.g. "???") can pass stage 1 (a trailing
	// "?" alone is enough, by design — see stage1QuestionSignal's own
	// doc comment) and reach vector search, paying a real embedding
	// call, while tokenize(query) — which BM25/keyword search depends on
	// — finds zero real terms. That silent "keyword search never even
	// ran this turn" case was indistinguishable from "it ran and found
	// nothing," with nothing in the result or logs to tell them apart.
	// Scoped to specifically this cause, not keywordSearchEnabled()'s
	// own admin opt-out, which an admin already knows about from their
	// own config.
	KeywordSearchSkippedNoTermsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "hupi_keyword_search_skipped_no_terms_total",
		Help: "Retrieval turns where keyword search was silently skipped because the query tokenized to zero real search terms (e.g. punctuation-only), despite passing stage 1 and reaching vector search.",
	})

	// AttachmentIngestTotal/AttachmentIngestDuration cover file/image
	// attachment processing (internal/gateway/attachments.go) —
	// deliberately no filename/content label, matching this file's own
	// privacy/cardinality rule at the top.
	AttachmentIngestTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hupi_attachment_ingest_total",
		Help: `Attachment extraction/captioning outcomes, by kind ("document", "image") and result ("ok", "warning", "error", "rejected").`,
	}, []string{"kind", "result"})

	AttachmentIngestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "hupi_attachment_ingest_duration_seconds",
		Help: "Attachment extraction/captioning duration in seconds, by kind.",
	}, []string{"kind"})
)

// InstrumentHandler wraps h to record HTTPRequestsTotal/HTTPRequestDuration
// for every request. route is a fixed string supplied at registration
// time (e.g. "/v1/chat/completions") — deliberately never derived from
// the request URL, so a path parameter (a team id, say) can never leak
// into a label value and blow up cardinality.
func InstrumentHandler(route string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h(rec, r)
		HTTPRequestsTotal.WithLabelValues(route, r.Method, strconv.Itoa(rec.status)).Inc()
		HTTPRequestDuration.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush lets a streamed response (gateway.handleStream's SSE path) keep
// working through this wrapper. Embedding http.ResponseWriter as an
// interface field only promotes methods that interface itself declares
// — Flush isn't one of them (that's the separate http.Flusher
// interface), so without this, a real *http.response's Flush method
// exists on the concrete value underneath but is never promoted to
// *statusRecorder, and handleStream's own `w.(http.Flusher)` type
// assertion fails with "streaming not supported" on every request this
// wraps — found for real by testing streamed chat completions through
// the actual registered route (metrics.InstrumentHandler wraps
// /v1/chat/completions in cmd/hupi/main.go), not a synthetic case.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
