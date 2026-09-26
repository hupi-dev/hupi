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
func sendChatTurn(handler *gateway.Handler, userID, model, content string) (string, error) {
	reqBody, err := json.Marshal(wireChatRequest{
		Model:    model,
		Messages: []wireMessage{{Role: "user", Content: content}},
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
		if _, err := sendChatTurn(handler, userID, answerModel, content); err != nil {
			return nil, fmt.Errorf("bench: replay session %d of %s: %w", i+1, conv.id, err)
		}
		day := sess.date.Truncate(24 * time.Hour)
		dates[day.Format("2006-01-02")] = day
	}
	return dates, nil
}

// answerQuestions sends every QA question in conv through handler as a
// new chat turn, at queryTime (should be after every session's own
// date, and after consolidation has run for all of them) — this
// exercises the real retrieval+generation+capture path exactly like any
// other request, not a special "QA mode." Returns one prediction per
// question, in the same order as conv.qa.
func answerQuestions(handler *gateway.Handler, userID, answerModel string, conv benchConversation, queryTime time.Time) ([]string, error) {
	handler.Now = func() time.Time { return queryTime }
	answers := make([]string, len(conv.qa))
	for i, qa := range conv.qa {
		answer, err := sendChatTurn(handler, userID, answerModel, qa.question)
		if err != nil {
			return nil, fmt.Errorf("bench: answer question %d of %s: %w", i, conv.id, err)
		}
		answers[i] = answer
	}
	return answers, nil
}
