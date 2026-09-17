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
		return EmbedResponse{Vectors: [][]float32{fill(req.Dimensions, 1)}}, nil
	}
	return EmbedResponse{Vectors: [][]float32{fill(f.nativeLen, 1)}}, nil
}

// fill builds a vector of n copies of v — using a non-zero value (instead
// of Go's zero-valued default) so padding tests can tell "the model's
// real output" apart from "zeros appended by dimensionPadded" by content,
// not just by length.
func fill(n int, v float32) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = v
	}
	return out
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

// TestVerifyEmbeddingDimensions_PadsWhenNativeIsSmaller is the common
// local-model case: something like nomic-embed-text (768 dims) or
// bge-large-en/mxbai-embed-large (1024 dims) has no truncation knob at
// all, but is shorter than EmbeddingDimensions rather than longer —
// verification must zero-pad it instead of trying (and failing) an
// explicit dimensions request that no provider offers for *upsampling*.
func TestVerifyEmbeddingDimensions_PadsWhenNativeIsSmaller(t *testing.T) {
	p := fakeProvider{nativeLen: 768, rejectsDimensions: true}
	got, err := VerifyEmbeddingDimensions(context.Background(), p)
	if err != nil {
		t.Fatalf("VerifyEmbeddingDimensions: %v", err)
	}
	if _, wrapped := got.(dimensionPadded); !wrapped {
		t.Errorf("expected the provider to be wrapped in dimensionPadded, got %T", got)
	}
	resp, err := got.Embed(context.Background(), EmbedRequest{Input: []string{"x"}})
	if err != nil {
		t.Fatalf("Embed after verify: %v", err)
	}
	v := resp.Vectors[0]
	if len(v) != EmbeddingDimensions {
		t.Fatalf("got %d dims, want %d", len(v), EmbeddingDimensions)
	}
	for i := 0; i < 768; i++ {
		if v[i] != 1 {
			t.Fatalf("v[%d] = %v, want the model's real output (1) — padding must not touch the original values", i, v[i])
		}
	}
	for i := 768; i < EmbeddingDimensions; i++ {
		if v[i] != 0 {
			t.Fatalf("v[%d] = %v, want 0 — everything past the model's native length must be zero-padded", i, v[i])
		}
	}
}

// TestVerifyEmbeddingDimensions_FailsWhenNeitherPathMatches covers a model
// that's simply unusable: native output *longer* than required (so
// padding doesn't apply), and it rejects an explicit dimensions request
// too. Nothing should paper over this by guessing at a blind truncation.
func TestVerifyEmbeddingDimensions_FailsWhenNeitherPathMatches(t *testing.T) {
	p := fakeProvider{nativeLen: 3072, rejectsDimensions: true}
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
