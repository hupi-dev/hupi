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
// Two real embed calls, tried in this order:
//
//  1. Native output, no dimensions requested. Plenty of models (OpenAI's
//     own ada-002 among them) have no truncation knob at all and simply
//     always produce one fixed size — if that size already happens to be
//     EmbeddingDimensions, nothing else is needed.
//  2. Dimensions explicitly requested at EmbeddingDimensions. OpenAI's
//     text-embedding-3-* family supports truncating its (larger) native
//     output to a requested size on request.
//
// Native is tried first, not the explicit request, because asking for a
// specific dimension count is itself a hard error on some models — verified
// directly against OpenAI's API: text-embedding-ada-002 returns "This model
// does not support specifying dimensions" the moment `dimensions` is set at
// all, even to its own native size. Trying the request-first would make
// that otherwise-perfectly-usable model fail verification for a parameter
// it never needed in the first place.
//
// If neither path lands on EmbeddingDimensions, this returns an error and
// the caller should not proceed — every real write downstream assumes the
// embedder it's holding already produces the right length.
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
		"provider %s (%s) produces %d-dimension embeddings, not the required %d, and does not accept an explicit dimensions request either — every embedding column is a fixed vector(%d) (schema/0001_init.sql); configure a different active_embedding_provider, or run a schema migration to widen the column and re-embed everything",
		p.Vendor(), p.Model(), nativeLen, EmbeddingDimensions, EmbeddingDimensions,
	)
}
