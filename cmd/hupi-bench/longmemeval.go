package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// LongMemEval's own date strings look like "2023/05/30 (Tue) 23:40" —
// confirmed against the real downloaded longmemeval_s_cleaned.json (see
// bench/FORMAT.md), not assumed from the README. Go's time.Parse doesn't
// validate the weekday abbreviation against the computed date, it's just
// consumed and discarded, so "(Tue)" doesn't need to actually be correct
// for the parse to succeed.
const longMemEvalDateLayout = "2006/01/02 (Mon) 15:04"

// longMemEvalRaw mirrors one of longmemeval_s_cleaned.json's 500 top-level
// evaluation instances. Unlike LoCoMo (one conversation, many sessions,
// many questions all sharing the same replayed history), LongMemEval is
// one instance per question: each question_id carries its own
// haystack_sessions, which may differ from another question's even
// though both datasets ultimately get cloned from the same underlying
// pool of sessions.
type longMemEvalRaw struct {
	QuestionID       string              `json:"question_id"`
	QuestionType     string              `json:"question_type"`
	Question         string              `json:"question"`
	QuestionDate     string              `json:"question_date"`
	Answer           interface{}         `json:"answer"` // defensive stringify — see loadLongMemEvalAll, mirrors LoCoMo's own answer-type gotcha
	HaystackDates    []string            `json:"haystack_dates"`
	HaystackSessions [][]longMemEvalTurn `json:"haystack_sessions"`
}

type longMemEvalTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// HasAnswer marks the evidence turn(s) LongMemEval's own turn-level
	// recall metric grades against — unused here, since this harness only
	// drives their unmodified QA scoring (evaluate_qa.py), not the
	// separate turn/session-level recall metrics documented in their
	// own src/evaluation/ (see docs/BENCHMARKS.md once Step 7 writes up
	// which of their several metrics this harness actually reports).
	HasAnswer bool `json:"has_answer,omitempty"`
}

// loadLongMemEvalAll parses every evaluation instance in a
// longmemeval_{s,m,oracle}_cleaned.json file into the same
// benchConversation shape the LoCoMo adapter produces — turn/session are
// already benchmark-agnostic (see locomo.go's own doc comments), so only
// the parsing is new here. Each instance becomes its own
// benchConversation with exactly one qaItem, since LongMemEval questions
// don't share a haystack the way LoCoMo's do.
func loadLongMemEvalAll(path string) ([]benchConversation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var raw []longMemEvalRaw
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse longmemeval json: %w", err)
	}

	convs := make([]benchConversation, len(raw))
	for i, e := range raw {
		conv := benchConversation{id: e.QuestionID}
		if len(e.HaystackSessions) != len(e.HaystackDates) {
			return nil, fmt.Errorf("instance %s: %d haystack_sessions but %d haystack_dates", e.QuestionID, len(e.HaystackSessions), len(e.HaystackDates))
		}
		for si, rawTurns := range e.HaystackSessions {
			date, err := time.Parse(longMemEvalDateLayout, strings.TrimSpace(e.HaystackDates[si]))
			if err != nil {
				return nil, fmt.Errorf("instance %s: parse haystack_dates[%d] %q: %w", e.QuestionID, si, e.HaystackDates[si], err)
			}
			sess := session{date: date}
			for _, t := range rawTurns {
				sess.turns = append(sess.turns, turn{speaker: t.Role, text: t.Content})
			}
			conv.sessions = append(conv.sessions, sess)
		}

		questionDate, err := time.Parse(longMemEvalDateLayout, strings.TrimSpace(e.QuestionDate))
		if err != nil {
			return nil, fmt.Errorf("instance %s: parse question_date %q: %w", e.QuestionID, e.QuestionDate, err)
		}
		conv.qa = []qaItem{{
			question:     e.Question,
			answer:       fmt.Sprintf("%v", e.Answer),
			questionType: e.QuestionType,
			id:           e.QuestionID,
			queryTime:    questionDate,
		}}
		convs[i] = conv
	}
	return convs, nil
}
