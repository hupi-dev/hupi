package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestNewInteractionEpisode_FillsCaptureFields(t *testing.T) {
	ts := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	ep := NewInteractionEpisode("ep_x", ts, "mcp", "remember", "I always use pnpm", "")
	if ep.ID != "ep_x" || !ep.TS.Equal(ts) || ep.Type != "interaction" || ep.ProviderVendor != "mcp" || ep.ProviderModel != "remember" {
		t.Errorf("identity fields wrong: %+v", ep)
	}
	if ep.Hash != hashText("I always use pnpm\x00") {
		t.Errorf("hash = %q, want hash of input+NUL+output", ep.Hash)
	}
	if ep.Importance != estimateImportance("I always use pnpm", "") {
		t.Errorf("importance = %v, want the shared heuristic", ep.Importance)
	}
}

func TestNewEpisodeID_IsPrefixedAndTimeSortable(t *testing.T) {
	a := NewEpisodeID(time.UnixMilli(1000))
	b := NewEpisodeID(time.UnixMilli(2000))
	if !strings.HasPrefix(a, "ep_") || a >= b {
		t.Errorf("ids not prefixed/sortable: %q, %q", a, b)
	}
}

func TestBearerToken_RequiresExactPrefix(t *testing.T) {
	for _, tc := range []struct{ header, want string }{
		{"Bearer abc", "abc"},
		{"bearer abc", ""},
		{"Basic abc", ""},
		{"", ""},
	} {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if tc.header != "" {
			r.Header.Set("Authorization", tc.header)
		}
		if got := BearerToken(r); got != tc.want {
			t.Errorf("BearerToken(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}
