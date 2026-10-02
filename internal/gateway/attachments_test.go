package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hupi/internal/provider"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// newVisionTestHandler builds a Handler whose Registry.Vision() points
// at a real OpenAICompat adapter backed by an httptest server simulating
// the vision endpoint — reusing the real DescribeImage implementation
// against canned responses, the same "fake the upstream HTTP, not the
// adapter" style handler_test.go's newTestHandler already uses for
// ChatCompletion.
func newVisionTestHandler(t *testing.T, visionHandler http.HandlerFunc) *Handler {
	t.Helper()
	srv := httptest.NewServer(visionHandler)
	t.Cleanup(srv.Close)

	reg, err := provider.NewRegistry(provider.Config{
		ActiveChatProvider:          "chat",
		ActiveConsolidationProvider: "chat",
		ActiveEmbeddingProvider:     "chat",
		ActiveVisionProvider:        "vision",
		Providers: map[string]provider.ProfileConfig{
			"chat":   {Kind: provider.KindOpenAICompat, Vendor: "test", BaseURL: "http://unused.invalid", Model: "fake-model"},
			"vision": {Kind: provider.KindOpenAICompat, Vendor: "test", BaseURL: srv.URL, Model: "fake-vision-model"},
		},
	})
	if err != nil {
		t.Fatalf("provider.NewRegistry: %v", err)
	}
	return &Handler{Registry: reg}
}

func visionOKHandler(caption string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "fake-vision-model",
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": caption}}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}
}

func TestMergeAttachments_DocumentProvenanceFraming(t *testing.T) {
	h := &Handler{}
	messages := []provider.Message{{Role: provider.RoleUser, Content: "here's my resume"}}
	atts := []chatAttachment{{Type: "document", Filename: "resume.txt", Data: b64("5 years of Go experience.")}}

	got, warnings, err := h.mergeAttachments(context.Background(), messages, atts)
	if err != nil {
		t.Fatalf("mergeAttachments: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none for a clean plain-text attachment", warnings)
	}
	content := got[0].Content
	if !strings.Contains(content, "here's my resume") {
		t.Errorf("merged content = %q, want original text preserved", content)
	}
	if !strings.Contains(content, "[Attached file: resume.txt]") || !strings.Contains(content, "5 years of Go experience.") {
		t.Errorf("merged content = %q, want provenance-framed extracted text", content)
	}
}

func TestMergeAttachments_TargetsLastUserMessage(t *testing.T) {
	h := &Handler{}
	messages := []provider.Message{
		{Role: provider.RoleSystem, Content: "system prompt"},
		{Role: provider.RoleUser, Content: "first turn"},
		{Role: provider.RoleAssistant, Content: "first reply"},
		{Role: provider.RoleUser, Content: "second turn"},
	}
	atts := []chatAttachment{{Type: "document", Filename: "notes.txt", Data: b64("extra context")}}

	got, _, err := h.mergeAttachments(context.Background(), messages, atts)
	if err != nil {
		t.Fatalf("mergeAttachments: %v", err)
	}
	if strings.Contains(got[1].Content, "extra context") {
		t.Error("attachment text landed on the first user message, want only the last one")
	}
	if !strings.Contains(got[3].Content, "extra context") {
		t.Errorf("last user message = %q, want the attachment text merged in", got[3].Content)
	}
}

func TestMergeAttachments_RejectsWhenNoUserMessage(t *testing.T) {
	h := &Handler{}
	messages := []provider.Message{{Role: provider.RoleSystem, Content: "system only"}}
	atts := []chatAttachment{{Type: "document", Filename: "x.txt", Data: b64("x")}}

	_, _, err := h.mergeAttachments(context.Background(), messages, atts)
	if err == nil {
		t.Error("mergeAttachments() with no user message = nil error, want a rejection")
	}
}

func TestMergeAttachments_RejectsTooManyAttachments(t *testing.T) {
	h := &Handler{}
	messages := []provider.Message{{Role: provider.RoleUser, Content: "hi"}}
	atts := make([]chatAttachment, maxAttachmentsPerRequest+1)
	for i := range atts {
		atts[i] = chatAttachment{Type: "document", Filename: "x.txt", Data: b64("x")}
	}
	_, _, err := h.mergeAttachments(context.Background(), messages, atts)
	if err == nil {
		t.Error("mergeAttachments() over the attachment-count cap = nil error, want a rejection")
	}
}

func TestMergeAttachments_RejectsOversizedAttachment(t *testing.T) {
	h := &Handler{}
	messages := []provider.Message{{Role: provider.RoleUser, Content: "hi"}}
	big := strings.Repeat("a", maxAttachmentBytes+1)
	atts := []chatAttachment{{Type: "document", Filename: "big.txt", Data: b64(big)}}
	_, _, err := h.mergeAttachments(context.Background(), messages, atts)
	if err == nil {
		t.Error("mergeAttachments() over the size cap = nil error, want a rejection")
	}
}

func TestMergeAttachments_RejectsBadBase64(t *testing.T) {
	h := &Handler{}
	messages := []provider.Message{{Role: provider.RoleUser, Content: "hi"}}
	atts := []chatAttachment{{Type: "document", Filename: "x.txt", Data: "not-valid-base64!!!"}}
	_, _, err := h.mergeAttachments(context.Background(), messages, atts)
	if err == nil {
		t.Error("mergeAttachments() with invalid base64 = nil error, want a rejection")
	}
}

func TestMergeAttachments_RejectsDisallowedImageMIME(t *testing.T) {
	h := &Handler{}
	messages := []provider.Message{{Role: provider.RoleUser, Content: "hi"}}
	atts := []chatAttachment{{Type: "image", ContentType: "image/svg+xml", Filename: "x.svg", Data: b64("<svg/>")}}
	_, _, err := h.mergeAttachments(context.Background(), messages, atts)
	if err == nil {
		t.Error("mergeAttachments() with a disallowed image MIME = nil error, want a rejection")
	}
}

func TestMergeAttachments_RejectsInvalidType(t *testing.T) {
	h := &Handler{}
	messages := []provider.Message{{Role: provider.RoleUser, Content: "hi"}}
	atts := []chatAttachment{{Type: "video", Filename: "x.mp4", Data: b64("x")}}
	_, _, err := h.mergeAttachments(context.Background(), messages, atts)
	if err == nil {
		t.Error("mergeAttachments() with an unrecognized attachment type = nil error, want a rejection")
	}
}

func TestMergeAttachments_ImageCaptionSuccess(t *testing.T) {
	h := newVisionTestHandler(t, visionOKHandler("a golden retriever sitting in a park"))
	messages := []provider.Message{{Role: provider.RoleUser, Content: "what's in this photo?"}}
	atts := []chatAttachment{{Type: "image", ContentType: "image/png", Filename: "dog.png", Data: b64("fake-png-bytes")}}

	got, warnings, err := h.mergeAttachments(context.Background(), messages, atts)
	if err != nil {
		t.Fatalf("mergeAttachments: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none for a successful caption", warnings)
	}
	content := got[0].Content
	if !strings.Contains(content, "[Shared image: dog.png]") || !strings.Contains(content, "a golden retriever sitting in a park") {
		t.Errorf("merged content = %q, want the provenance-framed caption", content)
	}
}

// TestMergeAttachments_ImageCaptionDegradesOnVisionFailure is the real
// "never block the turn" behavior: a vision-provider failure must
// produce a placeholder marker and a warning, never fail the whole
// request.
func TestMergeAttachments_ImageCaptionDegradesOnVisionFailure(t *testing.T) {
	h := newVisionTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"upstream vision provider broke"}`))
	})
	messages := []provider.Message{{Role: provider.RoleUser, Content: "what's in this photo?"}}
	atts := []chatAttachment{{Type: "image", ContentType: "image/png", Filename: "dog.png", Data: b64("fake-png-bytes")}}

	got, warnings, err := h.mergeAttachments(context.Background(), messages, atts)
	if err != nil {
		t.Fatalf("mergeAttachments: %v, want a degraded placeholder instead of a request-level error", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly 1 for the failed caption", warnings)
	}
	content := got[0].Content
	if !strings.Contains(content, "[Shared image: dog.png — description unavailable]") {
		t.Errorf("merged content = %q, want the degraded placeholder marker", content)
	}
}
