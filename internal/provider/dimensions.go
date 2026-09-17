package provider

import (
	"context"
	"fmt"
)

// dimensionPinned wraps a Provider and asks it to truncate every Embed
// call to a fixed dimension count — see VerifyEmbeddingDimensions, which
// is the only thing that constructs one.
type dimensionPinned struct {
	Provider
	dimensions int
}

func (d dimensionPinned) Embed(ctx context.Context, req EmbedRequest) (EmbedResponse, error) {
	req.Dimensions = d.dimensions
	return d.Provider.Embed(ctx, req)
}

// dimensionPadded wraps a Provider whose native output is shorter than
// the target dimension count and right-pads every returned vector with
// zeros to reach it — see VerifyEmbeddingDimensions, which is the only
// thing that constructs one.
//
// This is exact, not an approximation: cosine similarity (what pgvector's
// `<=>` operator computes) is invariant to appending an equal-length run
// of zeros to both operands — the padding contributes nothing to either
// vector's dot product or norm, since every term it adds is zero. Two
// vectors padded this way compare identically to how their un-padded
// originals would, regardless of what model produced them or how it was
// trained. That's what makes this safe to apply generically to any
// too-short model, unlike truncating a too-*long* one (dimensionPinned's
// explicit-request path, or a hypothetical blind-slice fallback) — the
// latter is only valid for a model specifically trained to keep its
// meaning front-loaded (Matryoshka representation learning), which isn't
// something this package can verify from the outside.
type dimensionPadded struct {
	Provider
	dimensions int
}

func (d dimensionPadded) Embed(ctx context.Context, req EmbedRequest) (EmbedResponse, error) {
	resp, err := d.Provider.Embed(ctx, req)
	if err != nil {
		return EmbedResponse{}, err
	}
	for i, v := range resp.Vectors {
		if len(v) >= d.dimensions {
			continue
		}
		padded := make([]float32, d.dimensions)
		copy(padded, v)
		resp.Vectors[i] = padded
	}
	return resp, nil
}

// VerifyEmbeddingDimensions checks that p actually produces
// EmbeddingDimensions-length vectors, and returns the Provider a caller
// should use in its place from now on. This exists because every
// embedding column in the schema is a fixed vector(1536)
// (EmbeddingDimensions' doc comment) and pgvector hard-rejects any other
// length at insert time — without this check, pointing
// active_embedding_provider at a model with a different native output
// size wouldn't fail until the next real consolidation run or
// hupi-reembed batch tried to write a vector, deep into an unrelated
// operation.
//
// Up to three real embed calls, tried in this order:
//
//  1. Native output, no dimensions requested. Plenty of models (OpenAI's
//     own ada-002 among them) have no truncation knob at all and simply
//     always produce one fixed size — if that size already happens to be
//     EmbeddingDimensions, nothing else is needed.
//  2. If native output is *shorter* than EmbeddingDimensions, zero-pad it
//     (dimensionPadded) rather than try an explicit dimensions request —
//     asking a model to produce more dimensions than it natively supports
//     isn't a real capability any provider exposes, so there's nothing to
//     gain by asking first. This is what makes most local/offline
//     embedding models usable at all: they commonly sit at 384-1024
//     native dimensions, well short of 1536, and none of them need to
//     support any particular API shape for padding to apply.
//  3. Dimensions explicitly requested at EmbeddingDimensions, for a
//     native output *longer* than EmbeddingDimensions. OpenAI's
//     text-embedding-3-* family supports truncating its (larger) native
//     output to a requested size on request. Unlike padding, this is only
//     offered via an explicit request — blindly slicing a longer vector
//     without the model's cooperation would silently produce garbage for
//     any model not specifically trained to keep its meaning front-loaded.
//
// Native is tried before the explicit request, because asking for a
// specific dimension count is itself a hard error on some models — verified
// directly against OpenAI's API: text-embedding-ada-002 returns "This model
// does not support specifying dimensions" the moment `dimensions` is set at
// all, even to its own native size. Trying the request first would make
// that otherwise-perfectly-usable model fail verification for a parameter
// it never needed in the first place.
//
// If none of the three lands on EmbeddingDimensions — a native output
// longer than EmbeddingDimensions that also rejects an explicit request —
// this returns an error and the caller should not proceed; every real
// write downstream assumes the embedder it's holding already produces the
// right length.
func VerifyEmbeddingDimensions(ctx context.Context, p Provider) (Provider, error) {
	probe := EmbedRequest{Input: []string{"hupi embedding dimension check"}}

	resp, nativeErr := p.Embed(ctx, probe)
	if nativeErr == nil && len(resp.Vectors) > 0 && len(resp.Vectors[0]) == EmbeddingDimensions {
		return p, nil
	}
	nativeLen := -1
	if nativeErr == nil && len(resp.Vectors) > 0 {
		nativeLen = len(resp.Vectors[0])
	}

	if nativeLen >= 0 && nativeLen < EmbeddingDimensions {
		return dimensionPadded{Provider: p, dimensions: EmbeddingDimensions}, nil
	}

	probe.Dimensions = EmbeddingDimensions
	resp, err := p.Embed(ctx, probe)
	if err == nil && len(resp.Vectors) > 0 && len(resp.Vectors[0]) == EmbeddingDimensions {
		return dimensionPinned{Provider: p, dimensions: EmbeddingDimensions}, nil
	}

	if nativeErr != nil {
		return nil, fmt.Errorf(
			"provider %s (%s): can't verify embedding dimensions — native embed call failed: %w",
			p.Vendor(), p.Model(), nativeErr,
		)
	}
	return nil, fmt.Errorf(
		"provider %s (%s) produces %d-dimension embeddings, longer than the required %d, and does not accept an explicit dimensions request to shorten it — every embedding column is a fixed vector(%d) (schema/0001_init.sql), and truncating a vector without the model's support could silently discard meaningful content; configure a different active_embedding_provider, or run a schema migration to widen the column and re-embed everything",
		p.Vendor(), p.Model(), nativeLen, EmbeddingDimensions, EmbeddingDimensions,
	)
}
