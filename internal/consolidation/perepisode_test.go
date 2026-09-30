package consolidation

import (
	"context"
	"errors"
	"testing"

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
