// Command hupi-ingest-turns replays a real conversation into HUPI's
// gateway and runs real consolidation — the ingest_conversation half of
// docs/EVALMEM_INTEGRATION_PLAN.md's adapter contract.
//
// Input is a flat JSON array of turns on stdin, matching exactly what
// EvalMem's own LoCoMo sample builder hands an adapter
// (memory_eval.dataset.locomo_builder._flatten_conversation — verified
// against their real source, not guessed): each turn carries
// session_index and session_datetime (LoCoMo's own raw date string,
// e.g. "1:56 pm on 8 May, 2023"), not a pre-parsed timestamp. Turns are
// grouped back into sessions by session_index and replayed in date
// order — the same real gateway.Handler + backdated-Now path
// cmd/hupi-bench's own replayConversation uses, deliberately not
// cross-imported from that package (cmd/hupi-bench's types are
// unexported, and this tool's own replay loop is small enough that
// duplicating it is lower-risk than refactoring already-shipped,
// already-tested benchmark code to share it).
//
// Deliberately LoCoMo-shaped for now, matching EvalMem's own current
// scope (its repository ships a LoCoMo builder only, no LongMemEval one
// yet) — see docs/EVALMEM_INTEGRATION_PLAN.md for the honest status of
// what this does and doesn't cover.
//
// Usage:
//
//	hupi-ingest-turns -sample-id conv-26 -consolidate-bin ./hupi-consolidate < turns.json
//
// Prints a run_ctx JSON object to stdout: the scope owner the Python
// adapter needs for every subsequent call (retrieve_original,
// generate_online_answer, generate_oracle_answer, export_full_memory).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"hupi/internal/auth"
	"hupi/internal/bootstrap"
	"hupi/internal/gateway"
	"hupi/internal/identity"
	"hupi/internal/store"
)

const locomoDateLayout = "3:04 pm on 2 January, 2006"

// inputTurn mirrors EvalMem's own flattened per-turn shape exactly —
// see memory_eval.dataset.locomo_builder._flatten_conversation.
type inputTurn struct {
	DiaID           string `json:"dia_id"`
	Speaker         string `json:"speaker"`
	Text            string `json:"text"`
	TurnIndex       int    `json:"turn_index"`
	SessionIndex    int    `json:"session_index"`
	SessionDatetime string `json:"session_datetime"`
}

type runCtx struct {
	SampleID       string   `json:"sample_id"`
	ScopeKind      string   `json:"scope_kind"`
	ScopeOwner     string   `json:"scope_owner"`
	SessionDates   []string `json:"session_dates"`
	ConsolidatedOK bool     `json:"consolidated_ok"`
}

type staticAuth struct{}

func (staticAuth) Resolve(_ context.Context, apiKey string) (identity.Identity, error) {
	if apiKey == "" {
		return identity.Identity{}, fmt.Errorf("ingest-turns: empty bearer token")
	}
	return identity.Identity{UserID: apiKey}, nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("hupi-ingest-turns exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	sampleID := flag.String("sample-id", "", "the benchmark sample_id this conversation belongs to — used to derive the scope owner")
	answerModel := flag.String("answer-model", "", "providers.yaml profile name to replay with (defaults to active_chat_provider)")
	consolidateBin := flag.String("consolidate-bin", "./hupi-consolidate", "path to the real hupi-consolidate binary")
	skipConsolidate := flag.Bool("skip-consolidate", false, "replay only, skip running real consolidation (faster smoke tests; the Encoding Examiner needs consolidation to have run, so don't skip this for a real evaluation)")
	flag.Parse()

	if *sampleID == "" {
		return fmt.Errorf("ingest-turns: -sample-id is required")
	}

	var turns []inputTurn
	if err := json.NewDecoder(os.Stdin).Decode(&turns); err != nil {
		return fmt.Errorf("ingest-turns: parse turns from stdin: %w", err)
	}
	if len(turns) == 0 {
		return fmt.Errorf("ingest-turns: no turns given")
	}

	sessions, err := groupIntoSessions(turns)
	if err != nil {
		return err
	}

	ctx := context.Background()
	deps, err := bootstrap.Load(ctx)
	if err != nil {
		return err
	}
	defer deps.DB.Close()
	if err := bootstrap.VerifyEmbedding(ctx, deps); err != nil {
		return err
	}

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: fmt.Sprintf("user:evalmem-%s", *sampleID)}
	authStore := auth.New(deps.DB, deps.Keys)
	if err := authStore.CreateUser(ctx, scope.Owner, ""); err != nil {
		return fmt.Errorf("ingest-turns: provision scope: %w", err)
	}

	st := store.New(deps.DB, deps.Keys, deps.Registry.Embedding())
	handler := &gateway.Handler{
		Registry:  deps.Registry,
		Retriever: st,
		Capturer:  st,
		Auth:      staticAuth{},
	}

	distinctDates := map[string]bool{}
	for _, sess := range sessions {
		content := formatSession(sess)
		if content == "" {
			continue
		}
		date := sess.date
		handler.Now = func() time.Time { return date }
		if _, err := sendChatTurn(handler, scope.Owner, *answerModel, "", content); err != nil {
			return fmt.Errorf("ingest-turns: replay session at %s: %w", sess.date.Format(time.RFC3339), err)
		}
		distinctDates[sess.date.Format("2006-01-02")] = true
	}

	var sortedDates []string
	for d := range distinctDates {
		sortedDates = append(sortedDates, d)
	}
	sort.Strings(sortedDates)

	consolidatedOK := true
	if !*skipConsolidate {
		for _, d := range sortedDates {
			cmd := exec.Command(*consolidateBin, "-date", d)
			cmd.Stdout = os.Stderr // keep stdout clean for the run_ctx JSON below
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				slog.Error("consolidation failed for a date, continuing", "date", d, "error", err)
				consolidatedOK = false
			}
		}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(runCtx{
		SampleID:       *sampleID,
		ScopeKind:      scope.Kind,
		ScopeOwner:     scope.Owner,
		SessionDates:   sortedDates,
		ConsolidatedOK: consolidatedOK,
	})
}

type turn struct {
	speaker string
	text    string
}

type session struct {
	date  time.Time
	turns []turn
}

func groupIntoSessions(turns []inputTurn) ([]session, error) {
	bySession := map[int][]inputTurn{}
	dateBySession := map[int]string{}
	for _, t := range turns {
		bySession[t.SessionIndex] = append(bySession[t.SessionIndex], t)
		if t.SessionDatetime != "" {
			dateBySession[t.SessionIndex] = t.SessionDatetime
		}
	}

	var indices []int
	for idx := range bySession {
		indices = append(indices, idx)
	}
	sort.Ints(indices)

	sessions := make([]session, 0, len(indices))
	for _, idx := range indices {
		rawDate := strings.TrimSpace(dateBySession[idx])
		if rawDate == "" {
			return nil, fmt.Errorf("ingest-turns: session %d has no session_datetime on any of its turns", idx)
		}
		date, err := time.Parse(locomoDateLayout, rawDate)
		if err != nil {
			return nil, fmt.Errorf("ingest-turns: parse session_datetime %q for session %d: %w", rawDate, idx, err)
		}
		sess := session{date: date}
		for _, t := range bySession[idx] {
			sess.turns = append(sess.turns, turn{speaker: t.Speaker, text: t.Text})
		}
		sessions = append(sessions, sess)
	}
	return sessions, nil
}

func formatSession(sess session) string {
	var b strings.Builder
	for _, t := range sess.turns {
		fmt.Fprintf(&b, "%s: %s\n", t.speaker, t.text)
	}
	return strings.TrimSpace(b.String())
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

func sendChatTurn(handler *gateway.Handler, userID, model, systemPrompt, content string) (string, error) {
	msgs := []wireMessage{}
	if systemPrompt != "" {
		msgs = append(msgs, wireMessage{Role: "system", Content: systemPrompt})
	}
	msgs = append(msgs, wireMessage{Role: "user", Content: content})
	reqBody, err := json.Marshal(wireChatRequest{Model: model, Messages: msgs})
	if err != nil {
		return "", fmt.Errorf("ingest-turns: marshal request: %w", err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+userID)
	rec := httptest.NewRecorder()

	handler.HandleChatCompletions(rec, req)

	if rec.Code != 200 {
		return "", fmt.Errorf("ingest-turns: gateway returned %d: %s", rec.Code, rec.Body.String())
	}
	var resp wireChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return "", fmt.Errorf("ingest-turns: parse gateway response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("ingest-turns: gateway response had no choices: %s", rec.Body.String())
	}
	return resp.Choices[0].Message.Content, nil
}
