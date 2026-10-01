package consolidation

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/crypto"
	"hupi/internal/identity"
	"hupi/internal/provider"
)

func factsN(n int) []KeyFactOutput {
	facts := make([]KeyFactOutput, n)
	for i := range facts {
		facts[i] = KeyFactOutput{Fact: "fact"}
	}
	return facts
}

// TestGroundingCheckBatchesLargeFactListsPreservingOrder is the real,
// measured fix (docs/CONSOLIDATION_COMPLETENESS_PLAN.md, per-episode fact
// extraction verification): a single grounding call covering 25 facts
// mismatched its own count on every real attempt once per-episode
// extraction pushed a busy day's fact total past ~70. Splitting into
// groundingCheckBatchSize-sized calls keeps each individual list short.
// This test only exercises the split/reassemble bookkeeping (a fake
// provider can't reproduce the real model's own count-mismatch behavior),
// confirming batch boundaries and result order are correct.
func TestGroundingCheckBatchesLargeFactListsPreservingOrder(t *testing.T) {
	facts := factsN(25) // groundingCheckBatchSize=20 -> batches of 20, then 5
	fake := &fakeSequentialProvider{responses: []string{
		`{"grounded": [{"i": 1, "ok": true}, {"i": 2, "ok": true}, {"i": 3, "ok": true}, {"i": 4, "ok": true}, {"i": 5, "ok": true}, {"i": 6, "ok": true}, {"i": 7, "ok": true}, {"i": 8, "ok": true}, {"i": 9, "ok": true}, {"i": 10, "ok": true}, {"i": 11, "ok": false}, {"i": 12, "ok": false}, {"i": 13, "ok": false}, {"i": 14, "ok": false}, {"i": 15, "ok": false}, {"i": 16, "ok": false}, {"i": 17, "ok": false}, {"i": 18, "ok": false}, {"i": 19, "ok": false}, {"i": 20, "ok": false}]}`,
		`{"grounded": [{"i": 1, "ok": true}, {"i": 2, "ok": true}, {"i": 3, "ok": true}, {"i": 4, "ok": true}, {"i": 5, "ok": true}]}`,
	}}
	runner := New(nil, nil, nil, fake, nil)

	got, err := runner.groundingCheck(context.Background(), "source", facts)
	if err != nil {
		t.Fatalf("groundingCheck: %v", err)
	}
	if len(got) != 25 {
		t.Fatalf("groundingCheck() returned %d results, want 25", len(got))
	}
	if fake.calls != 2 {
		t.Fatalf("provider called %d times, want 2 (one per batch)", fake.calls)
	}
	for i := 0; i < 10; i++ {
		if !got[i] {
			t.Errorf("got[%d] = false, want true (first half of batch 1)", i)
		}
	}
	for i := 10; i < 20; i++ {
		if got[i] {
			t.Errorf("got[%d] = true, want false (second half of batch 1)", i)
		}
	}
	for i := 20; i < 25; i++ {
		if !got[i] {
			t.Errorf("got[%d] = false, want true (batch 2)", i)
		}
	}
}

// TestGroundingCheckMismatchOnOneBatchDoesNotDegradeOthers is the actual
// benefit the batching split provides: an incomplete response on one
// batch (which, per groundingCheckOne's retry-then-salvage-by-index
// behavior, forces only its unindexed facts to ungrounded, not the whole
// batch) must not also discard the other, correctly-matched batch's real
// results — the exact all-or-nothing blast radius that made this a real
// problem on busy days in the first place.
func TestGroundingCheckMismatchOnOneBatchDoesNotDegradeOthers(t *testing.T) {
	facts := factsN(25)
	batch1Incomplete := `{"grounded": [{"i": 1, "ok": true}, {"i": 2, "ok": true}, {"i": 3, "ok": true}, {"i": 4, "ok": true}, {"i": 5, "ok": true}, {"i": 6, "ok": true}, {"i": 7, "ok": true}, {"i": 8, "ok": true}, {"i": 9, "ok": true}, {"i": 10, "ok": true}, {"i": 11, "ok": true}, {"i": 12, "ok": true}, {"i": 13, "ok": true}, {"i": 14, "ok": true}, {"i": 15, "ok": true}, {"i": 16, "ok": true}, {"i": 17, "ok": true}, {"i": 18, "ok": true}]}` // 20 facts claimed, only indices 1-18 present -> incomplete
	fake := &fakeSequentialProvider{responses: []string{
		// batch 1, attempt 1: incomplete (missing indices 19, 20) -> triggers a retry
		batch1Incomplete,
		// batch 1, attempt 2 (retry): same incomplete shape -> falls back to index salvage
		batch1Incomplete,
		// batch 2: 5 facts, all 5 indices present -> matches, all true preserved
		`{"grounded": [{"i": 1, "ok": true}, {"i": 2, "ok": true}, {"i": 3, "ok": true}, {"i": 4, "ok": true}, {"i": 5, "ok": true}]}`,
	}}
	runner := New(nil, nil, nil, fake, nil)

	got, err := runner.groundingCheck(context.Background(), "source", facts)
	if err != nil {
		t.Fatalf("groundingCheck: %v", err)
	}
	if fake.calls != 3 {
		t.Errorf("provider called %d times, want 3 (batch 1's original attempt + its retry, then batch 2)", fake.calls)
	}
	for i := 0; i < 18; i++ {
		if !got[i] {
			t.Errorf("got[%d] = false, want true (batch 1's indexed facts must keep their real verdict despite the batch being incomplete)", i)
		}
	}
	for i := 18; i < 20; i++ {
		if got[i] {
			t.Errorf("got[%d] = true, want false (batch 1's two unindexed facts, salvaged to ungrounded)", i)
		}
	}
	for i := 20; i < 25; i++ {
		if !got[i] {
			t.Errorf("got[%d] = false, want true (batch 2 matched cleanly and must be unaffected by batch 1's incompleteness)", i)
		}
	}
}

func TestGroundingCheckSmallListSkipsBatching(t *testing.T) {
	facts := factsN(3)
	fake := &fakeSequentialProvider{responses: []string{
		`{"grounded": [{"i": 1, "ok": true}, {"i": 2, "ok": false}, {"i": 3, "ok": true}]}`,
	}}
	runner := New(nil, nil, nil, fake, nil)

	got, err := runner.groundingCheck(context.Background(), "source", facts)
	if err != nil {
		t.Fatalf("groundingCheck: %v", err)
	}
	if fake.calls != 1 {
		t.Errorf("provider called %d times, want exactly 1 for a list under the batch size", fake.calls)
	}
	if len(got) != 3 || got[0] != true || got[1] != false || got[2] != true {
		t.Errorf("groundingCheck() = %v, want [true false true]", got)
	}
}

func TestGroundingCheckEmptyInput(t *testing.T) {
	runner := New(nil, nil, nil, &fakeSequentialProvider{}, nil)
	got, err := runner.groundingCheck(context.Background(), "source", nil)
	if err != nil {
		t.Fatalf("groundingCheck: %v", err)
	}
	if got != nil {
		t.Errorf("groundingCheck(nil facts) = %v, want nil", got)
	}
}

// TestGroundingCheckOneExtraVerdictNoLongerZeroesWholeBatch is the direct
// regression test for the real got=21/want=20 failure this package used to
// handle by discarding every one of the 20 real verdicts (see
// TestLiveGroundingCheckReproducesBatchMismatchWithRealData for the live
// reproduction this fixture is modeled on). With index-tagged verdicts, an
// extra/duplicate entry no longer costs anything: every fact whose real
// index appears keeps its real verdict, and only a genuinely-missing
// index would fall back to ungrounded (none do here).
func TestGroundingCheckOneExtraVerdictNoLongerZeroesWholeBatch(t *testing.T) {
	facts := factsN(20)
	// 21 entries for 20 facts: indices 1-20 all true, plus one duplicate
	// entry for index 5 (the shape a live mismatch actually took — see
	// TestLiveGroundingCheckReproducesBatchMismatchWithRealData's
	// sneakers_scope_new_fact_day run, which returned 21 verdicts for a
	// 20-fact batch). indexVerdicts keeps the first value seen per index,
	// so the duplicate changes nothing.
	response := `{"grounded": [` +
		`{"i":1,"ok":true},{"i":2,"ok":true},{"i":3,"ok":true},{"i":4,"ok":true},{"i":5,"ok":true},` +
		`{"i":6,"ok":true},{"i":7,"ok":true},{"i":8,"ok":true},{"i":9,"ok":true},{"i":10,"ok":true},` +
		`{"i":11,"ok":true},{"i":12,"ok":true},{"i":13,"ok":true},{"i":14,"ok":true},{"i":15,"ok":true},` +
		`{"i":16,"ok":true},{"i":17,"ok":true},{"i":18,"ok":true},{"i":19,"ok":true},{"i":20,"ok":true},` +
		`{"i":5,"ok":false}]}`
	fake := &fakeSequentialProvider{responses: []string{response}}
	runner := New(nil, nil, nil, fake, nil)

	got, err := runner.groundingCheck(context.Background(), "source", facts)
	if err != nil {
		t.Fatalf("groundingCheck: %v", err)
	}
	if fake.calls != 1 {
		t.Errorf("provider called %d times, want exactly 1 — a complete-by-index response (even with an extra entry) must not trigger a retry", fake.calls)
	}
	for i, g := range got {
		if !g {
			t.Errorf("got[%d] = false, want true — one extra/duplicate verdict must not cost any fact its real verdict", i)
		}
	}
}

// TestLiveGroundingCheckReproducesBatchMismatch calls the real, configured
// grounding model (not a fake) with a synthetic 20-fact batch — the exact
// groundingCheckBatchSize ceiling — to confirm, against live output rather
// than inference from a benchmark log, that the model reliably returns a
// mismatched verdict count at this batch size and to pin down the actual
// shape of the extra/missing element. Skipped unless OPENAI_API_KEY is set
// (requires a real network call; not part of the normal `go test ./...`
// run). See docs/CONSOLIDATION_COMPLETENESS_PLAN.md and
// docs/LONGMEMEVAL_ACCURACY_PLAN.md for the real benchmark run (6
// LongMemEval conversations against providers.cloud.yaml's gpt-4.1
// grounding/consolidation profile) that first surfaced this as a reliable
// got=21/want=20 count mismatch, not a rare fluke.
func TestLiveGroundingCheckReproducesBatchMismatch(t *testing.T) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		t.Skip("OPENAI_API_KEY not set; skipping live grounding reproduction")
	}

	real := provider.NewOpenAICompat(provider.OpenAICompatConfig{
		Name:    "live-gpt41",
		Vendor:  "openai",
		Model:   "gpt-4.1",
		BaseURL: "https://api.openai.com/v1",
		APIKey:  apiKey,
	})
	runner := New(nil, nil, nil, real, nil)

	sourceText := "The user described 20 independent, unrelated facts about their week, numbered one through twenty below."
	for _, n := range []int{15, 18, 20, 25} {
		n := n
		t.Run(fmt.Sprintf("batch_size_%d", n), func(t *testing.T) {
			facts := make([]KeyFactOutput, n)
			for i := range facts {
				facts[i] = KeyFactOutput{Fact: fmt.Sprintf("Fact number %d happened as described in the source text.", i+1)}
			}
			// Call the chat completion directly, not groundingCheckOne, so
			// the raw model output is visible before groundingCheckOne's
			// own count-mismatch correction (which silently discards the
			// evidence this test exists to capture) ever runs.
			resp, err := runner.grounding.ChatCompletion(context.Background(), provider.ChatRequest{
				Messages: []provider.Message{
					{Role: provider.RoleSystem, Content: groundingSystemPrompt},
					{Role: provider.RoleUser, Content: buildGroundingPrompt(sourceText, facts)},
				},
			})
			if err != nil {
				t.Fatalf("ChatCompletion: %v", err)
			}
			t.Logf("batch size %d: raw response content:\n%s", n, resp.Message.Content)

			var result struct {
				Grounded []bool `json:"grounded"`
			}
			if err := json.Unmarshal([]byte(extractJSON(resp.Message.Content)), &result); err != nil {
				t.Logf("batch size %d: could not parse response as {grounded:[...]}: %v", n, err)
				return
			}
			t.Logf("batch size %d: sent %d facts, parsed %d verdicts", n, n, len(result.Grounded))
			if len(result.Grounded) != n {
				t.Logf("REPRODUCED mismatch at batch size %d: sent %d facts, got %d verdicts (verdicts=%v)", n, n, len(result.Grounded), result.Grounded)
			}
		})
	}
}

// TestLiveGroundingCheckReproducesBatchMismatchWithRealData replays the
// grounding call against the real, decrypted facts and source episode text
// from one of the actual LongMemEval benchmark scopes that hit the
// got=21/want=20 mismatch, instead of synthetic placeholder facts (which
// did not reproduce it — real, varied fact content is what triggers the
// model's miscounting, not merely hitting a 20-item list). Requires
// OPENAI_API_KEY, HUPI_DATABASE_URL, and HUPI_KEK pointed at the
// hupi_sample6_fresh benchmark DB (see
// /home/samuel/hupi-bench-run/fresh48-bin/fresh6_db.env and cloud.env).
// Skipped unless all three are set.
func TestLiveGroundingCheckReproducesBatchMismatchWithRealData(t *testing.T) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	dbURL := os.Getenv("HUPI_DATABASE_URL")
	kek := os.Getenv("HUPI_KEK")
	if apiKey == "" || dbURL == "" || kek == "" {
		t.Skip("OPENAI_API_KEY/HUPI_DATABASE_URL/HUPI_KEK not all set; skipping live real-data grounding reproduction")
	}

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	kekBytes, err := base64.StdEncoding.DecodeString(kek)
	if err != nil {
		t.Fatalf("decode HUPI_KEK: %v", err)
	}
	keys := crypto.NewKeyStore(db, kekBytes)

	// summaryID/offset identify one exact, real, contiguous
	// groundingCheckBatchSize-sized slice of a summary's facts (in the same
	// id order they were originally inserted/batched in) that is entirely
	// grounded=false today — found by direct inspection of
	// hupi_sample6_fresh (see /tmp/g.txt-style batch analysis performed
	// during this investigation): a lone genuine "false" verdict from the
	// model is rare and scattered, but a full, contiguous 20-for-20 false
	// run is the signature of the count-mismatch bug zeroing an entire
	// batch, not 20 independent real judgments. This replays that exact
	// batch's real fact content (not a reassembled mix of scattered
	// ungrounded facts from unrelated, correctly-counted batches, which an
	// earlier version of this test did and failed to reproduce anything).
	for _, tc := range []struct {
		name      string
		owner     string
		period    string
		summaryID string
		offset    int
	}{
		{"bbq_scope", "user:bench-longmemeval-hupi-gpt4_4edbafa2", "2023-07-01",
			"sum_user:bench-longmemeval-hupi-gpt4_4edbafa2_2023-07-01_daily_v1", 20},
		{"sneakers_scope_new_fact_day", "user:bench-longmemeval-hupi-07741c45", "2023-05-29",
			"sum_user:bench-longmemeval-hupi-07741c45_2023-05-29_daily_v1", 40},
		{"sneakers_scope_old_fact_day_batch_a", "user:bench-longmemeval-hupi-07741c45", "2023-05-25",
			"sum_user:bench-longmemeval-hupi-07741c45_2023-05-25_daily_v2", 20},
		{"sneakers_scope_old_fact_day_batch_b", "user:bench-longmemeval-hupi-07741c45", "2023-05-25",
			"sum_user:bench-longmemeval-hupi-07741c45_2023-05-25_daily_v2", 60},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: tc.owner}
			enc, _, err := keys.GetOrCreate(context.Background(), scope)
			if err != nil {
				t.Fatalf("GetOrCreate key: %v", err)
			}

			rows, err := db.QueryContext(context.Background(), `
				select fact from summary_key_facts
				where summary_id = $1
				order by id
				offset $2 limit 20`, tc.summaryID, tc.offset)
			if err != nil {
				t.Fatalf("query facts: %v", err)
			}
			defer rows.Close()

			var facts []KeyFactOutput
			for rows.Next() {
				var blob []byte
				if err := rows.Scan(&blob); err != nil {
					t.Fatalf("scan: %v", err)
				}
				plain, err := enc.Decrypt(blob)
				if err != nil {
					t.Fatalf("decrypt fact: %v", err)
				}
				facts = append(facts, KeyFactOutput{Fact: plain})
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			if len(facts) == 0 {
				t.Skipf("no facts found at offset %d for %s — summary row set may have changed", tc.offset, tc.summaryID)
			}
			t.Logf("%s: found %d real facts at offset %d of %s", tc.name, len(facts), tc.offset, tc.summaryID)

			// Real source text for that date: the same episodes any
			// consolidation run for this scope/period would have used,
			// decrypted and joined the same way joinSources does.
			epRows, err := db.QueryContext(context.Background(), `
				select id, ts, input_text, output_text from episodes
				where scope_kind = $1 and scope_owner = $2
				  and type = 'interaction' and ts::date = $3::date
				order by ts`, scope.Kind, scope.Owner, tc.period)
			if err != nil {
				t.Fatalf("query episodes: %v", err)
			}
			defer epRows.Close()

			var sourceText strings.Builder
			for epRows.Next() {
				var id, ts string
				var inBlob, outBlob []byte
				if err := epRows.Scan(&id, &ts, &inBlob, &outBlob); err != nil {
					t.Fatalf("scan episode: %v", err)
				}
				in, err := enc.Decrypt(inBlob)
				if err != nil {
					t.Fatalf("decrypt input_text: %v", err)
				}
				out, err := enc.Decrypt(outBlob)
				if err != nil {
					t.Fatalf("decrypt output_text: %v", err)
				}
				fmt.Fprintf(&sourceText, "--- id: %s (date: %s) ---\nUSER: %s\nASSISTANT: %s\n\n", id, ts, in, out)
			}
			if err := epRows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			if sourceText.Len() == 0 {
				t.Skipf("no episodes found for %s/%s", tc.owner, tc.period)
			}
			t.Logf("%s: source text is %d runes", tc.name, len([]rune(sourceText.String())))

			real := provider.NewOpenAICompat(provider.OpenAICompatConfig{
				Name:    "live-gpt41",
				Vendor:  "openai",
				Model:   "gpt-4.1",
				BaseURL: "https://api.openai.com/v1",
				APIKey:  apiKey,
			})
			resp, err := real.ChatCompletion(context.Background(), provider.ChatRequest{
				Messages: []provider.Message{
					{Role: provider.RoleSystem, Content: groundingSystemPrompt},
					{Role: provider.RoleUser, Content: buildGroundingPrompt(sourceText.String(), facts)},
				},
			})
			if err != nil {
				t.Fatalf("ChatCompletion: %v", err)
			}
			t.Logf("%s: raw response:\n%s", tc.name, resp.Message.Content)

			var result struct {
				Grounded []bool `json:"grounded"`
			}
			if err := json.Unmarshal([]byte(extractJSON(resp.Message.Content)), &result); err != nil {
				t.Fatalf("%s: could not parse response: %v", tc.name, err)
			}
			t.Logf("%s: sent %d facts, got %d verdicts", tc.name, len(facts), len(result.Grounded))
			if len(result.Grounded) != len(facts) {
				t.Logf("REPRODUCED with real data (%s): sent %d facts, got %d verdicts", tc.name, len(facts), len(result.Grounded))
			}
		})
	}
}

func TestBuildGroundingPromptIncludesSourceAndNumberedFacts(t *testing.T) {
	prompt := buildGroundingPrompt("the source text", []KeyFactOutput{
		{Fact: "fact one"}, {Fact: "fact two"},
	})
	if !strings.Contains(prompt, "the source text") {
		t.Error("buildGroundingPrompt() missing source text")
	}
	if !strings.Contains(prompt, "1. fact one") || !strings.Contains(prompt, "2. fact two") {
		t.Errorf("buildGroundingPrompt() = %q, want numbered facts", prompt)
	}
}
