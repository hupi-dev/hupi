// Command hupi-answer-question answers one question against an
// already-ingested scope, either through HUPI's real retrieval (native
// mode) or bypassing retrieval entirely with hand-fed oracle context
// (oracle mode) — the retrieve_original/generate_online_answer/
// generate_oracle_answer third of docs/EVALMEM_INTEGRATION_PLAN.md's
// adapter contract. hupi-ingest-turns is the other two thirds
// (ingest_conversation) plus hupi-export-memory (export_full_memory).
//
// Native mode captures the exact retrieved context (C_original) via
// gateway.Handler's OnRetrieve seam and returns it alongside the real
// answer — one real request serves both retrieve_original and
// generate_online_answer, since EvalMem's own adapter calls are
// independent methods but there's no reason to pay for two real chat
// completions when one already has everything needed.
//
// Oracle mode sets X-Hupi-Memory: off (the same per-request opt-out
// real clients use) so HUPI's own retrieval never runs, and manually
// injects exactly the given oracle context as the system message
// instead — the Generation Examiner needs to test the model in
// isolation from retrieval quality, not with HUPI's real retrieval
// additionally mixed in on top of the oracle text.
//
// Both modes set X-Hupi-Capture: off — a real, hard-won lesson from this
// same benchmark work (docs/BENCHMARKS.md §6): repeated diagnostic calls
// against the same run_ctx must not accumulate as real episodes, or a
// later call on the same scope could pick up an earlier call's own
// synthetic turn as if it were real conversation content.
//
// Usage:
//
//	hupi-answer-question -scope-owner user:evalmem-conv-30 -question "..."
//	hupi-answer-question -scope-owner user:evalmem-conv-30 -question "..." -oracle-context "..."
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"time"

	"hupi/internal/bootstrap"
	"hupi/internal/gateway"
	"hupi/internal/identity"
	"hupi/internal/store"
)

type staticAuth struct{}

func (staticAuth) Resolve(_ context.Context, apiKey string) (identity.Identity, error) {
	if apiKey == "" {
		return identity.Identity{}, fmt.Errorf("answer-question: empty bearer token")
	}
	return identity.Identity{UserID: apiKey}, nil
}

type output struct {
	Answer           string `json:"answer"`
	RetrievedContext string `json:"retrieved_context"`
	Mode             string `json:"mode"` // "native" or "oracle"
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hupi-answer-question:", err)
		os.Exit(1)
	}
}

func run() error {
	scopeOwner := flag.String("scope-owner", "", "the scope owner hupi-ingest-turns returned as run_ctx.scope_owner")
	scopeKind := flag.String("scope-kind", identity.ScopeKindPrivate, "private | shared")
	question := flag.String("question", "", "the question to answer")
	oracleContext := flag.String("oracle-context", "", "if set (even to a placeholder like NO_RELEVANT_MEMORY), answers in oracle mode: real retrieval is skipped, this text is injected directly instead")
	useOracle := flag.Bool("oracle", false, "explicit oracle-mode switch — required alongside -oracle-context so an intentionally empty oracle string isn't mistaken for \"native mode, no flag given\"")
	answerModel := flag.String("answer-model", "", "providers.yaml profile name to answer with (defaults to active_chat_provider)")
	flag.Parse()

	if *scopeOwner == "" || *question == "" {
		return fmt.Errorf("usage: hupi-answer-question -scope-owner <owner> -question \"...\" [-oracle -oracle-context \"...\"]")
	}
	scope := identity.Scope{Kind: *scopeKind, Owner: *scopeOwner}

	ctx := context.Background()
	deps, err := bootstrap.Load(ctx)
	if err != nil {
		return err
	}
	defer deps.DB.Close()
	if err := bootstrap.VerifyEmbedding(ctx, deps); err != nil {
		return err
	}

	st := store.New(deps.DB, deps.Keys, deps.Registry.Embedding())
	handler := &gateway.Handler{
		Registry:  deps.Registry,
		Retriever: st,
		Capturer:  st,
		Auth:      staticAuth{},
		Now:       func() time.Time { return time.Now() },
	}

	if *useOracle {
		answer, err := sendChatTurn(handler, scope.Owner, *answerModel, *oracleContext, qaConcisenessPrompt, *question, true)
		if err != nil {
			return err
		}
		return printOutput(output{Answer: answer, RetrievedContext: *oracleContext, Mode: "oracle"})
	}

	var retrieved string
	handler.OnRetrieve = func(r gateway.RetrievalResult) { retrieved = r.ContextMessage }
	answer, err := sendChatTurn(handler, scope.Owner, *answerModel, "", qaConcisenessPrompt, *question, false)
	if err != nil {
		return err
	}
	return printOutput(output{Answer: answer, RetrievedContext: retrieved, Mode: "native"})
}

// qaConcisenessPrompt is duplicated verbatim from cmd/hupi-bench/replay.go
// (same "small tools don't cross-import unexported types" rationale as
// this file's own sendChatTurn/wireMessage duplication) — the same real,
// hard-won fix documented there: HUPI's default answer style is verbose
// and hedging ("Based on the information I have..."), which scores badly
// against both LoCoMo's literal F1 scoring and EvalMem's own strict
// LLM-judged correctness, even when the underlying retrieved content was
// right. A first real EvalMem run against this tool without this prompt
// (docs/EVALMEM_INTEGRATION_PLAN.md step 7) confirmed the same gap here:
// every answer showed the same hedging style, and NEG/abstention accuracy
// was far below the tuned cmd/hupi-bench harness's own number on the same
// underlying model.
const qaConcisenessPrompt = `Answer the following question directly, using a short phrase rather than a full sentence or explanation — but include every specific detail the question asks for (a complete name, date, or list), not just the first word or a truncated fragment.

Always give dates as an absolute date (e.g. "7 May 2023"), never a relative term like "yesterday", "last year", or "this month".

Make your best specific attempt using anything relevant you've been told, even if you're not fully certain or the exact wording isn't stated verbatim — a specific, plausible answer inferred from related information is better than declining to answer. Only say "not mentioned" or "no information available" if there is truly nothing relevant to work with at all — not merely because the precise fact isn't stated in so many words.

Before answering, double-check WHO the retrieved information is actually about. A conversation between two people often has facts that apply to only one of them — if the question asks about person A but the fact you found belongs to person B, say so explicitly (e.g. "That's B's necklace, not A's — A's own necklace isn't mentioned") rather than answering as if it were A's.

When the answer is a list of items or a yes/no question, give ONLY the items or the yes/no verdict itself — do not add supporting context, dates, or an explanation for each item, even when that detail is available in what you were given. Having more detail available doesn't mean including it is more correct; match the specificity level the question actually asked for, not everything you know that's related.`

func printOutput(o output) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(o)
}

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type wireChatRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
}

type wireChoice struct {
	Message wireMessage `json:"message"`
}

type wireChatResponse struct {
	Choices []wireChoice `json:"choices"`
}

// sendChatTurn mirrors cmd/hupi-bench's own helper of the same name
// (duplicated deliberately — see this package's own doc comment on why
// these small tools don't cross-import cmd/hupi-bench's unexported
// types), with three additions: skipMemory sets X-Hupi-Memory: off for
// oracle mode, every call sets X-Hupi-Capture: off regardless of mode,
// and stylePrompt (qaConcisenessPrompt) is sent as its own additional
// system message after systemPrompt — so oracle mode gets both the
// oracle context and the style instruction as separate messages, not
// one string blurring the two together.
func sendChatTurn(handler *gateway.Handler, userID, model, systemPrompt, stylePrompt, content string, skipMemory bool) (string, error) {
	msgs := []wireMessage{}
	if systemPrompt != "" {
		msgs = append(msgs, wireMessage{Role: "system", Content: systemPrompt})
	}
	if stylePrompt != "" {
		msgs = append(msgs, wireMessage{Role: "system", Content: stylePrompt})
	}
	msgs = append(msgs, wireMessage{Role: "user", Content: content})
	reqBody, err := json.Marshal(wireChatRequest{Model: model, Messages: msgs})
	if err != nil {
		return "", fmt.Errorf("answer-question: marshal request: %w", err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+userID)
	req.Header.Set("X-Hupi-Capture", "off")
	if skipMemory {
		req.Header.Set("X-Hupi-Memory", "off")
	}
	rec := httptest.NewRecorder()

	handler.HandleChatCompletions(rec, req)

	if rec.Code != 200 {
		return "", fmt.Errorf("answer-question: gateway returned %d: %s", rec.Code, rec.Body.String())
	}
	var resp wireChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return "", fmt.Errorf("answer-question: parse gateway response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("answer-question: gateway response had no choices: %s", rec.Body.String())
	}
	return resp.Choices[0].Message.Content, nil
}
