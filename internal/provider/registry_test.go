package provider

import "testing"

func testProfile() ProfileConfig {
	return ProfileConfig{Kind: KindOpenAICompat, Vendor: "openai", Model: "gpt-4.1", BaseURL: "https://example.invalid"}
}

// TestRegistry_VisionDefaultsToChatProvider mirrors Grounding's own
// optional-with-fallback pattern: leaving active_vision_provider unset
// in providers.yaml should reuse the chat profile, not fail to start.
func TestRegistry_VisionDefaultsToChatProvider(t *testing.T) {
	reg, err := NewRegistry(Config{
		ActiveChatProvider:          "main",
		ActiveConsolidationProvider: "main",
		ActiveEmbeddingProvider:     "main",
		Providers:                   map[string]ProfileConfig{"main": testProfile()},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if reg.Vision() != reg.Chat() {
		t.Error("Vision() with no active_vision_provider set should default to Chat()'s profile")
	}
}

// TestRegistry_VisionUsesConfiguredProviderWhenSet confirms a distinct
// active_vision_provider is honored rather than always falling back.
func TestRegistry_VisionUsesConfiguredProviderWhenSet(t *testing.T) {
	reg, err := NewRegistry(Config{
		ActiveChatProvider:          "chat",
		ActiveConsolidationProvider: "chat",
		ActiveEmbeddingProvider:     "chat",
		ActiveVisionProvider:        "vision",
		Providers: map[string]ProfileConfig{
			"chat":   testProfile(),
			"vision": testProfile(),
		},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if reg.Vision() == reg.Chat() {
		t.Error("Vision() with active_vision_provider set to a distinct profile should not equal Chat()")
	}
	want, _ := reg.Named("vision")
	if reg.Vision() != want {
		t.Error("Vision() should return the profile named by active_vision_provider")
	}
}

// TestRegistry_RejectsUnknownVisionProvider mirrors the existing
// validation for every other role: an active_vision_provider pointing
// at an undefined profile must fail to start, not silently no-op.
func TestRegistry_RejectsUnknownVisionProvider(t *testing.T) {
	_, err := NewRegistry(Config{
		ActiveChatProvider:          "main",
		ActiveConsolidationProvider: "main",
		ActiveEmbeddingProvider:     "main",
		ActiveVisionProvider:        "does-not-exist",
		Providers:                   map[string]ProfileConfig{"main": testProfile()},
	})
	if err == nil {
		t.Error("NewRegistry() with an undefined active_vision_provider = nil error, want a validation failure")
	}
}
