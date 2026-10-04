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
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"time"

	"hupi/internal/auth"
	"hupi/internal/bootstrap"
	"hupi/internal/dbscope"
	"hupi/internal/gateway"
	"hupi/internal/identity"
	"hupi/internal/store"
)

// conversationResult pairs one replayed conversation/instance with its
// answers and each answer's exact retrieved context, in conv.qa order —
// the shared input both output writers (marshalLoCoMoPredictions,
// marshalLongMemEvalHypotheses) format differently, per their respective
// benchmark's own expected shape. retrievedContexts is the C_original
// docs/EVALMEM_INTEGRATION_PLAN.md's retrieve_original needs — captured
// via gateway.Handler.OnRetrieve, not a second Retrieve() call.
type conversationResult struct {
	conv              benchConversation
	answers           []string
	retrievedContexts []string
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
	answerOnly := flag.Bool("answer-only", false, "skip session replay and consolidation, re-answer questions against an already-consolidated scope from a prior run (e.g. to test a QA-prompt change without re-paying for replay/consolidation); ignored with -baseline, which never touches HUPI's memory anyway")
	answerModel := flag.String("answer-model", "", "providers.yaml profile name to answer with (defaults to active_chat_provider)")
	outFile := flag.String("out-file", "", "where to write predictions (default: stdout)")
	retrievedContextOutFile := flag.String("retrieved-context-out-file", "", "optional: also write each answer's exact retrieved context (C_original) here, for external diagnostic tooling (see docs/EVALMEM_INTEGRATION_PLAN.md) — never fed to either benchmark's own scoring code")
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
	// Still off unless HUPI_ENABLE_QUERY_EXPANSION is also set — see
	// Store.EnableQueryExpansion's own doc comment. Wired here so this
	// benchmark can actually exercise/measure query expansion when the
	// env var is set, same as it already exercises every other real
	// retrieval behavior.
	st.EnableQueryExpansion(deps.Registry.Chat())

	model := *answerModel

	// Resume support: if -out-file already has complete results for some
	// conversations (from a prior run interrupted mid-way — e.g. the real
	// OpenAI credit-exhaustion incident this was added for), skip
	// re-processing them instead of either redoing ~8 hours of paid-for
	// work or silently double-replaying an already-populated scope (see
	// resetScope's own doc comment on why the latter would corrupt data).
	priorAnswers, err := loadPriorAnswers(*benchmark, *outFile, convs)
	if err != nil {
		return err
	}
	if len(priorAnswers) > 0 {
		slog.Info("resuming from existing -out-file", "already_complete", len(priorAnswers), "total", len(convs))
	}

	var results []conversationResult
	var failedConvs []string
	for ci, conv := range convs {
		if answers, done := priorAnswers[conv.id]; done {
			slog.Info("skipping already-completed conversation", "index", ci, "id", conv.id)
			results = append(results, conversationResult{conv: conv, answers: answers, retrievedContexts: make([]string, len(conv.qa))})
			continue
		}

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

		if !*baseline && !*answerOnly {
			// Guarantee a clean scope before every real replay, whether
			// this conversation is brand new or was left partially
			// consolidated by a run that crashed before reaching this
			// point — replayConversation has no dedup guard, so
			// re-replaying into an already-populated scope would
			// silently double episodes rather than cleanly redo it.
			if err := resetScope(ctx, deps.DB, userID); err != nil {
				slog.Error("conversation failed, continuing to next", "id", conv.id, "error", fmt.Errorf("reset scope: %w", err))
				failedConvs = append(failedConvs, conv.id)
				continue
			}
		}

		if err := authStore.CreateUser(ctx, userID, ""); err != nil {
			slog.Error("conversation failed, continuing to next", "id", conv.id, "error", fmt.Errorf("provision scope: %w", err))
			failedConvs = append(failedConvs, conv.id)
			continue
		}

		handler := &gateway.Handler{
			Registry:  deps.Registry,
			Retriever: st,
			Capturer:  st,
			Auth:      staticAuth{},
		}

		var answers, retrievedContexts []string
		var convErr error
		switch {
		case *baseline:
			answers, retrievedContexts, convErr = runBaselineConversation(handler, userID, model, conv)
		case *answerOnly:
			answers, retrievedContexts, convErr = answerConversation(handler, userID, model, conv)
		default:
			answers, retrievedContexts, convErr = runHUPIConversation(handler, userID, model, conv, *consolidateBin)
		}
		if convErr != nil {
			// Log and continue rather than aborting the whole process:
			// this is exactly what turned a single late-run 429 into a
			// total loss of ~31 already-consolidated conversations'
			// worth of real, paid-for work. failedConvs is reported at
			// the end and can be retried by re-running the same command
			// with the same -out-file.
			slog.Error("conversation failed, continuing to next", "id", conv.id, "error", convErr)
			failedConvs = append(failedConvs, conv.id)
			continue
		}
		results = append(results, conversationResult{conv: conv, answers: answers, retrievedContexts: retrievedContexts})

		// Incremental write: persist progress after every conversation,
		// not just once at the very end, so a later failure (this run or
		// the process being killed) never discards already-completed
		// work.
		if *outFile != "" {
			if err := writeOutputs(results, *benchmark, *outFile, *retrievedContextOutFile); err != nil {
				return fmt.Errorf("bench: incremental write after %s: %w", conv.id, err)
			}
		}
	}

	if len(failedConvs) > 0 {
		slog.Warn("some conversations failed and were skipped — re-run the same command with the same -out-file to retry just these", "failed_count", len(failedConvs), "total", len(convs), "failed_ids", failedConvs)
	}

	if err := writeOutputs(results, *benchmark, *outFile, *retrievedContextOutFile); err != nil {
		return err
	}

	if len(failedConvs) > 0 {
		return fmt.Errorf("bench: %d of %d conversations failed (see log) — output written for the %d that succeeded; re-run with the same -out-file to retry just the failures", len(failedConvs), len(convs), len(results))
	}
	return nil
}

// writeOutputs marshals results into the requested benchmark's own
// expected shape and writes -out-file (or prints to stdout if unset),
// plus the optional -retrieved-context-out-file. Called after every
// conversation, not just once at the end of run() — see run()'s own
// comment on why (the real credit-exhaustion incident that discarded a
// whole night's consolidation work because output was previously only
// written once, at the very end).
func writeOutputs(results []conversationResult, benchmark, outFile, retrievedContextOutFile string) error {
	var out []byte
	var err error
	if benchmark == "longmemeval" {
		out, err = marshalLongMemEvalHypotheses(results)
	} else {
		out, err = marshalLoCoMoPredictions(results)
	}
	if err != nil {
		return err
	}

	if retrievedContextOutFile != "" {
		contextsOut, err := marshalRetrievedContexts(results)
		if err != nil {
			return err
		}
		if err := os.WriteFile(retrievedContextOutFile, contextsOut, 0o644); err != nil {
			return fmt.Errorf("bench: write retrieved contexts: %w", err)
		}
	}

	if outFile == "" {
		fmt.Println(string(out))
		return nil
	}
	return os.WriteFile(outFile, out, 0o644)
}

// loadPriorAnswers reads an existing -out-file from a prior (possibly
// interrupted) run and returns, per conversation ID, its answers in
// conv.qa order — but only for conversations that are genuinely
// complete: every one of their questions must have a real, non-empty
// recorded answer. answerQuestions/runBaselineConversation record a
// failed question as "" rather than aborting (see their own doc
// comments), so a conversation that partially failed last time is
// correctly treated as incomplete here and gets fully redone, not
// silently accepted with a hole in its answers. Returns (nil, nil) if
// outFile is unset or doesn't exist yet (first run).
func loadPriorAnswers(benchmark, outFile string, convs []benchConversation) (map[string][]string, error) {
	if outFile == "" {
		return nil, nil
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("bench: read existing -out-file for resume: %w", err)
	}

	complete := map[string][]string{}

	if benchmark == "longmemeval" {
		byQID := map[string]string{}
		dec := json.NewDecoder(bytes.NewReader(data))
		for {
			var line struct {
				QuestionID string `json:"question_id"`
				Hypothesis string `json:"hypothesis"`
			}
			if err := dec.Decode(&line); err != nil {
				if err == io.EOF {
					break
				}
				return nil, fmt.Errorf("bench: parse existing longmemeval output for resume: %w", err)
			}
			byQID[line.QuestionID] = line.Hypothesis
		}
		for _, conv := range convs {
			if len(conv.qa) == 0 {
				continue
			}
			answers := make([]string, len(conv.qa))
			ok := true
			for i, qa := range conv.qa {
				a, found := byQID[qa.id]
				if !found || a == "" {
					ok = false
					break
				}
				answers[i] = a
			}
			if ok {
				complete[conv.id] = answers
			}
		}
		return complete, nil
	}

	var entries []struct {
		SampleID string `json:"sample_id"`
		QA       []struct {
			HupiPrediction string `json:"hupi_prediction"`
		} `json:"qa"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("bench: parse existing locomo output for resume: %w", err)
	}
	bySample := map[string][]string{}
	for _, e := range entries {
		answers := make([]string, len(e.QA))
		for i, qa := range e.QA {
			answers[i] = qa.HupiPrediction
		}
		bySample[e.SampleID] = answers
	}
	for _, conv := range convs {
		answers, found := bySample[conv.id]
		if !found || len(answers) != len(conv.qa) {
			continue
		}
		ok := true
		for _, a := range answers {
			if a == "" {
				ok = false
				break
			}
		}
		if ok {
			complete[conv.id] = answers
		}
	}
	return complete, nil
}

// resetScope wipes any existing data for userID's private scope before a
// fresh replay. replayConversation/replayAndConsolidate have no dedup
// guard, so re-replaying into an already-populated scope (e.g. one left
// partially consolidated by a run that crashed mid-way — the real OpenAI
// credit-exhaustion incident this was added for) would silently double
// episodes rather than cleanly redo it. A no-op for a conversation
// that's never been touched. Table order and RLS-scoped-transaction
// requirement match internal/store's own test cleanup helper
// (scope_isolation_test.go's cleanupScope); scope_keys and users aren't
// RLS-protected (see internal/demo/store_test.go's own non-scoped
// cleanup), so those two are deleted outside the scoped transaction.
func resetScope(ctx context.Context, db *sql.DB, userID string) error {
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: userID}
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner); err != nil {
			return err
		}
		if _, err := tx.Exec(`delete from entity_relationships where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner); err != nil {
			return err
		}
		if _, err := tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner); err != nil {
			return err
		}
		if _, err := tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("reset scope: %w", err)
	}
	if _, err := db.ExecContext(ctx, `delete from scope_keys where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner); err != nil {
		return fmt.Errorf("reset scope: delete scope_keys: %w", err)
	}
	if _, err := db.ExecContext(ctx, `delete from users where id = $1`, userID); err != nil {
		return fmt.Errorf("reset scope: delete user: %w", err)
	}
	return nil
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

// marshalRetrievedContexts writes each answer's exact retrieved context
// (C_original) to its own file, deliberately separate from
// marshalLoCoMoPredictions/marshalLongMemEvalHypotheses: those two feed
// each benchmark's real, unmodified official scoring code verbatim, and
// this repo's own discipline throughout (docs/BENCHMARKS.md) has been to
// never blend harness-only additions into files fed to someone else's
// scoring code, even ones that would likely tolerate an extra JSON key
// harmlessly. This file exists purely for external diagnostic tooling
// (docs/EVALMEM_INTEGRATION_PLAN.md's retrieve_original/C_original).
func marshalRetrievedContexts(results []conversationResult) ([]byte, error) {
	convOuts := make([]map[string]any, len(results))
	for ci, r := range results {
		questions := make([]map[string]any, len(r.conv.qa))
		for i, qa := range r.conv.qa {
			entry := map[string]any{
				"index":             i,
				"question":          qa.question,
				"retrieved_context": r.retrievedContexts[i],
			}
			if qa.id != "" {
				entry["question_id"] = qa.id
			}
			questions[i] = entry
		}
		convOuts[ci] = map[string]any{
			"sample_id": r.conv.id,
			"questions": questions,
		}
	}
	out, err := json.MarshalIndent(convOuts, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("bench: marshal retrieved contexts: %w", err)
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
func runHUPIConversation(handler *gateway.Handler, userID, model string, conv benchConversation, consolidateBin string) ([]string, []string, error) {
	if err := replayAndConsolidate(handler, userID, model, conv, consolidateBin); err != nil {
		return nil, nil, err
	}
	return answerConversation(handler, userID, model, conv)
}

// replayAndConsolidate does everything runHUPIConversation does except
// answer the questions — split out so -answer-only (see run()) can skip
// straight to answerConversation and reuse an already-consolidated scope
// from a prior run, without re-paying for replay/consolidation just to
// test a QA-prompt change. The memory state a QA-prompt edit needs to be
// tested against doesn't change; only how the answer gets extracted from
// it does.
func replayAndConsolidate(handler *gateway.Handler, userID, model string, conv benchConversation, consolidateBin string) error {
	slog.Info("replaying sessions", "id", conv.id)
	dates, err := replayConversation(handler, userID, model, conv)
	if err != nil {
		return err
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
	return nil
}

// answerConversation is runHUPIConversation's tail: assumes conv's scope
// is already fully replayed and consolidated (either by
// replayAndConsolidate just now, or by a prior run when called via
// -answer-only), and just answers the questions against whatever memory
// state already exists for userID.
func answerConversation(handler *gateway.Handler, userID, model string, conv benchConversation) ([]string, []string, error) {
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
