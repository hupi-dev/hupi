package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// LoCoMo's own session date/time strings look like "1:56 pm on 8 May,
// 2023" — confirmed against the real downloaded locomo10.json, not
// assumed from the paper/README (see bench/FORMAT.md).
const locomoDateLayout = "3:04 pm on 2 January, 2006"

// turn is one message within a session, common across both benchmark
// adapters (see longmemeval.go).
type turn struct {
	speaker string
	text    string
}

// session is one dated block of turns.
type session struct {
	date  time.Time
	turns []turn
}

// qaItem is one benchmark question to answer after all sessions have
// been replayed and consolidated.
type qaItem struct {
	question string
	answer   string // stringified — see the LoCoMo answer-type gotcha below
	category int    // LoCoMo only; 0 for LongMemEval (uses questionType instead)
	// questionType is LongMemEval's category dimension (single-session-user,
	// multi-session, temporal-reasoning, knowledge-update,
	// single-session-preference, single-session-assistant) — unused by
	// the LoCoMo adapter.
	questionType string
}

// benchConversation is the common shape both adapters (locomo.go,
// longmemeval.go — the latter added in a later step) produce, so
// replay.go never needs to know which benchmark it's replaying.
type benchConversation struct {
	id       string
	sessions []session
	qa       []qaItem
}

// locomoRaw mirrors locomo10.json's actual structure closely enough for
// json.Unmarshal — session_N/session_N_date_time pairs are handled via
// a raw map pass rather than named fields, since N ranges up to 35 and
// isn't fixed per conversation.
type locomoRaw struct {
	SampleID     string          `json:"sample_id"`
	Conversation json.RawMessage `json:"conversation"`
	QA           []locomoQA      `json:"qa"`
}

type locomoTurn struct {
	Speaker string `json:"speaker"`
	DiaID   string `json:"dia_id"`
	Text    string `json:"text"`
}

type locomoQA struct {
	Question string      `json:"question"`
	Answer   interface{} `json:"answer"` // sometimes a JSON number, not always a string — see bench/FORMAT.md
	Category int         `json:"category"`
	Evidence []string    `json:"evidence"`
}

var sessionKeyRe = regexp.MustCompile(`^session_(\d+)$`)

// loadLoCoMo parses locomo10.json into the common benchConversation shape.
// convIndex selects which of the 10 conversations to load (0-based) —
// Step 2 only needs one at a time to prove the harness mechanics; Step 4
// scales this up to all 10.
func loadLoCoMo(path string, convIndex int) (benchConversation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return benchConversation{}, fmt.Errorf("read %s: %w", path, err)
	}
	var raw []locomoRaw
	if err := json.Unmarshal(data, &raw); err != nil {
		return benchConversation{}, fmt.Errorf("parse locomo json: %w", err)
	}
	if convIndex < 0 || convIndex >= len(raw) {
		return benchConversation{}, fmt.Errorf("conversation index %d out of range (0-%d)", convIndex, len(raw)-1)
	}
	c := raw[convIndex]

	var convMap map[string]json.RawMessage
	if err := json.Unmarshal(c.Conversation, &convMap); err != nil {
		return benchConversation{}, fmt.Errorf("parse conversation object: %w", err)
	}

	// Collect session numbers present, then build each session in order —
	// session_N_date_time and session_N are siblings, not a nested pair,
	// per the real data's own shape (see bench/FORMAT.md).
	var sessionNums []int
	for key := range convMap {
		if m := sessionKeyRe.FindStringSubmatch(key); m != nil {
			var n int
			fmt.Sscanf(m[1], "%d", &n)
			sessionNums = append(sessionNums, n)
		}
	}
	sort.Ints(sessionNums)

	conv := benchConversation{id: c.SampleID}
	for _, n := range sessionNums {
		dateKey := fmt.Sprintf("session_%d_date_time", n)
		turnsKey := fmt.Sprintf("session_%d", n)

		var dateStr string
		if raw, ok := convMap[dateKey]; ok {
			if err := json.Unmarshal(raw, &dateStr); err != nil {
				return benchConversation{}, fmt.Errorf("parse %s: %w", dateKey, err)
			}
		}
		date, err := time.Parse(locomoDateLayout, strings.TrimSpace(dateStr))
		if err != nil {
			return benchConversation{}, fmt.Errorf("parse date %q for %s: %w", dateStr, dateKey, err)
		}

		var rawTurns []locomoTurn
		if raw, ok := convMap[turnsKey]; ok {
			if err := json.Unmarshal(raw, &rawTurns); err != nil {
				return benchConversation{}, fmt.Errorf("parse %s: %w", turnsKey, err)
			}
		}
		sess := session{date: date}
		for _, t := range rawTurns {
			sess.turns = append(sess.turns, turn{speaker: t.Speaker, text: t.Text})
		}
		conv.sessions = append(conv.sessions, sess)
	}

	for _, qa := range c.QA {
		conv.qa = append(conv.qa, qaItem{
			question: qa.Question,
			answer:   fmt.Sprintf("%v", qa.Answer), // defensive stringify — answer is sometimes a JSON number
			category: qa.Category,
		})
	}

	return conv, nil
}
