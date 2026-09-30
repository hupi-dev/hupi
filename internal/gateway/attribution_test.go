package gateway

import (
	"context"
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

// TestAttributionCheck_DefaultsToUnusedOnMismatchedCount mirrors
// groundingCheck's own safe-degrade direction: a judge response with the
// wrong number of verdicts shouldn't fail the whole request, just
// default every citation to "not used."
func TestAttributionCheck_DefaultsToUnusedOnMismatchedCount(t *testing.T) {
	judge := &fakeJudge{content: `{"used": [true]}`}
	citations := []Citation{{Snippet: "a"}, {Snippet: "b"}, {Snippet: "c"}}

	got, err := attributionCheck(context.Background(), judge, "the answer", citations)
	if err != nil {
		t.Fatalf("attributionCheck: %v", err)
	}
	if len(got) != 3 || got[0] || got[1] || got[2] {
		t.Errorf("attributionCheck() = %v, want [false false false]", got)
	}
}

// TestAttributionCheck_DefaultsToUnusedOnUnparsableResponse mirrors
// groundingCheck's own behavior for a response with no valid JSON at
// all.
func TestAttributionCheck_DefaultsToUnusedOnUnparsableResponse(t *testing.T) {
	judge := &fakeJudge{content: "I'm not sure how to answer that."}
	citations := []Citation{{Snippet: "a"}}

	got, err := attributionCheck(context.Background(), judge, "the answer", citations)
	if err != nil {
		t.Fatalf("attributionCheck: %v", err)
	}
	if len(got) != 1 || got[0] {
		t.Errorf("attributionCheck() = %v, want [false]", got)
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
