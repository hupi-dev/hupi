package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"hupi/internal/provider"
)

func TestLoadRegistry_SupportsTools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.yaml")
	yaml := `active_chat_provider: default
active_consolidation_provider: default
active_embedding_provider: default
providers:
  default:
    kind: openai_compat
    vendor: openai
    api_base: http://127.0.0.1:1/v1
    model: gpt-4.1
  local:
    kind: openai_compat
    vendor: ollama
    api_base: http://127.0.0.1:1/v1
    model: tiny
    supports_tools: false
  explicit:
    kind: anthropic
    vendor: anthropic
    api_base: http://127.0.0.1:1/v1
    model: claude
    supports_tools: true
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := loadRegistry(path)
	if err != nil {
		t.Fatalf("loadRegistry: %v", err)
	}
	for name, want := range map[string]bool{"default": true, "local": false, "explicit": true} {
		p, ok := reg.Named(name)
		if !ok {
			t.Fatalf("profile %s missing", name)
		}
		if got := provider.SupportsTools(p); got != want {
			t.Errorf("%s: SupportsTools = %v, want %v", name, got, want)
		}
	}
}
