// Command hupi-bench replays public long-term-memory benchmarks
// (LoCoMo, LongMemEval) through HUPI's real gateway — real episodes,
// real nightly consolidation, real retrieval — then answers each
// benchmark's questions through the same real chat-completion path, so
// the resulting score reflects the actual product, not a shortcut. See
// the reviewed plan (docs/BENCHMARKS.md, once Step 6 lands) for the
// full design and why each piece works the way it does.
//
// Step 2 scope: one LoCoMo conversation, replayed and consolidated for
// real, predictions written to stdout/a JSON file — no official scoring
// integration yet (that's bench/score_locomo.py, Step 3).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"time"

	"hupi/internal/auth"
	"hupi/internal/bootstrap"
	"hupi/internal/gateway"
	"hupi/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("hupi-bench exited with error", "error", err)
		os.Exit(1)
	}
}

type prediction struct {
	Question   string `json:"question"`
	Expected   string `json:"expected"`
	Category   int    `json:"category,omitempty"`
	HupiAnswer string `json:"hupi_answer"`
}

func run() error {
	dataFile := flag.String("data-file", "", "path to locomo10.json")
	convIndex := flag.Int("conv-index", 0, "which conversation to replay (0-based)")
	answerModel := flag.String("answer-model", "", "providers.yaml profile name to answer with (defaults to active_chat_provider)")
	outFile := flag.String("out-file", "", "where to write predictions JSON (default: stdout)")
	consolidateBin := flag.String("consolidate-bin", "./hupi-consolidate", "path to the real hupi-consolidate binary")
	flag.Parse()

	if *dataFile == "" {
		return fmt.Errorf("bench: -data-file is required")
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

	conv, err := loadLoCoMo(*dataFile, *convIndex)
	if err != nil {
		return fmt.Errorf("bench: load conversation: %w", err)
	}
	slog.Info("loaded conversation", "id", conv.id, "sessions", len(conv.sessions), "questions", len(conv.qa))

	authStore := auth.New(deps.DB, deps.Keys)
	userID := fmt.Sprintf("user:bench-locomo-%s", conv.id)
	if err := authStore.CreateUser(ctx, userID, ""); err != nil {
		return fmt.Errorf("bench: provision scope: %w", err)
	}

	st := store.New(deps.DB, deps.Keys, deps.Registry.Embedding())
	handler := &gateway.Handler{
		Registry:  deps.Registry,
		Retriever: st,
		Capturer:  st,
		Auth:      staticAuth{},
	}

	model := *answerModel
	if model == "" {
		model = "" // empty model string resolves to active_chat_provider — see gateway's resolveProvider
	}

	slog.Info("replaying sessions")
	dates, err := replayConversation(handler, userID, model, conv)
	if err != nil {
		return err
	}

	var sortedDates []string
	for d := range dates {
		sortedDates = append(sortedDates, d)
	}
	sort.Strings(sortedDates)

	slog.Info("running real consolidation per fabricated date", "dates", sortedDates)
	var consolidationFailures int
	for _, d := range sortedDates {
		// Shelling out to the real, unmodified hupi-consolidate binary
		// rather than calling consolidation.Runner in-process: the
		// weekly/monthly/yearly rollup scheduling logic (dueRollups,
		// rollupJob) lives in cmd/hupi-consolidate's own package, not
		// internal/consolidation, deliberately (its own doc comment:
		// "calendar logic that belongs here, in the cron scheduling
		// layer") — so it isn't importable from a separate command, and
		// duplicating it here would risk silently drifting from the
		// real thing this harness is supposed to be measuring. This
		// also means every fabricated date gets exactly the same rollup
		// behavior a real nightly cron run would produce.
		cmd := exec.Command(*consolidateBin, "-date", d)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		// Log and continue on a single bad date, exactly like a real
		// nightly cron run would (hupi-consolidate's own main.go: "One
		// user's or team's bad day... shouldn't block everyone else's
		// consolidation"). Aborting the whole benchmark run over one
		// day's malformed LLM output would make an otherwise-working
		// harness look broken over a model-output-quality issue that's
		// real but separate from whether replay/consolidation/rollup
		// scheduling themselves are wired correctly.
		if err := cmd.Run(); err != nil {
			slog.Error("consolidation failed for a date, continuing", "date", d, "error", err)
			consolidationFailures++
		}
	}
	if consolidationFailures > 0 {
		slog.Warn("some dates failed to consolidate — answers may be missing memory from those days", "failed_dates", consolidationFailures, "total_dates", len(sortedDates))
	}

	// Query time: one day after the last session, so retrieval reflects
	// the full, already-consolidated history — LoCoMo's QA has no
	// per-question date of its own (see bench/FORMAT.md).
	queryTime := time.Now()
	if len(conv.sessions) > 0 {
		queryTime = conv.sessions[len(conv.sessions)-1].date.AddDate(0, 0, 1)
	}

	slog.Info("answering questions", "count", len(conv.qa), "query_time", queryTime)
	answers, err := answerQuestions(handler, userID, model, conv, queryTime)
	if err != nil {
		return err
	}

	preds := make([]prediction, len(conv.qa))
	for i, qa := range conv.qa {
		preds[i] = prediction{
			Question:   qa.question,
			Expected:   qa.answer,
			Category:   qa.category,
			HupiAnswer: answers[i],
		}
	}

	out, err := json.MarshalIndent(preds, "", "  ")
	if err != nil {
		return fmt.Errorf("bench: marshal predictions: %w", err)
	}
	if *outFile == "" {
		fmt.Println(string(out))
		return nil
	}
	return os.WriteFile(*outFile, out, 0o644)
}
