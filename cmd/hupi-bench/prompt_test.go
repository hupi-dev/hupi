package main

import (
	"testing"

	"hupi/internal/qaprompt"
)

// TestAnswerPromptForUsesSharedConcisePrompt confirms answerPromptFor
// still selects qaprompt.Concise for every question type except
// single-session-preference, after moving that prompt out of this
// package and into internal/qaprompt (shared with
// cmd/hupi-answer-question) — guards against the move accidentally
// changing which prompt gets selected.
func TestAnswerPromptForUsesSharedConcisePrompt(t *testing.T) {
	if got := answerPromptFor(qaItem{questionType: "temporal-reasoning"}); got != qaprompt.Concise {
		t.Errorf("answerPromptFor(temporal-reasoning) did not return qaprompt.Concise")
	}
	if got := answerPromptFor(qaItem{}); got != qaprompt.Concise {
		t.Errorf("answerPromptFor(no question type, e.g. LoCoMo) did not return qaprompt.Concise")
	}
	if got := answerPromptFor(qaItem{questionType: "single-session-preference"}); got != preferenceAnswerPrompt {
		t.Errorf("answerPromptFor(single-session-preference) did not return preferenceAnswerPrompt")
	}
}
