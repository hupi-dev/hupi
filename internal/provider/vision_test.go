package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOpenAICompat_DescribeImage_RequestShape asserts the exact wire
// shape sent to a vision-capable chat-completions endpoint: a
// multimodal content array with a text part and an image_url part whose
// url is a data: URI built from the caller's own bytes — never a
// client-supplied URL the server would fetch (SSRF avoidance is a
// structural property of this shape, not a runtime check).
func TestOpenAICompat_DescribeImage_RequestShape(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"model":"gpt-4.1","choices":[{"message":{"role":"assistant","content":"a red circle on a white background"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	p := NewOpenAICompat(OpenAICompatConfig{
		Name: "test", Vendor: "openai", Model: "gpt-4.1", BaseURL: srv.URL, APIKey: "test-key",
	})
	caption, err := p.DescribeImage(context.Background(), ImageInput{Data: []byte("fake-png-bytes"), MIMEType: "image/png"}, "describe this image")
	if err != nil {
		t.Fatalf("DescribeImage: %v", err)
	}
	if caption != "a red circle on a white background" {
		t.Errorf("DescribeImage() = %q, want the response content verbatim", caption)
	}

	if gotBody["model"] != "gpt-4.1" {
		t.Errorf("request model = %v, want gpt-4.1", gotBody["model"])
	}
	messages, _ := gotBody["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("request messages = %v, want exactly 1", messages)
	}
	msg := messages[0].(map[string]any)
	content, _ := msg["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("message content parts = %v, want exactly 2 (text, image_url)", content)
	}
	textPart := content[0].(map[string]any)
	if textPart["type"] != "text" || textPart["text"] != "describe this image" {
		t.Errorf("content[0] = %v, want {type:text, text:describe this image}", textPart)
	}
	imagePart := content[1].(map[string]any)
	if imagePart["type"] != "image_url" {
		t.Errorf("content[1].type = %v, want image_url", imagePart["type"])
	}
	imageURL, _ := imagePart["image_url"].(map[string]any)
	url, _ := imageURL["url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Errorf("image_url.url = %q, want a data: URI prefix, never a bare URL", url)
	}
}

// TestOpenAICompat_DescribeImage_UsesVisionModelOverride confirms a
// configured VisionModel is sent instead of the profile's own chat
// Model, for profiles whose chat Model isn't vision-capable.
func TestOpenAICompat_DescribeImage_UsesVisionModelOverride(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{}}`))
	}))
	defer srv.Close()

	p := NewOpenAICompat(OpenAICompatConfig{
		Name: "test", Vendor: "openai", Model: "text-only-reasoning-model", VisionModel: "gpt-4o", BaseURL: srv.URL, APIKey: "k",
	})
	if _, err := p.DescribeImage(context.Background(), ImageInput{Data: []byte("x"), MIMEType: "image/png"}, "describe"); err != nil {
		t.Fatalf("DescribeImage: %v", err)
	}
	if gotModel != "gpt-4o" {
		t.Errorf("request model = %q, want the configured VisionModel override %q", gotModel, "gpt-4o")
	}
}

// TestAnthropic_DescribeImage_RequestShape asserts Anthropic's own
// image content-block shape: base64 data with no "data:" prefix
// (Anthropic's convention differs from OpenAI's), under a
// source.media_type/source.data pair.
func TestAnthropic_DescribeImage_RequestShape(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"model":"claude-sonnet-5","content":[{"type":"text","text":"a blue square"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	p := NewAnthropic(AnthropicConfig{
		Name: "test", Vendor: "anthropic", Model: "claude-sonnet-5", BaseURL: srv.URL, APIKey: "k", APIVersion: "2023-06-01",
	})
	caption, err := p.DescribeImage(context.Background(), ImageInput{Data: []byte("fake-jpeg-bytes"), MIMEType: "image/jpeg"}, "describe this image")
	if err != nil {
		t.Fatalf("DescribeImage: %v", err)
	}
	if caption != "a blue square" {
		t.Errorf("DescribeImage() = %q, want the response text verbatim", caption)
	}

	messages, _ := gotBody["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("request messages = %v, want exactly 1", messages)
	}
	msg := messages[0].(map[string]any)
	content, _ := msg["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("message content parts = %v, want exactly 2 (image, text)", content)
	}
	imagePart := content[0].(map[string]any)
	if imagePart["type"] != "image" {
		t.Errorf("content[0].type = %v, want image", imagePart["type"])
	}
	source, _ := imagePart["source"].(map[string]any)
	if source["type"] != "base64" || source["media_type"] != "image/jpeg" {
		t.Errorf("content[0].source = %v, want {type:base64, media_type:image/jpeg}", source)
	}
	if data, _ := source["data"].(string); strings.HasPrefix(data, "data:") {
		t.Errorf("source.data = %q, want raw base64 with no data: prefix (OpenAI's convention, not Anthropic's)", data)
	}
}
