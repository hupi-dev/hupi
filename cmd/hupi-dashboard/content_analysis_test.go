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
