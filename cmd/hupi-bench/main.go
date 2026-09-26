// Command hupi-bench replays public long-term-memory benchmarks
// (LoCoMo, LongMemEval) through HUPI's real gateway — real episodes,
// real nightly consolidation, real retrieval — then answers each
// benchmark's questions through the same real chat-completion path, so
// the resulting score reflects the actual product, not a shortcut. See
// the reviewed plan (docs/BENCHMARKS.md, once Step 6 lands) for the
// full design and why each piece works the way it does.
//
// Writes predictions in each benchmark's own expected shape — LoCoMo's
// annotation-file shape (see the predictionKey doc comment below) so
// bench/score_locomo.py can score with LoCoMo's real, unmodified scoring
// code, or LongMemEval's {question_id, hypothesis} JSONL hypothesis-file
// shape for their own unmodified src/evaluation/evaluate_qa.py — no
// reimplemented metric logic in this repo at all, for either benchmark.
package main

import (
	"bytes"
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

// conversationResult pairs one replayed conversation/instance with its
// answers, in conv.qa order — the shared input both output writers
// (marshalLoCoMoPredictions, marshalLongMemEvalHypotheses) format
// differently, per their respective benchmark's own expected shape.
type conversationResult struct {
	conv    benchConversation
	answers []string
}

func main() {
	if err := run(); err != nil {
		slog.Error("hupi-bench exited with error", "error", err)
		os.Exit(1)
	}
}

// predictionKey is the field name LoCoMo's own scoring code is told to
// read the model's answer from (task_eval.evaluation.eval_question_answering's
// eval_key parameter) — bench/score_locomo.py passes this same literal
// string, so it must match exactly.
const predictionKey = "hupi_prediction"

func run() error {
	benchmark := flag.String("benchmark", "locomo", "which benchmark to replay: locomo or longmemeval")
	dataFile := flag.String("data-file", "", "path to locomo10.json (locomo) or longmemeval_*_cleaned.json (longmemeval)")
	convIndex := flag.Int("conv-index", 0, "which conversation/instance to replay (0-based); ignored if -all-conversations is set")
	allConversations := flag.Bool("all-conversations", false, "process every conversation/instance in the data file, not just -conv-index")
	baseline := flag.Bool("baseline", false, "no-memory baseline: skip HUPI replay/consolidation, stuff raw session transcripts directly into the answer-model's context instead")
	answerModel := flag.String("answer-model", "", "providers.yaml profile name to answer with (defaults to active_chat_provider)")
	outFile := flag.String("out-file", "", "where to write predictions (default: stdout)")
	consolidateBin := flag.String("consolidate-bin", "./hupi-consolidate", "path to the real hupi-consolidate binary")
	flag.Parse()

	if *dataFile == "" {
		return fmt.Errorf("bench: -data-file is required")
	}
	if *benchmark != "locomo" && *benchmark != "longmemeval" {
		return fmt.Errorf("bench: -benchmark must be locomo or longmemeval, got %q", *benchmark)
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

	var convs []benchConversation
	if *allConversations {
		convs, err = loadAll(*benchmark, *dataFile)
		if err != nil {
			return fmt.Errorf("bench: load conversations: %w", err)
		}
	} else {
		all, err := loadAll(*benchmark, *dataFile)
		if err != nil {
			return fmt.Errorf("bench: load conversations: %w", err)
		}
		if *convIndex < 0 || *convIndex >= len(all) {
			return fmt.Errorf("bench: conv-index %d out of range (0-%d)", *convIndex, len(all)-1)
		}
		convs = []benchConversation{all[*convIndex]}
	}
	slog.Info("loaded conversations", "benchmark", *benchmark, "count", len(convs), "baseline", *baseline)

	authStore := auth.New(deps.DB, deps.Keys)
	st := store.New(deps.DB, deps.Keys, deps.Registry.Embedding())

	model := *answerModel

	results := make([]conversationResult, len(convs))
	for ci, conv := range convs {
		slog.Info("processing conversation", "index", ci, "id", conv.id, "sessions", len(conv.sessions), "questions", len(conv.qa))

		// Baseline runs use a distinct scope from the HUPI-memory runs
		// (a "-baseline" suffix) even for the same conversation ID:
		// auth.Store.CreateUser is idempotent (insert ... on conflict do
		// nothing), so reusing the same scope across modes would let a
		// prior HUPI run's already-consolidated episodes leak into what's
		// supposed to be a memory-free control.
		modeTag := "hupi"
		if *baseline {
			modeTag = "baseline"
		}
		userID := fmt.Sprintf("user:bench-%s-%s-%s", *benchmark, modeTag, conv.id)
		if err := authStore.CreateUser(ctx, userID, ""); err != nil {
			return fmt.Errorf("bench: provision scope for %s: %w", conv.id, err)
		}

		handler := &gateway.Handler{
			Registry:  deps.Registry,
			Retriever: st,
			Capturer:  st,
			Auth:      staticAuth{},
		}

		var answers []string
		if *baseline {
			answers, err = runBaselineConversation(handler, userID, model, conv)
			if err != nil {
				return err
			}
		} else {
			answers, err = runHUPIConversation(handler, userID, model, conv, *consolidateBin)
			if err != nil {
				return err
			}
		}
		results[ci] = conversationResult{conv: conv, answers: answers}
	}

	var out []byte
	if *benchmark == "longmemeval" {
		out, err = marshalLongMemEvalHypotheses(results)
	} else {
		out, err = marshalLoCoMoPredictions(results)
	}
	if err != nil {
		return err
	}
	if *outFile == "" {
		fmt.Println(string(out))
		return nil
	}
	return os.WriteFile(*outFile, out, 0o644)
}

func loadAll(benchmark, dataFile string) ([]benchConversation, error) {
	if benchmark == "longmemeval" {
		return loadLongMemEvalAll(dataFile)
	}
	return loadLoCoMoAll(dataFile)
}

// marshalLoCoMoPredictions writes LoCoMo's own annotation-file shape --
// [{"sample_id": ..., "qa": [<original qa object> + hupi_prediction]}]
// -- plus one added field per question, rather than a harness-invented
// shape. This is what lets bench/score_locomo.py call LoCoMo's own,
// completely unmodified task_eval.evaluation_stats.analyze_aggr_acc
// directly: that function reads fields (e.g. evidence) straight off each
// qa entry, so anything less than the real entry, verbatim, would break
// under their own scoring code, not just a hand-rolled one.
func marshalLoCoMoPredictions(results []conversationResult) ([]byte, error) {
	convOuts := make([]map[string]any, len(results))
	for ci, r := range results {
		qaOut := make([]map[string]any, len(r.conv.qa))
		for i, qa := range r.conv.qa {
			var entry map[string]any
			if err := json.Unmarshal(qa.raw, &entry); err != nil {
				return nil, fmt.Errorf("bench: re-parse qa entry %d for output: %w", i, err)
			}
			entry[predictionKey] = r.answers[i]
			qaOut[i] = entry
		}
		convOuts[ci] = map[string]any{
			"sample_id": r.conv.id,
			"qa":        qaOut,
		}
	}
	out, err := json.MarshalIndent(convOuts, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("bench: marshal predictions: %w", err)
	}
	return out, nil
}

// marshalLongMemEvalHypotheses writes their own hypothesis-file shape --
// one JSON object per line, {"question_id": ..., "hypothesis": ...} --
// exactly what src/evaluation/evaluate_qa.py expects as its hyp_file
// argument, unmodified.
func marshalLongMemEvalHypotheses(results []conversationResult) ([]byte, error) {
	var buf bytes.Buffer
	for _, r := range results {
		for i, qa := range r.conv.qa {
			line, err := json.Marshal(map[string]any{
				"question_id": qa.id,
				"hypothesis":  r.answers[i],
			})
			if err != nil {
				return nil, fmt.Errorf("bench: marshal hypothesis line for %s: %w", qa.id, err)
			}
			buf.Write(line)
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes(), nil
}

// runHUPIConversation is the real-memory path: replay every session
// through the real gateway.Handler with backdated timestamps, run the
// real hupi-consolidate binary once per fabricated date, then answer the
// conversation's questions through the same real retrieval+generation
// path.
func runHUPIConversation(handler *gateway.Handler, userID, model string, conv benchConversation, consolidateBin string) ([]string, error) {
	slog.Info("replaying sessions", "id", conv.id)
	dates, err := replayConversation(handler, userID, model, conv)
	if err != nil {
		return nil, err
	}

	var sortedDates []string
	for d := range dates {
		sortedDates = append(sortedDates, d)
	}
	sort.Strings(sortedDates)

	slog.Info("running real consolidation per fabricated date", "id", conv.id, "dates", sortedDates)
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
		cmd := exec.Command(consolidateBin, "-date", d)
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
			slog.Error("consolidation failed for a date, continuing", "id", conv.id, "date", d, "error", err)
			consolidationFailures++
		}
	}
	if consolidationFailures > 0 {
		slog.Warn("some dates failed to consolidate — answers may be missing memory from those days", "id", conv.id, "failed_dates", consolidationFailures, "total_dates", len(sortedDates))
	}

	// Query time: one day after the last session, so retrieval reflects
	// the full, already-consolidated history — LoCoMo's QA has no
	// per-question date of its own (see bench/FORMAT.md).
	queryTime := time.Now()
	if len(conv.sessions) > 0 {
		queryTime = conv.sessions[len(conv.sessions)-1].date.AddDate(0, 0, 1)
	}

	// Logged as "default" because it's only the fallback: LongMemEval's
	// own qa[i].queryTime (its real question_date) takes priority per
	// question inside answerQuestions — this line ran before that
	// per-question override applies, so it would otherwise misleadingly
	// suggest every question in this conversation used the same instant.
	slog.Info("answering questions", "id", conv.id, "count", len(conv.qa), "default_query_time", queryTime)
	return answerQuestions(handler, userID, model, conv, queryTime)
}
