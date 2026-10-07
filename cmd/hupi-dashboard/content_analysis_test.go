package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

func TestLocalKeywordThemes_DropsStopwordsAndSingleCharTokensRanksByFrequency(t *testing.T) {
	text := "the pipeline refactor was discussed. the refactor took a long time. pipeline work continues. a i o"
	terms := localKeywordThemes(text, 10)

	byTerm := map[string]int{}
	for _, tf := range terms {
		byTerm[tf.Term] = tf.Count
	}
	if byTerm["the"] != 0 {
		t.Errorf("stopword %q leaked into terms: %+v", "the", terms)
	}
	if byTerm["a"] != 0 || byTerm["i"] != 0 || byTerm["o"] != 0 {
		t.Errorf("single-character tokens leaked into terms: %+v", terms)
	}
	if byTerm["pipeline"] != 2 {
		t.Errorf("pipeline count = %d, want 2", byTerm["pipeline"])
	}
	if byTerm["refactor"] != 2 {
		t.Errorf("refactor count = %d, want 2", byTerm["refactor"])
	}
	if len(terms) < 2 || terms[0].Count < terms[len(terms)-1].Count {
		t.Errorf("terms not sorted by descending count: %+v", terms)
	}
}

func TestLocalKeywordThemes_RespectsTopNLimit(t *testing.T) {
	terms := localKeywordThemes("alpha beta gamma delta epsilon zeta eta theta", 3)
	if len(terms) != 3 {
		t.Errorf("got %d terms, want exactly 3 (topN)", len(terms))
	}
}

// insertEpisodeWithText seeds a real encrypted episode — the encryption
// key actually resolved through enc, not a raw placeholder, so
// decryptRecentText's own keys.GetVersion call can round-trip it.
func insertEpisodeWithText(t *testing.T, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, id string, ts time.Time, inputText, outputText string) {
	t.Helper()
	enc, keyVersion, err := keys.GetOrCreate(context.Background(), scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	inputCT, err := enc.Encrypt(inputText)
	if err != nil {
		t.Fatalf("encrypt test episode input: %v", err)
	}
	outputCT, err := enc.Encrypt(outputText)
	if err != nil {
		t.Fatalf("encrypt test episode output: %v", err)
	}
	err = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into episodes (id, ts, type, input_text, output_text, hash, key_version, scope_kind, scope_owner)
			values ($1, $2, 'interaction', $3, $4, $5, $6, $7, $8)
		`, id, ts, inputCT, outputCT, hash(id), keyVersion, scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("insert test episode %s: %v", id, err)
	}
}

func TestDecryptRecentText_DecryptsWithinWindowOnly(t *testing.T) {
	db := testDB(t)
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-dashboard-content-analysis"}
	t.Cleanup(func() { cleanupScope(t, db, scope) })

	keys := crypto.NewKeyStore(db, make([]byte, 32)) // all-zero test KEK, never used for real data
	now := time.Now().UTC()
	insertEpisodeWithText(t, db, keys, scope, "ep_ca_recent", now.Add(-1*24*time.Hour), "discussing the zorbathon pipeline", "noted")
	insertEpisodeWithText(t, db, keys, scope, "ep_ca_old", now.Add(-90*24*time.Hour), "an entirely different topic", "noted")

	text, err := decryptRecentText(context.Background(), db, keys, scope, 30, defaultContentAnalysisMaxChars)
	if err != nil {
		t.Fatalf("decryptRecentText: %v", err)
	}
	if !strings.Contains(text, "zorbathon") {
		t.Errorf("decrypted text missing the recent episode's content, got: %q", text)
	}
	if strings.Contains(text, "entirely different topic") {
		t.Errorf("decrypted text includes the out-of-window episode, got: %q", text)
	}
}

func TestDecryptRecentText_RespectsMaxChars(t *testing.T) {
	db := testDB(t)
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-dashboard-content-analysis-cap"}
	t.Cleanup(func() { cleanupScope(t, db, scope) })

	keys := crypto.NewKeyStore(db, make([]byte, 32))
	now := time.Now().UTC()
	insertEpisodeWithText(t, db, keys, scope, "ep_ca_cap", now, strings.Repeat("word ", 1000), "")

	text, err := decryptRecentText(context.Background(), db, keys, scope, 30, 50)
	if err != nil {
		t.Fatalf("decryptRecentText: %v", err)
	}
	if len(text) > 50 {
		t.Errorf("decrypted text length = %d, want capped at 50", len(text))
	}
}

func TestDecryptConversations_ReturnsOneEntryPerEpisodeWithinIDList(t *testing.T) {
	db := testDB(t)
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-dashboard-mm-decrypt-convs"}
	t.Cleanup(func() { cleanupScope(t, db, scope) })

	keys := crypto.NewKeyStore(db, make([]byte, 32))
	now := time.Now().UTC()
	insertEpisodeWithText(t, db, keys, scope, "ep_wanted", now, "discussing the zorbathon pipeline", "noted")
	insertEpisodeWithText(t, db, keys, scope, "ep_not_requested", now, "an entirely different topic", "noted")

	contents, err := decryptConversations(context.Background(), db, keys, scope, []string{"ep_wanted"}, 2000, 200_000)
	if err != nil {
		t.Fatalf("decryptConversations: %v", err)
	}
	if len(contents) != 1 || contents[0].EpisodeID != "ep_wanted" {
		t.Fatalf("contents = %+v, want exactly one entry for ep_wanted", contents)
	}
	if !strings.Contains(contents[0].Text, "zorbathon") {
		t.Errorf("decrypted text missing expected content, got: %q", contents[0].Text)
	}
}

func TestDecryptConversations_RespectsPerConversationAndTotalCharCaps(t *testing.T) {
	db := testDB(t)
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-dashboard-mm-decrypt-caps"}
	t.Cleanup(func() { cleanupScope(t, db, scope) })

	keys := crypto.NewKeyStore(db, make([]byte, 32))
	now := time.Now().UTC()
	insertEpisodeWithText(t, db, keys, scope, "ep_long_1", now, strings.Repeat("word ", 1000), "")
	insertEpisodeWithText(t, db, keys, scope, "ep_long_2", now.Add(-time.Minute), strings.Repeat("word ", 1000), "")

	// Per-conversation cap: each row individually capped at 30 chars.
	contents, err := decryptConversations(context.Background(), db, keys, scope, []string{"ep_long_1", "ep_long_2"}, 30, 200_000)
	if err != nil {
		t.Fatalf("decryptConversations: %v", err)
	}
	if len(contents) != 2 {
		t.Fatalf("got %d entries, want 2", len(contents))
	}
	for _, c := range contents {
		if len(c.Text) > 30 {
			t.Errorf("episode %s text length = %d, want capped at 30", c.EpisodeID, len(c.Text))
		}
	}

	// Total budget cap: second row should be dropped entirely once the
	// first row alone exceeds maxTotalChars.
	contents, err = decryptConversations(context.Background(), db, keys, scope, []string{"ep_long_1", "ep_long_2"}, 2000, 100)
	if err != nil {
		t.Fatalf("decryptConversations: %v", err)
	}
	if len(contents) != 1 {
		t.Errorf("got %d entries, want exactly 1 once the total budget is exhausted by the first row: %+v", len(contents), contents)
	}
}

func TestAggregateConversationTopics_SumsAcrossConversationsAndRanksByFrequency(t *testing.T) {
	perConv := []ConversationTopics{
		{EpisodeID: "ep_1", Topics: []TermFrequency{{Term: "pipeline", Count: 2}, {Term: "refactor", Count: 1}}},
		{EpisodeID: "ep_2", Topics: []TermFrequency{{Term: "pipeline", Count: 3}, {Term: "launch", Count: 5}}},
	}
	terms := aggregateConversationTopics(perConv, 10)

	byTerm := map[string]int{}
	for _, t := range terms {
		byTerm[t.Term] = t.Count
	}
	if byTerm["pipeline"] != 5 {
		t.Errorf("pipeline total = %d, want 5 (summed across both conversations)", byTerm["pipeline"])
	}
	if byTerm["launch"] != 5 || byTerm["refactor"] != 1 {
		t.Errorf("terms = %+v, want launch=5 refactor=1", terms)
	}
	if len(terms) < 2 || terms[0].Count < terms[1].Count {
		t.Errorf("terms not sorted by descending count: %+v", terms)
	}
}

func TestAggregateConversationTopics_RespectsTopNLimit(t *testing.T) {
	perConv := []ConversationTopics{
		{EpisodeID: "ep_1", Topics: []TermFrequency{{Term: "a", Count: 1}, {Term: "b", Count: 1}, {Term: "c", Count: 1}}},
	}
	terms := aggregateConversationTopics(perConv, 2)
	if len(terms) != 2 {
		t.Errorf("got %d terms, want exactly 2 (topN)", len(terms))
	}
}
