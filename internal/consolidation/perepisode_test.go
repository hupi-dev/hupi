package consolidation

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/crypto"
	"hupi/internal/identity"
	"hupi/internal/provider"
)

// fakeSequentialProvider returns responses[i] for the i-th ChatCompletion
// call (repeating the last one if called more times than len(responses))
// — extractPerEpisodeFacts_test needs a different canned response per
// episode, unlike fakeConsolidationProvider's single fixed response.
type fakeSequentialProvider struct {
	responses []string
	calls     int
}

func (*fakeSequentialProvider) Name() string   { return "fake-sequential" }
func (*fakeSequentialProvider) Vendor() string { return "fake" }
func (*fakeSequentialProvider) Model() string  { return "fake-model" }

func (f *fakeSequentialProvider) ChatCompletion(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	i := f.calls
	if i >= len(f.responses) {
		i = len(f.responses) - 1
	}
	f.calls++
	resp := f.responses[i]
	if resp == "" {
		return provider.ChatResponse{}, errors.New("fake: simulated provider error")
	}
	return provider.ChatResponse{Message: provider.Message{Role: provider.RoleAssistant, Content: resp}}, nil
}

func (*fakeSequentialProvider) StreamChatCompletion(context.Context, provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	return nil, errFakeNotImplemented
}

func (*fakeSequentialProvider) Embed(context.Context, provider.EmbedRequest) (provider.EmbedResponse, error) {
	return provider.EmbedResponse{}, errFakeNotImplemented
}

// TestExtractPerEpisodeFactsAttributesFactsToTheirOwnEpisode is Phase B
// option 2's real behavior (docs/CONSOLIDATION_COMPLETENESS_PLAN.md):
// each episode gets its own independent call, and any fact it produces
// is attributed to that specific episode, not conflated with another.
func TestExtractPerEpisodeFactsAttributesFactsToTheirOwnEpisode(t *testing.T) {
	fake := &fakeSequentialProvider{responses: []string{
		`{"facts": ["The user downloaded the Ibotta cashback app."]}`,
		`{"facts": []}`,
		`{"facts": ["The user's max home loan budget is now $300,000."]}`,
	}}
	runner := New(nil, nil, fake, nil, nil)

	sources := []textSource{
		{id: "ep_1", text: "irrelevant"},
		{id: "ep_2", text: "irrelevant"},
		{id: "ep_3", text: "irrelevant"},
	}
	facts := runner.extractPerEpisodeFacts(context.Background(), sources)

	if len(facts) != 2 {
		t.Fatalf("extractPerEpisodeFacts() returned %d facts, want 2 (ep_2 had none)", len(facts))
	}
	if facts[0].Fact != "The user downloaded the Ibotta cashback app." || len(facts[0].SourceEpisodeIDs) != 1 || facts[0].SourceEpisodeIDs[0] != "ep_1" {
		t.Errorf("facts[0] = %+v, want the ep_1 fact attributed to ep_1", facts[0])
	}
	if facts[1].Fact != "The user's max home loan budget is now $300,000." || len(facts[1].SourceEpisodeIDs) != 1 || facts[1].SourceEpisodeIDs[0] != "ep_3" {
		t.Errorf("facts[1] = %+v, want the ep_3 fact attributed to ep_3, not ep_2", facts[1])
	}
}

// TestExtractPerEpisodeFactsSkipsFailedOrMalformedEpisodesWithoutFailing
// confirms this pass is best-effort per episode, matching its own doc
// comment: one bad call must not lose every other episode's real facts.
func TestExtractPerEpisodeFactsSkipsFailedOrMalformedEpisodesWithoutFailing(t *testing.T) {
	fake := &fakeSequentialProvider{responses: []string{
		"",                                    // simulated provider error
		"not json at all",                     // malformed
		`{"facts": ["A real, recoverable fact."]}`,
	}}
	runner := New(nil, nil, fake, nil, nil)

	sources := []textSource{
		{id: "ep_error", text: "irrelevant"},
		{id: "ep_malformed", text: "irrelevant"},
		{id: "ep_good", text: "irrelevant"},
	}
	facts := runner.extractPerEpisodeFacts(context.Background(), sources)

	if len(facts) != 1 {
		t.Fatalf("extractPerEpisodeFacts() returned %d facts, want 1 (only ep_good), got: %+v", len(facts), facts)
	}
	if facts[0].SourceEpisodeIDs[0] != "ep_good" {
		t.Errorf("facts[0].SourceEpisodeIDs = %v, want [ep_good]", facts[0].SourceEpisodeIDs)
	}
}

func TestExtractPerEpisodeFactsEmptyInput(t *testing.T) {
	runner := New(nil, nil, &fakeSequentialProvider{}, nil, nil)
	if facts := runner.extractPerEpisodeFacts(context.Background(), nil); facts != nil {
		t.Errorf("extractPerEpisodeFacts(nil) = %+v, want nil", facts)
	}
}

// TestChunkTextShortTextReturnedAsSingleChunk confirms the common
// case — a real production episode, virtually always well under
// perEpisodeChunkCharLimit — is a no-op: exactly one chunk, unchanged.
func TestChunkTextShortTextReturnedAsSingleChunk(t *testing.T) {
	text := "a short episode exchange"
	chunks := chunkText(text, perEpisodeChunkCharLimit)
	if len(chunks) != 1 || chunks[0] != text {
		t.Errorf("chunkText() = %+v, want a single unchanged chunk", chunks)
	}
}

// TestChunkTextSplitsLongTextIntoBoundedWindows is the real regression
// test for 852ce960: a long episode (cmd/hupi-bench replaying an entire
// multi-turn LongMemEval session as one episode, ~11,600 characters in
// the real case) must be split into windows no larger than the limit,
// covering every rune exactly once.
func TestChunkTextSplitsLongTextIntoBoundedWindows(t *testing.T) {
	text := strings.Repeat("x", 10)
	chunks := chunkText(text, 3)
	want := []string{"xxx", "xxx", "xxx", "x"}
	if len(chunks) != len(want) {
		t.Fatalf("chunkText() = %+v (%d chunks), want %+v (%d chunks)", chunks, len(chunks), want, len(want))
	}
	for i := range want {
		if chunks[i] != want[i] {
			t.Errorf("chunkText()[%d] = %q, want %q", i, chunks[i], want[i])
		}
		if len(chunks[i]) > 3 {
			t.Errorf("chunkText()[%d] = %q, exceeds the limit of 3", i, chunks[i])
		}
	}
	rejoined := strings.Join(chunks, "")
	if rejoined != text {
		t.Errorf("chunks rejoined = %q, want the original text %q (no rune lost or duplicated)", rejoined, text)
	}
}

// TestChunkTextSplitsByRunesNotBytes confirms multi-byte characters
// aren't split mid-character — the same real bug class
// docs/CODEBASE_SURVEY_AND_REVIEW.md already fixed once for
// truncateToBudget/hardTruncate in internal/store, recurring here for a
// second, independent text-splitting function.
func TestChunkTextSplitsByRunesNotBytes(t *testing.T) {
	text := "héllo wörld" // contains multi-byte runes
	for _, limit := range []int{1, 2, 3, 4, 5, 100} {
		chunks := chunkText(text, limit)
		for _, c := range chunks {
			if !utf8.ValidString(c) {
				t.Errorf("chunkText(%q, %d) produced an invalid UTF-8 chunk %q", text, limit, c)
			}
		}
		if strings.Join(chunks, "") != text {
			t.Errorf("chunkText(%q, %d) rejoined != original", text, limit)
		}
	}
}

// TestExtractPerEpisodeFactsChunksLongEpisodesAndMergesResults is the
// real regression test for the live failure this chunking fix exists for
// (852ce960): a single long episode must be split into multiple
// extraction calls, each covering a bounded window, with every chunk's
// facts merged together and still attributed to the one real episode ID.
func TestExtractPerEpisodeFactsChunksLongEpisodesAndMergesResults(t *testing.T) {
	fake := &fakeSequentialProvider{responses: []string{
		`{"facts": ["Fact from chunk 1."]}`,
		`{"facts": ["Fact from chunk 2."]}`,
	}}
	runner := New(nil, nil, fake, nil, nil)

	longText := strings.Repeat("a", perEpisodeChunkCharLimit) + strings.Repeat("b", perEpisodeChunkCharLimit)
	sources := []textSource{{id: "ep_long", text: longText}}
	facts := runner.extractPerEpisodeFacts(context.Background(), sources)

	if fake.calls != 2 {
		t.Fatalf("got %d extraction calls, want 2 (one per chunk)", fake.calls)
	}
	if len(facts) != 2 {
		t.Fatalf("extractPerEpisodeFacts() returned %d facts, want 2 (one from each chunk)", len(facts))
	}
	for _, f := range facts {
		if len(f.SourceEpisodeIDs) != 1 || f.SourceEpisodeIDs[0] != "ep_long" {
			t.Errorf("fact %+v not attributed to ep_long", f)
		}
	}
}

// TestLivePerEpisodeFactPromptExtracts2014ConstructionMilestone is the
// real-data verification for the 5809eb10 fix (multi-milestone timeline
// extraction, see TestSummarySystemPromptAndPerEpisodeFactPromptCoverMultiMilestoneTimelines).
// Direct decryption of the real stored memory for this scope/day showed
// "construction began in 2014" was never extracted at all before this
// fix, even though the source episode states it in the same paragraph as
// the contract-signed (2015) and keys-received (2016) dates, both of
// which WERE extracted. This replays the exact real episode text through
// the real, updated perEpisodeFactPrompt against the real gpt-4.1
// grounding/consolidation profile to confirm the fix actually works, not
// just that the prompt contains the right words. Requires OPENAI_API_KEY,
// HUPI_DATABASE_URL, and HUPI_KEK pointed at hupi_sample6_fresh.
func TestLivePerEpisodeFactPromptExtracts2014ConstructionMilestone(t *testing.T) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	dbURL := os.Getenv("HUPI_DATABASE_URL")
	kek := os.Getenv("HUPI_KEK")
	if apiKey == "" || dbURL == "" || kek == "" {
		t.Skip("OPENAI_API_KEY/HUPI_DATABASE_URL/HUPI_KEK not all set; skipping live real-data extraction verification")
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
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:bench-longmemeval-hupi-5809eb10"}
	enc, _, err := keys.GetOrCreate(context.Background(), scope)
	if err != nil {
		t.Fatalf("GetOrCreate key: %v", err)
	}

	rows, err := db.QueryContext(context.Background(), `
		select id, input_text, output_text from episodes
		where scope_kind = $1 and scope_owner = $2
		  and type = 'interaction' and ts::date = '2023-05-30'
		order by ts`, scope.Kind, scope.Owner)
	if err != nil {
		t.Fatalf("query episodes: %v", err)
	}
	defer rows.Close()

	var bajimayaSource *textSource
	for rows.Next() {
		var id string
		var inBlob, outBlob []byte
		if err := rows.Scan(&id, &inBlob, &outBlob); err != nil {
			t.Fatalf("scan: %v", err)
		}
		in, err := enc.Decrypt(inBlob)
		if err != nil {
			t.Fatalf("decrypt input_text: %v", err)
		}
		out, err := enc.Decrypt(outBlob)
		if err != nil {
			t.Fatalf("decrypt output_text: %v", err)
		}
		if strings.Contains(in, "Bajimaya") || strings.Contains(out, "Bajimaya") {
			src := textSource{id: id, text: "USER: " + in + "\nASSISTANT: " + out}
			bajimayaSource = &src
			break
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if bajimayaSource == nil {
		t.Skip("no episode mentioning Bajimaya found for this scope/day — data may have changed")
	}
	t.Logf("found source episode %s, %d runes", bajimayaSource.id, len([]rune(bajimayaSource.text)))

	real := provider.NewOpenAICompat(provider.OpenAICompatConfig{
		Name:    "live-gpt41",
		Vendor:  "openai",
		Model:   "gpt-4.1",
		BaseURL: "https://api.openai.com/v1",
		APIKey:  apiKey,
	})
	runner := New(nil, nil, real, nil, nil)

	facts := runner.extractPerEpisodeFacts(context.Background(), []textSource{*bajimayaSource})
	t.Logf("extracted %d facts:", len(facts))
	found2014 := false
	for _, f := range facts {
		t.Logf("  - %s", f.Fact)
		if strings.Contains(f.Fact, "2014") {
			found2014 = true
		}
	}
	if !found2014 {
		t.Errorf("no extracted fact mentions 2014 (construction-began year) — the multi-milestone fix did not recover it on this run")
	}
}
