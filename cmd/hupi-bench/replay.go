package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"time"

	"hupi/internal/gateway"
	"hupi/internal/identity"
)

// staticAuth is the harness's own minimal gateway.Authenticator: the
// bearer token IS the desired user ID, verbatim. Real deployments need a
// real API-key lookup (internal/auth.TeamStore); this harness fully
// controls both what token it sends and how it's resolved, so there's
// nothing to look up — this is not a shortcut around real auth, it's
// the harness intentionally not needing Tier-3 API-key machinery at all
// (see the plan's own note on internal/selfcheck/internal/demo already
// establishing this "isolated scope, no real API keys" pattern).
type staticAuth struct{}

func (staticAuth) Resolve(ctx context.Context, apiKey string) (identity.Identity, error) {
	if apiKey == "" {
		return identity.Identity{}, fmt.Errorf("bench: empty bearer token")
	}
	return identity.Identity{UserID: apiKey}, nil
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

// sendChatTurn drives handler.HandleChatCompletions exactly the way a
// real client would — a real HTTP request/response through the real
// handler, just without an actual listening socket (httptest.NewRecorder
// instead of a real network round trip). handler.Now must already be set
// to the desired fabricated timestamp by the caller before this runs.
// systemPrompt is optional (pass "" for none) — the handler prepends its
// own retrieved-memory system message in front of whatever's sent here
// (internal/gateway/handler.go's own "step 3: context injection"), so
// this one just rides along after it, not in place of it.
func sendChatTurn(handler *gateway.Handler, userID, model, systemPrompt, content string) (string, error) {
	msgs := []wireMessage{}
	if systemPrompt != "" {
		msgs = append(msgs, wireMessage{Role: "system", Content: systemPrompt})
	}
	msgs = append(msgs, wireMessage{Role: "user", Content: content})
	reqBody, err := json.Marshal(wireChatRequest{
		Model:    model,
		Messages: msgs,
	})
	if err != nil {
		return "", fmt.Errorf("bench: marshal request: %w", err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+userID)
	rec := httptest.NewRecorder()

	handler.HandleChatCompletions(rec, req)

	if rec.Code != 200 {
		return "", fmt.Errorf("bench: gateway returned %d: %s", rec.Code, rec.Body.String())
	}
	var resp wireChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return "", fmt.Errorf("bench: parse gateway response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("bench: gateway response had no choices: %s", rec.Body.String())
	}
	return resp.Choices[0].Message.Content, nil
}

// formatSession renders a LoCoMo/LongMemEval session's turns as one
// user-role message — "Speaker: text" per line. LoCoMo sessions are a
// two-human dialogue, not a human-and-AI chat, so there's no natural
// user/assistant split to preserve; feeding the whole session transcript
// as a single user turn is the simplest faithful stand-in — what matters
// for this harness is that the actual session content (who said what)
// lands in an episode HUPI can later retrieve and consolidate, not that
// the turn-by-turn exchange itself is realistic AI-chat shape.
func formatSession(sess session) string {
	var b strings.Builder
	for _, t := range sess.turns {
		fmt.Fprintf(&b, "%s: %s\n", t.speaker, t.text)
	}
	return strings.TrimSpace(b.String())
}

// replayConversation replays every session of conv through handler,
// backdating handler.Now to each session's real recorded date/time so
// episodes land with correct historical timestamps (see the plan's
// architecture note on gateway.Handler.Now as a test seam). Returns the
// set of distinct calendar dates touched, for the caller to run
// consolidation against afterward.
func replayConversation(handler *gateway.Handler, userID, answerModel string, conv benchConversation) (map[string]time.Time, error) {
	dates := map[string]time.Time{}
	for i, sess := range conv.sessions {
		handler.Now = func() time.Time { return sess.date }
		content := formatSession(sess)
		if content == "" {
			continue
		}
		if _, err := sendChatTurn(handler, userID, answerModel, "", content); err != nil {
			return nil, fmt.Errorf("bench: replay session %d of %s: %w", i+1, conv.id, err)
		}
		day := sess.date.Truncate(24 * time.Hour)
		dates[day.Format("2006-01-02")] = day
	}
	return dates, nil
}

// qaConcisenessPrompt is sent only during the QA phase, never during
// session replay: LoCoMo/LongMemEval's own expected answers are short
// phrases ("7 May 2023", "2022"), and their official scoring is literal
// word-overlap F1, not an LLM judge. A real, measured effect: HUPI's
// default hedging, multi-sentence answer style ("Unfortunately, the
// provided text snippet does not specify...") scores near-zero F1
// against a two-word expected answer even when the underlying retrieved
// content was in the right area — this doesn't fix genuine recall
// misses, but it stops good-content answers being scored as if they
// were wrong purely on phrasing.
const qaConcisenessPrompt = `Answer the following question as concisely as possible — a short phrase or a few words, not a full sentence or explanation. If you don't know, say so in as few words as possible.`

// runBaselineConversation is the no-memory control: no session replay, no
// consolidation, so this scope's real Retrieve call has nothing to find —
// instead every session's raw transcript is concatenated and sent as
// context alongside each question, in the same single request. This
// answers "how well does the underlying answer model do with the whole
// conversation dumped in its context window," which is the real baseline
// HUPI's own memory needs to beat, not just a number in isolation (see
// the plan's own "no-memory baseline" design note).
//
// This reuses sendChatTurn/handler exactly like the real-memory path, so
// the only difference between the two is what's actually in scope's
// memory when the question is asked — not a different code path to the
// answer model.
func runBaselineConversation(handler *gateway.Handler, userID, answerModel string, conv benchConversation) ([]string, error) {
	var transcript strings.Builder
	for _, sess := range conv.sessions {
		content := formatSession(sess)
		if content == "" {
			continue
		}
		fmt.Fprintf(&transcript, "=== Session on %s ===\n%s\n\n", sess.date.Format("2 January, 2006"), content)
	}
	fullTranscript := strings.TrimSpace(transcript.String())

	handler.Now = func() time.Time { return time.Now() }
	answers := make([]string, len(conv.qa))
	for i, qa := range conv.qa {
		content := fmt.Sprintf("%s\n\nQuestion: %s", fullTranscript, qa.question)
		answer, err := sendChatTurn(handler, userID, answerModel, qaConcisenessPrompt, content)
		if err != nil {
			return nil, fmt.Errorf("bench: baseline answer question %d of %s: %w", i, conv.id, err)
		}
		answers[i] = answer
	}
	return answers, nil
}

// answerQuestions sends every QA question in conv through handler as a
// new chat turn, at queryTime (should be after every session's own
// date, and after consolidation has run for all of them) — this
// exercises the real retrieval+generation+capture path exactly like any
// other request, not a special "QA mode." Returns one prediction per
// question, in the same order as conv.qa.
func answerQuestions(handler *gateway.Handler, userID, answerModel string, conv benchConversation, defaultQueryTime time.Time) ([]string, error) {
	answers := make([]string, len(conv.qa))
	for i, qa := range conv.qa {
		qt := defaultQueryTime
		if !qa.queryTime.IsZero() {
			qt = qa.queryTime
		}
		handler.Now = func() time.Time { return qt }
		answer, err := sendChatTurn(handler, userID, answerModel, qaConcisenessPrompt, qa.question)
		if err != nil {
			return nil, fmt.Errorf("bench: answer question %d of %s: %w", i, conv.id, err)
		}
		answers[i] = answer
	}
	return answers, nil
}
