package provider

import "errors"

// ErrEmbedNotSupported is returned by Embed on a Provider configured only
// for chat — e.g. an Anthropic profile, since Anthropic doesn't serve
// embeddings. The registry never routes embedding calls to such a
// provider itself (see registry.go), this exists for direct callers and
// for adapters to implement Provider without a separate optional
// interface.
var ErrEmbedNotSupported = errors.New("provider: embeddings not supported by this profile")
