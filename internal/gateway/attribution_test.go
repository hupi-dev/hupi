package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"hupi/internal/provider"
)

// fakeJudge is a minimal provider.Provider whose ChatCompletion always
// returns a canned response — enough to test attributionCheck's parsing/
// defaulting logic without any real LLM call.
type fakeJudge struct {
	content string
	calls   int
}

func (f *fakeJudge) Name() string   { return "fake-judge" }
func (f *fakeJudge) Vendor() string { return "test" }
func (f *fakeJudge) Model() string  { return "fake-judge-model" }

func (f *fakeJudge) ChatCompletion(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	f.calls++
	return provider.ChatResponse{Message: provider.Message{Role: provider.RoleAssistant, Content: f.content}}, nil
}

func (f *fakeJudge) StreamChatCompletion(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	panic("not used by attributionCheck")
}

func (f *fakeJudge) Embed(ctx context.Context, req provider.EmbedRequest) (provider.EmbedResponse, error) {
	panic("not used by attributionCheck")
}

// TestBuildAttributionPrompt_MarksInferredSnippets is Phase 4 of
// docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md: a citation whose
// IsInference is a pointer to true gets a "[inferred] " prefix in the
// judge's numbered snippet list; an ordinary citation (IsInference nil
// or pointing to false) doesn't.
func TestBuildAttributionPrompt_MarksInferredSnippets(t *testing.T) {
	trueVal := true
	falseVal := false
	citations := []Citation{
		{Snippet: "Joanna likely has asthma.", IsInference: &trueVal},
		{Snippet: "Joanna went hiking.", IsInference: &falseVal},
		{Snippet: "Dana's rent is $2200."}, // IsInference nil — an attribute citation
	}
	got := buildAttributionPrompt("the answer", citations)
	if !strings.Contains(got, "1. [inferred] Joanna likely has asthma.") {
		t.Errorf("prompt missing the [inferred] prefix for the inferred citation, got:\n%s", got)
	}
	if strings.Contains(got, "[inferred] Joanna went hiking.") {
		t.Errorf("prompt incorrectly prefixed a non-inferred citation, got:\n%s", got)
	}
	if strings.Contains(got, "[inferred] Dana's rent") {
		t.Errorf("prompt incorrectly prefixed a nil-IsInference citation, got:\n%s", got)
	}
}

func TestAttributionCheck_ParsesUsedVerdicts(t *testing.T) {
	judge := &fakeJudge{content: `{"used": [true, false]}`}
	citations := []Citation{{Snippet: "a"}, {Snippet: "b"}}

	got, err := attributionCheck(context.Background(), judge, "the answer", citations)
	if err != nil {
		t.Fatalf("attributionCheck: %v", err)
	}
	if len(got) != 2 || got[0] != true || got[1] != false {
		t.Errorf("attributionCheck() = %v, want [true false]", got)
	}
}

// TestAttributionCheck_ToleratesMarkdownFence confirms extractJSON
// actually gets exercised — a real, observed model behavior (wrapping
// "JSON only" output in a code fence anyway).
func TestAttributionCheck_ToleratesMarkdownFence(t *testing.T) {
	judge := &fakeJudge{content: "```json\n{\"used\": [true]}\n```"}
	got, err := attributionCheck(context.Background(), judge, "the answer", []Citation{{Snippet: "a"}})
	if err != nil {
		t.Fatalf("attributionCheck: %v", err)
	}
	if len(got) != 1 || got[0] != true {
		t.Errorf("attributionCheck() = %v, want [true]", got)
	}
}

// TestAttributionCheck_ErrorsOnMismatchedCount is a real regression test
// (docs/CODEBASE_SURVEY_AND_REVIEW.md finding A2): this used to default
// every citation to "not used" on a mismatched verdict count, with a nil
// error — indistinguishable, from the caller's side, from a genuine
// "checked and confirmed every citation unused" result, contradicting
// Citation.Used's own documented nil-means-"not checked" contract. It
// must return an error instead, so the caller's existing nil-preserving
// error path is what actually runs.
func TestAttributionCheck_ErrorsOnMismatchedCount(t *testing.T) {
	judge := &fakeJudge{content: `{"used": [true]}`}
	citations := []Citation{{Snippet: "a"}, {Snippet: "b"}, {Snippet: "c"}}

	got, err := attributionCheck(context.Background(), judge, "the answer", citations)
	if err == nil {
		t.Fatalf("attributionCheck() = (%v, nil), want a non-nil error on a mismatched verdict count", got)
	}
	if got != nil {
		t.Errorf("attributionCheck() = %v, want nil on error", got)
	}
}

// TestAttributionCheck_ErrorsOnUnparsableResponse is the same regression
// as above, for a response with no valid JSON at all.
func TestAttributionCheck_ErrorsOnUnparsableResponse(t *testing.T) {
	judge := &fakeJudge{content: "I'm not sure how to answer that."}
	citations := []Citation{{Snippet: "a"}}

	got, err := attributionCheck(context.Background(), judge, "the answer", citations)
	if err == nil {
		t.Fatalf("attributionCheck() = (%v, nil), want a non-nil error on an unparsable response", got)
	}
	if got != nil {
		t.Errorf("attributionCheck() = %v, want nil on error", got)
	}
}

// batchCountingJudge returns one `true` verdict per citation actually
// present in that call's own prompt (counting numbered snippet lines),
// rather than a fixed-size canned response — so it works correctly
// regardless of how large a batch attributionCheck hands it, and
// batches records how many separate calls it was given.
type batchCountingJudge struct {
	batches [][]string // each call's own Candidate memory snippets lines, in order
}

func (j *batchCountingJudge) Name() string   { return "batch-counting-judge" }
func (j *batchCountingJudge) Vendor() string { return "test" }
func (j *batchCountingJudge) Model() string  { return "batch-counting-judge-model" }

func (j *batchCountingJudge) ChatCompletion(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	prompt := req.Messages[1].Content
	_, list, _ := strings.Cut(prompt, "Candidate memory snippets:\n")
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(list, "\n"), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	j.batches = append(j.batches, lines)
	used := make([]bool, len(lines))
	for i := range used {
		used[i] = true
	}
	b, _ := json.Marshal(struct {
		Used []bool `json:"used"`
	}{Used: used})
	return provider.ChatResponse{Message: provider.Message{Role: provider.RoleAssistant, Content: string(b)}}, nil
}

func (j *batchCountingJudge) StreamChatCompletion(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	panic("not used by attributionCheck")
}

func (j *batchCountingJudge) Embed(ctx context.Context, req provider.EmbedRequest) (provider.EmbedResponse, error) {
	panic("not used by attributionCheck")
}

// TestAttributionCheck_SplitsIntoBatchesAboveTheCap is the fact-
// granularity regression this cap exists for: a request with more
// citations than attributionCheckBatchSize must split into multiple
// judge calls, not send one oversized list — and the results must still
// line up index-for-index with the original citations slice regardless
// of the split.
func TestAttributionCheck_SplitsIntoBatchesAboveTheCap(t *testing.T) {
	n := attributionCheckBatchSize*2 + 3 // two full batches plus a partial one
	citations := make([]Citation, n)
	for i := range citations {
		citations[i] = Citation{Snippet: fmt.Sprintf("snippet %d", i)}
	}
	judge := &batchCountingJudge{}

	got, err := attributionCheck(context.Background(), judge, "the answer", citations)
	if err != nil {
		t.Fatalf("attributionCheck: %v", err)
	}
	if len(got) != n {
		t.Fatalf("got %d verdicts, want %d", len(got), n)
	}
	for i, v := range got {
		if !v {
			t.Errorf("verdict[%d] = false, want true", i)
		}
	}
	if len(judge.batches) != 3 {
		t.Fatalf("judge received %d calls, want 3 (two full batches of %d plus one of 3)", len(judge.batches), attributionCheckBatchSize)
	}
	if len(judge.batches[0]) != attributionCheckBatchSize || len(judge.batches[1]) != attributionCheckBatchSize || len(judge.batches[2]) != 3 {
		t.Errorf("batch sizes = %d, %d, %d — want %d, %d, 3", len(judge.batches[0]), len(judge.batches[1]), len(judge.batches[2]), attributionCheckBatchSize, attributionCheckBatchSize)
	}
	// Each batch's own prompt must renumber from 1, not continue the
	// global count — same per-batch-fresh-numbering convention
	// groundingCheck's own batching already uses.
	if !strings.HasPrefix(judge.batches[1][0], "1. ") {
		t.Errorf("second batch's first line = %q, want it renumbered starting at 1", judge.batches[1][0])
	}
}

func TestAttributionCheck_NoOpWithoutCitations(t *testing.T) {
	judge := &fakeJudge{content: `{"used": []}`}
	got, err := attributionCheck(context.Background(), judge, "the answer", nil)
	if err != nil {
		t.Fatalf("attributionCheck: %v", err)
	}
	if got != nil {
		t.Errorf("attributionCheck() = %v, want nil", got)
	}
	if judge.calls != 0 {
		t.Errorf("judge.calls = %d, want 0 (no LLM call for zero citations)", judge.calls)
	}
}
