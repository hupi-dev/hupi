package provider

import (
	"context"
	"errors"
	"testing"
)

// fakeProvider simulates the handful of real behaviors
// VerifyEmbeddingDimensions has to tell apart — verified against OpenAI's
// actual API before writing this: ada-002 rejects `dimensions` outright
// even at its own native size, while text-embedding-3-* honors it.
type fakeProvider struct {
	nativeLen         int
	rejectsDimensions bool
	alwaysErrors      bool
}

func (f fakeProvider) Name() string   { return "fake" }
func (f fakeProvider) Vendor() string { return "fake-vendor" }
func (f fakeProvider) Model() string  { return "fake-model" }

func (f fakeProvider) ChatCompletion(context.Context, ChatRequest) (ChatResponse, error) {
	return ChatResponse{}, errors.New("fakeProvider: not implemented, not needed by these tests")
}

func (f fakeProvider) StreamChatCompletion(context.Context, ChatRequest) (<-chan StreamChunk, error) {
	return nil, errors.New("fakeProvider: not implemented, not needed by these tests")
}

func (f fakeProvider) Embed(_ context.Context, req EmbedRequest) (EmbedResponse, error) {
	if f.alwaysErrors {
		return EmbedResponse{}, errors.New("fakeProvider: embeddings not supported")
	}
	if req.Dimensions > 0 {
		if f.rejectsDimensions {
			return EmbedResponse{}, errors.New("This model does not support specifying dimensions.")
		}
		return EmbedResponse{Vectors: [][]float32{make([]float32, req.Dimensions)}}, nil
	}
	return EmbedResponse{Vectors: [][]float32{make([]float32, f.nativeLen)}}, nil
}

// TestVerifyEmbeddingDimensions_NativeMatchSkipsWrapping is the ada-002
// case: native output already the right length, and it errors if asked
// for dimensions explicitly — verification must succeed via the native
// path and never even need the fallback.
func TestVerifyEmbeddingDimensions_NativeMatchSkipsWrapping(t *testing.T) {
	p := fakeProvider{nativeLen: EmbeddingDimensions, rejectsDimensions: true}
	got, err := VerifyEmbeddingDimensions(context.Background(), p)
	if err != nil {
		t.Fatalf("VerifyEmbeddingDimensions: %v", err)
	}
	if _, wrapped := got.(dimensionPinned); wrapped {
		t.Error("expected the original provider back unwrapped when native output already matches — no need to ever request dimensions from a model that would reject it")
	}
	resp, err := got.Embed(context.Background(), EmbedRequest{Input: []string{"x"}})
	if err != nil {
		t.Fatalf("Embed after verify: %v", err)
	}
	if len(resp.Vectors[0]) != EmbeddingDimensions {
		t.Errorf("got %d dims, want %d", len(resp.Vectors[0]), EmbeddingDimensions)
	}
}

// TestVerifyEmbeddingDimensions_WrapsWhenTruncationNeeded is the
// text-embedding-3-large case: native output is larger, but it honors an
// explicit dimensions request — verification must wrap the provider so
// a caller that never sets Dimensions itself still gets the right length.
func TestVerifyEmbeddingDimensions_WrapsWhenTruncationNeeded(t *testing.T) {
	p := fakeProvider{nativeLen: 3072}
	got, err := VerifyEmbeddingDimensions(context.Background(), p)
	if err != nil {
		t.Fatalf("VerifyEmbeddingDimensions: %v", err)
	}
	if _, wrapped := got.(dimensionPinned); !wrapped {
		t.Error("expected the provider to be wrapped since native output needed truncation")
	}
	resp, err := got.Embed(context.Background(), EmbedRequest{Input: []string{"x"}})
	if err != nil {
		t.Fatalf("Embed after verify: %v", err)
	}
	if len(resp.Vectors[0]) != EmbeddingDimensions {
		t.Errorf("got %d dims, want %d — the wrapper should inject Dimensions even though the caller didn't set it", len(resp.Vectors[0]), EmbeddingDimensions)
	}
}

// TestVerifyEmbeddingDimensions_FailsWhenNeitherPathMatches covers a model
// that's simply unusable: wrong native length, and it rejects an explicit
// dimensions request too. Nothing should paper over this.
func TestVerifyEmbeddingDimensions_FailsWhenNeitherPathMatches(t *testing.T) {
	p := fakeProvider{nativeLen: 768, rejectsDimensions: true}
	if _, err := VerifyEmbeddingDimensions(context.Background(), p); err == nil {
		t.Fatal("expected an error, got none")
	}
}

// TestVerifyEmbeddingDimensions_FailsWhenEmbedNotSupportedAtAll covers a
// chat-only profile (e.g. Anthropic) misconfigured as active_embedding_provider
// — Embed itself always errors, and verification must surface that clearly
// rather than treat it as "0-dimension, try the other path."
func TestVerifyEmbeddingDimensions_FailsWhenEmbedNotSupportedAtAll(t *testing.T) {
	p := fakeProvider{alwaysErrors: true}
	if _, err := VerifyEmbeddingDimensions(context.Background(), p); err == nil {
		t.Fatal("expected an error, got none")
	}
}
