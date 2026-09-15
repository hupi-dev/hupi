package gateway

import (
	"context"
	"errors"
	"testing"

	"hupi/internal/provider"
)

var errEpisodeTestNotImplemented = errors.New("fakeProvider: not implemented, not needed by this test")

type fakeProvider struct{}

func (fakeProvider) Name() string   { return "fake" }
func (fakeProvider) Vendor() string { return "fake-vendor" }
func (fakeProvider) Model() string  { return "fake-model" }

func (fakeProvider) ChatCompletion(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{}, errEpisodeTestNotImplemented
}

func (fakeProvider) StreamChatCompletion(context.Context, provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	return nil, errEpisodeTestNotImplemented
}

func (fakeProvider) Embed(context.Context, provider.EmbedRequest) (provider.EmbedResponse, error) {
	return provider.EmbedResponse{}, errEpisodeTestNotImplemented
}

// TestBuildEpisode_SetsActorUserID is a regression test for
// docs/GAP_CLOSURE_PLAN.md §4.3's audit hooks: a team-routed capture needs
// to know which individual member sent the message, distinct from the
// team scope it's written into (internal/store.Capture reads exactly this
// field — see internal/store/audit_test.go's
// TestCapture_AuditActorIsIndividualNotWorkspace for the write side).
func TestBuildEpisode_SetsActorUserID(t *testing.T) {
	h := &Handler{}
	ep := h.buildEpisode("ep1", fakeProvider{}, "input", "output", RetrievalResult{Gate: GateSkipped}, false, "user:alice")
	if ep.ActorUserID != "user:alice" {
		t.Errorf("ActorUserID = %q, want %q", ep.ActorUserID, "user:alice")
	}
}
