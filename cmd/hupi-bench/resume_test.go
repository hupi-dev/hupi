package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadPriorAnswers_NoOutFile confirms first-run behavior (no
// -out-file given, or none exists yet) is a no-op resume, not an error.
func TestLoadPriorAnswers_NoOutFile(t *testing.T) {
	convs := []benchConversation{{id: "conv-1", qa: []qaItem{{question: "q"}}}}

	got, err := loadPriorAnswers("locomo", "", convs)
	if err != nil || got != nil {
		t.Fatalf("empty -out-file: got (%v, %v), want (nil, nil)", got, err)
	}

	got, err = loadPriorAnswers("locomo", filepath.Join(t.TempDir(), "missing.json"), convs)
	if err != nil || got != nil {
		t.Fatalf("nonexistent -out-file: got (%v, %v), want (nil, nil)", got, err)
	}
}

func TestLoadPriorAnswers_LongMemEval(t *testing.T) {
	convs := []benchConversation{
		{id: "complete-conv", qa: []qaItem{{id: "q1", question: "a"}, {id: "q2", question: "b"}}},
		{id: "partial-conv", qa: []qaItem{{id: "q3", question: "c"}, {id: "q4", question: "d"}}},
		{id: "untouched-conv", qa: []qaItem{{id: "q5", question: "e"}}},
	}

	// q1/q2 both answered (complete-conv done); q3 answered but q4 has an
	// empty hypothesis (the sendChatTurn-failure marker) so partial-conv
	// must NOT count as complete; q5 never appears at all (untouched-conv).
	content := `{"question_id":"q1","hypothesis":"answer one"}
{"question_id":"q2","hypothesis":"answer two"}
{"question_id":"q3","hypothesis":"answer three"}
{"question_id":"q4","hypothesis":""}
`
	path := filepath.Join(t.TempDir(), "predictions.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := loadPriorAnswers("longmemeval", path, convs)
	if err != nil {
		t.Fatalf("loadPriorAnswers: %v", err)
	}

	if answers, ok := got["complete-conv"]; !ok || len(answers) != 2 || answers[0] != "answer one" || answers[1] != "answer two" {
		t.Errorf("complete-conv: got %v, want [answer one, answer two]", answers)
	}
	if _, ok := got["partial-conv"]; ok {
		t.Errorf("partial-conv: should not be marked complete (q4 has an empty/failed answer)")
	}
	if _, ok := got["untouched-conv"]; ok {
		t.Errorf("untouched-conv: should not be marked complete (never appears in the prior file)")
	}
}

func TestLoadPriorAnswers_LoCoMo(t *testing.T) {
	convs := []benchConversation{
		{id: "conv-26", qa: []qaItem{{question: "a"}, {question: "b"}}},
		{id: "conv-30", qa: []qaItem{{question: "c"}, {question: "d"}}},
		{id: "conv-41", qa: []qaItem{{question: "e"}}},
	}

	// conv-26 fully answered; conv-30 has one empty prediction (failed
	// question, must not count as complete); conv-41 never appears.
	content := `[
	  {"sample_id": "conv-26", "qa": [{"hupi_prediction": "ans1"}, {"hupi_prediction": "ans2"}]},
	  {"sample_id": "conv-30", "qa": [{"hupi_prediction": "ans3"}, {"hupi_prediction": ""}]}
	]`
	path := filepath.Join(t.TempDir(), "predictions.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := loadPriorAnswers("locomo", path, convs)
	if err != nil {
		t.Fatalf("loadPriorAnswers: %v", err)
	}

	if answers, ok := got["conv-26"]; !ok || len(answers) != 2 || answers[0] != "ans1" || answers[1] != "ans2" {
		t.Errorf("conv-26: got %v, want [ans1, ans2]", answers)
	}
	if _, ok := got["conv-30"]; ok {
		t.Errorf("conv-30: should not be marked complete (one empty/failed prediction)")
	}
	if _, ok := got["conv-41"]; ok {
		t.Errorf("conv-41: should not be marked complete (never appears in the prior file)")
	}
}
