package provider

import "fmt"

// Kind selects which adapter a profile is built from. Adding a vendor
// family with a genuinely different wire shape (not OpenAI-compatible,
// not Anthropic's shape) means adding both a Kind and an adapter — a
// bounded, occasional cost, not something that happens per endpoint.
type Kind string

const (
	KindOpenAICompat Kind = "openai_compat"
	KindAnthropic    Kind = "anthropic"
)

// ProfileConfig is the Go-side shape of one entry under `providers:` in
// providers.yaml (see ARCHITECTURE.md § Provider abstraction). Parsing the
// YAML file into this struct is left to the config-loading package (a thin
// concern, not sketched here); Registry only needs the parsed result.
type ProfileConfig struct {
	Kind       Kind
	Vendor     string // e.g. "openai", "azure-openai", "groq", "ollama", "anthropic"
	BaseURL    string
	APIKey     string
	Model      string
	APIVersion string // Anthropic only
}

// Config mirrors providers.yaml: named profiles, plus which named profile
// fills each role. active_consolidation_provider is intentionally a
// separate setting from active_chat_provider — see ARCHITECTURE.md
// § Provider abstraction on why that stability matters.
//
// ActiveGroundingProvider is optional: leave it blank to run the
// grounding-check pass (ARCHITECTURE.md § Consolidation integrity
// safeguards) on the same profile as consolidation, or point it at a
// separate, cheaper model — grounding is a yes/no fact check per claim,
// not prose generation, so it doesn't need the same model quality as
// writing the summary itself.
type Config struct {
	ActiveChatProvider          string
	ActiveConsolidationProvider string
	ActiveGroundingProvider     string
	ActiveEmbeddingProvider     string
	Providers                   map[string]ProfileConfig
}

// Registry builds and holds one Provider instance per configured profile,
// and knows which profile fills each of the roles the rest of the system
// cares about.
type Registry struct {
	byName        map[string]Provider
	chatName      string
	consolidation string
	grounding     string
	embedding     string
}

func NewRegistry(cfg Config) (*Registry, error) {
	reg := &Registry{byName: make(map[string]Provider, len(cfg.Providers))}

	for name, pc := range cfg.Providers {
		p, err := build(name, pc)
		if err != nil {
			return nil, fmt.Errorf("provider registry: profile %q: %w", name, err)
		}
		reg.byName[name] = p
	}

	groundingProvider := cfg.ActiveGroundingProvider
	if groundingProvider == "" {
		groundingProvider = cfg.ActiveConsolidationProvider
	}

	for role, name := range map[string]string{
		"active_chat_provider":          cfg.ActiveChatProvider,
		"active_consolidation_provider": cfg.ActiveConsolidationProvider,
		"active_grounding_provider":     groundingProvider,
		"active_embedding_provider":     cfg.ActiveEmbeddingProvider,
	} {
		if _, ok := reg.byName[name]; !ok {
			return nil, fmt.Errorf("provider registry: %s %q is not a defined profile", role, name)
		}
	}
	reg.chatName = cfg.ActiveChatProvider
	reg.consolidation = cfg.ActiveConsolidationProvider
	reg.grounding = groundingProvider
	reg.embedding = cfg.ActiveEmbeddingProvider

	return reg, nil
}

func build(name string, pc ProfileConfig) (Provider, error) {
	switch pc.Kind {
	case KindOpenAICompat:
		return NewOpenAICompat(OpenAICompatConfig{
			Name: name, Vendor: pc.Vendor, Model: pc.Model, BaseURL: pc.BaseURL, APIKey: pc.APIKey,
		}), nil
	case KindAnthropic:
		return NewAnthropic(AnthropicConfig{
			Name: name, Vendor: pc.Vendor, Model: pc.Model, BaseURL: pc.BaseURL, APIKey: pc.APIKey, APIVersion: pc.APIVersion,
		}), nil
	default:
		return nil, fmt.Errorf("unknown provider kind %q", pc.Kind)
	}
}

// Chat is what the gateway forwards a live user turn to — see
// ARCHITECTURE.md § Request lifecycle, step 4.
func (r *Registry) Chat() Provider { return r.byName[r.chatName] }

// Consolidation is what the nightly rollup job uses to write summaries —
// deliberately not necessarily the same profile as Chat.
func (r *Registry) Consolidation() Provider { return r.byName[r.consolidation] }

// Grounding is what the second-pass fact check
// (internal/consolidation/grounding.go) uses — defaults to Consolidation's
// profile if active_grounding_provider was left unset in providers.yaml.
func (r *Registry) Grounding() Provider { return r.byName[r.grounding] }

// Embedding is used both for live retrieval queries and for index rebuild
// during consolidation.
func (r *Registry) Embedding() Provider { return r.byName[r.embedding] }

// Named looks up a specific profile by name — used by the switch-provider
// admin path and by tests, not by the request-serving hot path.
func (r *Registry) Named(name string) (Provider, bool) {
	p, ok := r.byName[name]
	return p, ok
}
